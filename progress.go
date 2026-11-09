package message

import (
	"errors"
	"fmt"
	"io"
	"mime"
)

// Reads that make no progress (ROB-001-007, issue #7).
//
// io.Reader lets a read return (0, nil), "nothing happened", and the standard
// library disagrees about how long to wait for something to happen: bufio gives
// up after 100 such reads in one fill, while io.ReadAll, io.Copy, the base64
// decoder and a golang.org/x/text transform.Reader ask again for ever. Under
// Parse and ParseContext one rule applies wherever a reader the parser does not
// own could stall it: 100 consecutive reads that asked for bytes and got exactly
// (0, nil) are a stall, and the 99 before them are passed on as they came. A
// read that delivers a byte resets the count. An empty request is answered
// without asking the reader and counts nothing. An error, io.EOF and a reader's
// own io.ErrNoProgress included, neither counts nor resets and is passed on
// unchanged, together with any bytes that came with it. Nothing is retried,
// nothing sleeps and no goroutine is started. The rule holds under every
// profile and every Limits value, LimitDisabled included; nothing turns it off.
//
// It is applied in three places, each to a reader the parser cannot see past:
//
//   - The input (progressReader). The count sits directly over the caller's
//     reader, below the cancellation gate, the MaxInputBytes limiter and every
//     buffer and decoder, so it does not depend on buffering, read-ahead, a
//     decoder's internal loop or a WriterTo/ReaderFrom copy. A stall ends the
//     whole parse: one input.no_progress diagnostic of SeverityFatal, nil path,
//     at the offset of the next input byte, Cause io.ErrNoProgress, and the
//     session's stop, published as a MaxInputBytes overflow publishes its own —
//     an earlier published stop wins with nothing recorded, a sink refusing the
//     diagnostic wins, MaxDiagnosticCount may drop the diagnostic but not the
//     stop. The input is never read again, and a leaf body recognizes the stop
//     by identity as it recognizes the input budget's (see rawBody.note). The
//     same reader also makes the input's own first error final, io.EOF
//     included, without making it a stop (ROB-010-006; see progressReader).
//   - The decoders of a leaf body (bodyGuard). A CharsetReader's reader may
//     return (0, nil) without reading anything. Reads in which the decoders
//     delivered nothing are counted unless a stage took input during them: a
//     wire byte of the body, or a byte of the charset stage's own input, which
//     a transfer decoder may already hold buffered, so the wire count alone
//     would not see it. Either resets the count, so a decoder that consumes
//     input while it emits nothing is not stalled (the body budgets bound it).
//     A stall ends that body alone with body.decoder_no_progress, as a
//     per-body budget does; a parent multipart still reaches the following
//     parts.
//   - The CharsetReader readers of encoded words in the Content-Type and
//     Content-Disposition parameters the engine interprets (metadataDecoder).
//     A stall keeps that parameter value raw, as an unknown charset does; the
//     field's other values are still decoded, and the field records one
//     header.decoder_no_progress diagnostic however many of its values stalled.
//
// Read, ReadWithOptions, New, NewMultipart, mail.CreateReader, the Header
// accessors and textproto are unchanged: on them a reader that returns (0, nil)
// for ever still spins wherever it did. A reader that blocks, or loops inside
// one call without returning — a source, a CharsetReader or the reader it
// returned — cannot be interrupted. A count below zero or above len(p) breaks
// io.Reader and is outside this item (ROB-120-001): it is passed on, neither
// counted nor reset.

// maxConsecutiveEmptyReads is how many consecutive (0, nil) reads are a stall,
// the same number bufio gives up after.
const maxConsecutiveEmptyReads = 100

const (
	inputNoProgressAction    = "stopped reading the input after 100 consecutive reads returned no bytes and no error, and read nothing further from it; everything already returned stays valid"
	bodyNoProgressAction     = "stopped this body after its decoders returned no bytes and no error on 100 consecutive reads without taking input; the header and the bytes already delivered stay valid, and a parent multipart can continue with the next part unless another limit or the diagnostic sink stops it"
	metadataNoProgressAction = "kept the whole raw text of every parameter value whose charset decoder made no progress, decoded the other parameter values as usual, left the raw field unchanged, and continued parsing"
)

// watchProgress returns r under this parse's no-progress rule, or r itself for
// a nil session, which is what Read and ReadWithOptions pass.
func (s *ParseSession) watchProgress(r io.Reader) io.Reader {
	if s == nil {
		return r
	}
	return &progressReader{r: r, session: s}
}

