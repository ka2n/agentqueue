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
)

// unrelatedSettings is a settings file that already has hooks and other keys
// belonging to somebody else. Nothing in it may change except by addition.
const unrelatedSettings = `{
  "model": "opus",
  "permissions": {
    "allow": ["Bash(ls:*)"]
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {"type": "command", "command": "/usr/bin/mylint"}
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {"type": "command", "command": "/opt/other/notify.sh"}
        ]
      }
    ]
  }
}
`

// fakeCommand is the invocation the tests install, standing in for a real
// agentqueue path. It must never be the user's actual binary.
const fakeCommand = "/tmp/agentqueue-test-bin/agentqueue"

// installArgs builds an install invocation that touches only path.
func installArgs(path string, extra ...string) []string {
	return append([]string{"install", "--agent", "claude", "--settings", path, "--command", fakeCommand}, extra...)
}

// readSettings decodes a settings file for assertions.
func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return out
}

// commandsFor lists the hook commands configured for one event.
func commandsFor(t *testing.T, settings map[string]any, event string) []string {
	t.Helper()
	got := eventCommands(settings, event)
	if got == nil {
		return []string{}
	}
	return got
}

func TestInstallIntoSettingsWithUnrelatedHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(unrelatedSettings), 0o644); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	var out, errOut bytes.Buffer
	if code := run(context.Background(), installArgs(path, "--yes"), strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("install exit = %d (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "installed 5 hook(s)") {
		t.Fatalf("install output = %q", out.String())
	}

	settings := readSettings(t, path)
	// Unrelated top-level keys survive.
	if settings["model"] != "opus" {
		t.Fatalf("model = %v, want opus", settings["model"])
	}
	perms, ok := settings["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions = %v, want the original object", settings["permissions"])
	}
	if diff := cmp.Diff([]any{"Bash(ls:*)"}, perms["allow"]); diff != "" {
		t.Fatalf("permissions.allow mismatch (-want +got):\n%s", diff)
	}
	// Unrelated hooks survive, on their own event and on a shared one.
	if diff := cmp.Diff([]string{"/usr/bin/mylint"}, commandsFor(t, settings, "PreToolUse")); diff != "" {
		t.Fatalf("PreToolUse mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{
		"/opt/other/notify.sh",
		fakeCommand + " hook claude",
	}, commandsFor(t, settings, eventStop)); diff != "" {
		t.Fatalf("Stop mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{
		fakeCommand + " register --agent claude",
		fakeCommand + " hook claude",
	}, commandsFor(t, settings, eventSessionStart)); diff != "" {
		t.Fatalf("SessionStart mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{fakeCommand + " hook claude"}, commandsFor(t, settings, eventUserPromptSubmit)); diff != "" {
		t.Fatalf("UserPromptSubmit mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{fakeCommand + " unregister --agent claude"}, commandsFor(t, settings, eventSessionEnd)); diff != "" {
		t.Fatalf("SessionEnd mismatch (-want +got):\n%s", diff)
	}

	// The backup holds the file as it was before the first write.
	backup, err := os.ReadFile(path + settingsBackupSuffix)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backup) != unrelatedSettings {
		t.Fatalf("backup differs from the original file:\n%s", backup)
	}

	// A second install is a no-op that says so.
	out.Reset()
	if code := run(context.Background(), installArgs(path, "--yes"), strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("second install exit = %d (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "already installed") {
		t.Fatalf("second install output = %q, want a no-op notice", out.String())
	}
	if diff := cmp.Diff(settings, readSettings(t, path)); diff != "" {
		t.Fatalf("second install changed the file (-first +second):\n%s", diff)
	}

	// Uninstall takes back exactly what install added.
	out.Reset()
	code := run(context.Background(),
		[]string{"uninstall", "--agent", "claude", "--settings", path, "--command", fakeCommand, "--yes"},
		strings.NewReader(""), &out, &errOut)
	if code != exitOK {
		t.Fatalf("uninstall exit = %d (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "removed 5 hook(s)") {
		t.Fatalf("uninstall output = %q", out.String())
	}
	after := readSettings(t, path)
	if diff := cmp.Diff([]string{"/usr/bin/mylint"}, commandsFor(t, after, "PreToolUse")); diff != "" {
		t.Fatalf("PreToolUse after uninstall (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"/opt/other/notify.sh"}, commandsFor(t, after, eventStop)); diff != "" {
		t.Fatalf("Stop after uninstall (-want +got):\n%s", diff)
	}
	for _, event := range []string{eventSessionStart, eventUserPromptSubmit, eventSessionEnd} {
		if got := commandsFor(t, after, event); len(got) != 0 {
			t.Fatalf("%s after uninstall = %v, want the key pruned", event, got)
		}
	}
	if after["model"] != "opus" {
		t.Fatalf("uninstall lost the model key: %v", after["model"])
	}
	if strings.Contains(string(mustRead(t, path)), "agentqueue") {
		t.Fatalf("agentqueue survives in the file:\n%s", mustRead(t, path))
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return body
}

func TestInstallIntoMissingFileCreatesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "settings.json")
	var out, errOut bytes.Buffer
	if code := run(context.Background(), installArgs(path, "--yes"), strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("install exit = %d (stderr: %s)", code, errOut.String())
	}
	settings := readSettings(t, path)
	if got := len(commandsFor(t, settings, eventSessionStart)); got != 2 {
		t.Fatalf("SessionStart has %d commands, want 2", got)
	}
	// Nothing existed, so there is nothing to back up.
	if _, err := os.Stat(path + settingsBackupSuffix); !os.IsNotExist(err) {
		t.Fatalf("a backup was written for a file that did not exist: %v", err)
	}
}

func TestInstallDryRunWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	var out, errOut bytes.Buffer
	if code := run(context.Background(), installArgs(path, "--dry-run"), strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("install --dry-run exit = %d (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "--dry-run: nothing written") {
		t.Fatalf("dry-run output = %q", out.String())
	}
	if !strings.Contains(out.String(), "hookEventName") && !strings.Contains(out.String(), "SessionStart") {
		t.Fatalf("dry-run did not print the block it would add:\n%s", out.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("--dry-run created %s", path)
	}
}

func TestInstallPrintEmitsPasteableBlock(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"install", "--print", "--command", fakeCommand}, strings.NewReader(""), &out, &errOut)
	if code != exitOK {
		t.Fatalf("install --print exit = %d (stderr: %s)", code, errOut.String())
	}
	var block struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(out.Bytes(), &block); err != nil {
		t.Fatalf("--print output is not the hooks block (%v):\n%s", err, out.String())
	}
	var events []string
	for event := range block.Hooks {
		events = append(events, event)
	}
	want := []string{eventSessionEnd, eventSessionStart, eventStop, eventUserPromptSubmit}
	if diff := cmp.Diff(want, sorted(events)); diff != "" {
		t.Fatalf("--print events mismatch (-want +got):\n%s", diff)
	}
	for _, group := range block.Hooks[eventSessionStart] {
		for _, h := range group.Hooks {
			if h.Type != "command" {
				t.Fatalf("hook type = %q, want command", h.Type)
			}
			if !strings.HasPrefix(h.Command, fakeCommand+" ") {
				t.Fatalf("hook command = %q, want the --command invocation", h.Command)
			}
		}
	}
}

// sorted returns a sorted copy, so table comparisons do not depend on map
// iteration order.
func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// TestInstallWithoutTTYNeverPrompts is what keeps install safe to run from a
// script: no terminal means no write.
func TestInstallWithoutTTYNeverPrompts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"install", "--settings", path, "--command", fakeCommand}, strings.NewReader(""), &out, &errOut)
	if code != exitOK {
		t.Fatalf("install exit = %d (stderr: %s)", code, errOut.String())
	}
	for _, want := range []string{"AGENT", "claude", "--agent"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("install output is missing %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("install wrote %s without a terminal", path)
	}
}

