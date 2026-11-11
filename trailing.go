package message

import (
	"io"
	"strconv"
)

// The input after the message (ROB-010-005, issue #19).
//
// # What can follow a message
//
// One input holds one message. RFC 5322 gives a message body no end marker of
// its own, so the body of a message that frames no parts runs to the end of
// the input, and every byte of it is body, a header-like line or a whole
// second message pasted after the first included. Nothing here looks inside a
// body: no body is scanned, split or reinterpreted.
//
// A message whose body is a multipart does have an end of its own: its
// closing delimiter line. RFC 2046 calls whatever follows it the epilogue, and
// says it is to be ignored; a non-empty one is conformant, whatever it says.
// It is also where another message, concatenated after this one, would be.
// Those bytes belong to no entity. NextPart, Walk and mail.Reader end at the
// closing delimiter and do not read on for them. That is not to say none of
// them was ever read from the input: the buffered readers of the parse fetch
// ahead as they always have, so some of them may already be held in a buffer
// and counted against Limits.MaxInputBytes; the rest is never checked.
//
// # What ProfileForensic adds
//
// Under ProfileForensic, and only there, the first iterator of the message's
// own multipart body that returns io.EOF at a proven closing delimiter makes
// the input after that delimiter's line ending available to the caller:
// ParseSession.TrailingInputReader returns a reader of it, and
// ParseSession.TrailingInput a snapshot of what reading it has shown. Making it
// available reads no input; it allocates the reader and its bounded state.
// This feature reads nothing more unless the caller reads that reader: no
// iterator, Walk, mail.Reader, Close or accessor peeks at or drains it for
// this, and what the buffers do not already hold stays in the input, which
// belongs to the caller. Every other profile records nothing, makes nothing
// available and reads exactly what it always read. MultipartEpilogue, asked of
// the iterator that reached the delimiter, is the caller reading that reader:
// it reads the epilogue through it, so the bytes it keeps are delivered here,
// counted and looked at, and are not delivered again (multipart_epilogue.go).
//
// The reader delivers the bytes as the input holds them, every one of them,
// once: what the multipart reader's buffer still holds, then what the
// message's buffered reader holds, then the input, until the input ends. It
// keeps no copy. Bytes it delivered cannot be read again, from it or from the
// session; replay is #41's. The snapshot is metadata, never the bytes.
//
// Ownership is one-shot, as for every body of a parse. The trailing input is
// only the reader's while nothing else reads the message's input: reading the
// message's Body directly or writing the message with WriteTo — whose
// verbatim copy of a multipart body includes everything after its closing
// delimiter — before or instead of iterating it consumes those bytes as body,
// is not an inspection, and leaves TrailingUnknown; doing either, or
// iterating the message's body with a second MultipartReader, once the closing
// delimiter has been reached is not supported, and nothing then guarantees
// what the reader still delivers.
//
// # What a read returns
//
// While the tail is open, a read of at least one byte is a scope of the
// parse: before it reads, it reports a stop the session already has — a
// sink's, a budget's, or a cancellation of the ParseContext, seen even while
// the bytes it would deliver are already buffered — and reads nothing. After
// the reader below returns n bytes and err, the n bytes are delivered and
// counted, and then:
//
//   - a stop of the session, seen after the read (a cancellation, the input
//     budget proving an overflow, the input stalling, or a sink refusing the
//     diagnostic below), is returned with them, and ends the tail: Stopped;
//   - io.EOF itself, and only that value, ends it: Ended, and the snapshot's
//     Location is then the whole of the trailing input;
//   - any other error of the input — a wrapped io.EOF, io.ErrUnexpectedEOF and
//     bufio.ErrBufferFull included — is returned as the identical value and
//     ends it: Failed. It is no stop, and no diagnostic is recorded for the
//     error itself; message-like evidence the bytes delivered with it, or
//     before it, already proved is still kept and still reported, below. The
//     input is never read again after it, as for every read of a parse;
//   - otherwise the read returns (n, nil) and the tail stays Open.
//
// The first of these ends is final. Every later read returns it again, as
// itself, and reads nothing: a stop or a cancellation that comes after a tail
// already Ended or Failed does not replace what ended it, as it does not
// revoke an io.EOF a body already returned. A read of zero bytes reads
// nothing and inspects nothing: it returns (0, nil) while the tail is open,
// even when the parse has been stopped meanwhile, and what ended the tail
// after that. The reader is for one goroutine, as every read of a parse is: it
// is not safe for concurrent use, and must not be read from inside a
// DiagnosticSink or a CharsetReader. It blocks while the input blocks;
// cancelling the context of ParseContext never interrupts a read. A caller
// that stops reading leaves the rest unread and releases nothing it does not
// own; the reader needs no Close. Parts already returned stay valid, whatever
// happens to the tail.
//
// # Budgets
//
// The trailing input is input: Limits.MaxInputBytes counts what is read for it
// once, at the source, as it counts every other read, and its budget and the
// buffered prefetch keep the semantics they have for every read. A tail that
// runs past that budget ends with the session's limit.input_bytes stop; the
// bytes already delivered stay delivered, and a read returns the n bytes it
// got with what ended it, but which of the admitted bytes a stop finds still
// undelivered in a buffer depends on how the input arrived, and no promise is
// made that every byte up to the budget is delivered. Limits.MaxEpilogueBytes
// is not applied to this reader, which the caller reads as far as it likes; it
// bounds what MultipartEpilogue keeps, which reads through this reader and
// stops one byte past that budget. With every budget disabled the reader still
// holds no copy of what it delivers, and the classification below holds at
// most one line of bounded length.
//
// # Evidence
//
// As the bytes are delivered, and only the bytes delivered, the first 4096 of
// them are looked at for whether they begin like the header of another
// message. Bytes a buffer holds but the reader has not delivered are not
// looked at. Line endings are "\r\n", '\n' or '\r', the longest that
// matches: a '\r' is decided by the byte after it, a '\n' making it a
// "\r\n", or by io.EOF itself, and a failure before that byte leaves it
// undecided. After any number of blank lines — lines of nothing but spaces
// and tabs — the first line that is not blank begins like a message when
// either:
//
//   - it is an mbox "From_" line of exactly the grammar ParseOptions.
//     RecognizeMboxEnvelope recognizes (mbox.go), at most 998 bytes before its
//     line ending, or Limits.MaxLineBytes if that is smaller, and that line
//     ending; the evidence then ends with the line ending — after the '\n' of
//     a "\r\n", before the byte that follows a lone '\r'; or
//   - it and the next line that is not a continuation (a line beginning with a
//     space or a tab) both begin as header fields: a name of visible ASCII but
//     ':', then ':', then a space, a tab or a line ending. A blank line between
//     them ends the header and decides against it. The second field is proved
//     by the byte after its ':', without waiting for the rest of its value:
//     the evidence ends with that space, tab or '\n', or, for an empty value
//     ended by a '\r', with the line ending as above.
//
// That is a suspicion and no more: the bytes are not parsed as a message, no
// address, date or field is interpreted, and nothing is split off, removed or
// unescaped. A conformant epilogue can begin exactly like that, and a
// concatenated message that does not — one preceded by other text, a byte order
// mark, a line beginning with white space, a single field, or whose decisive
// bytes lie past the first 4096 — is not recognized. A decision needs its
// bytes within those 4096; the byte after them is never looked at, and its
// arrival decides Unrecognized, even when it is the byte a '\r' at the last
// position waited for. io.EOF right after the 4096 still decides. The
// evidence, once decided, stays as it is whatever later ends the tail:
//
//   - MessageLike: as above. The session records one input.trailing_messages
//     diagnostic of SeverityInfo for it, with the nil path and the evidence's
//     Location, and no byte of the input in its Message or Action — unless a
//     stop of the session was seen first, by the read that proved it. The
//     evidence and the bytes delivered are in the snapshot before the sink is
//     called, so a sink may read TrailingInput; it must not read the tail. A
//     sink that refuses it stops the parse, and that read returns its bytes
//     with the stop. MaxDiagnosticCount and DiscardDeliveredDiagnostics affect
//     only the diagnostic, never the evidence.
//   - Empty: the input ended, with io.EOF itself, right after the delimiter.
//   - Blank: the input ended, with io.EOF itself, within the 4096 bytes, and
//     every byte before was a space, a tab or a line ending.
//   - Unrecognized: the bytes looked at decide that it does not begin like a
//     message, or a byte past the 4096 arrived first. It says nothing about
//     the rest.
//   - Undecided: not read far enough to decide; a tail that failed or stopped
//     before its evidence was decided stays so.
//
// No diagnostic is recorded for any but MessageLike: an epilogue is not a
// defect, and neither is an ordinary, blank, empty or binary one.

