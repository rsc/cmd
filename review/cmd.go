// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// newFlags returns a flag set for a subcommand.
// newFlags returns a flag set for a subcommand. Every command takes -db,
// so that flags always follow the command they belong to.
func newFlags(name, args string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ExitOnError)
	f.StringVar(&dbFile, "db", "", "use `file` as the review database")
	f.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: review %s %s\n", name, args)
		f.PrintDefaults()
		os.Exit(2)
	}
	return f
}

// jsonThread is the shape an agent sees when it asks for JSON. Line is
// where the comment was written; CurrentLine is where that line has
// ended up in the working tree, which is what the agent is about to edit.
type jsonThread struct {
	ID          int64         `json:"id"`
	Change      string        `json:"change"`
	Subject     string        `json:"subject"`
	Snapshot    int           `json:"snapshot"`
	File        string        `json:"file"`
	Side        string        `json:"side"`
	Line        int           `json:"line"`
	CurrentLine int           `json:"current_line"`
	Stale       bool          `json:"stale"`
	Anchor      string        `json:"anchor,omitempty"`
	Context     []jsonLine    `json:"context,omitempty"`
	Resolved    bool          `json:"resolved"`
	Comments    []jsonComment `json:"comments"`
}

type jsonLine struct {
	Line   int    `json:"line"`
	Text   string `json:"text"`
	Anchor bool   `json:"anchor,omitempty"`
}

type jsonComment struct {
	Author    string `json:"author"`
	FromAgent bool   `json:"from_agent"`
	Draft     bool   `json:"draft"`
	Body      string `json:"body"`
	Time      string `json:"time"`
}

// A threadView is a thread with everything needed to act on it worked
// out: where its line lives now, and the code the comment was written
// against. Those are two different things, and both are wanted: the
// current line says where to make the change, while the snapshot's text
// says what the comment meant. Showing the current text instead would be
// showing the reviewer's words next to something they never saw.
type threadView struct {
	Thread  *Thread
	Snap    *Snapshot
	Line    int  // line in the file as it is now, 0 if it has gone
	Stale   bool // the line it was attached to could not be found
	Context []jsonLine
}

// currentLines returns the file a thread is attached to as it stands now,
// which is the working tree rather than any recorded snapshot: that is
// what the agent is about to edit.
func currentLines(r *Review, t *Thread, snap *Snapshot) []string {
	if t.File == CommitMsgFile {
		// The commit message has no file on disk. Use the change's message
		// as it stands, falling back to the one the snapshot recorded if
		// the change is no longer pending.
		if snap != nil {
			if c, err := r.Change(snap.Key); err == nil {
				lines, _ := splitLines(commitMsgContent(c))
				return lines
			}
			lines, _ := splitLines(commitMsgContent(&Change{
				Parent: snap.Parent, Author: snap.Author, Date: snap.Date, Message: snap.Message,
			}))
			return lines
		}
		return nil
	}
	data, err := os.ReadFile(filepath.Join(r.Root(), t.File))
	if err != nil {
		return nil
	}
	lines, _ := splitLines(data)
	return lines
}

// snapshotLines returns the file a thread is attached to as it stood in
// the snapshot the comment was written against.
func snapshotLines(r *Review, t *Thread, snap *Snapshot) []string {
	if snap == nil {
		return nil
	}
	// Side "new" is the snapshot's own content; side "old" is the parent
	// it was diffed against.
	rev := snap.Rev
	if t.Side == "old" {
		rev = snap.Parent
	}
	data, err := FileContent(r.Repo, rev, t.File)
	if err != nil {
		// The commit may have been collected if snapshots were not pinned.
		return nil
	}
	lines, _ := splitLines(data)
	return lines
}

