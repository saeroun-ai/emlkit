package message

import (
	"io"
	"mime"
	"strings"
)

// Tolerant transfer decoding (ROB-070-001 to ROB-070-009).
//
// A transfer-encoded body that is damaged is still decoded as far as its bytes
// say anything: the decoders here never fail on what the body holds, only on
// what its input fails with, and what they had to assume is reported once per
// kind and per body as a diagnostic of the parse the body belongs to. A body
// built without a parse — Read, ReadWithOptions, New — is decoded the same way
// and reports nothing, having nowhere to report to.
//
// Under ProfileStrict the long-standing decoders are used instead: a Base64
// body with a byte outside its alphabet ends with the standard library's
// corruption error after the bytes before it, and nothing is assumed.
//
// Every decoder is a stream: it holds one fixed input buffer and the output of
// that buffer, never the body, and it never returns no bytes and no error
// unless its input did.

// A cteNote is one kind of repair a transfer decoder reports.
type cteNote uint8

const (
	noteB64Junk cteNote = iota
	noteB64PaddingMissing
	noteB64PaddingInvalid
	noteB64DataAfterPadding
	noteB64LengthInvalid
	noteB64URLAlphabet
	noteQPInvalidHex
	noteQPTrailingEquals
	noteQPInvalidSoftBreak
	noteQPControlByte
	noteQPOverlongLine
	noteUUInvalid
	noteUUMissingBegin
	noteUUMissingEnd
	noteSevenBit
	cteNoteCount
)

// cteNoteInfo is what each note records.
var cteNoteInfo = [cteNoteCount]struct {
	code     Code
	severity Severity
	message  string
	action   string
}{
	noteB64Junk:             {CodeCTEBase64Junk, SeverityRecovered, "base64 body holds bytes outside the base64 alphabet", "skipped those bytes and decoded the rest"},
	noteB64PaddingMissing:   {CodeCTEBase64PaddingMissing, SeverityRecovered, "base64 body ends without the padding its length needs", "decoded the final incomplete group as if it were padded"},
	noteB64PaddingInvalid:   {CodeCTEBase64PaddingInvalid, SeverityRecovered, "base64 body holds padding where none belongs", "ignored the misplaced padding"},
	noteB64DataAfterPadding: {CodeCTEBase64DataAfterPadding, SeverityRecovered, "base64 body continues after its padding", "decoded the data after the padding as a further base64 segment, or ignored it under CompatibilityPythonEmail"},
	noteB64LengthInvalid:    {CodeCTEBase64LengthInvalid, SeverityIncomplete, "base64 body ends with a single character that encodes no whole byte", "decoded everything before it and dropped that character"},
	noteB64URLAlphabet:      {CodeCTEBase64URLAlphabet, SeverityRecovered, "base64 body uses the URL-safe alphabet", "read '-' and '_' as '+' and '/'"},
	noteQPInvalidHex:        {CodeCTEQPInvalidHex, SeverityRecovered, "quoted-printable body holds '=' not followed by two hexadecimal digits", "kept the '=' and what follows it as literal text"},
	noteQPTrailingEquals:    {CodeCTEQPTrailingEquals, SeverityRecovered, "quoted-printable body ends with '='", "kept the '=' as literal text"},
	noteQPInvalidSoftBreak:  {CodeCTEQPInvalidSoftBreak, SeverityRecovered, "quoted-printable soft line break is followed by white space or ended by a bare CR", "read it as a soft line break"},
	noteQPControlByte:       {CodeCTEQPControlByte, SeverityInfo, "quoted-printable body holds an unencoded control byte", "kept the byte"},
	noteQPOverlongLine:      {CodeCTEQPOverlongLine, SeverityInfo, "quoted-printable body holds an encoded line longer than 998 bytes", "decoded the line"},
	noteUUInvalid:           {CodeCTEUUEncodeInvalid, SeverityRecovered, "uuencoded body holds a line that is not a whole uuencoded line", "decoded what the line holds and went on"},
	noteUUMissingBegin:      {CodeCTEUUEncodeMissingBegin, SeverityRecovered, "uuencoded body has no begin line", "decoded from the first uuencoded line"},
	noteUUMissingEnd:        {CodeCTEUUEncodeMissingEnd, SeverityIncomplete, "uuencoded body has no end line", "decoded up to the end of the body"},
	noteSevenBit:            {CodeCTESevenBitViolation, SeverityInfo, "body declared 7bit holds a NUL or a byte above 127", "kept every byte"},
}

