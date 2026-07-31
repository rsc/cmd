// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// cmdServe implements "mote serve URL".
func cmdServe(args []string) {
	if len(args) != 1 {
		usage()
	}
	url := args[0]
	switch {
	case url == "-":
		if err := serve(stdioConn{}, ""); err != nil {
			log.Fatal(err)
		}
	case strings.HasPrefix(url, "tcp://"):
		serveTCP(url)
	case strings.HasPrefix(url, "tail:"):
		serveTail(url)
	default:
		log.Fatalf("cannot serve %s", url)
	}
}

// stdioConn is an io.ReadWriteCloser for serving on standard input and output.
type stdioConn struct{}

func (stdioConn) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdioConn) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdioConn) Close() error                { return nil }

// maxSessions is the maximum number of sessions served at once.
// The limit bounds the resources that clients, which are unauthenticated
// until their handshakes finish, can tie up. Connections beyond the limit
// wait in the listener's queue.
const maxSessions = 64

// serveListener accepts connections on ln and serves a session on each,
// using password to encrypt the session (or "" for transports that are
// already secure). It does not return.
func serveListener(ln net.Listener, password string) {
	sem := make(chan struct{}, maxSessions)
	var delay time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				log.Fatal(err)
			}
			// Transient failure, such as running out of file descriptors.
			// Back off and keep serving instead of killing the server:
			// otherwise a flood of connections could shut it down.
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			log.Printf("accept: %v (retrying in %v)", err, delay)
			time.Sleep(delay)
			continue
		}
		delay = 0
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			defer conn.Close()
			if err := serve(conn, password); err != nil {
				log.Print(err)
			}
		}()
	}
}

// serve runs one server session on rw: handshake, optional encryption,
// setup and file upload, command execution, output streaming, exit status.
// It is the entire server; every transport ends up here.
// It does not close rw.
func serve(rw io.ReadWriteCloser, password string) error {
	// Bound how long an unauthenticated peer can hold the connection.
	// The deadline is cleared once the session is established, because
	// the commands that follow can take arbitrarily long.
	deadline, _ := rw.(deadlineConn)
	if deadline != nil {
		deadline.SetDeadline(time.Now().Add(handshakeTimeout))
	}
	if err := serverHandshake(rw); err != nil {
		return err
	}
	if password != "" {
		s, err := secureServer(rw, password)
		if err != nil {
			return err
		}
		rw = s
	}
	if deadline != nil {
		deadline.SetDeadline(time.Time{})
	}
	conn := newConn(rw)

	if err := conn.writePacket(&Response{Type: "Info", GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}, nil); err != nil {
		return err
	}
	fail := func(format string, args ...any) error {
		err := fmt.Errorf(format, args...)
		conn.writePacket(&Response{Type: "Exit", Error: err.Error()}, nil)
		return err
	}

	var req Request
	if _, err := conn.readPacket(&req); err != nil {
		return fmt.Errorf("reading request: %v", err)
	}
	if req.Type != "Setup" {
		return fail("unexpected request type %q", req.Type)
	}
	if len(req.Args) == 0 {
		return fail("malformed Setup request")
	}
	sizes := make(map[string]int64)
	for _, f := range req.Files {
		if !validHash(f.Hash) || f.Size < 0 {
			return fail("malformed file %q in Setup request", f.Path)
		}
		sizes[f.Hash] = f.Size
	}

	// Ask for any files missing from the cache.
	var need []string
	seen := make(map[string]bool)
	for _, f := range req.Files {
		if !seen[f.Hash] && !inCache(f.Hash, f.Size) {
			seen[f.Hash] = true
			need = append(need, f.Hash)
		}
	}
	if len(need) > 0 {
		if err := conn.writePacket(&Response{Type: "Need", Need: need}, nil); err != nil {
			return err
		}
		var up Request
		size, body, err := conn.readPacketStream(&up)
		if err != nil {
			return fmt.Errorf("reading upload: %v", err)
		}
		if up.Type != "Upload" {
			return fail("unexpected request type %q during upload", up.Type)
		}
		var want int64
		for _, hash := range need {
			want += sizes[hash]
		}
		if size != want {
			return fail("upload size %d does not match requested %d", size, want)
		}
		for _, hash := range need {
			if err := saveToCache(hash, sizes[hash], body); err != nil {
				return fail("%v", err)
			}
		}
	}

	// Reconstruct the directory tree in a temporary directory.
	tmpdir, err := os.MkdirTemp("", "mote-")
	if err != nil {
		return fail("%v", err)
	}
	defer os.RemoveAll(tmpdir)
	for _, f := range req.Files {
		dst, err := remotePath(tmpdir, f.Path)
		if err != nil {
			return fail("%v", err)
		}
		if err := copyFromCache(f.Hash, dst); err != nil {
			return fail("%v", err)
		}
	}
	dir, err := remotePath(tmpdir, req.Dir)
	if err != nil {
		return fail("%v", err)
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return fail("%v", err)
	}

	// Everything is in place; wait for the Start request.
	if err := conn.writePacket(&Response{Type: "Ready"}, nil); err != nil {
		return err
	}
	var start Request
	if _, err := conn.readPacket(&start); err != nil {
		return fmt.Errorf("reading request: %v", err)
	}
	if start.Type == "Kill" {
		return fail("killed before start")
	}
	if start.Type != "Start" {
		return fail("unexpected request type %q", start.Type)
	}

	// Start the command.
	c := exec.Command(req.Args[0])
	c.Args = req.Args
	c.Dir = dir
	c.Env = append(os.Environ(), req.Env...)
	setpgid(c)
	stdout, err := c.StdoutPipe()
	if err != nil {
		return fail("%v", err)
	}
	stderr, err := c.StderrPipe()
	if err != nil {
		return fail("%v", err)
	}
	if err := c.Start(); err != nil {
		return fail("%v", err)
	}

	// Watch for a Kill request (or a hangup) from the client.
	// The exited check avoids killing a reused pid after the command is gone.
	exited := make(chan struct{})
	go func() {
		for {
			var req Request
			if _, err := conn.readPacket(&req); err != nil || req.Type == "Kill" {
				select {
				case <-exited:
				default:
					killGroup(c)
				}
				return
			}
		}
	}()

	// Stream output until both pipes close, then report the exit status.
	var wg sync.WaitGroup
	wg.Add(2)
	go copyOutput(&wg, conn, c, stdout, false)
	go copyOutput(&wg, conn, c, stderr, true)
	wg.Wait()
	c.Wait()
	close(exited)
	cleanCache()
	ps := c.ProcessState
	return conn.writePacket(&Response{Type: "Exit", ExitCode: ps.ExitCode(), Status: ps.String()}, nil)
}

// copyOutput streams the command output read from r to the client
// as Output responses, killing the command if the client is gone.
// It decrements wg when the output pipe closes.
func copyOutput(wg *sync.WaitGroup, conn *Conn, c *exec.Cmd, r io.Reader, stderr bool) {
	defer wg.Done()
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if err := conn.writePacket(&Response{Type: "Output", Stderr: stderr}, buf[:n]); err != nil {
				killGroup(c)
				return
			}
		}
		if err != nil {
			return
		}
	}
}
