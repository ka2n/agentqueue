package agentqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CodexSessionLister enumerates rollout transcripts. The filesystem scan is
// the default deliberately: it needs no SQLite driver and still works when
// Codex's optional state database is missing, locked, or on an older schema.
type CodexSessionLister struct {
	options SessionListerOptions
}

// NewCodexSessionLister creates a Codex session lister.
func NewCodexSessionLister(options SessionListerOptions) *CodexSessionLister {
	return &CodexSessionLister{options: options}
}

type codexSessionMeta struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Payload   struct {
		ID        string `json:"id"`
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
		Timestamp string `json:"timestamp"`
	} `json:"payload"`
}

type codexSessionName struct {
	ID         string `json:"id"`
	ThreadName string `json:"thread_name"`
}

func (l *CodexSessionLister) List(ctx context.Context) ([]Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	home, err := l.options.homeDir()
	if err != nil {
		return nil, err
	}
	codexHome := strings.TrimSpace(l.options.CodexHome)
	if codexHome == "" {
		codexHome = strings.TrimSpace(os.Getenv("CODEX_HOME"))
	}
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	codexHome, err = l.options.configuredPath(codexHome, filepath.Join(home, ".codex"))
	if err != nil {
		return nil, fmt.Errorf("resolve Codex home: %w", err)
	}
	names := readCodexSessionNames(filepath.Join(codexHome, "session_index.jsonl"))
	rolloutsDir := filepath.Join(codexHome, "sessions")
	var sessions []Session
	err = filepath.WalkDir(rolloutsDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		id, started, ok := parseCodexRolloutName(entry.Name())
		if !ok {
			return nil
		}
		meta, ok := readCodexSessionMeta(path)
		if !ok || strings.TrimSpace(meta.Payload.Cwd) == "" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		lastActivity := info.ModTime()
		if lastActivity.IsZero() {
			lastActivity = started
		}
		label := names[id]
		if label == "" {
			label = filepath.Base(filepath.Clean(meta.Payload.Cwd))
		}
		sessions = append(sessions, Session{
			Agent:        "codex",
			SessionID:    id,
			Cwd:          meta.Payload.Cwd,
			Label:        label,
			LastActivity: lastActivity,
			Source:       sourceCodexRollout,
		})
		return nil
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("scan Codex sessions: %w", err)
	}
	return finishSessions(l.options, sessions)
}

func readCodexSessionMeta(path string) (codexSessionMeta, bool) {
	var meta codexSessionMeta
	if !readFirstLine(path, func(line []byte) bool {
		if json.Unmarshal(line, &meta) != nil {
			return false
		}
		return meta.Type == "session_meta" && (meta.Payload.Cwd != "" || meta.Payload.ID != "" || meta.Payload.SessionID != "")
	}) {
		return codexSessionMeta{}, false
	}
	return meta, true
}

func parseCodexRolloutName(name string) (id string, started time.Time, ok bool) {
	base := strings.TrimSuffix(name, ".jsonl")
	const prefix = "rollout-"
	if !strings.HasPrefix(base, prefix) {
		return "", time.Time{}, false
	}
	rest := strings.TrimPrefix(base, prefix)
	const timestampLength = len("2006-01-02T15-04-05")
	if len(rest) <= timestampLength {
		return "", time.Time{}, false
	}
	started, err := time.ParseInLocation("2006-01-02T15-04-05", rest[:timestampLength], time.Local)
	if err != nil {
		return "", time.Time{}, false
	}
	id = strings.TrimLeft(rest[timestampLength:], "-_")
	if id == "" {
		return "", time.Time{}, false
	}
	return id, started, true
}

func readCodexSessionNames(path string) map[string]string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	names := make(map[string]string)
	scanner := newLineScanner(file)
	for scanner.Scan() {
		var row codexSessionName
		if json.Unmarshal(scanner.Bytes(), &row) != nil || strings.TrimSpace(row.ID) == "" {
			continue
		}
		if strings.TrimSpace(row.ThreadName) != "" {
			names[row.ID] = row.ThreadName
		}
	}
	return names
}
