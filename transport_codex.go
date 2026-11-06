package agentqueue

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

const maxNoticeOutput = 500

var queuedReceipt = regexp.MustCompile(`(?m)^Queued message ([0-9a-fA-F-]{36}) for thread ([0-9a-fA-F-]{36})\.?\s*$`)

type codexTransport struct {
	run        func(context.Context, string, ...string) ([]byte, error)
	runProfile func(context.Context, string, string, ...string) ([]byte, error)
}

func (*codexTransport) Name() string { return "codex" }
func (c *codexTransport) Notify(ctx context.Context, n Notice) error {
	_, err := c.NotifyWithReceipt(ctx, n)
	return err
}
func (c *codexTransport) NotifyWithReceipt(ctx context.Context, n Notice) (HostReceipt, error) {
	if n.Address == nil || !filepath.IsAbs(n.Address.Profile) {
		return HostReceipt{State: "deferred"}, nil
	}
	target, err := uuid.Parse(n.Target.Name)
	if err != nil {
		return HostReceipt{}, fmt.Errorf("Codex notification requires a thread UUID")
	}
	args := []string{"queue", "--thread", n.Target.Name, "--message", noticeMessage(n)}
	var out []byte
	if c.runProfile != nil {
		out, err = c.runProfile(ctx, n.Address.Profile, "codex", args...)
	} else if c.run != nil {
		out, err = c.run(ctx, "codex", args...)
	} else {
		command := exec.CommandContext(ctx, "codex", args...)
		env := make([]string, 0, len(os.Environ())+1)
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "CODEX_HOME=") {
				env = append(env, value)
			}
		}
		command.Env = append(env, "CODEX_HOME="+n.Address.Profile)
		out, err = command.CombinedOutput()
	}
	if err != nil {
		if ctx.Err() != nil {
			return HostReceipt{}, fmt.Errorf("%w: Codex queue timed out or was cancelled", ErrNotificationUncertain)
		}
		return HostReceipt{}, fmt.Errorf("codex queue: %w: %s", err, truncate(strings.TrimSpace(string(out)), maxNoticeOutput))
	}
	matches := queuedReceipt.FindStringSubmatch(string(out))
	if len(matches) != 3 {
		return HostReceipt{}, fmt.Errorf("%w: unrecognized Codex queue receipt", ErrNotificationUncertain)
	}
	if _, err := uuid.Parse(matches[1]); err != nil {
		return HostReceipt{}, fmt.Errorf("%w: invalid queue ID", ErrNotificationUncertain)
	}
	received, err := uuid.Parse(matches[2])
	if err != nil || target != received {
		return HostReceipt{}, fmt.Errorf("%w: queue receipt names another thread", ErrNotificationUncertain)
	}
	return HostReceipt{State: "queued", QueueID: matches[1], ThreadID: matches[2]}, nil
}
func noticeMessage(n Notice) string {
	return fmt.Sprintf("[agentqueue notice %s; generated %s] Your inbox changed. Run this receive command unchanged for each batch:\n%s\nHandle and ACK each item, then repeat the same command until empty. Preserve --notice when present: it acknowledges only this notification, even if bodies were delivered earlier. This is a signal, not a new request. An empty result needs no retry.", n.ID, n.GeneratedAt.UTC().Format(time.RFC3339Nano), n.FetchCmd)
}
func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "... (truncated)"
}
func init() { Register(&codexTransport{}) }
