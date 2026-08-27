// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
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
	// A change with no stable identity of its own — a git commit with no
	// Change-Id trailer, still keyed by its own hash — gets one now,
	// before it is recorded under a key an amend is about to invalidate.
	if !c.Working && c.Key == c.Rev {
		key, err := r.Repo.EnsureStableKey(c.Rev)
		if err != nil {
			return nil, false, fmt.Errorf("recording a stable key: %v", err)
		}
		c.Key = key
	}
	s, created, err = r.DB.AddSnapshot(r.Root(), c)
	if err != nil {
		return s, created, err
	}
	// Carry the marks along the whole chain: file by file for the files
	// that did not move, and snapshot by snapshot where a rebase brought
	// nothing of this change's own along. This runs whether or not a
	// snapshot was recorded, so that a mark put on after the fact settles
	// too, and grabbing is what a reviewer reaches for when the marks look
	// out of date.
	if err := r.SpreadMarks(c.Key); err != nil {
		return s, created, err
	}
	if !created || !r.Pin {
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
func (r *Review) carryReviewed(prev, next *Snapshot) error {
	marks, err := r.DB.Reviewed(prev.ID)
	if err != nil || len(marks) == 0 {
		return err
	}
	// Nothing to do if they are all carried already, which is the usual
	// answer once a chain has settled and is worth an early return: the
	// listing below costs a subprocess.
	have, err := r.DB.Reviewed(next.ID)
	if err != nil {
		return err
	}
	todo := false
	for file := range marks {
		if !have[file] {
			todo = true
			break
		}
	}
	if !todo {
		return nil
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
	// it as it is rendered — all but the lines naming the parent, which a
	// rebase moves without a word of the message changing.
	if !sameMessage(r.Repo.Kind(), prev.Change(), next.Change()) {
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

// sameMessage reports whether two commit messages differ only in the lines
// naming the parent commit. Those move with every rebase, whatever it was
// that moved, so they are no reason to read a message again; the stable
// name of the parent is compared on its own, since moving onto a different
// parent change is a real move.
//
// A snapshot recorded before parent identities were stored has none, and
// an unknown parent is not a different one: refusing there would leave
// every such snapshot asking for its message to be read again forever.
func sameMessage(kind string, prev, next *Change) bool {
	if msgWithoutParents(kind, prev) != msgWithoutParents(kind, next) {
		return false
	}
	if prev.ParentKey != "" && next.ParentKey != "" {
		return prev.ParentKey == next.ParentKey
	}
	return true
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
		// Not snapshotted; shown live as it stands right now. In git that
		// is the sentinel WorkingRev, read off disk; in jj it is the empty
		// commit's own hash, already valid to diff and read content from.
		v.TargetRev = c.Rev
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
	if err := r.addCommentedFiles(v, c); err != nil {
		return nil, err
	}
	return v, nil
}

// addCommentedFiles appends the files that carry comments but do not
// appear in the diff. Comparing against the snapshot last marked reviewed
// narrows the file list to what has moved since, which is the point of
// doing it; but a comment written on a file that has not moved would then
// have nowhere to be listed, and a comment nobody can reach is a comment
// lost. Such a file has no status: the change did nothing to it.
func (r *Review) addCommentedFiles(v *View, c *Change) error {
	threads, err := r.DB.Threads(r.Root(), c.Key)
	if err != nil || len(threads) == 0 {
		return err
	}
	seen := map[string]bool{}
	for _, f := range v.Files {
		seen[f.Path] = true
	}
	var add []string
	for _, t := range threads {
		if !seen[t.File] {
			seen[t.File] = true
			add = append(add, t.File)
		}
	}
	slices.Sort(add)
	for _, path := range add {
		// Only what is still there. A comment on a file deleted long ago
		// has nothing left to be shown beside; it keeps its place in the
		// change's comment history instead.
		if _, err := r.Repo.Content(v.TargetRev, path); err != nil {
			continue
		}
		v.Files = append(v.Files, &File{Path: path})
	}
	return nil
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

// MarkSnapshotFiles marks, or unmarks, every file a snapshot changes,
// which is what marking the snapshot itself reviewed is a statement
// about. Leaving the files unmarked would make the snapshot's own mark
// disagree with every line beneath it.
//
// The files are those the snapshot changes against its parent commit,
// which is the whole of the change as it stood — not what has happened
// since some earlier snapshot, since it is the whole that was read. The
// commit message is one of them, as it is everywhere else.
//
// Unmarking clears them again. A mark that could be set as a group but
// only taken back one file at a time would be a trap, and the button is
// the indicator: it has to be able to tell the truth in both positions.
func (r *Review) MarkSnapshotFiles(snap *Snapshot, on bool) error {
	files, err := r.Repo.Files(snap.Parent, snap.Rev)
	if err != nil {
		// The commit may be gone if snapshots were never pinned. The
		// snapshot's own mark still stands; only the files are missed.
		return nil
	}
	if err := r.DB.SetReviewed(snap.ID, CommitMsgFile, on); err != nil {
		return err
	}
	for _, f := range files {
		if err := r.DB.SetReviewed(snap.ID, f.Path, on); err != nil {
			return err
		}
	}
	return nil
}

// SpreadMarks carries a snapshot's marks forward to the ones after it that
// hold nothing of the change's own work: a rebase moved the change, and
// there is nothing new to read or to think again about. Editing a commit
// low in a stack would otherwise strip the marks from every commit above
// it, not one of which changed anything.
//
// Both marks travel. Reviewed says the snapshot was read, and it was read;
// LGTM says it looked good, and what looked good is still there. Carrying
// one without the other would leave a change reviewed but no longer signed
// off for no reason anyone could point at.
//
// It walks the whole chain, so marking an old snapshot reaches the newest
// one through however many rebases lie between, and running it twice
// changes nothing.
func (r *Review) SpreadMarks(key string) error {
	snaps, err := r.DB.Snapshots(r.Root(), key)
	if err != nil {
		return err
	}
	for i := 1; i < len(snaps); i++ {
		prev, next := snaps[i-1], snaps[i]
		// Files first: a file that did not move keeps its mark whether or
		// not anything else in the snapshot did.
		if err := r.carryReviewed(prev, next); err != nil {
			return err
		}
		var carry []string
		for _, kind := range []string{markReviewed, markLGTM} {
			was, err := r.DB.SnapshotMark(prev.ID, kind)
			if err != nil {
				return err
			}
			done, err := r.DB.SnapshotMark(next.ID, kind)
			if err != nil {
				return err
			}
			if was && !done {
				carry = append(carry, kind)
			}
		}
		if len(carry) == 0 {
			continue
		}
		// Asked and answered: neither snapshot can move, so a snapshot
		// already known to hold work of its own still does.
		own, err := r.DB.SnapshotMark(next.ID, markOwnWork)
		if err != nil {
			return err
		}
		if own {
			continue
		}
		ok, err := r.onlyInherited(prev, next)
		if errors.Is(err, errNoAnswer) {
			// The repository would not say; ask again another time rather
			// than writing down something that may not be true.
			continue
		}
		if err != nil {
			return err
		}
		if !ok {
			if err := r.DB.SetSnapshotMark(next.ID, markOwnWork, true); err != nil {
				return err
			}
			continue
		}
		for _, kind := range carry {
			if err := r.DB.SetSnapshotMark(next.ID, kind, true); err != nil {
				return err
			}
			// An LGTM says the snapshot was read, whatever the older data
			// says, so it brings the reviewed mark with it.
			if kind == markLGTM {
				if err := r.DB.SetSnapshotReviewed(next.ID, true); err != nil {
					return err
				}
			}
		}
		if err := r.MarkSnapshotFiles(next, true); err != nil {
			return err
		}
	}
	return nil
}

// errNoAnswer reports that the repository would not say — a commit gone,
// a listing that failed — so the verdict is not one to write down.
var errNoAnswer = errors.New("cannot tell what changed")

// onlyInherited reports whether everything that differs between two
// snapshots was done by some other commit. A file counts as the change's
// own if the change touches it in either snapshot, which is the same test
// the file list uses to call a file rebase-only, so what the reader is
// told and what this concludes cannot drift apart.
//
// Not being able to tell is not the same as yes: whenever the repository
// will not answer, the answer here is no, which asks for another look
// rather than skipping one.
func (r *Review) onlyInherited(prev, next *Snapshot) (bool, error) {
	changed, err := r.Repo.Files(prev.Rev, next.Rev)
	if err != nil {
		return false, errNoAnswer
	}
	if len(changed) > 0 {
		own, err := r.pathSet(prev.Parent, prev.Rev, next.Parent, next.Rev)
		if err != nil {
			return false, errNoAnswer
		}
		// What the rebase itself touched. A file the change edits that the
		// rebase left alone cannot be showing anything but the change's own
		// work, which settles the common case — an ordinary amend — without
		// reading a single file.
		moved, err := r.pathSet(prev.Parent, next.Parent)
		if err != nil {
			return false, errNoAnswer
		}
		// Reading the two snapshots as a diff lets a file the change edits
		// be asked the same question the file list asks it: is every changed
		// line in it something a commit below put there? A change that edits
		// a file and a rebase that sweeps the same file are not the same
		// thing, and only the second one leaves nothing to read.
		v := &View{Base: prev, Target: next, BaseRev: prev.Rev, TargetRev: next.Rev}
		for _, f := range changed {
			if !own[f.Path] && (f.OldPath == "" || !own[f.OldPath]) {
				continue // the change leaves this file alone
			}
			if !moved[f.Path] && (f.OldPath == "" || !moved[f.OldPath]) {
				return false, nil
			}
			if !r.allInherited(v, f) {
				return false, nil
			}
		}
	}
	// The commit message is not one of the repository's files, and a rebase
	// moves the line naming the parent without a word of it changing.
	return sameMessage(r.Repo.Kind(), prev.Change(), next.Change()), nil
}

// msgWithoutParents renders a commit message file without either of the
// lines naming its parent. The commit ID moves with every rebase, and the
// stable name moves only when the change really has been put somewhere
// else — which is worth knowing, and is why the caller asks about it
// separately rather than by reading these lines.
func msgWithoutParents(kind string, c *Change) string {
	lines := strings.Split(string(commitMsgContent(kind, c)), "\n")
	out := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(l, "Git Parent:") || strings.HasPrefix(l, "Parent:") ||
			strings.HasPrefix(l, "JJ Parent:") || strings.HasPrefix(l, "Change Parent:") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// MarkInherited marks the rows of a file's diff whose edits the change did
// not make: they came along when it was rebased onto a rewritten parent.
func (r *Review) MarkInherited(v *View, f *File, old, new []byte, rows []Row) {
	if oldParent, newParent, ok := r.inherited(v, f); ok {
		MarkRebased(rows, old, new, oldParent, newParent)
	}
}

// inherited returns the contents of one file in the parent commits of the
// view's two sides. Diffing those two shows everything the change's own
// diff contains but did not put there: edit a commit low in a stack and
// every commit above it moves onto the new version, so the edit appears in
// each of their snapshot-to-snapshot diffs too.
//
// It reports ok=false when the question does not arise: when the left side
// is the target's own parent, when both sides sit on the same parent, or
// for the commit message, which belongs to this change alone.
func (r *Review) inherited(v *View, f *File) (old, new []byte, ok bool) {
	if v.Base == nil || v.Target == nil || v.Base.Parent == v.Target.Parent {
		return nil, nil, false
	}
	if f.Path == CommitMsgFile {
		return nil, nil, false
	}
	// A file missing from a parent commit reads as empty, which is what it
	// means here: an edit that creates a file is an edit like any other.
	old, _ = r.Repo.Content(v.Base.Parent, f.Old())
	new, _ = r.Repo.Content(v.Target.Parent, f.Path)
	return old, new, true
}

// A FileStat is what the file list says about one file beyond its name:
// how many lines the diff it opens adds and deletes, and whether that
// leaves nothing at all.
//
// Lines a rebase brought are left out of the counts. They are not this
// change's work, and counting them would put a four-figure number beside
// a file the change never touched.
type FileStat struct {
	Added      int
	Deleted    int
	RebaseOnly bool
}

// FileStats measures every file in a view.
//
// A file the change touches in neither snapshot needs no measuring: every
// line in it came from below, so the counts are zero and the file is
// rebase-only. The rest are diffed, and the ones the rebase touched as
// well have their inherited lines marked first so they can be left out.
func (r *Review) FileStats(v *View) (map[string]FileStat, error) {
	var own, moved map[string]bool
	rebased := v.Base != nil && v.Target != nil && v.Base.Parent != v.Target.Parent
	if rebased {
		var err error
		if own, err = r.pathSet(v.Base.Parent, v.Base.Rev, v.Target.Parent, v.Target.Rev); err != nil {
			return nil, err
		}
		if moved, err = r.pathSet(v.Base.Parent, v.Target.Parent); err != nil {
			return nil, err
		}
	}

	out := make(map[string]FileStat, len(v.Files))
	for _, f := range v.Files {
		// A file with no status is not in the diff at all; it is listed for
		// its comments, and has nothing to count.
		if f.Status == 0 {
			continue
		}
		// The commit message belongs to this change alone; no rebase can
		// have brought it. It is measured like any other file below, where
		// nothing marks it inherited, so it never comes out rebase-only.
		touches := !rebased || f.Path == CommitMsgFile ||
			own[f.Path] || (f.OldPath != "" && own[f.OldPath])
		if !touches {
			out[f.Path] = FileStat{RebaseOnly: true}
			continue
		}
		old, new, err := r.Contents(v, f)
		if err != nil {
			continue
		}
		rows := Diff(old, new).Rows
		if rebased && (moved[f.Path] || (f.OldPath != "" && moved[f.OldPath])) {
			r.MarkInherited(v, f, old, new, rows)
		}
		st := FileStat{RebaseOnly: AllRebased(rows)}
		for _, row := range rows {
			if row.Kind == RowEqual || row.Kind == RowSkip {
				continue
			}
			if row.L.Num > 0 && !row.RebasedL {
				st.Deleted++
			}
			if row.R.Num > 0 && !row.RebasedR {
				st.Added++
			}
		}
		out[f.Path] = st
	}
	return out, nil
}

// pathSet returns every path touched between the given pairs of revisions.
func (r *Review) pathSet(revs ...string) (map[string]bool, error) {
	set := map[string]bool{}
	for i := 0; i+1 < len(revs); i += 2 {
		files, err := r.Repo.Files(revs[i], revs[i+1])
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			set[f.Path] = true
			if f.OldPath != "" {
				set[f.OldPath] = true
			}
		}
	}
	return set, nil
}

// allInherited reports whether every changed line of a file's diff came
// along with a rebase. A file with nothing changed in it is not inherited
// but unchanged, and says so by answering false.
func (r *Review) allInherited(v *View, f *File) bool {
	old, new, err := r.Contents(v, f)
	if err != nil {
		return false
	}
	rows := Diff(old, new).Rows
	r.MarkInherited(v, f, old, new, rows)
	return AllRebased(rows)
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
