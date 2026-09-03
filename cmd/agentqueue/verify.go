package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// The safety invariant.
//
// install merges hook entries into a settings file it does not own: the file
// holds the user's own hooks, and other tools' hooks, under a shape this tool
// only partly understands. Rather than trusting the merge code to be careful,
// the prospective document is checked against the original before anything is
// written, and a violation aborts the write.
//
// Both directions are checked by the same machinery:
//
//   - install may only add, and only its own entries.
//   - uninstall may only remove its own entries, and may prune the wrappers
//     and event lists that leaves empty.
//
// Nothing outside the "hooks" subtree may change either way.

// hookWrapper is one element of an event's array: normally an object with a
// "hooks" array plus other fields such as "matcher". A wrapper that does not
// have that shape is kept whole in raw and compared verbatim, because this
// tool has no business reasoning about a shape it did not write.
type hookWrapper struct {
	// conforming is whether the wrapper had an object shape with a "hooks"
	// array, so fields and entries are meaningful.
	conforming bool
	// fields is the wrapper without its "hooks" key: "matcher" and anything
	// else the user put there, all of which must survive untouched.
	fields map[string]any
	// entries are the individual {type, command, ...} hook objects.
	entries []any
	// raw is the wrapper as it appeared.
	raw any
}

// hooksObject returns the settings' "hooks" object. ok is false when the key
// is absent or holds something other than an object.
func hooksObject(settings map[string]any) (map[string]any, bool) {
	raw, ok := settings["hooks"]
	if !ok {
		return map[string]any{}, true
	}
	m, ok := raw.(map[string]any)
	return m, ok
}

// parseWrappers splits an event's value into wrappers. ok is false when the
// value is not an array at all, in which case the caller falls back to
// comparing it verbatim.
func parseWrappers(v any) ([]hookWrapper, bool) {
	if v == nil {
		return nil, true
	}
	groups, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]hookWrapper, 0, len(groups))
	for _, g := range groups {
		group, ok := g.(map[string]any)
		if !ok {
			out = append(out, hookWrapper{raw: g})
			continue
		}
		inner, ok := group["hooks"].([]any)
		if !ok {
			out = append(out, hookWrapper{raw: g})
			continue
		}
		fields := make(map[string]any, len(group))
		for k, val := range group {
			if k == "hooks" {
				continue
			}
			fields[k] = val
		}
		out = append(out, hookWrapper{conforming: true, fields: fields, entries: inner, raw: g})
	}
	return out, true
}

// canonJSON is a comparable rendering of a hook entry. encoding/json sorts
// object keys, so two entries that differ only in key order compare equal -
// which is what we want, since the write itself reorders keys.
func canonJSON(v any) string {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%#v", v)
	}
	return string(body)
}

// entryCommand returns a hook entry's command string, or "" if it has none.
func entryCommand(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	cmd, _ := m["command"].(string)
	return cmd
}

// entryCounts counts the entries of an event, keyed by canonical form, across
// every wrapper.
func entryCounts(wrappers []hookWrapper) map[string]int {
	counts := map[string]int{}
	for _, w := range wrappers {
		for _, e := range w.entries {
			counts[canonJSON(e)]++
		}
	}
	return counts
}

// entryDelta is one entry that appears in one document and not the other.
type entryDelta struct {
	Event   string
	Entry   any
	Command string
	JSON    string
}

// diffEntries lists the entries present in to but not in from, per event,
// comparing multisets so a duplicated entry is reported once per copy. Events
// whose value is not an array are skipped: they are compared verbatim
// elsewhere.
func diffEntries(from, to map[string]any) []entryDelta {
	var out []entryDelta
	for _, event := range sortedKeys(to) {
		toWrappers, ok := parseWrappers(to[event])
		if !ok {
			continue
		}
		fromWrappers, ok := parseWrappers(from[event])
		if !ok {
			fromWrappers = nil
		}
		remaining := entryCounts(fromWrappers)
		for _, w := range toWrappers {
			for _, e := range w.entries {
				key := canonJSON(e)
				if remaining[key] > 0 {
					remaining[key]--
					continue
				}
				out = append(out, entryDelta{Event: event, Entry: e, Command: entryCommand(e), JSON: key})
			}
		}
	}
	return out
}

