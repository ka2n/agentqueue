package agentqueue

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

const notificationTimeout = 10 * time.Second
const uncertainLifetime = 30 * time.Second
const drainLifetime = 5 * time.Minute

// HostReceipt is a submission receipt, never a body read or processing receipt.
// queued has a verifiable host queue ID; written is a socket write only;
// deferred means the existing receiving integration must deliver the body.
type HostReceipt struct {
	State    string `json:"state"`
	QueueID  string `json:"queue_id,omitempty"`
	ThreadID string `json:"thread_id,omitempty"`
}

type NotificationStatus struct {
	WakeProtocol int         `json:"wake_protocol,omitempty"`
	Coalescing   bool        `json:"coalescing,omitempty"`
	ID           string      `json:"id"`
	State        string      `json:"state"`
	GeneratedAt  time.Time   `json:"generated_at"`
	NotifiedIDs  []string    `json:"notified_ids"`
	AttemptIDs   []string    `json:"attempt_ids,omitempty"`
	DrainUntil   time.Time   `json:"drain_until,omitempty"`
	Receipt      HostReceipt `json:"receipt"`
}

// Notification reads only transport state; it does not read or consume bodies.
func (q *Queue) Notification(t Target) (NotificationStatus, error) {
	base, err := q.targetDir(t)
	if err != nil {
		return NotificationStatus{}, err
	}
	data, err := os.ReadFile(filepath.Join(base, "notification.json"))
	if os.IsNotExist(err) {
		return NotificationStatus{}, nil
	}
	if err != nil {
		return NotificationStatus{}, err
	}
	var state NotificationStatus
	if err := json.Unmarshal(data, &state); err != nil {
		return NotificationStatus{}, fmt.Errorf("decode notification state: %w", err)
	}
	return state, nil
}

func writePrivateJSON(base, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Join(base, tmpDirName), "state-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(base, name))
}

func (q *Queue) lockNotification(ctx context.Context, t Target) (string, func(), error) {
	base, err := q.ensureDirs(t)
	if err != nil {
		return "", nil, err
	}
	lock := flock.New(filepath.Join(base, "notification.lock"))
	locked, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil || !locked {
		lock.Close()
		if err == nil {
			err = ErrDeliveryInProgress
		}
		return "", nil, err
	}
	return base, func() { lock.Close() }, nil
}

func (s NotificationStatus) flightActive(now time.Time) bool {
	return s.Coalescing && s.WakeProtocol == WakeProtocolVersion && (s.State == "queued" || s.State == "receiving" && now.Before(s.DrainUntil))
}

func (q *Queue) notifyPending(ctx context.Context, t Target, id string, transport Transport) error {
	base, unlock, err := q.lockNotification(ctx, t)
	if err != nil {
		return err
	}
	defer unlock()
	state, err := q.Notification(t)
	if err != nil {
		return err
	}
	pending, err := q.List(t, StatePending)
	if err != nil {
		return err
	}
	var requested *Item
	ids := make([]string, 0, len(pending))
	for i := range pending {
		if pending[i].Target != t {
			return fmt.Errorf("%w: stored target mismatch", ErrInvalidTarget)
		}
		ids = append(ids, pending[i].ID)
		if pending[i].ID == id {
			requested = &pending[i]
		}
	}
	if requested == nil {
		for _, s := range []State{StateClaimed, StateDone} {
			if _, err := readItem(filepath.Join(base, s.String(), id+".json")); err == nil {
				return nil
			} else if !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		return ErrNotFound
	}
	address, addressErr := q.Address(t)
	if addressErr != nil && !errors.Is(addressErr, ErrNotFound) {
		return addressErr
	}
	compatible := address != nil && address.WakeProtocol == WakeProtocolVersion
	now := q.now().UTC()
	if compatible && state.flightActive(now) {
		state.NotifiedIDs = ids
		return writePrivateJSON(base, "notification.json", state)
	}
	if state.State == "written" || state.State == "queued" && !state.Coalescing {
		for _, known := range state.NotifiedIDs {
			if known == id {
				return nil
			}
		}
	}
	if (state.State == "submitting" || state.State == "uncertain") && now.Before(state.GeneratedAt.Add(uncertainLifetime)) {
		return ErrDeliveryInProgress
	}
	encoded, _ := json.Marshal(ids)
	sum := sha256.Sum256(append([]byte(t.String()+"\n"), encoded...))
	notice := Notice{Target: t, ItemID: id, Pending: len(pending), FetchCmd: q.FetchCmd(t), Item: requested, ID: hex.EncodeToString(sum[:16]), GeneratedAt: now}
	notice.Address = address
	if receiver, e := q.Receiver(t); e == nil {
		notice.Receiver = receiver
	} else if !errors.Is(e, ErrNotFound) {
		return e
	}
	native, isNative := transport.(ReceiptTransport)
	if !isNative {
		if err := transport.Notify(ctx, notice); err != nil {
			return fmt.Errorf("%w: %w", ErrNotify, err)
		}
		return nil
	}
	if compatible {
		if state.WakeProtocol == WakeProtocolVersion && state.ID != "" && (state.State == "failed" || state.State == "uncertain" || state.State == "submitting") {
			notice.ID = state.ID
		} else {
			var identity [16]byte
			if _, err := rand.Read(identity[:]); err != nil {
				return err
			}
			notice.ID = hex.EncodeToString(identity[:])
		}
		notice.FetchCmd += " --notice " + notice.ID
		state.WakeProtocol = WakeProtocolVersion
	} else {
		state.WakeProtocol = 0
	}
	state.Coalescing = false
	state.Receipt = HostReceipt{}
	state.ID, state.State, state.GeneratedAt, state.AttemptIDs = notice.ID, "submitting", now, ids
	if err := writePrivateJSON(base, "notification.json", state); err != nil {
		return err
	}
	attempt, cancel := context.WithTimeout(ctx, notificationTimeout)
	defer cancel()
	receipt, deliveryErr := native.NotifyWithReceipt(attempt, notice)
	if deliveryErr != nil {
		state.State = "failed"
		if errors.Is(deliveryErr, ErrNotificationUncertain) {
			state.State = "uncertain"
		}
		if err := writePrivateJSON(base, "notification.json", state); err != nil {
			return errors.Join(deliveryErr, err)
		}
		return fmt.Errorf("%w: %w", ErrNotify, deliveryErr)
	}
	if receipt.State != "queued" && receipt.State != "written" && receipt.State != "deferred" {
		return fmt.Errorf("%w: invalid host receipt", ErrNotificationUncertain)
	}
	state.State, state.Receipt = receipt.State, receipt
	state.Coalescing = compatible && receipt.State == "queued"
	if receipt.State != "deferred" {
		state.NotifiedIDs = ids
	}
	state.AttemptIDs = nil
	if err := writePrivateJSON(base, "notification.json", state); err != nil {
		return fmt.Errorf("%w: receipt persistence failed: %w", ErrNotificationUncertain, err)
	}
	return nil
}
