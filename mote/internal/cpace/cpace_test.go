// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cpace

import (
	"bytes"
	"crypto/ecdh"
	"encoding/hex"
	"strings"
	"testing"
)

// unhex decodes a hexadecimal string, ignoring spaces and newlines.
func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Test vectors from draft-irtf-cfrg-cpace-21, Appendix B.1,
// for CPace using group X25519 and hash SHA-512.
const (
	tvPRS = "Password"
	tvCI  = "\x0bA_initiator\x0bB_responder"
	tvSID = "7e4b4791d6a8ef019b936c79fb7f2c57"

	tvGenString = `
		0843506163653235350850617373776f72646d000000000000000000
		00000000000000000000000000000000000000000000000000000000
		00000000000000000000000000000000000000000000000000000000
		00000000000000000000000000000000000000000000000000000000
		00000000000000000000000000000000180b415f696e69746961746f
		720b425f726573706f6e646572107e4b4791d6a8ef019b936c79fb7f
		2c57`
	tvGenHash = "03998087bdb1a2617bbe25ef5a7c18cd4f84f902328701790958755ee4aed1d3"
	tvGen     = "d04bf6d41f6a289632a2e929fa29bebd51092512a7829fdde7d314b62f05a73f"

	tvADa = "ADa"
	tvYa  = "21b4f4bd9e64ed355c3eb676a28ebedaf6d8f17bdc365995b319097153044080"
	tvMa  = "1d13c89278cdadd826f6d8d7f887701430f8380ddc17611cdd6dc989ce0c9f32"

	tvADb = "ADb"
	tvYb  = "848b0779ff415f0af4ea14df9dd1d3c29ac41d836c7808896c4eba19c51ac40a"
	tvMb  = "248cccf6d5cdc3646f0ad593f9e6cef4e69d4945f8372e623512ecea32185623"

	tvK = "5b067effbdc0b2a0e1d907b21ebb25cfedb96a852179a847c37e43ee71322c6b"

	tvTranscriptIR = `
		201d13c89278cdadd826f6d8d7f887701430f8380ddc17611cdd6dc9
		89ce0c9f320341446120248cccf6d5cdc3646f0ad593f9e6cef4e69d
		4945f8372e623512ecea3218562303414462`
	tvISKInput = `
		0c43506163653235355f49534b107e4b4791d6a8ef019b936c79fb7f
		2c57205b067effbdc0b2a0e1d907b21ebb25cfedb96a852179a847c3
		7e43ee71322c6b201d13c89278cdadd826f6d8d7f887701430f8380d
		dc17611cdd6dc989ce0c9f320341446120248cccf6d5cdc3646f0ad5
		93f9e6cef4e69d4945f8372e623512ecea3218562303414462`
	tvISKIR = `
		6e19b875f7a561d6b3ca3dbb9ef42ac55de3e717881018204b8922b4
		d5e53bb2aa82c300bea7b65d2b671da71922ddf6472301b79bc270ad
		fa8bf413285f2263`
	tvSIDOutputIR = `
		cbc73f62589bbc96ab6a95ec2363df621e93bc3b0cea83ba6b9571d0
		05fa8f5d2d08f7165622777fa484c02a9e6b20a84ee2dbebae8c53be
		757dcfc0eebdeb5f`

	tvTranscriptOC = `
		6f6320248cccf6d5cdc3646f0ad593f9e6cef4e69d4945f8372e6235
		12ecea3218562303414462201d13c89278cdadd826f6d8d7f8877014
		30f8380ddc17611cdd6dc989ce0c9f3203414461`
	tvISKSY = `
		eef745e2f6e7ae2b1a1e53da340e777167a07fe150436648c51fb199
		c11f3cbabfc683a2b48e1af5881940dc398d375c95e6b4ae9948a45b
		8770de0656382be4`
	tvSIDOutputOC = `
		3a504e9c7f1f7fa7314861e2c487d13f28566f3043f0ca760d22c491
		1aca0dd8b1f12a7ad0862eb92d08a76120140412ae6b8322e99d75cf
		1d20d8cfde2b40fe`
)

