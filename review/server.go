// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed static tmpl
var embedFS embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"since":       since,
	"base":        path.Base,
	"keySections": func() []keySection { return keyHelp },
	"keyMap":      func() template.JS { return template.JS(keyMapJSON()) },
	"kbd":         kbdHTML,
	"threadArg":   threadArg,
	"reviewedArg": reviewedArg,
	"snapshotArg": snapshotReviewedArg,
	"lgtmArg":     lgtmArg,
}).ParseFS(embedFS, "tmpl/*.html"))

// since renders a time the way Gerrit does in change lists.
func since(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		return fmt.Sprintf("%d hour%s ago", h, plural(h))
	case d < 365*24*time.Hour:
		return t.Format("Jan 2")
	}
	return t.Format("Jan 2, 2006")
}

// kbdHTML renders key sequences for the help dialog, so that "g i" shows
// as two separate keys pressed in turn.
func kbdHTML(keys []string) template.HTML {
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(` <span class="or">or</span> `)
		}
		for j, part := range strings.Fields(k) {
			if j > 0 {
				b.WriteString(" ")
			}
			fmt.Fprintf(&b, "<kbd>%s</kbd>", template.HTMLEscapeString(part))
		}
	}
	return template.HTML(b.String())
}

func threadArg(t *Thread, repo, user string) *threadFrag {
	return &threadFrag{Thread: t, Repo: repo, User: user}
}

// reviewedInfo drives the "mark reviewed" button. Building the URL in Go
// keeps file names containing & or ? from breaking the markup.
type reviewedInfo struct {
	URL      string
	Reviewed bool
}

func reviewedArg(repo string, snapshotID int64, file string, on bool) *reviewedInfo {
	q := url.Values{"snapshot": {strconv.FormatInt(snapshotID, 10)}, "f": {file}}
	q.Set("on", boolParam(!on))
	return &reviewedInfo{URL: "/" + url.PathEscape(repo) + "/f/reviewed?" + q.Encode(), Reviewed: on}
}

// snapshotReviewedArg drives the reviewed button on a whole snapshot.
func snapshotReviewedArg(repo string, snapshotID int64, on bool) *reviewedInfo {
	q := url.Values{"snapshot": {strconv.FormatInt(snapshotID, 10)}}
	q.Set("on", boolParam(!on))
	return &reviewedInfo{URL: "/" + url.PathEscape(repo) + "/f/snapshot-reviewed?" + q.Encode(), Reviewed: on}
}

// lgtmArg drives the LGTM button, which marks the snapshot being viewed
// as looking good. Like the reviewed mark it belongs to that snapshot, so
// a later snapshot starts out without it.
func lgtmArg(repo string, snapshotID int64, on bool) *reviewedInfo {
	q := url.Values{"snapshot": {strconv.FormatInt(snapshotID, 10)}}
	q.Set("on", boolParam(!on))
	return &reviewedInfo{URL: "/" + url.PathEscape(repo) + "/f/lgtm?" + q.Encode(), Reviewed: on}
}

func boolParam(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// A server serves the review web interface for every repository the
// database knows about, each under its own short name.
type server struct {
	db   *DB
	home string // the repository review was started in
	pin  bool
	user string
	mux  *http.ServeMux

	mu   sync.Mutex
	open map[string]*Review // by repository path
}

// newServer serves every repository in db. home, if not empty, is the
// repository review was started in, which is listed even if it has never
// been reviewed; the server runs perfectly well without one, since the
// repositories it serves come from the database.
func newServer(db *DB, home string, pin bool) *server {
	user := "you"
	if home != "" {
		user = gitUser(home)
	}
	s := &server{
		db:   db,
		home: home,
		pin:  pin,
		user: user,
		mux:  http.NewServeMux(),
		open: map[string]*Review{},
	}
	if home != "" {
		if _, err := s.db.RepoName(home); err != nil {
			log.Printf("naming %s: %v", home, err)
		}
	}

	// Browsers ask for this unprompted; answering it here keeps it out of
	// the log as an unknown repository.
	s.mux.HandleFunc("GET /favicon.ico", http.NotFound)
	s.mux.HandleFunc("GET "+healthPath, func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, healthBody)
	})
	s.mux.HandleFunc("GET /{$}", s.handle(s.all))
	s.mux.HandleFunc("GET /{repo}", s.repoHandle(s.changes))
	s.mux.HandleFunc("GET /{repo}/c/{key}", s.repoHandle(s.files))
	s.mux.HandleFunc("GET /{repo}/d/{key}", s.repoHandle(s.diff))
	s.mux.HandleFunc("POST /{repo}/snapshot", s.repoHandle(s.snapshot))
	s.mux.HandleFunc("POST /{repo}/publish", s.repoHandle(s.publish))
	s.mux.HandleFunc("POST /prefs", s.handle(s.prefs))
	s.mux.HandleFunc("GET /{repo}/f/form", s.repoHandle(s.form))
	s.mux.HandleFunc("POST /{repo}/f/comment", s.repoHandle(s.comment))
	s.mux.HandleFunc("POST /{repo}/f/resolve", s.repoHandle(s.resolve))
	s.mux.HandleFunc("GET /{repo}/f/thread", s.repoHandle(s.thread))
	s.mux.HandleFunc("GET /{repo}/f/editform", s.repoHandle(s.editForm))
	s.mux.HandleFunc("POST /{repo}/f/edit", s.repoHandle(s.edit))
	s.mux.HandleFunc("POST /{repo}/f/delete", s.repoHandle(s.deleteComment))
	s.mux.HandleFunc("POST /{repo}/f/undelete", s.repoHandle(s.undelete))
	s.mux.HandleFunc("POST /{repo}/f/reviewed", s.repoHandle(s.reviewed))
	s.mux.HandleFunc("POST /{repo}/f/snapshot-reviewed", s.repoHandle(s.snapshotReviewed))
	s.mux.HandleFunc("POST /{repo}/f/lgtm", s.repoHandle(s.lgtm))
	s.mux.HandleFunc("GET /{repo}/f/expand", s.repoHandle(s.expand))
	s.mux.HandleFunc("GET /{repo}/f/inline", s.repoHandle(s.inline))
	// The assets sit at the top level as literal paths. A /static/ prefix
	// would collide with /{repo}/snapshot and /{repo}/c/{key}, which the
	// mux rightly refuses to order; a literal always beats a wildcard.
	for _, name := range staticFiles {
		s.mux.HandleFunc("GET /"+name, serveStatic("static/"+name))
	}
	// Anything else may be the full path of a repository, which redirects
	// to the short name it is known by.
	s.mux.HandleFunc("GET /{path...}", s.handle(s.byPath))
	return s
}

