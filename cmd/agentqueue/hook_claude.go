package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ka2n/agentqueue"
)

// Hook event names this command acts on. Every other event is ignored.
const (
	eventSessionStart     = "SessionStart"
	eventUserPromptSubmit = "UserPromptSubmit"
	eventStop             = "Stop"
	eventSubagentStop     = "SubagentStop"
)

// selfCheckToken is deliberately stable: install uses it to prove that the
// command it is about to put into Claude's settings is this version of the
// binary and that the hook subcommand is available.
const selfCheckToken = "agentqueue hook claude: self-check ok"

// sourceCompact is the SessionStart source that means context is being rebuilt
// after compaction rather than a new session beginning.
const sourceCompact = "compact"

// defaultHookMax is how many items one hook invocation delivers.
const defaultHookMax = 5

// hookInput is the subset of the Claude Code hook payload this command reads.
// Claude Code sends a superset; unknown fields are ignored.
type hookInput struct {
	SessionID      string `json:"session_id"`
	HookEventName  string `json:"hook_event_name"`
	Cwd            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
	Model          string `json:"model"`

	// Source is set on SessionStart: startup, resume, clear, compact or fork.
	Source string `json:"source"`
	// StopHookActive is set on Stop and SubagentStop. It is true when the turn
	// is already continuing because a Stop hook blocked it, and is the loop
	// guard: never block again while it holds.
	StopHookActive bool `json:"stop_hook_active"`
}

// hookSpecificOutput carries the context injection back to Claude Code.
type hookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// hookOutput is what the command prints on stdout when it delivered something.
//
// Decision and Reason are top-level, not inside hookSpecificOutput: the hooks
// reference lists Stop and SubagentStop under "top-level decision", and their
// hookSpecificOutput accepts only additionalContext.
type hookOutput struct {
	Decision           string              `json:"decision,omitempty"`
	Reason             string              `json:"reason,omitempty"`
	HookSpecificOutput *hookSpecificOutput `json:"hookSpecificOutput,omitempty"`
}

// hookLogger writes diagnostics without ever reaching stdout, which belongs to
// the hook protocol.
type hookLogger struct {
	stderr io.Writer
	file   io.WriteCloser
}

func newHookLogger(path string, stderr io.Writer) *hookLogger {
	l := &hookLogger{stderr: stderr}
	if strings.TrimSpace(path) == "" {
		return l
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "agentqueue hook: open --log %s: %v\n", path, err)
		return l
	}
	l.file = f
	return l
}

func (l *hookLogger) logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.stderr, "agentqueue hook: %s\n", msg)
	if l.file != nil {
		fmt.Fprintf(l.file, "%s agentqueue hook: %s\n", time.Now().Format(time.RFC3339), msg)
	}
}

func (l *hookLogger) Close() {
	if l.file != nil {
		_ = l.file.Close()
	}
}

// cmdHookClaude serves every Claude Code hook this library installs. It reads
// the hook payload on stdin, dispatches on hook_event_name, and prints a
// context injection only when it actually claimed something.
//
// It is deliberately hard to fail: a hook that errors out or prints garbage
// disrupts the user's session, so any internal problem is logged and the
// command exits 0 with empty stdout.
func cmdHookClaude(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	// Keep the probe out of the normal flag help: it is an install-time
	// implementation detail, not a hook mode users need to invoke manually.
	if len(args) == 1 && args[0] == "--self-check" {
		fmt.Fprintln(stdout, selfCheckToken)
		return exitOK, nil
	}

	var (
		fs      = newFlagSet("hook claude")
		to      = fs.String("to", "", "target session as <agent>:<name>, overriding the payload's session_id")
		root    = fs.String("root", "", "queue root directory")
		logPath = fs.String("log", "", "append diagnostics to this file")
		max     = fs.Int("max", defaultHookMax, "deliver at most this many messages per hook run")
		noBlock = fs.Bool("no-block", false, "never emit decision: block on Stop, only additionalContext")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitUsage, fmt.Errorf("%w\nusage: agentqueue hook claude [--to <agent>:<name>] [--max 5] [--log FILE] [--no-block]", err)
		}
		// A misconfigured hook must not break the session either.
		fmt.Fprintf(stderr, "agentqueue hook: %v\n", err)
		return exitOK, nil
	}

	log := newHookLogger(*logPath, stderr)
	defer log.Close()

	out, err := hookClaude(*to, *root, *max, *noBlock, stdin, os.Getenv, log)
	if err != nil {
		log.logf("%v", err)
		return exitOK, nil
	}
	if out == nil {
		// Nothing pending: print nothing at all.
		return exitOK, nil
	}
	body, err := json.Marshal(out)
	if err != nil {
		log.logf("encode hook output: %v", err)
		return exitOK, nil
	}
	fmt.Fprintln(stdout, string(body))
	return exitOK, nil
}

