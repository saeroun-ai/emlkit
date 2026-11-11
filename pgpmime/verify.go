package pgpmime

import (
	"strings"
	"time"
)

// A Mode is how a signature relates to the data it signs.
type Mode uint8

// Signature modes.
const (
	// ModeDetached: a signature over separate data, the first part of a
	// PGP/MIME multipart/signed or a file beside a .sig.
	ModeDetached Mode = iota + 1
	// ModeCleartext: a cleartext signed message (RFC 9580 §7).
	ModeCleartext
	// ModeInline: a signed message whose signature packets travel with the
	// signed literal data, as gpg --sign writes it.
	ModeInline
)

// A VerifyRequest is what a Verifier needs to check a signature.
type VerifyRequest struct {
	Mode Mode
	// Signed is exactly the bytes the signature covers, for ModeDetached and
	// ModeCleartext; nil for ModeInline, whose Signature carries them.
	Signed []byte
	// Signature is the binary OpenPGP data, armor removed: the signature
	// packets, or the whole message for ModeInline.
	Signature []byte
	// Signatures are the decoded signature packets, unverified, to pick a
	// key by their issuer.
	Signatures []*Signature
	// Micalg is the micalg of a multipart/signed, lower-cased, or "".
	Micalg string
}

// A Verification is a Verifier's verdict on one signature.
type Verification struct {
	Valid       bool
	KeyID       KeyID
	Fingerprint Fingerprint
	// Signer is who the verifier attributes the key to, such as a user ID.
	Signer  string
	Created time.Time
	// Reason says why the signature is not valid, or what qualifies it.
	Reason string
}

// A Verifier checks OpenPGP signatures; the caller implements it with an
// OpenPGP library or by calling gpg. It returns one Verification for each
// signature it checked, and an error only when it could not check at all.
type Verifier interface {
	Verify(req *VerifyRequest) ([]Verification, error)
}

// A DecryptRequest is what a Decrypter needs to decrypt a message.
type DecryptRequest struct {
	// Message is the binary OpenPGP message, armor removed: its session key
	// packets and its encrypted data.
	Message []byte
	// Recipients are the key IDs of the public-key session key packets, 0
	// for an anonymous one; Passphrase reports a symmetric-key one.
	Recipients []KeyID
	Passphrase bool
	// Encrypted is the encrypted data packet.
	Encrypted *Encrypted
}

// A Decryption is a Decrypter's result.
type Decryption struct {
	// Plaintext is the content of the literal data. For PGP/MIME it is a MIME
	// entity, to be parsed as one (RFC 3156 §4).
	Plaintext []byte
	// Signatures are the verdicts on signatures inside the encrypted data,
	// for a message signed and encrypted at once (RFC 3156 §6.2).
	Signatures []Verification
}

// A Decrypter decrypts OpenPGP messages; the caller implements it. It
// should bound the plaintext it returns.
type Decrypter interface {
	Decrypt(req *DecryptRequest) (*Decryption, error)
}

// Canonicalize returns raw with each line ending made CR LF, the form
// RFC 3156 §5 signs: a lone LF or a lone CR becomes CR LF, as an MTA that
// converted line endings undid it. It returns raw itself when it is already
// canonical. Nothing else is changed: trailing white space is signed.
func Canonicalize(raw []byte) []byte {
	ok := true
	for i, c := range raw {
		if c == '\n' && (i == 0 || raw[i-1] != '\r') || c == '\r' && (i+1 == len(raw) || raw[i+1] != '\n') {
			ok = false
			break
		}
	}
	if ok {
		return raw
	}
	out := make([]byte, 0, len(raw)+len(raw)/16+2)
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; {
		case c == '\r' && i+1 < len(raw) && raw[i+1] == '\n':
			out = append(out, '\r', '\n')
			i++
		case c == '\r' || c == '\n':
			out = append(out, '\r', '\n')
		default:
			out = append(out, c)
		}
	}
	return out
}

// pick returns the binary data and packets of r: the input of a binary
// result, else those of the first block of one of the types.
func pick(r *Result, input []byte, types ...ArmorType) ([]byte, []Packet, []Issue) {
	if r.Binary {
		return input, r.Packets, r.Issues
	}
	for _, b := range r.Blocks {
		for _, t := range types {
			if b.Type == t {
				return b.Data, b.Packets, append(r.Issues, b.Issues...)
			}
		}
	}
	return nil, nil, r.Issues
}

func signaturesOf(s *state, ps []Packet, is *[]Issue, inline bool) []*Signature {
	var sigs []*Signature
	for i := range ps {
		sig := ps[i].Signature
		if sig == nil {
			continue
		}
		sigs = append(sigs, sig)
		if sig.Type != SigBinary && sig.Type != SigText && !inline {
			s.add(is, CodeVerifySigType, ps[i].Offset, "signature of type %s is not over a document", sig.Type)
		}
	}
	if len(sigs) == 0 {
		s.add(is, CodeVerifyNoSignature, -1, "no signature packet")
	}
	return sigs
}

