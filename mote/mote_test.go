// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for ssh, gomote, and go
// when invoked under those names, so that the subprocess transports
// can be tested without the real commands. See doc.go's TESTING comment.
func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case "ssh":
		sshMockMain()
	case "gomote":
		gomoteMockMain()
	case "go":
		goMockMain()
	}
	os.Exit(m.Run())
}

// setupDirs points the mote config and cache at temporary directories.
func setupDirs(t *testing.T) {
	t.Setenv("MOTECONFIG", t.TempDir())
	t.Setenv("MOTECACHE", t.TempDir())
}

// runPipe runs a full client/server session over an in-memory pipe.
func runPipe(t *testing.T, password string, files []*File, dir string, args []string) (exit int, status, stdout, stderr string) {
	t.Helper()
	cconn, sconn := net.Pipe()
	defer cconn.Close()
	done := make(chan error, 1)
	go func() {
		done <- serve(sconn, password)
		sconn.Close()
	}()
	var outb, errb bytes.Buffer
	exit, status, goos, goarch, err := runSession(cconn, password, files, dir, args, &outb, &errb)
	if err != nil {
		t.Fatalf("runSession: %v", err)
	}
	cconn.Close()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if goos != runtime.GOOS || goarch != runtime.GOARCH {
		t.Errorf("session reported GOOS-GOARCH %s-%s, want %s-%s", goos, goarch, runtime.GOOS, runtime.GOARCH)
	}
	return exit, status, outb.String(), errb.String()
}

func TestRunEcho(t *testing.T) {
	setupDirs(t)
	exit, _, stdout, stderr := runPipe(t, "", nil, "/mote-test", []string{"echo", "hello", "world"})
	if exit != 0 || stdout != "hello world\n" || stderr != "" {
		t.Errorf("echo: exit=%d stdout=%q stderr=%q, want 0, %q, %q", exit, stdout, stderr, "hello world\n", "")
	}
}

func TestExitCode(t *testing.T) {
	setupDirs(t)
	exit, _, _, _ := runPipe(t, "", nil, "/mote-test", []string{"sh", "-c", "exit 3"})
	if exit != 3 {
		t.Errorf("exit=%d, want 3", exit)
	}
}

func TestStderr(t *testing.T) {
	setupDirs(t)
	exit, _, stdout, stderr := runPipe(t, "", nil, "/mote-test", []string{"sh", "-c", "echo out; echo err >&2"})
	if exit != 0 || stdout != "out\n" || stderr != "err\n" {
		t.Errorf("exit=%d stdout=%q stderr=%q, want 0, %q, %q", exit, stdout, stderr, "out\n", "err\n")
	}
}

func TestUploadRun(t *testing.T) {
	setupDirs(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "x.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho fromscript $(basename $(pwd))\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var files []*File
	if err := addFile(&files, script); err != nil {
		t.Fatal(err)
	}
	slashDir := filepath.ToSlash(dir)
	exit, _, stdout, _ := runPipe(t, "", files, slashDir, []string{"./x.sh"})
	want := "fromscript " + filepath.Base(dir) + "\n"
	if exit != 0 || stdout != want {
		t.Errorf("exit=%d stdout=%q, want 0, %q", exit, stdout, want)
	}
	// Second run should find the file already cached (and still work).
	if !inCache(files[0].Hash, files[0].Size) {
		t.Errorf("file not in cache after run")
	}
	exit, _, stdout, _ = runPipe(t, "", files, slashDir, []string{"./x.sh"})
	if exit != 0 || stdout != want {
		t.Errorf("cached run: exit=%d stdout=%q, want 0, %q", exit, stdout, want)
	}
}

func TestRelativeUpload(t *testing.T) {
	// Command in a sibling directory, like the ../testprog example in doc.go.
	setupDirs(t)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "myprog"), 0o777)
	script := filepath.Join(dir, "testprog")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho sibling\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var files []*File
	if err := addFile(&files, script); err != nil {
		t.Fatal(err)
	}
	exit, _, stdout, _ := runPipe(t, "", files, filepath.ToSlash(filepath.Join(dir, "myprog")), []string{"../testprog"})
	if exit != 0 || stdout != "sibling\n" {
		t.Errorf("exit=%d stdout=%q, want 0, %q", exit, stdout, "sibling\n")
	}
}

