// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// schemaVersion is the current database schema version, recorded in
// SQLite's user_version so that future changes can be migrated.
const schemaVersion = 4

// migrations[v] upgrades a database from version v to version v+1.
// A fresh database is created from schema below and needs none of them.
var migrations = map[int]string{
	1: `CREATE TABLE reviewed_snapshot (
		snapshot_id INTEGER PRIMARY KEY REFERENCES snapshot(id),
		created     INTEGER NOT NULL
	);`,
	// Reviewed was the first of several marks a snapshot can carry, so
	// the single-purpose table becomes a general one.
	2: `CREATE TABLE snapshot_mark (
		snapshot_id INTEGER NOT NULL REFERENCES snapshot(id),
		kind        TEXT NOT NULL,
		created     INTEGER NOT NULL,
		PRIMARY KEY(snapshot_id, kind)
	);
	INSERT INTO snapshot_mark (snapshot_id, kind, created)
		SELECT snapshot_id, 'reviewed', created FROM reviewed_snapshot;
	DROP TABLE reviewed_snapshot;`,
	// Repositories gained short names, used in URLs, which have to stay
	// put once handed out.
	3: `CREATE TABLE repo (
		path TEXT PRIMARY KEY,
		name TEXT NOT NULL UNIQUE
	);`,
}

// The marks a snapshot can carry. Both are lost when a new snapshot
// arrives, in the sense that they belong to the snapshot they were put
// on and say nothing about any later one.
const (
	markReviewed = "reviewed"
	markLGTM     = "lgtm"
)

const schema = `
CREATE TABLE change (
	id         INTEGER PRIMARY KEY,
	repo       TEXT NOT NULL,
	change_key TEXT NOT NULL,
	UNIQUE(repo, change_key)
);
CREATE TABLE snapshot (
	id        INTEGER PRIMARY KEY,
	change_id INTEGER NOT NULL REFERENCES change(id),
	n         INTEGER NOT NULL,
	rev       TEXT NOT NULL,
	parent    TEXT NOT NULL,
	subject   TEXT NOT NULL,
	message   TEXT NOT NULL,
	author    TEXT NOT NULL,
	date      INTEGER NOT NULL,
	created   INTEGER NOT NULL,
	UNIQUE(change_id, n)
);
CREATE TABLE thread (
	id          INTEGER PRIMARY KEY,
	snapshot_id INTEGER NOT NULL REFERENCES snapshot(id),
	file        TEXT NOT NULL,
	side        TEXT NOT NULL,
	line        INTEGER NOT NULL,
	anchor_text TEXT NOT NULL,
	resolved    INTEGER NOT NULL DEFAULT 0,
	created     INTEGER NOT NULL
);
CREATE TABLE comment (
	id         INTEGER PRIMARY KEY,
	thread_id  INTEGER NOT NULL REFERENCES thread(id),
	author     TEXT NOT NULL,
	from_agent INTEGER NOT NULL DEFAULT 0,
	body       TEXT NOT NULL,
	draft      INTEGER NOT NULL DEFAULT 1,
	created    INTEGER NOT NULL
);
CREATE TABLE reviewed (
	snapshot_id INTEGER NOT NULL REFERENCES snapshot(id),
	file        TEXT NOT NULL,
	PRIMARY KEY(snapshot_id, file)
);
CREATE TABLE snapshot_mark (
	snapshot_id INTEGER NOT NULL REFERENCES snapshot(id),
	kind        TEXT NOT NULL,
	created     INTEGER NOT NULL,
	PRIMARY KEY(snapshot_id, kind)
);
CREATE TABLE repo (
	path TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE
);
CREATE TABLE pref (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE INDEX thread_loc ON thread(snapshot_id, file);
`

// A Snapshot is a recorded state of a change: the commit ID the change
// pointed at when the snapshot was grabbed. It is Gerrit's patch set,
// kept locally.
type Snapshot struct {
	ID       int64
	ChangeID int64
	Key      string // the change key this belongs to
	N        int    // 1..N within the change
	Rev      string
	Parent   string
	Subject  string
	Message  string
	Author   string
	Date     time.Time
	Created  time.Time
}

// ShortRev returns an abbreviated commit ID for display.
func (s *Snapshot) ShortRev() string { return shortRev(s.Rev) }

