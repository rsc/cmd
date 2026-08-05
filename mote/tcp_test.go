// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
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
	if err := setPassword("tcp://"+ln.Addr().String(), "s3cret"); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				serve(conn, "s3cret", nil)
			}()
		}
	}()

	rawURL := fmt.Sprintf("tcp://%s", ln.Addr())
	conn, err := dialServer(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	runConn(t, conn, []string{"echo", "over tcp"}, "over tcp\n")
}

func TestCheckTCPURL(t *testing.T) {
	// A URL with a path is an old one, with the password in it.
	for _, bad := range []string{"tcp://h:1/pw", "tcp://h:1/a/b"} {
		if err := checkTCPURL(mustParse(t, bad)); err == nil {
			t.Errorf("checkTCPURL(%s) succeeded, want error", bad)
		}
	}
	for _, good := range []string{"tcp://h:1", "tcp://h:1/"} {
		if err := checkTCPURL(mustParse(t, good)); err != nil {
			t.Errorf("checkTCPURL(%s) = %v, want success", good, err)
		}
	}
	// The client's URL and the server's differ in host but must
	// produce the same key when the server names its own host.
	for _, tt := range []struct{ url, key string }{
		{"tcp://h:1", "tcp://h:1"},
		{"tcp://h:1/", "tcp://h:1"},
		{"tcp://:1", "tcp://:1"},
	} {
		if key := tcpKey(mustParse(t, tt.url)); key != tt.key {
			t.Errorf("tcpKey(%s) = %q, want %q", tt.url, key, tt.key)
		}
	}
}

func TestTCPPassword(t *testing.T) {
	setupDirs(t)
	if _, _, err := dialTCP(mustParse(t, "tcp://h:1")); err == nil || !strings.Contains(err.Error(), "mote login") {
		t.Errorf("dialTCP without saved password: %v, want login error", err)
	}
	if _, _, err := dialTCP(mustParse(t, "tcp://:0")); err == nil || !strings.Contains(err.Error(), "host") {
		t.Errorf("dialTCP without host: %v, want host error", err)
	}

	// Each server has its own entry, and a password may contain spaces.
	if err := setPassword("tcp://h:1", "s3cret"); err != nil {
		t.Fatal(err)
	}
	if err := setPassword("tcp://h:2", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if err := setPassword("tcp://h:1", "s3cret2"); err != nil { // replaces
		t.Fatal(err)
	}
	for _, tt := range []struct{ key, password string }{
		{"tcp://h:1", "s3cret2"},
		{"tcp://h:2", "correct horse battery staple"},
	} {
		if pw, err := lookupPassword(tt.key); err != nil || pw != tt.password {
			t.Errorf("lookupPassword(%s) = %q, %v; want %q", tt.key, pw, err, tt.password)
		}
	}
	if _, err := lookupPassword("tcp://h:3"); err == nil {
		t.Errorf("lookupPassword of unknown server succeeded, want error")
	}

	// Comments and blank lines are ignored; a line with no password is not.
	if err := os.WriteFile(passwordFile(), []byte("# comment\n\ntcp://h:1\ts3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pw, err := lookupPassword("tcp://h:1"); err != nil || pw != "s3cret" {
		t.Errorf("lookupPassword = %q, %v; want %q", pw, err, "s3cret")
	}
	for _, bad := range []string{"tcp://h:1\n", "tcp://h:1 \n"} {
		if err := os.WriteFile(passwordFile(), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readPasswords(); err == nil {
			t.Errorf("readPasswords(%q) succeeded, want malformed line error", bad)
		}
	}
}

func mustParse(t *testing.T, s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
