// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"debug/pe"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestExeName checks the .exe suffix that a Windows server adds to the
// executables it is sent. The server it is testing is a Windows one
// whatever system runs the test, so exeName takes the GOOS as an
// argument rather than reading runtime.GOOS.
func TestExeName(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "hello.go")
	if err := os.WriteFile(src, []byte("package main\n\nfunc main() { println(\"hi\") }\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	build := func(goos string) string {
		bin := filepath.Join(dir, "hello-"+goos)
		c := exec.Command("go", "build", "-o", bin, src)
		c.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0", "GOFLAGS=", "GO111MODULE=off")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", goos, err, out)
		}
		return bin
	}
	windows := build("windows")
	linux := build("linux")
	library := markDLL(t, windows, filepath.Join(dir, "hello-dll"))
	text := filepath.Join(dir, "text")
	if err := os.WriteFile(text, []byte("not a binary\n"), 0o666); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		goos string
		dst  string
		src  string
		want string
	}{
		{"windows", "prog", windows, "prog.exe"},
		// A test binary's name ends in .test, which Windows does not
		// consider executable either.
		{"windows", "strings.test", windows, "strings.test.exe"},
		{"windows", "prog.exe", windows, "prog.exe"},
		{"windows", "PROG.EXE", windows, "PROG.EXE"},
		// Only programs: a library is loaded by name, and anything that
		// is not a Windows binary is not a program to be run.
		{"windows", "hello.dll", library, "hello.dll"},
		{"windows", "testdata/input", text, "testdata/input"},
		{"windows", "prog", linux, "prog"},
		// Every other system runs a file under the name it arrived with.
		{"linux", "prog", windows, "prog"},
		{"darwin", "prog", linux, "prog"},
	}
	for _, tt := range tests {
		if got := exeName(tt.goos, tt.dst, tt.src); got != tt.want {
			t.Errorf("exeName(%s, %q, %s) = %q, want %q",
				tt.goos, tt.dst, filepath.Base(tt.src), got, tt.want)
		}
	}
}

// TestClientPath checks which command names name an uploaded file.
func TestClientPath(t *testing.T) {
	tests := []struct{ dir, name, want string }{
		{"/home/rsc/mypkg", "/tmp/go-build/b001/mypkg.test", "/tmp/go-build/b001/mypkg.test"},
		{"/home/rsc/mypkg", "./mypkg.test", "/home/rsc/mypkg/mypkg.test"},
		{"/home/rsc/mypkg", "../testprog", "/home/rsc/testprog"},
		// A Windows client sends native paths, absolute ones with a volume.
		{"C:/Users/rsc/mypkg", `C:\Users\rsc\mypkg\mypkg.test`, "C:/Users/rsc/mypkg/mypkg.test"},
		{"C:/Users/rsc/mypkg", `.\mypkg.test`, "C:/Users/rsc/mypkg/mypkg.test"},
		// A bare command name is not a file the client uploaded: mote
		// uploads a command only when it names a path. The server finds
		// this one on its own PATH.
		{"/home/rsc/mypkg", "hostname", ""},
	}
	for _, tt := range tests {
		if got := clientPath(tt.dir, tt.name); got != tt.want {
			t.Errorf("clientPath(%q, %q) = %q, want %q", tt.dir, tt.name, got, tt.want)
		}
	}
}

// markDLL copies the Windows binary src to dst, setting the flag that
// marks it a library rather than a program, and returns dst.
func markDLL(t *testing.T, src, dst string) string {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	// Characteristics is the last field of the 20-byte COFF header,
	// which follows the "PE\0\0" signature at the offset named at 0x3c.
	off := int(binary.LittleEndian.Uint32(data[0x3c:])) + 4 + 18
	binary.LittleEndian.PutUint16(data[off:], binary.LittleEndian.Uint16(data[off:])|pe.IMAGE_FILE_DLL)
	if err := os.WriteFile(dst, data, 0o666); err != nil {
		t.Fatal(err)
	}
	return dst
}
