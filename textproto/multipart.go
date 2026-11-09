// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//

package textproto

// Multipart is defined in RFC 2046.

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
)

// ErrMissingBoundaryClose is returned (joined with io.ErrUnexpectedEOF) when
// the multipart input ends before the closing "--boundary--" delimiter was
// found. The body bytes accumulated up to EOF are still delivered.
var ErrMissingBoundaryClose = errors.New("multipart: missing closing boundary")

var emptyParams = make(map[string]string)

// This constant needs to be at least 76 for this package to work correctly.
// This is because \r\n--separator_of_len_70- would fill the buffer and it
// wouldn't be safe to consume a single byte from it.
const peekBufferSize = 4096

// A Part represents a single part in a multipart body.
type Part struct {
	Header Header

	mr *MultipartReader

	// r is either a reader directly reading from mr
	r io.Reader

	n       int   // known data bytes waiting in mr.bufReader
	total   int64 // total data bytes read already
	err     error // error to return when n == 0
	readErr error // read error observed from mr.bufReader

	headerOffset int64 // offset of the part's header block in mr's input
	bodyOffset   int64 // offset of the part's body in mr's input
}

// HeaderOffset returns the offset of the first byte of this part's header
// block, relative to the first byte of the exact input this part's
// MultipartReader was given.
//
// The reference point is that input and nothing wider. A multipart body is
// usually a slice of some larger message, and this offset says nothing about
// where that body sits in it; a caller that wants a position in the whole
// message adds the position of the body itself.
//
// It is the position of the part itself: the first byte after the boundary
// delimiter line that introduced it. A part with no header block at all — two
// delimiter lines in a row — starts its body there too, so HeaderOffset and
// BodyOffset are equal for it.
//
// The offset is always a real position. A Part exists only because the reader
// parsed it out of that input, so there is no state in which its position is
// unavailable and no returned value that means "unknown".
//
// The offset is the parser's own position, not how far ahead the reader has
// buffered, so it does not depend on how the input chunks its reads.
func (p *Part) HeaderOffset() int64 {
	return p.headerOffset
}

// BodyOffset returns the offset of the first byte of this part's body,
// relative to the first byte of the exact input this part's MultipartReader
// was given, on the same terms as HeaderOffset.
//
// The offset describes that input as it was given to the reader. Nothing at
// this layer decodes a transfer encoding, so it is the position of the part's
// undecoded bytes, whatever Content-Transfer-Encoding its header declares.
func (p *Part) BodyOffset() int64 {
	return p.bodyOffset
}

// NewMultipartReader creates a new multipart reader reading from r using the
// given MIME boundary.
//
// The boundary is usually obtained from the "boundary" parameter of
// the message's "Content-Type" header. Use mime.ParseMediaType to
// parse such headers.
func NewMultipartReader(r io.Reader, boundary string) *MultipartReader {
	b := []byte("\r\n--" + boundary + "--")
	src := &stickyErrorReader{r: r}
	return &MultipartReader{
		src:              src,
		bufReader:        bufio.NewReaderSize(src, peekBufferSize),
		nl:               b[:2],
		nlDashBoundary:   b[:len(b)-2],
		dashBoundaryDash: b[2:],
		dashBoundary:     b[2 : len(b)-2],
	}
}

// stickyErrorReader is an io.Reader which never calls Read on its
// underlying Reader once an error has been seen. (the io.Reader
// interface's contract promises nothing about the return values of
// Read calls after an error, yet this package does do multiple Reads
// after error)
//
// It also counts the bytes it has delivered, which is how the reader above it
// knows where in the input it is; see MultipartReader.offset.
type stickyErrorReader struct {
	r   io.Reader
	n   int64 // bytes read from r so far
	err error
}

func (r *stickyErrorReader) Read(p []byte) (n int, _ error) {
	if r.err != nil {
		return 0, r.err
	}
	n, r.err = r.r.Read(p)
	r.n += int64(n)
	return n, r.err
}

// offset returns how many bytes of its input the reader has consumed: what it
// has pulled from the source, less what the buffered reader still holds ahead
// of the parse cursor.
//
// Counting at the source and subtracting the buffer, rather than counting at
// the cursor, is what makes the result the parser's own position: a reader
// that hands out one byte at a time and one that hands out the whole message
// at once fill the buffer differently but reach the same offset.
func (r *MultipartReader) offset() int64 {
	return r.src.n - int64(r.bufReader.Buffered())
}

