// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"strings"
)

// A change records that a[A0:A1] is replaced by b[B0:B1].
// At least one of the two ranges is non-empty.
type change struct {
	A0, A1 int
	B0, B1 int
}

// diffChanges returns the changes converting a into b,
// in increasing order of position. The edit script is minimal:
// it uses the fewest possible insertions and deletions.
func diffChanges[T comparable](a, b []T) []change {
	var out []change
	diffRange(a, b, 0, len(a), 0, len(b), &out)
	return coalesce(out)
}

// coalesce merges changes that abut. Myers's algorithm reports a block of
// lines replaced by an unrelated block as a deletion immediately followed
// by an insertion; joining them lets the two blocks be shown side by side
// as a single edit rather than as an unrelated removal and addition.
func coalesce(c []change) []change {
	out := c[:0]
	for _, x := range c {
		if n := len(out); n > 0 && out[n-1].A1 == x.A0 && out[n-1].B1 == x.B0 {
			out[n-1].A1 = x.A1
			out[n-1].B1 = x.B1
			continue
		}
		out = append(out, x)
	}
	return out
}

// diffRange appends to *out the changes converting a[a0:a1] into b[b0:b1].
// It trims the common prefix and suffix and then splits the remaining
// problem in half at a point on an optimal edit path, recursing on each half.
func diffRange[T comparable](a, b []T, a0, a1, b0, b1 int, out *[]change) {
	for a0 < a1 && b0 < b1 && a[a0] == b[b0] {
		a0++
		b0++
	}
	for a0 < a1 && b0 < b1 && a[a1-1] == b[b1-1] {
		a1--
		b1--
	}
	if a0 == a1 && b0 == b1 {
		return
	}
	if a0 == a1 || b0 == b1 {
		// Pure insertion or pure deletion.
		*out = append(*out, change{a0, a1, b0, b1})
		return
	}
	x, y := bisect(a[a0:a1], b[b0:b1])
	if x <= 0 && y <= 0 || x >= a1-a0 && y >= b1-b0 {
		// No split point: replace the whole range. Should not happen,
		// but recursing on a degenerate split would not terminate.
		*out = append(*out, change{a0, a1, b0, b1})
		return
	}
	diffRange(a, b, a0, a0+x, b0, b0+y, out)
	diffRange(a, b, a0+x, a1, b0+y, b1, out)
}

// bisect finds a point (x, y) lying on an optimal edit path from
// (0, 0) to (len(a), len(b)), dividing the problem roughly in half.
// It runs Myers's algorithm forward from the start and backward from
// the end until the two searches meet. It reports (-1, -1) if a and b
// have nothing in common.
func bisect[T comparable](a, b []T) (int, int) {
	n, m := len(a), len(b)
	maxD := (n + m + 1) / 2
	off := maxD + 1
	size := 2*maxD + 3

	// v1[off+k] is how far the forward search has reached on diagonal k,
	// v2[off+k] the same for the backward search. -1 means unvisited.
	v1 := make([]int, size)
	v2 := make([]int, size)
	for i := range v1 {
		v1[i] = -1
		v2[i] = -1
	}
	v1[off+1] = 0
	v2[off+1] = 0

	delta := n - m
	// When delta is odd the two searches can only overlap during the
	// forward pass; when it is even, only during the backward pass.
	front := delta%2 != 0

	// The k ranges shrink as diagonals run off the edge of the grid.
	k1start, k1end := 0, 0
	k2start, k2end := 0, 0

	for d := 0; d <= maxD; d++ {
		for k1 := -d + k1start; k1 <= d-k1end; k1 += 2 {
			i := off + k1
			var x1 int
			if k1 == -d || (k1 != d && v1[i-1] < v1[i+1]) {
				x1 = v1[i+1]
			} else {
				x1 = v1[i-1] + 1
			}
			y1 := x1 - k1
			for x1 < n && y1 < m && a[x1] == b[y1] {
				x1++
				y1++
			}
			v1[i] = x1
			switch {
			case x1 > n:
				k1end += 2
			case y1 > m:
				k1start += 2
			case front:
				if j := off + delta - k1; 0 <= j && j < size && v2[j] != -1 {
					if x1 >= n-v2[j] {
						return x1, y1
					}
				}
			}
		}
		for k2 := -d + k2start; k2 <= d-k2end; k2 += 2 {
			i := off + k2
			var x2 int
			if k2 == -d || (k2 != d && v2[i-1] < v2[i+1]) {
				x2 = v2[i+1]
			} else {
				x2 = v2[i-1] + 1
			}
			y2 := x2 - k2
			for x2 < n && y2 < m && a[n-x2-1] == b[m-y2-1] {
				x2++
				y2++
			}
			v2[i] = x2
			switch {
			case x2 > n:
				k2end += 2
			case y2 > m:
				k2start += 2
			case !front:
				if j := off + delta - k2; 0 <= j && j < size && v1[j] != -1 {
					x1 := v1[j]
					y1 := x1 - (j - off)
					if x1 >= n-x2 {
						return x1, y1
					}
				}
			}
		}
	}
	return -1, -1
}