// cteNotes reports the repairs of one body's transfer decoder, each kind once,
// at the first place it was seen. The zero value, and one with no session,
// reports nothing.
//
// A repair is reported once the body has shown that it went on after it: when
// a later line ending of the body is read, or the body ends with io.EOF. One
// seen on the last, unended line of a body whose input then failed is not
// reported: those bytes may be anything the failure cut short — a multipart
// delimiter, say — and the failure is what the body reports (ROB-010-006).
type cteNotes struct {
	ctx     parseContext
	seen    uint32
	pending uint32
	at      [cteNoteCount]int64
}

// note marks k, observed offset bytes into the body as it was read.
func (n *cteNotes) note(k cteNote, offset int64) {
	if n == nil || n.ctx.session == nil || n.seen&(1<<k) != 0 {
		return
	}
	n.seen |= 1 << k
	n.pending |= 1 << k
	n.at[k] = offset
}

// settle records every pending note: the body went on, or ended.
//
// A sink that stops the parse while it is told is not this body's stop: a body
// already returned may still be read after one, as it always could, so the
// session keeps the stop and the decoder goes on.
func (n *cteNotes) settle() {
	if n == nil || n.pending == 0 {
		return
	}
	// In the order of where each was seen, then of kind.
	for n.pending != 0 {
		first := cteNoteCount
		for k := cteNote(0); k < cteNoteCount; k++ {
			if n.pending&(1<<k) != 0 && (first == cteNoteCount || n.at[k] < n.at[first]) {
				first = k
			}
		}
		n.pending &^= 1 << first
		info := cteNoteInfo[first]
		n.ctx.recordBody(Diagnostic{
			Code:     info.code,
			Severity: info.severity,
			Message:  info.message,
			Field:    "Content-Transfer-Encoding",
			Action:   info.action,
			Location: n.ctx.bodyLocationAt(n.at[first]),
		})
	}
	n.pending = 0
}

// drop forgets the pending notes: the body's input failed.
func (n *cteNotes) drop() {
	if n != nil {
		n.pending = 0
	}
}

// end settles or drops the pending notes for a body that ended with err.
func (n *cteNotes) end(err error) {
	if err == io.EOF {
		n.settle()
	} else {
		n.drop()
	}
}

// cteChunk is how much of a body a tolerant decoder reads at a time.
const cteChunk = 4096

// canonicalCTE returns the transfer encoding a Content-Transfer-Encoding value
// names, in the form every switch uses, and whether reading it that way took a
// repair.
//
// Case, surrounding white space and comments are syntax and no repair. A
// repair is anything else that leaves a known mechanism: surrounding quotes,
// text after the token (";", ",", white space and what follows), an RFC 2047
// encoded word, and the spellings mail software is known to write for the
// standard mechanisms ("7-bit", "8bits", "base-64", "quoted_printable", "qp",
// "uuencode"). A value that names no known mechanism even so is returned lower
// cased and trimmed, unrepaired, and is unknown to the decoders as before.
func canonicalCTE(raw string) (name string, repaired bool) {
	orig := strings.TrimSpace(raw)
	s := strings.ToLower(orig)
	if knownCTE(s) {
		return s, false
	}
	t, commented := stripCTEComments(orig)
	t = strings.TrimSpace(t)
	if commented && knownCTE(strings.ToLower(t)) {
		return strings.ToLower(t), false
	}
	// An encoded word is decoded before its case is folded.
	if strings.HasPrefix(t, "=?") && len(t) <= 256 {
		if dec, err := new(mime.WordDecoder).Decode(t); err == nil {
			t = strings.TrimSpace(dec)
		}
	}
	t = strings.ToLower(t)
	// What follows the token: parameters, a second value, an unclosed comment.
	if i := strings.IndexAny(t, ";,("); i >= 0 {
		t = t[:i]
	}
	t = strings.Trim(t, "\"'<> \t\r\n")
	if strings.HasPrefix(t, "quoted printable") {
		// One mechanism written with a space.
		t = "quoted-printable"
	} else if i := strings.IndexAny(t, " \t\r\n"); i >= 0 {
		t = strings.Trim(t[:i], "\"'<>")
	}
	switch t {
	case "7-bit", "7bits", "7-bits", "7_bit":
		t = "7bit"
	case "8-bit", "8bits", "8-bits", "8_bit", "8bitmime":
		t = "8bit"
	case "base-64", "base_64", "b64":
		t = "base64"
	case "quoted_printable", "quotedprintable", "qp", "quoted-print":
		t = "quoted-printable"
	case "uuencode", "x-uue", "uue", "x-uuencoded", "uuencoded":
		t = "x-uuencode"
	}
	if t != "" && knownCTE(t) {
		return t, true
	}
	return s, false
}

