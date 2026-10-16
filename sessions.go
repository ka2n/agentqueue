package agentqueue

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/ka2n/crossagent/agent"
	"github.com/ka2n/crossagent/sessioncontext"
	crosssessions "github.com/ka2n/crossagent/sessions"
)

// Session joins discovery metadata with queue registration facts.
type Session struct {
	crosssessions.Session
	Mailbox    bool                     `json:"mailbox"`
	Registered bool                     `json:"registered"`
	Context    *sessioncontext.Snapshot `json:"context,omitempty"`
}

// SessionsOptions filters the combined discovery and address records.
type SessionsOptions struct {
	Agent     agent.Name
	Cwd       string
	Limit     int
	Discovery crosssessions.SessionListerOptions
}

// Sessions reads stored snapshots without probing Git or terminal state. A
// snapshot may replace discovery's cwd only when its observation is at least as
// recent as LastActivity. Sorting also uses snapshot observation time (or address
// update time for address-only records); LastActivity itself remains unchanged.
// Address-only records report no activity or liveness.
func (q *Queue) Sessions(ctx context.Context, opts SessionsOptions) ([]Session, error) {
	if opts.Agent != "" && !opts.Agent.Valid() {
		return nil, fmt.Errorf("invalid agent %q", opts.Agent)
	}
	if opts.Limit < 0 {
		return nil, fmt.Errorf("limit must not be negative")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	discovery := opts.Discovery
	discovery.Limit = 0
	listers := crosssessions.NewSessionListers(discovery)
	names := agent.Names()
	if opts.Agent != "" {
		names = []agent.Name{opts.Agent}
	}
	rows := make(map[string]Session)
	sortTimes := make(map[string]time.Time)
	key := func(a, id string) string { return a + "\x00" + id }
	for _, name := range names {
		found, err := listers[name].List(ctx)
		if err != nil {
			return nil, fmt.Errorf("list %s sessions: %w", name, err)
		}
		for _, session := range found {
			rows[key(string(session.Agent), session.SessionID)] = Session{Session: session}
		}
	}
	boxes, err := q.Mailboxes(string(opts.Agent))
	if err != nil {
		return nil, fmt.Errorf("read queue status: %w", err)
	}
	for _, box := range boxes {
		name := agent.Name(box.Target.Agent)
		if !name.Valid() {
			continue
		}
		k := key(string(name), box.Target.Name)
		row, exists := rows[k]
		if !exists {
			if box.Address == nil {
				continue
			}
			row.Session = crosssessions.Session{Agent: name, SessionID: box.Target.Name, Cwd: box.Address.Cwd, Source: "queue-address"}
		}
		row.Mailbox = true
		row.Registered = box.Address != nil
		if box.Address != nil {
			row.Context = box.Address.Context
			if row.Context != nil && !row.Context.ObservedAt.IsZero() && !row.Context.ObservedAt.Before(row.LastActivity) {
				row.Cwd = box.Address.Cwd
			}
		}
		rows[k] = row
		observed := row.LastActivity
		if row.Context != nil && row.Context.ObservedAt.After(observed) {
			observed = row.Context.ObservedAt
		}
		if !exists && box.Address != nil && box.Address.UpdatedAt.After(observed) {
			observed = box.Address.UpdatedAt
		}
		sortTimes[k] = observed
	}
	result := make([]Session, 0, len(rows))
	for _, row := range rows {
		if opts.Cwd != "" && filepath.Clean(row.Cwd) != filepath.Clean(opts.Cwd) {
			continue
		}
		result = append(result, row)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		at, bt := a.LastActivity, b.LastActivity
		if updated := sortTimes[key(string(a.Agent), a.SessionID)]; updated.After(at) {
			at = updated
		}
		if updated := sortTimes[key(string(b.Agent), b.SessionID)]; updated.After(bt) {
			bt = updated
		}
		if !at.Equal(bt) {
			return at.After(bt)
		}
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		return a.SessionID < b.SessionID
	})
	if opts.Limit > 0 && len(result) > opts.Limit {
		result = result[:opts.Limit]
	}
	return result, nil
}
