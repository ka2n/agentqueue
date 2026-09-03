package agentqueue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func mustOpen(t *testing.T, opts ...Option) (*Queue, string) {
	t.Helper()
	root := t.TempDir()
	q, err := Open(root, opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return q, root
}

func TestPushThenListRoundTrips(t *testing.T) {
	q, root := mustOpen(t)
	target := Target{Agent: "codex", Name: "sess-1"}
	meta := map[string]string{"from": "tester", "kind": "note"}

	pushed, err := q.Push(target, "hello there", meta)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if pushed.ID == "" {
		t.Fatal("Push returned an empty item id")
	}
	if pushed.CreatedAt.IsZero() {
		t.Fatal("Push returned a zero CreatedAt")
	}

	got, err := q.List(target, StatePending)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []Item{*pushed}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("List(pending) mismatch (-want +got):\n%s", diff)
	}

	file := filepath.Join(root, "codex", "sess-1", "pending", pushed.ID+".json")
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("expected pending file at %s: %v", file, err)
	}

	entries, err := os.ReadDir(filepath.Join(root, "codex", "sess-1", "tmp"))
	if err != nil {
		t.Fatalf("read tmp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("tmp dir is not empty: %v", entries)
	}
}

func TestPushRejectsInvalidTarget(t *testing.T) {
	q, _ := mustOpen(t)
	for _, target := range []Target{{Agent: "", Name: "x"}, {Agent: "codex", Name: ""}, {Agent: "..", Name: "x"}, {Agent: "codex", Name: "."}} {
		if _, err := q.Push(target, "x", nil); !errors.Is(err, ErrInvalidTarget) {
			t.Fatalf("Push(%+v) error = %v, want ErrInvalidTarget", target, err)
		}
	}
}

func TestPathSanitizationStaysInsideRoot(t *testing.T) {
	q, root := mustOpen(t)
	target := Target{Agent: "co dex/../x", Name: "../../escape me"}

	item, err := q.Push(target, "payload", nil)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	dir, err := q.dir(target, StatePending)
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs root: %v", err)
	}
	file := filepath.Join(dir, item.ID+".json")
	rel, err := filepath.Rel(absRoot, file)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		t.Fatalf("item file %s escaped root %s (rel %s)", file, absRoot, rel)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("expected file at %s: %v", file, err)
	}
	// The whole target must collapse to exactly two path segments.
	if got := strings.Count(rel, string(filepath.Separator)); got != 3 {
		t.Fatalf("relative path %q has %d separators, want 3 (agent/name/pending/file)", rel, got)
	}
}

