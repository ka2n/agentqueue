package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ka2n/agentqueue/hookserve"
	"github.com/ka2n/crossagent/agent"
)

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
		return hookserve.SelfCheck(stdout), nil
	}

	var (
		fs      = newFlagSet("hook claude")
		to      = fs.String("to", "", "target session as <agent>:<name>, overriding the payload's session_id")
		root    = fs.String("root", "", "queue root directory")
		logPath = fs.String("log", "", "append diagnostics to this file")
		max     = fs.Int("max", hookserve.DefaultMax, "deliver at most this many messages per hook run")
		noBlock = fs.Bool("no-block", false, "never emit decision: block on Stop, only additionalContext")
	)
	setFlagUsage(fs, stdout, args,
		"agentqueue hook claude [--to <agent>:<name>] [--root DIR] [--max N] [--log FILE] [--no-block]",
		"Serve Claude Code's synchronous hook protocol. Read the JSON hook payload from stdin, claim pending messages for the session, and print the injection JSON to stdout. The hook always exits 0, including internal failures, so it cannot disrupt a session; diagnostics go to stderr and optionally --log. Keep it synchronous because stdout is the delivery protocol.")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitUsage, fmt.Errorf("%w\nusage: agentqueue hook claude [--to <agent>:<name>] [--root DIR] [--max N] [--log FILE] [--no-block]", err)
		}
		// A misconfigured hook must not break the session either.
		fmt.Fprintf(stderr, "agentqueue hook: %v\n", err)
		return exitOK, nil
	}

	log := newHookLogger(*logPath, stderr)
	defer log.Close()

	payload, err := hookserve.ParsePayload(stdin)
	if err != nil {
		log.logf("%v", err)
		return exitOK, nil
	}

	q, err := openQueue(*root)
	if err != nil {
		log.logf("%v", err)
		return exitOK, nil
	}

	out, _, err := hookserve.HandleClaude(q, payload, hookserve.Options{
		To:      *to,
		Max:     *max,
		NoBlock: *noBlock,
		Getenv:  os.Getenv,
		Logf:    log.logf,
	})
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

// cmdHook dispatches "hook <agent>". Only claude has hooks; every other agent
// either needs no setup or has no integration yet.
func cmdHook(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 {
		return exitUsage, errors.New("usage: agentqueue hook claude [flags]")
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		return cmdHookClaude([]string{"-h"}, stdin, stdout, stderr)
	}
	switch args[0] {
	case agent.Claude.String():
		return cmdHookClaude(args[1:], stdin, stdout, stderr)
	default:
		return exitUsage, fmt.Errorf("no hook integration for agent %q; only %s has one", args[0], agent.Claude)
	}
}
