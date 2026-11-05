package agentqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// maxNoticeOutput caps how much command output is quoted back in an error.
const maxNoticeOutput = 500

// codexTransport hands the complete message to Codex's own queue. Command
// success is host acceptance, not proof that the model has processed the body.
type codexTransport struct {
	// run executes the notify command. It is injectable so tests can assert the
	// argv without spawning codex.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Name returns the agent name this transport serves.
func (c *codexTransport) Name() string { return "codex" }

func (c *codexTransport) DirectDelivery() bool { return true }

// Notify hands the complete body to the named Codex thread.
func (c *codexTransport) Notify(ctx context.Context, n Notice) error {
	run := c.run
	if run == nil {
		run = execRun
	}
	if n.Item == nil || strings.TrimSpace(n.Item.Text) == "" {
		return fmt.Errorf("Codex delivery requires a message body")
	}
	msg := deliveryMessage(n)
	out, err := run(ctx, "codex", "queue", "--thread", n.Target.Name, "--message", msg)
	if err != nil {
		return fmt.Errorf("codex queue: %w: %s", err, truncate(strings.TrimSpace(string(out)), maxNoticeOutput))
	}
	return nil
}

func deliveryMessage(n Notice) string {
	var message strings.Builder
	fmt.Fprintf(&message, "[agentqueue message id=%s target=%s created_at=%s]\n", n.ItemID, n.Target, n.Item.CreatedAt.UTC().Format(time.RFC3339Nano))
	if len(n.Item.Meta) > 0 {
		metadata, _ := json.Marshal(n.Item.Meta)
		fmt.Fprintf(&message, "metadata: %s\n", metadata)
	}
	message.WriteString("\n")
	message.WriteString(n.Item.Text)
	message.WriteString("\n\n[agentqueue: body delivered directly; no queue fetch or transport ACK is required.]")
	return message.String()
}

// execRun is the default runner: run the command and capture both streams.
func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// truncate shortens s to at most max bytes, marking that it was cut.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "... (truncated)"
}

func init() { Register(&codexTransport{}) }
