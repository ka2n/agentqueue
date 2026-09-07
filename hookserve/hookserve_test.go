package hookserve

import (
	"strings"
	"testing"

	"github.com/ka2n/agentqueue"
)

func openQueue(t *testing.T) *agentqueue.Queue {
	t.Helper()
	q, err := agentqueue.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return q
}

func countIn(t *testing.T, q *agentqueue.Queue, target agentqueue.Target, st agentqueue.State) int {
	t.Helper()
	items, err := q.List(target, st)
	if err != nil {
		t.Fatalf("List %v: %v", st, err)
	}
	return len(items)
}

func noEnv(string) string { return "" }

func TestParsePayload(t *testing.T) {
	t.Run("empty stream is a zero payload", func(t *testing.T) {
		got, err := ParsePayload(strings.NewReader("   \n"))
		if err != nil {
			t.Fatalf("ParsePayload: %v", err)
		}
		if got != (Payload{}) {
			t.Fatalf("got %+v, want zero payload", got)
		}
	})
	t.Run("fields decode", func(t *testing.T) {
		got, err := ParsePayload(strings.NewReader(`{"hook_event_name":"Stop","session_id":"s1","stop_hook_active":true,"cwd":"/w","transcript_path":"/t.jsonl","model":"m","source":"resume"}`))
		if err != nil {
			t.Fatalf("ParsePayload: %v", err)
		}
		want := Payload{
			SessionID:      "s1",
			HookEventName:  "Stop",
			Cwd:            "/w",
			TranscriptPath: "/t.jsonl",
			Model:          "m",
			Source:         "resume",
			StopHookActive: true,
		}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
	t.Run("malformed json is an error", func(t *testing.T) {
		if _, err := ParsePayload(strings.NewReader("{not json")); err == nil {
			t.Fatal("ParsePayload: want error on malformed json")
		}
	})
}

func TestHandleClaudeDeliversAndClaims(t *testing.T) {
	q := openQueue(t)
	target := agentqueue.Target{Agent: "claude", Name: "s1"}
	item, err := q.Push(target, "hello there", map[string]string{"from": "ci"})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	out, delivered, err := HandleClaude(q, Payload{HookEventName: "UserPromptSubmit", SessionID: "s1"}, Options{Getenv: noEnv})
	if err != nil {
		t.Fatalf("HandleClaude: %v", err)
	}
	if !delivered || out == nil || out.HookSpecificOutput == nil {
		t.Fatalf("delivered=%v out=%+v, want a delivery", delivered, out)
	}
	if out.HookSpecificOutput.HookEventName != "UserPromptSubmit" {
		t.Fatalf("hookEventName = %q", out.HookSpecificOutput.HookEventName)
	}
	if out.Decision != "" {
		t.Fatalf("UserPromptSubmit set a decision: %q", out.Decision)
	}
	ctx := out.HookSpecificOutput.AdditionalContext
	for _, want := range []string{item.ID, "hello there", "meta from=ci", "already claimed", "agentqueue ack --to claude:s1"} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("additionalContext missing %q:\n%s", want, ctx)
		}
	}
	// The item moved pending -> claimed.
	if got := countIn(t, q, target, agentqueue.StatePending); got != 0 {
		t.Fatalf("pending = %d, want 0", got)
	}
	if got := countIn(t, q, target, agentqueue.StateClaimed); got != 1 {
		t.Fatalf("claimed = %d, want 1", got)
	}
}

func TestHandleClaudeStopBlocksThenGuards(t *testing.T) {
	target := agentqueue.Target{Agent: "claude", Name: "s1"}

	t.Run("stop_hook_active false blocks", func(t *testing.T) {
		q := openQueue(t)
		if _, err := q.Push(target, "act on me", nil); err != nil {
			t.Fatalf("Push: %v", err)
		}
		out, delivered, err := HandleClaude(q, Payload{HookEventName: "Stop", SessionID: "s1", StopHookActive: false}, Options{Getenv: noEnv})
		if err != nil || !delivered {
			t.Fatalf("HandleClaude: err=%v delivered=%v", err, delivered)
		}
		if out.Decision != "block" || strings.TrimSpace(out.Reason) == "" {
			t.Fatalf("decision=%q reason=%q, want block with a reason", out.Decision, out.Reason)
		}
	})

	t.Run("stop_hook_active true delivers without blocking", func(t *testing.T) {
		q := openQueue(t)
		if _, err := q.Push(target, "act on me", nil); err != nil {
			t.Fatalf("Push: %v", err)
		}
		out, delivered, err := HandleClaude(q, Payload{HookEventName: "Stop", SessionID: "s1", StopHookActive: true}, Options{Getenv: noEnv})
		if err != nil || !delivered {
			t.Fatalf("HandleClaude: err=%v delivered=%v", err, delivered)
		}
		if out.Decision != "" || out.Reason != "" {
			t.Fatalf("decision=%q reason=%q, want no block while a stop hook is active", out.Decision, out.Reason)
		}
		if out.HookSpecificOutput == nil || !strings.Contains(out.HookSpecificOutput.AdditionalContext, "act on me") {
			t.Fatalf("additionalContext did not carry the item: %+v", out.HookSpecificOutput)
		}
	})

	t.Run("no-block suppresses the decision", func(t *testing.T) {
		q := openQueue(t)
		if _, err := q.Push(target, "act on me", nil); err != nil {
			t.Fatalf("Push: %v", err)
		}
		out, _, err := HandleClaude(q, Payload{HookEventName: "Stop", SessionID: "s1", StopHookActive: false}, Options{NoBlock: true, Getenv: noEnv})
		if err != nil {
			t.Fatalf("HandleClaude: %v", err)
		}
		if out.Decision != "" {
			t.Fatalf("decision = %q, want empty under NoBlock", out.Decision)
		}
	})
}