// A Span is a byte range within a line, used to highlight
// the parts of a line that changed.
type Span struct {
	Lo, Hi int
}

// A Line is one side of a diff row.
type Line struct {
	Num   int    // 1-based line number, or 0 if this side is blank
	Text  string // line content, without the trailing newline
	Spans []Span // changed byte ranges within Text
}

// A RowKind describes what a diff row shows.
type RowKind int

const (
	RowEqual   RowKind = iota // unchanged line, shown on both sides
	RowReplace                // changed line, shown on both sides
	RowDelete                 // line removed, left side only
	RowInsert                 // line added, right side only
	RowSkip                   // placeholder for a run of hidden unchanged lines
)

// A Row is one row of a side-by-side diff.
type Row struct {
	Kind RowKind
	L, R Line

	// Total reports whether this row belongs to a chunk that only
	// adds lines or only removes them, with nothing on the other side.
	// Gerrit paints such rows with the strong highlight color rather
	// than the pale one.
	Total bool

	// NoIntraline reports that there is no part of this line to single
	// out as changed, so the whole line is painted with the strong color:
	// either the two sides differ too much for highlighting a part of
	// them to help, or the line has no counterpart at all.
	NoIntraline bool

	// Rebased reports that this row's edit was inherited rather than made
	// by the change being viewed: the same edit appears in the diff of the
	// two sides' parent commits. Gerrit calls such a region "due to rebase"
	// and paints it in muted colors. See MarkRebased.
	Rebased bool

	// For RowSkip: how many unchanged lines are hidden, and the
	// 1-based line numbers they start at on each side.
	Count        int
	LFrom, RFrom int
}

// A FileDiff is the diff of a single file.
type FileDiff struct {
	Rows   []Row
	Binary bool
	// LNoNewline and RNoNewline report that the corresponding side
	// did not end with a newline.
	LNoNewline, RNoNewline bool
}

// maxIntralineChange is the fraction of a line that may change before
// intraline highlighting is abandoned and the whole line is highlighted.
// When a chunk pairs two lines that have nothing to do with each other,
// a character diff finds a scattering of shared letters and highlights
// the gaps between them; marking the whole line changed reads better.
const maxIntralineChange = 0.5

// mergeSpanGap is the number of unchanged runes that may separate two
// changed ranges before they are shown as separate highlights. Without
// this, character-level diffs produce a confetti of tiny highlights.
const mergeSpanGap = 4

// isBinary reports whether data looks like a binary file.
// Like git, it looks for a NUL byte near the start.
func isBinary(data []byte) bool {
	if len(data) > 8000 {
		data = data[:8000]
	}
	return bytes.IndexByte(data, 0) >= 0
}

// splitLines splits data into lines, dropping the trailing newline of each.
// It also reports whether data was non-empty and lacked a final newline.
func splitLines(data []byte) (lines []string, noNewline bool) {
	if len(data) == 0 {
		return nil, false
	}
	s := string(data)
	if strings.HasSuffix(s, "\n") {
		s = s[:len(s)-1]
	} else {
		noNewline = true
	}
	return strings.Split(s, "\n"), noNewline
}

// Diff computes the side-by-side diff of old and new.
func Diff(old, new []byte) *FileDiff {
	if isBinary(old) || isBinary(new) {
		return &FileDiff{Binary: true}
	}
	a, aNo := splitLines(old)
	b, bNo := splitLines(new)
	d := &FileDiff{LNoNewline: aNo, RNoNewline: bNo}

	ai, bi := 0, 0
	for _, c := range diffChanges(a, b) {
		for ; ai < c.A0; ai, bi = ai+1, bi+1 {
			d.Rows = append(d.Rows, Row{
				Kind: RowEqual,
				L:    Line{Num: ai + 1, Text: a[ai]},
				R:    Line{Num: bi + 1, Text: b[bi]},
			})
		}
		d.Rows = append(d.Rows, chunkRows(a[c.A0:c.A1], b[c.B0:c.B1], c.A0, c.B0)...)
		ai, bi = c.A1, c.B1
	}
	for ; ai < len(a); ai, bi = ai+1, bi+1 {
		d.Rows = append(d.Rows, Row{
			Kind: RowEqual,
			L:    Line{Num: ai + 1, Text: a[ai]},
			R:    Line{Num: bi + 1, Text: b[bi]},
		})
	}
	return d
}