func newPart(mr *MultipartReader) (*Part, error) {
	// The delimiter line that introduced this part has already been consumed,
	// so the reader is standing on the first byte of the part's header block.
	bp := &Part{mr: mr, headerOffset: mr.offset()}
	scan := &headerScan{lim: mr.limits, base: bp.headerOffset}
	if err := bp.populateHeaders(scan); err != nil {
		// Only this reader's own budgets make a HeaderLimitError. The test is
		// against the sentinel no reader can present, not against the ones it
		// can, so an input that fails with ErrHeaderTooBig of its own stays the
		// read failure it is.
		if isBudgetExhausted(err) {
			// The part is gone, so the position it had is reported with the
			// failure rather than lost with it. The refused line or field is
			// placed the same way: its offset within the block plus where the
			// block itself began.
			return nil, scan.limitError()
		}
		return nil, err
	}
	bp.bodyOffset = mr.offset()
	bp.r = partReader{bp}
	return bp, nil
}

func (bp *Part) populateHeaders(scan *headerScan) error {
	// Two boundary lines in a row mean an empty part with no header block. If
	// the next bytes already form a boundary, return an empty header instead
	// of trying to parse the boundary line as a header.
	if peek, _ := bp.mr.bufReader.Peek(len(bp.mr.dashBoundary)); bytes.HasPrefix(peek, bp.mr.dashBoundary) {
		bp.Header = Header{}
		return nil
	}

	// readHeader rather than ReadHeaderLimits: the budget failure is caught by
	// newPart, which knows where this part began and can say so. Converting it
	// here would produce a HeaderLimitError with no position and then have to be
	// unpicked.
	header, err := readHeader(bp.mr.bufReader, scan)
	if err == nil {
		bp.Header = header
	}
	return err
}

// Read reads the body of a part, after its headers and before the
// next part (if any) begins.
func (p *Part) Read(d []byte) (n int, err error) {
	return p.r.Read(d)
}

// partReader implements io.Reader by reading raw bytes directly from the
// wrapped *Part, without doing any Transfer-Encoding decoding.
type partReader struct {
	p *Part
}

func (pr partReader) Read(d []byte) (int, error) {
	p := pr.p
	br := p.mr.bufReader

	// Read into buffer until we identify some data to return,
	// or we find a reason to stop (boundary or read error).
	for p.n == 0 && p.err == nil {
		peek, _ := br.Peek(br.Buffered())
		p.n, p.err = scanUntilBoundary(peek, p.mr.dashBoundary, p.mr.nlDashBoundary, p.total, p.readErr)
		if p.n == 0 && p.err == nil {
			// Force buffered I/O to read more into buffer.
			_, p.readErr = br.Peek(len(peek) + 1)
			if p.readErr == io.EOF {
				p.readErr = io.ErrUnexpectedEOF
			}
		}
	}

	// Read out from "data to return" part of buffer.
	if p.n == 0 {
		return 0, p.err
	}
	n := len(d)
	if n > p.n {
		n = p.n
	}
	n, _ = br.Read(d[:n])
	p.total += int64(n)
	p.n -= n
	if p.n == 0 {
		return n, p.err
	}
	return n, nil
}

// scanUntilBoundary scans buf to identify how much of it can be safely
// returned as part of the Part body.
// dashBoundary is "--boundary".
// nlDashBoundary is "\r\n--boundary" or "\n--boundary", depending on what mode we are in.
// The comments below (and the name) assume "\n--boundary", but either is accepted.
// total is the number of bytes read out so far. If total == 0, then a leading "--boundary" is recognized.
// readErr is the read error, if any, that followed reading the bytes in buf.
// scanUntilBoundary returns the number of data bytes from buf that can be
// returned as part of the Part body and also the error to return (if any)
// once those data bytes are done.
func scanUntilBoundary(buf, dashBoundary, nlDashBoundary []byte, total int64, readErr error) (int, error) {
	if total == 0 {
		// At beginning of body, allow dashBoundary.
		if bytes.HasPrefix(buf, dashBoundary) {
			switch matchAfterPrefix(buf, dashBoundary, readErr) {
			case -1:
				return len(dashBoundary), nil
			case 0:
				return 0, nil
			case +1:
				return 0, io.EOF
			}
		}
		if bytes.HasPrefix(dashBoundary, buf) {
			if readErr != nil {
				// EOF reached without ever seeing a boundary; flush the
				// buffered bytes as body rather than holding them back for a
				// boundary that will never arrive.
				return len(buf), errors.Join(readErr, ErrMissingBoundaryClose)
			}
			return 0, readErr
		}
	}

	// Search for "\n--boundary".
	if i := bytes.Index(buf, nlDashBoundary); i >= 0 {
		switch matchAfterPrefix(buf[i:], nlDashBoundary, readErr) {
		case -1:
			return i + len(nlDashBoundary), nil
		case 0:
			return i, nil
		case +1:
			return i, io.EOF
		}
	}
	if bytes.HasPrefix(nlDashBoundary, buf) {
		if readErr != nil {
			return len(buf), errors.Join(readErr, ErrMissingBoundaryClose)
		}
		return 0, readErr
	}

	// Otherwise, anything up to the final \n is not part of the boundary
	// and so must be part of the body.
	// Also if the section from the final \n onward is not a prefix of the boundary,
	// it too must be part of the body.
	i := bytes.LastIndexByte(buf, nlDashBoundary[0])
	if i >= 0 && bytes.HasPrefix(nlDashBoundary, buf[i:]) {
		if readErr != nil {
			return len(buf), errors.Join(readErr, ErrMissingBoundaryClose)
		}
		return i, nil
	}
	if readErr != nil {
		return len(buf), errors.Join(readErr, ErrMissingBoundaryClose)
	}
	return len(buf), readErr
}

