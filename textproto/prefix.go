package textproto

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math"
)

// Malformed prefixes (ROB-020-001).
//
// A field begins with its name, so a header block whose first line begins with
// white space — SP or HTAB — does not begin with a field. Such a line looks
// like the continuation of a field, but there is no field before it to
// continue. Every entry point used to refuse the whole block for it, which lost
// every field after it, and in a multipart the part and, for a caller that
// stopped at the first error, every part after it.
//
// The block is now read on. Its first logical candidate — the first line, every
// continuation line after it (each beginning with white space), and whatever a
// '\r' that turns out to be content joins to them, exactly as the lines of a
// field are gathered — is its malformed prefix. The prefix is kept apart from
// the fields, byte for byte, and interpreted as nothing: it is not a field, so
// Get, Has, Values, Fields, Len and Map never see it, whatever it looks like,
// and nothing of a line that would be a field without its leading white space
// (" Subject: x", " Content-Type: multipart/mixed; ...") is read as one. The
// block goes on after it: the line after the prefix is read as the block's
// next line always is — the empty line that ends the block, or a field
// candidate, held to every rule it always was held to.
//
// Lines of white space alone are lines of the prefix like any other: no line
// but an empty one ends a block (a whitespace-only separator is ROB-020-004's).
// A line that does not begin with white space begins no line of the prefix. It
// is taken into it only as a field's line would be taken into that field: as
// the rest of the prefix's last physical line, when the '\r' ending that line
// proves to be content because the line after it does not begin
// 1*name-byte *WSP ":" (ROB-010-002). The empty line ending the block, a
// field, and — in a part's header — a delimiter line of its multipart (below)
// are never taken into it.
//
// The prefix is read on the terms of any field. Its line endings are those of
// ReadHeader, are reported in LineEndings, and a last line the input ended with
// io.EOF is kept and reported unterminated. It is one logical candidate for the
// budgets: the shared field count is asked before its first byte is read, as
// for any candidate, it is charged to the field counts once it has been read
// whole, and its lines and bytes are charged to MaxLineBytes, MaxFieldBytes,
// MaxBytes and the shared bytes as they are read, each byte once. A block a
// budget refuses keeps no prefix, as it keeps no field. A read failure other
// than io.EOF keeps the lines read whole before it and drops the line it cut —
// and a line before it that a '\r' still left undecided — exactly as for a
// field, and is returned as itself.
//
// The prefix's bytes are captured as they are read, after the charge that
// admitted them, so they are the input's own bytes — line endings, NUL, 8-bit
// and control bytes as they were — and never more than the budgets admitted.
//
// # In a part's header
//
// A part's header block is read straight out of its multipart's input, so a
// block whose separating empty line is missing would read the next delimiter
// line as one of its own lines. After a malformed prefix that would be a
// regression: the block used to be refused at its first line, and a caller
// that asked for the next part found that delimiter and the part after it. So
// in a part's header that began with a malformed prefix, every later line is
// first looked at as a delimiter line of that multipart: "--", the boundary,
// "--" for the closing one, spaces and tabs, then a line ending — "\r\n", '\n',
// or a '\r' the byte after it, or io.EOF, has decided — or the end of the
// input. Only a proved delimiter ends the block, where the line begins: it is
// not charged to the header, the part's body is empty, and the multipart
// reader frames it as the end of this part, as it frames every delimiter; Part
// HeaderSeparatorMissing reports it. Anything else — a byte after the white
// space that is no line ending (`--B : value` is a field), or a failure before
// the line is decided — is read as the line it always was, from its first byte,
// held to every budget of the block.
//
// Most such lines are decided by looking, through the scanner's sticky peek,
// at no more than one buffer. A line whose white space runs past the buffer is
// decided by holding it: its bytes are taken out of the buffered reader, up to
// the framing line budget, MaxLineBytes — the bound the multipart reader puts
// on a delimiter line — or, with no line budget, one buffer, the bound it then
// puts on an undecided framing line. No budget of the header alone bounds the
// hold: a delimiter is no header content, so one is never refused for what
// MaxBytes, MaxFieldBytes, MaxCount or the shared budget have left, and the
// hold may look at more bytes than those have left before the line is known to
// be the header's, as any look-ahead may. Proved a delimiter, the held bytes
// are handed to the multipart reader as the candidate it was deciding, and no
// header budget is charged for them; otherwise they are read as the first
// bytes of the line, charged then, once, to every budget of the block, which
// refuses the line where it overflows one — a line held to the bound
// overflows the line budget, and a spent shared field count refuses it at its
// first byte. With no line budget a line still undecided past one buffer is
// refused as the multipart reader refuses any such framing line, with its own
// error wrapping bufio.ErrBufferFull, which ends the iteration: such a line is
// decided only within that buffer — unless HeaderLimits.UnboundedFramingLines
// is set, when it is held, without bound, until it is decided. A boundary that holds a '\r' or '\n' is
// never held. A part's header in which an orphan record was recovered is
// looked at the same way after it (ROB-020-002; see orphan.go). Other header
// blocks, and the message's own header, are not looked at this way;
// recovering a missing separator in general is ROB-020-005's.

