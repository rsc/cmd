// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// gitLogFormat produces one record per commit, NUL-separated fields
// terminated by \x01, so that commit messages containing newlines
// survive parsing intact.
const gitLogFormat = "--format=%H%x00%P%x00%an <%ae>%x00%at%x00%B%x01"

// A gitRepo is a git repository.
type gitRepo struct {
	root string
}

func (r *gitRepo) Kind() string { return "git" }
func (r *gitRepo) Root() string { return r.root }

// Changes returns the commits not reachable from the upstream remote,
// newest first, preceded by the uncommitted working tree if it is dirty.
// Only the upstream says what has landed: pushing to another remote, such
// as a fork used to open a pull request, does not make a commit any less
// pending, and must not make it disappear from review.
func (r *gitRepo) Changes() ([]*Change, error) {
	out, err := run(r.root, "git", "log", gitLogFormat, "HEAD", "--not", "--remotes="+r.upstream())
	if err != nil {
		// A repository with no commits at all has no HEAD to log.
		if _, err2 := run(r.root, "git", "rev-parse", "HEAD"); err2 != nil {
			out = nil
		} else {
			return nil, err
		}
	}

	notes := r.noteKeys()
	var changes []*Change
	for _, rec := range records(out, 1) {
		c, err := parseGitCommit(rec, notes)
		if err != nil {
			return nil, err
		}
		changes = append(changes, c)
	}

	w, err := r.working()
	if err != nil {
		return nil, err
	}
	if w != nil {
		changes = append([]*Change{w}, changes...)
	}
	r.setParentKeys(changes, notes)
	return changes, nil
}

// upstream returns the remote whose refs say what has landed: the one the
// current branch tracks, which is what git itself means by upstream and
// what git push and git pull go to.
//
// A repository that says nothing about it is taken to use origin, which is
// the name git gives the remote it clones from. Not every repository does:
// one whose upstream is called something else was reading its whole
// history as pending, since nothing had landed on a remote it does not
// have.
func (r *gitRepo) upstream() string {
	branch, err := run(r.root, "git", "branch", "--show-current")
	if err != nil {
		return "origin"
	}
	name := strings.TrimSpace(string(branch))
	if name == "" {
		return "origin" // detached: no branch to have an upstream
	}
	out, err := run(r.root, "git", "config", "--get", "branch."+name+".remote")
	if err != nil {
		return "origin"
	}
	if remote := strings.TrimSpace(string(out)); remote != "" {
		return remote
	}
	return "origin"
}

// setParentKeys fills in each change's ParentKey. Stacked changes name one
// another, so the parents that have to be fetched are usually only the
// commit the stack sits on, and one call fetches all of them.
//
// A commit with no Change-Id trailer has no identity apart from its hash,
// which the commit message file already shows on its own line, so those
// parents are left unnamed rather than named twice.
func (r *gitRepo) setParentKeys(changes []*Change, notes map[string]string) {
	keys := make(map[string]string, len(changes))
	for _, c := range changes {
		keys[c.Rev] = c.Key
	}
	var missing []string
	for _, c := range changes {
		if c.Parent == "" || c.Parent == emptyTree {
			continue
		}
		if _, ok := keys[c.Parent]; !ok {
			keys[c.Parent] = ""
			missing = append(missing, c.Parent)
		}
	}
	if len(missing) > 0 {
		args := append([]string{"git", "log", "--no-walk", gitLogFormat}, missing...)
		if out, err := run(r.root, args...); err == nil {
			for _, rec := range records(out, 1) {
				if p, err := parseGitCommit(rec, notes); err == nil {
					keys[p.Rev] = p.Key
				}
			}
		}
	}
	for _, c := range changes {
		if k := keys[c.Parent]; k != "" && k != c.Parent {
			c.ParentKey = k
		}
	}
}

// working returns a synthetic change for the uncommitted working tree,
// or nil if the working tree is clean. In jj the working copy is a real
// commit; giving git one too keeps the two backends behaving alike.
func (r *gitRepo) working() (*Change, error) {
	out, err := run(r.root, "git", "status", "--porcelain")
	if err != nil || len(out) == 0 {
		return nil, err
	}
	head, err := run(r.root, "git", "rev-parse", "HEAD")
	parent := emptyTree
	if err == nil {
		parent = strings.TrimSpace(string(head))
	}
	return &Change{
		Key:     WorkingRev,
		Rev:     WorkingRev,
		Parent:  parent,
		Subject: "Uncommitted changes",
		Author:  gitUser(r.root),
		Date:    time.Now(),
		Working: true,
	}, nil
}