// TestInstallWithoutConfirmationRefusesToWrite covers --agent with no --yes and
// no terminal: the confirmation cannot be asked, so nothing is written.
func TestInstallWithoutConfirmationRefusesToWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	var out, errOut bytes.Buffer
	code := run(context.Background(), installArgs(path), strings.NewReader(""), &out, &errOut)
	if code != exitError {
		t.Fatalf("install exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(errOut.String(), "confirmation") {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("install wrote %s without confirmation", path)
	}
}

func TestUninstallWithNothingInstalled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(unrelatedSettings), 0o644); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	var out, errOut bytes.Buffer
	code := run(context.Background(),
		[]string{"uninstall", "--settings", path, "--command", fakeCommand, "--yes"},
		strings.NewReader(""), &out, &errOut)
	if code != exitOK {
		t.Fatalf("uninstall exit = %d (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "nothing to do") {
		t.Fatalf("uninstall output = %q", out.String())
	}
	if diff := cmp.Diff(unrelatedSettings, string(mustRead(t, path))); diff != "" {
		t.Fatalf("uninstall rewrote a file it had nothing to change (-want +got):\n%s", diff)
	}
}

func TestUninstallOtherAgentWritesNothing(t *testing.T) {
	var out, errOut bytes.Buffer
	for _, agent := range []string{"codex", "pi"} {
		out.Reset()
		code := run(context.Background(), []string{"uninstall", "--agent", agent, "--command", fakeCommand}, strings.NewReader(""), &out, &errOut)
		if code != exitOK {
			t.Fatalf("uninstall --agent %s exit = %d (stderr: %s)", agent, code, errOut.String())
		}
		if !strings.Contains(out.String(), "nothing to remove") {
			t.Fatalf("uninstall --agent %s output = %q", agent, out.String())
		}
	}
}

