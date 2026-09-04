package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/ka2n/agentqueue"
)

// hookPayload renders a hook stdin payload.
func hookPayload(t *testing.T, fields map[string]any) string {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return string(body)
}

// pushOne enqueues one message and returns its id.
func pushOne(t *testing.T, root, to, text string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"push", "--root", root, "--to", to, text}, strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("push exit = %d (stderr: %s)", code, errOut.String())
	}
	return strings.TrimSpace(out.String())
}

// runHook feeds a payload to `hook claude` and returns stdout.
func runHook(t *testing.T, root, payload string, extra ...string) string {
	t.Helper()
	args := append([]string{"hook", "claude", "--root", root}, extra...)
	var out, errOut bytes.Buffer
	if code := run(context.Background(), args, strings.NewReader(payload), &out, &errOut); code != exitOK {
		t.Fatalf("hook exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	return out.String()
}

// countIn returns how many item files sit in one state directory.
func countIn(t *testing.T, root, agent, name, state string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, agent, name, state))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read %s: %v", state, err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

func TestHookClaudeDeliversPerEvent(t *testing.T) {
	tests := []struct {
		name         string
		payload      map[string]any
		pending      int
		args         []string
		wantSilent   bool
		wantEvent    string
		wantDecision string
		wantBodies   []string
		wantItems    int
		wantClaimed  int
	}{
		{
			name:        "session start delivers and blocks nothing",
			payload:     map[string]any{"hook_event_name": "SessionStart", "session_id": "s1", "source": "startup"},
			pending:     1,
			wantEvent:   "SessionStart",
			wantBodies:  []string{"message 1"},
			wantClaimed: 1,
		},
		{
			name:        "session start on resume delivers",
			payload:     map[string]any{"hook_event_name": "SessionStart", "session_id": "s1", "source": "resume"},
			pending:     1,
			wantEvent:   "SessionStart",
			wantClaimed: 1,
		},
		{
			name:       "session start on compact does not deliver",
			payload:    map[string]any{"hook_event_name": "SessionStart", "session_id": "s1", "source": "compact"},
			pending:    1,
			wantSilent: true,
		},
		{
			name:        "user prompt submit delivers",
			payload:     map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "s1", "prompt": "hi"},
			pending:     1,
			wantEvent:   "UserPromptSubmit",
			wantClaimed: 1,
		},
		{
			name:         "stop blocks so the turn continues",
			payload:      map[string]any{"hook_event_name": "Stop", "session_id": "s1", "stop_hook_active": false},
			pending:      1,
			wantEvent:    "Stop",
			wantDecision: "block",
			wantClaimed:  1,
		},
		{
			name:        "stop does not block again while a stop hook is active",
			payload:     map[string]any{"hook_event_name": "Stop", "session_id": "s1", "stop_hook_active": true},
			pending:     1,
			wantEvent:   "Stop",
			wantClaimed: 1,
		},
		{
			name:        "no-block suppresses the decision",
			payload:     map[string]any{"hook_event_name": "Stop", "session_id": "s1", "stop_hook_active": false},
			pending:     1,
			args:        []string{"--no-block"},
			wantEvent:   "Stop",
			wantClaimed: 1,
		},
		{
			name:         "subagent stop blocks too",
			payload:      map[string]any{"hook_event_name": "SubagentStop", "session_id": "s1", "stop_hook_active": false},
			pending:      1,
			wantEvent:    "SubagentStop",
			wantDecision: "block",
			wantClaimed:  1,
		},
		{
			name:       "nothing pending prints nothing",
			payload:    map[string]any{"hook_event_name": "Stop", "session_id": "s1"},
			pending:    0,
			wantSilent: true,
		},
		{
			name:       "unhandled event prints nothing",
			payload:    map[string]any{"hook_event_name": "PreCompact", "session_id": "s1"},
			pending:    1,
			wantSilent: true,
		},
		{
			name:       "no session id anywhere prints nothing",
			payload:    map[string]any{"hook_event_name": "Stop"},
			pending:    1,
			wantSilent: true,
		},
		{
			name:        "max caps how many are claimed",
			payload:     map[string]any{"hook_event_name": "Stop", "session_id": "s1", "stop_hook_active": true},
			pending:     4,
			args:        []string{"--max", "2"},
			wantEvent:   "Stop",
			wantItems:   2,
			wantClaimed: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for i := 1; i <= tt.pending; i++ {
				pushOne(t, root, "claude:s1", "message "+string(rune('0'+i)))
			}

			stdout := runHook(t, root, hookPayload(t, tt.payload), tt.args...)

			if tt.wantSilent {
				if stdout != "" {
					t.Fatalf("stdout = %q, want empty", stdout)
				}
				if got := countIn(t, root, "claude", "s1", "pending"); got != tt.pending {
					t.Fatalf("pending = %d, want %d untouched", got, tt.pending)
				}
				return
			}

			var got hookOutput
			if err := json.Unmarshal([]byte(stdout), &got); err != nil {
				t.Fatalf("stdout is not hook JSON (%v): %s", err, stdout)
			}
			if got.HookSpecificOutput == nil {
				t.Fatalf("no hookSpecificOutput in %s", stdout)
			}
			if got.HookSpecificOutput.HookEventName != tt.wantEvent {
				t.Fatalf("hookEventName = %q, want %q", got.HookSpecificOutput.HookEventName, tt.wantEvent)
			}
			if got.Decision != tt.wantDecision {
				t.Fatalf("decision = %q, want %q", got.Decision, tt.wantDecision)
			}
			if tt.wantDecision == "block" && strings.TrimSpace(got.Reason) == "" {
				t.Fatal("decision block with no reason")
			}
			ctx := got.HookSpecificOutput.AdditionalContext
			for _, want := range tt.wantBodies {
				if !strings.Contains(ctx, want) {
					t.Fatalf("additionalContext %q does not carry %q", ctx, want)
				}
			}
			if tt.wantItems > 0 {
				// Item ids embed a millisecond timestamp, so pushes inside one
				// millisecond have no defined order; count what arrived rather
				// than naming which bodies.
				if got := strings.Count(ctx, "\n--- id "); got != tt.wantItems {
					t.Fatalf("additionalContext carries %d item(s), want %d:\n%s", got, tt.wantItems, ctx)
				}
			}
			if got := countIn(t, root, "claude", "s1", "claimed"); got != tt.wantClaimed {
				t.Fatalf("claimed = %d, want %d", got, tt.wantClaimed)
			}
			if got := countIn(t, root, "claude", "s1", "pending"); got != tt.pending-tt.wantClaimed {
				t.Fatalf("pending = %d, want %d", got, tt.pending-tt.wantClaimed)
			}
		})
	}
}