// trailingEvidenceWindow is how many bytes at the start of the trailing input
// the evidence is decided from.
const trailingEvidenceWindow = 4096

// trailingMessagesAction is what the parse did with a trailing input that
// begins like another message.
const trailingMessagesAction = "parsed none of it as a message: the bytes stay the input after the message, delivered verbatim by ParseSession.TrailingInputReader; nothing was split, removed or interpreted"

// A TrailingStatus is how far the input after the message is known.
type TrailingStatus uint8

const (
	// TrailingUnknown is the zero value: nothing is known. The parse is not
	// ProfileForensic; or no iterator of the message's own multipart body has
	// returned io.EOF at a proven closing delimiter — it was not iterated that
	// far, or ended another way, the closing delimiter missing included; or the
	// body was read directly, copied by WriteTo or replaced, so what follows it
	// was consumed as body or never placed.
	TrailingUnknown TrailingStatus = iota

	// TrailingNone is that, structurally, there is no trailing input: the
	// message, as the parse interpreted its header, frames no parts, so its
	// body runs to the end of the input. It says nothing about that body or
	// about how much of the input was read or checked.
	TrailingNone

	// TrailingOpen is that the closing delimiter was reached and the input
	// after it has not been read to an end; some of it may have been read.
	TrailingOpen

	// TrailingEnded is that the input after the closing delimiter was read to
	// io.EOF: Location is all of it.
	TrailingEnded

	// TrailingFailed is that an error of the input ended reading it: Err is
	// that error, the identical value.
	TrailingFailed

	// TrailingStopped is that a stop of the parse ended reading it: Err is the
	// session's FatalError.
	TrailingStopped
)

