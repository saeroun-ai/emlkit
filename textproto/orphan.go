package textproto

import (
	"bufio"
	"bytes"
	"io"
	"math"
)

// Orphan continuation lines (ROB-020-002).
//
// A continuation line — one that begins with white space, SP or HTAB —
// continues the logical candidate before it. When that candidate is a field,
// the line is part of the field, as it always was: folding, whitespace-only
// lines included, and a candidate the semicolon fallback accepts as a field
// ("X-Bad; value" then " more") are read exactly as before. When the candidate
// is damaged, so that no field can be read from it, its continuation lines
// continue nothing: they are orphans. Every such block used to be refused
// whole, losing every field after it, or, for an empty name, to lose the
// lines silently.
//
// A damaged candidate is one the rules a header block has always been read
// with refuse to make a field of, judged on the whole logical candidate as
// they always were — the name is what comes before the first colon anywhere in
// it, or the first semicolon when it has no colon:
//
//   - one with neither a colon nor a semicolon ("broken line");
//   - one whose name holds a byte a name cannot hold: white space ("Bad Name:
//     v"), NUL, a control or 8-bit byte, or the line ending of its first line,
//     when the only colon is on a continuation line ("broken" then " orphan:
//     value", and "X-Test; value" then " folded: value", whose colon wins over
//     its semicolon as it always did);
//   - one whose name is empty (": value", or "; value" with no colon).
//
// A damaged candidate with at least one continuation line read whole is now
// kept, its damaged first line and its continuation lines together, as one
// orphan record: apart from the fields, byte for byte, interpreted as nothing.
// The damaged first line is kept with its continuations because it is what
// makes them orphans: written back, a run of lines that begins with white
// space would continue whatever field was written before it, while one that
// begins with the damaged line is read back as the same record. The block is
// read on after it as usual.
//
// A damaged candidate with no continuation line is read exactly as before: an
// empty name is skipped, and any other is the error it always was ("malformed
// MIME header line", "malformed MIME header key"). What a damaged line with no
// continuation, or a colonless line, should be is ROB-020-003's and
// ROB-020-006's to decide; nothing here gives a damaged line a name, a value
// or a key.
//
// # What is a continuation line read whole
//
// The lines of a candidate are gathered as they always were: a continuation
// line is one whose first byte is SP or HTAB, and a '\r' that proves to be
// content joins the line after it to the line it was in (ROB-010-002), so the
// physical lines of a candidate are the lines of that rule. A '\r' after a
// valid field joins what follows to that field: "Subject: before\r: value\r
// orphan" is a Subject whose value is "before\r: value orphan", as it always
// was, and no record. A damaged line whose last line ended with a '\r' that the
// next line settles: if that line proves to be the damaged line's content, it
// joins it, and the continuation lines it brings are the damaged line's; if it
// proves to be a field, the '\r' ended the damaged line and nothing continues
// it. An empty name is held for that, and read on past, as it always was. Any
// other damaged line was refused at its '\r', with the next line unread, and
// still is unless settleCR proves, within what the block's budgets admit of
// those bytes (or crLegacyCap with none), that they join it and bring a
// continuation line: only then are they read, as the record's. Anything that
// does not prove a record — a following field included — is in the reader
// afterwards exactly as before, whatever was looked at to decide it, and the
// decision does not depend on the reader's buffer size or the input's
// chunking. A proof the budgets cannot admit is no proof: the old error, not a
// budget refusal. A spent shared field count refuses first, as for any line
// after an unsettled '\r', and a read failure other than io.EOF met by the
// look is returned as itself. A proof that looked past a caller's reader's
// buffer, and did not prove a record, grows that buffer (see restoreReader).
// A continuation line that a read
// failure other than io.EOF cut is no line, as for any field, and one the
// input ended with io.EOF is a line; a candidate left with no continuation
// line read whole is a damaged line with none.
//
// # Budgets and failures
//
// A record is one logical candidate, read on the terms of any field: the
// shared field count is asked before its first byte, it is charged to the field
// counts once, read whole, and every byte to MaxLineBytes, MaxFieldBytes,
// MaxBytes and the shared bytes once, as read. A block a budget refuses keeps
// no record. A failure other than io.EOF keeps the lines read whole before it,
// so a record keeps only whole lines, and one left with no continuation is not
// kept; the failure is returned as itself.
//
// A record's exact bytes are rebuilt from the bytes kept for the candidate —
// every physical line ended by "\r\n" — and the line ending each line had in
// the input, which the scanner notes, one byte a line, for every candidate as
// it is read, in storage it reuses across the block. Rebuilding allocates only
// for a damaged candidate, its exact size, once the candidate has been read
// and charged; a healthy candidate costs one stored byte a line and no
// allocation, unless it has more than len(headerScan.termsFix) physical lines,
// when the storage grows once and is reused by the rest of the block. A record
// retains at most the bytes the budgets admitted for its candidate.
//
// # In a part's header
//
// In a part's header in which a record has been recovered, every later line is
// first looked at as a delimiter line of its multipart, exactly as after a
// malformed prefix (see prefix.go): a proved one ends the block, the part's
// body is empty, Part.HeaderSeparatorMissing reports it, and the part after it
// follows. Before this, such a part's header was refused at the damaged line,
// and asking for the next part found that delimiter; reading on past the
// damage must not read it as a line of the header. A line held to decide it is
// charged on the same terms as after a prefix. A damaged line whose '\r' is
// still to be settled is looked at too: a line that is, or may be, a
// delimiter decides it with nothing consumed past it, as the error it always
// was. Headers without a record or a prefix are not looked at this way;
// recovering a missing separator in general is ROB-020-005's.
//
// # Serialization
//
// WriteHeader writes each record in its place: before the first field, of
// those read from the block, that followed it, and that the header still has,
// or after every field when none does. Fields added since (Add, AddRaw, Set)
// are written first, as they always are, and records never move among the
// fields read with them; Copy and every mutation keep them, HeaderFromMap does
// not. Every byte is written as read; a last line the input ended with no line
// ending is followed by "\r\n", and one ended by a bare '\r' by "\n", so that
// what follows can never continue it. A record never begins with white space,
// so it never continues the field written before it. An unmodified complete
// block whose lines all end with "\r\n" and whose only damaged lines are
// records is written back byte for byte; damaged lines with no continuation
// are still not written. That is not a replay of the input (ROB-020-007).

