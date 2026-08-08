// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCrossOriginProtection checks that a page on another site cannot
// drive the server, which matters because it listens on localhost where
// any page in the browser can reach it.
func TestCrossOriginProtection(t *testing.T) {
	s, r, _ := newTestServer(t)
	h := protect(s)
	snapshot := repoURL(t, r, "/snapshot")

	// A cross-site POST is refused.
	req := httptest.NewRequest("POST", snapshot, strings.NewReader(url.Values{"key": {"Itest1"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-site POST = %d, want 403", w.Code)
	}

	// The same request from our own pages is allowed.
	req = httptest.NewRequest("POST", snapshot, strings.NewReader(url.Values{"key": {"Itest1"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusForbidden {
		t.Errorf("same-origin POST = 403, want it allowed")
	}

	// So are requests with no such headers, which is how the command line
	// and other non-browser callers reach it.
	req = httptest.NewRequest("POST", snapshot, strings.NewReader(url.Values{"key": {"Itest1"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusForbidden {
		t.Errorf("header-less POST = 403, want it allowed")
	}

	// Reading is always safe, whatever the origin.
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("cross-site GET = %d, want it allowed", w.Code)
	}
}

func TestHealthEndpoint(t *testing.T) {
	s, _, _ := newTestServer(t)
	w := get(t, s, healthPath)
	if w.Code != http.StatusOK || w.Body.String() != healthBody {
		t.Errorf("GET %s = %d %q, want 200 %q", healthPath, w.Code, w.Body.String(), healthBody)
	}
}

// TestServerLifecycle starts a real background server, finds it, restarts
// it, and stops it.
func TestServerLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a background process")
	}
	exe := filepath.Join(t.TempDir(), "review")
	if out, err := run(".", "go", "build", "-o", exe, "."); err != nil {
		t.Fatalf("building review: %v\n%s", err, out)
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "review.db")
	repo := newGitRepo(t)
	t.Cleanup(func() { run(repo, exe, "stop", "-db", db) })

	// Nothing running yet.
	if st := findServer(db); st != nil {
		t.Fatalf("found a server before starting one: %+v", st)
	}
	if out, err := run(repo, exe, "stop", "-db", db); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(out), "not running") {
		t.Errorf("stop with nothing running said %q", out)
	}

	// Plain review starts one in the background and returns.
	out, err := run(repo, exe, "-db", db, "-a", "localhost:0", "-n")
	if err != nil {
		t.Fatalf("review: %v\n%s", err, out)
	}
	st := findServer(db)
	if st == nil {
		t.Fatalf("no server after starting one; review said:\n%s", out)
	}
	if !alive(st.Addr) {
		t.Fatal("server is not answering")
	}
	// It printed where to look, and the state names the process.
	if !strings.Contains(string(out), st.Addr) {
		t.Errorf("review did not report the address:\n%s", out)
	}
	if st.PID <= 0 {
		t.Errorf("state has no process: %+v", st)
	}

	// Running review again attaches to the same server rather than
	// starting a second one.
	out, err = run(repo, exe, "-db", db, "-n")
	if err != nil {
		t.Fatalf("second review: %v\n%s", err, out)
	}
	st2 := findServer(db)
	if st2 == nil || st2.PID != st.PID {
		t.Errorf("second run started a new server: %+v then %+v", st, st2)
	}
	// It printed the URL of this repository, not just the root.
	if !strings.Contains(string(out), "http://"+st.Addr+"/") {
		t.Errorf("second run did not print a URL:\n%s", out)
	}

	// Restart keeps the address but replaces the process.
	if out, err = run(repo, exe, "restart", "-db", db); err != nil {
		t.Fatalf("restart: %v\n%s", err, out)
	}
	st3 := findServer(db)
	if st3 == nil {
		t.Fatal("no server after restart")
	}
	if st3.PID == st.PID {
		t.Error("restart did not replace the process")
	}
	if st3.Addr != st.Addr {
		t.Errorf("restart moved the server from %s to %s", st.Addr, st3.Addr)
	}

	// Stop leaves nothing behind.
	if out, err = run(repo, exe, "stop", "-db", db); err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "stopped") {
		t.Errorf("stop said %q", out)
	}
	if alive(st3.Addr) {
		t.Error("server still answering after stop")
	}
	if _, err := os.Stat(statePath(db)); !os.IsNotExist(err) {
		t.Error("stop left the state file behind")
	}
}

// TestStaleStateFile checks that a state file naming a dead server does
// not make review think one is running.
func TestStaleStateFile(t *testing.T) {
	db := filepath.Join(t.TempDir(), "review.db")
	if err := writeState(db, &serverState{Addr: "localhost:1", PID: 999999}); err != nil {
		t.Fatal(err)
	}
	if st := findServer(db); st != nil {
		t.Errorf("found a server from a stale state file: %+v", st)
	}
	if _, err := os.Stat(statePath(db)); !os.IsNotExist(err) {
		t.Error("stale state file was not cleaned up")
	}
}
