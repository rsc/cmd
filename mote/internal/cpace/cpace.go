// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package cpace implements the CPace balanced password-authenticated key
// exchange, which lets two parties that share a low-entropy secret (a
// password) derive a strong shared key without exposing the password to
// offline dictionary attacks.
//
// This package implements the cipher suite CPACE-X25519-SHA512 of
// “CPace, a balanced composable PAKE”, draft-irtf-cfrg-cpace-21,
// https://datatracker.ietf.org/doc/draft-irtf-cfrg-cpace/21/.
//
// A CPace exchange is a single round trip. Each party calls [Start] with
// the shared password and the other inputs the two parties are expected to
// agree on, sends the resulting message and its associated data to the
// other party, and then calls [State.Finish] with the message and
// associated data it receives. Both parties derive the same intermediate
// session key if and only if they started with the same password, channel
// identifier, and session identifier, and each saw what the other sent.
//
//	state, msg, err := cpace.Start(&cpace.Config{
//		Role:      cpace.Initiator,
//		Password:  password,
//		ChannelID: []byte("client.example\x00server.example"),
//	})
//	if err != nil {
//		return err
//	}
//	if err := send(msg, nil); err != nil {
//		return err
//	}
//	peerMsg, peerAD, err := recv()
//	if err != nil {
//		return err
//	}
//	isk, err := state.Finish(peerMsg, peerAD)
//
// Applications should not use the intermediate session key directly.
// Instead they should derive keys from it using a key derivation function
// such as [crypto/hkdf].
//
// By itself CPace provides implicit authentication only: the exchange
// always produces a key, and a party learns that the other party used the
// same password only by observing that the two keys match. Applications
// that need explicit authentication should add a key confirmation round
// using [State.Tag] and [State.Verify], which also strengthens the forward
// secrecy of the exchange.
package cpace

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"errors"
	"io"
)

// Suite constants for CPACE-X25519-SHA512.
const (
	dsi         = "CPace255"       // domain separation identifier
	dsiISK      = dsi + "_ISK"     // domain separation identifier for the session key
	dsiSID      = "CPaceSidOutput" // domain separation identifier for the session identifier
	dsiMac      = "CPaceMac"       // domain separation identifier for the confirmation key
	blockSize   = sha512.BlockSize // input block size of H
	elementSize = 32               // size of a group element and of a scalar
)

// A Role identifies the part a party plays in a CPace exchange.
// The zero Role is invalid: every party must choose a role explicitly.
type Role int

const (
	// Initiator and Responder are the roles of the two parties in an
	// exchange with a defined message ordering, in which the initiator's
	// message precedes the responder's.
	Initiator Role = 1 + iota
	Responder

	// Symmetric is the role of both parties in an exchange without a
	// defined message ordering, in which either party may speak first.
	//
	// Using Symmetric requires the associated data of the two parties to
	// differ in every exchange; otherwise an attacker can relay a party's
	// own message back to it, making it derive a key with itself. If that
	// cannot be guaranteed, use Initiator and Responder instead.
	Symmetric
)

// A Config holds the inputs to a CPace exchange.
// All fields except Role and Password are optional.
type Config struct {
	// Role is this party's role in the exchange.
	// Both parties must agree on the pair of roles being used.
	Role Role

	// Password is the shared secret, called PRS in the draft.
	// It can be a password, or a value derived from one, for example by a
	// password hashing function. Clear text passwords should be encoded
	// according to RFC 8265.
	Password []byte

	// ChannelID identifies the channel connecting the two parties,
	// called CI in the draft. It might hold the identities of the two
	// parties, their network addresses, or a service name. It is not sent
	// on the wire, so it may contain confidential information, but both
	// parties must use the same value to derive the same key.
	//
	// If the exchange is meant to authenticate party identities, they
	// should be included here or in AD. If both identities are included
	// here, the encoding must distinguish the two roles, listing the
	// initiator's identity first.
	ChannelID []byte

	// SessionID identifies this exchange, called sid in the draft. If the
	// two parties can agree on a unique value before the exchange starts,
	// using it here binds the exchange to this session. It should not be
	// reused across exchanges. If it is left empty, [State.SessionID]
	// returns a unique identifier derived during the exchange instead.
	SessionID []byte

	// AD is this party's associated data, called ADa and ADb in the
	// draft. Unlike ChannelID, it is sent on the wire in the clear, so it
	// must not contain confidential information. It is authenticated by
	// the exchange: the two parties derive the same key only if each saw
	// what the other sent.
	AD []byte

	// Rand is the source of randomness used to sample the secret scalar.
	// If Rand is nil, [crypto/rand.Reader] is used.
	Rand io.Reader
}