// chunkRows builds the rows for one changed chunk: the lines del were
// replaced by the lines ins, starting at 0-based line numbers a0 and b0.
// Removed and added lines are paired off from the top, the way Gerrit
// aligns them, so that a one-line edit shows as a single row.
func chunkRows(del, ins []string, a0, b0 int) []Row {
	total := len(del) == 0 || len(ins) == 0
	n := min(len(del), len(ins))

	var rows []Row
	for i := range n {
		l := Line{Num: a0 + i + 1, Text: del[i]}
		r := Line{Num: b0 + i + 1, Text: ins[i]}
		ls, rs, ok := intraline(l.Text, r.Text)
		l.Spans, r.Spans = ls, rs
		rows = append(rows, Row{Kind: RowReplace, L: l, R: r, Total: total, NoIntraline: !ok})
	}
	// The lines past the pairing have nothing on the other side, so all of
	// their content is new: mark them as having no intraline information,
	// which colors the whole line rather than leaving it in the pale shade
	// used for a line that merely contains a change.
	for i := n; i < len(del); i++ {
		rows = append(rows, Row{
			Kind:        RowDelete,
			L:           Line{Num: a0 + i + 1, Text: del[i]},
			Total:       total,
			NoIntraline: true,
		})
	}
	for i := n; i < len(ins); i++ {
		rows = append(rows, Row{
			Kind:        RowInsert,
			R:           Line{Num: b0 + i + 1, Text: ins[i]},
			Total:       total,
			NoIntraline: true,
		})
	}
	return rows
}

// MarkRebased marks the rows whose edit also appears in the diff of the two
// sides' parent commits, oldParent and newParent. When a commit lower in a
// stack is edited, rebasing carries its edits into every commit above it, so
// they turn up in those commits' own snapshot-to-snapshot diffs even though
// those commits did not make them. Those are exactly the edits that repeat
// between the two diffs.
//
// Edits are matched by the text they remove and add rather than by position,
// because the change's own edits shift the line numbers on one side and not
// the other. An edit that draws inherited and new lines into a single chunk
// matches nothing and stays unmarked, which errs toward showing a line as the
// change's own work rather than muting one that is.
func MarkRebased(rows []Row, oldParent, newParent []byte) {
	if len(rows) == 0 || bytes.Equal(oldParent, newParent) {
		return
	}
	if isBinary(oldParent) || isBinary(newParent) {
		return
	}
	inherited := make(map[string]int)
	eachChunk(Diff(oldParent, newParent).Rows, func(c []Row) {
		inherited[chunkKey(c)]++
	})
	if len(inherited) == 0 {
		return
	}
	// Each inherited edit accounts for one edit here, so that a file with
	// two identical edits, only one of which came from below, keeps the
	// other one marked as this change's own.
	eachChunk(rows, func(c []Row) {
		k := chunkKey(c)
		if inherited[k] == 0 {
			return
		}
		inherited[k]--
		for i := range c {
			c[i].Rebased = true
		}
	})
}

// AnyRebased reports whether any row is inherited from a rebase.
func AnyRebased(rows []Row) bool {
	for _, r := range rows {
		if r.Rebased {
			return true
		}
	}
	return false
}

// AllRebased reports whether every changed row is inherited from a rebase,
// so that the diff holds nothing the change did itself. A diff with no
// changed rows at all reports false: nothing was inherited there either.
func AllRebased(rows []Row) bool {
	changed := false
	for _, r := range rows {
		if r.Kind == RowEqual || r.Kind == RowSkip {
			continue
		}
		if !r.Rebased {
			return false
		}
		changed = true
	}
	return changed
}

// eachChunk calls f on each maximal run of changed rows.
func eachChunk(rows []Row, f func([]Row)) {
	for i := 0; i < len(rows); {
		if rows[i].Kind == RowEqual || rows[i].Kind == RowSkip {
			i++
			continue
		}
		j := i
		for j < len(rows) && rows[j].Kind != RowEqual && rows[j].Kind != RowSkip {
			j++
		}
		f(rows[i:j])
		i = j
	}
}

