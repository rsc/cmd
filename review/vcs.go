// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// CommitMsgFile is the path of the synthetic file holding a change's commit
// message. Like Gerrit, review presents the commit message as the first file
// of every change, so that it can be reviewed and commented on like any other.
const CommitMsgFile = "/COMMIT_MSG"

// WorkingRev names the uncommitted working tree. The git backend uses it as
// the revision of its synthetic working-tree change. In jj the working copy
// is an ordinary commit, so it needs no special case.
const WorkingRev = "working"

// emptyTree is git's hash of the empty tree, used as the base of a root commit.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// pinRef is the ref namespace holding snapshot pins. It is deliberately
// not refs/review, which reads too much like jj-codereview's
// refs/remotes/review labels for its patch sets.
const pinRef = "refs/reviewed/"

// zeroID is jj's commit ID for the virtual root commit.
const zeroID = "0000000000000000000000000000000000000000"

// A Change is one commit under review.
type Change struct {
	Key     string // stable identity, surviving amends; see changeKey
	Rev     string // commit ID, or WorkingRev
	Parent  string // parent commit ID, the default diff base
	Subject string // first line of Message

	// ParentKey is the parent commit's stable identity, when there is one:
	// its jj change ID, or its Change-Id trailer. Unlike Parent it does not
	// move when the commit below is amended, so the commit message file can
	// name the parent in a way that only changes when the parent really does.
	ParentKey string

	Message string    // full commit message
	Author  string    // "Name <email>"
	Date    time.Time // author date
	Working bool      // uncommitted working tree, which cannot be snapshotted
	Current bool      // jj's working-copy commit, @
}

// ShortRev returns an abbreviated form of c.Rev for display.
func (c *Change) ShortRev() string {
	return shortRev(c.Rev)
}

