// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"net/netip"
	"testing"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"
)

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
