package message

import "strconv"

// A Profile selects how much interpretation the parser is allowed to apply to
// a malformed message and how much of the original input it keeps.
//
// Profiles never change the result for conformant input: a well-formed message
// parses identically under every profile.
//
// The constants below describe what each profile is for; what each does today
// is what its producers implement, documented on Parse. In particular no
// profile validates RFC 5322's required fields, and none reconstructs the
// input's original bytes: a header ended by io.EOF, or one with no fields or
// no body, is framed the same under every profile and every Compatibility,
// while what its fields say is interpreted as usual, and Entity.WriteTo
// serializes such an input rather than replaying it (ROB-010-001).
type Profile uint8

const (
	// ProfileTolerant applies known deterministic repairs, reports each of
	// them as a Diagnostic, and returns usable partial data for truncated
	// entities. It is the recommended production profile, and it is
	// deliberately the zero value so that an unset Profile is the default.
	ProfileTolerant Profile = iota

	// ProfileStrict stops the current entity when an RFC violation affects
	// framing or semantics. Already parsed parents and siblings remain
	// available. Features that are syntactically valid but unsupported are
	// not violations; their raw form stays usable. A header written with bare
	// LF line endings is accepted, as it always has been; one with a line
	// ended by a bare CR, or a last line the input ended before any line
	// ending, is refused for that entity, and a multipart with a bare CR
	// before or after a delimiter line is refused for its remaining parts
	// (ROB-010-002).
	ProfileStrict

	// ProfileForensic includes every tolerant result and diagnostic, and
	// additionally preserves raw spans for the message, headers, fields,
	// preamble, epilogue, parts, transfer-encoded bodies and damaged regions
	// that could not be assigned a meaning.
	ProfileForensic
)

// String returns the canonical lower-case name of the profile, or a
// "Profile(n)" placeholder for an unknown value.
func (p Profile) String() string {
	switch p {
	case ProfileTolerant:
		return "tolerant"
	case ProfileStrict:
		return "strict"
	case ProfileForensic:
		return "forensic"
	}
	return "Profile(" + strconv.Itoa(int(p)) + ")"
}

// A Compatibility names the policy used to break ties when a malformed
// construct has several plausible interpretations. It tunes individual
// decisions without creating a separate parser, and it is orthogonal to
// Profile.
type Compatibility uint8

const (
	// CompatibilityConservative prefers raw or incomplete results over
	// speculative structure. It is the default policy and therefore the zero
	// value.
	CompatibilityConservative Compatibility = iota

	// CompatibilityAngus follows Eclipse Angus Mail where its repairs differ
	// from the conservative choice.
	CompatibilityAngus

	// CompatibilityMimeKit follows MimeKit's loose structured-header and MIME
	// parsing choices.
	CompatibilityMimeKit

	// CompatibilityPythonEmail follows the defect handling of Python's email
	// package.
	CompatibilityPythonEmail
)

// String returns the canonical lower-case name of the policy, or a
// "Compatibility(n)" placeholder for an unknown value.
func (c Compatibility) String() string {
	switch c {
	case CompatibilityConservative:
		return "conservative"
	case CompatibilityAngus:
		return "angus"
	case CompatibilityMimeKit:
		return "mimekit"
	case CompatibilityPythonEmail:
		return "python-email"
	}
	return "Compatibility(" + strconv.Itoa(int(c)) + ")"
}
