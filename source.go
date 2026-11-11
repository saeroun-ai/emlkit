package message

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

// A Source is the input of a parse kept readable at any offset, so that the
// untouched bytes of a message and of each of its parts can be read again —
// before, while and after their decoded form is read (ROB-110-001).
//
// There are two kinds, and they answer the same questions:
//
//   - NewSource wraps an input that can already be read at any offset: an
//     *os.File, a *bytes.Reader, a memory map. Nothing is copied.
//   - NewRecordedSource wraps a stream that can be read once. The bytes are
//     kept as the parse takes them from the stream, in memory or in a store
//     the caller supplies, up to a limit the caller chooses.
//
// ParseAt and ParseSource parse a Source; Entity.RawReader, RawHeader, RawBody
// and DecodedBody then read from it. See docs/robustness-notes/raw-source.md.
//
// A Source is safe for concurrent use: ReadAt and the readers the Entity
// methods return may be used from any number of goroutines, while the parse
// that records into the Source goes on in another.
type Source struct {
	// at and size are the input of a seekable Source; at is nil for a
	// recorded one.
	at   io.ReaderAt
	size int64

	// in is the stream of a recorded Source, limit how many of its bytes may
	// be kept (negative for no limit), and store where they are kept, nil
	// for memory. used is set by the one parse a recorded Source serves.
	in    io.Reader
	limit int64
	store RawStore
	used  atomic.Bool

	// mu guards what follows. It is held to write while a chunk is kept and
	// to read while a ReadAt copies, so a caller's store never sees a Write
	// overlap a ReadAt. It is never held while the stream is read.
	mu       sync.RWMutex
	buf      []byte
	kept     int64 // bytes of the input that are readable: its first kept bytes
	seen     int64 // bytes the stream has delivered
	ended    bool  // the stream returned an error, io.EOF included
	storeErr error
}

// A RawStore is where a recorded Source keeps the input: Write appends, and
// ReadAt reads back what was written, at the offsets of the input. An
// *os.File opened for reading and writing is one.
//
// The Source serializes the two: Write is never called while another Write or
// a ReadAt is running. ReadAt may be called from several goroutines at once,
// as io.ReaderAt allows. The Source neither closes nor removes the store.
type RawStore interface {
	io.Writer
	io.ReaderAt
}

// RecordOptions configures NewRecordedSource. A nil *RecordOptions is the
// zero value.
type RecordOptions struct {
	// MaxBytes bounds how many bytes of the input are kept: its first
	// MaxBytes bytes. Zero means DefaultRecordBytes; a negative value means
	// no limit, which the caller then answers for. The parse is not affected
	// by the limit: it reads the whole stream either way, and only the raw
	// reads of what lies past the limit fail, with ErrRawNotRecorded.
	MaxBytes int64

	// Store is where the bytes are kept. Nil keeps them in memory.
	Store RawStore
}

// DefaultRecordBytes is how many bytes of its input a recorded Source keeps
// when RecordOptions.MaxBytes is zero: 8 MiB.
const DefaultRecordBytes = 8 << 20

var (
	// ErrNoRawSource is returned by the raw reads of an entity whose parse
	// was given no Source: every entity but one from ParseAt or ParseSource.
	ErrNoRawSource = errors.New("message: the entity was not parsed from a Source")

	// ErrRawSpanUnknown is returned by the raw reads of an entity whose
	// bytes in the input are not known: one read out of bytes a transfer
	// decoder produced, a part of a body the caller replaced or of a
	// boundary the parser inferred, and — for a recorded Source — a part
	// whose end the parse has not reached yet.
	ErrRawSpanUnknown = errors.New("message: the entity's bytes in the input are not known")

	// ErrRawNotRecorded is returned by a raw read of bytes a recorded
	// Source did not keep: they lie past RecordOptions.MaxBytes, or past the
	// point its store failed at (see Source.Err).
	ErrRawNotRecorded = errors.New("message: the input was not recorded that far")

	// ErrSourceUsed is returned by ParseSource for a recorded Source that
	// was already parsed: its stream can be read once.
	ErrSourceUsed = errors.New("message: a recorded Source can be parsed once")
)

// NewSource returns a Source that reads the size bytes of r in place, with no
// copy. r must not change while the Source is in use. A Source of this kind
// may be parsed any number of times.
func NewSource(r io.ReaderAt, size int64) *Source {
	if size < 0 {
		size = 0
	}
	return &Source{at: r, size: size}
}

// NewRecordedSource returns a Source that keeps the bytes of r as a parse
// reads them, within opts. It reads nothing itself: what the parse has not
// taken from r is not in it, and a parse that stops early leaves the rest
// unread. It serves one parse.
func NewRecordedSource(r io.Reader, opts *RecordOptions) *Source {
	var o RecordOptions
	if opts != nil {
		o = *opts
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = DefaultRecordBytes
	}
	return &Source{in: r, limit: o.MaxBytes, store: o.Store}
}

