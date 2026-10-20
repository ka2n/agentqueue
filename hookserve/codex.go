package hookserve

import (
	"fmt"
	"strings"

	"github.com/ka2n/agentqueue"
	"github.com/ka2n/crossagent/agent"
)

const CodexSelfCheckToken = "agentqueue hook codex: self-check ok"

// HandleCodex claims queued messages only at events that accept model-visible
// context. Stop is deliberately excluded because its output contract differs.
func HandleCodex(q *agentqueue.Queue, payload Payload, opts Options) (*Output, bool, error) {
	switch payload.HookEventName {
	case eventPostToolUse, eventUserPromptSubmit:
	default:
		return nil, false, nil
	}
	var target agentqueue.Target
	if to := strings.TrimSpace(opts.To); to != "" {
		var err error
		target, err = agentqueue.ParseTarget(to)
		if err != nil {
			return nil, false, err
		}
	} else {
		name := strings.TrimSpace(payload.SessionID)
		if name == "" {
			name = strings.TrimSpace(opts.getenv()("CODEX_THREAD_ID"))
		}
		if name == "" {
			opts.logf("no target for %s event: no --to, session_id or $CODEX_THREAD_ID", payload.HookEventName)
			return nil, false, nil
		}
		target = agentqueue.Target{Agent: agent.Codex.String(), Name: name}
	}
	if target.Agent != agent.Codex.String() {
		return nil, false, fmt.Errorf("Codex hook target must use codex namespace: %s", target)
	}
	if err := RegisterAddress(q, target, payload, opts.getenv()); err != nil {
		opts.logf("register %s: %v", target, err)
	}
	items, err := ClaimUpTo(q, target, opts.Max)
	if err != nil || len(items) == 0 {
		return nil, false, err
	}
	return &Output{HookSpecificOutput: &HookSpecificOutput{
		HookEventName:     payload.HookEventName,
		AdditionalContext: RenderDelivery(q, target, items),
	}}, true, nil
}
