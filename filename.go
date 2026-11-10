package message

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Filenames (ROB-050-009, ROB-050-010).
//
// A message can name the file of one part in four places: the filename
// parameter of Content-Disposition, in its RFC 2231 form and in its plain
// form, and the older name parameter of Content-Type, in the same two forms.
// Mail in the wild uses any of them, several at once, and not always with the
// same value. None is discarded here: each is a FilenameCandidate, and the one
// to use comes first.
//
// The name a message gives is the sender's, and is kept as it was given.
// Whether it is safe to use as the name of a file on disk is a separate
// question with a separate answer, SafeFilename, which is never applied
// implicitly.

// A FilenameSource says which parameter of which field a filename candidate
// comes from. The sources are declared in order of precedence.
type FilenameSource uint8

const (
	// FilenameDispositionExtended is the RFC 2231 form of the filename
	// parameter of Content-Disposition: filename*=… or filename*0=….
	FilenameDispositionExtended FilenameSource = iota + 1

	// FilenameDisposition is the plain filename parameter of
	// Content-Disposition.
	FilenameDisposition

	// FilenameContentTypeExtended is the RFC 2231 form of the name parameter of
	// Content-Type.
	FilenameContentTypeExtended

	// FilenameContentType is the plain name parameter of Content-Type, which
	// RFC 2183 supersedes and mail still carries.
	FilenameContentType
)

// String returns a short description of the source, such as
// "Content-Disposition filename*".
func (s FilenameSource) String() string {
	switch s {
	case FilenameDispositionExtended:
		return "Content-Disposition filename*"
	case FilenameDisposition:
		return "Content-Disposition filename"
	case FilenameContentTypeExtended:
		return "Content-Type name*"
	case FilenameContentType:
		return "Content-Type name"
	}
	return fmt.Sprintf("FilenameSource(%d)", uint8(s))
}

// A FilenameCandidate is one filename a part's header offers.
type FilenameCandidate struct {
	// Source is where the candidate comes from.
	Source FilenameSource

	// Filename is the name as the message gives it, decoded and otherwise
	// unchanged: it may hold path separators, control characters or anything
	// else, and it is not UTF-8 when Parameter.Undecoded is set. It is the
	// original name, for display or for records; SafeFilename derives a name
	// that can be given to a file.
	Filename string

	// Parameter is the parameter candidate the filename was read from, with
	// its raw text, its form and the charset it declares.
	Parameter ParameterCandidate
}

// filenameCandidates returns the filename candidates of a part whose
// Content-Disposition and Content-Type have the given parameter candidates:
// every one, the one to use first and the rest in order of precedence.
//
// The precedence is Content-Disposition filename*, Content-Disposition
// filename, Content-Type name*, Content-Type name; within one source, the
// order of the field. The candidate to use is the first in that order that is
// not empty and whose bytes were decoded, then the first that is not empty,
// and failing that the first.
func filenameCandidates(disposition, contentType []ParameterCandidate) []FilenameCandidate {
	var out []FilenameCandidate
	add := func(cands []ParameterCandidate, name string, extended bool, source FilenameSource) {
		for _, c := range cands {
			if c.Name == name && (c.Form != ParameterPlain) == extended {
				out = append(out, FilenameCandidate{Source: source, Filename: c.Value, Parameter: c})
			}
		}
	}
	add(disposition, "filename", true, FilenameDispositionExtended)
	add(disposition, "filename", false, FilenameDisposition)
	add(contentType, "name", true, FilenameContentTypeExtended)
	add(contentType, "name", false, FilenameContentType)

	pick := -1
	for i, c := range out {
		if c.Filename != "" && !c.Parameter.Undecoded {
			pick = i
			break
		}
	}
	if pick < 0 {
		for i, c := range out {
			if c.Filename != "" {
				pick = i
				break
			}
		}
	}
	if pick > 0 {
		chosen := out[pick]
		copy(out[1:pick+1], out[:pick])
		out[0] = chosen
	}
	return out
}

