// Package hookserve serves the Claude Code synchronous hook protocol against an
// agentqueue.Queue.
//
// The protocol is: Claude Code runs a hook command at a session boundary
// (SessionStart, UserPromptSubmit, Stop, SubagentStop), writes a JSON payload to
// its stdin, and reads a JSON injection back on its stdout. This package turns
// that payload into a queue read: it claims the pending messages addressed to the
// session and renders them as the additionalContext Claude Code injects.
//
// The logic lived in the standalone agentqueue binary; it is factored out here so
// a second program can serve the same protocol against the same queue without
// forking the code. The caller owns queue-root resolution and opens the Queue, so
// each program serves messages from its own root.
package hookserve

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ka2n/agentqueue"
	"github.com/ka2n/crossagent/agent"
	crosshooks "github.com/ka2n/crossagent/hooks"
)

// Hook event names this package acts on. Every other event is ignored.
const (
	eventSessionStart     = string(crosshooks.EventSessionStart)
	eventUserPromptSubmit = string(crosshooks.EventUserPromptSubmit)
	eventStop             = string(crosshooks.EventStop)
	eventSubagentStop     = string(crosshooks.EventSubagentStop)
)

// SelfCheckToken is the stable string a caller emits for
// `hook claude --self-check`. An install probe runs the command it is about to
// wire into Claude's settings and matches this token to prove the binary is the
// expected version and that the hook subcommand is available. It is deliberately
// stable across programs, so a probe written against one program keeps matching.
const SelfCheckToken = "agentqueue hook claude: self-check ok"

// sourceCompact is the SessionStart source that means context is being rebuilt
// after compaction rather than a new session beginning.
const sourceCompact = crosshooks.SourceCompact

// DefaultMax is how many items one hook invocation delivers when Options.Max is
// not set to a positive value.
const DefaultMax = 5

