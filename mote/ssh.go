// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// sshControlPath is the ControlPath setting naming the socket for the
// shared ssh connection to a server.
const sshControlPath = "ControlPath ~/.ssh/sockets/mote-%r@%h-%p"

// sshDest returns the port arguments and the [user@]host destination
// for the ssh://[user@]host[:port] URL u.
func sshDest(u *url.URL) (portArgs []string, target string) {
	if port := u.Port(); port != "" {
		portArgs = append(portArgs, "-p", port)
	}
	target = u.Hostname()
	if user := u.User.Username(); user != "" {
		target = user + "@" + target
	}
	return portArgs, target
}

// dialSSH connects to an ssh://[user@]host[:port] server by running
// ssh with connection sharing enabled and "mote serve -" on the far end.
// Standard error from ssh is hidden unless an error (such as a
// handshake timeout) happens; password prompts still work, because
// ssh prints those directly to the terminal.
func dialSSH(u *url.URL) (io.ReadWriteCloser, error) {
	if home, err := os.UserHomeDir(); err == nil {
		os.MkdirAll(filepath.Join(home, ".ssh", "sockets"), 0o700)
	}
	args := []string{
		"-o", "ControlMaster auto",
		"-o", sshControlPath,
		"-o", "ControlPersist 1800",
	}
	portArgs, target := sshDest(u)
	args = append(args, portArgs...)
	args = append(args, target, "mote", "serve", "-")
	c := exec.Command("ssh", args...)
	c.Stderr = new(bytes.Buffer)
	return startProcConn(c)
}

// closeSSH shuts down the shared ssh connection to u, if one is running.
func closeSSH(u *url.URL) error {
	portArgs, target := sshDest(u)
	closed, err := sshControlExit(sshControlPath, target, portArgs)
	if err != nil {
		return err
	}
	if closed {
		log.Printf("closed shared ssh connection to %s", target)
	} else {
		log.Printf("no shared ssh connection to %s", target)
	}
	return nil
}

// closeSSHSocket shuts down the shared ssh connection whose control
// socket is the file sock (~/.ssh/sockets/mote-user@host-port).
func closeSSHSocket(sock string) error {
	// The socket path has no % expansions, so the ssh destination
	// argument, though required, goes unused.
	name := strings.TrimPrefix(filepath.Base(sock), "mote-")
	if i := strings.LastIndex(name, "-"); i > 0 {
		name = name[:i] // drop the port
	}
	closed, err := sshControlExit("ControlPath "+sock, "unused", nil)
	if err != nil {
		return fmt.Errorf("close ssh://%s: %v", name, err)
	}
	if closed {
		log.Printf("closed shared ssh connection to %s", name)
	} else {
		log.Printf("no shared ssh connection to %s", name)
	}
	return nil
}

// sshControlExit runs ssh -O exit against the control socket named by
// the ControlPath option, reporting whether there was a shared
// connection to close.
func sshControlExit(controlPath, dest string, portArgs []string) (closed bool, err error) {
	args := append([]string{"-o", controlPath, "-O", "exit"}, portArgs...)
	args = append(args, dest)
	c := exec.Command("ssh", args...)
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = &out
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(out.String())
		if strings.Contains(msg, "No such file or directory") || strings.Contains(msg, "Connection refused") {
			// The control socket does not exist or is stale.
			return false, nil
		}
		if msg != "" {
			return false, fmt.Errorf("ssh -O exit: %v\n%s", err, msg)
		}
		return false, fmt.Errorf("ssh -O exit: %v", err)
	}
	return true, nil
}
