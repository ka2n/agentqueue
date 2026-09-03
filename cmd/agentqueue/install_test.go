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

// TestInstallDiffShowsTheRealChange is the presentation half of the safety
// story: what install prints has to be the file's before/after, not just the
// block it adds, because a block on its own reads like a replacement.
func TestInstallDiffShowsTheRealChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(unrelatedSettings), 0o644); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), installArgs(path, "--diff"), strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("install --diff exit = %d (stderr: %s)", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{
		path,
		"(exists,",
		"change:  +5 hook entries, -0 removed, 0 modified",
		// Every event in the file is accounted for, including the one
		// agentqueue never touches.
		"PreToolUse: 1 existing entry kept, none added",
		"Stop: 1 existing entry kept, 1 added",
		"SessionStart: 0 existing entries kept, 2 added (event not in the file yet)",
		"--- " + path,
		"+++ " + path,
		"@@ ",
		`+            "command": "` + fakeCommand + ` hook claude"`,
		"key order on disk will change",
		"backup:  " + path + settingsBackupSuffix,
		"--diff: nothing written",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("install --diff output is missing %q:\n%s", want, got)
		}
	}
	// A diff must never show a removal here: the merge only appends.
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "--") {
			t.Fatalf("install --diff shows a deleted line %q:\n%s", line, got)
		}
	}
	if diff := cmp.Diff(unrelatedSettings, string(mustRead(t, path))); diff != "" {
		t.Fatalf("--diff changed the file (-want +got):\n%s", diff)
	}
	if _, err := os.Stat(path + settingsBackupSuffix); !os.IsNotExist(err) {
		t.Fatalf("--diff took a backup, so it touched the disk: %v", err)
	}
}

// TestInstallRefusesAnUnsafeMerge covers a settings file whose event value is
// not an array of hook groups. The merge would replace it, so the invariant
// check has to stop the write.
func TestInstallRefusesAnUnsafeMerge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	const seed = `{"hooks": {"Stop": "see the other file"}}` + "\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), installArgs(path, "--yes"), strings.NewReader(""), &out, &errOut); code != exitError {
		t.Fatalf("install exit = %d, want %d (stdout: %s)", code, exitError, out.String())
	}
	for _, want := range []string{"refusing to write", "Stop"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("stderr = %q, want it to mention %q", errOut.String(), want)
		}
	}
	if diff := cmp.Diff(seed, string(mustRead(t, path))); diff != "" {
		t.Fatalf("a refused install still wrote to the file (-want +got):\n%s", diff)
	}
}

