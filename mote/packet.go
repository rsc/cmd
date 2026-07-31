// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// A Request is the JSON metadata sent from client to server.
// See protocol.md.
type Request struct {
	Type  string
	Error string   `json:",omitzero"`
	Files []*File  `json:",omitzero"`
	Args  []string `json:",omitzero"`
	Dir   string   `json:",omitzero"`
	Env   []string `json:",omitzero"`
	Addr  string   `json:",omitzero"` // Dial, to the Tailscale daemon
}

// A File describes a file to be placed on the remote system.
type File struct {
	Path string
	Hash string
	Size int64
}

// A Response is the JSON metadata sent from server to client.
// See protocol.md.
type Response struct {
	Type     string
	Error    string   `json:",omitempty"`
	Need     []string `json:",omitempty"`
	Stderr   bool     `json:",omitzero"`
	ExitCode int      `json:",omitzero"`
	Status   string   `json:",omitzero"`
	GOOS     string   `json:",omitzero"`
	GOARCH   string   `json:",omitzero"`
}

// maxJSON is the maximum accepted size for the JSON section of a packet.
// The binary section is unlimited (uploads can be arbitrarily large),
// but the JSON metadata should always be small.
const maxJSON = 1 << 20

// maxHandshake is the maximum accepted size for a handshake packet.
// The CPace messages, the key confirmation tags, and the Noise messages
// are all well under 128 bytes.
const maxHandshake = 1024

// A Conn is a mote protocol connection, reading and writing framed
// packets on an underlying stream.
// Each packet is a 32-bit big-endian JSON length, a 32-bit big-endian
// binary data length, the JSON, and then the binary data.
//
// On the client, GOOS and GOARCH record the server's operating system
// and architecture, from the Info response read by dialServer.
//
// A Conn reads only the exact bytes of each packet (no buffering).
// The encryption handshake messages travel as packets on the plaintext
// stream, and then a new Conn is created on top of the encrypted
// stream; exact reads mean no bytes are lost to a buffer during that
// switch.
type Conn struct {
	GOOS   string
	GOARCH string
	rw     io.ReadWriteCloser
	wmu    sync.Mutex
}

func newConn(rw io.ReadWriteCloser) *Conn {
	return &Conn{rw: rw}
}

func (c *Conn) Close() error {
	return c.rw.Close()
}

// writePacket writes a packet with the JSON encoding of js
// (or no JSON at all if js is nil) followed by the binary data.
func (c *Conn) writePacket(js any, data []byte) error {
	var enc []byte
	if js != nil {
		var err error
		enc, err = json.Marshal(js)
		if err != nil {
			return err
		}
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:], uint32(len(enc)))
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(data)))
	if _, err := c.rw.Write(hdr[:]); err != nil {
		return err
	}
	// Skip zero-length writes: the reader does not issue a Read for an
	// empty section, and a zero-length Write blocks on net.Pipe.
	if len(enc) > 0 {
		if _, err := c.rw.Write(enc); err != nil {
			return err
		}
	}
	if len(data) > 0 {
		if _, err := c.rw.Write(data); err != nil {
			return err
		}
	}
	return nil
}

// writePacketStream writes a packet whose binary data is size bytes
// copied from r. It is an error for r to have fewer or more than
// size bytes.
func (c *Conn) writePacketStream(js any, size int64, r io.Reader) error {
	var enc []byte
	if js != nil {
		var err error
		enc, err = json.Marshal(js)
		if err != nil {
			return err
		}
	}
	if int64(uint32(size)) != size {
		return fmt.Errorf("data too large for packet")
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:], uint32(len(enc)))
	binary.BigEndian.PutUint32(hdr[4:], uint32(size))
	if _, err := c.rw.Write(hdr[:]); err != nil {
		return err
	}
	if len(enc) > 0 {
		if _, err := c.rw.Write(enc); err != nil {
			return err
		}
	}
	n, err := io.CopyN(c.rw, r, size)
	if err == io.EOF {
		return fmt.Errorf("short data stream: %d bytes copied, want %d", n, size)
	}
	if err != nil {
		return err
	}
	var extra [1]byte
	if n, _ := r.Read(extra[:]); n > 0 {
		return fmt.Errorf("data stream longer than %d bytes", size)
	}
	return nil
}

// readPacket reads a packet, decoding the JSON section into js
// (which should be a pointer) and returning the binary data.
func (c *Conn) readPacket(js any) ([]byte, error) {
	size, body, err := c.readPacketStream(js)
	if err != nil {
		return nil, err
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(body, data); err != nil {
		return nil, err
	}
	return data, nil
}

// readHandshakePacket reads a handshake packet: a packet with no JSON
// section and at most maxHandshake bytes of data.
//
// Handshake packets arrive before the peer has been authenticated, so
// unlike readPacket, which trusts the packet header enough to allocate a
// buffer for an upload of any size, readHandshakePacket checks the size
// before allocating. Otherwise any peer that can connect to the server
// could make it allocate up to 4 GB by sending an eight-byte header.
func (c *Conn) readHandshakePacket() ([]byte, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(c.rw, hdr[:]); err != nil {
		return nil, err
	}
	jsize := binary.BigEndian.Uint32(hdr[0:])
	dsize := binary.BigEndian.Uint32(hdr[4:])
	if jsize != 0 {
		return nil, fmt.Errorf("handshake packet has JSON section")
	}
	if dsize > maxHandshake {
		return nil, fmt.Errorf("handshake packet too large: %d bytes", dsize)
	}
	data := make([]byte, dsize)
	if _, err := io.ReadFull(c.rw, data); err != nil {
		return nil, err
	}
	return data, nil
}

// readPacketStream reads a packet header and JSON section, decoding the
// JSON into js, and returns a reader for the binary data section.
// The caller must fully read the returned reader before the next
// call to readPacket or readPacketStream.
func (c *Conn) readPacketStream(js any) (size int64, body io.Reader, err error) {
	var hdr [8]byte
	if _, err := io.ReadFull(c.rw, hdr[:]); err != nil {
		return 0, nil, err
	}
	jsize := binary.BigEndian.Uint32(hdr[0:])
	dsize := binary.BigEndian.Uint32(hdr[4:])
	if jsize > maxJSON {
		return 0, nil, fmt.Errorf("packet JSON too large: %d bytes", jsize)
	}
	if jsize > 0 {
		enc := make([]byte, jsize)
		if _, err := io.ReadFull(c.rw, enc); err != nil {
			return 0, nil, err
		}
		if js != nil {
			if err := json.Unmarshal(enc, js); err != nil {
				return 0, nil, err
			}
		}
	}
	return int64(dsize), io.LimitReader(c.rw, int64(dsize)), nil
}
