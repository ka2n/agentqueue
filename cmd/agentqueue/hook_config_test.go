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

func assertNoClaudeMarkerFields(t *testing.T, entry map[string]any) {
	t.Helper()
	for _, key := range []string{crosshooks.MarkerOwnerField, crosshooks.MarkerIDField(agentqueueToolName)} {
		if _, ok := entry[key]; ok {
			t.Fatalf("Claude hook unexpectedly has %s: %#v", key, entry)
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
	foreignMarked := map[string]any{
		"type": "command", "command": "other-tool stop", "installedBy": "other-tool", "x-other-tool-id": "stop",
	}
	foreignStop := map[string]any{
		"type": "command", "command": "other-tool finish", "async": true,
	}
	writeJSONSettings(t, path, map[string]any{
		"permissions": map[string]any{"allow": []any{"Read"}},
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"matcher": "*",
				"hooks": []any{
					map[string]any{"type": "command", "command": old + " register --agent claude", crosshooks.MarkerOwnerField: agentqueueToolName, crosshooks.MarkerIDField(agentqueueToolName): "old-register"},
					map[string]any{"type": "command", "command": old + " hook claude"},
					foreignStart,
					foreignMarked,
				},
			}},
			"UserPromptSubmit": []any{map[string]any{
				"hooks": []any{map[string]any{"type": "command", "command": old + " hook claude", crosshooks.MarkerOwnerField: agentqueueToolName, crosshooks.MarkerIDField(agentqueueToolName): "old-prompt"}},
			}},
			"Stop": []any{map[string]any{
				"matcher": "",
				"hooks": []any{
					map[string]any{"type": "command", "command": old + " hook claude"},
					foreignStop,
				},
			}},
			"SessionEnd": []any{map[string]any{
				"hooks": []any{map[string]any{"type": "command", "command": old + " unregister --agent claude", crosshooks.MarkerOwnerField: agentqueueToolName, crosshooks.MarkerIDField(agentqueueToolName): "old-unregister"}},
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
	if len(plan.Unmarked) != 0 {
		t.Fatalf("Claude predicate matches were reported as unmarked: %#v", plan.Unmarked)
	}
	if !plan.HasChanges {
		t.Fatal("stale agentqueue commands were not converged")
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

	wantCommands := map[string]int{
		string(crosshooks.EventSessionStart):     2,
		string(crosshooks.EventUserPromptSubmit): 1,
		string(crosshooks.EventStop):             1,
		string(crosshooks.EventSessionEnd):       1,
	}
	for event, want := range wantCommands {
		var owned int
		for _, entry := range commandEntriesForTest(settings, event) {
			command, _ := entry["command"].(string)
			if strings.HasPrefix(command, invocation+" ") {
				owned++
				assertNoClaudeMarkerFields(t, entry)
			}
		}
		if owned != want {
			t.Fatalf("%s has %d agentqueue entries, want %d", event, owned, want)
		}
	}

	entries := commandEntriesForTest(settings, string(crosshooks.EventSessionStart))
	var foundStartForeign, foundMarkedForeign bool
	for _, entry := range entries {
		switch entry["command"] {
		case foreignStart["command"]:
			foundStartForeign = true
			if entry["vendor"].(map[string]any)["keep"] != true {
				t.Fatalf("foreign SessionStart entry changed: %#v", entry)
			}
		case foreignMarked["command"]:
			foundMarkedForeign = true
			if entry[crosshooks.MarkerOwnerField] != foreignMarked[crosshooks.MarkerOwnerField] || entry[crosshooks.MarkerIDField("other-tool")] != foreignMarked[crosshooks.MarkerIDField("other-tool")] {
				t.Fatalf("foreign marked entry changed: %#v", entry)
			}
		}
	}
	if !foundStartForeign || !foundMarkedForeign {
		t.Fatalf("foreign SessionStart entries were changed or removed: %#v", entries)
	}
	entries = commandEntriesForTest(settings, string(crosshooks.EventStop))
	if len(entries) != 2 || entries[1]["command"] != foreignStop["command"] || entries[1]["async"] != true {
		t.Fatalf("foreign Stop entry changed or removed: %#v", entries)
	}

	second, err := manager.PlanInstall()
	if err != nil {
		t.Fatal(err)
	}
	if second.HasChanges || len(second.Unmarked) != 0 {
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

func TestInstallWritesClaudeHooksWithoutMarkerFields(t *testing.T) {
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
	settings := readJSONSettings(t, path)
	for _, event := range []string{
		string(crosshooks.EventSessionStart),
		string(crosshooks.EventUserPromptSubmit),
		string(crosshooks.EventStop),
		string(crosshooks.EventSessionEnd),
	} {
		for _, entry := range commandEntriesForTest(settings, event) {
			assertNoClaudeMarkerFields(t, entry)
		}
	}

	printed, err := json.Marshal(hookBlock(claudeHookSpecs(invocation)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(printed), crosshooks.MarkerOwnerField) || strings.Contains(string(printed), crosshooks.MarkerIDField(agentqueueToolName)) {
		t.Fatalf("--print block contains Claude marker fields: %s", printed)
	}
}

func TestInstallAfterClaudeStripsMarkersIsByteIdentical(t *testing.T) {
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

	// Simulate an older install followed by Claude Code rewriting settings and
	// stripping the unknown marker fields from every entry.
	settings := readJSONSettings(t, path)
	for _, event := range []string{
		string(crosshooks.EventSessionStart),
		string(crosshooks.EventUserPromptSubmit),
		string(crosshooks.EventStop),
		string(crosshooks.EventSessionEnd),
	} {
		for _, entry := range commandEntriesForTest(settings, event) {
			entry[crosshooks.MarkerOwnerField] = agentqueueToolName
			entry[crosshooks.MarkerIDField(agentqueueToolName)] = event
		}
	}
	writeJSONSettings(t, path, settings)
	for _, event := range []string{
		string(crosshooks.EventSessionStart),
		string(crosshooks.EventUserPromptSubmit),
		string(crosshooks.EventStop),
		string(crosshooks.EventSessionEnd),
	} {
		for _, entry := range commandEntriesForTest(settings, event) {
			delete(entry, crosshooks.MarkerOwnerField)
			delete(entry, crosshooks.MarkerIDField(agentqueueToolName))
		}
	}
	writeJSONSettings(t, path, settings)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = manager.PlanInstall()
	if err != nil {
		t.Fatal(err)
	}
	if plan.HasChanges || len(plan.Unmarked) != 0 || plan.Summary.Added != 0 || plan.Summary.Removed != 0 {
		t.Fatalf("marker stripping caused install churn: %+v", plan)
	}
	var output strings.Builder
	if err := applyHookPlan(context.Background(), manager, plan, strings.NewReader(""), &output, true, false, false, true); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "claude: nothing to do\n" {
		t.Fatalf("marker-stripped reinstall output = %q, want nothing to do", got)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("marker-stripped reinstall changed file bytes:\nbefore=%q\nafter=%q", before, after)
	}
}