// String returns the lower-case name of the status, or a "TrailingStatus(n)"
// placeholder for an unknown value.
func (s TrailingStatus) String() string {
	switch s {
	case TrailingUnknown:
		return "unknown"
	case TrailingNone:
		return "none"
	case TrailingOpen:
		return "open"
	case TrailingEnded:
		return "ended"
	case TrailingFailed:
		return "failed"
	case TrailingStopped:
		return "stopped"
	}
	return "TrailingStatus(" + strconv.Itoa(int(s)) + ")"
}

// A TrailingEvidence is what the start of the trailing input showed; see the
// evidence rules in trailing.go and ParseSession.TrailingInputReader.
type TrailingEvidence uint8

const (
	// TrailingEvidenceUndecided is the zero value: not read far enough to
	// decide.
	TrailingEvidenceUndecided TrailingEvidence = iota

	// TrailingEvidenceEmpty is that the input ended, with io.EOF, right after
	// the closing delimiter.
	TrailingEvidenceEmpty

	// TrailingEvidenceBlank is that the input ended, with io.EOF, within the
	// first 4096 bytes, all of them spaces, tabs and line endings.
	TrailingEvidenceBlank

	// TrailingEvidenceUnrecognized is that the bytes looked at decided that it
	// does not begin like a message, or that more than 4096 bytes arrived
	// first. It says nothing about the rest.
	TrailingEvidenceUnrecognized

	// TrailingEvidenceMessageLike is that it begins like the header of another
	// message. It may be one, or an epilogue that looks like one.
	TrailingEvidenceMessageLike
)

// String returns the lower-case name of the evidence, or a
// "TrailingEvidence(n)" placeholder for an unknown value.
func (e TrailingEvidence) String() string {
	switch e {
	case TrailingEvidenceUndecided:
		return "undecided"
	case TrailingEvidenceEmpty:
		return "empty"
	case TrailingEvidenceBlank:
		return "blank"
	case TrailingEvidenceUnrecognized:
		return "unrecognized"
	case TrailingEvidenceMessageLike:
		return "message-like"
	}
	return "TrailingEvidence(" + strconv.Itoa(int(e)) + ")"
}

