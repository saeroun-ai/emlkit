package smime

import (
	"bytes"
	"compress/zlib"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"io"
	"math/big"
	"strconv"
	"time"
)

// An Algorithm is an AlgorithmIdentifier.
type Algorithm struct {
	// OID is the algorithm in dotted form, and Name its common name, empty
	// when the package has none for it; see OIDName.
	OID  string
	Name string
	// Params is the encoding of the parameters as it was sent, tag and
	// length included — 05 00 for an explicit NULL — or nil when absent.
	Params []byte
}

// String returns the name, or the dotted identifier when there is none.
func (a Algorithm) String() string {
	if a.Name != "" {
		return a.Name
	}
	return a.OID
}

// An Identifier names a certificate the way CMS does: by issuer and serial
// number, or by subject key identifier. Exactly one of the two forms is set
// in a well-formed object.
type Identifier struct {
	// Issuer is the encoding of the issuer's Name as it was sent, and Serial
	// the serial number; both are zero for the key identifier form.
	Issuer []byte
	Serial *big.Int
	// SubjectKeyID is the subject key identifier, or nil.
	SubjectKeyID []byte
}

// IssuerString renders the issuer's name, "CN=...,O=...", or returns the
// empty string when there is none or it is not DER.
func (id Identifier) IssuerString() string {
	if len(id.Issuer) == 0 || len(id.Issuer) > maxSmallOctets {
		return ""
	}
	var rdn pkix.RDNSequence
	if rest, err := asn1.Unmarshal(id.Issuer, &rdn); err != nil || len(rest) != 0 {
		return ""
	}
	return rdn.String()
}

// Matches reports whether the identifier names cert.
func (id Identifier) Matches(cert *x509.Certificate) bool {
	if cert == nil {
		return false
	}
	if id.Serial != nil && len(id.Issuer) > 0 {
		return cert.SerialNumber != nil && cert.SerialNumber.Cmp(id.Serial) == 0 &&
			bytes.Equal(cert.RawIssuer, id.Issuer)
	}
	return len(id.SubjectKeyID) > 0 && bytes.Equal(cert.SubjectKeyId, id.SubjectKeyID)
}

// A Certificate is one certificate a SignedData carries.
type Certificate struct {
	// Raw is the encoding as it was sent. It is always kept.
	Raw []byte
	// Cert is the parsed certificate, nil when Err says why not, or when
	// Other is set.
	Cert *x509.Certificate
	Err  error
	// Other is set for a choice that is not an X.509 certificate: an
	// attribute certificate or another format.
	Other bool
}

// An Attribute is one signed or unsigned attribute.
type Attribute struct {
	// Type is the attribute type in dotted form, and Name its common name or
	// the empty string.
	Type string
	Name string
	// Values is the content of the attribute's SET OF values as it was sent:
	// the encodings of the values, one after another.
	Values []byte
}

// A SignerInfo is what one signer says about itself and the content.
type SignerInfo struct {
	Version int
	// Identifier names the signer's certificate.
	Identifier
	DigestAlgorithm    Algorithm
	SignatureAlgorithm Algorithm
	// Signature is the signature value.
	Signature []byte

	// HasSignedAttributes reports that the signature covers a set of signed
	// attributes — among them the digest of the content — and not the content
	// directly. SignedAttributes lists them all, in order.
	HasSignedAttributes bool
	SignedAttributes    []Attribute

	// ContentType, MessageDigest, SigningTime and SMIMECapabilities are the
	// values of those signed attributes: zero when absent. SigningTime is the
	// signer's claim, not a trusted time. SMIMECapabilities is the encoding
	// of the capabilities SEQUENCE.
	ContentType       string
	MessageDigest     []byte
	SigningTime       time.Time
	SMIMECapabilities []byte

	// UnsignedAttributes lists the attributes the signature does not cover;
	// HasCounterSignature and HasTimeStamp say two common ones are there.
	UnsignedAttributes  []Attribute
	HasCounterSignature bool
	HasTimeStamp        bool

	// attrsBody is the content of the signed attributes' SET, and attrsOK
	// whether it arrived whole.
	attrsBody []byte
	attrsOK   bool
}

// A SignedData is a CMS SignedData: content that may be carried inside it,
// and the signatures over it.
type SignedData struct {
	Version          int
	DigestAlgorithms []Algorithm

	// ContentType is the type of the encapsulated content, in dotted form:
	// OIDData for a message.
	ContentType string
	// HasContent reports that the content is carried inside (an opaque
	// signature) and not apart from it (a detached one). Content is that
	// content: the bytes that were signed, which for S/MIME are a MIME
	// entity. ContentTruncated says Content is only the beginning: the input
	// ended, or MaxContentBytes was reached.
	HasContent       bool
	Content          []byte
	ContentTruncated bool

	// Certificates are the certificates carried along, in order. HasCRLs
	// reports that revocation lists are there as well; they are not read.
	Certificates []Certificate
	HasCRLs      bool

	// Signers are the signer infos, in order. A SignedData with none only
	// carries certificates.
	Signers []SignerInfo
}

