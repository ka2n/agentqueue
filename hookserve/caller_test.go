package hookserve

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ka2n/agentqueue"
	"github.com/ka2n/crossagent/processcontext"
)

func TestClaudeExportsCallerWithoutPendingMessages(t *testing.T) {
	root := filepath.Join(t.TempDir(), "queue 'quoted")
	q, err := agentqueue.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "env")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	getenv := func(k string) string {
		if k == "CLAUDE_ENV_FILE" {
			return file.Name()
		}
		return ""
	}
	_, _, err = HandleClaude(q, Payload{HookEventName: "SessionStart", SessionID: "session'quoted", Cwd: t.TempDir()}, Options{Getenv: getenv})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", "-c", `. "$1"; printf '%s\n%s\n%s\n' "$CROSSAGENT_AGENT" "$CROSSAGENT_SESSION_ID" "$AGENTQUEUE_ROOT"`, "sh", file.Name())
	out, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	values := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(values) != 3 || values[2] != root {
		t.Fatalf("invalid export: %q", out)
	}
	env := map[string]string{"CROSSAGENT_AGENT": values[0], "CROSSAGENT_SESSION_ID": values[1]}
	current, err := processcontext.Resolve(processcontext.Options{Getenv: func(k string) string { return env[k] }})
	if err != nil || current.SessionID != "session'quoted" || current.Agent.String() != "claude" {
		t.Fatalf("got %+v, %v", current, err)
	}
}
