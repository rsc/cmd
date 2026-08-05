// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"strings"
)

// checkTCPURL checks that u is a well-formed tcp://host:port URL.
// The password used to be the URL path, so a URL with a path is
// probably an old one: say where the password lives now instead of
// silently connecting with a different password than the user typed.
func checkTCPURL(u *url.URL) error {
	if strings.Trim(u.Path, "/") != "" || u.Opaque != "" {
		return fmt.Errorf("tcp server URL must have the form tcp://host:port; the password is set by mote login")
	}
	return nil
}

// tcpKey returns the password.txt key for the tcp server URL u:
// the URL without any trailing slash, so that a client and a server
// looking up the same URL agree however it was typed.
// A server listening on all interfaces uses the key tcp://:port.
func tcpKey(u *url.URL) string {
	return "tcp://" + u.Host
}

// dialTCP connects to a direct TCP server.
func dialTCP(u *url.URL) (io.ReadWriteCloser, string, error) {
	if err := checkTCPURL(u); err != nil {
		return nil, "", err
	}
	if u.Hostname() == "" || u.Port() == "" {
		return nil, "", fmt.Errorf("tcp server URL must include host and port")
	}
	password, err := lookupPassword(tcpKey(u))
	if err != nil {
		return nil, "", err
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		return nil, "", err
	}
	return conn, password, nil
}

// serveTCP implements "mote serve tcp://host:port".
func serveTCP(rawURL string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		log.Fatal(err)
	}
	if err := checkTCPURL(u); err != nil {
		log.Fatal(err)
	}
	// The key is the URL as typed, so a server listening on all
	// interfaces looks up tcp://:port, not its own host name.
	password, err := lookupPassword(tcpKey(u))
	if err != nil {
		log.Fatal(err)
	}
	ln, err := net.Listen("tcp", u.Host)
	if err != nil {
		log.Fatal(err)
	}
	host := u.Hostname()
	if host == "" {
		host, _ = os.Hostname()
	}
	port := ln.Addr().(*net.TCPAddr).Port
	log.Printf("serving tcp://%s", net.JoinHostPort(host, fmt.Sprint(port)))
	log.Fatal(serveListener(ln, password, nil))
}