// A RecipientKind is the way a content-encryption key is given to a recipient.
type RecipientKind uint8

const (
	// RecipientOther is a recipient info of a kind the package does not
	// know.
	RecipientOther RecipientKind = iota
	// RecipientKeyTransport encrypts the key to the recipient's public key
	// (RSA).
	RecipientKeyTransport
	// RecipientKeyAgreement derives a key with the recipient's public key
	// (ECDH, DH).
	RecipientKeyAgreement
	// RecipientKEK wraps the key with a symmetric key both sides hold.
	RecipientKEK
	// RecipientPassword derives the wrapping key from a password.
	RecipientPassword
)

// String returns a short name of the kind.
func (k RecipientKind) String() string {
	switch k {
	case RecipientKeyTransport:
		return "key-transport"
	case RecipientKeyAgreement:
		return "key-agreement"
	case RecipientKEK:
		return "kek"
	case RecipientPassword:
		return "password"
	}
	return "other"
}

// A RecipientInfo is one recipient of an EnvelopedData. A key-agreement info
// that lists several recipient keys yields one RecipientInfo for each.
type RecipientInfo struct {
	Kind    RecipientKind
	Version int
	// Identifier names the recipient's certificate; for RecipientKEK its
	// SubjectKeyID is the key identifier.
	Identifier
	KeyEncryptionAlgorithm Algorithm
	// EncryptedKey is the content-encryption key as encrypted for this
	// recipient.
	EncryptedKey []byte
}

// An EnvelopedData is a CMS EnvelopedData or AuthEnvelopedData: encrypted
// content and the recipients who can decrypt it. The package describes it and
// decrypts nothing by itself; see Decrypter.
type EnvelopedData struct {
	Version int
	// Authenticated is set for AuthEnvelopedData (RFC 5083).
	Authenticated bool
	// HasOriginatorInfo reports certificates or CRLs of the originator.
	HasOriginatorInfo bool
	Recipients        []RecipientInfo

	// ContentType is the type of the encrypted content, and
	// ContentEncryptionAlgorithm what it is encrypted with; its Params hold
	// the IV or nonce.
	ContentType                string
	ContentEncryptionAlgorithm Algorithm
	// HasEncryptedContent reports that the ciphertext is carried inside;
	// EncryptedContent is it, and ContentTruncated says it is incomplete.
	HasEncryptedContent bool
	EncryptedContent    []byte
	ContentTruncated    bool

	// MAC is the authentication tag of an AuthEnvelopedData, and AuthAttrs
	// the content of its authenticated attributes' SET, nil when absent.
	MAC       []byte
	AuthAttrs []byte
}

// A CompressedData is a CMS CompressedData (RFC 3274).
type CompressedData struct {
	Version              int
	CompressionAlgorithm Algorithm
	// ContentType is the type of the content that was compressed.
	ContentType string
	// Compressed is the content as it was sent, and Content what it inflates
	// to — nil when the algorithm is not zlib or the stream is unusable.
	// ContentTruncated says Content is only the beginning: the stream ended
	// early or MaxInflatedBytes was reached.
	Compressed       []byte
	Content          []byte
	ContentTruncated bool
}

// A Message is one parsed CMS object.
type Message struct {
	// Kind is what the object is, decided from its content: an opaque
	// signature, a detached one, certificates only, enveloped, compressed.
	Kind Kind
	// ContentType is the outer content type in dotted form.
	ContentType string
	// Armor is the text form the object arrived in.
	Armor Armor
	// Indefinite reports BER indefinite lengths, and Truncated that the input
	// ended inside the object. TrailingBytes counts what followed it.
	Indefinite    bool
	Truncated     bool
	TrailingBytes int

	// At most one of these is set, according to the content type. Data is
	// the content of a bare id-data object.
	Signed     *SignedData
	Enveloped  *EnvelopedData
	Compressed *CompressedData
	Data       []byte

	// Issues are everything that was tolerated or lost, in discovery order.
	Issues []Issue
}

// Content returns the content the object reveals without a key: the
// encapsulated content of a SignedData, the inflated content of a
// CompressedData, or the content of an id-data object. It is nil for a
// detached signature and for encrypted content.
func (m *Message) Content() []byte {
	switch {
	case m == nil:
		return nil
	case m.Signed != nil:
		return m.Signed.Content
	case m.Compressed != nil:
		return m.Compressed.Content
	}
	return m.Data
}

