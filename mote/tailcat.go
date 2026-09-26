// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/filter"
)

// Tailcat is Tailscale's data plane without its control plane: the same
// WireGuard tunnels, NAT traversal, and DERP relays as tail://, but
// with no tailnet, no account, and no names. A server is identified by
// a tailcat address, a string holding its public keys and the DERP
// region where clients find it. The address contains a pre-shared key,
// so it is a secret: anyone who has it can connect. The client keeps it
// in password.txt, keyed by the tailcat://name URL that stands for it.
//
// Bringing a tailcat client up takes a fraction of a second, so unlike
// tail:// there is no daemon: each command opens its own tunnel.
//
// A client can also have a key of its own, and a server can be told
// which client keys to answer, so that the address alone is not enough
// to reach it. See tailcatClientKey and cmdAllow.

// tailcatDialTimeout bounds a connection attempt: fetching the DERP
// map, reaching the relay, and finding a path to the server.
// It is a variable for testing.
var tailcatDialTimeout = 30 * time.Second

// tailcatDir returns the tailcat subdirectory of the configuration
// directory, creating it if necessary. It holds the server key and the
// cached relay map.
func tailcatDir() string {
	dir := filepath.Join(configDir(), "tailcat")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatal(err)
	}
	return dir
}

// tailcatKeyFile is the file holding this machine's tailcat server key.
func tailcatKeyFile() string {
	return filepath.Join(tailcatDir(), "key.json")
}

// tailcatClientKeyFile is the file holding this machine's tailcat client key.
func tailcatClientKeyFile() string {
	return filepath.Join(tailcatDir(), "client.json")
}

// tailcatAllowedFile is the file listing the client keys the server answers.
func tailcatAllowedFile() string {
	return filepath.Join(tailcatDir(), "allowed.txt")
}

// tailcatName returns the password.txt key for the tailcat://name URL u.
func tailcatName(u *url.URL) string {
	return "tailcat://" + u.Host
}

// tailcatLogf returns the logger for tailcat's own messages,
// which are debugging output unless -v is given.
func tailcatLogf() logger.Logf {
	if *verbose {
		return log.Printf
	}
	return logger.Discard
}

