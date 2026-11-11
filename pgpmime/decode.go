package pgpmime

import (
	"compress/bzip2"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"crypto/sha256"
	"io"
	"time"
)

// A Signature is the metadata of a signature packet. None of it is
// verified: the issuer is who the packet claims made it.
type Signature struct {
	Version    int
	Type       SigType
	PubKeyAlgo PubKeyAlgo
	Hash       HashAlgo
	// Created is the signature creation time, zero when absent. Expires is
	// when it expires, zero for never.
	Created, Expires time.Time
	// IssuerKeyID is from the issuer subpacket, or implied by
	// IssuerFingerprint, or from a version 3 signature; zero when absent.
	IssuerKeyID       KeyID
	IssuerFingerprint Fingerprint
	// SignerUserID is the signer's user ID subpacket, often an address.
	SignerUserID string
	// HashPrefix is the left 16 bits of the signed hash.
	HashPrefix [2]byte
	// Subpackets is how many subpackets were read, hashed and unhashed.
	Subpackets int
}

// A OnePass is a one-pass signature packet: the announcement, before the
// signed literal data, of a signature that follows it.
type OnePass struct {
	Version     int
	Type        SigType
	Hash        HashAlgo
	PubKeyAlgo  PubKeyAlgo
	KeyID       KeyID
	Fingerprint Fingerprint // version 6 only
	// Nested reports that the packet's flag is 0: the signature also covers
	// the one-pass signatures that follow it.
	Nested bool
}

// An EncryptedKey is a public-key encrypted session key packet: one
// recipient of an encrypted message.
type EncryptedKey struct {
	Version     int
	KeyID       KeyID       // zero when anonymous
	Fingerprint Fingerprint // version 6 only
	Algorithm   PubKeyAlgo
	// Anonymous reports a wildcard recipient (gpg --throw-keyids).
	Anonymous bool
}

// A SymmetricKey is a symmetric-key encrypted session key packet: the
// message can be decrypted with a passphrase.
type SymmetricKey struct {
	Version int
	Cipher  CipherAlgo
	AEAD    AEADAlgo // versions 5 and 6
	// S2K is the string-to-key specifier type: 0 simple, 1 salted, 3
	// iterated and salted, 4 Argon2.
	S2K uint8
}

// Encrypted is the metadata of an encrypted data packet.
type Encrypted struct {
	// Version is the packet version, 0 for the versionless Symmetrically
	// Encrypted Data packet (tag 9).
	Version int
	// Protected reports integrity protection: a version 1 or 2 SEIPD packet
	// or a LibrePGP AEAD packet. Tag 9 data is not protected.
	Protected bool
	// Cipher, AEAD and ChunkSize are given by version 2 SEIPD and AEAD
	// packets; for the others the session key packet carries the cipher.
	Cipher    CipherAlgo
	AEAD      AEADAlgo
	ChunkSize int64
}

// A Key is the metadata of a public or secret key or subkey packet. For a
// secret key only the public part is reported.
type Key struct {
	Version   int
	Subkey    bool
	Secret    bool
	Created   time.Time
	Algorithm PubKeyAlgo
	// Bits is the size of an RSA, DSA or Elgamal key; Curve the curve of an
	// elliptic-curve key.
	Bits  int
	Curve string
	// Fingerprint and KeyID are computed from the public part: SHA-1 for a
	// version 4 key, SHA-256 for versions 5 and 6, and only the key ID, the
	// low 64 bits of the modulus, for an RSA version 3 key.
	Fingerprint Fingerprint
	KeyID       KeyID
}

// A Literal is a literal data packet: the content of a message.
type Literal struct {
	// Format is 'b' binary, 't' text, 'u' UTF-8 text, 'm' MIME (LibrePGP),
	// or 'l'/'1' local.
	Format byte
	// FileName is the name the sender gave; "_CONSOLE" asks that the content
	// is shown and not saved.
	FileName string
	// Date is the date the packet gives, zero when 0.
	Date time.Time
	// Data is the content, up to Limits.MaxKeptBytes for the whole parse.
	// Size is how long the content is, and Truncated whether Data holds only
	// its start.
	Data      []byte
	Size      int64
	Truncated bool
}