// Parse reads one CMS object: binary BER or DER, or PEM or Base64 text of it.
//
// It returns an error only when the input is not a CMS object at all
// (ErrNotCMS), or when Limits.MaxElements ran out before its outer structure
// was read (ErrLimitExceeded); the Message then holds only the issues.
// Otherwise it returns what could be read, and Message.Issues says what was
// tolerated, repaired or lost. A truncated object yields the fields that
// arrived.
func Parse(data []byte, limits *Limits) (*Message, error) {
	m, _, err := parse(data, limits, nil)
	return m, err
}

// ExtractSignedContent takes the encapsulated content out of a SignedData:
// the bytes an opaque-signed S/MIME message (application/pkcs7-mime;
// smime-type=signed-data) hides, which are the real message's MIME entity.
//
// The input may be DER, BER with indefinite lengths and a chunked content,
// PEM, or Base64 text; bytes after the object are ignored and reported. For a
// truncated input the content read so far is returned, info.ContentTruncated
// is set and an issue says so. A detached signature yields nil content and
// info.HasContent false.
//
// content is a slice of the input when the input is DER, and a new buffer of
// at most Limits.MaxContentBytes otherwise. The signature is not checked: see
// SignedData.Verify.
func ExtractSignedContent(data []byte, limits *Limits) (content []byte, info *SignedData, issues []Issue, err error) {
	m, _, err := parse(data, limits, nil)
	if err != nil {
		return nil, nil, m.Issues, err
	}
	if m.Signed == nil {
		return nil, nil, m.Issues, ErrNotSignedData
	}
	return m.Signed.Content, m.Signed, m.Issues, nil
}

// WriteSignedContent is ExtractSignedContent for a large content: it writes
// the encapsulated content to w chunk by chunk instead of reassembling it, and
// returns how many bytes it wrote. info.Content stays nil. An error of w ends
// the reading and is returned.
func WriteSignedContent(w io.Writer, data []byte, limits *Limits) (n int64, info *SignedData, issues []Issue, err error) {
	sink := func(b []byte) error {
		_, err := w.Write(b)
		return err
	}
	m, p, err := parse(data, limits, sink)
	if err != nil {
		return 0, nil, m.Issues, err
	}
	if m.Signed == nil {
		return 0, nil, m.Issues, ErrNotSignedData
	}
	return p.sunk, m.Signed, m.Issues, p.sinkErr
}

func parse(data []byte, limits *Limits, sink func([]byte) error) (*Message, *parser, error) {
	lim := limits.resolve()
	p := newParser(nil, lim)
	p.sink = sink
	m := &Message{}
	if len(data) > lim.MaxInputBytes {
		data = data[:lim.MaxInputBytes]
		p.issue(CodeLimitInput, Error, -1, "input longer than "+strconv.Itoa(lim.MaxInputBytes)+" bytes; the rest is not read")
	}
	ber, armor, ok := p.dearmor(data)
	m.Armor = armor
	if !ok {
		m.Issues = p.issues
		return m, p, ErrNotCMS
	}
	p.data = ber

	// fail is the error for an input whose outer structure cannot be read:
	// it is no CMS object, unless a budget stopped the reading first.
	fail := func() (*Message, *parser, error) {
		m.ContentType = ""
		m.Issues = p.issues
		if p.exhausted {
			return m, p, ErrLimitExceeded
		}
		return m, p, ErrNotCMS
	}
	top := p.top()
	ci, ok := top.next()
	if !ok || !ci.is(classUniversal, tagSequence) || !ci.constructed {
		return fail()
	}
	cc := p.in(ci, 1)
	typ, ok := cc.take(classUniversal, tagOID)
	if !ok {
		return fail()
	}
	if m.ContentType = p.oid(typ); m.ContentType == "" {
		return fail()
	}

	// content [0] EXPLICIT ANY: the wrapper is at depth 2, the content at 3.
	var inner elem
	have := false
	if wrap, ok := cc.take(classContext, 0); ok && wrap.constructed {
		ic := p.in(wrap, 2)
		inner, have = ic.next()
	}
	if !have {
		p.missing("the content of the ContentInfo", ci.start)
	}
	switch m.ContentType {
	case OIDSignedData:
		m.Signed = &SignedData{}
		if have {
			p.signedData(m.Signed, inner, 3)
		}
		m.Kind = signedKind(m.Signed)
	case OIDEnvelopedData, OIDAuthEnvelopedData:
		m.Enveloped = &EnvelopedData{Authenticated: m.ContentType == OIDAuthEnvelopedData}
		if have {
			p.envelopedData(m.Enveloped, inner, 3)
		}
		m.Kind = KindEnvelopedData
		if m.Enveloped.Authenticated {
			m.Kind = KindAuthEnvelopedData
		}
	case OIDCompressedData:
		m.Compressed = &CompressedData{}
		if have {
			p.compressedData(m.Compressed, inner, 3)
		}
		m.Kind = KindCompressedData
	case OIDData:
		m.Kind = KindUnknown
		if have {
			m.Data, _ = p.collect(inner, 3, lim.MaxContentBytes, CodeLimitContent)
		}
	default:
		m.Kind = KindUnknown
		name := OIDName(m.ContentType)
		if name == "" {
			name = m.ContentType
		}
		p.issue(CodeCMSUnknownType, Warning, typ.start, "content type "+name+" is not read")
	}

	if !ci.truncated && ci.end < len(p.data) {
		m.TrailingBytes = len(p.data) - ci.end
		p.issue(CodeCMSTrailing, Warning, ci.end, strconv.Itoa(m.TrailingBytes)+" bytes follow the object")
	}
	m.Truncated = p.truncated
	m.Indefinite = p.indefSeen
	m.Issues = p.issues
	return m, p, nil
}

