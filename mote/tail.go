// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"tailscale.com/tsnet"
)

// tailPort is the TCP port used for mote over Tailscale (MOTE).
const tailPort = 6683

// tailNames returns the names for which login credentials exist
// (the tail-name subdirectories of the configuration directory).
func tailNames() []string {
	var names []string
	entries, _ := os.ReadDir(configDir())
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "tail-") {
			names = append(names, strings.TrimPrefix(e.Name(), "tail-"))
		}
	}
	return names
}

// hostTailName returns the default name for this machine:
// the first element of the local host name.
func hostTailName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		log.Fatalf("cannot determine host name; use mote login tail://name")
	}
	name, _, _ := strings.Cut(host, ".")
	return name
}

// clientTailName returns the name to use for the local client:
// the host name if credentials exist for it, the single registered
// name if there is exactly one, and otherwise the host name
// (for which credentials will be established).
func clientTailName() string {
	names := tailNames()
	host := hostTailName()
	if len(names) == 1 && names[0] != host {
		return names[0]
	}
	return host
}

// registeredTailName returns the single registered name,
// for use expanding the shorthand "tail:".
func registeredTailName() (string, error) {
	names := tailNames()
	switch len(names) {
	case 0:
		return "", fmt.Errorf("not logged in to Tailscale; run mote login tail://name or mote serve tail://name")
	case 1:
		return names[0], nil
	}
	return "", fmt.Errorf("multiple Tailscale logins (%s); name one explicitly", strings.Join(names, ", "))
}

// tsnetServer returns a tsnet server for the given name,
// registering on the tailnet as mote-name and storing credentials
// in the tail-name configuration subdirectory.
// If no credentials exist yet, it prompts for a Tailscale auth key.
func tsnetServer(name string) *tsnet.Server {
	dir := filepath.Join(configDir(), "tail-"+name)
	entries, _ := os.ReadDir(dir)
	needKey := len(entries) == 0
	srv := &tsnet.Server{
		Hostname:      "mote-" + name,
		Dir:           dir,
		AdvertiseTags: []string{"tag:mote"}, // see the Tailscale section in doc.go
		UserLogf:      onceLogf(log.Printf), // tsnet repeats the login URL every few seconds
		Logf:          func(string, ...any) {},
	}
	if *verbose {
		srv.Logf = log.Printf
		srv.UserLogf = log.Printf
	}
	if needKey {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Fatal(err)
		}
		fmt.Fprintf(os.Stderr, "Tailscale auth key: ")
		sc := bufio.NewScanner(os.Stdin)
		if !sc.Scan() || strings.TrimSpace(sc.Text()) == "" {
			log.Fatal("no Tailscale auth key provided")
		}
		srv.AuthKey = strings.TrimSpace(sc.Text())
	}
	return srv
}

// onceLogf returns a logger that suppresses repeated messages.
func onceLogf(f func(string, ...any)) func(string, ...any) {
	var mu sync.Mutex
	seen := make(map[string]bool)
	return func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		mu.Lock()
		defer mu.Unlock()
		if !seen[msg] {
			seen[msg] = true
			f("%s", msg)
		}
	}
}

// tailConn is a connection over the tailnet; closing it also
// shuts down the local tsnet node.
type tailConn struct {
	net.Conn
	srv *tsnet.Server
}

func (c *tailConn) Close() error {
	err := c.Conn.Close()
	c.srv.Close()
	return err
}

// dialTail connects to a tail://name server over Tailscale.
func dialTail(u *url.URL) (io.ReadWriteCloser, error) {
	server := u.Host
	if server == "" {
		return nil, fmt.Errorf("server URL must have the form tail://name")
	}
	srv := tsnetServer(clientTailName())
	ctx := context.Background()
	if _, err := srv.Up(ctx); err != nil {
		srv.Close()
		return nil, fmt.Errorf("tailscale: %v", err)
	}
	conn, err := srv.Dial(ctx, "tcp", fmt.Sprintf("mote-%s:%d", server, tailPort))
	if err != nil {
		srv.Close()
		return nil, fmt.Errorf("dial mote-%s: %v", server, err)
	}
	return &tailConn{Conn: conn, srv: srv}, nil
}

// serveTail implements "mote serve tail://name" (or "mote serve tail:").
func serveTail(rawURL string) {
	var name string
	if rawURL == "tail:" || rawURL == "tail://" {
		var err error
		name, err = registeredTailName()
		if err != nil {
			log.Fatal(err)
		}
	} else {
		u, err := url.Parse(rawURL)
		if err != nil || u.Host == "" {
			log.Fatalf("serve URL must have the form tail://name")
		}
		name = u.Host
	}
	srv := tsnetServer(name)
	defer srv.Close()
	if _, err := srv.Up(context.Background()); err != nil {
		log.Fatalf("tailscale: %v", err)
	}
	ln, err := srv.Listen("tcp", fmt.Sprintf(":%d", tailPort))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("serving tail://%s", name)
	serveListener(ln, "")
}

// cmdLogin implements "mote login URL", establishing Tailscale
// credentials for tail://name.
func cmdLogin(args []string) {
	if len(args) != 1 {
		usage()
	}
	u, err := url.Parse(args[0])
	if err != nil || u.Scheme != "tail" || u.Host == "" {
		log.Fatalf("login URL must have the form tail://name")
	}
	srv := tsnetServer(u.Host)
	defer srv.Close()
	if _, err := srv.Up(context.Background()); err != nil {
		log.Fatalf("tailscale: %v", err)
	}
	log.Printf("logged in as mote-%s", u.Host)
}