// knownCTE reports whether s is a mechanism the decoders implement, in
// canonical form.
func knownCTE(s string) bool {
	switch s {
	case "", "7bit", "8bit", "binary", "base64", "quoted-printable", "x-uuencode":
		return true
	}
	return false
}

// stripCTEComments removes RFC 5322 comments from a short header value, and
// reports whether it held one. A value with an unclosed comment, or one too
// long to be a mechanism, is returned as it is.
func stripCTEComments(s string) (string, bool) {
	if len(s) > 1024 || !strings.Contains(s, "(") {
		return s, false
	}
	var b strings.Builder
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && depth > 0 && i+1 < len(s):
			i++
		case c == '(':
			depth++
		case c == ')' && depth > 0:
			depth--
		case depth == 0:
			b.WriteByte(c)
		}
	}
	if depth != 0 {
		return s, false
	}
	return b.String(), true
}

// base64 byte classes.
const (
	b64Pad   = 64
	b64Space = 65
	b64URL62 = 66
	b64URL63 = 67
	b64Junk  = 255
)

var b64Class = func() (t [256]uint8) {
	for i := range t {
		t[i] = b64Junk
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	for i := 0; i < len(alphabet); i++ {
		t[alphabet[i]] = uint8(i)
	}
	t['='] = b64Pad
	t[' '], t['\t'], t['\r'], t['\n'] = b64Space, b64Space, b64Space, b64Space
	t['-'], t['_'] = b64URL62, b64URL63
	return t
}()

// A tolerantBase64 decodes a Base64 body that may be damaged (ROB-070-004 to
// ROB-070-007): line endings and white space anywhere are framing; a byte
// outside the alphabet is skipped; '-' and '_' are read as the URL-safe
// alphabet's 62 and 63; padding ends a group wherever it stands, and data
// after it is decoded as a further segment, as concatenated encodings need;
// misplaced padding is ignored; and a body that ends inside a group yields the
// whole bytes that group holds — one for two characters, two for three — and
// drops a single left-over character, which holds no whole byte.
type tolerantBase64 struct {
	r     io.Reader
	notes *cteNotes

	in  []byte
	out []byte // decoded output of the last chunk
	pos int    // how much of out was delivered
	err error  // what the input ended with, delivered after out

	consumed int64 // body bytes read
	quad     [4]uint8
	nq       int
	// afterPad is 0 inside data, 1 after the one '=' of a two-character group
	// (one more may follow), 2 after a complete padding.
	afterPad uint8

	// stopAtPadding makes padding end the data, and stopped reports that it
	// did: what follows is read and not decoded.
	stopAtPadding bool
	stopped       bool
}

func (d *tolerantBase64) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if d.pos < len(d.out) {
			n := copy(p, d.out[d.pos:])
			d.pos += n
			return n, nil
		}
		if d.err != nil {
			return 0, d.err
		}
		if d.in == nil {
			d.in = make([]byte, pfChunkFirst)
			d.out = make([]byte, 0, pfChunkFirst)
		}
		d.in = pfChunk(d.in, d.consumed)
		n, err := d.r.Read(d.in)
		n = max(0, min(n, len(d.in)))
		d.out, d.pos = d.out[:0], 0
		d.decode(d.in[:n])
		if err != nil {
			d.finish(err == io.EOF)
			d.notes.end(err)
			d.err = err
			continue
		}
		if n == 0 {
			return 0, nil
		}
	}
}