func TestHandleClaudeEmptyQueueIsSilent(t *testing.T) {
	q := openQueue(t)
	out, delivered, err := HandleClaude(q, Payload{HookEventName: "Stop", SessionID: "s1"}, Options{Getenv: noEnv})
	if err != nil {
		t.Fatalf("HandleClaude: %v", err)
	}
	if delivered || out != nil {
		t.Fatalf("delivered=%v out=%+v, want nothing", delivered, out)
	}
}

func TestHandleClaudeUnhandledEventIsSilent(t *testing.T) {
	q := openQueue(t)
	if _, err := q.Push(agentqueue.Target{Agent: "claude", Name: "s1"}, "untouched", nil); err != nil {
		t.Fatalf("Push: %v", err)
	}
	out, delivered, err := HandleClaude(q, Payload{HookEventName: "PreCompact", SessionID: "s1"}, Options{Getenv: noEnv})
	if err != nil || delivered || out != nil {
		t.Fatalf("HandleClaude on PreCompact: err=%v delivered=%v out=%+v", err, delivered, out)
	}
	if got := countIn(t, q, agentqueue.Target{Agent: "claude", Name: "s1"}, agentqueue.StatePending); got != 1 {
		t.Fatalf("pending = %d, want the item left alone", got)
	}
}

func TestHandleClaudeSessionStartCompactDoesNotDeliver(t *testing.T) {
	q := openQueue(t)
	target := agentqueue.Target{Agent: "claude", Name: "s1"}
	if _, err := q.Push(target, "should stay pending", nil); err != nil {
		t.Fatalf("Push: %v", err)
	}
	out, delivered, err := HandleClaude(q, Payload{HookEventName: "SessionStart", SessionID: "s1", Source: "compact", Cwd: t.TempDir()}, Options{Getenv: noEnv})
	if err != nil || delivered || out != nil {
		t.Fatalf("HandleClaude compact: err=%v delivered=%v out=%+v", err, delivered, out)
	}
	if got := countIn(t, q, target, agentqueue.StatePending); got != 1 {
		t.Fatalf("pending = %d, want the item untouched on compact", got)
	}
	// The address is still recorded before the compaction short-circuit.
	if _, err := q.Address(target); err != nil {
		t.Fatalf("Address after SessionStart(compact): %v", err)
	}
}

func TestHandleClaudeSessionStartRegistersAddress(t *testing.T) {
	q := openQueue(t)
	target := agentqueue.Target{Agent: "claude", Name: "s1"}
	cwd := t.TempDir()
	if _, err := q.Push(target, "welcome", nil); err != nil {
		t.Fatalf("Push: %v", err)
	}
	out, delivered, err := HandleClaude(q, Payload{HookEventName: "SessionStart", SessionID: "s1", Source: "startup", Cwd: cwd}, Options{Getenv: noEnv})
	if err != nil || !delivered || out == nil {
		t.Fatalf("HandleClaude startup: err=%v delivered=%v out=%+v", err, delivered, out)
	}
	addr, err := q.Address(target)
	if err != nil {
		t.Fatalf("Address: %v", err)
	}
	if addr.Cwd != cwd {
		t.Fatalf("recorded cwd = %q, want %q", addr.Cwd, cwd)
	}
}

func TestHandleClaudeMaxCaps(t *testing.T) {
	q := openQueue(t)
	target := agentqueue.Target{Agent: "claude", Name: "s1"}
	for i := 0; i < 4; i++ {
		if _, err := q.Push(target, "msg", nil); err != nil {
			t.Fatalf("Push: %v", err)
		}
	}
	out, _, err := HandleClaude(q, Payload{HookEventName: "Stop", SessionID: "s1", StopHookActive: true}, Options{Max: 2, Getenv: noEnv})
	if err != nil {
		t.Fatalf("HandleClaude: %v", err)
	}
	if got := strings.Count(out.HookSpecificOutput.AdditionalContext, "\n--- id "); got != 2 {
		t.Fatalf("delivered %d items, want 2", got)
	}
	if got := countIn(t, q, target, agentqueue.StateClaimed); got != 2 {
		t.Fatalf("claimed = %d, want 2", got)
	}
	if got := countIn(t, q, target, agentqueue.StatePending); got != 2 {
		t.Fatalf("pending = %d, want 2 left", got)
	}
}

