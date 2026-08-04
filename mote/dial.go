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
		// Unlike the others, the gomote transport runs the handshake
		// itself: when the direct connection fails it has a second way
		// in to try, and only the handshake says whether the first
		// one worked. See dialGomote.
		return dialGomote(u)
	}
	if err != nil {
		return nil, err
	}
	conn, err := clientConn(rwc, password)
	if err != nil {
		return nil, abortConn(rwc, err)
	}
	return conn, nil
}

// abortConn tears down a connection after a failed session, giving the
// transport a chance to add its own diagnostics to err.
func abortConn(rwc io.ReadWriteCloser, err error) error {
	if a, ok := rwc.(interface{ abort(error) error }); ok {
		return a.abort(err)
	}
	rwc.Close()
	return err
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
		// Bound how long a stalled server can hold the client in the
		// encryption handshake and the Info exchange that follows.
		// (clientHandshake sets its own deadline for the server hello.)
		if d, ok := rwc.(deadlineConn); ok {
			d.SetDeadline(time.Now().Add(handshakeTimeout))
			defer d.SetDeadline(time.Time{})
		}
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
// On a terminal connection (see startPtyConn) both are the same file,
// the terminal's master half.
type procConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *bytes.Buffer // captured standard error, or nil if passed through
	pty    bool          // stdin and stdout are a terminal
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

// startPtyConn starts c on a new pseudo-terminal and returns a procConn
// for the terminal's master half. A subprocess on a terminal behaves as
// if a person were running it: in particular, ssh asks the far end for a
// terminal of its own, which is what makes "gomote ssh" start a shell.
// Standard error is left as the caller set it, keeping the transport's
// own diagnostics out of the protocol stream.
// It returns an error matching errors.ErrUnsupported on systems that
// have no pseudo-terminals.
func startPtyConn(c *exec.Cmd) (*procConn, error) {
	master, slave, err := openPty()
	if err != nil {
		return nil, fmt.Errorf("opening terminal: %w", err)
	}
	c.Stdin, c.Stdout = slave, slave
	if err := c.Start(); err != nil {
		master.Close()
		slave.Close()
		return nil, err
	}
	// Let go of the terminal: with only the subprocess holding it, reads
	// on the master half report the end of the connection when it exits.
	slave.Close()
	stderr, _ := c.Stderr.(*bytes.Buffer)
	return &procConn{cmd: c, stdin: master, stdout: master, stderr: stderr, pty: true}, nil
}

func (p *procConn) Read(b []byte) (int, error) {
	n, err := p.stdout.Read(b)
	if err != nil && p.pty && ptyEOF(err) {
		// A terminal whose subprocess has exited reports EIO on some
		// systems. It means the same thing here as the end of a pipe.
		err = io.EOF
	}
	return n, err
}

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
	if !p.pty { // same file as stdin; closing it twice is not an error, but say what is meant
		p.stdout.Close()
	}
	err := p.cmd.Wait()
	if err != nil && p.stderr != nil {
		if msg := strings.TrimSpace(p.stderr.String()); msg != "" {
			err = fmt.Errorf("%v\n%s", err, msg)
		}
	}
	return err
}

// abort tears down the connection after a protocol failure, appending
// transport diagnostics (such as the ssh subprocess's standard error)
// to err when there are any.
func (c *Conn) abort(err error) error { return abortConn(c.rw, err) }

// abort tears down the subprocess after a failed handshake, appending
// any captured standard error text to err: when the handshake times out
// or the subprocess hangs up, its diagnostics usually explain why.
// Closing stdin tells a healthy subprocess to exit; it gets a few
// seconds to do so and to flush those diagnostics (the handshake can
// fail before the final standard error output has arrived), and then
// it is killed, so that a wedged ssh or gomote cannot hang the client.
func (p *procConn) abort(err error) error {
	p.stdin.Close()
	done := make(chan struct{})
	go func() {
		p.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		p.cmd.Process.Kill()
		<-done
	}
	if !p.pty {
		p.stdout.Close()
	}
	if p.stderr != nil {
		if msg := strings.TrimSpace(p.stderr.String()); msg != "" {
			err = fmt.Errorf("%v\n%s", err, msg)
		}
	}
	return err
}
