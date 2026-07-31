// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// dialServer connects to the server named by rawURL, runs the
// connection handshake and any encryption handshake, and reads the
// server's initial Info response, returning a connection ready for Run.
func dialServer(rawURL string) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid server URL %s: %v", rawURL, err)
	}
	var rwc io.ReadWriteCloser
	password := ""
	switch u.Scheme {
	default:
		return nil, fmt.Errorf("unknown server URL scheme %s://", u.Scheme)
	case "ssh":
		rwc, err = dialSSH(u)
	case "tcp":
		rwc, password, err = dialTCP(u)
	case "tail":
		rwc, err = dialTail(u)
	case "gomote":
		rwc, err = dialGomote(u)
	}
	if err != nil {
		return nil, err
	}
	conn, err := clientConn(rwc, password)
	if err != nil {
		rwc.Close()
		return nil, err
	}
	return conn, nil
}

// clientConn runs the client side of the connection handshake and
// optional encryption handshake on rwc and reads the server's initial
// Info response, recording the server's GOOS and GOARCH in the
// returned connection.
func clientConn(rwc io.ReadWriteCloser, password string) (*Conn, error) {
	if err := clientHandshake(rwc); err != nil {
		return nil, err
	}
	if password != "" {
		s, err := secureClient(rwc, password)
		if err != nil {
			return nil, err
		}
		rwc = s
	}
	conn := newConn(rwc)
	var resp Response
	if _, err := conn.readPacket(&resp); err != nil {
		return nil, fmt.Errorf("reading server info: %v", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("server: %s", resp.Error)
	}
	if resp.Type != "Info" {
		return nil, fmt.Errorf("unexpected response type %q, want Info", resp.Type)
	}
	conn.GOOS, conn.GOARCH = resp.GOOS, resp.GOARCH
	return conn, nil
}

// A procConn is an io.ReadWriteCloser connected to a subprocess's
// standard input and output, used for the ssh and gomote transports.
type procConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *bytes.Buffer // captured standard error, or nil if passed through
}

// startProcConn starts c and returns a procConn connected to it.
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
	stderr, _ := c.Stderr.(*bytes.Buffer)
	return &procConn{cmd: c, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

func (p *procConn) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *procConn) Write(b []byte) (int, error) { return p.stdin.Write(b) }

// SetReadDeadline sets a read deadline when the subprocess pipe
// supports one (*os.File does), for the handshake stall timeout.
func (p *procConn) SetReadDeadline(t time.Time) error {
	if f, ok := p.stdout.(*os.File); ok {
		return f.SetReadDeadline(t)
	}
	return errors.ErrUnsupported
}

func (p *procConn) Close() error {
	p.stdin.Close()
	p.stdout.Close()
	err := p.cmd.Wait()
	if err != nil && p.stderr != nil {
		if msg := strings.TrimSpace(p.stderr.String()); msg != "" {
			err = fmt.Errorf("%v\n%s", err, msg)
		}
	}
	return err
}
