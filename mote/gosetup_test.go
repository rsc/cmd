// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGoSetup(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(exe)
	// Put the hook directory on PATH so a second run finds the hooks.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cmdGoSetup(nil)

	m, err := filepath.Glob(filepath.Join(dir, "go_*_exec"))
	if err != nil || len(m) == 0 {
		t.Fatalf("no hooks installed: %v, %v", m, err)
	}
	host := filepath.Join(dir, "go_"+runtime.GOOS+"_"+runtime.GOARCH+"_exec")
	for _, f := range m {
		if f == host {
			t.Errorf("installed hook for host GOOS-GOARCH")
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "#!/bin/sh\nexec mote -t \"$@\"\n" {
			t.Errorf("%s: wrong contents %q", f, data)
		}
		if !strings.HasPrefix(filepath.Base(f), "go_") {
			t.Errorf("bad hook name %s", f)
		}
	}
}
