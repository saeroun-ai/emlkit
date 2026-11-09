package message

import (
	"fmt"
	"io"
	"mime"
	"strings"

	"github.com/saeroun-ai/emlkit/internal/errgraph"
)

type UnknownCharsetError struct {
	e error
}

func (u UnknownCharsetError) Unwrap() error { return u.e }

func (u UnknownCharsetError) Error() string {
	return "unknown charset: " + u.e.Error()
}

// IsUnknownCharset returns a boolean indicating whether the error is known to
// report that the charset advertised by the entity is unknown: whether an
// UnknownCharsetError value with a cause is err, or is reachable from it.
//
// It walks err's error graph exactly as IsFatal does, with the same bounds and
// the same guarantees: Unwrap() error and Unwrap() []error are followed, so a
// warning wrapped by fmt.Errorf with %w, joined by errors.Join, held by a
// custom Unwrap or carried by a FatalError is found; no Is, As or Error method
// is called; and a warning deeper or wider than the walk's bounds is reported
// as absent.
//
// Being a search of the whole graph, it answers whether a warning is anywhere
// in err, which is not the question the parser asks. The parser treats an
// error as a warning about a part only when the error it returned with that
// part is itself one (see Entity.Walk and mail.Reader.NextPart), and the
// warning's own cause may hold anything a CharsetReader failed with, a
// FatalError included.
//
// The match is the value type UnknownCharsetError, as before; a
// *UnknownCharsetError is not one. Every warning the parser reports carries the
// error that explains it, so the zero UnknownCharsetError{} is not a warning
// and does not match. The cause is unexported, so a composite literal in
// another package can only build that zero value; a genuine warning the
// parser returned can still be copied and handed on like any value, and is
// still matched. Before ROB-120-007 this was errors.As, which matched the zero
// value, called As methods and followed Unwrap without a bound.
func IsUnknownCharset(err error) bool {
	return errgraph.Find(err, isCharsetWarning)
}

// isCharsetWarning reports whether err is itself a charset warning with a
// cause. It is a type assertion and a nil check, so it calls nothing on err.
func isCharsetWarning(err error) bool {
	u, ok := err.(UnknownCharsetError)
	return ok && u.e != nil
}

// CharsetReader, if non-nil, defines a function to generate charset-conversion
// readers, converting from the provided charset into UTF-8. Charsets are always
// lower-case. utf-8 and us-ascii charsets are handled by default. One of the
// the CharsetReader's result values must be non-nil.
//
// Importing github.com/saeroun-ai/emlkit/charset will set CharsetReader to
// a function that handles most common charsets. Alternatively, CharsetReader
// can be set to e.g. golang.org/x/net/html/charset.NewReaderLabel.
var CharsetReader func(charset string, input io.Reader) (io.Reader, error)

// charsetReader calls CharsetReader if non-nil.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	charset = strings.ToLower(charset)
	if charset == "" || charset == "utf-8" || charset == "us-ascii" {
		// An empty charset means unspecified, i.e. effectively us-ascii.
		return input, nil
	}
	if CharsetReader != nil {
		r, err := CharsetReader(charset, input)
		if err != nil {
			return r, UnknownCharsetError{err}
		}
		return r, nil
	}
	return input, UnknownCharsetError{fmt.Errorf("message: unhandled charset %q", charset)}
}

// decodeHeader decodes an internationalized header field. If it fails, it
// returns the input string and the error.
func decodeHeader(s string) (string, error) {
	wordDecoder := mime.WordDecoder{CharsetReader: charsetReader}
	dec, err := wordDecoder.DecodeHeader(s)
	if err != nil {
		return s, err
	}
	return dec, nil
}

func encodeHeader(s string) string {
	return mime.QEncoding.Encode("utf-8", s)
}