func TestInstallUnknownAgent(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"install", "--agent", "frobnicate", "--command", fakeCommand}, strings.NewReader(""), &out, &errOut)
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(errOut.String(), "unknown agent") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestSettingsPath(t *testing.T) {
	tests := []struct {
		name     string
		scope    string
		override string
		env      map[string]string
		want     string
		wantErr  bool
	}{
		{name: "override wins", scope: scopeUser, override: "/tmp/x.json", env: map[string]string{"HOME": "/home/u"}, want: "/tmp/x.json"},
		{name: "user scope", scope: scopeUser, env: map[string]string{"HOME": "/home/u"}, want: "/home/u/.claude/settings.json"},
		{name: "empty scope is user", env: map[string]string{"HOME": "/home/u"}, want: "/home/u/.claude/settings.json"},
		{name: "project scope", scope: scopeProject, want: filepath.Join(".claude", "settings.json")},
		{name: "local scope", scope: scopeLocal, want: filepath.Join(".claude", "settings.local.json")},
		{name: "unknown scope", scope: "global", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := settingsPath(tt.scope, tt.override, func(k string) string { return tt.env[k] })
			if tt.wantErr {
				if err == nil {
					t.Fatalf("settingsPath = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("settingsPath: %v", err)
			}
			if got != tt.want {
				t.Fatalf("settingsPath = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveHookCommand(t *testing.T) {
	found := func(string) (string, error) { return "/usr/local/bin/agentqueue", nil }
	missing := func(string) (string, error) { return "", os.ErrNotExist }

	tests := []struct {
		name     string
		override string
		exe      string
		look     func(string) (string, error)
		want     string
		wantErr  bool
	}{
		{name: "override wins", override: "/opt/aq/agentqueue", exe: "/tmp/x", look: found, want: "/opt/aq/agentqueue"},
		{name: "override with a space is quoted", override: "/opt/my tools/agentqueue", look: found, want: `'/opt/my tools/agentqueue'`},
		{name: "bare name when on PATH", exe: "/tmp/go-build/exe/agentqueue", look: found, want: "agentqueue"},
		{name: "absolute path when not on PATH", exe: "/tmp/go-build/exe/agentqueue", look: missing, want: "/tmp/go-build/exe/agentqueue"},
		{name: "no path at all", exe: "", look: missing, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveHookCommand(tt.override, tt.exe, tt.look)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveHookCommand = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveHookCommand: %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveHookCommand = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOwnedCommand(t *testing.T) {
	const invocation = "/opt/aq/agentqueue"
	tests := []struct {
		cmd  string
		want bool
	}{
		{cmd: "/opt/aq/agentqueue hook claude", want: true},
		{cmd: "agentqueue hook claude", want: true},
		{cmd: "/usr/local/bin/agentqueue register --agent claude", want: true},
		{cmd: "/usr/bin/mylint", want: false},
		{cmd: "/opt/other/notify.sh --agentqueue", want: false},
		{cmd: "echo agentqueue", want: false},
		{cmd: "", want: false},
	}
	for _, tt := range tests {
		if got := ownedCommand(tt.cmd, invocation); got != tt.want {
			t.Fatalf("ownedCommand(%q) = %v, want %v", tt.cmd, got, tt.want)
		}
	}
}

func TestActionKey(t *testing.T) {
	tests := []struct{ in, want string }{
		{in: "agentqueue hook claude", want: "hook claude"},
		{in: "agentqueue hook claude --log /tmp/x --max 3", want: "hook claude"},
		{in: "agentqueue register --agent claude", want: "register"},
		{in: "agentqueue unregister", want: "unregister"},
		{in: "agentqueue", want: ""},
	}
	for _, tt := range tests {
		if got := actionKey(tt.in); got != tt.want {
			t.Fatalf("actionKey(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestInstallDoesNotDuplicateCustomizedEntries covers a hook the user edited by
// hand: the flags differ, but it is the same integration point, so install must
// leave it alone rather than adding a second copy.
func TestInstallDoesNotDuplicateCustomizedEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	seed := map[string]any{
		"hooks": map[string]any{
			eventStop: []any{map[string]any{"hooks": []any{
				map[string]any{"type": "command", "command": fakeCommand + " hook claude --log /tmp/aq.log --no-block"},
			}}},
		},
	}
	body, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	var out, errOut bytes.Buffer
	if code := run(context.Background(), installArgs(path, "--yes"), strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("install exit = %d (stderr: %s)", code, errOut.String())
	}
	settings := readSettings(t, path)
	if diff := cmp.Diff([]string{fakeCommand + " hook claude --log /tmp/aq.log --no-block"}, commandsFor(t, settings, eventStop)); diff != "" {
		t.Fatalf("Stop mismatch (-want +got):\n%s", diff)
	}
	if !strings.Contains(out.String(), "installed 4 hook(s)") {
		t.Fatalf("install output = %q, want the 4 missing entries only", out.String())
	}
}

// TestIsTTYRejectsDevNull is the case a service manager, a cron job or a hook
// actually hands a process: a character device that is not a terminal. Reading
// it as interactive would make install prompt where nobody can answer.
func TestIsTTYRejectsDevNull(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer devNull.Close()
	if isTTY(devNull) {
		t.Fatalf("isTTY(%s) = true, want false", os.DevNull)
	}
	if isTTY(strings.NewReader("")) {
		t.Fatal("isTTY(non-file reader) = true, want false")
	}
	regular, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer regular.Close()
	if isTTY(regular) {
		t.Fatal("isTTY(regular file) = true, want false")
	}
}
