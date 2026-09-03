package main

import (
	"fmt"
	"strings"
)

// A line diff, small enough to keep in this file and with no dependencies.
// It exists so `install` can show what it is about to do to a settings file
// instead of only the block it adds - a block, read on its own, looks like a
// replacement of everything around it.

// diffKind is what happened to one line.
type diffKind byte

const (
	diffKeep   diffKind = ' '
	diffDelete diffKind = '-'
	diffInsert diffKind = '+'
)

// diffOp is one line of the diff, with the 1-based line numbers it sits at in
// the two inputs. aLine is 0 for an insertion and bLine is 0 for a deletion.
type diffOp struct {
	Kind  diffKind
	Text  string
	aLine int
	bLine int
}

// diffLines produces the shortest edit script between a and b, by the usual
// LCS dynamic program. Settings files are hundreds of lines at most, so the
// O(len(a)*len(b)) table is not worth avoiding.
func diffLines(a, b []string) []diffOp {
	// lcs[i][j] is the length of the longest common subsequence of a[i:] and
	// b[j:], so the walk below can pick the move that keeps the most lines.
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
				continue
			}
			if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var ops []diffOp
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{Kind: diffKeep, Text: a[i], aLine: i + 1, bLine: j + 1})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{Kind: diffDelete, Text: a[i], aLine: i + 1})
			i++
		default:
			ops = append(ops, diffOp{Kind: diffInsert, Text: b[j], bLine: j + 1})
			j++
		}
	}
	for ; i < len(a); i++ {
		ops = append(ops, diffOp{Kind: diffDelete, Text: a[i], aLine: i + 1})
	}
	for ; j < len(b); j++ {
		ops = append(ops, diffOp{Kind: diffInsert, Text: b[j], bLine: j + 1})
	}
	return ops
}

// diffStat counts the changed lines, which is what the one-line summary above
// a diff reports.
func diffStat(ops []diffOp) (added, deleted int) {
	for _, op := range ops {
		switch op.Kind {
		case diffInsert:
			added++
		case diffDelete:
			deleted++
		}
	}
	return added, deleted
}

// unifiedDiff renders a unified diff with ctx lines of context and the usual
// ---/+++/@@ headers. Identical inputs produce the empty string, so a caller
// can use the result to decide whether there is anything to show.
func unifiedDiff(a, b []string, aLabel, bLabel string, ctx int) string {
	if ctx < 0 {
		ctx = 0
	}
	ops := diffLines(a, b)
	if added, deleted := diffStat(ops); added == 0 && deleted == 0 {
		return ""
	}

	// An op is printed when it is a change or sits within ctx ops of one.
	show := make([]bool, len(ops))
	for i, op := range ops {
		if op.Kind == diffKeep {
			continue
		}
		lo, hi := max(0, i-ctx), min(len(ops)-1, i+ctx)
		for k := lo; k <= hi; k++ {
			show[k] = true
		}
	}

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n", aLabel)
	fmt.Fprintf(&out, "+++ %s\n", bLabel)
	for i := 0; i < len(ops); {
		if !show[i] {
			i++
			continue
		}
		// One hunk: the run of consecutive printed ops starting here.
		end := i
		for end < len(ops) && show[end] {
			end++
		}
		hunk := ops[i:end]

		aStart, aCount, bStart, bCount := 0, 0, 0, 0
		for _, op := range hunk {
			if op.Kind != diffInsert {
				if aCount == 0 {
					aStart = op.aLine
				}
				aCount++
			}
			if op.Kind != diffDelete {
				if bCount == 0 {
					bStart = op.bLine
				}
				bCount++
			}
		}
		// An empty side has no line to point at, so unified diff points at
		// the line it would come after, which is 0 at the top of a file.
		if aCount == 0 {
			aStart = countBefore(ops[:i], diffInsert)
		}
		if bCount == 0 {
			bStart = countBefore(ops[:i], diffDelete)
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", aStart, aCount, bStart, bCount)
		for _, op := range hunk {
			fmt.Fprintf(&out, "%c%s\n", byte(op.Kind), op.Text)
		}
		i = end
	}
	return out.String()
}

// countBefore counts the ops before a hunk that consume the side whose own
// counter is empty, i.e. every op except the given kind.
func countBefore(ops []diffOp, skip diffKind) int {
	n := 0
	for _, op := range ops {
		if op.Kind != skip {
			n++
		}
	}
	return n
}

// splitLines splits a text into lines for diffing, without a trailing empty
// line for the final newline.
func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