func (d *tolerantBase64) decode(b []byte) {
	for i, c := range b {
		if d.stopped {
			break
		}
		v := b64Class[c]
		switch {
		case v < 64:
		case v == b64Space:
			if c == '\n' {
				d.notes.settle()
			}
			continue
		case v == b64Pad:
			d.pad(d.consumed + int64(i))
			continue
		case v == b64URL62:
			d.notes.note(noteB64URLAlphabet, d.consumed+int64(i))
			v = 62
		case v == b64URL63:
			d.notes.note(noteB64URLAlphabet, d.consumed+int64(i))
			v = 63
		default:
			d.notes.note(noteB64Junk, d.consumed+int64(i))
			continue
		}
		if d.afterPad != 0 {
			if d.afterPad == 1 {
				d.notes.note(noteB64PaddingInvalid, d.consumed+int64(i))
			}
			d.notes.note(noteB64DataAfterPadding, d.consumed+int64(i))
			if d.stopAtPadding {
				// CompatibilityPythonEmail: padding ends the data, and what
				// follows it is not decoded.
				d.stopped = true
				continue
			}
			d.afterPad = 0
		}
		d.quad[d.nq] = v
		d.nq++
		if d.nq == 4 {
			q := d.quad
			d.out = append(d.out, q[0]<<2|q[1]>>4, q[1]<<4|q[2]>>2, q[2]<<6|q[3])
			d.nq = 0
		}
	}
	d.consumed += int64(len(b))
}

// pad reads one '='.
func (d *tolerantBase64) pad(offset int64) {
	switch d.nq {
	case 2:
		d.flush()
		d.afterPad = 1
	case 3:
		d.flush()
		d.afterPad = 2
	case 1:
		// One character and padding: no whole byte.
		d.notes.note(noteB64LengthInvalid, offset)
		d.nq = 0
		d.afterPad = 2
	default:
		if d.afterPad == 1 {
			d.afterPad = 2
			return
		}
		d.notes.note(noteB64PaddingInvalid, offset)
	}
}

// flush emits the whole bytes of an incomplete group of two or three
// characters.
func (d *tolerantBase64) flush() {
	q := d.quad
	switch d.nq {
	case 2:
		d.out = append(d.out, q[0]<<2|q[1]>>4)
	case 3:
		d.out = append(d.out, q[0]<<2|q[1]>>4, q[1]<<4|q[2]>>2)
	}
	d.nq = 0
}

// finish ends the body. At the end of the input, eof, the last group yields
// the whole bytes it holds and what it lacks is reported. A failure of the
// input yields and reports nothing more: what it cut is not known.
func (d *tolerantBase64) finish(eof bool) {
	if !eof {
		d.nq = 0
		return
	}
	switch {
	case d.nq == 1:
		d.notes.note(noteB64LengthInvalid, d.consumed)
		d.nq = 0
	case d.nq > 1:
		d.notes.note(noteB64PaddingMissing, d.consumed)
		d.flush()
	case d.afterPad == 1:
		d.notes.note(noteB64PaddingInvalid, d.consumed)
	}
}

// quoted-printable decoder states.
const (
	qpText  = iota
	qpEq    // after '='
	qpEqHex // after '=' and one hexadecimal digit
	qpEqWS  // after '=' and white space
	qpEqCR  // after '=', optional white space and a CR
)

// qpRunMax bounds the white space a tolerantQP holds back to learn whether a
// line ending follows it. A longer run is content up to that bound.
const qpRunMax = 1024

