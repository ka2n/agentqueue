package agentqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ClaudeSessionLister discovers active sessions through `claude agents --json`
// and falls back to Claude's transcript tree when that command is unavailable.
type ClaudeSessionLister struct {
	options SessionListerOptions
}

// NewClaudeSessionLister creates a Claude session lister.
func NewClaudeSessionLister(options SessionListerOptions) *ClaudeSessionLister {
	return &ClaudeSessionLister{options: options}
}

type claudeAgentRow struct {
	ID             string          `json:"id"`
	Cwd            string          `json:"cwd"`
	SessionID      string          `json:"sessionId"`
	Name           string          `json:"name"`
	StartedAt      json.RawMessage `json:"startedAt"`
	UpdatedAt      json.RawMessage `json:"updatedAt"`
	LastActivityAt json.RawMessage `json:"lastActivityAt"`
}

func (l *ClaudeSessionLister) List(ctx context.Context) ([]Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	command := strings.TrimSpace(l.options.ClaudeCommand)
	if command == "" {
		command = "claude"
	}
	if output, err := l.options.command(ctx, command, "agents", "--json"); err == nil {
		var rows []claudeAgentRow
		if json.Unmarshal(output, &rows) == nil {
			sessions := make([]Session, 0, len(rows))
			for _, row := range rows {
				id := strings.TrimSpace(row.SessionID)
				if id == "" {
					id = strings.TrimSpace(row.ID)
				}
				lastActivity := parseSessionTime(row.LastActivityAt)
				if lastActivity.IsZero() {
					lastActivity = parseSessionTime(row.UpdatedAt)
				}
				if lastActivity.IsZero() {
					lastActivity = parseSessionTime(row.StartedAt)
				}
				sessions = append(sessions, Session{
					Agent:        "claude",
					SessionID:    id,
					Cwd:          row.Cwd,
					Label:        row.Name,
					LastActivity: lastActivity,
				})
			}
			return finishSessions(l.options, sessions)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return l.listTranscripts(ctx)
}

func (l *ClaudeSessionLister) listTranscripts(ctx context.Context) ([]Session, error) {
	home, err := l.options.homeDir()
	if err != nil {
		return nil, err
	}
	configDir := strings.TrimSpace(l.options.ClaudeConfigDir)
	if configDir == "" {
		configDir = strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	}
	if configDir == "" {
		configDir = filepath.Join(home, ".claude")
	}
	configDir, err = l.options.configuredPath(configDir, filepath.Join(home, ".claude"))
	if err != nil {
		return nil, fmt.Errorf("resolve Claude config directory: %w", err)
	}
	projectsDir := filepath.Join(configDir, "projects")
	projects, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return finishSessions(l.options, nil)
		}
		return nil, fmt.Errorf("read Claude projects: %w", err)
	}

	var sessions []Session
	for _, project := range projects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !project.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(projectsDir, project.Name()))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read Claude project %s: %w", project.Name(), err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(projectsDir, project.Name(), entry.Name())
			id, cwd, ok := readClaudeTranscript(path, strings.TrimSuffix(entry.Name(), ".jsonl"))
			if !ok {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("stat Claude transcript %s: %w", path, err)
			}
			sessions = append(sessions, Session{
				Agent:        "claude",
				SessionID:    id,
				Cwd:          cwd,
				LastActivity: info.ModTime(),
			})
		}
	}
	return finishSessions(l.options, sessions)
}

func readClaudeTranscript(path, fallbackID string) (id, cwd string, ok bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer file.Close()
	scanner := newLineScanner(file)
	for scanner.Scan() {
		var line struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
		}
		if json.Unmarshal(scanner.Bytes(), &line) != nil || strings.TrimSpace(line.Cwd) == "" {
			continue
		}
		id = strings.TrimSpace(line.SessionID)
		if id == "" {
			id = fallbackID
		}
		if id == "" {
			return "", "", false
		}
		return id, filepath.Clean(strings.TrimSpace(line.Cwd)), true
	}
	return "", "", false
}

// EncodeClaudeCWD returns the project-directory key Claude Code uses for a
// working directory. Every byte outside ASCII letters and digits becomes '-';
// this includes slash, dot, underscore, and an existing hyphen remains '-'.
func EncodeClaudeCWD(cwd string) string {
	var encoded strings.Builder
	encoded.Grow(len(cwd))
	for i := range len(cwd) {
		c := cwd[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			encoded.WriteByte(c)
		} else {
			encoded.WriteByte('-')
		}
	}
	return encoded.String()
}