// filenamesConflict reports whether the candidates name more than one file:
// whether a candidate after the first has a name that is not empty and differs
// from the first's.
func filenamesConflict(names []FilenameCandidate) bool {
	for i := 1; i < len(names); i++ {
		if names[i].Filename != "" && names[i].Filename != names[0].Filename {
			return true
		}
	}
	return false
}

// filenameConflictDiagnostic is the diagnostic of an entity whose filename
// candidates differ.
func filenameConflictDiagnostic(names []FilenameCandidate) Diagnostic {
	field := "Content-Disposition"
	if names[0].Source >= FilenameContentTypeExtended {
		field = "Content-Type"
	}
	return Diagnostic{
		Code:     CodeMIMEFilenameConflict,
		Severity: SeverityInfo,
		Field:    field,
		Message:  fmt.Sprintf("the entity's header offers %d filename candidates that do not all name the same file", len(names)),
		Action:   "chose the " + names[0].Source.String() + " candidate by precedence (Content-Disposition filename*, filename, then Content-Type name*, name), kept every candidate available, and left the raw fields unchanged",
	}
}

// parameterCandidates reads the candidates of the parameters of the header's
// Content-Type and Content-Disposition as they are now, tolerantly and under
// the default parameter budgets.
func (h *Header) parameterCandidates() (contentType, disposition []ParameterCandidate) {
	var c parseContext
	contentType = c.candidatesOf("Content-Type", h.Get("Content-Type"), nil)
	disposition = c.candidatesOf("Content-Disposition", h.Get("Content-Disposition"), func() string {
		for _, c := range contentType {
			if c.Name == "charset" && c.Chosen {
				return c.Value
			}
		}
		return ""
	})
	return contentType, disposition
}

// FilenameCandidates returns every filename the header offers for its part,
// the one to use first, or nil when it names no file.
//
// The candidates come from four places, in this order of precedence: the
// RFC 2231 form of the Content-Disposition filename parameter (filename*=… or
// filename*0=…), its plain form, the RFC 2231 form of the Content-Type name
// parameter, and its plain form. Each occurrence is one candidate, so a
// parameter that repeats contributes several, in the order of the field. The
// first candidate returned is the one to use: the first by that precedence
// that is not empty and whose bytes could be decoded, then the first that is
// not empty, then the first. The rest follow in order of precedence, so a
// caller that disagrees with the choice has everything it needs to make
// another.
//
// Both fields are read tolerantly, as Parse reads them under ProfileTolerant:
// unquoted names with spaces, quotes that are never closed, unescaped Windows
// paths, RFC 2231 series in any order or with gaps, charsets decoded through
// CharsetReader, RFC 2047 encoded words and raw 8-bit bytes are all read
// rather than refused; see ParseMediaTypeTolerant. Unlike ContentType and
// ContentDisposition, which parse under no budget, they are read under the
// default parameter budgets of DefaultLimits. Nothing is recorded anywhere;
// Entity.FilenameCandidates is the reading that belongs to a parse.
//
// A candidate's Filename is the sender's name, never a safe one: it may hold
// path separators or control characters. Pass it to SafeFilename before using
// it as the name of a file.
func (h *Header) FilenameCandidates() []FilenameCandidate {
	contentType, disposition := h.parameterCandidates()
	return filenameCandidates(disposition, contentType)
}