func TestHandleClaudeNoTargetIsSilent(t *testing.T) {
	q := openQueue(t)
	logged := 0
	out, delivered, err := HandleClaude(q, Payload{HookEventName: "Stop"}, Options{
		Getenv: noEnv,
		Logf:   func(string, ...any) { logged++ },
	})
	if err != nil || delivered || out != nil {
		t.Fatalf("HandleClaude: err=%v delivered=%v out=%+v", err, delivered, out)
	}
	if logged == 0 {
		t.Fatal("expected a diagnostic for the missing target")
	}
}

func TestTargetDerivation(t *testing.T) {
	tests := []struct {
		name      string
		to        string
		sessionID string
		env       map[string]string
		want      string
		wantErr   bool
	}{
		{name: "to wins", to: "claude:flag", sessionID: "payload", env: map[string]string{"CLAUDE_CODE_SESSION_ID": "env"}, want: "claude:flag"},
		{name: "payload beats env", sessionID: "payload", env: map[string]string{"CLAUDE_CODE_SESSION_ID": "env"}, want: "claude:payload"},
		{name: "env fallback", env: map[string]string{"CLAUDE_CODE_SESSION_ID": "env"}, want: "claude:env"},
		{name: "bare to means claude", to: "just-a-name", want: "claude:just-a-name"},
		{name: "nothing anywhere", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Target(tt.to, tt.sessionID, func(k string) string { return tt.env[k] })
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Target = %v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Target: %v", err)
			}
			if got.String() != tt.want {
				t.Fatalf("Target = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSelfCheck(t *testing.T) {
	var b strings.Builder
	if code := SelfCheck(&b); code != 0 {
		t.Fatalf("SelfCheck code = %d, want 0", code)
	}
	if strings.TrimSpace(b.String()) != SelfCheckToken {
		t.Fatalf("SelfCheck wrote %q, want %q", b.String(), SelfCheckToken)
	}
}

func TestRenderDeliveryDefaultAckLine(t *testing.T) {
	q := openQueue(t)
	target := agentqueue.Target{Agent: "claude", Name: "s1"}
	item, err := q.Push(target, "hello", nil)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	got := RenderDelivery(q, target, []agentqueue.Item{*item})
	want := "Acknowledge each one after acting on it: agentqueue ack --to claude:s1 <id>"
	if !strings.Contains(got, want) {
		t.Fatalf("RenderDelivery missing default ack line %q:\n%s", want, got)
	}
}

func TestHandleClaudeUsesQueueAckCmd(t *testing.T) {
	q, err := agentqueue.Open(t.TempDir(), agentqueue.WithAckCmd(func(target agentqueue.Target, id string) string {
		return "jill queue ack --to " + target.String() + " " + id
	}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	target := agentqueue.Target{Agent: "claude", Name: "s1"}
	if _, err := q.Push(target, "hello", nil); err != nil {
		t.Fatalf("Push: %v", err)
	}

	out, delivered, err := HandleClaude(q, Payload{HookEventName: "UserPromptSubmit", SessionID: "s1"}, Options{Getenv: noEnv})
	if err != nil || !delivered || out == nil || out.HookSpecificOutput == nil {
		t.Fatalf("HandleClaude: err=%v delivered=%v out=%+v", err, delivered, out)
	}
	ctx := out.HookSpecificOutput.AdditionalContext
	want := "Acknowledge each one after acting on it: jill queue ack --to claude:s1 <id>"
	if !strings.Contains(ctx, want) {
		t.Fatalf("additionalContext missing custom ack line %q:\n%s", want, ctx)
	}
	if strings.Contains(ctx, "agentqueue ack") {
		t.Fatalf("additionalContext still names the default ack command:\n%s", ctx)
	}
}

// A queue customized only for fetch still renders the default ack wording: the
// two customizations are independent through the hookserve rendering path too.
func TestHandleClaudeAckIndependentOfFetch(t *testing.T) {
	q, err := agentqueue.Open(t.TempDir(), agentqueue.WithFetchCmd(func(target agentqueue.Target) string {
		return "jill queue take --to " + target.String() + " --next"
	}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	target := agentqueue.Target{Agent: "claude", Name: "s1"}
	if _, err := q.Push(target, "hello", nil); err != nil {
		t.Fatalf("Push: %v", err)
	}
	out, delivered, err := HandleClaude(q, Payload{HookEventName: "UserPromptSubmit", SessionID: "s1"}, Options{Getenv: noEnv})
	if err != nil || !delivered || out == nil || out.HookSpecificOutput == nil {
		t.Fatalf("HandleClaude: err=%v delivered=%v out=%+v", err, delivered, out)
	}
	ctx := out.HookSpecificOutput.AdditionalContext
	want := "Acknowledge each one after acting on it: agentqueue ack --to claude:s1 <id>"
	if !strings.Contains(ctx, want) {
		t.Fatalf("additionalContext missing default ack line %q:\n%s", want, ctx)
	}
}
