// Command agentqueue sends and receives messages for AI coding-agent sessions.
//
// A message is addressed to a target written as <agent>:<name>. An external
// producer pushes; the agent session consumes. See `agentqueue -h`.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ka2n/agentqueue"
)

// Exit codes. They are part of the CLI contract: an agent's shell needs to tell
// "nothing arrived" from "something broke".
const (
	exitOK      = 0
	exitError   = 1
	exitUsage   = 2
	exitTimeout = 3
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run dispatches a subcommand and returns the process exit code. main is a thin
// wrapper around it so the whole CLI is testable.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stdout)
		return exitOK
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(stdout)
		return exitOK
	}

	// code is the exit status on success; only wait reports anything but 0.
	var (
		err  error
		code = exitOK
	)
	switch args[0] {
	case "push":
		err = cmdPush(ctx, args[1:], stdin, stdout)
	case "wait":
		code, err = cmdWait(ctx, args[1:], stdout, stderr)
	case "list":
		err = cmdList(args[1:], stdout)
	case "take":
		err = cmdTake(args[1:], stdout)
	case "ack":
		err = cmdAck(args[1:], stdout)
	case "hook":
		code, err = cmdHook(args[1:], stdin, stdout, stderr)
	case "register":
		err = cmdRegister(args[1:], stdin, stdout)
	case "unregister":
		err = cmdUnregister(args[1:], stdin, stdout)
	case "targets":
		err = cmdTargets(args[1:], stdout)
	case "sessions":
		err = cmdSessions(ctx, args[1:], stdout)
	case "install":
		err = cmdInstall(args[1:], stdin, stdout, stderr)
	case "uninstall":
		err = cmdUninstall(args[1:], stdin, stdout)
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n\n", args[0])
		usage(stderr)
		return exitUsage
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitUsage
		}
		fmt.Fprintf(stderr, "agentqueue: %v\n", err)
		if code == exitUsage {
			// The subcommand already classified this as a usage problem.
			return exitUsage
		}
		return exitError
	}
	return code
}

// usage lists every subcommand. It is written for an agent reader, because the
// arrival notice pushed into a codex thread points the agent at these commands.
func usage(w io.Writer) {
	fmt.Fprint(w, `agentqueue - a message queue for AI coding-agent sessions.

Each message is addressed to a target written as <agent>:<name>, for example
codex:1f0a-thread or claude:reviewer. A bare name means the claude agent, so
"reviewer" is "claude:reviewer".

Usage:
  agentqueue <command> [flags]

Commands:
  push   Enqueue a message for an agent session and notify it.
           agentqueue push --to claude:reviewer "please review PR 42"
           agentqueue push --to codex:1f0a-thread -   # body from stdin
  wait   Block until a message arrives. Run this in the background from a
         claude session; the command exits on arrival, which resumes your turn.
           agentqueue wait --to claude:reviewer --timeout 3600 --take
  list   List the messages held for a target in one state.
           agentqueue list --to claude:reviewer --state pending
  take   Claim a message and print it, so no other consumer receives it.
           agentqueue take --to codex:1f0a-thread --next
  ack    Mark a message done once you have acted on it.
           agentqueue ack --to codex:1f0a-thread 1756800000000-0a1b2c3d4e5f

Setup and session commands:
  install     Detect the agents you have and set up their integration. For
              claude that means hook entries in a settings file; codex needs
              none. Confirms before writing; --dry-run and --print show the
              exact JSON block instead. Use --skip-self-check only when the
              command is known to be an older or wrapped binary.
                agentqueue install --agent claude --scope user
  uninstall   Remove the hooks install added, and only those.
  hook claude Serve a Claude Code hook: read the payload on stdin, claim what
              is pending and inject it into the session. Installed by
              "install"; you do not run this by hand.
  register    Record where this session can be reached, so a producer can
              address it by session id or by working directory.
  unregister  Drop that record.
  targets     List the mailboxes under the queue root and what they hold.
  sessions    Discover Claude, Codex and Pi sessions from their own storage.
              Use --agent, --cwd, --limit and --json to narrow or script it.

Common flags:
  --to <agent>:<name>   target session (required)
  --root <dir>          queue root directory
  --json                print JSON instead of a human summary

Queue root resolution, in order:
  --root, $AGENTQUEUE_ROOT, $XDG_STATE_HOME/agentqueue, ~/.local/state/agentqueue

Exit codes:
  0  success (for wait: at least one message arrived)
  1  an error occurred
  2  a usage problem
  3  wait timed out with no message (not an error)

Delivery depends on the agent. A claude session is delivered to by hooks: with
"agentqueue install", a queued message is injected at the next session
boundary - session start, the next prompt, or the end of a turn - and the item
is claimed as it is delivered, so no message arrives twice. A claude session
can also pull, by running "agentqueue wait" as a background command and
resuming when it exits; that needs no hooks. A codex session is pushed an
arrival notice on enqueue; the notice carries no message body, only the id and
the command that fetches it.
`)
}

