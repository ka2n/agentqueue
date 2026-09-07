package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	crosshooks "github.com/ka2n/crossagent/hooks"
)

func writeJSONSettings(t *testing.T, path string, settings map[string]any) {
	t.Helper()
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readJSONSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	return settings
}

func commandEntriesForTest(settings map[string]any, event string) []map[string]any {
	hooks, _ := settings["hooks"].(map[string]any)
	groups, _ := hooks[event].([]any)
	var entries []map[string]any
	for _, rawGroup := range groups {
		group, _ := rawGroup.(map[string]any)
		inner, _ := group["hooks"].([]any)
		for _, rawEntry := range inner {
			if entry, ok := rawEntry.(map[string]any); ok {
				entries = append(entries, entry)
			}
		}
	}
	return entries
}

func assertClaudeCommandEntry(t *testing.T, entry map[string]any) string {
	t.Helper()
	if len(entry) != 2 || entry["type"] != "command" {
		t.Fatalf("Claude hook has unexpected JSON fields: %#v", entry)
	}
	command, ok := entry["command"].(string)
	if !ok || command == "" {
		t.Fatalf("Claude hook has no command: %#v", entry)
	}
	for key := range entry {
		if key != "type" && key != "command" {
			t.Fatalf("Claude hook has unexpected JSON field %q: %#v", key, entry)
		}
	}
	return command
}

func assertClaudeSuffix(t *testing.T, entry map[string]any, wantClean, wantID string) {
	t.Helper()
	command := assertClaudeCommandEntry(t, entry)
	clean, owner, id, marked := crosshooks.ParseCommandSuffixMarker(command)
	if !marked {
		t.Fatalf("Claude command has no suffix marker: %q", command)
	}
	if !strings.HasPrefix(command, clean+" #crossagent:v1:") {
		t.Fatalf("Claude command does not have the expected suffix prefix: %q", command)
	}
	if clean != wantClean || owner != agentqueueToolName || id != wantID {
		t.Fatalf("Claude suffix = clean %q, owner %q, id %q; want clean %q, owner %q, id %q", clean, owner, id, wantClean, agentqueueToolName, wantID)
	}
}

func assertClaudeOwnedEntries(t *testing.T, settings map[string]any, want map[string]map[string]string) {
	t.Helper()
	owns := crosshooks.MatchBasename(agentqueueToolName)
	for event, expected := range want {
		found := make(map[string]int)
		for _, entry := range commandEntriesForTest(settings, event) {
			command, ok := entry["command"].(string)
			if !ok || !owns.Match(crosshooks.HookEntry{Command: command}) {
				continue
			}
			clean, owner, _, marked := crosshooks.ParseCommandSuffixMarker(command)
			if !marked || owner != agentqueueToolName {
				t.Fatalf("owned Claude command is missing its agentqueue suffix: %#v", entry)
			}
			wantID, ok := expected[clean]
			if !ok {
				t.Fatalf("unexpected owned Claude command %q in %s", clean, event)
			}
			assertClaudeSuffix(t, entry, clean, wantID)
			found[clean]++
		}
		for clean := range expected {
			if found[clean] != 1 {
				t.Fatalf("%s has %d entries for clean command %q, want one", event, found[clean], clean)
			}
		}
	}
}

