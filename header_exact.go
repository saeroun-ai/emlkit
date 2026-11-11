package message

import (
	"io"
	"strings"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Byte-exact headers under Parse (ROB-020-006, ROB-020-007).
//
// Every profile but ProfileStrict reads header blocks with
// textproto.HeaderLimits.Recover, which keeps every byte of a block: the line
// ending each line had, a line with an empty name (reported once per block as
// header.empty_name, and kept with the other damaged lines in
// Header.OrphanContinuations), and how the block ended. See textproto's
// header_exact.go. A header that has not been changed since is written back
// as those bytes:
//
//   - by textproto.WriteHeader, CreateWriter and Writer.CreatePart when its
//     block ended with an empty line ended by CRLF or LF;
//   - by Entity.WriteTo and textproto.WriteHeaderAsRead in every case — a
//     block the body followed with no empty line, one the input ended, one
//     whose empty line a bare CR ended — as long as, for WriteTo, the body
//     written after it is the one that was read.
//
// Header.Raw and HeaderFields.Raw keep ending every physical line with CRLF;
// Header.RawAsRead has the input's line endings. A field's 8-bit bytes are in
// both, and in Get; Text is the view that is always valid UTF-8.
//
// Read, ReadWithOptions, New and ProfileStrict are as they were.

// hpCreateWriter is CreateWriter. For e, the entity being written, whose body
// will be copied as it was read, the header is written as it was read too.
func hpCreateWriter(w io.Writer, header Header, e *Entity) (*Writer, error) {
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
	write := textproto.WriteHeader
	if e != nil && e.hpBodyAsRead(ww) {
		write = textproto.WriteHeaderAsRead
	}
	if err := write(w, header.Header); err != nil {
		return nil, err
	}
	return ww, nil
}

// hpBodyAsRead reports whether writeBodyTo will copy e's body to w exactly as
// it was read: the condition of its verbatim copy.
func (e *Entity) hpBodyAsRead(w *Writer) bool {
	if _, ok := e.Body.(*multipartBody); ok {
		return false
	}
	return e.originalBody != nil && sameReader(e.Body, e.decodedBody) &&
		strings.EqualFold(w.encoding, e.encoding) && strings.EqualFold(w.charset, e.charset)
}
