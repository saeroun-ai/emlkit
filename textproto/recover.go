package textproto

import (
	"bufio"
	"bytes"
	"io"
)

// Header recovery (ROB-020-003 to ROB-020-006, ROB-060-010).
//
// A header block read with HeaderLimits.Recover does not fail at a damaged
// line — one with no colon or semicolon, or whose name holds a byte a name
// cannot — that no continuation line follows. Such a line used to be the
// block's "malformed MIME header line" or "malformed MIME header key" error,
// which left a message with one damaged line unreadable. With Recover it is
// decided, in this order:
//
//  1. It continues the field before it — a continuation line that lost its
//     leading white space — when that field's value ends with ';' and the line
//     reads as a parameter ("name=..."); when it ends with ',' and the line
//     holds an '@' or itself ends with ','; or when it is a Received field
//     and the line begins one of its clauses, or follows its ';'. The field
//     keeps the line as its last physical line, exactly as read.
//  2. It is kept apart as a damaged line — an orphan record of one line — when
//     a line with a well-known field name follows it before the block's empty
//     line, within one buffer of the reader and recoverLines lines: the block
//     evidently goes on.
//  3. Otherwise the block ends before it, and the line is the first line of
//     the body: the empty line that should have separated them is missing.
//     The line's bytes are handed back, unread.
//
// A white space only line before such a body is a continuation of the field
// before it, as it always was; the block then ends at the first line after it
// that is no field, which is how a separator of spaces or tabs is read
// (ROB-020-004).
//
// A damaged line ended by a bare '\r' is decided as before (see settleCR), and
// so is one with an empty name. Nothing is looked at past one buffer of the
// reader, so the decision is the same however the input chunks its reads, and
// depends on the reader's size only below recoverWindow.

// recoverWindow and recoverLines bound the look past a damaged line.
const (
	recoverWindow = 4096
	recoverLines  = 64
)

// A headerHeal is what recovery did to one header block.
type headerHeal struct {
	joined   int   // lines joined to the field before them
	joinedAt int64 // where the first began, relative to the block

	damaged int // damaged lines kept apart, each one line

	sepMissing bool  // the block ended before a line that began the body
	sepAt      int64 // where that line began, relative to the block
	wsLine     bool  // the line before it was white space only
}

// UnindentedContinuations reports how many damaged lines of the block were
// read as continuation lines that had lost their leading white space, and
// where the first began, relative to the block. It is zero for a header not
// read with HeaderLimits.Recover.
func (h *Header) UnindentedContinuations() (n int, first int64) {
	if h == nil || h.heal == nil {
		return 0, 0
	}
	return h.heal.joined, h.heal.joinedAt
}

// BodySeparatorMissing reports that the block was ended, under
// HeaderLimits.Recover, before a damaged line that began the body, with no
// empty line between them: where that line began, relative to the block, and
// whether the line before it held only white space.
func (h *Header) BodySeparatorMissing() (missing bool, at int64, whitespaceLine bool) {
	if h == nil || h.heal == nil {
		return false, 0, false
	}
	return h.heal.sepMissing, h.heal.sepAt, h.heal.wsLine
}

// ReadHeaderRecover is ReadHeaderLimits with limits.Recover set. When the
// block ended before a line that begins the body, unread holds that line's
// bytes, which were read out of r and belong to what follows the header: the
// caller reads them before anything else it reads from r.
func ReadHeaderRecover(r *bufio.Reader, limits HeaderLimits) (h Header, unread []byte, err error) {
	limits.Recover = true
	scan := &headerScan{lim: limits}
	h, err = readHeader(r, scan)
	if isBudgetExhausted(err) {
		return Header{}, nil, scan.limitError()
	}
	return h, scan.unread, err
}

// unindented reports whether line, a damaged candidate, reads as a
// continuation of the field p that lost its leading white space.
func (p *pendingField) unindented(line []byte) bool {
	if p.kind != pendField || p.kv == nil || p.valueStart > len(p.kv) {
		return false
	}
	value := bytes.TrimRight(p.kv[p.valueStart:], " \t\r\n")
	if len(value) == 0 {
		return false
	}
	switch value[len(value)-1] {
	case ';':
		// A parameter; or, in a Received field, the date after its clauses.
		return parameterLike(line) || p.key == "Received"
	case ',':
		// The next item of a list: an address or a message identifier, or an
		// item that itself ends with a comma.
		t := bytes.TrimRight(line, " \t\r\n")
		return bytes.IndexByte(t, '@') >= 0 || (len(t) > 0 && t[len(t)-1] == ',')
	}
	if p.key == "Received" {
		return receivedClause(line)
	}
	return false
}

