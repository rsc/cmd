// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// A scriptReader replays a scripted sequence of delays and data chunks,
// then returns EOF. It implements SetReadDeadline so that the
// handshake stall timeout can be tested with synctest's fake clock.
type scriptStep struct {
	delay time.Duration
	data  string
}

type scriptReader struct {
	steps    []scriptStep
	cur      string
	deadline time.Time
}

func (r *scriptReader) SetReadDeadline(t time.Time) error {
	r.deadline = t
	return nil
}

func (r *scriptReader) Read(p []byte) (int, error) {
	for r.cur == "" {
		if len(r.steps) == 0 {
			return 0, io.EOF
		}
		step := r.steps[0]
		if !r.deadline.IsZero() && time.Now().Add(step.delay).After(r.deadline) {
			time.Sleep(time.Until(r.deadline))
			return 0, os.ErrDeadlineExceeded
		}
		r.steps = r.steps[1:]
		time.Sleep(step.delay)
		r.cur = step.data
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

func TestScanServerHello(t *testing.T) {
	tests := []struct {
		name  string
		steps []scriptStep
		err   string // "" for success
	}{
		{"immediate", []scriptStep{{0, serverHello}}, ""},
		{"banner", []scriptStep{{0, "Welcome to kremvax.\nNo access.\n"}, {time.Second, serverHello}}, ""},
		{"crlf", []scriptStep{{0, "mote server hello \x00\x01\xfe\xff\r\n"}}, "not binary safe"},
		{"mangled", []scriptStep{{0, "mote server hello \x01\xfe\xff\n"}}, "not binary safe"},
		{"eof", []scriptStep{{0, "permission denied\n"}}, "permission denied"},
		{"stall", []scriptStep{{0, "starting up\n"}, {2 * time.Minute, serverHello}}, "timeout"},
		{"toolong", []scriptStep{{0, strings.Repeat("x", 70<<10)}}, "no server hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				err := scanServerHello(&scriptReader{steps: tt.steps})
				if tt.err == "" {
					if err != nil {
						t.Fatalf("scanServerHello: %v, want success", err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("scanServerHello: %v, want error containing %q", err, tt.err)
				}
			})
		})
	}
}

func TestServeHandshakeTimeout(t *testing.T) {
	// A peer that connects and then stalls must be disconnected instead
	// of tying up a connection and a goroutine indefinitely: until the
	// handshake finishes it has not proved it knows the password.
	synctest.Test(t, func(t *testing.T) {
		cconn, sconn := net.Pipe()
		defer cconn.Close()
		defer sconn.Close()
		start := time.Now()
		done := make(chan error, 1)
		go func() { done <- serve(sconn, "s3cret", nil) }()

		// Read the server hello but never answer it.
		hello := make([]byte, len(serverHello))
		if _, err := io.ReadFull(cconn, hello); err != nil {
			t.Fatalf("reading server hello: %v", err)
		}
		err := <-done
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("serve: %v, want deadline exceeded", err)
		}
		if d := time.Since(start); d != handshakeTimeout {
			t.Errorf("serve gave up after %v, want %v", d, handshakeTimeout)
		}
	})
}

func TestServeHandshakeDeadlineCleared(t *testing.T) {
	// Once the handshake is done the deadline must be cleared on both
	// sides, because the commands that follow can run for any length
	// of time. Idle past the deadline and then use the session.
	synctest.Test(t, func(t *testing.T) {
		cconn, sconn := net.Pipe()
		defer cconn.Close()
		defer sconn.Close()
		done := make(chan error, 1)
		go func() { done <- serve(sconn, "s3cret", nil) }()
		conn, err := clientConn(cconn, "s3cret")
		if err != nil {
			t.Fatalf("clientConn: %v", err)
		}
		time.Sleep(2 * handshakeTimeout)

		// A bogus request is enough to show the session still works,
		// and ends it without running a command.
		if err := conn.writePacket(&Request{Type: "Nonsense"}, nil); err != nil {
			t.Fatalf("writePacket: %v", err)
		}
		var resp Response
		if _, err := conn.readPacket(&resp); err != nil {
			t.Fatalf("reading response: %v", err)
		}
		if !strings.Contains(resp.Error, "unexpected request type") {
			t.Errorf("response Error = %q, want unexpected request type", resp.Error)
		}
		<-done
	})
}

// pipeRW adapts separate read and write streams into an io.ReadWriter.
type pipeRW struct {
	io.Reader
	io.Writer
}

func TestHandshake(t *testing.T) {
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	client := pipeRW{cr, cw}
	server := pipeRW{sr, sw}
	done := make(chan error, 1)
	go func() { done <- serverHandshake(server) }()
	if err := clientHandshake(client); err != nil {
		t.Fatalf("clientHandshake: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("serverHandshake: %v", err)
	}
}

func TestServerHandshakeBadClient(t *testing.T) {
	sr, cw := io.Pipe()
	server := pipeRW{sr, io.Discard}
	done := make(chan error, 1)
	go func() { done <- serverHandshake(server) }()
	io.WriteString(cw, "mote client hello \xff\xfe\x01\x00\r\n")
	if err := <-done; err == nil || !strings.Contains(err.Error(), "not binary safe") {
		t.Fatalf("serverHandshake: %v, want binary safety error", err)
	}
}
