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
	"slices"
	"strings"
	"sync"
	"time"
)

// cmdServe implements "mote serve URL".
//
// Along with the URLs that name a server, it accepts "-", to serve on
// standard input and output, and "-hex-", to serve there in hex (see
// hex.go). The hex form is spelled as a URL rather than a flag because
// nothing but mote itself has any reason to ask for it; serve parses
// no flags, so the leading dash costs nothing.
func cmdServe(args []string) {
	if len(args) != 1 {
		usage()
	}
	url := args[0]
	switch {
	case url == "-":
		if err := serve(stdioConn{}, "", nil); err != nil {
			log.Fatal(err)
		}
	case url == "-hex-":
		if err := serve(serveHex(), "", nil); err != nil {
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

// serveHex prepares standard input and output for a hex-encoded
// session: it takes the terminal, if there is one, out of cooked mode,
// so that it stops echoing back what the client writes and stops
// rewriting the bytes passing through; it writes the handshake line
// that tells the client hex encoding is beginning; and it returns the
// encoded connection to serve. See hex.go.
func serveHex() io.ReadWriteCloser {
	if _, err := makeStdinRaw(); err != nil {
		// Report and keep going: the terminal may already be raw, and
		// if it is not, the client reports the resulting mangled data.
		// This lands ahead of the handshake line, where the client
		// treats it as preamble.
		fmt.Fprintf(os.Stderr, "mote: %v\n", err)
	}
	os.Stdout.WriteString(hexHandshake)
	return newHexConn(stdioConn{})
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
// already secure) and env as the base environment for the commands it
// runs (or nil for this process's environment).
// It returns when ln is closed.
func serveListener(ln net.Listener, password string, env []string) error {
	sem := make(chan struct{}, maxSessions)
	var delay time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
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
			if err := serve(conn, password, env); err != nil {
				log.Print(err)
			}
		}()
	}
}

// serve runs one server session on rw: handshake, optional encryption,
// setup and file upload, command execution, output streaming, exit status.
// It is the entire server; every transport ends up here.
// The command runs with env as its base environment, or this process's
// environment if env is nil. It does not close rw.
func serve(rw io.ReadWriteCloser, password string, env []string) error {
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

	// Start the command. A command naming an uploaded file runs from the
	// temporary tree: exec resolves a relative name like ./prog against
	// Dir, but an absolute name is a path on the client, which must be
	// mapped the same way the uploaded files were. ("go test" runs its
	// test binaries by absolute path.)
	name := req.Args[0]
	slash := strings.ReplaceAll(name, `\`, "/") // a Windows client sends native paths in Args
	for _, f := range req.Files {
		if f.Path == slash {
			if name, err = remotePath(tmpdir, f.Path); err != nil {
				return fail("%v", err)
			}
			break
		}
	}
	c := exec.Command(name)
	c.Args = req.Args
	c.Dir = dir
	if env == nil {
		env = os.Environ()
	}
	c.Env = slices.Concat(env, req.Env) // Concat, not append: env may be shared
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
