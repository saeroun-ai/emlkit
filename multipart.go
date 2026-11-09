package message

import (
	"io"

	"github.com/saeroun-ai/emlkit/textproto"
)

// MultipartReader is an iterator over parts in a MIME multipart body.
type MultipartReader interface {
	io.Closer

	// NextPart returns the next part in the multipart or an error. When there are
	// no more parts, the error io.EOF is returned.
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

	p, err := r.r.NextPart()
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

		// A part header past one of its budgets, or a framing line past the
		// line budget, is a defect of the message, so it is recorded and
		// reported as one. Either ends this iterator: the reader is standing
		// inside a part whose extent it never learned, or on a line it could not
		// finish, and there is nothing safe to resume from.
		//
		// textproto.HeaderLimit and textproto.LineLimit are what decide that,
		// rather than the shape of the error: a value shaped like a refusal is
		// something the message's own bytes can put here, and one of the shapes
		// it can put here is a typed nil that would be dereferenced below.
		if limit, refused := textproto.HeaderLimit(err); refused {
			return nil, r.headerLimitFailure(limit)
		}
		if limit, refused := textproto.LineLimit(err); refused {
			return nil, r.lineLimitFailure(limit)
		}
		return nil, err
	}

	// The part reports its position within the multipart body; child turns
	// that into a position in the original input, extends the parent's path
	// with this part's index, carries the parent's session down to it, and
	// counts it one level deeper than the entity whose body this is.
	ctx := r.parent.child(r.next, p.HeaderOffset(), p.BodyOffset())

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

	r *io.PipeReader
	w *Writer

	// err is the sticky failure of setting the body up: the writer for this
	// header could not be created, so no worker was started and no pipe was
	// published. Every Read reports it again.
	err error

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
// the writer refuses — closes the pipe with that error, which every later Read
// then reports, and the worker exits.
func (m *multipartBody) Read(p []byte) (n int, err error) {
	if m.err != nil {
		return 0, m.err
	}
	if m.r == nil {
		r, w := io.Pipe()

		mw, err := createWriter(w, &m.header)
		if err != nil {
			r.Close()
			w.Close()
			m.err = err
			return 0, err
		}
		m.r, m.w = r, mw

		// Prevent calls to NextPart to succeed
		m.i = len(m.parts)

		go func() {
			if err := m.writeBodyTo(m.w); err != nil {
				w.CloseWithError(err)
				return
			}

			if err := m.w.Close(); err != nil {
				w.CloseWithError(err)
				return
			}

			w.Close()
		}()
	}

	return m.r.Read(p)
}

// Close implements io.Closer.
func (m *multipartBody) Close() error {
	if m.r != nil {
		m.r.Close()
	}
	return nil
}

// NextPart implements MultipartReader.
func (m *multipartBody) NextPart() (*Entity, error) {
	if m.i >= len(m.parts) {
		return nil, io.EOF
	}

	part := m.parts[m.i]
	m.i++
	return part, nil
}

func (m *multipartBody) writeBodyTo(w *Writer) error {
	for _, p := range m.parts {
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
