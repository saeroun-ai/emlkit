package smime

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
)

// A Status is the outcome of one check.
type Status uint8

const (
	// NotChecked says the check was not made: something it needs is absent
	// or unsupported. The note beside it says what.
	NotChecked Status = iota
	// Passed says the check was made and held.
	Passed
	// Failed says the check was made and did not hold.
	Failed
)

// String returns "not-checked", "passed" or "failed".
func (s Status) String() string {
	switch s {
	case Passed:
		return "passed"
	case Failed:
		return "failed"
	}
	return "not-checked"
}

// VerifyOptions configures SignedData.Verify. Nil means no extra
// certificates and no chain building.
type VerifyOptions struct {
	// Certificates are candidates for the signers' certificates besides the
	// ones the SignedData carries, for a signature sent without them. They
	// are also offered as intermediates when a chain is built.
	Certificates []*x509.Certificate

	// X509, when non-nil and holding Roots, asks for each signer's
	// certificate to be chained to those roots. The options are used as
	// given, with two additions: the certificates of the SignedData are
	// added to a copy of Intermediates, and an empty KeyUsages means
	// x509.ExtKeyUsageEmailProtection. CurrentTime is the caller's to set;
	// the zero value is now. A SignerInfo's SigningTime is the signer's own
	// claim and is never used for it.
	//
	// Without Roots no chain is built: the system's trust store is never
	// consulted implicitly.
	X509 *x509.VerifyOptions
}

// A SignerResult is what was established about one signer. Its three
// statuses are separate facts and none implies another:
//
//   - Digest: the content is the content the signer's attributes describe.
//   - Signature: the signature is valid for the public key of Certificate.
//   - Chain: Certificate chains to a root the caller trusts.
//
// Digest and Signature together say the content is as it was when the holder
// of that key signed it. They say nothing about who that is: the certificate
// comes from the message, and anyone can make one. Only Chain, and the
// caller's own comparison of Certificate's addresses with the message's
// author, speak to identity.
type SignerResult struct {
	// Signer is the signer info the result is about.
	Signer *SignerInfo
	// Certificate is the certificate the signer identifier names, taken from
	// the SignedData or from VerifyOptions.Certificates; nil when none
	// matches.
	Certificate *x509.Certificate

	// Digest is whether the content's digest equals the message-digest
	// attribute. It is NotChecked when the signer has no signed attributes —
	// the signature then covers the content itself, and Signature alone
	// speaks for it — when there is no content to hash, or when the digest
	// algorithm is not supported.
	Digest     Status
	DigestNote string

	// Signature is whether the signature over the signed attributes (or over
	// the content, when there are none) is valid for Certificate's key.
	Signature     Status
	SignatureNote string

	// Chain is whether Certificate chains to VerifyOptions.X509.Roots;
	// NotChecked when no roots were given. ChainErr is why it failed, and
	// Chains what was built.
	Chain    Status
	ChainErr error
	Chains   [][]*x509.Certificate

	// ContentTypeMismatch reports a content-type attribute that differs from
	// the encapsulated content type, which RFC 5652 forbids.
	ContentTypeMismatch bool
}

// ContentIntact reports that the content is the content this signer's key
// signed: the signature is valid, and the digest matches or the signature
// covers the content directly. It is not a statement of trust.
func (r SignerResult) ContentIntact() bool {
	if r.Signature != Passed {
		return false
	}
	return r.Digest == Passed || (r.Signer != nil && !r.Signer.HasSignedAttributes)
}