// matchAfterPrefix checks whether buf should be considered to match the boundary.
// The prefix is "--boundary" or "\r\n--boundary" or "\n--boundary",
// and the caller has verified already that bytes.HasPrefix(buf, prefix) is true.
//
// matchAfterPrefix returns +1 if the buffer does match the boundary,
// meaning the prefix is followed by a dash, space, tab, cr, nl, or end of input.
// It returns -1 if the buffer definitely does NOT match the boundary,
// meaning the prefix is followed by some other character.
// For example, "--foobar" does not match "--foo".
// It returns 0 more input needs to be read to make the decision,
// meaning that len(buf) == len(prefix) and readErr == nil.
func matchAfterPrefix(buf, prefix []byte, readErr error) int {
	if len(buf) == len(prefix) {
		if readErr != nil {
			return +1
		}
		return 0
	}
	c := buf[len(prefix)]
	if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '-' {
		return +1
	}
	return -1
}

func (p *Part) Close() error {
	io.Copy(ioutil.Discard, p)
	return nil
}

// MultipartReader is an iterator over parts in a MIME multipart body.
// MultipartReader's underlying parser consumes its input as needed. Seeking
// isn't supported.
type MultipartReader struct {
	// src is the input, counting what it has delivered so that the reader can
	// report where in it the parse cursor stands.
	src       *stickyErrorReader
	bufReader *bufio.Reader

	currentPart *Part
	partsRead   int
	done        bool

	// limits are the budgets of each part's header block, and of the framing
	// lines between parts, or the zero value for no budget at all. See
	// SetHeaderLimits.
	limits HeaderLimits

	// lineBuf accumulates one framing line while it is read under a line
	// budget. It is reused across lines, so a reader holds one line at a time
	// and nothing grows with the number of lines. It stays nil while no line
	// budget is set, which is when framing lines are read the way they always
	// were.
	lineBuf []byte

	nl               []byte // "\r\n" or "\n" (set after seeing first boundary line)
	nlDashBoundary   []byte // nl + "--boundary"
	dashBoundaryDash []byte // "--boundary--"
	dashBoundary     []byte // "--boundary"
}

// SetMaxHeaderBytes limits the header block of every part this reader returns
// to n bytes, counted as ReadHeaderLimit counts them: the block as it appears
// in the input, through the empty line that ends it.
//
// The budget is per part and is never cumulative. Each part's header block is
// read under the whole of n, so a message of many parts is not refused for the
// headers its earlier parts already spent.
//
// A part whose header block exceeds n ends the iteration: NextPart returns a
// *HeaderLimitError, and every later call returns io.EOF, because a block that
// overran its budget leaves the input somewhere inside a part that this reader
// cannot resynchronize to. The parts already returned stay valid and readable.
//
// n of zero or below is no budget, which is what a reader does by default.
// SetMaxHeaderBytes must be called before the first NextPart; calling it later
// has no effect on a part that has already been read.
//
// It sets one budget and leaves the others alone; SetHeaderLimits sets them all
// at once.
func (r *MultipartReader) SetMaxHeaderBytes(n int64) {
	r.limits.MaxBytes = n
}