// A MalformedPrefix is the malformed prefix a header block began with: the
// lines at its start that began with white space, which no field began before,
// kept byte for byte and interpreted as nothing. See Header.MalformedPrefix.
//
// It is an immutable view. The zero value is no prefix, which is what a Header
// that was not read from a block, or whose block began with a field, an empty
// line or the end of the input, reports. Copies of a MalformedPrefix, and of a
// Header, share the same bytes, which nothing modifies; Bytes hands out a copy.
// Its methods do no I/O and are safe for concurrent use.
type MalformedPrefix struct {
	p *malformedPrefix
}

type malformedPrefix struct {
	// b is the prefix's bytes, exactly as they were read. It is never
	// modified once built.
	b []byte

	// start is where the prefix begins, relative to the first byte of its
	// block: 0, or the length of the UTF-8 byte order mark before it.
	start int64

	// lines is how many physical lines the prefix holds.
	lines int
}

// MalformedPrefix returns the malformed prefix the block this header was read
// from began with, or the zero MalformedPrefix when it began with none. A nil
// Header has none.
//
// The prefix is the header's own, as its byte order mark is: Copy keeps it, and
// so does every mutation — Add, AddRaw, Set, Del and HeaderFields.Del change
// the fields and never the prefix. HeaderFromMap(h.Map()) builds a header
// without it. WriteHeader writes it first, before every field; see WriteHeader.
func (h *Header) MalformedPrefix() MalformedPrefix {
	if h == nil {
		return MalformedPrefix{}
	}
	return MalformedPrefix{p: h.prefix}
}

// Present reports whether there is a prefix.
func (p MalformedPrefix) Present() bool {
	return p.p != nil
}

// Bytes returns the prefix's bytes exactly as they were read — every line
// ending as it was, and a last line the input ended with io.EOF without one —
// as a new slice on every call, or nil when there is no prefix.
func (p MalformedPrefix) Bytes() []byte {
	if p.p == nil {
		return nil
	}
	return bytes.Clone(p.p.b)
}

// Location returns where the prefix lies in its header block, as the half-open
// range [start, end) of offsets relative to the block's first byte, on the
// terms of LineEndings: a UTF-8 byte order mark before it is counted, so a
// marked block's prefix begins at 3. end-start is len(Bytes()). It returns
// (0, 0) when there is no prefix. Where the block itself lies is the caller's
// to add: Part.HeaderOffset for a part.
func (p MalformedPrefix) Location() (start, end int64) {
	if p.p == nil {
		return 0, 0
	}
	return p.p.start, p.p.start + int64(len(p.p.b))
}

// Lines returns how many physical lines the prefix holds, as ReadHeader
// delimits lines — a '\r' that was content joins two into one — or 0 when
// there is no prefix.
func (p MalformedPrefix) Lines() int {
	if p.p == nil {
		return 0
	}
	return p.p.lines
}

