package agentqueue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func writeSessionFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}

func setSessionMTime(t *testing.T, path string, value time.Time) {
	t.Helper()
	if err := os.Chtimes(path, value, value); err != nil {
		t.Fatalf("set mtime for %s: %v", path, err)
	}
}

func TestEncodeClaudeCWD(t *testing.T) {
	tests := []struct {
		cwd  string
		want string
	}{
		{cwd: "/home/katsuma/src/github.com/ka2n/jill", want: "-home-katsuma-src-github-com-ka2n-jill"},
		{cwd: "/tmp/project.with_under_score", want: "-tmp-project-with-under-score"},
		{cwd: "/home/u/.config/project_name", want: "-home-u--config-project-name"},
	}
	for _, tt := range tests {
		if got := EncodeClaudeCWD(tt.cwd); got != tt.want {
			t.Errorf("EncodeClaudeCWD(%q) = %q, want %q", tt.cwd, got, tt.want)
		}
	}
}

func TestEncodePiCWD(t *testing.T) {
	tests := []struct {
		cwd  string
		want string
	}{
		{cwd: "/home/katsuma/src/github.com/ka2n/jill", want: "--home-katsuma-src-github.com-ka2n-jill--"},
		{cwd: "/tmp/project.with_under_score", want: "--tmp-project.with_under_score--"},
	}
	for _, tt := range tests {
		if got := EncodePiCWD(tt.cwd); got != tt.want {
			t.Errorf("EncodePiCWD(%q) = %q, want %q", tt.cwd, got, tt.want)
		}
	}
}

func TestClaudeSessionListerUsesCLIJSON(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	cwd := filepath.Join(home, "checkout")
	id := "claude-session-1"
	q, err := Open(root)
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}
	if _, err := q.Push(Target{Agent: "claude", Name: id}, "queued", nil); err != nil {
		t.Fatalf("push queue item: %v", err)
	}
	if err := q.PutAddress(Address{Agent: "claude", Name: id, Cwd: cwd}); err != nil {
		t.Fatalf("put address: %v", err)
	}

	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "fixture-claude" || !cmp.Equal(args, []string{"agents", "--json"}) {
			return nil, errors.New("unexpected command")
		}
		return []byte(`[
  {"id":"short","sessionId":"claude-session-1","cwd":"` + cwd + `","startedAt":1788480000123,"name":"review worker"}
]`), nil
	}
	lister := NewClaudeSessionLister(SessionListerOptions{
		HomeDir:       home,
		QueueRoot:     root,
		ClaudeCommand: "fixture-claude",
		CommandRunner: runner,
	})
	sessions, err := lister.List(context.Background())
	if err != nil {
		t.Fatalf("list Claude sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1: %#v", len(sessions), sessions)
	}
	got := sessions[0]
	if got.SessionID != id || got.Cwd != cwd || got.Label != "review worker" {
		t.Fatalf("session = %#v, want id/cwd/label", got)
	}
	if !got.Mailbox || !got.Registered {
		t.Fatalf("queue status = mailbox %v, registered %v, want both", got.Mailbox, got.Registered)
	}
	if want := time.UnixMilli(1788480000123).UTC(); !got.LastActivity.Equal(want) {
		t.Fatalf("last activity = %s, want %s", got.LastActivity, want)
	}
}

