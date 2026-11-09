package message

import (
	"bufio"
	"io"
)

// mbox envelope lines (ROB-010-004).
//
// A message saved from an mbox file often still begins with the line that
// separated it from the message before it in that file: "From ", the envelope
// sender, and the time the message was received — the "From_" line. That line
// is not part of the message. RFC 5322 has no such field, so read as a header
// line it is a malformed one, and the parse fails on its first line.
//
// ParseOptions.RecognizeMboxEnvelope asks Parse and ParseContext to recognize
// one such line at the very first byte of the input and keep it apart from the
// message. It is off by default, and with it off nothing here runs: not one
// byte more is looked at than before, and every input is framed exactly as it
// was. Read, ReadWithOptions, New, textproto, the mail package's constructors,
// the header of every multipart part and the body of a message/rfc822 part
// never recognize one.
//
// This is the parsing of one message, not of an mbox archive. Only one line is
// recognized, at offset 0 and nowhere else; the message still ends where the
// input ends. Nothing splits the input at a later "From " line, interprets a
// Content-Length field, or undoes the ">From " escaping some mbox writers apply
// to body lines: a "From " or ">From " line in the header block after the
// envelope, or in any body, is read exactly as it was before — a malformed
// header line, or content.
//
// # What is recognized
//
// RFC 4155 describes a default format — "From", one space, an addr-spec, one
// space, a ctime timestamp in UTC, an end of line — and says that the separator
// lines found in practice vary in their sender, their timestamp and their line
// endings. No grammar recognizes them all, and this one does not claim to. It
// recognizes the ctime-shaped timestamp the common writers produce, and nothing
// that only resembles a line of text, such as "From the 10:30 meeting":
//
//	envelope = "From" SP sender 1*WSP stamp [1*WSP "remote" 1*WSP "from" 1*WSP host] EOL
//	sender   = 1*VCHAR, not beginning with ":"
//	stamp    = weekday 1*WSP month 1*WSP day 1*WSP time 1*WSP zoned-year
//	zoned-year = year / zone 1*WSP year / year 1*WSP zone
//	weekday  = "Mon" / "Tue" / "Wed" / "Thu" / "Fri" / "Sat" / "Sun"
//	month    = "Jan" / "Feb" / ... / "Dec"
//	day      = 1*2DIGIT, 1 to 31
//	time     = 2DIGIT ":" 2DIGIT ":" 2DIGIT, 00-23, 00-59, 00-60
//	year     = 4DIGIT
//	zone     = 2*5UPPER-ALPHA / ("+" / "-") 2DIGIT 2DIGIT, 00-23 and 00-59
//	host     = 1*VCHAR
//	EOL      = CRLF / LF / CR
//	WSP      = SP / HTAB
//
// The names are case-sensitive. The weekday is not checked against the date,
// nor the day against the month: this recognizes the shape of a timestamp and
// gives it no meaning. Every byte of the line before its line ending is a
// visible ASCII character, a space or a tab; there is no white space before
// the sender and none after the last token. A line ending is required: a line
// the input ends with io.EOF before one is not an envelope. A '\r' is a line
// ending, with the '\n' after it when there is one, so a '\r' is decided by the
// byte after it, or by io.EOF; a '\r' that any other failure of the input came
// after is undecided, and the line is not an envelope.
//
// The line before its line ending is recognized only if it has at most 998
// bytes, or Limits.MaxLineBytes if that is smaller, so at most 1000 bytes with
// its line ending. That is an explicit local bound of this recognizer on what
// it looks at and keeps, not a length RFC 4155 or any mbox format sets; 998 is
// the most RFC 5322 lets a line hold. A longer line is not an envelope.
//
// The recognizer is a bounded scan, not an incremental grammar parser. It
// stops early at a byte that cannot begin "From " or cannot be in the line at
// all — anything but visible ASCII, a space or a tab before the line ending —
// and once the line is longer than the bound; the sender and the timestamp are
// checked against the grammar only once the line ending has been found. It
// looks at no more than max(5, bound+2) bytes of the input: "From ", then the
// line up to one byte past the bound, then the one byte after a '\r'. The
// buffered reader below it may already hold more of the source than that, up
// to its 4096-byte buffer.
//
// Not recognized, among others: a sender beginning with ':' or with white
// space ("From : x" is a From field, as it always was); a lower-case "from" or
// an upper-case "FROM"; ">From "; a timestamp in any other order, with a
// two-digit year, with fractional seconds, with a zone written otherwise, or
// with two zones; anything else after the year; trailing white space; a byte
// outside visible ASCII, space and tab, including 8-bit text in the sender;
// and a UTF-8 or other byte order mark before "From" — the mark is the header
// block's, as ROB-010-003 frames it, and the line after it is the header's
// first line, as before.
//
// A line that is not recognized is not consumed: every byte of it is read by
// the header reader on the same terms as with the option off, which in practice
// means the parse fails on it as it always did. Nothing records that a line
// was looked at and not recognized.
//
// # What a recognized line becomes
//
// Its bytes, line ending included, are kept by the session, which
// ParseSession.MboxEnvelope returns, and they are no bytes of the message: the
// message's header block begins after them. That block is read exactly as the
// header of an input that began there would be — a byte order mark at its
// start (ROB-010-003) and the line endings of its lines (ROB-010-002) included
// — so a header, its diagnostics and its refusals are the same as for that
// input, at positions moved by the envelope's length. Positions stay absolute:
// the root's header offset is the envelope's length, and its body offset, and
// every part's offsets, are positions in the input as before. Only
// textproto.HeaderLimitError, which a header budget's refusal carries as its
// cause, keeps the offsets it always had, relative to where its reader began
// the block; the diagnostic's Location is the absolute one.
//
// The line is no header line, so none of the header budgets — MaxHeaderBytes,
// MaxHeaderFieldBytes, MaxHeaderCount and the shared MaxTotalHeaderBytes and
// MaxTotalHeaderCount — counts it, and its line ending is in no
// textproto.LineEndings and draws no line-ending note. It is part of the input,
// so MaxInputBytes counts it; and Limits.MaxLineBytes lowers the bound above,
// so a line longer than that is left to the header reader, which refuses it as
// before. What is kept is one line of at most 1000 bytes, line ending
// included, once per parse.
//
// It is recorded at most once, as header.mbox_envelope, SeverityInfo, at the
// line's bytes, with the root's nil path, before every diagnostic of the
// header. It is the same under every Profile, ProfileStrict included, and every
// Compatibility: the caller asked for the line to be recognized, and it never
// enters the message. Its Message carries none of the line's bytes. It is not
// recorded when a stop of the parse was published first (see below), it may be
// replaced or omitted under Limits.MaxDiagnosticCount, and it may be left
// unretained under DiscardDeliveredDiagnostics; the line is kept in every one
// of those cases.
//
// The sender and the timestamp are bytes the input supplied. The recognizer
// checks their shape against the grammar above and nothing more: they are not
// parsed as an address or a time, not compared with the message's
// Return-Path, From, Sender or Date fields, and not offered as a sender or a
// date. They are unauthenticated and authenticate nothing.
//
// No writer writes the line: it is not in the Header, so Entity.WriteTo,
// textproto.WriteHeader and the Writer write the message alone. A caller who
// wants it back in front of the message writes MboxEnvelope.Bytes itself, and
// whatever escaping an mbox it writes needs is its own; this is not raw replay
// of the input (ROB-020-007, ROB-110).
//
// # Input errors, budgets and stops
//
// The line is decided from the bytes the input delivered, looked at one byte
// further only while they do not decide it, so whether a line is recognized,
// and which bytes it is, does not depend on how the input chunks its reads;
// nor, therefore, does the framing of the root header after it. Nothing is
// claimed here about decoded bodies, which this item does not touch. Under Parse the input's first error is final
// (ROB-010-006): an error before the line is decided leaves no envelope, and
// the header reader then meets the identical error where it always did. A stop
// of the parse — MaxInputBytes, the no-progress rule, a cancellation — that
// keeps a byte the decision needs from arriving leaves no envelope either. A
// stop published while the bytes already delivered decided a whole envelope —
// by a read that delivered them and more, or a cancellation seen after it —
// keeps the line, records no diagnostic, and is what Parse returns, with no
// entity. A DiagnosticSink that stops the parse on header.mbox_envelope ends
// it there, with the line kept and no entity. The sink may read
// ParseSession.MboxEnvelope while it is called.

