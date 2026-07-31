// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The Tailscale daemon.
//
// Bringing a tsnet node up takes a few seconds, and Tailscale does not
// expect nodes to come and go once per command, so mote keeps one node
// running in a background daemon, the way ssh keeps one connection in a
// ControlMaster. The first mote that needs the tailnet starts the daemon;
// later motes find it on a unix socket in the node's configuration
// directory and reuse it. The daemon exits after daemonIdleTimeout with
// nothing to do.
//
// Because the daemon owns the node, a mote client and a mote server on
// the same machine share it, which is not possible when each command
// brings up its own node.
//
// See the daemon section of protocol.md for the socket protocol.

// daemonIdleTimeout is how long the daemon stays running with no clients.
// It is a variable for testing.
var daemonIdleTimeout = 30 * time.Minute

// daemonStartTimeout is how long a client waits for a daemon it started
// to publish its socket. The first bring-up of a node is the slow one.
const daemonStartTimeout = 60 * time.Second

// daemonDialTimeout bounds one attempt to reach a server on the tailnet.
// A node that has just come up may have no path to the peer yet, and the
// connect then hangs for the operating system's whole TCP timeout, which
// is over a minute. Giving up early and trying again finds the path once
// Tailscale has established it. It is a variable for testing.
var daemonDialTimeout = 15 * time.Second

// errLocked is reported by lockFile when another process holds the lock.
var errLocked = errors.New("lock held by another process")

// tailDir returns the configuration directory for the named node,
// which holds the Tailscale credentials, the daemon's socket, lock,
// and log file.
func tailDir(name string) string {
	return filepath.Join(configDir(), "tail-"+name)
}

func servicePath(name string) string { return filepath.Join(tailDir(name), "service") }
func lockPath(name string) string    { return filepath.Join(tailDir(name), "lock") }
func logPath(name string) string     { return filepath.Join(tailDir(name), "log") }

// tailStatePath is the file where tsnet stores the node's credentials.
// Its presence means the node has been registered on the tailnet.
func tailStatePath(name string) string {
	return filepath.Join(tailDir(name), "tailscaled.state")
}

// haveTailCredentials reports whether the named node has been registered.
func haveTailCredentials(name string) bool {
	info, err := os.Stat(tailStatePath(name))
	return err == nil && info.Size() > 0
}

// A tailNet is the network a daemon serves: the tailnet in ordinary use,
// or a stand-in during testing. *tsnet.Server implements tailNet.
type tailNet interface {
	Dial(ctx context.Context, network, addr string) (net.Conn, error)
	Listen(network, addr string) (net.Listener, error)
	Close() error
}

// Client side.

