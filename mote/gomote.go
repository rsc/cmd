// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
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
// the mote server on the instance over gomote ssh.
//
// There are two ways to start the server, and which ones work depends
// on the versions of the gomote command and the ssh proxy in use, so
// dialGomote tries the good one and falls back to the one that works
// everywhere. Only a completed handshake proves that a way worked,
// which is why this transport runs the handshake itself.
func dialGomote(u *url.URL) (*Conn, error) {
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
	conn, cmdErr := gomoteCommand(inst)
	if cmdErr == nil {
		return conn, nil
	}
	conn, shellErr := gomoteShell(inst)
	if shellErr == nil {
		return conn, nil
	}
	return nil, fmt.Errorf("gomote ssh %s %s: %v\ngomote ssh %s (shell): %v",
		inst, moteServe, cmdErr, inst, shellErr)
}

// moteServe and moteServeHex are the commands that start the mote
// server on a gomote instance, where "gomote put" left the binary in
// the home directory: the plain server for a connection that carries
// bytes unchanged, and the hex server for one that does not.
const (
	moteServe    = "./mote serve -"
	moteServeHex = "./mote serve -hex-"
)

// gomoteCommand connects to the mote server on inst by handing the
// server command to gomote ssh, which runs it without a terminal, so
// the connection carries the protocol byte for byte.
//
// This is the way in that keeps no terminal in the path, and the one
// that older software cannot do: gomote commands that take only an
// instance name reject the command outright, and ssh proxies that
// serve only interactive sessions reject one that asks for no terminal
// ("scp etc not yet supported"; go.dev/issue/21140). Either failure
// arrives here as a failed handshake, and the caller tries the shell.
func gomoteCommand(inst string) (*Conn, error) {
	c := exec.Command("gomote", "ssh", inst, moteServe)
	c.Stderr = new(bytes.Buffer) // hidden unless an error is reported
	p, err := startProcConn(c)
	if err != nil {
		return nil, err
	}
	conn, err := clientConn(p, "")
	if err != nil {
		return nil, p.abort(err)
	}
	return conn, nil
}

// gomoteShell connects to the mote server on inst the way a person
// would: it runs gomote ssh with no command, waits for the shell, and
// types a command line that replaces the shell with the mote server.
//
// The shell only appears if ssh asks the far end for a terminal, and
// ssh only asks when it has a terminal of its own, so mote gives it
// one. That terminal and the ones the ssh proxy adds along the way
// rewrite newlines and echo back what they read, so the server runs in
// hex, which survives all of it. Anything the shell printed before the
// hex handshake is preamble, including the echo of the command line.
func gomoteShell(inst string) (*Conn, error) {
	c := exec.Command("gomote", "ssh", inst)
	c.Stderr = new(bytes.Buffer) // hidden unless an error is reported
	p, err := startPtyConn(c)
	if errors.Is(err, errors.ErrUnsupported) {
		// No terminals on this system. Plain pipes still reach a proxy
		// that accepts sessions without one.
		p, err = startProcConn(c)
	}
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(p, "exec "+moteServeHex+"\n"); err != nil {
		return nil, p.abort(err)
	}
	if err := scanHexHandshake(p); err != nil {
		return nil, p.abort(err)
	}
	conn, err := clientConn(newHexConn(p), "")
	if err != nil {
		return nil, p.abort(err)
	}
	return conn, nil
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
// like gotip-linux-amd64, gotip-linux-amd64-longtest, or
// gotip-linux-amd64_c4dh96-perf_vs_release.
func builderOSArch(builder string) (goos, goarch string, err error) {
	f := strings.Split(builder, "-")
	if len(f) < 3 {
		return "", "", fmt.Errorf("cannot parse GOOS-GOARCH from builder name %s", builder)
	}
	goos, goarch = f[1], f[2]
	if prefix, _, hasRunMod := strings.Cut(goarch, "_"); hasRunMod {
		goarch = prefix
	}
	return goos, goarch, nil
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

// gomoteInstances returns the gomote instances in the mote group
// that have the given builder type ("" for all of them).
func gomoteInstances(builder string) ([]string, error) {
	out, err := gomoteOutput(exec.Command("gomote", "list"))
	if err != nil {
		return nil, err
	}
	// Lines look like "name (group1, group2)\tbuilderType\thostType\texpires ...".
	var insts []string
	for line := range strings.Lines(string(out)) {
		f := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
		if len(f) < 2 || (builder != "" && f[1] != builder) {
			continue
		}
		name, groups, ok := strings.Cut(f[0], " (")
		if !ok {
			continue
		}
		if slices.Contains(strings.Split(strings.TrimSuffix(groups, ")"), ", "), gomoteGroup) {
			insts = append(insts, name)
		}
	}
	return insts, nil
}

// closeGomote destroys the gomote instances backing gomote://builder.
func closeGomote(u *url.URL) error {
	insts, err := gomoteInstances(u.Host)
	if err != nil {
		return err
	}
	if len(insts) == 0 {
		log.Printf("no gomote instance for %s", u.Host)
		return nil
	}
	return destroyGomotes(insts)
}

// destroyGomotes destroys the given gomote instances.
func destroyGomotes(insts []string) error {
	for _, inst := range insts {
		if _, err := gomoteOutput(exec.Command("gomote", "destroy", inst)); err != nil {
			return err
		}
		log.Printf("destroyed gomote %s", inst)
	}
	return nil
}

// gomoteInstance returns the name of a gomote instance for the given
// builder, reusing an instance from the mote group if one is listed
// and creating one in the mote group otherwise.
func gomoteInstance(builder string) (string, error) {
	insts, err := gomoteInstances(builder)
	if err != nil {
		return "", err
	}
	if len(insts) > 0 {
		return insts[0], nil
	}

	// Create an instance in the mote group.
	// The -new-group flag creates the group but fails if it exists;
	// for an existing group, $GOMOTE_GROUP names the group to add to.
	create := exec.Command("gomote", "create", "-new-group="+gomoteGroup, builder)
	if gomoteGroupExists() {
		create = exec.Command("gomote", "create", builder)
		create.Env = append(os.Environ(), "GOMOTE_GROUP="+gomoteGroup)
	}
	out, err := gomoteOutput(create)
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
