// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"strconv"
)

// A Review pairs a repository with the database holding its comments.
type Review struct {
	Repo Repo
	DB   *DB
	Pin  bool   // record a git ref for each snapshot, so it survives gc
	Name string // the short name this repository has in URLs
}

// Root is the repository root, which keys the database.
func (r *Review) Root() string { return r.Repo.Root() }

// Change returns the change with the given key, which may be a change key,
// a commit ID, or a unique prefix of either.
func (r *Review) Change(key string) (*Change, error) {
	changes, err := r.Repo.Changes()
	if err != nil {
		return nil, err
	}
	var match []*Change
	for _, c := range changes {
		switch {
		case c.Key == key, c.Rev == key:
			return c, nil
		case hasPrefix(c.Key, key), hasPrefix(c.Rev, key):
			match = append(match, c)
		}
	}
	switch len(match) {
	case 0:
		return nil, fmt.Errorf("no pending change matching %q", key)
	case 1:
		return match[0], nil
	}
	return nil, fmt.Errorf("%q matches %d changes", key, len(match))
}

func hasPrefix(s, prefix string) bool {
	return len(prefix) > 0 && len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// EnsureSnapshot returns the snapshots of a change, recording snapshot 1
// if the change has never been snapshotted. A change always has at least
// one snapshot, so that there is something for comments to attach to.
func (r *Review) EnsureSnapshot(c *Change) ([]*Snapshot, error) {
	snaps, err := r.DB.Snapshots(r.Root(), c.Key)
	if err != nil {
		return nil, err
	}
	if len(snaps) > 0 || c.Working {
		return snaps, nil
	}
	s, _, err := r.Grab(c)
	if err != nil {
		return nil, err
	}
	return []*Snapshot{s}, nil
}

// Grab records the current state of a change as a snapshot. If the change
// has not moved since the last snapshot, it reports created=false and
// returns that snapshot unchanged, so that grabbing twice is harmless.
func (r *Review) Grab(c *Change) (s *Snapshot, created bool, err error) {
	before, err := r.DB.Snapshots(r.Root(), c.Key)
	if err != nil {
		return nil, false, err
	}
	s, created, err = r.DB.AddSnapshot(r.Root(), c)
	if err != nil || !created {
		return s, created, err
	}
	if n := len(before); n > 0 {
		if err := r.carryReviewed(before[n-1], s, c); err != nil {
			return s, created, err
		}
	}
	if !r.Pin {
		return s, created, nil
	}
	// Pin the commit so that amending the change away does not let the
	// snapshot's commit be garbage collected out from under the comments.
	if err := r.Repo.Pin(fmt.Sprintf("%s/%d", refName(c.Key), s.N), s.Rev); err != nil {
		return s, created, fmt.Errorf("recording snapshot %d: %v", s.N, err)
	}
	return s, created, nil
}

// carryReviewed marks in the new snapshot every file that was marked in
// the one before it and has not changed since. A new snapshot usually
// touches one or two files, and asking for the whole change to be read
// again because of that would make the marks worthless.
//
// Only files whose contents are identical carry over: a file that moved
// is a file to look at again, and the marks on the snapshot itself never
// carry, since they are a verdict on a state that no longer exists.
func (r *Review) carryReviewed(prev, next *Snapshot, c *Change) error {
	marks, err := r.DB.Reviewed(prev.ID)
	if err != nil || len(marks) == 0 {
		return err
	}

	changed, err := r.Repo.Files(prev.Rev, next.Rev)
	if err != nil {
		// Without knowing what changed, carrying nothing is the safe
		// answer: it asks for another look rather than skipping one.
		return nil
	}
	moved := map[string]bool{}
	for _, f := range changed {
		moved[f.Path] = true
		if f.OldPath != "" {
			moved[f.OldPath] = true
		}
	}
	// The commit message is not one of the repository's files, so compare
	// it as it is rendered, which takes in the parent and author too.
	if !bytes.Equal(commitMsgContent(prev.Change()), commitMsgContent(c)) {
		moved[CommitMsgFile] = true
	}

	for file := range marks {
		if moved[file] {
			continue
		}
		if err := r.DB.SetReviewed(next.ID, file, true); err != nil {
			return err
		}
	}
	return nil
}

// A View is a diff of one change between two revisions: a target snapshot
// on the right, and on the left either an earlier snapshot or the target's
// parent commit.
type View struct {
	Change    *Change
	Snapshots []*Snapshot
	Target    *Snapshot // right side; nil for the uncommitted working tree
	Base      *Snapshot // left side, or nil when comparing against the parent
	TargetRev string
	BaseRev   string
	Files     []*File

	// AutoBase reports that Base was chosen because it is the last
	// snapshot marked reviewed, rather than asked for in the URL.
	AutoBase bool
}

// BaseName describes the left side for display.
func (v *View) BaseName() string {
	if v.Base == nil {
		return "Parent"
	}
	return fmt.Sprintf("Snapshot %d", v.Base.N)
}

// TargetName describes the right side for display.
func (v *View) TargetName() string {
	if v.Target == nil {
		return "Working tree"
	}
	return fmt.Sprintf("Snapshot %d", v.Target.N)
}

// View builds the diff view of a change. base selects the left side:
// "" or "parent" means the target's parent commit, and a number selects
// that snapshot. target selects the right side: "" means the newest
// snapshot, and a number selects that one.
func (r *Review) View(c *Change, base, target string) (*View, error) {
	snaps, err := r.EnsureSnapshot(c)
	if err != nil {
		return nil, err
	}
	v := &View{Change: c, Snapshots: snaps}

	switch {
	case c.Working:
		// The working tree is not snapshotted; it is always itself.
		v.TargetRev = WorkingRev
		v.BaseRev = c.Parent
	default:
		if len(snaps) == 0 {
			return nil, fmt.Errorf("change %s has no snapshots", c.Key)
		}
		v.Target = snaps[len(snaps)-1]
		if target != "" {
			s, err := findSnapshot(snaps, target)
			if err != nil {
				return nil, err
			}
			v.Target = s
		}
		v.TargetRev = v.Target.Rev
		v.BaseRev = v.Target.Parent
		switch base {
		case "":
			// No base was asked for. Default to the newest snapshot that
			// has already been reviewed, so that opening a file shows only
			// what has changed since it was last looked at. With nothing
			// reviewed yet, this falls through to the parent commit and
			// the whole change is shown.
			prev, err := r.DB.LastReviewedSnapshot(r.Root(), c.Key, v.Target.N)
			if err != nil {
				return nil, err
			}
			if prev != nil {
				v.Base = prev
				v.BaseRev = prev.Rev
				v.AutoBase = true
			}
		case "parent":
			// Explicitly the parent commit: leave the base unset.
		default:
			s, err := findSnapshot(snaps, base)
			if err != nil {
				return nil, err
			}
			if s.N >= v.Target.N {
				return nil, fmt.Errorf("base snapshot %d is not older than snapshot %d", s.N, v.Target.N)
			}
			v.Base = s
			v.BaseRev = s.Rev
		}
	}

	files, err := r.Repo.Files(v.BaseRev, v.TargetRev)
	if err != nil {
		return nil, err
	}
	// The commit message is reviewed like any other file, and comes first.
	v.Files = append([]*File{{Path: CommitMsgFile, Status: 'M'}}, files...)
	return v, nil
}

func findSnapshot(snaps []*Snapshot, s string) (*Snapshot, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil, fmt.Errorf("invalid snapshot %q", s)
	}
	for _, snap := range snaps {
		if snap.N == n {
			return snap, nil
		}
	}
	return nil, fmt.Errorf("no snapshot %d", n)
}