// An OrphanContinuation is an orphan record of a header block: a damaged line
// that no field could be read from — no colon or semicolon, an invalid name, or
// an empty name — and the continuation lines that followed it, at least one,
// kept together byte for byte and interpreted as nothing. See
// Header.OrphanContinuations.
//
// It is an immutable view. The zero value is no record: Bytes returns nil,
// Location (0, 0) and Lines 0. Copies share the same bytes, which nothing
// modifies; Bytes hands out a copy. Its methods do no I/O and are safe for
// concurrent use.
type OrphanContinuation struct {
	o *orphanRecord
}

// An orphanRecord is one orphan record of a block. It is never modified once
// built.
type orphanRecord struct {
	// b is the record's exact bytes: its damaged first line and its
	// continuation lines, every line ending as it was read, and a last line
	// the input ended with io.EOF without one.
	b []byte

	// start is where the record begins, relative to the first byte of its
	// block, on the terms of LineEndings.
	start int64

	// lines is how many physical lines it holds, at least two.
	lines int

	// after is how many fields of the block were read before it: it is
	// written before the field whose seq is greater than after.
	after int
}

// OrphanContinuations returns the orphan records of the block this header was
// read from, in the order they were read, as a new slice on every call, or nil
// when it had none. A nil Header has none.
//
// The records are the header's own, as its malformed prefix is: Copy keeps
// them, and so does every mutation — Add, AddRaw, Set, Del and
// HeaderFields.Del change the fields and never the records. They are not
// fields: Get, Has, Values, Fields, FieldsByKey, Len, Raw and Map never see
// them, so an indented "Content-Type" or "Content-Transfer-Encoding" in one
// declares nothing. HeaderFromMap(h.Map()) builds a header without them.
// WriteHeader writes each in its place among the fields; see WriteHeader.
func (h *Header) OrphanContinuations() []OrphanContinuation {
	if h == nil || len(h.orphans) == 0 {
		return nil
	}
	l := make([]OrphanContinuation, len(h.orphans))
	for i, o := range h.orphans {
		l[i] = OrphanContinuation{o: o}
	}
	return l
}

