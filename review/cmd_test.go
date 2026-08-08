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
