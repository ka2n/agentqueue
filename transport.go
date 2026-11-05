package agentqueue

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNotify means a persisted message's notification failed.
var ErrNotify = errors.New("notify failed")

// ErrNotificationUncertain means a notification may have been submitted, but a
// verifiable host receipt or its durable recording is unavailable.
var ErrNotificationUncertain = errors.New("notification result uncertain")

// ErrDeliveryInProgress means another notifier owns the target reservation.
var ErrDeliveryInProgress = errors.New("notification in progress")

// ErrDeliveryAccepted is retained for callers of the previous body-handoff API.
// Notifications no longer archive or acknowledge message bodies.
var ErrDeliveryAccepted = ErrNotificationUncertain

type Notice struct {
	Target      Target
	ItemID      string
	Pending     int
	FetchCmd    string
	Item        *Item
	ID          string
	GeneratedAt time.Time
	Address     *Address
	Receiver    *Receiver
}

// Transport is the host integration for a target. A no-op keeps existing hook
// and extension receivers available without claiming an idle wake capability.
type Transport interface {
	Name() string
	Notify(context.Context, Notice) error
}

// ReceiptTransport reports native notification submission independently of
// payload ownership and application acknowledgement.
type ReceiptTransport interface {
	Transport
	NotifyWithReceipt(context.Context, Notice) (HostReceipt, error)
}

// DirectTransport is retained for source compatibility; notifications do not
// hand off or acknowledge the body even when an old adapter implements it.
type DirectTransport interface {
	Transport
	DirectDelivery() bool
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

// PushAndNotify stores the body before notifying; neither notification nor its
// receipt changes pending/claimed/done payload state.
func (q *Queue) PushAndNotify(ctx context.Context, t Target, text string, meta map[string]string) (*Item, error) {
	if _, ok := Lookup(t.Agent); !ok {
		return nil, fmt.Errorf("%w: no transport registered for agent %q; register a transport with agentqueue.Register", ErrInvalidTarget, t.Agent)
	}
	item, err := q.Push(t, text, meta)
	if err != nil {
		return nil, err
	}
	return item, q.Notify(ctx, t, item.ID)
}

// Notify submits a signal for currently unnotified unread messages. Accepted
// notification snapshots are durable and separate from body read/ACK state.
func (q *Queue) Notify(ctx context.Context, t Target, id string) error {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\") {
		return fmt.Errorf("%w: invalid item id", ErrNotFound)
	}
	transport, ok := Lookup(t.Agent)
	if !ok {
		return fmt.Errorf("%w: no transport registered for agent %q", ErrInvalidTarget, t.Agent)
	}
	return q.notifyPending(ctx, t, id, transport)
}
