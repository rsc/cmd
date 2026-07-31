// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package noise implements the Noise_NNpsk0_25519_ChaChaPoly_SHA256
// handshake pattern from the Noise Protocol Framework (revision 34),
// which is all of Noise that mote needs: an ephemeral-ephemeral key
// agreement mixed with a pre-shared key established out of band
// (by the CPace exchange).
package noise

import (
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"golang.org/x/crypto/chacha20poly1305"
)

// protocolName is the Noise protocol name that seeds the handshake hash.
const protocolName = "Noise_NNpsk0_25519_ChaChaPoly_SHA256"

// A CipherState encrypts or decrypts a sequence of messages with a key
// and an incrementing nonce, as defined in Noise §5.1.
type CipherState struct {
	aead cipher.AEAD
	n    uint64
}

func newCipherState(key []byte) *CipherState {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		// key is always 32 bytes, from HKDF.
		panic("noise: " + err.Error())
	}
	return &CipherState{aead: aead}
}

// nonce returns the 96-bit ChaChaPoly nonce for counter n:
// 32 zero bits followed by n in little-endian order (Noise §12.2).
func nonce(n uint64) []byte {
	var buf [12]byte
	binary.LittleEndian.PutUint64(buf[4:], n)
	return buf[:]
}

// Encrypt appends to out the encryption of plaintext with associated
// data ad, using and incrementing the nonce counter.
func (c *CipherState) Encrypt(out, ad, plaintext []byte) ([]byte, error) {
	if c.n == math.MaxUint64 {
		return nil, errors.New("noise: nonce exhausted")
	}
	out = c.aead.Seal(out, nonce(c.n), plaintext, ad)
	c.n++
	return out, nil
}

// Decrypt appends to out the decryption of ciphertext with associated
// data ad, using and incrementing the nonce counter.
func (c *CipherState) Decrypt(out, ad, ciphertext []byte) ([]byte, error) {
	if c.n == math.MaxUint64 {
		return nil, errors.New("noise: nonce exhausted")
	}
	out, err := c.aead.Open(out, nonce(c.n), ciphertext, ad)
	if err != nil {
		return nil, errors.New("noise: decryption failed")
	}
	c.n++
	return out, nil
}

// hkdf is the HKDF construction from Noise §4.3,
// returning num (2 or 3) hash-sized outputs.
func hkdf(chainingKey, input []byte, num int) (out [][]byte) {
	mac := func(key, data []byte) []byte {
		h := hmac.New(sha256.New, key)
		h.Write(data)
		return h.Sum(nil)
	}
	temp := mac(chainingKey, input)
	last := []byte(nil)
	for i := 1; i <= num; i++ {
		data := make([]byte, 0, len(last)+1)
		data = append(data, last...)
		data = append(data, byte(i))
		last = mac(temp, data)
		out = append(out, last)
	}
	return out
}

// A symmetricState is the SymmetricState object from Noise §5.2.
type symmetricState struct {
	ck []byte
	h  []byte
	cs *CipherState
}

func newSymmetricState() *symmetricState {
	h := sha256.Sum256([]byte(protocolName)) // len(protocolName) > 32
	ss := &symmetricState{ck: h[:], h: h[:]}
	ss.mixHash(nil) // empty prologue
	return ss
}

func (ss *symmetricState) mixHash(data []byte) {
	h := sha256.New()
	h.Write(ss.h)
	h.Write(data)
	ss.h = h.Sum(nil)
}

func (ss *symmetricState) mixKey(input []byte) {
	out := hkdf(ss.ck, input, 2)
	ss.ck = out[0]
	ss.cs = newCipherState(out[1])
}

func (ss *symmetricState) mixKeyAndHash(input []byte) {
	out := hkdf(ss.ck, input, 3)
	ss.ck = out[0]
	ss.mixHash(out[1])
	ss.cs = newCipherState(out[2])
}

func (ss *symmetricState) encryptAndHash(plaintext []byte) ([]byte, error) {
	if ss.cs == nil {
		ss.mixHash(plaintext)
		return plaintext, nil
	}
	ct, err := ss.cs.Encrypt(nil, ss.h, plaintext)
	if err != nil {
		return nil, err
	}
	ss.mixHash(ct)
	return ct, nil
}

