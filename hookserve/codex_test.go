package hookserve

import (
	"strings"
	"sync"
	"testing"

	"github.com/ka2n/agentqueue"
)

func TestCodexDeliveryBoundaries(t *testing.T) {
	for _, event := range []string{"PostToolUse", "UserPromptSubmit", "Stop", "SessionStart", "PostToolUseFailure"} {
		t.Run(event, func(t *testing.T) {
			q := openQueue(t)
			target := agentqueue.Target{Agent: "codex", Name: "thread"}
			item, err := q.Push(target, "urgent context", nil)
			if err != nil {
				t.Fatal(err)
			}
			out, delivered, err := HandleCodex(q, Payload{SessionID: "thread", HookEventName: event}, Options{Getenv: noEnv})
			if err != nil {
				t.Fatal(err)
			}
			want := event == "PostToolUse" || event == "UserPromptSubmit"
			if delivered != want {
				t.Fatalf("delivered=%v want %v", delivered, want)
			}
			if !want {
				if out != nil || countIn(t, q, target, agentqueue.StatePending) != 1 {
					t.Fatal("unsupported event claimed message")
				}
				return
			}
			if out.Decision != "" || out.Reason != "" {
				t.Fatal("Codex delivery must not block")
			}
			for _, text := range []string{item.ID, "urgent context", "agentqueue ack --to codex:thread"} {
				if !strings.Contains(out.HookSpecificOutput.AdditionalContext, text) {
					t.Fatalf("missing %q", text)
				}
			}
			if countIn(t, q, target, agentqueue.StateClaimed) != 1 {
				t.Fatal("not claimed")
			}
		})
	}
}

func TestCodexTargetResolution(t *testing.T) {
	for _, tc := range []struct{ name, session, to, env, target string }{
		{"payload wins", "payload", "", "env", "payload"},
		{"environment fallback", "", "", "env", "env"},
		{"explicit wins", "payload", "codex:explicit", "env", "explicit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := openQueue(t)
			target := agentqueue.Target{Agent: "codex", Name: tc.target}
			if _, err := q.Push(target, "body", nil); err != nil {
				t.Fatal(err)
			}
			_, ok, err := HandleCodex(q, Payload{SessionID: tc.session, HookEventName: "PostToolUse"}, Options{To: tc.to, Getenv: func(key string) string {
				if key == "CODEX_THREAD_ID" {
					return tc.env
				}
				return ""
			}})
			if err != nil || !ok {
				t.Fatalf("delivered %v: %v", ok, err)
			}
		})
	}
	q := openQueue(t)
	if _, ok, err := HandleCodex(q, Payload{HookEventName: "PostToolUse"}, Options{Getenv: noEnv}); err != nil || ok {
		t.Fatalf("missing target: %v %v", ok, err)
	}
	if _, _, err := HandleCodex(q, Payload{HookEventName: "PostToolUse"}, Options{To: "claude:other"}); err == nil {
		t.Fatal("accepted another agent namespace")
	}
}

func TestCodexConcurrentHooksClaimOnce(t *testing.T) {
	q := openQueue(t)
	target := agentqueue.Target{Agent: "codex", Name: "thread"}
	if _, err := q.Push(target, "one message", nil); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan bool, 8)
	for range 8 {
		wg.Go(func() {
			_, ok, err := HandleCodex(q, Payload{SessionID: "thread", HookEventName: "PostToolUse"}, Options{Getenv: noEnv})
			if err != nil {
				t.Error(err)
			}
			results <- ok
		})
	}
	wg.Wait()
	close(results)
	n := 0
	for ok := range results {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("delivered %d times", n)
	}
}
