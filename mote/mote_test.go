// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"io"
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
func runPipe(t *testing.T, password string, files []*File, dir string, args []string) (code int, status, stdout, stderr string) {
	t.Helper()
	cconn, sconn := net.Pipe()
	defer cconn.Close()
	done := make(chan error, 1)
	go func() {
		done <- serve(sconn, password, nil)
		sconn.Close()
	}()
	conn, err := clientConn(cconn, password)
	if err != nil {
		t.Fatalf("clientConn: %v", err)
	}
	var outb, errb bytes.Buffer
	w, err := conn.Run(&Exec{Args: args, Dir: dir, Files: files, Stdout: &outb, Stderr: &errb})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	cconn.Close()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if conn.GOOS != runtime.GOOS || conn.GOARCH != runtime.GOARCH {
		t.Errorf("session reported GOOS-GOARCH %s-%s, want %s-%s", conn.GOOS, conn.GOARCH, runtime.GOOS, runtime.GOARCH)
	}
	return w.Code, w.Status, outb.String(), errb.String()
}

func TestRunEcho(t *testing.T) {
	setupDirs(t)
	code, _, stdout, stderr := runPipe(t, "", nil, "/mote-test", []string{"echo", "hello", "world"})
	if code != 0 || stdout != "hello world\n" || stderr != "" {
		t.Errorf("echo: code=%d stdout=%q stderr=%q, want 0, %q, %q", code, stdout, stderr, "hello world\n", "")
	}
}

func TestExitCode(t *testing.T) {
	setupDirs(t)
	code, _, _, _ := runPipe(t, "", nil, "/mote-test", []string{"sh", "-c", "exit 3"})
	if code != 3 {
		t.Errorf("code=%d, want 3", code)
	}
}