// staticFiles are served from the top level, so that no repository name
// can shadow them.
var staticFiles = []string{"style.css", "keys.js", "htmx.min.js", "favicon.svg"}

func serveStatic(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		http.ServeFileFS(w, req, embedFS, path)
	}
}

// review returns the repository with the given short name, opening it the
// first time it is asked for.
func (s *server) review(name string) (*Review, error) {
	path, err := s.db.RepoPath(name)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.open[path]; r != nil {
		return r, nil
	}
	repo, err := OpenRepo(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	r := &Review{Repo: repo, DB: s.db, Pin: s.pin, Name: name}
	s.open[path] = r
	return r, nil
}

// repoHandle wraps a handler that works within one repository, named by
// the first element of the path.
func (s *server) repoHandle(f func(http.ResponseWriter, *http.Request, *Review) error) http.HandlerFunc {
	return s.handle(func(w http.ResponseWriter, req *http.Request) error {
		r, err := s.review(req.PathValue("repo"))
		if err != nil {
			return err
		}
		return f(w, req, r)
	})
}

// byPath redirects the full path of a repository to its short name, so
// that a path pasted from a shell reaches the right page.
func (s *server) byPath(w http.ResponseWriter, req *http.Request) error {
	path := filepath.Clean(req.URL.Path)
	known, err := s.repoPaths()
	if err != nil {
		return err
	}
	for _, p := range known {
		if p == path {
			name, err := s.db.RepoName(p)
			if err != nil {
				return err
			}
			http.Redirect(w, req, "/"+url.PathEscape(name), http.StatusSeeOther)
			return nil
		}
	}
	http.NotFound(w, req)
	return nil
}

// repoPaths returns every repository to show, which is everything that
// has been reviewed plus the one review was started in.
func (s *server) repoPaths() ([]string, error) {
	paths, err := s.db.Repos()
	if err != nil {
		return nil, err
	}
	if s.home != "" && !slices.Contains(paths, s.home) {
		paths = append(paths, s.home)
	}
	slices.Sort(paths)
	return paths, nil
}

func (s *server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	s.mux.ServeHTTP(w, req)
}

// handle wraps a handler that can fail, reporting errors to the browser
// rather than losing them.
func (s *server) handle(f func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if err := f(w, req); err != nil {
			log.Printf("%s %s: %v", req.Method, req.URL, err)
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	}
}

// head is the data every page needs.
type head struct {
	Title    string
	Page     string // which keyboard shortcut context applies
	Repo     string // short name, empty on the all-repositories page
	RepoPath string
	RepoURL  string
	FileName string // the file being viewed, for the top bar
	Kind     string
	User     string
}

func (s *server) head(r *Review, title, page string) head {
	return head{
		Title: title, Page: page,
		Repo: r.Name, RepoPath: r.Root(), RepoURL: "/" + url.PathEscape(r.Name),
		Kind: r.Repo.Kind(), User: s.user,
	}
}

// headAll is the header for the page listing every repository.
func (s *server) headAll(title string) head {
	return head{Title: title, Page: "all", User: s.user}
}

// changeInfo summarizes one change for the change list.
type changeInfo struct {
	*Change
	Snapshots  int
	Comments   int
	Unresolved int
	Drafts     int
	Reviewed   bool // the newest snapshot has been marked reviewed
	LGTM       bool // the newest snapshot has been marked LGTM
	URL        string
}

type changesPage struct {
	head
	Changes []*changeInfo
}

func (s *server) changes(w http.ResponseWriter, req *http.Request, r *Review) error {
	changes, err := r.Repo.Changes()
	if err != nil {
		return err
	}
	p := &changesPage{head: s.head(r, "review: "+r.Name, "changes")}
	for _, c := range changes {
		info, err := s.changeInfo(r, c)
		if err != nil {
			return err
		}
		info.URL = s.filesURL(r, c.Key, "", "")
		p.Changes = append(p.Changes, info)
	}
	return tmpl.ExecuteTemplate(w, "changes.html", p)
}

// repoInfo names one repository on the all-repositories page.
type repoInfo struct {
	Name string
	Path string
	URL  string
	Err  string // why its changes could not be listed, if they could not
}

// allRow is one pending change, wherever it lives.
type allRow struct {
	*changeInfo
	Repo *repoInfo
	URL  string
}

type allPage struct {
	head
	Rows  []*allRow
	Repos []*repoInfo
}

// all lists the pending changes of every repository that has been
// reviewed, newest commit first, so that the work most recently touched
// is at the top whichever repository it is in.
func (s *server) all(w http.ResponseWriter, req *http.Request) error {
	paths, err := s.repoPaths()
	if err != nil {
		return err
	}
	p := &allPage{head: s.headAll("review")}
	for _, path := range paths {
		name, err := s.db.RepoName(path)
		if err != nil {
			return err
		}
		info := &repoInfo{Name: name, Path: path, URL: "/" + url.PathEscape(name)}
		p.Repos = append(p.Repos, info)

		// A repository that has been moved or deleted must not take the
		// whole page down with it.
		r, err := s.review(name)
		if err != nil {
			info.Err = err.Error()
			continue
		}
		changes, err := r.Repo.Changes()
		if err != nil {
			info.Err = err.Error()
			continue
		}
		for _, c := range changes {
			ci, err := s.changeInfo(r, c)
			if err != nil {
				return err
			}
			p.Rows = append(p.Rows, &allRow{changeInfo: ci, Repo: info, URL: s.filesURL(r, c.Key, "", "")})
		}
	}
	// Newest commit first, across every repository.
	sort.SliceStable(p.Rows, func(i, j int) bool {
		return p.Rows[i].Date.After(p.Rows[j].Date)
	})
	return tmpl.ExecuteTemplate(w, "all.html", p)
}

// messageLines is how much of a commit message the change page shows
// before folding the rest away.
const messageLines = 10

// splitMessage divides a commit message into the part to show and the
// part to fold away, and reports how many lines were folded.
func splitMessage(msg string) (head, rest string, more int) {
	msg = strings.TrimRight(msg, "\n")
	lines := strings.Split(msg, "\n")
	if len(lines) <= messageLines {
		return msg, "", 0
	}
	return strings.Join(lines[:messageLines], "\n"),
		strings.Join(lines[messageLines:], "\n"),
		len(lines) - messageLines
}

// changeInfo gathers the counts shown beside a change in either list.
func (s *server) changeInfo(r *Review, c *Change) (*changeInfo, error) {
	info := &changeInfo{Change: c}
	snaps, err := r.DB.Snapshots(r.Root(), c.Key)
	if err != nil {
		return nil, err
	}
	info.Snapshots = len(snaps)
	if n := len(snaps); n > 0 {
		// Only the newest snapshot counts: reviewing an older one says
		// nothing about the state the change is in now.
		if info.Reviewed, err = r.DB.SnapshotReviewed(snaps[n-1].ID); err != nil {
			return nil, err
		}
		if info.LGTM, err = r.DB.SnapshotLGTM(snaps[n-1].ID); err != nil {
			return nil, err
		}
	}
	threads, err := r.DB.Threads(r.Root(), c.Key)
	if err != nil {
		return nil, err
	}
	for _, t := range threads {
		info.Comments += len(t.Comments)
		if !t.Resolved {
			info.Unresolved++
		}
		for _, cm := range t.Comments {
			if cm.Draft {
				info.Drafts++
			}
		}
	}
	return info, nil
}

// fileInfo summarizes one file for the file list and diff header.
type fileInfo struct {
	*File
	Comments   int
	Unresolved int
	Reviewed   bool
	URL        string
}

// Name returns the file's display name.
func (f *fileInfo) Name() string {
	if f.Path == CommitMsgFile {
		return "Commit Message"
	}
	if f.OldPath != "" {
		return f.OldPath + " → " + f.Path
	}
	return f.Path
}

// nav holds the URL parameters and links the base selector and page
// actions need. The fields are named apart from View's Base and Target
// so that the two can be embedded side by side.
type nav struct {
	RepoName    string
	Key         string
	BaseParam   string
	TargetParam string
	FileParam   string
	SelfURL     string
	PageURL     string
}

type filesPage struct {
	head
	*View
	nav
	MessageHead string // the first messageLines lines of the commit message
	MessageRest string // the rest of it, empty when there is no more
	MoreLines   int
	Files       []*fileInfo
	Reviewed    map[int64]bool // snapshots marked reviewed, by snapshot ID
	LGTM        bool           // the snapshot being viewed is marked LGTM
	Threads     []*threadFrag  // comment history, oldest first
	Comments    int
	Unresolved  int
	Drafts      int
}

func (s *server) view(req *http.Request, r *Review) (*Change, *View, error) {
	c, err := r.Change(req.PathValue("key"))
	if err != nil {
		return nil, nil, err
	}
	v, err := r.View(c, req.FormValue("base"), req.FormValue("s"))
	if err != nil {
		return nil, nil, err
	}
	return c, v, nil
}

func (s *server) files(w http.ResponseWriter, req *http.Request, r *Review) error {
	c, v, err := s.view(req, r)
	if err != nil {
		return err
	}
	threads, err := r.DB.Threads(r.Root(), c.Key)
	if err != nil {
		return err
	}
	reviewed := map[string]bool{}
	if v.Target != nil {
		if reviewed, err = r.DB.Reviewed(v.Target.ID); err != nil {
			return err
		}
	}

	base, target := req.FormValue("base"), req.FormValue("s")
	p := &filesPage{
		head: s.head(r, c.Subject, "files"),
		View: v,
		nav: nav{
			RepoName: r.Name, Key: c.Key, BaseParam: base, TargetParam: target,
			SelfURL: s.filesURL(r, c.Key, base, target),
			PageURL: "/" + url.PathEscape(r.Name) + "/c/" + url.PathEscape(c.Key),
		},
	}
	for _, f := range v.Files {
		info := &fileInfo{File: f, Reviewed: reviewed[f.Path], URL: s.diffURL(r, c.Key, f.Path, base, target)}
		for _, t := range threads {
			if t.File != f.Path {
				continue
			}
			info.Comments += len(t.Comments)
			if !t.Resolved {
				info.Unresolved++
			}
		}
		p.Files = append(p.Files, info)
	}
	if p.Reviewed, err = r.DB.ReviewedSnapshots(r.Root(), c.Key); err != nil {
		return err
	}
	if v.Target != nil {
		if p.LGTM, err = r.DB.SnapshotLGTM(v.Target.ID); err != nil {
			return err
		}
	}
	p.MessageHead, p.MessageRest, p.MoreLines = splitMessage(c.Message)
	p.Drafts = countDrafts(threads)

	// The comment history reads chronologically, not in file order the
	// way the per-file counts above are gathered.
	history := append([]*Thread{}, threads...)
	sort.Slice(history, func(i, j int) bool {
		if !history[i].Created.Equal(history[j].Created) {
			return history[i].Created.Before(history[j].Created)
		}
		return history[i].ID < history[j].ID
	})
	for _, t := range history {
		p.Comments += len(t.Comments)
		if !t.Resolved {
			p.Unresolved++
		}
		p.Threads = append(p.Threads, s.threadFrag(r, t, c.Key))
	}
	return tmpl.ExecuteTemplate(w, "files.html", p)
}

// threadFrag wraps a thread for display away from its diff, with a link
// back to the file and snapshot it was written against.
func (s *server) threadFrag(r *Review, t *Thread, key string) *threadFrag {
	return &threadFrag{
		Thread: t,
		Repo:   r.Name,
		User:   s.user,
		Link: s.diffURL(r, key, t.File, "", strconv.Itoa(t.SnapshotN)) +
			"#thread-" + strconv.FormatInt(t.ID, 10),
	}
}

func countDrafts(threads []*Thread) int {
	n := 0
	for _, t := range threads {
		for _, c := range t.Comments {
			if c.Draft {
				n++
			}
		}
	}
	return n
}

func (s *server) diffURL(r *Review, key, file, base, target string) string {
	q := url.Values{"f": {file}}
	if base != "" {
		q.Set("base", base)
	}
	if target != "" {
		q.Set("s", target)
	}
	return "/" + url.PathEscape(r.Name) + "/d/" + url.PathEscape(key) + "?" + q.Encode()
}

func (s *server) filesURL(r *Review, key, base, target string) string {
	q := url.Values{}
	if base != "" {
		q.Set("base", base)
	}
	if target != "" {
		q.Set("s", target)
	}
	u := "/" + url.PathEscape(r.Name) + "/c/" + url.PathEscape(key)
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// renderRow is one row of the rendered diff, with its comment threads.
type renderRow struct {
	Row
	LeftHTML     template.HTML
	RightHTML    template.HTML
	LeftThreads  []*Thread
	RightThreads []*Thread
	LeftFormURL  string
	RightFormURL string
	ExpandURL    string
}

// Class returns the CSS class for the row's kind.
func (r *renderRow) Class() string {
	switch r.Kind {
	case RowReplace:
		return "replace"
	case RowDelete:
		return "delete"
	case RowInsert:
		return "insert"
	case RowSkip:
		return "skip"
	}
	return "equal"
}

// LeftClass and RightClass give the highlight classes Gerrit uses: "remove"
// and "add" for the pale line background, plus "total" when the chunk only
// adds or only removes and the strong color covers the whole line.
func (r *renderRow) LeftClass() string {
	return sideClass(r, "remove", r.Kind == RowReplace || r.Kind == RowDelete)
}

func (r *renderRow) RightClass() string {
	return sideClass(r, "add", r.Kind == RowReplace || r.Kind == RowInsert)
}

func sideClass(r *renderRow, name string, changed bool) string {
	if !changed {
		return ""
	}
	c := name
	if r.Total {
		c += " total"
	}
	if r.NoIntraline {
		c += " nointra"
	}
	return c
}

type diffPage struct {
	head
	*View
	nav
	File      *fileInfo
	Rows      []*renderRow
	Stale     []*Thread
	Unified   bool
	Context   int
	TabSize   int
	Binary    bool
	Prev      *fileInfo
	Next      *fileInfo
	FilesURL  string
	FilesJSON template.JS
	Drafts    int
}

// Cols is the number of columns in the diff table.
func (p *diffPage) Cols() int {
	if p.Unified {
		return 2
	}
	return 4
}

// diffOpts collects the display preferences for a diff.
type diffOpts struct {
	context int
	tabSize int
	unified bool
}

func (s *server) opts(req *http.Request) diffOpts {
	o := diffOpts{
		context: s.pref("context", 10),
		tabSize: s.pref("tabsize", 8),
		unified: s.pref("unified", 0) != 0,
	}
	if u := req.FormValue("unified"); u != "" {
		o.unified = u == "1"
	}
	if x := req.FormValue("context"); x != "" {
		if n, err := strconv.Atoi(x); err == nil {
			o.context = n
		}
	}
	return o
}

func (s *server) pref(key string, def int) int {
	n, err := strconv.Atoi(s.db.Pref(key, strconv.Itoa(def)))
	if err != nil {
		return def
	}
	return n
}

func (s *server) diff(w http.ResponseWriter, req *http.Request, r *Review) error {
	c, v, err := s.view(req, r)
	if err != nil {
		return err
	}
	name := req.FormValue("f")
	f := v.File(name)
	if f == nil {
		return fmt.Errorf("no file %q in this change", name)
	}
	base, target := req.FormValue("base"), req.FormValue("s")
	o := s.opts(req)

	old, new, err := r.Contents(v, f)
	if err != nil {
		return err
	}
	oldLines, _ := splitLines(old)
	newLines, _ := splitLines(new)

	fd := Diff(old, new)
	rows := Collapse(fd.Rows, o.context)
	if o.unified {
		rows = Unified(rows)
	}

	threads, err := r.DB.Threads(r.Root(), c.Key)
	if err != nil {
		return err
	}
	left, right := v.PlaceThreads(threads, name, oldLines, newLines)
	leftAt, rightAt := ThreadsByLine(left), ThreadsByLine(right)

	p := &diffPage{
		head:     s.head(r, path.Base(name)+" · "+c.Subject, "diff"),
		View:     v,
		nav:      nav{RepoName: r.Name, Key: c.Key, BaseParam: base, TargetParam: target, FileParam: name, PageURL: "/" + url.PathEscape(r.Name) + "/d/" + url.PathEscape(c.Key)},
		File:     &fileInfo{File: f},
		Unified:  o.unified,
		Context:  o.context,
		TabSize:  o.tabSize,
		Binary:   fd.Binary,
		Stale:    append(append([]*Thread{}, leftAt[0]...), rightAt[0]...),
		FilesURL: s.filesURL(r, c.Key, base, target),
		Drafts:   countDrafts(threads),
	}
	p.SelfURL = s.diffURL(r, c.Key, name, base, target)
	// The top bar stays put while the diff scrolls, so naming the file
	// there answers "what am I looking at" without scrolling back up.
	p.FileName = p.File.Name()
	p.Rows = s.renderRows(r, rows, p, leftAt, rightAt)

	// Neighbouring files, for the ] and [ shortcuts and the J/K jumps.
	type fileEntry struct {
		Path     string `json:"path"`
		URL      string `json:"url"`
		Comments int    `json:"comments"`
		Reviewed bool   `json:"reviewed"`
	}
	reviewed := map[string]bool{}
	if v.Target != nil {
		if reviewed, err = r.DB.Reviewed(v.Target.ID); err != nil {
			return err
		}
	}
	var entries []fileEntry
	for i, ff := range v.Files {
		n := 0
		for _, t := range threads {
			if t.File == ff.Path {
				n += len(t.Comments)
			}
		}
		entries = append(entries, fileEntry{ff.Path, s.diffURL(r, c.Key, ff.Path, base, target), n, reviewed[ff.Path]})
		if ff.Path != name {
			continue
		}
		if i > 0 {
			p.Prev = &fileInfo{File: v.Files[i-1], URL: s.diffURL(r, c.Key, v.Files[i-1].Path, base, target)}
		}
		if i+1 < len(v.Files) {
			p.Next = &fileInfo{File: v.Files[i+1], URL: s.diffURL(r, c.Key, v.Files[i+1].Path, base, target)}
		}
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	p.FilesJSON = template.JS(data)

	p.File.Reviewed = reviewed[name]
	return tmpl.ExecuteTemplate(w, "diff.html", p)
}

// renderRows turns diff rows into rendered rows, attaching comment threads
// and the URLs that open a comment form on each line.
func (s *server) renderRows(r *Review, rows []Row, p *diffPage, leftAt, rightAt map[int][]*Thread) []*renderRow {
	var out []*renderRow
	for _, row := range rows {
		rr := &renderRow{Row: row}
		rr.LeftHTML = lineHTML(row.L.Text, row.L.Spans)
		rr.RightHTML = lineHTML(row.R.Text, row.R.Spans)
		if row.L.Num > 0 {
			rr.LeftThreads = leftAt[row.L.Num]
			rr.LeftFormURL = s.formURL(p, "old", row.L.Num)
		}
		if row.R.Num > 0 {
			rr.RightThreads = rightAt[row.R.Num]
			rr.RightFormURL = s.formURL(p, "new", row.R.Num)
		}
		if row.Kind == RowSkip {
			rr.ExpandURL = s.expandURL(p, row)
		}
		out = append(out, rr)
	}
	return out
}

func (s *server) formURL(p *diffPage, side string, line int) string {
	q := url.Values{
		"key":  {p.Key},
		"f":    {p.FileParam},
		"side": {side},
		"line": {strconv.Itoa(line)},
	}
	if p.BaseParam != "" {
		q.Set("base", p.BaseParam)
	}
	if p.TargetParam != "" {
		q.Set("s", p.TargetParam)
	}
	if p.Unified {
		q.Set("unified", "1")
	}
	return "/" + url.PathEscape(p.RepoName) + "/f/form?" + q.Encode()
}

func (s *server) expandURL(p *diffPage, row Row) string {
	q := url.Values{
		"key":   {p.Key},
		"f":     {p.FileParam},
		"from":  {strconv.Itoa(row.LFrom)},
		"to":    {strconv.Itoa(row.LFrom + row.Count - 1)},
		"rfrom": {strconv.Itoa(row.RFrom)},
	}
	if p.BaseParam != "" {
		q.Set("base", p.BaseParam)
	}
	if p.TargetParam != "" {
		q.Set("s", p.TargetParam)
	}
	if p.Unified {
		q.Set("unified", "1")
	}
	return "/" + url.PathEscape(p.RepoName) + "/f/expand?" + q.Encode()
}

// fragPage builds the page context a row fragment needs, for the handlers
// that render rows outside a full page load.
func (s *server) fragPage(req *http.Request, r *Review) (*Change, *View, *diffPage, error) {
	c, err := r.Change(req.FormValue("key"))
	if err != nil {
		return nil, nil, nil, err
	}
	base, target := req.FormValue("base"), req.FormValue("s")
	v, err := r.View(c, base, target)
	if err != nil {
		return nil, nil, nil, err
	}
	o := s.opts(req)
	p := &diffPage{
		head:    s.head(r, "", "diff"),
		View:    v,
		nav:     nav{RepoName: r.Name, Key: c.Key, BaseParam: base, TargetParam: target, FileParam: req.FormValue("f")},
		Unified: o.unified,
		TabSize: o.tabSize,
		Context: o.context,
	}
	return c, v, p, nil
}

// expand returns the rows hidden behind an "N unchanged lines" marker.
func (s *server) expand(w http.ResponseWriter, req *http.Request, r *Review) error {
	c, v, p, err := s.fragPage(req, r)
	if err != nil {
		return err
	}
	f := v.File(p.FileParam)
	if f == nil {
		return fmt.Errorf("no file %q in this change", p.FileParam)
	}
	old, new, err := r.Contents(v, f)
	if err != nil {
		return err
	}
	oldLines, _ := splitLines(old)
	newLines, _ := splitLines(new)

	from, _ := strconv.Atoi(req.FormValue("from"))
	to, _ := strconv.Atoi(req.FormValue("to"))
	rfrom, _ := strconv.Atoi(req.FormValue("rfrom"))

	threads, err := r.DB.Threads(r.Root(), c.Key)
	if err != nil {
		return err
	}
	left, right := v.PlaceThreads(threads, p.FileParam, oldLines, newLines)
	leftAt, rightAt := ThreadsByLine(left), ThreadsByLine(right)

	var rows []Row
	for i := 0; from+i <= to; i++ {
		l, r := from+i, rfrom+i
		if l < 1 || l > len(oldLines) || r < 1 || r > len(newLines) {
			break
		}
		rows = append(rows, Row{
			Kind: RowEqual,
			L:    Line{Num: l, Text: oldLines[l-1]},
			R:    Line{Num: r, Text: newLines[r-1]},
		})
	}
	p.Rows = s.renderRows(r, rows, p, leftAt, rightAt)
	return tmpl.ExecuteTemplate(w, "rows", p)
}

// inline renders a file's diff as a table, for the file list's inline diffs.
func (s *server) inline(w http.ResponseWriter, req *http.Request, r *Review) error {
	c, v, p, err := s.fragPage(req, r)
	if err != nil {
		return err
	}
	f := v.File(p.FileParam)
	if f == nil {
		return fmt.Errorf("no file %q in this change", p.FileParam)
	}
	old, new, err := r.Contents(v, f)
	if err != nil {
		return err
	}
	oldLines, _ := splitLines(old)
	newLines, _ := splitLines(new)

	fd := Diff(old, new)
	rows := Collapse(fd.Rows, p.Context)
	if p.Unified {
		rows = Unified(rows)
	}
	threads, err := r.DB.Threads(r.Root(), c.Key)
	if err != nil {
		return err
	}
	left, right := v.PlaceThreads(threads, p.FileParam, oldLines, newLines)
	p.SelfURL = s.diffURL(r, c.Key, p.FileParam, p.BaseParam, p.TargetParam)
	p.Rows = s.renderRows(r, rows, p, ThreadsByLine(left), ThreadsByLine(right))

	fmt.Fprintf(w, `<table class="diff inlinediff%s"><tbody>`, map[bool]string{true: " unified"}[p.Unified])
	if err := tmpl.ExecuteTemplate(w, "rows", p); err != nil {
		return err
	}
	fmt.Fprint(w, `</tbody></table>`)
	return nil
}

func (s *server) snapshot(w http.ResponseWriter, req *http.Request, r *Review) error {
	key := req.FormValue("key")
	changes, err := r.Repo.Changes()
	if err != nil {
		return err
	}
	for _, c := range changes {
		if c.Working || (key != "" && c.Key != key) {
			continue
		}
		if _, _, err := r.Grab(c); err != nil {
			return err
		}
	}
	return s.back(w, req)
}

func (s *server) publish(w http.ResponseWriter, req *http.Request, r *Review) error {
	c, err := r.Change(req.FormValue("key"))
	if err != nil {
		return err
	}
	if _, err := r.DB.Publish(r.Root(), c.Key); err != nil {
		return err
	}
	return s.back(w, req)
}

func (s *server) prefs(w http.ResponseWriter, req *http.Request) error {
	for _, k := range []string{"context", "tabsize"} {
		if v := req.FormValue(k); v != "" {
			if _, err := strconv.Atoi(v); err != nil {
				return fmt.Errorf("%s: %v", k, err)
			}
			if err := s.db.SetPref(k, v); err != nil {
				return err
			}
		}
	}
	if err := s.db.SetPref("unified", boolParam(req.FormValue("unified") == "1")); err != nil {
		return err
	}
	return s.back(w, req)
}

// back sends the browser to where it came from, so that an action taken
// from a diff returns to that diff.
func (s *server) back(w http.ResponseWriter, req *http.Request) error {
	to := req.FormValue("return")
	if to == "" || !strings.HasPrefix(to, "/") {
		to = "/"
	}
	w.Header().Set("HX-Redirect", to)
	http.Redirect(w, req, to, http.StatusSeeOther)
	return nil
}

// threadFrag is the data a thread or comment form fragment needs.
type threadFrag struct {
	Thread  *Thread
	Comment *Comment // the draft being edited, for the edit form
	Link    string   // where to jump to see this thread in its diff
	Repo    string   // short name of the repository, for the fragment URLs
	Key     string
	Base    string
	Target  string
	File    string
	Side    string
	Line    int
	Unified bool
	User    string
}

// form returns an empty comment or reply form.
func (s *server) form(w http.ResponseWriter, req *http.Request, r *Review) error {
	if id := req.FormValue("thread"); id != "" {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return err
		}
		t, err := r.DB.Thread(n)
		if err != nil {
			return err
		}
		return tmpl.ExecuteTemplate(w, "replyform", &threadFrag{Thread: t, Repo: r.Name, User: s.user})
	}
	f := &threadFrag{
		Repo:    r.Name,
		Key:     req.FormValue("key"),
		Base:    req.FormValue("base"),
		Target:  req.FormValue("s"),
		File:    req.FormValue("f"),
		Side:    req.FormValue("side"),
		Unified: req.FormValue("unified") == "1",
		User:    s.user,
	}
	f.Line, _ = strconv.Atoi(req.FormValue("line"))
	if f.Side != "old" && f.Side != "new" {
		return fmt.Errorf("invalid side %q", f.Side)
	}
	return tmpl.ExecuteTemplate(w, "commentrow", f)
}

// comment saves a new comment, either replying to a thread or starting one.
func (s *server) comment(w http.ResponseWriter, req *http.Request, r *Review) error {
	body := strings.TrimSpace(req.FormValue("body"))
	if body == "" {
		return fmt.Errorf("empty comment")
	}
	c := &Comment{Author: s.user, Body: body, Draft: true}

	if id := req.FormValue("thread"); id != "" {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return err
		}
		if _, err := r.DB.AddComment(n, c); err != nil {
			return err
		}
		if req.FormValue("resolve") != "" {
			if err := r.DB.SetResolved(n, true); err != nil {
				return err
			}
		}
		return s.renderThread(w, r, n)
	}

	_, v, _, err := s.fragPage(req, r)
	if err != nil {
		return err
	}
	side := req.FormValue("side")
	snapshotID, storedSide, err := v.ThreadTarget(side)
	if err != nil {
		return err
	}
	file := req.FormValue("f")
	line, _ := strconv.Atoi(req.FormValue("line"))

	// Record the text of the line the comment is attached to, so that the
	// comment can be found again after the change is amended.
	anchor, err := s.anchorText(r, v, file, side, line)
	if err != nil {
		return err
	}
	t, err := r.DB.AddThread(snapshotID, file, storedSide, line, anchor, c)
	if err != nil {
		return err
	}
	return tmpl.ExecuteTemplate(w, "threadrow", &threadFrag{
		Thread:  t,
		Repo:    r.Name,
		Side:    side,
		Unified: req.FormValue("unified") == "1",
		User:    s.user,
	})
}

// anchorText returns the text of the line a new comment is attached to.
func (s *server) anchorText(r *Review, v *View, file, side string, line int) (string, error) {
	if line <= 0 {
		return "", nil
	}
	f := v.File(file)
	if f == nil {
		return "", fmt.Errorf("no file %q in this change", file)
	}
	old, new, err := r.Contents(v, f)
	if err != nil {
		return "", err
	}
	data := new
	if side == "old" {
		data = old
	}
	lines, _ := splitLines(data)
	if line > len(lines) {
		return "", nil
	}
	return lines[line-1], nil
}

// thread re-renders a thread, which is how a form is cancelled.
func (s *server) thread(w http.ResponseWriter, req *http.Request, r *Review) error {
	id, err := strconv.ParseInt(req.FormValue("thread"), 10, 64)
	if err != nil {
		return err
	}
	return s.renderThread(w, r, id)
}

// editForm returns a form holding an existing draft comment's text.
func (s *server) editForm(w http.ResponseWriter, req *http.Request, r *Review) error {
	id, err := strconv.ParseInt(req.FormValue("comment"), 10, 64)
	if err != nil {
		return err
	}
	c, err := r.DB.Comment(id)
	if err != nil {
		return err
	}
	if !c.Draft {
		return fmt.Errorf("comment %d is published and cannot be edited", id)
	}
	return tmpl.ExecuteTemplate(w, "editform", &threadFrag{Comment: c, Repo: r.Name, User: s.user})
}

func (s *server) edit(w http.ResponseWriter, req *http.Request, r *Review) error {
	id, err := strconv.ParseInt(req.FormValue("comment"), 10, 64)
	if err != nil {
		return err
	}
	c, err := r.DB.Comment(id)
	if err != nil {
		return err
	}
	body := strings.TrimSpace(req.FormValue("body"))
	if body == "" {
		return fmt.Errorf("empty comment")
	}
	if err := r.DB.EditComment(id, body); err != nil {
		return err
	}
	return s.renderThread(w, r, c.ThreadID)
}

// undoFrag holds everything needed to put a deleted draft back, so that
// deleting one needs no confirmation: the way out is to undo it, which is
// quicker to read than a dialog is to dismiss.
type undoFrag struct {
	Repo   string
	User   string
	Thread *Thread // what is left of the thread, nil if the draft was all of it
	Body   string
	Author string

	// Enough to rebuild the thread when the draft was its only comment.
	Snapshot int64
	File     string
	Side     string
	Line     int
	Anchor   string
	Resolved bool
}

func (s *server) deleteComment(w http.ResponseWriter, req *http.Request, r *Review) error {
	id, err := strconv.ParseInt(req.FormValue("comment"), 10, 64)
	if err != nil {
		return err
	}
	c, err := r.DB.Comment(id)
	if err != nil {
		return err
	}
	// Read the thread before deleting: if this draft is all of it, the
	// thread goes too and this is the only record of where it was.
	t, err := r.DB.Thread(c.ThreadID)
	if err != nil {
		return err
	}
	gone, err := r.DB.DeleteComment(id)
	if err != nil {
		return err
	}

	undo := &undoFrag{
		Repo: r.Name, User: s.user, Body: c.Body, Author: c.Author,
		Snapshot: t.SnapshotID, File: t.File, Side: t.Side,
		Line: t.Line, Anchor: t.AnchorText, Resolved: t.Resolved,
	}
	if !gone {
		if undo.Thread, err = r.DB.Thread(c.ThreadID); err != nil {
			return err
		}
	}
	return tmpl.ExecuteTemplate(w, "deleted", undo)
}

// undelete puts back a draft deleted a moment ago, rebuilding its thread
// if that went with it.
func (s *server) undelete(w http.ResponseWriter, req *http.Request, r *Review) error {
	body := strings.TrimSpace(req.FormValue("body"))
	if body == "" {
		return fmt.Errorf("nothing to undelete")
	}
	c := &Comment{Author: req.FormValue("author"), Body: body, Draft: true}
	if c.Author == "" {
		c.Author = s.user
	}

	if id := req.FormValue("thread"); id != "" {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return err
		}
		if _, err := r.DB.AddComment(n, c); err != nil {
			return err
		}
		return s.renderThread(w, r, n)
	}

	snapshot, err := strconv.ParseInt(req.FormValue("snapshot"), 10, 64)
	if err != nil {
		return err
	}
	line, _ := strconv.Atoi(req.FormValue("line"))
	t, err := r.DB.AddThread(snapshot, req.FormValue("f"), req.FormValue("side"), line, req.FormValue("anchor"), c)
	if err != nil {
		return err
	}
	if req.FormValue("resolved") == "1" {
		if err := r.DB.SetResolved(t.ID, true); err != nil {
			return err
		}
	}
	return s.renderThread(w, r, t.ID)
}

