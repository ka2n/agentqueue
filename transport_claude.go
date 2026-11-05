package agentqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
)

type claudeTransport struct{}

func (*claudeTransport) Name() string { return "claude" }
func (c *claudeTransport) Notify(ctx context.Context, n Notice) error {
	_, err := c.NotifyWithReceipt(ctx, n)
	return err
}
func (*claudeTransport) NotifyWithReceipt(ctx context.Context, n Notice) (HostReceipt, error) {
	if runtime.GOOS == "windows" || n.Receiver == nil || n.Receiver.Socket == "" || n.Receiver.Token == "" {
		return HostReceipt{State: "deferred"}, nil
	}
	socket := strings.TrimPrefix(n.Receiver.Socket, "uds:")
	conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", socket)
	if err != nil {
		return HostReceipt{}, fmt.Errorf("Claude receiver connection failed: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return HostReceipt{}, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	auth, _ := json.Marshal(map[string]string{"type": "auth", "token": n.Receiver.Token})
	attributes := ""
	if n.Receiver.PermissionMode == "bypassPermissions" {
		attributes = ` from-name="agentqueue" from-mode="bypass"`
	}
	content := fmt.Sprintf("<cross-session-message from=\"agentqueue\"%s>\n%s\n</cross-session-message>", attributes, noticeMessage(n))
	frame, _ := json.Marshal(map[string]any{"msgV": 1, "msg_id": uuid.NewString(), "type": "user", "message": map[string]string{"role": "user", "content": content}, "priority": "next", "from": "agentqueue"})
	if _, err := io.WriteString(conn, string(auth)+"\n"+string(frame)+"\n"); err != nil {
		return HostReceipt{}, fmt.Errorf("%w: Claude socket write failed", ErrNotificationUncertain)
	}
	return HostReceipt{State: "written"}, nil
}
func init() { Register(&claudeTransport{}) }