// signedKind tells an opaque signature, a detached one and a bag of
// certificates apart.
func signedKind(sd *SignedData) Kind {
	switch {
	case sd.HasContent:
		return KindSignedData
	case len(sd.Signers) == 0:
		return KindCertsOnly
	}
	return KindSignature
}

func (p *parser) missing(what string, off int) {
	if p.truncated || p.exhausted {
		return
	}
	p.issue(CodeCMSMissing, Error, off, what+" is missing")
}

// oid decodes an OBJECT IDENTIFIER element, reporting one that is not.
func (p *parser) oid(e elem) string {
	if e.truncated {
		return ""
	}
	s, ok := decodeOID(p.content(e))
	if !ok {
		p.issue(CodeCMSOID, Error, e.start, "object identifier is malformed")
	}
	return s
}

func (p *parser) version(c *cursor, what string) int {
	e, ok := c.take(classUniversal, tagInteger)
	if !ok {
		p.missing(what+" version", c.pos)
		return 0
	}
	v, _ := smallInt(p.content(e))
	return v
}

// algorithm reads an AlgorithmIdentifier.
func (p *parser) algorithm(e elem, depth int) (a Algorithm) {
	if !e.is(classUniversal, tagSequence) {
		p.issue(CodeCMSUnexpected, Warning, e.start, "an algorithm identifier is not a SEQUENCE")
		return a
	}
	c := p.in(e, depth)
	o, ok := c.take(classUniversal, tagOID)
	if !ok {
		p.missing("an algorithm's identifier", e.start)
		return a
	}
	a.OID = p.oid(o)
	a.Name = OIDName(a.OID)
	if par, ok := c.next(); ok {
		a.Params = p.raw(par)
	}
	return a
}

// takeAlgorithm reads the AlgorithmIdentifier that is next in c.
func (p *parser) takeAlgorithm(c *cursor, depth int, what string) Algorithm {
	e, ok := c.take(classUniversal, tagSequence)
	if !ok {
		p.missing(what, c.pos)
		return Algorithm{}
	}
	return p.algorithm(e, depth)
}

// small reads a short OCTET STRING: a signature, a digest, a key.
func (p *parser) small(e elem, depth int) []byte {
	b, _ := p.collect(e, depth, maxSmallOctets, CodeLimitContent)
	return b
}

const maxDigestAlgorithms = 16

// signedData reads a SignedData from e, which is at the given depth.
func (p *parser) signedData(sd *SignedData, e elem, depth int) {
	if !e.is(classUniversal, tagSequence) {
		p.issue(CodeCMSUnexpected, Error, e.start, "SignedData is not a SEQUENCE")
		return
	}
	c := p.in(e, depth)
	sd.Version = p.version(&c, "SignedData")

	if set, ok := c.take(classUniversal, tagSet); ok {
		ac := p.in(set, depth+1)
		for len(sd.DigestAlgorithms) < maxDigestAlgorithms {
			a, ok := ac.next()
			if !ok {
				break
			}
			sd.DigestAlgorithms = append(sd.DigestAlgorithms, p.algorithm(a, depth+2))
		}
	} else {
		p.missing("SignedData digest algorithms", c.pos)
	}

	if eci, ok := c.take(classUniversal, tagSequence); ok {
		ec := p.in(eci, depth+1)
		if o, ok := ec.take(classUniversal, tagOID); ok {
			sd.ContentType = p.oid(o)
		}
		if wrap, ok := ec.take(classContext, 0); ok {
			sd.HasContent = true
			p.encapsulated(sd, wrap, depth+2)
		} else if !eci.truncated {
			p.issue(CodeCMSContentAbsent, Info, eci.start, "no encapsulated content: the signature is detached")
		}
	} else {
		p.missing("SignedData encapsulated content info", c.pos)
	}

	if certs, ok := c.take(classContext, 0); ok {
		p.certificates(sd, certs, depth+1)
	}
	if _, ok := c.take(classContext, 1); ok {
		sd.HasCRLs = true
	}

	set, ok := c.take(classUniversal, tagSet)
	if !ok {
		p.missing("SignedData signer infos", c.pos)
		return
	}
	sc := p.in(set, depth+1)
	for {
		s, ok := sc.next()
		if !ok {
			break
		}
		if len(sd.Signers) >= p.lim.MaxSigners {
			p.issue(CodeLimitSigners, Error, s.start, "more than "+strconv.Itoa(p.lim.MaxSigners)+" signers; the rest are not read")
			break
		}
		if !s.is(classUniversal, tagSequence) {
			p.issue(CodeCMSUnexpected, Warning, s.start, "a signer info is not a SEQUENCE")
			continue
		}
		sd.Signers = append(sd.Signers, p.signerInfo(s, depth+2))
	}
	if len(sd.Signers) == 0 && !set.truncated {
		p.issue(CodeCMSNoSigners, Info, set.start, "no signers: the object only carries certificates")
	}
}

