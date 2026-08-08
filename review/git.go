// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
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

// Changes returns the commits not reachable from any remote ref, newest
// first, preceded by the uncommitted working tree if it is dirty.
func (r *gitRepo) Changes() ([]*Change, error) {
	out, err := run(r.root, "git", "log", gitLogFormat, "HEAD", "--not", "--remotes")
	if err != nil {
		// A repository with no commits at all has no HEAD to log.
		if _, err2 := run(r.root, "git", "rev-parse", "HEAD"); err2 != nil {
			out = nil
		} else {
			return nil, err
		}
	}

	var changes []*Change
	for _, rec := range records(out, 1) {
		c, err := parseGitCommit(rec)
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
	return changes, nil
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
	return parseGitCommit(recs[0])
}

func parseGitCommit(rec string) (*Change, error) {
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
		Key:     changeKey(f[0], msg),
		Rev:     f[0],
		Parent:  parent,
		Subject: subject(msg),
		Message: msg,
		Author:  f[2],
		Date:    time.Unix(sec, 0),
	}, nil
}

// changeKey returns the stable identity of a git commit: its Change-Id
// trailer if it has one, so that comments survive an amend, and otherwise
// the commit hash, which does not.
func changeKey(rev, msg string) string {
	for line := range strings.Lines(msg) {
		line = strings.TrimSpace(line)
		if id, ok := strings.CutPrefix(line, "Change-Id:"); ok {
			if id = strings.TrimSpace(id); id != "" {
				return id
			}
		}
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
