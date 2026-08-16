// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTestServer builds a git repository with one committed change and one
// uncommitted file, and serves it.
func newTestServer(t *testing.T) (*server, *Review, string) {
	t.Helper()
	r, dir := newReview(t)
	write(t, dir, "a.go", "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "add a.go\n\nChange-Id: Itest1\n")
	return newServer(r.DB, r.Root(), r.Pin), r, dir
}

// repoURL turns a repository-relative path into a request path, since
// every page now lives under the repository's short name.
func repoURL(t *testing.T, r *Review, path string) string {
	t.Helper()
	if r.Name == "" {
		name, err := r.DB.RepoName(r.Root())
		if err != nil {
			t.Fatal(err)
		}
		r.Name = name
	}
	return "/" + r.Name + path
}

func get(t *testing.T, s *server, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func post(t *testing.T, s *server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

func mustGet(t *testing.T, s *server, path string) string {
	t.Helper()
	w := get(t, s, path)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

func TestServerChangeList(t *testing.T) {
	s, r, _ := newTestServer(t)
	body := mustGet(t, s, repoURL(t, r, ""))
	for _, want := range []string{"add a.go", "Snapshot all", "review"} {
		if !strings.Contains(body, want) {
			t.Errorf("change list missing %q", want)
		}
	}
	// The keyboard table must be present for keys.js to dispatch from.
	if !strings.Contains(body, `id="keymap"`) {
		t.Error("change list has no keymap")
	}
	// Its links stay inside the repository.
	if !strings.Contains(body, `data-href="/`+r.Name+`/c/Itest1"`) {
		t.Errorf("change does not link within the repository:\n%s", body)
	}
}

// TestServerAllRepos checks the home page: every repository's pending
// changes in one list, newest commit first, each naming where it lives.
func TestServerAllRepos(t *testing.T) {
	s, r, _ := newTestServer(t)

	// A second repository, committed later, so it must sort first. The
	// date is explicit because git timestamps have one-second resolution
	// and both commits would otherwise land in the same second.
	other, dirOther := newReview(t)
	other.DB.Close()
	other.DB = r.DB
	write(t, dirOther, "b.go", "package other\n")
	do(t, dirOther, "git", "add", ".")
	do(t, dirOther, "git", "commit", "-q", "--date=2030-01-01T00:00:00", "-m", "add b.go\n\nChange-Id: Iother\n")
	// Reviewing it is what puts it on the home page.
	if _, err := other.DB.RepoName(other.Root()); err != nil {
		t.Fatal(err)
	}
	changes, err := other.Repo.Changes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.EnsureSnapshot(changes[0]); err != nil {
		t.Fatal(err)
	}

	body := mustGet(t, s, "/")
	if !strings.Contains(body, "add a.go") || !strings.Contains(body, "add b.go") {
		t.Fatalf("home page does not list both repositories:\n%s", body)
	}
	// Newest commit first.
	if i, j := strings.Index(body, "add b.go"), strings.Index(body, "add a.go"); i > j {
		t.Error("changes are not sorted with the newest commit first")
	}
	// Each row names its repository and links into it.
	nameA, _ := r.DB.RepoName(r.Root())
	nameB, _ := r.DB.RepoName(other.Root())
	for _, want := range []string{`href="/` + nameA + `"`, `href="/` + nameB + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("home page missing repository link %s:\n%s", want, body)
		}
	}
	// It is its own keyboard context.
	if !strings.Contains(body, `data-page="all"`) {
		t.Error("home page is not the all-repositories context")
	}
}

// TestServerRepoNames checks that repositories sharing a base name get
// distinct, stable names, and that a full path redirects to one.
func TestServerRepoNames(t *testing.T) {
	s, r, _ := newTestServer(t)
	nameA, err := r.DB.RepoName(r.Root())
	if err != nil {
		t.Fatal(err)
	}

	// Two more repositories whose directories share a base name.
	base := t.TempDir()
	var paths []string
	for _, sub := range []string{"one/shared", "two/shared"} {
		dir := filepath.Join(base, sub)
		if err := os.MkdirAll(dir, 0777); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, dir)
	}
	n1, err := r.DB.RepoName(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	n2, err := r.DB.RepoName(paths[1])
	if err != nil {
		t.Fatal(err)
	}
	if n1 != "shared" || n2 != "shared.1" {
		t.Errorf("names = %q, %q; want shared and shared.1", n1, n2)
	}
	// Asking again gives the same answers: names must not move.
	if again, _ := r.DB.RepoName(paths[0]); again != n1 {
		t.Errorf("name changed from %q to %q", n1, again)
	}

	// A full path redirects to the short name.
	w := get(t, s, r.Root())
	if w.Code != http.StatusSeeOther {
		t.Fatalf("GET %s = %d, want a redirect", r.Root(), w.Code)
	}
	if got := w.Header().Get("Location"); got != "/"+nameA {
		t.Errorf("Location = %q, want /%s", got, nameA)
	}
	// An unknown path is simply not found.
	if w := get(t, s, "/no/such/repo"); w.Code != http.StatusNotFound {
		t.Errorf("GET /no/such/repo = %d, want 404", w.Code)
	}
}

func TestServerFileList(t *testing.T) {
	s, r, _ := newTestServer(t)
	body := mustGet(t, s, repoURL(t, r, "/c/Itest1"))
	for _, want := range []string{"Commit Message", "a.go", "Base", "Snapshot 1"} {
		if !strings.Contains(body, want) {
			t.Errorf("file list missing %q:\n%s", want, body)
		}
	}
}

func TestServerDiff(t *testing.T) {
	s, r, _ := newTestServer(t)
	body := mustGet(t, s, repoURL(t, r, "/d/Itest1?f=a.go"))

	// A wholly new file is a pure addition, so Gerrit paints every line
	// with the strong color: the "total" class.
	if !strings.Contains(body, "add total") {
		t.Errorf("new file diff has no total-add rows:\n%s", body)
	}
	if !strings.Contains(body, "package main") {
		t.Error("diff does not show the file contents")
	}
	// The commit message is diffable too.
	msg := mustGet(t, s, repoURL(t, r, "/d/Itest1?f=")+url.QueryEscape(CommitMsgFile))
	if !strings.Contains(msg, "Change-Id: Itest1") {
		t.Errorf("commit message diff missing the message:\n%s", msg)
	}
}

func TestServerDiffEscapesContent(t *testing.T) {
	s, r, dir := newTestServer(t)
	write(t, dir, "x.go", "var s = \"<script>alert(1)</script>\"\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "add x.go\n\nChange-Id: Iescape\n")

	body := mustGet(t, s, repoURL(t, r, "/d/Iescape?f=x.go"))
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("file contents were not escaped into the page")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("escaped content not found:\n%s", body)
	}
}

// TestServerCommentRoundTrip walks the whole reviewing loop: open a form,
// save a comment, see it in the page, reply, and resolve.
func TestServerCommentRoundTrip(t *testing.T) {
	s, r, _ := newTestServer(t)

	form := mustGet(t, s, repoURL(t, r, "/f/form?key=Itest1&f=a.go&side=new&line=4"))
	if !strings.Contains(form, "<textarea") || !strings.Contains(form, `name="line" value="4"`) {
		t.Fatalf("comment form:\n%s", form)
	}

	w := post(t, s, repoURL(t, r, "/f/comment"), url.Values{
		"key":  {"Itest1"},
		"f":    {"a.go"},
		"side": {"new"},
		"line": {"4"},
		"body": {"why println?"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("saving comment = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "why println?") {
		t.Fatalf("saved comment not rendered:\n%s", w.Body.String())
	}

	// It must be stored against the snapshot, with the line's text as its
	// anchor, and it must appear in the diff.
	threads, err := r.DB.Threads(r.Root(), "Itest1")
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 {
		t.Fatalf("got %d threads, want 1", len(threads))
	}
	th := threads[0]
	if th.Line != 4 || th.Side != "new" || th.SnapshotN != 1 {
		t.Errorf("thread = %+v", th)
	}
	if th.AnchorText != "\tprintln(\"hi\")" {
		t.Errorf("anchor text = %q, want the line's text", th.AnchorText)
	}
	if !th.Comments[0].Draft {
		t.Error("a comment written in the web UI should start as a draft")
	}

	body := mustGet(t, s, repoURL(t, r, "/d/Itest1?f=a.go"))
	if !strings.Contains(body, "why println?") || !strings.Contains(body, "draft") {
		t.Errorf("diff does not show the draft comment:\n%s", body)
	}

	// Reply and resolve in one step.
	id := th.ID
	w = post(t, s, repoURL(t, r, "/f/comment?thread=")+strconv.FormatInt(id, 10), url.Values{
		"body":    {"clearer than fmt here"},
		"resolve": {"1"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("reply = %d: %s", w.Code, w.Body.String())
	}
	got, _ := r.DB.Thread(id)
	if len(got.Comments) != 2 || !got.Resolved {
		t.Errorf("after reply: %d comments, resolved=%v", len(got.Comments), got.Resolved)
	}

	// Resolving toggles.
	if w := post(t, s, repoURL(t, r, "/f/resolve?thread=")+strconv.FormatInt(id, 10), nil); w.Code != http.StatusOK {
		t.Fatalf("resolve = %d: %s", w.Code, w.Body.String())
	}
	if got, _ = r.DB.Thread(id); got.Resolved {
		t.Error("resolve did not toggle back to unresolved")
	}
}

func TestServerAgentCommentIsDistinct(t *testing.T) {
	s, r, _ := newTestServer(t)
	post(t, s, repoURL(t, r, "/f/comment"), url.Values{
		"key": {"Itest1"}, "f": {"a.go"}, "side": {"new"}, "line": {"4"}, "body": {"question"},
	})
	threads, _ := r.DB.Threads(r.Root(), "Itest1")
	id := threads[0].ID

	// An agent replies through the CLI path, published immediately.
	if _, err := r.DB.AddComment(id, &Comment{Author: "agent", Body: "fixed", FromAgent: true}); err != nil {
		t.Fatal(err)
	}

	body := mustGet(t, s, repoURL(t, r, "/d/Itest1?f=a.go"))
	if !strings.Contains(body, `class="comment agent"`) {
		t.Errorf("agent comment is not styled differently:\n%s", body)
	}
	if !strings.Contains(body, `chip agentchip`) {
		t.Error("agent comment has no agent chip")
	}
}

func TestServerPublish(t *testing.T) {
	s, r, _ := newTestServer(t)
	post(t, s, repoURL(t, r, "/f/comment"), url.Values{
		"key": {"Itest1"}, "f": {"a.go"}, "side": {"new"}, "line": {"1"}, "body": {"a draft"},
	})

	if body := mustGet(t, s, repoURL(t, r, "/c/Itest1")); !strings.Contains(body, "Publish 1 draft") {
		t.Errorf("file list has no publish button:\n%s", body)
	}
	w := post(t, s, repoURL(t, r, "/publish"), url.Values{"key": {"Itest1"}, "return": {repoURL(t, r, "/c/Itest1")}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("publish = %d: %s", w.Code, w.Body.String())
	}
	threads, _ := r.DB.Threads(r.Root(), "Itest1")
	if threads[0].Comments[0].Draft {
		t.Error("comment is still a draft after publishing")
	}
}

// TestServerBackRejectsOffsiteRedirect checks that the return parameter
// cannot be used to bounce the browser somewhere else.
func TestServerBackRejectsOffsiteRedirect(t *testing.T) {
	s, r, _ := newTestServer(t)
	w := post(t, s, repoURL(t, r, "/publish"), url.Values{"key": {"Itest1"}, "return": {"https://example.com/"}})
	if got := w.Header().Get("Location"); got != "/" {
		t.Errorf("Location = %q, want /", got)
	}
	if got := w.Header().Get("HX-Redirect"); got != "/" {
		t.Errorf("HX-Redirect = %q, want /", got)
	}
}

func TestServerSnapshotAndBase(t *testing.T) {
	s, r, dir := newTestServer(t)
	mustGet(t, s, repoURL(t, r, "/c/Itest1")) // implicit snapshot 1

	write(t, dir, "a.go", "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "--amend", "--no-edit")

	w := post(t, s, repoURL(t, r, "/snapshot"), url.Values{"key": {"Itest1"}, "return": {repoURL(t, r, "/c/Itest1")}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("snapshot = %d: %s", w.Code, w.Body.String())
	}
	snaps, _ := r.DB.Snapshots(r.Root(), "Itest1")
	if len(snaps) != 2 {
		t.Fatalf("got %d snapshots, want 2", len(snaps))
	}

	// Snapshot 1 against snapshot 2 shows only the amend.
	body := mustGet(t, s, repoURL(t, r, "/d/Itest1?f=a.go&base=1&s=2"))
	// One line changed, so the two versions pair up in a single row.
	if !strings.Contains(body, "diffrow replace") {
		t.Errorf("snapshot-to-snapshot diff has no paired row:\n%s", body)
	}
	// It is an edit, not a wholly new file, so it must not be "total".
	if strings.Contains(body, "add total") {
		t.Error("edit between snapshots was rendered as a pure addition")
	}
	// The intraline diff must narrow the change to the letters that
	// actually differ, leaving the shared "h" outside the highlight.
	if !strings.Contains(body, `<span class="i">i</span>`) ||
		!strings.Contains(body, `<span class="i">ello</span>`) {
		t.Errorf("intraline highlight not narrowed to the changed letters:\n%s", body)
	}
}

func TestServerExpandContext(t *testing.T) {
	s, r, dir := newTestServer(t)
	var big strings.Builder
	for i := range 60 {
		big.WriteString("line ")
		big.WriteString(strconv.Itoa(i))
		big.WriteString("\n")
	}
	write(t, dir, "big.txt", big.String())
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "add big\n\nChange-Id: Ibig\n")
	mustGet(t, s, repoURL(t, r, "/c/Ibig"))

	edited := strings.Replace(big.String(), "line 30\n", "line thirty\n", 1)
	write(t, dir, "big.txt", edited)
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "--amend", "--no-edit")
	post(t, s, repoURL(t, r, "/snapshot"), url.Values{"key": {"Ibig"}})

	body := mustGet(t, s, repoURL(t, r, "/d/Ibig?f=big.txt&base=1&s=2"))
	if !strings.Contains(body, "unchanged line") {
		t.Fatalf("long unchanged runs were not collapsed:\n%s", body)
	}
	// The expander must return the hidden rows.
	m := regexp.MustCompile(`hx-get="([^"]*f/expand[^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no expand link in collapsed diff")
	}
	rows := mustGet(t, s, htmlUnescape(m[1]))
	if !strings.Contains(rows, "line 0") {
		t.Errorf("expanded rows do not include the hidden lines:\n%s", rows)
	}
}

func htmlUnescape(s string) string {
	return strings.NewReplacer("&amp;", "&", "&#34;", `"`, "&#39;", "'").Replace(s)
}

func TestServerUnifiedMode(t *testing.T) {
	s, r, dir := newTestServer(t)
	mustGet(t, s, repoURL(t, r, "/c/Itest1"))
	write(t, dir, "a.go", "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "--amend", "--no-edit")
	post(t, s, repoURL(t, r, "/snapshot"), url.Values{"key": {"Itest1"}})

	body := mustGet(t, s, repoURL(t, r, "/d/Itest1?f=a.go&base=1&s=2&unified=1"))
	if !strings.Contains(body, "diff unified") {
		t.Errorf("unified mode not applied:\n%s", body)
	}
	// Unified rows carry one side each: the paired row becomes a removal
	// followed by an addition, and both versions are still shown.
	if strings.Contains(body, "diffrow replace") {
		t.Error("unified diff still has side-by-side paired rows")
	}
	for _, want := range []string{"diffrow delete", "diffrow insert",
		`<span class="i">i</span>`, `<span class="i">ello</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("unified diff missing %q:\n%s", want, body)
		}
	}
}

func TestServerReviewedToggle(t *testing.T) {
	s, r, _ := newTestServer(t)
	mustGet(t, s, repoURL(t, r, "/c/Itest1"))
	snaps, _ := r.DB.Snapshots(r.Root(), "Itest1")

	w := post(t, s, repoURL(t, r, "/f/reviewed?snapshot=")+strconv.FormatInt(snaps[0].ID, 10)+"&f=a.go&on=1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("mark reviewed = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "reviewed on") {
		t.Errorf("button not switched on:\n%s", w.Body.String())
	}
	got, _ := r.DB.Reviewed(snaps[0].ID)
	if !got["a.go"] {
		t.Error("file not recorded as reviewed")
	}
}

func TestServerWorkingTreeCannotBeCommented(t *testing.T) {
	s, r, dir := newTestServer(t)
	write(t, dir, "dirty.txt", "uncommitted\n")

	body := mustGet(t, s, "/")
	if !strings.Contains(body, "uncommitted") {
		t.Fatalf("working tree change missing from the change list:\n%s", body)
	}
	diff := mustGet(t, s, repoURL(t, r, "/d/working?f=dirty.txt"))
	if !strings.Contains(diff, "cannot be commented on") {
		t.Errorf("diff of uncommitted changes does not explain the limit:\n%s", diff)
	}
	if strings.Contains(diff, "button class=\"linenum\"") {
		t.Error("comment buttons offered on uncommitted changes")
	}
	// And the server must refuse if a comment is posted anyway.
	w := post(t, s, repoURL(t, r, "/f/comment"), url.Values{
		"key": {"working"}, "f": {"dirty.txt"}, "side": {"new"}, "line": {"1"}, "body": {"x"},
	})
	if w.Code == http.StatusOK {
		t.Error("comment on uncommitted changes was accepted")
	}
}

func TestServerErrors(t *testing.T) {
	s, r, _ := newTestServer(t)
	for _, path := range []string{
		repoURL(t, r, "/c/nosuchchange"),
		repoURL(t, r, "/d/Itest1?f=nosuchfile.go"),
		repoURL(t, r, "/d/Itest1?f=a.go&s=99"),
		repoURL(t, r, "/f/form?key=Itest1&f=a.go&side=sideways&line=1"),
	} {
		if w := get(t, s, path); w.Code == http.StatusOK {
			t.Errorf("GET %s = 200, want an error", path)
		}
	}
}

func TestServerStaticFiles(t *testing.T) {
	s, _, _ := newTestServer(t)
	for _, path := range []string{"/style.css", "/keys.js", "/htmx.min.js"} {
		if w := get(t, s, path); w.Code != http.StatusOK {
			t.Errorf("GET %s = %d", path, w.Code)
		}
	}
}

// TestKeyActionsHaveHandlers is the check that keeps the shortcut table
// and its implementation from drifting: every action named in the Go
// table must have a handler in keys.js.
func TestKeyActionsHaveHandlers(t *testing.T) {
	src, err := os.ReadFile("static/keys.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(src)
	for _, action := range keyActions() {
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(action) + `\s*\(`).MatchString(js) {
			t.Errorf("shortcut action %q has no handler in keys.js", action)
		}
	}
}

// TestHelpDialogListsEveryBinding checks that the rendered help dialog
// really is generated from the shortcut table.
func TestHelpDialogListsEveryBinding(t *testing.T) {
	s, r, _ := newTestServer(t)
	body := mustGet(t, s, repoURL(t, r, "/d/Itest1?f=a.go"))
	for _, section := range keyHelp {
		if !strings.Contains(body, section.Title) {
			t.Errorf("help dialog missing section %q", section.Title)
		}
		for _, b := range section.Bindings {
			if !strings.Contains(body, b.Text) {
				t.Errorf("help dialog missing %q (%v)", b.Text, b.Keys)
			}
		}
	}
	// The v combo, which drives snapshot comparison, must be there.
	if !strings.Contains(body, "Diff against latest snapshot") {
		t.Error("help dialog missing the snapshot combo")
	}
}

func TestSincePlural(t *testing.T) {
	now := time.Now()
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{5 * time.Minute, "5 min ago"},
		{90 * time.Minute, "1 hour ago"},
		{5 * time.Hour, "5 hours ago"},
	}
	for _, tt := range tests {
		if got := since(now.Add(-tt.d)); got != tt.want {
			t.Errorf("since(-%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

// TestChangePageCommentHistory checks that the change page lists every
// comment thread below the snapshot list, oldest first, each linking back
// to the line it is attached to.
func TestChangePageCommentHistory(t *testing.T) {
	s, r, dir := newTestServer(t)
	mustGet(t, s, repoURL(t, r, "/c/Itest1")) // implicit snapshot 1

	// A comment on snapshot 1, published.
	post(t, s, repoURL(t, r, "/f/comment"), url.Values{
		"key": {"Itest1"}, "f": {"a.go"}, "side": {"new"}, "line": {"4"}, "body": {"first remark"},
	})
	post(t, s, repoURL(t, r, "/publish"), url.Values{"key": {"Itest1"}})

	// Amend, snapshot 2, and comment again, this time on the commit message.
	write(t, dir, "a.go", "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "--amend", "--no-edit")
	post(t, s, repoURL(t, r, "/snapshot"), url.Values{"key": {"Itest1"}})
	post(t, s, repoURL(t, r, "/f/comment"), url.Values{
		"key": {"Itest1"}, "s": {"2"}, "f": {CommitMsgFile}, "side": {"new"},
		"line": {"5"}, "body": {"second remark"},
	})

	body := mustGet(t, s, repoURL(t, r, "/c/Itest1"))

	// Both threads are listed.
	for _, want := range []string{"first remark", "second remark"} {
		if !strings.Contains(body, want) {
			t.Errorf("change page missing %q:\n%s", want, body)
		}
	}
	// Oldest first.
	if i, j := strings.Index(body, "first remark"), strings.Index(body, "second remark"); i > j {
		t.Error("comment history is not in chronological order")
	}
	// After the file list, not before it.
	if files, hist := strings.Index(body, `class="filelist"`), strings.Index(body, `class="history"`); files > hist {
		t.Error("comment history is above the file list, want below")
	}
	// The commit message pseudo-file is named, not shown as /COMMIT_MSG.
	if !strings.Contains(body, "Commit Message:5") {
		t.Errorf("commit message thread badly labelled:\n%s", body)
	}
	// A comment written against an older snapshot links to that snapshot
	// against the newest one, so following it shows what became of the
	// line. A comment on the newest snapshot has nothing to compare with
	// and links to it plainly.
	if !strings.Contains(body, "base=1&amp;f=a.go&amp;s=2#thread-") {
		t.Errorf("old comment does not link from its snapshot to the newest:\n%s", body)
	}
	if !strings.Contains(body, "f=%2FCOMMIT_MSG&amp;s=2#thread-") {
		t.Errorf("comment on the newest snapshot did not link to it plainly:\n%s", body)
	}
	// Drafts and unresolved counts are summarized.
	for _, want := range []string{"2 comments", "2 unresolved", "1 draft"} {
		if !strings.Contains(body, want) {
			t.Errorf("history summary missing %q", want)
		}
	}

	// Resolving from the change page works, and the swapped-in fragment
	// keeps its link rather than degrading to plain text.
	threads, _ := r.DB.Threads(r.Root(), "Itest1")
	w := post(t, s, repoURL(t, r, "/f/resolve?thread=")+strconv.FormatInt(threads[0].ID, 10), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("resolve from change page = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "#thread-") {
		t.Errorf("re-rendered thread lost its link:\n%s", w.Body.String())
	}
}

// TestFileListCounts checks that each file reports both how many comments
// it has and how many of its threads are still unresolved, rather than
// only one or the other.
func TestFileListCounts(t *testing.T) {
	s, r, _ := newTestServer(t)
	mustGet(t, s, repoURL(t, r, "/c/Itest1"))

	// Two threads on a.go: one that gets resolved, one that does not.
	post(t, s, repoURL(t, r, "/f/comment"), url.Values{
		"key": {"Itest1"}, "f": {"a.go"}, "side": {"new"}, "line": {"1"}, "body": {"one"},
	})
	post(t, s, repoURL(t, r, "/f/comment"), url.Values{
		"key": {"Itest1"}, "f": {"a.go"}, "side": {"new"}, "line": {"4"}, "body": {"two"},
	})
	threads, _ := r.DB.Threads(r.Root(), "Itest1")
	if len(threads) != 2 {
		t.Fatalf("got %d threads, want 2", len(threads))
	}
	// A reply makes the comment count differ from the thread count, so the
	// two numbers cannot be confused for one another.
	if _, err := r.DB.AddComment(threads[0].ID, &Comment{Author: "agent", Body: "done", FromAgent: true}); err != nil {
		t.Fatal(err)
	}
	if err := r.DB.SetResolved(threads[0].ID, true); err != nil {
		t.Fatal(err)
	}

	body := mustGet(t, s, repoURL(t, r, "/c/Itest1"))
	counts := fileCounts(t, body, "a.go")
	// 3 comments across 2 threads, 1 thread still unresolved.
	if !strings.Contains(counts, "3 comments") {
		t.Errorf("file row does not report the comment count:\n%s", counts)
	}
	if !strings.Contains(counts, "1 unresolved") {
		t.Errorf("file row does not report the unresolved count:\n%s", counts)
	}

	// A file with no comments reports neither.
	if got := fileCounts(t, body, CommitMsgFile); strings.TrimSpace(got) != "" {
		t.Errorf("uncommented file shows counts: %q", got)
	}
}

// fileCounts returns the contents of one file row's counts cell.
func fileCounts(t *testing.T, body, file string) string {
	t.Helper()
	row := fileRow(t, body, file)
	i := strings.Index(row, `<td class="counts">`)
	if i < 0 {
		t.Fatalf("no counts cell for %q", file)
	}
	row = row[i+len(`<td class="counts">`):]
	j := strings.Index(row, "</td>")
	if j < 0 {
		t.Fatalf("unterminated counts cell for %q", file)
	}
	return row[:j]
}

// fileRow returns the markup of one row of the file list.
func fileRow(t *testing.T, body, file string) string {
	t.Helper()
	i := strings.Index(body, `data-file="`+file+`"`)
	if i < 0 {
		t.Fatalf("no row for %q in file list", file)
	}
	j := strings.Index(body[i:], "</tr>")
	if j < 0 {
		t.Fatalf("unterminated row for %q", file)
	}
	return body[i : i+j]
}

// TestChangeListCounts checks that the change list reports comment and
// unresolved counts the same way the file list does.
func TestChangeListCounts(t *testing.T) {
	s, r, _ := newTestServer(t)
	mustGet(t, s, repoURL(t, r, "/c/Itest1"))

	post(t, s, repoURL(t, r, "/f/comment"), url.Values{
		"key": {"Itest1"}, "f": {"a.go"}, "side": {"new"}, "line": {"1"}, "body": {"one"},
	})
	post(t, s, repoURL(t, r, "/f/comment"), url.Values{
		"key": {"Itest1"}, "f": {"a.go"}, "side": {"new"}, "line": {"4"}, "body": {"two"},
	})
	threads, _ := r.DB.Threads(r.Root(), "Itest1")
	if _, err := r.DB.AddComment(threads[0].ID, &Comment{Author: "agent", Body: "done", FromAgent: true}); err != nil {
		t.Fatal(err)
	}
	if err := r.DB.SetResolved(threads[0].ID, true); err != nil {
		t.Fatal(err)
	}

	counts := changeCounts(t, mustGet(t, s, "/"), "Itest1")
	// 3 comments across 2 threads, 1 thread unresolved, and both drafts
	// still unpublished.
	for _, want := range []string{"3 comments", "1 unresolved", "2 draft"} {
		if !strings.Contains(counts, want) {
			t.Errorf("change row missing %q:\n%s", want, counts)
		}
	}

	// After publishing, the comment and unresolved counts remain but the
	// draft chip goes away.
	post(t, s, repoURL(t, r, "/publish"), url.Values{"key": {"Itest1"}})
	counts = changeCounts(t, mustGet(t, s, "/"), "Itest1")
	if strings.Contains(counts, "draft") {
		t.Errorf("draft chip survived publishing:\n%s", counts)
	}
	for _, want := range []string{"3 comments", "1 unresolved"} {
		if !strings.Contains(counts, want) {
			t.Errorf("change row missing %q after publishing:\n%s", want, counts)
		}
	}
}

// changeCounts returns the contents of one change row's counts cell.
func changeCounts(t *testing.T, body, key string) string {
	t.Helper()
	i := strings.Index(body, `data-key="`+key+`"`)
	if i < 0 {
		t.Fatalf("no row for change %q", key)
	}
	row := body[i:]
	if j := strings.Index(row, "</tr>"); j >= 0 {
		row = row[:j]
	}
	k := strings.Index(row, `<td class="counts">`)
	if k < 0 {
		t.Fatalf("no counts cell for change %q", key)
	}
	row = row[k+len(`<td class="counts">`):]
	j := strings.Index(row, "</td>")
	if j < 0 {
		t.Fatalf("unterminated counts cell for change %q", key)
	}
	return row[:j]
}

// twoSnapshots leaves the test change with snapshot 1 and snapshot 2,
// differing by one line of a.go.
func twoSnapshots(t *testing.T, s *server, r *Review, dir string) {
	t.Helper()
	mustGet(t, s, repoURL(t, r, "/c/Itest1")) // implicit snapshot 1
	write(t, dir, "a.go", "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "--amend", "--no-edit")
	post(t, s, repoURL(t, r, "/snapshot"), url.Values{"key": {"Itest1"}})
}

func TestSnapshotReviewedToggle(t *testing.T) {
	s, r, dir := newTestServer(t)
	twoSnapshots(t, s, r, dir)
	snaps, _ := r.DB.Snapshots(r.Root(), "Itest1")

	// The change page offers a reviewed button per snapshot, unlit.
	body := mustGet(t, s, repoURL(t, r, "/c/Itest1"))
	if !strings.Contains(body, repoURL(t, r, "/f/snapshot-reviewed?")) {
		t.Fatalf("no snapshot reviewed button on the change page:\n%s", body)
	}
	if strings.Contains(body, `class="reviewed on"`) {
		t.Error("a snapshot is lit as reviewed before anything was marked")
	}

	id := strconv.FormatInt(snaps[0].ID, 10)
	w := post(t, s, repoURL(t, r, "/f/snapshot-reviewed?snapshot=")+id+"&on=1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("mark snapshot reviewed = %d: %s", w.Code, w.Body.String())
	}
	// The button comes back lit, and is its own indicator.
	if !strings.Contains(w.Body.String(), `class="reviewed on"`) {
		t.Errorf("button did not light up:\n%s", w.Body.String())
	}
	if ok, _ := r.DB.SnapshotReviewed(snaps[0].ID); !ok {
		t.Error("snapshot not recorded as reviewed")
	}

	// It stays lit on reload, and toggles back off.
	if body = mustGet(t, s, repoURL(t, r, "/c/Itest1")); !strings.Contains(body, `class="reviewed on"`) {
		t.Error("reviewed snapshot is not lit after reload")
	}
	if w = post(t, s, repoURL(t, r, "/f/snapshot-reviewed?snapshot=")+id+"&on=0", nil); w.Code != http.StatusOK {
		t.Fatalf("unmark = %d", w.Code)
	}
	if ok, _ := r.DB.SnapshotReviewed(snaps[0].ID); ok {
		t.Error("snapshot still reviewed after unmarking")
	}

	// A snapshot that does not exist is refused.
	if w = post(t, s, repoURL(t, r, "/f/snapshot-reviewed?snapshot=99999&on=1"), nil); w.Code == http.StatusOK {
		t.Error("marking a nonexistent snapshot succeeded")
	}
}

// TestAutoBaseFromReviewedSnapshot is the point of marking snapshots:
// opening a file afterwards shows only what changed since then.
func TestAutoBaseFromReviewedSnapshot(t *testing.T) {
	s, r, dir := newTestServer(t)
	twoSnapshots(t, s, r, dir)
	snaps, _ := r.DB.Snapshots(r.Root(), "Itest1")

	// With nothing reviewed, the base is the parent: the whole change.
	c, _ := r.Change("Itest1")
	v, err := r.View(c, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Base != nil || v.AutoBase {
		t.Fatalf("base = %+v, auto=%v; want the parent commit", v.Base, v.AutoBase)
	}

	// Mark snapshot 1 reviewed. Now the base is snapshot 1.
	if err := r.DB.SetSnapshotReviewed(snaps[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if v, err = r.View(c, "", ""); err != nil {
		t.Fatal(err)
	}
	if v.Base == nil || v.Base.N != 1 || !v.AutoBase {
		t.Fatalf("base = %+v, auto=%v; want snapshot 1", v.Base, v.AutoBase)
	}
	if v.Target.N != 2 {
		t.Fatalf("target = %d, want 2", v.Target.N)
	}

	// The diff page picks it up, and says why.
	body := mustGet(t, s, repoURL(t, r, "/d/Itest1?f=a.go"))
	if !strings.Contains(body, "since you last reviewed") {
		t.Errorf("diff does not explain the automatic base:\n%s", body)
	}
	// Only the amended line shows, not the whole new file.
	if strings.Contains(body, "add total") {
		t.Error("diff against last reviewed snapshot rendered as a whole new file")
	}

	// An explicit "parent" overrides the automatic choice, so the base
	// selector can still get back to the whole change.
	if v, err = r.View(c, "parent", ""); err != nil {
		t.Fatal(err)
	}
	if v.Base != nil || v.AutoBase {
		t.Errorf("explicit parent was overridden: base=%+v", v.Base)
	}

	// Reviewing snapshot 2 as well must not make it its own base.
	if err := r.DB.SetSnapshotReviewed(snaps[1].ID, true); err != nil {
		t.Fatal(err)
	}
	if v, err = r.View(c, "", ""); err != nil {
		t.Fatal(err)
	}
	if v.Base == nil || v.Base.N != 1 {
		t.Errorf("base = %+v, want snapshot 1 (the newest reviewed one older than the target)", v.Base)
	}

	// Viewing snapshot 1 itself has no older reviewed snapshot to use.
	if v, err = r.View(c, "", "1"); err != nil {
		t.Fatal(err)
	}
	if v.Base != nil {
		t.Errorf("base = %+v, want the parent for the first snapshot", v.Base)
	}
}

// TestMigrate checks that databases written by older versions of review
// are upgraded rather than rejected, keeping their comments and their
// snapshot marks.
func TestMigrate(t *testing.T) {
	for _, from := range []int{1, 2} {
		t.Run(fmt.Sprintf("from%d", from), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "old.db")
			d, err := OpenDB(path)
			if err != nil {
				t.Fatal(err)
			}
			snap, _, err := d.AddSnapshot("/repo", testChange("k1", "r1", "one"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.AddThread(snap.ID, "a.go", "new", 1, "x", &Comment{Author: "rsc", Body: "keep me"}); err != nil {
				t.Fatal(err)
			}

			// Rewind to the older schema, carrying a reviewed mark across.
			for _, drop := range []string{
				"DROP TABLE snapshot_mark",
				"DROP TABLE repo",
				"ALTER TABLE snapshot DROP COLUMN parent_key",
			} {
				if _, err := d.sql.Exec(drop); err != nil {
					t.Fatal(err)
				}
			}
			if from >= 2 {
				if _, err := d.sql.Exec(migrations[1]); err != nil {
					t.Fatal(err)
				}
				if _, err := d.sql.Exec("INSERT INTO reviewed_snapshot (snapshot_id, created) VALUES (?, 1)", snap.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := d.sql.Exec(fmt.Sprintf("PRAGMA user_version = %d", from)); err != nil {
				t.Fatal(err)
			}
			d.Close()

			d2, err := OpenDB(path)
			if err != nil {
				t.Fatalf("opening a version %d database: %v", from, err)
			}
			defer d2.Close()

			threads, err := d2.Threads("/repo", "k1")
			if err != nil {
				t.Fatal(err)
			}
			if len(threads) != 1 || threads[0].Comments[0].Body != "keep me" {
				t.Fatalf("migration lost comments: %+v", threads)
			}
			// A reviewed mark recorded under the old schema survives.
			ok, err := d2.SnapshotReviewed(snap.ID)
			if err != nil {
				t.Fatal(err)
			}
			if want := from >= 2; ok != want {
				t.Errorf("SnapshotReviewed = %v, want %v", ok, want)
			}
			// And the new mark works.
			if err := d2.SetSnapshotLGTM(snap.ID, true); err != nil {
				t.Fatalf("LGTM unusable after migration: %v", err)
			}
			var v int
			if err := d2.sql.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
				t.Fatal(err)
			}
			if v != schemaVersion {
				t.Errorf("user_version = %d, want %d", v, schemaVersion)
			}
		})
	}
}

// TestDiffFileData checks the per-file data the keyboard layer needs to
// jump to the next unreviewed file when n runs off the end of a diff.
func TestDiffFileData(t *testing.T) {
	s, r, _ := newTestServer(t)
	mustGet(t, s, repoURL(t, r, "/c/Itest1"))
	snaps, _ := r.DB.Snapshots(r.Root(), "Itest1")
	if err := r.DB.SetReviewed(snaps[0].ID, CommitMsgFile, true); err != nil {
		t.Fatal(err)
	}
	post(t, s, repoURL(t, r, "/f/comment"), url.Values{
		"key": {"Itest1"}, "f": {"a.go"}, "side": {"new"}, "line": {"1"}, "body": {"hm"},
	})

	body := mustGet(t, s, repoURL(t, r, "/d/Itest1?f=a.go"))
	i := strings.Index(body, `id="filedata"`)
	if i < 0 {
		t.Fatal("no filedata in the diff page")
	}
	data := body[i:]
	data = data[strings.Index(data, ">")+1 : strings.Index(data, "</script>")]

	var files []struct {
		Path     string `json:"path"`
		URL      string `json:"url"`
		Comments int    `json:"comments"`
		Reviewed bool   `json:"reviewed"`
	}
	if err := json.Unmarshal([]byte(data), &files); err != nil {
		t.Fatalf("filedata is not valid JSON: %v\n%s", err, data)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2: %+v", len(files), files)
	}
	// Every file the diff can move to, in order, with the state n needs.
	if files[0].Path != CommitMsgFile || !files[0].Reviewed {
		t.Errorf("commit message entry = %+v, want it marked reviewed", files[0])
	}
	if files[1].Path != "a.go" || files[1].Reviewed || files[1].Comments != 1 {
		t.Errorf("a.go entry = %+v, want unreviewed with one comment", files[1])
	}
	if files[1].URL == "" {
		t.Error("file entry has no URL to navigate to")
	}
}

// TestCommitMsgAgainstParentIsAllNew checks that the commit message is
// diffed against nothing when the base is the parent commit: the parent's
// message belongs to a different change and is not an earlier draft of
// this one. Against an earlier snapshot it still diffs normally.
func TestCommitMsgAgainstParentIsAllNew(t *testing.T) {
	s, r, dir := newTestServer(t)
	c, _ := r.Change("Itest1")

	v, err := r.View(c, "parent", "")
	if err != nil {
		t.Fatal(err)
	}
	old, new, err := r.Contents(v, v.File(CommitMsgFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 0 {
		t.Errorf("commit message base against the parent = %q, want empty", old)
	}
	if !strings.Contains(string(new), "add a.go") {
		t.Errorf("commit message content = %q", new)
	}
	// Every line is therefore an addition.
	for _, row := range Diff(old, new).Rows {
		if row.Kind != RowInsert {
			t.Fatalf("row %v is not an addition; the parent message leaked in", rowString(row))
		}
	}
	body := mustGet(t, s, repoURL(t, r, "/d/Itest1?f=")+url.QueryEscape(CommitMsgFile)+"&base=parent")
	if !strings.Contains(body, "add total") {
		t.Errorf("commit message diff is not rendered as wholly new:\n%s", body)
	}

	// Reword the message and snapshot: now the two snapshots differ, and
	// the message must diff against the older snapshot rather than vanish.
	mustGet(t, s, repoURL(t, r, "/c/Itest1"))
	do(t, dir, "git", "commit", "-q", "--amend", "-m", "add a.go, reworded\n\nChange-Id: Itest1\n")
	post(t, s, repoURL(t, r, "/snapshot"), url.Values{"key": {"Itest1"}})

	c, _ = r.Change("Itest1")
	v, err = r.View(c, "1", "2")
	if err != nil {
		t.Fatal(err)
	}
	old, new, err = r.Contents(v, v.File(CommitMsgFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(old), "add a.go") || strings.Contains(string(old), "reworded") {
		t.Errorf("snapshot 1 message = %q, want the original wording", old)
	}
	if !strings.Contains(string(new), "reworded") {
		t.Errorf("snapshot 2 message = %q, want the new wording", new)
	}
}

func TestEditAndDeleteDraft(t *testing.T) {
	s, r, _ := newTestServer(t)
	post(t, s, repoURL(t, r, "/f/comment"), url.Values{
		"key": {"Itest1"}, "f": {"a.go"}, "side": {"new"}, "line": {"1"}, "body": {"frist"},
	})
	threads, _ := r.DB.Threads(r.Root(), "Itest1")
	th := threads[0]
	id := strconv.FormatInt(th.Comments[0].ID, 10)

	// A draft offers edit and delete; the diff page shows them.
	body := mustGet(t, s, repoURL(t, r, "/d/Itest1?f=a.go"))
	if !strings.Contains(body, repoURL(t, r, "/f/editform?comment=")+id) || !strings.Contains(body, repoURL(t, r, "/f/delete?comment=")+id) {
		t.Fatalf("draft comment has no edit and delete buttons:\n%s", body)
	}

	// The edit form is prefilled.
	form := mustGet(t, s, repoURL(t, r, "/f/editform?comment=")+id)
	if !strings.Contains(form, "frist") {
		t.Errorf("edit form is not prefilled:\n%s", form)
	}

	w := post(t, s, repoURL(t, r, "/f/edit?comment=")+id, url.Values{"body": {"first, actually"}})
	if w.Code != http.StatusOK {
		t.Fatalf("edit = %d: %s", w.Code, w.Body.String())
	}
	got, _ := r.DB.Thread(th.ID)
	if got.Comments[0].Body != "first, actually" {
		t.Errorf("body = %q after edit", got.Comments[0].Body)
	}
	if !strings.Contains(w.Body.String(), "first, actually") {
		t.Errorf("edit did not return the updated thread:\n%s", w.Body.String())
	}
	// An empty edit is refused rather than blanking the comment.
	if w = post(t, s, repoURL(t, r, "/f/edit?comment=")+id, url.Values{"body": {"   "}}); w.Code == http.StatusOK {
		t.Error("an empty edit was accepted")
	}

	// Published comments can be neither edited nor deleted.
	post(t, s, repoURL(t, r, "/publish"), url.Values{"key": {"Itest1"}})
	if w = post(t, s, repoURL(t, r, "/f/edit?comment=")+id, url.Values{"body": {"sneaky"}}); w.Code == http.StatusOK {
		t.Error("a published comment was edited")
	}
	if w = post(t, s, repoURL(t, r, "/f/delete?comment=")+id, nil); w.Code == http.StatusOK {
		t.Error("a published comment was deleted")
	}
	if w = get(t, s, repoURL(t, r, "/f/editform?comment=")+id); w.Code == http.StatusOK {
		t.Error("an edit form was offered for a published comment")
	}
	if got, _ = r.DB.Thread(th.ID); got.Comments[0].Body != "first, actually" {
		t.Errorf("published comment changed: %q", got.Comments[0].Body)
	}
}

// TestDeleteDraftRemovesEmptyThread checks that deleting the only comment
// in a thread takes the thread with it, rather than leaving a marker on a
// line with nothing to say.
func TestDeleteDraftRemovesEmptyThread(t *testing.T) {
	s, r, _ := newTestServer(t)
	post(t, s, repoURL(t, r, "/f/comment"), url.Values{
		"key": {"Itest1"}, "f": {"a.go"}, "side": {"new"}, "line": {"1"}, "body": {"one"},
	})
	threads, _ := r.DB.Threads(r.Root(), "Itest1")
	th := threads[0]

	// With a second draft in the thread, deleting one leaves the thread.
	second, err := r.DB.AddComment(th.ID, &Comment{Author: "rsc", Body: "two", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	w := post(t, s, repoURL(t, r, "/f/delete?comment=")+strconv.FormatInt(second.ID, 10), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
	}
	got, err := r.DB.Thread(th.ID)
	if err != nil {
		t.Fatalf("thread went away while it still had a comment: %v", err)
	}
	if len(got.Comments) != 1 || got.Comments[0].Body != "one" {
		t.Fatalf("comments = %+v", got.Comments)
	}
	if !strings.Contains(w.Body.String(), "one") {
		t.Errorf("delete did not return the remaining thread:\n%s", w.Body.String())
	}

	// Deleting the last one removes the thread, leaving only the way back.
	w = post(t, s, repoURL(t, r, "/f/delete?comment=")+strconv.FormatInt(got.Comments[0].ID, 10), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "Undelete draft") {
		t.Errorf("deleting the last comment did not offer to undo it:\n%s", body)
	} else if strings.Contains(body, `id="thread-`) {
		t.Errorf("deleting the last comment left a thread behind:\n%s", body)
	}
	if threads, _ = r.DB.Threads(r.Root(), "Itest1"); len(threads) != 0 {
		t.Errorf("thread survived its last comment: %+v", threads)
	}
	if body := mustGet(t, s, repoURL(t, r, "/d/Itest1?f=a.go")); strings.Contains(body, "one") {
		t.Error("deleted comment still shows in the diff")
	}
}

// TestUndeleteDraft checks that a deleted draft can be put back, both
// when its thread survived it and when it did not.
func TestUndeleteDraft(t *testing.T) {
	s, r, _ := newTestServer(t)
	post(t, s, repoURL(t, r, "/f/comment"), url.Values{
		"key": {"Itest1"}, "f": {"a.go"}, "side": {"new"}, "line": {"4"}, "body": {"only draft"},
	})
	threads, _ := r.DB.Threads(r.Root(), "Itest1")
	th := threads[0]

	// Deleting the only comment takes the thread with it; the fragment
	// carries everything needed to rebuild it.
	w := post(t, s, repoURL(t, r, "/f/delete?comment=")+strconv.FormatInt(th.Comments[0].ID, 10), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
	}
	form := w.Body.String()
	for _, want := range []string{
		`name="body" value="only draft"`,
		`name="f" value="a.go"`,
		`name="side" value="new"`,
		`name="line" value="4"`,
		`name="anchor" value="` + "\tprintln(&#34;hi&#34;)",
	} {
		if !strings.Contains(form, want) {
			t.Errorf("undo form missing %s:\n%s", want, form)
		}
	}

	w = post(t, s, repoURL(t, r, "/f/undelete"), url.Values{
		"body": {"only draft"}, "author": {"rsc"}, "snapshot": {strconv.FormatInt(th.SnapshotID, 10)},
		"f": {"a.go"}, "side": {"new"}, "line": {"4"}, "anchor": {"\tprintln(\"hi\")"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("undelete = %d: %s", w.Code, w.Body.String())
	}
	threads, _ = r.DB.Threads(r.Root(), "Itest1")
	if len(threads) != 1 {
		t.Fatalf("got %d threads after undelete, want 1", len(threads))
	}
	back := threads[0]
	if len(back.Comments) != 1 || back.Comments[0].Body != "only draft" {
		t.Fatalf("restored thread = %+v", back.Comments)
	}
	if !back.Comments[0].Draft {
		t.Error("restored comment is not a draft")
	}
	// It came back where it was, so it still shows against its line.
	if back.Line != 4 || back.File != "a.go" || back.AnchorText != "\tprintln(\"hi\")" {
		t.Errorf("restored thread landed at %s:%d anchored to %q", back.File, back.Line, back.AnchorText)
	}

	// Now with a thread that survives: delete a second draft from it.
	second, err := r.DB.AddComment(back.ID, &Comment{Author: "rsc", Body: "second draft", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	w = post(t, s, repoURL(t, r, "/f/delete?comment=")+strconv.FormatInt(second.ID, 10), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The rest of the thread stays on the page, with the way back beside it.
	if !strings.Contains(body, "only draft") {
		t.Errorf("deleting one draft hid the rest of the thread:\n%s", body)
	}
	if !strings.Contains(body, "Undelete draft") || !strings.Contains(body, `name="thread"`) {
		t.Errorf("undo form does not name the surviving thread:\n%s", body)
	}

	w = post(t, s, repoURL(t, r, "/f/undelete"), url.Values{
		"body": {"second draft"}, "author": {"rsc"}, "thread": {strconv.FormatInt(back.ID, 10)},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("undelete = %d: %s", w.Code, w.Body.String())
	}
	again, _ := r.DB.Thread(back.ID)
	if len(again.Comments) != 2 || again.Comments[1].Body != "second draft" {
		t.Fatalf("thread after undelete = %+v", again.Comments)
	}
}

// TestChangeListReviewedChip checks that a change whose newest snapshot
// is marked reviewed says so in the change list.
func TestChangeListReviewedChip(t *testing.T) {
	s, r, dir := newTestServer(t)
	twoSnapshots(t, s, r, dir)
	snaps, _ := r.DB.Snapshots(r.Root(), "Itest1")

	if body := mustGet(t, s, "/"); strings.Contains(body, "reviewedchip") {
		t.Error("change is marked reviewed before anything was marked")
	}

	// Marking an older snapshot is not enough: it says nothing about the
	// state the change is in now.
	if err := r.DB.SetSnapshotReviewed(snaps[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if body := mustGet(t, s, "/"); strings.Contains(body, "reviewedchip") {
		t.Error("an older reviewed snapshot marked the whole change reviewed")
	}

	// Marking the newest one does it.
	if err := r.DB.SetSnapshotReviewed(snaps[1].ID, true); err != nil {
		t.Fatal(err)
	}
	if body := mustGet(t, s, "/"); !strings.Contains(body, "reviewedchip") {
		t.Errorf("change list does not show the reviewed chip:\n%s", body)
	}

	// A new snapshot makes it unreviewed again.
	write(t, dir, "a.go", "package main\n\nfunc main() {\n\tprintln(\"third\")\n}\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "--amend", "--no-edit")
	post(t, s, repoURL(t, r, "/snapshot"), url.Values{"key": {"Itest1"}})
	if body := mustGet(t, s, "/"); strings.Contains(body, "reviewedchip") {
		t.Error("a new snapshot did not clear the reviewed chip")
	}
}

// TestLGTM checks the LGTM button and that it belongs to the snapshot it
// was put on: a new snapshot arrives without it.
func TestLGTM(t *testing.T) {
	s, r, dir := newTestServer(t)
	mustGet(t, s, repoURL(t, r, "/c/Itest1"))
	snaps, _ := r.DB.Snapshots(r.Root(), "Itest1")

	body := mustGet(t, s, repoURL(t, r, "/c/Itest1"))
	if !strings.Contains(body, repoURL(t, r, "/f/lgtm?")) {
		t.Fatalf("change page has no LGTM button:\n%s", body)
	}
	if strings.Contains(body, "lgtm on") {
		t.Error("LGTM is lit before it was pressed")
	}

	id := strconv.FormatInt(snaps[0].ID, 10)
	w := post(t, s, repoURL(t, r, "/f/lgtm?snapshot=")+id+"&on=1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("LGTM = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "lgtm on") || !strings.Contains(w.Body.String(), "✓ LGTM") {
		t.Errorf("LGTM button did not light up:\n%s", w.Body.String())
	}
	if ok, _ := r.DB.SnapshotLGTM(snaps[0].ID); !ok {
		t.Error("LGTM not recorded")
	}
	if body = mustGet(t, s, repoURL(t, r, "/c/Itest1")); !strings.Contains(body, "lgtm on") {
		t.Error("LGTM is not lit after reload")
	}
	// And it shows in the change list.
	if body = mustGet(t, s, "/"); !strings.Contains(body, "lgtmchip") {
		t.Errorf("change list does not show LGTM:\n%s", body)
	}

	// A new snapshot loses it.
	write(t, dir, "a.go", "package main\n\nfunc main() {\n\tprintln(\"more\")\n}\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "--amend", "--no-edit")
	post(t, s, repoURL(t, r, "/snapshot"), url.Values{"key": {"Itest1"}})

	if body = mustGet(t, s, "/"); strings.Contains(body, "lgtmchip") {
		t.Error("LGTM survived a new snapshot")
	}
	if body = mustGet(t, s, repoURL(t, r, "/c/Itest1")); strings.Contains(body, "lgtm on") {
		t.Error("LGTM button is lit for a snapshot that was never marked")
	}
	// The old snapshot keeps its own LGTM, though.
	if ok, _ := r.DB.SnapshotLGTM(snaps[0].ID); !ok {
		t.Error("the LGTM on snapshot 1 was lost")
	}
	if body = mustGet(t, s, repoURL(t, r, "/c/Itest1?s=1")); !strings.Contains(body, "lgtm on") {
		t.Error("viewing snapshot 1 does not show its LGTM")
	}

	// Toggling off works.
	if w = post(t, s, repoURL(t, r, "/f/lgtm?snapshot=")+id+"&on=0", nil); w.Code != http.StatusOK {
		t.Fatalf("un-LGTM = %d", w.Code)
	}
	if ok, _ := r.DB.SnapshotLGTM(snaps[0].ID); ok {
		t.Error("LGTM still set after toggling off")
	}
	if w = post(t, s, repoURL(t, r, "/f/lgtm?snapshot=99999&on=1"), nil); w.Code == http.StatusOK {
		t.Error("LGTM on a nonexistent snapshot succeeded")
	}
}

// TestChangePageReviewShortcut checks that M is bound on the change page,
// where it means the whole change rather than one file, and that the two
// meanings stay in their own contexts.
func TestChangePageReviewShortcut(t *testing.T) {
	// The whole table is sent to every page and filtered client-side by
	// section, so what matters is which pages each section applies to.
	pagesFor := func(action string) []string {
		for _, sec := range keyHelp {
			for _, b := range sec.Bindings {
				if b.Action == action {
					return sec.Pages
				}
			}
		}
		return nil
	}
	if got := pagesFor("reviewChange"); !slices.Equal(got, []string{"files"}) {
		t.Errorf("reviewChange applies to %v, want the change page only", got)
	}
	if got := pagesFor("reviewedNextFile"); !slices.Equal(got, []string{"diff"}) {
		t.Errorf("reviewedNextFile applies to %v, want diffs only", got)
	}
	// Both are bound to M, in their own contexts.
	for _, action := range []string{"reviewChange", "reviewedNextFile"} {
		var keys []string
		for _, sec := range keyHelp {
			for _, b := range sec.Bindings {
				if b.Action == action {
					keys = b.Keys
				}
			}
		}
		if !slices.Contains(keys, "M") {
			t.Errorf("%s is bound to %v, want M", action, keys)
		}
	}

	s, r, dir := newTestServer(t)
	twoSnapshots(t, s, r, dir)
	body := mustGet(t, s, repoURL(t, r, "/c/Itest1"))
	if !strings.Contains(body, "Mark the change reviewed and go up to the change list") {
		t.Error("help dialog does not describe M on the change page")
	}
	// It acts on the snapshot being viewed, which the timeline marks.
	if !strings.Contains(body, `<li class="sel">`) {
		t.Errorf("timeline does not mark the selected snapshot:\n%s", body)
	}
}

// TestInlineDiffURL checks that the file list offers each file's diff as a
// fragment carrying the base and target the page is showing. Rebuilding
// that URL in the browser from the resolved snapshot numbers loses the
// difference between "the parent" and "whatever was last reviewed", and
// asks for files that are not in the resulting diff.
func TestInlineDiffURL(t *testing.T) {
	s, r, dir := newTestServer(t)
	twoSnapshots(t, s, r, dir)
	snaps, _ := r.DB.Snapshots(r.Root(), "Itest1")

	// With snapshot 1 reviewed, a bare URL bases the diff on it, while an
	// explicit parent asks for the whole change: different file lists.
	if err := r.DB.SetSnapshotReviewed(snaps[0].ID, true); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct{ query, want string }{
		{"?base=parent", "base=parent"},
		{"?base=1&s=2", "base=1"},
	} {
		body := mustGet(t, s, repoURL(t, r, "/c/Itest1")+tt.query)
		if !strings.Contains(body, "data-inline=") {
			t.Fatalf("%s: file list offers no inline diff:\n%s", tt.query, body)
		}
		if !strings.Contains(body, tt.want) {
			t.Errorf("%s: inline URL does not carry %s", tt.query, tt.want)
		}
	}

	// The URL it offers must actually serve that file's diff.
	body := mustGet(t, s, repoURL(t, r, "/c/Itest1")+"?base=parent")
	m := regexp.MustCompile(`data-inline="([^"]*f=a\.go[^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no inline URL for a.go:\n%s", body)
	}
	frag := mustGet(t, s, htmlUnescape(m[1]))
	if !strings.Contains(frag, "inlinediff") || !strings.Contains(frag, "package main") {
		t.Errorf("inline fragment is not a diff of the file:\n%s", frag)
	}

	// And the button that shows them all is on the page.
	if !strings.Contains(body, `id="expandall"`) {
		t.Error("no Expand all button on the change page")
	}
}

// TestRebasePresentation checks the two things the pages say about edits a
// change inherited from a rebase: a chip on the files it did not touch at
// all, and muted colors on the lines it did not write.
func TestRebasePresentation(t *testing.T) {
	r, _ := newStackedReview(t)
	s := newServer(r.DB, r.Root(), r.Pin)

	files := mustGet(t, s, repoURL(t, r, "/c/"+topChangeKey+"?base=1&s=2"))
	for _, want := range []string{
		`data-file="deep.txt"`, `data-file="shared.txt"`, "rebase only",
	} {
		if !strings.Contains(files, want) {
			t.Errorf("file list missing %s:\n%s", want, files)
		}
	}
	// Only deep.txt is in that state: the change edits shared.txt itself,
	// so that one is merely unreviewed.
	if n := strings.Count(files, "rebase only"); n != 1 {
		t.Errorf("file list marks %d files rebase-only, want 1", n)
	}
	if i, j := strings.Index(files, "rebase only"), strings.Index(files, `data-file="shared.txt"`); i > j {
		t.Error("shared.txt is marked rebase-only, but the change edits it")
	}

	diff := mustGet(t, s, repoURL(t, r, "/d/"+topChangeKey+"?base=1&s=2&f=shared.txt"))
	if !strings.Contains(diff, "Muted lines came along with a rebase") {
		t.Error("diff does not explain its muted lines")
	}
	// One edit is the change's own and one came from below, so each side
	// carries both a plain highlight and a muted one.
	var plain, muted []string
	for _, m := range regexp.MustCompile(`class="code ([^"]+)"`).FindAllStringSubmatch(diff, -1) {
		if strings.Contains(m[1], "rebased") {
			muted = append(muted, m[1])
		} else {
			plain = append(plain, m[1])
		}
	}
	if len(plain) != 2 || len(muted) != 2 {
		t.Errorf("highlighted cells: plain %q, muted %q; want two of each", plain, muted)
	}
	for _, cells := range [][]string{plain, muted} {
		if len(cells) == 2 && !(strings.Contains(cells[0], "remove") && strings.Contains(cells[1], "add")) {
			t.Errorf("cells %q are not a removal and an addition", cells)
		}
	}

	// Against the parent commit the whole diff is the change's own work,
	// so nothing is muted and the note does not appear.
	whole := mustGet(t, s, repoURL(t, r, "/d/"+topChangeKey+"?base=parent&s=2&f=shared.txt"))
	if strings.Contains(whole, "rebased") || strings.Contains(whole, "Muted lines") {
		t.Error("the parent-to-snapshot diff mutes lines")
	}
}

// TestRebaseOnlyBanner covers what the base selector says when it has
// chosen the base itself. Normally it explains the choice; when the file
// on screen is one the change never touched, the more useful thing to say
// is that none of what follows is the change's own work.
func TestRebaseOnlyBanner(t *testing.T) {
	r, _ := newStackedReview(t)
	s := newServer(r.DB, r.Root(), r.Pin)

	// Mark snapshot 1 reviewed, which is what makes the base automatic.
	snaps, err := r.DB.Snapshots(r.Root(), topChangeKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("got %d snapshots, want 2", len(snaps))
	}
	if err := r.DB.SetSnapshotReviewed(snaps[0].ID, true); err != nil {
		t.Fatal(err)
	}

	const (
		chose  = "showing changes since you last reviewed"
		theirs = "changes in file are entirely due to earlier commits"
		note   = "Muted lines came along with a rebase"
	)

	// deep.txt is wholly inherited: say so, and do not follow it with the
	// note saying the same thing about the colors.
	got := mustGet(t, s, repoURL(t, r, "/d/"+topChangeKey+"?f=deep.txt"))
	if !strings.Contains(got, theirs) {
		t.Errorf("diff of an untouched file does not say so:\n%s", got)
	}
	for _, unwanted := range []string{chose, note} {
		if strings.Contains(got, unwanted) {
			t.Errorf("diff of an untouched file also says %q", unwanted)
		}
	}

	// shared.txt is partly the change's own work, so the base selector
	// explains its choice as before and the note explains the colors.
	got = mustGet(t, s, repoURL(t, r, "/d/"+topChangeKey+"?f=shared.txt"))
	if !strings.Contains(got, chose) || !strings.Contains(got, note) {
		t.Errorf("diff of an edited file lost its explanations:\n%s", got)
	}
	if strings.Contains(got, theirs) {
		t.Errorf("diff of an edited file claims the change did not touch it")
	}

	// With a base asked for in the URL there is no choice to explain, so
	// the note is the only thing that says why the lines are muted.
	got = mustGet(t, s, repoURL(t, r, "/d/"+topChangeKey+"?base=1&s=2&f=deep.txt"))
	if !strings.Contains(got, note) {
		t.Errorf("muted lines went unexplained:\n%s", got)
	}
	for _, unwanted := range []string{chose, theirs} {
		if strings.Contains(got, unwanted) {
			t.Errorf("an explicit base produced %q", unwanted)
		}
	}

	// The change page has no one file to say it about.
	got = mustGet(t, s, repoURL(t, r, "/c/"+topChangeKey))
	if !strings.Contains(got, chose) || strings.Contains(got, theirs) {
		t.Errorf("change page banner is wrong:\n%s", got)
	}
}

// TestReviewedThreeStates covers the file's three states and the two things
// the keyboard needs to step over what is not this change's work: the
// rebase flag on each file, and the mark on each inherited diff row.
func TestReviewedThreeStates(t *testing.T) {
	r, _ := newStackedReview(t)
	s := newServer(r.DB, r.Root(), r.Pin)
	url := repoURL(t, r, "/c/"+topChangeKey+"?base=1&s=2")

	// deep.txt is rebase-only; shared.txt is unreviewed.
	files := mustGet(t, s, url)
	if !strings.Contains(files, "rebase only") || !strings.Contains(files, "mark reviewed") {
		t.Errorf("file list is missing a state:\n%s", files)
	}

	// Marking the rebase-only file reviewed still works, and the verdict
	// wins over the fact: a file someone has read is reviewed.
	snaps, err := r.DB.Snapshots(r.Root(), topChangeKey)
	if err != nil {
		t.Fatal(err)
	}
	target := snaps[len(snaps)-1]
	if err := r.DB.SetReviewed(target.ID, "deep.txt", true); err != nil {
		t.Fatal(err)
	}
	files = mustGet(t, s, url)
	if strings.Contains(files, "rebase only") {
		t.Error("a file marked reviewed still shows as rebase-only")
	}
	if n := strings.Count(files, "✓ reviewed"); n != 1 {
		t.Errorf("%d files marked reviewed, want 1", n)
	}
	if err := r.DB.SetReviewed(target.ID, "deep.txt", false); err != nil {
		t.Fatal(err)
	}

	// The file list the keyboard walks says which files to skip, so that M
	// passes over a file with nothing in it to review.
	diff := mustGet(t, s, repoURL(t, r, "/d/"+topChangeKey+"?base=1&s=2&f=shared.txt"))
	var entries []struct {
		Path       string `json:"path"`
		RebaseOnly bool   `json:"rebase"`
	}
	data := diff[strings.Index(diff, `id="filedata"`):]
	data = data[strings.Index(data, ">")+1 : strings.Index(data, "</script>")]
	if err := json.Unmarshal([]byte(html.UnescapeString(data)), &entries); err != nil {
		t.Fatalf("parsing file data: %v\n%s", err, data)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Path] = e.RebaseOnly
	}
	if !got["deep.txt"] || got["shared.txt"] || got[CommitMsgFile] {
		t.Errorf("rebase flags = %v; want only deep.txt", got)
	}

	// And every row of an inherited chunk is marked, so that n and p can
	// step over the chunk the same way they step over unchanged text.
	for _, tt := range []struct {
		file  string
		want  int
		other int
	}{
		// Each edit replaces one line, so it is one side-by-side row.
		{file: "deep.txt", want: 1, other: 0},
		{file: "shared.txt", want: 1, other: 1},
	} {
		body := mustGet(t, s, repoURL(t, r, "/d/"+topChangeKey+"?base=1&s=2&f="+tt.file))
		rebased, plain := 0, 0
		for _, m := range regexp.MustCompile(`<tr class="diffrow ([^"]+)"`).FindAllStringSubmatch(body, -1) {
			switch {
			case strings.Contains(m[1], "rebased"):
				rebased++
			case !strings.Contains(m[1], "equal"):
				plain++
			}
		}
		if rebased != tt.want || plain != tt.other {
			t.Errorf("%s: %d rebased rows and %d of the change's own; want %d and %d",
				tt.file, rebased, plain, tt.want, tt.other)
		}
	}
}

// TestRebaseOnlyFilesHidden checks that the file list leaves out the files
// the change does not touch, and offers them at the foot of the list.
func TestRebaseOnlyFilesHidden(t *testing.T) {
	r, _ := newStackedReview(t)
	s := newServer(r.DB, r.Root(), r.Pin)

	files := mustGet(t, s, repoURL(t, r, "/c/"+topChangeKey+"?base=1&s=2"))
	// The rows are still rendered, so that showing them costs no round
	// trip; the table says they start out hidden.
	if !strings.Contains(files, `class="filelist hiderebase"`) {
		t.Errorf("file list does not hide anything:\n%s", files)
	}
	if !strings.Contains(files, `class="item filerow rebaseonly"`) {
		t.Error("no row is marked rebase-only")
	}
	if n := strings.Count(files, "rebaseonly"); n != 1 {
		t.Errorf("%d rows marked rebase-only, want 1", n)
	}
	if !strings.Contains(files, "show 1 rebase-only file\n") {
		t.Errorf("no link to show the hidden file:\n%s", files)
	}
	if !strings.Contains(files, `data-count="1"`) {
		t.Error("the link does not carry the count the label is rebuilt from")
	}

	// Against the parent commit the change is all its own work, so there
	// is nothing to hide and no link to offer.
	whole := mustGet(t, s, repoURL(t, r, "/c/"+topChangeKey+"?base=parent&s=2"))
	for _, unwanted := range []string{"hiderebase", "rebaseonly", "rebase-only file"} {
		if strings.Contains(whole, unwanted) {
			t.Errorf("the whole-change file list mentions %q", unwanted)
		}
	}
}

// TestRebaseOnlyWithCommentsShown checks that a comment keeps a file in the
// list. The file is still one the change does not touch, so it still reads
// rebase-only; what it is not is out of sight.
func TestRebaseOnlyWithCommentsShown(t *testing.T) {
	r, _ := newStackedReview(t)
	s := newServer(r.DB, r.Root(), r.Pin)
	url := repoURL(t, r, "/c/"+topChangeKey+"?base=1&s=2")

	// Without a comment, deep.txt is held back.
	files := mustGet(t, s, url)
	if !strings.Contains(files, "show 1 rebase-only file") {
		t.Fatalf("deep.txt is not held back to begin with:\n%s", files)
	}

	snaps, err := r.DB.Snapshots(r.Root(), topChangeKey)
	if err != nil {
		t.Fatal(err)
	}
	target := snaps[len(snaps)-1]
	if _, err := r.DB.AddThread(target.ID, "deep.txt", "new", 2, "two BOTTOM-V2", &Comment{
		Author: "rsc", Body: "did this come from below?", Draft: false,
	}); err != nil {
		t.Fatal(err)
	}

	files = mustGet(t, s, url)
	if strings.Contains(files, "rebaseonly") {
		t.Error("a file with a comment on it is still held back")
	}
	// Nothing left to hide, so the link goes away.
	for _, unwanted := range []string{"hiderebase", "rebase-only file"} {
		if strings.Contains(files, unwanted) {
			t.Errorf("file list still mentions %q with nothing to hide", unwanted)
		}
	}
	// The state is unchanged: the change still does not touch the file.
	if !strings.Contains(files, "rebase only") {
		t.Error("the file stopped reading rebase-only when it was shown")
	}
	if !strings.Contains(files, "1 comment") {
		t.Error("the comment count is missing")
	}
}

// TestCommentedFileListedWhenUnchanged checks that a file carrying comments
// stays reachable even when the view it is shown in has no diff for it at
// all. Comparing two snapshots narrows the list to what moved between them;
// a comment on anything else must not fall out of the world with it.
func TestCommentedFileListedWhenUnchanged(t *testing.T) {
	r, _ := newStackedReview(t)
	s := newServer(r.DB, r.Root(), r.Pin)
	url := repoURL(t, r, "/c/"+topChangeKey+"?base=1&s=2")

	// base.txt is in the repository but in neither change, so it is in no
	// diff of this change at all.
	before := mustGet(t, s, url)
	if strings.Contains(before, `data-file="base.txt"`) {
		t.Fatal("base.txt is in the change's diff after all")
	}

	snaps, err := r.DB.Snapshots(r.Root(), topChangeKey)
	if err != nil {
		t.Fatal(err)
	}
	target := snaps[len(snaps)-1]
	if _, err := r.DB.AddThread(target.ID, "base.txt", "new", 1, "base", &Comment{
		Author: "rsc", Body: "is this still wanted?", Draft: false,
	}); err != nil {
		t.Fatal(err)
	}

	files := mustGet(t, s, url)
	if !strings.Contains(files, `data-file="base.txt"`) {
		t.Error("a commented file is missing from the list")
	}
	// It has no status letter, because the change did nothing to it.
	if !strings.Contains(files, `class="status status-"`) {
		t.Error("the listed file was given a status it does not have")
	}
	// It is not rebase-only either: nothing was carried in by a rebase.
	if strings.Contains(files, "rebase only") && !strings.Contains(files, "show 1 rebase-only file") {
		t.Error("an unchanged file was labelled rebase-only")
	}

	// And it opens, showing the file and the comment on it rather than an
	// error about the file not being part of the change.
	diff := mustGet(t, s, repoURL(t, r, "/d/"+topChangeKey+"?base=1&s=2&f=base.txt"))
	if !strings.Contains(diff, "is this still wanted?") {
		t.Error("the comment is not on the page")
	}
	if !strings.Contains(diff, "base") {
		t.Error("the file's text is not shown")
	}

	// A comment on something long since gone leaves no row to click: it
	// keeps its place in the change's comment history instead.
	if _, err := r.DB.AddThread(target.ID, "vanished.txt", "new", 1, "gone", &Comment{
		Author: "rsc", Body: "about a file that is not there", Draft: false,
	}); err != nil {
		t.Fatal(err)
	}
	files = mustGet(t, s, url)
	if strings.Contains(files, `data-file="vanished.txt"`) {
		t.Error("listed a file that does not exist")
	}
	if !strings.Contains(files, "about a file that is not there") {
		t.Error("the comment on it is not in the history either")
	}
}

// TestCommentLinkOpensAgainstLatest follows the link from a comment in the
// change page's history and checks where it lands: the comment's own
// snapshot on the left, the newest on the right, and the comment beside
// the text it was written about rather than adrift.
func TestCommentLinkOpensAgainstLatest(t *testing.T) {
	r, _ := newStackedReview(t)
	s := newServer(r.DB, r.Root(), r.Pin)

	snaps, err := r.DB.Snapshots(r.Root(), topChangeKey)
	if err != nil {
		t.Fatal(err)
	}
	// A comment on shared.txt as it stood in snapshot 1, on the line the
	// change itself went on to edit.
	th, err := r.DB.AddThread(snaps[0].ID, "shared.txt", "new", 5, "TOP1", &Comment{
		Author: "rsc", Body: "this name reads badly", Draft: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	body := mustGet(t, s, repoURL(t, r, "/c/"+topChangeKey))
	want := "base=1&amp;f=shared.txt&amp;s=2#thread-" + strconv.FormatInt(th.ID, 10)
	if !strings.Contains(body, want) {
		t.Fatalf("history does not link %s:\n%s", want, body)
	}

	diff := mustGet(t, s, repoURL(t, r, "/d/"+topChangeKey+"?base=1&s=2&f=shared.txt"))
	i := strings.Index(diff, "this name reads badly")
	if i < 0 {
		t.Fatalf("the comment is not on the page:\n%s", diff)
	}
	// It is anchored, not adrift: the snapshot it belongs to is on screen,
	// so it is neither stale nor labelled as coming from somewhere else.
	for _, chip := range []string{`class="chip stalechip"`, `class="chip snap"`} {
		if strings.Contains(diff, chip) {
			t.Errorf("comment carries %s though its own snapshot is shown", chip)
		}
	}
	// And it sits in the left-hand thread cell, beside the text it is
	// about, with what replaced that text alongside on the right.
	row := strings.LastIndex(diff[:i], `<tr class="threadrow">`)
	if row < 0 {
		t.Fatal("the comment is not in a thread row")
	}
	if n := strings.Count(diff[row:i], `<td class="threadcell"`); n != 1 {
		t.Errorf("comment is in thread cell %d of its row, want the first (left)", n)
	}
}

// TestPublishAll covers the repository-wide publish button: it counts the
// drafts it would publish, publishes all of them across every change, and
// is offered only when there is something to publish.
func TestPublishAll(t *testing.T) {
	r, dir := newReview(t)
	s := newServer(r.DB, r.Root(), r.Pin)

	write(t, dir, "a.go", "package p\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "one\n\nChange-Id: Ione\n")
	write(t, dir, "b.go", "package q\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "two\n\nChange-Id: Itwo\n")

	// With no drafts anywhere, the button is not there.
	page := mustGet(t, s, repoURL(t, r, ""))
	if strings.Contains(page, "Publish All") {
		t.Errorf("publish button offered with nothing to publish:\n%s", page)
	}

	// A draft on each change, and one already published for company.
	for _, key := range []string{"Ione", "Itwo"} {
		c, err := r.Change(key)
		if err != nil {
			t.Fatal(err)
		}
		snaps, err := r.EnsureSnapshot(c)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.DB.AddThread(snaps[0].ID, CommitMsgFile, "new", 1, "", &Comment{
			Author: "rsc", Body: "draft on " + key, Draft: true,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.DB.AddThread(snaps[0].ID, CommitMsgFile, "new", 2, "", &Comment{
			Author: "rsc", Body: "published on " + key, Draft: false,
		}); err != nil {
			t.Fatal(err)
		}
	}

	page = mustGet(t, s, repoURL(t, r, ""))
	if !strings.Contains(page, "Publish All (2 drafts)") {
		t.Errorf("publish button missing or miscounted:\n%s", page)
	}

	// Publishing with no change named publishes them all.
	w := post(t, s, repoURL(t, r, "/publish"), url.Values{"return": {"/"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /publish = %d: %s", w.Code, w.Body.String())
	}
	for _, key := range []string{"Ione", "Itwo"} {
		threads, err := r.DB.Threads(r.Root(), key)
		if err != nil {
			t.Fatal(err)
		}
		for _, th := range threads {
			for _, c := range th.Comments {
				if c.Draft {
					t.Errorf("%s: %q is still a draft", key, c.Body)
				}
			}
		}
	}
	if n, err := r.DB.DraftCount(r.Root()); err != nil || n != 0 {
		t.Errorf("DraftCount = %d, %v; want 0", n, err)
	}

	// And the button goes away again.
	page = mustGet(t, s, repoURL(t, r, ""))
	if strings.Contains(page, "Publish All") {
		t.Error("publish button still offered after publishing everything")
	}
}

// TestPublishOneStillWorks checks that naming a change still publishes
// only that change, now that naming nothing means all of them.
func TestPublishOneStillWorks(t *testing.T) {
	r, dir := newReview(t)
	s := newServer(r.DB, r.Root(), r.Pin)

	write(t, dir, "a.go", "package p\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "one\n\nChange-Id: Ione\n")
	write(t, dir, "b.go", "package q\n")
	do(t, dir, "git", "add", ".")
	do(t, dir, "git", "commit", "-q", "-m", "two\n\nChange-Id: Itwo\n")

	for _, key := range []string{"Ione", "Itwo"} {
		c, _ := r.Change(key)
		snaps, err := r.EnsureSnapshot(c)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.DB.AddThread(snaps[0].ID, CommitMsgFile, "new", 1, "", &Comment{
			Author: "rsc", Body: "draft on " + key, Draft: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	post(t, s, repoURL(t, r, "/publish"), url.Values{"key": {"Ione"}, "return": {"/"}})
	if n, err := r.DB.DraftCount(r.Root()); err != nil || n != 1 {
		t.Errorf("DraftCount = %d, %v; want 1 left on the other change", n, err)
	}
}
