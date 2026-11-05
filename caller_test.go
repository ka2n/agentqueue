package agentqueue

import (
	"path/filepath"
	"testing"
)

func TestResolveCaller(t *testing.T) {
	for _, tc := range []struct {
		name   string
		env    map[string]string
		target string
		want   string
		fails  bool
	}{
		{name: "codex", env: map[string]string{"CODEX_THREAD_ID": "thread"}, want: "codex:thread"},
		{name: "claude", env: map[string]string{"CLAUDE_CODE_SESSION_ID": "session"}, want: "claude:session"},
		{name: "host handoff", env: map[string]string{"CROSSAGENT_AGENT": "pi", "CROSSAGENT_SESSION_ID": "pi-session"}, want: "pi:pi-session"},
		{name: "override", env: map[string]string{"CROSSAGENT_AGENT": "pi", "CROSSAGENT_SESSION_ID": "parent"}, target: "claude:chosen", want: "claude:chosen"},
		{name: "missing", fails: true},
		{name: "ambiguous", env: map[string]string{"CODEX_THREAD_ID": "a", "CLAUDE_CODE_SESSION_ID": "b"}, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			c, err := ResolveCaller(CallerOptions{Target: tc.target, Root: root, Getenv: func(k string) string { return tc.env[k] }})
			if tc.fails {
				if err == nil {
					t.Fatal("expected resolution failure")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Target.String() != tc.want || c.Root != root {
				t.Fatalf("got %+v", c)
			}
		})
	}
}

func TestResolveRoot(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"HOME": home, "XDG_STATE_HOME": filepath.Join(home, "state"), "AGENTQUEUE_ROOT": filepath.Join(home, "queue")}
	getenv := func(k string) string { return env[k] }
	for _, tc := range []struct{ override, clear, want string }{
		{override: filepath.Join(home, "explicit"), want: filepath.Join(home, "explicit")},
		{want: env["AGENTQUEUE_ROOT"]},
		{clear: "AGENTQUEUE_ROOT", want: filepath.Join(home, "state", "agentqueue")},
		{clear: "XDG_STATE_HOME", want: filepath.Join(home, ".local", "state", "agentqueue")},
	} {
		delete(env, tc.clear)
		got, err := ResolveRoot(tc.override, getenv)
		if err != nil || got != tc.want {
			t.Fatalf("got %q, %v; want %q", got, err, tc.want)
		}
	}
}
