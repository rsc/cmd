// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Building test binaries.

package main

import (
	"fmt"
	"strings"
	"sync"
)

// build builds all the test binaries needed for the benchmarks.
// It writes them to a .benchlab subdirectory.
func (l *Lab) build() error {
	// Using mkdir instead of os.MkdirAll for easier replacement in tests.
	if _, err := l.runLocal(0, "mkdir", "-p", ".benchlab"); err != nil {
		return err
	}

	// Don't switch to a new commit if there are pending changes.
	dirty, err := l.gitDirty()
	if err != nil {
		return err
	}
	if len(dirty) > 0 {
		return fmt.Errorf("git repo has modified files:\n\t%s", strings.Join(dirty, "\n\t"))
	}

	// Return to current git checkout when we're done.
	ref, err := l.gitCurrent()
	if err != nil {
		return err
	}
	defer func() {
		if err := l.gitCheckout(ref); err != nil {
			l.log.Print(err)
		}
	}()

	var mu sync.Mutex
	l.built = make(map[commitBuild]*exe)
	for _, commit := range l.Commits {
		if err := l.gitCheckout(commit); err != nil {
			return err
		}
		err := parDo(l, l.builds, func(b *build) error {
			exe, err := l.buildAt(commit, b)
			if err != nil {
				return err
			}
			mu.Lock()
			l.built[commitBuild{commit, b}] = exe
			mu.Unlock()
			return nil
		})
		if err != nil {
			return fmt.Errorf("builds failed")
		}
	}
	return nil
}

func (l *Lab) buildAt(commit string, b *build) (*exe, error) {
	name := ".benchlab/benchlab." + hash(commit, b.goos, b.goarch, b.env, b.flags) + ".exe"

	// Build binary.
	cmd := []string{"GOOS=" + b.goos, "GOARCH=" + b.goarch}
	cmd = append(cmd, b.env...)
	cmd = append(cmd, "go", "test", "-c", "-o", name)
	cmd = append(cmd, b.flags...)
	if l.Pkg != "" {
		cmd = append(cmd, l.Pkg)
	}
	if _, err := l.runLocal(0, cmd...); err != nil {
		return nil, err
	}

	// Fetch build ID for binary to use as key in cache.
	id, err := l.runLocal(runTrim, "go", "tool", "buildid", name)
	if err != nil {
		return nil, err
	}
	id = hash(id) // id is too long and has slashes

	return &exe{name: name, id: id}, nil
}