// ParseMediaTypeTolerant reads a Content-Type value the way Parse reads one
// under ProfileTolerant, without a parse: it returns the media type, the
// parameters, and a diagnostic for every repair and every nonstandard form it
// read, and it never fails. An empty value is "text/plain" with no parameters,
// as for a part with no Content-Type.
//
// A value mime.ParseMediaType accepts is returned as that parser returns it,
// with RFC 2047 encoded words in parameter values decoded as Header.ContentType
// decodes them. What it refuses is read anyway: a type with no subtype or no
// slash by the defaults of RFC 2045 section 5.2 and RFC 2046 ("text" is
// text/plain, "multipart" multipart/mixed, anything unreadable
// application/octet-stream), values that are not tokens up to the next
// semicolon, quoted strings that are never closed to the end of the field,
// RFC 2231 values in any segment order and in any charset CharsetReader
// decodes, and repeated parameters by a fixed precedence — the RFC 2231 form
// first, then the first occurrence — except that a boundary repeated with
// different values has no value at all. The rules are set out at the top of
// mediatype_tolerant.go and in docs/robustness-notes/mime-parameters.md.
//
// A parameter value is UTF-8 unless its bytes could not be decoded, in which
// case it holds the bytes and a mime.parameter_unknown_charset diagnostic says
// so. The diagnostics carry a Code, a Severity, the field name, a Message and
// an Action, and no position, since v belongs to no input. The value is read
// under the default parameter budgets of DefaultLimits, and a value past one
// keeps the parameters within it, with a limit.* diagnostic. No header budget
// bounds a value a caller holds, so one longer than the default
// Limits.MaxHeaderFieldBytes of 64 KiB is still read, in time linear in its
// length, but keeps any RFC 2047 encoded words undecoded.
func ParseMediaTypeTolerant(v string) (mediaType string, params map[string]string, diags []Diagnostic) {
	if strings.TrimSpace(v) == "" {
		return "text/plain", nil, nil
	}
	r := tolerantField("Content-Type", v, nil)
	return r.value, r.params, r.owed
}

// ParseContentDispositionTolerant reads a Content-Disposition value the way
// Parse reads one under ProfileTolerant, on the terms of
// ParseMediaTypeTolerant. An empty value is an empty disposition with no
// parameters and no diagnostic. A value with parameters and no readable
// disposition type is an "attachment", which is how RFC 2183 section 2.8 has an
// unrecognized disposition treated, and a missing semicolon after the type —
// "attachment filename=a.pdf" — is read as if it were there.
func ParseContentDispositionTolerant(v string) (disposition string, params map[string]string, diags []Diagnostic) {
	if strings.TrimSpace(v) == "" {
		return "", nil, nil
	}
	r := tolerantField("Content-Disposition", v, nil)
	return r.value, r.params, r.owed
}

// ParseParameterCandidates returns every candidate value of every parameter
// of a Content-Type or Content-Disposition value, read as
// ParseMediaTypeTolerant and ParseContentDispositionTolerant read it. field is
// the name of the field v is the value of, "Content-Type" or
// "Content-Disposition" in any case; any other name returns nil.
//
// The parameter maps those functions return hold one value per parameter. This
// is the rest: each occurrence of a parameter that repeats, and both forms of
// one given as name and as name*, in the order of the field, each with the raw
// text it was read from, and the one that is in the map marked Chosen.
func ParseParameterCandidates(field, v string) []ParameterCandidate {
	var c parseContext
	switch {
	case strings.EqualFold(field, "Content-Type"):
		return c.candidatesOf("Content-Type", v, nil)
	case strings.EqualFold(field, "Content-Disposition"):
		return c.candidatesOf("Content-Disposition", v, nil)
	}
	return nil
}

// maxSafeFilenameBytes is the longest name SafeFilename returns, in bytes: the
// limit of one path component on the common filesystems.
const maxSafeFilenameBytes = 255

// unnamedFilename is what SafeFilename returns for a name with nothing usable
// in it.
const unnamedFilename = "unnamed"

