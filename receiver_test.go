package agentqueue

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateReceiverAndNativeSignal(t *testing.T) {
	root, err := os.MkdirTemp("", "aq-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	socket := filepath.Join(root, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	frames := make(chan []string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			frames <- nil
			return
		}
		defer conn.Close()
		scanner := bufio.NewScanner(conn)
		var lines []string
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		frames <- lines
	}()
	q, err := Open(filepath.Join(root, "queue"))
	if err != nil {
		t.Fatal(err)
	}
	target := Target{Agent: "claude", Name: "session"}
	if err := q.PutAddress(Address{Agent: target.Agent, Name: target.Name}); err != nil {
		t.Fatal(err)
	}
	if err := q.PutReceiver(target, Receiver{Socket: socket, Token: "secret-token", PermissionMode: "bypassPermissions"}); err != nil {
		t.Fatal(err)
	}
	public, _ := q.Address(target)
	data, _ := json.Marshal(public)
	if strings.Contains(string(data), "secret-token") {
		t.Fatal("private token leaked")
	}
	item, err := q.PushAndNotify(context.Background(), target, "private body", nil)
	if err != nil {
		t.Fatal(err)
	}
	lines := <-frames
	if len(lines) != 2 {
		t.Fatalf("frames=%v", lines)
	}
	var auth map[string]string
	if err := json.Unmarshal([]byte(lines[0]), &auth); err != nil || auth["token"] != "secret-token" {
		t.Fatalf("auth=%v %v", auth, err)
	}
	var frame struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &frame); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(frame.Message.Content, "private body") || !strings.Contains(frame.Message.Content, "receive") || !strings.Contains(frame.Message.Content, `from-mode="bypass"`) {
		t.Fatalf("signal=%s", frame.Message.Content)
	}
	status, err := q.Notification(target)
	if err != nil || status.Receipt.State != "written" {
		t.Fatalf("status=%+v %v", status, err)
	}
	pending, err := q.List(target, StatePending)
	if err != nil || len(pending) != 1 || pending[0].ID != item.ID {
		t.Fatalf("pending=%+v %v", pending, err)
	}
	if err := q.RemoveAddress(target); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Receiver(target); err != ErrNotFound {
		t.Fatalf("receiver survived unregister: %v", err)
	}
}