// A State holds one party's state during a CPace exchange.
type State struct {
	role Role
	sid  []byte
	ad   []byte
	msg  []byte
	priv *ecdh.PrivateKey

	sidOutput []byte
	tag       []byte // this party's key confirmation tag
	peerTag   []byte // the other party's expected key confirmation tag
}

// Start starts a CPace exchange, returning the state needed to finish it
// and the message to send to the other party. The message must be sent
// along with cfg.AD, which the other party passes to [State.Finish].
func Start(cfg *Config) (*State, []byte, error) {
	switch cfg.Role {
	case Initiator, Responder, Symmetric:
	default:
		return nil, nil, errors.New("cpace: invalid role")
	}

	// G.sample_scalar is 32 uniformly random bytes;
	// X25519 clamps them as RFC 7748 requires.
	scalar := make([]byte, elementSize)
	r := cfg.Rand
	if r == nil {
		r = rand.Reader
	}
	if _, err := io.ReadFull(r, scalar); err != nil {
		return nil, nil, err
	}
	return start(cfg, scalar)
}

// start is Start with the secret scalar supplied by the caller, for tests.
func start(cfg *Config, scalar []byte) (*State, []byte, error) {
	priv, err := ecdh.X25519().NewPrivateKey(scalar)
	if err != nil {
		return nil, nil, err
	}
	g, err := ecdh.X25519().NewPublicKey(generator(cfg.Password, cfg.ChannelID, cfg.SessionID))
	if err != nil {
		// unreachable: generator always returns 32 bytes,
		// the only thing NewPublicKey checks.
		return nil, nil, err
	}
	// Y = G.scalar_mult(y, g).
	msg, err := priv.ECDH(g)
	if err != nil {
		// untested: ECDH fails only if the generator is a low-order point,
		// which the hash to the curve produces with negligible probability.
		return nil, nil, errors.New("cpace: invalid generator: " + err.Error())
	}
	s := &State{
		role: cfg.Role,
		sid:  bytes.Clone(cfg.SessionID),
		ad:   bytes.Clone(cfg.AD),
		msg:  msg,
		priv: priv,
	}
	return s, bytes.Clone(msg), nil
}

// Finish completes a CPace exchange using the message and associated data
// received from the other party, returning the intermediate session key.
//
// Finish reports an error if the message is not a valid group element or
// encodes a low-order point, which indicates either a corrupted message or
// an attempt to force a known key. It can be called only once: the secret
// scalar of an exchange must never be reused.
func (s *State) Finish(msg, ad []byte) ([]byte, error) {
	if s.priv == nil {
		return nil, errors.New("cpace: exchange already finished")
	}
	priv := s.priv
	s.priv = nil

	peer, err := ecdh.X25519().NewPublicKey(msg)
	if err != nil {
		return nil, errors.New("cpace: invalid peer message")
	}
	// K = G.scalar_mult_vfy(y, Y'), aborting if K is the neutral element,
	// which ECDH reports as an error.
	k, err := priv.ECDH(peer)
	if err != nil {
		return nil, errors.New("cpace: invalid peer message: low order point")
	}
	defer clear(k)

	ya, ada, yb, adb := s.msg, s.ad, msg, ad
	if s.role == Responder {
		ya, ada, yb, adb = yb, adb, ya, ada
	}

	// ISK = H.hash(lv_cat(G.DSI || "_ISK", sid, K) || transcript(Ya, ADa, Yb, ADb))
	b := lvCat(nil, []byte(dsiISK), s.sid, k)
	b = s.transcript(b, ya, ada, yb, adb)
	isk := sha512.Sum512(b)
	clear(b)

	// sid_output = H.hash("CPaceSidOutput" || transcript(Ya, ADa, Yb, ADb))
	b = s.transcript([]byte(dsiSID), ya, ada, yb, adb)
	sidOutput := sha512.Sum512(b)
	s.sidOutput = sidOutput[:]

	// Key confirmation tags, each computed over the message its sender sent:
	// mac_key = H.hash("CPaceMac" || sid || ISK),
	// Ta = MAC(mac_key, lv_cat(Ya, ADa)), Tb = MAC(mac_key, lv_cat(Yb, ADb)).
	b = append([]byte(dsiMac), s.sid...)
	b = append(b, isk[:]...)
	macKey := sha512.Sum512(b)
	clear(b)
	s.tag = mac(macKey[:], s.msg, s.ad)
	s.peerTag = mac(macKey[:], msg, ad)
	clear(macKey[:])

	return isk[:], nil
}

