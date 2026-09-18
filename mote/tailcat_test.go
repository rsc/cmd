// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/derp/derpserver"
	"tailscale.com/net/stun/stuntest"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// testTailcatKey returns a server key whose address names a DERP relay
// (and STUN server) running in this process, so that the transport
// test needs no network. This is tailscale.com/tstest/integration's
// RunDERPAndSTUN, copied rather than imported: that package brings in
// most of tailscaled.
func testTailcatKey(t *testing.T) *tailcat.PrivateKey {
	t.Helper()
	d := derpserver.New(key.NewNode(), logger.Discard)
	srv := httptest.NewUnstartedServer(derpserver.Handler(d))
	srv.Config.ErrorLog = logger.StdLogger(logger.Discard)
	srv.Config.TLSNextProto = make(map[string]func(*http.Server, *tls.Conn, http.Handler))
	srv.StartTLS()
	stunAddr, stunCleanup := stuntest.Serve(t)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		d.Close()
		stunCleanup()
	})

	k := tailcat.NewPrivateKey()
	k.Public.Region = []*tailcfg.DERPRegion{{
		RegionID:   1,
		RegionCode: "test",
		Nodes: []*tailcfg.DERPNode{{
			Name:             "t1",
			RegionID:         1,
			HostName:         "127.0.0.1",
			IPv4:             "127.0.0.1",
			IPv6:             "none",
			STUNPort:         stunAddr.Port,
			DERPPort:         srv.Listener.Addr().(*net.TCPAddr).Port,
			InsecureForTests: true,
		}},
	}}
	return k
}

func TestTailcatTransport(t *testing.T) {
	setupDirs(t)
	k := testTailcatKey(t)
	srv, ln, err := startTailcat(k)
	if err != nil {
		t.Fatalf("startTailcat: %v", err)
	}
	defer srv.Close()
	defer ln.Close()
	go serveListener(ln, "", nil)

	// The client learns the address the way "mote login" saves it.
	if err := setPassword("tailcat://test", string(tailcatAddr(k))); err != nil {
		t.Fatal(err)
	}
	for i := range 2 { // a second command opens a second tunnel
		conn, err := dialServer("tailcat://test")
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		runConn(t, conn, []string{"echo", "over tailcat"}, "over tailcat\n")
		conn.Close()
	}

	// A name with no saved address says how to save one.
	if _, err := dialServer("tailcat://other"); err == nil || !strings.Contains(err.Error(), "mote login tailcat://other") {
		t.Errorf("dial tailcat://other: %v, want error mentioning mote login tailcat://other", err)
	}
}