// BaseN returns the base snapshot number, or 0 for the parent commit.
func (v *View) BaseN() int {
	if v.Base == nil {
		return 0
	}
	return v.Base.N
}

// TargetN returns the target snapshot number, or 0 for the working tree.
func (v *View) TargetN() int {
	if v.Target == nil {
		return 0
	}
	return v.Target.N
}

// LatestN returns the number of the newest snapshot.
func (v *View) LatestN() int {
	if len(v.Snapshots) == 0 {
		return 0
	}
	return v.Snapshots[len(v.Snapshots)-1].N
}

// CanComment reports whether comments can be written against this view.
// The uncommitted working tree has no snapshot for them to attach to.
func (v *View) CanComment() bool { return v.Target != nil }

// ThreadTarget reports which snapshot a new comment on the given side of
// this view belongs to, and the side to record it under. It is the inverse
// of PlaceThreads: a comment on the left of a snapshot-to-snapshot diff is
// a comment on the older snapshot's own content.
func (v *View) ThreadTarget(side string) (snapshotID int64, storedSide string, err error) {
	if v.Target == nil {
		return 0, "", fmt.Errorf("cannot comment on uncommitted changes: commit them first")
	}
	switch side {
	case "new":
		return v.Target.ID, "new", nil
	case "old":
		if v.Base != nil {
			return v.Base.ID, "new", nil
		}
		return v.Target.ID, "old", nil
	}
	return 0, "", fmt.Errorf("invalid side %q", side)
}

