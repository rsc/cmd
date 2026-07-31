// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsFileCmd(t *testing.T) {
	if isFileCmd("hostname") {
		t.Errorf("isFileCmd(hostname) = true")
	}
	if !isFileCmd("./helloworld") {
		t.Errorf("isFileCmd(./helloworld) = false")
	}
	if !isFileCmd("../testprog") {
		t.Errorf("isFileCmd(../testprog) = false")
	}
}

func TestUploadList(t *testing.T) {
	mod := t.TempDir()
	mkfile := func(name, data string) {
		name = filepath.Join(mod, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(name), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(data), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	mkfile("go.mod", "module m\n")
	mkfile("testdata/root.txt", "root")
	mkfile("a/b/testdata/deep.txt", "deep")
	mkfile("a/b/prog", "binary")
	mkfile("extra/data.txt", "extra")
	t.Chdir(filepath.Join(mod, "a", "b"))

	files, err := uploadList("./prog", []string{filepath.Join(mod, "extra")}, true)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		if !strings.HasPrefix(f.Path, filepath.ToSlash(mod)) {
			t.Errorf("path %q not under module dir", f.Path)
		}
		names = append(names, strings.TrimPrefix(f.Path, filepath.ToSlash(mod)))
		if !validHash(f.Hash) {
			t.Errorf("bad hash %q for %s", f.Hash, f.Path)
		}
	}
	want := []string{"/a/b/prog", "/extra/data.txt", "/a/b/testdata/deep.txt", "/testdata/root.txt"}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Errorf("uploadList = %v, want %v", names, want)
	}

	// Without a slash, the command is not uploaded.
	files, err = uploadList("hostname", nil, false)
	if err != nil || len(files) != 0 {
		t.Errorf("uploadList(hostname) = %v, %v; want empty", files, err)
	}
}

func TestRemotePath(t *testing.T) {
	tests := []struct {
		in   string
		want string // slash form relative to tmp
		err  bool
	}{
		{"/home/gopher/x", "home/gopher/x", false},
		{"C:/Users/gopher/x", "Users/gopher/x", false},
		{"/a/../../etc/passwd", "", true}, // .. rejected
		{"../x", "", true},                // .. rejected
		{`/a\b`, "", true},
	}
	for _, tt := range tests {
		got, err := remotePath("/tmp/mote-1", tt.in)
		if tt.err {
			if err == nil {
				t.Errorf("remotePath(%q) = %q, want error", tt.in, got)
			}
			continue
		}
		want := filepath.Join("/tmp/mote-1", filepath.FromSlash(tt.want))
		if err != nil || got != want {
			t.Errorf("remotePath(%q) = %q, %v; want %q", tt.in, got, err, want)
		}
	}
}