// A TrailingInput is a snapshot of what is known of the input after the
// message: ParseSession.TrailingInput returns one. It is a value, it is never
// updated, and it holds none of the input's bytes. The zero value is
// TrailingUnknown.
type TrailingInput struct {
	status    TrailingStatus
	start     int64
	delivered int64
	evidence  TrailingEvidence
	evStart   int64
	evEnd     int64
	err       error
}

// Status reports how far the trailing input is known.
func (t TrailingInput) Status() TrailingStatus { return t.status }

// Start returns the absolute offset of the first byte after the closing
// delimiter line, its line ending included, and true; or false, and 0, while
// the status is TrailingUnknown or TrailingNone, when there is no such
// position.
func (t TrailingInput) Start() (int64, bool) {
	if t.status < TrailingOpen {
		return 0, false
	}
	return t.start, true
}

// Delivered returns how many bytes the trailing input's reader has delivered.
func (t TrailingInput) Delivered() int64 { return t.delivered }

// Location returns the absolute range of the bytes delivered so far, from
// Start; it is the whole of the trailing input only when the status is
// TrailingEnded. It is the zero Location, with no offset, when there is no
// Start.
func (t TrailingInput) Location() Location {
	if t.status < TrailingOpen {
		return Location{}
	}
	return LocationRange(t.start, t.start+t.delivered)
}

// Evidence returns what the start of the trailing input showed, and for
// TrailingEvidenceMessageLike the absolute range of the bytes that showed it:
// from the first byte of the first line that is not blank to the end of what
// decided it. The Location is the zero Location for every other evidence.
func (t TrailingInput) Evidence() (TrailingEvidence, Location) {
	if t.evidence != TrailingEvidenceMessageLike {
		return t.evidence, Location{}
	}
	return t.evidence, LocationRange(t.evStart, t.evEnd)
}

// Err returns what ended reading the trailing input when the status is
// TrailingFailed or TrailingStopped — the input's own error, or the session's
// stop, each the identical value the reader returned — and nil otherwise.
func (t TrailingInput) Err() error { return t.err }

