// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"slices"
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
	// Working reports that this change has nothing of its own yet to
	// snapshot: git's synthetic uncommitted-working-tree change, or jj's
	// working-copy commit while it is still empty and undescribed — the
	// state jj itself treats as disposable, replacing it the moment real
	// work starts. Neither is durable enough to record a snapshot of.
	Working bool

	Current bool // jj's working-copy commit, @
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

// A GraphRow is one row of a list of changes drawn with a graph beside it:
// a change, or, where Change is nil, a row of graph alone, joining a branch
// back to the line below it.
//
// The row says what its graph holds, not how to draw it: which lanes it
// passes through, and where its own node or corner sits. Drawing it is the
// caller's business — see GraphRow.Graph.
type GraphRow struct {
	*Change

	// Lanes reports, for each lane, whether a line runs the whole height of
	// this row: a branch neither starting nor ending here, passing by. The
	// row's own lane, Col, is never one of them.
	Lanes []bool

	// Col is the lane the row draws in: the change's node, or, on a join
	// row, the lane the branch coming down Join joins into.
	Col int

	// Up and Down report that the change's own line carries on out of the
	// row: up to what is sitting on it, down to what it sits on.
	Up, Down bool

	// Join, on a join row, is the lane whose line ends here, curving left
	// into Col. It is -1 on a change's row.
	Join int

	// More reports that this row stands for the changes left out at one
	// end of a chain that was trimmed. Its lanes are the ones whose lines
	// cross the cut, drawn running on past it. See window.
	More bool
}

// chain returns the changes connected to c by parent links: the stack c
// sits in, whether above it, below it, or on a branch beside it, drawn as
// graph rows in the order the change list already has them. It returns nil
// when c is connected to nothing, since a chain of one is only the change
// already on the screen.
//
// Only first parents are followed, so the chain is a tree. A merge in the
// pending set therefore hangs off the side it was made from, which is also
// the side its diff is against.
func chain(changes []*Change, c *Change) []GraphRow {
	byRev := make(map[string]*Change, len(changes))
	for _, x := range changes {
		byRev[x.Rev] = x
	}
	kids := make(map[string][]*Change)
	for _, x := range changes {
		if p := byRev[x.Parent]; p != nil {
			kids[p.Rev] = append(kids[p.Rev], x)
		}
	}
	if c = byRev[c.Rev]; c == nil {
		return nil
	}

	// Walk out from c both ways: down to what it sits on, and up to
	// everything sitting on it. What is reached is the whole stack, since
	// the commit the stack was started on is not pending and so not here
	// to connect one stack to the next.
	in := make(map[string]bool)
	var walk func(*Change)
	walk = func(x *Change) {
		if in[x.Rev] {
			return
		}
		in[x.Rev] = true
		if p := byRev[x.Parent]; p != nil {
			walk(p)
		}
		for _, k := range kids[x.Rev] {
			walk(k)
		}
	}
	walk(c)
	if len(in) < 2 {
		return nil
	}

	var stack []*Change
	for _, x := range changes {
		if in[x.Rev] {
			stack = append(stack, x)
		}
	}
	return graph(stack)
}

// The relation chain shows at most chainMax changes: the one being viewed,
// chainSide of them above it and chainSide below. Where one side has fewer
// than its share, the other takes what is left over, so that the chain is
// as long as it is allowed to be wherever there is enough of it to be.
const (
	chainSide = 5
	chainMax  = 2*chainSide + 1
)

// window trims a chain to the changes nearest the one being viewed, at
// rev. An end named by up or down is left whole instead, which is what
// clicking the ... at a trimmed end asks for.
//
// A trimmed end gets a row of its own standing for what was left out,
// carrying the lanes whose lines cross the cut so that the graph shows
// them running on past it rather than stopping there.
func window(rows []GraphRow, rev string, up, down bool) []GraphRow {
	self, above, below := -1, 0, 0
	for i, r := range rows {
		switch {
		case r.Change == nil:
		case r.Rev == rev:
			self = i
		case self < 0:
			above++
		default:
			below++
		}
	}
	if self < 0 {
		return rows
	}

	takeAbove, takeBelow := min(above, chainSide), min(below, chainSide)
	// A side with fewer than its share leaves the rest to the other.
	if spare := chainMax - 1 - takeAbove - takeBelow; spare > 0 {
		if takeAbove == above {
			takeBelow = min(below, takeBelow+spare)
		} else {
			takeAbove = min(above, takeAbove+spare)
		}
	}
	if up {
		takeAbove = above
	}
	if down {
		takeBelow = below
	}
	if takeAbove == above && takeBelow == below {
		return rows
	}

	lo, hi := self, self
	for n := takeAbove; n > 0; {
		lo--
		if rows[lo].Change != nil {
			n--
		}
	}
	for n := takeBelow; n > 0; {
		hi++
		if rows[hi].Change != nil {
			n--
		}
	}
	out := append([]GraphRow{}, rows[lo:hi+1]...)
	if takeAbove < above {
		out = append([]GraphRow{more(out[0], true)}, out...)
	}
	if takeBelow < below {
		out = append(out, more(out[len(out)-1], false))
	}

	// A lane only the changes left out were using would draw an empty
	// column, so the width is taken from what is left rather than kept.
	w := 0
	for _, r := range out {
		w = max(w, r.Col+1, r.Join+1)
		for i, on := range r.Lanes {
			if on {
				w = max(w, i+1)
			}
		}
	}
	for i := range out {
		out[i].Lanes = out[i].Lanes[:w]
	}
	return out
}

