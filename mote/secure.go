// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"rsc.io/cmd/mote/internal/cpace"
	"rsc.io/cmd/mote/internal/noise"
)

// The encryption layer used for direct TCP connections:
// a CPace handshake authenticates the shared password and derives a key,
// and then a Noise NNpsk0 handshake using that key as the pre-shared key
// establishes the encrypted channel.
// The handshake messages travel in the standard packet framing
// with no JSON section. See the PROTOCOL comment in doc.go.

// channelID is the CPace channel identifier, fixed for the mote protocol.
var channelID = []byte("rsc.io/cmd/mote tcp")

// derivePSK converts the CPace intermediate session key
// into the 32-byte Noise pre-shared key.
func derivePSK(key []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, key, nil, "mote noise psk", 32)
}

// secureServer runs the server side of the encryption handshake,
// returning an encrypted channel layered over rw.
// The server is the CPace initiator and the Noise responder.
func secureServer(rw io.ReadWriter, password string) (io.ReadWriter, error) {
	pc := newPacketConn(rw)
	st, msg1, err := cpace.Start(&cpace.Config{Role: cpace.Initiator, Password: []byte(password), ChannelID: channelID})
	if err != nil {
		return nil, err
	}
	if err := pc.writePacket(nil, msg1); err != nil {
		return nil, err
	}
	msg2, err := pc.readPacket(nil)
	if err != nil {
		return nil, err
	}
	key, err := st.Finish(msg2, nil)
	if err != nil {
		return nil, err
	}
	if err := pc.writePacket(nil, st.Tag()); err != nil {
		return nil, err
	}
	tag, err := pc.readPacket(nil)
	if err != nil {
		return nil, err
	}
	if err := st.Verify(tag); err != nil {
		return nil, fmt.Errorf("client used incorrect password")
	}
	psk, err := derivePSK(key)
	if err != nil {
		return nil, err
	}
	hs, err := noise.NewHandshakeState(false, psk)
	if err != nil {
		return nil, err
	}
	m1, err := pc.readPacket(nil)
	if err != nil {
		return nil, err
	}
	if _, _, _, err := hs.ReadMessage(nil, m1); err != nil {
		return nil, fmt.Errorf("noise handshake: %v", err)
	}
	m2, cs1, cs2, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("noise handshake: %v", err)
	}
	if err := pc.writePacket(nil, m2); err != nil {
		return nil, err
	}
	return &secureStream{rw: rw, enc: cs2, dec: cs1}, nil
}

// secureClient runs the client side of the encryption handshake,
// returning an encrypted channel layered over rw.
// The client is the CPace responder and the Noise initiator.
func secureClient(rw io.ReadWriter, password string) (io.ReadWriter, error) {
	pc := newPacketConn(rw)
	msg1, err := pc.readPacket(nil)
	if err != nil {
		return nil, err
	}
	st, msg2, err := cpace.Start(&cpace.Config{Role: cpace.Responder, Password: []byte(password), ChannelID: channelID})
	if err != nil {
		return nil, err
	}
	key, err := st.Finish(msg1, nil)
	if err != nil {
		return nil, err
	}
	if err := pc.writePacket(nil, msg2); err != nil {
		return nil, err
	}
	tag, err := pc.readPacket(nil)
	if err != nil {
		return nil, err
	}
	if err := st.Verify(tag); err != nil {
		return nil, fmt.Errorf("incorrect password for server")
	}
	if err := pc.writePacket(nil, st.Tag()); err != nil {
		return nil, err
	}
	psk, err := derivePSK(key)
	if err != nil {
		return nil, err
	}
	hs, err := noise.NewHandshakeState(true, psk)
	if err != nil {
		return nil, err
	}
	m1, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("noise handshake: %v", err)
	}
	if err := pc.writePacket(nil, m1); err != nil {
		return nil, err
	}
	m2, err := pc.readPacket(nil)
	if err != nil {
		return nil, err
	}
	_, cs1, cs2, err := hs.ReadMessage(nil, m2)
	if err != nil {
		return nil, fmt.Errorf("noise handshake: %v", err)
	}
	return &secureStream{rw: rw, enc: cs1, dec: cs2}, nil
}

// maxNoisePlaintext is the largest plaintext that fits in one Noise
// transport message (65535-byte ciphertext limit minus the 16-byte tag).
const maxNoisePlaintext = 65535 - 16

// A secureStream is an encrypted stream layered over rw.
// Each Noise transport message is framed by a 16-bit big-endian
// ciphertext length.
type secureStream struct {
	rw   io.ReadWriter
	enc  *noise.CipherState
	dec  *noise.CipherState
	rbuf []byte
	wmu  sync.Mutex
}

func (s *secureStream) Read(p []byte) (int, error) {
	for len(s.rbuf) == 0 {
		var hdr [2]byte
		if _, err := io.ReadFull(s.rw, hdr[:]); err != nil {
			return 0, err
		}
		ct := make([]byte, binary.BigEndian.Uint16(hdr[:]))
		if _, err := io.ReadFull(s.rw, ct); err != nil {
			return 0, err
		}
		pt, err := s.dec.Decrypt(nil, nil, ct)
		if err != nil {
			return 0, fmt.Errorf("decrypting message: %v", err)
		}
		s.rbuf = pt
	}
	n := copy(p, s.rbuf)
	s.rbuf = s.rbuf[n:]
	return n, nil
}

func (s *secureStream) Write(p []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	total := 0
	for len(p) > 0 {
		n := min(len(p), maxNoisePlaintext)
		var frame [2]byte
		ct, err := s.enc.Encrypt(frame[:], nil, p[:n])
		if err != nil {
			return total, fmt.Errorf("encrypting message: %v", err)
		}
		binary.BigEndian.PutUint16(ct[:2], uint16(len(ct)-2))
		if _, err := s.rw.Write(ct); err != nil {
			return total, err
		}
		p = p[n:]
		total += n
	}
	return total, nil
}
