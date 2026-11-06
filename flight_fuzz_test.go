package agentqueue

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func FuzzInboxFlightStateMachine(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{6, 2, 4, 3, 6, 2, 13, 3, 6, 2, 22, 3, 6, 2, 31, 3, 7, 6, 8})
	f.Add([]byte{6, 0, 7, 50, 1, 2, 7})
	f.Add([]byte{0, 2, 1, 4, 3, 6, 7, 0, 8, 5, 7})
	f.Fuzz(func(t *testing.T, trace []byte) {
		if len(trace) > 64 {
			trace = trace[:64]
		}
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		q, _ := mustOpen(t, WithClock(func() time.Time { return now }))
		var submitted []string
		transport := &receiptStub{send: func(_ context.Context, n Notice) (HostReceipt, error) {
			if !strings.Contains(n.FetchCmd, "--notice "+n.ID) {
				t.Fatal("hint omitted identity-bound receipt command")
			}
			submitted = append(submitted, n.ID)
			return HostReceipt{State: "queued", QueueID: "model-host"}, nil
		}}
		registerForTest(t, transport)
		target := Target{Agent: transport.Name(), Name: "model"}
		if err := q.PutAddress(Address{Agent: target.Agent, Name: target.Name, WakeProtocol: WakeProtocolVersion}); err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		var ids []string
		states := map[string]State{}
		bodies := map[string]string{}
		acquired := map[string]bool{}
		flight, currentNotice := "idle", ""
		var drainDeadline time.Time
		expectedWakes := int32(0)
		notify := func(id string) {
			t.Helper()
			newWake := states[id] == StatePending && !(flight == "queued" || flight == "draining" && now.Before(drainDeadline))
			if newWake {
				expectedWakes++
				flight = "queued"
			}
			if err := q.Notify(ctx, target, id); err != nil {
				t.Fatal(err)
			}
			if newWake && len(submitted) > 0 {
				currentNotice = submitted[len(submitted)-1]
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
			op, arg := action%9, int(action/9)
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
			case 1:
				if len(ids) > 0 {
					notify(ids[arg%len(ids)])
				}
			case 2, 3, 7, 8:
				limit := 1 + arg%3
				pending := pendingCount()
				providedNotice := currentNotice
				if op == 8 {
					providedNotice = strings.Repeat("0", 32)
					if len(submitted) > 1 {
						providedNotice = submitted[0]
					}
				}
				noticeRead := op == 7 || op == 8
				validNotice := noticeRead && providedNotice != "" && providedNotice == currentNotice && flight != "idle"
				if validNotice {
					if pending == 0 {
						flight = "idle"
					} else {
						flight = "draining"
						drainDeadline = now.Add(5 * time.Minute)
					}
				}
				var batch []Item
				var err error
				switch op {
				case 2:
					batch, err = q.ReceiveAutomatic(ctx, target, limit)
				case 3:
					batch, err = q.Receive(target, limit)
				default:
					if providedNotice == "" {
						providedNotice = strings.Repeat("0", 32)
					}
					batch, err = q.ReceiveNotice(target, providedNotice, limit)
				}
				if err != nil {
					t.Fatal(err)
				}
				expected := min(pending, limit)
				if noticeRead && !validNotice {
					expected = 0
				}
				if len(batch) != expected {
					t.Fatalf("step %d op=%d: receive count=%d expected=%d", step, op, len(batch), expected)
				}
				for _, item := range batch {
					state, known := states[item.ID]
					if !known || state != StatePending || acquired[item.ID] || item.Text != bodies[item.ID] {
						t.Fatalf("step %d: duplicate/changed body %s", step, item.ID)
					}
					acquired[item.ID] = true
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
				t.Fatalf("step %d op=%d: wakes=%d expected=%d flight=%s trace=%v", step, op, actual, expectedWakes, flight, trace)
			}
			found := map[string]bool{}
			for _, state := range []State{StatePending, StateClaimed, StateDone} {
				items, err := q.List(target, state)
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range items {
					if found[item.ID] || states[item.ID] != state || bodies[item.ID] != item.Text {
						t.Fatalf("step %d: body state/content changed: %+v in %s", step, item, state)
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
