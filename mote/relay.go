// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
)

// cmdRelay implements "mote relay localport:server:remoteport...",
// forwarding connections to each local port to the corresponding port
// on the named server, through a mote session per connection.
func cmdRelay(args []string) {
	if len(args) == 0 {
		usage()
	}
	for _, arg := range args {
		lport, server, rport, err := parseRelay(arg)
		if err != nil {
			log.Fatal(err)
		}
		url, err := resolveServer(server, "", nil)
		if err != nil {
			log.Fatal(err)
		}
		// Loopback only: the relayed port is reachable by whoever can
		// reach the local one, so do not offer it to the whole network.
		ln, err := net.Listen("tcp", "localhost:"+lport)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("relaying %s to %s port %s", ln.Addr(), server, rport)
		go relayListener(ln, url, "localhost:"+rport)
	}
	select {} // until interrupted
}

// parseRelay parses a relay specification localport:server:remoteport,
// where server is an alias or a server URL, which may itself contain
// colons: the ports are the text before the first colon and after the
// last one.
func parseRelay(spec string) (lport, server, rport string, err error) {
	lport, rest, ok1 := strings.Cut(spec, ":")
	i := strings.LastIndex(rest, ":")
	if !ok1 || i < 0 {
		return "", "", "", fmt.Errorf("invalid relay %s: must have the form localport:server:remoteport", spec)
	}
	server, rport = rest[:i], rest[i+1:]
	for _, port := range []string{lport, rport} {
		if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
			return "", "", "", fmt.Errorf("invalid relay %s: bad port %q", spec, port)
		}
	}
	if server == "" {
		return "", "", "", fmt.Errorf("invalid relay %s: missing server", spec)
	}
	return lport, server, rport, nil
}

// relayListener accepts connections on ln and relays each one to addr,
// a host:port as seen from the server at url, until ln is closed.
// Each connection is its own mote session: the transports share what
// they can already (an ssh connection, a gomote instance), and a
// session carries one byte stream.
func relayListener(ln net.Listener, url, addr string) {
	for {
		lc, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer lc.Close()
			conn, err := dialServer(url)
			if err != nil {
				log.Print(err)
				return
			}
			rw, err := conn.Dial(addr)
			if err != nil {
				log.Print(conn.abort(err))
				return
			}
			proxy(lc, rw)
		}()
	}
}
