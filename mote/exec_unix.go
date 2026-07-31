// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// setpgid arranges for the command to run in its own process group,
// so that killGroup can kill the command and all its children.
func setpgid(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup kills the command's process group.
func killGroup(c *exec.Cmd) {
	if c.Process != nil {
		syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
}

// detach arranges for the command to run in a new session, with no
// controlling terminal, so that it outlives the mote that started it
// and is not killed by an interrupt meant for that mote.
func detach(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
