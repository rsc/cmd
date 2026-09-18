// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
)

func TestParseRelay(t *testing.T) {
	tests := []struct {
		spec                 string
		lport, server, rport string
		wantErr              bool
	}{
		{"5901:node:5900", "5901", "node", "5900", false},
		{"5901:ssh://kremvax:5900", "5901", "ssh://kremvax", "5900", false},
		{"5901:tcp://kremlsun:6683:5900", "5901", "tcp://kremlsun:6683", "5900", false},
		{"0:node:5900", "0", "node", "5900", false}, // any free local port
		{"5901", "", "", "", true},
		{"5901:node", "", "", "", true},
		{"5901::5900", "", "", "", true},
		{"vnc:node:5900", "", "", "", true},
		{"5901:node:70000", "", "", "", true},
	}
	for _, tt := range tests {
		lport, server, rport, err := parseRelay(tt.spec)
		if lport != tt.lport || server != tt.server || rport != tt.rport || (err != nil) != tt.wantErr {
			t.Errorf("parseRelay(%q) = %q, %q, %q, %v; want %q, %q, %q, error=%v",
				tt.spec, lport, server, rport, err, tt.lport, tt.server, tt.rport, tt.wantErr)
		}
	}
}

// echoListener starts a TCP server that writes back each line it reads,
// standing in for the service being relayed.
func echoListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	return ln
}

// TestDial checks the Dial request over an in-memory session.
func TestDial(t *testing.T) {
	setupDirs(t)
	echo := echoListener(t)
	cconn, sconn := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- serve(sconn, "", nil)
		sconn.Close()
	}()
	conn, err := clientConn(cconn, "")
	if err != nil {
		t.Fatalf("clientConn: %v", err)
	}
	rw, err := conn.Dial(echo.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if _, err := io.WriteString(rw, "hello relay\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(rw).ReadString('\n')
	if err != nil || line != "hello relay\n" {
		t.Fatalf("read %q, %v; want %q", line, err, "hello relay\n")
	}
	// Hanging up ends the session on the server.
	rw.Close()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
}

// TestDialRemoteClose checks that the client sees the end of the
// relayed connection when the server's side of it closes.
func TestDialRemoteClose(t *testing.T) {
	setupDirs(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		io.WriteString(c, "bye\n")
		c.Close()
	}()
	// A TCP session rather than net.Pipe: serveListener closes the
	// connection when serve returns, which is what the client waits for.
	sln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sln.Close()
	go serveListener(sln, "", nil)
	cconn, err := net.Dial("tcp", sln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := clientConn(cconn, "")
	if err != nil {
		t.Fatalf("clientConn: %v", err)
	}
	rw, err := conn.Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	data, err := io.ReadAll(rw)
	if err != nil || string(data) != "bye\n" {
		t.Fatalf("ReadAll = %q, %v; want %q, EOF", data, err, "bye\n")
	}
}

func TestDialRefused(t *testing.T) {
	setupDirs(t)
	// A port with nothing listening: the error comes back as the
	// session's failure.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	cconn, sconn := net.Pipe()
	go func() {
		serve(sconn, "", nil)
		sconn.Close()
	}()
	conn, err := clientConn(cconn, "")
	if err != nil {
		t.Fatalf("clientConn: %v", err)
	}
	if _, err := conn.Dial(addr); err == nil || !strings.Contains(err.Error(), "server: dial tcp") {
		t.Fatalf("Dial to closed port: %v, want server dial error", err)
	}
}

// TestRelay runs the whole relay: a local listener, a mote session per
// connection over the tcp transport, and the echo service behind it.
func TestRelay(t *testing.T) {
	setupDirs(t)
	echo := echoListener(t)
	_, rport, _ := net.SplitHostPort(echo.Addr().String())

	// The mote server, over tcp:// with a password, so that the relayed
	// bytes cross the encrypted stream.
	sln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sln.Close()
	url := fmt.Sprintf("tcp://%s", sln.Addr())
	if err := setPassword(url, "s3cret"); err != nil {
		t.Fatal(err)
	}
	go serveListener(sln, "s3cret", nil)

	// The relay's local listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go relayListener(ln, url, "localhost:"+rport)

	for i := range 2 { // each connection is its own session
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		msg := fmt.Sprintf("hello %d\n", i)
		if _, err := io.WriteString(c, msg); err != nil {
			t.Fatal(err)
		}
		line, err := bufio.NewReader(c).ReadString('\n')
		if err != nil || line != msg {
			t.Fatalf("connection %d: read %q, %v; want %q", i, line, err, msg)
		}
		c.Close()
	}
}
