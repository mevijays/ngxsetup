package render

import (
	"fmt"
	"strings"
)

// Diff renders a unified-style diff between two versions of a file.
//
// Configuration changes on a production web server deserve to be seen before
// they are applied, which is what `--diff` exists for. The algorithm is a
// straightforward longest-common-subsequence walk: config files are small, and
// a dependency-free implementation keeps the binary self-contained.
func Diff(name string, old, new []byte) string {
	if string(old) == string(new) {
		return ""
	}
	oldLines := splitLines(old)
	newLines := splitLines(new)

	var b strings.Builder
	fmt.Fprintf(&b, "--- %s (current)\n+++ %s (proposed)\n", name, name)

	for _, h := range hunks(oldLines, newLines, 3) {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.oldStart+1, h.oldCount, h.newStart+1, h.newCount)
		for _, l := range h.lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := strings.TrimSuffix(string(b), "\n")
	return strings.Split(s, "\n")
}

type op struct {
	kind byte // ' ', '-', '+'
	text string
}

type hunk struct {
	oldStart, oldCount int
	newStart, newCount int
	lines              []string
}

// lcs builds the standard dynamic-programming table and walks it back into an
// edit script.
func lcs(a, b []string) []op {
	n, m := len(a), len(b)
	// Guard against pathological memory use on unexpectedly large files.
	if n*m > 4_000_000 {
		ops := make([]op, 0, n+m)
		for _, l := range a {
			ops = append(ops, op{'-', l})
		}
		for _, l := range b {
			ops = append(ops, op{'+', l})
		}
		return ops
	}

	table := make([][]int, n+1)
	for i := range table {
		table[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				table[i][j] = table[i+1][j+1] + 1
			} else if table[i+1][j] >= table[i][j+1] {
				table[i][j] = table[i+1][j]
			} else {
				table[i][j] = table[i][j+1]
			}
		}
	}

	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{' ', a[i]})
			i, j = i+1, j+1
		case table[i+1][j] >= table[i][j+1]:
			ops = append(ops, op{'-', a[i]})
			i++
		default:
			ops = append(ops, op{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{'+', b[j]})
	}
	return ops
}

// hunks groups the edit script into contiguous regions with surrounding
// context, so an operator sees the change and enough around it to place it.
func hunks(a, b []string, context int) []hunk {
	ops := lcs(a, b)

	// Mark which operations are close enough to a change to be worth printing.
	keep := make([]bool, len(ops))
	for i, o := range ops {
		if o.kind == ' ' {
			continue
		}
		for j := max(0, i-context); j <= min(len(ops)-1, i+context); j++ {
			keep[j] = true
		}
	}

	var out []hunk
	oldLine, newLine := 0, 0
	i := 0
	for i < len(ops) {
		if !keep[i] {
			if ops[i].kind != '+' {
				oldLine++
			}
			if ops[i].kind != '-' {
				newLine++
			}
			i++
			continue
		}
		h := hunk{oldStart: oldLine, newStart: newLine}
		for i < len(ops) && keep[i] {
			o := ops[i]
			h.lines = append(h.lines, string(o.kind)+o.text)
			if o.kind != '+' {
				oldLine++
				h.oldCount++
			}
			if o.kind != '-' {
				newLine++
				h.newCount++
			}
			i++
		}
		out = append(out, h)
	}
	return out
}
