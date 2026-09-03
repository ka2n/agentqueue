package agentqueue

import "context"

// claudeTransport serves "claude" targets.
//
// Delivery to a Claude Code session happens outside this transport. With
// `agentqueue install`, hooks claim pending items and inject them at the next
// session boundary; without hooks, the session itself runs
// `agentqueue wait --to claude:<name>` as a background command and the harness
// resumes the turn when that command exits. Neither is a channel a producer can
// push a notice into, so Notify is a no-op and enqueuing is complete once the
// item is stored.
type claudeTransport struct{}

// Name returns the agent name this transport serves.
func (c *claudeTransport) Name() string { return "claude" }

// Notify does nothing; delivery happens when the session's Wait returns.
func (c *claudeTransport) Notify(context.Context, Notice) error { return nil }

func init() { Register(&claudeTransport{}) }