// sortedKeys returns a map's keys in a stable order, so every message and
// listing this file produces is deterministic.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// checkOutsideHooks verifies that the two documents agree on everything except
// the "hooks" subtree.
func checkOutsideHooks(before, after map[string]any) error {
	strip := func(m map[string]any) map[string]any {
		out := make(map[string]any, len(m))
		for k, v := range m {
			if k == "hooks" {
				continue
			}
			out[k] = v
		}
		return out
	}
	b, a := strip(before), strip(after)
	if reflect.DeepEqual(b, a) {
		return nil
	}
	for _, k := range sortedKeys(b) {
		av, ok := a[k]
		if !ok {
			return fmt.Errorf("the key %q would be removed", k)
		}
		if !reflect.DeepEqual(b[k], av) {
			return fmt.Errorf("the key %q would change from %s to %s", k, canonJSON(b[k]), canonJSON(av))
		}
	}
	for _, k := range sortedKeys(a) {
		if _, ok := b[k]; !ok {
			return fmt.Errorf("the key %q would be added outside \"hooks\"", k)
		}
	}
	return errors.New(`something outside "hooks" would change`)
}

// wrapperMatch finds an unused wrapper in candidates that accounts for want:
// the same non-"hooks" fields, and entries that contain all of want's
// entries. It returns the index, or -1 with an explanation.
//
// A candidate whose fields match but whose entries do not is remembered
// separately, which is what makes the error say "an entry went missing"
// rather than "the wrapper is gone".
func wrapperMatch(want hookWrapper, candidates []hookWrapper, used []bool) (int, error) {
	fieldsMatched := -1
	var missing string
	for i, c := range candidates {
		if used[i] {
			continue
		}
		if !want.conforming || !c.conforming {
			if want.conforming == c.conforming && reflect.DeepEqual(want.raw, c.raw) {
				return i, nil
			}
			continue
		}
		if !reflect.DeepEqual(want.fields, c.fields) {
			continue
		}
		if fieldsMatched < 0 {
			fieldsMatched = i
		}
		have := entryCounts([]hookWrapper{c})
		lost := ""
		for _, e := range want.entries {
			key := canonJSON(e)
			if have[key] > 0 {
				have[key]--
				continue
			}
			lost = key
			break
		}
		if lost == "" {
			return i, nil
		}
		if missing == "" {
			missing = lost
		}
	}
	if fieldsMatched >= 0 && missing != "" {
		return -1, fmt.Errorf("the hook entry %s is no longer there", missing)
	}
	if !want.conforming {
		return -1, fmt.Errorf("the entry %s is no longer there", canonJSON(want.raw))
	}
	return -1, fmt.Errorf("no wrapper with the fields %s survives, so the entries under it (%s) would be lost or moved",
		canonJSON(want.fields), canonJSON(want.entries))
}

// verifyChangeSafe is the gate every write passes through. It compares the
// prospective document with the original and rejects anything that is not the
// change the command is allowed to make:
//
//  1. nothing outside "hooks" changed,
//  2. every hook entry that is not agentqueue's own is still present, under a
//     wrapper that kept its other fields,
//  3. every entry that appeared is an agentqueue command, and only if this
//     command is allowed to add,
//  4. every entry that disappeared is an agentqueue command, and only if this
//     command is allowed to remove.
//
// Check 2 runs on copies with agentqueue's own entries stripped out, because
// what happens to our entries is the business of checks 3 and 4: install may
// add them, uninstall may remove them, and an update may do both. Stripping
// also takes care of the pruning either direction does - a wrapper that held
// only our entries, an event whose wrappers all went, the "hooks" key itself -
// since the stripped views never had them.
//
// label names the operation in the error messages.
func verifyChangeSafe(before, after map[string]any, cmdPrefix, label string, allowAdd, allowRemove bool) error {
	if strings.TrimSpace(cmdPrefix) == "" {
		return errors.New("cannot verify the change without knowing how the agentqueue binary is spelled")
	}
	if err := checkOutsideHooks(before, after); err != nil {
		return err
	}
	beforeHooks, bok := hooksObject(before)
	afterHooks, aok := hooksObject(after)
	if !bok || !aok {
		if !reflect.DeepEqual(before["hooks"], after["hooks"]) {
			return errors.New(`the "hooks" key is not an object and its value would change`)
		}
		return nil
	}

	// 2. Nothing belonging to anybody else is lost, moved or altered.
	beforeForeign, err := foreignHooks(before, cmdPrefix)
	if err != nil {
		return err
	}
	afterForeign, err := foreignHooks(after, cmdPrefix)
	if err != nil {
		return err
	}
	for _, event := range sortedKeys(beforeForeign) {
		if err := checkEventPreserved(event, beforeForeign[event], afterForeign[event]); err != nil {
			return err
		}
	}

	// 3. Additions.
	for _, added := range diffEntries(beforeHooks, afterHooks) {
		if !allowAdd {
			return fmt.Errorf("%s would gain the entry %s, but %s must only remove", added.Event, added.JSON, label)
		}
		if !strings.HasPrefix(strings.TrimSpace(added.Command), cmdPrefix) {
			return fmt.Errorf("%s would gain the entry %s, which is not an agentqueue hook (its command does not start with %s)",
				added.Event, added.JSON, cmdPrefix)
		}
	}

	// 4. Removals.
	for _, removed := range diffEntries(afterHooks, beforeHooks) {
		if !allowRemove {
			return fmt.Errorf("%s would lose the entry %s, but %s must only add", removed.Event, removed.JSON, label)
		}
		if !ownedCommand(removed.Command, cmdPrefix) {
			return fmt.Errorf("%s would lose the entry %s, which is not an agentqueue hook", removed.Event, removed.JSON)
		}
	}
	return nil
}