// TrailingInput returns a snapshot of what is known of the input after the
// message: under ProfileForensic, the input after the closing delimiter of
// the message's own multipart body once an iterator of that body has reached
// it, or that there is none because the message frames no parts. See the
// account at the top of trailing.go.
//
// It reads nothing and changes nothing. It is safe for concurrent use, a
// DiagnosticSink may call it, and it returns the zero TrailingInput, which is
// TrailingUnknown, for a nil session.
func (s *ParseSession) TrailingInput() TrailingInput {
	if s == nil {
		return TrailingInput{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.trailing; t != nil {
		return TrailingInput{
			status:    t.status,
			start:     t.start,
			delivered: t.delivered,
			evidence:  t.evidence,
			evStart:   t.evStart,
			evEnd:     t.evEnd,
			err:       t.err,
		}
	}
	if s.trailingNone {
		return TrailingInput{status: TrailingNone}
	}
	return TrailingInput{}
}

// TrailingInputReader returns the one reader of the input after the closing
// delimiter of the message's own multipart body, and true, once under
// ProfileForensic an iterator of that body has returned io.EOF at that
// delimiter; and nil and false before that, for every other parse and for a
// nil session. Every call returns the same reader. It delivers the bytes
// verbatim, once, and is the one way to inspect them, provided nothing else
// reads the message's input; see the account at the top of trailing.go for
// that ownership, what each read returns, the evidence it gathers and the one
// diagnostic it may record.
//
// Calling it reads nothing and changes nothing.
func (s *ParseSession) TrailingInputReader() (io.Reader, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	t := s.trailing
	s.mu.Unlock()
	if t == nil {
		return nil, false
	}
	return t, true
}

// noteUnframedRoot notes, under ProfileForensic, that the message's root frames
// no parts, so that there is no trailing input. It reads nothing.
func (s *ParseSession) noteUnframedRoot() {
	if s == nil || s.cfg.profile != ProfileForensic {
		return
	}
	s.mu.Lock()
	s.trailingNone = true
	s.mu.Unlock()
}

// noteTrailing registers, under ProfileForensic, the input after the closing
// delimiter this iterator consumed, when this iterator frames the message's own
// body as the parse read it. The first iterator to get there wins. It reads
// nothing, and allocates nothing once a trailing input is registered: every
// later NextPart of an ended iterator returns io.EOF through here.
func (r *multipartReader) noteTrailing() {
	s := r.parent.session
	if s == nil || s.cfg.profile != ProfileForensic {
		return
	}
	// The root of the input: no path, a known origin — a body the caller
	// replaced has none — and nothing descended through.
	o := r.parent.origin
	if !o.known || len(o.path) != 0 || r.parent.depth != 0 {
		return
	}
	s.mu.Lock()
	registered := s.trailing != nil
	s.mu.Unlock()
	if registered {
		return
	}
	rd, off, ok := r.r.Epilogue()
	if !ok {
		return
	}
	lineCap := maxMboxEnvelopeContent
	if l := s.Limits().MaxLineBytes; l > 0 && l < int64(lineCap) {
		lineCap = int(l)
	}
	t := &trailingTail{session: s, src: rd, start: o.bodyOffset + off, status: TrailingOpen}
	t.cls.lineCap = lineCap
	// Looked at again: another iterator of the same body may have registered
	// meanwhile, and the first stays. The iterator that registered it reads its
	// epilogue through it; see MultipartEpilogue.
	s.mu.Lock()
	if s.trailing == nil {
		s.trailing = t
		r.epi.tail = t
	}
	s.mu.Unlock()
}

// A trailingTail is the input after the message's closing delimiter, and the
// one reader of it.
type trailingTail struct {
	session *ParseSession
	start   int64 // absolute offset of its first byte

	// Owned by the goroutine reading it.
	src  io.Reader // the multipart reader's epilogue; nil once ended
	term error     // what ended it, returned by every later read
	cls  trailingClassifier

	// Guarded by session.mu; TrailingInput reports them.
	status    TrailingStatus
	delivered int64
	evidence  TrailingEvidence
	evStart   int64 // absolute
	evEnd     int64 // absolute
	err       error
}

// Read implements io.Reader; see the account at the top of trailing.go.
func (t *trailingTail) Read(p []byte) (int, error) {
	if t.term != nil {
		return 0, t.term
	}
	if len(p) == 0 {
		return 0, nil
	}
	s := t.session
	if f := s.fatalErr(); f != nil {
		t.end(TrailingStopped, f)
		return 0, f
	}

	n, err := t.src.Read(p)
	if n < 0 {
		n = 0
	} else if n > len(p) {
		n = len(p)
	}

	// The evidence is gathered from the bytes delivered and from io.EOF
	// itself, and nothing else.
	newly := t.cls.feed(p[:n])
	if err == io.EOF && t.cls.finish() {
		newly = true
	}

	var stop error
	if f := s.fatalErr(); f != nil {
		stop = f
	}

	// What this read showed is published before anything is told about it, so
	// that a sink may read it.
	s.mu.Lock()
	t.delivered += int64(n)
	t.evidence = t.cls.evidence
	if t.cls.evidence == TrailingEvidenceMessageLike {
		t.evStart, t.evEnd = t.start+t.cls.evStart, t.start+t.cls.evEnd
	}
	s.mu.Unlock()

	// One diagnostic, for message-like evidence, unless the parse had already
	// stopped. A sink that refuses it stops the parse here.
	if newly && stop == nil {
		if rerr := s.record(t.messageLike()); rerr != nil {
			stop = rerr
		} else if f := s.fatalErr(); f != nil {
			stop = f
		}
	}

	switch {
	case stop != nil:
		t.end(TrailingStopped, stop)
		return n, stop
	case err == io.EOF:
		t.end(TrailingEnded, io.EOF)
		return n, io.EOF
	case err != nil:
		t.end(TrailingFailed, err)
		return n, err
	}
	return n, nil
}

// end ends the tail with status, which every later read reports as err. The
// reference to the multipart reader's epilogue is dropped: nothing reads it
// again through this tail. That reader, its buffers and the input stay owned
// by whoever else holds them.
func (t *trailingTail) end(status TrailingStatus, err error) {
	t.term = err
	t.src = nil
	t.cls.line = nil
	s := t.session
	s.mu.Lock()
	t.status = status
	if status != TrailingEnded {
		t.err = err
	}
	s.mu.Unlock()
}

// messageLike is the diagnostic of message-like evidence. It carries no byte
// of the input.
func (t *trailingTail) messageLike() Diagnostic {
	return Diagnostic{
		Code:     CodeInputTrailingMessages,
		Severity: SeverityInfo,
		Message:  "the input after the closing delimiter of the message's multipart body begins like the header of another message; it may be a concatenated message or an epilogue that only looks like one",
		Action:   trailingMessagesAction,
		Location: LocationRange(t.start+t.cls.evStart, t.start+t.cls.evEnd),
	}
}

// The phases of a trailingClassifier.
const (
	tcBlank = iota // in leading blank lines
	tcFirst        // in the first line that is not blank
	tcRest         // in the rest of a header field line, or a continuation
	tcNext         // at the start of a line after the first header field
	tcName         // in the name of the second header field
)

// A trailingClassifier decides the evidence of the trailing input from its
// bytes as they are delivered, one at a time, so that how they are chunked
// cannot change it. It holds at most lineCap bytes, the line it is checking
// for the mbox grammar. Offsets are relative to the trailing input.
type trailingClassifier struct {
	lineCap int // the most bytes of a "From_" line before its line ending

	off      int64 // bytes looked at
	decided  bool
	evidence TrailingEvidence
	evStart  int64
	evEnd    int64

	phase int
	cr    bool  // a '\r' ended a line; a '\n' next completes its ending
	ws    bool  // tcBlank: this line so far is spaces and tabs
	first int64 // where the first line that is not blank begins
	pos   int   // tcFirst: bytes of that line so far
	field bool  // tcFirst, tcName: still possibly a header field
	name  int   // bytes of its name so far
	colon bool  // its ':' was just seen
	from  bool  // tcFirst: still possibly a "From_" line
	line  []byte

	// proofCR is that the evidence is proven up to a '\r' — the line ending
	// of a valid "From_" line, or the empty value of the second header
	// field — whose own extent the next byte decides: a '\n' completes a
	// "\r\n" and is part of the evidence, anything else follows a lone '\r'
	// and is not, and io.EOF itself ends the input after it. A failure before
	// that byte leaves the evidence undecided; that byte arriving past the
	// window, where it cannot be looked at, decides it unrecognized.
	proofCR bool
}

// decide fixes the evidence. For message-like evidence it spans [start, end).
func (c *trailingClassifier) decide(e TrailingEvidence, start, end int64) {
	c.decided = true
	c.evidence = e
	if e == TrailingEvidenceMessageLike {
		c.evStart, c.evEnd = start, end
	}
	c.line = nil
}

// feed looks at b, the next bytes delivered, and reports whether it decided
// message-like evidence.
func (c *trailingClassifier) feed(b []byte) bool {
	for _, x := range b {
		if c.decided {
			return false
		}
		if c.off >= trailingEvidenceWindow {
			c.decide(TrailingEvidenceUnrecognized, 0, 0)
			return false
		}
		c.step(x, c.off)
		c.off++
		if c.decided && c.evidence == TrailingEvidenceMessageLike {
			return true
		}
	}
	return false
}

// finish decides the evidence of a trailing input io.EOF ended, and reports
// whether it decided message-like evidence.
func (c *trailingClassifier) finish() bool {
	if c.decided {
		return false
	}
	switch {
	case c.off == 0:
		c.decide(TrailingEvidenceEmpty, 0, 0)
	case c.phase == tcBlank:
		c.decide(TrailingEvidenceBlank, 0, 0)
	case c.proofCR:
		// A '\r' io.EOF follows is a line ending of its own.
		c.decide(TrailingEvidenceMessageLike, c.first, c.off)
		return true
	default:
		c.decide(TrailingEvidenceUnrecognized, 0, 0)
	}
	return false
}

// trailingFieldName reports whether x may be in a header field name: visible
// ASCII but ':'.
func trailingFieldName(x byte) bool {
	return x >= 0x21 && x <= 0x7e && x != ':'
}

// step looks at x, the byte at offset at.
func (c *trailingClassifier) step(x byte, at int64) {
	if c.proofCR {
		// The byte after the '\r' that ends the evidence: a '\n' completes
		// its "\r\n", anything else follows a lone '\r' and is not part of it.
		end := at
		if x == '\n' {
			end = at + 1
		}
		c.decide(TrailingEvidenceMessageLike, c.first, end)
		return
	}

	// A '\n' right after a '\r' completes the line ending the '\r' already
	// ended; anything else is looked at as the first byte of the next line.
	if c.cr {
		c.cr = false
		if x == '\n' {
			return
		}
	}

	switch c.phase {
	case tcBlank:
		switch x {
		case ' ', '\t':
			c.ws = true
		case '\n':
			c.ws = false
		case '\r':
			c.ws, c.cr = false, true
		default:
			if c.ws {
				// The first line that is not blank begins with white space.
				c.decide(TrailingEvidenceUnrecognized, 0, 0)
				return
			}
			c.phase, c.first = tcFirst, at
			c.field, c.from = true, c.lineCap >= len("From ")
			c.stepFirst(x, at)
		}
	case tcFirst:
		c.stepFirst(x, at)
	case tcRest:
		switch x {
		case '\n':
			c.phase = tcNext
		case '\r':
			c.phase, c.cr = tcNext, true
		}
	case tcNext:
		switch x {
		case ' ', '\t':
			c.phase = tcRest // a continuation line
		case '\r', '\n':
			// A blank line ends the header after one field.
			c.decide(TrailingEvidenceUnrecognized, 0, 0)
		default:
			c.phase, c.field, c.name, c.colon = tcName, true, 0, false
			c.stepName(x, at)
		}
	case tcName:
		c.stepName(x, at)
	}
}

// stepFirst looks at x, a byte of the first line that is not blank.
func (c *trailingClassifier) stepFirst(x byte, at int64) {
	if c.colon {
		// The byte after the first field's ':'. A field and a "From_" line
		// cannot both begin a line, so the latter is no longer possible.
		switch x {
		case ' ', '\t':
			c.phase = tcRest
		case '\n':
			c.phase = tcNext
		case '\r':
			c.phase, c.cr = tcNext, true
		default:
			c.decide(TrailingEvidenceUnrecognized, 0, 0)
		}
		return
	}

	if c.field {
		switch {
		case x == ':' && c.name > 0:
			c.colon = true
		case trailingFieldName(x):
			c.name++
		default:
			c.field = false
		}
	}

	if c.from {
		const prefix = "From "
		switch {
		case c.pos < len(prefix):
			if x != prefix[c.pos] {
				c.from = false
			} else {
				c.line = append(c.line, x)
			}
		case x == '\n' || x == '\r':
			if !mboxEnvelopeText(string(c.line[len(prefix):])) {
				c.decide(TrailingEvidenceUnrecognized, 0, 0)
				return
			}
			if x == '\n' {
				c.decide(TrailingEvidenceMessageLike, c.first, at+1)
				return
			}
			c.proofCR = true
			c.pos++
			return
		case x == ' ' || x == '\t' || (x >= 0x21 && x <= 0x7e):
			if len(c.line) >= c.lineCap {
				c.from = false
			} else {
				c.line = append(c.line, x)
			}
		default:
			c.from = false
		}
		if !c.from {
			c.line = nil
		}
	}

	c.pos++
	if !c.field && !c.from {
		c.decide(TrailingEvidenceUnrecognized, 0, 0)
	}
}

// stepName looks at x, a byte of the second header field's name, or the byte
// after its ':'. A space, a tab or a '\n' there proves the evidence with that
// byte, without waiting for the rest of the value; a '\r' is a line ending
// whose extent the byte after it decides, see proofCR.
func (c *trailingClassifier) stepName(x byte, at int64) {
	if c.colon {
		switch x {
		case ' ', '\t', '\n':
			c.decide(TrailingEvidenceMessageLike, c.first, at+1)
		case '\r':
			c.proofCR = true
		default:
			c.decide(TrailingEvidenceUnrecognized, 0, 0)
		}
		return
	}
	switch {
	case x == ':' && c.name > 0:
		c.colon = true
	case trailingFieldName(x):
		c.name++
	default:
		c.decide(TrailingEvidenceUnrecognized, 0, 0)
	}
}
