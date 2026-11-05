package agentqueue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func registerForTest(t *testing.T, tr Transport) {
	t.Helper()
	old, had := Lookup(tr.Name())
	Register(tr)
	t.Cleanup(func() {
		registryMu.Lock()
		defer registryMu.Unlock()
		if had {
			registry[tr.Name()] = old
		} else {
			delete(registry, tr.Name())
		}
	})
}

type receiptStub struct {
	calls atomic.Int32
	send  func(context.Context, Notice) (HostReceipt, error)
}

func (*receiptStub) Name() string { return "test-receipts" }
func (s *receiptStub) Notify(ctx context.Context, n Notice) error {
	_, err := s.NotifyWithReceipt(ctx, n)
	return err
}
func (s *receiptStub) NotifyWithReceipt(ctx context.Context, n Notice) (HostReceipt, error) {
	s.calls.Add(1)
	if s.send != nil {
		return s.send(ctx, n)
	}
	return HostReceipt{State: "queued", QueueID: "host-id"}, nil
}

func TestLookupRegisteredTransports(t *testing.T) {
	for _, name := range []string{"claude", "codex", "pi"} {
		if _, ok := Lookup(name); !ok {
			t.Fatalf("missing %s", name)
		}
	}
}
func TestNotificationReceiptDoesNotConsumeBody(t *testing.T) {
	q, _ := mustOpen(t)
	transport := &receiptStub{}
	registerForTest(t, transport)
	target := Target{Agent: transport.Name(), Name: "session"}
	first, err := q.PushAndNotify(context.Background(), target, "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := q.Notify(context.Background(), target, first.ID); err != nil {
			t.Fatal(err)
		}
	}
	if transport.calls.Load() != 1 {
		t.Fatal("same unread item repeatedly notified")
	}
	state, err := q.Notification(target)
	if err != nil || state.State != "queued" || len(state.NotifiedIDs) != 1 {
		t.Fatalf("bad receipt: %+v %v", state, err)
	}
	second, err := q.PushAndNotify(context.Background(), target, "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	if transport.calls.Load() != 2 {
		t.Fatal("new unread item was suppressed")
	}
	received, err := q.Receive(target, 20)
	if err != nil || len(received) != 2 {
		t.Fatalf("not consumed atomically: %+v %v", received, err)
	}
	empty, err := q.Receive(target, 20)
	if err != nil || len(empty) != 0 {
		t.Fatal("stale notice must be a successful empty receive")
	}
	for _, item := range received {
		if err := q.Ack(target, item.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{first.ID, second.ID} {
		if err := q.Notify(context.Background(), target, id); err != nil {
			t.Fatal(err)
		}
	}
	if transport.calls.Load() != 2 {
		t.Fatal("acknowledged body was notified again")
	}
}
func TestFailedNotificationRetriesSamePersistedBody(t *testing.T) {
	q, _ := mustOpen(t)
	transport := &receiptStub{}
	registerForTest(t, transport)
	transport.send = func(context.Context, Notice) (HostReceipt, error) {
		if transport.calls.Load() == 1 {
			return HostReceipt{}, errors.New("unavailable")
		}
		return HostReceipt{State: "queued", QueueID: "receipt"}, nil
	}
	target := Target{Agent: transport.Name(), Name: "s"}
	item, err := q.PushAndNotify(context.Background(), target, "body", nil)
	if !errors.Is(err, ErrNotify) {
		t.Fatalf("got %v", err)
	}
	if err := q.Notify(context.Background(), target, item.ID); err != nil {
		t.Fatal(err)
	}
	received, err := q.Receive(target, 1)
	if err != nil || len(received) != 1 || received[0].ID != item.ID {
		t.Fatalf("lost body/identity: %+v %v", received, err)
	}
}
func TestConcurrentNotificationWritersAndSnapshot(t *testing.T) {
	q, _ := mustOpen(t)
	transport := &receiptStub{}
	registerForTest(t, transport)
	target := Target{Agent: transport.Name(), Name: "s"}
	item, err := q.Push(target, "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	transport.send = func(ctx context.Context, _ Notice) (HostReceipt, error) {
		close(entered)
		select {
		case <-release:
			return HostReceipt{State: "queued"}, nil
		case <-ctx.Done():
			return HostReceipt{}, ctx.Err()
		}
	}
	done := make(chan error, 1)
	go func() { done <- q.Notify(context.Background(), target, item.ID) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := q.Notify(ctx, target, item.ID); err == nil {
		t.Fatal("competing notification bypassed ownership")
	}
	// Consumers own bodies independently of the notifier.
	if received, err := q.Receive(target, 1); err != nil || len(received) != 1 {
		t.Fatal("notifier locked the body")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if transport.calls.Load() != 1 {
		t.Fatal("duplicate host submission")
	}
}
func TestUncertainReservationExpiresWithoutAcknowledgingBody(t *testing.T) {
	now := time.Now().UTC()
	q, root := mustOpen(t, WithClock(func() time.Time { return now }))
	transport := &receiptStub{}
	registerForTest(t, transport)
	target := Target{Agent: transport.Name(), Name: "s"}
	item, err := q.Push(target, "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	base, err := q.targetDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivateJSON(base, "notification.json", NotificationStatus{State: "submitting", GeneratedAt: now, AttemptIDs: []string{item.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := q.Notify(context.Background(), target, item.ID); !errors.Is(err, ErrDeliveryInProgress) {
		t.Fatalf("uncertain reservation ignored: %v (%s)", err, root)
	}
	now = now.Add(uncertainLifetime + time.Second)
	if err := q.Notify(context.Background(), target, item.ID); err != nil {
		t.Fatal(err)
	}
	if received, err := q.Receive(target, 1); err != nil || len(received) != 1 {
		t.Fatal("reservation changed read state")
	}
}
func TestUncertainHostResultIsNotImmediatelyResent(t *testing.T) {
	q, _ := mustOpen(t)
	transport := &receiptStub{send: func(context.Context, Notice) (HostReceipt, error) { return HostReceipt{}, ErrNotificationUncertain }}
	registerForTest(t, transport)
	target := Target{Agent: transport.Name(), Name: "s"}
	item, err := q.PushAndNotify(context.Background(), target, "body", nil)
	if !errors.Is(err, ErrNotificationUncertain) {
		t.Fatal(err)
	}
	if err := q.Notify(context.Background(), target, item.ID); !errors.Is(err, ErrDeliveryInProgress) {
		t.Fatal(err)
	}
	if transport.calls.Load() != 1 {
		t.Fatal("immediate replay of uncertain signal")
	}
}
func TestPullIntegrationsKeepBodyPending(t *testing.T) {
	for _, name := range []string{"claude", "pi"} {
		t.Run(name, func(t *testing.T) {
			q, _ := mustOpen(t)
			item, err := q.PushAndNotify(context.Background(), Target{Agent: name, Name: "s"}, "body", nil)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := q.List(item.Target, StatePending)
			if err != nil || len(pending) != 1 {
				t.Fatal("no-op changed payload ownership")
			}
		})
	}
}
func TestUnknownAgentDoesNotPublish(t *testing.T) {
	q, _ := mustOpen(t)
	item, err := q.PushAndNotify(context.Background(), Target{Agent: "missing", Name: "s"}, "body", nil)
	if item != nil || err == nil || !strings.Contains(err.Error(), "agentqueue.Register") {
		t.Fatalf("%+v %v", item, err)
	}
}

const testThread = "01a1a947-2087-78b3-bafc-0a245b11cd3e"
const testQueueID = "738e06d2-1bab-4e10-9ad0-7fe29c5a2c69"

func codexNotice(t *testing.T) Notice {
	t.Helper()
	return Notice{Target: Target{Agent: "codex", Name: testThread}, ID: "notice-id", FetchCmd: "myctl receive", GeneratedAt: time.Now(), Address: &Address{Profile: t.TempDir()}, Item: &Item{Text: "do not put this body in a signal"}}
}
func TestCodexReceiptAndTargetProfile(t *testing.T) {
	n := codexNotice(t)
	tr := &codexTransport{runProfile: func(_ context.Context, profile, name string, args ...string) ([]byte, error) {
		if profile != n.Address.Profile || name != "codex" || args[0] != "queue" || args[2] != testThread {
			t.Fatalf("wrong destination: %s %s %v", profile, name, args)
		}
		message := args[len(args)-1]
		if !strings.Contains(message, n.FetchCmd) || strings.Contains(message, n.Item.Text) || strings.Contains(message, "pending") {
			t.Fatalf("wrong nudge: %s", message)
		}
		return []byte(fmt.Sprintf("Queued message %s for thread %s.\n", testQueueID, testThread)), nil
	}}
	receipt, err := tr.NotifyWithReceipt(context.Background(), n)
	if err != nil || receipt.QueueID != testQueueID || receipt.State != "queued" {
		t.Fatalf("%+v %v", receipt, err)
	}
	for _, output := range []string{"success", fmt.Sprintf("Queued message %s for thread %s.\n", testQueueID, testQueueID)} {
		tr.runProfile = func(context.Context, string, string, ...string) ([]byte, error) { return []byte(output), nil }
		if _, err := tr.NotifyWithReceipt(context.Background(), n); !errors.Is(err, ErrNotificationUncertain) {
			t.Fatalf("false receipt: %v", err)
		}
	}
}