func TestClaudeSessionListerFallsBackToTranscripts(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	config := filepath.Join(home, "claude-config")
	cwd := "/tmp/project.with_under_score"
	project := filepath.Join(config, "projects", EncodeClaudeCWD(cwd))
	newer := filepath.Join(project, "new-session.jsonl")
	older := filepath.Join(project, "old-session.jsonl")
	writeSessionFixture(t, newer, `{"type":"file-history-snapshot"}
{"type":"user","cwd":"`+cwd+`","sessionId":"new-session"}
`)
	writeSessionFixture(t, older, `{"type":"user","cwd":"/tmp/old","sessionId":"old-session"}
`)
	writeSessionFixture(t, filepath.Join(project, "malformed.jsonl"), `{"type":"user","cwd":`)
	writeSessionFixture(t, filepath.Join(project, "truncated.jsonl"), `{"type":"user"`)
	setSessionMTime(t, older, time.Unix(10, 0))
	setSessionMTime(t, newer, time.Unix(20, 0))

	lister := NewClaudeSessionLister(SessionListerOptions{
		HomeDir:         home,
		QueueRoot:       root,
		ClaudeConfigDir: config,
		ClaudeCommand:   "missing-claude",
		CommandRunner: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("command not found")
		},
	})
	sessions, err := lister.List(context.Background())
	if err != nil {
		t.Fatalf("list Claude transcript sessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2 (malformed files skipped): %#v", len(sessions), sessions)
	}
	if sessions[0].SessionID != "new-session" || sessions[1].SessionID != "old-session" {
		t.Fatalf("sessions are not newest first: %#v", sessions)
	}
	if sessions[0].Label != "project.with_under_score" {
		t.Fatalf("fallback label = %q, want project.with_under_score", sessions[0].Label)
	}
	if sessions[0].Mailbox || sessions[0].Registered {
		t.Fatalf("unexpected queue status: %#v", sessions[0])
	}
}

func TestCodexSessionListerScansRolloutsWithoutSQLite(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	codexHome := filepath.Join(home, "codex-home")
	idNew := "019abcde-1111-7777-8888-aaaaaaaaaaaa"
	idOld := "019abcde-2222-7777-8888-bbbbbbbbbbbb"
	newPath := filepath.Join(codexHome, "sessions", "2026", "09", "04", "rollout-2026-09-04T10-00-00-"+idNew+".jsonl")
	oldPath := filepath.Join(codexHome, "sessions", "2026", "09", "03", "rollout-2026-09-03T10-00-00-"+idOld+".jsonl")
	writeSessionFixture(t, newPath, `{"type":"session_meta","payload":{"id":"`+idNew+`","cwd":"/tmp/new-codex"}}
`)
	writeSessionFixture(t, oldPath, `{"type":"session_meta","payload":{"session_id":"`+idOld+`","cwd":"/tmp/old-codex"}}
`)
	writeSessionFixture(t, filepath.Join(codexHome, "sessions", "2026", "09", "04", "rollout-2026-09-04T11-00-00-bad.jsonl"), `{"type":"session_meta"`)
	writeSessionFixture(t, filepath.Join(codexHome, "sessions", "2026", "09", "04", "not-a-rollout.jsonl"), `{"type":"session_meta","payload":{"cwd":"/tmp/no"}}
`)
	writeSessionFixture(t, filepath.Join(codexHome, "session_index.jsonl"), `{"id":"`+idNew+`","thread_name":"new thread"}
not-json
`)
	// A corrupt state DB must not prevent the zero-dependency rollout scan.
	writeSessionFixture(t, filepath.Join(codexHome, "state_1.sqlite"), "not a sqlite database")
	setSessionMTime(t, oldPath, time.Unix(10, 0))
	setSessionMTime(t, newPath, time.Unix(20, 0))

	lister := NewCodexSessionLister(SessionListerOptions{
		HomeDir:   home,
		QueueRoot: root,
		CodexHome: codexHome,
	})
	sessions, err := lister.List(context.Background())
	if err != nil {
		t.Fatalf("list Codex sessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2: %#v", len(sessions), sessions)
	}
	if sessions[0].SessionID != idNew || sessions[1].SessionID != idOld {
		t.Fatalf("sessions are not newest first: %#v", sessions)
	}
	if sessions[0].Label != "new thread" || sessions[1].Label != "old-codex" {
		t.Fatalf("labels = %q, %q", sessions[0].Label, sessions[1].Label)
	}
}

