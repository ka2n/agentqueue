package agentqueue

import (
	"context"
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
	batch, err := q.Receive(target, 2)
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
		batch, err = q.Receive(target, 2)
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
			first, err := q.Receive(target, 1)
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
	if empty, err := q.Receive(target, 20); err != nil || len(empty) != 0 {
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
	q, _ := mustOpen(t)
	tr := &receiptStub{}
	registerForTest(t, tr)
	target := Target{Agent: tr.Name(), Name: "legacy"}
	ctx := context.Background()
	if err := q.PutAddress(Address{Agent: target.Agent, Name: target.Name}); err != nil {
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
		t.Fatalf("legacy receiver enabled new protocol: %+v %v calls=%d", state, err, tr.calls.Load())
	}
}
