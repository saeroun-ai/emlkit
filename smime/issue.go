package smime

import (
	"errors"
	"strconv"
)

// A Severity says how much an Issue cost.
type Severity uint8

const (
	// Info is a notable property of the input that lost nothing: BER where
	// DER was expected, a PEM wrapper, a detached signature.
	Info Severity = iota
	// Warning is a defect that was tolerated: the result is what a lenient
	// reader would make of the input.
	Warning
	// Error is a defect that lost something: a truncated object, a budget
	// that was reached, a certificate that does not parse.
	Error
)

// String returns "info", "warning" or "error".
func (s Severity) String() string {
	switch s {
	case Info:
		return "info"
	case Warning:
		return "warning"
	case Error:
		return "error"
	}
	return "Severity(" + strconv.Itoa(int(s)) + ")"
}

// A Code is the stable identifier of an Issue. Program logic branches on the
// Code, never on the Message.
type Code string

// The codes this package reports.
const (
	// Armor: the input was not bare DER.
	CodeArmorPEM      Code = "armor.pem"           // PEM wrapper removed
	CodeArmorBase64   Code = "armor.base64"        // Base64 text decoded
	CodeArmorNested   Code = "armor.nested"        // encoded more than once
	CodeArmorTrailing Code = "armor.trailing_data" // text after the Base64 ended

	// BER: the encoding layer.
	CodeBERIndefinite      Code = "ber.indefinite_length"    // BER indefinite lengths used
	CodeBERTruncated       Code = "ber.truncated"            // the input ended inside an element
	CodeBERLength          Code = "ber.length_exceeds_input" // declared length runs past the input
	CodeBERLengthInvalid   Code = "ber.length_invalid"       // reserved or oversized length form
	CodeBERTag             Code = "ber.tag_invalid"          // tag number too large
	CodeBERIndefPrimitive  Code = "ber.indefinite_primitive" // indefinite length on a primitive
	CodeBERUnexpectedEOC   Code = "ber.unexpected_eoc"       // end-of-contents in a definite element
	CodeLimitDepth         Code = "limit.depth"
	CodeLimitElements      Code = "limit.elements"
	CodeLimitInput         Code = "limit.input_bytes"
	CodeLimitContent       Code = "limit.content_bytes"
	CodeLimitInflated      Code = "limit.inflated_bytes"
	CodeLimitCertificates  Code = "limit.certificates"
	CodeLimitSigners       Code = "limit.signers"
	CodeLimitRecipients    Code = "limit.recipients"
	CodeLimitAttributes    Code = "limit.attributes"
	CodeLimitIssues        Code = "limit.issues"
	CodeLimitCertificateSz Code = "limit.certificate_bytes"

	// CMS: the structure.
	CodeCMSUnexpected       Code = "cms.unexpected_element"       // an element of another type than the syntax has there
	CodeCMSMissing          Code = "cms.missing_field"            // a required field is absent
	CodeCMSTrailing         Code = "cms.trailing_data"            // bytes after the ContentInfo
	CodeCMSUnknownType      Code = "cms.unknown_content_type"     // a content type this package does not read
	CodeCMSContentAbsent    Code = "cms.content_absent"           // SignedData without eContent: detached
	CodeCMSChunked          Code = "cms.chunked_content"          // content reassembled from a constructed OCTET STRING
	CodeCMSChunkInvalid     Code = "cms.chunk_invalid"            // a chunk that is not an OCTET STRING
	CodeCMSContentNotOctets Code = "cms.content_not_octet_string" // PKCS #7 style content; kept as its encoding
	CodeCMSOID              Code = "cms.oid_invalid"
	CodeCMSCertificate      Code = "cms.certificate_invalid" // x509 could not parse it; raw bytes kept
	CodeCMSCertificateOther Code = "cms.certificate_other"   // attribute or other certificate format
	CodeCMSAttribute        Code = "cms.attribute_invalid"
	CodeCMSAttributeRepeat  Code = "cms.attribute_repeated"
	CodeCMSSigningTime      Code = "cms.signing_time_invalid"
	CodeCMSAttrsNotDER      Code = "cms.signed_attributes_not_der"
	CodeCMSNoSigners        Code = "cms.no_signers"
	CodeCMSInflate          Code = "cms.inflate_failed"
	CodeCMSCompression      Code = "cms.compression_unknown"
)

// An Issue is one thing the reader tolerated, repaired or lost.
type Issue struct {
	// Code identifies the observation.
	Code Code
	// Severity is what it cost.
	Severity Severity
	// Offset is the position in the DER the observation is about — the input
	// itself, or what it decoded to when it was PEM or Base64 — or -1.
	Offset int64
	// Message describes it for a person. It is not part of the contract.
	Message string
}

// String renders the issue as "severity code at offset: message".
func (i Issue) String() string {
	s := i.Severity.String() + " " + string(i.Code)
	if i.Offset >= 0 {
		s += " at " + strconv.FormatInt(i.Offset, 10)
	}
	if i.Message != "" {
		s += ": " + i.Message
	}
	return s
}

// HasIssue reports whether issues holds one with the given code.
func HasIssue(issues []Issue, code Code) bool {
	for i := range issues {
		if issues[i].Code == code {
			return true
		}
	}
	return false
}

// Errors returned when nothing usable could be read.
var (
	// ErrNotCMS reports input that is not a CMS ContentInfo, in DER, BER, PEM
	// or Base64.
	ErrNotCMS = errors.New("smime: not a CMS object")
	// ErrLimitExceeded reports that Limits.MaxElements was used up before
	// the outer structure of the object could be read.
	ErrLimitExceeded = errors.New("smime: element budget exceeded")
	// ErrNotSignedData reports a CMS object of another content type where
	// SignedData was asked for.
	ErrNotSignedData = errors.New("smime: not CMS SignedData")
	// ErrNotEnveloped reports a CMS object that holds no EnvelopedData or
	// AuthEnvelopedData where one was to be decrypted.
	ErrNotEnveloped = errors.New("smime: not CMS EnvelopedData")
	// ErrNoRecipient reports that a Decrypter found no recipient it holds the
	// key of.
	ErrNoRecipient = errors.New("smime: no matching recipient")
	// ErrUnsupported reports an algorithm a Decrypter does not implement.
	ErrUnsupported = errors.New("smime: unsupported algorithm")
)
