// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !unix

package main

import "os/exec"

func setpgid(c *exec.Cmd) {}

func killGroup(c *exec.Cmd) {
	if c.Process != nil {
		c.Process.Kill()
	}
}

// detach is a no-op on systems without sessions: a process started
// here already outlives its parent.
func detach(c *exec.Cmd) {}