func (s *server) resolve(w http.ResponseWriter, req *http.Request, r *Review) error {
	id, err := strconv.ParseInt(req.FormValue("thread"), 10, 64)
	if err != nil {
		return err
	}
	t, err := r.DB.Thread(id)
	if err != nil {
		return err
	}
	if err := r.DB.SetResolved(id, !t.Resolved); err != nil {
		return err
	}
	return s.renderThread(w, r, id)
}

func (s *server) renderThread(w http.ResponseWriter, r *Review, id int64) error {
	t, err := r.DB.Thread(id)
	if err != nil {
		return err
	}
	f := &threadFrag{Thread: t, Repo: r.Name, User: s.user}
	if snap, err := r.DB.SnapshotByID(t.SnapshotID); err == nil {
		f = s.threadFrag(r, t, snap.Key)
	}
	return tmpl.ExecuteTemplate(w, "thread", f)
}

func (s *server) reviewed(w http.ResponseWriter, req *http.Request, r *Review) error {
	id, err := strconv.ParseInt(req.FormValue("snapshot"), 10, 64)
	if err != nil {
		return err
	}
	file := req.FormValue("f")
	on := req.FormValue("on") == "1"
	if err := r.DB.SetReviewed(id, file, on); err != nil {
		return err
	}
	return tmpl.ExecuteTemplate(w, "reviewedbutton", reviewedArg(r.Name, id, file, on))
}

