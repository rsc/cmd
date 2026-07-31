// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"io"
	"net/url"
	"os/exec"
)

// dialServer connects to the server named by rawURL,
// returning the connection and the password to use for encryption
// ("" for transports that are already secure).
func dialServer(rawURL string) (io.ReadWriteCloser, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("invalid server URL %s: %v", rawURL, err)
	}
	switch u.Scheme {
	case "ssh":
		c, err := dialSSH(u)
		return c, "", err
	case "tcp":
		return dialTCP(u)
	case "tail":
		c, err := dialTail(u)
		return c, "", err
	case "gomote":
		c, err := dialGomote(u)
		return c, "", err
	}
	return nil, "", fmt.Errorf("unknown server URL scheme %s://", u.Scheme)
}

// A procConn is an io.ReadWriteCloser connected to a subprocess's
// standard input and output, used for the ssh and gomote transports.
type procConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

// startProcConn starts c and returns a procConn connected to it.
// The subprocess's standard error goes to mote's standard error.
func startProcConn(c *exec.Cmd) (*procConn, error) {
	stdin, err := c.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, err
	}
	return &procConn{cmd: c, stdin: stdin, stdout: stdout}, nil
}

func (p *procConn) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *procConn) Write(b []byte) (int, error) { return p.stdin.Write(b) }

func (p *procConn) Close() error {
	p.stdin.Close()
	p.stdout.Close()
	return p.cmd.Wait()
}
