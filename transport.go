package agentqueue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ErrNotify means the item was persisted but its arrival notice failed.
var ErrNotify = errors.New("notify failed")

// ErrDeliveryAccepted means the host accepted the body but local archival failed.
// The item remains claimed; retrying delivery could repeat work in the host.
var ErrDeliveryAccepted = errors.New("host accepted delivery but archival failed")

// ErrDeliveryInProgress means another receiver owns the item. It may be sending
// or recovering an uncertain handoff; do not resend or assume host acceptance.
var ErrDeliveryInProgress = errors.New("delivery in progress or uncertain")

// Notice carries a queued item's identity and complete payload. Transports that
// opt in to direct delivery must accept the body before returning success.
type Notice struct {
	Target   Target
	ItemID   string
	Pending  int
	FetchCmd string
	Item     *Item
}

// Transport accepts queued-item handoffs for one kind of agent.
type Transport interface {
	// Name is the agent name this transport serves, as used in a Target.
	Name() string
	// Notify handles the payload. Pull-based transports may no-op until their receiver claims it.
	Notify(ctx context.Context, n Notice) error
}

// DirectTransport opts in to exclusive full-body handoff. Other transports keep
// their existing pending-item consumer protocol.
type DirectTransport interface {
	Transport
	DirectDelivery() bool
}

func directDelivery(transport Transport) bool {
	direct, ok := transport.(DirectTransport)
	return ok && direct.DirectDelivery()
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

// PushAndNotify persists an item and asks its transport to deliver it. Direct
// transports own the claim before publication; pull consumers retain pending items.
func (q *Queue) PushAndNotify(ctx context.Context, t Target, text string, meta map[string]string) (*Item, error) {
	transport, ok := Lookup(t.Agent)
	if !ok {
		return nil, fmt.Errorf("%w: no transport registered for agent %q; register a transport with agentqueue.Register", ErrInvalidTarget, t.Agent)
	}
	state := StatePending
	if directDelivery(transport) {
		state = StateClaimed
	}
	item, err := q.push(t, text, meta, state)
	if err != nil {
		return nil, err
	}
	return item, q.notifyItem(ctx, transport, *item)
}

// Notify retries the same persisted item. A direct transport claims it first;
// an item already claimed or done belongs to another consumer and is not resent.
func (q *Queue) Notify(ctx context.Context, t Target, id string) error {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\") {
		return fmt.Errorf("%w: invalid item id", ErrNotFound)
	}
	transport, ok := Lookup(t.Agent)
	if !ok {
		return fmt.Errorf("%w: no transport registered for agent %q", ErrInvalidTarget, t.Agent)
	}
	var item *Item
	var err error
	if directDelivery(transport) {
		item, err = q.Take(t, id)
	} else {
		dir, e := q.dir(t, StatePending)
		if e != nil {
			return e
		}
		item, err = readItem(filepath.Join(dir, id+".json"))
	}
	if errors.Is(err, ErrNotFound) {
		for _, state := range []State{StateClaimed, StateDone} {
			dir, e := q.dir(t, state)
			if e != nil {
				return e
			}
			if _, e = readItem(filepath.Join(dir, id+".json")); e == nil {
				if state == StateClaimed {
					return fmt.Errorf("%w: %s", ErrDeliveryInProgress, id)
				}
				return nil
			} else if !errors.Is(e, ErrNotFound) {
				return e
			}
		}
	}
	if err != nil {
		return err
	}
	if item.ID != id || item.Target != t {
		return fmt.Errorf("%w: persisted item identity mismatch", ErrInvalidTarget)
	}
	return q.notifyItem(ctx, transport, *item)
}

func (q *Queue) notifyItem(ctx context.Context, transport Transport, item Item) error {
	pending, err := q.List(item.Target, StatePending)
	if err != nil {
		return q.notificationFailure(transport, item, err)
	}
	notice := Notice{Target: item.Target, ItemID: item.ID, Pending: len(pending), FetchCmd: q.FetchCmd(item.Target), Item: &item}
	if err := transport.Notify(ctx, notice); err != nil {
		return q.notificationFailure(transport, item, err)
	}
	if directDelivery(transport) {
		if err := q.Ack(item.Target, item.ID); err != nil {
			return fmt.Errorf("%w: %s: %w; do not resend", ErrDeliveryAccepted, item.ID, err)
		}
	}
	return nil
}

func (q *Queue) notificationFailure(transport Transport, item Item, cause error) error {
	if directDelivery(transport) {
		base, err := q.targetDir(item.Target)
		if err == nil {
			err = os.Rename(filepath.Join(base, StateClaimed.String(), item.ID+".json"), filepath.Join(base, StatePending.String(), item.ID+".json"))
		}
		if err != nil {
			cause = errors.Join(cause, fmt.Errorf("restore pending item: %w", err))
		}
	}
	return fmt.Errorf("%w: %s: %w", ErrNotify, item.Target, cause)
}
