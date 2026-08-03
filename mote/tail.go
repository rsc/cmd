// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// tailPort is the TCP port used for mote over Tailscale (MOTE).
const tailPort = 6683

// tailNames returns the names mote has a node directory for
// (the tail-name subdirectories of the configuration directory).
func tailNames() []string {
	var names []string
	entries, _ := os.ReadDir(configDir())
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "tail-") {
			names = append(names, strings.TrimPrefix(e.Name(), "tail-"))
		}
	}
	return names
}

// loggedInTailNames returns the names this machine is logged in as.
// A node directory can exist without credentials in it: bringing a node
// up creates the directory before the registration that fills it in, so
// an abandoned or failed registration leaves one behind. Such a
// directory is not a login and must not be mistaken for one.
func loggedInTailNames() []string {
	var names []string
	for _, name := range tailNames() {
		if haveTailCredentials(name) {
			names = append(names, name)
		}
	}
	return names
}

// hostTailName returns the default name for this machine:
// the first element of the local host name.
func hostTailName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		log.Fatalf("cannot determine host name; use mote login tail://name")
	}
	name, _, _ := strings.Cut(host, ".")
	return name
}

// clientTailName returns the name to use for the local client:
// the host name if this machine is logged in as it, the single name it
// is logged in as if there is exactly one, and otherwise the host name
// (for which credentials will be established).
// An existing login comes first because registering is the expensive,
// visible operation: a machine that has run “mote login tail://name”
// or “mote serve tail://name” should not add a second node to the
// tailnet just to run a command.
func clientTailName() (string, error) {
	names := loggedInTailNames()
	host := hostTailName()
	if len(names) == 0 || slices.Contains(names, host) {
		return host, nil
	}
	if len(names) == 1 {
		return names[0], nil
	}
	return "", fmt.Errorf("multiple Tailscale logins (%s); run mote login tail://%s to use this host name", strings.Join(names, ", "), host)
}

// registeredTailName returns the single registered name,
// for use expanding the shorthand "tail:".
func registeredTailName() (string, error) {
	names := loggedInTailNames()
	switch len(names) {
	case 0:
		return "", fmt.Errorf("not logged in to Tailscale; run mote login tail://name or mote serve tail://name")
	case 1:
		return names[0], nil
	}
	return "", fmt.Errorf("multiple Tailscale logins (%s); name one explicitly", strings.Join(names, ", "))
}

// tsnetServer returns a tsnet server for the given name, registering on
// the tailnet as mote-name and storing credentials in the tail-name
// configuration subdirectory. The caller must set AuthKey to register a
// node that has no credentials yet.
func tsnetServer(name string) *tsnet.Server {
	srv := &tsnet.Server{
		Hostname:      "mote-" + name,
		Dir:           tailDir(name),
		AdvertiseTags: []string{"tag:mote"}, // see the Tailscale section in doc.go
		UserLogf:      onceLogf(log.Printf), // tsnet repeats the login URL every few seconds
		Logf:          func(string, ...any) {},
	}
	if *verbose {
		srv.Logf = log.Printf
		srv.UserLogf = log.Printf
	}
	return srv
}

// tailLogin registers the named node on the tailnet if it has no
// credentials yet, prompting for a Tailscale auth key.
// The daemon runs in the background with no terminal to prompt on,
// so registration always happens in the foreground, here.
func tailLogin(name string) error {
	if haveTailCredentials(name) {
		return nil
	}
	// Prompt before creating the node directory, which tsnet does itself:
	// an abandoned prompt should leave nothing behind that later looks
	// like a login. See loggedInTailNames.
	fmt.Fprintf(os.Stderr, "Tailscale auth key: ")
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) == "" {
		return fmt.Errorf("no Tailscale auth key provided")
	}
	srv := tsnetServer(name)
	srv.AuthKey = strings.TrimSpace(sc.Text())
	defer srv.Close()
	if _, err := srv.Up(context.Background()); err != nil {
		return fmt.Errorf("tailscale: %v", err)
	}
	return nil
}

