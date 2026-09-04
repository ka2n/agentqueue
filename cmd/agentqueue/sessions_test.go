package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ka2n/agentqueue"
)

func TestCmdSessionsJSONFiltersAndReportsQueueStatus(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	piDir := filepath.Join(home, "pi", "agent")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	id := "01cli-session"
	path := filepath.Join(piDir, "sessions", agentqueue.EncodePiCWD(cwd), "2026-09-04T10-00-00-000Z_"+id+".jsonl")
	body := `{"type":"session","version":3,"id":"` + id + `","timestamp":"2026-09-04T10:00:00.000Z","cwd":"` + cwd + `"}
`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create Pi fixture: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write Pi fixture: %v", err)
	}
	q, err := agentqueue.Open(root)
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}
	if _, err := q.Push(agentqueue.Target{Agent: "pi", Name: id}, "pending", nil); err != nil {
		t.Fatalf("push queue item: %v", err)
	}
	if err := q.PutAddress(agentqueue.Address{Agent: "pi", Name: id, Cwd: cwd}); err != nil {
		t.Fatalf("put queue address: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", piDir)
	t.Setenv("AGENTQUEUE_ROOT", root)

	var out bytes.Buffer
	if err := cmdSessions(context.Background(), []string{"--agent", "pi", "--cwd", ".", "--json"}, &out); err != nil {
		t.Fatalf("cmdSessions: %v", err)
	}
	var sessions []agentqueue.Session
	if err := json.Unmarshal(out.Bytes(), &sessions); err != nil {
		t.Fatalf("parse JSON output: %v\n%s", err, out.String())
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %#v, want one row", sessions)
	}
	if sessions[0].SessionID != id || !sessions[0].Mailbox || !sessions[0].Registered {
		t.Fatalf("session row = %#v, want id with both queue flags", sessions[0])
	}
}

func TestCmdSessionsHumanTableAndLimit(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	piDir := filepath.Join(home, ".pi", "agent")
	cwd := filepath.Join(home, "project")
	for i, id := range []string{"one", "two"} {
		path := filepath.Join(piDir, "sessions", agentqueue.EncodePiCWD(cwd), "2026-09-04T10-00-0"+string(rune('0'+i))+"-000Z_"+id+".jsonl")
		body := `{"type":"session","id":"` + id + `","timestamp":"2026-09-04T10:00:00.000Z","cwd":"` + cwd + `"}
`
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create fixture: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", piDir)
	t.Setenv("AGENTQUEUE_ROOT", root)
	var out bytes.Buffer
	if err := cmdSessions(context.Background(), []string{"--agent", "pi", "--cwd", cwd, "--limit", "1"}, &out); err != nil {
		t.Fatalf("cmdSessions: %v", err)
	}
	got := out.String()
	for _, want := range []string{"AGENT", "SESSION ID", "CWD", "LABEL", "LAST ACTIVITY", "MAILBOX", "REGISTERED", "pi"} {
		if !strings.Contains(got, want) {
			t.Fatalf("table output missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "project") != 2 { // header plus the single row's cwd/label
		t.Fatalf("--limit output = %q, want one data row", got)
	}
}

func TestCmdSessionsRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--agent", "unknown"},
		{"--limit", "-1"},
		{"unexpected"},
	} {
		if err := cmdSessions(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Fatalf("cmdSessions(%v) succeeded, want error", args)
		}
	}
}

func TestRunDispatchesSessions(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENTQUEUE_ROOT", root)
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, "pi", "agent"))
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"sessions", "--agent", "pi", "--json"}, strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("run sessions exit = %d (stderr: %s)", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) != "[]" {
		t.Fatalf("empty sessions JSON = %q, want []", out.String())
	}
}
