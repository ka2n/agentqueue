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

func TestInstallAdoptsSyntheticUnmarkedHooksAndPreservesForeignEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	old := "/old/path/agentqueue"
	foreignStart := map[string]any{
		"type": "command", "command": "other-tool start", "vendor": map[string]any{"keep": true},
	}
	foreignStop := map[string]any{
		"type": "command", "command": "other-tool stop", "async": true,
	}
	foreignMarked := map[string]any{
		"type": "command", "command": old + " hook claude --foreign", "installedBy": "other-tool", "x-other-tool-id": "stop",
	}
	writeJSONSettings(t, path, map[string]any{
		"permissions": map[string]any{"allow": []any{"Read"}},
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"matcher": "*",
				"hooks": []any{
					map[string]any{"type": "command", "command": old + " register --agent claude"},
					map[string]any{"type": "command", "command": old + " hook claude"},
					foreignStart,
					foreignMarked,
				},
			}},
			"UserPromptSubmit": []any{map[string]any{
				"hooks": []any{map[string]any{"type": "command", "command": old + " hook claude"}},
			}},
			"Stop": []any{map[string]any{
				"matcher": "",
				"hooks": []any{
					map[string]any{"type": "command", "command": old + " hook claude"},
					foreignStop,
				},
			}},
			"SessionEnd": []any{map[string]any{
				"hooks": []any{map[string]any{"type": "command", "command": old + " unregister --agent claude"}},
			}},
		},
	})

	manager, err := claudeConfigManager("user", path, "/new/path/agentqueue", "/new/path/agentqueue")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := manager.PlanInstall()
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Unmarked) != 5 {
		t.Fatalf("unmarked entries = %d, want 5: %#v", len(plan.Unmarked), plan.Unmarked)
	}
	if plan.Summary.Added != 5 || plan.Summary.Removed != 5 || plan.Summary.Modified != 5 {
		t.Fatalf("adoption summary = %+v, want five rewritten entries", plan.Summary)
	}
	if plan.Diff == "" || !strings.Contains(plan.Diff, crosshooks.MarkerOwnerField) {
		t.Fatalf("adoption diff does not show marker additions:\n%s", plan.Diff)
	}
	var planOutput strings.Builder
	printHookPlan(&planOutput, plan)
	if !strings.Contains(planOutput.String(), "adoption: adopting 5 unmarked agentqueue hooks by default") {
		t.Fatalf("adoption is not visible in the rendered plan:\n%s", planOutput.String())
	}

	if err := manager.Apply(context.Background(), plan, crosshooks.ApplyOptions{SkipProbe: true}); err != nil {
		t.Fatal(err)
	}
	settings := readJSONSettings(t, path)
	if got := settings["permissions"].(map[string]any)["allow"].([]any)[0]; got != "Read" {
		t.Fatalf("unrelated settings changed: %#v", settings["permissions"])
	}

	// SessionStart deliberately has two delivery points; each declared
	// hook has exactly one marked entry after adoption.
	wantIDs := map[string][]string{
		string(crosshooks.EventSessionStart):     {"session-start-register", "session-start-hook"},
		string(crosshooks.EventUserPromptSubmit): {"user-prompt-submit-hook"},
		string(crosshooks.EventStop):             {"stop-hook"},
		string(crosshooks.EventSessionEnd):       {"session-end-unregister"},
	}
	for event, ids := range wantIDs {
		entries := commandEntriesForTest(settings, event)
		counts := make(map[string]int)
		for _, entry := range entries {
			if entry[crosshooks.MarkerOwnerField] == agentqueueToolName {
				id, ok := entry[crosshooks.MarkerIDField(agentqueueToolName)].(string)
				if !ok || id == "" {
					t.Fatalf("%s adopted entry has no stable id: %#v", event, entry)
				}
				counts[id]++
			}
		}
		if len(counts) != len(ids) {
			t.Fatalf("%s has marked ids %v, want exactly %v", event, counts, ids)
		}
		for _, id := range ids {
			if counts[id] != 1 {
				t.Fatalf("%s has marker %q %d times, want once", event, id, counts[id])
			}
		}
	}

	entries := commandEntriesForTest(settings, string(crosshooks.EventSessionStart))
	if len(entries) != 4 {
		t.Fatalf("SessionStart entries = %#v, want two agentqueue entries and two foreign entries", entries)
	}
	var foundStartForeign bool
	for _, entry := range entries {
		if entry["command"] == foreignStart["command"] {
			foundStartForeign = true
			if entry["vendor"].(map[string]any)["keep"] != true {
				t.Fatalf("foreign SessionStart entry changed: %#v", entry)
			}
		}
	}
	if !foundStartForeign {
		t.Fatalf("foreign SessionStart entry disappeared: %#v", entries)
	}
	var foundMarkedForeign bool
	for _, entry := range entries {
		if entry["command"] == foreignMarked["command"] {
			foundMarkedForeign = true
			if entry[crosshooks.MarkerOwnerField] != foreignMarked[crosshooks.MarkerOwnerField] || entry[crosshooks.MarkerIDField("other-tool")] != foreignMarked[crosshooks.MarkerIDField("other-tool")] {
				t.Fatalf("foreign marked entry changed: %#v", entry)
			}
		}
	}
	if !foundMarkedForeign {
		t.Fatalf("foreign marked SessionStart entry disappeared: %#v", entries)
	}
	entries = commandEntriesForTest(settings, string(crosshooks.EventStop))
	var foundStopForeign bool
	for _, entry := range entries {
		if entry["command"] == foreignStop["command"] {
			foundStopForeign = true
			if entry["async"] != true {
				t.Fatalf("foreign Stop entry changed: %#v", entry)
			}
		}
	}
	if !foundStopForeign {
		t.Fatalf("foreign Stop entry disappeared: %#v", entries)
	}

	second, err := manager.PlanInstall()
	if err != nil {
		t.Fatal(err)
	}
	if second.HasChanges || len(second.Unmarked) != 0 {
		t.Fatalf("adoption did not converge: %+v", second)
	}
}
