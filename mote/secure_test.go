// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"strings"
	"testing"
)

func TestSecureRoundTrip(t *testing.T) {
	cconn, sconn := net.Pipe()
	defer cconn.Close()
	defer sconn.Close()
	type result struct {
		rw  io.ReadWriteCloser
		err error
	}
	ch := make(chan result, 1)
	go func() {
		rw, err := secureServer(sconn, "s3cret")
		ch <- result{rw, err}
	}()
	crw, err := secureClient(cconn, "s3cret")
	if err != nil {
		t.Fatalf("secureClient: %v", err)
	}
	sres := <-ch
	if sres.err != nil {
		t.Fatalf("secureServer: %v", sres.err)
	}
	srw := sres.rw

	// Client to server, spanning multiple Noise messages.
	big := make([]byte, 200<<10)
	rand.Read(big)
	go func() {
		crw.Write(big)
		crw.Write([]byte("done"))
	}()
	buf := make([]byte, len(big)+4)
	if _, err := io.ReadFull(srw, buf); err != nil {
		t.Fatalf("server read: %v", err)
	}
	if !bytes.Equal(buf[:len(big)], big) || string(buf[len(big):]) != "done" {
		t.Fatalf("server read corrupted data")
	}

	// Server to client.
	go srw.Write([]byte("hello client"))
	buf = make([]byte, 12)
	if _, err := io.ReadFull(crw, buf); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(buf) != "hello client" {
		t.Fatalf("client read %q", buf)
	}
}

func TestSecureWrongPassword(t *testing.T) {
	cconn, sconn := net.Pipe()
	defer cconn.Close()
	defer sconn.Close()
	ch := make(chan error, 1)
	go func() {
		_, err := secureServer(sconn, "password1")
		ch <- err
		sconn.Close()
	}()
	_, err := secureClient(cconn, "password2")
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("secureClient: %v, want password error", err)
	}
	cconn.Close()
	if err := <-ch; err == nil {
		t.Fatalf("secureServer succeeded with mismatched password")
	}
}

func TestSecureSession(t *testing.T) {
	// Full client/server session over the encrypted channel.
	setupDirs(t)
	exit, _, stdout, _ := runPipe(t, "s3cret", nil, "/mote-test", []string{"echo", "encrypted"})
	if exit != 0 || stdout != "encrypted\n" {
		t.Errorf("exit=%d stdout=%q, want 0, %q", exit, stdout, "encrypted\n")
	}
}