// TestPrependLen checks the prepend_len test vectors
// from draft-irtf-cfrg-cpace-21, Appendix A.1.2.
func TestPrependLen(t *testing.T) {
	r127 := make([]byte, 127)
	for i := range r127 {
		r127[i] = byte(i)
	}
	r128 := append(bytes.Clone(r127), 127)

	tests := []struct {
		in  []byte
		out string
	}{
		{[]byte(""), "00"},
		{[]byte("1234"), "0431323334"},
		{r127, "7f" + hex.EncodeToString(r127)},
		{r128, "8001" + hex.EncodeToString(r128)},
	}
	for _, tt := range tests {
		if out := prependLen(nil, tt.in); !bytes.Equal(out, unhex(t, tt.out)) {
			t.Errorf("prependLen(%x) = %x, want %s", tt.in, out, tt.out)
		}
	}
}

// TestLVCat checks the lv_cat test vector
// from draft-irtf-cfrg-cpace-21, Appendix A.1.4.
func TestLVCat(t *testing.T) {
	out := lvCat(nil, []byte("1234"), []byte("5"), []byte(""), []byte("678"))
	if want := unhex(t, "043132333401350003363738"); !bytes.Equal(out, want) {
		t.Errorf("lvCat = %x, want %x", out, want)
	}
}

// TestOCat checks the o_cat test vectors
// from draft-irtf-cfrg-cpace-21, Appendix A.3.3.
func TestOCat(t *testing.T) {
	tests := []struct {
		v1, v2 string
		out    string
	}{
		{"ABCD", "BCD", "6f6342434441424344"},
		{"BCD", "ABCDE", "6f634243444142434445"},
	}
	for _, tt := range tests {
		if out := oCat(nil, []byte(tt.v1), []byte(tt.v2)); !bytes.Equal(out, unhex(t, tt.out)) {
			t.Errorf("oCat(%q, %q) = %x, want %s", tt.v1, tt.v2, out, tt.out)
		}
	}
}

// TestTranscript checks the transcript_ir and transcript_oc test vectors
// from draft-irtf-cfrg-cpace-21, Appendix A.3.5 and A.3.7.
func TestTranscript(t *testing.T) {
	tests := []struct {
		role    Role
		ya, ada string
		yb, adb string
		out     string
	}{
		{Initiator, "123", "PartyA", "234", "PartyB", "03313233065061727479410332333406506172747942"},
		{Initiator, "3456", "PartyA", "2345", "PartyB", "043334353606506172747941043233343506506172747942"},
		{Symmetric, "123", "PartyA", "234", "PartyB", "6f6303323334065061727479420331323306506172747941"},
		{Symmetric, "3456", "PartyA", "2345", "PartyB", "6f63043334353606506172747941043233343506506172747942"},
	}
	for _, tt := range tests {
		s := &State{role: tt.role}
		out := s.transcript(nil, []byte(tt.ya), []byte(tt.ada), []byte(tt.yb), []byte(tt.adb))
		if !bytes.Equal(out, unhex(t, tt.out)) {
			t.Errorf("transcript(%v, %q, %q, %q, %q) = %x, want %s",
				tt.role, tt.ya, tt.ada, tt.yb, tt.adb, out, tt.out)
		}
	}
}

// TestGenerator checks the calculate_generator test vectors
// from draft-irtf-cfrg-cpace-21, Appendix B.1.1.
func TestGenerator(t *testing.T) {
	prs, ci, sid := []byte(tvPRS), []byte(tvCI), unhex(t, tvSID)

	s := generatorString(prs, ci, sid)
	if want := unhex(t, tvGenString); !bytes.Equal(s, want) {
		t.Fatalf("generatorString = %x, want %x", s, want)
	}
	g := generator(prs, ci, sid)
	if want := unhex(t, tvGen); !bytes.Equal(g, want) {
		t.Errorf("generator = %x, want %x", g, want)
	}

	// The hash of the generator string is mapped to the curve by Elligator 2.
	var u [32]byte
	copy(u[:], unhex(t, tvGenHash))
	var r fe
	feSetBytes(&r, &u)
	if got := elligator2(&r); !bytes.Equal(got[:], unhex(t, tvGen)) {
		t.Errorf("elligator2 = %x, want %s", got, tvGen)
	}
}

