// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// The web interface runs as one background server per database, shared by
// every repository. Running "review" in a second repository does not start
// a second server: it snapshots that repository, tells the running server
// nothing at all — the server reads the same database — and opens a tab.

// A serverState records where the background server is listening and
// which process it is, so that later invocations can find and stop it.
type serverState struct {
	Addr string `json:"addr"`
	PID  int    `json:"pid"`
}

// healthPath is the URL used to tell a live review server from a stale
// state file or from something else listening on the same port.
const healthPath = "/healthz"

// healthBody is what that URL answers with.
const healthBody = "review\n"

func statePath(db string) string { return filepath.Join(filepath.Dir(db), "server.json") }
func serverLog(db string) string { return filepath.Join(filepath.Dir(db), "server.log") }

func readState(db string) (*serverState, error) {
	data, err := os.ReadFile(statePath(db))
	if err != nil {
		return nil, err
	}
	var st serverState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func writeState(db string, st *serverState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(db), 0700); err != nil {
		return err
	}
	return os.WriteFile(statePath(db), append(data, '\n'), 0600)
}

// alive reports whether a review server is answering at addr.
func alive(addr string) bool {
	c := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Get("http://" + addr + healthPath)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	buf := make([]byte, len(healthBody))
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode == http.StatusOK && string(buf[:n]) == healthBody
}

// findServer returns the running server for a database, or nil. A state
// file left behind by a server that has since died is removed, so that
// the next run starts a fresh one rather than reporting a phantom.
func findServer(db string) *serverState {
	st, err := readState(db)
	if err != nil {
		return nil
	}
	if !alive(st.Addr) {
		os.Remove(statePath(db))
		return nil
	}
	return st
}

// startServer runs review in the background and waits for it to listen.
func startServer(db, addr string) (*serverState, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(serverLog(db), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "serve", "-a", addr, "-db", db)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// A new session, so that the server outlives the shell that started it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// The server writes its state once it is listening, which is also how
	// its real address is learned when the wanted one was busy.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st := findServer(db); st != nil {
			return st, nil
		}
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			return nil, fmt.Errorf("server exited at once; see %s", serverLog(db))
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("server did not start; see %s", serverLog(db))
}

// stopServer stops the background server and reports whether one was running.
func stopServer(db string) bool {
	st := findServer(db)
	if st == nil {
		os.Remove(statePath(db))
		return false
	}
	p, err := os.FindProcess(st.PID)
	if err != nil {
		os.Remove(statePath(db))
		return false
	}
	// Ask first, so that the server can tidy up after itself.
	p.Signal(os.Interrupt)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(st.Addr) {
			os.Remove(statePath(db))
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	p.Kill()
	os.Remove(statePath(db))
	return true
}

// cmdServe runs the web server in the foreground. Review runs this for
// itself, in the background; it is not in the usage message.
func cmdServe(args []string) {
	f := newFlags("serve", "[-a addr]")
	serveFlags(f)
	f.Parse(args)
	if f.NArg() != 0 {
		f.Usage()
	}

	db := dbPath()
	d, err := OpenDB(db)
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()

	// A repository under the current directory is listed even if it has
	// never been reviewed, but review serves the database, so it runs
	// anywhere.
	home := ""
	if dir, err := os.Getwd(); err == nil {
		if repo, err := OpenRepo(dir); err == nil {
			home = repo.Root()
		}
	}
	// A new binary is usually a new set of instructions, and the server
	// outlives the shell that installed the old ones, so bring any copy
	// already installed up to date before serving.
	for _, path := range updateSkills() {
		fmt.Fprintf(stderr, "review: updated skill at %s\n", path)
	}

	// Leaving a state file behind would make the next run think a server
	// is up when it is not.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		os.Remove(statePath(db))
		os.Exit(0)
	}()
	defer os.Remove(statePath(db))

	err = serve(newServer(d, home, true), addr, func(actual string) {
		if err := writeState(db, &serverState{Addr: actual, PID: os.Getpid()}); err != nil {
			log.Printf("recording server state: %v", err)
		}
		fmt.Fprintf(stderr, "review: serving at http://%s\n", actual)
	})
	if err != nil {
		log.Fatal(err)
	}
}

// cmdOpen is what plain "review" does: make sure the background server is
// running, record where this repository stands, and open a browser at it.
func cmdOpen(args []string) {
	f := newFlags("", "[-a addr] [-n]")
	serveFlags(f)
	f.Usage = usage
	f.Parse(args)
	if f.NArg() != 0 {
		usage()
	}

	db := dbPath()
	st := findServer(db)
	started := false
	if st == nil {
		var err error
		if st, err = startServer(db, addr); err != nil {
			log.Fatal(err)
		}
		started = true
	}

	// A snapshot on the way in gives the reviewer a fixed point, so that
	// whatever the agent does next can be compared against what is being
	// looked at now.
	// Outside a repository there is nothing to snapshot, and the page
	// listing every repository is the right place to land.
	target := "http://" + st.Addr + "/"
	switch name, err := snapshotHere(db); {
	case err == nil:
		target += name
	case !errors.Is(err, ErrNoRepo):
		fmt.Fprintf(stderr, "review: %v\n", err)
	}

	if started {
		fmt.Fprintf(stdout, "review: serving at http://%s\n", st.Addr)
	}
	if noOpen {
		fmt.Fprintf(stdout, "%s\n", target)
		return
	}
	openBrowser(target)
}

// snapshotHere records a snapshot of every change in the repository
// holding the current directory, and returns the name it is served under.
func snapshotHere(db string) (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	repo, err := OpenRepo(dir)
	if err != nil {
		return "", err
	}
	d, err := OpenDB(db)
	if err != nil {
		return "", err
	}
	defer d.Close()

	r := &Review{Repo: repo, DB: d, Pin: true}
	changes, err := r.Repo.Changes()
	if err != nil {
		return "", err
	}
	for _, c := range changes {
		if c.Working {
			continue
		}
		if _, _, err := r.Grab(c); err != nil {
			return "", err
		}
	}
	return d.RepoName(repo.Root())
}

func cmdStop(args []string) {
	f := newFlags("stop", "")
	f.Parse(args)
	if f.NArg() != 0 {
		f.Usage()
	}
	if stopServer(dbPath()) {
		fmt.Fprintln(stdout, "review: stopped")
		return
	}
	fmt.Fprintln(stdout, "review: not running")
}

func cmdRestart(args []string) {
	f := newFlags("restart", "[-a addr]")
	serveFlags(f)
	f.Parse(args)
	if f.NArg() != 0 {
		f.Usage()
	}

	db := dbPath()
	// Keep the address it was already on, so that open tabs keep working,
	// unless the global -a asks for a different one.
	want := addr
	if want == defaultAddr {
		if st, err := readState(db); err == nil && st.Addr != "" {
			want = st.Addr
		}
	}
	stopServer(db)
	st, err := startServer(db, want)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(stdout, "review: serving at http://%s\n", st.Addr)
}
