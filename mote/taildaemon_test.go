// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// A fakeNet stands in for the tailnet in daemon tests: Listen returns a
// local TCP listener, and Dial connects to it, so that a daemon, a mote
// server, and a mote client can all run in one process.
type fakeNet struct {
	ln net.Listener
}

func (f *fakeNet) Listen(network, addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f.ln = ln
	return ln, nil
}

func (f *fakeNet) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if f.ln == nil {
		return nil, fmt.Errorf("nothing listening for %s", addr)
	}
	return net.Dial("tcp", f.ln.Addr().String())
}

func (f *fakeNet) Close() error {
	if f.ln != nil {
		return f.ln.Close()
	}
	return nil
}

// setupDaemonDirs points $MOTECONFIG at a short path. Unix socket names
// are limited to around 100 bytes and t.TempDir() paths are long, so the
// daemon's socket does not fit under one.
func setupDaemonDirs(t *testing.T) {
	t.Helper()
	setupDirs(t)
	dir, err := os.MkdirTemp("", "mote")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("MOTECONFIG", dir)
}

// startTestDaemon runs a daemon for the node "test" on fn,
// stopping it when the test ends.
func startTestDaemon(t *testing.T, fn *fakeNet) string {
	t.Helper()
	const name = "test"
	d, err := newDaemon(name, fn)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.run() }()
	t.Cleanup(func() {
		d.stop()
		<-done
	})
	return name
}

func TestDaemonDial(t *testing.T) {
	// A client reaches a server through the daemon: Dial, Connected,
	// and then a whole session over the proxied connection.
	setupDaemonDirs(t)
	fn := new(fakeNet)
	name := startTestDaemon(t, fn)

	// A mote server on the far side of the fake tailnet.
	ln, err := fn.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	go serveListener(ln, "", nil)

	rwc, err := daemonDial(name, "mote-far:6683")
	if err != nil {
		t.Fatalf("daemonDial: %v", err)
	}
	conn, err := clientConn(rwc, "")
	if err != nil {
		t.Fatalf("clientConn: %v", err)
	}
	defer conn.Close()
	var stdout, stderr bytes.Buffer
	w, err := conn.Run(&Exec{
		Args:   []string{"echo", "through the daemon"},
		Dir:    "/mote-test",
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if w.Code != 0 || stdout.String() != "through the daemon\n" {
		t.Errorf("code=%d stdout=%q stderr=%q", w.Code, stdout.String(), stderr.String())
	}
}

func TestDaemonStop(t *testing.T) {
	// "mote close tail://name" stops the daemon.
	setupDaemonDirs(t)
	const name = "test"
	d, err := newDaemon(name, new(fakeNet))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.run() }()
	if err := daemonStop(name); err != nil {
		t.Fatalf("daemonStop: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon still running after daemonStop")
	}
	// Stopping a daemon that is not running is not an error
	// (and must not start one).
	if err := daemonStop(name); err != nil {
		t.Fatalf("daemonStop with no daemon: %v", err)
	}
}

func TestDaemonDialNoServer(t *testing.T) {
	setupDaemonDirs(t)
	fn := new(fakeNet)
	name := startTestDaemon(t, fn)
	if _, err := daemonDial(name, "mote-nowhere:6683"); err == nil {
		t.Fatal("daemonDial succeeded with nothing listening")
	}
}

// A stalledNet never completes a dial, like a fresh node with no path
// to the peer yet.
type stalledNet struct{ fakeNet }

func (s *stalledNet) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestDaemonDialStalled(t *testing.T) {
	// A dial that stalls must give up rather than hang for the whole TCP
	// connect timeout, which is over a minute.
	setupDaemonDirs(t)
	defer func(d time.Duration) { daemonDialTimeout = d }(daemonDialTimeout)
	daemonDialTimeout = 100 * time.Millisecond

	name := "test"
	d, err := newDaemon(name, new(stalledNet))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.run() }()
	defer func() { d.stop(); <-done }()

	start := time.Now()
	if _, err := daemonDial(name, "mote-stalled:6683"); err == nil {
		t.Fatal("daemonDial succeeded against a stalled network")
	}
	// Two attempts, so twice the timeout, plus room for slow machines.
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("daemonDial gave up after %v, want about %v", d, 2*daemonDialTimeout)
	}
}

