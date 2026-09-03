package agentqueue

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// maxNoticeOutput caps how much command output is quoted back in an error.
const maxNoticeOutput = 500

// codexTransport serves "codex" targets by shelling out to the codex CLI.
//
// Codex sessions are push-based: `codex queue --thread <name> --message <text>`
// injects a message into a running thread. Only the arrival notice is pushed,
// never the item body, so the agent fetches the payload itself.
type codexTransport struct {
	// run executes the notify command. It is injectable so tests can assert the
	// argv without spawning codex.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Name returns the agent name this transport serves.
func (c *codexTransport) Name() string { return "codex" }

// Notify pushes a one-line arrival notice into the codex thread.
func (c *codexTransport) Notify(ctx context.Context, n Notice) error {
	run := c.run
	if run == nil {
		run = execRun
	}
	msg := noticeMessage(n)
	out, err := run(ctx, "codex", "queue", "--thread", n.Target.Name, "--message", msg)
	if err != nil {
		return fmt.Errorf("codex queue: %w: %s", err, truncate(strings.TrimSpace(string(out)), maxNoticeOutput))
	}
	return nil
}

// noticeMessage renders the single-line notice. It names the item and how to
// fetch it, and never includes the item text.
func noticeMessage(n Notice) string {
	pending := n.Pending
	if pending < 1 {
		pending = 1
	}
	return fmt.Sprintf("[agentqueue] 1 new message for %s (id %s, %d pending). Fetch it with: %s",
		n.Target, n.ItemID, pending, n.FetchCmd)
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