func (ss *symmetricState) decryptAndHash(ciphertext []byte) ([]byte, error) {
	if ss.cs == nil {
		ss.mixHash(ciphertext)
		return ciphertext, nil
	}
	pt, err := ss.cs.Decrypt(nil, ss.h, ciphertext)
	if err != nil {
		return nil, err
	}
	ss.mixHash(ciphertext)
	return pt, nil
}

// split returns the two transport cipher states (Noise §5.2):
// the first for messages from initiator to responder,
// the second for messages from responder to initiator.
func (ss *symmetricState) split() (*CipherState, *CipherState) {
	out := hkdf(ss.ck, nil, 2)
	return newCipherState(out[0]), newCipherState(out[1])
}

// A HandshakeState runs the two-message NNpsk0 handshake:
//
//	-> psk, e
//	<- e, ee
type HandshakeState struct {
	ss        *symmetricState
	initiator bool
	psk       []byte
	e         *ecdh.PrivateKey
	re        *ecdh.PublicKey
	msg       int
}

// NewHandshakeState returns a HandshakeState using the 32-byte
// pre-shared key psk. The initiator sends the first message.
func NewHandshakeState(initiator bool, psk []byte) (*HandshakeState, error) {
	if len(psk) != 32 {
		return nil, fmt.Errorf("noise: pre-shared key must be 32 bytes, not %d", len(psk))
	}
	return &HandshakeState{ss: newSymmetricState(), initiator: initiator, psk: psk}, nil
}

// WriteMessage appends the next handshake message, carrying the
// encrypted payload, to out. On the final handshake message it also
// returns the two transport cipher states, in the order returned
// by split.
func (hs *HandshakeState) WriteMessage(out, payload []byte) ([]byte, *CipherState, *CipherState, error) {
	if hs.initiator != (hs.msg == 0) || hs.msg > 1 {
		return nil, nil, nil, errors.New("noise: out of turn")
	}
	if hs.msg == 0 {
		hs.ss.mixKeyAndHash(hs.psk)
	}
	e, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	hs.e = e
	pub := e.PublicKey().Bytes()
	out = append(out, pub...)
	hs.ss.mixHash(pub)
	hs.ss.mixKey(pub) // psk handshakes mix e into the key as well
	if hs.msg == 1 {
		dh, err := hs.e.ECDH(hs.re)
		if err != nil {
			return nil, nil, nil, errors.New("noise: bad peer ephemeral key")
		}
		hs.ss.mixKey(dh)
	}
	ct, err := hs.ss.encryptAndHash(payload)
	if err != nil {
		return nil, nil, nil, err
	}
	out = append(out, ct...)
	hs.msg++
	if hs.msg == 2 {
		c1, c2 := hs.ss.split()
		return out, c1, c2, nil
	}
	return out, nil, nil, nil
}

// ReadMessage processes the next handshake message, appending the
// decrypted payload to out. On the final handshake message it also
// returns the two transport cipher states, in the order returned
// by split.
func (hs *HandshakeState) ReadMessage(out, message []byte) ([]byte, *CipherState, *CipherState, error) {
	if hs.initiator == (hs.msg == 0) || hs.msg > 1 {
		return nil, nil, nil, errors.New("noise: out of turn")
	}
	if hs.msg == 0 {
		hs.ss.mixKeyAndHash(hs.psk)
	}
	if len(message) < 32 {
		return nil, nil, nil, errors.New("noise: message too short")
	}
	pub, rest := message[:32], message[32:]
	re, err := ecdh.X25519().NewPublicKey(pub)
	if err != nil {
		return nil, nil, nil, errors.New("noise: bad peer ephemeral key")
	}
	hs.re = re
	hs.ss.mixHash(pub)
	hs.ss.mixKey(pub) // psk handshakes mix e into the key as well
	if hs.msg == 1 {
		dh, err := hs.e.ECDH(hs.re)
		if err != nil {
			return nil, nil, nil, errors.New("noise: bad peer ephemeral key")
		}
		hs.ss.mixKey(dh)
	}
	pt, err := hs.ss.decryptAndHash(rest)
	if err != nil {
		return nil, nil, nil, err
	}
	out = append(out, pt...)
	hs.msg++
	if hs.msg == 2 {
		c1, c2 := hs.ss.split()
		return out, c1, c2, nil
	}
	return out, nil, nil, nil
}
