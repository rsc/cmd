// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// do runs a command in dir, failing the test if it does not succeed.
func do(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := run(dir, args...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return string(out)
}

func write(t *testing.T, dir, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0666); err != nil {
		t.Fatal(err)
	}
}

// newGitRepo creates a repository with one commit already pushed to a
// simulated remote, so that only later commits count as pending.
func newGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	dir := t.TempDir()
	do(t, dir, "git", "init", "-q", "-b", "main")
	do(t, dir, "git", "config", "user.name", "Tester")
	do(t, dir, "git", "config", "user.email", "tester@example.com")
	write(t, dir, "base.txt", "base\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "base commit")
	// Pretend this commit is already on a remote.
	head := do(t, dir, "git", "rev-parse", "HEAD")
	do(t, dir, "git", "update-ref", "refs/remotes/origin/main", head[:len(head)-1])
	return dir
}

func TestGitChanges(t *testing.T) {
	dir := newGitRepo(t)
	write(t, dir, "a.txt", "one\ntwo\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "add a.txt\n\nBody text.\n")

	r, err := OpenRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind() != "git" {
		t.Fatalf("Kind = %q, want git", r.Kind())
	}

	changes, err := r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	// The base commit is on the remote, so only "add a.txt" is pending.
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	c := changes[0]
	if c.Subject != "add a.txt" {
		t.Errorf("Subject = %q, want %q", c.Subject, "add a.txt")
	}
	if c.Author != "Tester <tester@example.com>" {
		t.Errorf("Author = %q", c.Author)
	}
	// With no Change-Id trailer, the key falls back to the commit hash.
	if c.Key != c.Rev {
		t.Errorf("Key = %q, want the commit hash %q", c.Key, c.Rev)
	}

	files, err := r.Files(c.Parent, c.Rev)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "a.txt" || files[0].Status != 'A' {
		t.Fatalf("Files = %+v, want one added a.txt", files[0])
	}

	data, err := r.Content(c.Rev, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\ntwo\n" {
		t.Errorf("Content = %q", data)
	}
}

// TestGitChangesIgnoresNonOriginRemote checks that pushing a commit to a
// remote other than origin -- a fork used to open a pull request, say --
// does not make it disappear from the pending list. Only origin says a
// commit has landed.
func TestGitChangesIgnoresNonOriginRemote(t *testing.T) {
	dir := newGitRepo(t)
	write(t, dir, "a.txt", "one\ntwo\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "add a.txt")
	// Pretend this commit was pushed to a fork remote to open a PR.
	head := do(t, dir, "git", "rev-parse", "HEAD")
	do(t, dir, "git", "update-ref", "refs/remotes/fork/main", head[:len(head)-1])

	r, err := OpenRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Subject != "add a.txt" {
		t.Fatalf("Changes = %+v, want the commit pushed only to the fork remote still pending", changes)
	}
}

func TestGitChangeID(t *testing.T) {
	dir := newGitRepo(t)
	write(t, dir, "a.txt", "x\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "subject\n\nChange-Id: I1234567890abcdef\n")

	r, _ := OpenRepo(dir)
	changes, err := r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	if got := changes[0].Key; got != "I1234567890abcdef" {
		t.Errorf("Key = %q, want the Change-Id trailer", got)
	}

	// Amending must not change the key, which is the whole point of it.
	before := changes[0].Key
	write(t, dir, "a.txt", "y\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "--amend", "--no-edit")
	changes, err = r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].Key != before {
		t.Errorf("Key changed across amend: %q -> %q", before, changes[0].Key)
	}
	if changes[0].Rev == "" {
		t.Error("Rev is empty")
	}
}

// TestGitStableKeyNoChangeID checks that a commit with no Change-Id
// trailer, whose only identity is otherwise its hash, keeps the key
// EnsureStableKey mints for it across an amend that changes the hash.
func TestGitStableKeyNoChangeID(t *testing.T) {
	dir := newGitRepo(t)
	write(t, dir, "a.txt", "x\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "subject, no Change-Id")

	r, err := OpenRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	rev := changes[0].Rev
	if changes[0].Key != rev {
		t.Fatalf("Key = %q before EnsureStableKey, want the hash %q", changes[0].Key, rev)
	}

	key, err := r.EnsureStableKey(rev)
	if err != nil {
		t.Fatal(err)
	}
	if key == rev {
		t.Fatalf("EnsureStableKey returned the hash unchanged, want a minted key")
	}
	if again, err := r.EnsureStableKey(rev); err != nil || again != key {
		t.Errorf("EnsureStableKey(rev) a second time = %q, %v, want %q, nil (idempotent)", again, err, key)
	}

	changes, err = r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].Key != key {
		t.Errorf("Key = %q after EnsureStableKey, want %q", changes[0].Key, key)
	}

	// Amending changes the hash. The note travels with it, since
	// EnsureStableKey configured the repository to carry it forward.
	write(t, dir, "a.txt", "y\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "--amend", "--no-edit")
	changes, err = r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].Rev == rev {
		t.Fatal("amend did not change the hash; test is not exercising anything")
	}
	if changes[0].Key != key {
		t.Errorf("Key changed across amend: %q -> %q, want it to stay %q", key, changes[0].Key, key)
	}
}

func TestGitWorkingTree(t *testing.T) {
	dir := newGitRepo(t)

	r, _ := OpenRepo(dir)
	changes, err := r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("clean repo has %d changes, want 0", len(changes))
	}

	// A modified file and an untracked file both belong to the working change.
	write(t, dir, "base.txt", "base\nmore\n")
	write(t, dir, "new.txt", "brand new\n")
	changes, err = r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || !changes[0].Working {
		t.Fatalf("got %+v, want one working change", changes)
	}

	files, err := r.Files(changes[0].Parent, changes[0].Rev)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]byte{}
	for _, f := range files {
		got[f.Path] = f.Status
	}
	if got["base.txt"] != 'M' || got["new.txt"] != 'A' {
		t.Errorf("Files = %v, want base.txt M and new.txt A", got)
	}

	data, err := r.Content(WorkingRev, "new.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "brand new\n" {
		t.Errorf("Content = %q", data)
	}
}

func TestGitRename(t *testing.T) {
	dir := newGitRepo(t)
	write(t, dir, "old.txt", "aaa\nbbb\nccc\nddd\neee\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "add old.txt")
	do(t, dir, "git", "mv", "old.txt", "new.txt")
	do(t, dir, "git", "commit", "-q", "-m", "rename")

	r, _ := OpenRepo(dir)
	changes, _ := r.Changes()
	files, err := r.Files(changes[0].Parent, changes[0].Rev)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Status != 'R' {
		t.Fatalf("Files = %+v, want one rename", files)
	}
	if files[0].Old() != "old.txt" || files[0].Path != "new.txt" {
		t.Errorf("rename %q -> %q", files[0].Old(), files[0].Path)
	}
}

func TestGitPinSurvivesGC(t *testing.T) {
	dir := newGitRepo(t)
	write(t, dir, "a.txt", "one\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "first")

	r, _ := OpenRepo(dir)
	changes, _ := r.Changes()
	old := changes[0].Rev
	if err := r.Pin(refName(changes[0].Key)+"/1", old); err != nil {
		t.Fatal(err)
	}

	// Amend the commit away, then garbage collect aggressively.
	write(t, dir, "a.txt", "two\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "--amend", "--no-edit")
	do(t, dir, "git", "reflog", "expire", "--expire=now", "--all")
	do(t, dir, "git", "gc", "--prune=now", "-q")

	// The pinned commit must still be readable.
	data, err := r.Content(old, "a.txt")
	if err != nil {
		t.Fatalf("pinned commit was collected: %v", err)
	}
	if string(data) != "one\n" {
		t.Errorf("Content = %q, want %q", data, "one\n")
	}
}

func TestCommitMsgFile(t *testing.T) {
	dir := newGitRepo(t)
	write(t, dir, "a.txt", "x\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "the subject\n\nThe body.\n")

	r, _ := OpenRepo(dir)
	changes, _ := r.Changes()
	data, err := FileContent(r, changes[0].Rev, CommitMsgFile)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{"Parent:", "Author:     Tester <tester@example.com>", "the subject", "The body."} {
		if !strings.Contains(s, want) {
			t.Errorf("commit message file missing %q:\n%s", want, s)
		}
	}
	// The parent here is an ordinary commit with no Change-Id, so it has
	// no identity beyond its hash and is named once, not twice.
	if strings.Contains(s, "Change Parent:") || strings.Contains(s, "Git Parent:") {
		t.Errorf("commit message file names a parent that has no stable identity:\n%s", s)
	}
}

// TestCommitMsgParent checks that a change whose parent carries a Change-Id
// names it separately from the commit ID. The commit ID moves whenever the
// commit below is amended; the Change-Id does not, so a diff of the commit
// message tells a rebase apart from a real change of parent.
func TestCommitMsgParent(t *testing.T) {
	dir := newGitRepo(t)
	write(t, dir, "a.txt", "x\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "bottom\n\nChange-Id: Ibottom\n")
	write(t, dir, "b.txt", "y\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "top\n\nChange-Id: Itop\n")

	r, _ := OpenRepo(dir)
	changes, err := r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	top := changes[0]
	if top.Key != "Itop" {
		t.Fatalf("newest change is %+v", top)
	}
	if top.ParentKey != "Ibottom" {
		t.Errorf("ParentKey = %q, want %q", top.ParentKey, "Ibottom")
	}
	// Commit resolves the parent the same way Changes does, so that the
	// commit message of an old snapshot renders identically.
	again, err := r.Commit(top.Rev)
	if err != nil {
		t.Fatal(err)
	}
	if again.ParentKey != top.ParentKey {
		t.Errorf("Commit ParentKey = %q, Changes ParentKey = %q", again.ParentKey, top.ParentKey)
	}

	data, err := FileContent(r, top.Rev, CommitMsgFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Change Parent: Ibottom\n", "Git Parent:    " + shortRev(top.Parent) + "\n"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("commit message file missing %q:\n%s", want, data)
		}
	}

	// Amending the parent moves the commit ID and leaves the Change-Id
	// alone, which is the whole point of showing both.
	do(t, dir, "git", "reset", "-q", "--hard", "HEAD~1")
	write(t, dir, "a.txt", "x2\n")
	do(t, dir, "git", "commit", "-q", "-a", "--amend", "--no-edit")
	do(t, dir, "git", "cherry-pick", top.Rev)

	changes, err = r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	if moved := changes[0]; moved.ParentKey != top.ParentKey || moved.Parent == top.Parent {
		t.Errorf("after rebase ParentKey = %q (want %q), Parent = %q (want it to have moved from %q)",
			moved.ParentKey, top.ParentKey, moved.Parent, top.Parent)
	}
}

// newJJRepo creates a jj repository, skipping the test if jj is missing.
func newJJRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not found")
	}
	dir := t.TempDir()
	if _, err := run(dir, "jj", "git", "init", "."); err != nil {
		t.Skipf("jj git init: %v", err)
	}
	do(t, dir, "jj", "config", "set", "--repo", "user.name", "Tester")
	do(t, dir, "jj", "config", "set", "--repo", "user.email", "tester@example.com")
	return dir
}

func TestJJChanges(t *testing.T) {
	dir := newJJRepo(t)
	write(t, dir, "a.txt", "one\ntwo\n")
	do(t, dir, "jj", "describe", "-m", "first change\n\nBody.\n")

	r, err := OpenRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind() != "jj" {
		t.Fatalf("Kind = %q, want jj", r.Kind())
	}

	changes, err := r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) == 0 {
		t.Fatal("no changes")
	}
	c := changes[0]
	if c.Subject != "first change" {
		t.Errorf("Subject = %q", c.Subject)
	}
	if !c.Current {
		t.Errorf("newest mutable change is not the working copy")
	}
	// Uncommitted work is visible without a synthetic working change,
	// because jj's working copy is itself a commit.
	if c.Working {
		t.Errorf("jj change marked Working")
	}

	files, err := r.Files(c.Parent, c.Rev)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "a.txt" || files[0].Status != 'A' {
		t.Fatalf("Files = %+v, want one added a.txt", files)
	}

	data, err := r.Content(c.Rev, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\ntwo\n" {
		t.Errorf("Content = %q", data)
	}
}

// TestJJPendingAll checks that a repository defining pendingall() decides
// for itself which commits are pending. jj-codereview defines it, and the
// difference that shows is the copy of a commit that mailing it leaves
// behind: without it, a mailed stack is listed twice over, each copy
// carrying the change ID of the commit it was made from.
func TestJJPendingAll(t *testing.T) {
	dir := newJJRepo(t)
	write(t, dir, "a.txt", "one\n")
	do(t, dir, "jj", "describe", "-m", "first")
	do(t, dir, "jj", "new", "-m", "second")
	write(t, dir, "b.txt", "two\n")

	// With no pendingall() defined, everything mutable is pending.
	r, _ := OpenRepo(dir)
	changes, err := r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("got %d changes, want 2: %+v", len(changes), changes)
	}

	// Defining it settles the question instead.
	do(t, dir, "jj", "config", "set", "--repo", `revset-aliases."pendingall()"`, "mutable() ~ @")
	changes, err = r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Subject != "first" {
		t.Fatalf("with pendingall() got %+v, want only the change it names", changes)
	}
}

// TestJJChangeIDStable is the case git cannot handle without a Change-Id
// trailer: rewriting the commit keeps the change ID, so comments stay put.
func TestJJChangeIDStable(t *testing.T) {
	dir := newJJRepo(t)
	write(t, dir, "a.txt", "one\n")
	do(t, dir, "jj", "describe", "-m", "before")

	r, _ := OpenRepo(dir)
	changes, err := r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	key, rev := changes[0].Key, changes[0].Rev

	write(t, dir, "a.txt", "two\n")
	do(t, dir, "jj", "describe", "-m", "after")

	changes, err = r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].Key != key {
		t.Errorf("change ID changed: %q -> %q", key, changes[0].Key)
	}
	if changes[0].Rev == rev {
		t.Errorf("commit ID did not change: %q", rev)
	}
	if changes[0].Subject != "after" {
		t.Errorf("Subject = %q, want after", changes[0].Subject)
	}
}

func TestJJRename(t *testing.T) {
	dir := newJJRepo(t)
	write(t, dir, "old.txt", "aaa\nbbb\nccc\n")
	do(t, dir, "jj", "describe", "-m", "add")
	do(t, dir, "jj", "new", "-m", "rename")
	if err := os.Rename(filepath.Join(dir, "old.txt"), filepath.Join(dir, "new.txt")); err != nil {
		t.Fatal(err)
	}

	r, _ := OpenRepo(dir)
	changes, _ := r.Changes()
	files, err := r.Files(changes[0].Parent, changes[0].Rev)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Status != 'R' {
		t.Fatalf("Files = %+v, want one rename", files)
	}
	// The point of using a template instead of --summary: exact paths,
	// not the "{old.txt => new.txt}" form.
	if files[0].Old() != "old.txt" || files[0].Path != "new.txt" {
		t.Errorf("rename %q -> %q", files[0].Old(), files[0].Path)
	}
}

// TestJJPin checks that snapshots can be pinned in a non-colocated repo,
// where the backing git directory is found through .jj/repo/store/git_target.
func TestJJPin(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not found")
	}
	dir := t.TempDir()
	if _, err := run(dir, "jj", "git", "init", "--no-colocate", "."); err != nil {
		t.Skipf("jj git init --no-colocate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		t.Skip("repo is colocated; nothing to test")
	}
	do(t, dir, "jj", "config", "set", "--repo", "user.name", "Tester")
	do(t, dir, "jj", "config", "set", "--repo", "user.email", "tester@example.com")
	write(t, dir, "a.txt", "one\n")
	do(t, dir, "jj", "describe", "-m", "first")

	r, _ := OpenRepo(dir)
	changes, _ := r.Changes()
	if err := r.Pin(refName(changes[0].Key)+"/1", changes[0].Rev); err != nil {
		t.Fatalf("Pin: %v", err)
	}
}

// TestJJCommitMsgParent checks the jj half of the two-line parent header.
// In jj the parent's change ID comes straight out of the log template, and
// it survives the automatic rebase that editing a commit below sets off.
func TestJJCommitMsgParent(t *testing.T) {
	dir := newJJRepo(t)
	write(t, dir, "a.txt", "one\n")
	do(t, dir, "jj", "describe", "-m", "bottom")
	do(t, dir, "jj", "new", "-m", "top")
	write(t, dir, "b.txt", "two\n")

	r, _ := OpenRepo(dir)
	changes, err := r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	top := changes[0]
	if top.Subject != "top" {
		t.Fatalf("newest change is %+v", top)
	}
	if top.ParentKey == "" || top.ParentKey == top.Parent {
		t.Fatalf("ParentKey = %q, Parent = %q", top.ParentKey, top.Parent)
	}

	data, err := FileContent(r, top.Rev, CommitMsgFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"JJ Parent:  " + top.ParentKey + "\n", "Git Parent: " + shortRev(top.Parent) + "\n"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("commit message file missing %q:\n%s", want, data)
		}
	}

	// Editing the commit below rewrites it and rebases this one onto the
	// new version: the git parent moves, the jj parent does not.
	do(t, dir, "jj", "edit", top.ParentKey)
	write(t, dir, "a.txt", "one and a half\n")
	do(t, dir, "jj", "edit", top.Key)

	changes, err = r.Changes()
	if err != nil {
		t.Fatal(err)
	}
	var moved *Change
	for _, c := range changes {
		if c.Key == top.Key {
			moved = c
		}
	}
	if moved == nil {
		t.Fatalf("change %s is gone", top.Key)
	}
	if moved.ParentKey != top.ParentKey {
		t.Errorf("after rebase ParentKey = %q, want %q", moved.ParentKey, top.ParentKey)
	}
	if moved.Parent == top.Parent {
		t.Errorf("after rebase Parent = %q, unchanged", moved.Parent)
	}
}