// Bytes returns the record's bytes exactly as they were read — its damaged
// first line, its continuation lines, every line ending as it was, and a last
// line the input ended with io.EOF without one — as a new slice on every call,
// or nil for the zero OrphanContinuation.
func (o OrphanContinuation) Bytes() []byte {
	if o.o == nil {
		return nil
	}
	return bytes.Clone(o.o.b)
}

// Location returns where the record lies in its header block, as the half-open
// range [start, end) of offsets relative to the block's first byte, on the
// terms of LineEndings: a UTF-8 byte order mark at the block's start is
// counted. end-start is len(Bytes()). It returns (0, 0) for the zero
// OrphanContinuation. Where the block itself lies is the caller's to add:
// Part.HeaderOffset for a part.
func (o OrphanContinuation) Location() (start, end int64) {
	if o.o == nil {
		return 0, 0
	}
	return o.o.start, o.o.start + int64(len(o.o.b))
}

// Lines returns how many physical lines the record holds, as ReadHeader
// delimits lines — a '\r' that was content joins two into one — which is at
// least two: the damaged line and Lines()-1 continuation lines. It returns 0
// for the zero OrphanContinuation.
func (o OrphanContinuation) Lines() int {
	if o.o == nil {
		return 0
	}
	return o.o.lines
}

// record builds the orphan record p holds, after fields read before it, or
// nil when p is left with fewer than two lines.
func (p *pendingField) record(after int) *orphanRecord {
	lines := bytes.Count(p.kv, []byte{'\n'})
	if (lines < 2 && !p.single) || len(p.raw) == 0 {
		return nil
	}
	return &orphanRecord{
		b:     p.raw[:len(p.raw):len(p.raw)],
		start: p.start,
		lines: lines,
		after: after,
	}
}

// crLegacyCap bounds settleCR's proof when no byte budget of the block is in
// force (ReadHeader, and Read's own header reader): 64 KiB, the default
// MaxHeaderFieldBytes and MaxLineBytes a parse holds a field to. It does not
// depend on any reader's buffer size.
const crLegacyCap = 64 << 10

