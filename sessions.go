package agentqueue

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Session is a discovered agent session. Mailbox and Registered describe the
// matching target in the agentqueue root, rather than the agent's own storage.
type Session struct {
	Agent        string    `json:"agent"`
	SessionID    string    `json:"session_id"`
	Cwd          string    `json:"cwd"`
	Label        string    `json:"label,omitempty"`
	LastActivity time.Time `json:"last_activity"`
	Mailbox      bool      `json:"mailbox"`
	Registered   bool      `json:"registered"`
}

// CommandRunner runs an agent CLI. It is injectable so callers and tests can
// exercise the documented Claude JSON interface without depending on a real
// installation.
type CommandRunner func(context.Context, string, ...string) ([]byte, error)

// SessionListerOptions configures the storage locations used by the session
// listers. Empty agent-specific paths follow each agent's normal environment
// variables and home-directory defaults.
type SessionListerOptions struct {
	HomeDir         string
	QueueRoot       string
	ClaudeConfigDir string
	CodexHome       string
	PiAgentDir      string
	ClaudeCommand   string
	CommandRunner   CommandRunner
}

// SessionLister discovers sessions for one agent.
type SessionLister interface {
	List(context.Context) ([]Session, error)
}

// NewSessionListers returns the built-in listers keyed by their target agent
// name. Each lister is independent, so adding an agent does not change the
// storage logic of the others.
func NewSessionListers(opts SessionListerOptions) map[string]SessionLister {
	return map[string]SessionLister{
		"claude": NewClaudeSessionLister(opts),
		"codex":  NewCodexSessionLister(opts),
		"pi":     NewPiSessionLister(opts),
	}
}

// SortSessions sorts sessions newest first, with deterministic tie breakers.
func SortSessions(sessions []Session) {
	sort.SliceStable(sessions, func(i, j int) bool {
		left, right := sessions[i], sessions[j]
		if !left.LastActivity.Equal(right.LastActivity) {
			return left.LastActivity.After(right.LastActivity)
		}
		if left.Agent != right.Agent {
			return left.Agent < right.Agent
		}
		if left.SessionID != right.SessionID {
			return left.SessionID < right.SessionID
		}
		return left.Cwd < right.Cwd
	})
}

func (o SessionListerOptions) homeDir() (string, error) {
	value := strings.TrimSpace(o.HomeDir)
	if value == "" {
		value = strings.TrimSpace(os.Getenv("HOME"))
	}
	if value == "" {
		var err error
		value, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
	}
	return absoluteSessionPath(value, value)
}

func (o SessionListerOptions) queueRoot() (string, error) {
	home, err := o.homeDir()
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(o.QueueRoot)
	if value == "" {
		value = strings.TrimSpace(os.Getenv("AGENTQUEUE_ROOT"))
	}
	if value == "" {
		if xdg := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); xdg != "" {
			value = filepath.Join(xdg, "agentqueue")
		} else {
			value = filepath.Join(home, ".local", "state", "agentqueue")
		}
	}
	return absoluteSessionPath(value, home)
}

func absoluteSessionPath(value, home string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("empty session path")
	}
	if value == "~" {
		value = home
	} else if strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~`+string(filepath.Separator)) {
		value = filepath.Join(home, value[2:])
	}
	path, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve session path %q: %w", value, err)
	}
	return filepath.Clean(path), nil
}

func (o SessionListerOptions) configuredPath(value, fallback string) (string, error) {
	home, err := o.homeDir()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		value = fallback
	}
	return absoluteSessionPath(value, home)
}

func (o SessionListerOptions) command(ctx context.Context, name string, args ...string) ([]byte, error) {
	if o.CommandRunner != nil {
		return o.CommandRunner(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).Output()
}

func finishSessions(opts SessionListerOptions, sessions []Session) ([]Session, error) {
	root, err := opts.queueRoot()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]Session, len(sessions))
	for _, session := range sessions {
		if strings.TrimSpace(session.Agent) == "" || strings.TrimSpace(session.SessionID) == "" || strings.TrimSpace(session.Cwd) == "" {
			continue
		}
		session.Agent = strings.TrimSpace(session.Agent)
		session.SessionID = strings.TrimSpace(session.SessionID)
		session.Cwd = filepath.Clean(strings.TrimSpace(session.Cwd))
		if session.Label == "" {
			session.Label = filepath.Base(session.Cwd)
		}
		session.Mailbox, session.Registered = targetStatus(root, Target{Agent: session.Agent, Name: session.SessionID})
		key := session.Agent + "\x00" + session.SessionID
		if previous, ok := seen[key]; !ok || session.LastActivity.After(previous.LastActivity) {
			seen[key] = session
		}
	}
	out := make([]Session, 0, len(seen))
	for _, session := range seen {
		out = append(out, session)
	}
	SortSessions(out)
	return out, nil
}

func targetStatus(root string, target Target) (mailbox, registered bool) {
	agent, err := sanitizeSegment(target.Agent)
	if err != nil {
		return false, false
	}
	name, err := sanitizeSegment(target.Name)
	if err != nil {
		return false, false
	}
	base := filepath.Join(root, agent, name)
	if info, err := os.Stat(base); err != nil || !info.IsDir() {
		return false, false
	} else {
		mailbox = true
	}
	if info, err := os.Stat(filepath.Join(base, addrFileName)); err == nil && !info.IsDir() {
		registered = true
	}
	return mailbox, registered
}

// parseSessionTime accepts the timestamp forms used by the three agent
// stores: RFC3339 text, Unix seconds, and Unix milliseconds.
func parseSessionTime(raw json.RawMessage) time.Time {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return time.Time{}
	}
	if strings.HasPrefix(value, `"`) {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return time.Time{}
		}
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed
		}
		value = text
	}
	if integer, err := strconv.ParseInt(value, 10, 64); err == nil {
		if integer > 1e11 || integer < -1e11 {
			return time.UnixMilli(integer).UTC()
		}
		return time.Unix(integer, 0).UTC()
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return time.Time{}
	}
	if number > 1e11 || number < -1e11 {
		number /= 1000
	}
	seconds := int64(number)
	nanos := int64((number - float64(seconds)) * float64(time.Second))
	return time.Unix(seconds, nanos).UTC()
}

func parseTextTime(value string) time.Time {
	if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value)); err == nil {
		return parsed
	}
	return time.Time{}
}

func readFirstLine(path string, decode func([]byte) bool) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	scanner := newLineScanner(file)
	if !scanner.Scan() {
		return false
	}
	return decode(scanner.Bytes())
}

// newLineScanner keeps a large enough limit for Codex's metadata line, which
// includes serialized instructions, while still refusing unbounded input.
func newLineScanner(file *os.File) *bufio.Scanner {
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	return scanner
}