// Compressed is the metadata of a compressed data packet. Its content is
// walked, and its packets follow it in the list.
type Compressed struct {
	Algorithm CompressionAlgo
	// Size is how many bytes were inflated, and Truncated whether inflating
	// stopped at a limit before the content ended.
	Size      int64
	Truncated bool
}

func unixTime(b []byte) time.Time {
	if t := be32(b); t != 0 {
		return time.Unix(int64(t), 0).UTC()
	}
	return time.Time{}
}

// decode decodes the body of the packet (*ps)[idx].
func (s *state) decode(data []byte, bodyStart int, h header, idx, depth int, ps *[]Packet, is *[]Issue) {
	p := &(*ps)[idx]
	off := p.Offset
	if !p.Tag.known() {
		s.add(is, CodePacketUnknownTag, off, "packet tag %d is reserved or unknown", uint8(p.Tag))
		return
	}
	if h.partial && !dataPacket(p.Tag) {
		s.add(is, CodePacketPartialNotAllowed, off, "%s packet has a partial body length", p.Tag)
		return
	}
	b := p.Body
	malformed := func(what string) {
		s.add(is, CodePacketMalformed, off, "%s packet: %s", p.Tag, what)
	}
	version := func(v byte) {
		s.add(is, CodePacketVersion, off, "%s packet of version %d is not decoded", p.Tag, v)
	}
	switch p.Tag {
	case TagSignature:
		if len(b) == 0 {
			malformed("empty body")
			return
		}
		switch b[0] {
		case 3, 4, 5, 6:
			if sig, why := s.signature(b, off, is); sig != nil {
				p.Signature = sig
			} else {
				malformed(why)
			}
		default:
			version(b[0])
		}
	case TagOnePass:
		if len(b) == 0 {
			malformed("empty body")
			return
		}
		switch b[0] {
		case 3:
			if len(b) < 13 {
				malformed("body too short")
				return
			}
			p.OnePass = &OnePass{Version: 3, Type: SigType(b[1]), Hash: HashAlgo(b[2]),
				PubKeyAlgo: PubKeyAlgo(b[3]), KeyID: KeyID(be64(b[4:12])), Nested: b[12] == 0}
		case 6:
			if len(b) < 5 || len(b) < 5+int(b[4])+33 {
				malformed("body too short")
				return
			}
			fp := b[5+int(b[4]) : 5+int(b[4])+32]
			p.OnePass = &OnePass{Version: 6, Type: SigType(b[1]), Hash: HashAlgo(b[2]),
				PubKeyAlgo: PubKeyAlgo(b[3]), Fingerprint: clone(fp), KeyID: keyIDOf(fp),
				Nested: b[5+int(b[4])+32] == 0}
		default:
			version(b[0])
		}
	case TagPKESK:
		if len(b) == 0 {
			malformed("empty body")
			return
		}
		switch b[0] {
		case 3:
			if len(b) < 10 {
				malformed("body too short")
				return
			}
			k := KeyID(be64(b[1:9]))
			p.EncryptedKey = &EncryptedKey{Version: 3, KeyID: k, Algorithm: PubKeyAlgo(b[9]), Anonymous: k == 0}
		case 6:
			if len(b) < 3 || len(b) < 3+int(b[1]) {
				malformed("body too short")
				return
			}
			n := int(b[1])
			ek := &EncryptedKey{Version: 6, Algorithm: PubKeyAlgo(b[2+n]), Anonymous: n == 0}
			if n > 1 {
				ek.Fingerprint = clone(b[3 : 2+n])
				ek.KeyID = keyIDOf(ek.Fingerprint)
			}
			p.EncryptedKey = ek
		default:
			version(b[0])
		}
	case TagSKESK:
		switch {
		case len(b) < 3:
			malformed("body too short")
		case b[0] == 4:
			p.SymmetricKey = &SymmetricKey{Version: 4, Cipher: CipherAlgo(b[1]), S2K: b[2]}
		case b[0] == 5 && len(b) >= 4:
			p.SymmetricKey = &SymmetricKey{Version: 5, Cipher: CipherAlgo(b[1]), AEAD: AEADAlgo(b[2]), S2K: b[3]}
		case b[0] == 6 && len(b) >= 6:
			p.SymmetricKey = &SymmetricKey{Version: 6, Cipher: CipherAlgo(b[2]), AEAD: AEADAlgo(b[3]), S2K: b[5]}
		case b[0] == 5 || b[0] == 6:
			malformed("body too short")
		default:
			version(b[0])
		}
	case TagPublicKey, TagPublicSubkey, TagSecretKey, TagSecretSubkey:
		if k, why := parseKey(p.Tag, b); k != nil {
			p.Key = k
		} else if why != "" {
			malformed(why)
		} else {
			version(b[0])
		}
	case TagUserID:
		if s.userIDs >= s.lim.MaxUserIDs {
			if !s.userIDCapped {
				s.userIDCapped = true
				s.add(is, CodeUserIDLimit, off, "more than %d user IDs; the rest are not kept", s.lim.MaxUserIDs)
			}
			return
		}
		s.userIDs++
		n := s.keep(int64(len(b)))
		if n < int64(len(b)) {
			s.keptIssue(is, off)
		}
		p.UserID = string(b[:n])
	case TagSED:
		p.Encrypted = &Encrypted{}
		s.add(is, CodeLegacyEncryption, off, "data is encrypted without integrity protection")
	case TagSEIPD, TagAEAD:
		var hb [4]byte
		n, _ := io.ReadFull(newChunkReader(data, bodyStart, h), hb[:])
		if n == 0 {
			malformed("empty body")
			return
		}
		e := &Encrypted{Version: int(hb[0]), Protected: true}
		switch {
		case p.Tag == TagSEIPD && hb[0] == 1:
		case p.Tag == TagSEIPD && hb[0] == 2, p.Tag == TagAEAD && hb[0] == 1:
			if n < 4 {
				malformed("body too short")
				return
			}
			e.Cipher, e.AEAD = CipherAlgo(hb[1]), AEADAlgo(hb[2])
			if hb[3] <= 30 {
				e.ChunkSize = 1 << (hb[3] + 6)
			}
		default:
			version(hb[0])
			return
		}
		p.Encrypted = e
	case TagLiteral:
		p.Literal = s.literal(newChunkReader(data, bodyStart, h), p.Length, off, is)
	case TagCompressed:
		s.compressed(data, bodyStart, h, idx, depth, ps, is)
	}
}

