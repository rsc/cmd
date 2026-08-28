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
	"sync"
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

	// Which revset says what is pending, read once: it is a question about
	// the configuration, and the configuration does not change under us.
	pendingOnce   sync.Once
	pendingRevset string

	// The Gerrit changes the repository's commits have been uploaded as,
	// read once and kept: a page draws many changes and they all want the
	// same answer. See cls.
	clsOnce sync.Once
	cls     map[string]string // Change-Id trailer -> CL number
	clsBase string            // the URL those numbers live under
}

func (r *jjRepo) Kind() string { return "jj" }
func (r *jjRepo) Root() string { return r.root }

// jjPendingAll is the revset naming the commits to review, where the
// repository says what it means by pending. jj-codereview defines
// pendingall(), and leaving the definition to it is what keeps the two
// tools agreeing about what is under review.
//
// The difference that shows is the copy of each commit that mailing it
// leaves behind, which jj-codereview keeps under a remote bookmark of its
// own. A repository that lets those be rewritten keeps them inside
// mutable(), where they arrive as a second copy of every mailed stack —
// and, since a copy carries the change ID of the commit it was made from,
// as the same change twice.
const jjPendingAll = "pendingall()"

// jjMutable is what to review in a repository that does not define
// pendingall(): everything still open to being rewritten.
const jjMutable = "mutable()"

// pending returns the revset of the commits to review. Looking the alias
// up in the configuration answers the question without touching the
// repository, which asking for pendingall() and reading the failure would
// not: in a large repository that costs more than everything else here.
func (r *jjRepo) pending() string {
	r.pendingOnce.Do(func() {
		r.pendingRevset = jjMutable
		if _, err := run(r.root, "jj", "config", "get", `revset-aliases."pendingall()"`); err == nil {
			r.pendingRevset = jjPendingAll
		}
	})
	return r.pendingRevset
}

// Changes returns the pending commits, newest first. In jj the working copy
// is itself a commit, so uncommitted work appears here without a special case.
func (r *jjRepo) Changes() ([]*Change, error) {
	out, err := run(r.root, "jj", "log", "-r", r.pending(), "--no-graph", "-T", jjLogTemplate)
	if err != nil {
		return nil, err
	}
	cls, base := r.gerritCLs()
	var changes []*Change
	for _, rec := range records(out, 1) {
		c, err := parseJJCommit(rec)
		if err != nil {
			return nil, err
		}
		if n := cls[changeIDTrailer(c.Message)]; n != "" {
			c.CL, c.CLURL = n, base+"/"+n
		}
		changes = append(changes, c)
	}
	return changes, nil
}

// clBookmarks is the revset of the commits carrying a bookmark that names
// a Gerrit change: jj-codereview writes cl/12345 for the change itself and
// cl/12345/2 for each patch set it uploads. A glob does not match across
// the slash in the second, so the names are matched by regexp.
const clBookmarks = `bookmarks(regex:"^cl/") | remote_bookmarks(regex:"^cl/")`

// jjCLTemplate reports a commit's bookmarks and the Change-Id in its
// message, and nothing else: the messages are long and only that one line
// of them is wanted.
const jjCLTemplate = `bookmarks ++ "\0" ++ ` +
	`description.lines().filter(|l| l.starts_with("Change-Id:")).join(" ") ++ "\x01"`

// gerritCLs returns the number of the Gerrit change each commit has been
// uploaded as, keyed by the Change-Id in its message, along with the URL
// those numbers live under.
//
// It is the uploaded copy of a commit that carries the number, under a
// bookmark of its own. The commit it was made from has been amended and
// rebased since and is no longer that commit; the Change-Id is what the
// two still have in common, so it is what the numbers are keyed by.
//
// A repository whose commits do not go to Gerrit says so by not naming a
// Gerrit at all, which is asked first because it is a question about the
// configuration rather than about the repository, and costs accordingly.
func (r *jjRepo) gerritCLs() (map[string]string, string) {
	r.clsOnce.Do(func() {
		r.cls = map[string]string{}
		if r.clsBase = r.gerritURL(); r.clsBase == "" {
			return
		}
		out, err := run(r.root, "jj", "log", "-r", clBookmarks, "--no-graph", "-T", jjCLTemplate)
		if err != nil {
			return
		}
		for _, rec := range records(out, 1) {
			f := strings.SplitN(rec, "\x00", 2)
			if len(f) != 2 {
				continue
			}
			id := changeIDTrailer(f[1])
			if id == "" {
				continue
			}
			for _, name := range strings.Fields(f[0]) {
				if n := clNumber(name); n != "" {
					r.cls[id] = n
					break
				}
			}
		}
	})
	return r.cls, r.clsBase
}

// gerritURL returns the base URL of the Gerrit server the repository's
// changes are reviewed on, which jj-codereview keeps in a template alias
// so that its own log templates can link to it. The alias holds a template
// expression, which for a plain URL is the URL in quotes.
func (r *jjRepo) gerritURL() string {
	out, err := run(r.root, "jj", "config", "get", `template-aliases."gerriturl()"`)
	if err != nil {
		return ""
	}
	url := strings.Trim(strings.TrimSpace(string(out)), `"`)
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return ""
	}
	return strings.TrimSuffix(url, "/")
}

// clNumber returns the Gerrit change number a bookmark names, or "" if it
// names something else. A bookmark is cl/12345 for the change and
// cl/12345/2 for the second patch set uploaded, and a bookmark read from a
// remote arrives with @ and the remote's name on the end.
func clNumber(name string) string {
	name, _, _ = strings.Cut(name, "@")
	rest, ok := strings.CutPrefix(name, "cl/")
	if !ok {
		return ""
	}
	num, _, _ := strings.Cut(rest, "/")
	if num == "" {
		return ""
	}
	for _, c := range num {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return num
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
