package message

import (
	"fmt"
	"io"
	"sync"

	"github.com/saeroun-ai/emlkit/textproto"
)

// MultipartReader is an iterator over parts in a MIME multipart body.
type MultipartReader interface {
	io.Closer

	// NextPart returns the next part in the multipart or an error. When there are
	// no more parts, the error io.EOF is returned.
	//
	// A parsed multipart body framed by a usable boundary but with no bytes at
	// all — the body of a header-only multipart message such as
	// "Content-Type: multipart/mixed; boundary=B\r\n" — holds no delimiter and
	// so no part: the first call
	// returns no part and an error wrapping io.EOF, "multipart: NextPart: EOF",
	// and every later call io.EOF (ROB-010-001 pins this; how an empty container
	// is treated is ROB-060-009's).
	//
	// Entity.Body must be read completely before the next call to NextPart,
	// otherwise it will be discarded.
	NextPart() (*Entity, error)
}

type multipartReader struct {
	r                          *textproto.MultipartReader
	skipTextAttachmentDecoding bool

	// parent is the parse context of the entity whose body is being iterated:
	// the session the parts belong to, and the origin they extend. The origin
	// is the zero value when that body is not known to be the parsed input's
	// bytes; the session travels down either way.
	parent parseContext

	// next is the zero-based index, within this multipart, of the part the
	// next successful NextPart will return.
	next int

	// fatal is the sticky failure of this iterator, or nil while it may
	// continue. Its scope is this multipart and nothing wider: the parts
	// already returned stay valid, and so does the entity whose body this is.
	// It is returned unchanged by every later call, so that a caller that asks
	// again is told the same thing rather than handed a second interpretation
	// of an input the reader can no longer place itself in.
	fatal error

	// ends is the delimiter line endings of r already looked at; see
	// delimiterEndings in line_endings.go.
	ends textproto.LineEndings

	// Multipart recovery; see multipart_heal.go. healing reports that this
	// iterator heals its framing; declared is the boundary the entity
	// declared, empty for none; inferred that r was replaced by a reader of
	// the kept body framed on a boundary found in it; endNoted that the end of
	// the iteration has been looked at; reused that the declared boundary is
	// one of an enclosing multipart's, to be reported by the first NextPart.
	healing  bool
	declared string
	inferred bool
	endNoted bool
	reused   bool

	// digest reports that the entity is a multipart/digest, whose parts are
	// message/rfc822 unless they say otherwise.
	digest bool
}