func TestInstallUsesClaudeCommandOwnershipAndPreservesForeignEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	old := "/old/path/agentqueue"
	invocation := "/new/path/agentqueue"
	foreignStart := map[string]any{
		"type": "command", "command": "other-tool start", "vendor": map[string]any{"keep": true},
	}
	foreignMetadata := map[string]any{
		"type": "command", "command": "other-tool stop", "provider": "other-tool", "hookID": "stop",
	}
	foreignStop := map[string]any{
		"type": "command", "command": "other-tool finish", "async": true,
	}
	// Env-wrapped commands are never recognized as agentqueue's own, even
	// when the wrapped command names agentqueue: the strict default matcher
	// does not unwrap `env VAR=value ...`, and agentqueue does not opt into
	// MatchEnvWrapped.
	foreignEnvWrapped := map[string]any{
		"type": "command", "command": "env X=1 agentqueue hook claude",
	}
	writeJSONSettings(t, path, map[string]any{
		"permissions": map[string]any{"allow": []any{"Read"}},
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"matcher": "*",
				"hooks": []any{
					map[string]any{"type": "command", "command": crosshooks.BuildCommandSuffixMarker(old+" register --agent claude", agentqueueToolName, "session-start-register")},
					map[string]any{"type": "command", "command": crosshooks.BuildCommandSuffixMarker(old+" hook claude", agentqueueToolName, "session-start-hook")},
					foreignStart,
					foreignMetadata,
				},
			}},
			"UserPromptSubmit": []any{map[string]any{
				"hooks": []any{map[string]any{"type": "command", "command": crosshooks.BuildCommandSuffixMarker(old+" hook claude", agentqueueToolName, "user-prompt-submit-hook")}},
			}},
			"Stop": []any{map[string]any{
				"matcher": "",
				"hooks": []any{
					map[string]any{"type": "command", "command": crosshooks.BuildCommandSuffixMarker(old+" hook claude", agentqueueToolName, "stop-hook")},
					foreignStop,
					foreignEnvWrapped,
				},
			}},
			"SessionEnd": []any{map[string]any{
				"hooks": []any{map[string]any{"type": "command", "command": crosshooks.BuildCommandSuffixMarker(old+" unregister --agent claude", agentqueueToolName, "session-end-unregister")}},
			}},
		},
	})

	manager, err := claudeConfigManager("user", path, invocation, invocation)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := manager.PlanInstall()
	if err != nil {
		t.Fatal(err)
	}
	if plan.Style != crosshooks.MarkerStyleCommandSuffix {
		t.Fatalf("Claude auto marker style = %q, want command suffix", plan.Style)
	}
	if !plan.HasChanges || plan.Summary.Added != 5 || plan.Summary.Removed != 5 || plan.Summary.Modified != 5 {
		t.Fatalf("stale command convergence plan = %+v", plan)
	}
	var planOutput strings.Builder
	printHookPlan(&planOutput, plan)
	if strings.Contains(strings.ToLower(planOutput.String()), "adopt") {
		t.Fatalf("Claude plan mentions adoption:\n%s", planOutput.String())
	}

	if err := manager.Apply(context.Background(), plan, crosshooks.ApplyOptions{SkipProbe: true}); err != nil {
		t.Fatal(err)
	}
	settings := readJSONSettings(t, path)
	if got := settings["permissions"].(map[string]any)["allow"].([]any)[0]; got != "Read" {
		t.Fatalf("unrelated settings changed: %#v", settings["permissions"])
	}

	wantCommands := map[string]map[string]string{
		string(crosshooks.EventSessionStart): {
			invocation + " register --agent claude": "session-start-register",
			invocation + " hook claude":             "session-start-hook",
		},
		string(crosshooks.EventUserPromptSubmit): {invocation + " hook claude": "user-prompt-submit-hook"},
		string(crosshooks.EventStop):             {invocation + " hook claude": "stop-hook"},
		string(crosshooks.EventSessionEnd):       {invocation + " unregister --agent claude": "session-end-unregister"},
	}
	assertClaudeOwnedEntries(t, settings, wantCommands)

	entries := commandEntriesForTest(settings, string(crosshooks.EventSessionStart))
	var foundStartForeign, foundMetadataForeign bool
	for _, entry := range entries {
		switch entry["command"] {
		case foreignStart["command"]:
			foundStartForeign = true
			if entry["vendor"].(map[string]any)["keep"] != true {
				t.Fatalf("foreign SessionStart entry changed: %#v", entry)
			}
		case foreignMetadata["command"]:
			foundMetadataForeign = true
			if entry["provider"] != foreignMetadata["provider"] || entry["hookID"] != foreignMetadata["hookID"] {
				t.Fatalf("foreign metadata entry changed: %#v", entry)
			}
		}
	}
	if !foundStartForeign || !foundMetadataForeign {
		t.Fatalf("foreign SessionStart entries were changed or removed: %#v", entries)
	}
	entries = commandEntriesForTest(settings, string(crosshooks.EventStop))
	if len(entries) != 3 || entries[1]["command"] != foreignStop["command"] || entries[1]["async"] != true {
		t.Fatalf("foreign Stop entry changed or removed: %#v", entries)
	}
	if entries[2]["command"] != foreignEnvWrapped["command"] {
		t.Fatalf("env-wrapped foreign Stop entry changed or removed: %#v", entries)
	}

	second, err := manager.PlanInstall()
	if err != nil {
		t.Fatal(err)
	}
	if second.HasChanges || second.Summary.Added != 0 || second.Summary.Removed != 0 || second.Summary.Modified != 0 {
		t.Fatalf("same-command reinstall is not a no-op: %+v", second)
	}
	var secondOutput strings.Builder
	if err := applyHookPlan(context.Background(), manager, second, strings.NewReader(""), &secondOutput, true, false, false, true); err != nil {
		t.Fatal(err)
	}
	if got := secondOutput.String(); got != "claude: nothing to do\n" {
		t.Fatalf("same-command reinstall output = %q, want nothing to do", got)
	}
}

