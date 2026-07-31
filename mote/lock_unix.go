// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix

package main

import (
	"os"
	"syscall"
)

// lockFile creates or opens the named file and takes an exclusive lock
// on it, returning the open file. Closing the file releases the lock,
// but the daemon holds it until it exits, so that the lock names the one
// running daemon: the kernel releases it even if the daemon crashes.
// If another process holds the lock, lockFile returns errLocked.
func lockFile(name string) (*os.File, error) {
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, errLocked
		}
		return nil, err
	}
	return f, nil
}
