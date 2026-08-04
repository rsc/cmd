// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const (
	serverHello = "mote server hello \x00\x01\xfe\xff\n"
	clientHello = "mote client hello \xff\xfe\x01\x00\n"

	// helloPrefix is the text part of the server hello, used to detect
	// a hello that arrived over a non-binary-safe connection.
	helloPrefix = "mote server hello "

	// maxPreamble is the maximum amount of text (login banners and other
	// output) tolerated before the server hello.
	maxPreamble = 64 << 10

	// helloTimeout is how long the client waits for more preamble text
	// before giving up on seeing a server hello.
	helloTimeout = 60 * time.Second

	// handshakeTimeout is how long a peer has to complete the hello
	// exchange and the encryption handshake, which move a small, fixed
	// number of bytes. A peer that takes longer is stalling, and until
	// the handshake finishes it has not proved it knows the password.
	handshakeTimeout = 60 * time.Second
)

// A deadlineReader is a reader with read deadlines,
// as implemented by net.Conn and *os.File.
type deadlineReader interface {
	SetReadDeadline(time.Time) error
}

// A deadlineConn is a connection with read and write deadlines,
// as implemented by net.Conn. Connections that are pipes to another
// process (the ssh and gomote transports) are not deadlineConns:
// they are already authenticated by the transport.
type deadlineConn interface {
	SetDeadline(time.Time) error
}

// clientHandshake reads the connection preamble and server hello
// and then sends the client hello.
// See protocol.md.
func clientHandshake(rw io.ReadWriter) error {
	if err := scanServerHello(rw); err != nil {
		return err
	}
	if _, err := io.WriteString(rw, clientHello); err != nil {
		return fmt.Errorf("write client hello: %v", err)
	}
	return nil
}

// scanServerHello scans the bytes coming from the server for the server
// hello line.
func scanServerHello(r io.Reader) error {
	return scanHello(r, "server hello", func(line string) (bool, error) {
		if line == serverHello {
			return true, nil
		}
		if strings.HasPrefix(line, helloPrefix) {
			// The text arrived but the binary bytes around it did not.
			return false, fmt.Errorf("connection to server is not binary safe")
		}
		return false, nil
	})
}

// scanHello scans the bytes coming from the server for the hello line
// that match accepts, ignoring up to maxPreamble bytes of preceding
// text (such as ssh login banners). If the server sends too much text,
// stalls for more than helloTimeout, or hangs up, scanHello reports the
// text as an error message, naming what it was waiting for. It reads one
// byte at a time, so it never consumes bytes beyond the hello line's
// newline.
//
// The connection is a network connection or a pipe (*os.File), both of
// which implement read deadlines; the stall timeout uses those.
// If r has no working SetReadDeadline, scanHello reads without
// a timeout.
func scanHello(r io.Reader, what string, match func(line string) (bool, error)) error {
	rd, _ := r.(deadlineReader)
	if rd != nil {
		if err := rd.SetReadDeadline(time.Now().Add(helloTimeout)); err != nil {
			rd = nil
		} else {
			defer rd.SetReadDeadline(time.Time{})
		}
	}

	var preamble, line []byte
	fail := func(format string, args ...any) error {
		msg := strings.TrimSpace(string(preamble) + string(line))
		if len(msg) > 2048 {
			msg = msg[:2048] + "..."
		}
		if msg != "" {
			return fmt.Errorf(format+": server said:\n%s", append(args, msg)...)
		}
		return fmt.Errorf(format, args...)
	}
	var buf [1]byte
	for {
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return fail("timeout waiting for %s", what)
			}
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return fail("connection closed waiting for %s", what)
			}
			return fail("reading %s: %v", what, err)
		}
		if rd != nil {
			rd.SetReadDeadline(time.Now().Add(helloTimeout))
		}

		line = append(line, buf[0])
		if buf[0] != '\n' {
			if len(preamble)+len(line) > maxPreamble {
				return fail("no %s in first %d bytes", what, maxPreamble)
			}
			continue
		}
		ok, err := match(string(line))
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		preamble = append(preamble, line...)
		line = nil
	}
}

// serverHandshake sends the server hello and reads the client hello.
// See protocol.md.
func serverHandshake(rw io.ReadWriter) error {
	if _, err := io.WriteString(rw, serverHello); err != nil {
		return fmt.Errorf("write server hello: %v", err)
	}
	var line []byte
	var buf [1]byte
	for {
		if _, err := io.ReadFull(rw, buf[:]); err != nil {
			return fmt.Errorf("reading client hello: %w", err)
		}
		line = append(line, buf[0])
		if buf[0] == '\n' {
			break
		}
		if len(line) > len(clientHello) {
			return fmt.Errorf("malformed client hello")
		}
	}
	if string(line) != clientHello {
		if strings.HasPrefix(string(line), "mote client hello ") {
			return fmt.Errorf("connection to client is not binary safe")
		}
		return fmt.Errorf("malformed client hello")
	}
	return nil
}
