// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cpace

var (
	// feA is the Curve25519 parameter A, and feAHalf is A/2.
	feA     = fe{486662, 0, 0, 0}
	feAHalf = fe{243331, 0, 0, 0}

	// feNegA is -A mod p, computed in init.
	feNegA fe
)

func init() {
	feSub(&feNegA, &feZero, &feA)
}

// elligator2 maps the field element r to the u-coordinate of a point
// on Curve25519, using the Elligator 2 map with the non-square Z = 2,
// as specified in draft-irtf-cfrg-cpace-21, Appendix A.5:
//
//	v = -A / (1 + Z*r^2)
//	epsilon = (v^3 + A*v^2 + B*v)^((q-1)/2)
//	u = epsilon*v - (1 - epsilon) * A/2
//
// The curve parameter B is 1, and epsilon is the Legendre symbol of the
// curve equation evaluated at v, so it is 1 if v is the u-coordinate of a
// curve point and -1 if it is not. In the latter case -v-A is used instead,
// which then is a u-coordinate on the curve.
func elligator2(r *fe) [32]byte {
	// v = -A / (1 + 2*r^2)
	var v, t fe
	feSquare(&t, r)
	feAdd(&t, &t, &t)
	feAdd(&t, &t, &feOne)
	feInvert(&t, &t)
	feMul(&v, &feNegA, &t)

	// w = v^3 + A*v^2 + v = v * (v^2 + A*v + 1)
	var w fe
	feSquare(&w, &v)
	feMul(&t, &feA, &v)
	feAdd(&w, &w, &t)
	feAdd(&w, &w, &feOne)
	feMul(&w, &w, &v)

	// epsilon = w^((p-1)/2)
	var eps fe
	feChi(&eps, &w)

	// u = epsilon*v - (1 - epsilon) * A/2
	var u fe
	feMul(&u, &eps, &v)
	feSub(&t, &feOne, &eps)
	feMul(&t, &t, &feAHalf)
	feSub(&u, &u, &t)

	return feBytes(&u)
}
