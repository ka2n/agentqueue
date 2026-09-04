// Package agentqueue is a dependency-free message queue for AI coding-agent
// sessions.
//
// An external producer pushes a message for a named agent session; the session
// consumes it. Delivery is per-agent: a "claude" session is delivered to by
// Claude Code hooks, which claim pending items and inject them at the next
// session boundary, and can also pull by blocking on Wait; a "codex" target is
// pushed an arrival notice.
//
// Storage is a directory tree with one subtree per target. All state changes are
// atomic renames, so multiple processes can share a queue root without locks.
//
// The library is pure Go and builds with CGO disabled.
package agentqueue

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sentinel errors returned by the queue.
var (
	// ErrEmpty means there was no pending item to claim.
	ErrEmpty = errors.New("queue is empty")
	// ErrTimeout means Wait reached its deadline without an item arriving.
	ErrTimeout = errors.New("timeout waiting for item")
	// ErrNotFound means the requested item id does not exist in the queue.
	ErrNotFound = errors.New("item not found")
	// ErrInvalidTarget means a target string or its agent/name parts are unusable.
	ErrInvalidTarget = errors.New("invalid target")
)

// defaultAgent is the agent assumed when a target string carries no "agent:" prefix.
const defaultAgent = "claude"

// Target names a single agent session queue.
type Target struct {
	Agent string `json:"agent"`
	Name  string `json:"name"`
}

// String renders the target in its "agent:name" wire form.
func (t Target) String() string {
	return t.Agent + ":" + t.Name
}

// ParseTarget parses an "agent:name" target string. A string without a colon is
// read as a name for the default agent, so "foo" means "claude:foo".
func ParseTarget(s string) (Target, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Target{}, fmt.Errorf("%w: empty target", ErrInvalidTarget)
	}
	agent, name := defaultAgent, s
	if i := strings.Index(s, ":"); i >= 0 {
		agent, name = s[:i], s[i+1:]
	}
	agent = strings.TrimSpace(agent)
	name = strings.TrimSpace(name)
	if agent == "" {
		return Target{}, fmt.Errorf("%w: empty agent in %q", ErrInvalidTarget, s)
	}
	if name == "" {
		return Target{}, fmt.Errorf("%w: empty name in %q", ErrInvalidTarget, s)
	}
	return Target{Agent: agent, Name: name}, nil
}

// State is the lifecycle stage of an item, and also the directory holding it.
type State int

// Item states, in lifecycle order.
const (
	StatePending State = iota
	StateClaimed
	StateDone
)

// String returns the directory name for the state.
func (s State) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateClaimed:
		return "claimed"
	case StateDone:
		return "done"
	default:
		return fmt.Sprintf("state(%d)", int(s))
	}
}

// ParseState parses a state name as printed by State.String.
func ParseState(s string) (State, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "pending", "":
		return StatePending, nil
	case "claimed":
		return StateClaimed, nil
	case "done":
		return StateDone, nil
	default:
		return 0, fmt.Errorf("unknown state %q", s)
	}
}

// Item is one queued message.
type Item struct {
	ID        string            `json:"id"`
	Target    Target            `json:"target"`
	Text      string            `json:"text"`
	Meta      map[string]string `json:"meta,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// newID generates an item id that sorts by creation time and does not collide.
// It is a variable so tests can stub it.
var newID = func() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand never fails on supported platforms; fall back to the clock
		// so id generation cannot panic in a queue push.
		copy(b[:], fmt.Sprintf("%06d", time.Now().Nanosecond()%1000000))
	}
	return fmt.Sprintf("%013d-%s", time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}
