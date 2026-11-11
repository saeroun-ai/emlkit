package smime

import "strings"

// A Kind is what an S/MIME part or CMS object is.
type Kind uint8

const (
	// KindNone is a part that is not S/MIME.
	KindNone Kind = iota
	// KindDetachedSignature is the multipart/signed container of a detached
	// signature: its first part is the signed content, its second the
	// signature.
	KindDetachedSignature
	// KindSignature is the signature of a detached signature: an
	// application/pkcs7-signature part, a SignedData without content.
	KindSignature
	// KindSignedData is an opaque signature: a SignedData that carries the
	// content it signs. The message proper is inside; see
	// ExtractSignedContent.
	KindSignedData
	// KindEnvelopedData is encrypted content.
	KindEnvelopedData
	// KindAuthEnvelopedData is content under authenticated encryption.
	KindAuthEnvelopedData
	// KindCompressedData is compressed content.
	KindCompressedData
	// KindCertsOnly is a SignedData that only carries certificates.
	KindCertsOnly
	// KindUnknown is an S/MIME part whose kind could not be decided, or a CMS
	// object of a type the package does not read.
	KindUnknown
)

// String returns the smime-type style name of the kind.
func (k Kind) String() string {
	switch k {
	case KindNone:
		return "none"
	case KindDetachedSignature:
		return "multipart-signed"
	case KindSignature:
		return "signature"
	case KindSignedData:
		return "signed-data"
	case KindEnvelopedData:
		return "enveloped-data"
	case KindAuthEnvelopedData:
		return "authEnveloped-data"
	case KindCompressedData:
		return "compressed-data"
	case KindCertsOnly:
		return "certs-only"
	}
	return "unknown"
}

// A Classification is what Classify decided and on what grounds.
type Classification struct {
	// Kind is the decision.
	Kind Kind
	// Source says what decided it: "content" (the bytes), "smime-type",
	// "media-type", "protocol" or "filename". Empty for KindNone.
	Source string
	// Declared is what the smime-type parameter (or the media type itself)
	// says, and Sniffed what the first bytes are; either is KindNone when it
	// says nothing.
	Declared Kind
	Sniffed  Kind
	// Mismatch reports that both speak and disagree — a part labelled
	// enveloped-data that is a SignedData, say. Kind then follows the bytes.
	Mismatch bool
	// NotCMS reports a part labelled S/MIME whose first bytes were given and
	// are no CMS object. Kind is then what the labels say.
	NotCMS bool
	// Legacy reports an "x-" media type or protocol: application/x-pkcs7-mime
	// and its kin.
	Legacy bool
	// GenericType reports a part that is S/MIME only by its file name:
	// application/octet-stream carrying smime.p7m.
	GenericType bool
	// Micalg is the micalg parameter of a multipart/signed, as written.
	Micalg string
	// ContentTypeOID is the CMS content type the first bytes declare, in
	// dotted form, or empty.
	ContentTypeOID string
}

// IsSMIME reports whether the part is S/MIME at all.
func (c Classification) IsSMIME() bool { return c.Kind != KindNone }

