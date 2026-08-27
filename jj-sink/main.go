// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Jj-sink moves a Jujutsu revision as far down its stack as it can go
// without introducing conflicts.
//
// Usage:
//
//	jj-sink [-m max] rev
//
// The rev can be a change id, a commit id, a bookmark, or any revset
// naming exactly one revision.
//
// Jj-sink repeatedly swaps rev with its parent, stopping before a swap
// that would introduce a conflict and never moving rev past a merge
// commit or an immutable commit. The only check is that the rebases
// apply cleanly, so a revision can still sink past a commit it depends
// on semantically: the result needs building and testing like any other
// rebase.
//
// The -m flag limits the move to at most max commits.
//
// Jj-sink collapses the rebases into a single operation, so that one
// jj undo reverts the entire sink. Collapsing discards the individual
// rebases from jj op log, leaving one operation attributed to jj-sink
// that restores the sunk state.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

var max = flag.Int("m", 0, "move rev down at most `max` commits")

func usage() {
	fmt.Fprintf(os.Stderr, "usage: jj-sink [-m max] rev\n")
	flag.PrintDefaults()
	os.Exit(2)
}

func main() {
	log.SetPrefix("jj-sink: ")
	log.SetFlags(0)

	flag.Usage = usage
	flag.Parse()
	if flag.NArg() != 1 {
		usage()
	}

	// Resolve the argument to a change id.
	// Commit ids change under every rebase below; change ids do not.
	ids := strings.Fields(query("log", "--no-graph", "-r", flag.Arg(0), "-T", `change_id.short() ++ "\n"`))
	if len(ids) != 1 {
		log.Fatalf("%s matches %d revisions, want 1", flag.Arg(0), len(ids))
	}
	rev := ids[0]

	start := op()
	conflicts := count("conflicts()")
	moved := 0
	for *max <= 0 || moved < *max {
		// Stop at a merge commit, and at an immutable commit,
		// which includes the root commit and so the bottom of
		// the stack. Swapping rev with its parent rewrites the
		// parent too, so an immutable parent is as far as rev
		// can go.
		parent := rev + "-"
		if count(parent) != 1 || count(parent+" & immutable()") != 0 {
			break
		}
		before := op()
		if _, ok := tryJJ("rebase", "-r", rev, "-B", parent); !ok {
			break
		}
		if count("conflicts()") > conflicts {
			// Undo this swap alone. Restoring by operation id avoids
			// the race that jj undo would have with any concurrently
			// running jj command.
			jj("op", "restore", before)
			break
		}
		moved++
	}

	if op() != start {
		// Collapse the rebases into a single operation,
		// so that one jj undo reverts the entire sink.
		//
		// Restore the original state and then the sunk state, so that
		// the operation left behind is an op restore. Jj describes an
		// operation by the command that made it and provides no way to
		// set that text, so without this the description would be the
		// last rebase alone, naming an intermediate commit that the
		// abandon below makes unreachable. A restore at least describes
		// every sink the same way.
		if moved > 0 {
			sunk := op()
			jj("op", "restore", start)
			jj("op", "restore", sunk)
		}
		jj("op", "abandon", start+"..@-")
	}

	if moved == 0 {
		log.Printf("%s cannot move down", rev)
		return
	}
	show(rev)
}

// op returns the id of the current operation.
func op() string {
	return query("op", "log", "-n", "1", "--no-graph", "-T", "self.id().short()")
}

// count returns the number of revisions matching revset.
func count(revset string) int {
	return len(strings.Fields(query("log", "--no-graph", "-r", revset, "-T", `"x\n"`)))
}

// show prints the log entry for rev, to report where it ended up.
func show(rev string) {
	cmd := exec.Command("jj", "log", "-r", rev)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("jj log -r %s failed: %v", rev, err)
	}
}

// query runs a jj command that only reads the repo.
// Not snapshotting the working copy keeps these commands
// from adding operations for the collapse to clean up.
func query(args ...string) string {
	return jj(append([]string{"--ignore-working-copy", "--no-pager"}, args...)...)
}

// jj runs a jj command, dying if it fails.
func jj(args ...string) string {
	out, ok := tryJJ(args...)
	if !ok {
		log.Fatalf("jj %s failed\n%s", strings.Join(args, " "), out)
	}
	return out
}

// tryJJ runs a jj command, returning its combined output
// and whether it succeeded.
//
// The operations are recorded as jj-sink's rather than the user's,
// to mark the collapsed operation in jj op log as a sink rather than
// a restore run by hand.
func tryJJ(args ...string) (string, bool) {
	cmd := exec.Command("jj", args...)
	cmd.Env = append(os.Environ(), "JJ_OP_USERNAME=jj-sink")
	out, err := cmd.CombinedOutput()
	return strings.TrimSuffix(string(out), "\n"), err == nil
}
