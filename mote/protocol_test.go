// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestPacketRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	pc := newPacketConn(struct {
		io.Reader
		io.Writer
	}{&buf, &buf})

	req := &Request{Type: "Run", Cmd: "./x", Args: []string{"./x", "-v"}, Dir: "/home/gopher",
		Files: []*File{{Path: "/home/gopher/x", Hash: strings.Repeat("ab", 32), Size: 5}}}
	if err := pc.writePacket(req, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	var got Request
	data, err := pc.readPacket(&got)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(&got, req) || string(data) != "hello" {
		t.Errorf("round trip: got %+v %q", got, data)
	}
}

func TestPacketNoJSON(t *testing.T) {
	var buf bytes.Buffer
	pc := newPacketConn(struct {
		io.Reader
		io.Writer
	}{&buf, &buf})
	if err := pc.writePacket(nil, []byte("raw")); err != nil {
		t.Fatal(err)
	}
	// First 4 bytes should encode JSON length 0.
	if binary.BigEndian.Uint32(buf.Bytes()) != 0 {
		t.Errorf("JSON length = %d, want 0", binary.BigEndian.Uint32(buf.Bytes()))
	}
	data, err := pc.readPacket(nil)
	if err != nil || string(data) != "raw" {
		t.Fatalf("readPacket = %q, %v", data, err)
	}
}

func TestPacketStream(t *testing.T) {
	var buf bytes.Buffer
	pc := newPacketConn(struct {
		io.Reader
		io.Writer
	}{&buf, &buf})
	content := strings.Repeat("data", 1000)
	if err := pc.writePacketStream(&Request{Type: "Upload"}, int64(len(content)), strings.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	var req Request
	size, body, err := pc.readPacketStream(&req)
	if err != nil || req.Type != "Upload" || size != int64(len(content)) {
		t.Fatalf("readPacketStream = %d, %+v, %v", size, req, err)
	}
	got, err := io.ReadAll(body)
	if err != nil || string(got) != content {
		t.Fatalf("body read failed: %d bytes, %v", len(got), err)
	}
}

func TestPacketJSONTooLarge(t *testing.T) {
	var buf bytes.Buffer
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:], maxJSON+1)
	buf.Write(hdr[:])
	pc := newPacketConn(struct {
		io.Reader
		io.Writer
	}{&buf, &buf})
	if _, err := pc.readPacket(nil); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("readPacket: %v, want too-large error", err)
	}
}

func TestPacketShortStream(t *testing.T) {
	var buf bytes.Buffer
	pc := newPacketConn(struct {
		io.Reader
		io.Writer
	}{&buf, &buf})
	err := pc.writePacketStream(nil, 10, strings.NewReader("short"))
	if err == nil || !strings.Contains(err.Error(), "short") {
		t.Fatalf("writePacketStream: %v, want short-stream error", err)
	}
}