// more returns the row standing for the changes left out beyond r, which
// is the first row of the window when above is set and the last when it
// is not. Every line crossing the cut is carried into it, the change's
// own included.
func more(r GraphRow, above bool) GraphRow {
	m := GraphRow{More: true, Col: -1, Join: -1, Lanes: slices.Clone(r.Lanes)}
	if above && r.Up || !above && r.Down {
		m.Lanes[r.Col] = true
	}
	return m
}

// graph draws a set of changes as jj log draws one. Each line of
// development holds a lane, a change is drawn on the lane waiting for it,
// and where two lanes come to be waiting for the same change below they
// join at once, in a row of their own, rather than running side by side
// down to it.
//
// The changes keep the order they arrive in, which is the order the change
// list shows them in: newest first. That order is already a valid one to
// draw, since a change is always newer than the change it sits on — except
// where a clock has lied, which topo puts right.
func graph(changes []*Change) []GraphRow {
	byRev := make(map[string]*Change, len(changes))
	for _, x := range changes {
		byRev[x.Rev] = x
	}
	parent := func(x *Change) string {
		if p := byRev[x.Parent]; p != nil {
			return p.Rev
		}
		return ""
	}

	var rows []GraphRow
	var lanes []string // what each lane is waiting for; "" is free
	for _, x := range topo(changes, parent) {
		col := -1
		for i, w := range lanes {
			if w == x.Rev {
				col = i
				break
			}
		}
		up := col >= 0
		if !up {
			// A branch nothing is waiting on yet: the tip of the chain, or
			// of one of its branches. It starts a lane of its own.
			col = slices.Index(lanes, "")
			if col < 0 {
				col = len(lanes)
				lanes = append(lanes, "")
			}
		}
		row := GraphRow{Change: x, Col: col, Up: up, Join: -1, Lanes: passing(lanes, col, -1)}
		lanes[col] = parent(x)
		row.Down = lanes[col] != ""
		rows = append(rows, row)

		// Nothing has two parents here, so at most one other lane can have
		// come to be waiting for the same change as this one.
		if lanes[col] != "" {
			for i, w := range lanes {
				if i != col && w == lanes[col] {
					a, b := min(i, col), max(i, col)
					rows = append(rows, GraphRow{Col: a, Join: b, Lanes: passing(lanes, a, b)})
					lanes[b] = ""
					break
				}
			}
		}
		for len(lanes) > 0 && lanes[len(lanes)-1] == "" {
			lanes = lanes[:len(lanes)-1]
		}
	}

	// Every row is given the same number of lanes, so that what is drawn
	// beside the graph starts in the same place all the way down.
	w := 0
	for _, r := range rows {
		w = max(w, len(r.Lanes), r.Col+1, r.Join+1)
	}
	for i, r := range rows {
		rows[i].Lanes = append(r.Lanes, make([]bool, w-len(r.Lanes))...)
	}
	return rows
}

// topo returns the changes in an order the graph can be drawn in: every
// change after everything sitting on it, so that a line always runs down
// the page from a change to what it sits on.
//
// The order given is kept wherever it already satisfies that, which is
// everywhere the commit times are honest, since a change is written after
// the change it sits on. Where they are not — a clock set wrong, or a
// commit date carried across a rebase — the change is held back until
// what sits on it has been drawn, rather than left dangling.
func topo(changes []*Change, parent func(*Change) string) []*Change {
	kids := make(map[string]int)
	for _, x := range changes {
		if p := parent(x); p != "" {
			kids[p]++
		}
	}
	out := make([]*Change, 0, len(changes))
	done := make(map[string]bool)
	for len(out) < len(changes) {
		n := len(out)
		for _, x := range changes {
			if done[x.Rev] || kids[x.Rev] > 0 {
				continue
			}
			done[x.Rev] = true
			out = append(out, x)
			if p := parent(x); p != "" {
				kids[p]--
			}
		}
		// A cycle cannot happen in a commit graph, but a corrupt one must
		// not spin here: draw what is left in the order it came in.
		if len(out) == n {
			for _, x := range changes {
				if !done[x.Rev] {
					out = append(out, x)
				}
			}
			break
		}
	}
	return out
}

// passing reports which lanes have a line running the whole height of a
// row: every open lane except the one or two the row draws in itself.
func passing(lanes []string, col, join int) []bool {
	out := make([]bool, len(lanes))
	for i, w := range lanes {
		out[i] = w != "" && i != col && i != join
	}
	return out
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