// TestTailcatAllowed checks that a server with an allowed list answers
// only the clients on it.
func TestTailcatAllowed(t *testing.T) {
	setupDirs(t)
	k := testTailcatKey(t)
	if err := setPassword("tailcat://test", string(tailcatAddr(k))); err != nil {
		t.Fatal(err)
	}
	// This machine's client key, and a server that allows only some other one.
	ck, err := makeTailcatClientKey()
	if err != nil {
		t.Fatalf("makeTailcatClientKey: %v", err)
	}
	if ck2, err := makeTailcatClientKey(); err != nil || !ck2.Equal(ck) {
		t.Fatalf("second makeTailcatClientKey = %v, %v; want the same key", ck2, err)
	}
	other := key.NewNode().Public()
	if added, err := allowTailcatClient(other); err != nil || !added {
		t.Fatalf("allowTailcatClient(other) = %v, %v; want true, nil", added, err)
	}
	srv, ln, err := startTailcat(k)
	if err != nil {
		t.Fatalf("startTailcat: %v", err)
	}
	go serveListener(ln, "", nil)

	defer func(d time.Duration) { tailcatDialTimeout = d }(tailcatDialTimeout)
	tailcatDialTimeout = 3 * time.Second
	if _, err := dialServer("tailcat://test"); err == nil || !strings.Contains(err.Error(), "allowed") {
		t.Fatalf("dial from disallowed client: %v, want error mentioning allowed", err)
	}
	srv.Close()
	ln.Close()

	// Allowing this client lets it in; allowing it again adds nothing.
	for i, want := range []bool{true, false} {
		if added, err := allowTailcatClient(ck.Public()); err != nil || added != want {
			t.Fatalf("allowTailcatClient #%d = %v, %v; want %v, nil", i, added, err, want)
		}
	}
	allowed, err := readTailcatAllowed()
	if err != nil {
		t.Fatal(err)
	}
	if len(allowed) != 2 || allowed[0] != other || allowed[1] != ck.Public() {
		t.Fatalf("readTailcatAllowed = %v, want [%v %v]", allowed, other, ck.Public())
	}
	srv, ln, err = startTailcat(k)
	if err != nil {
		t.Fatalf("startTailcat: %v", err)
	}
	defer srv.Close()
	defer ln.Close()
	go serveListener(ln, "", nil)
	conn, err := dialServer("tailcat://test")
	if err != nil {
		t.Fatalf("dial from allowed client: %v", err)
	}
	runConn(t, conn, []string{"echo", "allowed"}, "allowed\n")
	conn.Close()

	// A malformed list is an error naming the line.
	if err := os.WriteFile(tailcatAllowedFile(), []byte("# comment\n\nnodekey:xyz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readTailcatAllowed(); err == nil || !strings.Contains(err.Error(), "allowed.txt:3") {
		t.Errorf("readTailcatAllowed with malformed line: %v, want error at line 3", err)
	}
}

func TestTailcatKeyFile(t *testing.T) {
	setupDirs(t)
	// A saved key is read back as saved: same address.
	k := tailcat.NewPrivateKey()
	k.Public.RegionID = 42
	data, err := json.MarshalIndent(k, "", "\t")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tailcatKeyFile(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	k2, err := tailcatKey()
	if err != nil {
		t.Fatalf("tailcatKey: %v", err)
	}
	if !k2.Private.Equal(k.Private) || tailcatAddr(k2) != tailcatAddr(k) {
		t.Errorf("tailcatKey read back a different key: address %s, want %s", tailcatAddr(k2), tailcatAddr(k))
	}
	if err := checkTailcatAddr(tailcatAddr(k)); err != nil {
		t.Errorf("checkTailcatAddr(%s) = %v", tailcatAddr(k), err)
	}

	// A corrupt file is an error naming the file, not a new key.
	if err := os.WriteFile(tailcatKeyFile(), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tailcatKey(); err == nil || !strings.Contains(err.Error(), tailcatKeyFile()) {
		t.Errorf("tailcatKey with corrupt file: %v, want error naming %s", err, tailcatKeyFile())
	}
}

func TestCheckTailcatAddr(t *testing.T) {
	for _, bad := range []string{"", "kremvax", "tcnotanaddress", "tc" + strings.Repeat("A", 100)} {
		if err := checkTailcatAddr(tailcat.Addr(bad)); err == nil {
			t.Errorf("checkTailcatAddr(%q) succeeded, want error", bad)
		}
	}
	// An address from an old tailcat server, with no path-discovery key,
	// is rejected too: the client would refuse it later with a worse message.
	old := tailcat.ConnInfo{ServerPublic: tailcat.NewPrivateKey().Public.ServerPublic, RegionID: 1}
	if err := checkTailcatAddr(old.Addr()); err == nil {
		t.Errorf("checkTailcatAddr(old address) succeeded, want error")
	}
}

func TestDerpMapCache(t *testing.T) {
	setupDirs(t)
	var c derpMapCache
	const url = "https://example.com/derpmap.json"
	if _, _, _, ok := c.Get(url); ok {
		t.Fatal("Get on empty cache succeeded")
	}
	start := time.Now()
	if err := c.Put(url, []byte(`{"Regions":{}}`), `"etag1"`); err != nil {
		t.Fatalf("Put: %v", err)
	}
	data, etag, storedAt, ok := c.Get(url)
	if !ok || string(data) != `{"Regions":{}}` || etag != `"etag1"` {
		t.Errorf("Get = %q, %q, %v; want map, etag1, true", data, etag, ok)
	}
	if storedAt.Before(start.Add(-time.Second)) || storedAt.After(time.Now().Add(time.Second)) {
		t.Errorf("Get storedAt = %v, want about now", storedAt)
	}
	// The cache holds one map, for one URL.
	if _, _, _, ok := c.Get("https://other.example/derpmap.json"); ok {
		t.Error("Get for a different URL succeeded")
	}
}
