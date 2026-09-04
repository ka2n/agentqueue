package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ka2n/agentqueue"
	"github.com/ka2n/crossagent/agent"
)

// readHookPayload decodes a hook payload from stdin when one is there.
//
// register is wired into a SessionStart hook, where stdin carries the payload,
// but it is also useful to run by hand from a session's own shell, where stdin
// is a terminal and reading it would hang. So a terminal or an empty stream
// yields an empty payload rather than blocking.
func readHookPayload(stdin io.Reader) (hookInput, error) {
	var in hookInput
	if isTTY(stdin) {
		return in, nil
	}
	body, err := io.ReadAll(stdin)
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

// addressFor builds the address to record for a session, preferring the hook
// payload over the environment for every field it carries.
func addressFor(t agentqueue.Target, in hookInput, getenv func(string) string, pid int) agentqueue.Address {
	cwd := strings.TrimSpace(in.Cwd)
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	return agentqueue.Address{
		Agent: t.Agent,
		Name:  t.Name,
		Cwd:   cwd,
		PID:   pid,
		// Recorded for diagnostics only. Writing to this socket from a
		// process that is not itself a Claude Code session is accepted and
		// then dropped, so it is not a delivery path. The token that goes
		// with it ($CLAUDE_CODE_MESSAGING_TOKEN) is a credential and is
		// deliberately never stored.
		Socket:       strings.TrimSpace(getenv("CLAUDE_CODE_MESSAGING_SOCKET")),
		AgentVersion: strings.TrimSpace(getenv("CLAUDE_CODE_VERSION")),
	}
}

// registerAddress records the session's address. It is called both by the
// register subcommand and by the SessionStart hook.
func registerAddress(q *agentqueue.Queue, t agentqueue.Target, in hookInput, getenv func(string) string) error {
	// In a hook the parent is the process that spawned it - the agent or the
	// shell it ran the hook through - which is the closest thing to the
	// session's own pid available without probing. It is diagnostic only.
	return q.PutAddress(addressFor(t, in, getenv, os.Getppid()))
}

// registerTarget derives the target to register: --to wins, then the payload's
// session_id, then $CLAUDE_CODE_SESSION_ID, under the given agent.
func registerTarget(to string, name agent.Name, sessionID string, getenv func(string) string) (agentqueue.Target, error) {
	if s := strings.TrimSpace(to); s != "" {
		target, err := agentqueue.ParseTarget(s)
		if err != nil {
			return agentqueue.Target{}, err
		}
		return checkTargetAgent(target)
	}
	session := strings.TrimSpace(sessionID)
	if session == "" {
		session = strings.TrimSpace(getenv("CLAUDE_CODE_SESSION_ID"))
	}
	if session == "" {
		return agentqueue.Target{}, errors.New("no --to, session_id on stdin or $CLAUDE_CODE_SESSION_ID")
	}
	return agentqueue.Target{Agent: name.String(), Name: session}, nil
}

// --- register ---

func cmdRegister(args []string, stdin io.Reader, stdout io.Writer) error {
	var (
		fs        = newFlagSet("register")
		to        = fs.String("to", "", "target session as <agent>:<name>")
		agentFlag = fs.String("agent", string(agent.Claude), "agent name to register under")
		root      = fs.String("root", "", "queue root directory")
		quiet     = fs.Bool("quiet", false, "print nothing on success")
		asJSON    = fs.Bool("json", false, "print the recorded address as JSON")
	)
	setFlagUsage(fs, stdout, args,
		"agentqueue register [--agent AGENT] [--to <agent>:<name>] [--root DIR] [--quiet] [--json]",
		"Record where a session can be reached. With no --to, read the session id and cwd from a hook payload on stdin or the session environment; --to overrides that payload. Re-registering updates the address. Use --quiet for hook-friendly output.")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\nusage: agentqueue register [--agent AGENT] [--to <agent>:<name>] [--root DIR] [--quiet] [--json]", err)
	}

	name, err := parseAgent(*agentFlag)
	if err != nil {
		return err
	}
	in, err := readHookPayload(stdin)
	if err != nil {
		return err
	}
	target, err := registerTarget(*to, name, in.SessionID, os.Getenv)
	if err != nil {
		// Nothing to register is not a failure: the hook this runs from must
		// stay silent when it cannot tell which session it is in.
		return nil
	}
	q, err := openQueue(*root)
	if err != nil {
		return err
	}
	addr := addressFor(target, in, os.Getenv, os.Getppid())
	if err := q.PutAddress(addr); err != nil {
		return err
	}
	if *asJSON {
		stored, err := q.Address(target)
		if err != nil {
			return err
		}
		return printJSON(stdout, stored)
	}
	if !*quiet {
		fmt.Fprintf(stdout, "registered %s\n", target)
	}
	return nil
}

// --- unregister ---

func cmdUnregister(args []string, stdin io.Reader, stdout io.Writer) error {
	var (
		fs        = newFlagSet("unregister")
		to        = fs.String("to", "", "target session as <agent>:<name>")
		agentFlag = fs.String("agent", string(agent.Claude), "agent name the session was registered under")
		root      = fs.String("root", "", "queue root directory")
		quiet     = fs.Bool("quiet", false, "print nothing on success")
	)
	setFlagUsage(fs, stdout, args,
		"agentqueue unregister [--agent AGENT] [--to <agent>:<name>] [--root DIR] [--quiet]",
		"Remove the recorded address for a session. With no --to, read the session id from a hook payload on stdin or the session environment. Removing a missing address succeeds; use --quiet for hook-friendly output.")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\nusage: agentqueue unregister [--agent AGENT] [--to <agent>:<name>] [--root DIR] [--quiet]", err)
	}

	name, err := parseAgent(*agentFlag)
	if err != nil {
		return err
	}
	in, err := readHookPayload(stdin)
	if err != nil {
		return err
	}
	target, err := registerTarget(*to, name, in.SessionID, os.Getenv)
	if err != nil {
		return nil
	}
	q, err := openQueue(*root)
	if err != nil {
		return err
	}
	if err := q.RemoveAddress(target); err != nil {
		return err
	}
	if !*quiet {
		fmt.Fprintf(stdout, "unregistered %s\n", target)
	}
	return nil
}
