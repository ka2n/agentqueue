package agentqueue

import "context"

// piTransport serves "pi" targets. Pi's extension watches its mailbox, so a
// filesystem write is the arrival signal and no separate notification is
// needed here.
type piTransport struct{}

func (p *piTransport) Name() string { return "pi" }

func (p *piTransport) Notify(context.Context, Notice) error { return nil }

func init() { Register(&piTransport{}) }
