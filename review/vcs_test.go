// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
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