// --- shared helpers ---

// newFlagSet returns a flag set that reports errors through the returned error
// rather than writing to stderr and exiting.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// progName is the name of this binary, used in the arrival notice's fetch
// command so the notice points at the CLI the agent actually has.
func progName(argv0 string) string {
	base := strings.TrimSuffix(filepath.Base(argv0), ".exe")
	if base == "" || base == "." || base == ".." || base == string(filepath.Separator) {
		return "agentqueue"
	}
	for i := range len(base) {
		c := base[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			// A generated or otherwise odd argv[0] would render an
			// uncopyable command; fall back to the published name.
			return "agentqueue"
		}
	}
	return base
}

// resolveRoot picks the queue root: the --root flag, then $AGENTQUEUE_ROOT, then
// $XDG_STATE_HOME/agentqueue, then ~/.local/state/agentqueue.
func resolveRoot(override string, getenv func(string) string) (string, error) {
	if s := strings.TrimSpace(override); s != "" {
		return s, nil
	}
	if s := strings.TrimSpace(getenv("AGENTQUEUE_ROOT")); s != "" {
		return s, nil
	}
	if s := strings.TrimSpace(getenv("XDG_STATE_HOME")); s != "" {
		return filepath.Join(s, "agentqueue"), nil
	}
	home := strings.TrimSpace(getenv("HOME"))
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return "", fmt.Errorf("resolve queue root: no --root, $AGENTQUEUE_ROOT, $XDG_STATE_HOME or home directory: %w", err)
		}
	}
	return filepath.Join(home, ".local", "state", "agentqueue"), nil
}

// openQueue resolves the root and opens the queue with this binary's own fetch
// command, so notices tell the agent to run "<prog> take --to <t> --next".
func openQueue(override string) (*agentqueue.Queue, error) {
	root, err := resolveRoot(override, os.Getenv)
	if err != nil {
		return nil, err
	}
	prog := progName(os.Args[0])
	return agentqueue.Open(root, agentqueue.WithFetchCmd(func(t agentqueue.Target) string {
		return fmt.Sprintf("%s take --to %s --next", prog, t)
	}))
}

// resolveTarget parses the --to value.
func resolveTarget(to string) (agentqueue.Target, error) {
	if strings.TrimSpace(to) == "" {
		return agentqueue.Target{}, errors.New("--to is required, for example --to codex:my-thread")
	}
	return agentqueue.ParseTarget(to)
}

// parseMeta turns repeated key=value flags into a map.
func parseMeta(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --meta %q: want key=value", p)
		}
		out[k] = v
	}
	return out, nil
}

// metaFlag collects a repeatable --meta key=value flag.
type metaFlag []string

func (m *metaFlag) String() string { return strings.Join(*m, ",") }

func (m *metaFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// printJSON writes v as indented JSON.
func printJSON(w io.Writer, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	_, err = fmt.Fprintln(w, string(body))
	return err
}

// firstLine returns the first line of s, shortened for a summary listing.
func firstLine(s string) string {
	line := s
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i] + " ..."
	}
	line = strings.TrimSpace(line)
	if len(line) > 200 {
		line = line[:200] + "..."
	}
	return line
}

