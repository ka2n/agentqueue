package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ka2n/agentqueue"
)

func cmdSessions(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("sessions")
	agent := fs.String("agent", "", "restrict results to claude, codex or pi")
	cwd := fs.String("cwd", "", "restrict results to this working directory")
	asJSON := fs.Bool("json", false, "print JSON instead of a table")
	limit := fs.Int("limit", 0, "show at most N sessions (zero means all)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\nusage: agentqueue sessions [--agent claude|codex|pi] [--cwd PATH] [--json] [--limit N]", err)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q\nusage: agentqueue sessions [--agent claude|codex|pi] [--cwd PATH] [--json] [--limit N]", fs.Arg(0))
	}
	if *limit < 0 {
		return fmt.Errorf("--limit must not be negative")
	}

	var cwdFilter string
	if value := strings.TrimSpace(*cwd); value != "" {
		var err error
		cwdFilter, err = filepath.Abs(value)
		if err != nil {
			return fmt.Errorf("resolve --cwd %q: %w", value, err)
		}
		cwdFilter = filepath.Clean(cwdFilter)
	}
	root, err := resolveRoot("", os.Getenv)
	if err != nil {
		return err
	}
	listers := agentqueue.NewSessionListers(agentqueue.SessionListerOptions{QueueRoot: root})
	names := []string{"claude", "codex", "pi"}
	if value := strings.TrimSpace(*agent); value != "" {
		if _, ok := listers[value]; !ok {
			return fmt.Errorf("unknown agent %q: want claude, codex or pi", value)
		}
		names = []string{value}
	}

	sessions := make([]agentqueue.Session, 0)
	for _, name := range names {
		found, err := listers[name].List(ctx)
		if err != nil {
			return fmt.Errorf("list %s sessions: %w", name, err)
		}
		for _, session := range found {
			if cwdFilter != "" && filepath.Clean(session.Cwd) != cwdFilter {
				continue
			}
			sessions = append(sessions, session)
		}
	}
	agentqueue.SortSessions(sessions)
	if *limit > 0 && len(sessions) > *limit {
		sessions = sessions[:*limit]
	}
	if *asJSON {
		return printJSON(stdout, sessions)
	}
	printSessionTable(stdout, sessions)
	return nil
}

func printSessionTable(stdout io.Writer, sessions []agentqueue.Session) {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tSESSION ID\tCWD\tLABEL\tLAST ACTIVITY\tMAILBOX\tREGISTERED")
	for _, session := range sessions {
		lastActivity := "-"
		if !session.LastActivity.IsZero() {
			lastActivity = session.LastActivity.UTC().Format(time.RFC3339Nano)
		}
		label := session.Label
		if label == "" {
			label = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			session.Agent,
			session.SessionID,
			session.Cwd,
			label,
			lastActivity,
			yesNo(session.Mailbox),
			yesNo(session.Registered),
		)
	}
	if len(sessions) == 0 {
		fmt.Fprintln(tw, "(none)\t\t\t\t\t\t")
	}
	_ = tw.Flush()
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