// A Thread is a comment thread anchored to one line of one file in one
// snapshot. Side is "new" for the snapshot's own content and "old" for
// the content of the parent it was diffed against.
type Thread struct {
	ID         int64
	SnapshotID int64
	File       string
	Side       string
	Line       int
	AnchorText string
	Resolved   bool
	Created    time.Time
	Comments   []*Comment

	// Fields filled in for display, not stored.
	SnapshotN int  // which snapshot this thread belongs to
	ShowLine  int  // where to draw it in the diff being viewed
	Stale     bool // its anchor text could not be found
	Other     bool // it belongs to a snapshot other than the one displayed
}

// Unresolved reports whether the thread still needs attention.
func (t *Thread) Unresolved() bool { return !t.Resolved }

// Where names the place a thread is attached to, for display away from
// the diff itself.
func (t *Thread) Where() string {
	name := t.File
	if name == CommitMsgFile {
		name = "Commit Message"
	}
	if t.Line > 0 {
		return fmt.Sprintf("%s:%d", name, t.Line)
	}
	return name
}

// A Comment is one message in a thread.
type Comment struct {
	ID        int64
	ThreadID  int64
	Author    string
	FromAgent bool
	Body      string
	Draft     bool
	Created   time.Time
}

// A DB is the review database.
type DB struct {
	sql *sql.DB
}

// DefaultDBPath returns the path of the review database. It follows
// XDG_CONFIG_HOME, defaulting to $HOME/.config, which is where the
// other tools on this machine keep their configuration.
func DefaultDBPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "review", "review.db")
}

// OpenDB opens the review database, creating it if necessary.
func OpenDB(path string) (*DB, error) {
	// The path becomes a file: URI, which must be absolute: a relative
	// path would turn its leading element into a URI authority.
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	u.RawQuery = url.Values{
		"_pragma": {"busy_timeout(10000)", "journal_mode(WAL)", "foreign_keys(1)"},
	}.Encode()

	sdb, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	// One connection at a time. This is a single-user local tool, and
	// serializing writes avoids SQLITE_BUSY entirely.
	sdb.SetMaxOpenConns(1)

	d := &DB{sql: sdb}
	if err := d.init(); err != nil {
		sdb.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) init() error {
	var v int
	if err := d.sql.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	switch {
	case v == schemaVersion:
		return nil
	case v > schemaVersion:
		return fmt.Errorf("database schema version %d is newer than this program understands (%d)", v, schemaVersion)
	case v == 0:
		if _, err := d.sql.Exec(schema); err != nil {
			return err
		}
	default:
		// Upgrade an existing database one version at a time, so that a
		// database written by an older review keeps its comments.
		for i := v; i < schemaVersion; i++ {
			m, ok := migrations[i]
			if !ok {
				return fmt.Errorf("no migration from database schema version %d", i)
			}
			if _, err := d.sql.Exec(m); err != nil {
				return fmt.Errorf("migrating database from version %d: %v", i, err)
			}
		}
	}
	_, err := d.sql.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion))
	return err
}

// Close closes the database.
func (d *DB) Close() error { return d.sql.Close() }

