package message

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime/quotedprintable"
	"strings"

	"github.com/saeroun-ai/emlkit/internal/errgraph"
)

type UnknownEncodingError struct {
	e error
}

func (u UnknownEncodingError) Unwrap() error { return u.e }

func (u UnknownEncodingError) Error() string {
	return "encoding error: " + u.e.Error()
}

// IsUnknownEncoding returns a boolean indicating whether the error is known to
// report that the encoding advertised by the entity is unknown: whether an
// UnknownEncodingError value with a cause is err, or is reachable from it.
//
// It walks and matches on the terms IsUnknownCharset describes, with the same
// bounds: the value type UnknownEncodingError with a cause, found through
// wrapping and joins, without calling an Is, As or Error method. The zero
// UnknownEncodingError{}, the only value a composite literal in another
// package can build, does not match. Like IsUnknownCharset it searches the
// whole graph, which the parser's own warning decision does not.
func IsUnknownEncoding(err error) bool {
	return errgraph.Find(err, isEncodingWarning)
}

// isEncodingWarning reports whether err is itself a transfer-encoding warning
// with a cause. It is a type assertion and a nil check, so it calls nothing on
// err.
func isEncodingWarning(err error) bool {
	u, ok := err.(UnknownEncodingError)
	return ok && u.e != nil
}

// isWarning reports whether err itself, not anything it wraps, is one of the
// two decoding warnings the parser returns together with a usable entity.
//
// It is what the parser's own control flow asks, because a warning the parser
// reports is always returned as itself: entity construction never wraps one.
// Asking only about err's own dynamic type means no Unwrap of anything an
// input or a caller's component supplied is ever called on the way.
func isWarning(err error) bool {
	return isEncodingWarning(err) || isCharsetWarning(err)
}

// normalizeEncodingName returns the transfer encoding name in the one form
// every switch on it uses.
//
// It exists so that the decoder chosen for a body and the budget applied to it
// can never disagree about which encoding was declared; see framedEncoding and
// canonicalCTE, whose repairs it applies.
func normalizeEncodingName(enc string) string {
	name, _ := canonicalCTE(enc)
	return name
}

// encodingReader returns the tolerant decoder of enc, a Content-Transfer-Encoding
// value, reading r, for a body that belongs to no parse; see
// parseContext.transferDecoder.
func encodingReader(enc string, r io.Reader) (io.Reader, error) {
	if dec, ok := (parseContext{}).transferDecoder(normalizeEncodingName(enc), r); ok {
		return dec, nil
	}
	return nil, unhandledEncoding(enc)
}

// unhandledEncoding is the cause of an UnknownEncodingError for enc.
func unhandledEncoding(enc string) error {
	return fmt.Errorf("unhandled encoding %q", enc)
}

// legacyEncodingReader returns the decoders used before the tolerant ones, and
// still under ProfileStrict: the standard library's Base64 decoder, which ends
// with its corruption error at a byte outside the alphabet, and its
// quoted-printable reader over lenientQPReader. name is a canonical name.
func legacyEncodingReader(name string, r io.Reader) (io.Reader, error) {
	var dec io.Reader
	switch name {
	case "quoted-printable":
		dec = quotedprintable.NewReader(&lenientQPReader{br: bufio.NewReader(r)})
	case "base64":
		wrapped := &whitespaceReplacingReader{wrapped: r}
		dec = base64.NewDecoder(base64.StdEncoding, wrapped)
	case "7bit", "8bit", "binary", "":
		dec = r
	default:
		return nil, unhandledEncoding(name)
	}
	return dec, nil
}

type nopCloser struct {
	io.Writer
}

func (nopCloser) Close() error {
	return nil
}

func encodingWriter(enc string, w io.Writer) (io.WriteCloser, error) {
	var wc io.WriteCloser
	switch strings.ToLower(enc) {
	case "quoted-printable":
		wc = quotedprintable.NewWriter(w)
	case "base64":
		wc = base64.NewEncoder(base64.StdEncoding, &lineWrapper{w: w, maxLineLen: 76})
	case "7bit", "8bit":
		wc = nopCloser{&lineWrapper{w: w, maxLineLen: 998}}
	case "binary", "":
		wc = nopCloser{w}
	default:
		return nil, fmt.Errorf("unhandled encoding %q", enc)
	}
	return wc, nil
}

// whitespaceReplacingReader replaces space and tab characters with a LF so
// base64 bodies with a continuation indent can be decoded by the base64 decoder
// even though it is against the spec.
type whitespaceReplacingReader struct {
	wrapped io.Reader
}

