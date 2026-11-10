package message

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Writer writes message entities.
//
// If the message is not multipart, it should be used as a WriteCloser. Don't
// forget to call Close.
//
// If the message is multipart, users can either use CreatePart to write child
// parts or Write to directly pipe a multipart message. In any case, Close must
// be called at the end.
type Writer struct {
	w  io.Writer
	c  io.Closer
	mw *textproto.MultipartWriter

	// rawWriter is the destination before the transfer-encoding wrapper is
	// applied; encoding and charset record what that wrapper encodes to.
	// writeBodyTo uses these to copy a body through verbatim when the source
	// and destination encoding/charset match.
	rawWriter io.Writer
	encoding  string
	charset   string
}

// createWriter creates a new Writer writing to w with the provided header.
// Nothing is written to w when it is called. header is modified in-place.
//
// A Content-Type that does not parse is refused before header is touched. A
// well-formed one may still be refused afterwards, as before, for a reason
// found later — an unhandled transfer encoding or charset — and by then header
// may already carry a generated boundary or have lost its
// Content-Transfer-Encoding: this function is not transactional. Its public
// callers pass it a copy, so a caller's header is never changed either way.
//
// A Content-Type that does not parse is refused (ROB-120-008, issue #58). The
// writer decides from that field whether it is writing a multipart body, which
// boundary frames it, and which charset its text has, and an unparseable value
// answers none of those: reading the raw value as the media type would treat
// "MULTIPART/MIXED; boundary=B; broken" as some other type and
// "multipart/mixed; boundary=B; broken" as a multipart with no parameters at
// all. So nothing is guessed, normalized or generated for it — no media type,
// no parameter map, no boundary — and the error says why. A missing
// Content-Type is not malformed; it is text/plain, as Header.ContentType says.
//
// Reading a damaged field is a different job, done by Parse, which may repair
// it for interpretation while keeping the raw header unchanged; such an entity
// therefore cannot be written back through this writer. See Entity.WriteTo.
//
// A header whose block began with a UTF-16 or UTF-32 byte order mark is
// refused first (ROB-010-003): that block was never read, its bytes are the
// entity's body, and neither writing fields for it nor writing the mark before
// them would represent it. See bom.go.
func createWriter(w io.Writer, header *Header) (*Writer, error) {
	if m := header.ByteOrderMark(); wideMark(m) {
		return nil, errWideHeaderWrite(m)
	}
	ww := &Writer{w: w, rawWriter: w}

	mediaType, mediaParams, err := header.ContentType()
	if err != nil {
		return nil, fmt.Errorf("message: cannot write an entity whose Content-Type %q does not parse: %w",
			header.Get("Content-Type"), err)
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		ww.mw = textproto.NewMultipartWriter(ww.w)

		// Do not set ww's io.Closer for now: if this is a multipart entity but
		// CreatePart is not used (only Write is used), then the final boundary
		// is expected to be written by the user too. In this case, ww.Close
		// shouldn't write the final boundary.

		// boundary가 비어 있거나, RFC 2046을 위반해 SetBoundary가 실패하면
		// MultipartWriter는 랜덤 boundary로 본문 구분선을 쓴다. 그 경우 실제로
		// 사용되는 boundary로 헤더를 동기화하지 않으면 헤더의 boundary와 본문
		// 구분선이 어긋나 메시지 구조 전체가 깨진다.
		if boundary := mediaParams["boundary"]; boundary == "" || ww.mw.SetBoundary(boundary) != nil {
			mediaParams["boundary"] = ww.mw.Boundary()
			header.SetContentType(mediaType, mediaParams)
		}

		header.Del("Content-Transfer-Encoding")
	} else {
		ww.encoding = header.Get("Content-Transfer-Encoding")
		wc, err := encodingWriter(ww.encoding, ww.w)
		if err != nil {
			return nil, err
		}
		ww.w = wc
		ww.c = wc
	}

	ww.charset = mediaParams["charset"]
	switch strings.ToLower(ww.charset) {
	case "", "us-ascii", "utf-8":
		// This is OK
	default:
		// Anything else is invalid
		return nil, fmt.Errorf("unhandled charset %q", ww.charset)
	}

	return ww, nil
}