// tailcatKey returns this machine's tailcat server key, generating and
// saving one if there is none yet. Generating a key picks the nearest
// DERP region now and fixes it in the key, so that the address stays
// the same across restarts: clients have written it down.
func tailcatKey() (*tailcat.PrivateKey, error) {
	file := tailcatKeyFile()
	data, err := os.ReadFile(file)
	if err == nil {
		var k tailcat.PrivateKey
		if err := json.Unmarshal(data, &k); err != nil {
			return nil, fmt.Errorf("%s: %v", file, err)
		}
		return &k, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	k := tailcat.NewPrivateKey()
	ci := tailcat.ConnInfo{RegionID: -1} // -1 means the nearest region
	if err := ci.Expand(context.Background(), tailcat.ExpandForServer, derpMapCache{}); err != nil {
		return nil, fmt.Errorf("choosing DERP region: %v", err)
	}
	k.Public.RegionID = ci.Region[0].RegionID
	data, err = json.MarshalIndent(k, "", "\t")
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(file, data, 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

// tailcatAddr returns the address clients use to reach the server
// holding k. It names the DERP region by number, so the address does
// not change when the region's relays do.
func tailcatAddr(k *tailcat.PrivateKey) tailcat.Addr {
	return k.Public.Addr()
}

// A tailcatClientKey is the content of client.json, this machine's
// identity as a tailcat client. Without one, each command makes up a
// key of its own, which is fine for a server that answers any client
// but not for one with an allowed list. The file has the same form as
// tailcat's own client key files, so one of those can be copied in.
type tailcatClientKey struct {
	Private key.NodePrivate
}

// loadTailcatClientKey returns this machine's tailcat client key,
// or the zero key if it has none.
func loadTailcatClientKey() (key.NodePrivate, error) {
	file := tailcatClientKeyFile()
	data, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return key.NodePrivate{}, nil
	}
	if err != nil {
		return key.NodePrivate{}, err
	}
	var k tailcatClientKey
	if err := json.Unmarshal(data, &k); err != nil {
		return key.NodePrivate{}, fmt.Errorf("%s: %v", file, err)
	}
	if k.Private.IsZero() {
		return key.NodePrivate{}, fmt.Errorf("%s: no key", file)
	}
	return k.Private, nil
}

// makeTailcatClientKey returns this machine's tailcat client key,
// generating and saving one if it has none.
func makeTailcatClientKey() (key.NodePrivate, error) {
	k, err := loadTailcatClientKey()
	if err != nil || !k.IsZero() {
		return k, err
	}
	k = key.NewNode()
	data, err := json.MarshalIndent(tailcatClientKey{Private: k}, "", "\t")
	if err != nil {
		return key.NodePrivate{}, err
	}
	if err := writeFileAtomic(tailcatClientKeyFile(), data, 0o600); err != nil {
		return key.NodePrivate{}, err
	}
	return k, nil
}

// readTailcatAllowed returns the client keys listed in allowed.txt,
// one per line, in the order written there.
func readTailcatAllowed() ([]key.NodePublic, error) {
	file := tailcatAllowedFile()
	data, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var keys []key.NodePublic
	lineno := 0
	for line := range strings.Lines(string(data)) {
		lineno++
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var k key.NodePublic
		if err := k.UnmarshalText([]byte(line)); err != nil {
			return nil, fmt.Errorf("%s:%d: malformed key", file, lineno)
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// allowTailcatClient adds k to allowed.txt,
// reporting whether it was not already there.
func allowTailcatClient(k key.NodePublic) (added bool, err error) {
	keys, err := readTailcatAllowed()
	if err != nil {
		return false, err
	}
	if slices.Contains(keys, k) {
		return false, nil
	}
	keys = append(keys, k)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s\n", k)
	}
	if err := writeFileAtomic(tailcatAllowedFile(), []byte(b.String()), 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// cmdAllow implements "mote allow tailcat: [key]": with a key, it adds
// that client key to the ones the server answers; without one, it
// lists them. A server with no allowed list answers any client that
// has its address.
func cmdAllow(args []string) {
	if len(args) < 1 || len(args) > 2 {
		usage()
	}
	if args[0] != "tailcat:" && args[0] != "tailcat://" {
		log.Fatalf("mote allow applies only to tailcat:")
	}
	if len(args) == 1 {
		keys, err := readTailcatAllowed()
		if err != nil {
			log.Fatal(err)
		}
		for _, k := range keys {
			fmt.Println(k)
		}
		return
	}
	var k key.NodePublic
	if err := k.UnmarshalText([]byte(args[1])); err != nil {
		log.Fatalf("invalid client key %q: want the nodekey:... printed by mote login tailcat:client", args[1])
	}
	added, err := allowTailcatClient(k)
	if err != nil {
		log.Fatal(err)
	}
	if !added {
		log.Printf("%s is already allowed", k)
		return
	}
	log.Printf("allowed %s in %s (a running mote serve tailcat: must be restarted to see it)", k, tailcatAllowedFile())
}

// startTailcat starts a tailcat server holding k and returns it along
// with the listener that delivers connections to mote's port.
// The server answers only the clients in allowed.txt, if there are any.
func startTailcat(k *tailcat.PrivateKey) (*tailcat.Server, net.Listener, error) {
	allowed, err := readTailcatAllowed()
	if err != nil {
		return nil, nil, err
	}
	ln := newTailcatListener()
	srv := &tailcat.Server{
		Key:                 k.Private,
		PresharedKey:        k.Public.PresharedKey,
		DisablePresharedKey: k.Public.PresharedKey.IsZero(),
		RegionID:            k.Public.RegionID,
		DERPMapCache:        derpMapCache{},
		ServedTCPPorts:      []filter.PortRange{{First: tailPort, Last: tailPort}},
		Logf:                tailcatLogf(),
		OnTCP: func(port uint16) func(net.Conn) {
			if port != tailPort {
				return nil
			}
			return ln.handle
		},
	}
	if len(allowed) > 0 {
		// A nil AllowClient answers everyone; an empty allowed.txt
		// means the same, so only install the set when it has keys.
		var allow tailcat.KeySet
		for _, k := range allowed {
			allow.Add(k)
		}
		srv.AllowClient = allow.Contains
		log.Printf("answering %d allowed client keys", len(allowed))
	}
	if len(k.Public.Region) > 0 {
		// A key made for a particular relay (or for a test) names the
		// region outright instead of referring to the DERP map.
		srv.Region = k.Public.Region[0]
	}
	if err := srv.Start(); err != nil {
		return nil, nil, err
	}
	return srv, ln, nil
}

// serveTailcat implements "mote serve tailcat:".
func serveTailcat(rawURL string) {
	if rawURL != "tailcat:" && rawURL != "tailcat://" {
		log.Fatalf("serve URL must be tailcat:")
	}
	k, err := tailcatKey()
	if err != nil {
		log.Fatal(err)
	}
	srv, ln, err := startTailcat(k)
	if err != nil {
		// The region a key names can disappear from the DERP map;
		// then there is nothing to do but start over with a new key.
		log.Fatalf("tailcat: %v\n(if the DERP region is gone, remove %s to make a new key and address)", err, tailcatKeyFile())
	}
	defer srv.Close()
	log.Printf("serving tailcat address %s", tailcatAddr(k))
	log.Fatal(serveListener(ln, "", nil))
}

// tailcatLogin implements the tailcat forms of "mote login":
//
//	mote login tailcat:         the server side: make this machine's server
//	                            key, if it has none, and print its address
//	mote login tailcat:client   the client side: make this machine's client
//	                            key, if it has none, and print its public key
//	mote login tailcat://name   the client side: prompt for a server's
//	                            address and save it under tailcat://name
func tailcatLogin(u *url.URL) error {
	switch {
	case u.Opaque == "client":
		k, err := makeTailcatClientKey()
		if err != nil {
			return err
		}
		fmt.Println(k.Public())
		return nil
	case u.Opaque != "":
		return fmt.Errorf("login URL must have the form tailcat:, tailcat:client, or tailcat://name")
	case u.Host == "":
		k, err := tailcatKey()
		if err != nil {
			return err
		}
		fmt.Println(tailcatAddr(k))
		return nil
	}
	name := tailcatName(u)
	addr, err := promptSecret("tailcat address", name)
	if err != nil {
		return err
	}
	if err := checkTailcatAddr(tailcat.Addr(addr)); err != nil {
		return err
	}
	if err := setPassword(name, addr); err != nil {
		return err
	}
	log.Printf("wrote address for %s to %s", name, passwordFile())
	return nil
}

// checkTailcatAddr checks that addr is a usable tailcat address.
func checkTailcatAddr(addr tailcat.Addr) error {
	ci, err := tailcat.ParseAddr(addr)
	if err != nil {
		return fmt.Errorf("invalid tailcat address: %v", err)
	}
	if ci.ServerDiscoPublic.IsZero() {
		return fmt.Errorf("invalid tailcat address: made by an old tailcat server")
	}
	return nil
}

// lookupTailcatAddr returns the address saved for the tailcat://name URL u.
func lookupTailcatAddr(u *url.URL) (tailcat.Addr, error) {
	name := tailcatName(u)
	passwords, err := readPasswords()
	if err != nil {
		return "", err
	}
	addr := passwords[name]
	if addr == "" {
		return "", fmt.Errorf("no address for %s; run mote login %s", name, name)
	}
	return tailcat.Addr(addr), nil
}

// dialTailcat connects to a tailcat://name server,
// at the address saved for it by "mote login".
func dialTailcat(u *url.URL) (io.ReadWriteCloser, error) {
	if u.Host == "" {
		return nil, fmt.Errorf("server URL must have the form tailcat://name")
	}
	addr, err := lookupTailcatAddr(u)
	if err != nil {
		return nil, err
	}
	// The client key, if this machine has one; tailcat makes up a key
	// for the command otherwise.
	k, err := loadTailcatClientKey()
	if err != nil {
		return nil, err
	}
	client := &tailcat.Client{Server: addr, Key: k, Logf: tailcatLogf(), DERPMapCache: derpMapCache{}}
	ctx, cancel := context.WithTimeout(context.Background(), tailcatDialTimeout)
	defer cancel() // governs the dial only, not the connection it returns
	conn, err := client.DialTCPPort(ctx, tailPort)
	if err != nil {
		client.Close()
		if ctx.Err() != nil {
			// A server that is down and one that is ignoring this
			// client, because its key is not allowed, look the same
			// from here: silence.
			return nil, fmt.Errorf("tailcat: no answer from %s in %v (is the server running, and is this client allowed?)", tailcatName(u), tailcatDialTimeout)
		}
		return nil, fmt.Errorf("tailcat: %v", err)
	}
	return &tailcatConn{Conn: conn, client: client}, nil
}

// A tailcatConn is a connection through a tailcat client, which is
// closed along with the connection: the client is the tunnel the
// connection runs through, and nothing else uses it.
type tailcatConn struct {
	net.Conn
	client *tailcat.Client
}

func (c *tailcatConn) Close() error {
	err := c.Conn.Close()
	c.client.Close()
	return err
}

// A tailcatListener is a net.Listener fed by a tailcat server, whose
// connections arrive as calls to a handler rather than from Accept.
// It lets serveListener serve a tailcat server like any other.
type tailcatListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newTailcatListener() *tailcatListener {
	return &tailcatListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

// handle is the tailcat server's handler for a connection to mote's port.
// It returns once Accept has taken the connection; the connection
// stays open when it does.
func (l *tailcatListener) handle(c net.Conn) {
	select {
	case l.conns <- c:
	case <-l.done:
		c.Close()
	}
}

func (l *tailcatListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *tailcatListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *tailcatListener) Addr() net.Addr { return tailcatNetAddr{} }

// A tailcatNetAddr is the address of a tailcatListener: mote's port on
// whatever tailcat address the server has.
type tailcatNetAddr struct{}

func (tailcatNetAddr) Network() string { return "tailcat" }
func (tailcatNetAddr) String() string  { return fmt.Sprintf(":%d", tailPort) }

// A derpMapCache keeps the DERP map that tailcat fetches in the
// configuration directory, so that the fetch is skipped when the map
// is fresh (tailcat uses a map under an hour old without asking) and
// so that a stale map still serves when the map server is unreachable.
type derpMapCache struct{}

func derpMapFile() string {
	return filepath.Join(tailcatDir(), "derpmap.json")
}

// A derpMapEntry is the file format of the cache: the map itself,
// along with the URL it came from and the server's ETag for it.
// The file's modification time is when it was stored.
type derpMapEntry struct {
	URL  string
	ETag string `json:",omitempty"`
	Map  json.RawMessage
}

func (derpMapCache) Get(url string) (data []byte, etag string, storedAt time.Time, ok bool) {
	file := derpMapFile()
	info, err := os.Stat(file)
	if err != nil {
		return nil, "", time.Time{}, false
	}
	data, err = os.ReadFile(file)
	if err != nil {
		return nil, "", time.Time{}, false
	}
	var e derpMapEntry
	if err := json.Unmarshal(data, &e); err != nil || e.URL != url {
		return nil, "", time.Time{}, false
	}
	return e.Map, e.ETag, info.ModTime(), true
}

func (derpMapCache) Put(url string, data []byte, etag string) error {
	enc, err := json.Marshal(derpMapEntry{URL: url, ETag: etag, Map: data})
	if err != nil {
		return err
	}
	// Rewriting the same map still updates the modification time,
	// which is what restarts tailcat's freshness window.
	return writeFileAtomic(derpMapFile(), enc, 0o600)
}

// writeFileAtomic writes data to file by way of a temporary file,
// so that a reader never sees a partial file.
func writeFileAtomic(file string, data []byte, perm os.FileMode) error {
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
