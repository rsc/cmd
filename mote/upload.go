// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// uploadList returns the list of files to upload: the command itself
// if it names a file (contains a slash), the -u paths, and, if testdata
// is true, the testdata directories from the current directory up to
// the Go module root.
func uploadList(cmdName string, extra []string, testdata bool) ([]*File, error) {
	var files []*File
	if isFileCmd(cmdName) {
		if err := addFile(&files, cmdName); err != nil {
			return nil, err
		}
	}
	for _, p := range extra {
		if err := addTree(&files, p); err != nil {
			return nil, err
		}
	}
	if testdata {
		dir, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		for {
			td := filepath.Join(dir, "testdata")
			if info, err := os.Stat(td); err == nil && info.IsDir() {
				if err := addTree(&files, td); err != nil {
					return nil, err
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return files, nil
}

// isFileCmd reports whether the command name refers to a file
// to be uploaded, as opposed to a command found on the remote PATH.
func isFileCmd(name string) bool {
	return strings.Contains(name, "/") || strings.Contains(name, string(filepath.Separator))
}

// cmdFile returns the file named by the command name, supplying the
// .exe suffix that a Windows executable has and the name does not:
// “mote ./strings” runs ./strings.exe, and “mote ./mypkg.test” runs
// ./mypkg.test.exe, which is what “go test -c” writes when it builds
// for Windows.
//
// The suffix belongs to the file, not to this machine: the binary is
// built for Windows on whatever system cross-compiles it, so the
// suffix is supplied wherever mote runs. An exact match always wins,
// so a file really named strings is still the one that “mote
// ./strings” means. A name that matches nothing is returned unchanged,
// leaving the upload to report the file as missing.
func cmdFile(name string) string {
	if isFileCmd(name) && !isRegular(name) && isRegular(name+".exe") {
		return name + ".exe"
	}
	return name
}

// isRegular reports whether name is an ordinary file.
func isRegular(name string) bool {
	info, err := os.Stat(name)
	return err == nil && info.Mode().IsRegular()
}

// addTree adds the file or directory tree rooted at name to files.
func addTree(files *[]*File, name string) error {
	info, err := os.Stat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return addFile(files, name)
	}
	return filepath.WalkDir(name, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			return addFile(files, path)
		}
		return nil
	})
}

// addFile adds the single file name to files,
// computing its SHA-256 hash and recording its absolute slash-form path.
func addFile(files *[]*File, name string) error {
	abs, err := filepath.Abs(name)
	if err != nil {
		return err
	}
	f, err := os.Open(abs)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	*files = append(*files, &File{
		Path: filepath.ToSlash(abs),
		Hash: hex.EncodeToString(h.Sum(nil)),
		Size: size,
	})
	return nil
}

// A lazyFile is an io.Reader that opens the named file at first Read
// and closes it at EOF, so that a long upload list does not hold
// many open files at once.
type lazyFile struct {
	name string
	f    *os.File
}

func (l *lazyFile) Read(p []byte) (int, error) {
	if l.f == nil {
		f, err := os.Open(l.name)
		if err != nil {
			return 0, err
		}
		l.f = f
	}
	n, err := l.f.Read(p)
	if err == io.EOF {
		l.f.Close()
	}
	return n, err
}
