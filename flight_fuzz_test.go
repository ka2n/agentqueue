package agentqueue

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func FuzzInboxFlightStateMachine(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{6, 2, 4, 6, 2, 12, 6, 2, 20, 6, 2, 28, 3, 6})
	f.Add([]byte{6, 0, 3, 45, 7, 2, 3})
	f.Add([]byte{0, 2, 1, 4, 3, 6, 3, 0, 7, 5, 3})
	f.Fuzz(func(t *testing.T, trace []byte) {
		if len(trace) > 64 {
			trace = trace[:64]
		}
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		q, _ := mustOpen(t, WithClock(func() time.Time { return now }))
		transport := &receiptStub{}
		registerForTest(t, transport)
		target := Target{Agent: transport.Name(), Name: "model"}
		if err := q.PutAddress(Address{Agent: target.Agent, Name: target.Name, WakeProtocol: WakeProtocolVersion}); err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		var ids []string
		states := map[string]State{}
		bodies := map[string]string{}
		observed := map[string]bool{}
		flight := "idle"
		var drainDeadline time.Time
		expectedWakes := int32(0)
		notify := func(id string) {
			t.Helper()
			if states[id] == StatePending && !(flight == "queued" || flight == "draining" && now.Before(drainDeadline)) {
				expectedWakes++
				flight = "queued"
			}
			if err := q.Notify(ctx, target, id); err != nil {
				t.Fatal(err)
			}
		}
		pendingCount := func() int {
			count := 0
			for _, state := range states {
				if state == StatePending {
					count++
				}
			}
			return count
		}
		for step, action := range trace {
			op, arg := action&7, int(action>>3)
			switch op {
			case 0, 6:
				text := fmt.Sprintf("body-%d", len(ids))
				item, err := q.Push(target, text, nil)
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, item.ID)
				states[item.ID] = StatePending
				bodies[item.ID] = text
				if op == 6 {
					notify(item.ID)
				}
			case 1, 7:
				if len(ids) > 0 {
					index := arg % len(ids)
					if op == 7 {
						index = len(ids) - 1
					}
					notify(ids[index])
				}
			case 2, 3:
				limit := 1 + arg%3
				pending := pendingCount()
				if op == 3 && flight != "idle" {
					if pending == 0 {
						flight = "idle"
					} else {
						flight = "draining"
						drainDeadline = now.Add(5 * time.Minute)
					}
				}
				var batch []Item
				var err error
				if op == 2 {
					batch, err = q.ReceiveAutomatic(ctx, target, limit)
				} else {
					batch, err = q.Receive(target, limit)
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(batch) != min(pending, limit) {
					t.Fatalf("step %d: delayed body delivery: got %d pending=%d limit=%d", step, len(batch), pending, limit)
				}
				for _, item := range batch {
					if state, known := states[item.ID]; !known || state != StatePending || observed[item.ID] || item.Text != bodies[item.ID] {
						t.Fatalf("step %d: duplicate acquisition %s", step, item.ID)
					}
					observed[item.ID] = true
					states[item.ID] = StateClaimed
				}
			case 4:
				if len(ids) > 0 {
					id := ids[arg%len(ids)]
					if states[id] == StateClaimed {
						if err := q.Ack(target, id); err != nil {
							t.Fatal(err)
						}
						states[id] = StateDone
					}
				}
			case 5:
				now = now.Add(time.Duration(1+arg%7) * time.Minute)
			}
			if actual := transport.calls.Load(); actual != expectedWakes {
				t.Fatalf("step %d op=%d: native wakes=%d expected=%d flight=%s trace=%v", step, op, actual, expectedWakes, flight, trace)
			}
			found := map[string]bool{}
			for _, state := range []State{StatePending, StateClaimed, StateDone} {
				items, err := q.List(target, state)
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range items {
					if found[item.ID] || states[item.ID] != state || bodies[item.ID] != item.Text {
						t.Fatalf("step %d: body state/content changed unexpectedly: %+v in %s", step, item, state)
					}
					found[item.ID] = true
				}
			}
			if len(found) != len(ids) {
				t.Fatalf("step %d: body lost: found=%d pushed=%d", step, len(found), len(ids))
			}
		}
	})
}