func clone(b []byte) []byte { return append([]byte(nil), b...) }

func (s *state) keptIssue(is *[]Issue, off int64) {
	if !s.keptCapped {
		s.keptCapped = true
		s.add(is, CodeKeptLimit, off, "more than %d bytes of packet content; the rest is not kept", s.lim.MaxKeptBytes)
	}
}

// signature decodes a signature packet body of version 3 to 6. It returns
// nil and the reason when the body is shorter than its fields.
func (s *state) signature(b []byte, off int64, is *[]Issue) (*Signature, string) {
	sig := &Signature{Version: int(b[0])}
	if b[0] == 3 {
		if len(b) < 19 || b[1] != 5 {
			return nil, "version 3 body too short or hashed length not 5"
		}
		sig.Type, sig.Created = SigType(b[2]), unixTime(b[3:7])
		sig.IssuerKeyID = KeyID(be64(b[7:15]))
		sig.PubKeyAlgo, sig.Hash = PubKeyAlgo(b[15]), HashAlgo(b[16])
		copy(sig.HashPrefix[:], b[17:19])
		return sig, ""
	}
	if len(b) < 4 {
		return nil, "body too short"
	}
	sig.Type, sig.PubKeyAlgo, sig.Hash = SigType(b[1]), PubKeyAlgo(b[2]), HashAlgo(b[3])
	lenSize := 2
	if b[0] == 6 {
		lenSize = 4
	}
	rest := b[4:]
	var expiry uint32
	for _, hashed := range []bool{true, false} {
		if len(rest) < lenSize {
			return nil, "body too short for its subpacket length"
		}
		var n uint64
		if lenSize == 2 {
			n = uint64(be16(rest))
		} else {
			n = uint64(be32(rest))
		}
		rest = rest[lenSize:]
		if n > uint64(len(rest)) {
			return nil, "subpacket area longer than the body"
		}
		if e, ok := s.subpackets(rest[:n], hashed, sig, off, is); ok {
			if hashed {
				expiry = e
			}
		}
		rest = rest[n:]
	}
	if len(rest) < 2 {
		return nil, "body too short for the hash prefix"
	}
	copy(sig.HashPrefix[:], rest[:2])
	if expiry != 0 && !sig.Created.IsZero() {
		sig.Expires = sig.Created.Add(time.Duration(expiry) * time.Second)
	}
	return sig, ""
}