func (r *whitespaceReplacingReader) Read(p []byte) (int, error) {
	n, err := r.wrapped.Read(p)

	for i := 0; i < n; i++ {
		if p[i] == ' ' || p[i] == '\t' {
			p[i] = '\n'
		}
	}

	return n, err
}

// lenientQPReader normalises a quoted-printable byte stream so the standard
// quotedprintable.Reader does not reject malformed input seen in real-world
// mail, while passing conformant QP through byte-for-byte:
//   - a "=" not followed by two hex digits or CR/LF is escaped to "=3D"
//   - a raw control byte (other than TAB/CR/LF) is hex-escaped to "=XX"
//   - an over-long line is split with a soft line break so the decoder's
//     internal line buffer cannot overflow
type lenientQPReader struct {
	br      *bufio.Reader
	pending []byte // produced bytes not yet returned
	col     int    // current output line length, for soft-wrapping
}

// qpSoftWrapLen is below bufio's default 4096 buffer and the 998 QP line limit.
const qpSoftWrapLen = 990

func (r *lenientQPReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if err := r.produce(); err != nil {
			if len(r.pending) == 0 {
				return 0, err
			}
			break
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// produce reads one input byte and appends its (possibly normalised) output.
func (r *lenientQPReader) produce() error {
	b, err := r.br.ReadByte()
	if err != nil {
		return err
	}

	switch {
	case b == '\n':
		r.col = 0
		r.pending = append(r.pending, b)
	case b == '\r':
		r.pending = append(r.pending, b)
	case b == '=':
		next, _ := r.br.Peek(2)
		switch {
		case len(next) >= 1 && (next[0] == '\r' || next[0] == '\n'):
			// Soft line break: keep "=" as-is.
			r.pending = append(r.pending, b)
			r.col++
		case len(next) >= 2 && isHexByte(next[0]) && isHexByte(next[1]):
			// Valid "=XX": pass the whole escape through unchanged.
			r.softWrap()
			r.pending = append(r.pending, b, next[0], next[1])
			r.br.Discard(2)
			r.col += 3
		default:
			// Malformed "=": emit a literal "=3D".
			r.emitHex('=')
		}
	case b < ' ' && b != '\t':
		// Raw control byte: hex-escape it so the decoder accepts it.
		r.emitHex(b)
	default:
		r.softWrap()
		r.pending = append(r.pending, b)
		r.col++
	}
	return nil
}

func (r *lenientQPReader) emitHex(b byte) {
	const hex = "0123456789ABCDEF"
	r.softWrap()
	r.pending = append(r.pending, '=', hex[b>>4], hex[b&0x0f])
	r.col += 3
}

func (r *lenientQPReader) softWrap() {
	if r.col >= qpSoftWrapLen {
		r.pending = append(r.pending, '=', '\r', '\n')
		r.col = 0
	}
}

func isHexByte(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'A' && b <= 'F') || (b >= 'a' && b <= 'f')
}

type lineWrapper struct {
	w          io.Writer
	maxLineLen int

	curLineLen int
	cr         bool
}

func (w *lineWrapper) Write(b []byte) (int, error) {
	var written int
	for len(b) > 0 {
		var l []byte
		l, b = cutLine(b, w.maxLineLen-w.curLineLen)

		lf := bytes.HasSuffix(l, []byte("\n"))
		l = bytes.TrimSuffix(l, []byte("\n"))

		n, err := w.w.Write(l)
		if err != nil {
			return written, err
		}
		written += n

		cr := bytes.HasSuffix(l, []byte("\r"))
		if len(l) == 0 {
			cr = w.cr
		}

		if !lf && len(b) == 0 {
			w.curLineLen += len(l)
			w.cr = cr
			break
		}
		w.curLineLen = 0

		ending := []byte("\r\n")
		if cr {
			ending = []byte("\n")
		}
		_, err = w.w.Write(ending)
		if err != nil {
			return written, err
		}
		// If the written `\n` was part of the input bytes slice, then account for it.
		if lf {
			written++
		}
		w.cr = false
	}

	return written, nil
}

func cutLine(b []byte, max int) ([]byte, []byte) {
	for i := 0; i < len(b); i++ {
		if b[i] == '\r' && i == max {
			continue
		}
		if b[i] == '\n' {
			return b[:i+1], b[i+1:]
		}
		if i >= max {
			return b[:i], b[i:]
		}
	}
	return b, nil
}