// chunkKey identifies an edit by the text it removes and the text it adds,
// so that the same edit is recognized wherever in the file it lands.
func chunkKey(rows []Row) string {
	var b strings.Builder
	for _, r := range rows {
		if r.L.Num > 0 {
			b.WriteString(r.L.Text)
			b.WriteByte('\n')
		}
	}
	b.WriteByte(0)
	for _, r := range rows {
		if r.R.Num > 0 {
			b.WriteString(r.R.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// intraline computes the changed byte ranges within a pair of lines.
// It reports ok=false if the lines differ so much that highlighting
// the differences would be noisier than highlighting the whole line.
func intraline(x, y string) (xs, ys []Span, ok bool) {
	if x == y {
		return nil, nil, true
	}
	ar := []rune(x)
	br := []rune(y)
	if len(ar) == 0 || len(br) == 0 {
		return nil, nil, false
	}

	var aRanges, bRanges []Span
	changed := 0
	for _, c := range diffChanges(ar, br) {
		if c.A1 > c.A0 {
			aRanges = append(aRanges, Span{c.A0, c.A1})
			changed += c.A1 - c.A0
		}
		if c.B1 > c.B0 {
			bRanges = append(bRanges, Span{c.B0, c.B1})
			changed += c.B1 - c.B0
		}
	}
	if float64(changed) > maxIntralineChange*float64(len(ar)+len(br)) {
		return nil, nil, false
	}
	return byteSpans(ar, mergeSpans(aRanges)), byteSpans(br, mergeSpans(bRanges)), true
}

// mergeSpans joins ranges separated by only a few unchanged runes.
func mergeSpans(s []Span) []Span {
	if len(s) < 2 {
		return s
	}
	out := s[:1]
	for _, sp := range s[1:] {
		last := &out[len(out)-1]
		if sp.Lo-last.Hi <= mergeSpanGap {
			last.Hi = sp.Hi
		} else {
			out = append(out, sp)
		}
	}
	return out
}

// byteSpans converts rune index ranges into byte offset ranges within string(r).
func byteSpans(r []rune, spans []Span) []Span {
	if len(spans) == 0 {
		return nil
	}
	// off[i] is the byte offset of rune i.
	off := make([]int, len(r)+1)
	n := 0
	for i, c := range r {
		off[i] = n
		n += len(string(c))
	}
	off[len(r)] = n

	out := make([]Span, len(spans))
	for i, sp := range spans {
		out[i] = Span{off[sp.Lo], off[sp.Hi]}
	}
	return out
}

// Collapse replaces runs of more than 2*ctx unchanged rows with a single
// RowSkip row, leaving ctx rows of context on each side of every change
// and of every row that keep reports as worth showing anyway. A ctx of
// zero or less leaves the rows unchanged, and a nil keep asks to show
// nothing beyond the changes themselves.
//
// keep is how comments survive collapsing. A comment can sit on a line
// that is nowhere near anything the change did — on a file it did not
// touch at all, once one is listed for its comments — and a comment
// folded away behind an expander is one nobody will answer.
func Collapse(rows []Row, ctx int, keep func(Row) bool) []Row {
	if ctx <= 0 {
		return rows
	}
	shown := func(r Row) bool {
		return r.Kind != RowEqual || (keep != nil && keep(r))
	}
	var out []Row
	for i := 0; i < len(rows); {
		if shown(rows[i]) {
			out = append(out, rows[i])
			i++
			continue
		}
		j := i
		for j < len(rows) && !shown(rows[j]) {
			j++
		}
		// rows[i:j] is a run of unchanged rows. Keep ctx of them next to
		// each adjacent change; there is no change before the start of the
		// file or after the end, so those sides keep nothing.
		before, after := ctx, ctx
		if i == 0 {
			before = 0
		}
		if j == len(rows) {
			after = 0
		}
		if j-i <= before+after {
			out = append(out, rows[i:j]...)
			i = j
			continue
		}
		out = append(out, rows[i:i+before]...)
		skip := rows[i+before : j-after]
		out = append(out, Row{
			Kind:  RowSkip,
			Count: len(skip),
			LFrom: skip[0].L.Num,
			RFrom: skip[0].R.Num,
		})
		out = append(out, rows[j-after:j]...)
		i = j
	}
	return out
}

// Unified flattens side-by-side rows into unified rows: each changed row
// becomes a separate left-only or right-only row, with all removals in a
// chunk preceding all additions.
func Unified(rows []Row) []Row {
	var out []Row
	for i := 0; i < len(rows); {
		switch rows[i].Kind {
		case RowEqual, RowSkip:
			out = append(out, rows[i])
			i++
			continue
		}
		// Gather one chunk of changed rows and split it into
		// removals followed by additions.
		j := i
		for j < len(rows) && rows[j].Kind != RowEqual && rows[j].Kind != RowSkip {
			j++
		}
		for _, r := range rows[i:j] {
			if r.Kind == RowReplace || r.Kind == RowDelete {
				out = append(out, Row{Kind: RowDelete, L: r.L, Total: r.Total, NoIntraline: r.NoIntraline, Rebased: r.Rebased})
			}
		}
		for _, r := range rows[i:j] {
			if r.Kind == RowReplace || r.Kind == RowInsert {
				out = append(out, Row{Kind: RowInsert, R: r.R, Total: r.Total, NoIntraline: r.NoIntraline, Rebased: r.Rebased})
			}
		}
		i = j
	}
	return out
}