func TestTakeNextClaimsOldestFirst(t *testing.T) {
	q, root := mustOpen(t)
	target := Target{Agent: "claude", Name: "s"}

	first, err := q.Push(target, "first", nil)
	if err != nil {
		t.Fatalf("Push first: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	second, err := q.Push(target, "second", nil)
	if err != nil {
		t.Fatalf("Push second: %v", err)
	}

	got, err := q.TakeNext(target)
	if err != nil {
		t.Fatalf("TakeNext: %v", err)
	}
	if got.ID != first.ID {
		t.Fatalf("TakeNext claimed %q (%q), want oldest %q", got.ID, got.Text, first.ID)
	}
	if _, err := os.Stat(filepath.Join(root, "claude", "s", "claimed", first.ID+".json")); err != nil {
		t.Fatalf("claimed file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "claude", "s", "pending", first.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("pending file still present, stat err = %v", err)
	}

	got, err = q.TakeNext(target)
	if err != nil {
		t.Fatalf("TakeNext second: %v", err)
	}
	if got.ID != second.ID {
		t.Fatalf("TakeNext claimed %q, want %q", got.ID, second.ID)
	}

	if _, err := q.TakeNext(target); !errors.Is(err, ErrEmpty) {
		t.Fatalf("TakeNext on drained queue error = %v, want ErrEmpty", err)
	}
}

func TestTakeByID(t *testing.T) {
	q, _ := mustOpen(t)
	target := Target{Agent: "codex", Name: "s"}
	a, err := q.Push(target, "a", nil)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	got, err := q.Take(target, a.ID)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if diff := cmp.Diff(*a, *got); diff != "" {
		t.Fatalf("Take mismatch (-want +got):\n%s", diff)
	}
	if _, err := q.Take(target, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Take error = %v, want ErrNotFound", err)
	}
	if _, err := q.Take(target, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Take(unknown) error = %v, want ErrNotFound", err)
	}
}

func TestTakeNextConcurrentClaimsEachItemOnce(t *testing.T) {
	q, _ := mustOpen(t)
	target := Target{Agent: "codex", Name: "race"}

	const n = 20
	pushed := make(map[string]struct{}, n)
	for range n {
		item, err := q.Push(target, "x", nil)
		if err != nil {
			t.Fatalf("Push: %v", err)
		}
		pushed[item.ID] = struct{}{}
	}

	var (
		mu      sync.Mutex
		claimed []string
		wg      sync.WaitGroup
	)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				item, err := q.TakeNext(target)
				if errors.Is(err, ErrEmpty) {
					return
				}
				if err != nil {
					t.Errorf("TakeNext: %v", err)
					return
				}
				mu.Lock()
				claimed = append(claimed, item.ID)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(claimed) != n {
		t.Fatalf("claimed %d items, want %d", len(claimed), n)
	}
	seen := make(map[string]struct{}, n)
	for _, id := range claimed {
		if _, dup := seen[id]; dup {
			t.Fatalf("item %q claimed more than once", id)
		}
		seen[id] = struct{}{}
		if _, ok := pushed[id]; !ok {
			t.Fatalf("claimed unknown item %q", id)
		}
	}
	if diff := cmp.Diff(len(pushed), len(seen)); diff != "" {
		t.Fatalf("claim count mismatch (-want +got):\n%s", diff)
	}
}

func TestAck(t *testing.T) {
	q, root := mustOpen(t)
	target := Target{Agent: "codex", Name: "s"}

	t.Run("claimed to done", func(t *testing.T) {
		item, err := q.Push(target, "a", nil)
		if err != nil {
			t.Fatalf("Push: %v", err)
		}
		if _, err := q.Take(target, item.ID); err != nil {
			t.Fatalf("Take: %v", err)
		}
		if err := q.Ack(target, item.ID); err != nil {
			t.Fatalf("Ack: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "codex", "s", "done", item.ID+".json")); err != nil {
			t.Fatalf("done file missing: %v", err)
		}
	})

	t.Run("pending to done", func(t *testing.T) {
		item, err := q.Push(target, "b", nil)
		if err != nil {
			t.Fatalf("Push: %v", err)
		}
		if err := q.Ack(target, item.ID); err != nil {
			t.Fatalf("Ack pending: %v", err)
		}
		done, err := q.List(target, StateDone)
		if err != nil {
			t.Fatalf("List(done): %v", err)
		}
		var found bool
		for _, d := range done {
			if d.ID == item.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("item %q not in done state: %+v", item.ID, done)
		}
	})

	t.Run("unknown id", func(t *testing.T) {
		if err := q.Ack(target, "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Ack(unknown) error = %v, want ErrNotFound", err)
		}
	})
}

func TestListSkipsCorruptFiles(t *testing.T) {
	q, root := mustOpen(t)
	target := Target{Agent: "codex", Name: "s"}
	good, err := q.Push(target, "good", nil)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	bad := filepath.Join(root, "codex", "s", "pending", "0000000000000-deadbeefcafe.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}

	got, err := q.List(target, StatePending)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != good.ID {
		t.Fatalf("List = %+v, want only %q", got, good.ID)
	}
}

func TestListEmptyTargetIsNotAnError(t *testing.T) {
	q, _ := mustOpen(t)
	got, err := q.List(Target{Agent: "codex", Name: "never-used"}, StatePending)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List = %+v, want empty", got)
	}
}

func TestWaitReturnsImmediatelyWhenPendingExists(t *testing.T) {
	q, _ := mustOpen(t)
	target := Target{Agent: "claude", Name: "s"}
	item, err := q.Push(target, "already here", nil)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	start := time.Now()
	got, err := q.Wait(context.Background(), target, time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("Wait took %s, want immediate return", elapsed)
	}
	if len(got) != 1 || got[0].ID != item.ID {
		t.Fatalf("Wait = %+v, want %q", got, item.ID)
	}

	// Wait must not claim: the item stays pending.
	pending, err := q.List(target, StatePending)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %+v, want the item left in place", pending)
	}
}

func TestWaitBlocksUntilPush(t *testing.T) {
	q, _ := mustOpen(t, WithPollInterval(5*time.Millisecond))
	target := Target{Agent: "claude", Name: "s"}

	go func() {
		time.Sleep(50 * time.Millisecond)
		if _, err := q.Push(target, "late arrival", nil); err != nil {
			t.Errorf("Push: %v", err)
		}
	}()

	start := time.Now()
	got, err := q.Wait(context.Background(), target, time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 40*time.Millisecond {
		t.Fatalf("Wait returned after %s, want it to block for the push", elapsed)
	}
	if len(got) != 1 || got[0].Text != "late arrival" {
		t.Fatalf("Wait = %+v, want the late arrival", got)
	}
}

func TestWaitTimeout(t *testing.T) {
	q, _ := mustOpen(t, WithPollInterval(5*time.Millisecond))
	target := Target{Agent: "claude", Name: "s"}

	_, err := q.Wait(context.Background(), target, 40*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Wait error = %v, want ErrTimeout", err)
	}
}

func TestWaitContextCancel(t *testing.T) {
	q, _ := mustOpen(t, WithPollInterval(5*time.Millisecond))
	target := Target{Agent: "claude", Name: "s"}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	_, err := q.Wait(ctx, target, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want context.Canceled", err)
	}
}

func TestFetchCmdDefault(t *testing.T) {
	q, _ := mustOpen(t)
	got := q.FetchCmd(Target{Agent: "codex", Name: "thread-1"})
	want := "agentqueue take --to codex:thread-1 --next"
	if got != want {
		t.Fatalf("FetchCmd() = %q, want %q", got, want)
	}
}

func TestWithFetchCmdOverridesDefault(t *testing.T) {
	q, _ := mustOpen(t, WithFetchCmd(func(target Target) string {
		return "jill queue take --to " + target.String() + " --next"
	}))
	got := q.FetchCmd(Target{Agent: "claude", Name: "reviewer"})
	want := "jill queue take --to claude:reviewer --next"
	if got != want {
		t.Fatalf("FetchCmd() = %q, want %q", got, want)
	}
}

func TestWithFetchCmdIgnoresNil(t *testing.T) {
	q, _ := mustOpen(t, WithFetchCmd(nil))
	want := "agentqueue take --to codex:s --next"
	if got := q.FetchCmd(Target{Agent: "codex", Name: "s"}); got != want {
		t.Fatalf("FetchCmd() = %q, want the default %q", got, want)
	}
}

func TestWithClockStampsCreatedAt(t *testing.T) {
	fixed := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	q, _ := mustOpen(t, WithClock(func() time.Time { return fixed }))
	item, err := q.Push(Target{Agent: "codex", Name: "s"}, "x", nil)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !item.CreatedAt.Equal(fixed) {
		t.Fatalf("CreatedAt = %s, want %s", item.CreatedAt, fixed)
	}
}

func TestWithPollIntervalZeroKeepsDefault(t *testing.T) {
	q, _ := mustOpen(t, WithPollInterval(0))
	if got := q.poll(); got != defaultPollInterval {
		t.Fatalf("poll() = %s, want %s", got, defaultPollInterval)
	}
}
