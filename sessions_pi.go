package agentqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PiSessionLister enumerates Pi's JSONL session files under its agent
// directory. It does not depend on Pi being installed or running.
type PiSessionLister struct {
	options SessionListerOptions
}

// NewPiSessionLister creates a Pi session lister.
func NewPiSessionLister(options SessionListerOptions) *PiSessionLister {
	return &PiSessionLister{options: options}
}

type piSessionHeader struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
}

func (l *PiSessionLister) List(ctx context.Context) ([]Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	home, err := l.options.homeDir()
	if err != nil {
		return nil, err
	}
	agentDir := strings.TrimSpace(l.options.PiAgentDir)
	if agentDir == "" {
		agentDir = strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR"))
	}
	if agentDir == "" {
		agentDir = filepath.Join(home, ".pi", "agent")
	}
	agentDir, err = l.options.configuredPath(agentDir, filepath.Join(home, ".pi", "agent"))
	if err != nil {
		return nil, fmt.Errorf("resolve Pi agent directory: %w", err)
	}

	var sessions []Session
	sessionsDir := filepath.Join(agentDir, "sessions")
	err = filepath.WalkDir(sessionsDir, func(path string, entry os.DirEntry, walkErr error) error {
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
		id, ok := parsePiSessionName(entry.Name())
		if !ok {
			return nil
		}
		header, ok := readPiSessionHeader(path)
		if !ok || strings.TrimSpace(header.Cwd) == "" {
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
			lastActivity = parseTextTime(header.Timestamp)
		}
		sessions = append(sessions, Session{
			Agent:        "pi",
			SessionID:    id,
			Cwd:          header.Cwd,
			Label:        filepath.Base(filepath.Clean(header.Cwd)),
			LastActivity: lastActivity,
		})
		return nil
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("scan Pi sessions: %w", err)
	}
	return finishSessions(l.options, sessions)
}

func readPiSessionHeader(path string) (piSessionHeader, bool) {
	var header piSessionHeader
	if !readFirstLine(path, func(line []byte) bool {
		if json.Unmarshal(line, &header) != nil {
			return false
		}
		return header.Type == "session" && strings.TrimSpace(header.ID) != ""
	}) {
		return piSessionHeader{}, false
	}
	return header, true
}

func parsePiSessionName(name string) (string, bool) {
	base := strings.TrimSuffix(name, ".jsonl")
	separator := strings.LastIndexByte(base, '_')
	if separator < 0 || separator == len(base)-1 {
		return "", false
	}
	id := strings.TrimSpace(base[separator+1:])
	if id == "" {
		return "", false
	}
	return id, true
}

// EncodePiCWD returns the directory key used by Pi's
// getDefaultSessionDirPath: the resolved path loses its leading separator,
// every slash, backslash, or Windows drive colon becomes '-', and the result
// is wrapped in double hyphens. Dots and underscores are retained.
func EncodePiCWD(cwd string) string {
	cwd = filepath.Clean(cwd)
	if resolved, err := filepath.Abs(cwd); err == nil {
		cwd = resolved
	}
	if strings.HasPrefix(cwd, "/") || strings.HasPrefix(cwd, `\`) {
		cwd = cwd[1:]
	}
	var encoded strings.Builder
	encoded.Grow(len(cwd) + 4)
	for i := range len(cwd) {
		switch cwd[i] {
		case '/', '\\', ':':
			encoded.WriteByte('-')
		default:
			encoded.WriteByte(cwd[i])
		}
	}
	return "--" + encoded.String() + "--"
}