// A tolerantQP decodes a quoted-printable body that may be damaged
// (ROB-070-008). Conformant input decodes as the standard library decodes it:
// "=XX" in either case of hexadecimal digit, soft line breaks, white space
// before a line ending dropped as transport padding, and every other byte —
// 8-bit ones included — passed on. In addition:
//
//   - '=' followed by anything but two hexadecimal digits or a line ending is
//     literal text, with what follows it;
//   - a soft line break may have white space between '=' and its line ending,
//     and may be ended by a bare LF or a bare CR as well as CRLF;
//   - '=' at the end of the body is literal text;
//   - control bytes are passed on, and no line is too long.
//
// It holds no line: its state is a few bytes and one bounded run of white
// space.
type tolerantQP struct {
	r     io.Reader
	notes *cteNotes

	in  []byte
	out []byte
	pos int
	err error

	consumed int64
	state    uint8
	hex1     byte
	// run is the white space and CRs held back in text until what follows says
	// whether they end a line; eqWS the white space after an '='.
	run  []byte
	eqWS []byte
	col  int64
}

func (d *tolerantQP) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if d.pos < len(d.out) {
			n := copy(p, d.out[d.pos:])
			d.pos += n
			return n, nil
		}
		if d.err != nil {
			return 0, d.err
		}
		if d.in == nil {
			d.in = make([]byte, pfChunkFirst)
			d.out = make([]byte, 0, pfChunkFirst)
		}
		d.in = pfChunk(d.in, d.consumed)
		n, err := d.r.Read(d.in)
		n = max(0, min(n, len(d.in)))
		d.out, d.pos = d.out[:0], 0
		for i := 0; i < n; i++ {
			d.step(d.in[i], d.consumed+int64(i))
		}
		d.consumed += int64(n)
		if err != nil {
			d.finish(err == io.EOF)
			d.notes.end(err)
			d.err = err
			continue
		}
		if n == 0 {
			return 0, nil
		}
	}
}

// step reads one byte of the body, found at offset.
func (d *tolerantQP) step(c byte, offset int64) {
	if c == '\n' {
		// Only a line ending proves a line, and so an overlong one.
		if d.col > 999 {
			d.notes.note(noteQPOverlongLine, offset)
		}
		d.col = 0
		d.notes.settle()
	} else {
		d.col++
	}
	switch d.state {
	case qpEq:
		switch {
		case isHexByte(c):
			d.hex1 = c
			d.state = qpEqHex
		case c == ' ' || c == '\t':
			d.eqWS = append(d.eqWS[:0], c)
			d.state = qpEqWS
		case c == '\r':
			d.state = qpEqCR
		case c == '\n':
			d.state = qpText
		default:
			d.notes.note(noteQPInvalidHex, offset)
			d.out = append(d.out, '=')
			d.state = qpText
			d.text(c, offset)
		}
	case qpEqHex:
		if isHexByte(c) {
			d.out = append(d.out, qpUnhex(d.hex1)<<4|qpUnhex(c))
			d.state = qpText
			return
		}
		d.notes.note(noteQPInvalidHex, offset)
		d.out = append(d.out, '=', d.hex1)
		d.state = qpText
		d.text(c, offset)
	case qpEqWS:
		switch {
		case (c == ' ' || c == '\t') && len(d.eqWS) < qpRunMax:
			d.eqWS = append(d.eqWS, c)
		case c == '\r':
			d.notes.note(noteQPInvalidSoftBreak, offset)
			d.state = qpEqCR
		case c == '\n':
			d.notes.note(noteQPInvalidSoftBreak, offset)
			d.state = qpText
		default:
			// Not a soft line break after all: literal text.
			d.notes.note(noteQPInvalidHex, offset)
			d.out = append(d.out, '=')
			d.state = qpText
			for _, w := range d.eqWS {
				d.text(w, offset)
			}
			d.eqWS = d.eqWS[:0]
			d.text(c, offset)
		}
	case qpEqCR:
		d.state = qpText
		if c == '\n' {
			return
		}
		// A soft line break ended by a bare CR.
		d.notes.note(noteQPInvalidSoftBreak, offset)
		d.text(c, offset)
	default:
		d.text(c, offset)
	}
}

