// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Git interactions.

package main

import (
	"fmt"
	"os"
	"strings"
)

// gitDirty returns a list of dirty files in the current checkout
// that should block changing to a different commit.
// We refuse to change if there are any modified tracked files
// and also if any untracked new files end in ".go".
func (l *Lab) gitDirty() ([]string, error) {
	out, err := l.runLocal(0, "git", "status", "--porcelain")
	if err != nil {
		return nil, err
	}

	var dirty []string
	for line := range strings.Lines(out) {
		if len(line) >= 3 && (line[0] == 'M' || line[1] == 'M' ||
			strings.HasPrefix(line, "?? ") && strings.HasSuffix(line, ".go\n")) {
			// Modified file, staged or not.
			dirty = append(dirty, strings.TrimSpace(line[2:]))
		}
	}
	return dirty, nil
}

// gitResolve resolves the l.Commits list to specific commit hashes.
func (l *Lab) gitResolve() error {
	var commits []string
	for _, commit := range l.Commits {
		args := []string{"git", "rev-list", "--reverse"}
		if !strings.Contains(commit, "..") {
			args = append(args, "-n", "1")
		}
		args = append(args, commit)
		out, err := l.runLocal(0, args...)
		if err != nil {
			return fmt.Errorf("git rev-list %s: %v\n%s", commit, err, out)
		}
		for _, hash := range strings.Fields(out) {
			if len(hash) > 11 {
				hash = hash[:11]
			}
			commits = append(commits, hash)
		}
	}
	l.Commits = commits

	fmt.Fprintln(os.Stderr, "RESOLVE", l.Commits)
	return nil
}

// gitCurrent returns the current git checkout location,
// for use returning to that checkout after the builds.
func (l *Lab) gitCurrent() (string, error) {
	// Want to move back to current branch if possible, not just that commit.
	// --abbrev-ref prints a branch name or else HEAD.
	ref, err := l.runLocal(runTrim, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if ref != "HEAD" {
		return ref, err
	}

	// Not on a branch.
	// Resolve HEAD to specific commit, since HEAD will move
	// as we check out different commits.
	return l.runLocal(runTrim, "git", "rev-parse", "HEAD")
}

// gitCheckout changes to the target ref.
func (l *Lab) gitCheckout(ref string) error {
	_, err := l.runLocal(0, "git", "checkout", ref)
	return err
}
