// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
)

// cmdClose implements "mote close [URL]", shutting down the background
// state kept for a server: the shared ssh connection, the local
// Tailscale daemon, or the gomote instance. With no URL, it shuts
// down all of them.
func cmdClose(args []string) {
	if len(args) == 0 {
		if err := closeAll(); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(args) != 1 {
		usage()
	}
	name := args[0]
	if name == "tail:" || name == "tail://" {
		reg, err := registeredTailName()
		if err != nil {
			log.Fatal(err)
		}
		name = "tail://" + reg
	}
	rawURL, err := resolveServer(name, "", nil)
	if err != nil {
		log.Fatal(err)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		log.Fatalf("invalid server URL %s: %v", rawURL, err)
	}
	switch u.Scheme {
	default:
		err = fmt.Errorf("unknown server URL scheme %s://", u.Scheme)
	case "tcp":
		err = errors.New("nothing to close")
	case "ssh":
		err = closeSSH(u)
	case "tail":
		err = daemonStop(u.Host)
	case "gomote":
		err = closeGomote(u.Host)
	}
	if err != nil {
		log.Fatalf("close %s: %v", rawURL, err)
	}
}

// closeAll shuts down everything mote commands may have left running:
// every shared ssh connection, every local Tailscale daemon, and every
// gomote instance mote recorded. It keeps going past failures and
// reports them together at the end.
func closeAll() error {
	var errs []error
	if home, err := os.UserHomeDir(); err == nil {
		socks, _ := filepath.Glob(filepath.Join(home, ".ssh", "sockets", "mote-*"))
		for _, sock := range socks {
			if err := closeSSHSocket(sock); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for _, name := range tailNames() {
		if err := daemonStop(name); err != nil {
			errs = append(errs, fmt.Errorf("close tail://%s: %v", name, err))
		}
	}
	// Only the recorded instances, so that a machine that has never used
	// gomote runs no gomote command at all: they are slow, because every
	// one of them is a network round trip.
	for _, builder := range gomoteBuilders() {
		if err := closeGomote(builder); err != nil {
			errs = append(errs, fmt.Errorf("close gomote://%s: %v", builder, err))
		}
	}
	return errors.Join(errs...)
}
