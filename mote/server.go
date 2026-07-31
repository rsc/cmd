// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
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

// stdioConn is an io.ReadWriter for serving on standard input and output.
type stdioConn struct{}

func (stdioConn) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdioConn) Write(p []byte) (int, error) { return os.Stdout.Write(p) }

// serve runs one server session on rw: handshake, optional encryption,
// file upload, command execution, output streaming, exit status.
// It is the entire server; every transport ends up here.
func serve(rw io.ReadWriter, password string) error {
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
	pc := newPacketConn(rw)

	// GOOS and GOARCH ride on the first response of the session.
	sentInfo := false
	info := func(r *Response) *Response {
		if !sentInfo {
			sentInfo = true
			r.GOOS = runtime.GOOS
			r.GOARCH = runtime.GOARCH
		}
		return r
	}
	fail := func(format string, args ...any) error {
		err := fmt.Errorf(format, args...)
		pc.writePacket(info(&Response{Type: "Exit", Error: err.Error()}), nil)
		return err
	}

	var req Request
	if _, err := pc.readPacket(&req); err != nil {
		return fmt.Errorf("reading request: %v", err)
	}
	if req.Type != "Run" {
		return fail("unexpected request type %q", req.Type)
	}
	if len(req.Args) == 0 || req.Cmd == "" {
		return fail("malformed Run request")
	}
	sizes := make(map[string]int64)
	for _, f := range req.Files {
		if !validHash(f.Hash) || f.Size < 0 {
			return fail("malformed file %q in Run request", f.Path)
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
		if err := pc.writePacket(info(&Response{Type: "Need", Need: need}), nil); err != nil {
			return err
		}
		var up Request
		size, body, err := pc.readPacketStream(&up)
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

	// Start the command.
	c := exec.Command(req.Cmd)
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
	if err := pc.writePacket(info(&Response{Type: "Start"}), nil); err != nil {
		killGroup(c)
		c.Wait()
		return err
	}

	// Watch for a Kill request (or a hangup) from the client.
	// The exited check avoids killing a reused pid after the command is gone.
	exited := make(chan struct{})
	go func() {
		for {
			var req Request
			if _, err := pc.readPacket(&req); err != nil || req.Type == "Kill" {
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
	for _, out := range []struct {
		r      io.Reader
		stderr bool
	}{{stdout, false}, {stderr, true}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 32<<10)
			for {
				n, err := out.r.Read(buf)
				if n > 0 {
					if err := pc.writePacket(&Response{Type: "Output", Stderr: out.stderr}, buf[:n]); err != nil {
						killGroup(c)
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}
	wg.Wait()
	c.Wait()
	close(exited)
	ps := c.ProcessState
	return pc.writePacket(info(&Response{Type: "Exit", ExitCode: ps.ExitCode(), Status: ps.String()}), nil)
}
