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
	"github.com/ka2n/agentqueue/hookserve"
	"github.com/ka2n/crossagent/agent"
)

// TestRegisterWritesAddressWithoutToken is the security property: the session's
// messaging token is in the environment when a hook runs, and it must not end
// up on disk.
func TestRegisterWritesAddressWithoutToken(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(t.TempDir(), "checkout")
	const token = "sk-messaging-token-do-not-store"
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", token)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "/run/user/1000/claude/s1.sock")
	t.Setenv("CLAUDE_CODE_VERSION", "2.1.0")

	payload := hookPayload(t, map[string]any{
		"hook_event_name": "SessionStart",
		"session_id":      "sess-1",
		"source":          "startup",
		"cwd":             cwd,
	})
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"register", "--root", root}, strings.NewReader(payload), &out, &errOut); code != exitOK {
		t.Fatalf("register exit = %d (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "registered claude:sess-1") {
		t.Fatalf("register output = %q", out.String())
	}

	path := filepath.Join(root, "claude", "sess-1", "addr.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat addr.json: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("addr.json mode = %04o, want 0600", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read addr.json: %v", err)
	}
	if strings.Contains(string(body), token) {
		t.Fatalf("addr.json contains the messaging token:\n%s", body)
	}
	var addr map[string]any
	if err := json.Unmarshal(body, &addr); err != nil {
		t.Fatalf("decode addr.json: %v", err)
	}
	for _, forbidden := range []string{"token", "messaging_token", "CLAUDE_CODE_MESSAGING_TOKEN"} {
		if _, ok := addr[forbidden]; ok {
			t.Fatalf("addr.json has a %q field", forbidden)
		}
	}
	if addr["cwd"] != cwd {
		t.Fatalf("cwd = %v, want %q", addr["cwd"], cwd)
	}
	if addr["socket"] != "/run/user/1000/claude/s1.sock" {
		t.Fatalf("socket = %v, want the diagnostic socket path", addr["socket"])
	}
	if addr["agent_version"] != "2.1.0" {
		t.Fatalf("agent_version = %v, want 2.1.0", addr["agent_version"])
	}
	if pid, ok := addr["pid"].(float64); !ok || pid <= 0 {
		t.Fatalf("pid = %v, want a positive number", addr["pid"])
	}

	// The cwd index is what lets a producer address "the session in this
	// directory" without knowing the session id.
	byCwd := filepath.Join(root, "claude", "_by_cwd")
	dirs, err := os.ReadDir(byCwd)
	if err != nil {
		t.Fatalf("read _by_cwd: %v", err)
	}
	if len(dirs) != 1 {
		t.Fatalf("_by_cwd holds %d entries, want 1", len(dirs))
	}
	markers, err := os.ReadDir(filepath.Join(byCwd, dirs[0].Name()))
	if err != nil {
		t.Fatalf("read cwd index: %v", err)
	}
	if len(markers) != 1 || markers[0].Name() != "sess-1" {
		t.Fatalf("cwd index holds %v, want a sess-1 marker", markers)
	}

	// unregister removes both records.
	out.Reset()
	if code := run(context.Background(), []string{"unregister", "--root", root}, strings.NewReader(payload), &out, &errOut); code != exitOK {
		t.Fatalf("unregister exit = %d (stderr: %s)", code, errOut.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("addr.json still there after unregister: %v", err)
	}
	if _, err := os.Stat(filepath.Join(byCwd, dirs[0].Name(), "sess-1")); !os.IsNotExist(err) {
		t.Fatalf("cwd marker still there after unregister: %v", err)
	}
}

func TestRegisterFallsBackToEnvSessionID(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "from-env")

	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"register", "--root", root}, strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("register exit = %d (stderr: %s)", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(root, "claude", "from-env", "addr.json")); err != nil {
		t.Fatalf("stat addr.json: %v", err)
	}
}

// TestRegisterWithoutASessionIsSilent covers the hook case where the payload
// says nothing and the environment is empty: there is nothing to record, and
// that must not be reported as a failure.
func TestRegisterWithoutASessionIsSilent(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	for _, cmd := range []string{"register", "unregister"} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), []string{cmd, "--root", t.TempDir()}, strings.NewReader(""), &out, &errOut); code != exitOK {
			t.Fatalf("%s exit = %d, want 0 (stderr: %s)", cmd, code, errOut.String())
		}
		if out.String() != "" {
			t.Fatalf("%s printed %q, want nothing", cmd, out.String())
		}
	}
}