// daemonConn returns a connection to the daemon for the named local node,
// starting the daemon if it is not already running.
func daemonConn(name string) (net.Conn, error) {
	if c, err := net.Dial("unix", servicePath(name)); err == nil {
		return c, nil
	}
	if err := startDaemon(name); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(daemonStartTimeout)
	for {
		c, err := net.Dial("unix", servicePath(name))
		if err == nil {
			return c, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out starting Tailscale daemon for mote-%s%s", name, daemonLogTail(name))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// startDaemon starts a daemon for the named local node in the background.
func startDaemon(name string) error {
	// The daemon has no terminal to prompt on, so register the node here
	// if it has not been registered yet.
	if err := tailLogin(name); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(tailDir(name), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(logPath(name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	// Passing -v along makes Tailscale's own logging land in the log
	// file, which is where anyone debugging the daemon will look. It
	// only takes effect when this command is the one starting it.
	args := []string{"tail-daemon", name}
	if *verbose {
		args = append([]string{"-v"}, args...)
	}
	c := exec.Command(exe, args...)
	c.Dir = configDir() // do not hold the caller's directory open
	c.Stdout = f
	c.Stderr = f
	detach(c)
	if err := c.Start(); err != nil {
		return err
	}
	// The daemon outlives this process; do not wait for it.
	return c.Process.Release()
}

// daemonLogTail returns the end of the daemon's log file, formatted for
// appending to an error message. Whatever stopped the daemon from
// starting, such as an expired auth key, is at the end of that file.
func daemonLogTail(name string) string {
	data, err := os.ReadFile(logPath(name))
	if err != nil {
		return ""
	}
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}
	return ":\n" + strings.Join(lines, "\n")
}

// daemonDial connects through the daemon for the named local node to
// addr on the tailnet, returning a connection carrying the mote protocol.
func daemonDial(name, addr string) (io.ReadWriteCloser, error) {
	// A daemon that is shutting down after its idle timeout can accept a
	// connection and then close it, so a hangup before the reply is worth
	// one retry: the second attempt starts a fresh daemon.
	for retry := 0; ; retry++ {
		conn, err := daemonConn(name)
		if err != nil {
			return nil, err
		}
		c := newConn(conn)
		var resp Response
		if err := c.writePacket(&Request{Type: "Dial", Addr: addr}, nil); err == nil {
			_, err = c.readPacket(&resp)
		}
		if err != nil {
			conn.Close()
			if retry == 0 {
				continue
			}
			return nil, fmt.Errorf("dialing %s: %v", addr, err)
		}
		if resp.Error != "" {
			conn.Close()
			return nil, errors.New(resp.Error)
		}
		if resp.Type != "Connected" {
			conn.Close()
			return nil, fmt.Errorf("dialing %s: unexpected response type %q", addr, resp.Type)
		}
		// The daemon now proxies raw bytes. A Conn does no buffering,
		// so no bytes are waiting in c; the connection can be handed on.
		return conn, nil
	}
}

// daemonServe asks the daemon for the named local node to serve the
// tailnet, and prints the daemon's log output until the connection ends.
// Hanging up tells the daemon to stop serving.
func daemonServe(name string) error {
	conn, err := daemonConn(name)
	if err != nil {
		return err
	}
	defer conn.Close()
	c := newConn(conn)
	if err := c.writePacket(&Request{Type: "Serve", Env: os.Environ()}, nil); err != nil {
		return err
	}
	for {
		var resp Response
		data, err := c.readPacket(&resp)
		if err != nil {
			return fmt.Errorf("Tailscale daemon hung up: %v", err)
		}
		if resp.Error != "" {
			return errors.New(resp.Error)
		}
		switch resp.Type {
		default:
			return fmt.Errorf("unexpected response type %q", resp.Type)
		case "Serving":
			log.Printf("serving tail://%s", name)
		case "Log":
			os.Stderr.Write(data)
		}
	}
}

// Daemon side.

// A daemon holds one Tailscale node and shares it with the mote clients
// and the mote server that connect to its socket.
type daemon struct {
	name string
	net  tailNet
	log  *logFanout

	wg sync.WaitGroup // connected clients, waited for before run returns

	mu      sync.Mutex
	active  int          // connected clients, including any mote serve
	serving bool         // a mote serve is registered
	idle    *time.Timer  // fires when active has been zero for daemonIdleTimeout
	svc     net.Listener // the service socket, closed to stop the daemon
}

// cmdTailDaemon implements the hidden "mote tail-daemon name" command,
// which mote runs in the background for itself.
func cmdTailDaemon(args []string) {
	if len(args) != 1 {
		usage()
	}
	if err := runDaemon(args[0], nil); err != nil {
		log.Fatal(err)
	}
}

// runDaemon runs the daemon for the named local node until it is idle.
// If tn is nil, the daemon brings up the node's tsnet server and takes
// over this process's logging, because the process is the daemon;
// tests pass a stand-in network instead.
func runDaemon(name string, tn tailNet) error {
	own := tn == nil
	if err := os.MkdirAll(tailDir(name), 0o700); err != nil {
		return err
	}

	// The lock names the one running daemon: two tsnet servers sharing a
	// state directory would corrupt it. The kernel drops the lock if the
	// daemon dies, so there is no stale lock to clean up.
	lock, err := lockFile(lockPath(name))
	if err == errLocked {
		return nil // another daemon is already running
	}
	if err != nil {
		return err
	}
	if lock != nil {
		defer lock.Close()
	}

	if own {
		if !haveTailCredentials(name) {
			return fmt.Errorf("no Tailscale credentials for mote-%s; run mote login tail://%s", name, name)
		}
		srv := tsnetServer(name)
		if _, err := srv.Up(context.Background()); err != nil {
			srv.Close()
			return fmt.Errorf("tailscale: %v", err)
		}
		tn = srv
	}
	defer tn.Close()

	d, err := newDaemon(name, tn)
	if err == errLocked {
		return nil // another daemon bound the socket first
	}
	if err != nil {
		return err
	}
	if own {
		log.SetOutput(d.log)
		log.Printf("serving %s for mote-%s", servicePath(name), name)
	}
	return d.run()
}

// newDaemon prepares a daemon for the named node on the network tn,
// binding the socket that clients connect to.
func newDaemon(name string, tn tailNet) (*daemon, error) {
	svc, err := listenService(name)
	if err != nil {
		return nil, err
	}
	d := &daemon{name: name, net: tn, svc: svc, log: newLogFanout(os.Stderr)}
	d.idle = time.AfterFunc(daemonIdleTimeout, d.expire)
	return d, nil
}

// run serves clients until the daemon is stopped or has been idle for
// daemonIdleTimeout.
func (d *daemon) run() error {
	defer d.svc.Close()
	defer d.idle.Stop()
	for {
		conn, err := d.svc.Accept()
		if err != nil {
			// Socket closed by expire or stop. Expiring means there
			// were no clients, but stopping can cut one off, so wait
			// before the caller tears the network down under them.
			d.wg.Wait()
			return nil
		}
		d.add(1)
		d.wg.Add(1)
		go d.client(conn)
	}
}

// stop shuts the daemon down, idle or not.
func (d *daemon) stop() { d.svc.Close() }

// listenService binds the daemon's service socket, first removing a
// socket left behind by a daemon that died. Removing it is safe because
// the caller holds the daemon lock.
func listenService(name string) (net.Listener, error) {
	dir := tailDir(name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := servicePath(name)
	if c, err := net.Dial("unix", path); err == nil {
		// Should not happen while holding the lock, but never take a
		// socket away from a daemon that is answering on it.
		c.Close()
		return nil, errLocked
	}
	os.Remove(path)
	return net.Listen("unix", path)
}

// add adjusts the count of connected clients, arming the idle timer
// when the last one goes away and disarming it when the first arrives.
func (d *daemon) add(n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.active += n
	if d.active > 0 {
		d.idle.Stop()
	} else {
		d.idle.Reset(daemonIdleTimeout)
	}
}

// expire shuts the daemon down after daemonIdleTimeout with no clients.
func (d *daemon) expire() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active > 0 {
		return // a client arrived while the timer was firing
	}
	d.svc.Close() // stops the accept loop in runDaemon
}

// client serves one connection to the daemon's socket.
func (d *daemon) client(conn net.Conn) {
	defer d.wg.Done()
	defer d.add(-1)
	c := newConn(conn)
	var req Request
	if _, err := c.readPacket(&req); err != nil {
		conn.Close()
		return
	}
	switch req.Type {
	default:
		c.writePacket(&Response{Type: "Error", Error: fmt.Sprintf("unexpected request type %q", req.Type)}, nil)
		conn.Close()
	case "Dial":
		d.dial(c, conn, &req)
	case "Serve":
		d.serve(c, conn, &req)
	}
}

// dialNet connects to addr on the tailnet, trying twice: a node that has
// just come up often has no path to the peer for the first attempt.
func (d *daemon) dialNet(addr string) (net.Conn, error) {
	var err error
	for try := range 2 {
		var ctx context.Context
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), daemonDialTimeout)
		var nc net.Conn
		nc, err = d.net.Dial(ctx, "tcp", addr)
		cancel() // governs the dial only, not the connection it returns
		if err == nil {
			return nc, nil
		}
		if try == 0 {
			log.Printf("dial %s: %v; trying again", addr, err)
		}
	}
	return nil, err
}

// dial connects to req.Addr on the tailnet and then proxies raw bytes
// between it and the client until either end hangs up.
func (d *daemon) dial(c *Conn, conn net.Conn, req *Request) {
	nc, err := d.dialNet(req.Addr)
	if err != nil {
		c.writePacket(&Response{Type: "Error", Error: fmt.Sprintf("dial %s: %v", req.Addr, err)}, nil)
		conn.Close()
		return
	}
	if err := c.writePacket(&Response{Type: "Connected"}, nil); err != nil {
		nc.Close()
		conn.Close()
		return
	}
	proxy(conn, nc)
}

// serve registers a mote server, starts listening on the tailnet, and
// waits for the mote server to hang up, which stops the listener.
func (d *daemon) serve(c *Conn, conn net.Conn, req *Request) {
	defer conn.Close()
	d.mu.Lock()
	if d.serving {
		d.mu.Unlock()
		c.writePacket(&Response{Type: "Error", Error: fmt.Sprintf("already serving tail://%s", d.name)}, nil)
		return
	}
	ln, err := d.net.Listen("tcp", fmt.Sprintf(":%d", tailPort))
	if err != nil {
		d.mu.Unlock()
		c.writePacket(&Response{Type: "Error", Error: err.Error()}, nil)
		return
	}
	d.serving = true
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		d.serving = false
		d.mu.Unlock()
		ln.Close()
		d.log.detach(c)
	}()

	if err := c.writePacket(&Response{Type: "Serving"}, nil); err != nil {
		return
	}
	// Send the daemon's log output to this mote server's terminal,
	// so that Tailscale's messages and session errors are visible.
	d.log.attach(c)
	go serveListener(ln, "", req.Env)

	// Sessions run until they finish; the mote server hanging up only
	// stops the listener. Read until then. The client sends nothing.
	for {
		if _, err := c.readPacket(new(Request)); err != nil {
			return
		}
	}
}

// proxy copies bytes between a and b until either direction ends,
// then closes both.
func proxy(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
	<-done
}

// A logFanout writes the daemon's log output to its log file and to the
// registered mote server, if there is one.
type logFanout struct {
	mu sync.Mutex
	w  io.Writer
	c  *Conn
}

func newLogFanout(w io.Writer) *logFanout { return &logFanout{w: w} }

func (l *logFanout) attach(c *Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.c = c
}

func (l *logFanout) detach(c *Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.c == c {
		l.c = nil
	}
}

func (l *logFanout) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.w.Write(p)
	if l.c != nil {
		l.c.writePacket(&Response{Type: "Log"}, p)
	}
	return len(p), nil
}