// view works out where a thread's line is now, and gathers ctx lines of
// the text it was written against.
func view(r *Review, t *Thread, snap *Snapshot, ctx int) *threadView {
	v := &threadView{Thread: t, Snap: snap}
	v.Line, v.Stale = Anchor(t, currentLines(r, t, snap))

	// The context comes from the snapshot, not the working tree: it is
	// there to explain the comment, and the code may since have changed
	// past the point where the comment makes sense against it.
	lines := snapshotLines(r, t, snap)
	if t.Line <= 0 || t.Line > len(lines) || ctx < 0 {
		return v
	}
	lo := max(1, t.Line-ctx)
	hi := min(len(lines), t.Line+ctx)
	for n := lo; n <= hi; n++ {
		v.Context = append(v.Context, jsonLine{Line: n, Text: lines[n-1], Anchor: n == t.Line})
	}
	return v
}

func cmdComments(args []string) {
	f := newFlags("comments", "[-json] [-all] [-drafts] [-s n] [-c n] [change]")
	asJSON := f.Bool("json", false, "print comments as JSON instead of text")
	all := f.Bool("all", false, "include resolved threads")
	drafts := f.Bool("drafts", false, "include unpublished drafts")
	snapN := f.Int("s", 0, "only comments written against snapshot `n`")
	ctx := f.Int("c", 3, "show `n` lines of context around each comment")
	f.Parse(args)
	if f.NArg() > 1 {
		f.Usage()
	}

	r := open(true)
	defer r.DB.Close()

	var threads []*Thread
	var err error
	if f.NArg() == 1 {
		c, err := r.Change(f.Arg(0))
		if err != nil {
			log.Fatal(err)
		}
		if threads, err = r.DB.Threads(r.Root(), c.Key); err != nil {
			log.Fatal(err)
		}
	} else if threads, err = r.DB.AllThreads(r.Root()); err != nil {
		log.Fatal(err)
	}

	snaps := map[int64]*Snapshot{}
	var keep []*threadView
	for _, t := range threads {
		if t.Resolved && !*all {
			continue
		}
		if *snapN != 0 && t.SnapshotN != *snapN {
			continue
		}
		// Drafts are the reviewer's unfinished thoughts. An agent should
		// not act on them until they are published.
		var visible []*Comment
		for _, c := range t.Comments {
			if c.Draft && !*drafts {
				continue
			}
			visible = append(visible, c)
		}
		if len(visible) == 0 {
			continue
		}
		t.Comments = visible
		if _, ok := snaps[t.SnapshotID]; !ok {
			s, err := r.DB.SnapshotByID(t.SnapshotID)
			if err != nil {
				log.Fatal(err)
			}
			snaps[t.SnapshotID] = s
		}
		keep = append(keep, view(r, t, snaps[t.SnapshotID], *ctx))
	}

	sortByStack(r, keep)

	if *asJSON {
		printJSON(keep)
		return
	}
	printText(keep)
}

// sortByStack orders threads by the commit they belong to, oldest first,
// so that reading the output in order means fixing the base of a stack
// before whatever is built on top of it. Threads on the same commit keep
// the order the database gave them, by snapshot, file, and line.
func sortByStack(r *Review, views []*threadView) {
	// Changes come back with children before parents, so counting down
	// from the end puts the oldest commit first.
	rank := map[string]int{}
	if changes, err := r.Repo.Changes(); err == nil {
		for i, c := range changes {
			rank[c.Key] = len(changes) - i
		}
	}
	// A comment on a commit that is no longer pending has no place in the
	// stack; leave those at the end.
	const unknown = math.MaxInt
	at := func(v *threadView) int {
		if v.Snap == nil {
			return unknown
		}
		if n, ok := rank[v.Snap.Key]; ok {
			return n
		}
		return unknown
	}
	sort.SliceStable(views, func(i, j int) bool { return at(views[i]) < at(views[j]) })
}

