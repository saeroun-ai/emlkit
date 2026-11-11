package pgpmime

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// A Tag is an OpenPGP packet type (RFC 9580 §5).
type Tag uint8

// Packet tags.
const (
	TagReserved      Tag = 0
	TagPKESK         Tag = 1 // Public-Key Encrypted Session Key
	TagSignature     Tag = 2
	TagSKESK         Tag = 3 // Symmetric-Key Encrypted Session Key
	TagOnePass       Tag = 4 // One-Pass Signature
	TagSecretKey     Tag = 5
	TagPublicKey     Tag = 6
	TagSecretSubkey  Tag = 7
	TagCompressed    Tag = 8
	TagSED           Tag = 9 // Symmetrically Encrypted Data, without integrity protection
	TagMarker        Tag = 10
	TagLiteral       Tag = 11
	TagTrust         Tag = 12
	TagUserID        Tag = 13
	TagPublicSubkey  Tag = 14
	TagUserAttribute Tag = 17
	TagSEIPD         Tag = 18 // Symmetrically Encrypted and Integrity Protected Data
	TagMDC           Tag = 19
	TagAEAD          Tag = 20 // LibrePGP OCB Encrypted Data
	TagPadding       Tag = 21
)

var tagNames = [...]string{
	TagReserved: "reserved", TagPKESK: "pkesk", TagSignature: "signature",
	TagSKESK: "skesk", TagOnePass: "one-pass-signature", TagSecretKey: "secret-key",
	TagPublicKey: "public-key", TagSecretSubkey: "secret-subkey",
	TagCompressed: "compressed", TagSED: "sed", TagMarker: "marker",
	TagLiteral: "literal", TagTrust: "trust", TagUserID: "user-id",
	TagPublicSubkey: "public-subkey", 15: "", 16: "", TagUserAttribute: "user-attribute",
	TagSEIPD: "seipd", TagMDC: "mdc", TagAEAD: "aead", TagPadding: "padding",
}

func (t Tag) String() string {
	if int(t) < len(tagNames) && tagNames[t] != "" {
		return tagNames[t]
	}
	return fmt.Sprintf("tag-%d", uint8(t))
}

// known reports whether the tag is one RFC 9580 or LibrePGP defines.
func (t Tag) known() bool {
	return t != TagReserved && int(t) < len(tagNames) && tagNames[t] != ""
}

// PubKeyAlgo is a public-key algorithm ID (RFC 9580 §9.1).
type PubKeyAlgo uint8

// HashAlgo is a hash algorithm ID (RFC 9580 §9.5).
type HashAlgo uint8

// CipherAlgo is a symmetric cipher ID (RFC 9580 §9.3).
type CipherAlgo uint8

// AEADAlgo is an AEAD mode ID (RFC 9580 §9.6).
type AEADAlgo uint8

// CompressionAlgo is a compression algorithm ID (RFC 9580 §9.4).
type CompressionAlgo uint8

// SigType is a signature type (RFC 9580 §5.2.1).
type SigType uint8

// Signature types a mail reader meets.
const (
	SigBinary       SigType = 0x00
	SigText         SigType = 0x01
	SigStandalone   SigType = 0x02
	SigCertGeneric  SigType = 0x10
	SigCertPositive SigType = 0x13
	SigSubkeyBind   SigType = 0x18
	SigPrimaryBind  SigType = 0x19
	SigDirectKey    SigType = 0x1F
	SigKeyRevoke    SigType = 0x20
	SigSubkeyRevoke SigType = 0x28
	SigCertRevoke   SigType = 0x30
	SigTimestamp    SigType = 0x40
	SigThirdParty   SigType = 0x50
)

func algoName(names map[uint8]string, v uint8) string {
	if s, ok := names[v]; ok {
		return s
	}
	return fmt.Sprintf("unknown(%d)", v)
}

var pubKeyNames = map[uint8]string{
	1: "RSA", 2: "RSA-E", 3: "RSA-S", 16: "Elgamal", 17: "DSA", 18: "ECDH",
	19: "ECDSA", 20: "Elgamal-SE", 22: "EdDSALegacy", 25: "X25519",
	26: "X448", 27: "Ed25519", 28: "Ed448",
}

