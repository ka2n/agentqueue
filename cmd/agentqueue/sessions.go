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

// sessionRow is the CLI view of a crossagent session. Mailbox and Registered
// are queue facts, not session-discovery facts, so they are joined here from
// agentqueue's mailbox directories and addr.json records.
type sessionRow struct {
	Agent        agent.Name `json:"agent"`
	SessionID    string     `json:"session_id"`
	Cwd          string     `json:"cwd"`
	Label        string     `json:"label,omitempty"`
	LastActivity time.Time  `json:"last_activity"`
	Source       string     `json:"source"`
	State        string     `json:"state"`
	Mailbox      bool       `json:"mailbox"`
	Registered   bool       `json:"registered"`
}

func cmdSessions(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("sessions")
	agentFlag := fs.String("agent", "", "restrict results to claude, codex or pi")
	cwd := fs.String("cwd", "", "restrict results to this working directory")
	asJSON := fs.Bool("json", false, "print JSON instead of a table")
	limit := fs.Int("limit", 0, "show at most N sessions (zero means all)")
	maxAge := fs.Duration("max-age", crosssessions.DefaultClaudeMaxAge, "skip Claude transcript files older than this duration")
	setFlagUsage(fs, stdout, args,
		"agentqueue sessions [--agent claude|codex|pi] [--cwd PATH] [--limit N] [--max-age DURATION] [--json]",
		"List session locations discovered by crossagent and join them with agentqueue mailbox/address state, including SOURCE and STATE. This reports location metadata, not process liveness; no process probing is performed and /proc is never read.")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\nusage: agentqueue sessions [--agent claude|codex|pi] [--cwd PATH] [--limit N] [--max-age DURATION] [--json]", err)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q\nusage: agentqueue sessions [--agent claude|codex|pi] [--cwd PATH] [--limit N] [--max-age DURATION] [--json]", fs.Arg(0))
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

	root, err := resolveRoot("", os.Getenv)
	if err != nil {
		return err
	}
	q, err := agentqueue.Open(root)
	if err != nil {
		return err
	}
	boxes, err := q.Mailboxes("")
	if err != nil {
		return fmt.Errorf("read queue status: %w", err)
	}
	status := make(map[string]queueSessionStatus, len(boxes))
	for _, box := range boxes {
		status[sessionStatusKey(box.Target.Agent, box.Target.Name)] = queueSessionStatus{
			mailbox:    true,
			registered: box.Address != nil,
		}
	}

	options := crosssessions.SessionListerOptions{
		Resolver:     paths.DefaultResolver(),
		ClaudeMaxAge: *maxAge,
	}
	listers := crosssessions.NewSessionListers(options)
	names := agent.Names()
	if value := strings.TrimSpace(*agentFlag); value != "" {
		name, err := parseAgent(value)
		if err != nil {
			return err
		}
		names = []agent.Name{name}
	}

	discovered := make([]crosssessions.Session, 0)
	for _, name := range names {
		found, err := listers[name].List(ctx)
		if err != nil {
			return fmt.Errorf("list %s sessions: %w", name, err)
		}
		for _, session := range found {
			if cwdFilter != "" && filepath.Clean(session.Cwd) != cwdFilter {
				continue
			}
			discovered = append(discovered, session)
		}
	}
	crosssessions.SortSessions(discovered)
	if *limit > 0 && len(discovered) > *limit {
		discovered = discovered[:*limit]
	}

	rows := make([]sessionRow, 0, len(discovered))
	for _, session := range discovered {
		state := status[sessionStatusKey(session.Agent.String(), session.SessionID)]
		rows = append(rows, sessionRow{
			Agent:        session.Agent,
			SessionID:    session.SessionID,
			Cwd:          session.Cwd,
			Label:        session.Label,
			LastActivity: session.LastActivity,
			Source:       session.Source,
			State:        session.State,
			Mailbox:      state.mailbox,
			Registered:   state.registered,
		})
	}
	if *asJSON {
		return printJSON(stdout, rows)
	}
	printSessionTable(stdout, rows)
	return nil
}

type queueSessionStatus struct {
	mailbox    bool
	registered bool
}

func sessionStatusKey(agent, name string) string { return agent + "\x00" + name }

func printSessionTable(stdout io.Writer, sessions []sessionRow) {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tSESSION ID\tCWD\tLABEL\tLAST ACTIVITY\tSOURCE\tSTATE\tMAILBOX\tREGISTERED")
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			session.Agent,
			session.SessionID,
			session.Cwd,
			label,
			lastActivity,
			session.Source,
			state,
			yesNo(session.Mailbox),
			yesNo(session.Registered),
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
