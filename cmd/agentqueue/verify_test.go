package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// verifyPrefix is how the agentqueue binary is spelled in these tests.
const verifyPrefix = "/opt/aq/agentqueue"

// doc parses a settings document written inline, so the tests read like the
// files they stand for.
func doc(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("parse test document: %v\n%s", err, body)
	}
	return out
}

// beforeDoc is a settings file that belongs to somebody else: two events, a
// wrapper with a matcher, a wrapper without one, and keys outside "hooks".
const beforeDoc = `{
  "model": "opus",
  "permissions": {"allow": ["Bash(ls:*)"]},
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [
        {"type": "command", "command": "/usr/bin/mylint"},
        {"type": "command", "command": "/usr/bin/other"}
      ]}
    ],
    "Stop": [
      {"hooks": [{"type": "command", "command": "/opt/other/notify.sh"}]}
    ]
  }
}`

// afterDoc is what a correct merge produces: the same document with our own
// wrappers appended.
const afterDoc = `{
  "model": "opus",
  "permissions": {"allow": ["Bash(ls:*)"]},
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [
        {"type": "command", "command": "/usr/bin/mylint"},
        {"type": "command", "command": "/usr/bin/other"}
      ]}
    ],
    "Stop": [
      {"hooks": [{"type": "command", "command": "/opt/other/notify.sh"}]},
      {"hooks": [{"type": "command", "command": "/opt/aq/agentqueue hook claude"}]}
    ],
    "SessionStart": [
      {"hooks": [
        {"type": "command", "command": "/opt/aq/agentqueue register --agent claude"},
        {"type": "command", "command": "/opt/aq/agentqueue hook claude"}
      ]}
    ]
  }
}`

func TestVerifyMergeSafeAcceptsAnAdditiveMerge(t *testing.T) {
	if err := verifyMergeSafe(doc(t, beforeDoc), doc(t, afterDoc), verifyPrefix); err != nil {
		t.Fatalf("verifyMergeSafe rejected a correct merge: %v", err)
	}
}

func TestVerifyMergeSafeAcceptsAFileThatHadNoHooks(t *testing.T) {
	before := doc(t, `{"model": "opus"}`)
	after := doc(t, `{"model": "opus", "hooks": {"Stop": [{"hooks": [
		{"type": "command", "command": "/opt/aq/agentqueue hook claude"}]}]}}`)
	if err := verifyMergeSafe(before, after, verifyPrefix); err != nil {
		t.Fatalf("verifyMergeSafe rejected a first install: %v", err)
	}
}