// subpackets reads a signature subpacket area into sig. It returns the
// signature expiration subpacket, if any, and false when the area was
// malformed or over the limit.
func (s *state) subpackets(b []byte, hashed bool, sig *Signature, off int64, is *[]Issue) (expiry uint32, ok bool) {
	for len(b) > 0 {
		if sig.Subpackets >= s.lim.MaxSubpackets {
			s.add(is, CodeSubpacketLimit, off, "signature has more than %d subpackets; the rest are not read", s.lim.MaxSubpackets)
			return expiry, false
		}
		var l uint64
		switch o := b[0]; {
		case o < 192:
			l, b = uint64(o), b[1:]
		case o < 255:
			if len(b) < 2 {
				s.add(is, CodePacketMalformed, off, "signature subpacket length is cut short")
				return expiry, false
			}
			l, b = uint64(o-192)<<8+uint64(b[1])+192, b[2:]
		default:
			if len(b) < 5 {
				s.add(is, CodePacketMalformed, off, "signature subpacket length is cut short")
				return expiry, false
			}
			l, b = uint64(be32(b[1:])), b[5:]
		}
		if l == 0 || l > uint64(len(b)) {
			s.add(is, CodePacketMalformed, off, "signature subpacket of length %d does not fit its area", l)
			return expiry, false
		}
		typ, c := b[0]&0x7f, b[1:l]
		b = b[l:]
		sig.Subpackets++
		switch typ {
		case 2:
			if len(c) == 4 && (hashed || sig.Created.IsZero()) {
				sig.Created = unixTime(c)
			}
		case 3:
			if len(c) == 4 && hashed {
				expiry = be32(c)
			}
		case 16:
			if len(c) == 8 && sig.IssuerKeyID == 0 {
				sig.IssuerKeyID = KeyID(be64(c))
			}
		case 28:
			if sig.SignerUserID == "" && len(c) > 0 {
				n := s.keep(int64(len(c)))
				if n < int64(len(c)) {
					s.keptIssue(is, off)
				}
				sig.SignerUserID = string(c[:n])
			}
		case 33:
			if len(c) >= 21 && sig.IssuerFingerprint == nil {
				sig.IssuerFingerprint = clone(c[1:])
				if sig.IssuerKeyID == 0 {
					sig.IssuerKeyID = keyIDOf(c[1:])
				}
			}
		}
	}
	return expiry, true
}

// mpiEnd returns the offset after the MPI at i of m.
func mpiEnd(m []byte, i int) (int, bool) {
	if i+2 > len(m) {
		return 0, false
	}
	n := (int(be16(m[i:])) + 7) / 8
	if n > len(m)-i-2 {
		return 0, false
	}
	return i + 2 + n, true
}

