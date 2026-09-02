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
	if err := l.fs.WriteFile(".benchlab/.gitignore", []byte("*\n"), 0666); err != nil {
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
	l.built = make(map[commitBuild][]*exe)
	for _, commit := range l.Commits {
		if err := l.gitCheckout(commit); err != nil {
			return err
		}
		err := parDo(l, l.builds, func(b *build) error {
			exes, err := l.buildAt(commit, b)
			if err != nil {
				return err
			}
			mu.Lock()
			l.built[commitBuild{commit, b}] = exes
			mu.Unlock()
			return nil
		})
		if err != nil {
			return fmt.Errorf("builds failed")
		}
	}
	return nil
}

// layoutSeeds returns the linker layout seeds to build for each commit.
// The seed 0 means the linker's default layout, without randomization.
//
// With randomization enabled there is one seed per rep, so that a benchmark
// runs on a differently laid out binary each time. A score that depends on
// where the linker happened to put things—a hot loop straddling a cache line,
// two hot addresses colliding in a predictor—then varies from rep to rep
// within a commit instead of masquerading as a difference between commits.
// Such an effect can be large: a 40% swing in one accumulate benchmark, on
// one machine, with identical instructions at identical alignment, is what
// prompted this.
func (l *Lab) layoutSeeds() []int {
	if !l.RandLayout {
		return []int{0}
	}
	seeds := make([]int, max(1, l.Reps))
	for i := range seeds {
		seeds[i] = i + 1
	}
	return seeds
}

// buildAt builds the test binaries for commit using build configuration b,
// one for each seed returned by layoutSeeds.
func (l *Lab) buildAt(commit string, b *build) ([]*exe, error) {
	var exes []*exe
	for _, seed := range l.layoutSeeds() {
		e, err := l.buildOne(commit, b, seed)
		if err != nil {
			return nil, err
		}
		exes = append(exes, e)
	}
	return exes, nil
}

// buildOne builds a single test binary, laid out using the given seed.
func (l *Lab) buildOne(commit string, b *build, seed int) (*exe, error) {
	flags := b.flags
	if seed != 0 {
		flags = withRandLayout(flags, seed)
	}

	// The seed reaches the name through flags, so each layout gets its own
	// binary, and an unrandomized build keeps the name it had before.
	name := ".benchlab/" + hash(commit, b.goos, b.goarch, b.env, flags) + ".exe"

	// Build binary.
	cmd := []string{"GOOS=" + b.goos, "GOARCH=" + b.goarch}
	cmd = append(cmd, b.env...)
	cmd = append(cmd, "go", "test", "-c", "-o", name)
	cmd = append(cmd, flags...)
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

	return &exe{name: name, id: id, seed: seed}, nil
}

// withRandLayout returns flags with the linker's -randlayout=seed added.
// A host configuration can set its own linker flags, as in
// “local:ldflags=-w”, so merge into that setting rather than replacing it.
func withRandLayout(flags []string, seed int) []string {
	randlayout := fmt.Sprintf("-randlayout=%d", seed)
	out := append([]string(nil), flags...)
	for i, f := range out {
		if arg, ok := strings.CutPrefix(f, "-ldflags="); ok {
			out[i] = "-ldflags=" + arg + " " + randlayout
			return out
		}
	}
	return append(out, "-ldflags="+randlayout)
}
