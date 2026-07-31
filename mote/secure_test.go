// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
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

func TestSecureFrameLength(t *testing.T) {
	// The frame's length prefix is authenticated along with the message,
	// so a frame that is well formed except that its length prefix was
	// not the associated data must fail to decrypt.
	cconn, sconn := net.Pipe()
	defer cconn.Close()
	defer sconn.Close()
	ch := make(chan io.ReadWriteCloser, 1)
	go func() {
		rw, err := secureServer(sconn, "s3cret")
		if err != nil {
			t.Errorf("secureServer: %v", err)
		}
		ch <- rw
	}()
	crw, err := secureClient(cconn, "s3cret")
	if err != nil {
		t.Fatalf("secureClient: %v", err)
	}
	srw := <-ch
	if srw == nil {
		t.Fatal("secureServer failed")
	}
	cs, ss := crw.(*secureStream), srw.(*secureStream)

	// Move the streams off the pipe, so that frames can be written by
	// hand and the two cipher states stay in step.
	var buf bytes.Buffer
	cs.rw = bufConn{&buf}
	ss.rw = bufConn{&buf}
	if _, err := cs.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 64)
	n, err := ss.Read(p)
	if err != nil || string(p[:n]) != "hello" {
		t.Fatalf("Read = %q, %v, want %q", p[:n], err, "hello")
	}

	// Same message, same length prefix, but sealed without the length
	// prefix as associated data: the server must reject it.
	ct, err := cs.enc.Encrypt(nil, nil, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(ct)))
	buf.Write(hdr[:])
	buf.Write(ct)
	if _, err := ss.Read(p); err == nil {
		t.Fatalf("Read succeeded on frame with unauthenticated length")
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
