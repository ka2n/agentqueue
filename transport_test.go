package agentqueue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestLookupRegisteredTransports(t *testing.T) {
	for _, agent := range []string{"claude", "codex", "pi"} {
		tr, ok := Lookup(agent)
		if !ok {
			t.Fatalf("Lookup(%q) not registered", agent)
		}
		if tr.Name() != agent {
			t.Fatalf("Lookup(%q).Name() = %q", agent, tr.Name())
		}
	}
	if _, ok := Lookup("nobody"); ok {
		t.Fatal("Lookup(\"nobody\") = ok, want not registered")
	}
}

func TestClaudeTransportNotifyIsNoop(t *testing.T) {
	tr := &claudeTransport{}
	notice := Notice{Target: Target{Agent: "claude", Name: "s"}, ItemID: "id", Pending: 1, FetchCmd: "agentqueue take"}
	if err := tr.Notify(context.Background(), notice); err != nil {
		t.Fatalf("Notify: %v", err)
	}
}

func TestPushAndNotifyPiLeavesItemForExtension(t *testing.T) {
	q, root := mustOpen(t)
	target := Target{Agent: "pi", Name: "session-1"}
	item, err := q.PushAndNotify(context.Background(), target, "body text", nil)
	if err != nil {
		t.Fatalf("PushAndNotify: %v", err)
	}
	if item == nil {
		t.Fatal("PushAndNotify returned a nil item")
	}
	if _, err := os.Stat(filepath.Join(root, "pi", "session-1", "pending", item.ID+".json")); err != nil {
		t.Fatalf("item not pending: %v", err)
	}
}

func TestCodexTransportNotifyBuildsArgv(t *testing.T) {
	var gotName string
	var gotArgs []string
	tr := &codexTransport{run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotName, gotArgs = name, args
		return nil, nil
	}}

	notice := Notice{
		Target:   Target{Agent: "codex", Name: "01a0-thread"},
		ItemID:   "1700000000000-abcdef123456",
		Pending:  3,
		FetchCmd: "agentqueue take --to codex:01a0-thread --next",
	}
	if err := tr.Notify(context.Background(), notice); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if gotName != "codex" {
		t.Fatalf("command = %q, want %q", gotName, "codex")
	}
	if len(gotArgs) != 5 {
		t.Fatalf("args = %v, want 5 elements", gotArgs)
	}
	wantPrefix := []string{"queue", "--thread", "01a0-thread", "--message"}
	if diff := cmp.Diff(wantPrefix, gotArgs[:4]); diff != "" {
		t.Fatalf("argv prefix mismatch (-want +got):\n%s", diff)
	}

	msg := gotArgs[4]
	if strings.Contains(msg, "\n") {
		t.Fatalf("message is not a single line: %q", msg)
	}
	for _, want := range []string{notice.ItemID, notice.FetchCmd, notice.Target.String(), "3 pending"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q does not contain %q", msg, want)
		}
	}
}

func TestCodexTransportNotifyOmitsItemBody(t *testing.T) {
	const secret = "this body must never travel on the notification path"
	var msg string
	tr := &codexTransport{run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		msg = args[len(args)-1]
		return nil, nil
	}}
	notice := Notice{Target: Target{Agent: "codex", Name: "s"}, ItemID: "id-1", Pending: 1, FetchCmd: "agentqueue take --to codex:s --next"}
	if err := tr.Notify(context.Background(), notice); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if strings.Contains(msg, secret) {
		t.Fatalf("message leaked the item body: %q", msg)
	}
}

func TestCodexTransportNotifyReportsExitError(t *testing.T) {
	tr := &codexTransport{run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("codex: no such thread"), errors.New("exit status 1")
	}}
	err := tr.Notify(context.Background(), Notice{Target: Target{Agent: "codex", Name: "s"}, ItemID: "id"})
	if err == nil {
		t.Fatal("Notify = nil, want error")
	}
	if !strings.Contains(err.Error(), "no such thread") {
		t.Fatalf("error %q does not include the captured output", err)
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("error %q does not wrap the exec error", err)
	}
}

func TestCodexTransportNotifyTruncatesOutput(t *testing.T) {
	long := strings.Repeat("x", 2000)
	tr := &codexTransport{run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte(long), errors.New("boom")
	}}
	err := tr.Notify(context.Background(), Notice{Target: Target{Agent: "codex", Name: "s"}, ItemID: "id"})
	if err == nil {
		t.Fatal("Notify = nil, want error")
	}
	if len(err.Error()) > 800 {
		t.Fatalf("error message is %d bytes, want the output truncated", len(err.Error()))
	}
}