// parameterLike reports whether line begins token *WSP "=".
func parameterLike(line []byte) bool {
	i := 0
	for i < len(line) && isTokenByte(line[i]) {
		i++
	}
	if i == 0 {
		return false
	}
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	return i < len(line) && line[i] == '='
}

func isTokenByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return bytes.IndexByte([]byte("!#$%&'*+-.^_`|~"), c) >= 0
}

// receivedClause reports whether line begins a clause of a Received field.
func receivedClause(line []byte) bool {
	for _, w := range []string{"from ", "by ", "via ", "with ", "id ", "for ", "(", ";"} {
		if len(line) >= len(w) && bytes.EqualFold(line[:len(w)], []byte(w)) {
			return true
		}
	}
	return false
}

// knownFieldNames are field names that say a header block goes on. Names with
// the prefixes of knownFieldPrefixes are known too.
var knownFieldNames = map[string]bool{
	"from": true, "to": true, "cc": true, "bcc": true, "subject": true, "date": true,
	"sender": true, "reply-to": true, "message-id": true, "in-reply-to": true,
	"references": true, "mime-version": true, "received": true, "return-path": true,
	"delivered-to": true, "envelope-to": true, "errors-to": true, "importance": true,
	"priority": true, "precedence": true, "organization": true, "user-agent": true,
	"keywords": true, "comments": true, "thread-topic": true, "thread-index": true,
	"auto-submitted": true, "mailing-list": true, "disposition-notification-to": true,
	"return-receipt-to": true, "accept-language": true, "authentication-results": true,
	"dkim-signature": true, "domainkey-signature": true, "received-spf": true,
	"feedback-id": true, "require-recipient-valid-since": true, "sensitivity": true,
	"apparently-to": true, "status": true, "lines": true, "newsgroups": true,
	"path": true, "followup-to": true, "expires": true, "archived-at": true,
	"autocrypt": true, "openpgp": true, "bimi-selector": true, "tls-required": true,
}

var knownFieldPrefixes = []string{"x-", "content-", "list-", "resent-", "arc-", "original-", "dkim-"}

// knownField reports whether line begins with a well-known field name, any
// white space and a colon.
func knownField(line []byte) bool {
	i := bytes.IndexByte(line, ':')
	if i <= 0 || i > 64 {
		return false
	}
	name := bytes.TrimRight(line[:i], " \t")
	if len(name) == 0 {
		return false
	}
	var lower [64]byte
	for j, c := range name {
		if !validHeaderKeyByte(c) {
			return false
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		lower[j] = c
	}
	n := lower[:len(name)]
	if knownFieldNames[string(n)] {
		return true
	}
	for _, p := range knownFieldPrefixes {
		if len(n) > len(p) && string(n[:len(p)]) == p {
			return true
		}
	}
	return false
}

// goesOn reports whether the lines after a damaged line, which r stands at,
// show that the header block goes on: a line with a well-known field name
// before the block's empty line, the end of the input, a delimiter line of
// the part's multipart, recoverLines lines or the end of what one buffer of r
// holds, at most recoverWindow bytes. Nothing is consumed. A failure of the
// input the look met, other than io.EOF, is returned.
func (s *headerScan) goesOn(r *bufio.Reader) (bool, error) {
	buf, _ := s.peek(r, min(r.Size(), recoverWindow))
	if s.fail != nil && s.fail != io.EOF {
		return false, s.fail
	}
	eof := s.fail == io.EOF
	for pos, lines := 0, 0; pos < len(buf) && lines < recoverLines; lines++ {
		j := bytes.IndexAny(buf[pos:], "\r\n")
		var line []byte
		next := len(buf)
		switch {
		case j < 0 && !eof:
			// A line the window cut: not known.
			return false, nil
		case j < 0:
			line = buf[pos:]
		default:
			line = buf[pos : pos+j]
			next = pos + j + 1
			if buf[pos+j] == '\r' && next < len(buf) && buf[next] == '\n' {
				next++
			}
		}
		switch {
		case len(line) == 0:
			return false, nil
		case line[0] == ' ' || line[0] == '\t':
		case s.parent != nil && delimiterOf(s.parent, line):
			return false, nil
		case knownField(line):
			return true, nil
		}
		pos = next
	}
	return false, nil
}

// delimiterOf reports whether line, without its line ending, is a delimiter
// line of mr.
func delimiterOf(mr *MultipartReader, line []byte) bool {
	ok, _ := mr.delimiterLine(line)
	return ok
}

// blankLine reports whether line holds only white space and line endings.
func blankLine(line []byte) bool {
	return len(bytes.TrimLeft(line, " \t\r\n")) == 0
}
