package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ka2n/agentqueue"
	"github.com/ka2n/agentqueue/hookserve"
)

func TestHookCodexProtocol(t *testing.T) {
	root := t.TempDir()
	q, err := agentqueue.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	target := agentqueue.Target{Agent: "codex", Name: "thread"}
	if _, err := q.Push(target, "new requirement", nil); err != nil {
		t.Fatal(err)
	}
	var out, diag bytes.Buffer
	code, err := cmdHookCodex([]string{"--root", root}, strings.NewReader(`{"session_id":"thread","hook_event_name":"PostToolUse","tool_name":"Bash","tool_response":{"exit_code":1}}`), &out, &diag)
	if err != nil || code != exitOK {
		t.Fatalf("code=%d err=%v diagnostics=%s", code, err, &diag)
	}
	var got hookserve.Output
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.HookSpecificOutput == nil || !strings.Contains(got.HookSpecificOutput.AdditionalContext, "new requirement") || got.Decision != "" {
		t.Fatalf("output %+v", got)
	}
	out.Reset()
	code, err = cmdHookCodex([]string{"--root", root}, strings.NewReader(`{"session_id":"thread","hook_event_name":"PostToolUse"}`), &out, &diag)
	if err != nil || code != exitOK || out.Len() != 0 {
		t.Fatalf("duplicate delivery: code=%d err=%v out=%s", code, err, &out)
	}
}

func TestHookCodexFailOpenAndSelfCheck(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", "")
	for _, payload := range []string{"{bad json", `{"hook_event_name":"PostToolUse"}`} {
		var out, diag bytes.Buffer
		code, err := cmdHookCodex([]string{"--root", t.TempDir()}, strings.NewReader(payload), &out, &diag)
		if err != nil || code != exitOK || out.Len() != 0 || diag.Len() == 0 {
			t.Fatalf("code=%d err=%v out=%s diagnostics=%s", code, err, &out, &diag)
		}
	}
	var out, diag bytes.Buffer
	code, err := cmdHookCodex([]string{"--self-check"}, strings.NewReader(""), &out, &diag)
	if err != nil || code != exitOK || strings.TrimSpace(out.String()) != hookserve.CodexSelfCheckToken {
		t.Fatalf("self check: %d %v %s", code, err, &out)
	}
}
