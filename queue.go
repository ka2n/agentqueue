package agentqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// defaultPollInterval is how often Wait rescans the pending directory.
const defaultPollInterval = 250 * time.Millisecond

// tmpDirName holds partially written items before they are renamed into place.
const tmpDirName = "tmp"

// Queue is a filesystem-backed queue rooted at a directory.
//
// A Queue holds no open handles and takes no locks: every state change is an
// atomic rename, so any number of processes may share the same root.
type Queue struct {
	root     string
	now      func() time.Time
	fetchCmd func(Target) string

	mu           sync.RWMutex
	pollInterval time.Duration
}

// Option configures a Queue at Open time.
type Option func(*Queue)

// WithFetchCmd sets how the queue renders the command an agent runs to claim
// the next item for a target. The rendered string is what an arrival notice
// carries, so a program embedding this library should point it at its own CLI.
// The default is "agentqueue take --to <target> --next".
func WithFetchCmd(fn func(Target) string) Option {
	return func(q *Queue) {
		if fn != nil {
			q.fetchCmd = fn
		}
	}
}

// WithPollInterval sets how often Wait rescans the pending directory.
// A value of zero or less keeps the default of 250ms.
func WithPollInterval(d time.Duration) Option {
	return func(q *Queue) { q.pollInterval = d }
}

// WithClock replaces the clock used to stamp item creation times. It exists for
// tests and for callers with their own time source.
func WithClock(now func() time.Time) Option {
	return func(q *Queue) {
		if now != nil {
			q.now = now
		}
	}
}

// defaultFetchCmd renders the standalone CLI's take command.
func defaultFetchCmd(t Target) string {
	return fmt.Sprintf("agentqueue take --to %s --next", t)
}

// Open prepares root as a queue directory, creating it if needed.
func Open(root string, opts ...Option) (*Queue, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("empty queue root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve queue root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("create queue root: %w", err)
	}
	q := &Queue{
		root:         abs,
		now:          time.Now,
		fetchCmd:     defaultFetchCmd,
		pollInterval: defaultPollInterval,
	}
	for _, opt := range opts {
		opt(q)
	}
	return q, nil
}

// Root returns the absolute queue root directory.
func (q *Queue) Root() string { return q.root }

// FetchCmd is the command an agent runs to claim the next item for t.
func (q *Queue) FetchCmd(t Target) string {
	if q.fetchCmd == nil {
		return defaultFetchCmd(t)
	}
	return q.fetchCmd(t)
}

func (q *Queue) poll() time.Duration {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.pollInterval <= 0 {
		return defaultPollInterval
	}
	return q.pollInterval
}

// sanitizeSegment reduces s to a single safe path segment. Every byte outside
// [A-Za-z0-9._-] becomes '_', so no input can introduce a separator or traverse
// out of the queue root.
func sanitizeSegment(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("%w: empty path segment", ErrInvalidTarget)
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "." || out == ".." {
		return "", fmt.Errorf("%w: unusable path segment %q", ErrInvalidTarget, s)
	}
	return out, nil
}

// targetDir returns the directory holding all states for t.
func (q *Queue) targetDir(t Target) (string, error) {
	agent, err := sanitizeSegment(t.Agent)
	if err != nil {
		return "", fmt.Errorf("target agent: %w", err)
	}
	name, err := sanitizeSegment(t.Name)
	if err != nil {
		return "", fmt.Errorf("target name: %w", err)
	}
	return filepath.Join(q.root, agent, name), nil
}

// dir returns the directory holding items of the given state for t.
func (q *Queue) dir(t Target, st State) (string, error) {
	base, err := q.targetDir(t)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, st.String()), nil
}

// ensureDirs creates the full directory set for a target.
func (q *Queue) ensureDirs(t Target) (string, error) {
	base, err := q.targetDir(t)
	if err != nil {
		return "", err
	}
	for _, sub := range []string{StatePending.String(), StateClaimed.String(), StateDone.String(), tmpDirName} {
		if err := os.MkdirAll(filepath.Join(base, sub), 0o755); err != nil {
			return "", fmt.Errorf("create queue directory: %w", err)
		}
	}
	return base, nil
}

