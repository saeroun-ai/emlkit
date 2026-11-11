package pgpmime

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"slices"
	"strings"
	"testing"
	"time"
)

// newLen is a new-format body length.
func newLen(n int) []byte {
	switch {
	case n < 192:
		return []byte{byte(n)}
	case n < 8384:
		n -= 192
		return []byte{byte(n>>8) + 192, byte(n)}
	}
	return []byte{255, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
}

func newPkt(tag Tag, body []byte) []byte {
	return cat([]byte{0xC0 | byte(tag)}, newLen(len(body)), body)
}

func oldPkt(tag Tag, lt int, body []byte) []byte {
	h := []byte{0x80 | byte(tag)<<2 | byte(lt)}
	n := len(body)
	switch lt {
	case 0:
		h = append(h, byte(n))
	case 1:
		h = append(h, byte(n>>8), byte(n))
	case 2:
		h = append(h, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	return append(h, body...)
}

func literalBody(name string, content []byte) []byte {
	b := append([]byte{'b', byte(len(name))}, name...)
	return append(append(b, 0x65, 0x00, 0x00, 0x00), content...)
}

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func rep(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

// subpacket builds a signature subpacket with a one-octet length.
func subpacket(typ byte, content ...byte) []byte {
	return append([]byte{byte(len(content) + 1), typ}, content...)
}

func sigV4(hashed, unhashed []byte) []byte {
	b := []byte{4, 0x00, 22, 8, byte(len(hashed) >> 8), byte(len(hashed))}
	b = append(b, hashed...)
	b = append(b, byte(len(unhashed)>>8), byte(len(unhashed)))
	return append(append(b, unhashed...), 0xAB, 0xCD)
}

func rsaMaterial() []byte {
	n := append([]byte{0x08, 0x00}, rep(0xC1, 256)...)
	n[len(n)-8] = 0x11 // low 64 bits of the modulus: 11C1C1C1C1C1C1C1
	return append(n, 0x00, 0x11, 0x01, 0x00, 0x01)
}

func TestPacketFraming(t *testing.T) {
	content := bytes.Repeat([]byte("0123456789"), 100)
	lit := literalBody("f.txt", content)
	partial := cat([]byte{0xC0 | 11, 0xE9}, lit[:512], []byte{0xE8}, lit[512:768], newLen(len(lit)-768), lit[768:])
	rsaKey := cat([]byte{4, 0x60, 0, 0, 0, 1}, rsaMaterial())
	rsaFP := sha1.Sum(cat([]byte{0x99, byte(len(rsaKey) >> 8), byte(len(rsaKey))}, rsaKey))
	created := []byte{0x6A, 0x00, 0x00, 0x00}
	fp32 := rep(0x5A, 32)
	tests := []struct {
		name  string
		data  []byte
		tags  string
		codes []Code
		check func(t *testing.T, ps []Packet)
	}{
		{"old 1-octet", oldPkt(TagLiteral, 0, literalBody("a", []byte("hi"))), "literal", nil,
			func(t *testing.T, ps []Packet) {
				if l := ps[0].Literal; string(l.Data) != "hi" || l.FileName != "a" || l.Format != 'b' || l.Date.IsZero() {
					t.Errorf("literal %+v", l)
				}
			}},
		{"old 2-octet", oldPkt(TagLiteral, 1, lit), "literal", nil, literalIs(content)},
		{"old 4-octet", oldPkt(TagLiteral, 2, lit), "literal", nil, literalIs(content)},
		{"old indeterminate", oldPkt(TagLiteral, 3, lit), "literal", nil, literalIs(content)},
		{"new 2-octet", newPkt(TagLiteral, lit), "literal", nil, literalIs(content)},
		{"new 5-octet", newPkt(TagLiteral, literalBody("", rep('x', 9000))), "literal", nil, literalIs(rep('x', 9000))},
		{"partial", partial, "literal", nil, func(t *testing.T, ps []Packet) {
			literalIs(content)(t, ps)
			if !ps[0].Partial || ps[0].Body != nil || ps[0].Length != int64(len(lit)) {
				t.Errorf("packet %+v", ps[0])
			}
		}},
		{"partial, short first chunk", cat([]byte{0xC0 | 11, 0xE0}, lit[:1], newLen(len(lit)-1), lit[1:]),
			"literal", sorted(CodePacketPartialShort), literalIs(content)},
		{"partial signature", cat([]byte{0xC2, 0xE9}, rep(4, 512), []byte{0}), "signature", sorted(CodePacketPartialNotAllowed), nil},
		{"marker", oldPkt(TagMarker, 0, []byte("PGP")), "marker", nil, nil},
		{"unknown tag", newPkt(60, []byte{1, 2}), "tag-60", sorted(CodePacketUnknownTag), nil},
		{"reserved tag", oldPkt(0, 0, []byte{1}), "reserved", sorted(CodePacketUnknownTag), nil},
		{"junk after a packet", cat(oldPkt(TagMarker, 0, []byte("PGP")), []byte{0x00, 0x01}), "marker", sorted(CodePacketInvalidHeader), nil},
		{"header cut", []byte{0xCB}, "", sorted(CodePacketTruncated), nil},
		{"body cut", newPkt(TagLiteral, lit)[:100], "literal", sorted(CodePacketTruncated), func(t *testing.T, ps []Packet) {
			if !ps[0].Truncated || ps[0].Length != 97 || len(ps[0].Literal.Data) != 97-11 {
				t.Errorf("packet %+v", ps[0])
			}
		}},
		{"absurd 5-octet length", []byte{0xCB, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 'b', 0, 0, 0, 0, 0, 'x'}, "literal", sorted(CodePacketTruncated),
			func(t *testing.T, ps []Packet) {
				if ps[0].Length != 7 || ps[0].Literal.Size != 1 || string(ps[0].Literal.Data) != "x" {
					t.Errorf("packet %+v literal %+v", ps[0], ps[0].Literal)
				}
			}},
		{"absurd partial chunk", cat([]byte{0xCB, 0xFE}, literalBody("", []byte("abcd"))), "literal", sorted(CodePacketTruncated), nil},
		{"absurd hashed area", cat([]byte{0xC2, 10}, []byte{4, 0, 22, 8, 0xFF, 0xFF, 1, 2, 3, 4}), "signature", sorted(CodePacketMalformed), nil},
		{"absurd v6 hashed area", newPkt(TagSignature, []byte{6, 0, 27, 10, 0xFF, 0xFF, 0xFF, 0xFF, 1}), "signature", sorted(CodePacketMalformed), nil},
		{"absurd key material", newPkt(TagPublicKey, []byte{6, 1, 2, 3, 4, 27, 0xFF, 0xFF, 0xFF, 0xFF, 1}), "public-key", sorted(CodePacketMalformed), nil},
		{"absurd subpacket", newPkt(TagSignature, sigV4([]byte{255, 0xFF, 0xFF, 0xFF, 0xFF, 2}, nil)), "signature", sorted(CodePacketMalformed),
			func(t *testing.T, ps []Packet) {
				if s := ps[0].Signature; s == nil || s.HashPrefix != [2]byte{0xAB, 0xCD} {
					t.Errorf("signature %+v", s)
				}
			}},
		{"v4 signature", newPkt(TagSignature, sigV4(cat(subpacket(2, created...), subpacket(3, 0, 0, 0x0E, 0x10), subpacket(28, []byte("a@b")...)),
			subpacket(16, 1, 2, 3, 4, 5, 6, 7, 8))), "signature", nil,
			func(t *testing.T, ps []Packet) {
				s := ps[0].Signature
				if s.Created.Unix() != 0x6A000000 || s.Expires.Sub(s.Created) != time.Hour || s.SignerUserID != "a@b" ||
					s.IssuerKeyID != 0x0102030405060708 || s.Subpackets != 4 || s.Hash.Micalg() != "pgp-sha256" {
					t.Errorf("signature %+v", s)
				}
			}},
		{"v3 signature", newPkt(TagSignature, cat([]byte{3, 5, 0x01}, created, rep(7, 8), []byte{1, 2, 0xAB, 0xCD})), "signature", nil,
			func(t *testing.T, ps []Packet) {
				if s := ps[0].Signature; s.Type != SigText || s.IssuerKeyID != 0x0707070707070707 || s.Hash != 2 || s.PubKeyAlgo != 1 {
					t.Errorf("signature %+v", s)
				}
			}},
		{"v6 PKESK", newPkt(TagPKESK, cat([]byte{6, 33, 6}, fp32, []byte{25}, rep(0, 40))), "pkesk", nil,
			func(t *testing.T, ps []Packet) {
				if k := ps[0].EncryptedKey; k.KeyID != 0x5A5A5A5A5A5A5A5A || len(k.Fingerprint) != 32 || k.Algorithm != 25 || k.Anonymous {
					t.Errorf("key %+v", k)
				}
			}},
		{"anonymous recipients", cat(newPkt(TagPKESK, []byte{6, 0, 25, 1}), newPkt(TagPKESK, cat([]byte{3}, rep(0, 8), []byte{1, 0}))), "pkesk pkesk", nil,
			func(t *testing.T, ps []Packet) {
				if !ps[0].EncryptedKey.Anonymous || !ps[1].EncryptedKey.Anonymous || ps[1].EncryptedKey.KeyID != 0 {
					t.Errorf("keys %+v %+v", ps[0].EncryptedKey, ps[1].EncryptedKey)
				}
			}},
		{"v6 one-pass", newPkt(TagOnePass, cat([]byte{6, 0, 10, 27, 16}, rep(1, 16), fp32, []byte{1})), "one-pass-signature", nil,
			func(t *testing.T, ps []Packet) {
				if o := ps[0].OnePass; o.KeyID != 0x5A5A5A5A5A5A5A5A || o.Nested || o.Hash != 10 {
					t.Errorf("one-pass %+v", o)
				}
			}},
		{"v6 SKESK", newPkt(TagSKESK, []byte{6, 30, 9, 2, 20, 4, 1, 2}), "skesk", nil,
			func(t *testing.T, ps []Packet) {
				if k := ps[0].SymmetricKey; k.Cipher != 9 || k.AEAD != 2 || k.S2K != 4 {
					t.Errorf("skesk %+v", k)
				}
			}},
		{"SEIPD v2", newPkt(TagSEIPD, cat([]byte{2, 9, 2, 16}, rep(0, 48))), "seipd", nil,
			func(t *testing.T, ps []Packet) {
				if e := ps[0].Encrypted; e.Version != 2 || !e.Protected || e.Cipher != 9 || e.AEAD != 2 || e.ChunkSize != 1<<22 {
					t.Errorf("encrypted %+v", e)
				}
			}},
		{"LibrePGP AEAD", newPkt(TagAEAD, cat([]byte{1, 7, 2, 10}, rep(0, 40))), "aead", nil,
			func(t *testing.T, ps []Packet) {
				if e := ps[0].Encrypted; !e.Protected || e.Cipher != 7 || e.ChunkSize != 1<<16 {
					t.Errorf("encrypted %+v", e)
				}
			}},
		{"SEIPD unknown version", newPkt(TagSEIPD, []byte{9, 1, 2}), "seipd", sorted(CodePacketVersion), nil},
		{"SED", newPkt(TagSED, rep(0x33, 30)), "sed", sorted(CodeLegacyEncryption),
			func(t *testing.T, ps []Packet) {
				if s := Summarize(ps); !s.Encrypted || s.Protected {
					t.Errorf("summary %+v", s)
				}
			}},
		{"unknown compression", newPkt(TagCompressed, []byte{7, 1, 2}), "compressed", sorted(CodeCompressionUnknown), nil},
		{"corrupt ZLIB", newPkt(TagCompressed, []byte{2, 0x78, 0x9C, 0xFF, 0xFF, 0xFF}), "compressed", sorted(CodeDecompressError), nil},
		{"stored compression", newPkt(TagCompressed, cat([]byte{0}, newPkt(TagLiteral, lit))), "compressed >literal", nil,
			func(t *testing.T, ps []Packet) {
				literalIs(content)(t, ps[1:])
				if ps[0].Compressed.Size != int64(len(lit)+3) {
					t.Errorf("compressed %+v", ps[0].Compressed)
				}
			}},
		{"RSA key", cat(oldPkt(TagPublicKey, 1, rsaKey), oldPkt(TagSecretKey, 1, cat(rsaKey, []byte{0, 0x01, 0x01, 0x01}))), "public-key secret-key", nil,
			func(t *testing.T, ps []Packet) {
				k, sk := ps[0].Key, ps[1].Key
				if k.Bits != 2048 || !bytes.Equal(k.Fingerprint, rsaFP[:]) || k.KeyID != keyIDOf(rsaFP[:]) || k.Created.Unix() != 0x60000000 {
					t.Errorf("key %+v", k)
				}
				if !sk.Secret || !bytes.Equal(sk.Fingerprint, k.Fingerprint) {
					t.Errorf("secret key %+v", sk)
				}
			}},
		{"v3 RSA key", oldPkt(TagPublicKey, 1, cat([]byte{3, 0x60, 0, 0, 1, 0, 0, 1}, rsaMaterial())), "public-key", nil,
			func(t *testing.T, ps []Packet) {
				if k := ps[0].Key; k.KeyID != 0x11C1C1C1C1C1C1C1 || k.Bits != 2048 || k.Fingerprint != nil {
					t.Errorf("key %+v", k)
				}
			}},
		{"key version 9", newPkt(TagPublicKey, []byte{9, 1, 2}), "public-key", sorted(CodePacketVersion), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ps, is := ParsePackets(tt.data, nil)
			if got := tags(ps); got != tt.tags {
				t.Errorf("packets %q, want %q", got, tt.tags)
			}
			if got := codes(is); !slices.Equal(got, tt.codes) {
				t.Errorf("issues %v, want %v", is, tt.codes)
			}
			if tt.check != nil && len(ps) > 0 {
				tt.check(t, ps)
			}
		})
	}
}

func literalIs(content []byte) func(t *testing.T, ps []Packet) {
	return func(t *testing.T, ps []Packet) {
		t.Helper()
		l := ps[0].Literal
		if l == nil || !bytes.Equal(l.Data, content) || l.Size != int64(len(content)) || l.Truncated {
			t.Errorf("literal %+v", l)
		}
	}
}

func zlibBytes(b []byte) []byte {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func TestBudgets(t *testing.T) {
	bomb := newPkt(TagCompressed, cat([]byte{2}, zlibBytes(newPkt(TagLiteral, literalBody("", make([]byte, 10<<20))))))
	nested := newPkt(TagLiteral, literalBody("", []byte("deep")))
	for range 10 {
		nested = newPkt(TagCompressed, cat([]byte{0}, nested))
	}
	var markers, uids, unknown, subs []byte
	for range 500 {
		markers = append(markers, oldPkt(TagMarker, 0, []byte("PGP"))...)
		unknown = append(unknown, newPkt(50, nil)...)
	}
	for range 10 {
		uids = append(uids, newPkt(TagUserID, []byte("someone <a@b>"))...)
	}
	for range 100 {
		subs = append(subs, subpacket(100, 1)...)
	}
	tests := []struct {
		name  string
		data  []byte
		lim   Limits
		code  Code
		check func(t *testing.T, ps []Packet)
	}{
		{"decompressed bytes", bomb, Limits{MaxDecompressedBytes: 1 << 20}, CodeDecompressLimit, func(t *testing.T, ps []Packet) {
			if c := ps[0].Compressed; c.Size != 1<<20 || !c.Truncated || !ps[1].Truncated || ps[1].Literal.Size > 1<<20 {
				t.Errorf("compressed %+v literal %+v", c, ps[1])
			}
		}},
		{"depth", nested, Limits{MaxDepth: 3}, CodeDepthLimit, func(t *testing.T, ps []Packet) {
			if len(ps) != 4 || ps[3].Depth != 3 || !ps[3].Compressed.Truncated {
				t.Errorf("packets %q", tags(ps))
			}
		}},
		{"packets", markers, Limits{MaxPackets: 100}, CodePacketLimit, func(t *testing.T, ps []Packet) {
			if len(ps) != 100 {
				t.Errorf("%d packets", len(ps))
			}
		}},
		{"user IDs", uids, Limits{MaxUserIDs: 3}, CodeUserIDLimit, func(t *testing.T, ps []Packet) {
			if s := Summarize(ps); len(s.UserIDs) != 3 {
				t.Errorf("user IDs %q", s.UserIDs)
			}
		}},
		{"kept bytes", newPkt(TagLiteral, literalBody("", rep('k', 10000))), Limits{MaxKeptBytes: 100}, CodeKeptLimit, func(t *testing.T, ps []Packet) {
			if l := ps[0].Literal; len(l.Data) != 100 || l.Size != 10000 || !l.Truncated {
				t.Errorf("literal %+v", l)
			}
		}},
		{"subpackets", newPkt(TagSignature, sigV4(subs, nil)), Limits{MaxSubpackets: 10}, CodeSubpacketLimit, func(t *testing.T, ps []Packet) {
			if ps[0].Signature.Subpackets != 10 {
				t.Errorf("%d subpackets", ps[0].Signature.Subpackets)
			}
		}},
		{"issues", unknown, Limits{MaxIssues: 10}, CodeIssueLimit, nil},
		{"input", markers, Limits{MaxInputBytes: 50}, CodeInputLimit, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ps, is := ParsePackets(tt.data, &tt.lim)
			if !HasIssue(is, tt.code) {
				t.Errorf("issues %v lack %s", is, tt.code)
			}
			if tt.name == "issues" && len(is) != 11 {
				t.Errorf("%d issues", len(is))
			}
			if tt.check != nil {
				tt.check(t, ps)
			}
		})
	}

	// Armor budgets.
	block := "-----BEGIN PGP MESSAGE-----\n\nygNQR1A=\n-----END PGP MESSAGE-----\n"
	r := Parse([]byte(strings.Repeat(block, 100)), &Limits{MaxBlocks: 5})
	if len(r.Blocks) != 5 || !HasIssue(r.Issues, CodeBlockLimit) {
		t.Errorf("%d blocks, issues %v", len(r.Blocks), r.Issues)
	}
	r = Parse([]byte(strings.Replace(block, "\n\n", "\n"+strings.Repeat("Comment: c\n", 50)+"\n", 1)), &Limits{MaxArmorHeaders: 4})
	if b := r.Blocks[0]; len(b.Headers) != 4 || !HasIssue(b.Issues, CodeArmorHeaderLimit) || tags(b.Packets) != "marker" {
		t.Errorf("block %+v", b)
	}
}

// TestTruncation cuts each vector at every offset: nothing panics, and a
// cut that is not between packets is reported.
func TestTruncation(t *testing.T) {
	for _, f := range []string{"encrypted.asc", "signed-zlib.asc", "pubkey.asc", "signed-bzip2.gpg", "signed-zip.gpg", "symmetric.asc", "rfc9580-v6-cert.asc"} {
		r := Parse([]byte(vector(t, f)), nil)
		data, ps := []byte(vector(t, f)), r.Packets
		if !r.Binary {
			data, ps = r.Blocks[0].Data, r.Blocks[0].Packets
		}
		bounds := map[int]bool{0: true, len(data): true}
		for _, p := range ps {
			if p.Depth == 0 {
				bounds[int(p.Offset)] = true
			}
		}
		for i := range len(data) {
			_, is := ParsePackets(data[:i], nil)
			if !bounds[i] && len(is) == 0 {
				t.Errorf("%s cut at %d: no issue", f, i)
			}
		}
		// The armored text, cut at many offsets.
		text := vector(t, f)
		for i := 0; i < len(text); i += 7 {
			Parse([]byte(text[:i]), nil)
		}
	}
}