func TestInstallWritesClaudeHooksWithSuffixMarkers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	invocation := "/opt/agentqueue"
	manager, err := claudeConfigManager("user", path, invocation, invocation)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := manager.PlanInstall()
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Apply(context.Background(), plan, crosshooks.ApplyOptions{SkipProbe: true}); err != nil {
		t.Fatal(err)
	}

	want := map[string]map[string]string{
		string(crosshooks.EventSessionStart): {
			invocation + " register --agent claude": "session-start-register",
			invocation + " hook claude":             "session-start-hook",
		},
		string(crosshooks.EventUserPromptSubmit): {invocation + " hook claude": "user-prompt-submit-hook"},
		string(crosshooks.EventStop):             {invocation + " hook claude": "stop-hook"},
		string(crosshooks.EventSessionEnd):       {invocation + " unregister --agent claude": "session-end-unregister"},
	}
	settings := readJSONSettings(t, path)
	assertClaudeOwnedEntries(t, settings, want)

	// --print must produce the same durable command marker, while still using
	// only Claude's known type and command fields.
	assertClaudeOwnedEntries(t, hookBlock(claudeHookSpecs(invocation)), want)
}

func TestInstallAfterClaudeHandEditConvergesLostSuffix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	invocation := "/opt/agentqueue"
	manager, err := claudeConfigManager("user", path, invocation, invocation)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := manager.PlanInstall()
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Apply(context.Background(), plan, crosshooks.ApplyOptions{SkipProbe: true}); err != nil {
		t.Fatal(err)
	}

	// Claude preserves the known type and command fields; there are no unknown
	// ownership keys to strip. Simulate a hand edit that removes the suffix and
	// adds a flag to one command.
	settings := readJSONSettings(t, path)
	entries := commandEntriesForTest(settings, string(crosshooks.EventStop))
	if len(entries) != 1 {
		t.Fatalf("Stop entries = %#v, want one", entries)
	}
	entry := entries[0]
	command := assertClaudeCommandEntry(t, entry)
	clean, _, _, marked := crosshooks.ParseCommandSuffixMarker(command)
	if !marked {
		t.Fatalf("fresh Stop command has no suffix: %q", command)
	}
	handEdited := clean + " --hand-edited"
	entry["command"] = handEdited
	matcher := manager.Matcher
	if matcher == nil {
		matcher = crosshooks.MatchBasename(agentqueueToolName)
	}
	if !matcher.Match(crosshooks.HookEntry{Command: handEdited}) {
		t.Fatalf("Claude ownership matcher did not recognize hand-edited command %q", handEdited)
	}
	writeJSONSettings(t, path, settings)

	// The Claude manager falls back to MatchBasename for suffix-less legacy
	// entries; convergence then writes the declared HookSpec.Command
	// verbatim, discarding the hand-edited flag rather than preserving it.
	plan, err = manager.PlanInstall()
	if err != nil {
		t.Fatal(err)
	}
	if !plan.HasChanges || plan.Summary.Added != 1 || plan.Summary.Removed != 1 || plan.Summary.Modified != 1 {
		t.Fatalf("lost-suffix convergence plan = %+v", plan)
	}
	if !strings.Contains(plan.Diff, "#crossagent:v1:") {
		t.Fatalf("lost-suffix convergence diff lacks a suffix: %s", plan.Diff)
	}
	if err := manager.Apply(context.Background(), plan, crosshooks.ApplyOptions{SkipProbe: true}); err != nil {
		t.Fatal(err)
	}

	assertClaudeOwnedEntries(t, readJSONSettings(t, path), map[string]map[string]string{
		string(crosshooks.EventStop): {invocation + " hook claude": "stop-hook"},
	})
	second, err := manager.PlanInstall()
	if err != nil {
		t.Fatal(err)
	}
	if second.HasChanges || second.Summary.Added != 0 || second.Summary.Removed != 0 || second.Summary.Modified != 0 {
		t.Fatalf("lost-suffix convergence did not settle: %+v", second)
	}
}