// NextPart implements MultipartReader.
func (r *multipartReader) NextPart() (*Entity, error) {
	// Iterating to the next part is a scope of this parse, so it does not begin
	// on a session that has already been stopped. Nothing is recovered here:
	// the parts already returned stay valid, and this iterator ends with the
	// same sticky error every other scope of the parse reports.
	if err := r.parent.fatalErr(); err != nil {
		return nil, err
	}

	// This iterator's own failure is sticky for the same reason and on the same
	// terms, but its scope is this multipart alone. It is checked after the
	// session's, because a stop the caller asked for describes more of the
	// parse than a defect of one part does.
	if r.fatal != nil {
		return nil, r.fatal
	}

	if r.reused {
		r.reused = false
		if fatal := r.noteFraming(CodeMultipartNestedBoundaryReused, SeverityRecovered,
			fmt.Sprintf("multipart declares the boundary %q of a multipart it is nested in", r.declared), boundaryReusedAction); fatal != nil {
			return nil, fatal
		}
	}

	p, err := r.r.NextPart()

	// The delimiter lines the reader proved since this iterator last looked —
	// while a part's Body was read, or by this call — are looked at after
	// every call, the closing delimiter's included, unless the session has
	// stopped or this call ran out of the parse's shared header budget, which
	// stops it below. A call that framed a part or ended with io.EOF is refused
	// for them at once. One that failed otherwise reports that failure as it
	// always did; a Strict refusal they owe is kept as this iterator's sticky
	// failure, which the next call returns, unless the failure is a budget
	// refusal of this iterator's own, which then takes its place. A stop of
	// the session a diagnostic here caused is returned first either way.
	framed := err == nil || err == io.EOF
	_, totalRefused := r.parent.session.headerTotalRefusal(err)
	if (framed || r.parent.fatalErr() == nil) && !totalRefused {
		if lerr := r.delimiterEndings(framed); lerr != nil {
			return nil, lerr
		}
	}

	if err != nil {
		// The input budget is counted at the source this iterator reads, so it
		// can end the whole parse in the middle of this call — while a part the
		// caller left unread is drained, a framing line is read, or a part
		// header is — and the reader below then fails with that stop, wrapped
		// in whatever it says about the read it could not finish. The stop is
		// the session's and outranks this iterator's own failures, so it is
		// returned first and as the identical value every scope reports, and no
		// header or line budget is diagnosed for a read the stop cut short.
		if stop := r.parent.fatalErr(); stop != nil {
			return nil, stop
		}

		// The parse's shared header budget ran out on this part's block. That
		// is the whole parse's failure, so it is published rather than kept as
		// this iterator's; see limit_header_total.go. It is recognized by
		// identity before anything below walks err.
		if ref, ok := r.parent.session.headerTotalRefusal(err); ok {
			ctx := r.parent.child(r.next, ref.Offset(), ref.Offset())
			r.fatal = r.parent.session.headerTotalFailure(ctx, ref)
			return nil, r.fatal
		}

		// A part header past one of its budgets, or a framing line past the
		// line budget, is a defect of the message, so it is recorded and
		// reported as one. Either ends this iterator: the reader is standing
		// inside a part whose extent it never learned, or on a line it could not
		// finish, and there is nothing safe to resume from.
		//
		// textproto returns its own refusal as itself, never wrapped, so only
		// err's own dynamic type is asked about, and nothing err wraps — a
		// read failure of the caller's source, with whatever methods it has —
		// is unwrapped to look further. textproto.HeaderLimit and
		// textproto.LineLimit then decide whether that value is genuine rather
		// than shaped like one: a reader's error can be a forged refusal, or a
		// typed nil that would be dereferenced below. Given a value of their
		// own type they call no method of anything but that type.
		//
		// A genuine refusal made elsewhere and handed back by the source is
		// the one value this cannot tell from the iterator's own: nothing in
		// the type system tells a value from itself.
		if e, ok := err.(*textproto.HeaderLimitError); ok {
			if limit, refused := textproto.HeaderLimit(e); refused && !isTotalHeaderKind(limit.Kind()) {
				return nil, r.headerLimitFailure(limit)
			}
		}
		if e, ok := err.(*textproto.LineLimitError); ok {
			if limit, refused := textproto.LineLimit(e); refused {
				return nil, r.lineLimitFailure(limit)
			}
		}
		// The iteration of the message's own multipart body that ended at a
		// proven closing delimiter makes the input after it available, under
		// ProfileForensic, to an explicit inspection. Nothing is read here;
		// see trailing.go.
		if err == io.EOF {
			// Under recovery the end of the framing is looked at once: what it
			// lacked is recorded, and a body it could not frame is returned
			// as a part or framed again on the boundary found in it. See
			// multipart_heal.go.
			if r.healing && !r.endNoted {
				r.endNoted = true
				part, herr, again := r.healEnd()
				switch {
				case again:
					return r.NextPart()
				case part != nil || herr != nil:
					return part, herr
				}
			}
			r.noteTrailing()
		}
		return nil, err
	}

	// The part reports its position within the multipart body; child turns
	// that into a position in the original input, extends the parent's path
	// with this part's index, carries the parent's session down to it, and
	// counts it one level deeper than the entity whose body this is.
	ctx := r.parent.child(r.next, p.HeaderOffset(), p.BodyOffset())
	// A part whose header began with a malformed prefix, or held an orphan
	// record, and was ended by the next delimiter line is diagnosed when it is
	// built; see firstline.go and orphan.go.
	ctx.separatorMissing = p.HeaderSeparatorMissing()
	ctx.digestPart = r.digest

	// The budgets on the shape of the tree are charged here, where a child has
	// been framed and nothing but its header block has been read yet. A refused
	// child is not returned and nothing further about it is touched: no body,
	// no decoder, no entity, and none of its own parts. See limit_tree.go for
	// why the check waits until a child is known to exist, and for the
	// difference between the two failures.
	if err := r.admitPart(ctx); err != nil {
		return nil, err
	}

	r.next++

	return newWithOptions(Header{p.Header}, p, r.skipTextAttachmentDecoding, ctx)
}