// chainChanges builds a pending set from a list of "rev parent" pairs, in
// the order a change list would put them: newest first. A third field, @,
// marks the working copy.
func chainChanges(pairs ...string) []*Change {
	var changes []*Change
	for _, p := range pairs {
		f := strings.Fields(p)
		c := &Change{Key: f[0], Rev: f[0], Parent: f[1], Subject: f[0]}
		c.Current = len(f) > 2 && f[2] == "@"
		changes = append(changes, c)
	}
	return changes
}

// chainDraw renders a chain in the characters jj log prints one in, so
// that a test can say what it wants to see. The page draws the same rows
// as lines instead; this is the same shape, in a form a test can read.
func chainDraw(t *testing.T, entries []GraphRow) string {
	var b strings.Builder
	for _, e := range entries {
		if len(e.Lanes) != len(entries[0].Lanes) {
			t.Errorf("row %+v has %d lanes, want %d", e, len(e.Lanes), len(entries[0].Lanes))
		}
		row := make([]rune, 2*len(e.Lanes)-1)
		for i := range row {
			row[i] = ' '
		}
		for lane, on := range e.Lanes {
			if on {
				row[2*lane] = '│'
			}
		}
		if e.Change != nil {
			row[2*e.Col] = '○'
			if e.Current || e.Working {
				row[2*e.Col] = '@'
			}
		} else {
			row[2*e.Col], row[2*e.Join] = '├', '╯'
			for i := 2*e.Col + 1; i < 2*e.Join; i++ {
				if row[i] == '│' {
					row[i] = '┼'
				} else {
					row[i] = '─'
				}
			}
		}
		fmt.Fprintf(&b, "%s", strings.TrimRight(string(row), " "))
		if e.Change != nil {
			fmt.Fprintf(&b, " %s", e.Rev)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func TestChain(t *testing.T) {
	// A stack of three on main, and a second stack beside it, which shares
	// only the commit both were started on and so is a chain of its own.
	stacks := chainChanges("c b", "b a", "a main", "z main")
	// A fork: the working copy sits on the bottom change, not on the top.
	forked := chainChanges("c b", "w b @", "b a", "a main")
	// Two branches off one change, one of them carrying a stack of its own,
	// which is the shape jj log draws with a join row per branch.
	tree := chainChanges("e d", "d b", "c b", "b a", "z a", "a main")

	for _, tt := range []struct {
		name    string
		changes []*Change
		at      string
		want    string
	}{
		{"tip", stacks, "c", "○ c\n○ b\n○ a\n"},
		{"middle", stacks, "b", "○ c\n○ b\n○ a\n"},
		{"bottom", stacks, "a", "○ c\n○ b\n○ a\n"},
		{"beside", stacks, "z", ""},
		{"alone", chainChanges("a main"), "a", ""},
		{"forked", forked, "c", "○ c\n│ @ w\n├─╯\n○ b\n○ a\n"},
		{"forked from below", forked, "a", "○ c\n│ @ w\n├─╯\n○ b\n○ a\n"},
		{"tree", tree, "a", "○ e\n○ d\n│ ○ c\n├─╯\n○ b\n│ ○ z\n├─╯\n○ a\n"},
		{"absent", stacks, "gone", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := chainDraw(t, chain(tt.changes, &Change{Rev: tt.at}))
			if got != tt.want {
				t.Errorf("chain at %s:\n%s\nwant:\n%s", tt.at, got, tt.want)
			}
		})
	}
}

// TestGraph checks the whole pending set drawn as one graph, which is what
// the change list shows. The changes keep the order they came in.
func TestGraph(t *testing.T) {
	for _, tt := range []struct {
		name    string
		changes []*Change
		want    string
	}{
		// Two stacks sharing only the commit they were both started on,
		// which is not pending: nothing joins them, and the second reuses
		// the lane the first has finished with.
		{"two stacks", chainChanges("c b", "b a", "a main", "z main"), "○ c\n○ b\n○ a\n○ z\n"},
		// A branch off the middle of a stack, listed newest first, joins
		// the stack at the change it was branched from.
		{"branch", chainChanges("c b", "w b", "b a", "a main"), "○ c\n│ ○ w\n├─╯\n○ b\n○ a\n"},
		// A change listed before something sitting on it: a clock set
		// wrong, or a date carried across a rebase. Drawing it in the
		// order given would leave a line hanging off the bottom, so the
		// change is held back until what sits on it has been drawn.
		{"skewed clock", chainChanges("a main", "b a"), "○ b\n○ a\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := chainDraw(t, graph(tt.changes))
			if got != tt.want {
				t.Errorf("graph:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

// TestChainWindow checks the window a long chain is trimmed to: the change
// being viewed with chainSide of them either side, an end short of that
// leaving the rest to the other, and a row standing for what was left out
// at each end that was trimmed.
func TestChainWindow(t *testing.T) {
	// A stack of twenty on main, newest first, as a change list has them.
	var pairs []string
	for i := 19; i > 0; i-- {
		pairs = append(pairs, fmt.Sprintf("c%02d c%02d", i, i-1))
	}
	pairs = append(pairs, "c00 main")
	changes := chainChanges(pairs...)

	for _, tt := range []struct {
		at       string
		up, down bool
		want     []string // the changes shown, newest first
		more     int      // rows standing for changes left out
	}{
		{at: "c10", want: []string{"c15", "c14", "c13", "c12", "c11", "c10", "c09", "c08", "c07", "c06", "c05"}, more: 2},
		// Nothing above the tip, so the whole window goes below it.
		{at: "c19", want: []string{"c19", "c18", "c17", "c16", "c15", "c14", "c13", "c12", "c11", "c10", "c09"}, more: 1},
		// And nothing below the oldest.
		{at: "c00", want: []string{"c10", "c09", "c08", "c07", "c06", "c05", "c04", "c03", "c02", "c01", "c00"}, more: 1},
		// Asking for one end whole leaves the other end trimmed.
		{at: "c19", down: true, want: nil, more: 0},
		{at: "c10", up: true, want: []string{"c19", "c18", "c17", "c16", "c15", "c14", "c13", "c12", "c11", "c10", "c09", "c08", "c07", "c06", "c05"}, more: 1},
	} {
		t.Run(tt.at, func(t *testing.T) {
			rows := window(chain(changes, &Change{Rev: tt.at}), tt.at, tt.up, tt.down)
			var got []string
			more := 0
			for _, r := range rows {
				switch {
				case r.More:
					more++
				case r.Change != nil:
					got = append(got, r.Rev)
				}
			}
			if tt.want != nil && !slices.Equal(got, tt.want) {
				t.Errorf("window at %s = %v, want %v", tt.at, got, tt.want)
			}
			if tt.want == nil && len(got) != 20 {
				t.Errorf("window at %s shows %d changes, want the whole chain of 20", tt.at, len(got))
			}
			if more != tt.more {
				t.Errorf("window at %s has %d rows for changes left out, want %d", tt.at, more, tt.more)
			}
		})
	}
}

// TestChainEnds checks where a change's own line runs out of its row: a
// line up to what is sitting on it, and down to what it sits on. The chain
// stops at the change the stack was started on, which is not pending, so
// the bottom row's line has nowhere below to go.
func TestChainEnds(t *testing.T) {
	rows := chain(chainChanges("c b", "b a", "a main"), &Change{Rev: "b"})
	want := []struct{ up, down bool }{{false, true}, {true, true}, {true, false}}
	if len(rows) != len(want) {
		t.Fatalf("chain has %d rows, want %d", len(rows), len(want))
	}
	for i, w := range want {
		if rows[i].Up != w.up || rows[i].Down != w.down {
			t.Errorf("%s: Up, Down = %v, %v, want %v, %v", rows[i].Rev, rows[i].Up, rows[i].Down, w.up, w.down)
		}
	}
}

// TestJJRootParent checks that the virtual root commit, which has a change
// ID like any other jj commit, is not named as a parent: there is no change
// there to have moved.
func TestJJRootParent(t *testing.T) {
	dir := newJJRepo(t)
	write(t, dir, "a.txt", "one\n")
	do(t, dir, "jj", "describe", "-m", "only")

	r, _ := OpenRepo(dir)
	changes, _ := r.Changes()
	for _, c := range changes {
		if c.Parent == zeroID && c.ParentKey != "" {
			t.Errorf("change %s on the root commit has ParentKey %q", c.Key, c.ParentKey)
		}
	}
}