var hashNames = map[uint8]string{
	1: "MD5", 2: "SHA1", 3: "RIPEMD160", 8: "SHA256", 9: "SHA384",
	10: "SHA512", 11: "SHA224", 12: "SHA3-256", 14: "SHA3-512",
}

var cipherNames = map[uint8]string{
	0: "plaintext", 1: "IDEA", 2: "TripleDES", 3: "CAST5", 4: "Blowfish",
	7: "AES128", 8: "AES192", 9: "AES256", 10: "Twofish", 11: "Camellia128",
	12: "Camellia192", 13: "Camellia256",
}

var aeadNames = map[uint8]string{1: "EAX", 2: "OCB", 3: "GCM"}

var compressionNames = map[uint8]string{0: "uncompressed", 1: "ZIP", 2: "ZLIB", 3: "BZip2"}

func (a PubKeyAlgo) String() string      { return algoName(pubKeyNames, uint8(a)) }
func (a HashAlgo) String() string        { return algoName(hashNames, uint8(a)) }
func (a CipherAlgo) String() string      { return algoName(cipherNames, uint8(a)) }
func (a AEADAlgo) String() string        { return algoName(aeadNames, uint8(a)) }
func (a CompressionAlgo) String() string { return algoName(compressionNames, uint8(a)) }
func (t SigType) String() string         { return fmt.Sprintf("0x%02X", uint8(t)) }

// Micalg returns the RFC 3156 micalg name of the hash, such as "pgp-sha256",
// or "" for an unknown hash.
func (a HashAlgo) Micalg() string {
	s, ok := hashNames[uint8(a)]
	if !ok {
		return ""
	}
	return "pgp-" + strings.ToLower(s)
}

// HashByName returns the hash a Hash armor header or a micalg (with or
// without its "pgp-" prefix) names, case-insensitively.
func HashByName(s string) (HashAlgo, bool) {
	s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "pgp-")
	for id, n := range hashNames {
		if strings.ToLower(n) == s {
			return HashAlgo(id), true
		}
	}
	return 0, false
}

// A KeyID is the 64-bit ID of an OpenPGP key. Zero is the wildcard of an
// anonymous recipient.
type KeyID uint64

func (k KeyID) String() string { return fmt.Sprintf("%016X", uint64(k)) }

// A Fingerprint is a key fingerprint: 20 bytes for a version 4 key, 32 for
// version 5 and 6.
type Fingerprint []byte

func (f Fingerprint) String() string { return strings.ToUpper(hex.EncodeToString(f)) }

// keyIDOf returns the key ID a fingerprint implies: the low 64 bits of a
// version 4 fingerprint, the high 64 bits of a longer one.
func keyIDOf(f []byte) KeyID {
	switch {
	case len(f) == 20:
		return KeyID(be64(f[12:20]))
	case len(f) >= 32:
		return KeyID(be64(f[:8]))
	}
	return 0
}

func be16(b []byte) uint32 { return uint32(b[0])<<8 | uint32(b[1]) }
func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
func be64(b []byte) uint64 { return uint64(be32(b[:4]))<<32 | uint64(be32(b[4:8])) }

// curveNames maps the curve OIDs of RFC 9580 §9.2 to their names.
var curveNames = map[string]string{
	"2b06010401da470f01":   "Ed25519Legacy",
	"2b060104019755010501": "Curve25519Legacy",
	"2a8648ce3d030107":     "NIST P-256",
	"2b81040022":           "NIST P-384",
	"2b81040023":           "NIST P-521",
	"2b2403030208010107":   "brainpoolP256r1",
	"2b240303020801010b":   "brainpoolP384r1",
	"2b240303020801010d":   "brainpoolP512r1",
	"2b6570":               "Ed25519",
	"2b656e":               "X25519",
	"2b6571":               "Ed448",
	"2b656f":               "X448",
}

func curveName(oid []byte) string {
	h := hex.EncodeToString(oid)
	if n, ok := curveNames[h]; ok {
		return n
	}
	return "oid:" + h
}