// Close implements io.Closer.
func (r *multipartReader) Close() error {
	return nil
}

type multipartBody struct {
	header Header
	parts  []*Entity

	// mu guards the lifecycle below — reserving the setup, publishing the pipe,
	// closing, and the iteration index — and nothing else. It is never held
	// while foreign code can run: not across createWriter, whose interpretation
	// of the header can call the configured CharsetReader, nor while the pipe
	// is read or written, nor while a part's body is read. So a Close — from
	// another goroutine, or from inside such a hook — never waits for the Read
	// it closes (ROB-001-006).
	mu sync.Mutex

	r *io.PipeReader

	// err is the sticky failure of setting the body up: the writer for this
	// header could not be created, so no worker was started and no pipe was
	// published. It is only published while the body is not closed, and every
	// Read reports it again from then on, Close or not.
	err error

	// closed is set by Close, once and for good. A body closed before its
	// setup's outcome was published never publishes one — no pipe, no worker,
	// no setup failure; see Close.
	closed bool

	// setup and closing are made together, by the Read that reserves the
	// setup, and are nil before it. setup is closed once that Read has
	// published its outcome, or found the body closed and published nothing;
	// closing is closed by the first Close. A concurrent Read waits for
	// whichever comes first, so a Close wakes it without waiting for a
	// CharsetReader the setup may be blocked in. Each is closed exactly once,
	// under mu, and neither is ever reset.
	setup   chan struct{}
	closing chan struct{}

	i int
}

// Read implements io.Reader.
//
// The body is produced by a worker goroutine writing the parts into a pipe.
// The pipe is published only once the writer for the header exists: if that
// fails — a Content-Type the writer refuses (ROB-120-008), an unhandled charset
// — the error is kept and returned by this and every later Read, rather than
// leaving a pipe that no worker will ever write to, on which a second Read
// would block for ever. A failure while the worker writes the parts — a part
// the writer refuses — closes the pipe with that error, which later Reads
// then report until the body is closed, and the worker exits.
//
// The writer is created with no lock held, because interpreting the header can
// call the configured CharsetReader, which may block or call Close. The first
// terminal state published wins, and stays: a setup failure published before
// any Close is reported by every later Read, Close or not; a Close that comes
// before the setup's outcome is published wins over that outcome, whether the
// writer was created or failed — it is discarded, no pipe, worker or failure is
// published, and this Read and every later one return io.ErrClosedPipe. A Read
// made meanwhile by another goroutine waits for the outcome, or for a Close,
// whichever comes first: a Close wakes it at once with io.ErrClosedPipe, even
// while the setting-up Read is still inside a CharsetReader, which cannot be
// interrupted and returns only when the hook does. A CharsetReader that reads
// this same body from inside that hook is not supported, as reading a body
// from inside its own setup never was.
//
// The first Read ends the iteration NextPart offers, as it always has.
func (m *multipartBody) Read(p []byte) (n int, err error) {
	m.mu.Lock()
	for {
		if m.err != nil {
			err := m.err
			m.mu.Unlock()
			return 0, err
		}
		if r := m.r; r != nil {
			m.mu.Unlock()
			return r.Read(p)
		}
		if m.closed {
			m.mu.Unlock()
			return 0, io.ErrClosedPipe
		}
		setup := m.setup
		if setup == nil {
			break
		}
		// Another Read is setting the body up. Its outcome is published before
		// setup is closed, and Close sets m.closed before it closes the closing
		// channel, so the next pass finds whichever woke this one. No goroutine
		// is started for the wait.
		closing := m.closing
		m.mu.Unlock()
		select {
		case <-setup:
		case <-closing:
		}
		m.mu.Lock()
	}

	// This Read reserves the setup, and m is not touched while the lock is
	// released.
	//
	// The writer is given a struct copy of the header, as it was always given
	// m.header itself. A textproto.Header holds its fields in a map and a slice
	// that such a copy shares — with m.header and with the entity's Header,
	// which NewMultipart built from the same value, and with the caller's value
	// it was given — so the one edit createWriter can make, filling in a
	// boundary for a header that has none or one the writer cannot use, reaches
	// those headers through the shared map and backing array, as it did before
	// this change. That is long-standing and is not changed here; CreateWriter
	// and CreatePart, which copy the header first, are the public writers.
	setup, closing := make(chan struct{}), make(chan struct{})
	m.setup, m.closing = setup, closing
	header := m.header
	m.mu.Unlock()

	pr, pw := io.Pipe()
	mw, err := createWriter(pw, &header)

	// The outcome is published, and setup closed, under mu, so a waiting Read
	// finds it; setup is closed before this Read goes on to read the pipe, so
	// nothing waits behind that. A Close that came first wins over the
	// outcome, a failure included: nothing is published.
	m.mu.Lock()
	switch {
	case m.closed:
		close(setup)
		m.mu.Unlock()
		pr.Close()
		pw.Close()
		return 0, io.ErrClosedPipe
	case err != nil:
		m.err = err
		close(setup)
		m.mu.Unlock()
		pr.Close()
		pw.Close()
		return 0, err
	}
	m.r = pr

	// Prevent calls to NextPart to succeed
	m.i = len(m.parts)

	// The worker is given what it writes rather than reading it from m, and it
	// never takes mu: the parts are fixed at construction.
	parts := m.parts
	go func() {
		if err := writeParts(mw, parts); err != nil {
			pw.CloseWithError(err)
			return
		}

		if err := mw.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}

		pw.Close()
	}()
	close(setup)
	m.mu.Unlock()

	return pr.Read(p)
}

