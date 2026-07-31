// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"io"
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
)

// clientHandshake reads the connection preamble and server hello
// and then sends the client hello.
// See the PROTOCOL comment in doc.go.
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
// hello line, ignoring up to maxPreamble bytes of preceding text (such as
// ssh login banners). If the server sends too much text, stalls for more
// than helloTimeout, or hangs up, scanServerHello reports the text as an
// error message. It reads one byte at a time, so it never consumes bytes
// beyond the hello line's newline.
func scanServerHello(r io.Reader) error {
	type readResult struct {
		b   byte
		err error
	}
	req := make(chan struct{})
	results := make(chan readResult, 1)
	go func() {
		var buf [1]byte
		for range req {
			_, err := io.ReadFull(r, buf[:])
			results <- readResult{buf[0], err}
			if err != nil {
				return
			}
		}
	}()
	defer close(req)

	timer := time.NewTimer(helloTimeout)
	defer timer.Stop()

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
	for {
		req <- struct{}{}
		var r readResult
		select {
		case r = <-results:
		case <-timer.C:
			return fail("timeout waiting for server hello")
		}
		if r.err != nil {
			if r.err == io.EOF || r.err == io.ErrUnexpectedEOF {
				return fail("connection closed waiting for server hello")
			}
			return fail("reading server hello: %v", r.err)
		}
		if !timer.Stop() {
			<-timer.C
		}
		timer.Reset(helloTimeout)

		line = append(line, r.b)
		if r.b != '\n' {
			if len(preamble)+len(line) > maxPreamble {
				return fail("no server hello in first %d bytes", maxPreamble)
			}
			continue
		}
		if string(line) == serverHello {
			return nil
		}
		if strings.HasPrefix(string(line), helloPrefix) {
			return fmt.Errorf("connection to server is not binary safe")
		}
		preamble = append(preamble, line...)
		line = nil
	}
}

// serverHandshake sends the server hello and reads the client hello.
// See the PROTOCOL comment in doc.go.
func serverHandshake(rw io.ReadWriter) error {
	if _, err := io.WriteString(rw, serverHello); err != nil {
		return fmt.Errorf("write server hello: %v", err)
	}
	var line []byte
	var buf [1]byte
	for {
		if _, err := io.ReadFull(rw, buf[:]); err != nil {
			return fmt.Errorf("reading client hello: %v", err)
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
