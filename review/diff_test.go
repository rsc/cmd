// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

// lcsLen returns the length of the longest common subsequence of a and b,
// computed by dynamic programming. It is the reference against which the
// Myers implementation is checked.
func lcsLen[T comparable](a, b []T) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
			} else {
				cur[j] = max(prev[j], cur[j-1])
			}
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// apply rebuilds b from a and the changes between them.
func apply[T comparable](a, b []T, changes []change) []T {
	var out []T
	ai := 0
	for _, c := range changes {
		out = append(out, a[ai:c.A0]...)
		out = append(out, b[c.B0:c.B1]...)
		ai = c.A1
	}
	return append(out, a[ai:]...)
}

func TestDiffChanges(t *testing.T) {
	// Random sequences over a small alphabet, so that matches are common
	// and the interesting cases (repeated lines, ambiguous alignments)
	// come up often.
	r := rand.New(rand.NewSource(1))
	for i := range 3000 {
		alpha := 1 + r.Intn(4)
		a := randSeq(r, r.Intn(30), alpha)
		b := randSeq(r, r.Intn(30), alpha)

		changes := diffChanges(a, b)

		if got := apply(a, b, changes); !slices.Equal(got, b) {
			t.Fatalf("#%d: apply(%v, changes) = %v, want %v\nchanges: %v", i, a, got, b, changes)
		}

		// Changes must be in order, non-empty, and non-adjacent.
		prev := -1
		for _, c := range changes {
			if c.A0 > c.A1 || c.B0 > c.B1 || c.A0 == c.A1 && c.B0 == c.B1 {
				t.Fatalf("#%d: bad change %v in %v", i, c, changes)
			}
			if c.A0 <= prev {
				t.Fatalf("#%d: changes out of order: %v", i, changes)
			}
			prev = c.A1
		}

		// The edit script must be minimal.
		edits := 0
		for _, c := range changes {
			edits += (c.A1 - c.A0) + (c.B1 - c.B0)
		}
		if want := len(a) + len(b) - 2*lcsLen(a, b); edits != want {
			t.Fatalf("#%d: %d edits, want %d\na = %v\nb = %v\nchanges = %v", i, edits, want, a, b, changes)
		}
	}
}

func randSeq(r *rand.Rand, n, alpha int) []string {
	s := make([]string, n)
	for i := range s {
		s[i] = string(rune('a' + r.Intn(alpha)))
	}
	return s
}

func TestDiffChangesEdges(t *testing.T) {
	tests := []struct {
		a, b string
		want []change
	}{
		{"", "", nil},
		{"a", "a", nil},
		{"", "a", []change{{0, 0, 0, 1}}},
		{"a", "", []change{{0, 1, 0, 0}}},
		{"abc", "abc", nil},
		{"abc", "axc", []change{{1, 2, 1, 2}}},
		{"abc", "ac", []change{{1, 2, 1, 1}}},
		{"ac", "abc", []change{{1, 1, 1, 2}}},
		{"abcd", "dcba", []change{{0, 3, 0, 0}, {4, 4, 1, 4}}},
	}
	for _, tt := range tests {
		a := strings.Split(tt.a, "")
		b := strings.Split(tt.b, "")
		if tt.a == "" {
			a = nil
		}
		if tt.b == "" {
			b = nil
		}
		got := diffChanges(a, b)
		if !slices.Equal(got, tt.want) {
			// Any minimal script is acceptable; check the edit count instead
			// of insisting on one particular alignment.
			edits := 0
			for _, c := range got {
				edits += (c.A1 - c.A0) + (c.B1 - c.B0)
			}
			want := len(a) + len(b) - 2*lcsLen(a, b)
			if edits != want {
				t.Errorf("diffChanges(%q, %q) = %v (%d edits), want %v (%d edits)", tt.a, tt.b, got, edits, tt.want, want)
			}
		}
	}
}

