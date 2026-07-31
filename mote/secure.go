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
// with no JSON section. See protocol.md.

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
func secureServer(rw io.ReadWriteCloser, password string) (_ io.ReadWriteCloser, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("securing session: %w", err)
		}
	}()
	c := newConn(rw)
	st, msg1, err := cpace.Start(&cpace.Config{Role: cpace.Initiator, Password: []byte(password), ChannelID: channelID})
	if err != nil {
		return nil, err
	}
	if err := c.writePacket(nil, msg1); err != nil {
		return nil, err
	}
	msg2, err := c.readHandshakePacket()
	if err != nil {
		return nil, err
	}
	key, err := st.Finish(msg2, nil)
	if err != nil {
		return nil, err
	}
	// Check the client's key confirmation before sending the server's.
	// Either way an attacker gets only one password guess per connection,
	// but the server listens on an open port, so it must not be the one
	// to answer an unauthenticated guess: a client that guessed wrong
	// sees only a hangup. The client, which chose to connect, goes first.
	tag, err := c.readHandshakePacket()
	if err != nil {
		return nil, err
	}
	if err := st.Verify(tag); err != nil {
		return nil, fmt.Errorf("client used incorrect password")
	}
	if err := c.writePacket(nil, st.Tag()); err != nil {
		return nil, err
	}
	psk, err := derivePSK(key)
	if err != nil {
		return nil, err
	}
	hs, err := noise.NewHandshakeState(false, psk)
	if err != nil {
		return nil, err
	}
	m1, err := c.readHandshakePacket()
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
	if err := c.writePacket(nil, m2); err != nil {
		return nil, err
	}
	return &secureStream{rw: rw, enc: cs2, dec: cs1}, nil
}

// secureClient runs the client side of the encryption handshake,
// returning an encrypted channel layered over rw.
// The client is the CPace responder and the Noise initiator.
func secureClient(rw io.ReadWriteCloser, password string) (_ io.ReadWriteCloser, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("securing session: %w", err)
		}
	}()
	c := newConn(rw)
	msg1, err := c.readHandshakePacket()
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
	if err := c.writePacket(nil, msg2); err != nil {
		return nil, err
	}
	// Send the client's key confirmation before checking the server's;
	// see the comment in secureServer.
	if err := c.writePacket(nil, st.Tag()); err != nil {
		return nil, err
	}
	tag, err := c.readHandshakePacket()
	if err != nil {
		// The server hangs up instead of answering a key confirmation
		// that failed, so a hangup here usually means a wrong password.
		return nil, fmt.Errorf("no reply to key confirmation (incorrect password?): %v", err)
	}
	if err := st.Verify(tag); err != nil {
		return nil, fmt.Errorf("incorrect password for server")
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
	if err := c.writePacket(nil, m1); err != nil {
		return nil, err
	}
	m2, err := c.readHandshakePacket()
	if err != nil {
		return nil, err
	}
	_, cs1, cs2, err := hs.ReadMessage(nil, m2)
	if err != nil {
		return nil, fmt.Errorf("noise handshake: %v", err)
	}
	return &secureStream{rw: rw, enc: cs1, dec: cs2}, nil
}

// noiseOverhead is the number of bytes a Noise transport message adds
// to its plaintext: the ChaChaPoly authentication tag.
const noiseOverhead = 16

// maxNoisePlaintext is the largest plaintext that fits in one Noise
// transport message (65535-byte ciphertext limit minus the tag).
const maxNoisePlaintext = 65535 - noiseOverhead

// A secureStream is an encrypted stream layered over rw.
// Each Noise transport message is framed by a 16-bit big-endian
// ciphertext length, which is authenticated along with the message.
//
// Read and Write may be used concurrently with each other,
// as the client and server both do, but each is single-threaded:
// the cipher states advance a nonce for every message and so must
// see the messages in order.
type secureStream struct {
	rw   io.ReadWriteCloser
	enc  *noise.CipherState
	dec  *noise.CipherState
	rmu  sync.Mutex
	rbuf []byte
	wmu  sync.Mutex
}

func (s *secureStream) Read(p []byte) (int, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	for len(s.rbuf) == 0 {
		var hdr [2]byte
		if _, err := io.ReadFull(s.rw, hdr[:]); err != nil {
			return 0, err
		}
		ct := make([]byte, binary.BigEndian.Uint16(hdr[:]))
		if _, err := io.ReadFull(s.rw, ct); err != nil {
			return 0, err
		}
		pt, err := s.dec.Decrypt(nil, hdr[:], ct)
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
		// The length prefix is the message's associated data, so that
		// changing it in flight fails decryption instead of silently
		// desynchronizing the framing. The message cannot fit in hdr's
		// capacity, so Encrypt returns a new buffer holding hdr followed
		// by the ciphertext, leaving hdr itself intact to authenticate.
		var hdr [2]byte
		binary.BigEndian.PutUint16(hdr[:], uint16(n+noiseOverhead))
		ct, err := s.enc.Encrypt(hdr[:], hdr[:], p[:n])
		if err != nil {
			return total, fmt.Errorf("encrypting message: %v", err)
		}
		if _, err := s.rw.Write(ct); err != nil {
			return total, err
		}
		p = p[n:]
		total += n
	}
	return total, nil
}

func (s *secureStream) Close() error {
	return s.rw.Close()
}
