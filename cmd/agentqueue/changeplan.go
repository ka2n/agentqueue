package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// The confirmation output.
//
// What install used to show was the JSON block it was about to add. Read on
// its own, that block looks like a replacement for the whole "hooks" section,
// which is exactly the wrong impression: the merge appends. So the
// confirmation now shows the real before/after of the file - a per-event
// account of what is kept, and a unified diff of the two documents - and the
// write itself is gated on verifyMergeSafe.

// diffContextLines is how much context the confirmation diff carries.
const diffContextLines = 3

// eventChange is the account of one hook event, for the "preserved" listing.
type eventChange struct {
	Event    string
	Existing int
	Kept     int
	Added    int
	Removed  int
	Modified int
	// IsNew is whether the event had no wrappers in the original file.
	IsNew bool
}

// changeSummary is the whole change, in entries rather than lines.
type changeSummary struct {
	Added    int
	Removed  int
	Modified int
	Events   []eventChange
}

// summarizeChange counts what the edit does to the hook entries, from the two
// documents themselves.
//
// A "modified" entry is one that leaves and comes back on the same event
// under the same action - "hook claude" for example - with different flags:
// the multiset diff sees a removal and an addition, but the user sees one
// entry rewritten, so the pair is reported that way and not double-counted.
func summarizeChange(before, after map[string]any) changeSummary {
	beforeHooks, _ := hooksObject(before)
	afterHooks, _ := hooksObject(after)

	added := diffEntries(beforeHooks, afterHooks)
	removed := diffEntries(afterHooks, beforeHooks)

	perEvent := map[string]*eventChange{}
	get := func(event string) *eventChange {
		if e, ok := perEvent[event]; ok {
			return e
		}
		e := &eventChange{Event: event}
		perEvent[event] = e
		return e
	}
	for _, event := range sortedKeys(beforeHooks) {
		w, _ := parseWrappers(beforeHooks[event])
		e := get(event)
		for _, count := range entryCounts(w) {
			e.Existing += count
		}
	}
	for _, event := range sortedKeys(afterHooks) {
		e := get(event)
		if e.Existing == 0 {
			e.IsNew = true
		}
	}
	for _, a := range added {
		get(a.Event).Added++
	}
	for _, r := range removed {
		get(r.Event).Removed++
	}

	// Pair removals with additions that are the same action on the same
	// event: one entry rewritten, not one gone and one new.
	pairedAdd := make([]bool, len(added))
	for _, r := range removed {
		for i, a := range added {
			if pairedAdd[i] || a.Event != r.Event {
				continue
			}
			if actionKey(a.Command) != actionKey(r.Command) || actionKey(a.Command) == "" {
				continue
			}
			pairedAdd[i] = true
			e := get(r.Event)
			e.Modified++
			e.Added--
			e.Removed--
			break
		}
	}

	out := changeSummary{}
	for _, event := range sortedEventKeys(perEvent) {
		e := perEvent[event]
		e.Kept = e.Existing - e.Removed - e.Modified
		if e.Kept < 0 {
			e.Kept = 0
		}
		out.Added += e.Added
		out.Removed += e.Removed
		out.Modified += e.Modified
		out.Events = append(out.Events, *e)
	}
	return out
}

// sortedEventKeys orders the events for printing: the ones agentqueue writes
// first, in the order it writes them, then everything else alphabetically, so
// the listing reads the same way twice in a row.
func sortedEventKeys(m map[string]*eventChange) []string {
	var ours, others []string
	for event := range m {
		if isOurEvent(event) {
			ours = append(ours, event)
			continue
		}
		others = append(others, event)
	}
	sort.Slice(ours, func(i, j int) bool { return ourEventIndex(ours[i]) < ourEventIndex(ours[j]) })
	sort.Strings(others)
	return append(ours, others...)
}

func isOurEvent(event string) bool { return ourEventIndex(event) >= 0 }

func ourEventIndex(event string) int {
	for i, e := range claudeEventOrder {
		if e == event {
			return i
		}
	}
	return -1
}