// File returns the file with the given path in the view.
func (v *View) File(path string) *File {
	for _, f := range v.Files {
		if f.Path == path {
			return f
		}
	}
	return nil
}

// Contents returns the old and new contents of one file in the view.
func (r *Review) Contents(v *View, f *File) (old, new []byte, err error) {
	// The parent commit's message is not an earlier draft of this commit's
	// message; it belongs to a different change entirely. So against the
	// parent the commit message reads as a wholly new file, as it does in
	// Gerrit. Against an earlier snapshot it diffs normally, which is how
	// a reworded message shows up.
	blankBase := f.Path == CommitMsgFile && v.Base == nil

	if f.Status != 'A' && !blankBase {
		old, err = FileContent(r.Repo, v.BaseRev, f.Old())
		if err != nil {
			return nil, nil, err
		}
	}
	if f.Status != 'D' {
		new, err = FileContent(r.Repo, v.TargetRev, f.Path)
		if err != nil {
			return nil, nil, err
		}
	}
	return old, new, nil
}

// PlaceThreads decides where each of a change's comment threads should be
// drawn in one file of a view. Threads written against the snapshots being
// displayed appear at their own line; threads from other snapshots are
// re-anchored onto the target by searching for the text they were attached
// to, and are marked stale if that text is gone.
//
// A left-side comment in a snapshot-to-snapshot diff is a comment on the
// older snapshot's own content, which is why it is found by matching the
// base snapshot with side "new".
func (v *View) PlaceThreads(all []*Thread, file string, oldLines, newLines []string) (left, right []*Thread) {
	for _, t := range all {
		if t.File != file {
			continue
		}
		t.Other = false
		t.Stale = false

		switch {
		case v.Target != nil && t.SnapshotID == v.Target.ID && t.Side == "new":
			t.ShowLine, t.Stale = Anchor(t, newLines)
			right = append(right, t)

		case v.Base != nil && t.SnapshotID == v.Base.ID && t.Side == "new":
			t.ShowLine, t.Stale = Anchor(t, oldLines)
			left = append(left, t)

		case v.Base == nil && v.Target != nil && t.SnapshotID == v.Target.ID && t.Side == "old":
			t.ShowLine, t.Stale = Anchor(t, oldLines)
			left = append(left, t)

		default:
			// A thread from some other snapshot of this change. Show it
			// against the new side, where the reviewer is looking.
			t.Other = true
			t.ShowLine, t.Stale = Anchor(t, newLines)
			right = append(right, t)
		}
	}
	return left, right
}

// ThreadsByLine groups threads by the line they are drawn at. Stale
// threads, whose anchor text is gone, are grouped at line 0 so that the
// caller can list them at the top of the file rather than lose them.
func ThreadsByLine(threads []*Thread) map[int][]*Thread {
	m := map[int][]*Thread{}
	for _, t := range threads {
		line := t.ShowLine
		if t.Stale {
			line = 0
		}
		m[line] = append(m[line], t)
	}
	return m
}
