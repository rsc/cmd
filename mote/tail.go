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
	"net/url"
	"os"
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

// tsnetServer returns a tsnet server for the given name, registering on
// the tailnet as mote-name and storing credentials in the tail-name
// configuration subdirectory. The caller must set AuthKey to register a
// node that has no credentials yet.
func tsnetServer(name string) *tsnet.Server {
	srv := &tsnet.Server{
		Hostname:      "mote-" + name,
		Dir:           tailDir(name),
		AdvertiseTags: []string{"tag:mote"}, // see the Tailscale section in doc.go
		UserLogf:      onceLogf(log.Printf), // tsnet repeats the login URL every few seconds
		Logf:          func(string, ...any) {},
	}
	if *verbose {
		srv.Logf = log.Printf
		srv.UserLogf = log.Printf
	}
	return srv
}

// tailLogin registers the named node on the tailnet if it has no
// credentials yet, prompting for a Tailscale auth key.
// The daemon runs in the background with no terminal to prompt on,
// so registration always happens in the foreground, here.
func tailLogin(name string) error {
	if haveTailCredentials(name) {
		return nil
	}
	dir := tailDir(name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Tailscale auth key: ")
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) == "" {
		return fmt.Errorf("no Tailscale auth key provided")
	}
	srv := tsnetServer(name)
	srv.AuthKey = strings.TrimSpace(sc.Text())
	defer srv.Close()
	if _, err := srv.Up(context.Background()); err != nil {
		return fmt.Errorf("tailscale: %v", err)
	}
	return nil
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

// dialTail connects to a tail://name server over Tailscale,
// through the daemon holding this machine's node.
func dialTail(u *url.URL) (io.ReadWriteCloser, error) {
	server := u.Host
	if server == "" {
		return nil, fmt.Errorf("server URL must have the form tail://name")
	}
	return daemonDial(clientTailName(), fmt.Sprintf("mote-%s:%d", server, tailPort))
}

// serveTail implements "mote serve tail://name" (or "mote serve tail:"),
// asking the daemon holding the node to serve the tailnet and printing
// the daemon's log output until interrupted.
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
	if err := daemonServe(name); err != nil {
		log.Fatal(err)
	}
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
	if err := tailLogin(u.Host); err != nil {
		log.Fatal(err)
	}
	log.Printf("logged in as mote-%s", u.Host)
}
