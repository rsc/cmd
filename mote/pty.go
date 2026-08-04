// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix

package main

import (
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/term"
)

var errUnsupported = errors.ErrUnsupported

// openPty returns the two halves of a new pseudo-terminal, with the
// terminal in raw mode: mote uses a terminal to make the program on
// the other end behave as if a person were typing, not to have one
// rewrite the bytes passing through it.
func openPty() (master, slave *os.File, err error) {
	master, slave, err = pty.Open()
	if err != nil {
		return nil, nil, err
	}
	if _, err := term.MakeRaw(int(slave.Fd())); err != nil {
		master.Close()
		slave.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

// makeStdinRaw takes standard input out of cooked mode if it is a
// terminal, so that the terminal neither echoes back what is written
// to the program nor rewrites the bytes in either direction.
// It reports whether standard input is a terminal.
func makeStdinRaw() (bool, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, nil
	}
	_, err := term.MakeRaw(int(os.Stdin.Fd()))
	return true, err
}

// ptyEOF reports whether err is a read error from a pseudo-terminal
// whose other end has been closed. Systems disagree: some report a
// plain end of file and some report EIO.
func ptyEOF(err error) bool {
	return errors.Is(err, syscall.EIO) || errors.Is(err, io.EOF)
}
