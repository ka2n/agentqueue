package agentqueue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestQueuedWakeCoalescesUntilInboxDrain(t *testing.T) {
	q, _ := mustOpen(t)
	tr := &receiptStub{}
	registerForTest(t, tr)
	target := Target{Agent: tr.Name(), Name: "session"}
	ctx := context.Background()
	if err := q.PutAddress(Address{Agent: target.Agent, Name: target.Name, WakeProtocol: WakeProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	push := func(text string) {
		t.Helper()
		if _, err := q.PushAndNotify(ctx, target, text, nil); err != nil {
			t.Fatal(err)
		}
	}
	for range 4 {
		push("message")
	}
	if tr.calls.Load() != 1 {
		t.Fatalf("queued %d wakes for one inbox", tr.calls.Load())
	}
	initial, _ := q.Notification(target)
	automatic, err := q.ReceiveAutomatic(ctx, target, 1)
	if err != nil || len(automatic) != 1 {
		t.Fatalf("automatic delivery blocked: %+v %v", automatic, err)
	}
	fallback, err := q.WaitReceive(ctx, target, 20*time.Millisecond, 1)
	if err != nil || len(fallback) != 1 {
		t.Fatalf("fallback delivery blocked: %+v %v", fallback, err)
	}
	automatic = append(automatic, fallback...)
	batch, err := receiveCurrentNotice(t, q, target, 2)
	if err != nil || len(batch) != 2 {
		t.Fatalf("first batch=%+v %v", batch, err)
	}
	push("during drain")
	if tr.calls.Load() != 1 {
		t.Fatal("new wake submitted while draining")
	}
	state, _ := q.Notification(target)
	if state.ID != initial.ID || state.State != "receiving" {
		t.Fatalf("flight replaced: %+v", state)
	}
	all := append(automatic, batch...)
	for _, want := range []int{1, 0} {
		batch, err = receiveCurrentNotice(t, q, target, 2)
		if err != nil || len(batch) != want {
			t.Fatalf("drain=%+v want=%d err=%v", batch, want, err)
		}
		all = append(all, batch...)
	}
	for _, item := range all {
		if err := q.Ack(target, item.ID); err != nil {
			t.Fatal(err)
		}
	}
	state, _ = q.Notification(target)
	if state.State != "received" {
		t.Fatalf("flight not released: %+v", state)
	}
	push("next wake")
	if tr.calls.Load() != 2 {
		t.Fatal("completed drain prevented next wake")
	}
}

func TestAutomaticReceiverFirstPreventsNativeWake(t *testing.T) {
	q, _ := mustOpen(t)
	tr := &receiptStub{}
	registerForTest(t, tr)
	target := Target{Agent: tr.Name(), Name: "session"}
	ctx := context.Background()
	if err := q.PutAddress(Address{Agent: target.Agent, Name: target.Name, WakeProtocol: WakeProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	item, err := q.Push(target, "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	items, err := q.ReceiveAutomatic(ctx, target, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("automatic receive=%+v %v", items, err)
	}
	if err := q.Notify(ctx, target, item.ID); err != nil {
		t.Fatal(err)
	}
	if tr.calls.Load() != 0 {
		t.Fatal("wake queued for previously claimed body")
	}
}

func TestInterruptedDrainAllowsRecovery(t *testing.T) {
	for _, mode := range []string{"automatic", "notify"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().UTC()
			q, _ := mustOpen(t, WithClock(func() time.Time { return now }))
			tr := &receiptStub{}
			registerForTest(t, tr)
			target := Target{Agent: tr.Name(), Name: "session"}
			ctx := context.Background()
			if err := q.PutAddress(Address{Agent: target.Agent, Name: target.Name, WakeProtocol: WakeProtocolVersion}); err != nil {
				t.Fatal(err)
			}
			var second *Item
			for range 2 {
				var err error
				second, err = q.PushAndNotify(ctx, target, "body", nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			first, err := receiveCurrentNotice(t, q, target, 1)
			if err != nil || len(first) != 1 {
				t.Fatal(err)
			}
			now = now.Add(drainLifetime + time.Second)
			if mode == "automatic" {
				items, err := q.ReceiveAutomatic(ctx, target, 1)
				if err != nil || len(items) != 1 {
					t.Fatalf("fallback blocked after lease: %+v %v", items, err)
				}
			} else {
				if first[0].ID == second.ID {
					items, _ := q.List(target, StatePending)
					second = &items[0]
				}
				if err := q.Notify(ctx, target, second.ID); err != nil {
					t.Fatal(err)
				}
				if tr.calls.Load() != 2 {
					t.Fatal("expired drain suppressed retry")
				}
			}
		})
	}
}

func TestWrittenSocketReceiptRetainsAutomaticFallback(t *testing.T) {
	q, _ := mustOpen(t)
	tr := &receiptStub{send: func(context.Context, Notice) (HostReceipt, error) { return HostReceipt{State: "written"}, nil }}
	registerForTest(t, tr)
	target := Target{Agent: tr.Name(), Name: "session"}
	ctx := context.Background()
	if err := q.PutAddress(Address{Agent: target.Agent, Name: target.Name, WakeProtocol: WakeProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.PushAndNotify(ctx, target, "body", nil); err != nil {
		t.Fatal(err)
	}
	items, err := q.ReceiveAutomatic(ctx, target, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("unverified socket suppressed fallback: %+v %v", items, err)
	}
}

func TestAutomaticConsumptionDoesNotQueueAnotherWake(t *testing.T) {
	q, _ := mustOpen(t)
	tr := &receiptStub{}
	registerForTest(t, tr)
	target := Target{Agent: tr.Name(), Name: "session"}
	ctx := context.Background()
	if err := q.PutAddress(Address{Agent: target.Agent, Name: target.Name, WakeProtocol: WakeProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	var wakeID string
	for i := range 4 {
		item, err := q.PushAndNotify(ctx, target, "body", nil)
		if err != nil {
			t.Fatal(err)
		}
		var received []Item
		if i%2 == 0 {
			received, err = q.ReceiveAutomatic(ctx, target, 1)
		} else {
			received, err = q.WaitReceive(ctx, target, time.Second, 1)
		}
		if err != nil || len(received) != 1 || received[0].ID != item.ID {
			t.Fatalf("body delayed/lost: %+v %v", received, err)
		}
		if err := q.Ack(target, item.ID); err != nil {
			t.Fatal(err)
		}
		if empty, err := q.ReceiveAutomatic(ctx, target, 20); err != nil || len(empty) != 0 {
			t.Fatalf("automatic empty read=%+v %v", empty, err)
		}
		if empty, err := q.Receive(target, 20); err != nil || len(empty) != 0 {
			t.Fatalf("ordinary empty receive=%+v %v", empty, err)
		}
		state, err := q.Notification(target)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			wakeID = state.ID
		}
		if state.ID != wakeID || state.State != "queued" || tr.calls.Load() != 1 {
			t.Fatalf("redundant wake: %+v calls=%d", state, tr.calls.Load())
		}
	}
	if empty, err := receiveCurrentNotice(t, q, target, 20); err != nil || len(empty) != 0 {
		t.Fatalf("stale wake=%+v %v", empty, err)
	}
	if _, err := q.PushAndNotify(ctx, target, "after wake observed", nil); err != nil {
		t.Fatal(err)
	}
	if tr.calls.Load() != 2 {
		t.Fatal("next inbox change could not wake receiver")
	}
}

func TestLegacyReceiverDoesNotEnableWakeCoalescing(t *testing.T) {
	for _, protocol := range []int{0, 1} {
		t.Run(fmt.Sprint(protocol), func(t *testing.T) {
			q, _ := mustOpen(t)
			var commands []string
			tr := &receiptStub{send: func(_ context.Context, n Notice) (HostReceipt, error) {
				commands = append(commands, n.FetchCmd)
				return HostReceipt{State: "queued"}, nil
			}}
			registerForTest(t, tr)
			target := Target{Agent: tr.Name(), Name: "legacy"}
			ctx := context.Background()
			if err := q.PutAddress(Address{Agent: target.Agent, Name: target.Name, WakeProtocol: protocol}); err != nil {
				t.Fatal(err)
			}
			first, err := q.PushAndNotify(ctx, target, "first", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := q.Notify(ctx, target, first.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := q.PushAndNotify(ctx, target, "second", nil); err != nil {
				t.Fatal(err)
			}
			state, err := q.Notification(target)
			if err != nil || state.Coalescing || tr.calls.Load() != 2 {
				t.Fatalf("legacy enabled notice protocol: %+v %v calls=%d", state, err, tr.calls.Load())
			}
			for _, cmd := range commands {
				if strings.Contains(cmd, " --notice ") {
					t.Fatal("legacy receiver got unsupported flag")
				}
			}
			state.Coalescing = true
			state.WakeProtocol = 0
			base, _ := q.targetDir(target)
			if err := writePrivateJSON(base, "notification.json", state); err != nil {
				t.Fatal(err)
			}
			if err := q.PutAddress(Address{Agent: target.Agent, Name: target.Name, WakeProtocol: WakeProtocolVersion}); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := q.PushAndNotify(ctx, target, "upgraded", nil); err != nil {
					t.Fatal(err)
				}
			}
			current, _ := q.Notification(target)
			if tr.calls.Load() != 3 || current.WakeProtocol != WakeProtocolVersion || !strings.Contains(commands[2], " --notice "+current.ID) {
				t.Fatalf("old flight migrated incorrectly: %+v calls=%d commands=%v", current, tr.calls.Load(), commands)
			}
		})
	}
}

func receiveCurrentNotice(t *testing.T, q *Queue, target Target, limit int) ([]Item, error) {
	t.Helper()
	state, err := q.Notification(target)
	if err != nil {
		t.Fatal(err)
	}
	return q.ReceiveNotice(target, state.ID, limit)
}

func TestOrdinaryReceivesAndStaleNoticeDoNotObserveCurrentWake(t *testing.T) {
	q, _ := mustOpen(t)
	tr := &receiptStub{}
	registerForTest(t, tr)
	target := Target{Agent: tr.Name(), Name: "session"}
	ctx := context.Background()
	if err := q.PutAddress(Address{Agent: target.Agent, Name: target.Name, WakeProtocol: WakeProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	first, err := q.PushAndNotify(ctx, target, "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	wake, _ := q.Notification(target)
	bodies, err := q.Receive(target, 20)
	if err != nil || len(bodies) != 1 {
		t.Fatalf("generic receive=%+v %v", bodies, err)
	}
	if err := q.Ack(target, first.ID); err != nil {
		t.Fatal(err)
	}
	if empty, err := q.Receive(target, 20); err != nil || len(empty) != 0 {
		t.Fatal(err)
	}
	second, err := q.PushAndNotify(ctx, target, "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.calls.Load() != 1 {
		t.Fatal("ordinary receive acknowledged a hint that never arrived")
	}
	unknown := "00000000000000000000000000000000"
	if empty, err := q.ReceiveNotice(target, unknown, 20); err != nil || len(empty) != 0 {
		t.Fatalf("unknown notice=%+v %v", empty, err)
	}
	bodies, err = q.ReceiveNotice(target, wake.ID, 20)
	if err != nil || len(bodies) != 1 || bodies[0].ID != second.ID {
		t.Fatalf("current notice=%+v %v", bodies, err)
	}
	if err := q.Ack(target, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.ReceiveNotice(target, wake.ID, 20); err != nil {
		t.Fatal(err)
	}
	third, err := q.PushAndNotify(ctx, target, "third", nil)
	if err != nil {
		t.Fatal(err)
	}
	current, _ := q.Notification(target)
	if current.ID == wake.ID || tr.calls.Load() != 2 {
		t.Fatal("next wave did not get its own notice")
	}
	if empty, err := q.ReceiveNotice(target, wake.ID, 20); err != nil || len(empty) != 0 {
		t.Fatalf("stale notice=%+v %v", empty, err)
	}
	pending, err := q.List(target, StatePending)
	if err != nil || len(pending) != 1 || pending[0].ID != third.ID {
		t.Fatalf("stale notice consumed current body: %+v %v", pending, err)
	}
	after, _ := q.Notification(target)
	if after.ID != current.ID || after.State != "queued" {
		t.Fatalf("stale notice cleared current flight: %+v", after)
	}
}

func TestNoticeObservedAfterUncertainSubmission(t *testing.T) {
	q, _ := mustOpen(t)
	tr := &receiptStub{send: func(context.Context, Notice) (HostReceipt, error) { return HostReceipt{}, ErrNotificationUncertain }}
	registerForTest(t, tr)
	target := Target{Agent: tr.Name(), Name: "uncertain"}
	if err := q.PutAddress(Address{Agent: target.Agent, Name: target.Name, WakeProtocol: WakeProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	item, err := q.PushAndNotify(context.Background(), target, "body", nil)
	if !errors.Is(err, ErrNotificationUncertain) {
		t.Fatal(err)
	}
	state, _ := q.Notification(target)
	batch, err := q.ReceiveNotice(target, state.ID, 20)
	if err != nil || len(batch) != 1 || batch[0].ID != item.ID {
		t.Fatalf("observed uncertain notice lost body: %+v %v", batch, err)
	}
	if empty, err := q.ReceiveNotice(target, state.ID, 20); err != nil || len(empty) != 0 {
		t.Fatal(err)
	}
	after, _ := q.Notification(target)
	if after.State != "received" {
		t.Fatalf("notice was not observed: %+v", after)
	}
}