// Push persists a new item in the pending state. It sends no notification;
// use PushAndNotify for that.
func (q *Queue) Push(t Target, text string, meta map[string]string) (*Item, error) {
	base, err := q.ensureDirs(t)
	if err != nil {
		return nil, err
	}

	item := &Item{
		ID:        newID(),
		Target:    t,
		Text:      text,
		CreatedAt: q.now().UTC().Truncate(time.Millisecond),
	}
	if len(meta) > 0 {
		item.Meta = make(map[string]string, len(meta))
		for k, v := range meta {
			item.Meta[k] = v
		}
	}

	body, err := json.Marshal(item)
	if err != nil {
		return nil, fmt.Errorf("encode item: %w", err)
	}

	// Write to tmp/ then rename, so a reader never observes a partial file.
	tmp := filepath.Join(base, tmpDirName, item.ID+".json.tmp")
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return nil, fmt.Errorf("write item: %w", err)
	}
	final := filepath.Join(base, StatePending.String(), item.ID+".json")
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("publish item: %w", err)
	}
	return item, nil
}

// itemIDs returns the ids present in the given state, sorted ascending.
func (q *Queue) itemIDs(t Target, st State) ([]string, error) {
	dir, err := q.dir(t, st)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// A target that has never been pushed to simply has no items.
			return nil, nil
		}
		return nil, fmt.Errorf("read queue directory: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(ids)
	return ids, nil
}

// readItem loads a single item file.
func readItem(path string) (*Item, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read item: %w", err)
	}
	var item Item
	if err := json.Unmarshal(body, &item); err != nil {
		return nil, fmt.Errorf("decode item %s: %w", filepath.Base(path), err)
	}
	return &item, nil
}

// List returns the items in the given state, oldest first. Files that cannot be
// decoded are skipped so one corrupt item does not hide the rest.
func (q *Queue) List(t Target, st State) ([]Item, error) {
	dir, err := q.dir(t, st)
	if err != nil {
		return nil, err
	}
	ids, err := q.itemIDs(t, st)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(ids))
	for _, id := range ids {
		item, err := readItem(filepath.Join(dir, id+".json"))
		if err != nil {
			continue
		}
		items = append(items, *item)
	}
	return items, nil
}

// claim renames a pending item into claimed/ and returns it. A rename failure
// means another consumer won the race, which is reported as ErrNotFound.
func (q *Queue) claim(t Target, id string) (*Item, error) {
	base, err := q.ensureDirs(t)
	if err != nil {
		return nil, err
	}
	from := filepath.Join(base, StatePending.String(), id+".json")
	to := filepath.Join(base, StateClaimed.String(), id+".json")
	if err := os.Rename(from, to); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("claim item: %w", err)
	}
	item, err := readItem(to)
	if err != nil {
		return nil, err
	}
	return item, nil
}

// TakeNext claims the oldest pending item. It returns ErrEmpty when the queue
// holds no pending item.
func (q *Queue) TakeNext(t Target) (*Item, error) {
	for {
		ids, err := q.itemIDs(t, StatePending)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, ErrEmpty
		}
		for _, id := range ids {
			item, err := q.claim(t, id)
			if errors.Is(err, ErrNotFound) {
				// Lost the race for this id; try the next one.
				continue
			}
			if err != nil {
				return nil, err
			}
			return item, nil
		}
		// Every candidate was taken by someone else. Rescan.
	}
}

// Take claims the pending item with the given id.
func (q *Queue) Take(t Target, id string) (*Item, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("%w: empty item id", ErrNotFound)
	}
	return q.claim(t, id)
}

// Ack marks an item done. It accepts a claimed id and, for convenience, a
// pending id that was read without being claimed.
func (q *Queue) Ack(t Target, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: empty item id", ErrNotFound)
	}
	base, err := q.ensureDirs(t)
	if err != nil {
		return err
	}
	to := filepath.Join(base, StateDone.String(), id+".json")
	for _, st := range []State{StateClaimed, StatePending} {
		from := filepath.Join(base, st.String(), id+".json")
		err := os.Rename(from, to)
		if err == nil {
			return nil
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("ack item: %w", err)
		}
	}
	return fmt.Errorf("%w: %s in %s", ErrNotFound, id, t)
}

// Wait blocks until at least one item is pending for t and returns those items
// without claiming them. A timeout of zero or less means wait indefinitely.
// It returns ErrTimeout on deadline and ctx.Err() when ctx ends first.
func (q *Queue) Wait(ctx context.Context, t Target, timeout time.Duration) ([]Item, error) {
	if _, err := q.targetDir(t); err != nil {
		return nil, err
	}

	var deadline <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}

	ticker := time.NewTicker(q.poll())
	defer ticker.Stop()

	for {
		items, err := q.List(t, StatePending)
		if err != nil {
			return nil, err
		}
		if len(items) > 0 {
			return items, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, ErrTimeout
		case <-ticker.C:
		}
	}
}