// settleCR decides a colonless or invalid damaged line with no continuation
// line whose last line ended with a '\r' still to be settled: whether the
// bytes after the '\r' prove, on the rules readLineSlice and take apply, that
// the '\r' is content — the next line does not begin 1*name-byte *WSP ":" —
// and that the line it joins (with any further '\r' that is content and the
// line it joins) ends and is followed by a continuation line, SP or HTAB. It
// reports true for a record, false for the damaged line's old error, or a
// budget refusal or read failure.
//
// The shared field count is asked first, as for any line after an unsettled
// '\r': a spent count refuses here, before anything is looked at.
//
// The proof is bounded by what the budgets would admit of those bytes, not by
// r's buffer: the joined physical line's content by MaxLineBytes, less the
// damaged line's content and the '\r' that becomes content (line endings are
// not content); every byte of the joined lines, line endings included, by
// what MaxFieldBytes, MaxBytes and the shared budget have left; and, when none
// of them is in force, by crLegacyCap. The byte after the joined lines that
// proves the continuation is looked at and counted by none of them. A proof
// that does not fit is no proof: the damaged line is its old error, not a
// budget refusal — nothing past it is read as the block's. A record that the
// proof admits is then read as any field is, and a budget its continuation
// lines overflow refuses the block where it says.
//
// In a part's header a line of the multipart — one that begins after the '\r'
// or after any '\r' or '\n' among those bytes — that is a whole delimiter
// line ("--", the boundary, "--" for the closing one, white space, then a line
// ending or the end of the input) is no record: the damaged line is its old
// error and the delimiter stays unread for the next part. A line that only
// begins like one is looked at as any line is.
//
// Bytes are looked at through the scanner's peek while they fit in r's buffer,
// one more than is held at a time, so a read failure or io.EOF is met at the
// same byte whatever the buffer size or chunking. Past the buffer they are
// taken out of r, exactly as many as are looked at. Then, for a record, they
// are the scanner's replay, read first as the record's bytes and charged then;
// otherwise they are given back to r (see restoreReader and
// MultipartReader.restore), so that what follows the '\r' is in r exactly as
// it was. A failure other than io.EOF before the proof is decided is returned
// as itself, after any bytes taken out are given back.
func (s *headerScan) settleCR(r *bufio.Reader) (record bool, err error) {
	if len(s.replay) > 0 {
		// Never the case: a damaged line read whole leaves no replay.
		return false, nil
	}
	if err := s.preflightField(); err != nil {
		return false, err
	}

	// The rooms the budgets leave, each compared, never summed.
	lineRoom := int64(math.MaxInt64)
	if l := s.lim.MaxLineBytes; l > 0 {
		lineRoom = l - s.lineBytes - 1
	}
	byteRoom := int64(math.MaxInt64)
	bounded := s.lim.MaxLineBytes > 0
	if l := s.lim.MaxFieldBytes; l > 0 {
		byteRoom, bounded = min(byteRoom, l-s.fieldBytes), true
	}
	if l := s.lim.MaxBytes; l > 0 {
		byteRoom, bounded = min(byteRoom, l-s.consumed), true
	}
	if room, ok := s.lim.Total.roomBytes(); ok {
		byteRoom, bounded = min(byteRoom, room), true
	}
	if !bounded {
		byteRoom = crLegacyCap
	}

	var held []byte
	holding := false
	// at returns the byte i bytes past the '\r'; ok is false at the end of
	// the input (io.EOF) or a read failure (that failure).
	at := func(i int) (c byte, ok bool, err error) {
		if !holding && i < r.Size() {
			b, perr := s.peek(r, i+1)
			if len(b) > i {
				return b[i], true, nil
			}
			if perr == nil {
				perr = s.fail
			}
			return 0, false, perr
		}
		if !holding {
			// Past the buffer: take what has been looked at out of r,
			// and from now on exactly as much as is looked at.
			holding = true
			held = make([]byte, 0, 2*i)
			b, _ := r.Peek(i)
			held = append(held, b...)
			r.Discard(i)
		}
		for len(held) <= i {
			if r.Buffered() == 0 {
				if _, perr := s.peek(r, 1); perr != nil {
					return 0, false, perr
				}
			}
			n := min(r.Buffered(), i+1-len(held))
			b, _ := r.Peek(n)
			held = append(held, b...)
			r.Discard(n)
		}
		return held[i], true, nil
	}
	done := func(rec bool, err error) (bool, error) {
		switch {
		case !holding:
		case rec:
			s.replay = held
		case s.parent != nil:
			s.parent.restore(held)
		default:
			restoreReader(r, held)
		}
		return rec, err
	}
	// eof ends the proof at the end of the input or a failure.
	eof := func(err error) (bool, error) {
		if err == nil || err == io.EOF {
			return done(false, nil)
		}
		return done(false, err)
	}

	var dash []byte
	if s.parent != nil && !bytes.ContainsAny(s.parent.dashBoundary, "\r\n") {
		dash = s.parent.dashBoundary
	}
	seg := 0 // where the current line of the multipart begins
	// delim reports whether the bytes from seg to end are a whole delimiter
	// line of the parent multipart, its line ending at end.
	delim := func(end int) bool {
		if dash == nil || end-seg < len(dash) {
			return false
		}
		k := seg
		for _, d := range dash {
			if c, _, _ := at(k); c != d {
				return false
			}
			k++
		}
		if end-k >= 2 {
			c1, _, _ := at(k)
			c2, _, _ := at(k + 1)
			if c1 == '-' && c2 == '-' {
				k += 2
			}
		}
		for ; k < end; k++ {
			if c, _, _ := at(k); !isSpace(c) {
				return false
			}
		}
		return true
	}

	var content, total int64
	// take counts one byte of the joined lines: content of the joined
	// physical line, or of its line ending. It reports whether the budgets
	// still admit them.
	take := func(isContent bool, n int64) bool {
		if isContent {
			content += n
		}
		total += n
		return content <= lineRoom && total <= byteRoom
	}

	i := 0
	for {
		// The line after a '\r', probed as a field's first line is.
		space := false
	probe:
		for {
			c, ok, err := at(i)
			switch {
			case !ok:
				// Only the end of the input ends a delimiter line; a
				// failure is returned as itself.
				if (err == nil || err == io.EOF) && delim(i) {
					return done(false, nil)
				}
				return eof(err)
			case c == ':':
				// A field: the '\r' ended the line before it.
				return done(false, nil)
			case !space && validHeaderKeyByte(c):
			case isSpace(c):
				space = true
			default:
				break probe
			}
			if !take(true, 1) {
				return done(false, nil)
			}
			i++
		}
		// Not a field: the '\r' was content, and this line continues the
		// damaged one.
		chained := false
	line:
		for {
			c, ok, err := at(i)
			if !ok {
				if (err == nil || err == io.EOF) && delim(i) {
					return done(false, nil)
				}
				return eof(err)
			}
			if c != '\r' && c != '\n' {
				if !take(true, 1) {
					return done(false, nil)
				}
				i++
				continue
			}
			if delim(i) {
				return done(false, nil)
			}
			if c == '\n' {
				if !take(false, 1) {
					return done(false, nil)
				}
				i++
				seg = i
				break line
			}
			n, ok, err := at(i + 1)
			if !ok {
				// A '\r' the end of the input follows ends the last line.
				return eof(err)
			}
			switch {
			case n == '\n':
				if !take(false, 2) {
					return done(false, nil)
				}
				i += 2
				seg = i
				break line
			case isSpace(n):
				// A bare '\r' ending the line, and a continuation line.
				if !take(false, 1) {
					return done(false, nil)
				}
				return done(true, nil)
			case n == '\r':
				// The next line begins with a '\r': no continuation.
				return done(false, nil)
			case validHeaderKeyByte(n):
				// Another '\r' the next line settles: content if it joins.
				if !take(true, 1) {
					return done(false, nil)
				}
				i++
				seg = i
				chained = true
				break line
			default:
				// Content, as a '\r' inside a line always is.
				if !take(true, 1) {
					return done(false, nil)
				}
				i++
				seg = i
			}
		}
		if chained {
			continue
		}
		// The line ended: a continuation line follows it, or nothing does.
		c, ok, err := at(i)
		if !ok {
			return eof(err)
		}
		if isSpace(c) {
			return done(true, nil)
		}
		return done(false, nil)
	}
}

