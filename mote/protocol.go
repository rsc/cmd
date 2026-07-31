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
// See the PROTOCOL comment in doc.go.
type Request struct {
	Type  string
	Error string   `json:",omitzero"`
	Files []*File  `json:",omitzero"`
	Cmd   string   `json:",omitzero"`
	Args  []string `json:",omitzero"`
	Dir   string   `json:",omitzero"`
	Env   []string `json:",omitzero"`
}

// A File describes a file to be placed on the remote system.
type File struct {
	Path string
	Hash string
	Size int64
}

// A Response is the JSON metadata sent from server to client.
// See the PROTOCOL comment in doc.go.
type Response struct {
	Type     string
	Error    string   `json:",omitempty"`
	Need     []string `json:",omitempty"`
	Stderr   bool
	ExitCode int    `json:",omitzero"`
	Status   string `json:",omitzero"`
	GOOS     string `json:",omitzero"`
	GOARCH   string `json:",omitzero"`
}

// maxJSON is the maximum accepted size for the JSON section of a packet.
// The binary section is unlimited (uploads can be arbitrarily large),
// but the JSON metadata should always be small.
const maxJSON = 1 << 20

// A packetConn reads and writes framed packets on an underlying stream.
// Each packet is a 32-bit big-endian JSON length, a 32-bit big-endian
// binary data length, the JSON, and then the binary data.
//
// packetConn reads only the exact bytes of each packet (no buffering),
// so a packetConn can be abandoned between packets and the underlying
// stream handed to a different layer, as happens during the encryption
// handshake.
type packetConn struct {
	r   io.Reader
	w   io.Writer
	wmu sync.Mutex
}

func newPacketConn(rw io.ReadWriter) *packetConn {
	return &packetConn{r: rw, w: rw}
}

// writePacket writes a packet with the JSON encoding of js
// (or no JSON at all if js is nil) followed by the binary data.
func (c *packetConn) writePacket(js any, data []byte) error {
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
	if _, err := c.w.Write(hdr[:]); err != nil {
		return err
	}
	// Skip zero-length writes: the reader does not issue a Read for an
	// empty section, and a zero-length Write blocks on net.Pipe.
	if len(enc) > 0 {
		if _, err := c.w.Write(enc); err != nil {
			return err
		}
	}
	if len(data) > 0 {
		if _, err := c.w.Write(data); err != nil {
			return err
		}
	}
	return nil
}

// writePacketStream writes a packet whose binary data is size bytes
// copied from r.
func (c *packetConn) writePacketStream(js any, size int64, r io.Reader) error {
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
	if _, err := c.w.Write(hdr[:]); err != nil {
		return err
	}
	if len(enc) > 0 {
		if _, err := c.w.Write(enc); err != nil {
			return err
		}
	}
	n, err := io.Copy(c.w, r)
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("short data stream: %d bytes copied, want %d", n, size)
	}
	return nil
}

// readPacket reads a packet, decoding the JSON section into js
// (which should be a pointer) and returning the binary data.
func (c *packetConn) readPacket(js any) ([]byte, error) {
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

// readPacketStream reads a packet header and JSON section, decoding the
// JSON into js, and returns a reader for the binary data section.
// The caller must fully read the returned reader before the next
// call to readPacket or readPacketStream.
func (c *packetConn) readPacketStream(js any) (size int64, body io.Reader, err error) {
	var hdr [8]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	jsize := binary.BigEndian.Uint32(hdr[0:])
	dsize := binary.BigEndian.Uint32(hdr[4:])
	if jsize > maxJSON {
		return 0, nil, fmt.Errorf("packet JSON too large: %d bytes", jsize)
	}
	if jsize > 0 {
		enc := make([]byte, jsize)
		if _, err := io.ReadFull(c.r, enc); err != nil {
			return 0, nil, err
		}
		if js != nil {
			if err := json.Unmarshal(enc, js); err != nil {
				return 0, nil, err
			}
		}
	}
	return int64(dsize), io.LimitReader(c.r, int64(dsize)), nil
}