// TestVerifyMergeSafeRejectsCorruptedResults is the point of the whole file:
// every way the merge could go wrong has to be caught before a write.
func TestVerifyMergeSafeRejectsCorruptedResults(t *testing.T) {
	tests := []struct {
		name string
		// after is the corrupted prospective document.
		after string
		// want is a fragment the error message has to contain, so a test
		// failure says which invariant broke.
		want string
	}{
		{
			name: "an existing entry is dropped",
			after: `{
			  "model": "opus",
			  "permissions": {"allow": ["Bash(ls:*)"]},
			  "hooks": {
			    "PreToolUse": [
			      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/usr/bin/mylint"}]}
			    ],
			    "Stop": [
			      {"hooks": [{"type": "command", "command": "/opt/other/notify.sh"}]},
			      {"hooks": [{"type": "command", "command": "/opt/aq/agentqueue hook claude"}]}
			    ]
			  }
			}`,
			want: "/usr/bin/other",
		},
		{
			name: "an existing command is edited",
			after: `{
			  "model": "opus",
			  "permissions": {"allow": ["Bash(ls:*)"]},
			  "hooks": {
			    "PreToolUse": [
			      {"matcher": "Bash", "hooks": [
			        {"type": "command", "command": "/usr/bin/mylint --quiet"},
			        {"type": "command", "command": "/usr/bin/other"}
			      ]}
			    ],
			    "Stop": [{"hooks": [{"type": "command", "command": "/opt/other/notify.sh"}]}]
			  }
			}`,
			want: "/usr/bin/mylint",
		},
		{
			name: "a matcher is removed from a wrapper we did not write",
			after: `{
			  "model": "opus",
			  "permissions": {"allow": ["Bash(ls:*)"]},
			  "hooks": {
			    "PreToolUse": [
			      {"hooks": [
			        {"type": "command", "command": "/usr/bin/mylint"},
			        {"type": "command", "command": "/usr/bin/other"}
			      ]}
			    ],
			    "Stop": [{"hooks": [{"type": "command", "command": "/opt/other/notify.sh"}]}]
			  }
			}`,
			want: "matcher",
		},
		{
			name: "a whole event disappears",
			after: `{
			  "model": "opus",
			  "permissions": {"allow": ["Bash(ls:*)"]},
			  "hooks": {
			    "Stop": [{"hooks": [{"type": "command", "command": "/opt/other/notify.sh"}]}]
			  }
			}`,
			want: "dropped",
		},
		{
			name: "the hooks section is replaced with only our own entries",
			after: `{
			  "model": "opus",
			  "permissions": {"allow": ["Bash(ls:*)"]},
			  "hooks": {
			    "Stop": [{"hooks": [{"type": "command", "command": "/opt/aq/agentqueue hook claude"}]}]
			  }
			}`,
			want: "PreToolUse",
		},
		{
			name: "a key outside hooks changes",
			after: `{
			  "model": "sonnet",
			  "permissions": {"allow": ["Bash(ls:*)"]},
			  "hooks": {
			    "PreToolUse": [
			      {"matcher": "Bash", "hooks": [
			        {"type": "command", "command": "/usr/bin/mylint"},
			        {"type": "command", "command": "/usr/bin/other"}
			      ]}
			    ],
			    "Stop": [{"hooks": [{"type": "command", "command": "/opt/other/notify.sh"}]}]
			  }
			}`,
			want: `"model" would change`,
		},
		{
			name: "a key outside hooks is removed",
			after: `{
			  "model": "opus",
			  "hooks": {
			    "PreToolUse": [
			      {"matcher": "Bash", "hooks": [
			        {"type": "command", "command": "/usr/bin/mylint"},
			        {"type": "command", "command": "/usr/bin/other"}
			      ]}
			    ],
			    "Stop": [{"hooks": [{"type": "command", "command": "/opt/other/notify.sh"}]}]
			  }
			}`,
			want: `"permissions" would be removed`,
		},
		{
			name: "a key outside hooks is added",
			after: `{
			  "model": "opus",
			  "permissions": {"allow": ["Bash(ls:*)"]},
			  "env": {"FOO": "bar"},
			  "hooks": {
			    "PreToolUse": [
			      {"matcher": "Bash", "hooks": [
			        {"type": "command", "command": "/usr/bin/mylint"},
			        {"type": "command", "command": "/usr/bin/other"}
			      ]}
			    ],
			    "Stop": [{"hooks": [{"type": "command", "command": "/opt/other/notify.sh"}]}]
			  }
			}`,
			want: `"env" would be added`,
		},
		{
			name: "a foreign entry is added",
			after: `{
			  "model": "opus",
			  "permissions": {"allow": ["Bash(ls:*)"]},
			  "hooks": {
			    "PreToolUse": [
			      {"matcher": "Bash", "hooks": [
			        {"type": "command", "command": "/usr/bin/mylint"},
			        {"type": "command", "command": "/usr/bin/other"},
			        {"type": "command", "command": "/usr/bin/curl http://example.invalid"}
			      ]}
			    ],
			    "Stop": [{"hooks": [{"type": "command", "command": "/opt/other/notify.sh"}]}]
			  }
			}`,
			want: "not an agentqueue hook",
		},
		{
			name: "an entry of ours is added under a different binary",
			after: `{
			  "model": "opus",
			  "permissions": {"allow": ["Bash(ls:*)"]},
			  "hooks": {
			    "PreToolUse": [
			      {"matcher": "Bash", "hooks": [
			        {"type": "command", "command": "/usr/bin/mylint"},
			        {"type": "command", "command": "/usr/bin/other"}
			      ]}
			    ],
			    "Stop": [
			      {"hooks": [{"type": "command", "command": "/opt/other/notify.sh"}]},
			      {"hooks": [{"type": "command", "command": "/somewhere/else/agentqueue hook claude"}]}
			    ]
			  }
			}`,
			want: "not an agentqueue hook",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyMergeSafe(doc(t, beforeDoc), doc(t, tt.after), verifyPrefix)
			if err == nil {
				t.Fatal("verifyMergeSafe accepted a corrupted document")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestVerifyMergeSafeNeedsAnInvocation guards against verifying with an empty
// prefix, which would let any added entry through.
func TestVerifyMergeSafeNeedsAnInvocation(t *testing.T) {
	if err := verifyMergeSafe(doc(t, beforeDoc), doc(t, afterDoc), "  "); err == nil {
		t.Fatal("verifyMergeSafe accepted an empty command prefix")
	}
}

// TestVerifyMergeSafeLeavesAnUnknownHooksShapeAlone covers a "hooks" key that
// is not an object: the tool refuses to reason about it, so the only safe
// answer is that it must not change.
func TestVerifyMergeSafeLeavesAnUnknownHooksShapeAlone(t *testing.T) {
	before := doc(t, `{"hooks": "see the other file"}`)
	if err := verifyMergeSafe(before, doc(t, `{"hooks": "see the other file"}`), verifyPrefix); err != nil {
		t.Fatalf("verifyMergeSafe rejected an untouched non-object hooks key: %v", err)
	}
	if err := verifyMergeSafe(before, doc(t, `{"hooks": {}}`), verifyPrefix); err == nil {
		t.Fatal("verifyMergeSafe accepted overwriting a non-object hooks key")
	}
}

// TestVerifyMergeSafeAcceptsAnEventThatIsNotAnArray covers a malformed event
// value: it may stay as it is, but it may not be rewritten.
func TestVerifyMergeSafeAcceptsAnEventThatIsNotAnArray(t *testing.T) {
	before := doc(t, `{"hooks": {"Stop": "nonsense"}}`)
	same := doc(t, `{"hooks": {"Stop": "nonsense"}}`)
	if err := verifyMergeSafe(before, same, verifyPrefix); err != nil {
		t.Fatalf("verifyMergeSafe rejected an untouched malformed event: %v", err)
	}
	changed := doc(t, `{"hooks": {"Stop": []}}`)
	if err := verifyMergeSafe(before, changed, verifyPrefix); err == nil {
		t.Fatal("verifyMergeSafe accepted rewriting a malformed event")
	}
}

func TestVerifyUninstallSafeAcceptsRemovalWithPruning(t *testing.T) {
	// SessionStart held only agentqueue entries, so the wrapper and the whole
	// event are expected to be pruned; Stop keeps the entry that is not ours.
	before := doc(t, afterDoc)
	after := doc(t, beforeDoc)
	if err := verifyUninstallSafe(before, after, verifyPrefix); err != nil {
		t.Fatalf("verifyUninstallSafe rejected a correct removal: %v", err)
	}

	// And the last removal takes the "hooks" key with it.
	onlyOurs := doc(t, `{"model": "opus", "hooks": {"Stop": [{"hooks": [
		{"type": "command", "command": "/opt/aq/agentqueue hook claude"}]}]}}`)
	if err := verifyUninstallSafe(onlyOurs, doc(t, `{"model": "opus"}`), verifyPrefix); err != nil {
		t.Fatalf("verifyUninstallSafe rejected pruning the hooks key: %v", err)
	}
}

func TestVerifyUninstallSafeRejectsCorruptedResults(t *testing.T) {
	tests := []struct {
		name   string
		before string
		after  string
		want   string
	}{
		{
			name:   "somebody else's hook is removed too",
			before: afterDoc,
			after: `{
			  "model": "opus",
			  "permissions": {"allow": ["Bash(ls:*)"]},
			  "hooks": {
			    "PreToolUse": [
			      {"matcher": "Bash", "hooks": [
			        {"type": "command", "command": "/usr/bin/mylint"},
			        {"type": "command", "command": "/usr/bin/other"}
			      ]}
			    ]
			  }
			}`,
			want: "/opt/other/notify.sh",
		},
		{
			name:   "an entry is added",
			before: afterDoc,
			after: `{
			  "model": "opus",
			  "permissions": {"allow": ["Bash(ls:*)"]},
			  "hooks": {
			    "PreToolUse": [
			      {"matcher": "Bash", "hooks": [
			        {"type": "command", "command": "/usr/bin/mylint"},
			        {"type": "command", "command": "/usr/bin/other"}
			      ]}
			    ],
			    "Stop": [
			      {"hooks": [
			        {"type": "command", "command": "/opt/other/notify.sh"},
			        {"type": "command", "command": "/usr/bin/curl http://example.invalid"}
			      ]}
			    ]
			  }
			}`,
			want: "must only remove",
		},
		{
			name:   "a matcher is dropped from a surviving wrapper",
			before: afterDoc,
			after: `{
			  "model": "opus",
			  "permissions": {"allow": ["Bash(ls:*)"]},
			  "hooks": {
			    "PreToolUse": [
			      {"hooks": [
			        {"type": "command", "command": "/usr/bin/mylint"},
			        {"type": "command", "command": "/usr/bin/other"}
			      ]}
			    ],
			    "Stop": [{"hooks": [{"type": "command", "command": "/opt/other/notify.sh"}]}]
			  }
			}`,
			want: "PreToolUse",
		},
		{
			name:   "a key outside hooks changes",
			before: afterDoc,
			after: `{
			  "model": "sonnet",
			  "permissions": {"allow": ["Bash(ls:*)"]},
			  "hooks": {
			    "PreToolUse": [
			      {"matcher": "Bash", "hooks": [
			        {"type": "command", "command": "/usr/bin/mylint"},
			        {"type": "command", "command": "/usr/bin/other"}
			      ]}
			    ],
			    "Stop": [{"hooks": [{"type": "command", "command": "/opt/other/notify.sh"}]}]
			  }
			}`,
			want: `"model" would change`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyUninstallSafe(doc(t, tt.before), doc(t, tt.after), verifyPrefix)
			if err == nil {
				t.Fatal("verifyUninstallSafe accepted a corrupted document")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestVerifyGuardsTheRealMergeCode runs the invariant against what
// mergeEntries and removeOwnedEntries actually produce, so the checks and the
// code they guard cannot drift apart.
func TestVerifyGuardsTheRealMergeCode(t *testing.T) {
	before := doc(t, unrelatedSettings)
	after, err := cloneSettings(before)
	if err != nil {
		t.Fatalf("cloneSettings: %v", err)
	}
	entries := claudeHookEntries(fakeCommand)
	if err := mergeEntries(after, entries); err != nil {
		t.Fatalf("mergeEntries: %v", err)
	}
	if err := verifyMergeSafe(before, after, fakeCommand); err != nil {
		t.Fatalf("verifyMergeSafe rejected mergeEntries' own output: %v", err)
	}

	back, err := cloneSettings(after)
	if err != nil {
		t.Fatalf("cloneSettings: %v", err)
	}
	if removed := removeOwnedEntries(back, fakeCommand); len(removed) != len(entries) {
		t.Fatalf("removeOwnedEntries removed %d entries, want %d", len(removed), len(entries))
	}
	if err := verifyUninstallSafe(after, back, fakeCommand); err != nil {
		t.Fatalf("verifyUninstallSafe rejected removeOwnedEntries' own output: %v", err)
	}
	// And the round trip lands back on the original document.
	if err := verifyMergeSafe(before, back, fakeCommand); err != nil {
		t.Fatalf("uninstall did not restore the original: %v", err)
	}
	if err := verifyUninstallSafe(before, back, fakeCommand); err != nil {
		t.Fatalf("uninstall changed something it did not add: %v", err)
	}
}

func TestSummarizeChangeCountsEntries(t *testing.T) {
	before := doc(t, beforeDoc)
	after := doc(t, afterDoc)
	sum := summarizeChange(before, after)
	if sum.Added != 3 || sum.Removed != 0 || sum.Modified != 0 {
		t.Fatalf("summary = +%d -%d ~%d, want +3 -0 ~0", sum.Added, sum.Removed, sum.Modified)
	}
	got := map[string]eventChange{}
	for _, e := range sum.Events {
		got[e.Event] = e
	}
	if e := got["PreToolUse"]; e.Kept != 2 || e.Added != 0 {
		t.Fatalf("PreToolUse = %+v, want 2 kept and nothing added", e)
	}
	if e := got["Stop"]; e.Kept != 1 || e.Added != 1 {
		t.Fatalf("Stop = %+v, want 1 kept and 1 added", e)
	}
	if e := got["SessionStart"]; e.Kept != 0 || e.Added != 2 || !e.IsNew {
		t.Fatalf("SessionStart = %+v, want a new event with 2 added", e)
	}

	// The other direction is the same change read backwards.
	sum = summarizeChange(after, before)
	if sum.Added != 0 || sum.Removed != 3 || sum.Modified != 0 {
		t.Fatalf("reverse summary = +%d -%d ~%d, want +0 -3 ~0", sum.Added, sum.Removed, sum.Modified)
	}
}

// TestSummarizeChangeCountsARewriteAsModified covers the same hook rewritten
// with different flags. The removal and the addition are both reported - they
// really do happen - and Modified says the pair is one entry rewritten rather
// than an entry lost and an unrelated one gained.
func TestSummarizeChangeCountsARewriteAsModified(t *testing.T) {
	before := doc(t, `{"hooks": {"Stop": [{"hooks": [
		{"type": "command", "command": "/opt/aq/agentqueue hook claude"}]}]}}`)
	after := doc(t, `{"hooks": {"Stop": [{"hooks": [
		{"type": "command", "command": "/opt/aq/agentqueue hook claude --no-block"}]}]}}`)
	sum := summarizeChange(before, after)
	if sum.Added != 1 || sum.Removed != 1 || sum.Modified != 1 {
		t.Fatalf("summary = +%d -%d ~%d, want +1 -1 ~1", sum.Added, sum.Removed, sum.Modified)
	}
	if len(sum.Events) != 1 || sum.Events[0].Kept != 0 {
		t.Fatalf("events = %+v, want the rewritten entry counted as removed, not kept", sum.Events)
	}
}

// TestVerifyUpdateSafe covers the gate the stale-path update passes through.
// It is the loosest of the three - it both adds and removes - so what it
// still refuses is the interesting part.
func TestVerifyUpdateSafe(t *testing.T) {
	// What convergeEntries actually produces is accepted.
	before := doc(t, `{
	  "model": "opus",
	  "hooks": {
	    "Stop": [{"matcher": "*", "hooks": [
	      {"type": "command", "command": "/opt/other/notify.sh"},
	      {"type": "command", "command": "/old/place/agentqueue hook claude"}
	    ]}]
	  }
	}`)
	after, err := cloneSettings(before)
	if err != nil {
		t.Fatalf("cloneSettings: %v", err)
	}
	added, removed, err := convergeEntries(after, verifyPrefix, claudeHookEntries(verifyPrefix))
	if err != nil {
		t.Fatalf("convergeEntries: %v", err)
	}
	if len(added) != 5 || len(removed) != 1 {
		t.Fatalf("convergeEntries added %d and removed %d, want 5 and 1", len(added), len(removed))
	}
	if err := verifyUpdateSafe(before, after, verifyPrefix); err != nil {
		t.Fatalf("verifyUpdateSafe rejected convergeEntries' own output: %v", err)
	}
	// An update is not allowed to leave a second copy behind either: the
	// stale entry is gone, not kept.
	if err := verifyMergeSafe(before, after, verifyPrefix); err == nil {
		t.Fatal("verifyMergeSafe accepted a document with an entry removed")
	}

	tests := []struct {
		name  string
		after string
		want  string
	}{
		{
			name: "somebody else's entry is removed along the way",
			after: `{
			  "model": "opus",
			  "hooks": {
			    "Stop": [{"matcher": "*", "hooks": [
			      {"type": "command", "command": "/opt/aq/agentqueue hook claude"}
			    ]}]
			  }
			}`,
			want: "/opt/other/notify.sh",
		},
		{
			name: "a foreign entry is added along the way",
			after: `{
			  "model": "opus",
			  "hooks": {
			    "Stop": [{"matcher": "*", "hooks": [
			      {"type": "command", "command": "/opt/other/notify.sh"},
			      {"type": "command", "command": "/opt/aq/agentqueue hook claude"},
			      {"type": "command", "command": "/usr/bin/curl http://example.invalid"}
			    ]}]
			  }
			}`,
			want: "not an agentqueue hook",
		},
		{
			name: "the wrapper's matcher is rewritten",
			after: `{
			  "model": "opus",
			  "hooks": {
			    "Stop": [{"matcher": "Bash", "hooks": [
			      {"type": "command", "command": "/opt/other/notify.sh"},
			      {"type": "command", "command": "/opt/aq/agentqueue hook claude"}
			    ]}]
			  }
			}`,
			want: "matcher",
		},
		{
			name: "a key outside hooks changes",
			after: `{
			  "model": "sonnet",
			  "hooks": {
			    "Stop": [{"matcher": "*", "hooks": [
			      {"type": "command", "command": "/opt/other/notify.sh"},
			      {"type": "command", "command": "/opt/aq/agentqueue hook claude"}
			    ]}]
			  }
			}`,
			want: `"model" would change`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyUpdateSafe(before, doc(t, tt.after), verifyPrefix)
			if err == nil {
				t.Fatal("verifyUpdateSafe accepted a corrupted document")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}