// SetHeaderLimits applies limits to every part this reader returns, and to the
// framing lines it reads between them.
//
// The per-block header budgets are per part and are never cumulative, on the
// terms SetMaxHeaderBytes describes and HeaderLimits states: each part's header
// block is read under the whole of every one. HeaderLimits.Total is the
// exception: every part's header block is charged to that one shared budget,
// together with whatever else reads with it, as ReadHeaderLimits describes. A
// part whose header block exceeds any budget ends the iteration, with a
// *HeaderLimitError naming the budget and the position — for Total, the shared
// budget's one refusal — and every later call returns io.EOF, because a block
// that overran its budget leaves the input somewhere inside a part this reader
// cannot resynchronize to. The parts already returned stay valid and readable.
// A part with no header block at all charges nothing, and neither do the
// framing lines, the preamble, the epilogue or the parts' bodies.
//
// HeaderLimits.MaxLineBytes reaches further than the header blocks. It also
// bounds the framing lines this reader has to materialize as lines: the
// preamble lines before the first delimiter, the delimiter lines themselves, and
// the empty line that separates a part's body from the delimiter that follows
// it. A framing line past the budget ends the iteration the same way, with a
// *LineLimitError saying where the line began. It does not bound a part's body,
// which is opaque content this reader streams and never accumulates as a line.
//
// The framing line budget is also what decides how such a line is read at all.
// With no line budget — the default, and what every caller had before this
// existed — a framing line is read from one of the reader's buffers, so a line
// longer than that buffer fails as it always has. With one, a framing line is
// accumulated across buffers up to the budget, which is what makes the budget
// the thing that decides, rather than a buffer size that was never meant to.
//
// SetHeaderLimits must be called before the first NextPart; calling it later has
// no effect on a part that has already been read.
func (r *MultipartReader) SetHeaderLimits(limits HeaderLimits) {
	r.limits = limits
}

// NextPart returns the next part in the multipart or an error.
// When there are no more parts, the error io.EOF is returned.
//
// Once NextPart has returned an EOF-class error (one satisfying
// errors.Is(err, io.EOF) or errors.Is(err, io.ErrUnexpectedEOF)), subsequent
// calls return io.EOF immediately. This guards callers against infinite loops
// on a body that ends without its closing boundary, whose underlying reader
// stays errored. A header block that exceeded the budget set by
// SetMaxHeaderBytes ends the iteration the same way, for the same reason: the
// reader is standing inside a part whose extent it never learned. An ordinary
// read failure does not end it, whatever that failure carries.
func (r *MultipartReader) NextPart() (*Part, error) {
	if r.done {
		return nil, io.EOF
	}
	p, err := r.nextPart()
	if err != nil {
		if _, refused := HeaderLimit(err); refused {
			r.done = true
		} else if _, refused := LineLimit(err); refused {
			r.done = true
		} else if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			r.done = true
		}
	}
	return p, err
}

// readFramingLine reads the next line of multipart framing — a preamble line, a
// delimiter line, or the empty line before one — and returns it exactly as
// bufio.Reader.ReadSlice would, terminator included.
//
// With no line budget it is ReadSlice, unchanged, down to which inputs it turns
// away and with which error: a line longer than the reader's buffer fails with
// bufio.ErrBufferFull, as it always has. That is deliberate. A budget is what
// should decide how long a line may be, and where there is no budget there is
// nothing to change the long-standing answer to.
//
// With one, the line is accumulated across buffers into lineBuf until it ends or
// until it passes the budget, and passing it returns a *LineLimitError saying
// where the line began. Nothing beyond the budget is ever retained: the check
// happens as each buffer arrives, so what is held is at most one buffer past it.
//
// The returned slice is valid until the next call, which is the same contract
// ReadSlice's result carries.
func (r *MultipartReader) readFramingLine() ([]byte, error) {
	if r.limits.MaxLineBytes <= 0 {
		return r.bufReader.ReadSlice('\n')
	}

	// Where this line begins, so that a refusal reports a real position in the
	// reader's input rather than where the failure was noticed.
	start := r.offset()

	line := r.lineBuf[:0]
	// data is the bytes of this line that are not its terminator, counted the
	// way headerScan counts them, including a '\r' that turns out not to have
	// been the start of one.
	var data int64
	// pendingCR records a '\r' that ended a full buffer. It may be the first
	// byte of a "\r\n" whose '\n' is in the next read, or an ordinary byte of
	// the line, and only that next read can say which, so it is held back
	// uncharged until then. Charging it early would refuse a line whose content
	// is exactly the budget because its terminator straddled a buffer edge.
	// This is headerScan.classify's rule, applied to framing lines.
	var pendingCR bool
	for {
		l, err := r.bufReader.ReadSlice('\n')

		n := int64(len(l))
		if pendingCR {
			pendingCR = false
			if len(l) == 0 || l[0] != '\n' {
				// Followed by something other than '\n', or by nothing at
				// all: it was content.
				n++
			}
		}
		switch {
		case err == nil:
			n--
			if len(l) >= 2 && l[len(l)-2] == '\r' {
				n--
			}
		case err == bufio.ErrBufferFull:
			// No terminator yet. A trailing '\r' may begin one, so it is held
			// back rather than charged.
			if len(l) > 0 && l[len(l)-1] == '\r' {
				pendingCR = true
				n--
			}
		}
		if n > r.limits.MaxLineBytes-data {
			r.lineBuf = line
			return nil, newLineLimitError(r.limits.MaxLineBytes, start)
		}
		data += n

		line = append(line, l...)
		if err == bufio.ErrBufferFull {
			continue
		}
		r.lineBuf = line
		return line, err
	}
}

