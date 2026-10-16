package agentqueue

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ka2n/crossagent/agent"
	"github.com/ka2n/crossagent/sessioncontext"
	crosssessions "github.com/ka2n/crossagent/sessions"
)

func TestSessionsJoinBeforeFilteringAndLimit(t *testing.T) {
	q, _ := mustOpen(t)
	sessionsDir := t.TempDir()
	fixture := `{"type":"session","version":3,"id":"discovered","timestamp":"2026-01-01T00:00:00Z","cwd":"/old"}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionsDir, "2026-01-01T00-00-00-000Z_discovered.jsonl"), []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(sessionsDir, "2026-01-01T00-00-00-000Z_discovered.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, addr := range []Address{
		{Agent: "pi", Name: "discovered", Cwd: "/new", Context: &sessioncontext.Snapshot{Cwd: "/new", ObservedAt: now.Add(-time.Hour)}},
		{Agent: "pi", Name: "fresh", Cwd: "/new", Context: &sessioncontext.Snapshot{Cwd: "/new", ObservedAt: now}},
		{Agent: "pi", Name: "legacy", Cwd: "/legacy"},
		{Agent: "custom", Name: "hidden", Cwd: "/new"},
	} {
		if err := q.PutAddress(addr); err != nil {
			t.Fatal(err)
		}
	}
	opts := SessionsOptions{Agent: agent.Pi, Cwd: "/new", Limit: 1, Discovery: crosssessions.SessionListerOptions{HomeDir: t.TempDir(), PiSessionDir: sessionsDir}}
	rows, err := q.Sessions(context.Background(), opts)
	if err != nil || len(rows) != 1 || rows[0].SessionID != "fresh" || !rows[0].LastActivity.IsZero() {
		t.Fatalf("limited: %+v %v", rows, err)
	}
	opts.Limit = 0
	rows, err = q.Sessions(context.Background(), opts)
	if err != nil || len(rows) != 2 {
		t.Fatalf("filtered: %+v %v", rows, err)
	}
	for _, row := range rows {
		if row.SessionID == "discovered" && (row.Source != crosssessions.SourcePiSession || row.Cwd != "/new" || row.LastActivity.IsZero()) {
			t.Fatalf("discovered: %+v", row)
		}
	}
	opts.Cwd = "/legacy"
	rows, err = q.Sessions(context.Background(), opts)
	if err != nil || len(rows) != 1 || rows[0].Context != nil {
		t.Fatalf("legacy: %+v %v", rows, err)
	}
}
