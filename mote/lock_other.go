// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !unix

package main

import "os"

// lockFile does nothing on systems without file locks, and never
// reports errLocked. Two daemons that start at the same moment are
// still reduced to one by the socket bind in listenService, which
// fails when the socket already exists; the lock only makes the
// stale-socket cleanup before that bind safe.
func lockFile(name string) (*os.File, error) {
	return nil, nil
}
