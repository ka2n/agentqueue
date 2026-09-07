package main

import (
	"fmt"
	"io"
	"os"

	"github.com/ka2n/agentqueue/hookserve"
	"github.com/ka2n/crossagent/agent"
)

// readHookPayload decodes a hook payload from stdin when one is there.
//
// register is wired into a SessionStart hook, where stdin carries the payload,
// but it is also useful to run by hand from a session's own shell, where stdin
// is a terminal and reading it would hang. So a terminal or an empty stream
// yields an empty payload rather than blocking.
func readHookPayload(stdin io.Reader) (hookserve.Payload, error) {
	if isTTY(stdin) {
		return hookserve.Payload{}, nil
	}
	return hookserve.ParsePayload(stdin)
}

// --- register ---

func cmdRegister(args []string, stdin io.Reader, stdout io.Writer) error {
	var (
		fs        = newFlagSet("register")
		to        = fs.String("to", "", "target session as <agent>:<name>")
		agentFlag = fs.String("agent", string(agent.Claude), "mailbox agent name to register under")
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

	in, err := readHookPayload(stdin)
	if err != nil {
		return err
	}
	target, err := hookserve.RegisterTarget(*to, *agentFlag, in.SessionID, os.Getenv)
	if err != nil {
		// Nothing to register is not a failure: the hook this runs from must
		// stay silent when it cannot tell which session it is in.
		return nil
	}
	q, err := openQueue(*root)
	if err != nil {
		return err
	}
	addr := hookserve.AddressFor(target, in, os.Getenv, os.Getppid())
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
		agentFlag = fs.String("agent", string(agent.Claude), "mailbox agent name the session was registered under")
		root      = fs.String("root", "", "queue root directory")
		quiet     = fs.Bool("quiet", false, "print nothing on success")
	)
	setFlagUsage(fs, stdout, args,
		"agentqueue unregister [--agent AGENT] [--to <agent>:<name>] [--root DIR] [--quiet]",
		"Remove the recorded address for a session. With no --to, read the session id from a hook payload on stdin or the session environment. Removing a missing address succeeds; use --quiet for hook-friendly output.")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\nusage: agentqueue unregister [--agent AGENT] [--to <agent>:<name>] [--root DIR] [--quiet]", err)
	}

	in, err := readHookPayload(stdin)
	if err != nil {
		return err
	}
	target, err := hookserve.RegisterTarget(*to, *agentFlag, in.SessionID, os.Getenv)
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
