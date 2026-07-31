// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cpace

import "math/bits"

// A fe is an element of the field GF(2^255-19),
// represented as four 64-bit limbs in little-endian order
// holding a value less than p = 2^255-19.
//
// All operations run in time independent of the values being operated on,
// except that exponents, which are fixed constants, are used bit by bit.
type fe [4]uint64

var (
	feZero = fe{0, 0, 0, 0}
	feOne  = fe{1, 0, 0, 0}

	// feP is the field modulus p = 2^255-19.
	feP = fe{0xffffffffffffffed, 0xffffffffffffffff, 0xffffffffffffffff, 0x7fffffffffffffff}

	// feInvExp is p-2 = 2^255-21, the exponent used for inversion.
	feInvExp = fe{0xffffffffffffffeb, 0xffffffffffffffff, 0xffffffffffffffff, 0x7fffffffffffffff}

	// feChiExp is (p-1)/2 = 2^254-10, the exponent used for the Legendre symbol.
	feChiExp = fe{0xfffffffffffffff6, 0xffffffffffffffff, 0xffffffffffffffff, 0x3fffffffffffffff}
)

// feReduce sets z = x mod p, assuming x < 2p.
func feReduce(z *fe, x *fe) {
	// t = x - p. If that borrows, x < p and we keep x instead.
	var t fe
	var b uint64
	t[0], b = bits.Sub64(x[0], feP[0], 0)
	t[1], b = bits.Sub64(x[1], feP[1], b)
	t[2], b = bits.Sub64(x[2], feP[2], b)
	t[3], b = bits.Sub64(x[3], feP[3], b)
	m := -b // all ones if x < p, all zeros otherwise
	z[0] = t[0] ^ (m & (t[0] ^ x[0]))
	z[1] = t[1] ^ (m & (t[1] ^ x[1]))
	z[2] = t[2] ^ (m & (t[2] ^ x[2]))
	z[3] = t[3] ^ (m & (t[3] ^ x[3]))
}

// feAdd sets z = x + y.
func feAdd(z, x, y *fe) {
	// x + y < 2p < 2^256, so the sum cannot overflow four limbs.
	var t fe
	var c uint64
	t[0], c = bits.Add64(x[0], y[0], 0)
	t[1], c = bits.Add64(x[1], y[1], c)
	t[2], c = bits.Add64(x[2], y[2], c)
	t[3], _ = bits.Add64(x[3], y[3], c)
	feReduce(z, &t)
}

// feSub sets z = x - y.
func feSub(z, x, y *fe) {
	var t fe
	var b uint64
	t[0], b = bits.Sub64(x[0], y[0], 0)
	t[1], b = bits.Sub64(x[1], y[1], b)
	t[2], b = bits.Sub64(x[2], y[2], b)
	t[3], b = bits.Sub64(x[3], y[3], b)

	// If the subtraction borrowed, add p back.
	m := -b
	var c uint64
	z[0], c = bits.Add64(t[0], feP[0]&m, 0)
	z[1], c = bits.Add64(t[1], feP[1]&m, c)
	z[2], c = bits.Add64(t[2], feP[2]&m, c)
	z[3], _ = bits.Add64(t[3], feP[3]&m, c)
}