// TestVectors checks the full protocol test vectors
// from draft-irtf-cfrg-cpace-21, Appendix B.1.2 to B.1.7.
func TestVectors(t *testing.T) {
	tests := []struct {
		name      string
		a, b      Role
		isk       string
		sidOutput string
	}{
		{"initiator-responder", Initiator, Responder, tvISKIR, tvSIDOutputIR},
		{"symmetric", Symmetric, Symmetric, tvISKSY, tvSIDOutputOC},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Password:  []byte(tvPRS),
				ChannelID: []byte(tvCI),
				SessionID: unhex(t, tvSID),
			}

			cfgA := *cfg
			cfgA.Role, cfgA.AD = tt.a, []byte(tvADa)
			a, ma, err := start(&cfgA, unhex(t, tvYa))
			if err != nil {
				t.Fatal(err)
			}
			if want := unhex(t, tvMa); !bytes.Equal(ma, want) {
				t.Fatalf("Ya = %x, want %x", ma, want)
			}

			cfgB := *cfg
			cfgB.Role, cfgB.AD = tt.b, []byte(tvADb)
			b, mb, err := start(&cfgB, unhex(t, tvYb))
			if err != nil {
				t.Fatal(err)
			}
			if want := unhex(t, tvMb); !bytes.Equal(mb, want) {
				t.Fatalf("Yb = %x, want %x", mb, want)
			}

			if got := a.SessionID(); got != nil {
				t.Errorf("SessionID before Finish = %x, want nil", got)
			}

			iskA, err := a.Finish(mb, []byte(tvADb))
			if err != nil {
				t.Fatal(err)
			}
			iskB, err := b.Finish(ma, []byte(tvADa))
			if err != nil {
				t.Fatal(err)
			}
			if want := unhex(t, tt.isk); !bytes.Equal(iskA, want) {
				t.Errorf("A: ISK = %x, want %x", iskA, want)
			}
			if !bytes.Equal(iskA, iskB) {
				t.Errorf("A and B derived different keys:\n%x\n%x", iskA, iskB)
			}
			want := unhex(t, tt.sidOutput)
			if got := a.SessionID(); !bytes.Equal(got, want) {
				t.Errorf("A: SessionID = %x, want %x", got, want)
			}
			if got := b.SessionID(); !bytes.Equal(got, want) {
				t.Errorf("B: SessionID = %x, want %x", got, want)
			}

			// The exchange cannot be finished twice:
			// the secret scalar must not be reused.
			if _, err := a.Finish(mb, []byte(tvADb)); err == nil {
				t.Errorf("second Finish succeeded")
			}
		})
	}
}

// TestSecretPoint checks the test vector for the secret point K
// from draft-irtf-cfrg-cpace-21, Appendix B.1.4, and that the input
// hashed to derive the session key is the one from Appendix B.1.5.
func TestSecretPoint(t *testing.T) {
	k := scalarMultVfy(t, unhex(t, tvYa), unhex(t, tvMb))
	if want := unhex(t, tvK); !bytes.Equal(k, want) {
		t.Errorf("scalar_mult_vfy(ya, Yb) = %x, want %x", k, want)
	}
	k = scalarMultVfy(t, unhex(t, tvYb), unhex(t, tvMa))
	if want := unhex(t, tvK); !bytes.Equal(k, want) {
		t.Errorf("scalar_mult_vfy(yb, Ya) = %x, want %x", k, want)
	}

	s := &State{role: Initiator}
	b := lvCat(nil, []byte(dsiISK), unhex(t, tvSID), k)
	b = s.transcript(b, unhex(t, tvMa), []byte(tvADa), unhex(t, tvMb), []byte(tvADb))
	if want := unhex(t, tvISKInput); !bytes.Equal(b, want) {
		t.Errorf("ISK input = %x, want %x", b, want)
	}
	if want := unhex(t, tvTranscriptIR); !bytes.Equal(b[len(b)-len(want):], want) {
		t.Errorf("transcript_ir = %x, want %x", b[len(b)-len(want):], want)
	}

	s = &State{role: Symmetric}
	b = s.transcript(nil, unhex(t, tvMa), []byte(tvADa), unhex(t, tvMb), []byte(tvADb))
	if want := unhex(t, tvTranscriptOC); !bytes.Equal(b, want) {
		t.Errorf("transcript_oc = %x, want %x", b, want)
	}
}

