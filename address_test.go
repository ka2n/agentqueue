package agentqueue

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestPutAddressRoundTrips(t *testing.T) {
	q, _ := mustOpen(t, WithClock(func() time.Time { return time.Unix(1756800000, 0) }))
	addr := Address{
		Agent:        "claude",
		Name:         "sess-1",
		Cwd:          "/home/u/src/proj",
		PID:          4242,
		Socket:       "/run/user/1000/claude/sess-1.sock",
		AgentVersion: "2.1.0",
	}
	if err := q.PutAddress(addr); err != nil {
		t.Fatalf("PutAddress: %v", err)
	}

	got, err := q.Address(Target{Agent: "claude", Name: "sess-1"})
	if err != nil {
		t.Fatalf("Address: %v", err)
	}
	want := addr
	want.UpdatedAt = time.Unix(1756800000, 0).UTC()
	if diff := cmp.Diff(&want, got); diff != "" {
		t.Fatalf("Address mismatch (-want +got):\n%s", diff)
	}
}

func TestPutAddressPermissions(t *testing.T) {
	q, root := mustOpen(t)
	if err := q.PutAddress(Address{Agent: "claude", Name: "s", Cwd: "/tmp/x"}); err != nil {
		t.Fatalf("PutAddress: %v", err)
	}

	info, err := os.Stat(filepath.Join(root, "claude", "s", addrFileName))
	if err != nil {
		t.Fatalf("stat addr.json: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("addr.json mode = %04o, want 0600", got)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	if got := rootInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("queue root mode = %04o, want 0700", got)
	}
}

func TestAddressMissingIsNotFound(t *testing.T) {
	q, _ := mustOpen(t)
	if _, err := q.Address(Target{Agent: "claude", Name: "never"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Address for unregistered target = %v, want ErrNotFound", err)
	}
}

func TestTargetsByCwdIndexesAndPrunes(t *testing.T) {
	q, _ := mustOpen(t)
	const cwd = "/home/u/src/proj"
	for _, name := range []string{"sess-b", "sess-a"} {
		if err := q.PutAddress(Address{Agent: "claude", Name: name, Cwd: cwd}); err != nil {
			t.Fatalf("PutAddress(%s): %v", name, err)
		}
	}
	// A session in a different directory must not show up.
	if err := q.PutAddress(Address{Agent: "claude", Name: "elsewhere", Cwd: "/home/u/other"}); err != nil {
		t.Fatalf("PutAddress(elsewhere): %v", err)
	}

	got, err := q.TargetsByCwd("claude", cwd)
	if err != nil {
		t.Fatalf("TargetsByCwd: %v", err)
	}
	want := []Target{{Agent: "claude", Name: "sess-a"}, {Agent: "claude", Name: "sess-b"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("TargetsByCwd mismatch (-want +got):\n%s", diff)
	}

	if err := q.RemoveAddress(Target{Agent: "claude", Name: "sess-a"}); err != nil {
		t.Fatalf("RemoveAddress: %v", err)
	}
	got, err = q.TargetsByCwd("claude", cwd)
	if err != nil {
		t.Fatalf("TargetsByCwd after remove: %v", err)
	}
	if diff := cmp.Diff([]Target{{Agent: "claude", Name: "sess-b"}}, got); diff != "" {
		t.Fatalf("TargetsByCwd mismatch (-want +got):\n%s", diff)
	}
	if _, err := q.Address(Target{Agent: "claude", Name: "sess-a"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Address after RemoveAddress = %v, want ErrNotFound", err)
	}
}

func TestTargetsByCwdUnknownDirectory(t *testing.T) {
	q, _ := mustOpen(t)
	got, err := q.TargetsByCwd("claude", "/nowhere")
	if err != nil {
		t.Fatalf("TargetsByCwd: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("TargetsByCwd = %v, want none", got)
	}
}

func TestRemoveAddressWithoutAddressIsNoOp(t *testing.T) {
	q, _ := mustOpen(t)
	if err := q.RemoveAddress(Target{Agent: "claude", Name: "ghost"}); err != nil {
		t.Fatalf("RemoveAddress: %v", err)
	}
}

// TestCwdSegmentDistinguishesSanitizedCollisions guards the reason the segment
// carries a hash: sanitizing alone maps distinct paths onto the same bytes.
func TestCwdSegmentDistinguishesSanitizedCollisions(t *testing.T) {
	a, err := cwdSegment("/home/u/a-b")
	if err != nil {
		t.Fatalf("cwdSegment: %v", err)
	}
	b, err := cwdSegment("/home/u/a_b")
	if err != nil {
		t.Fatalf("cwdSegment: %v", err)
	}
	if a == b {
		t.Fatalf("cwdSegment collided on /home/u/a-b and /home/u/a_b: %q", a)
	}
	if strings.ContainsAny(a, "/") {
		t.Fatalf("cwdSegment(%q) is not a single path segment", a)
	}
	long, err := cwdSegment("/" + strings.Repeat("deep/", 60))
	if err != nil {
		t.Fatalf("cwdSegment(long): %v", err)
	}
	if len(long) > maxCwdSegment+17 {
		t.Fatalf("cwdSegment(long) is %d bytes, want at most %d", len(long), maxCwdSegment+17)
	}
	if _, err := cwdSegment("  "); err == nil {
		t.Fatal("cwdSegment(blank) = nil error, want one")
	}
}

func TestMailboxesCountsStatesAndSkipsIndex(t *testing.T) {
	q, _ := mustOpen(t)
	target := Target{Agent: "claude", Name: "sess-1"}
	for range 3 {
		if _, err := q.Push(target, "hi", nil); err != nil {
			t.Fatalf("Push: %v", err)
		}
	}
	claimed, err := q.TakeNext(target)
	if err != nil {
		t.Fatalf("TakeNext: %v", err)
	}
	if err := q.Ack(target, claimed.ID); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if _, err := q.TakeNext(target); err != nil {
		t.Fatalf("TakeNext: %v", err)
	}
	if err := q.PutAddress(Address{Agent: "claude", Name: "sess-1", Cwd: "/w"}); err != nil {
		t.Fatalf("PutAddress: %v", err)
	}
	if _, err := q.Push(Target{Agent: "codex", Name: "thread"}, "hi", nil); err != nil {
		t.Fatalf("Push codex: %v", err)
	}

	boxes, err := q.Mailboxes("")
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	var names []string
	for _, mb := range boxes {
		names = append(names, mb.Target.String())
	}
	// _by_cwd is an index directory, not a mailbox.
	if diff := cmp.Diff([]string{"claude:sess-1", "codex:thread"}, names); diff != "" {
		t.Fatalf("mailbox targets mismatch (-want +got):\n%s", diff)
	}
	got := boxes[0]
	if got.Pending != 1 || got.Claimed != 1 || got.Done != 1 {
		t.Fatalf("counts = pending %d, claimed %d, done %d; want 1/1/1", got.Pending, got.Claimed, got.Done)
	}
	if got.Address == nil || got.Address.Cwd != "/w" {
		t.Fatalf("mailbox address = %+v, want the registered cwd", got.Address)
	}
	if boxes[1].Address != nil {
		t.Fatalf("codex mailbox has an address it never registered: %+v", boxes[1].Address)
	}

	only, err := q.Mailboxes("codex")
	if err != nil {
		t.Fatalf("Mailboxes(codex): %v", err)
	}
	if len(only) != 1 || only[0].Target.Agent != "codex" {
		t.Fatalf("Mailboxes(codex) = %+v, want the one codex mailbox", only)
	}
}