// A progressReader applies the no-progress rule to the input of a parse, and
// makes the input's first error final (ROB-010-006): the read that returned it
// returns it with its bytes, and every later read returns the identical value,
// io.EOF included, without asking the input again. io.Reader promises nothing
// about a read after an error, and the buffered readers above forget an error
// once they have handed it out, so without this a failure they reported once
// could be followed by more input, as though it had not happened. It is not a
// stop of the parse: no diagnostic is recorded, and the error stays the
// input's own value. It implements io.Reader alone, and is read by one
// goroutine at a time, as the stream of a parse is.
type progressReader struct {
	r       io.Reader
	session *ParseSession

	empty  int   // consecutive (0, nil) results to nonempty requests
	offset int64 // bytes the input has delivered
	failed error // the sticky stop, once there is one
	srcErr error // the input's own first error, once there is one
}

// Read implements io.Reader.
func (g *progressReader) Read(p []byte) (int, error) {
	if g.failed != nil {
		return 0, g.failed
	}
	if g.srcErr != nil {
		return 0, g.srcErr
	}
	if len(p) == 0 {
		return 0, nil
	}
	n, err := g.r.Read(p)
	if err != nil {
		g.srcErr = err
	}
	switch {
	case n > 0 && n <= len(p):
		g.empty = 0
		g.offset += int64(n)
	case n == 0 && err == nil:
		g.empty++
		if g.empty >= maxConsecutiveEmptyReads {
			g.failed = g.session.inputNoProgress(g.offset)
			return 0, g.failed
		}
	}
	return n, err
}

// inputNoProgress records the one input.no_progress diagnostic of a stalled
// input and publishes the session's stop, which it returns and notes as the
// input's failure. It follows inputOverflow: an earlier published stop wins and
// nothing is recorded, the diagnostic is delivered with no lock held, and a sink
// that refuses it has published its own stop by the time this one is offered.
func (s *ParseSession) inputNoProgress(offset int64) error {
	s.mu.Lock()
	stop := s.stop
	s.mu.Unlock()
	if stop == nil {
		d := Diagnostic{
			Code:     CodeInputNoProgress,
			Severity: SeverityFatal,
			Message:  fmt.Sprintf("input returned no bytes and no error on %d consecutive reads", maxConsecutiveEmptyReads),
			Action:   inputNoProgressAction,
			Location: LocationAt(offset),
			Cause:    io.ErrNoProgress,
		}
		_ = s.record(d)
		stop = newFatalDiagnostic(d)
	}
	s.mu.Lock()
	published := s.publishStop(stop)
	if s.inputStop == nil {
		s.inputStop = published
	}
	s.mu.Unlock()
	return published
}

// progress is how much input the stages under a leaf body's decoders have
// taken: wire bytes handed out by the raw reader, plus bytes the charset stage
// took from its own input (see newWithOptions). The two are counted apart
// because a transfer decoder may hold output it already decoded, which a
// CharsetReader's reader can consume a byte at a time while the wire count
// stands still. Only its changes matter.
func (g *bodyGuard) progress() int64 {
	return g.raw.admitted + g.charsetIn.n
}

// noProgress counts a read of a leaf body's decoders that returned (0, nil),
// and returns the body's failure once that makes a stall. before is progress()
// when the read began: if that moved, a stage took input and the count starts
// again.
//
// A stall reports, in order, the body's own local failure, the input's failure
// of this parse as the body received it, an ordinary failure of the input the
// decoders swallowed (with no diagnostic: the input failed, nothing stalled
// it), or else this body's own refusal, which a stop of the whole parse
// published earlier outranks (see refuseBody).
func (g *bodyGuard) noProgress(before int64) error {
	if g.progress() != before {
		g.empty = 0
		return nil
	}
	g.empty++
	if g.empty < maxConsecutiveEmptyReads {
		return nil
	}
	switch {
	case g.slot.err != nil:
		g.err = g.slot.err
	case g.slot.src != nil:
		g.err = g.slot.src
	case g.slot.ioErr != nil:
		g.err = g.slot.ioErr
	default:
		g.err = g.ctx.refuseBody(g.slot, Diagnostic{
			Code:     CodeBodyDecoderNoProgress,
			Severity: SeverityFatal,
			Message:  fmt.Sprintf("body decoders returned no bytes and no error on %d consecutive reads without taking input", maxConsecutiveEmptyReads),
			Action:   bodyNoProgressAction,
			Location: g.ctx.bodyLocationAt(g.raw.admitted),
			Cause:    io.ErrNoProgress,
		})
	}
	return g.err
}