func (s *server) snapshotReviewed(w http.ResponseWriter, req *http.Request, r *Review) error {
	id, err := strconv.ParseInt(req.FormValue("snapshot"), 10, 64)
	if err != nil {
		return err
	}
	if _, err := r.DB.SnapshotByID(id); err != nil {
		return err
	}
	on := req.FormValue("on") == "1"
	if err := r.DB.SetSnapshotReviewed(id, on); err != nil {
		return err
	}
	return tmpl.ExecuteTemplate(w, "reviewedbutton", snapshotReviewedArg(r.Name, id, on))
}

func (s *server) lgtm(w http.ResponseWriter, req *http.Request, r *Review) error {
	id, err := strconv.ParseInt(req.FormValue("snapshot"), 10, 64)
	if err != nil {
		return err
	}
	if _, err := r.DB.SnapshotByID(id); err != nil {
		return err
	}
	on := req.FormValue("on") == "1"
	if err := r.DB.SetSnapshotLGTM(id, on); err != nil {
		return err
	}
	return tmpl.ExecuteTemplate(w, "lgtmbutton", lgtmArg(r.Name, id, on))
}

// lineHTML renders one line of a diff, marking the byte ranges that changed
// within the line and any trailing whitespace.
func lineHTML(text string, spans []Span) template.HTML {
	if text == "" {
		return ""
	}
	// Trailing whitespace gets its own marker, as in Gerrit.
	trail := len(text)
	for trail > 0 && (text[trail-1] == ' ' || text[trail-1] == '\t') {
		trail--
	}
	if trail == len(text) {
		trail = -1
	}

	cuts := map[int]bool{0: true, len(text): true}
	for _, sp := range spans {
		cuts[sp.Lo] = true
		cuts[sp.Hi] = true
	}
	if trail >= 0 {
		cuts[trail] = true
	}
	at := make([]int, 0, len(cuts))
	for c := range cuts {
		if 0 <= c && c <= len(text) {
			at = append(at, c)
		}
	}
	sort.Ints(at)

	var b strings.Builder
	for i := 0; i+1 < len(at); i++ {
		lo, hi := at[i], at[i+1]
		if lo == hi {
			continue
		}
		var class string
		if inSpans(spans, lo) {
			class = "i"
		}
		if trail >= 0 && lo >= trail {
			if class != "" {
				class += " "
			}
			class += "ws"
		}
		seg := template.HTMLEscapeString(text[lo:hi])
		if class == "" {
			b.WriteString(seg)
			continue
		}
		fmt.Fprintf(&b, `<span class="%s">%s</span>`, class, seg)
	}
	return template.HTML(b.String())
}

func inSpans(spans []Span, i int) bool {
	for _, sp := range spans {
		if sp.Lo <= i && i < sp.Hi {
			return true
		}
	}
	return false
}

// serve runs the web interface until it fails. ready, if not nil, is
// called with the address actually listened on, which differs from the
// one asked for when that was busy.
func serve(s *server, addr string, ready func(string)) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// Fall back to any free port rather than refusing to start.
		ln, err = net.Listen("tcp", "localhost:0")
		if err != nil {
			return err
		}
	}
	if ready != nil {
		ready(ln.Addr().String())
	}
	return http.Serve(ln, protect(s))
}

// protect wraps the server in Go's cross-origin protection. The server
// listens on localhost and changes state over POST, so without this any
// page in the browser could drive it; requests carrying no Sec-Fetch-Site
// or Origin header, such as those from the command line, still pass.
func protect(h http.Handler) http.Handler {
	return http.NewCrossOriginProtection().Handler(h)
}

func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("opening browser: %v", err)
	}
}
