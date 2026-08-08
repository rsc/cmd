// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
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