// encapsulated reads eContent [0] EXPLICIT OCTET STRING from its wrapper.
func (p *parser) encapsulated(sd *SignedData, wrap elem, depth int) {
	body := wrap
	bodyDepth := depth
	if wrap.constructed {
		ic := p.in(wrap, depth)
		inner, ok := ic.next()
		if !ok {
			sd.ContentTruncated = p.truncated
			return
		}
		body, bodyDepth = inner, depth+1
		if !inner.is(classUniversal, tagOctetString) {
			// PKCS #7 let the content be any type; what is signed is its
			// content octets, so those are what is kept, as one string.
			p.issue(CodeCMSContentNotOctets, Warning, inner.start, "encapsulated content is not an OCTET STRING; its content octets are kept")
			body.constructed = false
		}
	}
	if body.constructed {
		p.issue(CodeCMSChunked, Info, body.start, "content is a constructed OCTET STRING; its chunks are joined")
	}
	if p.sink == nil {
		var complete bool
		sd.Content, complete = p.collect(body, bodyDepth, p.lim.MaxContentBytes, CodeLimitContent)
		sd.ContentTruncated = !complete
		return
	}
	clips := p.clips
	budget := int64(p.lim.MaxContentBytes)
	whole := p.walkOctets(body, bodyDepth, func(b []byte) bool {
		over := int64(len(b)) > budget-p.sunk
		if over {
			b = b[:budget-p.sunk]
			p.issue(CodeLimitContent, Error, body.start, "content longer than "+strconv.FormatInt(budget, 10)+" bytes; the rest is dropped")
		}
		if len(b) > 0 {
			if err := p.sink(b); err != nil {
				p.sinkErr = err
				return false
			}
			p.sunk += int64(len(b))
		}
		return !over
	})
	sd.ContentTruncated = !whole || p.clips != clips
}

var errCertTruncated = errors.New("smime: certificate is truncated")
var errCertTooLarge = errors.New("smime: certificate is larger than Limits.MaxCertificateBytes")

// certificates reads certificates [0] IMPLICIT SET OF CertificateChoices.
func (p *parser) certificates(sd *SignedData, set elem, depth int) {
	c := p.in(set, depth)
	for {
		e, ok := c.next()
		if !ok {
			return
		}
		if len(sd.Certificates) >= p.lim.MaxCertificates {
			p.issue(CodeLimitCertificates, Error, e.start, "more than "+strconv.Itoa(p.lim.MaxCertificates)+" certificates; the rest are not read")
			return
		}
		cert := Certificate{Raw: p.raw(e)}
		switch {
		case !e.is(classUniversal, tagSequence):
			cert.Other = true
			p.issue(CodeCMSCertificateOther, Info, e.start, "a certificate of another format is kept unparsed")
		case e.truncated:
			cert.Err = errCertTruncated
		case len(cert.Raw) > p.lim.MaxCertificateBytes:
			cert.Err = errCertTooLarge
			p.issue(CodeLimitCertificateSz, Error, e.start, "a certificate of "+strconv.Itoa(len(cert.Raw))+" bytes is kept unparsed")
		default:
			cert.Cert, cert.Err = x509.ParseCertificate(cert.Raw)
			if cert.Err != nil {
				p.issue(CodeCMSCertificate, Warning, e.start, "certificate does not parse: "+cert.Err.Error())
			}
		}
		sd.Certificates = append(sd.Certificates, cert)
	}
}