// scalarMultVfy returns G.scalar_mult_vfy(y, u), or nil if it is the
// neutral element.
func scalarMultVfy(t *testing.T, y, u []byte) []byte {
	t.Helper()
	priv, err := ecdh.X25519().NewPrivateKey(y)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ecdh.X25519().NewPublicKey(u)
	if err != nil {
		t.Fatal(err)
	}
	k, err := priv.ECDH(pub)
	if err != nil {
		return nil
	}
	return k
}

// TestLowOrder checks the scalar_mult_vfy test vectors for low order points
// from draft-irtf-cfrg-cpace-21, Appendix B.1.10. The values u0 through u5
// and u7 encode low order points on the curve or on its quadratic twist,
// including non-canonical encodings with bit 255 set, and must make a party
// receiving them abort. The remaining values must be accepted.
func TestLowOrder(t *testing.T) {
	s := unhex(t, "af46e36bf0527c9d3b16154b82465edd62144c0ac1fc5a18506a2244ba449aff")
	tests := []struct {
		u, q string
	}{
		{"0000000000000000000000000000000000000000000000000000000000000000", ""},
		{"0100000000000000000000000000000000000000000000000000000000000000", ""},
		{"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f", ""},
		{"e0eb7a7c3b41b8ae1656e3faf19fc46ada098deb9c32b1fd866205165f49b800", ""},
		{"5f9c95bca3508c24b1d0b1559c83ef5b04445cc4581c8e86d8224eddd09f1157", ""},
		{"edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f", ""},
		{"daffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
			"d8e2c776bbacd510d09fd9278b7edcd25fc5ae9adfba3b6e040e8d3b71b21806"},
		{"eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f", ""},
		{"dbffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
			"c85c655ebe8be44ba9c0ffde69f2fe10194458d137f09bbff725ce58803cdb38"},
		{"d9ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
			"db64dafa9b8fdd136914e61461935fe92aa372cb056314e1231bc4ec12417456"},
		{"cdeb7a7c3b41b8ae1656e3faf19fc46ada098deb9c32b1fd866205165f49b880",
			"e062dcd5376d58297be2618c7498f55baa07d7e03184e8aada20bca28888bf7a"},
		{"4c9c95bca3508c24b1d0b1559c83ef5b04445cc4581c8e86d8224eddd09f11d7",
			"993c6ad11c4c29da9a56f7691fd0ff8d732e49de6250b6c2e80003ff4629a175"},
	}
	for i, tt := range tests {
		u := unhex(t, tt.u)
		q := scalarMultVfy(t, s, u)
		if tt.q == "" {
			if q != nil {
				t.Errorf("u%x: scalar_mult_vfy = %x, want neutral element", i, q)
			}
		} else if want := unhex(t, tt.q); !bytes.Equal(q, want) {
			t.Errorf("u%x: scalar_mult_vfy = %x, want %x", i, q, want)
		}

		// A party receiving a low order point must abort.
		state, _, err := Start(&Config{Role: Initiator, Password: []byte(tvPRS)})
		if err != nil {
			t.Fatal(err)
		}
		_, err = state.Finish(u, nil)
		if (err == nil) != (tt.q != "") {
			t.Errorf("u%x: Finish error = %v, want error: %v", i, err, tt.q == "")
		}
	}
}