// Size returns how many bytes of the input the Source can read back — its
// first n bytes — and whether that is the whole input. A seekable Source is
// always complete. A recorded one is complete once the parse has read its
// stream to the end and every byte was kept.
func (s *Source) Size() (n int64, complete bool) {
	if s.at != nil {
		return s.size, true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.kept, s.ended && s.kept == s.seen
}

// Truncated reports that a recorded Source did not keep bytes its stream
// delivered: the input is longer than RecordOptions.MaxBytes, or the store
// failed.
func (s *Source) Truncated() bool {
	if s.at != nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.seen > s.kept
}

// Err returns the error the RawStore of a recorded Source failed with, or
// nil. Such a failure ends the recording and not the parse.
func (s *Source) Err() error {
	if s.at != nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storeErr
}

// ReadAt reads the input at off, as io.ReaderAt describes. Past the end of
// the input it returns io.EOF; past what a recorded Source kept of an input
// that went on, ErrRawNotRecorded.
func (s *Source) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("message: negative offset")
	}
	if s.at != nil {
		if off >= s.size {
			return 0, io.EOF
		}
		short := false
		if rest := s.size - off; int64(len(p)) > rest {
			p, short = p[:rest], true
		}
		n, err := s.at.ReadAt(p, off)
		if err == nil && short {
			err = io.EOF
		}
		return n, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	end := io.EOF
	if s.seen > s.kept || !s.ended {
		end = ErrRawNotRecorded
	}
	if off >= s.kept {
		return 0, end
	}
	short := false
	if rest := s.kept - off; int64(len(p)) > rest {
		p, short = p[:rest], true
	}
	var n int
	var err error
	if s.store == nil {
		n = copy(p, s.buf[off:])
	} else {
		n, err = s.store.ReadAt(p, off)
		if n == len(p) {
			err = nil
		}
	}
	if err == nil && short {
		err = end
	}
	return n, err
}

// open returns the reader a parse of the Source reads.
func (s *Source) open() (io.Reader, error) {
	if s == nil {
		return nil, ErrNoRawSource
	}
	if s.at != nil {
		return io.NewSectionReader(s.at, 0, s.size), nil
	}
	if !s.used.CompareAndSwap(false, true) {
		return nil, ErrSourceUsed
	}
	return &srcTee{s: s}, nil
}

// section returns a reader of the n bytes of the input at off.
func (s *Source) section(off, n int64) *io.SectionReader {
	if s.at != nil {
		return io.NewSectionReader(s.at, off, n)
	}
	return io.NewSectionReader(s, off, n)
}

// A srcTee is the stream of a recorded Source as its parse reads it: what
// the stream returns, unchanged, with every byte offered to the record.
type srcTee struct{ s *Source }

func (t *srcTee) Read(p []byte) (int, error) {
	n, err := t.s.in.Read(p)
	if m := n; m > 0 {
		if m > len(p) {
			m = len(p)
		}
		t.s.note(p[:m], err)
	} else if err != nil {
		t.s.note(nil, err)
	}
	return n, err
}

// note keeps b, the next bytes of the stream, as far as the limit and the
// store allow, and notes the stream's end. Once one byte was not kept, none
// after it is: what the Source reads back is a prefix of the input.
func (s *Source) note(b []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	whole := s.kept == s.seen
	s.seen += int64(len(b))
	if err != nil {
		s.ended = true
	}
	if !whole || s.storeErr != nil || len(b) == 0 {
		return
	}
	if s.limit >= 0 {
		if room := s.limit - s.kept; int64(len(b)) > room {
			b = b[:room]
		}
	}
	if len(b) == 0 {
		return
	}
	if s.store != nil {
		n, werr := s.store.Write(b)
		if n < 0 {
			n = 0
		} else if n > len(b) {
			n = len(b)
		}
		s.kept += int64(n)
		if werr == nil && n < len(b) {
			werr = io.ErrShortWrite
		}
		s.storeErr = werr
		return
	}
	if need := len(s.buf) + len(b); need > cap(s.buf) {
		// Grown by doubling, and never past the limit.
		c := 2 * cap(s.buf)
		if c < need {
			c = need
		}
		if c < 4096 {
			c = 4096
		}
		if s.limit >= 0 && int64(c) > s.limit {
			c = int(s.limit)
		}
		grown := make([]byte, len(s.buf), c)
		copy(grown, s.buf)
		s.buf = grown
	}
	s.buf = append(s.buf, b...)
	s.kept += int64(len(b))
}

// ParseAt is Parse for an input that can be read at any offset: the size
// bytes of r. The parse is the same — the same tree, bodies, diagnostics and
// errors as Parse gives for those bytes read as a stream — and the session
// keeps r, so that every entity can also hand out its untouched bytes and
// its decoded body again: see Entity.RawReader, RawHeader, RawBody and
// DecodedBody. It is ParseSource of NewSource(r, size).
func ParseAt(r io.ReaderAt, size int64, opts *ParseOptions) (*ParseSession, *Entity, error) {
	return ParseSource(nil, NewSource(r, size), opts)
}

// ParseSource is ParseContext for a Source. ctx may be nil, which cancels
// nothing. A seekable Source is read in place; a recorded one is read as the
// stream it wraps and keeps what the parse reads. Everything Parse and
// ParseContext document holds: the entities are the one-shot streaming
// entities they always were, and the Source is what makes their bytes
// readable again beside them.
//
// A nil Source returns ErrNoRawSource and a recorded Source already parsed
// ErrSourceUsed, each with a session, no entity, and nothing read.
func ParseSource(ctx context.Context, src *Source, opts *ParseOptions) (*ParseSession, *Entity, error) {
	var o ParseOptions
	if opts != nil {
		o = *opts
	}
	r, err := src.open()
	if err != nil {
		return newParseSessionWithOptions(o.Profile, o.Compatibility, diagnosticOptions{
			sink:        o.DiagnosticSink,
			noRetention: o.DiscardDeliveredDiagnostics,
		}), nil, err
	}
	session, e, perr := parse(ctx, r, &o)
	session.raw = &srcBinding{src: src, ctx: ctx, opts: o}
	return session, e, perr
}

// Source returns the Source this parse reads, or nil for a parse that was
// given none, which is every parse but one from ParseAt or ParseSource.
func (s *ParseSession) Source() *Source {
	if s == nil || s.raw == nil {
		return nil
	}
	return s.raw.src
}