// feMul sets z = x * y.
func feMul(z, x, y *fe) {
	// Schoolbook multiplication into the eight limbs of r.
	var r [8]uint64
	for i := range 4 {
		var carry uint64
		for j := range 4 {
			hi, lo := bits.Mul64(x[i], y[j])
			var c uint64
			lo, c = bits.Add64(lo, r[i+j], 0)
			hi += c
			lo, c = bits.Add64(lo, carry, 0)
			hi += c
			r[i+j] = lo
			carry = hi
		}
		r[i+4] = carry
	}

	// Fold the upper half down: 2^256 = 38 mod p.
	var h [5]uint64
	var carry uint64
	for i := range 4 {
		hi, lo := bits.Mul64(r[4+i], 38)
		var c uint64
		lo, c = bits.Add64(lo, carry, 0)
		hi += c
		h[i] = lo
		carry = hi
	}
	h[4] = carry

	var t fe
	var c uint64
	t[0], c = bits.Add64(r[0], h[0], 0)
	t[1], c = bits.Add64(r[1], h[1], c)
	t[2], c = bits.Add64(r[2], h[2], c)
	t[3], c = bits.Add64(r[3], h[3], c)

	// Fold the overflow down again. It is at most 38+1, so 38*top fits
	// in a single limb, and the addition can overflow at most once more.
	top := h[4] + c
	t[0], c = bits.Add64(t[0], top*38, 0)
	t[1], c = bits.Add64(t[1], 0, c)
	t[2], c = bits.Add64(t[2], 0, c)
	t[3], c = bits.Add64(t[3], 0, c)
	t[0], c = bits.Add64(t[0], c*38, 0)
	t[1], c = bits.Add64(t[1], 0, c)
	t[2], c = bits.Add64(t[2], 0, c)
	t[3], _ = bits.Add64(t[3], 0, c)

	// Fold bit 255 down: 2^255 = 19 mod p. Now t < 2^255+19 < 2p.
	b := t[3] >> 63
	t[3] &^= 1 << 63
	t[0], c = bits.Add64(t[0], b*19, 0)
	t[1], c = bits.Add64(t[1], 0, c)
	t[2], c = bits.Add64(t[2], 0, c)
	t[3], _ = bits.Add64(t[3], 0, c)

	feReduce(z, &t)
}

// feSquare sets z = x * x.
func feSquare(z, x *fe) {
	feMul(z, x, x)
}

// fePow sets z = x**e, for a public exponent e.
func fePow(z, x, e *fe) {
	r := feOne
	for i := 3; i >= 0; i-- {
		for j := 63; j >= 0; j-- {
			feSquare(&r, &r)
			if e[i]>>uint(j)&1 == 1 {
				feMul(&r, &r, x)
			}
		}
	}
	*z = r
}

// feInvert sets z = 1/x, or z = 0 if x = 0.
func feInvert(z, x *fe) {
	fePow(z, x, &feInvExp)
}

// feChi sets z to the Legendre symbol of x: 1 if x is a non-zero square,
// -1 if x is not a square, and 0 if x is zero.
func feChi(z, x *fe) {
	fePow(z, x, &feChiExp)
}

// feSetBytes sets z to the field element encoded by the 32 bytes of b,
// which are interpreted as a little-endian integer with bit 255 ignored,
// as in the decodeUCoordinate function of RFC 7748.
func feSetBytes(z *fe, b *[32]byte) {
	var t fe
	for i := range 4 {
		t[i] = uint64(b[8*i]) | uint64(b[8*i+1])<<8 | uint64(b[8*i+2])<<16 |
			uint64(b[8*i+3])<<24 | uint64(b[8*i+4])<<32 | uint64(b[8*i+5])<<40 |
			uint64(b[8*i+6])<<48 | uint64(b[8*i+7])<<56
	}
	t[3] &^= 1 << 63 // ignore bit 255, leaving t < 2^255 < 2p
	feReduce(z, &t)
}

// feBytes returns the 32-byte little-endian encoding of x,
// as in the encodeUCoordinate function of RFC 7748.
func feBytes(x *fe) [32]byte {
	var b [32]byte
	for i, v := range x {
		b[8*i] = byte(v)
		b[8*i+1] = byte(v >> 8)
		b[8*i+2] = byte(v >> 16)
		b[8*i+3] = byte(v >> 24)
		b[8*i+4] = byte(v >> 32)
		b[8*i+5] = byte(v >> 40)
		b[8*i+6] = byte(v >> 48)
		b[8*i+7] = byte(v >> 56)
	}
	return b
}
