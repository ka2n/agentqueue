package agentqueue

import (
	"errors"
	"strings"
	"testing"
)

func TestParseTarget(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Target
		wantErr bool
	}{
		{name: "codex", in: "codex:abc", want: Target{Agent: "codex", Name: "abc"}},
		{name: "claude", in: "claude:x", want: Target{Agent: "claude", Name: "x"}},
		{name: "bare defaults to claude", in: "foo", want: Target{Agent: "claude", Name: "foo"}},
		{name: "surrounding spaces", in: "  codex:abc  ", want: Target{Agent: "codex", Name: "abc"}},
		{name: "name with colon", in: "codex:a:b", want: Target{Agent: "codex", Name: "a:b"}},
		{name: "empty", in: "", wantErr: true},
		{name: "empty agent", in: ":x", wantErr: true},
		{name: "empty name", in: "a:", wantErr: true},
		{name: "only spaces", in: "   ", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTarget(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseTarget(%q) = %+v, want error", tt.in, got)
				}
				if !errors.Is(err, ErrInvalidTarget) {
					t.Fatalf("ParseTarget(%q) error = %v, want ErrInvalidTarget", tt.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTarget(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("ParseTarget(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestLibraryErrorsHaveNoProgramPrefix(t *testing.T) {
	for name, err := range map[string]error{
		"ErrEmpty":         ErrEmpty,
		"ErrTimeout":       ErrTimeout,
		"ErrNotFound":      ErrNotFound,
		"ErrInvalidTarget": ErrInvalidTarget,
		"ErrNotify":        ErrNotify,
	} {
		if strings.HasPrefix(err.Error(), "agentqueue: ") {
			t.Fatalf("%s = %q, library errors must not name the CLI", name, err)
		}
	}
	if err := func() error { _, err := ParseState("unknown"); return err }(); strings.HasPrefix(err.Error(), "agentqueue: ") {
		t.Fatalf("ParseState error = %q, library errors must not name the CLI", err)
	}
	if _, err := Open(""); strings.HasPrefix(err.Error(), "agentqueue: ") {
		t.Fatalf("Open error = %q, library errors must not name the CLI", err)
	}
}

func TestTargetString(t *testing.T) {
	got := Target{Agent: "codex", Name: "abc"}.String()
	if got != "codex:abc" {
		t.Fatalf("String() = %q, want %q", got, "codex:abc")
	}
}

func TestStateString(t *testing.T) {
	tests := []struct {
		st   State
		want string
	}{
		{StatePending, "pending"},
		{StateClaimed, "claimed"},
		{StateDone, "done"},
	}
	for _, tt := range tests {
		if got := tt.st.String(); got != tt.want {
			t.Fatalf("State(%d).String() = %q, want %q", tt.st, got, tt.want)
		}
	}
}

func TestNewIDSortableAndUnique(t *testing.T) {
	seen := make(map[string]struct{})
	var prev string
	for range 50 {
		id := newID()
		if len(id) != 13+1+12 {
			t.Fatalf("newID() = %q, unexpected length %d", id, len(id))
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("newID() produced duplicate %q", id)
		}
		seen[id] = struct{}{}
		// Only the millisecond prefix is ordered; the random suffix breaks ties.
		if prev != "" && strings.Compare(id[:13], prev[:13]) < 0 {
			t.Fatalf("newID() = %q sorts before previous %q", id, prev)
		}
		prev = id
	}
}