// A prefixReader reads b, bytes given back to a reader, and then src.
type prefixReader struct {
	b   []byte
	src io.Reader
}

func (p *prefixReader) Read(d []byte) (int, error) {
	if len(p.b) > 0 {
		n := copy(d, p.b)
		p.b = p.b[n:]
		if len(p.b) == 0 {
			p.b = nil
		}
		return n, nil
	}
	return p.src.Read(d)
}

// restoreReader gives held, bytes taken out of r by settleCR, back to r, a
// reader settleCR does not own: r then yields held, the bytes it still
// buffered, and then what it would have yielded, its pending failure
// included. The bytes it buffered are moved after held; a copy of r, whose
// buffer is then empty, keeps its source, its buffer and any failure not yet
// returned, and r is made a new reader of held, those bytes and that copy, so
// that no buffer is shared and r never reads itself. Nothing is read.
//
// r's buffer grows: to at least twice its size, and at least to hold every
// byte given back. A restore happens only when a proof looked past r's whole
// buffer, so each restore at least doubles it, and a proof that fits the
// grown buffer needs none: the copies a reader accumulates are as many as the
// doublings of its size, logarithmic in the longest proof it ever needed and
// independent of how many headers were read, and their buffers sum to less
// than its final size.
func restoreReader(r *bufio.Reader, held []byte) {
	n := r.Buffered()
	b, _ := r.Peek(n)
	pre := make([]byte, 0, len(held)+n)
	pre = append(append(pre, held...), b...)
	r.Discard(n)
	size := r.Size()
	if size <= math.MaxInt/2 {
		size *= 2
	} else {
		size = math.MaxInt
	}
	size = max(size, len(pre))
	old := new(bufio.Reader)
	*old = *r
	*r = *bufio.NewReaderSize(&prefixReader{b: pre, src: old}, size)
}

