// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// cmdGoSetup implements "mote go-setup", installing go_$GOOS_$GOARCH_exec
// hook scripts for all GOOS-GOARCH combinations that don't already have one.
func cmdGoSetup(args []string) {
	if len(args) != 0 {
		usage()
	}
	out, err := exec.Command("go", "tool", "dist", "list").Output()
	if err != nil {
		log.Fatalf("go tool dist list: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	dir := filepath.Dir(exe)
	n := 0
	for line := range strings.Lines(string(out)) {
		goos, goarch, ok := strings.Cut(strings.TrimSpace(line), "/")
		if !ok || goos == runtime.GOOS && goarch == runtime.GOARCH {
			continue
		}
		name := "go_" + goos + "_" + goarch + "_exec"
		if _, err := exec.LookPath(name); err == nil {
			continue
		}
		script := "#!/bin/sh\nexec mote -t \"$@\"\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o777); err != nil {
			log.Fatal(err)
		}
		n++
	}
	if n == 0 {
		fmt.Printf("no hooks left to install\n")
	} else {
		fmt.Printf("installed %d hooks in %s\n", n, dir)
	}
}