// Verify checks every signer of the SignedData and returns one result for
// each, in order.
//
// content is the signed content for a detached signature: the raw bytes of
// the first part of the multipart/signed exactly as transmitted, header block
// included, with CRLF line endings, up to but not including the line break
// before the boundary — message.Entity.RawSpan, or mail.Node.SpanStart and
// SpanEnd, give that range of the input. Bytes that were decoded, re-encoded
// or repaired by a parser are not what was signed and will not verify; a part
// the parser had to repair must be reported as unverifiable, not as forged.
// Pass nil for an opaque signature to verify the encapsulated content.
//
// Supported are RSA PKCS #1 v1.5, RSASSA-PSS and ECDSA with SHA-1, SHA-256,
// SHA-384 and SHA-512, and Ed25519. Anything else is NotChecked, with a note.
func (sd *SignedData) Verify(content []byte, opts *VerifyOptions) []SignerResult {
	if sd == nil {
		return nil
	}
	have := content != nil
	truncated := false
	if !have && sd.HasContent {
		content, have, truncated = sd.Content, true, sd.ContentTruncated
	}
	var extra []*x509.Certificate
	if opts != nil {
		extra = opts.Certificates
	}

	sums := make(map[crypto.Hash][]byte, 2)
	digest := func(h crypto.Hash) []byte {
		if s, ok := sums[h]; ok {
			return s
		}
		hh := h.New()
		hh.Write(content)
		s := hh.Sum(nil)
		sums[h] = s
		return s
	}

	out := make([]SignerResult, len(sd.Signers))
	for i := range sd.Signers {
		si := &sd.Signers[i]
		r := &out[i]
		r.Signer = si
		r.Certificate = sd.signerCertificate(si, extra)
		r.ContentTypeMismatch = si.ContentType != "" && sd.ContentType != "" && si.ContentType != sd.ContentType
		h, hok := hashOf(si.DigestAlgorithm.OID)

		// What the signature covers: the attributes, or the content.
		var signed []byte
		usable := false
		if si.HasSignedAttributes {
			switch {
			case !have:
				r.DigestNote = "no content to compare the digest with"
			case truncated:
				r.DigestNote = "the content is truncated"
			case !hok:
				r.DigestNote = "digest algorithm " + si.DigestAlgorithm.String() + " is not supported"
			case si.MessageDigest == nil:
				r.Digest = Failed
				r.DigestNote = "the signed attributes hold no message digest"
			case bytes.Equal(digest(h), si.MessageDigest):
				r.Digest = Passed
			default:
				r.Digest = Failed
				r.DigestNote = "the content is not the content that was signed"
			}
			if si.attrsOK {
				signed, usable = si.signedAttributesDER(), true
			} else {
				r.SignatureNote = "the signed attributes are truncated"
			}
		} else {
			r.DigestNote = "no signed attributes: the signature covers the content itself"
			switch {
			case !have:
				r.SignatureNote = "no content to verify the signature over"
			case truncated:
				r.SignatureNote = "the content is truncated"
			default:
				signed, usable = content, true
			}
		}
		if usable {
			r.Signature, r.SignatureNote = checkSignature(r.Certificate, si, signed, h, hok)
		}
		sd.chain(r, opts)
	}
	return out
}

// signerCertificate finds the certificate a signer identifier names.
func (sd *SignedData) signerCertificate(si *SignerInfo, extra []*x509.Certificate) *x509.Certificate {
	for i := range sd.Certificates {
		if c := sd.Certificates[i].Cert; c != nil && si.Matches(c) {
			return c
		}
	}
	for _, c := range extra {
		if si.Matches(c) {
			return c
		}
	}
	return nil
}

// chain builds the certificate chain of one result, when roots were given.
func (sd *SignedData) chain(r *SignerResult, opts *VerifyOptions) {
	if r.Certificate == nil || opts == nil || opts.X509 == nil || opts.X509.Roots == nil {
		return
	}
	vo := *opts.X509
	if vo.Intermediates != nil {
		vo.Intermediates = vo.Intermediates.Clone()
	} else {
		vo.Intermediates = x509.NewCertPool()
	}
	for i := range sd.Certificates {
		if c := sd.Certificates[i].Cert; c != nil {
			vo.Intermediates.AddCert(c)
		}
	}
	for _, c := range opts.Certificates {
		if c != nil {
			vo.Intermediates.AddCert(c)
		}
	}
	if len(vo.KeyUsages) == 0 {
		vo.KeyUsages = []x509.ExtKeyUsage{x509.ExtKeyUsageEmailProtection}
	}
	r.Chains, r.ChainErr = r.Certificate.Verify(vo)
	if r.ChainErr != nil {
		r.Chain = Failed
		return
	}
	r.Chain = Passed
}

// signedAttributesDER is what a signature over signed attributes covers: the
// attributes as a SET OF, where the message carries them under the implicit
// tag [0] (RFC 5652 section 5.4). The content is taken as it was sent.
func (si *SignerInfo) signedAttributesDER() []byte {
	return derSet(si.attrsBody)
}