func (r *MultipartReader) nextPart() (*Part, error) {
	if r.currentPart != nil {
		r.currentPart.Close()
	}
	if string(r.dashBoundary) == "--" {
		return nil, fmt.Errorf("multipart: boundary is empty")
	}
	expectNewPart := false
	for {
		line, err := r.readFramingLine()

		// A framing line past its budget is this reader's own refusal, and is
		// returned as itself rather than wrapped: it is not a failure of the
		// input, and nothing below it should be asked to explain it. The exact
		// dynamic type and this package's own mark are what identify it, so an
		// input that fails with ErrLineTooBig, or with a LineLimitError of its
		// own, is the ordinary read failure it is — including a typed nil of
		// that type, which passes the type assertion and would be dereferenced
		// by the mark if it were not excluded first.
		if e, ok := err.(*LineLimitError); ok && e != nil && e.fromParser {
			return nil, e
		}

		if err == io.EOF && r.isFinalBoundary(line) {
			// If the buffer ends in "--boundary--" without the
			// trailing "\r\n", ReadSlice will return an error
			// (since it's missing the '\n'), but this is a valid
			// multipart EOF so we need to return io.EOF instead of
			// a fmt-wrapped one.
			return nil, io.EOF
		}
		if err != nil {
			return nil, fmt.Errorf("multipart: NextPart: %w", err)
		}

		if r.isBoundaryDelimiterLine(line) {
			r.partsRead++
			bp, err := newPart(r)
			if err != nil {
				return nil, err
			}
			r.currentPart = bp
			return bp, nil
		}

		if r.isFinalBoundary(line) {
			// Expected EOF
			return nil, io.EOF
		}

		if expectNewPart {
			return nil, fmt.Errorf("multipart: expecting a new Part; got line %q", string(line))
		}

		if r.partsRead == 0 {
			// skip line
			continue
		}

		// Consume the "\n" or "\r\n" separator between the
		// body of the previous part and the boundary line we
		// now expect will follow. (either a new part or the
		// end boundary)
		if bytes.Equal(line, r.nl) {
			expectNewPart = true
			continue
		}

		return nil, fmt.Errorf("multipart: unexpected line in Next(): %q", line)
	}
}

// isFinalBoundary reports whether line is the final boundary line
// indicating that all parts are over.
// It matches `^--boundary--[ \t]*(\r\n)?$`
func (mr *MultipartReader) isFinalBoundary(line []byte) bool {
	if !bytes.HasPrefix(line, mr.dashBoundaryDash) {
		return false
	}
	rest := line[len(mr.dashBoundaryDash):]
	rest = skipLWSPChar(rest)
	return len(rest) == 0 || bytes.Equal(rest, mr.nl)
}

func (mr *MultipartReader) isBoundaryDelimiterLine(line []byte) (ret bool) {
	// https://tools.ietf.org/html/rfc2046#section-5.1
	//   The boundary delimiter line is then defined as a line
	//   consisting entirely of two hyphen characters ("-",
	//   decimal value 45) followed by the boundary parameter
	//   value from the Content-Type header field, optional linear
	//   whitespace, and a terminating CRLF.
	if !bytes.HasPrefix(line, mr.dashBoundary) {
		return false
	}
	rest := line[len(mr.dashBoundary):]
	rest = skipLWSPChar(rest)

	// On the first part, see our lines are ending in \n instead of \r\n
	// and switch into that mode if so. This is a violation of the spec,
	// but occurs in practice.
	if mr.partsRead == 0 && len(rest) == 1 && rest[0] == '\n' {
		mr.nl = mr.nl[1:]
		mr.nlDashBoundary = mr.nlDashBoundary[1:]
	}
	return bytes.Equal(rest, mr.nl)
}