// writeTo writes the prefix p, or nothing for a nil p, on the terms of
// WriteHeader.
func (p *malformedPrefix) writeTo(w io.Writer) error {
	if p == nil {
		return nil
	}
	return writeRawLines(w, p.b)
}

// keep captures b, bytes just read and charged, when a malformed prefix may
// still grow.
func (s *headerScan) keep(b []byte) {
	if s.capturing {
		s.capture = append(s.capture, b...)
	}
}

// unkeep drops the captured bytes from n on — those of a line a failure cut,
// or of what turned out not to be the prefix's — while the prefix may still
// grow. Once the capture has ended it holds the prefix whole until it is built,
// and nothing is dropped from it.
func (s *headerScan) unkeep(n int) {
	if s.capturing && n < len(s.capture) {
		s.capture = s.capture[:n]
	}
}

// lineKept records that the physical line whose captured bytes began at
// start was read whole. A line a '\r' joined to the one before it is that
// line continued, so the line it continues is still the last one.
func (s *headerScan) lineKept(start int) {
	if s.capturing && !s.lineRejoined {
		s.capLast = start
	}
}

// endCapture drops the captured bytes from n on and ends the capture: the
// prefix can no longer grow.
func (s *headerScan) endCapture(n int) {
	s.unkeep(n)
	s.capturing = false
}

// prefixOf builds the malformed prefix that began start bytes into the block,
// whose lines, each ended by "\r\n", are kv, from the bytes captured for it.
func (s *headerScan) prefixOf(start int64, kv []byte) *malformedPrefix {
	b := s.capture
	s.capture = nil
	if len(b) == 0 {
		return nil
	}
	return &malformedPrefix{
		b:     b[:len(b):len(b)],
		start: start,
		// Every line of kv ends with "\r\n", and no line's content holds a
		// '\n', which always ends a line.
		lines: bytes.Count(kv, []byte{'\n'}),
	}
}

// A delimiterLook is what looking at a line of a part's header, after a
// malformed prefix, says of it as a delimiter line of the part's multipart.
type delimiterLook uint8

const (
	// lookNot: not a delimiter, or not decided; the line is read as a line.
	lookNot delimiterLook = iota
	// lookProved: a whole delimiter line, still in the buffered reader.
	lookProved
	// lookLong: "--", the boundary, "--" for a closing one, and white space
	// that fills the buffer, or a '\r' that is its last byte; only holding the
	// line out of the buffer decides it. See holdDelimiter.
	lookLong
)

// parentDelimiter looks at the bytes at r's cursor as a delimiter line of
// s.parent, the multipart whose part header this block is, on the terms it
// recognizes one at the start of a part. It looks through the scanner's peek,
// so a failure it meets is recorded as the block's and nothing is read past it,
// one byte more than is held at a time, only while the bytes held still begin
// a delimiter line, and never more than the buffer holds. Nothing is consumed
// or charged.
//
// A line ending decides a delimiter only once it is known: "\r\n" or '\n', or a
// '\r' with the byte after it or io.EOF; a failure other than io.EOF before
// that leaves the line undecided, and it is read as a line, which reports the
// failure. A line whose name does not fit in the buffer is no delimiter.
func (s *headerScan) parentDelimiter(r *bufio.Reader) (delimiterLook, delimMatch) {
	mr := s.parent
	for {
		n := r.Buffered()
		buf, _ := r.Peek(n)
		full := n == r.Size()
		m := mr.match(buf, 0, 0, s.fail == io.EOF, full)
		switch {
		case m.status == matchDelim:
			return lookProved, m
		case m.status == matchFail:
			return lookNot, m
		case m.padding && full:
			// White space, or a '\r' whose next byte is not held, at the end
			// of a full buffer: this look cannot decide it.
			if bytes.ContainsAny(mr.dashBoundary, "\r\n") {
				return lookNot, m
			}
			return lookLong, m
		case full, s.fail != nil:
			return lookNot, m
		}
		s.peek(r, n+1)
	}
}