// TestExchange runs complete exchanges with random scalars and checks that
// the two parties agree if and only if they started from the same inputs.
func TestExchange(t *testing.T) {
	cfg := &Config{
		Password:  []byte("password"),
		ChannelID: []byte("client\x00server"),
		SessionID: []byte("session"),
	}
	roles := [][2]Role{{Initiator, Responder}, {Symmetric, Symmetric}}
	for _, role := range roles {
		isk, _ := exchange(t, cfg, role, func(cfg *Config) {})
		if len(isk) != 64 {
			t.Fatalf("ISK is %d bytes, want 64", len(isk))
		}

		// Any disagreement about the secret inputs changes the derived keys.
		changes := map[string]func(*Config){
			"password":  func(c *Config) { c.Password = []byte("Password") },
			"channelID": func(c *Config) { c.ChannelID = []byte("client\x00Server") },
			"sessionID": func(c *Config) { c.SessionID = []byte("Session") },
		}
		for name, change := range changes {
			iskA, iskB := exchange(t, cfg, role, change)
			if bytes.Equal(iskA, iskB) {
				t.Errorf("%v: differing %s produced the same key", role, name)
			}
		}
	}

	// Both parties must agree on the pair of roles being used.
	if iskA, iskB := exchange(t, cfg, [2]Role{Initiator, Initiator}, func(*Config) {}); bytes.Equal(iskA, iskB) {
		t.Errorf("two initiators produced the same key")
	}
}

// TestTamper checks that the messages and associated data are authenticated:
// a party that does not see what the other party sent derives a different key.
func TestTamper(t *testing.T) {
	cfg := &Config{Password: []byte("password")}
	cfgA := *cfg
	cfgA.Role, cfgA.AD = Initiator, []byte("a")
	cfgB := *cfg
	cfgB.Role, cfgB.AD = Responder, []byte("b")

	for _, what := range []string{"message", "ad"} {
		a, ma, err := Start(&cfgA)
		if err != nil {
			t.Fatal(err)
		}
		b, mb, err := Start(&cfgB)
		if err != nil {
			t.Fatal(err)
		}
		ad := cfgB.AD
		if what == "ad" {
			ad = []byte("B")
		} else {
			mb = bytes.Clone(mb)
			mb[0] ^= 1
		}
		iskA, err := a.Finish(mb, ad)
		if err != nil {
			t.Fatal(err)
		}
		iskB, err := b.Finish(ma, cfgA.AD)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(iskA, iskB) {
			t.Errorf("tampered %s produced the same key", what)
		}
	}
}

// exchange runs a complete exchange between two parties with the given
// configuration and roles, applying change to the second party's
// configuration, and returns the keys the two parties derived.
func exchange(t *testing.T, cfg *Config, roles [2]Role, change func(*Config)) (iskA, iskB []byte) {
	t.Helper()
	cfgA := *cfg
	cfgA.Role = roles[0]
	cfgB := *cfg
	cfgB.Role = roles[1]
	change(&cfgB)

	a, ma, err := Start(&cfgA)
	if err != nil {
		t.Fatal(err)
	}
	b, mb, err := Start(&cfgB)
	if err != nil {
		t.Fatal(err)
	}
	if iskA, err = a.Finish(mb, cfgB.AD); err != nil {
		t.Fatal(err)
	}
	if iskB, err = b.Finish(ma, cfgA.AD); err != nil {
		t.Fatal(err)
	}
	return iskA, iskB
}

