package agentqueue

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// addrFileName holds a session's address inside its target directory.
const addrFileName = "addr.json"

// byCwdDirName is the per-agent index from working directory to session name.
// It lives beside the session directories, so a session literally named
// "_by_cwd" would collide with it; the leading underscore makes that unlikely,
// since agent session ids are uuids or thread ids.
const byCwdDirName = "_by_cwd"

// maxCwdSegment bounds the readable part of a cwd index segment, leaving room
// for the hash suffix inside a 255-byte file name.
const maxCwdSegment = 80

// Address records where a live agent session can be reached, so a producer can
// address it by session name or by the directory it is working in.
//
// It deliberately has no field for the session's messaging token. Claude Code
// exports CLAUDE_CODE_MESSAGING_TOKEN alongside CLAUDE_CODE_MESSAGING_SOCKET,
// but that socket is not a usable delivery path for this library (raw writes
// from a non-session process are accepted and then silently dropped), so the
// token would buy nothing while turning every addr.json into a credential
// store. Socket is kept for diagnostics only.
type Address struct {
	Agent        string    `json:"agent"`
	Name         string    `json:"name"`
	Cwd          string    `json:"cwd,omitempty"`
	PID          int       `json:"pid,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
	Socket       string    `json:"socket,omitempty"`
	AgentVersion string    `json:"agent_version,omitempty"`
}

// Target returns the target this address belongs to.
func (a Address) Target() Target {
	return Target{Agent: a.Agent, Name: a.Name}
}

// cwdSegment reduces a working directory to one safe, collision-free path
// segment. The sanitized form is lossy - two distinct paths can sanitize to the
// same bytes - so a hash of the original is appended to keep the mapping
// injective while the segment stays readable.
func cwdSegment(cwd string) (string, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return "", fmt.Errorf("%w: empty cwd", ErrInvalidTarget)
	}
	readable, err := sanitizeSegment(cwd)
	if err != nil {
		return "", err
	}
	if len(readable) > maxCwdSegment {
		// Keep the tail: the leaf directories identify a checkout, the
		// leading /home/user prefix does not.
		readable = readable[len(readable)-maxCwdSegment:]
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(cwd))
	return fmt.Sprintf("%s-%016x", readable, h.Sum64()), nil
}

// secureRoot tightens the queue root to 0700. Addresses name live sessions and
// their working directories, which is more than the item bodies already stored
// here warrant leaving world-readable.
func (q *Queue) secureRoot() error {
	if err := os.Chmod(q.root, 0o700); err != nil {
		return fmt.Errorf("secure queue root: %w", err)
	}
	return nil
}

// PutAddress records where a session can be reached and indexes it by cwd.
// It overwrites any previous address for the same target.
func (q *Queue) PutAddress(a Address) error {
	t := a.Target()
	base, err := q.ensureDirs(t)
	if err != nil {
		return err
	}
	if err := q.secureRoot(); err != nil {
		return err
	}
	if a.UpdatedAt.IsZero() {
		a.UpdatedAt = q.now().UTC().Truncate(time.Second)
	} else {
		a.UpdatedAt = a.UpdatedAt.UTC().Truncate(time.Second)
	}

	body, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("encode address: %w", err)
	}
	// Write to tmp/ then rename, so a reader never observes a partial file.
	tmp := filepath.Join(base, tmpDirName, addrFileName+".tmp")
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("write address: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(base, addrFileName)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publish address: %w", err)
	}

	if a.Cwd != "" {
		dir, err := q.cwdIndexDir(t.Agent, a.Cwd)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create cwd index: %w", err)
		}
		name, err := sanitizeSegment(t.Name)
		if err != nil {
			return err
		}
		// An empty marker file: the name is the whole payload.
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			return fmt.Errorf("write cwd index: %w", err)
		}
	}
	return nil
}

// cwdIndexDir returns the directory listing the sessions of one agent that run
// in cwd.
func (q *Queue) cwdIndexDir(agent, cwd string) (string, error) {
	a, err := sanitizeSegment(agent)
	if err != nil {
		return "", fmt.Errorf("target agent: %w", err)
	}
	seg, err := cwdSegment(cwd)
	if err != nil {
		return "", err
	}
	return filepath.Join(q.root, a, byCwdDirName, seg), nil
}

// Address returns the recorded address for t, or ErrNotFound when the session
// never registered one.
func (q *Queue) Address(t Target) (*Address, error) {
	base, err := q.targetDir(t)
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(filepath.Join(base, addrFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read address: %w", err)
	}
	var a Address
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, fmt.Errorf("decode address for %s: %w", t, err)
	}
	return &a, nil
}

// RemoveAddress deletes the recorded address for t and prunes its cwd index
// entry. A target with no address is not an error.
func (q *Queue) RemoveAddress(t Target) error {
	base, err := q.targetDir(t)
	if err != nil {
		return err
	}
	// Read the address first: it is the only record of which cwd to prune.
	addr, err := q.Address(t)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err := os.Remove(filepath.Join(base, addrFileName)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove address: %w", err)
	}
	if addr == nil || addr.Cwd == "" {
		return nil
	}
	dir, err := q.cwdIndexDir(t.Agent, addr.Cwd)
	if err != nil {
		return err
	}
	name, err := sanitizeSegment(t.Name)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove cwd index: %w", err)
	}
	// Drop the directory once its last session is gone; a non-empty
	// directory fails here, which is the intended no-op.
	_ = os.Remove(dir)
	return nil
}

// TargetsByCwd lists the sessions of one agent registered as running in cwd,
// sorted by name. An unindexed directory yields no targets and no error.
func (q *Queue) TargetsByCwd(agent, cwd string) ([]Target, error) {
	dir, err := q.cwdIndexDir(agent, cwd)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read cwd index: %w", err)
	}
	targets := make([]Target, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		targets = append(targets, Target{Agent: agent, Name: e.Name()})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	return targets, nil
}

// Mailbox summarizes one target directory: how much it holds and whether a
// live session has claimed it.
type Mailbox struct {
	Target  Target   `json:"target"`
	Pending int      `json:"pending"`
	Claimed int      `json:"claimed"`
	Done    int      `json:"done"`
	Address *Address `json:"address,omitempty"`
}

// Mailboxes walks the queue root and summarizes every target directory. An
// agent name restricts the walk to that agent; an empty agent covers all of
// them. The result is sorted by target string.
func (q *Queue) Mailboxes(agent string) ([]Mailbox, error) {
	agents, err := q.agentDirs(agent)
	if err != nil {
		return nil, err
	}
	var out []Mailbox
	for _, a := range agents {
		entries, err := os.ReadDir(filepath.Join(q.root, a))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read agent directory: %w", err)
		}
		for _, e := range entries {
			if !e.IsDir() || e.Name() == byCwdDirName {
				continue
			}
			t := Target{Agent: a, Name: e.Name()}
			mb := Mailbox{Target: t}
			for _, st := range []State{StatePending, StateClaimed, StateDone} {
				ids, err := q.itemIDs(t, st)
				if err != nil {
					return nil, err
				}
				switch st {
				case StatePending:
					mb.Pending = len(ids)
				case StateClaimed:
					mb.Claimed = len(ids)
				case StateDone:
					mb.Done = len(ids)
				}
			}
			if addr, err := q.Address(t); err == nil {
				mb.Address = addr
			}
			out = append(out, mb)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target.String() < out[j].Target.String() })
	return out, nil
}

// agentDirs returns the agent directory names to walk.
func (q *Queue) agentDirs(agent string) ([]string, error) {
	if strings.TrimSpace(agent) != "" {
		a, err := sanitizeSegment(agent)
		if err != nil {
			return nil, fmt.Errorf("target agent: %w", err)
		}
		return []string{a}, nil
	}
	entries, err := os.ReadDir(q.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read queue root: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