func derSet(body []byte) []byte {
	n := len(body)
	out := make([]byte, 0, n+6)
	out = append(out, 0x31)
	switch {
	case n < 0x80:
		out = append(out, byte(n))
	case n < 1<<8:
		out = append(out, 0x81, byte(n))
	case n < 1<<16:
		out = append(out, 0x82, byte(n>>8), byte(n))
	case n < 1<<24:
		out = append(out, 0x83, byte(n>>16), byte(n>>8), byte(n))
	default:
		out = append(out, 0x84, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	return append(out, body...)
}

// impliedHash is the hash a combined signature algorithm names.
func impliedHash(oid string) (crypto.Hash, bool) {
	switch oid {
	case oidRSASHA1, oidRSASHA1Old, oidECDSASHA1:
		return crypto.SHA1, true
	case oidRSASHA256, oidECDSASHA256:
		return crypto.SHA256, true
	case oidRSASHA384, oidECDSASHA384:
		return crypto.SHA384, true
	case oidRSASHA512, oidECDSASHA512:
		return crypto.SHA512, true
	}
	return 0, false
}

func sum(h crypto.Hash, b []byte) []byte {
	hh := h.New()
	hh.Write(b)
	return hh.Sum(nil)
}

// checkSignature verifies si's signature over signed with cert's key. h is
// the signer's digest algorithm, when it is a supported one.
func checkSignature(cert *x509.Certificate, si *SignerInfo, signed []byte, h crypto.Hash, hok bool) (Status, string) {
	if cert == nil {
		return NotChecked, "no certificate matches the signer identifier"
	}
	if len(si.Signature) == 0 {
		return Failed, "the signer info holds no signature"
	}
	alg := si.SignatureAlgorithm
	verdict := func(ok bool, err error) (Status, string) {
		switch {
		case err != nil:
			return Failed, err.Error()
		case !ok:
			return Failed, "the signature does not match"
		}
		return Passed, ""
	}
	// A combined identifier names its own hash; a bare key algorithm leaves
	// it to the digest algorithm.
	if ih, ok := impliedHash(alg.OID); ok {
		h, hok = ih, true
	}
	unsupportedHash := "digest algorithm " + si.DigestAlgorithm.String() + " is not supported"

	switch alg.OID {
	case oidRSA, oidRSASHA1, oidRSASHA1Old, oidRSASHA256, oidRSASHA384, oidRSASHA512:
		pub, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok {
			return Failed, "the certificate's key is not an RSA key"
		}
		if !hok {
			return NotChecked, unsupportedHash
		}
		return verdict(true, rsa.VerifyPKCS1v15(pub, h, sum(h, signed), si.Signature))
	case oidRSAPSS:
		pub, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok {
			return Failed, "the certificate's key is not an RSA key"
		}
		ph, mgf, salt, ok := rsaParams(alg.Params)
		if !ok || ph != mgf {
			return NotChecked, "RSASSA-PSS parameters are not supported"
		}
		return verdict(true, rsa.VerifyPSS(pub, ph, sum(ph, signed), si.Signature,
			&rsa.PSSOptions{SaltLength: salt, Hash: ph}))
	case oidECPublicKey, oidECDSASHA1, oidECDSASHA256, oidECDSASHA384, oidECDSASHA512:
		pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
		if !ok {
			return Failed, "the certificate's key is not an ECDSA key"
		}
		if !hok {
			return NotChecked, unsupportedHash
		}
		return verdict(ecdsa.VerifyASN1(pub, sum(h, signed), si.Signature), nil)
	case oidEd25519:
		pub, ok := cert.PublicKey.(ed25519.PublicKey)
		if !ok {
			return Failed, "the certificate's key is not an Ed25519 key"
		}
		return verdict(ed25519.Verify(pub, signed, si.Signature), nil)
	}
	return NotChecked, "signature algorithm " + alg.String() + " is not supported"
}

// rsaParams reads the parameters RSASSA-PSS and RSAES-OAEP share: the hash
// [0], the mask generation function [1] — which must be MGF1 — and, for PSS,
// the salt length [2]. Absent fields have the defaults of RFC 4055: SHA-1 and
// a salt of 20.
func rsaParams(raw []byte) (h, mgf crypto.Hash, salt int, ok bool) {
	h, mgf, salt = crypto.SHA1, crypto.SHA1, 20
	if len(raw) == 0 {
		return h, mgf, salt, true
	}
	p := newParser(raw, (*Limits)(nil).resolve())
	top := p.top()
	seq, ok := top.take(classUniversal, tagSequence)
	if !ok || seq.truncated {
		return h, mgf, salt, false
	}
	c := p.in(seq, 1)
	if w, ok := c.take(classContext, 0); ok {
		wc := p.in(w, 2)
		if h, ok = hashOf(p.takeAlgorithm(&wc, 3, "hash").OID); !ok {
			return h, mgf, salt, false
		}
	}
	if w, ok := c.take(classContext, 1); ok {
		wc := p.in(w, 2)
		a := p.takeAlgorithm(&wc, 3, "mask generation function")
		if a.OID != oidMGF1 {
			return h, mgf, salt, false
		}
		ip := newParser(a.Params, p.lim)
		ic := ip.top()
		if mgf, ok = hashOf(ip.takeAlgorithm(&ic, 1, "hash").OID); !ok {
			return h, mgf, salt, false
		}
	}
	if w, ok := c.take(classContext, 2); ok {
		wc := p.in(w, 2)
		if n, ok := wc.take(classUniversal, tagInteger); ok {
			v, ok := smallInt(p.content(n))
			if !ok || v < 0 || v > 1024 {
				return h, mgf, salt, false
			}
			salt = v
		}
	}
	return h, mgf, salt, true
}