// text reads one byte outside an escape.
func (d *tolerantQP) text(c byte, offset int64) {
	switch {
	case c == ' ' || c == '\t' || c == '\r':
		if len(d.run) >= qpRunMax {
			d.out = append(d.out, d.run...)
			d.run = d.run[:0]
		}
		d.run = append(d.run, c)
	case c == '\n':
		// White space before a line ending is transport padding.
		if n := len(d.run); n > 0 && d.run[n-1] == '\r' {
			d.out = append(d.out, '\r')
		}
		d.run = d.run[:0]
		d.out = append(d.out, '\n')
	case c == '=':
		d.out = append(d.out, d.run...)
		d.run = d.run[:0]
		d.state = qpEq
	default:
		if c < ' ' || c == 0x7f {
			d.notes.note(noteQPControlByte, offset)
		}
		d.out = append(d.out, d.run...)
		d.run = d.run[:0]
		d.out = append(d.out, c)
	}
}

// finish ends the body in whatever state its last byte left: an escape the
// body ends inside is literal text, the '=' it ends with included, as it
// always was, whether the input ended or failed there. Only the end of the
// input, eof, reports it: what a failure cut is not known.
func (d *tolerantQP) finish(eof bool) {
	switch d.state {
	case qpEq:
		if eof {
			d.notes.note(noteQPTrailingEquals, d.consumed)
		}
		d.out = append(d.out, '=')
	case qpEqHex:
		if eof {
			d.notes.note(noteQPInvalidHex, d.consumed)
		}
		d.out = append(d.out, '=', d.hex1)
	}
	// White space the body ends with is transport padding, as before a line
	// ending; so is a soft line break the body ends inside.
	d.state = qpText
	d.run, d.eqWS = d.run[:0], d.eqWS[:0]
}

func qpUnhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

// uuLineMax bounds one line of a uuencoded body. A whole uuencoded line is at
// most 62 bytes; a longer one is cut there and reported.
const uuLineMax = 256

// A tolerantUU decodes a uuencoded body (ROB-070-009): the lines between a
// "begin" line and an "end" line. Lines before "begin" are skipped; with no
// "begin" line at all, decoding starts at the first line that is a whole
// uuencoded line; with no "end" line it runs to the end of the body. A line
// shorter than its length character says — trailing spaces are often stripped
// in transit — is decoded as if padded with them. Everything after "end" is
// ignored.
type tolerantUU struct {
	r     io.Reader
	notes *cteNotes

	in  []byte
	out []byte
	pos int
	err error

	consumed  int64
	line      []byte
	lineStart int64
	overlong  bool
	begun     bool
	ended     bool
}

func (d *tolerantUU) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if d.pos < len(d.out) {
			n := copy(p, d.out[d.pos:])
			d.pos += n
			return n, nil
		}
		if d.err != nil {
			return 0, d.err
		}
		if d.in == nil {
			d.in = make([]byte, pfChunkFirst)
			d.out = make([]byte, 0, pfChunkFirst)
			d.line = make([]byte, 0, uuLineMax)
		}
		d.in = pfChunk(d.in, d.consumed)
		n, err := d.r.Read(d.in)
		n = max(0, min(n, len(d.in)))
		d.out, d.pos = d.out[:0], 0
		for i := 0; i < n; i++ {
			c := d.in[i]
			if c == '\n' {
				d.endLine()
				d.notes.settle()
				d.lineStart = d.consumed + int64(i) + 1
				continue
			}
			if len(d.line) < uuLineMax {
				d.line = append(d.line, c)
			} else {
				d.overlong = true
			}
		}
		d.consumed += int64(n)
		if err != nil {
			if err == io.EOF {
				if len(d.line) > 0 {
					d.endLine()
				}
				if d.begun && !d.ended {
					d.notes.note(noteUUMissingEnd, d.consumed)
				}
			}
			d.notes.end(err)
			d.err = err
			continue
		}
		if n == 0 {
			return 0, nil
		}
	}
}

// uuWhole reports whether line is a whole uuencoded data line: a length
// character and exactly the characters that length needs, all in range.
func uuWhole(line []byte) bool {
	if len(line) == 0 {
		return false
	}
	n := int(line[0]-' ') & 63
	if line[0] < ' ' || line[0] > '`' || n == 0 || n > 45 {
		return false
	}
	if len(line)-1 != (n+2)/3*4 {
		return false
	}
	for _, c := range line[1:] {
		if c < ' ' || c > '`' {
			return false
		}
	}
	return true
}