// TestHookClaudeContextExplainsItself checks the parts of the injected text the
// agent needs: who sent it, that it is claimed, and how to acknowledge it.
func TestHookClaudeContextExplainsItself(t *testing.T) {
	root := t.TempDir()
	var out, errOut bytes.Buffer
	code := run(context.Background(),
		[]string{"push", "--root", root, "--to", "claude:s1", "--meta", "from=ci", "the build is green"},
		strings.NewReader(""), &out, &errOut)
	if code != exitOK {
		t.Fatalf("push exit = %d (stderr: %s)", code, errOut.String())
	}
	id := strings.TrimSpace(out.String())

	stdout := runHook(t, root, hookPayload(t, map[string]any{
		"hook_event_name": "UserPromptSubmit", "session_id": "s1",
	}))
	var got hookOutput
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode hook output: %v", err)
	}
	ctx := got.HookSpecificOutput.AdditionalContext
	for _, want := range []string{
		id,
		"the build is green",
		"meta from=ci",
		"external producer",
		"not written by the user",
		"already claimed",
		"agentqueue ack --to claude:s1",
	} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("additionalContext is missing %q:\n%s", want, ctx)
		}
	}
}

// TestHookClaudeDeliversOnceAcrossEvents is the property that lets all three
// delivery hooks be installed at the same time.
func TestHookClaudeDeliversOnceAcrossEvents(t *testing.T) {
	root := t.TempDir()
	pushOne(t, root, "claude:s1", "only once")

	delivered := 0
	for _, payload := range []map[string]any{
		{"hook_event_name": "SessionStart", "session_id": "s1", "source": "startup"},
		{"hook_event_name": "UserPromptSubmit", "session_id": "s1"},
		{"hook_event_name": "Stop", "session_id": "s1", "stop_hook_active": false},
	} {
		if stdout := runHook(t, root, hookPayload(t, payload)); strings.Contains(stdout, "only once") {
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatalf("the item was delivered %d times, want exactly 1", delivered)
	}
	if got := countIn(t, root, "claude", "s1", "claimed"); got != 1 {
		t.Fatalf("claimed = %d, want 1", got)
	}
}

func TestHookClaudeMalformedStdinIsQuiet(t *testing.T) {
	root := t.TempDir()
	pushOne(t, root, "claude:s1", "still here")

	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"hook", "claude", "--root", root}, strings.NewReader("{not json"), &out, &errOut)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if out.String() != "" {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
	if got := countIn(t, root, "claude", "s1", "pending"); got != 1 {
		t.Fatalf("pending = %d, want the item left alone", got)
	}
}

