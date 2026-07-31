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

// tcpPassword extracts the password from a tcp://host:port/password URL.
func tcpPassword(u *url.URL) (string, error) {
	password := strings.TrimPrefix(u.Path, "/")
	if password == "" || strings.Contains(password, "/") {
		return "", fmt.Errorf("tcp server URL must have the form tcp://host:port/password")
	}
	if password == "password" {
		return "", fmt.Errorf("choose a password other than %q", "password")
	}
	return password, nil
}

// dialTCP connects to a direct TCP server.
func dialTCP(u *url.URL) (io.ReadWriteCloser, string, error) {
	password, err := tcpPassword(u)
	if err != nil {
		return nil, "", err
	}
	if u.Hostname() == "" || u.Port() == "" {
		return nil, "", fmt.Errorf("tcp server URL must include host and port")
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		return nil, "", err
	}
	return conn, password, nil
}

// serveTCP implements "mote serve tcp://host:port/password".
func serveTCP(rawURL string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		log.Fatal(err)
	}
	password, err := tcpPassword(u)
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
	log.Printf("serving tcp://%s/%s", net.JoinHostPort(host, fmt.Sprint(port)), password)
	log.Fatal(serveListener(ln, password, nil))
}