// SafeFilename returns a name derived from name that is safe to use as the
// name of one file in a directory the caller chooses: a single path component,
// never empty, with nothing in it a filesystem or a shell treats specially.
//
// It is a pure function of its argument, and it is never applied implicitly:
// FilenameCandidate.Filename, the parameter maps and mail's Filename all keep
// the name the message gave. Derive the safe name where a file is created, and
// keep the original for display.
//
// The result is built as follows.
//
//   - Bytes that are not UTF-8 become "_".
//   - Everything up to the last slash or backslash is dropped, so no path
//     survives: "../../etc/passwd" and `C:\Users\a\b.txt` become "passwd" and
//     "b.txt".
//   - Control characters, NUL included, and the Unicode format characters that
//     reorder or hide text (bidirectional controls, zero-width spaces, the byte
//     order mark) are removed. The zero-width joiners are kept: they are
//     spelling in several scripts.
//   - The characters Windows does not allow in a name — < > : " | ? * — become
//     "_".
//   - Whitespace of any kind becomes a space, and leading and trailing spaces
//     and trailing dots are removed, since Windows drops them silently.
//   - A name that is only dots, such as "." and "..", has nothing left.
//   - A name whose stem is a Windows device name — CON, PRN, AUX, NUL, COM0 to
//     COM9, LPT0 to LPT9, CONIN$, CONOUT$, whatever its case and extension —
//     is prefixed with "_".
//   - A name longer than 255 bytes is cut on a UTF-8 boundary, keeping its
//     extension when the extension is short.
//   - A name with nothing left is "unnamed".
//
// It is idempotent: SafeFilename(SafeFilename(s)) is SafeFilename(s). It says
// nothing about whether a file of that name already exists, and two different
// names can have the same safe name; a caller that stores many files has to
// make them unique itself.
func SafeFilename(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}

	var b strings.Builder
	b.Grow(len(name))
	for len(name) > 0 {
		r, size := utf8.DecodeRuneInString(name)
		name = name[size:]
		switch {
		case r == utf8.RuneError && size <= 1:
			b.WriteByte('_')
		case unicode.IsSpace(r):
			b.WriteByte(' ')
		case unicode.IsControl(r), isInvisibleRune(r):
			// Dropped.
		case strings.ContainsRune(`<>:"|?*`, r):
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}

	s := trimFilename(b.String())
	if len(s) > maxSafeFilenameBytes {
		s = trimFilename(truncateFilename(s, maxSafeFilenameBytes))
	}
	if strings.Trim(s, ".") == "" {
		return unnamedFilename
	}
	if isWindowsDeviceName(s) {
		// The prefixed name is no device name, so cutting it again cannot make
		// it one.
		s = "_" + s
		if len(s) > maxSafeFilenameBytes {
			s = trimFilename(truncateFilename(s, maxSafeFilenameBytes))
		}
	}
	return s
}

// trimFilename removes the leading spaces and the trailing spaces and dots of
// a name.
func trimFilename(s string) string {
	return strings.TrimRight(strings.TrimLeft(s, " "), " .")
}

// isInvisibleRune reports whether r is a character that reorders or hides the
// text around it: a bidirectional control, a zero-width space, a byte order
// mark, a line or paragraph separator, or another format character.
func isInvisibleRune(r rune) bool {
	if r == '\u200c' || r == '\u200d' {
		// The zero-width joiners are spelling in several scripts and in emoji.
		return false
	}
	return unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029'
}

// isWindowsDeviceName reports whether the part of s before its first dot is a
// name Windows reserves for a device, in any case and with trailing spaces
// ignored, as Windows ignores them.
func isWindowsDeviceName(s string) bool {
	stem := s
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) {
		return stem[3] >= '0' && stem[3] <= '9'
	}
	// COM¹, COM², COM³ and their LPT counterparts are reserved as well.
	for _, p := range []string{"COM", "LPT"} {
		if rest, ok := strings.CutPrefix(stem, p); ok && (rest == "¹" || rest == "²" || rest == "³") {
			return true
		}
	}
	return false
}

// maxKeptExtensionBytes is the longest extension truncateFilename keeps whole.
const maxKeptExtensionBytes = 32

// truncateFilename cuts s to at most max bytes on a UTF-8 boundary. An
// extension of at most maxKeptExtensionBytes is kept and the stem is cut
// instead, so that a long name does not lose what says what kind of file it
// is.
func truncateFilename(s string, max int) string {
	ext := ""
	if i := strings.LastIndexByte(s, '.'); i > 0 && len(s)-i <= maxKeptExtensionBytes {
		s, ext = s[:i], s[i:]
	}
	keep := max - len(ext)
	if len(s) > keep {
		for keep > 0 && !utf8.RuneStart(s[keep]) {
			keep--
		}
		s = s[:keep]
	}
	return s + ext
}