func TestHookClaudeEmptyStdinIsQuiet(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"hook", "claude", "--root", t.TempDir()}, strings.NewReader(""), &out, &errOut)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if out.String() != "" {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
}

func TestHookClaudeBadFlagIsQuiet(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"hook", "claude", "--nope"}, strings.NewReader(""), &out, &errOut)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0 so a misconfigured hook cannot break a session", code)
	}
	if out.String() != "" {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
}

func TestHookClaudeUsesToOverrideAndLog(t *testing.T) {
	root := t.TempDir()
	pushOne(t, root, "claude:other", "for the override")
	logPath := filepath.Join(t.TempDir(), "hook.log")

	stdout := runHook(t, root,
		hookPayload(t, map[string]any{"hook_event_name": "Stop", "session_id": "s1"}),
		"--to", "claude:other", "--log", logPath)
	if !strings.Contains(stdout, "for the override") {
		t.Fatalf("stdout %q did not read the --to mailbox", stdout)
	}
	if got := countIn(t, root, "claude", "other", "claimed"); got != 1 {
		t.Fatalf("claimed in the override mailbox = %d, want 1", got)
	}
}

func TestHookClaudeSessionStartRegistersAddress(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(t.TempDir(), "work")
	runHook(t, root, hookPayload(t, map[string]any{
		"hook_event_name": "SessionStart", "session_id": "s1", "source": "startup", "cwd": cwd,
	}))

	body, err := os.ReadFile(filepath.Join(root, "claude", "s1", "addr.json"))
	if err != nil {
		t.Fatalf("read addr.json: %v", err)
	}
	var addr map[string]any
	if err := json.Unmarshal(body, &addr); err != nil {
		t.Fatalf("decode addr.json: %v", err)
	}
	if diff := cmp.Diff(cwd, addr["cwd"]); diff != "" {
		t.Fatalf("recorded cwd mismatch (-want +got):\n%s", diff)
	}
}

func TestHookDispatchRejectsUnknownAgent(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"hook", "codex"}, strings.NewReader(""), &out, &errOut)
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errOut.String(), "no hook integration") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestHookClaudeReadsCustomMailboxWithoutTransport(t *testing.T) {
	root := t.TempDir()
	q, err := agentqueue.Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	item, err := q.Push(agentqueue.Target{Agent: "custom", Name: "x"}, "custom hook body", nil)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	stdout := runHook(t, root, hookPayload(t, map[string]any{
		"hook_event_name": "UserPromptSubmit", "session_id": "ignored",
	}), "--to", "custom:x")
	if !strings.Contains(stdout, "custom hook body") || !strings.Contains(stdout, item.ID) {
		t.Fatalf("hook output = %q, want the custom item", stdout)
	}
	if got := countIn(t, root, "custom", "x", "claimed"); got != 1 {
		t.Fatalf("claimed = %d, want one custom item", got)
	}
}

func TestHookTargetDerivation(t *testing.T) {
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
		{name: "blank env is nothing", env: map[string]string{"CLAUDE_CODE_SESSION_ID": "  "}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := hookTarget(tt.to, tt.sessionID, func(k string) string { return tt.env[k] })
			if tt.wantErr {
				if err == nil {
					t.Fatalf("hookTarget = %v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("hookTarget: %v", err)
			}
			if got.String() != tt.want {
				t.Fatalf("hookTarget = %q, want %q", got, tt.want)
			}
		})
	}
}