// holdLimit returns how many bytes of a line holdDelimiter may hold: the
// framing line budget of the multipart, MaxLineBytes, which bounds a delimiter
// line however the header around it is budgeted, or, with no line budget, one
// buffer of size bytes, the bound the multipart reader then puts on an
// undecided framing line. framing reports the latter. With no line budget and
// HeaderLimits.UnboundedFramingLines there is no bound: the line is held until
// a byte decides it or the input ends or fails, as the multipart reader then
// holds any undecided framing line.
//
// No budget of the header alone — MaxBytes, MaxFieldBytes, MaxCount or the
// shared budget — bounds it: a delimiter is no header content, so a proved one
// must not be refused for what the header has left. The bytes held are looked
// at before they are known to be the header's, as any look-ahead is, and are
// charged to it only if they prove to be a line of it.
func (s *headerScan) holdLimit(size int) (limit int64, framing bool) {
	if l := s.parent.limits.MaxLineBytes; l > 0 {
		return l, false
	}
	if s.parent.limits.UnboundedFramingLines {
		return math.MaxInt64, false
	}
	return int64(size), true
}

// holdDelimiter decides a line parentDelimiter found lookLong for, m being that
// look's match, by holding it: the white space after its name is taken out of
// the buffered reader, up to holdLimit's bound, until a byte after it decides
// the line.
//
// A line ending — "\r\n", '\n', or a '\r' with the byte after it or io.EOF —
// or io.EOF itself proves a delimiter: the held bytes, which the header never
// charged, are handed to the multipart reader as the candidate it was deciding
// (MultipartReader.cand), so that the part's body, being empty, proves it and
// counts its line against the framing line budget, and holdDelimiter returns
// true. No budget of the header is charged for it. Any other byte, a failure
// before the line is decided, or the bound reached, leaves the held bytes in
// s.replay, to be read first as the line they begin and charged then, once, to
// every budget of the block, which refuses it where it overflows one; a line
// held to the bound overflows the line budget. With no line budget, the bound
// reached is the multipart reader's own refusal of a framing line longer than
// its buffer, returned as the error, which ends the iteration.
//
// What is held never passes the bound, and nothing is read past a failure.
func (s *headerScan) holdDelimiter(r *bufio.Reader, m delimMatch) (bool, error) {
	mr := s.parent
	start := mr.offset()
	limit, framing := s.holdLimit(r.Size())
	refuse := func() error {
		err := fmt.Errorf("multipart: NextPart: %w", bufio.ErrBufferFull)
		mr.refuseLine(err)
		return err
	}
	if int64(m.k) > limit {
		if framing {
			return false, refuse()
		}
		// The line already overflows the line budget: read as the line it
		// is, it is refused there.
		return false, nil
	}
	b, _ := r.Peek(m.k)
	held := append(make([]byte, 0, m.k+64), b...)
	r.Discard(m.k)
	proved := func() bool {
		mr.cand = heldCandidate{active: true, held: held, close: m.close, start: start}
		return true
	}
	for {
		if r.Buffered() == 0 {
			if _, err := s.peek(r, 1); err != nil {
				if err == io.EOF {
					return proved(), nil
				}
				s.replay = held
				return false, nil
			}
		}
		b, _ := r.Peek(r.Buffered())
		i := 0
		for i < len(b) && isSpace(b[i]) {
			i++
		}
		if room := limit - int64(len(held)); int64(i) > room {
			if framing {
				return false, refuse()
			}
			held = append(held, b[:room]...)
			r.Discard(int(room))
			s.replay = held
			return false, nil
		}
		held = append(held, b[:i]...)
		r.Discard(i)
		if i == len(b) {
			continue
		}
		switch b[i] {
		case '\n':
			return proved(), nil
		case '\r':
			// The byte after it, or io.EOF, decides whether the '\r' is a line
			// ending, which it then is either way; a failure leaves it undecided.
			if next, err := s.peek(r, 2); len(next) == 2 || err == io.EOF {
				return proved(), nil
			}
		}
		s.replay = held
		return false, nil
	}
}
