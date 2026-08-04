// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !unix

package main

import (
	"errors"
	"io"
	"os"
)

var errUnsupported = errors.ErrUnsupported

// openPty reports that this system has no pseudo-terminals to open.
// The transports that want one fall back to plain pipes.
func openPty() (master, slave *os.File, err error) {
	return nil, nil, errUnsupported
}

// makeStdinRaw reports that standard input is not a terminal,
// there being no terminals here to put into raw mode.
func makeStdinRaw() (bool, error) { return false, nil }

// ptyEOF reports whether err ends a read from a pseudo-terminal.
func ptyEOF(err error) bool { return errors.Is(err, io.EOF) }