// settingsLines renders a settings document the way it would be written -
// indented, with the keys sorted by encoding/json - split into lines for
// diffing.
func settingsLines(settings map[string]any) ([]string, error) {
	body, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode settings: %w", err)
	}
	return splitLines(string(body)), nil
}

// cloneSettings copies a settings document so the prospective new version can
// be built without touching the original, which the verification needs intact
// to compare against. The copy goes through JSON, which is the same encoding
// the file itself round-trips through.
func cloneSettings(settings map[string]any) (map[string]any, error) {
	body, err := json.Marshal(settings)
	if err != nil {
		return nil, fmt.Errorf("copy settings: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("copy settings: %w", err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// printChangePlan shows what the write would do: the file, the counts, what
// is kept per event and the diff. verb is "install" or "uninstall", and
// appears in the diff's +++ label.
func printChangePlan(w io.Writer, path, verb string, before, after map[string]any) error {
	if info, err := os.Stat(path); err == nil {
		fmt.Fprintf(w, "  file:    %s (exists, %d bytes)\n", path, info.Size())
	} else if os.IsNotExist(err) {
		fmt.Fprintf(w, "  file:    %s (does not exist yet, it will be created)\n", path)
	} else {
		fmt.Fprintf(w, "  file:    %s (cannot stat it: %v)\n", path, err)
	}

	sum := summarizeChange(before, after)
	fmt.Fprintf(w, "  change:  +%d hook entries, -%d removed, %d modified\n", sum.Added, sum.Removed, sum.Modified)

	if len(sum.Events) > 0 {
		fmt.Fprintln(w, "  events:  every hook event in the file, and what happens to it:")
		for _, e := range sum.Events {
			addedText := "none added"
			if e.Added > 0 {
				addedText = fmt.Sprintf("%d added", e.Added)
			}
			line := fmt.Sprintf("    %s: %d existing %s kept, %s", e.Event, e.Kept, plural(e.Kept, "entry", "entries"), addedText)
			if e.Removed > 0 {
				line += fmt.Sprintf(", %d removed", e.Removed)
			}
			if e.Modified > 0 {
				line += fmt.Sprintf(", %d modified", e.Modified)
			}
			if e.IsNew {
				line += " (event not in the file yet)"
			}
			fmt.Fprintln(w, line)
		}
	}

	beforeLines, err := settingsLines(before)
	if err != nil {
		return err
	}
	afterLines, err := settingsLines(after)
	if err != nil {
		return err
	}
	diff := unifiedDiff(beforeLines, afterLines,
		fmt.Sprintf("%s (now, key-sorted)", path),
		fmt.Sprintf("%s (after %s, key-sorted)", path, verb),
		diffContextLines)
	if diff == "" {
		fmt.Fprintln(w, "  diff:    none, the file already says what it would say")
		return nil
	}
	plus, minus := diffStat(diffLines(beforeLines, afterLines))
	fmt.Fprintf(w, "  diff:    +%d lines, -%d lines (%d lines of context)\n", plus, minus, diffContextLines)
	fmt.Fprintln(w, "  note:    writing the file re-encodes it as JSON, which sorts the keys, so")
	fmt.Fprintln(w, "           the key order on disk will change even where no value does. Both")
	fmt.Fprintln(w, "           sides of this diff are key-sorted, so it shows value changes only.")
	fmt.Fprintln(w)
	fmt.Fprint(w, diff)
	fmt.Fprintln(w)
	return nil
}

// plural picks the singular or plural noun for a count, so a line reads
// "1 existing entry kept" rather than "1 existing entries kept".
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// printPathWarning notes the one thing a bare `agentqueue` invocation costs: a
// hook does not run with the shell's environment, so a name found on the
// installing shell's $PATH is not guaranteed to be found later.
func printPathWarning(w io.Writer, invocation string) {
	if strings.TrimSpace(invocation) != "agentqueue" {
		return
	}
	fmt.Fprintln(w, "  note:    the hooks call the bare name `agentqueue`, resolved through $PATH.")
	fmt.Fprintln(w, "           Claude Code runs hooks with its own environment, which may have a")
	fmt.Fprintln(w, "           different $PATH than this shell; --command <absolute path> avoids")
	fmt.Fprintln(w, "           that. The default is unchanged.")
}