// endLine reads the line gathered so far.
func (d *tolerantUU) endLine() {
	line := d.line
	d.line = d.line[:0]
	overlong := d.overlong
	d.overlong = false
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	if d.ended {
		return
	}
	if !d.begun {
		switch {
		case len(line) >= 6 && string(line[:6]) == "begin ":
			d.begun = true
			return
		case uuWhole(line) && !overlong:
			d.notes.note(noteUUMissingBegin, d.lineStart)
			d.begun = true
		default:
			return
		}
	}
	if string(line) == "end" {
		d.ended = true
		return
	}
	if len(line) == 0 || line[0] == '`' {
		return
	}
	if overlong || line[0] < ' ' || line[0] > '`' {
		// No length character to go by, or more than a line can hold.
		d.notes.note(noteUUInvalid, d.lineStart)
		if line[0] < ' ' || line[0] > '`' {
			return
		}
	}
	n := int(line[0]-' ') & 63
	data := line[1:]
	// A line may be shorter than its length character says, because trailing
	// spaces were stripped in transit; it is then decoded as if padded with
	// them. A line that holds less than half of what its length needs is not
	// that: only the bytes its characters hold are decoded, so that a length
	// character alone cannot be made to stand for 45 bytes.
	if need := (n + 2) / 3 * 4; len(data)*2 < need {
		d.notes.note(noteUUInvalid, d.lineStart)
		n = min(n, len(data)*3/4)
	}
	at := func(i int) byte {
		if i < len(data) {
			if c := data[i]; c < ' ' || c > '`' {
				d.notes.note(noteUUInvalid, d.lineStart)
			}
			return (data[i] - ' ') & 63
		}
		return 0
	}
	for i := 0; n > 0; i += 4 {
		a, b, c, e := at(i), at(i+1), at(i+2), at(i+3)
		d.out = append(d.out, a<<2|b>>4)
		if n > 1 {
			d.out = append(d.out, b<<4|c>>2)
		}
		if n > 2 {
			d.out = append(d.out, c<<6|e)
		}
		n -= 3
	}
}

// A sevenBitMonitor passes a body declared 7bit through unchanged and reports,
// once, the first NUL or byte above 127 in it (ROB-070-003). It holds nothing
// and stops looking after the first.
type sevenBitMonitor struct {
	r        io.Reader
	notes    *cteNotes
	consumed int64
	found    bool
}

func (m *sevenBitMonitor) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	if n > 0 && !m.found {
		for i, c := range p[:min(n, len(p))] {
			if c >= 0x80 || c == 0 {
				// What the byte is does not depend on how the body ends, so
				// it is reported at once.
				m.found = true
				m.notes.note(noteSevenBit, m.consumed+int64(i))
				m.notes.settle()
				break
			}
		}
		m.consumed += int64(n)
	}
	return n, err
}

// transferDecoder returns the decoder of a body whose transfer encoding is
// name, a canonical name (see canonicalCTE), reading r; ok is false for a
// mechanism with no decoder. Under ProfileStrict Base64 and quoted-printable
// are decoded as they always were, and 7bit is not looked into.
func (c parseContext) transferDecoder(name string, r io.Reader) (dec io.Reader, ok bool) {
	strict := c.session.Profile() == ProfileStrict
	if strict {
		switch name {
		case "base64", "quoted-printable", "7bit", "8bit", "binary", "":
			dec, err := legacyEncodingReader(name, r)
			return dec, err == nil
		}
	}
	notes := &cteNotes{ctx: c}
	switch name {
	case "quoted-printable":
		return &tolerantQP{r: r, notes: notes}, true
	case "base64":
		return &tolerantBase64{r: r, notes: notes, stopAtPadding: c.session.Compatibility() == CompatibilityPythonEmail}, true
	case "x-uuencode":
		return &tolerantUU{r: r, notes: notes}, true
	case "7bit":
		if c.session == nil {
			return r, true
		}
		return &sevenBitMonitor{r: r, notes: notes}, true
	case "8bit", "binary", "":
		return r, true
	}
	return nil, false
}