func printJSON(views []*threadView) {
	out := []jsonThread{}
	for _, v := range views {
		t := v.Thread
		j := jsonThread{
			ID: t.ID, Snapshot: t.SnapshotN, File: t.File, Side: t.Side,
			Line: t.Line, CurrentLine: v.Line, Stale: v.Stale,
			Anchor: t.AnchorText, Context: v.Context, Resolved: t.Resolved,
		}
		if v.Snap != nil {
			j.Change, j.Subject = v.Snap.Key, v.Snap.Subject
		}
		for _, c := range t.Comments {
			j.Comments = append(j.Comments, jsonComment{
				Author: c.Author, FromAgent: c.FromAgent, Draft: c.Draft,
				Body: c.Body, Time: c.Created.Format("2006-01-02T15:04:05Z07:00"),
			})
		}
		out = append(out, j)
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		log.Fatal(err)
	}
}

func printText(views []*threadView) {
	if len(views) == 0 {
		fmt.Fprintln(stdout, "no comments")
		return
	}
	lastChange := ""
	for _, v := range views {
		t := v.Thread
		if v.Snap != nil && v.Snap.Key != lastChange {
			lastChange = v.Snap.Key
			fmt.Fprintf(stdout, "\n%s  %s\n", shortRev(v.Snap.Key), v.Snap.Subject)
		}

		where := t.Where()
		if t.Line > 0 && !v.Stale {
			where = fmt.Sprintf("%s:%d", displayFile(t.File), v.Line)
		}
		status := "unresolved"
		if t.Resolved {
			status = "resolved"
		}
		note := fmt.Sprintf("snapshot %d", t.SnapshotN)
		switch {
		case v.Stale:
			note += fmt.Sprintf(", written at line %d, now gone", t.Line)
		case v.Line != t.Line:
			note += fmt.Sprintf(", written at line %d", t.Line)
		}
		fmt.Fprintf(stdout, "\n  [%d] %s  %s  %s\n", t.ID, where, status, note)

		if len(v.Context) > 0 {
			fmt.Fprintf(stdout, "      as written against snapshot %d:\n", t.SnapshotN)
			for _, l := range v.Context {
				mark := " "
				if l.Anchor {
					mark = ">"
				}
				fmt.Fprintf(stdout, "    %s %5d  %s\n", mark, l.Line, l.Text)
			}
		}
		for _, c := range t.Comments {
			tag := ""
			if c.Draft {
				tag = " (draft)"
			}
			fmt.Fprintf(stdout, "      %s%s:\n", c.Author, tag)
			for line := range strings.Lines(strings.TrimRight(c.Body, "\n")) {
				fmt.Fprintf(stdout, "        %s", line)
				if !strings.HasSuffix(line, "\n") {
					fmt.Fprintln(stdout)
				}
			}
		}
	}
}

func displayFile(path string) string {
	if path == CommitMsgFile {
		return "Commit Message"
	}
	return path
}

func cmdReply(args []string) {
	f := newFlags("reply", "[-from name] [-resolve] thread [text]")
	from := f.String("from", "agent", "record the reply as coming from `name`")
	resolve := f.Bool("resolve", false, "mark the thread resolved")
	f.Parse(args)
	if f.NArg() < 1 || f.NArg() > 2 {
		f.Usage()
	}

	id, err := strconv.ParseInt(f.Arg(0), 10, 64)
	if err != nil {
		log.Fatalf("invalid thread %q", f.Arg(0))
	}
	body := f.Arg(1)
	if f.NArg() == 1 {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			log.Fatal(err)
		}
		body = string(data)
	}
	if strings.TrimSpace(body) == "" {
		log.Fatal("empty reply")
	}

	r := open(true)
	defer r.DB.Close()

	if strings.TrimSpace(*from) == "" {
		log.Fatal("empty -from name")
	}
	// Replies made here are published immediately: an agent has no publish
	// step, and its answers should be visible in the web UI at once. They
	// are also always marked as not coming from the reviewer, so the web UI
	// can draw them differently; the reviewer's own replies are written
	// there rather than here.
	if _, err := r.DB.AddComment(id, &Comment{
		Author: *from, Body: strings.TrimRight(body, "\n"), FromAgent: true, Draft: false,
	}); err != nil {
		log.Fatal(err)
	}
	if *resolve {
		if err := r.DB.SetResolved(id, true); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Fprintf(stdout, "replied to thread %d\n", id)
}

