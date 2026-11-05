package agentqueue

import "errors"

// Receive claims a bounded inbox snapshot. No messages is a successful stale
// signal, not a delivery failure. Callers ACK only IDs actually handled.
func (q *Queue) Receive(t Target, limit int) ([]Item, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	pending, err := q.List(t, StatePending)
	if err != nil {
		return nil, err
	}
	received := make([]Item, 0, min(limit, len(pending)))
	for _, item := range pending {
		if len(received) == limit {
			break
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