func gitUser(root string) string {
	name, err1 := run(root, "git", "config", "user.name")
	mail, err2 := run(root, "git", "config", "user.email")
	if err1 != nil || err2 != nil {
		return "you"
	}
	return fmt.Sprintf("%s <%s>", strings.TrimSpace(string(name)), strings.TrimSpace(string(mail)))
}

func (r *gitRepo) Commit(rev string) (*Change, error) {
	if rev == WorkingRev {
		return r.working()
	}
	out, err := run(r.root, "git", "log", "-1", gitLogFormat, rev)
	if err != nil {
		return nil, err
	}
	recs := records(out, 1)
	if len(recs) != 1 {
		return nil, fmt.Errorf("git log %s: got %d commits", rev, len(recs))
	}
	notes := r.noteKeys()
	c, err := parseGitCommit(recs[0], notes)
	if err != nil {
		return nil, err
	}
	r.setParentKeys([]*Change{c}, notes)
	return c, nil
}

func parseGitCommit(rec string, notes map[string]string) (*Change, error) {
	f := strings.SplitN(rec, "\x00", 5)
	if len(f) != 5 {
		return nil, fmt.Errorf("malformed git log record %q", rec)
	}
	parent := emptyTree
	if p := strings.Fields(f[1]); len(p) > 0 {
		parent = p[0]
	}
	sec, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("malformed git log date %q", f[3])
	}
	msg := f[4]
	return &Change{
		Key:     changeKey(f[0], msg, notes),
		Rev:     f[0],
		Parent:  parent,
		Subject: subject(msg),
		Message: msg,
		Author:  f[2],
		Date:    time.Unix(sec, 0),
	}, nil
}

// changeKey returns the stable identity of a git commit: its Change-Id
// trailer if it has one, so that comments survive an amend; otherwise the
// key EnsureStableKey minted for it and recorded in a git note, if Grab
// has snapshotted it before; otherwise the commit hash, which survives
// neither an amend nor, until something snapshots it, a rebase.
func changeKey(rev, msg string, notes map[string]string) string {
	if id := changeIDTrailer(msg); id != "" {
		return id
	}
	if key, ok := notes[rev]; ok {
		return key
	}
	return rev
}

func (r *gitRepo) Files(base, rev string) ([]*File, error) {
	if rev == WorkingRev {
		return r.workingFiles()
	}
	out, err := run(r.root, "git", "diff", "--name-status", "-M", "-z", base, rev)
	if err != nil {
		return nil, err
	}
	return parseGitNameStatus(string(out))
}

// parseGitNameStatus parses the NUL-separated output of git diff
// --name-status -z. Renames and copies occupy three fields instead of two.
func parseGitNameStatus(s string) ([]*File, error) {
	f := strings.Split(s, "\x00")
	var files []*File
	for i := 0; i < len(f); {
		if f[i] == "" {
			i++
			continue
		}
		status := f[i][0]
		if i+1 >= len(f) {
			return nil, fmt.Errorf("truncated git diff --name-status output")
		}
		if status == 'R' || status == 'C' {
			if i+2 >= len(f) {
				return nil, fmt.Errorf("truncated git diff --name-status rename")
			}
			files = append(files, &File{Status: status, OldPath: f[i+1], Path: f[i+2]})
			i += 3
			continue
		}
		files = append(files, &File{Status: status, Path: f[i+1]})
		i += 2
	}
	return files, nil
}