// printItem prints one item as JSON or as a header plus body.
func printItem(w io.Writer, item *agentqueue.Item, asJSON bool) error {
	if asJSON {
		return printJSON(w, item)
	}
	fmt.Fprintf(w, "id: %s\n", item.ID)
	fmt.Fprintf(w, "target: %s\n", item.Target)
	fmt.Fprintf(w, "created: %s\n", item.CreatedAt.Format(time.RFC3339))
	for k, v := range item.Meta {
		fmt.Fprintf(w, "meta.%s: %s\n", k, v)
	}
	fmt.Fprintf(w, "\n%s\n", item.Text)
	return nil
}

// --- push ---

func cmdPush(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	var (
		fs   = newFlagSet("push")
		to   = fs.String("to", "", "target session as <agent>:<name>")
		root = fs.String("root", "", "queue root directory")
		meta metaFlag
	)
	fs.Var(&meta, "meta", "metadata as key=value (repeatable)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\nusage: agentqueue push --to <agent>:<name> [--meta k=v]... [TEXT]", err)
	}

	target, err := resolveTarget(*to)
	if err != nil {
		return err
	}
	metaMap, err := parseMeta(meta)
	if err != nil {
		return err
	}

	rest := fs.Args()
	text := strings.Join(rest, " ")
	if len(rest) == 0 || text == "-" {
		body, err := io.ReadAll(stdin)
		if err != nil {
			return fmt.Errorf("read message from stdin: %w", err)
		}
		text = string(body)
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("message text is empty")
	}

	q, err := openQueue(*root)
	if err != nil {
		return err
	}
	item, err := q.PushAndNotify(ctx, target, text, metaMap)
	if item != nil {
		fmt.Fprintln(stdout, item.ID)
	}
	if err != nil {
		if errors.Is(err, agentqueue.ErrNotify) {
			// The message is stored; only the notice failed. Say so rather
			// than implying nothing was enqueued.
			return fmt.Errorf("message %s is queued but notifying %s failed: %w", item.ID, target, err)
		}
		return err
	}
	return nil
}

// --- wait ---

func cmdWait(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	var (
		fs      = newFlagSet("wait")
		to      = fs.String("to", "", "target session as <agent>:<name>")
		root    = fs.String("root", "", "queue root directory")
		timeout = fs.Int("timeout", 3600, "seconds to wait before giving up (0 = wait forever)")
		take    = fs.Bool("take", false, "claim the oldest pending message instead of only reporting arrivals")
		asJSON  = fs.Bool("json", false, "print JSON instead of a human summary")
	)
	if err := fs.Parse(args); err != nil {
		return exitUsage, fmt.Errorf("%w\nusage: agentqueue wait --to <agent>:<name> [--timeout 3600] [--take] [--json]", err)
	}

	target, err := resolveTarget(*to)
	if err != nil {
		return exitError, err
	}
	q, err := openQueue(*root)
	if err != nil {
		return exitError, err
	}

	var wait time.Duration
	if *timeout > 0 {
		wait = time.Duration(*timeout) * time.Second
	}
	items, err := q.Wait(ctx, target, wait)
	if err != nil {
		if errors.Is(err, agentqueue.ErrTimeout) {
			// A clean timeout gets its own exit code, not the generic failure.
			fmt.Fprintf(stderr, "timeout: no message for %s within %ds\n", target, *timeout)
			return exitTimeout, nil
		}
		return exitError, err
	}

	if *take {
		item, err := q.TakeNext(target)
		if err != nil {
			if errors.Is(err, agentqueue.ErrEmpty) {
				// Another consumer claimed it between Wait and TakeNext.
				return exitError, errors.New("a message arrived but another consumer claimed it first")
			}
			return exitError, err
		}
		return exitOK, printItem(stdout, item, *asJSON)
	}

	if *asJSON {
		return exitOK, printJSON(stdout, items)
	}
	// Without --take this output is the fetch path for a pull agent, so print
	// each body in full rather than only a summary line.
	fmt.Fprintf(stdout, "%d message(s) for %s:\n", len(items), target)
	for _, item := range items {
		fmt.Fprintf(stdout, "\n--- %s  %s\n", item.ID, item.CreatedAt.Format(time.RFC3339))
		for k, v := range item.Meta {
			fmt.Fprintf(stdout, "meta.%s: %s\n", k, v)
		}
		fmt.Fprintf(stdout, "%s\n", item.Text)
	}
	fmt.Fprintf(stdout, "\nClaim one with: %s\n", q.FetchCmd(target))
	return exitOK, nil
}