// maxMboxEnvelopeContent is the most bytes, before its line ending, a line
// recognized as an envelope may have.
const maxMboxEnvelopeContent = 998

// mboxEnvelopeAction is what the parse did with a recognized envelope line.
const mboxEnvelopeAction = "kept the line apart from the message, as ParseSession.MboxEnvelope returns it; the message's header block begins after it, and no writer writes it"

// An MboxEnvelope is the mbox "From_" envelope line a parse recognized at the
// start of its input, kept apart from the message; see
// ParseOptions.RecognizeMboxEnvelope. The zero value is no envelope.
//
// Its sender and timestamp are what the input said: their shape matched the
// recognizer's grammar, but they are unauthenticated, and nothing interprets
// them as an address or a date.
type MboxEnvelope struct {
	// raw is the line, line ending included. It is never modified.
	raw []byte
}

// Bytes returns the line as it was read, its line ending included, as a new
// slice on every call, or nil for the zero MboxEnvelope.
func (e MboxEnvelope) Bytes() []byte {
	if e.raw == nil {
		return nil
	}
	return append([]byte(nil), e.raw...)
}

// Location returns where the line is in the input: from offset 0 to the end of
// its line ending. It returns the zero Location for the zero MboxEnvelope.
func (e MboxEnvelope) Location() Location {
	if e.raw == nil {
		return Location{}
	}
	return LocationRange(0, int64(len(e.raw)))
}

