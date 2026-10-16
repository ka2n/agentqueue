package hookserve

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ka2n/agentqueue"
	"github.com/ka2n/crossagent/agent"
	crosssessions "github.com/ka2n/crossagent/sessions"
)

func TestHookRefreshAndHistoricalListing(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "topic")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "first")
	head := git("rev-parse", "HEAD")
	q := openQueue(t)
	target := agentqueue.Target{Agent: "pi", Name: "snapshot"}
	payload := Payload{SessionID: target.Name, Cwd: repo, HookEventName: eventSessionStart}
	getenv := func(string) string { return "" }
	opts := Options{To: target.String(), Getenv: getenv}
	if _, delivered, err := HandleClaude(q, payload, opts); err != nil || delivered {
		t.Fatalf("empty hook: %v %v", delivered, err)
	}
	discovery := crosssessions.SessionListerOptions{HomeDir: t.TempDir(), PiSessionDir: t.TempDir()}
	list := func() agentqueue.Session {
		t.Helper()
		rows, err := q.Sessions(context.Background(), agentqueue.SessionsOptions{Agent: agent.Pi, Discovery: discovery})
		if err != nil || len(rows) != 1 {
			t.Fatalf("list: %+v %v", rows, err)
		}
		return rows[0]
	}
	first := list()
	if first.Context == nil || first.Context.Git == nil || first.Context.Git.HEAD != head || !first.LastActivity.IsZero() || first.Source != "queue-address" {
		t.Fatalf("first: %+v", first)
	}
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "second")
	newer := git("rev-parse", "HEAD")
	// Removing executable discovery prevents a hidden Git/Herdr probe from succeeding.
	originalPath := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	if row := list(); row.Context.Git.HEAD != head {
		t.Fatal("listing refreshed historical snapshot")
	}
	t.Setenv("PATH", originalPath)
	payload.HookEventName = eventUserPromptSubmit
	if _, _, err := HandleClaude(q, payload, opts); err != nil {
		t.Fatal(err)
	}
	if row := list(); row.Context.Git.HEAD != newer {
		t.Fatal("next hook did not refresh HEAD")
	}

}

func TestEmptyMailboxRefreshBoundaries(t *testing.T) {
	q := openQueue(t)
	target := agentqueue.Target{Agent: "claude", Name: "root"}
	for _, event := range []string{eventSessionStart, eventUserPromptSubmit, eventStop} {
		cwd := filepath.Join(t.TempDir(), event)
		payload := Payload{SessionID: target.Name, Cwd: cwd, HookEventName: event}
		if _, _, err := HandleClaude(q, payload, Options{Getenv: func(string) string { return "" }}); err != nil {
			t.Fatal(err)
		}
		addr, err := q.Address(target)
		if err != nil || addr.Cwd != cwd || addr.Context == nil || time.Since(addr.Context.ObservedAt) > time.Second {
			t.Fatalf("%s: %+v %v", event, addr, err)
		}
	}
	addr, _ := q.Address(target)
	if _, _, err := HandleClaude(q, Payload{SessionID: target.Name, Cwd: "/child-unknown", HookEventName: eventSubagentStop}, Options{}); err != nil {
		t.Fatal(err)
	}
	after, _ := q.Address(target)
	if after.Cwd != addr.Cwd {
		t.Fatal("subagent event overwrote parent context")
	}
}