// mac returns MAC(key, lv_cat(msg, ad)).
func mac(key, msg, ad []byte) []byte {
	h := hmac.New(sha512.New, key)
	h.Write(lvCat(nil, msg, ad))
	return h.Sum(nil)
}

// Tag returns this party's key confirmation tag, to be sent to the other
// party after a successful [State.Finish], for applications that add an
// explicit key confirmation round. It returns nil if called before a
// successful Finish.
//
// Key confirmation gives the exchange explicit authentication: without it,
// a party that used the wrong password derives a key like any other and
// finds out only when the key fails to work. It also strengthens the
// forward secrecy of the exchange from weak to perfect. Each party sends
// its own tag and checks the other party's with [State.Verify], aborting
// if the check fails.
func (s *State) Tag() []byte {
	return bytes.Clone(s.tag)
}

// Verify checks the other party's key confirmation tag, reporting an error
// if it is not the expected one, which means the two parties did not derive
// the same key. It reports an error if called before a successful
// [State.Finish].
func (s *State) Verify(tag []byte) error {
	if s.peerTag == nil {
		return errors.New("cpace: exchange not finished")
	}
	if !hmac.Equal(tag, s.peerTag) {
		return errors.New("cpace: key confirmation failed")
	}
	return nil
}

// SessionID returns a public identifier for the exchange, derived from the
// exchange transcript. It is unique for honest parties and is meant for
// applications that had no session identifier available to pass to [Start].
// SessionID returns nil if called before a successful [State.Finish].
func (s *State) SessionID() []byte {
	return bytes.Clone(s.sidOutput)
}

// generatorString returns generator_string(G.DSI, PRS, CI, sid, H.s_in_bytes),
// which is lv_cat(DSI, PRS, zero_bytes(len_zpad), CI, sid). The zero padding
// fills the hash function's first input block, so that the number of bytes
// hashed does not depend on the password length.
func generatorString(prs, ci, sid []byte) []byte {
	b := lvCat(nil, []byte(dsi), prs)
	zpad := max(0, blockSize-1-len(b))
	return lvCat(b, make([]byte, zpad), ci, sid)
}

// generator returns the u-coordinate of the group generator derived from
// the password (PRS), channel identifier (CI), and session identifier (sid),
// implementing G.calculate_generator.
func generator(prs, ci, sid []byte) []byte {
	b := generatorString(prs, ci, sid)
	h := sha512.Sum512(b)
	clear(b)

	// Interpret the first 32 bytes of the hash as a field element, as in
	// decodeUCoordinate, and map it to the curve with Elligator 2.
	var u [32]byte
	copy(u[:], h[:elementSize])
	clear(h[:])
	var r fe
	feSetBytes(&r, &u)
	clear(u[:])
	g := elligator2(&r)
	return g[:]
}

// transcript appends transcript(Ya, ADa, Yb, ADb) to b, using ordered
// concatenation if the exchange has no defined message ordering.
func (s *State) transcript(b, ya, ada, yb, adb []byte) []byte {
	if s.role == Symmetric {
		return oCat(b, lvCat(nil, ya, ada), lvCat(nil, yb, adb))
	}
	return lvCat(lvCat(b, ya, ada), yb, adb)
}

// prependLen appends prepend_len(v) to b: v prefixed by its length,
// encoded using LEB128.
func prependLen(b, v []byte) []byte {
	n := len(v)
	for {
		if n < 128 {
			b = append(b, byte(n))
		} else {
			b = append(b, byte(n)&0x7f|0x80)
		}
		n >>= 7
		if n == 0 {
			break
		}
	}
	return append(b, v...)
}

// lvCat appends lv_cat(vs...) to b: the concatenation of the vs,
// each prefixed by its length.
func lvCat(b []byte, vs ...[]byte) []byte {
	for _, v := range vs {
		b = prependLen(b, v)
	}
	return b
}

// oCat appends o_cat(v1, v2) to b: the two values in lexicographic order,
// larger first, prefixed by "oc".
func oCat(b, v1, v2 []byte) []byte {
	b = append(b, "oc"...)
	if bytes.Compare(v1, v2) < 0 {
		v1, v2 = v2, v1
	}
	b = append(b, v1...)
	return append(b, v2...)
}