func param(params map[string]string, name string) string {
	if v, ok := params[name]; ok {
		return strings.TrimSpace(v)
	}
	for k, v := range params {
		if strings.EqualFold(k, name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func kindOfSMIMEType(v string) Kind {
	switch strings.ToLower(v) {
	case "signed-data":
		return KindSignedData
	case "enveloped-data":
		return KindEnvelopedData
	case "authenveloped-data", "authenticated-enveloped-data":
		return KindAuthEnvelopedData
	case "compressed-data":
		return KindCompressedData
	case "certs-only":
		return KindCertsOnly
	}
	return KindNone
}

func kindOfExtension(name string) Kind {
	name = strings.ToLower(strings.TrimRight(strings.TrimSpace(name), ". "))
	switch {
	case strings.HasSuffix(name, ".p7m"):
		return KindUnknown // signed, enveloped or compressed: the bytes say
	case strings.HasSuffix(name, ".p7s"):
		return KindSignature
	case strings.HasSuffix(name, ".p7c"):
		return KindCertsOnly
	case strings.HasSuffix(name, ".p7z"):
		return KindCompressedData
	}
	return KindNone
}

// Classify decides what S/MIME object a MIME part is, from plain values: its
// media type, the parameters of its Content-Type, its file name, and the
// first bytes of its decoded body. Any of them may be empty; head may be nil,
// and is best the first few hundred bytes or more. The media type and
// parameter names are matched without regard to case.
//
// The bytes decide when they can: the smime-type parameter is optional, often
// missing and sometimes wrong, and a part that says one thing and is another
// is reported in Mismatch. Without bytes the parameter decides, then the file
// name: .p7s a signature, .p7c certificates, .p7z compressed, and .p7m some
// CMS object that only its bytes can tell apart.
//
// For a multipart/signed the protocol parameter decides: application/
// pkcs7-signature, or its x- form, is a detached S/MIME signature, and any
// other protocol (PGP/MIME) is KindNone.
func Classify(mediaType string, params map[string]string, filename string, head []byte) Classification {
	var c Classification
	mt := strings.ToLower(strings.TrimSpace(mediaType))
	if filename == "" {
		filename = param(params, "name")
	}

	switch mt {
	case "multipart/signed":
		switch strings.ToLower(param(params, "protocol")) {
		case "application/x-pkcs7-signature":
			c.Legacy = true
			fallthrough
		case "application/pkcs7-signature":
			c.Kind = KindDetachedSignature
			c.Declared = KindDetachedSignature
			c.Source = "protocol"
			c.Micalg = param(params, "micalg")
		}
		return c
	case "application/x-pkcs7-signature":
		c.Legacy = true
		fallthrough
	case "application/pkcs7-signature":
		c.Declared = KindSignature
		c.Source = "media-type"
	case "application/x-pkcs7-mime":
		c.Legacy = true
		fallthrough
	case "application/pkcs7-mime":
		c.Declared = kindOfSMIMEType(param(params, "smime-type"))
		c.Source = "smime-type"
		if c.Declared == KindNone {
			c.Declared = kindOfExtension(filename)
			c.Source = "filename"
			if c.Declared == KindNone || c.Declared == KindSignature {
				// Unnamed, or named like a signature: still some CMS object.
				c.Declared = KindUnknown
				c.Source = "media-type"
			}
		}
	case "application/x-pkcs7-certificates", "application/pkcs7-certificates":
		c.Legacy = strings.Contains(mt, "/x-")
		c.Declared = KindCertsOnly
		c.Source = "media-type"
	default:
		// Mail gateways and some clients send S/MIME as a generic attachment;
		// RFC 8551 section 3.2.1 says to go by the file name then.
		if mt != "" && mt != "application/octet-stream" && mt != "application/x-download" {
			return c
		}
		c.Declared = kindOfExtension(filename)
		if c.Declared == KindNone {
			return c
		}
		c.GenericType = true
		c.Source = "filename"
	}

	c.Kind = c.Declared
	c.Sniffed, c.ContentTypeOID = Sniff(head)
	if c.Sniffed == KindNone {
		c.NotCMS = len(head) > 0
		return c
	}
	if c.Sniffed != c.Declared {
		c.Mismatch = c.Declared != KindUnknown
		c.Kind = c.Sniffed
		c.Source = "content"
	}
	return c
}

// sniffBytes is how much of a body Sniff looks at.
const sniffBytes = 8 << 10

// Sniff decides what CMS object the first bytes of a body are: binary BER or
// DER, or PEM or Base64 text of it. It reads the outer content type and, for
// a SignedData, far enough to tell an opaque signature from a detached one
// and from a bag of certificates when the bytes reach that far; a head that
// ends before it can tell is KindSignedData. It returns KindNone for bytes
// that are no CMS object, and KindUnknown, with the identifier, for a CMS
// content type the package does not read.
func Sniff(head []byte) (kind Kind, contentTypeOID string) {
	if len(head) == 0 {
		return KindNone, ""
	}
	if len(head) > sniffBytes {
		head = head[:sniffBytes]
	}
	lim := (&Limits{MaxElements: 1 << 16}).resolve()
	p := newParser(nil, lim)
	ber, _, ok := p.dearmor(head)
	if !ok {
		return KindNone, ""
	}
	p.data = ber
	top := p.top()
	ci, ok := top.next()
	if !ok || !ci.is(classUniversal, tagSequence) {
		return KindNone, ""
	}
	cc := p.in(ci, 1)
	typ, ok := cc.take(classUniversal, tagOID)
	if !ok || typ.truncated {
		return KindNone, ""
	}
	oid, ok := decodeOID(p.content(typ))
	if !ok {
		return KindNone, ""
	}
	switch oid {
	case OIDEnvelopedData:
		return KindEnvelopedData, oid
	case OIDAuthEnvelopedData:
		return KindAuthEnvelopedData, oid
	case OIDCompressedData:
		return KindCompressedData, oid
	case OIDSignedData:
	default:
		if !strings.HasPrefix(oid, "1.2.840.113549.1.") {
			// A SEQUENCE that starts with some identifier is not yet CMS.
			return KindNone, ""
		}
		return KindUnknown, oid
	}

	// SignedData: version, digest algorithms, encapsulated content info.
	wrap, ok := cc.take(classContext, 0)
	if !ok {
		return KindSignedData, oid
	}
	wc := p.in(wrap, 2)
	sd, ok := wc.take(classUniversal, tagSequence)
	if !ok {
		return KindSignedData, oid
	}
	sc := p.in(sd, 3)
	if _, ok := sc.take(classUniversal, tagInteger); !ok {
		return KindSignedData, oid
	}
	algs, ok := sc.take(classUniversal, tagSet)
	if !ok || algs.truncated {
		return KindSignedData, oid
	}
	eci, ok := sc.take(classUniversal, tagSequence)
	if !ok {
		return KindSignedData, oid
	}
	ec := p.in(eci, 4)
	if _, ok := ec.take(classUniversal, tagOID); !ok {
		return KindSignedData, oid
	}
	if _, ok := ec.take(classContext, 0); ok || eci.truncated {
		return KindSignedData, oid
	}
	// No content. With no digest algorithm either nothing is signed.
	if algs.body == algs.bodyEnd {
		return KindCertsOnly, oid
	}
	return KindSignature, oid
}
