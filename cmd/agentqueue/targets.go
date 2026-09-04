package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ka2n/agentqueue"
)

// cmdTargets lists every mailbox under the queue root. It is a directory walk:
// a target appears here because something was pushed to it or a session
// registered it, not because that session is known to be alive.
func cmdTargets(args []string, stdout io.Writer) error {
	var (
		fs        = newFlagSet("targets")
		agentFlag = fs.String("agent", "", "restrict the listing to one agent")
		root      = fs.String("root", "", "queue root directory")
		asJSON    = fs.Bool("json", false, "print JSON instead of a table")
	)
	setFlagUsage(fs, stdout, args,
		"agentqueue targets [--agent AGENT] [--root DIR] [--json]",
		"List queue mailboxes and their pending, claimed and done counts. A mailbox or recorded address is storage metadata, not proof that a session is currently running.")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\nusage: agentqueue targets [--agent AGENT] [--root DIR] [--json]", err)
	}

	filter := ""
	if value := strings.TrimSpace(*agentFlag); value != "" {
		name, err := parseAgent(value)
		if err != nil {
			return err
		}
		filter = name.String()
	}

	q, err := openQueue(*root)
	if err != nil {
		return err
	}
	boxes, err := q.Mailboxes(filter)
	if err != nil {
		return err
	}
	if *asJSON {
		if boxes == nil {
			boxes = []agentqueue.Mailbox{}
		}
		return printJSON(stdout, boxes)
	}
	if len(boxes) == 0 {
		fmt.Fprintf(stdout, "no mailbox under %s\n", q.Root())
		return nil
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TARGET\tPENDING\tCLAIMED\tDONE\tREGISTERED\tCWD\tUPDATED")
	for _, mb := range boxes {
		registered, cwd, updated := "no", "-", "-"
		if mb.Address != nil {
			registered = "yes"
			if mb.Address.Cwd != "" {
				cwd = mb.Address.Cwd
			}
			if !mb.Address.UpdatedAt.IsZero() {
				updated = mb.Address.UpdatedAt.UTC().Format(time.RFC3339)
			}
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\t%s\t%s\n",
			mb.Target, mb.Pending, mb.Claimed, mb.Done, registered, cwd, updated)
	}
	return tw.Flush()
}