// rowString renders a row compactly for golden comparisons.
func rowString(r Row) string {
	switch r.Kind {
	case RowEqual:
		return fmt.Sprintf("  %d %d %q", r.L.Num, r.R.Num, r.L.Text)
	case RowReplace:
		return fmt.Sprintf("~ %d %d %q|%q%s", r.L.Num, r.R.Num, r.L.Text, r.R.Text, totalMark(r))
	case RowDelete:
		return fmt.Sprintf("- %d %q%s", r.L.Num, r.L.Text, totalMark(r))
	case RowInsert:
		return fmt.Sprintf("+ %d %q%s", r.R.Num, r.R.Text, totalMark(r))
	case RowSkip:
		return fmt.Sprintf("... %d lines from %d/%d", r.Count, r.LFrom, r.RFrom)
	}
	return "?"
}

func totalMark(r Row) string {
	if r.Total {
		return " TOTAL"
	}
	return ""
}

func rowsString(rows []Row) string {
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(rowString(r))
		b.WriteString("\n")
	}
	return b.String()
}

func TestDiff(t *testing.T) {
	tests := []struct {
		name     string
		old, new string
		want     string
	}{
		{
			name: "modify",
			old:  "one\ntwo\nthree\n",
			new:  "one\nTWO\nthree\n",
			want: "  1 1 \"one\"\n~ 2 2 \"two\"|\"TWO\"\n  3 3 \"three\"\n",
		},
		{
			name: "insert only is total",
			old:  "one\nthree\n",
			new:  "one\ntwo\nthree\n",
			want: "  1 1 \"one\"\n+ 2 \"two\" TOTAL\n  2 3 \"three\"\n",
		},
		{
			name: "delete only is total",
			old:  "one\ntwo\nthree\n",
			new:  "one\nthree\n",
			want: "  1 1 \"one\"\n- 2 \"two\" TOTAL\n  3 2 \"three\"\n",
		},
		{
			name: "uneven chunk pairs from the top",
			old:  "a\nb\n",
			new:  "A\nB\nC\n",
			want: "~ 1 1 \"a\"|\"A\"\n~ 2 2 \"b\"|\"B\"\n+ 3 \"C\"\n",
		},
		{
			name: "new file",
			old:  "",
			new:  "x\ny\n",
			want: "+ 1 \"x\" TOTAL\n+ 2 \"y\" TOTAL\n",
		},
		{
			name: "deleted file",
			old:  "x\ny\n",
			new:  "",
			want: "- 1 \"x\" TOTAL\n- 2 \"y\" TOTAL\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Diff([]byte(tt.old), []byte(tt.new))
			if got := rowsString(d.Rows); got != tt.want {
				t.Errorf("Diff:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestDiffNoNewline(t *testing.T) {
	d := Diff([]byte("a\n"), []byte("a"))
	if d.LNoNewline || !d.RNoNewline {
		t.Errorf("LNoNewline=%v RNoNewline=%v, want false true", d.LNoNewline, d.RNoNewline)
	}
	if len(d.Rows) != 1 || d.Rows[0].Kind != RowEqual {
		t.Errorf("rows = %s, want one equal row", rowsString(d.Rows))
	}
}

func TestDiffBinary(t *testing.T) {
	d := Diff([]byte("a\x00b"), []byte("c"))
	if !d.Binary || d.Rows != nil {
		t.Errorf("Binary=%v rows=%d, want true 0", d.Binary, len(d.Rows))
	}
}

func TestIntraline(t *testing.T) {
	tests := []struct {
		x, y     string
		xs, ys   []Span
		wantOK   bool
		wantSame bool // x == y
	}{
		{x: "hello world", y: "hello there", xs: []Span{{6, 11}}, ys: []Span{{6, 11}}, wantOK: true},
		{x: "abc", y: "abc", wantOK: true, wantSame: true},
		{x: "xxxxxxxxxx", y: "yyyyyyyyyy", wantOK: false},
		{x: "", y: "abc", wantOK: false},
		// Two unrelated lines paired up by a chunk share a scattering of
		// letters. Highlighting the gaps between them is noise, so the
		// whole line is marked changed instead.
		{x: "func F() int { return 1 }", y: "// F returns the answer.", wantOK: false},
		// A real edit keeps its intraline highlight, however.
		{x: `	println("hi")`, y: `	println("hello")`, wantOK: true,
			xs: []Span{{11, 12}}, ys: []Span{{11, 15}}},
	}
	for _, tt := range tests {
		xs, ys, ok := intraline(tt.x, tt.y)
		if ok != tt.wantOK {
			t.Errorf("intraline(%q, %q) ok = %v, want %v", tt.x, tt.y, ok, tt.wantOK)
			continue
		}
		if !ok || tt.wantSame {
			continue
		}
		if !slices.Equal(xs, tt.xs) || !slices.Equal(ys, tt.ys) {
			t.Errorf("intraline(%q, %q) = %v, %v; want %v, %v", tt.x, tt.y, xs, ys, tt.xs, tt.ys)
		}
	}
}

// TestIntralineBytes checks that spans are byte offsets, not rune offsets,
// so that they can be used to slice the line directly.
func TestIntralineBytes(t *testing.T) {
	x := "π = 3.14159"
	y := "π = 3.14160"
	xs, ys, ok := intraline(x, y)
	if !ok || len(xs) != 1 || len(ys) != 1 {
		t.Fatalf("intraline = %v, %v, %v", xs, ys, ok)
	}
	if got := x[xs[0].Lo:xs[0].Hi]; got != "59" {
		t.Errorf("x span = %q, want %q", got, "59")
	}
	if got := y[ys[0].Lo:ys[0].Hi]; got != "60" {
		t.Errorf("y span = %q, want %q", got, "60")
	}
}

func TestIntralineSpansValid(t *testing.T) {
	// Spans must always be in range and non-overlapping, whatever the input.
	r := rand.New(rand.NewSource(2))
	for range 2000 {
		x := randText(r)
		y := randText(r)
		xs, ys, ok := intraline(x, y)
		if !ok {
			continue
		}
		checkSpans(t, x, xs)
		checkSpans(t, y, ys)
	}
}

func checkSpans(t *testing.T, s string, spans []Span) {
	t.Helper()
	prev := 0
	for _, sp := range spans {
		if sp.Lo < prev || sp.Hi < sp.Lo || sp.Hi > len(s) {
			t.Fatalf("bad span %v in %q (len %d)", sp, s, len(s))
		}
		prev = sp.Hi
	}
}

func randText(r *rand.Rand) string {
	n := r.Intn(20)
	var b strings.Builder
	for range n {
		b.WriteRune([]rune("abπ→ ")[r.Intn(5)])
	}
	return b.String()
}

func TestCollapse(t *testing.T) {
	// 20 unchanged lines, one change, 20 more unchanged lines.
	var old, new strings.Builder
	for i := range 20 {
		fmt.Fprintf(&old, "line %d\n", i)
		fmt.Fprintf(&new, "line %d\n", i)
	}
	old.WriteString("before\n")
	new.WriteString("after\n")
	for i := range 20 {
		fmt.Fprintf(&old, "tail %d\n", i)
		fmt.Fprintf(&new, "tail %d\n", i)
	}

	rows := Collapse(Diff([]byte(old.String()), []byte(new.String())).Rows, 3, nil)

	// Expect: skip, 3 context, change, 3 context, skip.
	var kinds []RowKind
	for _, r := range rows {
		kinds = append(kinds, r.Kind)
	}
	want := []RowKind{
		RowSkip, RowEqual, RowEqual, RowEqual,
		RowReplace,
		RowEqual, RowEqual, RowEqual, RowSkip,
	}
	if !slices.Equal(kinds, want) {
		t.Fatalf("Collapse gave:\n%s", rowsString(rows))
	}
	if rows[0].Count != 17 || rows[0].LFrom != 1 {
		t.Errorf("leading skip = %d lines from %d, want 17 from 1", rows[0].Count, rows[0].LFrom)
	}
	if last := rows[len(rows)-1]; last.Count != 17 || last.LFrom != 25 {
		t.Errorf("trailing skip = %d lines from %d, want 17 from 25", last.Count, last.LFrom)
	}
}

func TestCollapseKeepsShortRuns(t *testing.T) {
	old := "a\nx\nb\nc\nd\ny\ne\n"
	new := "a\nX\nb\nc\nd\nY\ne\n"
	rows := Collapse(Diff([]byte(old), []byte(new)).Rows, 3, nil)
	for _, r := range rows {
		if r.Kind == RowSkip {
			t.Fatalf("unexpected skip in short run:\n%s", rowsString(rows))
		}
	}
}

func TestCollapseNoContext(t *testing.T) {
	rows := Diff([]byte("a\nb\n"), []byte("a\nB\n")).Rows
	if got := rowsString(Collapse(rows, 0, nil)); got != rowsString(rows) {
		t.Errorf("Collapse with ctx 0 changed the rows:\n%s", got)
	}
}

func TestUnified(t *testing.T) {
	d := Diff([]byte("a\nb\nc\n"), []byte("a\nB\nC\nD\n"))
	got := rowsString(Unified(Collapse(d.Rows, 0, nil)))
	want := "  1 1 \"a\"\n- 2 \"b\"\n- 3 \"c\"\n+ 2 \"B\"\n+ 3 \"C\"\n+ 4 \"D\"\n"
	if got != want {
		t.Errorf("Unified:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedKeepsSkips(t *testing.T) {
	rows := []Row{
		{Kind: RowSkip, Count: 5},
		{Kind: RowReplace, L: Line{Num: 6, Text: "a"}, R: Line{Num: 6, Text: "b"}},
	}
	got := Unified(rows)
	if len(got) != 3 || got[0].Kind != RowSkip || got[1].Kind != RowDelete || got[2].Kind != RowInsert {
		t.Errorf("Unified:\n%s", rowsString(got))
	}
}

// TestChunkTailIsWhollyNew covers a chunk that both changes a line and
// adds lines after it. The added lines have no counterpart, so all of
// their content is new and they take the strong color, rather than the
// pale one used for a line that merely contains a change.
func TestChunkTailIsWhollyNew(t *testing.T) {
	old := "jj workflows, not to replace them. It only adds what jj itself lacks.\n"
	new := "jj workflows, not to replace them. It only adds what jj itself lacks;\n" +
		"the mail command below, for example, builds on jj's own\n" +
		"“jj gerrit upload” (available in jj v0.43 and later).\n"

	rows := Diff([]byte(old), []byte(new)).Rows
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3:\n%s", len(rows), rowsString(rows))
	}
	// The first line pairs off, so only the changed word is highlighted.
	if rows[0].Kind != RowReplace {
		t.Fatalf("row 0 = %s, want a paired row", rowString(rows[0]))
	}
	if rows[0].NoIntraline {
		t.Error("the paired line lost its intraline highlight")
	}
	if len(rows[0].R.Spans) == 0 {
		t.Error("the paired line has no intraline highlight")
	}
	// The two that follow have nothing on the left, so all of them is new.
	for _, i := range []int{1, 2} {
		r := rows[i]
		if r.Kind != RowInsert {
			t.Errorf("row %d = %s, want an addition", i, rowString(r))
			continue
		}
		if !r.NoIntraline {
			t.Errorf("row %d is an addition with no counterpart but is not marked wholly changed: %s", i, rowString(r))
		}
	}
	// It is still not a "total" chunk: the left side is not empty, so the
	// paired row keeps the pale background under its highlight.
	if rows[0].Total || rows[1].Total {
		t.Error("a chunk with both a removal and additions was marked total")
	}
}

// TestUnpairedRemovalIsWhollyOld is the same on the other side.
func TestUnpairedRemovalIsWhollyOld(t *testing.T) {
	rows := Diff([]byte("one\ntwo\nthree\n"), []byte("ONE\n")).Rows
	if len(rows) != 3 {
		t.Fatalf("got %d rows:\n%s", len(rows), rowsString(rows))
	}
	for _, i := range []int{1, 2} {
		if rows[i].Kind != RowDelete || !rows[i].NoIntraline {
			t.Errorf("row %d = %s, want a removal marked wholly changed", i, rowString(rows[i]))
		}
	}
}

// rebasedLines returns the text of every row marked as inherited.
func rebasedLines(rows []Row) []string {
	var out []string
	for _, r := range rows {
		if !r.Rebased {
			continue
		}
		if r.L.Num > 0 {
			out = append(out, "-"+r.L.Text)
		}
		if r.R.Num > 0 {
			out = append(out, "+"+r.R.Text)
		}
	}
	return out
}

func TestMarkRebased(t *testing.T) {
	// The parent commits differ in one line; the change's own diff carries
	// that edit plus one it made itself.
	oldParent := "a\nb\nc\nd\ne\n"
	newParent := "a\nB2\nc\nd\ne\n"
	old := "a\nb\nc\nd\nOWN1\n"
	new := "a\nB2\nc\nd\nOWN2\n"

	rows := Diff([]byte(old), []byte(new)).Rows
	MarkRebased(rows, []byte(oldParent), []byte(newParent))
	got := rebasedLines(rows)
	want := []string{"-b", "+B2"}
	if !slices.Equal(got, want) {
		t.Errorf("rebased = %q, want %q\n%s", got, want, rowsString(rows))
	}
	if !AnyRebased(rows) {
		t.Error("AnyRebased = false")
	}

	// Unified rows keep the mark, so the flattened view colors them too.
	if got := rebasedLines(Unified(rows)); !slices.Equal(got, want) {
		t.Errorf("unified rebased = %q, want %q", got, want)
	}
}

func TestMarkRebasedSameParents(t *testing.T) {
	// Two snapshots on the same parent inherit nothing: every edit in the
	// diff is the change's own, however much the parent commit contains.
	rows := Diff([]byte("a\nb\n"), []byte("a\nB2\n")).Rows
	MarkRebased(rows, []byte("a\nb\n"), []byte("a\nb\n"))
	if AnyRebased(rows) {
		t.Errorf("marked rows inherited from an unchanged parent:\n%s", rowsString(rows))
	}
}

// TestMarkRebasedCountsEdits checks that an inherited edit accounts for one
// edit in the change's diff, not for every edit that happens to match it.
func TestMarkRebasedCountsEdits(t *testing.T) {
	// The parent gained one "x -> y" edit. The change's diff has two,
	// because the change made the same edit somewhere else itself.
	oldParent := "x\n1\n2\n3\n4\n5\n6\n7\n8\nx\n"
	newParent := "y\n1\n2\n3\n4\n5\n6\n7\n8\nx\n"
	old := "x\n1\n2\n3\n4\n5\n6\n7\n8\nx\n"
	new := "y\n1\n2\n3\n4\n5\n6\n7\n8\ny\n"

	rows := Diff([]byte(old), []byte(new)).Rows
	MarkRebased(rows, []byte(oldParent), []byte(newParent))
	if got, want := len(rebasedLines(rows)), 2; got != want {
		t.Errorf("marked %d lines, want %d (one edit, two lines):\n%s", got, want, rowsString(rows))
	}
	if !rows[0].Rebased || rows[len(rows)-1].Rebased {
		t.Errorf("wrong edit marked:\n%s", rowsString(rows))
	}
}

// TestCollapseKeepsWantedRows checks that a row the caller asks to keep
// survives collapsing with context around it. Comments ride on this: one
// written far from any change would otherwise be folded away unread.
func TestCollapseKeepsWantedRows(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	old := b.String()
	new := strings.Replace(old, "line 1\n", "LINE 1\n", 1)

	rows := Diff([]byte(old), []byte(new)).Rows
	keep := func(r Row) bool { return r.R.Num == 40 }
	got := Collapse(rows, 3, keep)

	// Line 40 and three lines either side of it are shown, and the run is
	// broken into two skips rather than one.
	shown := map[int]bool{}
	skips := 0
	for _, r := range got {
		if r.Kind == RowSkip {
			skips++
			continue
		}
		shown[r.R.Num] = true
	}
	for n := 37; n <= 43; n++ {
		if !shown[n] {
			t.Errorf("line %d was collapsed away:\n%s", n, rowsString(got))
			break
		}
	}
	if skips != 2 {
		t.Errorf("got %d skip rows, want 2 (one either side of the kept line)", skips)
	}
	// Without the request it is a single run, as before.
	if plain := Collapse(rows, 3, nil); len(plain) >= len(got) {
		t.Errorf("keeping a row did not show more: %d rows with, %d without", len(got), len(plain))
	}
}