// skipLWSPChar returns b with leading spaces and tabs removed.
// RFC 822 defines:
//
//	LWSP-char = SPACE / HTAB
func skipLWSPChar(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	return b
}

// A MultipartWriter generates multipart messages.
type MultipartWriter struct {
	w        io.Writer
	boundary string
	lastpart *part
}

// NewMultipartWriter returns a new multipart Writer with a random boundary,
// writing to w.
func NewMultipartWriter(w io.Writer) *MultipartWriter {
	return &MultipartWriter{
		w:        w,
		boundary: randomBoundary(),
	}
}

// Boundary returns the Writer's boundary.
func (w *MultipartWriter) Boundary() string {
	return w.boundary
}

// SetBoundary overrides the Writer's default randomly-generated
// boundary separator with an explicit value.
//
// SetBoundary must be called before any parts are created, may only
// contain certain ASCII characters, and must be non-empty and
// at most 70 bytes long.
func (w *MultipartWriter) SetBoundary(boundary string) error {
	if w.lastpart != nil {
		return errors.New("mime: SetBoundary called after write")
	}
	// rfc2046#section-5.1.1
	if len(boundary) < 1 || len(boundary) > 70 {
		return errors.New("mime: invalid boundary length")
	}
	end := len(boundary) - 1
	for i, b := range boundary {
		if 'A' <= b && b <= 'Z' || 'a' <= b && b <= 'z' || '0' <= b && b <= '9' {
			continue
		}
		switch b {
		case '\'', '(', ')', '+', '_', ',', '-', '.', '/', ':', '=', '?':
			continue
		case ' ':
			if i != end {
				continue
			}
		}
		return errors.New("mime: invalid boundary character")
	}
	w.boundary = boundary
	return nil
}

func randomBoundary() string {
	var buf [30]byte
	_, err := io.ReadFull(rand.Reader, buf[:])
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", buf[:])
}

// CreatePart creates a new multipart section with the provided
// header. The body of the part should be written to the returned
// Writer. After calling CreatePart, any previous part may no longer
// be written to.
func (w *MultipartWriter) CreatePart(header Header) (io.Writer, error) {
	var b bytes.Buffer
	return w.CreatePartWithBuffer(header, &b)
}

// CreatePartWithBuffer behaves like CreatePart but uses the caller-supplied
// buffer to assemble the part's boundary line and header instead of allocating
// a new one. The buffer's current contents are written out as-is, so callers
// should pass an empty buffer; reusing one buffer across many parts avoids a
// per-part allocation. CreatePartWithBuffer drains the buffer before returning,
// so the same buffer can be passed again for the next part.
func (w *MultipartWriter) CreatePartWithBuffer(header Header, b *bytes.Buffer) (io.Writer, error) {
	if w.lastpart != nil {
		if err := w.lastpart.close(); err != nil {
			return nil, err
		}
	}
	if w.lastpart != nil {
		fmt.Fprintf(b, "\r\n--%s\r\n", w.boundary)
	} else {
		fmt.Fprintf(b, "--%s\r\n", w.boundary)
	}

	WriteHeader(b, header)

	_, err := io.Copy(w.w, b)
	if err != nil {
		return nil, err
	}
	p := &part{
		mw: w,
	}
	w.lastpart = p
	return p, nil
}

// Close finishes the multipart message and writes the trailing
// boundary end line to the output.
func (w *MultipartWriter) Close() error {
	if w.lastpart != nil {
		if err := w.lastpart.close(); err != nil {
			return err
		}
		w.lastpart = nil
	}
	_, err := fmt.Fprintf(w.w, "\r\n--%s--\r\n", w.boundary)
	return err
}

type part struct {
	mw     *MultipartWriter
	closed bool
	we     error // last error that occurred writing
}

func (p *part) close() error {
	p.closed = true
	return p.we
}

func (p *part) Write(d []byte) (n int, err error) {
	if p.closed {
		return 0, errors.New("multipart: can't write to finished part")
	}
	n, err = p.mw.w.Write(d)
	if err != nil {
		p.we = err
	}
	return
}