// CreateWriter creates a new message writer to w. If header contains an
// encoding, data written to the Writer will automatically be encoded with it.
// The charset needs to be utf-8 or us-ascii.
//
// A Content-Type that does not parse is refused with an error wrapping the
// parse error, and nothing is written to w; the header is neither normalized
// nor given a generated boundary. The caller's header is never modified.
//
// A header read from a block that began with a byte order mark (ROB-010-003;
// see textproto.Header.ByteOrderMark) keeps it in the copy written: a UTF-8
// mark is written once, before every field, the added MIME-Version included.
// A UTF-16 or UTF-32 mark means the block was not read, and such a header is
// refused before anything is written, as CreatePart refuses one for a part.
//
// A header read from a block that began with a malformed prefix (ROB-020-001;
// see textproto.Header.MalformedPrefix) keeps it in the copy written: it is
// written right after any UTF-8 mark and before every field, the added
// MIME-Version included, on the terms of textproto.WriteHeader. It declares
// nothing, so it decides nothing about what is written. Orphan records
// (ROB-020-002; see textproto.Header.OrphanContinuations) are kept the same
// way and written in their place among the fields read with them, after the
// added MIME-Version; they declare nothing either.
func CreateWriter(w io.Writer, header Header) (*Writer, error) {
	// Ensure that modifications are invisible to the caller
	header = header.Copy()

	// If the message uses MIME, it has to include MIME-Version
	if !header.Has("Mime-Version") {
		header.Set("MIME-Version", "1.0")
	}

	ww, err := createWriter(w, &header)
	if err != nil {
		return nil, err
	}
	if err := textproto.WriteHeader(w, header.Header); err != nil {
		return nil, err
	}
	return ww, nil
}

// Write implements io.Writer.
func (w *Writer) Write(b []byte) (int, error) {
	return w.w.Write(b)
}

// Close implements io.Closer.
func (w *Writer) Close() error {
	if w.c != nil {
		return w.c.Close()
	}
	return nil
}

// CreatePart returns a Writer to a new part in this multipart entity. If this
// entity is not multipart, it fails. The body of the part should be written to
// the returned io.WriteCloser.
//
// A part whose Content-Type does not parse is refused, as CreateWriter refuses
// one, before anything of it is written: the parts already written stand, the
// writer is left as it was, and a later CreatePart may still succeed.
func (w *Writer) CreatePart(header Header) (*Writer, error) {
	var b bytes.Buffer
	return w.CreatePartWithBuffer(header, &b)
}

// CreatePartWithBuffer behaves like CreatePart but lets the caller supply the
// buffer used to assemble the part, avoiding a per-part allocation when the
// same buffer is reused across many parts. See
// textproto.MultipartWriter.CreatePartWithBuffer for the buffer reuse semantics.
func (w *Writer) CreatePartWithBuffer(header Header, buffer *bytes.Buffer) (*Writer, error) {
	if w.mw == nil {
		return nil, errors.New("cannot create a part in a non-multipart message")
	}

	// cw -> ww -> pw -> w.mw -> w.w

	ww := &struct{ io.Writer }{nil}

	// ensure that modifications are invisible to the caller
	header = header.Copy()

	// A header this writer cannot interpret — see createWriter — is refused
	// before anything about this part is written or decided: no delimiter, no
	// header, nothing in buffer, and the writer is left as it was, so the
	// parts already written stand and the next CreatePart may still succeed.
	cw, err := createWriter(ww, &header)
	if err != nil {
		return nil, err
	}

	if w.c == nil {
		// We know that the user calls CreatePart so Close should write the final
		// boundary
		w.c = w.mw
	}

	pw, err := w.mw.CreatePartWithBuffer(header.Header, buffer)
	if err != nil {
		return nil, err
	}

	ww.Writer = pw
	return cw, nil
}