// MboxEnvelope returns the mbox envelope line this parse recognized at the
// start of its input, and whether there is one. There is one only for a parse
// asked to recognize it by ParseOptions.RecognizeMboxEnvelope, whose input
// began with one, and it stays once the parse recognized it, whatever happened
// to the header after it — including a header that could not be read, when
// Parse returned no entity. It returns false for a nil session.
//
// It is safe for concurrent use, a DiagnosticSink may call it, and every call
// returns a value of its own.
func (s *ParseSession) MboxEnvelope() (MboxEnvelope, bool) {
	if s == nil {
		return MboxEnvelope{}, false
	}
	s.mu.Lock()
	raw := s.mbox
	s.mu.Unlock()
	if raw == nil {
		return MboxEnvelope{}, false
	}
	return MboxEnvelope{raw: raw}, true
}

// readMboxEnvelope recognizes the envelope line br begins with, for a parse
// asked to, and consumes and keeps it. It returns where the message's header
// block begins — the line's length, or 0 — and the stop the parse must end
// with before reading it, if any.
//
// A line that is not recognized consumes nothing and returns no error, so the
// parse goes on exactly as with the option off.
func (s *ParseSession) readMboxEnvelope(br *bufio.Reader) (int64, error) {
	if s == nil || !s.cfg.mbox {
		return 0, nil
	}
	limit := maxMboxEnvelopeContent
	if l := s.Limits().MaxLineBytes; l > 0 && l < int64(limit) {
		limit = int(l)
	}
	n := mboxEnvelopeLen(br, limit)
	if n == 0 {
		return 0, nil
	}
	held, _ := br.Peek(n)
	raw := append([]byte(nil), held...)
	br.Discard(n)

	s.mu.Lock()
	s.mbox = raw
	s.mu.Unlock()

	// A stop published while the line was read — by a read that delivered it
	// and more, or a cancellation seen after it — is what the parse ends with.
	// The line is kept, but nothing is recorded after the stop.
	if stop := s.fatalErr(); stop != nil {
		return int64(n), stop
	}
	d := Diagnostic{
		Code:     CodeHeaderMboxEnvelope,
		Severity: SeverityInfo,
		Message:  "the input begins with an mbox From_ envelope line",
		Action:   mboxEnvelopeAction,
		Location: LocationRange(0, int64(n)),
	}
	if stop := s.record(d); stop != nil {
		return int64(n), stop
	}
	return int64(n), nil
}

