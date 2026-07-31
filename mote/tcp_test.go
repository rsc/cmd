// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
)

func TestTCPTransport(t *testing.T) {
	setupDirs(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				serve(conn, "s3cret")
			}()
		}
	}()

	rawURL := fmt.Sprintf("tcp://%s/s3cret", ln.Addr())
	rwc, password, err := dialServer(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	defer rwc.Close()
	if password != "s3cret" {
		t.Fatalf("password = %q", password)
	}
	var outb, errb bytes.Buffer
	exit, _, _, _, err := runSession(rwc, password, nil, "/mote-test", []string{"echo", "over tcp"}, &outb, &errb)
	if err != nil {
		t.Fatalf("runSession: %v", err)
	}
	if exit != 0 || outb.String() != "over tcp\n" {
		t.Errorf("exit=%d stdout=%q, want 0, %q", exit, outb.String(), "over tcp\n")
	}
}

func TestTCPPassword(t *testing.T) {
	for _, bad := range []string{"tcp://h:1", "tcp://h:1/", "tcp://h:1/a/b"} {
		u, err := url.Parse(bad)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tcpPassword(u); err == nil {
			t.Errorf("tcpPassword(%s) succeeded, want error", bad)
		}
	}
	u, _ := url.Parse("tcp://h:1/pw")
	if pw, err := tcpPassword(u); err != nil || pw != "pw" {
		t.Errorf("tcpPassword = %q, %v", pw, err)
	}
	if _, _, err := dialTCP(mustParse(t, "tcp://:0/pw")); err == nil || !strings.Contains(err.Error(), "host") {
		t.Errorf("dialTCP without host: %v, want host error", err)
	}
}

func mustParse(t *testing.T, s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
