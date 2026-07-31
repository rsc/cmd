// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noise

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
)

func handshake(t *testing.T, ipsk, rpsk []byte) (ic1, ic2, rc1, rc2 *CipherState, err error) {
	t.Helper()
	ihs, err := NewHandshakeState(true, ipsk)
	if err != nil {
		t.Fatal(err)
	}
	rhs, err := NewHandshakeState(false, rpsk)
	if err != nil {
		t.Fatal(err)
	}
	m1, c1, c2, err := ihs.WriteMessage(nil, []byte("payload one"))
	if err != nil || c1 != nil || c2 != nil {
		t.Fatalf("initiator WriteMessage: %v, %v, %v", err, c1, c2)
	}
	p1, c1, c2, err := rhs.ReadMessage(nil, m1)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if string(p1) != "payload one" || c1 != nil || c2 != nil {
		t.Fatalf("responder ReadMessage: %q, %v, %v", p1, c1, c2)
	}
	m2, rc1, rc2, err := rhs.WriteMessage(nil, []byte("payload two"))
	if err != nil || rc1 == nil || rc2 == nil {
		t.Fatalf("responder WriteMessage: %v", err)
	}
	p2, ic1, ic2, err := ihs.ReadMessage(nil, m2)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if string(p2) != "payload two" || ic1 == nil || ic2 == nil {
		t.Fatalf("initiator ReadMessage: %q", p2)
	}
	return ic1, ic2, rc1, rc2, nil
}

func TestHandshakeAndTransport(t *testing.T) {
	psk := make([]byte, 32)
	rand.Read(psk)
	ic1, ic2, rc1, rc2, err := handshake(t, psk, psk)
	if err != nil {
		t.Fatal(err)
	}

	// Initiator sends with c1, responder receives with c1; several messages.
	for i := 0; i < 3; i++ {
		msg := []byte(strings.Repeat("ping", i+1))
		ct, err := ic1.Encrypt(nil, nil, msg)
		if err != nil {
			t.Fatal(err)
		}
		pt, err := rc1.Decrypt(nil, nil, ct)
		if err != nil || !bytes.Equal(pt, msg) {
			t.Fatalf("i->r message %d: %q, %v", i, pt, err)
		}
	}
	// Responder sends with c2.
	ct, err := rc2.Encrypt(nil, nil, []byte("pong"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := ic2.Decrypt(nil, nil, ct)
	if err != nil || string(pt) != "pong" {
		t.Fatalf("r->i message: %q, %v", pt, err)
	}

	// Tampered ciphertext must fail.
	ct, err = ic1.Encrypt(nil, nil, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	ct[0] ^= 1
	if _, err := rc1.Decrypt(nil, nil, ct); err == nil {
		t.Fatal("tampered ciphertext decrypted")
	}
}

func TestWrongPSK(t *testing.T) {
	psk1 := make([]byte, 32)
	psk2 := make([]byte, 32)
	rand.Read(psk1)
	rand.Read(psk2)
	if _, _, _, _, err := handshake(t, psk1, psk2); err == nil {
		t.Fatal("handshake succeeded with mismatched PSKs")
	}
}

func TestBadPSKLength(t *testing.T) {
	if _, err := NewHandshakeState(true, make([]byte, 16)); err == nil {
		t.Fatal("NewHandshakeState accepted 16-byte PSK")
	}
}