// workingFiles lists the files that differ between HEAD and the working
// tree, including untracked files, which a plain git diff would omit.
func (r *gitRepo) workingFiles() ([]*File, error) {
	out, err := run(r.root, "git", "status", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	f := strings.Split(string(out), "\x00")
	var files []*File
	for i := 0; i < len(f); i++ {
		rec := f[i]
		if len(rec) < 4 {
			continue
		}
		x, y, path := rec[0], rec[1], rec[3:]
		switch {
		case x == '?' && y == '?':
			files = append(files, &File{Status: 'A', Path: path})
		case x == 'R' || x == 'C':
			// A rename records the old path in the following field.
			old := ""
			if i+1 < len(f) {
				old = f[i+1]
				i++
			}
			files = append(files, &File{Status: x, Path: path, OldPath: old})
		case x == 'A':
			files = append(files, &File{Status: 'A', Path: path})
		case x == 'D' || y == 'D':
			files = append(files, &File{Status: 'D', Path: path})
		default:
			files = append(files, &File{Status: 'M', Path: path})
		}
	}
	return files, nil
}

func (r *gitRepo) Content(rev, path string) ([]byte, error) {
	if rev == WorkingRev {
		data, err := os.ReadFile(filepath.Join(r.root, path))
		if os.IsNotExist(err) {
			return nil, nil
		}
		return data, err
	}
	if rev == emptyTree {
		return nil, nil
	}
	return run(r.root, "git", "cat-file", "blob", rev+":"+path)
}

func (r *gitRepo) Pin(name, rev string) error {
	if rev == WorkingRev {
		return nil
	}
	_, err := run(r.root, "git", "update-ref", pinRef+name, rev)
	return err
}

// reviewNotesRef holds one note per commit that changeKey has had to fall
// back to the commit's own hash for: a commit with no Change-Id trailer.
// The note's content is the key EnsureStableKey minted for it, so that a
// later amend or rebase, which changes the hash, does not also start the
// change over as if it were new.
const reviewNotesRef = "refs/notes/review-key"

// noteKeys reads every note under reviewNotesRef, returning the commit
// hashes that were still on it as of the last snapshot mapped to the key
// recorded for each. One call reads the whole repository's worth, which
// changeKey then just looks up per commit, rather than shelling out once
// per commit for what is normally a short list.
func (r *gitRepo) noteKeys() map[string]string {
	out, err := run(r.root, "git", "notes", "--ref="+reviewNotesRef, "list")
	if err != nil {
		return nil
	}
	var keys map[string]string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		blob, rev := f[0], f[1]
		content, err := run(r.root, "git", "cat-file", "blob", blob)
		if err != nil {
			continue
		}
		if keys == nil {
			keys = map[string]string{}
		}
		keys[rev] = strings.TrimSpace(string(content))
	}
	return keys
}

// EnsureStableKey mints a key for rev and records it in a git note under
// reviewNotesRef if rev does not already carry one there, and configures
// the repository to carry that note forward across the amend or rebase
// that is normally about to change rev's hash out from under it. Called
// only for a commit changeKey has resolved to its own hash, for lack of
// anything sturdier: see changeKey.
func (r *gitRepo) EnsureStableKey(rev string) (string, error) {
	if out, err := run(r.root, "git", "notes", "--ref="+reviewNotesRef, "show", rev); err == nil {
		if key := strings.TrimSpace(string(out)); key != "" {
			return key, nil
		}
	}
	if err := r.ensureNotesCarryForward(); err != nil {
		return "", err
	}
	key := newReviewKey()
	if _, err := run(r.root, "git", "notes", "--ref="+reviewNotesRef, "add", "-f", "-m", key, rev); err != nil {
		return "", err
	}
	return key, nil
}

// ensureNotesCarryForward configures the repository so that an amend or a
// rebase carries a reviewNotesRef note from the commit it rewrote to the
// commit it produced — git's own notes.rewrite mechanism, off by default.
// Idempotent: safe to call before every note this package writes.
func (r *gitRepo) ensureNotesCarryForward() error {
	if _, err := run(r.root, "git", "config", "notes.rewrite.amend", "true"); err != nil {
		return err
	}
	if _, err := run(r.root, "git", "config", "notes.rewrite.rebase", "true"); err != nil {
		return err
	}
	out, _ := run(r.root, "git", "config", "--get-all", "notes.rewriteRef")
	for _, ref := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if ref == reviewNotesRef {
			return nil
		}
	}
	_, err := run(r.root, "git", "config", "--add", "notes.rewriteRef", reviewNotesRef)
	return err
}

// newReviewKey returns a fresh, effectively unique change key in the same
// shape as a Change-Id, but prefixed R rather than I so that the two are
// never mistaken for each other in a log or a URL.
func newReviewKey() string {
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand not working is not a recoverable state
	}
	return "R" + hex.EncodeToString(b[:])
}
