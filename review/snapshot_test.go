// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
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
	v, err := r.View(c, "", "")
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
	v, err := r.View(c, "1", "2")
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
	if _, err := r.View(c, "2", "2"); err == nil {
		t.Error("View accepted a base equal to the target")
	}
	if _, err := r.View(c, "5", ""); err == nil {
		t.Error("View accepted a nonexistent snapshot")
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

	v, err := r.View(c, "", "") // parent against snapshot 2
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
	v1, err := r.View(c, "", "1")
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

	v, _ := r.View(changes[0], "", "")
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
	v, err := r.View(changes[0], "1", "2")
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
	v, err := r.View(top, "1", "2")
	if err != nil {
		t.Fatal(err)
	}

	// deep.txt is nowhere in the upper change's own work, so everything
	// its diff shows was done below it.
	only, err := r.RebaseOnly(v)
	if err != nil {
		t.Fatal(err)
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
	r.MarkInherited(v, f, rows)
	got := rebasedLines(rows)
	want := []string{"-b", "+B2"}
	if !slices.Equal(got, want) {
		t.Errorf("rebased = %q, want %q\n%s", got, want, rowsString(rows))
	}

	// Against the parent commit the whole diff is the change's own work,
	// however much of it a rebase carried along.
	pv, err := r.View(top, "parent", "2")
	if err != nil {
		t.Fatal(err)
	}
	if only, err := r.RebaseOnly(pv); err != nil {
		t.Fatal(err)
	} else if len(only) != 0 {
		t.Errorf("parent view reports rebase-only files %v", only)
	}
	pf := pv.File("shared.txt")
	old, new, err = r.Contents(pv, pf)
	if err != nil {
		t.Fatal(err)
	}
	rows = Diff(old, new).Rows
	r.MarkInherited(pv, pf, rows)
	if AnyRebased(rows) {
		t.Errorf("parent view marked rows inherited:\n%s", rowsString(rows))
	}
}
