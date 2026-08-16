// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// testDB is the database the CLI commands under test should use. Flags
// come after the command, so it is passed as one rather than set here.
var testDB string

// cli prefixes -db so that a command under test uses the test's database.
// It goes first because flag parsing stops at the first argument that is
// not a flag.
func cli(args ...string) []string {
	return append([]string{"-db", testDB}, args...)
}

// inRepo points the CLI at a repository and its own database.
func inRepo(t *testing.T) (*Review, string) {
	t.Helper()
	r, dir := newReview(t)
	t.Chdir(dir)
	// The CLI opens its own handles, so give it the same database file.
	path := t.TempDir() + "/review.db"
	d, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	r.DB.Close()
	r.DB = d
	testDB = path
	t.Cleanup(func() { testDB = ""; dbFile = "" })
	return r, dir
}

// TestCommentsFollowsMovedLines is the case that matters for an agent: a
// comment written against an old snapshot must be reported at the line it
// occupies now, with the code around it.
func TestCommentsFollowsMovedLines(t *testing.T) {
	r, dir := inRepo(t)
	write(t, dir, "a.go", "package p\n\nfunc F() int {\n\treturn 1\n}\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "add a.go\n\nChange-Id: Imoved\n")

	changes, _ := r.Repo.Changes()
	snaps, err := r.EnsureSnapshot(changes[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.DB.AddThread(snaps[0].ID, "a.go", "new", 4, "\treturn 1", &Comment{
		Author: "rsc", Body: "should be 42", Draft: false,
	}); err != nil {
		t.Fatal(err)
	}

	// Insert two lines above it, in the working tree only.
	write(t, dir, "a.go", "package p\n\n// F is the answer.\n// Really.\nfunc F() int {\n\treturn 1\n}\n")

	out := captureOut(t)
	cmdComments(cli("-c", "1"))
	got := out.String()

	// The line moved from 4 to 6, and the report says so.
	if !strings.Contains(got, "a.go:6") {
		t.Errorf("comment not reported at its current line:\n%s", got)
	}
	if !strings.Contains(got, "written at line 4") {
		t.Errorf("report does not say where the comment was written:\n%s", got)
	}
	// The context is the text the comment was written against, at the
	// line it was written at, not the working tree's version: a comment
	// read next to code the reviewer never saw can make no sense.
	if !strings.Contains(got, ">     4  \treturn 1") {
		t.Errorf("context is not the snapshot text at its own line:\n%s", got)
	}
	if !strings.Contains(got, "as written against snapshot 1") {
		t.Errorf("context is not labelled with its snapshot:\n%s", got)
	}
	if strings.Contains(got, "F is the answer") {
		t.Errorf("context leaked lines that only exist in the working tree:\n%s", got)
	}
	if !strings.Contains(got, "func F() int {") {
		t.Errorf("no surrounding context:\n%s", got)
	}
	if !strings.Contains(got, "should be 42") {
		t.Errorf("comment body missing:\n%s", got)
	}

	// The JSON form carries the same facts.
	out = captureOut(t)
	cmdComments(cli("-json", "-c", "1"))
	var js []jsonThread
	if err := json.Unmarshal([]byte(out.String()), &js); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out.String())
	}
	if len(js) != 1 {
		t.Fatalf("got %d threads, want 1", len(js))
	}
	if js[0].Line != 4 || js[0].CurrentLine != 6 || js[0].Stale {
		t.Errorf("line=%d current_line=%d stale=%v; want 4, 6, false", js[0].Line, js[0].CurrentLine, js[0].Stale)
	}
	var anchored int
	for _, l := range js[0].Context {
		if l.Anchor {
			anchored++
			// Numbered by the snapshot it came from, which is what "line"
			// means; "current_line" is the other one.
			if l.Line != 4 || l.Text != "\treturn 1" {
				t.Errorf("anchor context line = %+v", l)
			}
		}
	}
	if anchored != 1 {
		t.Errorf("%d context lines marked as the anchor, want 1", anchored)
	}
}

// TestCommentsReportsGoneLine checks what an agent sees when the code a
// comment was written against has been deleted outright.
func TestCommentsReportsGoneLine(t *testing.T) {
	r, dir := inRepo(t)
	write(t, dir, "a.go", "package p\n\nfunc F() int {\n\treturn 1\n}\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "add a.go\n\nChange-Id: Igone\n")

	changes, _ := r.Repo.Changes()
	snaps, _ := r.EnsureSnapshot(changes[0])
	r.DB.AddThread(snaps[0].ID, "a.go", "new", 4, "\treturn 1", &Comment{
		Author: "rsc", Body: "why 1?", Draft: false,
	})
	write(t, dir, "a.go", "package p\n\nfunc F() string {\n\treturn \"x\"\n}\n")

	out := captureOut(t)
	cmdComments(cli())
	got := out.String()
	if !strings.Contains(got, "written at line 4, now gone") {
		t.Errorf("report does not say the line is gone:\n%s", got)
	}
	// The snapshot text is the only remaining record of what was meant,
	// so it still has to be shown.
	if !strings.Contains(got, ">     4  \treturn 1") {
		t.Errorf("report does not show the code the comment was written against:\n%s", got)
	}
	if strings.Contains(got, "return \"x\"") {
		t.Errorf("showed the current code instead of the original:\n%s", got)
	}

	out = captureOut(t)
	cmdComments(cli("-json"))
	var js []jsonThread
	if err := json.Unmarshal([]byte(out.String()), &js); err != nil {
		t.Fatal(err)
	}
	if !js[0].Stale || js[0].CurrentLine != 0 || js[0].Anchor != "\treturn 1" {
		t.Errorf("json = %+v, want stale with the anchor text", js[0])
	}
}

// TestReplyFrom checks that -from names the author and defaults to agent.
func TestReplyFrom(t *testing.T) {
	r, dir := inRepo(t)
	write(t, dir, "a.go", "package p\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "add a.go\n\nChange-Id: Ifrom\n")
	changes, _ := r.Repo.Changes()
	snaps, _ := r.EnsureSnapshot(changes[0])
	th, err := r.DB.AddThread(snaps[0].ID, "a.go", "new", 1, "package p", &Comment{
		Author: "rsc", Body: "hm", Draft: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Default attribution is "agent".
	out := captureOut(t)
	cmdReply(cli(strconv.FormatInt(th.ID, 10), "done"))
	if !strings.Contains(out.String(), "replied to thread") {
		t.Errorf("reply not reported:\n%s", out.String())
	}
	got, _ := r.DB.Thread(th.ID)
	last := got.Comments[len(got.Comments)-1]
	if last.Author != "agent" || !last.FromAgent || last.Draft {
		t.Errorf("default reply = %+v; want author agent, from agent, published", last)
	}

	// A name overrides it, and is still marked as not the reviewer's.
	captureOut(t)
	cmdReply(cli("-from", "claude", "-resolve", strconv.FormatInt(th.ID, 10), "and resolved"))
	got, _ = r.DB.Thread(th.ID)
	last = got.Comments[len(got.Comments)-1]
	if last.Author != "claude" || !last.FromAgent {
		t.Errorf("reply = %+v; want author claude, from agent", last)
	}
	if !got.Resolved {
		t.Error("-resolve did not resolve the thread")
	}
}

func TestCommentsHidesDrafts(t *testing.T) {
	r, dir := inRepo(t)
	write(t, dir, "a.go", "package p\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "add a.go\n\nChange-Id: Idraft\n")
	changes, _ := r.Repo.Changes()
	snaps, _ := r.EnsureSnapshot(changes[0])
	r.DB.AddThread(snaps[0].ID, "a.go", "new", 1, "package p", &Comment{
		Author: "rsc", Body: "unfinished thought", Draft: true,
	})

	out := captureOut(t)
	cmdComments(cli())
	if !strings.Contains(out.String(), "no comments") {
		t.Errorf("drafts are visible to an agent:\n%s", out.String())
	}
	out = captureOut(t)
	cmdComments(cli("-drafts"))
	if !strings.Contains(out.String(), "unfinished thought") {
		t.Errorf("-drafts did not show the draft:\n%s", out.String())
	}
}

// TestCommentsOldestCommitFirst checks that comments are printed with the
// base of a stack before what is built on top of it, which is the order
// they have to be addressed in.
func TestCommentsOldestCommitFirst(t *testing.T) {
	r, dir := inRepo(t)

	// Two stacked commits: base, then top on top of it.
	write(t, dir, "base.go", "package p\n\nfunc Base() {}\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "p: add Base\n\nChange-Id: Ibase\n")
	write(t, dir, "top.go", "package p\n\nfunc Top() {}\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "p: add Top\n\nChange-Id: Itop\n")

	changes, err := r.Repo.Changes()
	if err != nil {
		t.Fatal(err)
	}
	// The repository lists children before parents, so the newest is first.
	if len(changes) != 2 || changes[0].Key != "Itop" || changes[1].Key != "Ibase" {
		t.Fatalf("changes = %+v, want Itop then Ibase", changes)
	}
	for _, c := range changes {
		snaps, err := r.EnsureSnapshot(c)
		if err != nil {
			t.Fatal(err)
		}
		file := "base.go"
		if c.Key == "Itop" {
			file = "top.go"
		}
		if _, err := r.DB.AddThread(snaps[0].ID, file, "new", 3, "func "+c.Key, &Comment{
			Author: "rsc", Body: "remark on " + c.Key, Draft: false,
		}); err != nil {
			t.Fatal(err)
		}
	}

	out := captureOut(t)
	cmdComments(cli("-c", "0"))
	got := out.String()

	i := strings.Index(got, "remark on Ibase")
	j := strings.Index(got, "remark on Itop")
	if i < 0 || j < 0 {
		t.Fatalf("both comments should be listed:\n%s", got)
	}
	if i > j {
		t.Errorf("the newer commit was printed first:\n%s", got)
	}

	// The JSON form is ordered the same way, since an agent reads it in
	// sequence just as a person reads the text.
	out = captureOut(t)
	cmdComments(cli("-json"))
	var js []jsonThread
	if err := json.Unmarshal([]byte(out.String()), &js); err != nil {
		t.Fatal(err)
	}
	if len(js) != 2 {
		t.Fatalf("got %d threads, want 2", len(js))
	}
	if js[0].Change != "Ibase" || js[1].Change != "Itop" {
		t.Errorf("JSON order = %s then %s, want Ibase then Itop", js[0].Change, js[1].Change)
	}
}

func TestParseFileLine(t *testing.T) {
	tests := []struct {
		arg  string
		file string
		line int
		bad  bool
	}{
		{arg: "a.go:42", file: "a.go", line: 42},
		{arg: "dir/a.go:1", file: "dir/a.go", line: 1},
		{arg: "a.go", file: "a.go"},
		{arg: "/COMMIT_MSG", file: "/COMMIT_MSG"},
		{arg: "/COMMIT_MSG:3", file: "/COMMIT_MSG", line: 3},
		// A colon that is not followed by a number is part of the name.
		{arg: "odd:name.go", file: "odd:name.go"},
		{arg: "a.go:0", bad: true},
		{arg: "a.go:-1", bad: true},
		{arg: "", bad: true},
	}
	for _, tt := range tests {
		file, line, err := parseFileLine(tt.arg)
		if tt.bad {
			if err == nil {
				t.Errorf("parseFileLine(%q) = %q, %d; want an error", tt.arg, file, line)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseFileLine(%q): %v", tt.arg, err)
		} else if file != tt.file || line != tt.line {
			t.Errorf("parseFileLine(%q) = %q, %d; want %q, %d", tt.arg, file, line, tt.file, tt.line)
		}
	}
}

// TestAdd covers starting a thread from the command line: it lands on the
// newest snapshot, records the line's text so it can be found again, and
// comes out of "review comments" like any other thread.
func TestAdd(t *testing.T) {
	r, dir := inRepo(t)
	write(t, dir, "a.go", "package p\n\nfunc F() {\n\tclose()\n}\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "add a.go\n\nChange-Id: Iadd\n")
	if _, err := r.Repo.Changes(); err != nil {
		t.Fatal(err)
	}

	out := captureOut(t)
	cmdAdd(cli("Iadd", "a.go:4", "This drops the error from close."))
	if !strings.Contains(out.String(), "added thread") {
		t.Fatalf("add not reported:\n%s", out.String())
	}

	threads, err := r.DB.Threads(r.Root(), "Iadd")
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 {
		t.Fatalf("got %d threads, want 1", len(threads))
	}
	th := threads[0]
	if th.File != "a.go" || th.Line != 4 || th.Side != "new" {
		t.Errorf("thread = %+v; want a.go line 4 on the new side", th)
	}
	if th.AnchorText != "\tclose()" {
		t.Errorf("AnchorText = %q, want the text of line 4", th.AnchorText)
	}
	if th.Resolved {
		t.Error("a new thread is already resolved")
	}
	if len(th.Comments) != 1 {
		t.Fatalf("thread has %d comments", len(th.Comments))
	}
	// Published at once and marked as not the reviewer's, like a reply.
	if c := th.Comments[0]; c.Author != "agent" || !c.FromAgent || c.Draft {
		t.Errorf("comment = %+v; want author agent, from agent, published", c)
	}

	// It is a real thread: the agent's own reading of the comments finds it.
	out.Reset()
	cmdComments(cli())
	if !strings.Contains(out.String(), "This drops the error from close.") {
		t.Errorf("added thread missing from comments:\n%s", out.String())
	}
}

func TestAddFileLevelAndCommitMsg(t *testing.T) {
	r, dir := inRepo(t)
	write(t, dir, "a.go", "package p\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "add a.go\n\nChange-Id: Iwhole\n")

	out := captureOut(t)
	cmdAdd(cli("-from", "claude", "Iwhole", "a.go", "This file needs a doc comment."))
	cmdAdd(cli("Iwhole", CommitMsgFile+":1", "Subject should say why."))
	_ = out

	threads, err := r.DB.Threads(r.Root(), "Iwhole")
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 2 {
		t.Fatalf("got %d threads, want 2", len(threads))
	}
	byFile := map[string]*Thread{}
	for _, th := range threads {
		byFile[th.File] = th
	}
	// A file-level comment has no line and no anchor text to lose.
	whole := byFile["a.go"]
	if whole == nil || whole.Line != 0 || whole.AnchorText != "" {
		t.Errorf("file-level thread = %+v", whole)
	}
	if whole != nil && whole.Comments[0].Author != "claude" {
		t.Errorf("-from ignored: %+v", whole.Comments[0])
	}
	// The commit message is commentable like any other file.
	if msg := byFile[CommitMsgFile]; msg == nil || msg.Line != 1 {
		t.Errorf("commit message thread = %+v", msg)
	}
}

// TestAddFileOutsideTheChange checks that a comment can be written on a
// file the change does not touch. Such a file still turns up in the diff
// of one snapshot against another, carried in by a rebase, and asking why
// it moved is a fair question to ask.
func TestAddFileOutsideTheChange(t *testing.T) {
	r, dir := inRepo(t)
	write(t, dir, "a.go", "package p\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "add a.go\n\nChange-Id: Ioutside\n")

	// base.txt belongs to the base commit; this change never touches it.
	out := captureOut(t)
	cmdAdd(cli("Ioutside", "base.txt:1", "Why did this move?"))
	if !strings.Contains(out.String(), "added thread") {
		t.Fatalf("add refused a file outside the change:\n%s", out.String())
	}
	threads, err := r.DB.Threads(r.Root(), "Ioutside")
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0].File != "base.txt" || threads[0].AnchorText != "base" {
		t.Fatalf("thread = %+v", threads[0])
	}

	// The guards that remain — an unknown file, a line past the end of one
	// that exists — end the process, so they are exercised by hand rather
	// than here.
}
