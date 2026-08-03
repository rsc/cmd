// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"net/netip"
	"os"
	"strings"
	"testing"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"
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
					if err := os.WriteFile(tailStatePath(name), []byte("state"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			name, err := clientTailName()
			if name != tt.want || (err != nil) != tt.wantErr {
				t.Errorf("clientTailName() = %q, %v; want %q, error=%v", name, err, tt.want, tt.wantErr)
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