// mboxEnvelopeLen returns the length, line ending included, of the envelope
// line r begins with, or 0 when it begins with none; see the grammar at the
// top of this file. The line before its line ending may have at most limit
// bytes. It consumes nothing.
//
// It looks one byte further than r holds only while what it holds does not
// decide the line. It stops early at a byte that cannot begin "From " or cannot
// be in the line, and once the line is longer than limit; the grammar of the
// sender and the timestamp is checked only after the line ending has been
// found. It never looks past the byte after a '\r', so it looks at no more than
// max(5, limit+2) bytes, which r's buffer, 4096 bytes in a parse, holds; r
// itself may have read more of its source into that buffer.
func mboxEnvelopeLen(r *bufio.Reader, limit int) int {
	var held []byte
	var fail error
	// at returns byte i of the input, or false when the input ended or failed
	// before it; fail is then the error that said so.
	at := func(i int) (byte, bool) {
		if i < len(held) {
			return held[i], true
		}
		if fail != nil {
			return 0, false
		}
		b, err := r.Peek(i + 1)
		held = b
		if len(b) > i {
			return b[i], true
		}
		fail = err
		return 0, false
	}

	const prefix = "From "
	for i := 0; i < len(prefix); i++ {
		if c, ok := at(i); !ok || c != prefix[i] {
			return 0
		}
	}
	n := len(prefix)
	if n > limit {
		return 0
	}
	var end byte
	for {
		c, ok := at(n)
		if !ok {
			// The input ended, or failed, before any line ending.
			return 0
		}
		if c == '\n' || c == '\r' {
			end = c
			break
		}
		if c != ' ' && c != '\t' && (c < 0x21 || c > 0x7e) {
			return 0
		}
		n++
		if n > limit {
			return 0
		}
	}
	if !mboxEnvelopeText(string(held[len(prefix):n])) {
		return 0
	}
	if end == '\n' {
		return n + 1
	}
	// A '\r': the byte after it says whether it begins a CRLF, and io.EOF that
	// it ends the input. Any other failure leaves it undecided.
	if c, ok := at(n + 1); ok {
		if c == '\n' {
			return n + 2
		}
		return n + 1
	}
	if fail == io.EOF {
		return n + 1
	}
	return 0
}

// mboxEnvelopeText reports whether s, the line after "From " and before its
// line ending, holding only visible ASCII, spaces and tabs, is a sender and a
// timestamp as the grammar at the top of this file describes.
func mboxEnvelopeText(s string) bool {
	if s == "" || mboxWSP(s[0]) || mboxWSP(s[len(s)-1]) || s[0] == ':' {
		return false
	}
	t := mboxFields(s)
	// sender, weekday, month, day, time, and at least a year.
	if len(t) < 6 {
		return false
	}
	if !mboxOneOf(t[1], "Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun") ||
		!mboxOneOf(t[2], "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec") ||
		!mboxDay(t[3]) || !mboxTime(t[4]) {
		return false
	}
	i := 5
	switch {
	case mboxYear(t[i]):
		i++
		if i < len(t) && mboxZone(t[i]) {
			i++
		}
	case mboxZone(t[i]) && i+1 < len(t) && mboxYear(t[i+1]):
		i += 2
	default:
		return false
	}
	rest := t[i:]
	switch len(rest) {
	case 0:
		return true
	case 3:
		// The host is a token, so it is one or more visible characters.
		return rest[0] == "remote" && rest[1] == "from"
	}
	return false
}

func mboxWSP(c byte) bool { return c == ' ' || c == '\t' }

// mboxFields splits s, which neither begins nor ends with white space, at each
// run of spaces and tabs.
func mboxFields(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if !mboxWSP(s[i]) {
			continue
		}
		if start < i {
			out = append(out, s[start:i])
		}
		start = i + 1
	}
	return append(out, s[start:])
}

func mboxOneOf(s string, names ...string) bool {
	for _, n := range names {
		if s == n {
			return true
		}
	}
	return false
}

// mboxDigits returns the value of s when it is all decimal digits.
func mboxDigits(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	v := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		v = v*10 + int(s[i]-'0')
	}
	return v, true
}

// mboxDay reports whether s is a day of a month: one or two digits, 1 to 31.
func mboxDay(s string) bool {
	v, ok := mboxDigits(s)
	return ok && len(s) <= 2 && v >= 1 && v <= 31
}

// mboxTime reports whether s is HH:MM:SS, 00-23, 00-59 and 00-60.
func mboxTime(s string) bool {
	if len(s) != 8 || s[2] != ':' || s[5] != ':' {
		return false
	}
	h, ok1 := mboxDigits(s[0:2])
	m, ok2 := mboxDigits(s[3:5])
	sec, ok3 := mboxDigits(s[6:8])
	return ok1 && ok2 && ok3 && h <= 23 && m <= 59 && sec <= 60
}

// mboxYear reports whether s is four digits.
func mboxYear(s string) bool {
	_, ok := mboxDigits(s)
	return ok && len(s) == 4
}

// mboxZone reports whether s is a time zone: two to five upper-case letters,
// or a sign and four digits, hours 00-23 and minutes 00-59.
func mboxZone(s string) bool {
	if len(s) == 5 && (s[0] == '+' || s[0] == '-') {
		h, ok1 := mboxDigits(s[1:3])
		m, ok2 := mboxDigits(s[3:5])
		return ok1 && ok2 && h <= 23 && m <= 59
	}
	if len(s) < 2 || len(s) > 5 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}
