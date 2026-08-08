// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newDB(t *testing.T) *DB {
	t.Helper()
	d, err := OpenDB(filepath.Join(t.TempDir(), "review.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func testChange(key, rev, subject string) *Change {
	return &Change{
		Key: key, Rev: rev, Parent: "parent" + rev,
		Subject: subject, Message: subject + "\n",
		Author: "Tester <tester@example.com>", Date: time.Unix(1000, 0),
	}
}

// TestOpenDBRelativePath checks that -db accepts an ordinary relative
// path. The database name becomes a file: URI, where a relative path
// would otherwise be read as having a URI authority.
func TestOpenDBRelativePath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(dir, "sub"))

	d, err := OpenDB("../review.db")
	if err != nil {
		t.Fatalf("OpenDB with a relative path: %v", err)
	}
	defer d.Close()
	if _, _, err := d.AddSnapshot("/repo", testChange("k1", "r1", "one")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "review.db")); err != nil {
		t.Errorf("database not created where asked: %v", err)
	}
}

func TestOpenDBTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review.db")
	d, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.AddSnapshot("/repo", testChange("k1", "r1", "one")); err != nil {
		t.Fatal(err)
	}
	d.Close()

	// Reopening must find the existing schema and data, not recreate it.
	d2, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	snaps, err := d2.Snapshots("/repo", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].Rev != "r1" {
		t.Fatalf("snapshots = %+v, want one at r1", snaps)
	}
}

func TestSnapshotNumbering(t *testing.T) {
	d := newDB(t)

	s1, created, err := d.AddSnapshot("/repo", testChange("k1", "r1", "one"))
	if err != nil || !created || s1.N != 1 {
		t.Fatalf("first snapshot = %+v, created=%v, err=%v", s1, created, err)
	}

	// Grabbing again with the change unmoved is a no-op.
	again, created, err := d.AddSnapshot("/repo", testChange("k1", "r1", "one"))
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("re-grabbing an unchanged change created a second snapshot")
	}
	if again.N != 1 || again.ID != s1.ID {
		t.Errorf("re-grab returned %+v, want snapshot 1", again)
	}

	// After the change moves, a new snapshot is recorded.
	s2, created, err := d.AddSnapshot("/repo", testChange("k1", "r2", "one, amended"))
	if err != nil || !created || s2.N != 2 {
		t.Fatalf("second snapshot = %+v, created=%v, err=%v", s2, created, err)
	}

	// Numbering is per change, so a different change starts again at 1.
	other, _, err := d.AddSnapshot("/repo", testChange("k2", "r9", "other"))
	if err != nil || other.N != 1 {
		t.Fatalf("other change snapshot = %+v, err=%v", other, err)
	}

	// And the same key in a different repository is a different change.
	elsewhere, _, err := d.AddSnapshot("/other-repo", testChange("k1", "r1", "one"))
	if err != nil || elsewhere.N != 1 {
		t.Fatalf("other repo snapshot = %+v, err=%v", elsewhere, err)
	}

	snaps, err := d.Snapshots("/repo", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 || snaps[0].N != 1 || snaps[1].N != 2 {
		t.Fatalf("snapshots = %+v, want 1 then 2", snaps)
	}
}

func TestSnapshotOfWorkingTreeFails(t *testing.T) {
	d := newDB(t)
	c := testChange(WorkingRev, WorkingRev, "uncommitted")
	c.Working = true
	if _, _, err := d.AddSnapshot("/repo", c); err == nil {
		t.Error("snapshotting the working tree succeeded, want an error")
	}
}