// Payload is the subset of the Claude Code hook payload this package reads.
// Claude Code sends a superset; unknown fields are ignored.
type Payload struct {
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

// HookSpecificOutput carries the context injection back to Claude Code.
type HookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// Output is what a caller prints on stdout when it delivered something.
//
// Decision and Reason are top-level, not inside HookSpecificOutput: the hooks
// reference lists Stop and SubagentStop under "top-level decision", and their
// hookSpecificOutput accepts only additionalContext.
type Output struct {
	Decision           string              `json:"decision,omitempty"`
	Reason             string              `json:"reason,omitempty"`
	HookSpecificOutput *HookSpecificOutput `json:"hookSpecificOutput,omitempty"`
}

// Options configures HandleClaude.
type Options struct {
	// To overrides the target the payload names. When empty the target is
	// derived from the payload's session_id, then $CLAUDE_CODE_SESSION_ID.
	To string
	// Max caps how many items one call claims. Zero or less means DefaultMax.
	Max int
	// NoBlock suppresses the Stop/SubagentStop block decision, delivering only
	// additionalContext.
	NoBlock bool
	// Getenv reads the environment. Nil means os.Getenv.
	Getenv func(string) string
	// Logf records non-fatal diagnostics (no target for an event, a failed
	// address registration). Nil discards them. It must never write to the
	// stdout that carries the protocol.
	Logf func(format string, args ...any)
}

func (o Options) getenv() func(string) string {
	if o.Getenv != nil {
		return o.Getenv
	}
	return os.Getenv
}

func (o Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// ParsePayload decodes a hook payload from r. An empty or whitespace-only stream
// yields a zero Payload and no error, so an event with no body is handled the
// same as one this package ignores. Malformed JSON is an error.
func ParsePayload(r io.Reader) (Payload, error) {
	var in Payload
	body, err := io.ReadAll(r)
	if err != nil {
		return in, fmt.Errorf("read hook payload: %w", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return in, nil
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return in, fmt.Errorf("decode hook payload: %w", err)
	}
	return in, nil
}

// HandleClaude serves one Claude Code hook against q. It dispatches on the
// payload's event, claims up to opts.Max pending messages for the target, and
// returns the injection to print. The bool reports whether anything was
// delivered; when it is false the returned Output is nil and the caller prints
// nothing.
//
// It claims items (TakeNext), it does not merely read them: claiming is what
// makes delivery exactly-once when several hooks are installed at the same time.
//
// On SessionStart it first records the session's address so a producer reacting
// to the injected context can already address the session, then, unless the
// source is compaction, delivers. Compaction rebuilds the context of a running
// session, where an injection would land outside any turn and be consumed
// invisibly, so nothing is delivered there.
func HandleClaude(q *agentqueue.Queue, payload Payload, opts Options) (*Output, bool, error) {
	switch payload.HookEventName {
	case eventSessionStart, eventUserPromptSubmit, eventStop, eventSubagentStop:
	default:
		// Not an event this package handles.
		return nil, false, nil
	}

	getenv := opts.getenv()

	target, err := Target(opts.To, payload.SessionID, getenv)
	if err != nil {
		// No session id anywhere: there is no mailbox to read, which is not
		// worth failing over.
		opts.logf("no target for %s event: %v", payload.HookEventName, err)
		return nil, false, nil
	}

	if payload.HookEventName == eventSessionStart {
		// Record the address before delivering, so a producer that reacts to
		// the injected context can already address this session.
		if err := RegisterAddress(q, target, payload, getenv); err != nil {
			opts.logf("register %s: %v", target, err)
		}
		if payload.Source == sourceCompact {
			// Compaction is rebuilding the context of a session that is
			// already running. Injecting a queued message there would land it
			// outside any turn and consume it invisibly.
			return nil, false, nil
		}
	}

	items, err := ClaimUpTo(q, target, opts.Max)
	if err != nil {
		return nil, false, err
	}
	if len(items) == 0 {
		return nil, false, nil
	}

	out := &Output{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName:     payload.HookEventName,
			AdditionalContext: RenderDelivery(q, target, items),
		},
	}
	// Stop is the only delivery point that can act without the user, but only
	// if the turn is prevented from ending. stop_hook_active means a Stop hook
	// already did that for this turn; blocking again is how a hook loops.
	if (payload.HookEventName == eventStop || payload.HookEventName == eventSubagentStop) && !payload.StopHookActive && !opts.NoBlock {
		out.Decision = "block"
		out.Reason = fmt.Sprintf("%d queued message(s) arrived for %s; act on them before finishing.", len(items), target)
	}
	return out, true, nil
}

// Target derives the mailbox to read: to, else sessionID, else
// $CLAUDE_CODE_SESSION_ID. The default agent is claude, but an explicit to may
// name any mailbox namespace.
func Target(to, sessionID string, getenv func(string) string) (agentqueue.Target, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if s := strings.TrimSpace(to); s != "" {
		target, err := agentqueue.ParseTarget(s)
		if err != nil {
			return agentqueue.Target{}, err
		}
		return target, nil
	}
	name := strings.TrimSpace(sessionID)
	if name == "" {
		name = strings.TrimSpace(getenv("CLAUDE_CODE_SESSION_ID"))
	}
	if name == "" {
		return agentqueue.Target{}, errors.New("no --to, session_id or $CLAUDE_CODE_SESSION_ID")
	}
	return agentqueue.Target{Agent: agent.Claude.String(), Name: name}, nil
}

// ClaimUpTo claims at most max pending items, oldest first. Max of zero or less
// means DefaultMax.
//
// Claiming is what makes delivery idempotent: several hooks can be installed at
// once, and whichever fires first moves the item out of pending/, so no item is
// ever delivered twice.
func ClaimUpTo(q *agentqueue.Queue, t agentqueue.Target, max int) ([]agentqueue.Item, error) {
	if max <= 0 {
		max = DefaultMax
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

// RenderDelivery formats claimed items as the additionalContext string.
//
// The text is phrased as a factual report rather than as instructions: the hooks
// reference warns that context framed as out-of-band system commands trips
// Claude's prompt-injection defenses. It stays compact because every byte here is
// spent from the session's context window.
//
// The ack command wording comes from q.AckCmd, so a program embedding this
// library (one that opened its queue WithAckCmd) tells the agent to run its own
// ack command rather than the standalone "agentqueue ack". The "<id>" passed to
// AckCmd is a literal placeholder: the summary line covers every delivered item,
// each of which prints its own id below. The "agentqueue delivered ..." preamble
// is left as the library naming itself in a description, not a command the agent
// runs, so it is intentionally not parameterized.
func RenderDelivery(q *agentqueue.Queue, t agentqueue.Target, items []agentqueue.Item) string {
	var b strings.Builder
	fmt.Fprintf(&b, "agentqueue delivered %d queued message(s) addressed to this session (%s).\n", len(items), t)
	b.WriteString("They were pushed by an external producer, not written by the user. They are already claimed, so no other consumer will see them.\n")
	fmt.Fprintf(&b, "Acknowledge each one after acting on it: %s\n", q.AckCmd(t, "<id>"))
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

// SelfCheck writes SelfCheckToken to w and returns the process exit code a
// caller should use (0). It is the body of `hook claude --self-check`.
func SelfCheck(w io.Writer) int {
	fmt.Fprintln(w, SelfCheckToken)
	return 0
}