func TestStderr(t *testing.T) {
	setupDirs(t)
	code, _, stdout, stderr := runPipe(t, "", nil, "/mote-test", []string{"sh", "-c", "echo out; echo err >&2"})
	if code != 0 || stdout != "out\n" || stderr != "err\n" {
		t.Errorf("code=%d stdout=%q stderr=%q, want 0, %q, %q", code, stdout, stderr, "out\n", "err\n")
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
	code, _, stdout, _ := runPipe(t, "", files, slashDir, []string{"./x.sh"})
	want := "fromscript " + filepath.Base(dir) + "\n"
	if code != 0 || stdout != want {
		t.Errorf("code=%d stdout=%q, want 0, %q", code, stdout, want)
	}
	// Second run should find the file already cached (and still work).
	if !inCache(files[0].Hash, files[0].Size) {
		t.Errorf("file not in cache after run")
	}
	code, _, stdout, _ = runPipe(t, "", files, slashDir, []string{"./x.sh"})
	if code != 0 || stdout != want {
		t.Errorf("cached run: code=%d stdout=%q, want 0, %q", code, stdout, want)
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
	code, _, stdout, _ := runPipe(t, "", files, filepath.ToSlash(filepath.Join(dir, "myprog")), []string{"../testprog"})
	if code != 0 || stdout != "sibling\n" {
		t.Errorf("code=%d stdout=%q, want 0, %q", code, stdout, "sibling\n")
	}
}

func TestCleanCache(t *testing.T) {
	setupDirs(t)
	stale, fresh := cacheFile(strings.Repeat("aa", 32)), cacheFile(strings.Repeat("bb", 32))
	for _, file := range []string{stale, fresh} {
		if err := os.MkdirAll(filepath.Dir(file), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("x"), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-cacheMaxAge - time.Hour)
	os.Chtimes(stale, old, old)
	cleanCache()
	if _, err := os.Stat(stale); err == nil {
		t.Errorf("stale cache file survived cleanCache")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh cache file deleted by cleanCache: %v", err)
	}
}

// startServeClient starts a server on one end of a pipe and completes
// the handshake and Info exchange on the other, for tests that
// drive the protocol directly.
func startServeClient(t *testing.T, password string) *Conn {
	t.Helper()
	cconn, sconn := net.Pipe()
	go func() {
		serve(sconn, password, nil)
		sconn.Close()
	}()
	t.Cleanup(func() { cconn.Close() })
	conn, err := clientConn(cconn, password)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestKill(t *testing.T) {
	setupDirs(t)
	conn := startServeClient(t, "")
	err := conn.writePacket(&Request{Type: "Setup", Args: []string{"sleep", "300"}, Dir: "/mote-test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if _, err := conn.readPacket(&resp); err != nil || resp.Type != "Ready" {
		t.Fatalf("got %+v, %v; want Ready", resp, err)
	}
	if err := conn.writePacket(&Request{Type: "Start"}, nil); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := conn.writePacket(&Request{Type: "Kill"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.readPacket(&resp); err != nil || resp.Type != "Exit" {
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
	conn := startServeClient(t, "")
	badHash := strings.Repeat("ab", 32)
	req := &Request{
		Type:  "Setup",
		Args:  []string{"./x"},
		Dir:   "/mote-test",
		Files: []*File{{Path: "/mote-test/x", Hash: badHash, Size: 5}},
	}
	if err := conn.writePacket(req, nil); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if _, err := conn.readPacket(&resp); err != nil || resp.Type != "Need" {
		t.Fatalf("got %+v, %v; want Need", resp, err)
	}
	if err := conn.writePacket(&Request{Type: "Upload"}, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.readPacket(&resp); err != nil {
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
	conn := startServeClient(t, "")
	err := conn.writePacket(&Request{Type: "Setup", Args: []string{"/nonexistent/command/xyzzy"}, Dir: "/mote-test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if _, err := conn.readPacket(&resp); err != nil || resp.Type != "Ready" {
		t.Fatalf("got %+v, %v; want Ready", resp, err)
	}
	if err := conn.writePacket(&Request{Type: "Start"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.readPacket(&resp); err != nil {
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

// runConn runs a command over an established connection and checks
// its output, for the transport tests.
func runConn(t *testing.T, conn *Conn, args []string, want string) {
	t.Helper()
	var outb, errb bytes.Buffer
	w, err := conn.Run(&Exec{Args: args, Dir: "/mote-test", Stdout: &outb, Stderr: &errb})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if w.Code != 0 || outb.String() != want {
		t.Errorf("code=%d stdout=%q, want 0, %q", w.Code, outb.String(), want)
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
	if strings.Contains(args, "-O exit") {
		// mote close: stop the shared connection. The ControlPath is
		// the %-template for a URL close and a literal socket path for
		// a close-everything sweep; both name kremvax.
		for _, want := range []string{"ControlPath ", "kremvax"} {
			if !strings.Contains(args, want) {
				log.Fatalf("missing %q in close args: %s", want, args)
			}
		}
		if os.Getenv("MOTE_TEST_SSH_NOMASTER") != "" {
			fmt.Fprintf(os.Stderr, "Control socket connect(/home/user/.ssh/sockets/mote-user@kremvax-22): No such file or directory\n")
			os.Exit(255)
		}
		fmt.Fprintf(os.Stderr, "Exit request sent.\n")
		os.Exit(0)
	}
	for _, want := range []string{"ControlMaster auto", "ControlPersist 1800", "ControlPath ~/.ssh/sockets/mote-%r@%h-%p", "kremvax mote serve -"} {
		if !strings.Contains(args, want) {
			log.Fatalf("missing %q in args: %s", want, args)
		}
	}
	if msg := os.Getenv("MOTE_TEST_SSH_FAIL"); msg != "" {
		// Simulate ssh failing to connect: diagnostics on standard error,
		// no server hello.
		fmt.Fprintf(os.Stderr, "%s\n", msg)
		os.Exit(255)
	}
	if msg := os.Getenv("MOTE_TEST_SSH_DIE"); msg != "" {
		// Simulate the connection dying mid-session: the handshake and
		// Info exchange succeed, and then the connection is gone.
		if err := serverHandshake(stdioConn{}); err != nil {
			log.Fatal(err)
		}
		conn := newConn(stdioConn{})
		conn.writePacket(&Response{Type: "Info", GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}, nil)
		fmt.Fprintf(os.Stderr, "%s\n", msg)
		os.Exit(255)
	}
	// A login banner, to exercise the client's preamble scanning.
	fmt.Printf("Welcome to kremvax.\nUnauthorized access is prohibited.\n")
	if err := serve(stdioConn{}, "", nil); err != nil {
		log.Fatal(err)
	}
	os.Exit(0)
}

func TestSSHTransport(t *testing.T) {
	setupDirs(t)
	mockPATH(t, "ssh")
	conn, err := dialServer("ssh://kremvax")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	runConn(t, conn, []string{"echo", "over ssh"}, "over ssh\n")
}

func TestSSHTransportError(t *testing.T) {
	// A failed handshake must report the transport's standard error text,
	// which usually explains what went wrong.
	setupDirs(t)
	mockPATH(t, "ssh")
	t.Setenv("MOTE_TEST_SSH_FAIL", "ssh: connect to host kremvax port 22: Connection refused")
	_, err := dialServer("ssh://kremvax")
	if err == nil || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("dialServer: %v, want ssh stderr in error", err)
	}
}

func TestSSHTransportSessionError(t *testing.T) {
	// A protocol failure after the handshake must also report the
	// transport's standard error text, via Conn.abort.
	setupDirs(t)
	mockPATH(t, "ssh")
	t.Setenv("MOTE_TEST_SSH_DIE", "Connection to kremvax closed by remote host.")
	conn, err := dialServer("ssh://kremvax")
	if err != nil {
		t.Fatalf("dialServer: %v", err)
	}
	_, err = conn.Run(&Exec{Args: []string{"echo", "hi"}, Dir: "/mote-test", Stdout: io.Discard, Stderr: io.Discard})
	if err == nil {
		t.Fatal("Run succeeded, want error")
	}
	err = conn.abort(err)
	if !strings.Contains(err.Error(), "closed by remote host") {
		t.Fatalf("abort: %v, want ssh stderr in error", err)
	}
}

// The gomote mock takes direction from the test environment:
// $MOTE_TEST_GOMOTE_INST is the instance name to expect (and print
// from create), $MOTE_TEST_GOMOTE_LIST is the "gomote list" output,
// and $MOTE_TEST_GOMOTE_GROUPS, if set, means the mote group exists.
func gomoteMockMain() {
	log.SetPrefix("gomote mock: ")
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal("no subcommand")
	}
	inst := os.Getenv("MOTE_TEST_GOMOTE_INST")
	if inst == "" {
		inst = "user-gotip-linux-amd64-0"
	}
	switch os.Args[1] {
	case "list":
		fmt.Print(os.Getenv("MOTE_TEST_GOMOTE_LIST"))
		os.Exit(0)
	case "group":
		if len(os.Args) == 3 && os.Args[2] == "list" {
			fmt.Printf("Name\tInstances\t\n")
			if os.Getenv("MOTE_TEST_GOMOTE_GROUPS") != "" {
				fmt.Printf("mote\t(none)\t\n")
			}
			os.Exit(0)
		}
	case "create":
		if len(os.Args) == 3 && os.Args[2] == "-list" {
			fmt.Print("gotip-linux-amd64\n" +
				"gotip-linux-amd64-longtest\n" +
				"gotip-linux-amd64-race\n" +
				"gotip-freebsd-amd64_14.2\n" +
				"gotip-freebsd-amd64_15.0\n" +
				"gotip-linux-ppc64_power8\n" +
				"gotip-linux-ppc64_power9\n" +
				"gotip-linux-ppc64_power10\n")
			os.Exit(0)
		}
		if os.Getenv("MOTE_TEST_GOMOTE_LIST") != "" {
			log.Fatalf("create called when reuse expected: %v", os.Args)
		}
		if os.Getenv("MOTE_TEST_GOMOTE_GROUPS") != "" {
			// Group exists: expect plain create with GOMOTE_GROUP set.
			if os.Getenv("GOMOTE_GROUP") != "mote" || len(os.Args) != 3 || os.Args[2] != "gotip-linux-amd64" {
				log.Fatalf("bad create args %v with GOMOTE_GROUP=%q", os.Args, os.Getenv("GOMOTE_GROUP"))
			}
		} else if len(os.Args) != 4 || os.Args[2] != "-new-group=mote" || os.Args[3] != "gotip-linux-amd64" {
			log.Fatalf("bad create args: %v", os.Args)
		}
		fmt.Printf("%s\n", inst)
		os.Exit(0)
	case "destroy":
		if len(os.Args) != 3 || os.Args[2] != inst {
			log.Fatalf("bad destroy args: %v", os.Args)
		}
		os.Exit(0)
	case "put":
		if len(os.Args) != 4 || os.Args[2] != inst {
			log.Fatalf("bad put args: %v", os.Args)
		}
		if _, err := os.Stat(os.Args[3]); err != nil {
			log.Fatalf("put: %v", err)
		}
		os.Exit(0)
	case "ssh":
		// Like the real gomote ssh: only an instance name, no command;
		// print the underlying ssh invocation, and hand the connection
		// to a "shell" that reads a command line from standard input.
		if len(os.Args) != 3 || os.Args[2] != inst {
			log.Fatalf("bad ssh args: %v", os.Args)
		}
		fmt.Printf("$ /usr/bin/ssh -p 2222 %s@gomotessh.golang.org\n", inst)
		var line []byte
		var b [1]byte
		for b[0] != '\n' {
			if _, err := os.Stdin.Read(b[:]); err != nil {
				log.Fatalf("reading command: %v", err)
			}
			line = append(line, b[0])
		}
		if string(line) != "exec ./mote serve -\n" {
			log.Fatalf("bad shell command %q", line)
		}
		if err := serve(stdioConn{}, "", nil); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}
	log.Fatalf("unexpected subcommand %s", os.Args[1])
}

func goMockMain() {
	// Mock "go build -o bin": create a dummy binary.
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
	conn, err := dialServer("gomote://gotip-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	runConn(t, conn, []string{"echo", "over gomote"}, "over gomote\n")
}

func TestGomoteTransportExistingGroup(t *testing.T) {
	setupDirs(t)
	mockPATH(t, "gomote", "go")
	t.Setenv("MOTE_TEST_GOMOTE_GROUPS", "mote")
	conn, err := dialServer("gomote://gotip-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	runConn(t, conn, []string{"echo", "over gomote"}, "over gomote\n")
}

func TestGomoteTransportReuse(t *testing.T) {
	setupDirs(t)
	mockPATH(t, "gomote", "go")
	// One matching instance outside the mote group (must not be reused)
	// and one inside it.
	t.Setenv("MOTE_TEST_GOMOTE_LIST",
		"user-gotip-linux-amd64-3\tgotip-linux-amd64\thost-amd64\texpires in 1h\n"+
			"user-gotip-linux-amd64-7 (mote, other)\tgotip-linux-amd64\thost-amd64\texpires in 1h\n")
	t.Setenv("MOTE_TEST_GOMOTE_INST", "user-gotip-linux-amd64-7")
	conn, err := dialServer("gomote://gotip-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	runConn(t, conn, []string{"echo", "over gomote"}, "over gomote\n")
}

func TestCloseSSH(t *testing.T) {
	setupDirs(t)
	mockPATH(t, "ssh")
	if err := closeSSH(mustParse(t, "ssh://kremvax")); err != nil {
		t.Fatalf("closeSSH: %v", err)
	}
	// No shared connection running is not an error.
	t.Setenv("MOTE_TEST_SSH_NOMASTER", "1")
	if err := closeSSH(mustParse(t, "ssh://kremvax")); err != nil {
		t.Fatalf("closeSSH with no shared connection: %v", err)
	}
}

func TestCloseAll(t *testing.T) {
	setupDirs(t)
	mockPATH(t, "ssh", "gomote")
	// A shared ssh connection, identified by its control socket.
	home := t.TempDir()
	t.Setenv("HOME", home)
	sock := filepath.Join(home, ".ssh", "sockets", "mote-user@kremvax-22")
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// One gomote instance in the mote group and one outside it,
	// which must be left alone.
	t.Setenv("MOTE_TEST_GOMOTE_LIST",
		"user-gotip-linux-amd64-7 (mote)\tgotip-linux-amd64\thost-amd64\texpires in 1h\n"+
			"user-gotip-linux-arm64-1\tgotip-linux-arm64\thost-arm64\texpires in 1h\n")
	t.Setenv("MOTE_TEST_GOMOTE_INST", "user-gotip-linux-amd64-7")
	if err := closeAll(); err != nil {
		t.Fatalf("closeAll: %v", err)
	}
}

func TestCloseGomote(t *testing.T) {
	setupDirs(t)
	mockPATH(t, "gomote")
	t.Setenv("MOTE_TEST_GOMOTE_LIST",
		"user-gotip-linux-amd64-7 (mote)\tgotip-linux-amd64\thost-amd64\texpires in 1h\n")
	t.Setenv("MOTE_TEST_GOMOTE_INST", "user-gotip-linux-amd64-7")
	if err := closeGomote(mustParse(t, "gomote://gotip-linux-amd64")); err != nil {
		t.Fatalf("closeGomote: %v", err)
	}
	// No instance to destroy is not an error.
	t.Setenv("MOTE_TEST_GOMOTE_LIST", "")
	if err := closeGomote(mustParse(t, "gomote://gotip-linux-amd64")); err != nil {
		t.Fatalf("closeGomote with no instance: %v", err)
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

func TestResolveServerGomote(t *testing.T) {
	setupDirs(t)
	mockPATH(t, "gomote")
	for _, tt := range []struct{ name, want string }{
		{"linux-amd64", "gomote://gotip-linux-amd64"},
		{"freebsd-amd64", "gomote://gotip-freebsd-amd64_15.0"},
		{"linux-ppc64", "gomote://gotip-linux-ppc64_power10"},
	} {
		if url, err := resolveServer(tt.name, "echo", nil); err != nil || url != tt.want {
			t.Errorf("resolveServer(%s) = %q, %v; want %q", tt.name, url, err, tt.want)
		}
	}
	if _, err := resolveServer("plan9-386", "echo", nil); err == nil {
		t.Errorf("resolveServer(plan9-386) succeeded, want error")
	}
}