// hookClaude is the testable core: it returns the output to print, or nil when
// there is nothing to say.
func hookClaude(to, root string, max int, noBlock bool, stdin io.Reader, getenv func(string) string, log *hookLogger) (*hookOutput, error) {
	body, err := io.ReadAll(stdin)
	if err != nil {
		return nil, fmt.Errorf("read hook payload: %w", err)
	}
	var in hookInput
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("decode hook payload: %w", err)
		}
	}

	switch in.HookEventName {
	case eventSessionStart, eventUserPromptSubmit, eventStop, eventSubagentStop:
	default:
		// Not an event this command handles.
		return nil, nil
	}

	target, err := hookTarget(to, in.SessionID, getenv)
	if err != nil {
		// No session id anywhere: there is no mailbox to read, which is not
		// worth failing over.
		log.logf("no target for %s event: %v", in.HookEventName, err)
		return nil, nil
	}

	q, err := openQueue(root)
	if err != nil {
		return nil, err
	}

	if in.HookEventName == eventSessionStart {
		// Record the address before delivering, so a producer that reacts to
		// the injected context can already address this session.
		if err := registerAddress(q, target, in, getenv); err != nil {
			log.logf("register %s: %v", target, err)
		}
		if in.Source == sourceCompact {
			// Compaction is rebuilding the context of a session that is
			// already running. Injecting a queued message there would land it
			// outside any turn and consume it invisibly.
			return nil, nil
		}
	}

	items, err := claimUpTo(q, target, max)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}

	out := &hookOutput{
		HookSpecificOutput: &hookSpecificOutput{
			HookEventName:     in.HookEventName,
			AdditionalContext: renderDelivery(target, items),
		},
	}
	// Stop is the only delivery point that can act without the user, but only
	// if the turn is prevented from ending. stop_hook_active means a Stop hook
	// already did that for this turn; blocking again is how a hook loops.
	if (in.HookEventName == eventStop || in.HookEventName == eventSubagentStop) && !in.StopHookActive && !noBlock {
		out.Decision = "block"
		out.Reason = fmt.Sprintf("%d queued message(s) arrived for %s; act on them before finishing.", len(items), target)
	}
	return out, nil
}

// hookTarget derives the mailbox to read: --to, else the payload's session_id,
// else $CLAUDE_CODE_SESSION_ID. The agent is always claude.
func hookTarget(to, sessionID string, getenv func(string) string) (agentqueue.Target, error) {
	if s := strings.TrimSpace(to); s != "" {
		return agentqueue.ParseTarget(s)
	}
	name := strings.TrimSpace(sessionID)
	if name == "" {
		name = strings.TrimSpace(getenv("CLAUDE_CODE_SESSION_ID"))
	}
	if name == "" {
		return agentqueue.Target{}, errors.New("no --to, session_id or $CLAUDE_CODE_SESSION_ID")
	}
	return agentqueue.Target{Agent: "claude", Name: name}, nil
}

// claimUpTo claims at most max pending items, oldest first.
//
// Claiming is what makes delivery idempotent: several hooks can be installed
// at once, and whichever fires first moves the item out of pending/, so no
// item is ever delivered twice.
func claimUpTo(q *agentqueue.Queue, t agentqueue.Target, max int) ([]agentqueue.Item, error) {
	if max <= 0 {
		max = defaultHookMax
	}
	var items []agentqueue.Item
	for len(items) < max {
		item, err := q.TakeNext(t)
		if errors.Is(err, agentqueue.ErrEmpty) {
			break
		}
		if err != nil {
			if len(items) > 0 {
				// Deliver what was already claimed rather than dropping it.
				break
			}
			return nil, err
		}
		items = append(items, *item)
	}
	return items, nil
}

// renderDelivery formats claimed items as the additionalContext string.
//
// The text is phrased as a factual report rather than as instructions: the
// hooks reference warns that context framed as out-of-band system commands
// trips Claude's prompt-injection defenses. It stays compact because every
// byte here is spent from the session's context window.
func renderDelivery(t agentqueue.Target, items []agentqueue.Item) string {
	var b strings.Builder
	fmt.Fprintf(&b, "agentqueue delivered %d queued message(s) addressed to this session (%s).\n", len(items), t)
	b.WriteString("They were pushed by an external producer, not written by the user. They are already claimed, so no other consumer will see them.\n")
	fmt.Fprintf(&b, "Acknowledge each one after acting on it: agentqueue ack --to %s <id>\n", t)
	for _, item := range items {
		fmt.Fprintf(&b, "\n--- id %s  %s", item.ID, item.CreatedAt.UTC().Format(time.RFC3339))
		if len(item.Meta) > 0 {
			keys := make([]string, 0, len(item.Meta))
			for k := range item.Meta {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			pairs := make([]string, 0, len(keys))
			for _, k := range keys {
				pairs = append(pairs, k+"="+item.Meta[k])
			}
			fmt.Fprintf(&b, "  meta %s", strings.Join(pairs, " "))
		}
		fmt.Fprintf(&b, "\n%s\n", strings.TrimRight(item.Text, "\n"))
	}
	return b.String()
}

// cmdHook dispatches "hook <agent>". Only claude has hooks; every other agent
// either needs no setup or has no integration yet.
func cmdHook(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 {
		return exitUsage, errors.New("usage: agentqueue hook claude [flags]")
	}
	switch args[0] {
	case "claude":
		return cmdHookClaude(args[1:], stdin, stdout, stderr)
	default:
		return exitUsage, fmt.Errorf("no hook integration for agent %q; only claude has one", args[0])
	}
}
