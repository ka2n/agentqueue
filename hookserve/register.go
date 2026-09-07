package hookserve

import (
	"errors"
	"os"
	"strings"

	"github.com/ka2n/agentqueue"
	"github.com/ka2n/crossagent/agent"
)

// errNoRegisterTarget means neither an explicit target, a payload session id,
// nor $CLAUDE_CODE_SESSION_ID identified a session to register.
var errNoRegisterTarget = errors.New("no target, session_id or $CLAUDE_CODE_SESSION_ID")

// AddressFor builds the address to record for a session, preferring the hook
// payload over the environment for every field it carries. pid is stored for
// diagnostics only; a hook has no direct handle on the session's own pid, so the
// caller typically passes os.Getppid(). getenv nil means os.Getenv.
func AddressFor(t agentqueue.Target, payload Payload, getenv func(string) string, pid int) agentqueue.Address {
	if getenv == nil {
		getenv = os.Getenv
	}
	cwd := strings.TrimSpace(payload.Cwd)
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	return agentqueue.Address{
		Agent: t.Agent,
		Name:  t.Name,
		Cwd:   cwd,
		PID:   pid,
		// Recorded for diagnostics only. Writing to this socket from a
		// process that is not itself a Claude Code session is accepted and
		// then dropped, so it is not a delivery path. The token that goes
		// with it ($CLAUDE_CODE_MESSAGING_TOKEN) is a credential and is
		// deliberately never stored.
		Socket:       strings.TrimSpace(getenv("CLAUDE_CODE_MESSAGING_SOCKET")),
		AgentVersion: strings.TrimSpace(getenv("CLAUDE_CODE_VERSION")),
	}
}

// RegisterAddress records the session's address. It is used both by a register
// subcommand and by the SessionStart delivery path. getenv nil means os.Getenv.
func RegisterAddress(q *agentqueue.Queue, t agentqueue.Target, payload Payload, getenv func(string) string) error {
	// In a hook the parent is the process that spawned it - the agent or the
	// shell it ran the hook through - which is the closest thing to the
	// session's own pid available without probing. It is diagnostic only.
	return q.PutAddress(AddressFor(t, payload, getenv, os.Getppid()))
}

// RegisterTarget derives the target to register: to wins, then sessionID, then
// $CLAUDE_CODE_SESSION_ID, under the given mailbox agent. Unlike sessions,
// address records are queue storage and may use a custom agent name; an empty
// agentName defaults to claude. getenv nil means os.Getenv.
func RegisterTarget(to, agentName, sessionID string, getenv func(string) string) (agentqueue.Target, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if s := strings.TrimSpace(to); s != "" {
		target, err := agentqueue.ParseTarget(s)
		if err != nil {
			return agentqueue.Target{}, err
		}
		return target, nil
	}
	session := strings.TrimSpace(sessionID)
	if session == "" {
		session = strings.TrimSpace(getenv("CLAUDE_CODE_SESSION_ID"))
	}
	if session == "" {
		return agentqueue.Target{}, errNoRegisterTarget
	}
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		agentName = agent.Claude.String()
	}
	return agentqueue.Target{Agent: agentName, Name: session}, nil
}