// changeID returns the row ID for the given change, creating it if needed.
func (d *DB) changeID(repo, key string) (int64, error) {
	var id int64
	err := d.sql.QueryRow("SELECT id FROM change WHERE repo = ? AND change_key = ?", repo, key).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	res, err := d.sql.Exec("INSERT INTO change (repo, change_key) VALUES (?, ?)", repo, key)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const snapshotCols = `s.id, s.change_id, c.change_key, s.n, s.rev, s.parent, s.subject, s.message, s.author, s.date, s.created`

func scanSnapshot(rows interface{ Scan(...any) error }) (*Snapshot, error) {
	var s Snapshot
	var date, created int64
	err := rows.Scan(&s.ID, &s.ChangeID, &s.Key, &s.N, &s.Rev, &s.Parent, &s.Subject, &s.Message, &s.Author, &date, &created)
	if err != nil {
		return nil, err
	}
	s.Date = time.Unix(date, 0)
	s.Created = time.Unix(created, 0)
	return &s, nil
}

// Snapshots returns all snapshots of a change, oldest first.
func (d *DB) Snapshots(repo, key string) ([]*Snapshot, error) {
	rows, err := d.sql.Query(`SELECT `+snapshotCols+`
		FROM snapshot s JOIN change c ON c.id = s.change_id
		WHERE c.repo = ? AND c.change_key = ? ORDER BY s.n`, repo, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Snapshot
	for rows.Next() {
		s, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Snapshot returns snapshot number n of a change.
func (d *DB) Snapshot(repo, key string, n int) (*Snapshot, error) {
	row := d.sql.QueryRow(`SELECT `+snapshotCols+`
		FROM snapshot s JOIN change c ON c.id = s.change_id
		WHERE c.repo = ? AND c.change_key = ? AND s.n = ?`, repo, key, n)
	return scanSnapshot(row)
}

// SnapshotByID returns the snapshot with the given row ID.
func (d *DB) SnapshotByID(id int64) (*Snapshot, error) {
	row := d.sql.QueryRow(`SELECT `+snapshotCols+`
		FROM snapshot s JOIN change c ON c.id = s.change_id WHERE s.id = ?`, id)
	return scanSnapshot(row)
}

// AddSnapshot records the current state of c as a new snapshot. If the
// newest existing snapshot already names the same commit, it returns that
// snapshot with created=false instead of recording a duplicate.
func (d *DB) AddSnapshot(repo string, c *Change) (s *Snapshot, created bool, err error) {
	if c.Working {
		return nil, false, fmt.Errorf("cannot snapshot uncommitted changes: commit them first")
	}
	existing, err := d.Snapshots(repo, c.Key)
	if err != nil {
		return nil, false, err
	}
	if n := len(existing); n > 0 && existing[n-1].Rev == c.Rev {
		return existing[n-1], false, nil
	}

	id, err := d.changeID(repo, c.Key)
	if err != nil {
		return nil, false, err
	}
	n := len(existing) + 1
	now := time.Now()
	res, err := d.sql.Exec(`INSERT INTO snapshot
		(change_id, n, rev, parent, subject, message, author, date, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, n, c.Rev, c.Parent, c.Subject, c.Message, c.Author, c.Date.Unix(), now.Unix())
	if err != nil {
		return nil, false, err
	}
	rowID, err := res.LastInsertId()
	if err != nil {
		return nil, false, err
	}
	return &Snapshot{
		ID: rowID, ChangeID: id, Key: c.Key, N: n,
		Rev: c.Rev, Parent: c.Parent, Subject: c.Subject, Message: c.Message,
		Author: c.Author, Date: c.Date, Created: now,
	}, true, nil
}

const threadCols = `t.id, t.snapshot_id, t.file, t.side, t.line, t.anchor_text, t.resolved, t.created, s.n`

func scanThread(rows interface{ Scan(...any) error }) (*Thread, error) {
	var t Thread
	var created int64
	var resolved int
	err := rows.Scan(&t.ID, &t.SnapshotID, &t.File, &t.Side, &t.Line, &t.AnchorText, &resolved, &created, &t.SnapshotN)
	if err != nil {
		return nil, err
	}
	t.Resolved = resolved != 0
	t.Created = time.Unix(created, 0)
	return &t, nil
}

// Threads returns every thread on a change, across all its snapshots,
// oldest first, with their comments attached.
func (d *DB) Threads(repo, key string) ([]*Thread, error) {
	rows, err := d.sql.Query(`SELECT `+threadCols+`
		FROM thread t
		JOIN snapshot s ON s.id = t.snapshot_id
		JOIN change c ON c.id = s.change_id
		WHERE c.repo = ? AND c.change_key = ?
		ORDER BY t.file, t.line, t.id`, repo, key)
	if err != nil {
		return nil, err
	}
	return d.collectThreads(rows)
}

// AllThreads returns every thread in a repository.
func (d *DB) AllThreads(repo string) ([]*Thread, error) {
	rows, err := d.sql.Query(`SELECT `+threadCols+`
		FROM thread t
		JOIN snapshot s ON s.id = t.snapshot_id
		JOIN change c ON c.id = s.change_id
		WHERE c.repo = ?
		ORDER BY c.change_key, s.n, t.file, t.line, t.id`, repo)
	if err != nil {
		return nil, err
	}
	return d.collectThreads(rows)
}

// Thread returns a single thread by ID, with its comments.
func (d *DB) Thread(id int64) (*Thread, error) {
	rows, err := d.sql.Query(`SELECT `+threadCols+`
		FROM thread t JOIN snapshot s ON s.id = t.snapshot_id WHERE t.id = ?`, id)
	if err != nil {
		return nil, err
	}
	ts, err := d.collectThreads(rows)
	if err != nil {
		return nil, err
	}
	if len(ts) == 0 {
		return nil, fmt.Errorf("no comment thread %d", id)
	}
	return ts[0], nil
}

func (d *DB) collectThreads(rows *sql.Rows) ([]*Thread, error) {
	defer rows.Close()
	var out []*Thread
	byID := map[int64]*Thread{}
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
		byID[t.ID] = t
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	if err := d.loadComments(byID); err != nil {
		return nil, err
	}
	return out, nil
}

func (d *DB) loadComments(byID map[int64]*Thread) error {
	ids := make([]string, 0, len(byID))
	args := make([]any, 0, len(byID))
	for id := range byID {
		ids = append(ids, "?")
		args = append(args, id)
	}
	q := `SELECT id, thread_id, author, from_agent, body, draft, created FROM comment
		WHERE thread_id IN (` + strings.Join(ids, ",") + `) ORDER BY created, id`
	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c Comment
		var fromAgent, draft int
		var created int64
		if err := rows.Scan(&c.ID, &c.ThreadID, &c.Author, &fromAgent, &c.Body, &draft, &created); err != nil {
			return err
		}
		c.FromAgent = fromAgent != 0
		c.Draft = draft != 0
		c.Created = time.Unix(created, 0)
		if t := byID[c.ThreadID]; t != nil {
			t.Comments = append(t.Comments, &c)
		}
	}
	return rows.Err()
}

// AddThread starts a new comment thread with one comment in it.
func (d *DB) AddThread(snapshotID int64, file, side string, line int, anchor string, c *Comment) (*Thread, error) {
	if side != "new" && side != "old" {
		return nil, fmt.Errorf("invalid side %q", side)
	}
	now := time.Now()
	res, err := d.sql.Exec(`INSERT INTO thread
		(snapshot_id, file, side, line, anchor_text, resolved, created)
		VALUES (?, ?, ?, ?, ?, 0, ?)`,
		snapshotID, file, side, line, anchor, now.Unix())
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if _, err := d.AddComment(id, c); err != nil {
		return nil, err
	}
	return d.Thread(id)
}

// AddComment appends a comment to a thread.
func (d *DB) AddComment(threadID int64, c *Comment) (*Comment, error) {
	if strings.TrimSpace(c.Body) == "" {
		return nil, errors.New("empty comment")
	}
	now := time.Now()
	res, err := d.sql.Exec(`INSERT INTO comment
		(thread_id, author, from_agent, body, draft, created) VALUES (?, ?, ?, ?, ?, ?)`,
		threadID, c.Author, boolInt(c.FromAgent), c.Body, boolInt(c.Draft), now.Unix())
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	out := *c
	out.ID = id
	out.ThreadID = threadID
	out.Created = now
	return &out, nil
}

// Comment returns a single comment by ID.
func (d *DB) Comment(id int64) (*Comment, error) {
	var c Comment
	var fromAgent, draft int
	var created int64
	err := d.sql.QueryRow(`SELECT id, thread_id, author, from_agent, body, draft, created
		FROM comment WHERE id = ?`, id).
		Scan(&c.ID, &c.ThreadID, &c.Author, &fromAgent, &c.Body, &draft, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("no comment %d", id)
	}
	if err != nil {
		return nil, err
	}
	c.FromAgent = fromAgent != 0
	c.Draft = draft != 0
	c.Created = time.Unix(created, 0)
	return &c, nil
}

// EditComment replaces the text of a draft comment. Published comments
// cannot be edited: once a reply may have been written against a comment,
// changing it out from under the reply would rewrite history.
func (d *DB) EditComment(id int64, body string) error {
	if strings.TrimSpace(body) == "" {
		return errors.New("empty comment")
	}
	res, err := d.sql.Exec("UPDATE comment SET body = ? WHERE id = ? AND draft = 1", body, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("comment %d is not an editable draft", id)
	}
	return nil
}

// DeleteComment removes a draft comment, and the thread with it if that
// was the only comment in it. It reports whether the thread is now gone.
func (d *DB) DeleteComment(id int64) (threadGone bool, err error) {
	c, err := d.Comment(id)
	if err != nil {
		return false, err
	}
	if !c.Draft {
		return false, fmt.Errorf("comment %d is published and cannot be deleted", id)
	}
	if _, err := d.sql.Exec("DELETE FROM comment WHERE id = ? AND draft = 1", id); err != nil {
		return false, err
	}
	var left int
	if err := d.sql.QueryRow("SELECT count(*) FROM comment WHERE thread_id = ?", c.ThreadID).Scan(&left); err != nil {
		return false, err
	}
	if left > 0 {
		return false, nil
	}
	// An empty thread is not a thread; drop it rather than leave a marker
	// on a line with nothing to say.
	_, err = d.sql.Exec("DELETE FROM thread WHERE id = ?", c.ThreadID)
	return true, err
}

// SetResolved sets a thread's resolved status.
func (d *DB) SetResolved(threadID int64, resolved bool) error {
	res, err := d.sql.Exec("UPDATE thread SET resolved = ? WHERE id = ?", boolInt(resolved), threadID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no comment thread %d", threadID)
	}
	return nil
}

// Publish marks every draft comment on a change as published, and reports
// how many it published.
func (d *DB) Publish(repo, key string) (int, error) {
	res, err := d.sql.Exec(`UPDATE comment SET draft = 0 WHERE draft = 1 AND thread_id IN (
			SELECT t.id FROM thread t
			JOIN snapshot s ON s.id = t.snapshot_id
			JOIN change c ON c.id = s.change_id
			WHERE c.repo = ? AND c.change_key = ?)`, repo, key)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// SetReviewed records whether a file has been marked reviewed in a snapshot.
func (d *DB) SetReviewed(snapshotID int64, file string, reviewed bool) error {
	var err error
	if reviewed {
		_, err = d.sql.Exec("INSERT OR IGNORE INTO reviewed (snapshot_id, file) VALUES (?, ?)", snapshotID, file)
	} else {
		_, err = d.sql.Exec("DELETE FROM reviewed WHERE snapshot_id = ? AND file = ?", snapshotID, file)
	}
	return err
}

// Reviewed returns the set of files marked reviewed in a snapshot.
func (d *DB) Reviewed(snapshotID int64) (map[string]bool, error) {
	rows, err := d.sql.Query("SELECT file FROM reviewed WHERE snapshot_id = ?", snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out[f] = true
	}
	return out, rows.Err()
}

// SetSnapshotMark records or clears one mark on a snapshot.
func (d *DB) SetSnapshotMark(snapshotID int64, kind string, on bool) error {
	var err error
	if on {
		_, err = d.sql.Exec(`INSERT INTO snapshot_mark (snapshot_id, kind, created)
			VALUES (?, ?, ?) ON CONFLICT(snapshot_id, kind) DO NOTHING`,
			snapshotID, kind, time.Now().Unix())
	} else {
		_, err = d.sql.Exec("DELETE FROM snapshot_mark WHERE snapshot_id = ? AND kind = ?", snapshotID, kind)
	}
	return err
}

// SnapshotMark reports whether a snapshot carries a mark.
func (d *DB) SnapshotMark(snapshotID int64, kind string) (bool, error) {
	var n int
	err := d.sql.QueryRow("SELECT count(*) FROM snapshot_mark WHERE snapshot_id = ? AND kind = ?",
		snapshotID, kind).Scan(&n)
	return n > 0, err
}

// SnapshotMarks returns the change's snapshots carrying a given mark.
func (d *DB) SnapshotMarks(repo, key, kind string) (map[int64]bool, error) {
	rows, err := d.sql.Query(`SELECT m.snapshot_id
		FROM snapshot_mark m
		JOIN snapshot s ON s.id = m.snapshot_id
		JOIN change c ON c.id = s.change_id
		WHERE c.repo = ? AND c.change_key = ? AND m.kind = ?`, repo, key, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// SetSnapshotReviewed records whether a whole snapshot has been reviewed.
func (d *DB) SetSnapshotReviewed(snapshotID int64, reviewed bool) error {
	return d.SetSnapshotMark(snapshotID, markReviewed, reviewed)
}

// SnapshotReviewed reports whether a snapshot has been marked reviewed.
func (d *DB) SnapshotReviewed(snapshotID int64) (bool, error) {
	return d.SnapshotMark(snapshotID, markReviewed)
}

// ReviewedSnapshots returns the change's snapshots marked reviewed.
func (d *DB) ReviewedSnapshots(repo, key string) (map[int64]bool, error) {
	return d.SnapshotMarks(repo, key, markReviewed)
}

// SetSnapshotLGTM records whether a snapshot looks good.
func (d *DB) SetSnapshotLGTM(snapshotID int64, lgtm bool) error {
	return d.SetSnapshotMark(snapshotID, markLGTM, lgtm)
}

// SnapshotLGTM reports whether a snapshot has been marked LGTM.
func (d *DB) SnapshotLGTM(snapshotID int64) (bool, error) {
	return d.SnapshotMark(snapshotID, markLGTM)
}

// LastReviewedSnapshot returns the newest reviewed snapshot of a change
// older than snapshot number n, or nil if there is none. It is what makes
// opening a file show only what has changed since it was last reviewed.
func (d *DB) LastReviewedSnapshot(repo, key string, n int) (*Snapshot, error) {
	row := d.sql.QueryRow(`SELECT `+snapshotCols+`
		FROM snapshot s
		JOIN change c ON c.id = s.change_id
		JOIN snapshot_mark m ON m.snapshot_id = s.id
		WHERE c.repo = ? AND c.change_key = ? AND m.kind = ? AND s.n < ?
		ORDER BY s.n DESC LIMIT 1`, repo, key, markReviewed, n)
	s, err := scanSnapshot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

// reservedRepoNames are the URL path elements review uses itself, which
// a repository must not be named after.
var reservedRepoNames = map[string]bool{
	"prefs": true, "healthz": true,
	"style.css": true, "keys.js": true, "htmx.min.js": true,
}

// RepoName returns the short name a repository is known by in URLs,
// assigning one the first time it is seen. Repositories that share a base
// name get .1, .2 and so on, and a name once assigned never changes, so
// that links keep working as other repositories come and go.
func (d *DB) RepoName(path string) (string, error) {
	var name string
	err := d.sql.QueryRow("SELECT name FROM repo WHERE path = ?", path).Scan(&name)
	if err == nil {
		return name, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	base := filepath.Base(path)
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "repo"
	}
	for i := 0; ; i++ {
		try := base
		if i > 0 {
			try = fmt.Sprintf("%s.%d", base, i)
		}
		if reservedRepoNames[try] {
			continue
		}
		var n int
		if err := d.sql.QueryRow("SELECT count(*) FROM repo WHERE name = ?", try).Scan(&n); err != nil {
			return "", err
		}
		if n > 0 {
			continue
		}
		if _, err := d.sql.Exec("INSERT INTO repo (path, name) VALUES (?, ?)", path, try); err != nil {
			return "", err
		}
		return try, nil
	}
}

// RepoPath returns the repository a short name refers to.
func (d *DB) RepoPath(name string) (string, error) {
	var path string
	err := d.sql.QueryRow("SELECT path FROM repo WHERE name = ?", name).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("no repository named %q", name)
	}
	return path, err
}

// Repos returns every repository that has been reviewed, by path.
func (d *DB) Repos() ([]string, error) {
	rows, err := d.sql.Query("SELECT DISTINCT repo FROM change ORDER BY repo")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Pref returns a stored preference, or def if it is not set.
func (d *DB) Pref(key, def string) string {
	var v string
	if err := d.sql.QueryRow("SELECT value FROM pref WHERE key = ?", key).Scan(&v); err != nil {
		return def
	}
	return v
}

// SetPref stores a preference.
func (d *DB) SetPref(key, value string) error {
	_, err := d.sql.Exec(`INSERT INTO pref (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// anchorSearch is how far from its recorded line review will look for a
// comment's anchor text before giving up and calling the thread stale.
const anchorSearch = 25

// Anchor finds the line where a thread should be drawn in a file whose
// contents have moved on since the comment was written. It looks for the
// exact text the comment was attached to, nearest to its original line.
// It reports stale if that text can no longer be found.
func Anchor(t *Thread, lines []string) (line int, stale bool) {
	if t.Line <= 0 {
		return 0, false // file-level comment, always in place
	}
	if t.AnchorText == "" {
		// Nothing to search for: keep the line if it still exists.
		if t.Line <= len(lines) {
			return t.Line, false
		}
		return 0, true
	}
	if t.Line <= len(lines) && lines[t.Line-1] == t.AnchorText {
		return t.Line, false
	}
	for d := 1; d <= anchorSearch; d++ {
		for _, n := range [2]int{t.Line - d, t.Line + d} {
			if n >= 1 && n <= len(lines) && lines[n-1] == t.AnchorText {
				return n, false
			}
		}
	}
	return 0, true
}