// DetachedRequest prepares the verification of a PGP/MIME signed message.
// signed is the first part of the multipart/signed exactly as it was sent
// — its header and body, from the byte after its delimiter line to the
// byte before the line ending of the next one: input[SpanStart:SpanEnd] of
// its mail.Node, or of message.Entity.RawSpan. signature is the decoded body
// of the second part, armored or binary, and micalg the multipart's
// parameter. The signed bytes are canonicalized to CR LF line endings; no
// other change is made, so a part that was re-encoded in transit, or that
// the parser repaired, does not verify, and its failure is no evidence of
// forgery.
//
// It returns nil, with the issues, when signed is nil or no signature
// packet is found.
func DetachedRequest(signed, signature []byte, micalg string, lim *Limits) (*VerifyRequest, []Issue) {
	s := newState(lim)
	var is []Issue
	if signed == nil {
		s.add(&is, CodeVerifyNoSigned, -1, "no raw bytes of the signed part")
		return nil, is
	}
	data, ps, more := pick(s.parse(signature), signature, ArmorSignature, ArmorMessage, ArmorUnknown)
	is = append(is, more...)
	sigs := signaturesOf(s, ps, &is, false)
	if len(sigs) == 0 {
		return nil, is
	}
	micalg = strings.ToLower(strings.TrimSpace(micalg))
	if h, ok := HashByName(micalg); ok {
		for _, sig := range sigs {
			if sig.Hash != h {
				s.add(&is, CodeMicalgMismatch, -1, "micalg is %q and the signature hash is %s", micalg, sig.Hash)
			}
		}
	}
	return &VerifyRequest{Mode: ModeDetached, Signed: Canonicalize(signed), Signature: data,
		Signatures: sigs, Micalg: micalg}, is
}

// CleartextRequest prepares the verification of a cleartext signature: the
// signed bytes are c.Canonical(). It returns nil when c has no signature
// block or packet.
func CleartextRequest(c *Cleartext) (*VerifyRequest, []Issue) {
	s := newState(nil)
	var is []Issue
	if c == nil || c.Signature == nil {
		s.add(&is, CodeVerifyNoSignature, -1, "no signature block")
		return nil, is
	}
	sigs := signaturesOf(s, c.Signature.Packets, &is, false)
	if len(sigs) == 0 {
		return nil, is
	}
	if len(c.Hash) > 0 {
		for _, sig := range sigs {
			named := false
			for _, n := range c.Hash {
				if h, ok := HashByName(n); ok && h == sig.Hash {
					named = true
				}
			}
			if !named {
				s.add(&is, CodeHashMismatch, -1, "the signature hash %s is not one the Hash header names", sig.Hash)
			}
		}
	}
	return &VerifyRequest{Mode: ModeCleartext, Signed: c.Canonical(), Signature: c.Signature.Data,
		Signatures: sigs}, is
}

// InlineRequest prepares the verification of a signed message that is not
// encrypted, armored (PGP MESSAGE) or binary: the signature packets travel
// with the literal data, inside a compressed packet or not. It returns nil
// when no signature packet is visible, which is the case of an encrypted
// message until it is decrypted.
func InlineRequest(message []byte, lim *Limits) (*VerifyRequest, []Issue) {
	s := newState(lim)
	data, ps, is := pick(s.parse(message), message, ArmorMessage)
	sigs := signaturesOf(s, ps, &is, true)
	if len(sigs) == 0 {
		return nil, is
	}
	return &VerifyRequest{Mode: ModeInline, Signature: data, Signatures: sigs}, is
}

// EncryptedRequest prepares the decryption of an encrypted message: the
// payload of a PGP/MIME multipart/encrypted, armored or binary, or an
// encrypted attachment. It returns nil when no encrypted data packet is
// found.
func EncryptedRequest(payload []byte, lim *Limits) (*DecryptRequest, []Issue) {
	s := newState(lim)
	data, ps, is := pick(s.parse(payload), payload, ArmorMessage, ArmorMessagePart, ArmorUnknown)
	req := &DecryptRequest{Message: data}
	for i := range ps {
		p := &ps[i]
		switch {
		case p.EncryptedKey != nil:
			req.Recipients = append(req.Recipients, p.EncryptedKey.KeyID)
		case p.SymmetricKey != nil:
			req.Passphrase = true
		case p.Encrypted != nil && req.Encrypted == nil:
			req.Encrypted = p.Encrypted
		}
	}
	if req.Encrypted == nil {
		s.add(&is, CodeNotEncrypted, -1, "no encrypted data packet")
		return nil, is
	}
	return req, is
}
