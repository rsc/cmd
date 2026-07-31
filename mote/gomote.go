// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// dialGomote connects to a gomote://builder server (for example
// gomote://gotip-linux-amd64), creating a gomote instance if needed,
// uploading a mote binary cross-compiled for the builder, and running
// "mote serve -" over gomote ssh.
func dialGomote(u *url.URL) (io.ReadWriteCloser, error) {
	builder := u.Host
	goos, goarch, err := builderOSArch(builder)
	if err != nil {
		return nil, err
	}
	inst, err := gomoteInstance(builder)
	if err != nil {
		return nil, err
	}
	bin, err := buildMote(goos, goarch)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(filepath.Dir(bin))
	put := exec.Command("gomote", "put", inst, bin)
	put.Stderr = os.Stderr
	if err := put.Run(); err != nil {
		return nil, fmt.Errorf("gomote put: %v", err)
	}
	c := exec.Command("gomote", "ssh", inst, "./mote", "serve", "-")
	c.Stderr = os.Stderr
	return startProcConn(c)
}

// builderOSArch extracts the GOOS and GOARCH from a builder name
// like gotip-linux-amd64 or gotip-linux-amd64-longtest.
func builderOSArch(builder string) (goos, goarch string, err error) {
	f := strings.Split(builder, "-")
	if len(f) < 3 {
		return "", "", fmt.Errorf("cannot parse GOOS-GOARCH from builder name %s", builder)
	}
	return f[1], f[2], nil
}

// gomoteInstance returns the name of a gomote instance for the given
// builder, reusing an existing instance if one is listed and
// creating one otherwise.
func gomoteInstance(builder string) (string, error) {
	out, err := exec.Command("gomote", "list").Output()
	if err != nil {
		return "", fmt.Errorf("gomote list: %v", err)
	}
	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == builder {
			return f[0], nil
		}
	}
	create := exec.Command("gomote", "create", builder)
	create.Stderr = os.Stderr
	out, err = create.Output()
	if err != nil {
		return "", fmt.Errorf("gomote create %s: %v", builder, err)
	}
	inst := strings.TrimSpace(string(out))
	if inst == "" {
		return "", fmt.Errorf("gomote create %s: no instance name in output", builder)
	}
	return inst, nil
}

// buildMote cross-compiles a mote binary for the given GOOS and GOARCH,
// returning the path to the binary in a new temporary directory.
func buildMote(goos, goarch string) (string, error) {
	dir, err := os.MkdirTemp("", "mote-build-")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "mote")
	c := exec.Command("go", "build", "-o", bin, "rsc.io/cmd/mote")
	c.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("cross-compiling mote for %s-%s: %v", goos, goarch, err)
	}
	return bin, nil
}