func cmdResolve(args []string, resolved bool) {
	name := "resolve"
	if !resolved {
		name = "unresolve"
	}
	f := newFlags(name, "thread...")
	f.Parse(args)
	if f.NArg() == 0 {
		f.Usage()
	}

	r := open(true)
	defer r.DB.Close()
	for _, arg := range f.Args() {
		id, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			log.Fatalf("invalid thread %q", arg)
		}
		if err := r.DB.SetResolved(id, resolved); err != nil {
			log.Fatal(err)
		}
		fmt.Fprintf(stdout, "%sd thread %d\n", name, id)
	}
}

func cmdPublish(args []string) {
	f := newFlags("publish", "[change]")
	f.Parse(args)
	if f.NArg() > 1 {
		f.Usage()
	}

	r := open(true)
	defer r.DB.Close()

	changes, err := r.Repo.Changes()
	if err != nil {
		log.Fatal(err)
	}
	if f.NArg() == 1 {
		c, err := r.Change(f.Arg(0))
		if err != nil {
			log.Fatal(err)
		}
		changes = []*Change{c}
	}
	total := 0
	for _, c := range changes {
		n, err := r.DB.Publish(r.Root(), c.Key)
		if err != nil {
			log.Fatal(err)
		}
		total += n
	}
	fmt.Fprintf(stdout, "published %d draft comment%s\n", total, plural(total))
}

func cmdSnapshot(args []string) {
	f := newFlags("snapshot", "[-nopin] [change...]")
	nopin := f.Bool("nopin", false, "do not record a git ref pinning the snapshot")
	f.Parse(args)

	r := open(*nopin)
	defer r.DB.Close()

	var changes []*Change
	if f.NArg() == 0 {
		all, err := r.Repo.Changes()
		if err != nil {
			log.Fatal(err)
		}
		changes = all
	} else {
		for _, arg := range f.Args() {
			c, err := r.Change(arg)
			if err != nil {
				log.Fatal(err)
			}
			changes = append(changes, c)
		}
	}

	for _, c := range changes {
		if c.Working {
			fmt.Fprintf(stdout, "skipping uncommitted changes: nothing to snapshot\n")
			continue
		}
		s, created, err := r.Grab(c)
		if err != nil {
			log.Fatal(err)
		}
		if created {
			fmt.Fprintf(stdout, "%s  snapshot %d  %s\n", shortRev(c.Key), s.N, c.Subject)
		} else {
			fmt.Fprintf(stdout, "%s  unchanged since snapshot %d  %s\n", shortRev(c.Key), s.N, c.Subject)
		}
	}
}

func cmdSnapshots(args []string) {
	f := newFlags("snapshots", "[change]")
	f.Parse(args)
	if f.NArg() > 1 {
		f.Usage()
	}

	r := open(true)
	defer r.DB.Close()

	changes, err := r.Repo.Changes()
	if err != nil {
		log.Fatal(err)
	}
	if f.NArg() == 1 {
		c, err := r.Change(f.Arg(0))
		if err != nil {
			log.Fatal(err)
		}
		changes = []*Change{c}
	}
	for _, c := range changes {
		if c.Working {
			continue
		}
		snaps, err := r.DB.Snapshots(r.Root(), c.Key)
		if err != nil {
			log.Fatal(err)
		}
		if len(snaps) == 0 {
			continue
		}
		fmt.Fprintf(stdout, "%s  %s\n", shortRev(c.Key), c.Subject)
		for _, s := range snaps {
			fmt.Fprintf(stdout, "  %d  %s  %s  %s\n", s.N, s.ShortRev(), s.Created.Format("2006-01-02 15:04"), s.Subject)
		}
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
