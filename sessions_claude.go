package agentqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ClaudeSessionLister discovers sessions from Claude's CLI and project
// transcripts. The sources are intentionally combined: the CLI does not report
// every interactive session.
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
	State          string          `json:"state"`
	StartedAt      json.RawMessage `json:"startedAt"`
	UpdatedAt      json.RawMessage `json:"updatedAt"`
	LastActivityAt json.RawMessage `json:"lastActivityAt"`
}

type claudeCandidate struct {
	id           string
	cwd          string
	cwdRank      int
	label        string
	labelRank    int
	state        string
	stateRank    int
	lastActivity time.Time
	activityRank int
	source       string
}

const maxClaudePrefixBytes = 2 * 1024 * 1024

// List unions all usable Claude sources and merges duplicate session ids.
func (l *ClaudeSessionLister) List(ctx context.Context) ([]Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	home, err := l.options.homeDir()
	if err != nil {
		return nil, err
	}
	configDir, err := l.configDir(home)
	if err != nil {
		return nil, err
	}
	cutoff := l.options.now().Add(-l.options.claudeMaxAge())

	var candidates []claudeCandidate
	cliCandidates, err := l.listCLISessions(ctx)
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, cliCandidates...)

	transcriptCandidates, err := l.listTranscripts(ctx, configDir, cutoff)
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, transcriptCandidates...)

	return finishSessions(l.options, mergeClaudeCandidates(candidates))
}

func (l *ClaudeSessionLister) configDir(home string) (string, error) {
	configDir := strings.TrimSpace(l.options.ClaudeConfigDir)
	if configDir == "" {
		configDir = strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	}
	if configDir == "" {
		configDir = filepath.Join(home, ".claude")
	}
	configDir, err := l.options.configuredPath(configDir, filepath.Join(home, ".claude"))
	if err != nil {
		return "", fmt.Errorf("resolve Claude config directory: %w", err)
	}
	return configDir, nil
}

func (l *ClaudeSessionLister) listCLISessions(ctx context.Context) ([]claudeCandidate, error) {
	command := strings.TrimSpace(l.options.ClaudeCommand)
	if command == "" {
		command = "claude"
	}
	output, err := l.options.command(ctx, command, "agents", "--json")
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil
	}
	var rows []claudeAgentRow
	if err := json.Unmarshal(output, &rows); err != nil {
		return nil, nil
	}

	candidates := make([]claudeCandidate, 0, len(rows))
	for _, row := range rows {
		id := strings.TrimSpace(row.SessionID)
		if id == "" {
			id = strings.TrimSpace(row.ID)
		}
		lastActivity, activityRank := claudeActivity(
			[]json.RawMessage{row.LastActivityAt, row.UpdatedAt, row.StartedAt},
			[]int{4, 3, 1},
		)
		state := row.State
		candidate := claudeCandidate{
			id:           id,
			cwd:          strings.TrimSpace(row.Cwd),
			cwdRank:      1,
			label:        strings.TrimSpace(row.Name),
			labelRank:    4,
			state:        state,
			stateRank:    2,
			lastActivity: lastActivity,
			activityRank: activityRank,
			source:       sourceClaudeAgents,
		}
		if candidate.cwd == "" {
			candidate.cwdRank = 0
		}
		if candidate.label == "" {
			candidate.labelRank = 0
		}
		if candidate.state == "" {
			candidate.stateRank = 0
		}
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

func (l *ClaudeSessionLister) listTranscripts(ctx context.Context, configDir string, cutoff time.Time) ([]claudeCandidate, error) {
	projectsDir := filepath.Join(configDir, "projects")
	projects, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, nil
	}

	var candidates []claudeCandidate
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
			// One inaccessible project should not hide all other sources.
			continue
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			info, err := entry.Info()
			if err != nil || info.ModTime().Before(cutoff) {
				continue
			}
			path := filepath.Join(projectsDir, project.Name(), entry.Name())
			id, cwd, ok := readClaudeTranscript(path, strings.TrimSuffix(entry.Name(), ".jsonl"))
			if !ok {
				continue
			}
			candidates = append(candidates, claudeCandidate{
				id:           id,
				cwd:          cwd,
				cwdRank:      5,
				lastActivity: info.ModTime(),
				activityRank: 2,
				source:       sourceClaudeTranscript,
			})
		}
	}
	return candidates, nil
}

func readClaudeTranscript(path, fallbackID string) (id, cwd string, ok bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer file.Close()
	scanner := newClaudeLineScanner(file)
	var scanned int
	for scanner.Scan() {
		line := scanner.Bytes()
		var row struct {
			SessionID       string `json:"sessionId"`
			LegacySessionID string `json:"session_id"`
			ID              string `json:"id"`
			Cwd             string `json:"cwd"`
		}
		if json.Unmarshal(line, &row) == nil && strings.TrimSpace(row.Cwd) != "" {
			id = strings.TrimSpace(row.SessionID)
			if id == "" {
				id = strings.TrimSpace(row.LegacySessionID)
			}
			if id == "" {
				id = strings.TrimSpace(row.ID)
			}
			if id == "" {
				id = fallbackID
			}
			if id != "" {
				return id, filepath.Clean(strings.TrimSpace(row.Cwd)), true
			}
		}
		scanned += len(line) + 1
		if scanned >= maxClaudePrefixBytes {
			break
		}
	}
	return "", "", false
}

func claudeActivity(raw []json.RawMessage, ranks []int) (time.Time, int) {
	for i, value := range raw {
		if parsed := parseSessionTime(value); !parsed.IsZero() {
			return parsed, ranks[i]
		}
	}
	return time.Time{}, 0
}

func mergeClaudeCandidates(candidates []claudeCandidate) []Session {
	type mergedCandidate struct {
		session                 Session
		sources                 map[string]bool
		cwdRank, labelRank      int
		stateRank, activityRank int
	}
	merged := make(map[string]*mergedCandidate, len(candidates))
	for _, candidate := range candidates {
		id := strings.TrimSpace(candidate.id)
		if id == "" {
			continue
		}
		current := merged[id]
		if current == nil {
			current = &mergedCandidate{
				session: Session{
					Agent:     "claude",
					SessionID: id,
				},
				sources: make(map[string]bool),
			}
			merged[id] = current
		}
		current.sources[candidate.source] = true
		if candidate.cwd != "" && candidate.cwdRank > current.cwdRank {
			current.session.Cwd = filepath.Clean(candidate.cwd)
			current.cwdRank = candidate.cwdRank
		}
		if candidate.label != "" && candidate.labelRank > current.labelRank {
			current.session.Label = candidate.label
			current.labelRank = candidate.labelRank
		}
		if candidate.state != "" && candidate.stateRank > current.stateRank {
			current.session.State = candidate.state
			current.stateRank = candidate.stateRank
		}
		if candidate.activityRank > current.activityRank ||
			(candidate.activityRank == current.activityRank && candidate.lastActivity.After(current.session.LastActivity)) {
			current.session.LastActivity = candidate.lastActivity
			current.activityRank = candidate.activityRank
		}
	}

	out := make([]Session, 0, len(merged))
	for _, candidate := range merged {
		candidate.session.Source = joinClaudeSources(candidate.sources)
		out = append(out, candidate.session)
	}
	return out
}

func joinClaudeSources(sources map[string]bool) string {
	ordered := []string{sourceClaudeAgents, sourceClaudeTranscript}
	parts := make([]string, 0, len(sources))
	for _, source := range ordered {
		if sources[source] {
			parts = append(parts, source)
		}
	}
	return strings.Join(parts, "+")
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
