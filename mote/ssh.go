// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
)

// dialSSH connects to an ssh://[user@]host[:port] server by running
// ssh with connection sharing enabled and "mote serve -" on the far end.
func dialSSH(u *url.URL) (io.ReadWriteCloser, error) {
	if home, err := os.UserHomeDir(); err == nil {
		os.MkdirAll(filepath.Join(home, ".ssh", "sockets"), 0o700)
	}
	args := []string{
		"-o", "ControlMaster auto",
		"-o", "ControlPath ~/.ssh/sockets/mote-%r@%h-%p",
		"-o", "ControlPersist 1800",
	}
	if port := u.Port(); port != "" {
		args = append(args, "-p", port)
	}
	target := u.Hostname()
	if user := u.User.Username(); user != "" {
		target = user + "@" + target
	}
	args = append(args, target, "mote", "serve", "-")
	c := exec.Command("ssh", args...)
	c.Stderr = os.Stderr
	return startProcConn(c)
}
