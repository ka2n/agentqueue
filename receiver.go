package agentqueue

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Receiver is private host routing/authentication material. It is not included
// in Address, session listings, CLI registration output or notification receipts.
type Receiver struct {
	Socket         string `json:"socket"`
	Token          string `json:"token"`
	PermissionMode string `json:"permission_mode,omitempty"`
}

func (q *Queue) PutReceiver(t Target, receiver Receiver) error {
	base, err := q.ensureDirs(t)
	if err != nil {
		return err
	}
	if err := q.secureRoot(); err != nil {
		return err
	}
	if receiver.Socket == "" || receiver.Token == "" {
		return q.RemoveReceiver(t)
	}
	return writePrivateJSON(base, "receiver.private.json", receiver)
}

func (q *Queue) Receiver(t Target) (*Receiver, error) {
	base, err := q.targetDir(t)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(base, "receiver.private.json")
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("receiver registration must be a private regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var receiver Receiver
	if err := json.Unmarshal(data, &receiver); err != nil {
		return nil, fmt.Errorf("decode private receiver: %w", err)
	}
	return &receiver, nil
}

func (q *Queue) RemoveReceiver(t Target) error {
	base, err := q.targetDir(t)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(base, "receiver.private.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
