// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cpace

import (
	"crypto/rand"
	"math/big"
	"testing"
)

var bigP = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))

// toBig returns x as a big.Int.
func toBig(x *fe) *big.Int {
	b := feBytes(x)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return new(big.Int).SetBytes(b[:])
}

// fromBig returns v mod p as a fe.
func fromBig(v *big.Int) *fe {
	var b [32]byte
	new(big.Int).Mod(v, bigP).FillBytes(b[:])
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	var x fe
	feSetBytes(&x, &b)
	return &x
}

// feValues returns field elements to test with: interesting edge cases
// followed by random values.
func feValues(t *testing.T) []*fe {
	t.Helper()
	var xs []*fe
	for _, v := range []*big.Int{
		big.NewInt(0),
		big.NewInt(1),
		big.NewInt(2),
		big.NewInt(38),
		new(big.Int).Sub(bigP, big.NewInt(1)),
		new(big.Int).Sub(bigP, big.NewInt(2)),
		new(big.Int).Rsh(bigP, 1),
		new(big.Int).Lsh(big.NewInt(1), 64),
		new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1)),
		new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 192), big.NewInt(1)),
		new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(1)),
	} {
		xs = append(xs, fromBig(v))
	}
	for range 64 {
		var b [32]byte
		rand.Read(b[:])
		var x fe
		feSetBytes(&x, &b)
		xs = append(xs, &x)
	}
	return xs
}

func TestFieldArith(t *testing.T) {
	xs := feValues(t)
	ops := []struct {
		name string
		fe   func(z, x, y *fe)
		big  func(x, y *big.Int) *big.Int
	}{
		{"add", feAdd, func(x, y *big.Int) *big.Int { return new(big.Int).Add(x, y) }},
		{"sub", feSub, func(x, y *big.Int) *big.Int { return new(big.Int).Sub(x, y) }},
		{"mul", feMul, func(x, y *big.Int) *big.Int { return new(big.Int).Mul(x, y) }},
	}
	for _, op := range ops {
		for _, x := range xs {
			for _, y := range xs {
				var z fe
				op.fe(&z, x, y)
				want := fromBig(op.big(toBig(x), toBig(y)))
				if z != *want {
					t.Fatalf("%s(%x, %x) = %x, want %x",
						op.name, feBytes(x), feBytes(y), feBytes(&z), feBytes(want))
				}
				if toBig(&z).Cmp(bigP) >= 0 {
					t.Fatalf("%s(%x, %x) is not reduced", op.name, feBytes(x), feBytes(y))
				}
			}
		}
	}

	// Aliasing the output with an input must work.
	for _, op := range ops {
		for _, x := range xs {
			z := *x
			op.fe(&z, &z, &z)
			want := fromBig(op.big(toBig(x), toBig(x)))
			if z != *want {
				t.Fatalf("aliased %s(%x, %x) = %x, want %x",
					op.name, feBytes(x), feBytes(x), feBytes(&z), feBytes(want))
			}
		}
	}
}

func TestFieldSquare(t *testing.T) {
	for _, x := range feValues(t) {
		var z, want fe
		feSquare(&z, x)
		feMul(&want, x, x)
		if z != want {
			t.Fatalf("square(%x) = %x, want %x", feBytes(x), feBytes(&z), feBytes(&want))
		}
	}
}

func TestFieldInvert(t *testing.T) {
	for _, x := range feValues(t) {
		var z fe
		feInvert(&z, x)
		want := new(big.Int).ModInverse(toBig(x), bigP)
		if want == nil {
			want = big.NewInt(0) // 1/0 is defined as 0
		}
		if got := toBig(&z); got.Cmp(want) != 0 {
			t.Fatalf("invert(%x) = %v, want %v", feBytes(x), got, want)
		}
	}
}

func TestFieldChi(t *testing.T) {
	for _, x := range feValues(t) {
		var z fe
		feChi(&z, x)
		// The Legendre symbol -1 is represented by p-1.
		want := new(big.Int).Mod(big.NewInt(int64(big.Jacobi(toBig(x), bigP))), bigP)
		if got := toBig(&z); got.Cmp(want) != 0 {
			t.Fatalf("chi(%x) = %v, want %v", feBytes(x), got, want)
		}
	}
}

func TestFieldBytes(t *testing.T) {
	// feSetBytes ignores bit 255 and reduces modulo p.
	for _, x := range feValues(t) {
		b := feBytes(x)
		var z fe
		feSetBytes(&z, &b)
		if z != *x {
			t.Fatalf("feSetBytes(feBytes(%x)) = %x", b, feBytes(&z))
		}
		b[31] |= 0x80
		feSetBytes(&z, &b)
		if z != *x {
			t.Fatalf("feSetBytes did not ignore bit 255 of %x", b)
		}
	}

	// Values between p and 2^255 are reduced.
	var b [32]byte
	new(big.Int).Add(bigP, big.NewInt(7)).FillBytes(b[:])
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	var z fe
	feSetBytes(&z, &b)
	if z != (fe{7, 0, 0, 0}) {
		t.Fatalf("feSetBytes(p+7) = %x, want 7", feBytes(&z))
	}
}

// TestElligator2 checks elligator2 against a straightforward
// implementation of the reference code from
// draft-irtf-cfrg-cpace-21, Appendix A.5.
func TestElligator2(t *testing.T) {
	for _, r := range feValues(t) {
		got := elligator2(r)
		want := elligator2Big(toBig(r))
		if got != want {
			t.Fatalf("elligator2(%x) = %x, want %x", feBytes(r), got, want)
		}
	}
}

// elligator2Big is elligator2 implemented using math/big:
//
//	v = -A / (1 + z*r^2)
//	epsilon = (v^3 + A*v^2 + B*v)^((q-1)/2)
//	x = epsilon*v - (1 - epsilon) * A/2
func elligator2Big(r *big.Int) [32]byte {
	q := bigP
	A := big.NewInt(486662)
	one := big.NewInt(1)
	mod := func(v *big.Int) *big.Int { return new(big.Int).Mod(v, q) }
	mul := func(x, y *big.Int) *big.Int { return mod(new(big.Int).Mul(x, y)) }
	add := func(x, y *big.Int) *big.Int { return mod(new(big.Int).Add(x, y)) }
	sub := func(x, y *big.Int) *big.Int { return mod(new(big.Int).Sub(x, y)) }

	den := add(one, mul(big.NewInt(2), mul(r, r)))
	inv := new(big.Int).ModInverse(den, q)
	if inv == nil {
		inv = big.NewInt(0)
	}
	v := mul(sub(big.NewInt(0), A), inv)

	w := add(add(mul(mul(v, v), v), mul(A, mul(v, v))), v)
	eps := new(big.Int).Exp(w, new(big.Int).Rsh(new(big.Int).Sub(q, one), 1), q)

	x := sub(mul(eps, v), mul(sub(one, eps), mul(A, new(big.Int).ModInverse(big.NewInt(2), q))))

	var b [32]byte
	x.FillBytes(b[:])
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return b
}
