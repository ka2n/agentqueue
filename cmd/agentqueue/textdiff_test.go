package main

import (
	"strings"
	"testing"
)

func TestUnifiedDiffIdenticalIsEmpty(t *testing.T) {
	lines := []string{"a", "b", "c"}
	if got := unifiedDiff(lines, lines, "x", "y", 3); got != "" {
		t.Fatalf("unifiedDiff of identical inputs = %q, want empty", got)
	}
	if got := unifiedDiff(nil, nil, "x", "y", 3); got != "" {
		t.Fatalf("unifiedDiff of two empty inputs = %q, want empty", got)
	}
}

func TestUnifiedDiffPureInsertion(t *testing.T) {
	before := []string{"a", "b", "c"}
	after := []string{"a", "b", "new1", "new2", "c"}
	got := unifiedDiff(before, after, "old", "new", 3)
	want := `--- old
+++ new
@@ -1,3 +1,5 @@
 a
 b
+new1
+new2
 c
`
	if got != want {
		t.Fatalf("unifiedDiff mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
	if added, deleted := diffStat(diffLines(before, after)); added != 2 || deleted != 0 {
		t.Fatalf("diffStat = +%d -%d, want +2 -0", added, deleted)
	}
}

func TestUnifiedDiffPureDeletion(t *testing.T) {
	before := []string{"a", "b", "c", "d"}
	after := []string{"a", "d"}
	got := unifiedDiff(before, after, "old", "new", 3)
	want := `--- old
+++ new
@@ -1,4 +1,2 @@
 a
-b
-c
 d
`
	if got != want {
		t.Fatalf("unifiedDiff mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedDiffChangeInTheMiddleKeepsContext(t *testing.T) {
	before := strings.Split("1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11", "\n")
	after := strings.Split("1\n2\n3\n4\n5\nSIX\n7\n8\n9\n10\n11", "\n")
	got := unifiedDiff(before, after, "old", "new", 3)
	want := `--- old
+++ new
@@ -3,7 +3,7 @@
 3
 4
 5
-6
+SIX
 7
 8
 9
`
	if got != want {
		t.Fatalf("unifiedDiff mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
	// Distant lines are outside the context window and must not appear.
	for _, line := range []string{" 1\n", " 11\n"} {
		if strings.Contains(got, line) {
			t.Fatalf("diff carries context beyond 3 lines (%q):\n%s", line, got)
		}
	}
}

// TestUnifiedDiffTwoHunks is the case that shows the hunk headers are computed
// rather than guessed: the second hunk's line numbers are offset by the first.
func TestUnifiedDiffTwoHunks(t *testing.T) {
	before := strings.Split("a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nm\nn", "\n")
	after := strings.Split("a\nB\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nM\nn", "\n")
	got := unifiedDiff(before, after, "old", "new", 3)
	want := `--- old
+++ new
@@ -1,5 +1,5 @@
 a
-b
+B
 c
 d
 e
@@ -10,5 +10,5 @@
 j
 k
 l
-m
+M
 n
`
	if got != want {
		t.Fatalf("unifiedDiff mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedDiffEmptySides(t *testing.T) {
	// A file that did not exist: everything is an insertion, and the old side
	// points at line 0, the way diff(1) writes it.
	got := unifiedDiff(nil, []string{"x", "y"}, "old", "new", 3)
	want := `--- old
+++ new
@@ -0,0 +1,2 @@
+x
+y
`
	if got != want {
		t.Fatalf("insert-into-empty mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}

	got = unifiedDiff([]string{"x", "y"}, nil, "old", "new", 3)
	want = `--- old
+++ new
@@ -1,2 +0,0 @@
-x
-y
`
	if got != want {
		t.Fatalf("delete-to-empty mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedDiffZeroContext(t *testing.T) {
	before := []string{"a", "b", "c"}
	after := []string{"a", "B", "c"}
	got := unifiedDiff(before, after, "old", "new", 0)
	want := `--- old
+++ new
@@ -2,1 +2,1 @@
-b
+B
`
	if got != want {
		t.Fatalf("zero-context mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestSplitLines(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{in: "", want: 0},
		{in: "\n", want: 0},
		{in: "a", want: 1},
		{in: "a\n", want: 1},
		{in: "a\nb\n", want: 2},
		{in: "a\n\nb", want: 3},
	}
	for _, tt := range tests {
		if got := len(splitLines(tt.in)); got != tt.want {
			t.Fatalf("splitLines(%q) = %d lines, want %d", tt.in, got, tt.want)
		}
	}
}