// identifier reads the two forms a signer or key-transport recipient is named
// by: IssuerAndSerialNumber, or [0] IMPLICIT SubjectKeyIdentifier.
func (p *parser) identifier(c *cursor, depth int, what string) (id Identifier) {
	if ias, ok := c.take(classUniversal, tagSequence); ok {
		return p.issuerAndSerial(ias, depth)
	}
	if ski, ok := c.take(classContext, 0); ok {
		id.SubjectKeyID = p.small(ski, depth)
		return id
	}
	p.missing(what, c.pos)
	return id
}

func (p *parser) issuerAndSerial(e elem, depth int) (id Identifier) {
	c := p.in(e, depth)
	name, ok := c.next()
	if !ok {
		p.missing("an issuer name", e.start)
		return id
	}
	id.Issuer = p.raw(name)
	serial, ok := c.take(classUniversal, tagInteger)
	if !ok {
		p.missing("a serial number", c.pos)
		return id
	}
	b := p.content(serial)
	if len(b) == 0 || len(b) > 256 || serial.truncated {
		p.issue(CodeCMSUnexpected, Warning, serial.start, "serial number of "+strconv.Itoa(len(b))+" bytes is not read")
		return id
	}
	id.Serial = new(big.Int).SetBytes(b)
	if b[0]&0x80 != 0 {
		// Two's complement: a negative serial, as x509 reads it.
		id.Serial.Sub(id.Serial, new(big.Int).Lsh(big.NewInt(1), uint(len(b))*8))
	}
	return id
}

// attributes reads a SET OF Attribute, calling each with its type and first
// value.
func (p *parser) attributes(set elem, depth int, each func(oid string, first elem, has bool, depth int)) (out []Attribute) {
	c := p.in(set, depth)
	for {
		a, ok := c.next()
		if !ok {
			return out
		}
		if len(out) >= p.lim.MaxAttributes {
			p.issue(CodeLimitAttributes, Error, a.start, "more than "+strconv.Itoa(p.lim.MaxAttributes)+" attributes; the rest are not read")
			return out
		}
		if !a.is(classUniversal, tagSequence) {
			p.issue(CodeCMSAttribute, Warning, a.start, "an attribute is not a SEQUENCE")
			continue
		}
		ac := p.in(a, depth+1)
		o, ok := ac.take(classUniversal, tagOID)
		if !ok {
			p.issue(CodeCMSAttribute, Warning, a.start, "an attribute has no type")
			continue
		}
		at := Attribute{Type: p.oid(o)}
		at.Name = OIDName(at.Type)
		if vs, ok := ac.take(classUniversal, tagSet); ok {
			at.Values = p.content(vs)
			vc := p.in(vs, depth+2)
			first, has := vc.next()
			if each != nil {
				each(at.Type, first, has, depth+3)
			}
		} else {
			p.issue(CodeCMSAttribute, Warning, a.start, "an attribute has no values")
		}
		out = append(out, at)
	}
}

// signerInfo reads a SignerInfo from e, which is at the given depth.
func (p *parser) signerInfo(e elem, depth int) (si SignerInfo) {
	c := p.in(e, depth)
	si.Version = p.version(&c, "SignerInfo")
	si.Identifier = p.identifier(&c, depth+1, "a signer identifier")
	si.DigestAlgorithm = p.takeAlgorithm(&c, depth+1, "a signer's digest algorithm")

	if attrs, ok := c.take(classContext, 0); ok {
		si.HasSignedAttributes = true
		si.attrsBody = p.content(attrs)
		si.attrsOK = !attrs.truncated
		if attrs.indefinite {
			p.issue(CodeCMSAttrsNotDER, Warning, attrs.start, "signed attributes are not DER; they are verified as sent")
		}
		var seenType, seenDigest bool
		si.SignedAttributes = p.attributes(attrs, depth+1, func(oid string, first elem, has bool, d int) {
			if !has {
				return
			}
			switch oid {
			case OIDAttrContentType:
				if seenType {
					p.issue(CodeCMSAttributeRepeat, Warning, first.start, "content-type attribute repeated; the first is used")
					return
				}
				seenType = true
				if first.is(classUniversal, tagOID) {
					si.ContentType = p.oid(first)
				}
			case OIDAttrMessageDigest:
				if seenDigest {
					p.issue(CodeCMSAttributeRepeat, Warning, first.start, "message-digest attribute repeated; the first is used")
					return
				}
				seenDigest = true
				if first.is(classUniversal, tagOctetString) {
					si.MessageDigest = p.small(first, d)
				}
			case OIDAttrSigningTime:
				t, ok := parseTime(first.tag, p.content(first))
				if !ok || first.class != classUniversal {
					p.issue(CodeCMSSigningTime, Warning, first.start, "signing time is not a valid time")
					return
				}
				si.SigningTime = t
			case OIDAttrSMIMECapabilities:
				si.SMIMECapabilities = p.raw(first)
			}
		})
	}

	si.SignatureAlgorithm = p.takeAlgorithm(&c, depth+1, "a signer's signature algorithm")
	if sig, ok := c.take(classUniversal, tagOctetString); ok {
		si.Signature = p.small(sig, depth+1)
	} else {
		p.missing("a signature", c.pos)
	}
	if un, ok := c.take(classContext, 1); ok {
		si.UnsignedAttributes = p.attributes(un, depth+1, func(oid string, _ elem, _ bool, _ int) {
			switch oid {
			case OIDAttrCounterSignature:
				si.HasCounterSignature = true
			case OIDAttrTimeStampToken:
				si.HasTimeStamp = true
			}
		})
	}
	return si
}

