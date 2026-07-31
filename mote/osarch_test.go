// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBinaryOSArch(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "hello.go")
	if err := os.WriteFile(src, []byte("package main\n\nfunc main() { println(\"hi\") }\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct{ goos, goarch string }{
		{"linux", "amd64"},
		{"windows", "arm64"},
		{"darwin", "arm64"},
	} {
		bin := filepath.Join(dir, "hello-"+target.goos+"-"+target.goarch)
		c := exec.Command("go", "build", "-o", bin, src)
		c.Env = append(os.Environ(), "GOOS="+target.goos, "GOARCH="+target.goarch, "CGO_ENABLED=0", "GOFLAGS=", "GO111MODULE=off")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("go build %s/%s: %v\n%s", target.goos, target.goarch, err, out)
		}
		goos, goarch, err := binaryOSArch(bin)
		if err != nil || goos != target.goos || goarch != target.goarch {
			t.Errorf("binaryOSArch(%s-%s binary) = %s, %s, %v", target.goos, target.goarch, goos, goarch, err)
		}
	}
}