func shortRev(rev string) string {
	if rev == WorkingRev {
		return rev
	}
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

// A File is one file changed by a change.
type File struct {
	Path    string // path in the new revision
	OldPath string // path in the old revision, if renamed or copied
	Status  byte   // 'A' added, 'M' modified, 'D' deleted, 'R' renamed, 'C' copied

	// A zero Status means the file is not in the diff at all, and is
	// listed only because it carries comments. See addCommentedFiles.
}

// Char is the status letter for display. A file listed only for its
// comments has none: the change did nothing to it.
func (f *File) Char() string {
	if f.Status == 0 {
		return ""
	}
	return string(f.Status)
}

// Old reports the path this file had in the base revision.
func (f *File) Old() string {
	if f.OldPath != "" {
		return f.OldPath
	}
	return f.Path
}

// A Repo is a version control repository holding changes to review.
type Repo interface {
	// Kind returns "git" or "jj".
	Kind() string

	// Root returns the absolute path of the repository root.
	Root() string

	// Changes returns the changes available for review, newest first.
	Changes() ([]*Change, error)

	// Commit returns the metadata for a single revision.
	Commit(rev string) (*Change, error)

	// Files returns the files that differ between base and rev.
	Files(base, rev string) ([]*File, error)

	// Content returns the contents of path at rev. The caller must only
	// ask for paths that exist at rev.
	Content(rev, path string) ([]byte, error)

	// Pin records a reference to rev under the given name, so that the
	// commit survives garbage collection after it has been amended away.
	Pin(name, rev string) error

	// EnsureStableKey returns a stable identity for rev, minting and
	// recording one if rev does not already have one on its own — a git
	// commit with no Change-Id trailer, whose only identity is otherwise
	// its hash, which does not survive an amend. It is a no-op returning
	// rev unchanged wherever every commit already has a stable identity,
	// as jj's do and a git commit with a Change-Id does.
	EnsureStableKey(rev string) (string, error)
}

// ErrNoRepo reports that a directory is not inside a repository, which
// callers distinguish from a repository that is there but broken.
var ErrNoRepo = errors.New("no jj or git repository")

// OpenRepo returns the repository containing dir, preferring jj when the
// directory is in both a jj and a git repository.
func OpenRepo(dir string) (Repo, error) {
	if out, err := run(dir, "jj", "root"); err == nil {
		root := strings.TrimSpace(string(out))
		return &jjRepo{root: root}, nil
	}
	if out, err := run(dir, "git", "rev-parse", "--show-toplevel"); err == nil {
		root := strings.TrimSpace(string(out))
		return &gitRepo{root: root}, nil
	}
	return nil, fmt.Errorf("%w at %s", ErrNoRepo, dir)
}

// FileContent returns the contents of path at rev, rendering the commit
// message pseudo-file when asked for it.
func FileContent(r Repo, rev, path string) ([]byte, error) {
	if path == CommitMsgFile {
		if rev == "" || rev == emptyTree || rev == zeroID {
			return nil, nil
		}
		c, err := r.Commit(rev)
		if err != nil {
			return nil, err
		}
		return commitMsgContent(r.Kind(), c), nil
	}
	return r.Content(rev, path)
}

// commitMsgContent renders a change's commit message as a reviewable file,
// with a header naming the parent, author, and date, as Gerrit does.
//
// The parent is named twice where it can be: once by its commit ID, which
// changes whenever the commit below is amended, and once by its stable
// identity, which does not. Stacked commits are rebased constantly, so the
// commit ID alone reports a change on the parent line every time anything
// below moves. The stable line stays put through all of that, and changes
// only when the change really is sitting on something else — which is
// worth noticing, and easy to miss in a line that always changes.
func commitMsgContent(kind string, c *Change) []byte {
	var b strings.Builder
	head := [][2]string{}
	if c.ParentKey != "" {
		label := "Change Parent:"
		if kind == "jj" {
			label = "JJ Parent:"
		}
		head = append(head, [2]string{label, c.ParentKey}, [2]string{"Git Parent:", shortRev(c.Parent)})
	} else {
		head = append(head, [2]string{"Parent:", shortRev(c.Parent)})
	}
	head = append(head,
		[2]string{"Author:", c.Author},
		[2]string{"Date:", c.Date.Format("Mon Jan 2 15:04:05 2006 -0700")})

	// Values line up in a column, at least as far out as the header has
	// always put them, so that adding a second parent line does not shift
	// every commit message and show up as a diff in every change.
	col := 12
	for _, h := range head {
		col = max(col, len(h[0])+1)
	}
	for _, h := range head {
		fmt.Fprintf(&b, "%-*s%s\n", col, h[0], h[1])
	}
	b.WriteString("\n")
	msg := c.Message
	if msg == "" {
		msg = "(no description set)"
	}
	b.WriteString(strings.TrimRight(msg, "\n"))
	b.WriteString("\n")
	return []byte(b.String())
}

// subject returns the first line of a commit message.
func subject(msg string) string {
	s := strings.TrimLeft(msg, "\n")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "(no description set)"
	}
	return s
}

// run runs a command in dir and returns its standard output.
func run(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("%s: %v: %s", strings.Join(args, " "), err, msg)
		}
		return nil, fmt.Errorf("%s: %v", strings.Join(args, " "), err)
	}
	return out, nil
}

// records splits output produced by a template using sep as a record
// terminator, dropping the empty tail and any newline the command added
// between records.
func records(out []byte, sep byte) []string {
	var recs []string
	for r := range strings.SplitSeq(string(out), string(sep)) {
		r = strings.TrimLeft(r, "\n")
		if r != "" {
			recs = append(recs, r)
		}
	}
	return recs
}

// refName converts a change key into a name safe to use in a git ref.
// Keys are commit hashes, jj change IDs, or Gerrit Change-Id trailers,
// all of which are already safe; this only guards against surprises.
func refName(key string) string {
	var b strings.Builder
	for _, c := range key {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "unnamed"
	}
	return b.String()
}