// TestConfirm checks the key confirmation round of
// draft-irtf-cfrg-cpace-21, section 10.4.
func TestConfirm(t *testing.T) {
	cfg := &Config{
		Password:  []byte("password"),
		ChannelID: []byte("client\x00server"),
		SessionID: []byte("session"),
	}
	roles := [][2]Role{{Initiator, Responder}, {Symmetric, Symmetric}}
	for _, role := range roles {
		// Parties that derived the same key confirm each other.
		a, b := parties(t, cfg, role, func(*Config) {})
		if err := a.Verify(b.Tag()); err != nil {
			t.Errorf("%v: A: Verify: %v", role, err)
		}
		if err := b.Verify(a.Tag()); err != nil {
			t.Errorf("%v: B: Verify: %v", role, err)
		}

		// The two tags cover different messages, so they differ.
		if bytes.Equal(a.Tag(), b.Tag()) {
			t.Errorf("%v: both parties produced the same tag", role)
		}
		if len(a.Tag()) != 64 {
			t.Errorf("%v: tag is %d bytes, want 64", role, len(a.Tag()))
		}

		// Parties that derived different keys do not.
		a, b = parties(t, cfg, role, func(c *Config) { c.Password = []byte("Password") })
		if err := a.Verify(b.Tag()); err == nil {
			t.Errorf("%v: A: Verify with the wrong password succeeded", role)
		}
		if err := b.Verify(a.Tag()); err == nil {
			t.Errorf("%v: B: Verify with the wrong password succeeded", role)
		}
	}

	// Neither tag is available before the exchange is finished,
	// and a tag of the wrong length is rejected.
	a, ma, err := Start(&Config{Role: Initiator, Password: cfg.Password})
	if err != nil {
		t.Fatal(err)
	}
	if tag := a.Tag(); tag != nil {
		t.Errorf("Tag before Finish = %x, want nil", tag)
	}
	if err := a.Verify(make([]byte, 64)); err == nil {
		t.Errorf("Verify before Finish succeeded")
	}
	b, mb, err := Start(&Config{Role: Responder, Password: cfg.Password})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Finish(mb, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Finish(ma, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(b.Tag()[:63]); err == nil {
		t.Errorf("Verify with a truncated tag succeeded")
	}
}

// parties runs a complete exchange as [exchange] does,
// returning the two parties' states.
func parties(t *testing.T, cfg *Config, roles [2]Role, change func(*Config)) (a, b *State) {
	t.Helper()
	cfgA := *cfg
	cfgA.Role, cfgA.AD = roles[0], []byte("a")
	cfgB := *cfg
	cfgB.Role, cfgB.AD = roles[1], []byte("b")
	change(&cfgB)

	a, ma, err := Start(&cfgA)
	if err != nil {
		t.Fatal(err)
	}
	b, mb, err := Start(&cfgB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Finish(mb, cfgB.AD); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Finish(ma, cfgA.AD); err != nil {
		t.Fatal(err)
	}
	return a, b
}

// TestRand checks that Config.Rand supplies the secret scalar.
func TestRand(t *testing.T) {
	_, m, err := Start(&Config{
		Role:      Initiator,
		Password:  []byte(tvPRS),
		ChannelID: []byte(tvCI),
		SessionID: unhex(t, tvSID),
		AD:        []byte(tvADa),
		Rand:      bytes.NewReader(unhex(t, tvYa)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := unhex(t, tvMa); !bytes.Equal(m, want) {
		t.Errorf("Ya = %x, want %x", m, want)
	}
}

func TestErrors(t *testing.T) {
	if _, _, err := Start(&Config{Password: []byte("password")}); err == nil {
		t.Errorf("Start with no role succeeded")
	}
	if _, _, err := Start(&Config{Role: Role(4), Password: []byte("password")}); err == nil {
		t.Errorf("Start with invalid role succeeded")
	}
	s, _, err := Start(&Config{Role: Initiator, Password: []byte("password")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Finish(make([]byte, 31), nil); err == nil {
		t.Errorf("Finish with short message succeeded")
	}
	cfg := &Config{Role: Initiator, Password: []byte("password"), Rand: bytes.NewReader(nil)}
	if _, _, err := Start(cfg); err == nil {
		t.Errorf("Start with failing Rand succeeded")
	}
	if _, _, err := start(cfg, make([]byte, 31)); err == nil {
		t.Errorf("start with short scalar succeeded")
	}
}
