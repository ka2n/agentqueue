package hookserve

import (
	"errors"
	"github.com/ka2n/agentqueue"
	"testing"
)

func TestReceiverRegistrationBindsCredentialsToPayloadSession(t *testing.T) {
	q := openQueue(t)
	env := map[string]string{"CLAUDE_CODE_MESSAGING_SOCKET": "/tmp/socket", "CLAUDE_CODE_MESSAGING_TOKEN": "secret"}
	getenv := func(k string) string { return env[k] }
	target := agentqueue.Target{Agent: "claude", Name: "own-session"}
	if err := RegisterAddress(q, target, Payload{SessionID: "another-session"}, getenv); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Receiver(target); !errors.Is(err, agentqueue.ErrNotFound) {
		t.Fatalf("registered foreign credentials: %v", err)
	}
	if err := RegisterAddress(q, target, Payload{SessionID: target.Name, PermissionMode: "bypassPermissions"}, getenv); err != nil {
		t.Fatal(err)
	}
	receiver, err := q.Receiver(target)
	if err != nil || receiver.Token != "secret" || receiver.PermissionMode != "bypassPermissions" {
		t.Fatalf("receiver=%+v %v", receiver, err)
	}
	delete(env, "CLAUDE_CODE_MESSAGING_TOKEN")
	if err := RegisterAddress(q, target, Payload{SessionID: target.Name}, getenv); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Receiver(target); !errors.Is(err, agentqueue.ErrNotFound) {
		t.Fatalf("stale credentials retained: %v", err)
	}
}