// --- list ---

func cmdList(args []string, stdout io.Writer) error {
	var (
		fs     = newFlagSet("list")
		to     = fs.String("to", "", "target session as <agent>:<name>")
		root   = fs.String("root", "", "queue root directory")
		state  = fs.String("state", "pending", "state to list: pending, claimed or done")
		asJSON = fs.Bool("json", false, "print JSON instead of a human summary")
	)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\nusage: agentqueue list --to <agent>:<name> [--state pending|claimed|done] [--json]", err)
	}

	target, err := resolveTarget(*to)
	if err != nil {
		return err
	}
	st, err := agentqueue.ParseState(*state)
	if err != nil {
		return err
	}
	q, err := openQueue(*root)
	if err != nil {
		return err
	}
	items, err := q.List(target, st)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(stdout, items)
	}
	if len(items) == 0 {
		fmt.Fprintf(stdout, "no %s message for %s\n", st, target)
		return nil
	}
	for _, item := range items {
		fmt.Fprintf(stdout, "%s  %s  %s\n", item.ID, item.CreatedAt.Format(time.RFC3339), firstLine(item.Text))
	}
	return nil
}

// --- take ---

func cmdTake(args []string, stdout io.Writer) error {
	var (
		fs     = newFlagSet("take")
		to     = fs.String("to", "", "target session as <agent>:<name>")
		root   = fs.String("root", "", "queue root directory")
		next   = fs.Bool("next", false, "claim the oldest pending message")
		asJSON = fs.Bool("json", false, "print JSON instead of a human summary")
	)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\nusage: agentqueue take --to <agent>:<name> [--next | ID] [--json]", err)
	}

	target, err := resolveTarget(*to)
	if err != nil {
		return err
	}
	rest := fs.Args()
	if *next && len(rest) > 0 {
		return errors.New("pass either --next or an ID, not both")
	}
	if !*next && len(rest) == 0 {
		return errors.New("pass --next or a message ID")
	}
	if len(rest) > 1 {
		return fmt.Errorf("take accepts at most one ID, got %d", len(rest))
	}
	q, err := openQueue(*root)
	if err != nil {
		return err
	}

	var item *agentqueue.Item
	if *next {
		item, err = q.TakeNext(target)
		if errors.Is(err, agentqueue.ErrEmpty) {
			return fmt.Errorf("no pending message for %s", target)
		}
	} else {
		item, err = q.Take(target, rest[0])
		if errors.Is(err, agentqueue.ErrNotFound) {
			return fmt.Errorf("no pending message %s for %s", rest[0], target)
		}
	}
	if err != nil {
		return err
	}
	return printItem(stdout, item, *asJSON)
}

// --- ack ---

func cmdAck(args []string, stdout io.Writer) error {
	var (
		fs   = newFlagSet("ack")
		to   = fs.String("to", "", "target session as <agent>:<name>")
		root = fs.String("root", "", "queue root directory")
	)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\nusage: agentqueue ack --to <agent>:<name> ID", err)
	}

	target, err := resolveTarget(*to)
	if err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return errors.New("ack takes exactly one message ID")
	}
	q, err := openQueue(*root)
	if err != nil {
		return err
	}
	if err := q.Ack(target, rest[0]); err != nil {
		if errors.Is(err, agentqueue.ErrNotFound) {
			return fmt.Errorf("no message %s for %s", rest[0], target)
		}
		return err
	}
	fmt.Fprintf(stdout, "acked %s\n", rest[0])
	return nil
}
