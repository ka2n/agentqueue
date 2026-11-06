package agentqueue

import (
	"context"
	"errors"
	"time"
)

// Receive claims an inbox batch and owns the drain until an empty receive.
// Callers handle and ACK each batch before receiving again.
func (q *Queue) Receive(t Target, limit int) ([]Item, error) {
	ctx, cancel := context.WithTimeout(context.Background(), notificationTimeout)
	defer cancel()
	return q.receive(ctx, t, limit, false)
}

// ReceiveAutomatic shares notification locking without acknowledging the
// queued wake. Hooks can deliver bodies before that wake reaches the model.
func (q *Queue) ReceiveAutomatic(ctx context.Context, t Target, limit int) ([]Item, error) {
	return q.receive(ctx, t, limit, true)
}

func (q *Queue) receive(ctx context.Context, t Target, limit int, automatic bool) ([]Item, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	base, unlock, err := q.lockNotification(ctx, t)
	if err != nil {
		return nil, err
	}
	defer unlock()
	state, err := q.Notification(t)
	if err != nil {
		return nil, err
	}
	pending, err := q.List(t, StatePending)
	if err != nil {
		return nil, err
	}
	now := q.now().UTC()
	if !automatic && (state.State == "queued" || state.State == "receiving") {
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
	return received, nil
}

// WaitReceive waits and claims atomically with other automatic receivers.
// Consuming a body does not mean a previously queued wake was observed.
func (q *Queue) WaitReceive(ctx context.Context, t Target, timeout time.Duration, limit int) ([]Item, error) {
	return q.wait(ctx, t, timeout, func(ctx context.Context, t Target) ([]Item, error) { return q.ReceiveAutomatic(ctx, t, limit) })
}