// publicMaterial returns the length of the public key material of algo at
// the start of m, with its size in bits or its curve.
func publicMaterial(algo PubKeyAlgo, m []byte) (n, bits int, curve string, ok bool) {
	mpis := func(count int) bool {
		if len(m) >= 2 {
			bits = int(be16(m))
		}
		for range count {
			if n, ok = mpiEnd(m, n); !ok {
				return false
			}
		}
		return true
	}
	oid := func() bool {
		if len(m) < 1 || m[0] == 0 || m[0] == 0xff || len(m) < 1+int(m[0]) {
			return false
		}
		curve = curveName(m[1 : 1+int(m[0])])
		n = 1 + int(m[0])
		return true
	}
	fixed := func(size int, c string) bool {
		curve = c
		n = size
		return len(m) >= size
	}
	switch algo {
	case 1, 2, 3:
		ok = mpis(2)
	case 16, 20:
		ok = mpis(3)
	case 17:
		ok = mpis(4)
	case 19, 22:
		ok = oid()
		if ok {
			n, ok = mpiEnd(m, n)
		}
	case 18:
		ok = oid()
		if ok {
			n, ok = mpiEnd(m, n)
		}
		if ok && (n >= len(m) || n+1+int(m[n]) > len(m)) {
			ok = false
		} else if ok {
			n += 1 + int(m[n])
		}
	case 25:
		ok = fixed(32, "X25519")
	case 26:
		ok = fixed(56, "X448")
	case 27:
		ok = fixed(32, "Ed25519")
	case 28:
		ok = fixed(57, "Ed448")
	}
	return n, bits, curve, ok
}

// parseKey decodes a key packet. It returns nil and "" for a version it does
// not decode, nil and the reason for a body shorter than its fields.
func parseKey(tag Tag, b []byte) (*Key, string) {
	if len(b) == 0 {
		return nil, "empty body"
	}
	k := &Key{Version: int(b[0]), Subkey: tag == TagPublicSubkey || tag == TagSecretSubkey,
		Secret: tag == TagSecretKey || tag == TagSecretSubkey}
	public := tag == TagPublicKey || tag == TagPublicSubkey
	switch b[0] {
	case 2, 3:
		if len(b) < 8 {
			return nil, "body too short"
		}
		k.Created, k.Algorithm = unixTime(b[1:5]), PubKeyAlgo(b[7])
		m := b[8:]
		if _, bits, _, ok := publicMaterial(k.Algorithm, m); ok {
			k.Bits = bits
			if k.Algorithm <= 3 {
				if end, _ := mpiEnd(m, 0); end >= 10 {
					k.KeyID = KeyID(be64(m[end-8 : end]))
				}
			}
		}
	case 4:
		if len(b) < 6 {
			return nil, "body too short"
		}
		k.Created, k.Algorithm = unixTime(b[1:5]), PubKeyAlgo(b[5])
		n, bits, curve, ok := publicMaterial(k.Algorithm, b[6:])
		k.Bits, k.Curve = bits, curve
		pub := b
		if !public {
			if !ok {
				return k, ""
			}
			pub = b[:6+n]
		}
		if len(pub) > 0xffff {
			return k, ""
		}
		h := sha1.New()
		h.Write([]byte{0x99, byte(len(pub) >> 8), byte(len(pub))})
		h.Write(pub)
		k.Fingerprint = h.Sum(nil)
		k.KeyID = keyIDOf(k.Fingerprint)
	case 5, 6:
		if len(b) < 10 {
			return nil, "body too short"
		}
		k.Created, k.Algorithm = unixTime(b[1:5]), PubKeyAlgo(b[5])
		ml := uint64(be32(b[6:10]))
		if ml > uint64(len(b)-10) {
			return nil, "key material longer than the body"
		}
		_, k.Bits, k.Curve, _ = publicMaterial(k.Algorithm, b[10:10+ml])
		pub := b[:10+ml]
		h := sha256.New()
		prefix := byte(0x9A)
		if b[0] == 6 {
			prefix = 0x9B
		}
		h.Write([]byte{prefix, byte(len(pub) >> 24), byte(len(pub) >> 16), byte(len(pub) >> 8), byte(len(pub))})
		h.Write(pub)
		k.Fingerprint = h.Sum(nil)
		k.KeyID = keyIDOf(k.Fingerprint)
	default:
		return nil, ""
	}
	return k, ""
}

