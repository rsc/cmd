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

// jjLogTemplate produces one record per commit, NUL-separated fields
// terminated by \x01, so that descriptions containing newlines survive.
const jjLogTemplate = `change_id ++ "\0" ++ commit_id ++ "\0" ++ ` +
	`parents.map(|p| p.commit_id()).join(" ") ++ "\0" ++ ` +
	`parents.map(|p| p.change_id()).join(" ") ++ "\0" ++ ` +
	`author.name() ++ " <" ++ author.email() ++ ">" ++ "\0" ++ ` +
	`author.timestamp().format("%s") ++ "\0" ++ ` +
	`if(current_working_copy, "1", "0") ++ "\0" ++ ` +
	`if(empty, "1", "0") ++ "\0" ++ ` +
	`description ++ "\x01"`

// jjDiffTemplate reports each changed file as a status character and the
// exact source and target paths. The default --summary output compacts
// renames into "{old => new}", which cannot be parsed back reliably.
const jjDiffTemplate = `self.status_char() ++ "\0" ++ ` +
	`self.source().path() ++ "\0" ++ self.target().path() ++ "\x01"`

// A jjRepo is a jj repository.
type jjRepo struct {
	root string
}

func (r *jjRepo) Kind() string { return "jj" }
func (r *jjRepo) Root() string { return r.root }

// Changes returns the mutable commits, newest first. In jj the working copy
// is itself a commit, so uncommitted work appears here without a special case.
func (r *jjRepo) Changes() ([]*Change, error) {
	out, err := run(r.root, "jj", "log", "-r", "mutable()", "--no-graph", "-T", jjLogTemplate)
	if err != nil {
		return nil, err
	}
	var changes []*Change
	for _, rec := range records(out, 1) {
		c, err := parseJJCommit(rec)
		if err != nil {
			return nil, err
		}
		changes = append(changes, c)
	}
	return changes, nil
}

func (r *jjRepo) Commit(rev string) (*Change, error) {
	out, err := run(r.root, "jj", "log", "-r", rev, "--no-graph", "-T", jjLogTemplate)
	if err != nil {
		return nil, err
	}
	recs := records(out, 1)
	if len(recs) != 1 {
		return nil, fmt.Errorf("jj log %s: got %d commits", rev, len(recs))
	}
	return parseJJCommit(recs[0])
}

func parseJJCommit(rec string) (*Change, error) {
	f := strings.SplitN(rec, "\x00", 9)
	if len(f) != 9 {
		return nil, fmt.Errorf("malformed jj log record %q", rec)
	}
	parent := zeroID
	if p := strings.Fields(f[2]); len(p) > 0 {
		parent = p[0]
	}
	var parentKey string
	if p := strings.Fields(f[3]); len(p) > 0 && parent != zeroID {
		parentKey = p[0]
	}
	sec, err := strconv.ParseInt(f[5], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("malformed jj log date %q", f[5])
	}
	current := f[6] == "1"
	empty := f[7] == "1"
	msg := f[8]
	return &Change{
		// jj change IDs are stable across amends by construction, which
		// is exactly the identity comments need.
		Key:       f[0],
		Rev:       f[1],
		Parent:    parent,
		ParentKey: parentKey,
		Subject:   subject(msg),
		Message:   msg,
		Author:    f[4],
		Date:      time.Unix(sec, 0),
		Current:   current,
		// The working-copy commit before it has any content or description
		// of its own is not a change yet — it is where jj puts you between
		// commands, and jj replaces it the moment you move on, abandoning it
		// if nothing was ever recorded there. Snapshotting it would just
		// leave a stranded row behind once jj does that.
		Working: current && empty && strings.TrimSpace(msg) == "",
	}, nil
}

func (r *jjRepo) Files(base, rev string) ([]*File, error) {
	out, err := run(r.root, "jj", "diff", "--from", base, "--to", rev, "-T", jjDiffTemplate)
	if err != nil {
		return nil, err
	}
	var files []*File
	for _, rec := range records(out, 1) {
		f := strings.SplitN(rec, "\x00", 3)
		if len(f) != 3 || f[0] == "" {
			return nil, fmt.Errorf("malformed jj diff record %q", rec)
		}
		file := &File{Status: f[0][0], Path: f[2]}
		if file.Status == 'R' || file.Status == 'C' {
			file.OldPath = f[1]
		}
		files = append(files, file)
	}
	return files, nil
}

func (r *jjRepo) Content(rev, path string) ([]byte, error) {
	if rev == zeroID {
		return nil, nil
	}
	return run(r.root, "jj", "file", "show", "-r", rev, "--", path)
}

// Pin writes a git ref naming rev in the git repository backing the jj repo,
// so that an amended-away commit survives garbage collection. It works for
// both colocated and non-colocated repositories: .jj/repo/store/git_target
// records where the backing git directory lives.
func (r *jjRepo) Pin(name, rev string) error {
	dir, err := r.gitDir()
	if err != nil {
		return err
	}
	_, err = run(r.root, "git", "--git-dir="+dir, "update-ref", pinRef+name, rev)
	return err
}

// EnsureStableKey is a no-op: every jj commit already has a stable change
// ID of its own, which Changes and Commit already use as Key.
func (r *jjRepo) EnsureStableKey(rev string) (string, error) { return rev, nil }

func (r *jjRepo) gitDir() (string, error) {
	store := filepath.Join(r.root, ".jj", "repo", "store")
	data, err := os.ReadFile(filepath.Join(store, "git_target"))
	if err != nil {
		return "", fmt.Errorf("finding git store for %s: %v", r.root, err)
	}
	target := strings.TrimSpace(string(data))
	if !filepath.IsAbs(target) {
		target = filepath.Join(store, target)
	}
	return filepath.Clean(target), nil
}