// TestUninstallDiffShowsTheRealChange is the mirror: removals, and only ours.
func TestUninstallDiffShowsTheRealChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(unrelatedSettings), 0o644); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), installArgs(path, "--yes"), strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("install exit = %d (stderr: %s)", code, errOut.String())
	}
	installed := mustRead(t, path)

	out.Reset()
	code := run(context.Background(),
		[]string{"uninstall", "--agent", "claude", "--settings", path, "--command", fakeCommand, "--diff"},
		strings.NewReader(""), &out, &errOut)
	if code != exitOK {
		t.Fatalf("uninstall --diff exit = %d (stderr: %s)", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{
		"change:  +0 hook entries, -5 removed, 0 modified",
		"@@ ",
		"--diff: nothing written",
		`-            "command": "` + fakeCommand + ` hook claude"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("uninstall --diff output is missing %q:\n%s", want, got)
		}
	}
	// Nothing belonging to somebody else may appear as a removal.
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "--") && !strings.Contains(line, "agentqueue") &&
			!strings.Contains(line, `"type": "command"`) && strings.Contains(line, "command") {
			t.Fatalf("uninstall --diff removes a line that is not ours: %q\n%s", line, got)
		}
	}
	if diff := cmp.Diff(string(installed), string(mustRead(t, path))); diff != "" {
		t.Fatalf("uninstall --diff changed the file (-want +got):\n%s", diff)
	}
}

// TestInstallWarnsAboutABarePathCommand covers the one caveat of the default
// invocation: a hook does not inherit the installing shell's $PATH.
func TestInstallWarnsAboutABarePathCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	var out, errOut bytes.Buffer
	code := run(context.Background(),
		[]string{"install", "--agent", "claude", "--settings", path, "--command", "agentqueue", "--diff"},
		strings.NewReader(""), &out, &errOut)
	if code != exitOK {
		t.Fatalf("install --diff exit = %d (stderr: %s)", code, errOut.String())
	}
	for _, want := range []string{"bare name `agentqueue`", "--command <absolute path>"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output is missing the $PATH note %q:\n%s", want, out.String())
		}
	}
	// The absolute-path invocation has nothing to warn about.
	out.Reset()
	if code := run(context.Background(), installArgs(path, "--diff"), strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("install --diff exit = %d (stderr: %s)", code, errOut.String())
	}
	if strings.Contains(out.String(), "bare name") {
		t.Fatalf("an absolute --command still warns about $PATH:\n%s", out.String())
	}
}

// TestInstallTableNamesTheDestinationPerAgent keeps the choice informed: the
// file each option would write is printed before the prompt.
func TestInstallTableNamesTheDestinationPerAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"install", "--settings", path, "--command", fakeCommand}, strings.NewReader(""), &out, &errOut)
	if code != exitOK {
		t.Fatalf("install exit = %d (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "claude hooks are written to "+path) {
		t.Fatalf("output does not name claude's destination:\n%s", out.String())
	}
	for _, agent := range []string{"codex", "pi"} {
		if !strings.Contains(out.String(), agent+" ") {
			t.Fatalf("output says nothing about %s:\n%s", agent, out.String())
		}
	}
}

// TestSettingsWriteIsAtomicAndKeepsTheMode covers the write itself: it goes
// through a temp file and a rename, so an interrupt cannot truncate somebody's
// settings.json, and the file's own mode survives the rewrite.
func TestSettingsWriteIsAtomicAndKeepsTheMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(unrelatedSettings), 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), installArgs(path, "--yes"), strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("install exit = %d (stderr: %s)", code, errOut.String())
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat settings: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("settings mode = %o, want 0600: a chmodded config must not come back more permissive", got)
	}
	// The backup is a copy of that file, so it is not more permissive either.
	backup, err := os.Stat(path + settingsBackupSuffix)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if got := backup.Mode().Perm(); got != 0o600 {
		t.Fatalf("backup mode = %o, want 0600", got)
	}

	// Nothing is left in the directory but the settings file and its backup:
	// the temp file the write went through is gone.
	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	want := map[string]bool{"settings.json": true, "settings.json" + settingsBackupSuffix: true}
	for _, e := range names {
		if !want[e.Name()] {
			t.Fatalf("the write left %q behind in %s", e.Name(), dir)
		}
	}

	// A new file, which has no mode to inherit, gets the default.
	fresh := filepath.Join(dir, "fresh", "settings.json")
	out.Reset()
	if code := run(context.Background(), installArgs(fresh, "--yes"), strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("install exit = %d (stderr: %s)", code, errOut.String())
	}
	info, err = os.Stat(fresh)
	if err != nil {
		t.Fatalf("stat fresh settings: %v", err)
	}
	if got := info.Mode().Perm(); got != defaultSettingsMode {
		t.Fatalf("new settings mode = %o, want %o", got, defaultSettingsMode)
	}
}

// TestInstallConvergesOnTheCurrentCommandPath is the reinstall-after-the-binary-
// moved case. ownedCommand recognises the old entries as ours whatever path
// they run, and missingEntries keys on the subcommand, so without this the
// reinstall would report "already installed" and leave the stale path in
// place.
func TestInstallConvergesOnTheCurrentCommandPath(t *testing.T) {
	const oldCommand = "/old/place/agentqueue"
	const newCommand = "/new/place/agentqueue"

	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(unrelatedSettings), 0o644); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	var out, errOut bytes.Buffer
	code := run(context.Background(),
		[]string{"install", "--agent", "claude", "--settings", path, "--command", oldCommand, "--yes"},
		strings.NewReader(""), &out, &errOut)
	if code != exitOK {
		t.Fatalf("first install exit = %d (stderr: %s)", code, errOut.String())
	}

	// The binary moved. Reinstalling has to rewrite, not skip.
	out.Reset()
	code = run(context.Background(),
		[]string{"install", "--agent", "claude", "--settings", path, "--command", newCommand, "--yes"},
		strings.NewReader(""), &out, &errOut)
	if code != exitOK {
		t.Fatalf("reinstall exit = %d (stderr: %s)", code, errOut.String())
	}
	wantNotice := "updated 5 hook(s) in " + path + " (command changed from " + oldCommand + " to " + newCommand + ")"
	if !strings.Contains(out.String(), wantNotice) {
		t.Fatalf("reinstall output = %q, want %q", out.String(), wantNotice)
	}
	// The summary tells the truth about the removals rather than hiding them
	// behind the rewrite pairing.
	for _, want := range []string{
		"change:  +5 hook entries, -5 removed, 5 modified",
		"SessionStart: 0 existing entries kept, 2 added, 2 removed, 2 modified",
		"PreToolUse: 1 existing entry kept, none added",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("reinstall output is missing %q:\n%s", want, out.String())
		}
	}

	settings := readSettings(t, path)
	// Exactly one entry per delivery point, all at the new path.
	wantCommands := map[string][]string{
		eventSessionStart:     {newCommand + " register --agent claude", newCommand + " hook claude"},
		eventUserPromptSubmit: {newCommand + " hook claude"},
		eventStop:             {"/opt/other/notify.sh", newCommand + " hook claude"},
		eventSessionEnd:       {newCommand + " unregister --agent claude"},
		// Untouched: agentqueue never wrote here.
		"PreToolUse": {"/usr/bin/mylint"},
	}
	for event, want := range wantCommands {
		if diff := cmp.Diff(want, commandsFor(t, settings, event)); diff != "" {
			t.Fatalf("%s mismatch (-want +got):\n%s", event, diff)
		}
	}
	if strings.Contains(string(mustRead(t, path)), oldCommand) {
		t.Fatalf("the old command path survives the update:\n%s", mustRead(t, path))
	}
	if settings["model"] != "opus" {
		t.Fatalf("the update lost the model key: %v", settings["model"])
	}

	// And a reinstall at the same path is a no-op again, so this converges
	// rather than rewriting on every run.
	out.Reset()
	code = run(context.Background(),
		[]string{"install", "--agent", "claude", "--settings", path, "--command", newCommand, "--yes"},
		strings.NewReader(""), &out, &errOut)
	if code != exitOK {
		t.Fatalf("third install exit = %d (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "already installed") {
		t.Fatalf("same-path reinstall output = %q, want a no-op notice", out.String())
	}
	if diff := cmp.Diff(settings, readSettings(t, path)); diff != "" {
		t.Fatalf("the same-path reinstall changed the file (-first +second):\n%s", diff)
	}
}

// TestInstallUpdateKeepsGroupingAndHandEditedFlags covers what the rewrite has
// to preserve: the wrapper group the user's own hooks share with ours, and a
// flag somebody added to one of our commands by hand.
func TestInstallUpdateKeepsGroupingAndHandEditedFlags(t *testing.T) {
	const newCommand = "/new/place/agentqueue"
	path := filepath.Join(t.TempDir(), "settings.json")
	seed := map[string]any{
		"hooks": map[string]any{
			eventStop: []any{map[string]any{
				"matcher": "*",
				"hooks": []any{
					map[string]any{"type": "command", "command": "/opt/other/notify.sh"},
					map[string]any{"type": "command", "command": "/old/place/agentqueue hook claude --log /tmp/aq.log --no-block"},
				},
			}},
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
	code := run(context.Background(),
		[]string{"install", "--agent", "claude", "--settings", path, "--command", newCommand, "--yes"},
		strings.NewReader(""), &out, &errOut)
	if code != exitOK {
		t.Fatalf("install exit = %d (stderr: %s)", code, errOut.String())
	}
	settings := readSettings(t, path)
	if diff := cmp.Diff([]string{
		"/opt/other/notify.sh",
		newCommand + " hook claude --log /tmp/aq.log --no-block",
	}, commandsFor(t, settings, eventStop)); diff != "" {
		t.Fatalf("Stop mismatch (-want +got):\n%s", diff)
	}
	// The rewritten entry stayed in the user's own wrapper group, matcher and
	// all, rather than being appended as a new group.
	hooks, _ := settings["hooks"].(map[string]any)
	groups, _ := hooks[eventStop].([]any)
	if len(groups) != 1 {
		t.Fatalf("Stop has %d wrapper groups, want the original 1:\n%s", len(groups), mustRead(t, path))
	}
	group, _ := groups[0].(map[string]any)
	if group["matcher"] != "*" {
		t.Fatalf("the wrapper lost its matcher: %v", group["matcher"])
	}
}

// TestInstallRefusesAsyncOwnedEntries covers a hand-edited config: an async
// hook's output is never read, and our output is the delivery, so such an
// entry looks installed and delivers nothing. Writing around it silently
// would keep it that way.
func TestInstallRefusesAsyncOwnedEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	seed := map[string]any{
		"hooks": map[string]any{
			eventStop: []any{map[string]any{"hooks": []any{
				map[string]any{"type": "command", "command": fakeCommand + " hook claude", "async": true},
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
	if code := run(context.Background(), installArgs(path, "--yes"), strings.NewReader(""), &out, &errOut); code != exitError {
		t.Fatalf("install exit = %d, want %d (stdout: %s)", code, exitError, out.String())
	}
	for _, want := range []string{"async", "deliver nothing", eventStop} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("stderr = %q, want it to mention %q", errOut.String(), want)
		}
	}
	if diff := cmp.Diff(string(body), string(mustRead(t, path))); diff != "" {
		t.Fatalf("a refused install still wrote to the file (-want +got):\n%s", diff)
	}
}

// TestInstalledEntriesAreNeverAsync is the invariant behind that refusal: our
// own entries must go in synchronous, because Claude Code only reads the
// output of a synchronous hook and that output is the whole delivery.
func TestInstalledEntriesAreNeverAsync(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	var out, errOut bytes.Buffer
	if code := run(context.Background(), installArgs(path, "--yes"), strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("install exit = %d (stderr: %s)", code, errOut.String())
	}
	if strings.Contains(string(mustRead(t, path)), "async") {
		t.Fatalf("an installed hook entry carries an async field:\n%s", mustRead(t, path))
	}
	// --print is the paste-it-in-yourself path, and must say the same thing.
	out.Reset()
	if code := run(context.Background(), []string{"install", "--print", "--command", fakeCommand}, strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("install --print exit = %d (stderr: %s)", code, errOut.String())
	}
	if strings.Contains(out.String(), "async") {
		t.Fatalf("--print emits an async field:\n%s", out.String())
	}
}

func TestSplitArgv0(t *testing.T) {
	tests := []struct {
		cmd, argv0, rest string
	}{
		{cmd: "agentqueue hook claude", argv0: "agentqueue", rest: " hook claude"},
		{cmd: "/opt/aq/agentqueue hook claude --max 3", argv0: "/opt/aq/agentqueue", rest: " hook claude --max 3"},
		{cmd: "'/opt/my tools/agentqueue' hook claude", argv0: `'/opt/my tools/agentqueue'`, rest: " hook claude"},
		{cmd: "agentqueue", argv0: "agentqueue", rest: ""},
		{cmd: "", argv0: "", rest: ""},
	}
	for _, tt := range tests {
		argv0, rest := splitArgv0(tt.cmd)
		if argv0 != tt.argv0 || rest != tt.rest {
			t.Fatalf("splitArgv0(%q) = (%q, %q), want (%q, %q)", tt.cmd, argv0, rest, tt.argv0, tt.rest)
		}
		if got, want := rewriteArgv0(tt.cmd, "/new/agentqueue"), "/new/agentqueue"+tt.rest; got != want {
			t.Fatalf("rewriteArgv0(%q) = %q, want %q", tt.cmd, got, want)
		}
	}
}

func TestStalePathEntries(t *testing.T) {
	settings := doc(t, `{"hooks": {
		"Stop": [{"hooks": [
			{"type": "command", "command": "/opt/other/notify.sh"},
			{"type": "command", "command": "/old/agentqueue hook claude"}
		]}],
		"SessionStart": [{"hooks": [
			{"type": "command", "command": "/opt/aq/agentqueue register --agent claude"}
		]}]
	}}`)
	if diff := cmp.Diff([]string{"/old/agentqueue hook claude"}, stalePathEntries(settings, "/opt/aq/agentqueue")); diff != "" {
		t.Fatalf("stalePathEntries mismatch (-want +got):\n%s", diff)
	}
	// Nothing is stale when every owned entry already runs this binary.
	current := doc(t, `{"hooks": {"Stop": [{"hooks": [
		{"type": "command", "command": "/opt/aq/agentqueue hook claude"}]}]}}`)
	if got := stalePathEntries(current, "/opt/aq/agentqueue"); len(got) != 0 {
		t.Fatalf("stalePathEntries = %v, want none", got)
	}
	// A hand-added flag is not a stale path: the binary is the same one.
	flagged := doc(t, `{"hooks": {"Stop": [{"hooks": [
		{"type": "command", "command": "/opt/aq/agentqueue hook claude --no-block"}]}]}}`)
	if got := stalePathEntries(flagged, "/opt/aq/agentqueue"); len(got) != 0 {
		t.Fatalf("stalePathEntries = %v, want none for a customized flag", got)
	}
}

// TestSettingsWriteCleansUpAfterAFailure is the other half of the atomic
// write: when the rename cannot happen, the temp file must not be left lying
// next to the user's config. A directory in place of the settings file is a
// cheap way to make the rename fail after the temp file has been written.
func TestSettingsWriteCleansUpAfterAFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("create the directory standing in for the settings file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed the directory: %v", err)
	}
	if err := saveSettings(path, map[string]any{"model": "opus"}); err == nil {
		t.Fatal("saveSettings reported success writing over a directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "settings.json" {
			t.Fatalf("a failed write left %q behind in %s", e.Name(), dir)
		}
	}
}

// TestInstallUpdateAcrossTwoWrapperGroups covers owned entries spread over
// two wrapper groups on the same event, which is what a config that was
// installed twice by different versions looks like: the replacement is one
// entry, in the group the first one was found in, and the group that held
// only the other copy goes away.
func TestInstallUpdateAcrossTwoWrapperGroups(t *testing.T) {
	const newCommand = "/new/place/agentqueue"
	path := filepath.Join(t.TempDir(), "settings.json")
	seed := map[string]any{
		"hooks": map[string]any{
			eventStop: []any{
				map[string]any{"matcher": "first", "hooks": []any{
					map[string]any{"type": "command", "command": "/opt/other/notify.sh"},
					map[string]any{"type": "command", "command": "/old/place/agentqueue hook claude"},
				}},
				map[string]any{"hooks": []any{
					map[string]any{"type": "command", "command": "/older/still/agentqueue hook claude"},
				}},
			},
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
	code := run(context.Background(),
		[]string{"install", "--agent", "claude", "--settings", path, "--command", newCommand, "--yes"},
		strings.NewReader(""), &out, &errOut)
	if code != exitOK {
		t.Fatalf("install exit = %d (stderr: %s)", code, errOut.String())
	}
	settings := readSettings(t, path)
	if diff := cmp.Diff([]string{
		"/opt/other/notify.sh",
		newCommand + " hook claude",
	}, commandsFor(t, settings, eventStop)); diff != "" {
		t.Fatalf("Stop mismatch (-want +got):\n%s", diff)
	}
	hooks, _ := settings["hooks"].(map[string]any)
	groups, _ := hooks[eventStop].([]any)
	if len(groups) != 1 {
		t.Fatalf("Stop has %d wrapper groups, want the emptied one pruned:\n%s", len(groups), mustRead(t, path))
	}
	group, _ := groups[0].(map[string]any)
	if group["matcher"] != "first" {
		t.Fatalf("the surviving wrapper is not the one that held the first owned entry: %v", group["matcher"])
	}
	// Both old paths are gone, and the notice names them.
	after := string(mustRead(t, path))
	for _, gone := range []string{"/old/place/agentqueue", "/older/still/agentqueue"} {
		if strings.Contains(after, gone) {
			t.Fatalf("%s survives the update:\n%s", gone, after)
		}
		if !strings.Contains(out.String(), gone) {
			t.Fatalf("the update notice does not mention %s:\n%s", gone, out.String())
		}
	}
}
