package mail

import (
	"errors"
	"io"
	"mime"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/saeroun-ai/emlkit"
)

// The tolerant accessors of this package (Header.AddressListTolerant,
// Header.DateTolerant, Header.MessageIDTolerant, Header.MsgIDListTolerant,
// Header.Received, Header.ListURLs, Header.ListID and the Parse*Tolerant
// functions behind them) read a structured header field the way the strict
// accessors do when the field is well formed, and otherwise keep everything
// that can still be understood: a damaged entry does not cost its valid
// neighbours, the original text of every entry stays available, and each
// repair is reported as a message.Diagnostic.
//
// They belong to no ParseSession, so they hold no configured budget. Their
// work is bounded structurally instead: every scanner is a single pass over
// the value with counters rather than recursion, a construct that is never
// closed is looked for once and then treated as literal text, and the two
// constants below bound what one call returns.
const (
	// maxTolerantEntries bounds the entries (addresses, message identifiers,
	// list URLs, Received clauses) one call returns. The input that follows
	// the last entry is left unparsed and reported; it stays in the result's
	// Raw value.
	maxTolerantEntries = 16384

	// maxTolerantDiagnostics bounds the diagnostics one call returns. As with
	// Limits.MaxDiagnosticCount, the request that reaches the bound is
	// replaced by one limit.diagnostic_count diagnostic and later repairs are
	// applied without being reported.
	maxTolerantDiagnostics = 64
)

const tolerantDiagnosticCountAction = "the diagnostic that reached this bound and every later one of this header field are omitted; parsing continued"

// diagnosticList collects the diagnostics of one tolerant accessor call.
type diagnosticList struct {
	field string
	list  []message.Diagnostic
	full  bool
}

// add records one diagnostic, or the terminal limit.diagnostic_count one in
// its place when it would be the last the bound admits.
func (d *diagnosticList) add(code message.Code, severity message.Severity, msg, action string) {
	if d.full {
		return
	}
	if len(d.list) >= maxTolerantDiagnostics-1 {
		d.full = true
		d.list = append(d.list, message.Diagnostic{
			Code:     message.CodeLimitDiagnosticCount,
			Severity: message.SeverityLimitExceeded,
			Message:  "mail: header field reached the limit of " + strconv.Itoa(maxTolerantDiagnostics) + " diagnostics",
			Field:    d.field,
			Action:   tolerantDiagnosticCountAction,
		})
		return
	}
	d.list = append(d.list, message.Diagnostic{
		Code:     code,
		Severity: severity,
		Message:  msg,
		Field:    d.field,
		Action:   action,
	})
}

// recovered records a repair that kept a usable value.
func (d *diagnosticList) recovered(code message.Code, msg, action string) {
	d.add(code, message.SeverityRecovered, msg, action)
}

// invalid records a value that could not be given a typed meaning and was kept
// raw only.
func (d *diagnosticList) invalid(code message.Code, msg, action string) {
	d.add(code, message.SeverityInvalidMetadata, msg, action)
}

// isWSP reports whether c is whitespace inside a header field value. A field
// read from a message is already unfolded, but a value set by a caller may
// still hold line breaks, so CR and LF count as whitespace too.
func isWSP(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// isControl reports whether c is a C0 control character other than the
// whitespace isWSP accepts, or DEL.
func isControl(c byte) bool {
	return (c < 0x20 && !isWSP(c)) || c == 0x7f
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

func isAlpha(c byte) bool { return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') }

// hasControl reports whether s holds a control character.
func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if isControl(s[i]) {
			return true
		}
	}
	return false
}

// isASCII reports whether s holds no byte above 0x7f.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// stripControl returns s without control characters.
func stripControl(s string) string {
	if !hasControl(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if !isControl(s[i]) {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// stripWSP returns s without whitespace.
func stripWSP(s string) string {
	i := 0
	for i < len(s) && !isWSP(s[i]) {
		i++
	}
	if i == len(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	b.WriteString(s[:i])
	for ; i < len(s); i++ {
		if !isWSP(s[i]) {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// trimWSP returns s without leading and trailing whitespace.
func trimWSP(s string) string {
	for len(s) > 0 && isWSP(s[0]) {
		s = s[1:]
	}
	for len(s) > 0 && isWSP(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	return s
}

// scanComment returns the index just past the ')' that closes the comment
// opened by the '(' at s[i], and whether there is one. Like
// headerParser.skipComment it is one pass with a depth counter: a backslash
// escapes the byte after it, and nesting costs nothing beyond the counter.
func scanComment(s string, i int) (end int, ok bool) {
	depth := 0
	for ; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return len(s), false
}

// scanQuoted returns the index just past the '"' that closes the quoted string
// opened by the '"' at s[i], and whether there is one. A backslash escapes the
// byte after it.
func scanQuoted(s string, i int) (end int, ok bool) {
	for i++; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i + 1, true
		}
	}
	return len(s), false
}

// unescape removes the backslash of every quoted-pair in s.
func unescape(s string) string {
	if strings.IndexByte(s, '\\') < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// commentText returns the text of the comment s, which includes its outer
// parentheses, with quoted-pairs resolved.
func commentText(s string) string {
	if len(s) >= 2 {
		s = s[1 : len(s)-1]
	}
	return trimWSP(unescape(s))
}

// isEncodedWord reports whether s has the outer shape of an RFC 2047
// encoded-word.
func isEncodedWord(s string) bool {
	return len(s) >= 8 && strings.HasPrefix(s, "=?") && strings.HasSuffix(s, "?=") && strings.Count(s, "?") == 4
}

// decodeWord decodes s if it is an RFC 2047 encoded-word the configured
// charsets can read. Otherwise it returns s unchanged: an encoded-word that
// cannot be decoded is kept as it was written, and unknownCharset reports
// that its charset, rather than its encoding, is what could not be read.
func decodeWord(s string) (decoded string, wasEncoded, unknownCharset bool) {
	if !isEncodedWord(s) {
		return s, false, false
	}
	// The decoder reads utf-8, us-ascii and iso-8859-1 itself and asks this
	// function for any other charset, so a failure here is a charset failure.
	dec := mime.WordDecoder{CharsetReader: func(charset string, input io.Reader) (io.Reader, error) {
		if message.CharsetReader == nil {
			unknownCharset = true
			return nil, errors.New("mail: unhandled charset " + strconv.Quote(charset))
		}
		r, err := message.CharsetReader(charset, input)
		if err != nil {
			unknownCharset = true
		}
		return r, err
	}}
	out, err := dec.Decode(s)
	if err != nil {
		return s, false, unknownCharset
	}
	return out, true, false
}

// quoteForMessage renders an input fragment for a diagnostic message, bounded
// so one diagnostic cannot repeat a whole oversized field.
func quoteForMessage(s string) string {
	const max = 64
	if len(s) > max {
		return strconv.Quote(s[:max]) + "..."
	}
	return strconv.Quote(s)
}