// onceLogf returns a logger that suppresses repeated messages.
func onceLogf(f func(string, ...any)) func(string, ...any) {
	var mu sync.Mutex
	seen := make(map[string]bool)
	return func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		mu.Lock()
		defer mu.Unlock()
		if !seen[msg] {
			seen[msg] = true
			f("%s", msg)
		}
	}
}

// A tsNet is the tailNet for a real tsnet node.
//
// Its Dial resolves host names strictly from the tailnet's own state
// and connects to the resulting Tailscale IP. It must never fall back
// to regular DNS: tail:// trusts the tailnet to authenticate both
// ends of the connection, so a mote session must not be able to fall
// through to some arbitrary internet host. (tsnet's own Dial consults
// the system resolver when the tailnet does not know the name.)
type tsNet struct {
	srv *tsnet.Server
}

func (t *tsNet) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ip, err := t.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	return t.srv.Dial(ctx, network, net.JoinHostPort(ip.String(), port))
}

// lookup resolves host on the tailnet, waiting until the context's
// deadline for the host to appear: a node that has just come up may
// not have received its list of peers yet.
func (t *tsNet) lookup(ctx context.Context, host string) (netip.Addr, error) {
	lc, err := t.srv.LocalClient()
	if err != nil {
		return netip.Addr{}, fmt.Errorf("tailscale: %v", err)
	}
	for {
		st, err := lc.Status(ctx)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("tailscale status: %v", err)
		}
		if ip, ok := tailPeerAddr(st, host); ok {
			return ip, nil
		}
		select {
		case <-ctx.Done():
			return netip.Addr{}, fmt.Errorf("no host %s on the tailnet", host)
		case <-time.After(1 * time.Second):
		}
	}
}

// tailPeerAddr returns the Tailscale IP of the tailnet peer named
// host in st. It checks both the peer's MagicDNS name (which changes
// to name-1, name-2, ... when names collide) and its machine name.
func tailPeerAddr(st *ipnstate.Status, host string) (netip.Addr, bool) {
	for _, peer := range st.Peer {
		name, _, _ := strings.Cut(peer.DNSName, ".")
		if (strings.EqualFold(name, host) || strings.EqualFold(peer.HostName, host)) && len(peer.TailscaleIPs) > 0 {
			return peer.TailscaleIPs[0], true
		}
	}
	return netip.Addr{}, false
}

func (t *tsNet) Listen(network, addr string) (net.Listener, error) {
	return t.srv.Listen(network, addr)
}

func (t *tsNet) Close() error {
	return t.srv.Close()
}

// dialTail connects to a tail://name server over Tailscale,
// through the daemon holding this machine's node.
func dialTail(u *url.URL) (io.ReadWriteCloser, error) {
	server := u.Host
	if server == "" {
		return nil, fmt.Errorf("server URL must have the form tail://name")
	}
	name, err := clientTailName()
	if err != nil {
		return nil, err
	}
	return daemonDial(name, fmt.Sprintf("mote-%s:%d", server, tailPort))
}

// serveTail implements "mote serve tail://name" (or "mote serve tail:"),
// asking the daemon holding the node to serve the tailnet and printing
// the daemon's log output until interrupted.
func serveTail(rawURL string) {
	var name string
	if rawURL == "tail:" || rawURL == "tail://" {
		var err error
		name, err = registeredTailName()
		if err != nil {
			log.Fatal(err)
		}
	} else {
		u, err := url.Parse(rawURL)
		if err != nil || u.Host == "" {
			log.Fatalf("serve URL must have the form tail://name")
		}
		name = u.Host
	}
	if err := daemonServe(name); err != nil {
		log.Fatal(err)
	}
}

// cmdLogin implements "mote login URL", establishing Tailscale
// credentials for tail://name.
func cmdLogin(args []string) {
	if len(args) != 1 {
		usage()
	}
	u, err := url.Parse(args[0])
	if err != nil || u.Scheme != "tail" || u.Host == "" {
		log.Fatalf("login URL must have the form tail://name")
	}
	if err := tailLogin(u.Host); err != nil {
		log.Fatal(err)
	}
	log.Printf("logged in as mote-%s", u.Host)
}