// parseTime reads a UTCTime or GeneralizedTime.
func parseTime(tag uint32, b []byte) (time.Time, bool) {
	if len(b) < 10 || len(b) > 32 {
		return time.Time{}, false
	}
	s := string(b)
	switch tag {
	case tagUTCTime:
		for _, layout := range []string{"060102150405Z0700", "0601021504Z0700"} {
			if t, err := time.Parse(layout, s); err == nil {
				// RFC 5652 section 11.3: years 50 to 99 are 19xx.
				if t.Year() >= 2050 {
					t = t.AddDate(-100, 0, 0)
				}
				return t, true
			}
		}
	case tagGeneralizedTime:
		for _, layout := range []string{"20060102150405Z0700", "200601021504Z0700"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// envelopedData reads an EnvelopedData or AuthEnvelopedData from e.
func (p *parser) envelopedData(ed *EnvelopedData, e elem, depth int) {
	if !e.is(classUniversal, tagSequence) {
		p.issue(CodeCMSUnexpected, Error, e.start, "EnvelopedData is not a SEQUENCE")
		return
	}
	c := p.in(e, depth)
	ed.Version = p.version(&c, "EnvelopedData")
	if _, ok := c.take(classContext, 0); ok {
		ed.HasOriginatorInfo = true
	}
	if set, ok := c.take(classUniversal, tagSet); ok {
		rc := p.in(set, depth+1)
		for {
			r, ok := rc.next()
			if !ok {
				break
			}
			if len(ed.Recipients) >= p.lim.MaxRecipients {
				p.issue(CodeLimitRecipients, Error, r.start, "more than "+strconv.Itoa(p.lim.MaxRecipients)+" recipients; the rest are not read")
				break
			}
			ed.Recipients = p.recipient(ed.Recipients, r, depth+2)
		}
	} else {
		p.missing("EnvelopedData recipient infos", c.pos)
	}

	if eci, ok := c.take(classUniversal, tagSequence); ok {
		ec := p.in(eci, depth+1)
		if o, ok := ec.take(classUniversal, tagOID); ok {
			ed.ContentType = p.oid(o)
		}
		ed.ContentEncryptionAlgorithm = p.takeAlgorithm(&ec, depth+2, "the content encryption algorithm")
		if ct, ok := ec.take(classContext, 0); ok {
			ed.HasEncryptedContent = true
			if ct.constructed {
				p.issue(CodeCMSChunked, Info, ct.start, "content is a constructed OCTET STRING; its chunks are joined")
			}
			var complete bool
			ed.EncryptedContent, complete = p.collect(ct, depth+2, p.lim.MaxContentBytes, CodeLimitContent)
			ed.ContentTruncated = !complete
		}
	} else {
		p.missing("EnvelopedData encrypted content info", c.pos)
	}
	if !ed.Authenticated {
		return
	}
	if aa, ok := c.take(classContext, 1); ok && !aa.truncated {
		ed.AuthAttrs = p.content(aa)
	}
	if mac, ok := c.take(classUniversal, tagOctetString); ok {
		ed.MAC = p.small(mac, depth+1)
	} else {
		p.missing("AuthEnvelopedData mac", c.pos)
	}
}

// recipient reads one RecipientInfo and appends what it names to out.
func (p *parser) recipient(out []RecipientInfo, e elem, depth int) []RecipientInfo {
	c := p.in(e, depth)
	var ri RecipientInfo
	switch {
	case e.is(classUniversal, tagSequence):
		ri.Kind = RecipientKeyTransport
		ri.Version = p.version(&c, "RecipientInfo")
		ri.Identifier = p.identifier(&c, depth+1, "a recipient identifier")
	case e.is(classContext, 1) && e.constructed:
		return p.keyAgreement(out, e, depth)
	case e.is(classContext, 2) && e.constructed:
		ri.Kind = RecipientKEK
		ri.Version = p.version(&c, "RecipientInfo")
		if kek, ok := c.take(classUniversal, tagSequence); ok {
			kc := p.in(kek, depth+1)
			if id, ok := kc.take(classUniversal, tagOctetString); ok {
				ri.SubjectKeyID = p.small(id, depth+2)
			}
		}
	case e.is(classContext, 3) && e.constructed:
		ri.Kind = RecipientPassword
		ri.Version = p.version(&c, "RecipientInfo")
		c.take(classContext, 0) // key derivation algorithm
	default:
		return append(out, ri)
	}
	ri.KeyEncryptionAlgorithm = p.takeAlgorithm(&c, depth+1, "a key encryption algorithm")
	if k, ok := c.take(classUniversal, tagOctetString); ok {
		ri.EncryptedKey = p.small(k, depth+1)
	}
	return append(out, ri)
}

// keyAgreement reads a KeyAgreeRecipientInfo: one algorithm, and one
// encrypted key for each recipient certificate.
func (p *parser) keyAgreement(out []RecipientInfo, e elem, depth int) []RecipientInfo {
	c := p.in(e, depth)
	ri := RecipientInfo{Kind: RecipientKeyAgreement}
	ri.Version = p.version(&c, "RecipientInfo")
	c.take(classContext, 0) // originator
	c.take(classContext, 1) // user keying material
	ri.KeyEncryptionAlgorithm = p.takeAlgorithm(&c, depth+1, "a key encryption algorithm")
	keys, ok := c.take(classUniversal, tagSequence)
	if !ok {
		return append(out, ri)
	}
	kc := p.in(keys, depth+1)
	n := 0
	for {
		k, ok := kc.next()
		if !ok || len(out) >= p.lim.MaxRecipients {
			break
		}
		one := ri
		ec := p.in(k, depth+2)
		if ias, ok := ec.take(classUniversal, tagSequence); ok {
			one.Identifier = p.issuerAndSerial(ias, depth+3)
		} else if rk, ok := ec.take(classContext, 0); ok && rk.constructed {
			rc := p.in(rk, depth+3)
			if id, ok := rc.take(classUniversal, tagOctetString); ok {
				one.SubjectKeyID = p.small(id, depth+4)
			}
		}
		if key, ok := ec.take(classUniversal, tagOctetString); ok {
			one.EncryptedKey = p.small(key, depth+3)
		}
		out = append(out, one)
		n++
	}
	if n == 0 {
		out = append(out, ri)
	}
	return out
}

// compressedData reads a CompressedData from e and inflates its content.
func (p *parser) compressedData(cd *CompressedData, e elem, depth int) {
	if !e.is(classUniversal, tagSequence) {
		p.issue(CodeCMSUnexpected, Error, e.start, "CompressedData is not a SEQUENCE")
		return
	}
	c := p.in(e, depth)
	cd.Version = p.version(&c, "CompressedData")
	cd.CompressionAlgorithm = p.takeAlgorithm(&c, depth+1, "the compression algorithm")
	eci, ok := c.take(classUniversal, tagSequence)
	if !ok {
		p.missing("CompressedData encapsulated content info", c.pos)
		return
	}
	ec := p.in(eci, depth+1)
	if o, ok := ec.take(classUniversal, tagOID); ok {
		cd.ContentType = p.oid(o)
	}
	wrap, ok := ec.take(classContext, 0)
	if !ok || !wrap.constructed {
		p.missing("CompressedData content", ec.pos)
		return
	}
	ic := p.in(wrap, depth+2)
	inner, ok := ic.next()
	if !ok {
		return
	}
	var complete bool
	cd.Compressed, complete = p.collect(inner, depth+3, p.lim.MaxContentBytes, CodeLimitContent)
	if cd.CompressionAlgorithm.OID != oidZlibCompress {
		p.issue(CodeCMSCompression, Warning, eci.start, "compression algorithm "+cd.CompressionAlgorithm.String()+" is not inflated")
		return
	}
	cd.Content, cd.ContentTruncated = p.inflate(cd.Compressed, inner.start)
	if !complete {
		cd.ContentTruncated = true
	}
}

// inflate decompresses a zlib stream into at most MaxInflatedBytes. The
// output buffer grows with what is actually produced, so a small stream that
// declares nothing costs only what it inflates to, up to the budget.
func (p *parser) inflate(b []byte, off int) (out []byte, truncated bool) {
	zr, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		p.issue(CodeCMSInflate, Error, off, "not a zlib stream: "+err.Error())
		return nil, true
	}
	budget := p.lim.MaxInflatedBytes
	out, err = io.ReadAll(io.LimitReader(zr, int64(budget)+1))
	switch {
	case len(out) > budget:
		p.issue(CodeLimitInflated, Error, off, "content inflates to more than "+strconv.Itoa(budget)+" bytes; the rest is dropped")
		return out[:budget], true
	case err != nil:
		p.issue(CodeCMSInflate, Error, off, "zlib stream is damaged after "+strconv.Itoa(len(out))+" bytes: "+err.Error())
		return out, true
	}
	return out, false
}