func TestPiSessionListerScansEncodedSessionDirectories(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	piDir := filepath.Join(home, "pi-agent")
	cwd := "/tmp/project.with_under_score"
	id := "01abc123-4567-7890-abcd-ef0123456789"
	path := filepath.Join(piDir, "sessions", EncodePiCWD(cwd), "2026-09-04T10-00-00-000Z_"+id+".jsonl")
	writeSessionFixture(t, path, `{"type":"session","version":3,"id":"`+id+`","timestamp":"2026-09-04T10:00:00.000Z","cwd":"`+cwd+`"}
`)
	writeSessionFixture(t, filepath.Join(piDir, "sessions", EncodePiCWD(cwd), "bad.jsonl"), `{"type":"session"`)
	writeSessionFixture(t, filepath.Join(piDir, "sessions", EncodePiCWD(cwd), "truncated_01bad.jsonl"), `{"type":"message"}
`)
	setSessionMTime(t, path, time.Unix(30, 0))

	lister := NewPiSessionLister(SessionListerOptions{
		HomeDir:    home,
		QueueRoot:  root,
		PiAgentDir: piDir,
	})
	sessions, err := lister.List(context.Background())
	if err != nil {
		t.Fatalf("list Pi sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1: %#v", len(sessions), sessions)
	}
	if sessions[0].SessionID != id || sessions[0].Cwd != cwd || sessions[0].Label != "project.with_under_score" {
		t.Fatalf("session = %#v", sessions[0])
	}
}

func TestSortSessionsNewestFirst(t *testing.T) {
	sessions := []Session{
		{Agent: "pi", SessionID: "old", LastActivity: time.Unix(1, 0)},
		{Agent: "claude", SessionID: "new", LastActivity: time.Unix(2, 0)},
		{Agent: "codex", SessionID: "tie", LastActivity: time.Unix(2, 0)},
	}
	SortSessions(sessions)
	want := []string{"claude:new", "codex:tie", "pi:old"}
	got := make([]string, len(sessions))
	for i, session := range sessions {
		got[i] = session.Agent + ":" + session.SessionID
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("sort mismatch (-want +got):\n%s", diff)
	}
}

func TestSessionListerHandlesMissingStorage(t *testing.T) {
	options := SessionListerOptions{
		HomeDir:   t.TempDir(),
		QueueRoot: t.TempDir(),
		CommandRunner: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("command not found")
		},
	}
	for name, lister := range NewSessionListers(options) {
		t.Run(name, func(t *testing.T) {
			got, err := lister.List(context.Background())
			if err != nil {
				t.Fatalf("list missing %s storage: %v", name, err)
			}
			if len(got) != 0 {
				t.Fatalf("list missing %s storage = %#v, want empty", name, got)
			}
		})
	}
}

func TestSessionListerOptionsHonorEnvironmentHomes(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, "codex")
	id := "019abcde-3333-7777-8888-cccccccccccc"
	path := filepath.Join(codexHome, "sessions", "2026", "09", "04", "rollout-2026-09-04T10-00-00-"+id+".jsonl")
	writeSessionFixture(t, path, `{"type":"session_meta","payload":{"cwd":"/tmp/env-codex"}}
`)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("AGENTQUEUE_ROOT", filepath.Join(home, "queue"))
	sessions, err := NewCodexSessionLister(SessionListerOptions{}).List(context.Background())
	if err != nil {
		t.Fatalf("list Codex sessions from environment: %v", err)
	}
	if len(sessions) != 1 || sessions[0].SessionID != id {
		t.Fatalf("environment-discovered sessions = %#v", sessions)
	}
}

func TestSessionFilenameParsers(t *testing.T) {
	if id, _, ok := parseCodexRolloutName("rollout-2026-09-04T10-00-00-abc.jsonl"); !ok || id != "abc" {
		t.Fatalf("parseCodexRolloutName returned (%q, %v)", id, ok)
	}
	if id, ok := parsePiSessionName("2026-09-04T10-00-00-000Z_abc.jsonl"); !ok || id != "abc" {
		t.Fatalf("parsePiSessionName returned (%q, %v)", id, ok)
	}
	for _, name := range []string{"bad.jsonl", "rollout-bad.jsonl", "2026-09-04.jsonl"} {
		if strings.HasPrefix(name, "rollout-") {
			if _, _, ok := parseCodexRolloutName(name); ok {
				t.Errorf("parseCodexRolloutName(%q) accepted malformed name", name)
			}
		} else if _, ok := parsePiSessionName(name); ok {
			t.Errorf("parsePiSessionName(%q) accepted malformed name", name)
		}
	}
}