// startServeClient starts a server on one end of a pipe and
// handshakes a raw packet connection on the other, for tests that
// drive the protocol directly.
func startServeClient(t *testing.T, password string) *packetConn {
	t.Helper()
	cconn, sconn := net.Pipe()
	go func() {
		serve(sconn, password)
		sconn.Close()
	}()
	t.Cleanup(func() { cconn.Close() })
	if err := clientHandshake(cconn); err != nil {
		t.Fatal(err)
	}
	return newPacketConn(cconn)
}

func TestKill(t *testing.T) {
	setupDirs(t)
	pc := startServeClient(t, "")
	err := pc.writePacket(&Request{Type: "Run", Cmd: "sleep", Args: []string{"sleep", "300"}, Dir: "/mote-test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if _, err := pc.readPacket(&resp); err != nil || resp.Type != "Start" {
		t.Fatalf("got %+v, %v; want Start", resp, err)
	}
	start := time.Now()
	if err := pc.writePacket(&Request{Type: "Kill"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.readPacket(&resp); err != nil || resp.Type != "Exit" {
		t.Fatalf("got %+v, %v; want Exit", resp, err)
	}
	if resp.ExitCode >= 0 {
		t.Errorf("ExitCode=%d Status=%q, want signal death", resp.ExitCode, resp.Status)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("kill took %v", d)
	}
}

func TestBadUploadHash(t *testing.T) {
	setupDirs(t)
	pc := startServeClient(t, "")
	badHash := strings.Repeat("ab", 32)
	req := &Request{
		Type:  "Run",
		Cmd:   "./x",
		Args:  []string{"./x"},
		Dir:   "/mote-test",
		Files: []*File{{Path: "/mote-test/x", Hash: badHash, Size: 5}},
	}
	if err := pc.writePacket(req, nil); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if _, err := pc.readPacket(&resp); err != nil || resp.Type != "Need" {
		t.Fatalf("got %+v, %v; want Need", resp, err)
	}
	if err := pc.writePacket(&Request{Type: "Upload"}, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.readPacket(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Type != "Exit" || !strings.Contains(resp.Error, "hash") {
		t.Fatalf("got %+v, want Exit with hash error", resp)
	}
	if inCache(badHash, 5) {
		t.Errorf("corrupt upload saved to cache")
	}
}

func TestServerError(t *testing.T) {
	setupDirs(t)
	pc := startServeClient(t, "")
	err := pc.writePacket(&Request{Type: "Run", Cmd: "/nonexistent/command/xyzzy", Args: []string{"/nonexistent/command/xyzzy"}, Dir: "/mote-test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if _, err := pc.readPacket(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Type != "Exit" || resp.Error == "" {
		t.Fatalf("got %+v, want Exit with error", resp)
	}
}

func TestVersionAndAliasDispatch(t *testing.T) {
	setupDirs(t)
	if err := setAlias("kremvax", "ssh://kremvax"); err != nil {
		t.Fatal(err)
	}
	if err := setAlias("linux-amd64", "ssh://kremvax"); err != nil {
		t.Fatal(err)
	}
	url, err := lookupAlias("kremvax")
	if err != nil || url != "ssh://kremvax" {
		t.Fatalf("lookupAlias = %q, %v", url, err)
	}
	aliases, err := readAliases()
	if err != nil || len(aliases) != 2 {
		t.Fatalf("readAliases = %v, %v", aliases, err)
	}
}

// mockPATH prepends a temp directory containing name as a symlink
// to the test binary, so that exec.Command(name, ...) reinvokes the
// test binary with os.Args[0] set to name.
func mockPATH(t *testing.T, names ...string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, name := range names {
		if err := os.Symlink(exe, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func sshMockMain() {
	log.SetPrefix("ssh mock: ")
	log.SetFlags(0)
	args := strings.Join(os.Args[1:], " ")
	for _, want := range []string{"ControlMaster auto", "ControlPersist 1800", "ControlPath ~/.ssh/sockets/mote-%r@%h-%p", "kremvax mote serve -"} {
		if !strings.Contains(args, want) {
			log.Fatalf("missing %q in args: %s", want, args)
		}
	}
	// A login banner, to exercise the client's preamble scanning.
	fmt.Printf("Welcome to kremvax.\nUnauthorized access is prohibited.\n")
	if err := serve(stdioConn{}, ""); err != nil {
		log.Fatal(err)
	}
	os.Exit(0)
}

func TestSSHTransport(t *testing.T) {
	setupDirs(t)
	mockPATH(t, "ssh")
	rwc, password, err := dialServer("ssh://kremvax")
	if err != nil {
		t.Fatal(err)
	}
	defer rwc.Close()
	var outb, errb bytes.Buffer
	exit, _, _, _, err := runSession(rwc, password, nil, "/mote-test", []string{"echo", "over ssh"}, &outb, &errb)
	if err != nil {
		t.Fatalf("runSession: %v", err)
	}
	if exit != 0 || outb.String() != "over ssh\n" {
		t.Errorf("exit=%d stdout=%q, want 0, %q", exit, outb.String(), "over ssh\n")
	}
}

func gomoteMockMain() {
	log.SetPrefix("gomote mock: ")
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal("no subcommand")
	}
	switch os.Args[1] {
	case "list":
		// No instances.
		os.Exit(0)
	case "create":
		if os.Args[2] != "gotip-linux-amd64" {
			log.Fatalf("create %s", os.Args[2])
		}
		fmt.Printf("user-gotip-linux-amd64-0\n")
		os.Exit(0)
	case "put":
		if len(os.Args) != 4 || os.Args[2] != "user-gotip-linux-amd64-0" {
			log.Fatalf("bad put args: %v", os.Args)
		}
		if _, err := os.Stat(os.Args[3]); err != nil {
			log.Fatalf("put: %v", err)
		}
		os.Exit(0)
	case "ssh":
		want := []string{"ssh", "user-gotip-linux-amd64-0", "./mote", "serve", "-"}
		if len(os.Args) != 6 || strings.Join(os.Args[1:], " ") != strings.Join(want, " ") {
			log.Fatalf("bad ssh args: %v", os.Args)
		}
		if err := serve(stdioConn{}, ""); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}
	log.Fatalf("unexpected subcommand %s", os.Args[1])
}

func goMockMain() {
	// Mock "go build -o bin rsc.io/cmd/mote": create a dummy binary.
	log.SetPrefix("go mock: ")
	log.SetFlags(0)
	if len(os.Args) >= 4 && os.Args[1] == "build" && os.Args[2] == "-o" {
		if err := os.WriteFile(os.Args[3], []byte("dummy"), 0o755); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}
	log.Fatalf("unexpected args: %v", os.Args)
}

func TestGomoteTransport(t *testing.T) {
	setupDirs(t)
	mockPATH(t, "gomote", "go")
	rwc, password, err := dialServer("gomote://gotip-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer rwc.Close()
	var outb, errb bytes.Buffer
	exit, _, _, _, err := runSession(rwc, password, nil, "/mote-test", []string{"echo", "over gomote"}, &outb, &errb)
	if err != nil {
		t.Fatalf("runSession: %v", err)
	}
	if exit != 0 || outb.String() != "over gomote\n" {
		t.Errorf("exit=%d stdout=%q, want 0, %q", exit, outb.String(), "over gomote\n")
	}
}

func TestResolveServer(t *testing.T) {
	setupDirs(t)
	if err := setAlias("kremvax", "ssh://kremvax"); err != nil {
		t.Fatal(err)
	}
	if url, err := resolveServer("kremvax", "echo", nil); err != nil || url != "ssh://kremvax" {
		t.Errorf("resolveServer(kremvax) = %q, %v", url, err)
	}
	if url, err := resolveServer("tcp://h:1/pw", "echo", nil); err != nil || url != "tcp://h:1/pw" {
		t.Errorf("resolveServer(URL) = %q, %v", url, err)
	}
	t.Setenv("MOTE", "kremvax")
	if url, err := resolveServer("", "echo", nil); err != nil || url != "ssh://kremvax" {
		t.Errorf("resolveServer with $MOTE = %q, %v", url, err)
	}
	t.Setenv("MOTE", "")
	t.Setenv("GOOS", "linux")
	t.Setenv("GOARCH", "amd64")
	if err := setAlias("linux-amd64", "tcp://h:1/pw"); err != nil {
		t.Fatal(err)
	}
	if url, err := resolveServer("", "echo", nil); err != nil || url != "tcp://h:1/pw" {
		t.Errorf("resolveServer with $GOOS/$GOARCH = %q, %v", url, err)
	}
}
