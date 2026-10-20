package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ka2n/agentqueue/hookserve"
)

func cmdHookCodex(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(args) == 1 && args[0] == "--self-check" {
		fmt.Fprintln(stdout, hookserve.CodexSelfCheckToken)
		return exitOK, nil
	}
	fs := newFlagSet("hook codex")
	to := fs.String("to", "", "target session as codex:<name>, overriding session_id")
	root := fs.String("root", "", "queue root directory")
	logPath := fs.String("log", "", "append diagnostics to this file")
	max := fs.Int("max", hookserve.DefaultMax, "deliver at most this many messages per hook run")
	setFlagUsage(fs, stdout, args,
		"agentqueue hook codex [--to codex:<name>] [--root DIR] [--max N] [--log FILE]",
		"Serve Codex's synchronous PostToolUse and UserPromptSubmit hooks. Claim queued messages and return additionalContext JSON. Internal failures are logged without disrupting the session.")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitUsage, err
		}
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
	out, _, err := hookserve.HandleCodex(q, payload, hookserve.Options{
		To: *to, Max: *max, Getenv: os.Getenv, Logf: log.logf,
	})
	if err != nil {
		log.logf("%v", err)
		return exitOK, nil
	}
	if out != nil {
		if err := json.NewEncoder(stdout).Encode(out); err != nil {
			log.logf("encode hook output: %v", err)
		}
	}
	return exitOK, nil
}
