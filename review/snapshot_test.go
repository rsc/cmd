// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"slices"
	"testing"
)

// newReview creates a git repository with a review database attached.
func newReview(t *testing.T) (*Review, string) {
	t.Helper()
	dir := newGitRepo(t)
	repo, err := OpenRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &Review{Repo: repo, DB: newDB(t), Pin: true}, dir
}

func TestGrabAndView(t *testing.T) {
	r, dir := newReview(t)
	write(t, dir, "a.go", "one\ntwo\nthree\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "add a.go\n\nChange-Id: Ideadbeef\n")

	changes, err := r.Repo.Changes()
	if err != nil {
		t.Fatal(err)
	}
	c := changes[0]

	// Viewing a change snapshots it implicitly, so there is always
	// something for comments to attach to.
	v, err := r.View(c, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Snapshots) != 1 || v.Target.N != 1 {
		t.Fatalf("snapshots = %+v, target = %+v", v.Snapshots, v.Target)
	}
	if v.Base != nil || v.BaseName() != "Parent" {
		t.Errorf("base = %+v, name %q; want the parent commit", v.Base, v.BaseName())
	}
	// The commit message is always the first file.
	if len(v.Files) != 2 || v.Files[0].Path != CommitMsgFile || v.Files[1].Path != "a.go" {
		t.Fatalf("files = %+v", v.Files)
	}

	old, new, err := r.Contents(v, v.Files[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 0 || string(new) != "one\ntwo\nthree\n" {
		t.Errorf("contents = %q, %q", old, new)
	}

	// The commit message pseudo-file has real content to comment on.
	_, msg, err := r.Contents(v, v.Files[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(msg) == 0 {
		t.Error("commit message file is empty")
	}
}

func TestViewBetweenSnapshots(t *testing.T) {
	r, dir := newReview(t)
	write(t, dir, "a.go", "one\ntwo\nthree\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "add a.go\n\nChange-Id: Ideadbeef\n")

	changes, _ := r.Repo.Changes()
	if _, err := r.EnsureSnapshot(changes[0]); err != nil {
		t.Fatal(err)
	}

	// Amend the change and grab a second snapshot.
	write(t, dir, "a.go", "one\nTWO\nthree\nfour\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "--amend", "--no-edit")

	changes, _ = r.Repo.Changes()
	c := changes[0]
	s2, created, err := r.Grab(c)
	if err != nil {
		t.Fatal(err)
	}
	if !created || s2.N != 2 {
		t.Fatalf("grab = %+v, created=%v; want snapshot 2", s2, created)
	}

	// Snapshot 1 against snapshot 2 shows only what the amend did.
	v, err := r.View(c, "1", "2", "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Base == nil || v.Base.N != 1 || v.Target.N != 2 {
		t.Fatalf("view = base %+v target %+v", v.Base, v.Target)
	}
	if v.BaseName() != "Snapshot 1" || v.TargetName() != "Snapshot 2" {
		t.Errorf("names = %q -> %q", v.BaseName(), v.TargetName())
	}

	old, new, err := r.Contents(v, v.File("a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(old) != "one\ntwo\nthree\n" || string(new) != "one\nTWO\nthree\nfour\n" {
		t.Errorf("contents = %q, %q", old, new)
	}
	d := Diff(old, new)
	var changed int
	for _, row := range d.Rows {
		if row.Kind != RowEqual {
			changed++
		}
	}
	if changed != 2 {
		t.Errorf("%d changed rows, want 2 (one edit, one addition):\n%s", changed, rowsString(d.Rows))
	}

	// A base that is not older than the target is refused.
	if _, err := r.View(c, "2", "2", ""); err == nil {
		t.Error("View accepted a base equal to the target")
	}
	if _, err := r.View(c, "5", "", ""); err == nil {
		t.Error("View accepted a nonexistent snapshot")
	}
}

// TestGrabAcrossAmendNoChangeID checks that a git change with no
// Change-Id trailer still lands its second snapshot on the same change
// as its first, after an amend that would otherwise make it look like a
// brand new change with a fresh hash and no history of its own.
func TestGrabAcrossAmendNoChangeID(t *testing.T) {
	r, dir := newReview(t)
	write(t, dir, "a.go", "one\ntwo\nthree\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "add a.go") // no Change-Id

	changes, _ := r.Repo.Changes()
	c := changes[0]
	rev1 := c.Rev
	if c.Key != rev1 {
		t.Fatalf("Key = %q before any snapshot, want the hash %q", c.Key, rev1)
	}
	if _, err := r.EnsureSnapshot(c); err != nil {
		t.Fatal(err)
	}
	key := c.Key // EnsureSnapshot mints the stable key into c itself
	if key == rev1 {
		t.Fatal("EnsureSnapshot left Key as the hash; want a minted key")
	}

	write(t, dir, "a.go", "one\nTWO\nthree\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "--amend", "--no-edit")

	changes, _ = r.Repo.Changes()
	if len(changes) != 1 {
		t.Fatalf("Changes = %d changes after amend, want 1 (not a second, unrelated change)", len(changes))
	}
	c2 := changes[0]
	if c2.Rev == rev1 {
		t.Fatal("amend did not change the hash; test is not exercising anything")
	}
	if c2.Key != key {
		t.Fatalf("Key after amend = %q, want the same key as before amend, %q", c2.Key, key)
	}

	s2, created, err := r.Grab(c2)
	if err != nil {
		t.Fatal(err)
	}
	if !created || s2.N != 2 {
		t.Fatalf("grab = %+v, created=%v; want snapshot 2 of the same change", s2, created)
	}
}

func TestPlaceThreads(t *testing.T) {
	r, dir := newReview(t)
	write(t, dir, "a.go", "one\ntwo\nthree\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "add a.go\n\nChange-Id: Ideadbeef\n")

	changes, _ := r.Repo.Changes()
	snaps, err := r.EnsureSnapshot(changes[0])
	if err != nil {
		t.Fatal(err)
	}
	s1 := snaps[0]

	// A comment on line 3 of snapshot 1, anchored to its text.
	if _, err := r.DB.AddThread(s1.ID, "a.go", "new", 3, "three", &Comment{
		Author: "rsc", Body: "rename this", Draft: false,
	}); err != nil {
		t.Fatal(err)
	}

	// Insert a line above it, so the comment's line number shifts.
	write(t, dir, "a.go", "zero\none\ntwo\nthree\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "--amend", "--no-edit")

	changes, _ = r.Repo.Changes()
	c := changes[0]
	if _, _, err := r.Grab(c); err != nil {
		t.Fatal(err)
	}

	v, err := r.View(c, "", "", "") // parent against snapshot 2
	if err != nil {
		t.Fatal(err)
	}
	all, err := r.DB.Threads(r.Root(), c.Key)
	if err != nil {
		t.Fatal(err)
	}
	old, new, _ := r.Contents(v, v.File("a.go"))
	oldLines, _ := splitLines(old)
	newLines, _ := splitLines(new)

	left, right := v.PlaceThreads(all, "a.go", oldLines, newLines)
	if len(left) != 0 || len(right) != 1 {
		t.Fatalf("placed %d left, %d right; want 0 and 1", len(left), len(right))
	}
	th := right[0]
	if !th.Other {
		t.Error("thread from snapshot 1 not marked as belonging to another snapshot")
	}
	if th.Stale {
		t.Error("thread marked stale even though its line still exists")
	}
	if th.ShowLine != 4 {
		t.Errorf("thread drawn at line %d, want 4 (it moved down one line)", th.ShowLine)
	}
	if th.SnapshotN != 1 {
		t.Errorf("SnapshotN = %d, want 1", th.SnapshotN)
	}

	// Viewing snapshot 1 again puts the comment back on its own line,
	// no longer marked as belonging elsewhere.
	v1, err := r.View(c, "", "1", "")
	if err != nil {
		t.Fatal(err)
	}
	_, new1, _ := r.Contents(v1, v1.File("a.go"))
	lines1, _ := splitLines(new1)
	_, right = v1.PlaceThreads(all, "a.go", nil, lines1)
	if len(right) != 1 || right[0].Other || right[0].ShowLine != 3 {
		t.Errorf("on snapshot 1 the thread is %+v, want line 3 and not Other", right[0])
	}
}

func TestPlaceThreadsStale(t *testing.T) {
	r, dir := newReview(t)
	write(t, dir, "a.go", "one\ntwo\nthree\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "add a.go\n\nChange-Id: Ideadbeef\n")

	changes, _ := r.Repo.Changes()
	snaps, _ := r.EnsureSnapshot(changes[0])
	r.DB.AddThread(snaps[0].ID, "a.go", "new", 2, "two", &Comment{Author: "rsc", Body: "hm"})

	// Delete the commented line entirely.
	write(t, dir, "a.go", "one\nthree\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "--amend", "--no-edit")
	changes, _ = r.Repo.Changes()
	r.Grab(changes[0])

	v, _ := r.View(changes[0], "", "", "")
	all, _ := r.DB.Threads(r.Root(), changes[0].Key)
	_, new, _ := r.Contents(v, v.File("a.go"))
	newLines, _ := splitLines(new)

	_, right := v.PlaceThreads(all, "a.go", nil, newLines)
	if len(right) != 1 || !right[0].Stale {
		t.Fatalf("thread = %+v, want it marked stale", right[0])
	}
	// Stale threads are grouped at line 0 so they are listed rather than lost.
	if got := ThreadsByLine(right); len(got[0]) != 1 {
		t.Errorf("ThreadsByLine = %v, want the stale thread at line 0", got)
	}
}

func TestChangeLookup(t *testing.T) {
	r, dir := newReview(t)
	write(t, dir, "a.go", "x\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "subject\n\nChange-Id: Ideadbeef\n")

	c, err := r.Change("Ideadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "subject" {
		t.Errorf("Subject = %q", c.Subject)
	}
	// A prefix of the commit hash works too.
	if _, err := r.Change(c.Rev[:8]); err != nil {
		t.Errorf("lookup by hash prefix: %v", err)
	}
	if _, err := r.Change("nosuchthing"); err == nil {
		t.Error("lookup of a nonexistent change succeeded")
	}
}

func TestGrabPinsCommit(t *testing.T) {
	r, dir := newReview(t)
	write(t, dir, "a.go", "one\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "first\n\nChange-Id: Ipinned\n")

	changes, _ := r.Repo.Changes()
	snaps, err := r.EnsureSnapshot(changes[0])
	if err != nil {
		t.Fatal(err)
	}
	old := snaps[0].Rev

	write(t, dir, "a.go", "two\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "--amend", "--no-edit")
	do(t, dir, "git", "reflog", "expire", "--expire=now", "--all")
	do(t, dir, "git", "gc", "--prune=now", "-q")

	// Snapshot 1's commit must still be diffable after collection.
	changes, _ = r.Repo.Changes()
	r.Grab(changes[0])
	v, err := r.View(changes[0], "1", "2", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Contents(v, v.File("a.go")); err != nil {
		t.Fatalf("snapshot 1 was garbage collected: %v", err)
	}
	if v.BaseRev != old {
		t.Errorf("base rev = %q, want %q", v.BaseRev, old)
	}
}

// TestCarryReviewedForward checks that marking a file reviewed survives a
// new snapshot when that file did not change, and does not when it did.
func TestCarryReviewedForward(t *testing.T) {
	r, dir := newReview(t)
	write(t, dir, "stable.go", "package p\n\nfunc Stable() {}\n")
	write(t, dir, "moving.go", "package p\n\nfunc Moving() int { return 1 }\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "p: add two files\n\nChange-Id: Icarry\n")

	changes, _ := r.Repo.Changes()
	snaps, err := r.EnsureSnapshot(changes[0])
	if err != nil {
		t.Fatal(err)
	}
	s1 := snaps[0]

	// Read everything, then let one file move underneath.
	for _, f := range []string{"stable.go", "moving.go", CommitMsgFile} {
		if err := r.DB.SetReviewed(s1.ID, f, true); err != nil {
			t.Fatal(err)
		}
	}
	write(t, dir, "moving.go", "package p\n\nfunc Moving() int { return 42 }\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "--amend", "--no-edit")

	changes, _ = r.Repo.Changes()
	s2, created, err := r.Grab(changes[0])
	if err != nil || !created {
		t.Fatalf("grab = %+v, created=%v, err=%v", s2, created, err)
	}

	got, err := r.DB.Reviewed(s2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got["stable.go"] {
		t.Error("an unchanged file lost its reviewed mark")
	}
	if got["moving.go"] {
		t.Error("a changed file kept its reviewed mark")
	}
	// The message did not change, so its mark carries too.
	if !got[CommitMsgFile] {
		t.Error("an unchanged commit message lost its reviewed mark")
	}
	// The snapshot's own mark is a verdict on a state that is gone.
	if ok, _ := r.DB.SnapshotReviewed(s2.ID); ok {
		t.Error("the snapshot itself was marked reviewed")
	}

	// Rewording the message alone unmarks it, and leaves the files alone.
	if err := r.DB.SetReviewed(s2.ID, "moving.go", true); err != nil {
		t.Fatal(err)
	}
	do(t, dir, "git", "commit", "-q", "--amend", "-m", "p: add two files, reworded\n\nChange-Id: Icarry\n")
	changes, _ = r.Repo.Changes()
	s3, created, err := r.Grab(changes[0])
	if err != nil || !created {
		t.Fatalf("grab = %+v, created=%v, err=%v", s3, created, err)
	}
	got, _ = r.DB.Reviewed(s3.ID)
	if got[CommitMsgFile] {
		t.Error("a reworded commit message kept its reviewed mark")
	}
	for _, f := range []string{"stable.go", "moving.go"} {
		if !got[f] {
			t.Errorf("%s changed only in the message but lost its mark", f)
		}
	}
}

// TestCarryReviewedAcrossRename checks that a file that moved is offered
// for reading again rather than inheriting the mark from its old path.
func TestCarryReviewedAcrossRename(t *testing.T) {
	r, dir := newReview(t)
	write(t, dir, "old.go", "package p\n\nfunc F() {}\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "p: add old.go\n\nChange-Id: Irename\n")

	changes, _ := r.Repo.Changes()
	snaps, _ := r.EnsureSnapshot(changes[0])
	if err := r.DB.SetReviewed(snaps[0].ID, "old.go", true); err != nil {
		t.Fatal(err)
	}

	do(t, dir, "git", "mv", "old.go", "new.go")
	do(t, dir, "git", "commit", "-q", "--amend", "--no-edit")
	changes, _ = r.Repo.Changes()
	s2, _, err := r.Grab(changes[0])
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.DB.Reviewed(s2.ID)
	if got["new.go"] || got["old.go"] {
		t.Errorf("a renamed file carried its mark: %v", got)
	}
}

// topChangeKey identifies the upper change built by newStackedReview.
const topChangeKey = "Ideadbeef02"

// newStackedReview builds two stacked changes, snapshots them, then edits
// the lower one and rebases the upper one onto it, amending that too, and
// snapshots again. The upper change's snapshot 1 to snapshot 2 diff then
// mixes an edit it made itself, in shared.txt, with two it did not: the
// rest of shared.txt and the whole of deep.txt.
func newStackedReview(t *testing.T) (*Review, string) {
	t.Helper()
	r, dir := newReview(t)
	write(t, dir, "shared.txt", "a\nb\nc\nd\ne\n")
	write(t, dir, "deep.txt", "one\ntwo\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "bottom\n\nChange-Id: Ideadbeef01\n")

	write(t, dir, "shared.txt", "a\nb\nc\nd\nTOP1\n")
	do(t, dir, "git", "commit", "-q", "-a", "-m", "top\n\nChange-Id: "+topChangeKey+"\n")

	top, err := r.Change(topChangeKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.EnsureSnapshot(top); err != nil {
		t.Fatal(err)
	}
	topRev := top.Rev

	do(t, dir, "git", "reset", "-q", "--hard", "HEAD~1")
	write(t, dir, "shared.txt", "a\nB2\nc\nd\ne\n")
	write(t, dir, "deep.txt", "one\nTWO\n")
	do(t, dir, "git", "commit", "-q", "-a", "--amend", "--no-edit")
	do(t, dir, "git", "cherry-pick", topRev)
	write(t, dir, "shared.txt", "a\nB2\nc\nd\nTOP2\n")
	do(t, dir, "git", "commit", "-q", "-a", "--amend", "--no-edit")

	if top, err = r.Change(topChangeKey); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Grab(top); err != nil {
		t.Fatal(err)
	}
	return r, dir
}

// TestRebaseInheritedEdits checks that the edits the upper change inherited
// from the rebase are told apart from the one it made itself.
func TestRebaseInheritedEdits(t *testing.T) {
	r, _ := newStackedReview(t)
	top, err := r.Change(topChangeKey)
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.View(top, "1", "2", "")
	if err != nil {
		t.Fatal(err)
	}

	// deep.txt is nowhere in the upper change's own work, so everything
	// its diff shows was done below it.
	stats, err := r.FileStats(v)
	if err != nil {
		t.Fatal(err)
	}
	only := map[string]bool{}
	for path, st := range stats {
		only[path] = st.RebaseOnly
	}
	if !only["deep.txt"] {
		t.Errorf("deep.txt not reported as rebase-only; got %v", only)
	}
	if only["shared.txt"] {
		t.Error("shared.txt reported as rebase-only, but the change edits it")
	}

	f := v.File("shared.txt")
	if f == nil {
		t.Fatalf("shared.txt missing from the diff; files = %+v", v.Files)
	}
	old, new, err := r.Contents(v, f)
	if err != nil {
		t.Fatal(err)
	}
	rows := Diff(old, new).Rows
	r.MarkInherited(v, f, old, new, rows)
	got := rebasedLines(rows)
	want := []string{"-b", "+B2"}
	if !slices.Equal(got, want) {
		t.Errorf("rebased = %q, want %q\n%s", got, want, rowsString(rows))
	}

	// Against the parent commit the whole diff is the change's own work,
	// however much of it a rebase carried along.
	pv, err := r.View(top, "parent", "2", "")
	if err != nil {
		t.Fatal(err)
	}
	if pstats, err := r.FileStats(pv); err != nil {
		t.Fatal(err)
	} else if pstats["shared.txt"].RebaseOnly || pstats["deep.txt"].RebaseOnly {
		t.Errorf("parent view reports rebase-only files: %v", pstats)
	}
	pf := pv.File("shared.txt")
	old, new, err = r.Contents(pv, pf)
	if err != nil {
		t.Fatal(err)
	}
	rows = Diff(old, new).Rows
	r.MarkInherited(pv, pf, old, new, rows)
	if AnyRebased(rows) {
		t.Errorf("parent view marked rows inherited:\n%s", rowsString(rows))
	}
}

// TestMarkSnapshotFilesMarksFiles checks that marking a snapshot reviewed
// is a statement about its files too, and that unmarking takes it back.
func TestMarkSnapshotFilesMarksFiles(t *testing.T) {
	r, dir := newReview(t)
	write(t, dir, "a.go", "package p\n")
	write(t, dir, "b.go", "package q\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "two files\n\nChange-Id: Imark\n")

	c, err := r.Change("Imark")
	if err != nil {
		t.Fatal(err)
	}
	snaps, err := r.EnsureSnapshot(c)
	if err != nil {
		t.Fatal(err)
	}
	snap := snaps[0]

	if err := r.MarkSnapshotFiles(snap, true); err != nil {
		t.Fatal(err)
	}
	marks, err := r.DB.Reviewed(snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{CommitMsgFile, "a.go", "b.go"} {
		if !marks[want] {
			t.Errorf("%s not marked reviewed; marks = %v", want, marks)
		}
	}
	// base.txt is in the repository but not in this change.
	if marks["base.txt"] {
		t.Error("marked a file the change does not touch")
	}

	if err := r.MarkSnapshotFiles(snap, false); err != nil {
		t.Fatal(err)
	}
	if marks, err = r.DB.Reviewed(snap.ID); err != nil {
		t.Fatal(err)
	} else if len(marks) != 0 {
		t.Errorf("unmarking left %v behind", marks)
	}
}

// TestSpreadMarksAcrossRebase is the case this exists for: a commit low
// in a stack is edited, everything above it is rebased, and the commits
// above have nothing new in them to read. Their reviewed marks should
// survive the move.
func TestSpreadMarksAcrossRebase(t *testing.T) {
	r, _ := newStackedReview(t)
	top, err := r.Change(topChangeKey)
	if err != nil {
		t.Fatal(err)
	}
	snaps, err := r.DB.Snapshots(r.Root(), topChangeKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("got %d snapshots, want 2", len(snaps))
	}

	// The stacked fixture amends the top change itself, so snapshot 2 has
	// work of its own and the mark must not carry.
	if err := r.DB.SetSnapshotReviewed(snaps[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if err := r.SpreadMarks(topChangeKey); err != nil {
		t.Fatal(err)
	}
	if on, err := r.DB.SnapshotReviewed(snaps[1].ID); err != nil {
		t.Fatal(err)
	} else if on {
		t.Fatal("carried the mark over a snapshot with work of its own")
	}

	// Now review snapshot 2 and rebase the change without touching it: the
	// commit below it moves, and nothing here changes but the parent.
	if err := r.DB.SetSnapshotReviewed(snaps[1].ID, true); err != nil {
		t.Fatal(err)
	}
	dir := r.Root()
	topRev := top.Rev
	do(t, dir, "git", "reset", "-q", "--hard", "HEAD~1")
	write(t, dir, "deep.txt", "one\nTHREE\n")
	do(t, dir, "git", "commit", "-q", "-a", "--amend", "--no-edit")
	do(t, dir, "git", "cherry-pick", topRev)

	if top, err = r.Change(topChangeKey); err != nil {
		t.Fatal(err)
	}
	if _, created, err := r.Grab(top); err != nil {
		t.Fatal(err)
	} else if !created {
		t.Fatal("the rebase did not produce a new snapshot")
	}

	snaps, err = r.DB.Snapshots(r.Root(), topChangeKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 3 {
		t.Fatalf("got %d snapshots, want 3", len(snaps))
	}
	if on, err := r.DB.SnapshotReviewed(snaps[2].ID); err != nil {
		t.Fatal(err)
	} else if !on {
		t.Error("a snapshot holding nothing but a rebase was left unreviewed")
	}
	// And its files came with it, since marking a snapshot marks them.
	marks, err := r.DB.Reviewed(snaps[2].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !marks["shared.txt"] || !marks[CommitMsgFile] {
		t.Errorf("files not marked with the snapshot: %v", marks)
	}
}

// TestSpreadMarksStopsAtRealWork checks the other half: a snapshot that
// changes something of the change's own is not carried over.
func TestSpreadMarksStopsAtRealWork(t *testing.T) {
	r, dir := newReview(t)
	write(t, dir, "a.go", "package p\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "one\n\nChange-Id: Iwork\n")

	c, err := r.Change("Iwork")
	if err != nil {
		t.Fatal(err)
	}
	snaps, err := r.EnsureSnapshot(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.DB.SetSnapshotReviewed(snaps[0].ID, true); err != nil {
		t.Fatal(err)
	}

	write(t, dir, "a.go", "package p\n\nvar X int\n")
	do(t, dir, "git", "commit", "-q", "-a", "--amend", "--no-edit")
	if c, err = r.Change("Iwork"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Grab(c); err != nil {
		t.Fatal(err)
	}

	snaps, err = r.DB.Snapshots(r.Root(), "Iwork")
	if err != nil {
		t.Fatal(err)
	}
	if on, err := r.DB.SnapshotReviewed(snaps[1].ID); err != nil {
		t.Fatal(err)
	} else if on {
		t.Error("an amended change was marked reviewed without being read")
	}
}

// TestGrabSpreadsWithNothingToGrab checks that grabbing settles the
// reviewed marks even when the change has not moved. The marks can fall
// behind whenever a snapshot is marked reviewed after a later one already
// exists, and grabbing is what a reviewer reaches for then.
func TestGrabSpreadsWithNothingToGrab(t *testing.T) {
	r, _ := newStackedReview(t)
	top, err := r.Change(topChangeKey)
	if err != nil {
		t.Fatal(err)
	}

	// Rebase the top change without touching it, so snapshot 3 holds
	// nothing of its own, and record it before anything is reviewed.
	dir := r.Root()
	topRev := top.Rev
	do(t, dir, "git", "reset", "-q", "--hard", "HEAD~1")
	write(t, dir, "deep.txt", "one\nTHREE\n")
	do(t, dir, "git", "commit", "-q", "-a", "--amend", "--no-edit")
	do(t, dir, "git", "cherry-pick", topRev)
	if top, err = r.Change(topChangeKey); err != nil {
		t.Fatal(err)
	}
	if _, created, err := r.Grab(top); err != nil || !created {
		t.Fatalf("grab = %v, %v; want a new snapshot", created, err)
	}

	snaps, err := r.DB.Snapshots(r.Root(), topChangeKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 3 {
		t.Fatalf("got %d snapshots, want 3", len(snaps))
	}

	// Now mark snapshot 2 reviewed directly, the way the CLI or an older
	// database would leave it, without going through the web handler that
	// spreads on its own.
	if err := r.DB.SetSnapshotReviewed(snaps[1].ID, true); err != nil {
		t.Fatal(err)
	}
	if on, err := r.DB.SnapshotReviewed(snaps[2].ID); err != nil {
		t.Fatal(err)
	} else if on {
		t.Fatal("snapshot 3 was already marked before the grab")
	}

	// Grabbing again records nothing, and settles the marks anyway.
	s, created, err := r.Grab(top)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("grab recorded a snapshot for a change that had not moved")
	}
	if s.N != 3 {
		t.Errorf("grab returned snapshot %d, want the newest", s.N)
	}
	if on, err := r.DB.SnapshotReviewed(snaps[2].ID); err != nil {
		t.Fatal(err)
	} else if !on {
		t.Error("grabbing did not carry the mark to the snapshot above")
	}
	marks, err := r.DB.Reviewed(snaps[2].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !marks["shared.txt"] {
		t.Errorf("files not marked with the snapshot: %v", marks)
	}
}

// TestOnlyInheritedUnknownParentKey covers a snapshot recorded before
// parent keys were stored. It has none, which is not a different parent
// but an unknown one, and must not block the carry for good.
func TestOnlyInheritedUnknownParentKey(t *testing.T) {
	r, _ := newStackedReview(t)
	snaps, err := r.DB.Snapshots(r.Root(), topChangeKey)
	if err != nil {
		t.Fatal(err)
	}
	prev, next := snaps[0], snaps[1]

	// Both keys known and equal: the message compares equal.
	prev.ParentKey, next.ParentKey = "kkk", "kkk"
	if msgWithoutParents("jj", prev.Change()) == msgWithoutParents("jj", next.Change()) {
		// The fixture amends the message's parent only, so this holds.
	} else {
		t.Fatal("fixture messages differ for reasons other than the parent")
	}

	// One side unrecorded: the rendered messages must still compare equal,
	// which is what the empty column used to break.
	prev.ParentKey, next.ParentKey = "", "kkk"
	if got, want := msgWithoutParents("jj", prev.Change()), msgWithoutParents("jj", next.Change()); got != want {
		t.Errorf("an unrecorded parent key changes the message:\n%q\nvs\n%q", got, want)
	}
}

// TestRebaseOnlyFileTheChangeEdits is the case the cheap test misses: the
// change edits a file, the rebase edits it too, and the change's own edit
// is the same on both sides, so the diff between the two snapshots holds
// nothing the change did.
func TestRebaseOnlyFileTheChangeEdits(t *testing.T) {
	r, dir := newReview(t)
	lines := func(first, tenth string) string {
		out := first + "\n"
		for i := 2; i <= 9; i++ {
			out += fmt.Sprintf("line %d\n", i)
		}
		return out + tenth + "\n"
	}
	write(t, dir, "shared.txt", lines("top of file", "bottom of file"))
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "lower\n\nChange-Id: Ilower\n")

	// The upper change edits the last line, and nothing else.
	write(t, dir, "shared.txt", lines("top of file", "bottom of file EDITED"))
	do(t, dir, "git", "commit", "-q", "-a", "-m", "upper\n\nChange-Id: Iupper\n")

	upper, err := r.Change("Iupper")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.EnsureSnapshot(upper); err != nil {
		t.Fatal(err)
	}
	upperRev := upper.Rev

	// Edit the lower change's first line and replay the upper one.
	do(t, dir, "git", "reset", "-q", "--hard", "HEAD~1")
	write(t, dir, "shared.txt", lines("top of file REWRITTEN", "bottom of file"))
	do(t, dir, "git", "commit", "-q", "-a", "--amend", "--no-edit")
	do(t, dir, "git", "cherry-pick", upperRev)

	if upper, err = r.Change("Iupper"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Grab(upper); err != nil {
		t.Fatal(err)
	}

	v, err := r.View(upper, "1", "2", "")
	if err != nil {
		t.Fatal(err)
	}
	if v.File("shared.txt") == nil {
		t.Fatal("shared.txt is not in the snapshot-to-snapshot diff")
	}
	stats, err := r.FileStats(v)
	if err != nil {
		t.Fatal(err)
	}
	only := map[string]bool{}
	for path, st := range stats {
		only[path] = st.RebaseOnly
	}
	if !only["shared.txt"] {
		t.Error("a file whose only difference came from below was not called rebase-only")
	}

	// Against the parent the change's own edit is there to see, so it is
	// not rebase-only in that view.
	pv, err := r.View(upper, "parent", "2", "")
	if err != nil {
		t.Fatal(err)
	}
	if stats, err := r.FileStats(pv); err != nil {
		t.Fatal(err)
	} else if stats["shared.txt"].RebaseOnly {
		t.Error("the whole-change view called an edited file rebase-only")
	}
}

// TestSpreadMarksCarriesLGTM checks that an LGTM travels across a rebase
// like the reviewed mark does. What looked good is still there; stripping
// the sign-off because a commit below it moved would ask for it again for
// no reason anyone could point at.
func TestSpreadMarksCarriesLGTM(t *testing.T) {
	r, _ := newStackedReview(t)
	top, err := r.Change(topChangeKey)
	if err != nil {
		t.Fatal(err)
	}
	snaps, err := r.DB.Snapshots(r.Root(), topChangeKey)
	if err != nil {
		t.Fatal(err)
	}
	latest := snaps[len(snaps)-1]
	if err := r.DB.SetSnapshotLGTM(latest.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := r.DB.SetSnapshotReviewed(latest.ID, true); err != nil {
		t.Fatal(err)
	}

	// Rebase the change without touching it.
	dir := r.Root()
	topRev := top.Rev
	do(t, dir, "git", "reset", "-q", "--hard", "HEAD~1")
	write(t, dir, "deep.txt", "one\nTHREE\n")
	do(t, dir, "git", "commit", "-q", "-a", "--amend", "--no-edit")
	do(t, dir, "git", "cherry-pick", topRev)
	if top, err = r.Change(topChangeKey); err != nil {
		t.Fatal(err)
	}
	if _, created, err := r.Grab(top); err != nil || !created {
		t.Fatalf("grab = %v, %v; want a new snapshot", created, err)
	}

	snaps, err = r.DB.Snapshots(r.Root(), topChangeKey)
	if err != nil {
		t.Fatal(err)
	}
	next := snaps[len(snaps)-1]
	if on, err := r.DB.SnapshotLGTM(next.ID); err != nil {
		t.Fatal(err)
	} else if !on {
		t.Error("the LGTM did not survive a rebase that changed nothing")
	}
	if on, err := r.DB.SnapshotReviewed(next.ID); err != nil {
		t.Fatal(err)
	} else if !on {
		t.Error("the reviewed mark did not travel with it")
	}
}

// TestSpreadMarksLGTMStopsAtRealWork checks the other half: an amended
// snapshot has something new to look at, so the sign-off does not carry.
func TestSpreadMarksLGTMStopsAtRealWork(t *testing.T) {
	r, dir := newReview(t)
	write(t, dir, "a.go", "package p\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "one\n\nChange-Id: Ilgtm\n")

	c, err := r.Change("Ilgtm")
	if err != nil {
		t.Fatal(err)
	}
	snaps, err := r.EnsureSnapshot(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.DB.SetSnapshotLGTM(snaps[0].ID, true); err != nil {
		t.Fatal(err)
	}

	write(t, dir, "a.go", "package p\n\nvar X int\n")
	do(t, dir, "git", "commit", "-q", "-a", "--amend", "--no-edit")
	if c, err = r.Change("Ilgtm"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Grab(c); err != nil {
		t.Fatal(err)
	}

	snaps, err = r.DB.Snapshots(r.Root(), "Ilgtm")
	if err != nil {
		t.Fatal(err)
	}
	if on, err := r.DB.SnapshotLGTM(snaps[1].ID); err != nil {
		t.Fatal(err)
	} else if on {
		t.Error("an amended snapshot inherited a sign-off it never earned")
	}
}

// TestCarryReviewedAcrossRebasedMessage is the case that kept a commit
// message asking to be read again: a rebase moves the line naming the
// parent, and nothing else about the message changes.
func TestCarryReviewedAcrossRebasedMessage(t *testing.T) {
	r, _ := newStackedReview(t)
	top, err := r.Change(topChangeKey)
	if err != nil {
		t.Fatal(err)
	}
	snaps, err := r.DB.Snapshots(r.Root(), topChangeKey)
	if err != nil {
		t.Fatal(err)
	}
	latest := snaps[len(snaps)-1]
	if err := r.DB.SetReviewed(latest.ID, CommitMsgFile, true); err != nil {
		t.Fatal(err)
	}

	// Rebase the change onto a rewritten commit below it, leaving its own
	// work and its message alone.
	dir := r.Root()
	topRev := top.Rev
	do(t, dir, "git", "reset", "-q", "--hard", "HEAD~1")
	write(t, dir, "deep.txt", "one\nTHREE\n")
	do(t, dir, "git", "commit", "-q", "-a", "--amend", "--no-edit")
	do(t, dir, "git", "cherry-pick", topRev)
	if top, err = r.Change(topChangeKey); err != nil {
		t.Fatal(err)
	}
	if _, created, err := r.Grab(top); err != nil || !created {
		t.Fatalf("grab = %v, %v; want a new snapshot", created, err)
	}

	snaps, err = r.DB.Snapshots(r.Root(), topChangeKey)
	if err != nil {
		t.Fatal(err)
	}
	next := snaps[len(snaps)-1]
	// The parent line moved, so the two renderings differ ...
	if string(commitMsgContent("git", latest.Change())) == string(commitMsgContent("git", next.Change())) {
		t.Fatal("the fixture's parent line did not move")
	}
	// ... but the message did not, so the mark carries.
	marks, err := r.DB.Reviewed(next.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !marks[CommitMsgFile] {
		t.Error("the commit message was left unreviewed after a rebase moved its parent line")
	}
}

// TestCarryReviewedStopsAtRewordedMessage checks the other half: rewording
// is a change to read, however the parent line moved.
func TestCarryReviewedStopsAtRewordedMessage(t *testing.T) {
	r, dir := newReview(t)
	write(t, dir, "a.go", "package p\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "before\n\nChange-Id: Iword\n")

	c, err := r.Change("Iword")
	if err != nil {
		t.Fatal(err)
	}
	snaps, err := r.EnsureSnapshot(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.DB.SetReviewed(snaps[0].ID, CommitMsgFile, true); err != nil {
		t.Fatal(err)
	}

	do(t, dir, "git", "commit", "-q", "--amend", "-m", "after, and rather different\n\nChange-Id: Iword\n")
	if c, err = r.Change("Iword"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Grab(c); err != nil {
		t.Fatal(err)
	}
	snaps, err = r.DB.Snapshots(r.Root(), "Iword")
	if err != nil {
		t.Fatal(err)
	}
	if marks, err := r.DB.Reviewed(snaps[1].ID); err != nil {
		t.Fatal(err)
	} else if marks[CommitMsgFile] {
		t.Error("a reworded message kept its reviewed mark")
	}
}

// TestMessageChangeIDIsNotAChange checks that a Change-Id trailer arriving
// in a commit message does not ask for the message to be read again. The
// tool that mails a change writes it there; the message says what it said
// before, and mailing a change must not undo the reading of it.
func TestMessageChangeIDIsNotAChange(t *testing.T) {
	before := &Change{Message: "subject\n\nThe body.\n", Parent: "abc", ParentKey: "Ibelow"}
	mailed := &Change{Message: "subject\n\nThe body.\n\nChange-Id: I123\n", Parent: "abc", ParentKey: "Ibelow"}
	if !sameMessage("git", before, mailed) {
		t.Errorf("a Change-Id arriving counted as a changed message:\n%s\n--- and ---\n%s",
			msgWithoutParents("git", before), msgWithoutParents("git", mailed))
	}
	// The other half: what the message says still counts.
	reworded := &Change{Message: "subject\n\nA different body.\n\nChange-Id: I123\n", Parent: "abc", ParentKey: "Ibelow"}
	if sameMessage("git", mailed, reworded) {
		t.Error("a reworded message did not count as changed")
	}
}

// TestMessageParentIsNotAChange checks that putting a change on a
// different parent does not ask for its message to be read again. Review
// writes the line that moved, from where the commit sits; what the author
// wrote says the same thing it did before.
func TestMessageParentIsNotAChange(t *testing.T) {
	msg := "subject\n\nThe body.\n"
	before := &Change{Message: msg, Parent: "aaa", ParentKey: "Ione"}
	moved := &Change{Message: msg, Parent: "bbb", ParentKey: "Itwo"}
	if !sameMessage("jj", before, moved) {
		t.Errorf("a different parent counted as a changed message:\n%s\n--- and ---\n%s",
			msgWithoutParents("jj", before), msgWithoutParents("jj", moved))
	}
}

// TestSpreadMarksSettlesFileMarks checks that the file marks are settled
// along the chain and not only when a snapshot is first recorded, so that
// grabbing repairs a chain recorded by an older binary.
func TestSpreadMarksSettlesFileMarks(t *testing.T) {
	r, dir := newReview(t)
	write(t, dir, "a.go", "package p\n")
	write(t, dir, "b.go", "package q\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "two files\n\nChange-Id: Isettle\n")

	c, err := r.Change("Isettle")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.EnsureSnapshot(c); err != nil {
		t.Fatal(err)
	}
	// A second snapshot touching only a.go.
	write(t, dir, "a.go", "package p\n\nvar X int\n")
	do(t, dir, "git", "commit", "-q", "-a", "--amend", "--no-edit")
	if c, err = r.Change("Isettle"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Grab(c); err != nil {
		t.Fatal(err)
	}
	snaps, err := r.DB.Snapshots(r.Root(), "Isettle")
	if err != nil {
		t.Fatal(err)
	}

	// Mark b.go in the older snapshot only, as if it had been marked after
	// the newer one was recorded.
	if err := r.DB.SetReviewed(snaps[0].ID, "b.go", true); err != nil {
		t.Fatal(err)
	}
	if marks, _ := r.DB.Reviewed(snaps[1].ID); marks["b.go"] {
		t.Fatal("b.go is already marked in the newer snapshot")
	}

	if err := r.SpreadMarks("Isettle"); err != nil {
		t.Fatal(err)
	}
	marks, err := r.DB.Reviewed(snaps[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !marks["b.go"] {
		t.Error("an unchanged file's mark was not settled forward")
	}
	if marks["a.go"] {
		t.Error("a file that changed was marked reviewed")
	}
}

// TestSpreadMarksCarriesPastEditedFiles is the case the cheap test missed:
// the rebase and the change touch the same files, but the change's own
// edits to them are unchanged, so there is still nothing new to read.
func TestSpreadMarksCarriesPastEditedFiles(t *testing.T) {
	r, dir := newReview(t)
	body := func(first, last string) string {
		s := first + "\n"
		for i := 2; i <= 9; i++ {
			s += fmt.Sprintf("line %d\n", i)
		}
		return s + last + "\n"
	}
	write(t, dir, "shared.txt", body("top", "bottom"))
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "lower\n\nChange-Id: Ilower\n")

	// The upper change edits the last line of the same file.
	write(t, dir, "shared.txt", body("top", "bottom EDITED"))
	do(t, dir, "git", "commit", "-q", "-a", "-m", "upper\n\nChange-Id: Iupper\n")

	upper, err := r.Change("Iupper")
	if err != nil {
		t.Fatal(err)
	}
	snaps, err := r.EnsureSnapshot(upper)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.DB.SetSnapshotReviewed(snaps[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if err := r.DB.SetSnapshotLGTM(snaps[0].ID, true); err != nil {
		t.Fatal(err)
	}
	upperRev := upper.Rev

	// Rewrite the lower change's first line and replay the upper one, so
	// both of them have touched shared.txt.
	do(t, dir, "git", "reset", "-q", "--hard", "HEAD~1")
	write(t, dir, "shared.txt", body("top REWRITTEN", "bottom"))
	do(t, dir, "git", "commit", "-q", "-a", "--amend", "--no-edit")
	do(t, dir, "git", "cherry-pick", upperRev)

	if upper, err = r.Change("Iupper"); err != nil {
		t.Fatal(err)
	}
	if _, created, err := r.Grab(upper); err != nil || !created {
		t.Fatalf("grab = %v, %v; want a new snapshot", created, err)
	}
	snaps, err = r.DB.Snapshots(r.Root(), "Iupper")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{markReviewed, markLGTM} {
		if on, err := r.DB.SnapshotMark(snaps[1].ID, kind); err != nil {
			t.Fatal(err)
		} else if !on {
			t.Errorf("%s did not carry across a rebase of a file the change edits", kind)
		}
	}
}

// TestSpreadMarksRecordsOwnWork checks that the expensive verdict is only
// reached once: a snapshot found to hold work of its own is noted, and the
// note is what later runs read.
func TestSpreadMarksRecordsOwnWork(t *testing.T) {
	r, dir := newReview(t)
	write(t, dir, "a.go", "package p\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "one\n\nChange-Id: Inote\n")

	c, err := r.Change("Inote")
	if err != nil {
		t.Fatal(err)
	}
	snaps, err := r.EnsureSnapshot(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.DB.SetSnapshotReviewed(snaps[0].ID, true); err != nil {
		t.Fatal(err)
	}

	write(t, dir, "a.go", "package p\n\nvar X int\n")
	do(t, dir, "git", "commit", "-q", "-a", "--amend", "--no-edit")
	if c, err = r.Change("Inote"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Grab(c); err != nil {
		t.Fatal(err)
	}
	snaps, err = r.DB.Snapshots(r.Root(), "Inote")
	if err != nil {
		t.Fatal(err)
	}
	if on, err := r.DB.SnapshotMark(snaps[1].ID, markOwnWork); err != nil {
		t.Fatal(err)
	} else if !on {
		t.Error("the verdict was not recorded, so every grab would ask again")
	}
	if on, err := r.DB.SnapshotReviewed(snaps[1].ID); err != nil {
		t.Fatal(err)
	} else if on {
		t.Error("a snapshot with work of its own was marked reviewed")
	}
}
