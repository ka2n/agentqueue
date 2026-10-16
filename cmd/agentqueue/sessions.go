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
	"github.com/ka2n/crossagent/agent"
	"github.com/ka2n/crossagent/paths"
	crosssessions "github.com/ka2n/crossagent/sessions"
)

type sessionRow = agentqueue.Session

func cmdSessions(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("sessions")
	rootFlag := fs.String("root", "", "queue root directory")
	agentFlag := fs.String("agent", "", "restrict results to claude, codex or pi")
	cwd := fs.String("cwd", "", "restrict results to this working directory")
	asJSON := fs.Bool("json", false, "print JSON instead of a table")
	limit := fs.Int("limit", 0, "show at most N sessions (zero means all)")
	maxAge := fs.Duration("max-age", crosssessions.DefaultClaudeMaxAge, "skip Claude transcript files older than this duration")
	setFlagUsage(fs, stdout, args,
		"agentqueue sessions [--root DIR] [--agent claude|codex|pi] [--cwd PATH] [--limit N] [--max-age DURATION] [--json]",
		"List session locations discovered by crossagent and join them with agentqueue mailbox/address state, including SOURCE and STATE. This reports location metadata, not process liveness; no process probing is performed and /proc is never read.")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\nusage: agentqueue sessions [--root DIR] [--agent claude|codex|pi] [--cwd PATH] [--limit N] [--max-age DURATION] [--json]", err)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q\nusage: agentqueue sessions [--root DIR] [--agent claude|codex|pi] [--cwd PATH] [--limit N] [--max-age DURATION] [--json]", fs.Arg(0))
	}
	if *limit < 0 {
		return fmt.Errorf("--limit must not be negative")
	}
	if *maxAge <= 0 {
		return fmt.Errorf("--max-age must be positive")
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

	root, err := resolveRoot(*rootFlag, os.Getenv)
	if err != nil {
		return err
	}
	q, err := agentqueue.Open(root)
	if err != nil {
		return err
	}
	var name agent.Name
	if value := strings.TrimSpace(*agentFlag); value != "" {
		name, err = parseAgent(value)
		if err != nil {
			return err
		}
	}
	rows, err := q.Sessions(ctx, agentqueue.SessionsOptions{Agent: name, Cwd: cwdFilter, Limit: *limit, Discovery: crosssessions.SessionListerOptions{Resolver: paths.DefaultResolver(), ClaudeMaxAge: *maxAge}})
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(stdout, rows)
	}
	printSessionTable(stdout, rows)
	return nil
}

func printSessionTable(stdout io.Writer, sessions []sessionRow) {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tSESSION ID\tCWD\tLABEL\tLAST ACTIVITY\tSOURCE\tSTATE\tMAILBOX\tREGISTERED\tWORKTREE\tBRANCH\tHEAD\tTERMINAL TITLE\tOBSERVED AT")
	for _, session := range sessions {
		lastActivity := "-"
		if !session.LastActivity.IsZero() {
			lastActivity = session.LastActivity.UTC().Format(time.RFC3339Nano)
		}
		label := session.Label
		if label == "" {
			label = "-"
		}
		state := session.State
		if state == "" {
			state = "-"
		}
		worktree, branch, head, title, observed := "-", "-", "-", "-", "-"
		if snapshot := session.Context; snapshot != nil {
			if snapshot.Git != nil {
				worktree, branch, head = snapshot.Git.WorktreeRoot, snapshot.Git.Branch, snapshot.Git.HEAD
				if len(head) > 12 {
					head = head[:12]
				}
			}
			if snapshot.Terminal != nil {
				title = snapshot.Terminal.Title
			}
			if !snapshot.ObservedAt.IsZero() {
				observed = snapshot.ObservedAt.UTC().Format(time.RFC3339Nano)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			session.Agent,
			session.SessionID,
			session.Cwd,
			label,
			lastActivity,
			session.Source,
			state,
			yesNo(session.Mailbox),
			yesNo(session.Registered), worktree, branch, head, title, observed,
		)
	}
	if len(sessions) == 0 {
		fmt.Fprintln(tw, "(none)\t\t\t\t\t\t\t\t")
	}
	_ = tw.Flush()
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