func TestUnregisterUnknownSessionIsNotAnError(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"unregister", "--root", t.TempDir(), "--to", "claude:never-there"}, strings.NewReader(""), &out, &errOut)
	if code != exitOK {
		t.Fatalf("unregister exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
}

func TestRegisterTargetDerivation(t *testing.T) {
	tests := []struct {
		name      string
		to        string
		agent     agent.Name
		sessionID string
		env       map[string]string
		want      string
		wantErr   bool
	}{
		{name: "to wins over agent", to: "codex:thread", agent: agent.Claude, want: "codex:thread"},
		{name: "to alias remains a mailbox name", to: "codex-cli:thread", agent: agent.Claude, want: "codex-cli:thread"},
		{name: "to with a custom agent is accepted", to: "gemini:thread", agent: agent.Claude, want: "gemini:thread"},
		{name: "agent plus payload id", agent: agent.Claude, sessionID: "s1", want: "claude:s1"},
		{name: "pi agent", agent: agent.Pi, sessionID: "s1", want: "pi:s1"},
		{name: "custom agent", agent: agent.Name("custom"), sessionID: "s1", want: "custom:s1"},
		{name: "env fallback", agent: agent.Claude, env: map[string]string{"CLAUDE_CODE_SESSION_ID": "env-id"}, want: "claude:env-id"},
		{name: "nothing to register", agent: agent.Claude, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := hookserve.RegisterTarget(tt.to, tt.agent.String(), tt.sessionID, func(k string) string { return tt.env[k] })
			if tt.wantErr {
				if err == nil {
					t.Fatalf("registerTarget = %v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("registerTarget: %v", err)
			}
			if got.String() != tt.want {
				t.Fatalf("registerTarget = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRegisterAcceptsCustomAgent covers address management for a mailbox
// namespace that crossagent does not know about.
func TestRegisterAcceptsCustomAgent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "custom-session")
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"register", "--root", root, "--agent", "custom"}, strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("register exit = %d (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "registered custom:custom-session") {
		t.Fatalf("register output = %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "custom", "custom-session", "addr.json")); err != nil {
		t.Fatalf("custom address not written: %v", err)
	}

	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"unregister", "--root", root, "--agent", "custom"}, strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("unregister exit = %d (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "unregistered custom:custom-session") {
		t.Fatalf("unregister output = %q", out.String())
	}
}

func TestTargetsListsMailboxes(t *testing.T) {
	root := t.TempDir()
	pushOne(t, root, "claude:s1", "hello")
	// Straight through the library: pushing to a codex target over the CLI
	// would try to notify a codex thread that does not exist.
	q, err := agentqueue.Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := q.Push(agentqueue.Target{Agent: "codex", Name: "thread-9"}, "hello", nil); err != nil {
		t.Fatalf("Push: %v", err)
	}
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"register", "--root", root, "--to", "claude:s1"}, strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("register exit = %d (stderr: %s)", code, errOut.String())
	}

	out.Reset()
	if code := run(context.Background(), []string{"targets", "--root", root}, strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("targets exit = %d (stderr: %s)", code, errOut.String())
	}
	table := out.String()
	for _, want := range []string{"claude:s1", "codex:thread-9", "PENDING", "REGISTERED"} {
		if !strings.Contains(table, want) {
			t.Fatalf("targets table is missing %q:\n%s", want, table)
		}
	}

	out.Reset()
	if code := run(context.Background(), []string{"targets", "--root", root, "--agent", "codex", "--json"}, strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("targets --json exit = %d (stderr: %s)", code, errOut.String())
	}
	var boxes []struct {
		Target  struct{ Agent, Name string }
		Pending int
		Address *struct{ Cwd string }
	}
	if err := json.Unmarshal(out.Bytes(), &boxes); err != nil {
		t.Fatalf("decode targets --json: %v\n%s", err, out.String())
	}
	if len(boxes) != 1 || boxes[0].Target.Name != "thread-9" || boxes[0].Pending != 1 {
		t.Fatalf("targets --json = %+v, want the one codex mailbox with 1 pending", boxes)
	}
	if boxes[0].Address != nil {
		t.Fatalf("codex mailbox reports an address: %+v", boxes[0].Address)
	}
}

func TestTargetsEmptyRoot(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"targets", "--root", t.TempDir()}, strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("targets exit = %d (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "no mailbox") {
		t.Fatalf("targets output = %q", out.String())
	}
}
