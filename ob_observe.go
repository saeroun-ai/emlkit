package message

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Observations a parse makes of bytes it reads as they stand (ROB-130-003,
// the corpus findings nul-in-text, subject-5000, mbox-from-line and
// cut-in-base64-attachment).
//
// None of them changes what is parsed: they say that the input holds
// something a spam classifier, or anyone deciding how far to trust the
// decoded view, cares about. Each is recorded under Parse with every profile
// but ProfileStrict, which keeps the diagnostics it always had, at most once
// per body or header block, after the bytes it is about have been read whole,
// so that how the input is delivered cannot change it; and its Message holds
// no byte of the input.

// Diagnostic codes of these observations.
const (
	// CodeBodyNUL reports, at SeverityInfo, a text/* leaf body under a 7bit,
	// 8bit or absent transfer encoding that holds a NUL byte, which RFC 2045
	// section 2.7 and 2.8 forbid in such data; text after a NUL is hidden
	// from many readers. Recorded once per body, at the first NUL, when the
	// body ends with io.EOF. Every byte is kept.
	CodeBodyNUL Code = "body.nul"

	// CodeHeaderOverlongLine reports, at SeverityInfo, a header block with a
	// line longer than the 998 bytes RFC 5322 section 2.1.1 allows before its
	// line ending. Recorded once per block, at the first such line, with the
	// field it belongs to in Field (empty for a damaged line kept apart). The
	// line is read whole as before; Limits.MaxLineBytes is what refuses one.
	CodeHeaderOverlongLine Code = "header.overlong_line"
)

// obRFC5322LineMax is how many bytes RFC 5322 lets a line hold before its
// CRLF.
const obRFC5322LineMax = 998

// obWatchNUL returns the decoded body of a leaf whose transfer encoding is name
// and whose media type is mediaType, watched for a NUL byte when it is text
// that no transfer encoding protects, or body unchanged. Text declared in a
// charset of 16 or 32 bits a character, whose bytes are NUL for every ASCII
// character, is not watched.
func (c parseContext) obWatchNUL(name, mediaType, charset string, body io.Reader) io.Reader {
	if c.session == nil || c.session.Profile() == ProfileStrict || !strings.HasPrefix(mediaType, "text/") || obWideCharset(charset) {
		return body
	}
	switch name {
	case "", "7bit", "8bit":
		return &obNULMonitor{r: body, ctx: c, at: -1}
	}
	return body
}

// obWideCharset reports whether a charset label names UTF-16, UTF-32, UCS-2 or
// UCS-4, in any of the spellings labels come in.
func obWideCharset(label string) bool {
	s := strings.Map(func(r rune) rune {
		switch r {
		case '-', '_', ' ', '"':
			return -1
		}
		return r
	}, strings.ToLower(label))
	for _, p := range []string{"utf16", "utf32", "ucs2", "ucs4", "iso10646ucs"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return s == "unicode" || s == "unicodefffe" || s == "unicodefeff"
}

// An obNULMonitor passes a body through unchanged and records body.nul once,
// for the first NUL byte in it, when the body ends with io.EOF. A body that
// begins with a UTF-16 or UTF-32 byte order mark is in a charset of its own,
// whatever it declares, and records nothing; charset.bom_mismatch says so.
//
// It is recorded at the end, and an end its source returns together with bytes
// is held for the next read, so that every reader after this one has seen every
// byte of the body before it: what those readers record about the body's bytes
// comes first, and what they record about its end after, however the bytes
// were chunked. It holds nothing but that end.
type obNULMonitor struct {
	r        io.Reader
	ctx      parseContext
	consumed int64
	at       int64 // where the first NUL is, or -1
	held     error // the end the source returned with the last bytes
	ended    bool
	mark     [4]byte // the body's first bytes, for a byte order mark
}

func (m *obNULMonitor) Read(p []byte) (int, error) {
	if m.held != nil {
		m.end(m.held)
		return 0, m.held
	}
	n, err := m.r.Read(p)
	n = max(0, min(n, len(p)))
	if m.consumed < int64(len(m.mark)) {
		copy(m.mark[m.consumed:], p[:n])
	}
	if m.at < 0 {
		if i := bytes.IndexByte(p[:n], 0); i >= 0 {
			m.at = m.consumed + int64(i)
		}
	}
	m.consumed += int64(n)
	if err != nil {
		if n > 0 {
			m.held = err
			return n, nil
		}
		m.end(err)
	}
	return n, err
}

// end records the observation for a body that ended with err: only io.EOF,
// for a body whose input failed is reported by that failure.
func (m *obNULMonitor) end(err error) {
	if m.ended {
		return
	}
	m.ended = true
	if err != io.EOF || m.at < 0 || obWideMark(m.mark[:min(m.consumed, int64(len(m.mark)))]) {
		return
	}
	// A sink that stops the parse here is the session's stop; the body has
	// ended either way.
	_ = m.ctx.recordBody(Diagnostic{
		Code:     CodeBodyNUL,
		Severity: SeverityInfo,
		Message:  "text body holds a NUL byte, which no transfer encoding protects",
		Action:   "kept every byte",
		Location: m.ctx.bodyLocationAt(m.at),
	})
}

// obWideMark reports whether b, the first bytes of a body, begin with a UTF-16
// or UTF-32 byte order mark.
func obWideMark(b []byte) bool {
	return bytes.HasPrefix(b, []byte{0xfe, 0xff}) || bytes.HasPrefix(b, []byte{0xff, 0xfe}) ||
		bytes.HasPrefix(b, []byte{0, 0, 0xfe, 0xff})
}

// obOverlongLineNotes appends the header.overlong_line note of h, the header
// block of the entity this context describes, when one of its lines is longer
// than RFC 5322 allows. ProfileStrict records none.
func (c parseContext) obOverlongLineNotes(h *textproto.Header, strict bool, notes []headerNote) []headerNote {
	if strict {
		return notes
	}
	key, start, n, ok := h.LongLine(obRFC5322LineMax)
	if !ok {
		return notes
	}
	return append(notes, headerNote{at: start, d: Diagnostic{
		Code:     CodeHeaderOverlongLine,
		Severity: SeverityInfo,
		Message:  fmt.Sprintf("a header block has a line of %d bytes, more than the %d RFC 5322 allows", n, obRFC5322LineMax),
		Field:    key,
		Action:   "read the line whole",
		Location: c.headerSpan(start, n),
	}})
}

// obMboxHint adds to d, the header.invalid_name note of h, that the damaged
// line at offset at of the block — the first it notes — is the first line of
// the input and an mbox "From_" envelope line of exactly the grammar
// ParseOptions.RecognizeMboxEnvelope recognizes, which was not asked for. What
// is parsed does not change: the line stays a damaged line kept apart.
func (c parseContext) obMboxHint(h *textproto.Header, at int64, d *Diagnostic) {
	s := c.session
	o := c.origin
	if s == nil || s.cfg.mbox || at != 0 || !o.known || o.unplaced || len(o.path) != 0 || o.headerOffset != 0 || c.msgDepth != 0 {
		return
	}
	var raw []byte
	for _, r := range h.OrphanContinuations() {
		if start, _ := r.Location(); start == at {
			raw = r.Bytes()
			break
		}
	}
	line := raw
	if i := bytes.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	} else {
		// A line the input ended before any line ending is no envelope.
		return
	}
	limit := maxMboxEnvelopeContent
	if l := s.Limits().MaxLineBytes; l > 0 && l < int64(limit) {
		limit = int(l)
	}
	const prefix = "From "
	if len(line) > limit || !bytes.HasPrefix(line, []byte(prefix)) {
		return
	}
	for _, b := range line {
		if b != ' ' && b != '\t' && (b < 0x21 || b > 0x7e) {
			return
		}
	}
	if !mboxEnvelopeText(string(line[len(prefix):])) {
		return
	}
	d.Message += "; the first, the first line of the input, is an mbox From_ envelope line, which ParseOptions.RecognizeMboxEnvelope keeps apart from the message"
	d.Action += "; set ParseOptions.RecognizeMboxEnvelope to read such a line as the mbox envelope"
}

// obInputCut reports whether the body this decoder reads, consumed bytes long,
// ended because the input did: it is a part of a multipart, the input ended
// with io.EOF, and the body's last byte is the input's last byte, so no
// closing delimiter of any multipart it is in followed it. The root's own body
// always ends where the input ends, so for it nothing can be told.
func (n *cteNotes) obInputCut(consumed int64) bool {
	if n == nil || n.ctx.session == nil || n.ctx.bounds == nil {
		return false
	}
	o := n.ctx.origin
	if !o.known || o.unplaced {
		return false
	}
	s := n.ctx.session
	s.mu.Lock()
	end := s.inputEnd
	s.mu.Unlock()
	return end == io.EOF && o.bodyOffset+consumed == s.prog.inputBytes.Load()
}