// literal decodes a literal data packet of length bytes from r.
func (s *state) literal(r io.Reader, length int64, off int64, is *[]Issue) *Literal {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		s.add(is, CodePacketMalformed, off, "literal packet: body too short")
		return nil
	}
	nameBuf := make([]byte, h[1])
	var date [4]byte
	if _, err := io.ReadFull(r, nameBuf); err != nil {
		s.add(is, CodePacketMalformed, off, "literal packet: body too short for its file name")
		return nil
	}
	if _, err := io.ReadFull(r, date[:]); err != nil {
		s.add(is, CodePacketMalformed, off, "literal packet: body too short for its date")
		return nil
	}
	l := &Literal{Format: h[0], FileName: string(nameBuf), Date: unixTime(date[:])}
	l.Size = length - int64(6+len(nameBuf))
	if l.Size < 0 {
		l.Size = 0
	}
	n := s.keep(l.Size)
	if n < l.Size {
		l.Truncated = true
		s.keptIssue(is, off)
	}
	l.Data = make([]byte, n)
	got, _ := io.ReadFull(r, l.Data)
	l.Data = l.Data[:got]
	return l
}

// compressed inflates the compressed packet (*ps)[idx] under the budget and
// walks its content one level deeper.
func (s *state) compressed(data []byte, bodyStart int, h header, idx, depth int, ps *[]Packet, is *[]Issue) {
	p := &(*ps)[idx]
	off := p.Offset
	r := newChunkReader(data, bodyStart, h)
	var a [1]byte
	if _, err := io.ReadFull(r, a[:]); err != nil {
		s.add(is, CodePacketMalformed, off, "compressed packet: empty body")
		return
	}
	c := &Compressed{Algorithm: CompressionAlgo(a[0])}
	p.Compressed = c
	if depth >= s.lim.MaxDepth {
		c.Truncated = true
		s.add(is, CodeDepthLimit, off, "compressed packets nest deeper than %d; this one is not inflated", s.lim.MaxDepth)
		return
	}
	var src io.Reader
	switch a[0] {
	case 0:
		src = r
	case 1:
		src = flate.NewReader(r)
	case 2:
		zr, err := zlib.NewReader(r)
		if err != nil {
			s.add(is, CodeDecompressError, off, "ZLIB header: %v", err)
			return
		}
		src = zr
	case 3:
		src = bzip2.NewReader(r)
	default:
		s.add(is, CodeCompressionUnknown, off, "compression algorithm %d is unknown", a[0])
		return
	}
	left := s.lim.MaxDecompressedBytes - s.inflated
	if left <= 0 {
		c.Truncated = true
		s.add(is, CodeDecompressLimit, off, "more than %d bytes inflated; this packet is not", s.lim.MaxDecompressedBytes)
		return
	}
	out, err := readCapped(src, left)
	if int64(len(out)) > left {
		out = out[:left]
		c.Truncated = true
		s.add(is, CodeDecompressLimit, off, "inflating stopped at the %d-byte budget", s.lim.MaxDecompressedBytes)
	}
	s.inflated += int64(len(out))
	c.Size = int64(len(out))
	if err != nil {
		s.add(is, CodeDecompressError, off, "%s data is corrupt after %d bytes: %v", c.Algorithm, len(out), err)
	}
	s.walk(out, depth+1, ps, is)
}

// readCapped reads r to its end, or to max+1 bytes, whichever is first.
func readCapped(r io.Reader, max int64) ([]byte, error) {
	limit := max + 1
	buf := make([]byte, 0, min(limit, 32<<10))
	for idle := 0; idle < 100; {
		if int64(len(buf)) == limit {
			return buf, nil
		}
		if len(buf) == cap(buf) {
			grown := make([]byte, len(buf), min(limit, int64(cap(buf))*2))
			copy(grown, buf)
			buf = grown
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return buf, err
		}
		if n == 0 {
			idle++
		} else {
			idle = 0
		}
	}
	return buf, io.ErrNoProgress
}
