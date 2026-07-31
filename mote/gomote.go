// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// gomoteGroup is the gomote instance group that mote creates
// and reuses instances in.
const gomoteGroup = "mote"

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
	if _, err := gomoteOutput(exec.Command("gomote", "put", inst, bin)); err != nil {
		return nil, err
	}
	// gomote ssh runs an interactive shell on the instance; it cannot
	// pass a command the way ssh can. The session has no pty, so the
	// shell reads commands from standard input without echoing them:
	// send one line replacing the shell with the mote server, and the
	// pipes then carry the mote protocol. Anything the shell prints
	// first is preamble text that the handshake skips.
	//
	// This does not work against today's gomote ssh proxy, which
	// rejects sessions that do not allocate a pty ("scp etc not yet
	// supported"; go.dev/issue/21140) and inserts a cooked pty of its
	// own that no byte-transparent protocol can survive. Fixing that
	// requires a change to the proxy, not to mote; until then the
	// proxy's message is reported as the handshake failure.
	c := exec.Command("gomote", "ssh", inst)
	c.Stderr = new(bytes.Buffer) // hidden unless an error is reported
	p, err := startProcConn(c)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(p, "exec ./mote serve -\n"); err != nil {
		return nil, p.abort(err)
	}
	return p, nil
}

// gomoteOutput runs the gomote command c, returning its standard output.
// Standard error is hidden unless the command fails.
func gomoteOutput(c *exec.Cmd) ([]byte, error) {
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s: %v\n%s", strings.Join(c.Args, " "), err, msg)
		}
		return nil, fmt.Errorf("%s: %v", strings.Join(c.Args, " "), err)
	}
	return out, nil
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

// gomoteBuilder returns the builder type to use for the given GOOS and
// GOARCH, from the list printed by "gomote create -list": the exact
// gotip-GOOS-GOARCH if it exists, and otherwise the last (in sorted
// order) of the gotip-GOOS-GOARCH[-_]* variants, ignoring the longtest,
// race, power8, and power9 builders.
func gomoteBuilder(goos, goarch string) (string, error) {
	out, err := gomoteOutput(exec.Command("gomote", "create", "-list"))
	if err != nil {
		return "", err
	}
	want := "gotip-" + goos + "-" + goarch
	best := ""
	for line := range strings.Lines(string(out)) {
		name := strings.TrimSpace(line)
		if name == want {
			return name, nil
		}
		if len(name) <= len(want) || !strings.HasPrefix(name, want) || (name[len(want)] != '-' && name[len(want)] != '_') {
			continue
		}
		if strings.Contains(name, "-longtest") || strings.Contains(name, "-race") ||
			strings.Contains(name, "_power8") || strings.Contains(name, "_power9") {
			continue
		}
		best = max(best, name)
	}
	if best == "" {
		return "", fmt.Errorf("no gomote builder for %s-%s", goos, goarch)
	}
	return best, nil
}

// gomoteInstance returns the name of a gomote instance for the given
// builder, reusing an instance from the mote group if one is listed
// and creating one in the mote group otherwise.
func gomoteInstance(builder string) (string, error) {
	out, err := gomoteOutput(exec.Command("gomote", "list"))
	if err != nil {
		return "", err
	}
	// Lines look like "name (group1, group2)\tbuilderType\thostType\texpires ...".
	for line := range strings.Lines(string(out)) {
		f := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
		if len(f) < 2 || f[1] != builder {
			continue
		}
		name, groups, ok := strings.Cut(f[0], " (")
		if !ok {
			continue
		}
		if slices.Contains(strings.Split(strings.TrimSuffix(groups, ")"), ", "), gomoteGroup) {
			return name, nil
		}
	}

	// Create an instance in the mote group.
	// The -new-group flag creates the group but fails if it exists;
	// for an existing group, $GOMOTE_GROUP names the group to add to.
	create := exec.Command("gomote", "create", "-new-group="+gomoteGroup, builder)
	if gomoteGroupExists() {
		create = exec.Command("gomote", "create", builder)
		create.Env = append(os.Environ(), "GOMOTE_GROUP="+gomoteGroup)
	}
	out, err = gomoteOutput(create)
	if err != nil {
		return "", err
	}
	inst := strings.TrimSpace(string(out))
	if inst == "" {
		return "", fmt.Errorf("gomote create %s: no instance name in output", builder)
	}
	return inst, nil
}

// gomoteGroupExists reports whether the mote instance group exists.
func gomoteGroupExists() bool {
	out, err := gomoteOutput(exec.Command("gomote", "group", "list"))
	if err != nil {
		return false
	}
	for line := range strings.Lines(string(out)) {
		if name, _, _ := strings.Cut(line, "\t"); name == gomoteGroup {
			return true
		}
	}
	return false
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
	// If this program's source directory exists on this machine,
	// build there instead, to pick up any local changes being tested.
	if _, file, _, ok := runtime.Caller(0); ok {
		if src := filepath.Dir(file); src != "" {
			if _, err := os.Stat(filepath.Join(src, "gomote.go")); err == nil {
				c = exec.Command("go", "build", "-o", bin)
				c.Dir = src
			}
		}
	}
	c.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		os.RemoveAll(dir)
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("cross-compiling mote for %s-%s: %v\n%s", goos, goarch, err, msg)
		}
		return "", fmt.Errorf("cross-compiling mote for %s-%s: %v", goos, goarch, err)
	}
	return bin, nil
}
