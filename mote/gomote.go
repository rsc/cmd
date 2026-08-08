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
	bin, err := buildMote(goos, goarch)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(filepath.Dir(bin))
	inst, err := gomoteInstance(builder, bin)
	if err != nil {
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

// gomoteFile is the file recording the instance mote created for
// gomote://builder. Asking gomote itself which instances are mote's
// means a network round trip on every mote command, so mote writes
// down the name it created and reads it back instead.
func gomoteFile(builder string) string {
	return filepath.Join(configDir(), "gomote-"+builder)
}

// recordedGomote returns the instance recorded for gomote://builder,
// or "" if there is none. The instance may no longer exist: gomotes
// expire on their own, and the user can destroy them.
func recordedGomote(builder string) string {
	data, err := os.ReadFile(gomoteFile(builder))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// gomoteBuilders returns the builders that mote has recorded instances for.
func gomoteBuilders() []string {
	var builders []string
	entries, _ := os.ReadDir(configDir())
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "gomote-") {
			builders = append(builders, strings.TrimPrefix(e.Name(), "gomote-"))
		}
	}
	return builders
}

// closeGomote destroys the gomote instance recorded for gomote://builder.
func closeGomote(builder string) error {
	inst := recordedGomote(builder)
	if inst == "" {
		log.Printf("no gomote instance for %s", builder)
		return nil
	}
	// Forget the instance whether or not destroying it works. A destroy
	// that fails is usually one whose instance has already expired, and
	// keeping that record would fail every close from here on. The error
	// names the instance in case it is still out there.
	os.Remove(gomoteFile(builder))
	if _, err := gomoteOutput(exec.Command("gomote", "destroy", inst)); err != nil {
		return err
	}
	log.Printf("destroyed gomote %s", inst)
	return nil
}

// gomoteInstance returns a gomote instance for the given builder with
// the mote binary bin copied onto it, reusing the recorded instance if
// there is one and creating an instance otherwise.
//
// The copy is what proves a recorded instance still exists, because it
// is the first thing done with the instance: an instance that expired
// or was destroyed since mote wrote it down fails here, and the record
// gives way to a new instance.
func gomoteInstance(builder, bin string) (string, error) {
	if inst := recordedGomote(builder); inst != "" {
		_, err := gomoteOutput(exec.Command("gomote", "put", inst, bin))
		if err == nil {
			return inst, nil
		}
		log.Printf("gomote %s is gone; creating another: %v", inst, err)
		os.Remove(gomoteFile(builder))
	}
	inst, err := createGomote(builder)
	if err != nil {
		return "", err
	}
	if _, err := gomoteOutput(exec.Command("gomote", "put", inst, bin)); err != nil {
		return "", err
	}
	return inst, nil
}

// createGomote creates a gomote instance for the given builder and
// records it, so that later mote commands reuse it and "mote close"
// can destroy it.
func createGomote(builder string) (string, error) {
	// Create an instance in the mote group.
	// The -new-group flag creates the group but fails if it exists;
	// for an existing group, $GOMOTE_GROUP names the group to add to.
	// The group is for the user's benefit: mote finds its own instances
	// by the names it records, not by the group.
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
	if err := os.WriteFile(gomoteFile(builder), []byte(inst+"\n"), 0o666); err != nil {
		// The instance is running and will go unrecorded: the next mote
		// command creates another, and "mote close" cannot destroy this
		// one. Say so; there is nothing to do but keep using it.
		log.Printf("recording gomote %s: %v", inst, err)
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