// stubTransport records Notify calls for PushAndNotify tests.
type stubTransport struct {
	name    string
	notices []Notice
	err     error
}

func (s *stubTransport) Name() string { return s.name }

func (s *stubTransport) Notify(_ context.Context, n Notice) error {
	s.notices = append(s.notices, n)
	return s.err
}

// registerForTest registers tr and restores the previous registration, if any,
// when the test ends.
func registerForTest(t *testing.T, tr Transport) {
	t.Helper()
	prev, had := Lookup(tr.Name())
	Register(tr)
	t.Cleanup(func() {
		registryMu.Lock()
		if had {
			registry[tr.Name()] = prev
		} else {
			delete(registry, tr.Name())
		}
		registryMu.Unlock()
	})
}

func TestPushAndNotify(t *testing.T) {
	t.Run("registered custom transport notifies once", func(t *testing.T) {
		q, root := mustOpen(t)
		stub := &stubTransport{name: "custom"}
		registerForTest(t, stub)
		target := Target{Agent: "custom", Name: "s"}

		item, err := q.PushAndNotify(context.Background(), target, "body text", map[string]string{"k": "v"})
		if err != nil {
			t.Fatalf("PushAndNotify: %v", err)
		}
		if len(stub.notices) != 1 {
			t.Fatalf("Notify called %d times, want 1", len(stub.notices))
		}
		want := Notice{
			Target:   target,
			ItemID:   item.ID,
			Pending:  1,
			FetchCmd: fmt.Sprintf("agentqueue take --to %s --next", target),
		}
		if diff := cmp.Diff(want, stub.notices[0]); diff != "" {
			t.Fatalf("notice mismatch (-want +got):\n%s", diff)
		}
		if _, err := os.Stat(filepath.Join(root, "custom", "s", "pending", item.ID+".json")); err != nil {
			t.Fatalf("item not pending: %v", err)
		}
	})

	t.Run("notify failure keeps the item", func(t *testing.T) {
		q, root := mustOpen(t)
		stub := &stubTransport{name: "stubfail", err: errors.New("transport down")}
		registerForTest(t, stub)
		target := Target{Agent: "stubfail", Name: "s"}

		item, err := q.PushAndNotify(context.Background(), target, "body text", nil)
		if err == nil {
			t.Fatal("PushAndNotify = nil error, want ErrNotify")
		}
		if !errors.Is(err, ErrNotify) {
			t.Fatalf("error = %v, want ErrNotify", err)
		}
		if item == nil {
			t.Fatal("PushAndNotify returned a nil item; the message would be lost")
		}
		if _, err := os.Stat(filepath.Join(root, "stubfail", "s", "pending", item.ID+".json")); err != nil {
			t.Fatalf("item not left pending: %v", err)
		}
	})

	t.Run("unknown agent", func(t *testing.T) {
		q, _ := mustOpen(t)
		item, err := q.PushAndNotify(context.Background(), Target{Agent: "ghost", Name: "s"}, "x", nil)
		if err == nil {
			t.Fatal("PushAndNotify = nil error, want unknown agent error")
		}
		if item != nil {
			t.Fatalf("item = %+v, want nil for an unknown agent", item)
		}
		if !strings.Contains(err.Error(), "ghost") {
			t.Fatalf("error %q does not name the unknown agent", err)
		}
		if !strings.Contains(err.Error(), "no transport registered") {
			t.Fatalf("error %q does not describe the missing transport", err)
		}
		if !strings.Contains(err.Error(), "agentqueue.Register") {
			t.Fatalf("error %q does not name the registration extension point", err)
		}
	})
}

func TestPushAndNotifyUsesConfiguredFetchCmd(t *testing.T) {
	var msg string
	tr := &codexTransport{run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		msg = args[len(args)-1]
		return nil, nil
	}}
	// Register under the real "codex" name so PushAndNotify picks up the stub.
	registerForTest(t, tr)

	q, _ := mustOpen(t, WithFetchCmd(func(target Target) string {
		return "myctl fetch " + target.String()
	}))
	target := Target{Agent: "codex", Name: "thread-9"}
	item, err := q.PushAndNotify(context.Background(), target, "the body", nil)
	if err != nil {
		t.Fatalf("PushAndNotify: %v", err)
	}
	if !strings.Contains(msg, "myctl fetch codex:thread-9") {
		t.Fatalf("notice %q does not carry the configured fetch command", msg)
	}
	if strings.Contains(msg, "the body") {
		t.Fatalf("notice %q leaked the item body", msg)
	}
	if !strings.Contains(msg, item.ID) {
		t.Fatalf("notice %q does not name item %q", msg, item.ID)
	}
}