// foreignHooks is the "hooks" object with every agentqueue entry taken out,
// and the wrappers and events that leaves empty pruned - the part of the file
// that has to come through any of our writes untouched. It reuses the removal
// the uninstall path uses, on a copy, so the two cannot describe "ours"
// differently.
func foreignHooks(settings map[string]any, cmdPrefix string) (map[string]any, error) {
	copied, err := cloneSettings(settings)
	if err != nil {
		return nil, err
	}
	removeOwnedEntries(copied, cmdPrefix)
	hooks, ok := hooksObject(copied)
	if !ok {
		return map[string]any{}, nil
	}
	return hooks, nil
}

// verifyMergeSafe is the gate install passes through: the prospective document
// must be the original plus agentqueue's own entries, and nothing else.
func verifyMergeSafe(before, after map[string]any, cmdPrefix string) error {
	return verifyChangeSafe(before, after, cmdPrefix, "install", true, false)
}

// verifyUpdateSafe is the gate the stale-path update passes through. It both
// adds and removes, but only agentqueue's own entries: an update rewrites our
// hooks to the current binary path and may touch nothing else.
func verifyUpdateSafe(before, after map[string]any, cmdPrefix string) error {
	return verifyChangeSafe(before, after, cmdPrefix, "an update", true, true)
}

// verifyUninstallSafe is the mirrored gate for uninstall: only entries
// agentqueue owns may disappear, nothing may be added, nothing outside
// "hooks" may change, and the wrappers and event lists that removal empties
// are expected to be pruned - a wrapper that held only agentqueue entries is
// allowed to vanish along with them, and so is an event whose wrappers all
// went, and the "hooks" key itself when no event is left.
func verifyUninstallSafe(before, after map[string]any, cmdPrefix string) error {
	return verifyChangeSafe(before, after, cmdPrefix, "uninstall", false, true)
}

// checkEventPreserved verifies that every wrapper of want's event is
// accounted for in have: a wrapper with the same other fields, holding at
// least the same entries. It is called with the original as want, so nothing
// may be lost - and because a matching wrapper has to hold want's entries
// itself, an entry cannot move from one wrapper to another either.
func checkEventPreserved(event string, want, have any) error {
	wantWrappers, ok := parseWrappers(want)
	if !ok {
		// Not an array: compare it whole rather than guess at its shape.
		if !reflect.DeepEqual(want, have) {
			return fmt.Errorf("%s: its value is not an array of hook groups and would change from %s to %s",
				event, canonJSON(want), canonJSON(have))
		}
		return nil
	}
	if len(wantWrappers) == 0 {
		return nil
	}
	haveWrappers, ok := parseWrappers(have)
	if !ok {
		return fmt.Errorf("%s: its value would become %s, which is not an array of hook groups", event, canonJSON(have))
	}
	if len(haveWrappers) == 0 {
		return fmt.Errorf("%s: the whole event would be dropped, losing %s", event, canonJSON(want))
	}
	used := make([]bool, len(haveWrappers))
	for _, w := range wantWrappers {
		idx, err := wrapperMatch(w, haveWrappers, used)
		if err != nil {
			return fmt.Errorf("%s: %w", event, err)
		}
		used[idx] = true
	}
	return nil
}