func TestThreadsAndComments(t *testing.T) {
	d := newDB(t)
	s, _, err := d.AddSnapshot("/repo", testChange("k1", "r1", "one"))
	if err != nil {
		t.Fatal(err)
	}

	th, err := d.AddThread(s.ID, "a.go", "new", 42, "\tx := 1", &Comment{
		Author: "rsc", Body: "why 1?", Draft: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if th.SnapshotN != 1 || th.Line != 42 || th.Resolved {
		t.Errorf("thread = %+v", th)
	}
	if len(th.Comments) != 1 || th.Comments[0].Body != "why 1?" {
		t.Fatalf("comments = %+v", th.Comments)
	}

	// An agent reply is published immediately and marked as coming from an agent.
	if _, err := d.AddComment(th.ID, &Comment{Author: "agent", Body: "It is a count.", FromAgent: true}); err != nil {
		t.Fatal(err)
	}
	got, err := d.Thread(th.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Comments) != 2 {
		t.Fatalf("got %d comments, want 2", len(got.Comments))
	}
	if !got.Comments[1].FromAgent || got.Comments[1].Draft {
		t.Errorf("agent reply = %+v, want FromAgent and not Draft", got.Comments[1])
	}

	// Empty comments are rejected rather than stored.
	if _, err := d.AddComment(th.ID, &Comment{Author: "rsc", Body: "  \n "}); err == nil {
		t.Error("empty comment was accepted")
	}

	if err := d.SetResolved(th.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, _ = d.Thread(th.ID); !got.Resolved {
		t.Error("thread not resolved")
	}
	if err := d.SetResolved(9999, true); err == nil {
		t.Error("resolving a nonexistent thread succeeded")
	}

	if _, err := d.AddThread(s.ID, "a.go", "sideways", 1, "", &Comment{Author: "rsc", Body: "x"}); err == nil {
		t.Error("invalid side was accepted")
	}
}

func TestPublish(t *testing.T) {
	d := newDB(t)
	s, _, _ := d.AddSnapshot("/repo", testChange("k1", "r1", "one"))
	th, err := d.AddThread(s.ID, "a.go", "new", 1, "x", &Comment{Author: "rsc", Body: "draft one", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddComment(th.ID, &Comment{Author: "rsc", Body: "draft two", Draft: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddComment(th.ID, &Comment{Author: "agent", Body: "already public", FromAgent: true}); err != nil {
		t.Fatal(err)
	}

	n, err := d.Publish("/repo", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("published %d comments, want 2", n)
	}
	got, _ := d.Thread(th.ID)
	for _, c := range got.Comments {
		if c.Draft {
			t.Errorf("comment %q still a draft", c.Body)
		}
	}

	// Publishing again publishes nothing.
	if n, _ := d.Publish("/repo", "k1"); n != 0 {
		t.Errorf("second publish reported %d comments, want 0", n)
	}
}

func TestThreadsScope(t *testing.T) {
	d := newDB(t)
	s1, _, _ := d.AddSnapshot("/repo", testChange("k1", "r1", "one"))
	s2, _, _ := d.AddSnapshot("/repo", testChange("k2", "r2", "two"))
	d.AddThread(s1.ID, "a.go", "new", 1, "a", &Comment{Author: "rsc", Body: "on k1"})
	d.AddThread(s2.ID, "b.go", "new", 1, "b", &Comment{Author: "rsc", Body: "on k2"})

	ts, err := d.Threads("/repo", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 || ts[0].Comments[0].Body != "on k1" {
		t.Fatalf("Threads(k1) = %+v", ts)
	}

	all, err := d.AllThreads("/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("AllThreads = %d threads, want 2", len(all))
	}

	// Threads follow the change across snapshots.
	s1b, _, _ := d.AddSnapshot("/repo", testChange("k1", "r1b", "one, amended"))
	d.AddThread(s1b.ID, "a.go", "new", 5, "a5", &Comment{Author: "rsc", Body: "on snapshot 2"})
	ts, _ = d.Threads("/repo", "k1")
	if len(ts) != 2 {
		t.Fatalf("after amend, Threads(k1) = %d, want 2", len(ts))
	}
	var ns []int
	for _, th := range ts {
		ns = append(ns, th.SnapshotN)
	}
	if !(ns[0] == 1 && ns[1] == 2 || ns[0] == 2 && ns[1] == 1) {
		t.Errorf("snapshot numbers = %v, want one thread on each", ns)
	}
}

func TestReviewedAndPrefs(t *testing.T) {
	d := newDB(t)
	s, _, _ := d.AddSnapshot("/repo", testChange("k1", "r1", "one"))

	if err := d.SetReviewed(s.ID, "a.go", true); err != nil {
		t.Fatal(err)
	}
	// Marking twice must not fail.
	if err := d.SetReviewed(s.ID, "a.go", true); err != nil {
		t.Fatal(err)
	}
	got, err := d.Reviewed(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got["a.go"] || len(got) != 1 {
		t.Errorf("Reviewed = %v, want a.go only", got)
	}
	if err := d.SetReviewed(s.ID, "a.go", false); err != nil {
		t.Fatal(err)
	}
	if got, _ = d.Reviewed(s.ID); len(got) != 0 {
		t.Errorf("Reviewed = %v, want empty", got)
	}

	if v := d.Pref("context", "10"); v != "10" {
		t.Errorf("Pref default = %q, want 10", v)
	}
	if err := d.SetPref("context", "25"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetPref("context", "3"); err != nil {
		t.Fatal(err)
	}
	if v := d.Pref("context", "10"); v != "3" {
		t.Errorf("Pref = %q, want 3", v)
	}
}

func TestAnchor(t *testing.T) {
	lines := []string{"one", "two", "three", "four", "five"}
	tests := []struct {
		name      string
		thread    *Thread
		wantLine  int
		wantStale bool
	}{
		{"exact", &Thread{Line: 3, AnchorText: "three"}, 3, false},
		{"moved down", &Thread{Line: 1, AnchorText: "four"}, 4, false},
		{"moved up", &Thread{Line: 5, AnchorText: "one"}, 1, false},
		{"gone", &Thread{Line: 3, AnchorText: "missing"}, 0, true},
		{"file level", &Thread{Line: 0, AnchorText: ""}, 0, false},
		{"no anchor text, line exists", &Thread{Line: 2}, 2, false},
		{"no anchor text, line gone", &Thread{Line: 99}, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, stale := Anchor(tt.thread, lines)
			if line != tt.wantLine || stale != tt.wantStale {
				t.Errorf("Anchor = %d, %v; want %d, %v", line, stale, tt.wantLine, tt.wantStale)
			}
		})
	}
}

// TestAnchorPrefersNearest checks that when the anchor text appears more
// than once, the comment lands on the occurrence closest to where it was.
func TestAnchorPrefersNearest(t *testing.T) {
	lines := []string{"x", "dup", "y", "z", "dup", "w"}
	line, stale := Anchor(&Thread{Line: 4, AnchorText: "dup"}, lines)
	if stale || line != 5 {
		t.Errorf("Anchor = %d, %v; want 5, false", line, stale)
	}
	line, stale = Anchor(&Thread{Line: 1, AnchorText: "dup"}, lines)
	if stale || line != 2 {
		t.Errorf("Anchor = %d, %v; want 2, false", line, stale)
	}
}

func TestAnchorOutOfRange(t *testing.T) {
	// A comment far beyond the end of a now-shorter file is stale,
	// and must not panic.
	if line, stale := Anchor(&Thread{Line: 1000, AnchorText: "gone"}, []string{"a"}); !stale || line != 0 {
		t.Errorf("Anchor = %d, %v; want 0, true", line, stale)
	}
	if line, stale := Anchor(&Thread{Line: 2, AnchorText: "a"}, nil); !stale || line != 0 {
		t.Errorf("Anchor on empty file = %d, %v; want 0, true", line, stale)
	}
}