// A lineTerm is how one physical line of a candidate ended in the input.
type lineTerm uint8

const (
	termCRLF lineTerm = iota // "\r\n"
	termLF                   // '\n'
	termCR                   // '\r', a line ending or content the next line joins
	termNone                 // io.EOF, before any line ending
)

// text returns the bytes of t.
func (t lineTerm) text() string {
	switch t {
	case termLF:
		return "\n"
	case termCR:
		return "\r"
	case termNone:
		return ""
	}
	return "\r\n"
}

// resetTerms begins the line endings of a new candidate.
func (s *headerScan) resetTerms() {
	s.termsN = 0
	s.termsMore = s.termsMore[:0]
}

// noteTerm notes how the physical line just kept ended.
func (s *headerScan) noteTerm(t lineTerm) {
	if s.termsN < len(s.termsFix) {
		s.termsFix[s.termsN] = t
	} else {
		s.termsMore = append(s.termsMore, t)
	}
	s.termsN++
}

// termAt returns how the i-th physical line of the candidate ended. A line
// with nothing noted — which kept lines never are — is given "\r\n".
func (s *headerScan) termAt(i int) lineTerm {
	switch {
	case i < 0 || i >= s.termsN:
		return termCRLF
	case i < len(s.termsFix):
		return s.termsFix[i]
	}
	return s.termsMore[i-len(s.termsFix)]
}

// crlf is the line ending every kept line is given.
var crlf = []byte("\r\n")

// exact returns the exact bytes of kv, the candidate just read, its lines
// ended by "\r\n" as they are kept: each line's content, which is the input's
// own, followed by the line ending it had. A line's content never holds
// "\r\n" — a '\r' followed by '\n' always ends a line — so every "\r\n" in kv
// ends one. It also returns where the last line begins in what it returns.
// The result is allocated once, at its exact size.
func (s *headerScan) exact(kv []byte) (b []byte, last int) {
	size := 0
	rest := kv
	for i := 0; ; i++ {
		j := bytes.Index(rest, crlf)
		if j < 0 {
			size += len(rest)
			break
		}
		size += j + len(s.termAt(i).text())
		rest = rest[j+len(crlf):]
	}
	b = make([]byte, 0, size)
	rest = kv
	for i := 0; len(rest) > 0; i++ {
		last = len(b)
		j := bytes.Index(rest, crlf)
		if j < 0 {
			b = append(b, rest...)
			break
		}
		b = append(b, rest[:j]...)
		b = append(b, s.termAt(i).text()...)
		rest = rest[j+len(crlf):]
	}
	return b, last
}

// writeRawLines writes b, raw lines kept apart from the fields, on the terms of
// WriteHeader: every byte in order, and a last line with no line ending given
// "\r\n", or one ended by a bare '\r' given "\n", so that nothing written after
// it can continue it.
func writeRawLines(w io.Writer, b []byte) error {
	if _, err := w.Write(b); err != nil {
		return err
	}
	var term string
	switch {
	case len(b) == 0:
	case b[len(b)-1] == '\n':
	case b[len(b)-1] == '\r':
		term = "\n"
	default:
		term = "\r\n"
	}
	if term == "" {
		return nil
	}
	_, err := io.WriteString(w, term)
	return err
}