// A metadataDecoder decodes the encoded words in the parameter values of one
// Content-Type or Content-Disposition the parse engine interprets, through the
// configured CharsetReader, with every reader that hook returns held to the
// no-progress rule. A nil *metadataDecoder decodes exactly as decodeHeader
// does, which is what every entry point but Parse uses.
//
// It belongs to one interpretation of one field and is used by one goroutine.
// A stall is decided per parameter value: the reader that stalled fails its
// word, mime.WordDecoder stops at that word, and the value keeps its whole raw
// text, as for an unknown charset. Every other value of the field is decoded
// independently, so which values decode never depends on the order they are
// visited in. stalled remembers the raw values that stalled, so that the
// whole-field reading and a later repair or retained-prefix reading of the same
// field do not hand the same value to the hook again, and makes the field owe
// one diagnostic however many values stalled. The parameter budgets bound how
// many values a field has, the header budgets how long they are, and each
// hook reader is called at most 100 times without progress.
type metadataDecoder struct {
	poll    func() bool // the parse's cancellation poll, or nil
	stalled map[string]bool
}

// errMetadataStalled and errMetadataCancelled end the copy of one encoded word.
// They are never inspected: the guard decides by its own flag, and the value
// keeps its raw text whichever ended it.
var (
	errMetadataStalled   = errors.New("message: encoded-word charset decoder made no progress")
	errMetadataCancelled = errors.New("message: parse cancelled while decoding an encoded word")
)

// decode returns v with its encoded words decoded, or v itself if any of them
// could not be, as decodeHeader does.
func (d *metadataDecoder) decode(v string) string {
	if d == nil {
		s, _ := decodeHeader(v)
		return s
	}
	if d.stalled[v] {
		return v
	}
	w := &valueDecoder{poll: d.poll}
	wd := mime.WordDecoder{CharsetReader: w.charsetReader}
	s, err := wd.DecodeHeader(v)
	if w.stalled {
		if d.stalled == nil {
			d.stalled = make(map[string]bool)
		}
		d.stalled[v] = true
		return v
	}
	if err != nil {
		return v
	}
	return s
}

// A valueDecoder guards the hook readers of the encoded words of one parameter
// value. stalled is set by the first of them that stalls.
type valueDecoder struct {
	poll    func() bool
	stalled bool
}

// charsetReader is charsetReader with the reader the hook returns guarded.
func (w *valueDecoder) charsetReader(charset string, input io.Reader) (io.Reader, error) {
	if w.stalled {
		return nil, errMetadataStalled
	}
	in := &countedInput{r: input}
	r, err := charsetReader(charset, in)
	if err != nil || r == nil {
		return r, err
	}
	return &metadataReader{r: r, in: in, val: w}, nil
}

// A countedInput is the input handed to a CharsetReader, counting what the
// hook's reader takes of it. It implements io.Reader alone, so the reader
// cannot take bytes around the count, and passes every result on unchanged.
type countedInput struct {
	r io.Reader
	n int64
}

func (c *countedInput) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 && n <= len(p) {
		c.n += int64(n)
	}
	return n, err
}

// A metadataReader is a CharsetReader's reader of one encoded word, under the
// no-progress rule: a read that returned (0, nil) while the reader took none of
// its own input counts, one that delivered output or took input resets the
// count. It implements io.Reader alone, so io.Copy cannot go around it through
// a WriterTo of the hook's reader. It also ends the copy once the parse is seen
// cancelled, before and after each read, without asking the hook.
type metadataReader struct {
	r     io.Reader
	in    *countedInput
	val   *valueDecoder
	empty int
}

func (m *metadataReader) Read(p []byte) (int, error) {
	if m.val.stalled {
		return 0, errMetadataStalled
	}
	if len(p) == 0 {
		return 0, nil
	}
	if m.val.poll != nil && m.val.poll() {
		return 0, errMetadataCancelled
	}
	before := m.in.n
	n, err := m.r.Read(p)
	switch {
	case n > 0 && n <= len(p):
		m.empty = 0
	case n == 0 && err == nil:
		if m.in.n != before {
			m.empty = 0
			break
		}
		m.empty++
		if m.empty >= maxConsecutiveEmptyReads {
			m.val.stalled = true
			return 0, errMetadataStalled
		}
	}
	if m.val.poll != nil && m.val.poll() {
		return n, errMetadataCancelled
	}
	return n, err
}

// metadataNoProgressDiagnostic is the one diagnostic owed by a field any of
// whose parameter values stalled an encoded-word charset decoder.
func metadataNoProgressDiagnostic(field string) Diagnostic {
	return Diagnostic{
		Code:     CodeHeaderDecoderNoProgress,
		Severity: SeverityInvalidMetadata,
		Field:    field,
		Message:  fmt.Sprintf("%s: an encoded-word charset decoder returned no bytes and no error on %d consecutive reads without taking input", field, maxConsecutiveEmptyReads),
		Action:   metadataNoProgressAction,
		Cause:    io.ErrNoProgress,
	}
}