func TestDaemonServe(t *testing.T) {
	// Registering a server makes the daemon listen on the tailnet, and
	// the commands it runs use the environment from the Serve packet
	// rather than the daemon's own.
	setupDaemonDirs(t)
	fn := new(fakeNet)
	name := startTestDaemon(t, fn)

	conn, err := daemonConn(name)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := newConn(conn)
	env := append(os.Environ(), "MOTE_TEST_ENV=from the serve packet")
	if err := c.writePacket(&Request{Type: "Serve", Env: env}, nil); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if _, err := c.readPacket(&resp); err != nil || resp.Type != "Serving" {
		t.Fatalf("got %+v, %v; want Serving", resp, err)
	}

	// A client arriving over the fake tailnet, as a remote mote would.
	rc, err := fn.Dial(context.Background(), "tcp", "mote-test:6683")
	if err != nil {
		t.Fatal(err)
	}
	remote, err := clientConn(rc, "")
	if err != nil {
		t.Fatalf("clientConn: %v", err)
	}
	defer remote.Close()
	var stdout, stderr bytes.Buffer
	w, err := remote.Run(&Exec{
		Args:   []string{"sh", "-c", "echo $MOTE_TEST_ENV"},
		Dir:    "/mote-test",
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if w.Code != 0 || stdout.String() != "from the serve packet\n" {
		t.Errorf("code=%d stdout=%q stderr=%q", w.Code, stdout.String(), stderr.String())
	}
}

func TestDaemonServeTwice(t *testing.T) {
	setupDaemonDirs(t)
	fn := new(fakeNet)
	name := startTestDaemon(t, fn)

	conn, err := daemonConn(name)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := newConn(conn)
	if err := c.writePacket(&Request{Type: "Serve", Env: os.Environ()}, nil); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if _, err := c.readPacket(&resp); err != nil || resp.Type != "Serving" {
		t.Fatalf("got %+v, %v; want Serving", resp, err)
	}

	conn2, err := daemonConn(name)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	c2 := newConn(conn2)
	if err := c2.writePacket(&Request{Type: "Serve", Env: os.Environ()}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.readPacket(&resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Error, "already serving") {
		t.Fatalf("second Serve: %+v, want already serving error", resp)
	}
}

func TestDaemonStopServing(t *testing.T) {
	// Stopping the daemon ends a running "mote serve". The server is
	// parked reading from the daemon and the daemon does not exit until
	// its clients do, so without a hangup neither would ever exit.
	setupDaemonDirs(t)
	const name = "test"
	d, err := newDaemon(name, new(fakeNet))
	if err != nil {
		t.Fatal(err)
	}
	run := make(chan error, 1)
	go func() { run <- d.run() }()

	serve := make(chan error, 1)
	go func() { serve <- daemonServe(name) }()

	// Wait for the registration, so that the stop has a server to find.
	for i := 0; ; i++ {
		d.mu.Lock()
		registered := d.serveConn != nil
		d.mu.Unlock()
		if registered {
			break
		}
		if i == 500 {
			d.stop()
			t.Fatal("mote serve never registered")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := daemonStop(name); err != nil {
		t.Fatalf("daemonStop: %v", err)
	}
	select {
	case err := <-serve:
		if err != nil {
			t.Errorf("mote serve: %v, want quiet exit", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("mote serve still running after daemonStop")
	}
	select {
	case err := <-run:
		if err != nil {
			t.Fatalf("daemon run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("daemon still running after daemonStop")
	}
}

func TestDaemonIdle(t *testing.T) {
	// The daemon holds the node open while clients are connected and
	// exits once it has been idle for daemonIdleTimeout.
	setupDaemonDirs(t)
	defer func(d time.Duration) { daemonIdleTimeout = d }(daemonIdleTimeout)
	daemonIdleTimeout = 100 * time.Millisecond

	fn := new(fakeNet)
	d, err := newDaemon("test", fn)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.run() }()

	// A connected client keeps the daemon alive past the timeout.
	hold, err := net.Dial("unix", servicePath("test"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * daemonIdleTimeout)
	select {
	case <-done:
		t.Fatal("daemon exited while a client was connected")
	default:
	}

	// With the client gone it exits, and its socket stops answering.
	hold.Close()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		d.stop()
		t.Fatal("daemon did not exit when idle")
	}
	if c, err := net.Dial("unix", servicePath("test")); err == nil {
		c.Close()
		t.Error("daemon socket still answering after exit")
	}
}

func TestDaemonLock(t *testing.T) {
	// Only one daemon may run for a node: two tsnet servers sharing a
	// state directory would corrupt it.
	setupDaemonDirs(t)
	defer func(d time.Duration) { daemonIdleTimeout = d }(daemonIdleTimeout)
	daemonIdleTimeout = 100 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- runDaemon("test", new(fakeNet)) }()

	// Hold a connection open so the first daemon does not go idle
	// while the test runs.
	var hold net.Conn
	for i := 0; ; i++ {
		var err error
		if hold, err = net.Dial("unix", servicePath("test")); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("first daemon exited early: %v", err)
		default:
		}
		if i == 500 {
			t.Fatalf("first daemon never published its socket: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A second daemon exits quietly rather than fighting for the node.
	if err := runDaemon("test", new(fakeNet)); err != nil {
		t.Fatalf("second daemon: %v, want quiet exit", err)
	}
	if _, err := hold.Write([]byte{}); err != nil {
		t.Fatalf("first daemon disturbed by the second: %v", err)
	}

	hold.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("first daemon: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("first daemon did not exit when idle")
	}
}