// Close implements io.Closer. It ends both what this body offers: the
// iteration over its parts, which then reports io.EOF from NextPart — the parts
// already returned stay the caller's — and Body, which then returns
// io.ErrClosedPipe from every Read, as the pipe it reads does once closed. The
// first terminal state published wins: a body whose setup failure was
// published before the Close keeps returning that failure, as it always has,
// while a Close that comes before the setup's outcome is published wins over
// it, a failure included.
//
// Close never waits for a Read. A body closed before its first Read, or while
// its first Read is still creating the writer — inside a CharsetReader the
// header's interpretation called, for example — never publishes a pipe, a
// worker or a setup failure, and a Read waiting for that setup returns
// io.ErrClosedPipe at once; the Read inside the CharsetReader returns it once
// the hook returns. A body closed while its worker is writing makes the
// worker's next write to the pipe fail, and the worker then exits: that is how
// a caller that stops reading early releases it, and a Close from a
// context.AfterFunc of the caller's own is safe, since Close may run
// concurrently with Read. What Close cannot do is make a part's own body
// return: a worker blocked reading a part whose source is blocked exits only
// once that read returns, so such a source needs its owner's cooperation. Close
// does not close the parts or their sources, which stay the caller's. A body
// abandoned partway without Close keeps its worker waiting on the pipe, holding
// the parts, for as long as the process runs.
//
// WriteTo writes the parts itself, without the pipe, and is not affected by
// Close. Close is idempotent and returns nil.
func (m *multipartBody) Close() error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		if m.closing != nil {
			close(m.closing)
		}
	}
	m.i = len(m.parts)
	r := m.r
	m.mu.Unlock()

	if r != nil {
		r.Close()
	}
	return nil
}

// NextPart implements MultipartReader.
func (m *multipartBody) NextPart() (*Entity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.i >= len(m.parts) {
		return nil, io.EOF
	}

	part := m.parts[m.i]
	m.i++
	return part, nil
}

func (m *multipartBody) writeBodyTo(w *Writer) error {
	return writeParts(w, m.parts)
}

// writeParts writes parts into w, one CreatePart each.
//
// A part from ParseContext whose parse has been cancelled is not started: its
// header is not written, and the parse's stop is returned. A part already
// being written stops at the next read of its body, as any read of it does.
func writeParts(w *Writer, parts []*Entity) error {
	for _, p := range parts {
		if stop := p.parse.session.cancelErr(); stop != nil {
			return stop
		}
		pw, err := w.CreatePart(p.Header)
		if err != nil {
			return err
		}

		if err := p.writeBodyTo(pw); err != nil {
			return err
		}
		if err := pw.Close(); err != nil {
			return err
		}
	}
	return nil
}
