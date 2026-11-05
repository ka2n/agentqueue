package agentqueue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
		Item:     &Item{Text: "first line\nsecond line", Meta: map[string]string{"task_id": "task"}},
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
	if strings.Contains(msg, notice.FetchCmd) || strings.Contains(msg, "pending)") {
		t.Fatalf("obsolete fetch notice: %s", msg)
	}
	for _, want := range []string{notice.ItemID, "first line\nsecond line", notice.Target.String(), "task_id"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q does not contain %q", msg, want)
		}
	}
}

func TestCodexTransportNotifyReportsExitError(t *testing.T) {
	tr := &codexTransport{run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("codex: no such thread"), errors.New("exit status 1")
	}}
	err := tr.Notify(context.Background(), Notice{Target: Target{Agent: "codex", Name: "s"}, ItemID: "id", Item: &Item{Text: "body"}})
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
	err := tr.Notify(context.Background(), Notice{Target: Target{Agent: "codex", Name: "s"}, ItemID: "id", Item: &Item{Text: "body"}})
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
			Item:     item,
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
	stub := &stubTransport{name: "custom-fetch"}
	registerForTest(t, stub)
	q, _ := mustOpen(t, WithFetchCmd(func(target Target) string { return "myctl fetch " + target.String() }))
	target := Target{Agent: "custom-fetch", Name: "s"}
	if _, err := q.PushAndNotify(context.Background(), target, "body", nil); err != nil {
		t.Fatal(err)
	}
	if len(stub.notices) != 1 || stub.notices[0].FetchCmd != "myctl fetch custom-fetch:s" {
		t.Fatalf("configured fetch lost: %+v", stub.notices)
	}
}

func TestDirectDeliveryClaimsBeforeHandoffAndArchives(t *testing.T) {
	q, _ := mustOpen(t)
	target := Target{Agent: "codex", Name: "thread"}
	var delivered string
	registerForTest(t, &codexTransport{run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if _, err := q.TakeNext(target); !errors.Is(err, ErrEmpty) {
			t.Errorf("competing consumer obtained body: %v", err)
		}
		delivered = args[len(args)-1]
		return nil, nil
	}})
	item, err := q.PushAndNotify(context.Background(), target, "complete\nbody", map[string]string{"task_id": "task"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(delivered, "complete\nbody") || !strings.Contains(delivered, item.ID) || strings.Contains(delivered, "Fetch it with:") {
		t.Fatalf("wrong payload: %s", delivered)
	}
	done, err := q.List(target, StateDone)
	if err != nil || len(done) != 1 || done[0].ID != item.ID {
		t.Fatalf("delivery not archived: %+v %v", done, err)
	}
	if _, err := q.TakeNext(target); !errors.Is(err, ErrEmpty) {
		t.Fatalf("success still pending: %v", err)
	}
}

func TestDirectDeliveryFailureRetriesSameBodyAndID(t *testing.T) {
	q, _ := mustOpen(t)
	target := Target{Agent: "codex", Name: "thread"}
	calls := 0
	registerForTest(t, &codexTransport{run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls++
		if !strings.Contains(args[len(args)-1], "body") {
			t.Fatal("missing body")
		}
		if calls == 1 {
			return nil, errors.New("host unavailable")
		}
		return nil, nil
	}})
	item, err := q.PushAndNotify(context.Background(), target, "body", nil)
	if !errors.Is(err, ErrNotify) {
		t.Fatalf("got %v", err)
	}
	pending, err := q.List(target, StatePending)
	if err != nil || len(pending) != 1 || pending[0].ID != item.ID {
		t.Fatalf("failed body not recoverable: %+v %v", pending, err)
	}
	if err := q.Notify(context.Background(), target, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := q.Notify(context.Background(), target, item.ID); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("redelivered accepted body: %d calls", calls)
	}
}

func TestConcurrentDirectRetryHasOneOwner(t *testing.T) {
	q, _ := mustOpen(t)
	target := Target{Agent: "codex", Name: "thread"}
	item, err := q.Push(target, "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	registerForTest(t, &codexTransport{run: func(context.Context, string, ...string) ([]byte, error) {
		calls.Add(1)
		close(entered)
		<-release
		return nil, nil
	}})
	finished := make(chan error, 1)
	go func() { finished <- q.Notify(context.Background(), target, item.ID) }()
	<-entered
	if err := q.Notify(context.Background(), target, item.ID); !errors.Is(err, ErrDeliveryInProgress) {
		t.Errorf("claimed item falsely reported acceptance: %v", err)
	}
	if _, err := q.TakeNext(target); !errors.Is(err, ErrEmpty) {
		t.Errorf("pull consumer won during handoff: %v", err)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("host received %d copies", calls.Load())
	}
}

func TestAcceptedDeliveryArchiveFailureDoesNotRequeue(t *testing.T) {
	q, root := mustOpen(t)
	target := Target{Agent: "codex", Name: "thread"}
	calls := 0
	registerForTest(t, &codexTransport{run: func(context.Context, string, ...string) ([]byte, error) {
		calls++
		claimed, err := q.List(target, StateClaimed)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim missing: %+v %v", claimed, err)
		}
		if err := os.Mkdir(filepath.Join(root, "codex", "thread", "done", claimed[0].ID+".json"), 0700); err != nil {
			t.Fatal(err)
		}
		return nil, nil
	}})
	item, err := q.PushAndNotify(context.Background(), target, "body", nil)
	if !errors.Is(err, ErrDeliveryAccepted) || errors.Is(err, ErrNotify) {
		t.Fatalf("acceptance lost: %v", err)
	}
	if _, err := q.TakeNext(target); !errors.Is(err, ErrEmpty) {
		t.Fatal("accepted body returned to pending")
	}
	if err := q.Notify(context.Background(), target, item.ID); !errors.Is(err, ErrDeliveryInProgress) {
		t.Fatalf("uncertain handoff falsely reported accepted: %v", err)
	}
	if calls != 1 {
		t.Fatalf("accepted body sent %d times", calls)
	}
}
