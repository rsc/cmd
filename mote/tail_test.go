// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"strings"
	"testing"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/persist"
)

func TestClientTailName(t *testing.T) {
	host := hostTailName()
	tests := []struct {
		name    string
		dirs    []string // node directories, "name" logged in, "name!" not
		want    string
		wantErr bool
	}{
		{"none", nil, host, false},
		{"host", []string{host}, host, false},
		{"other", []string{"other"}, "other", false},
		{"both", []string{host, "other"}, host, false},
		// An abandoned "mote login" leaves a directory with no
		// credentials in it, which is not a login to fall back to.
		{"abandoned", []string{host + "!", "other"}, "other", false},
		{"none-logged-in", []string{host + "!", "other!"}, host, false},
		{"ambiguous", []string{"other", "another"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MOTECONFIG", t.TempDir())
			for _, dir := range tt.dirs {
				name, loggedIn := dir, true
				if n, ok := strings.CutSuffix(dir, "!"); ok {
					name, loggedIn = n, false
				}
				if err := os.MkdirAll(tailDir(name), 0o700); err != nil {
					t.Fatal(err)
				}
				if loggedIn {
					writeTailState(t, name, testTailState(t, ipn.CurrentProfileStateKey, "profile-test"))
				}
			}
			name, err := clientTailName()
			if name != tt.want || (err != nil) != tt.wantErr {
				t.Errorf("clientTailName() = %q, %v; want %q, error=%v", name, err, tt.want, tt.wantErr)
			}
		})
	}
}

func testTailState(t *testing.T, startStateKey, selected ipn.StateKey) []byte {
	t.Helper()
	const profileKey = ipn.StateKey("profile-test")
	prefs := ipn.NewPrefs()
	prefs.Persist = &persist.Persist{
		PrivateNodeKey: key.NewNode(),
		UserProfile:    tailcfg.UserProfile{LoginName: "tagged-device"},
		NodeID:         "node-test",
	}
	profiles, err := json.Marshal(map[ipn.ProfileID]ipn.LoginProfile{
		"test": {ID: "test", Key: profileKey, UserProfile: prefs.Persist.UserProfile, NodeID: prefs.Persist.NodeID},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[ipn.StateKey][]byte{
		startStateKey:             []byte(selected),
		ipn.KnownProfilesStateKey: profiles,
		profileKey:                prefs.ToBytes(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeTailState(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(tailDir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tailStatePath(name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTailStateCredentials(t *testing.T) {
	for _, tt := range []struct {
		name     string
		data     []byte
		startKey ipn.StateKey
		want     bool
		wantErr  bool
	}{
		{"partial", []byte(`{"_machinekey":"cGFydGlhbA=="}`), ipn.CurrentProfileStateKey, false, false},
		{"tagged-id-zero", testTailState(t, ipn.CurrentProfileStateKey, "profile-test"), ipn.CurrentProfileStateKey, true, false},
		{"windows", testTailState(t, ipn.ServerModeStartKey, "profile-test"), ipn.ServerModeStartKey, true, false},
		{"non-current", testTailState(t, ipn.CurrentProfileStateKey, "other"), ipn.CurrentProfileStateKey, false, true},
		{"malformed", []byte("{"), ipn.CurrentProfileStateKey, false, true},
		{"malformed-profiles", []byte(`{"_profiles":""}`), ipn.CurrentProfileStateKey, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tailStateCredentials(tt.data, tt.startKey)
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Fatalf("tailStateCredentials = %v, %v; want %v, error=%v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestTailLoginRetry(t *testing.T) {
	t.Setenv("MOTECONFIG", t.TempDir())
	name := "retry"
	failed := errors.New("registration failed")
	transient := errors.New("temporary network failure")
	var keys []string
	if err := tailLoginWith(name, strings.NewReader("old-key\n"), func(key string) error {
		keys = append(keys, key)
		writeTailState(t, name, []byte(`{"_machinekey":"cGFydGlhbA=="}`))
		return failed
	}); !errors.Is(err, failed) {
		t.Fatalf("first login: %v", err)
	}
	registered := testTailState(t, ipn.CurrentProfileStateKey, "profile-test")
	if err := tailLoginWith(name, strings.NewReader("fresh-key\n"), func(key string) error {
		keys = append(keys, key)
		if _, err := os.Stat(tailStatePath(name)); !os.IsNotExist(err) {
			t.Errorf("partial state was not removed")
		}
		writeTailState(t, name, registered)
		return transient
	}); !errors.Is(err, transient) {
		t.Fatalf("retry: %v", err)
	}
	if err := tailLoginWith(name, strings.NewReader("unused\n"), func(key string) error {
		keys = append(keys, key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(tailStatePath(name))
	if err != nil || string(got) != string(registered) || strings.Join(keys, ",") != "old-key,fresh-key" {
		t.Fatalf("state or keys changed: state error=%v, keys=%q", err, keys)
	}
}

func TestTailLoginPreservesUnsafeState(t *testing.T) {
	for _, tt := range []struct {
		name string
		data []byte
	}{
		{"non-current", testTailState(t, ipn.CurrentProfileStateKey, "other")},
		{"malformed", []byte("{")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MOTECONFIG", t.TempDir())
			writeTailState(t, "test", tt.data)
			called := false
			err := tailLoginWith("test", strings.NewReader("key\n"), func(string) error {
				called = true
				return nil
			})
			got, readErr := os.ReadFile(tailStatePath("test"))
			if err == nil || called || readErr != nil || string(got) != string(tt.data) {
				t.Fatalf("err=%v, called=%v, readErr=%v", err, called, readErr)
			}
		})
	}
}

func TestTailPeerAddr(t *testing.T) {
	ip1 := netip.MustParseAddr("100.64.0.1")
	ip2 := netip.MustParseAddr("100.64.0.2")
	st := &ipnstate.Status{
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			// A name collision on the tailnet: MagicDNS renamed the
			// node, but the machine name still matches.
			key.NewNode().Public(): {
				HostName:     "mote-s7",
				DNSName:      "mote-s7-1.example.ts.net.",
				TailscaleIPs: []netip.Addr{ip1},
			},
			key.NewNode().Public(): {
				HostName:     "other",
				DNSName:      "mote-x.example.ts.net.",
				TailscaleIPs: []netip.Addr{ip2},
			},
			// A peer with no addresses must not be chosen.
			key.NewNode().Public(): {
				HostName: "mote-noip",
				DNSName:  "mote-noip.example.ts.net.",
			},
		},
	}
	tests := []struct {
		host string
		ip   netip.Addr
		ok   bool
	}{
		{"mote-s7", ip1, true}, // by machine name
		{"mote-x", ip2, true},  // by MagicDNS name
		{"MOTE-X", ip2, true},  // host names are case-insensitive
		{"mote-noip", netip.Addr{}, false},
		{"mote-nonexistent", netip.Addr{}, false},
	}
	for _, tt := range tests {
		ip, ok := tailPeerAddr(st, tt.host)
		if ip != tt.ip || ok != tt.ok {
			t.Errorf("tailPeerAddr(%q) = %v, %v; want %v, %v", tt.host, ip, ok, tt.ip, tt.ok)
		}
	}
}
