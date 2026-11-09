package message

import (
	"bytes"
	"fmt"
	"io"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Limits.MaxLineBytes outside a header block: the framing lines of a multipart
// body, and the framing lines of a line-oriented transfer encoding.
//
// # What the budget is for, and what it is not for
//
// It bounds a physical line that some part of the parser has to materialize or
// interpret as a line. Inside a header block that is every line of the block,
// and textproto charges it there while the block is read. Outside one there are
// exactly two such places, and they are here:
//
//   - the framing of a multipart body — the preamble lines before the first
//     delimiter, the delimiter lines, and the empty line between a part's body
//     and the delimiter that follows it;
//   - the framing of the transfer encodings that are defined in terms of lines,
//     which are quoted-printable and Base64.
//
// It is not a bound on body content. An opaque text, binary, 7bit, 8bit or
// encoded body that contains no line terminator is not one enormous line: it is
// streamed, is never accumulated into a line buffer, and is never refused for
// its length alone. What bounds it is MaxEncodedBodyBytes and
// MaxDecodedBodyBytes, which a later item enforces.
//
// That distinction is what shapes the transfer-encoding side of this. A decoder
// is handed a stream, not a line; whether the stream contains a line at all is
// something only a terminator can settle. So what runs there is a monitor and
// not a reader of lines: it counts the bytes since the last terminator without
// holding any of them, and it refuses only when a terminator arrives and proves
// that what preceded it was a framed line longer than the budget. A body that
// simply never terminates a line is never refused by it, and never costs it a
// byte of memory.
//
// # What a failure means
//
// The scope reading the line ends, and nothing is resynchronized. A multipart
// iterator that met an overlong framing line does not know where the next
// delimiter is, and a decoder that met an overlong encoded line is standing in
// the middle of one; guessing is the only alternative to stopping, and guessing
// is what a safety budget exists to avoid. Everything already returned stays
// valid and owned by the caller — the parts an iterator already yielded, and
// every byte a body already produced — as FatalError describes.
//
// Each failure records exactly one limit.line_bytes diagnostic, once, however
// many times the caller asks again.

// multipartLineAction and cteLineAction are what a limit.line_bytes diagnostic
// reports was done: nothing was repaired, and the scope stopped where the budget
// did, because there is nothing safe to resume from.
const (
	multipartLineAction = "stopped iterating this multipart and returned no further part"
	cteLineAction       = "stopped decoding this body and returned no further bytes"
)

// lineLimitFailure records the one diagnostic of a multipart framing line that
// exceeded the budget, ends this iterator, and returns the error every later
// call to it will return.
//
// The defect belongs to the entity whose body is being iterated rather than to
// any part of it: a framing line is the container's own syntax, and at the
// moment it is refused there is no part it could be attributed to. So the
// diagnostic carries that entity's path, and a position measured into that
// entity's body.
//
// It is kept for the same reason a refused part header is: the iterator must
// report it identically however often it is asked, recording it twice would put
// one defect in the session twice, and the reader cannot be asked again anyway.
func (r *multipartReader) lineLimitFailure(limit *textproto.LineLimitError) error {
	d := Diagnostic{
		Code:     CodeLimitLineBytes,
		Severity: SeverityLimitExceeded,
		Message:  fmt.Sprintf("multipart framing line exceeds the %d byte limit", limit.Limit()),
		Action:   multipartLineAction,
		Cause:    limit,
		// The offset the failure carries is relative to the body being
		// iterated, which is what this turns into a position in the original
		// input. An entity whose own position is not known supplies none, and
		// none is claimed.
		Location: r.parent.bodyLocationAt(limit.Offset()),
	}
	d = r.parent.completeBody(d)

	if fatal := r.parent.recordBody(d); fatal != nil {
		// A sink stopped the whole parse while it was being told about this
		// line. That stop outranks this scope's own failure and is what every
		// scope of the parse now reports, this one included.
		r.fatal = fatal
		return fatal
	}

	r.fatal = newFatalDiagnostic(d)
	return r.fatal
}

// framedEncoding reports whether the wire form of a transfer encoding is
// defined in terms of lines, so that its input has framing lines to bound at
// all.
//
// Quoted-printable is: its soft line breaks and its 76-character limit are line
// syntax. Base64 as used by MIME is: it is emitted in lines, and a decoder has
// to skip the terminators between them. The others — 7bit, 8bit, binary and no
// encoding at all — impose no line structure the parser interprets, so their
// bodies are opaque content and are left alone.
//
// The name is normalized exactly as encodingReader normalizes it, so that what
// is monitored and what is decoded can never disagree about which encoding this
// is.
func framedEncoding(enc string) bool {
	switch normalizeEncodingName(enc) {
	case "quoted-printable", "base64":
		return true
	}
	return false
}

// watchCTELines returns r with a monitor over it when this parse bounds framing
// lines and enc is an encoding that has them, and returns r unchanged
// otherwise.
//
// The monitor goes under the decoder rather than over it, because what the
// budget describes is the input's lines: what a decoder produces has no line
// structure the parser imposed, and a base64 body decodes to bytes whose
// newlines are the message author's content rather than framing.
//
// A parse that budgets nothing, a disabled budget, and an encoding with no line
// framing all return r itself, so nothing is wrapped that would only pass bytes
// through.
func (c parseContext) watchCTELines(enc string, r io.Reader) io.Reader {
	limits, ok := c.limits()
	if !ok || limits.MaxLineBytes <= 0 || !framedEncoding(enc) {
		return r
	}
	return &cteLineMonitor{r: r, ctx: c, enc: enc, limit: limits.MaxLineBytes}
}

// A cteLineMonitor passes a transfer-encoded body through unchanged while
// counting the length of each framed line in it, and refuses the body once a
// line terminator proves that the line it ended was longer than the budget.
//
// It holds no bytes. Everything it is given is handed straight on, and what it
// keeps is a handful of counters, so a body of any size — including one that
// never terminates a line at all — costs it the same fixed memory. That is the
// property that lets a line budget apply to a stream without turning a stream
// into a line.
//
// It refuses only on evidence. A run of bytes with no terminator after it is not
// a long line; it is a body that has not shown a line at all, and is bounded by
// the body budgets instead. Only a terminator settles what preceded it, so only
// a terminator can refuse it.
type cteLineMonitor struct {
	r     io.Reader
	ctx   parseContext
	enc   string
	limit int64

	// consumed is the raw bytes of this body handed to the caller so far, which
	// is also the offset, within the body, of the next byte to be delivered.
	consumed int64

	// lineStart is the offset within the body at which the current line began.
	lineStart int64

	// lastByte is the last byte handed on, which is what says whether a '\n' at
	// the very start of the next read was preceded by a '\r'. It is only read
	// when at least one byte has been delivered.
	lastByte byte

	// stopped is the sticky failure of this body: the fatal error the refusal
	// produced, returned unchanged by every later read. The diagnostic behind it
	// is recorded once, when it is set.
	stopped error
}

// Read implements io.Reader.
//
// It delivers what its source delivers until a line is refused, and then
// delivers the largest prefix that the refusal does not cover: the bytes of the
// refused line itself are not handed on, because the scope reading them has
// ended, while everything before it is complete and is the caller's. After that
// every read returns the identical error.
func (m *cteLineMonitor) Read(p []byte) (int, error) {
	if m.stopped != nil {
		return 0, m.stopped
	}

	n, err := m.r.Read(p)
	if n <= 0 {
		return n, err
	}

	keep := m.inspect(p[:n])
	m.consumed += int64(keep)
	if keep > 0 {
		m.lastByte = p[keep-1]
	}
	if m.stopped != nil {
		if keep == 0 {
			// Nothing may be delivered, so the failure is reported now rather
			// than after a read that made no progress.
			return 0, m.stopped
		}
		// The prefix goes out first; the failure is what the next read returns.
		return keep, nil
	}
	return n, err
}

// inspect scans the bytes just read for line terminators, charges each line
// they complete against the budget, and reports how many of those bytes may be
// delivered.
//
// It returns all of them while nothing is refused. When a line is refused it
// sets stopped and returns the bytes before that line began, which is none of
// them when the line began in an earlier read.
func (m *cteLineMonitor) inspect(b []byte) int {
	for i := 0; i < len(b); {
		j := bytes.IndexByte(b[i:], '\n')
		if j < 0 {
			// No terminator in what is left, so nothing here completes a line
			// and nothing here can be refused.
			break
		}

		end := i + j                   // index of the terminator in b
		pos := m.consumed + int64(end) // its offset within the body

		// The line is what precedes the terminator, and a '\r' immediately
		// before a '\n' is part of that terminator rather than content. The
		// '\r' may have arrived in an earlier read, which is what lastByte is
		// for.
		length := pos - m.lineStart
		if length > 0 {
			prev := m.lastByte
			if end > 0 {
				prev = b[end-1]
			}
			if prev == '\r' {
				length--
			}
		}

		if length > m.limit {
			m.refuse()
			keep := m.lineStart - m.consumed
			if keep < 0 {
				// The refused line began before this read, so none of what
				// arrived belongs to anything complete.
				return 0
			}
			return int(keep)
		}

		i = end + 1
		m.lineStart = m.consumed + int64(i)
	}
	return len(b)
}

// refuse records the one diagnostic of this refusal and sets the sticky error
// every later read returns.
func (m *cteLineMonitor) refuse() {
	d := Diagnostic{
		Code:     CodeLimitLineBytes,
		Severity: SeverityLimitExceeded,
		Message:  fmt.Sprintf("%s encoded line exceeds the %d byte limit", m.enc, m.limit),
		Field:    "Content-Transfer-Encoding",
		Action:   cteLineAction,
		// Where the refused line began, measured into this entity's body as it
		// appeared in the input, before any decoding. An entity whose own
		// position is not known supplies none, and none is claimed.
		Location: m.ctx.bodyLocationAt(m.lineStart),
	}
	d = m.ctx.completeBody(d)

	if fatal := m.ctx.recordBody(d); fatal != nil {
		// A sink stopped the whole parse while it was being told about this
		// line. That stop describes more of the parse than this body's own
		// failure does, so it is what this body reports from here on.
		m.stopped = fatal
		return
	}
	m.stopped = newFatalDiagnostic(d)
}
