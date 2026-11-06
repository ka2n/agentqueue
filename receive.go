package agentqueue

import (
	"context"
	"encoding/hex"
	"errors"
	"time"
)

// Receive claims bodies without acknowledging any queued native wake.
// Only ReceiveNotice can observe a specific notification.
func (q *Queue) Receive(t Target, limit int) ([]Item, error) {
	ctx, cancel := context.WithTimeout(context.Background(), notificationTimeout)
	defer cancel()
	return q.receive(ctx, t, limit, "")
}

// ReceiveAutomatic shares notification locking without acknowledging the
// queued wake. Hooks can deliver bodies before that wake reaches the model.
func (q *Queue) ReceiveAutomatic(ctx context.Context, t Target, limit int) ([]Item, error) {
	return q.receive(ctx, t, limit, "")
}

// ReceiveNotice observes only the named wake. Stale or completed IDs are
// successful no-ops and cannot consume bodies belonging to another flight.
func (q *Queue) ReceiveNotice(t Target, id string, limit int) ([]Item, error) {
	if decoded, err := hex.DecodeString(id); err != nil || len(decoded) != 16 {
		return nil, errors.New("notice must be a 32-character hexadecimal notification ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), notificationTimeout)
	defer cancel()
	return q.receive(ctx, t, limit, id)
}

func (q *Queue) receive(ctx context.Context, t Target, limit int, noticeID string) ([]Item, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	base, unlock, err := q.lockNotification(ctx, t)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var state NotificationStatus
	if noticeID != "" {
		state, err = q.Notification(t)
		if err != nil {
			return nil, err
		}
		if state.ID != noticeID || state.WakeProtocol != WakeProtocolVersion || state.State == "received" || state.State == "deferred" {
			return []Item{}, nil
		}
	}
	pending, err := q.List(t, StatePending)
	if err != nil {
		return nil, err
	}
	now := q.now().UTC()
	if noticeID != "" {
		state.Coalescing = true
		if len(pending) == 0 {
			state.State = "received"
			state.DrainUntil = now
		} else {
			state.State = "receiving"
			state.DrainUntil = now.Add(drainLifetime)
		}
		if err := writePrivateJSON(base, "notification.json", state); err != nil {
			return nil, err
		}
	}
	received := make([]Item, 0, min(limit, len(pending)))
	for _, item := range pending {
		if len(received) == limit {
			break
		}
		if err := ctx.Err(); err != nil {
			return received, err
		}
		claimed, err := q.Take(t, item.ID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return received, err
		}
		received = append(received, *claimed)
	}
	if noticeID != "" && len(received) == 0 && state.State != "received" {
		state.State = "received"
		state.DrainUntil = now
		if err := writePrivateJSON(base, "notification.json", state); err != nil {
			return received, err
		}
	}
	return received, nil
}

// WaitReceive waits and claims atomically with other automatic receivers.
// Consuming a body does not mean a previously queued wake was observed.
func (q *Queue) WaitReceive(ctx context.Context, t Target, timeout time.Duration, limit int) ([]Item, error) {
	return q.wait(ctx, t, timeout, func(ctx context.Context, t Target) ([]Item, error) { return q.ReceiveAutomatic(ctx, t, limit) })
}
