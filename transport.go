package agentqueue

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrNotify means the item was persisted but its arrival notice failed.
var ErrNotify = errors.New("agentqueue: notify failed")

// Notice is the arrival notification for a queued item.
//
// It deliberately omits the item body: the notification path only tells the
// agent that something arrived and how to fetch it, so a large or sensitive
// payload never travels over the notification channel.
type Notice struct {
	Target   Target
	ItemID   string
	Pending  int
	FetchCmd string
}

// Transport delivers arrival notices to one kind of agent.
type Transport interface {
	// Name is the agent name this transport serves, as used in a Target.
	Name() string
	// Notify delivers the arrival notice. Pull-based transports may no-op.
	Notify(ctx context.Context, n Notice) error
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Transport{}
)

// Register adds t to the package registry, keyed by its Name.
// A later registration for the same name replaces the earlier one.
func Register(t Transport) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[t.Name()] = t
}

// Lookup returns the transport registered for an agent name.
func Lookup(agent string) (Transport, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	t, ok := registry[agent]
	return t, ok
}

// Agents lists the registered agent names in sorted order.
func Agents() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// PushAndNotify persists an item and then delivers its arrival notice.
//
// A notify failure never loses the item: the item stays pending and is returned
// alongside an ErrNotify-wrapped error, so the caller can report or retry.
func (q *Queue) PushAndNotify(ctx context.Context, t Target, text string, meta map[string]string) (*Item, error) {
	transport, ok := Lookup(t.Agent)
	if !ok {
		return nil, fmt.Errorf("%w: no transport registered for agent %q; register a transport with agentqueue.Register", ErrInvalidTarget, t.Agent)
	}

	item, err := q.Push(t, text, meta)
	if err != nil {
		return nil, err
	}

	pending, err := q.List(t, StatePending)
	if err != nil {
		return item, fmt.Errorf("%w: count pending: %w", ErrNotify, err)
	}

	notice := Notice{
		Target:   t,
		ItemID:   item.ID,
		Pending:  len(pending),
		FetchCmd: q.FetchCmd(t),
	}
	if err := transport.Notify(ctx, notice); err != nil {
		return item, fmt.Errorf("%w: %s: %w", ErrNotify, t, err)
	}
	return item, nil
}
