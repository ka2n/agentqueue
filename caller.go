package agentqueue

import (
	"fmt"
	"github.com/ka2n/crossagent/processcontext"
	"os"
	"path/filepath"
	"strings"
)

// Caller identifies the session receiving replies and its queue directory.
type Caller struct {
	Target Target `json:"target"`
	Root   string `json:"root"`
}

// CallerOptions supplies explicit overrides and a host environment. Getenv nil
// uses os.Getenv. Hosts without a session environment must export
// CROSSAGENT_AGENT/CROSSAGENT_SESSION_ID from their lifecycle integration.
type CallerOptions struct {
	Target string
	Root   string
	Getenv func(string) string
}

// ResolveCaller resolves only authoritative session context; it never guesses
// from working directories, mailbox contents, or recently active sessions.
func ResolveCaller(opts CallerOptions) (Caller, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	target := strings.TrimSpace(opts.Target)
	if target == "" {
		current, err := processcontext.Resolve(processcontext.Options{Getenv: getenv})
		if err != nil {
			return Caller{}, fmt.Errorf("cannot identify caller: %w", err)
		}
		target = current.Agent.String() + ":" + current.SessionID
	}
	parsed, err := ParseTarget(target)
	if err != nil {
		return Caller{}, err
	}
	root, err := ResolveRoot(opts.Root, getenv)
	if err != nil {
		return Caller{}, err
	}
	return Caller{Target: parsed, Root: root}, nil
}

// ResolveRoot resolves an explicit root, AGENTQUEUE_ROOT, then the XDG state
// directory. Application-specific overrides belong in the caller.
func ResolveRoot(root string, getenv func(string) string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	root = strings.TrimSpace(root)
	if root == "" {
		root = strings.TrimSpace(getenv("AGENTQUEUE_ROOT"))
	}
	if root == "" {
		if state := strings.TrimSpace(getenv("XDG_STATE_HOME")); state != "" {
			root = filepath.Join(state, "agentqueue")
		} else {
			home := strings.TrimSpace(getenv("HOME"))
			if home == "" {
				return "", fmt.Errorf("cannot resolve queue root: supply a root, AGENTQUEUE_ROOT, XDG_STATE_HOME or HOME")
			}
			root = filepath.Join(home, ".local", "state", "agentqueue")
		}
	}
	return filepath.Abs(root)
}
