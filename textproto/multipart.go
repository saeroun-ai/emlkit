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

	// separatorMissing reports that the part's header block, which began with
	// a malformed prefix or held an orphan record, was ended by a delimiter
	// line rather than by an empty line; see HeaderSeparatorMissing.
	separatorMissing bool
}

// HeaderSeparatorMissing reports that this part's header block was ended by the
// next delimiter line of its multipart rather than by the empty line that
// separates a header from a body, so that the part's body is empty and the
// delimiter line, unread by the header, is what ended the part.
//
// It is only ever reported for a header block that began with a malformed
// prefix (see Header.MalformedPrefix), or in which an orphan record was
// recovered (see Header.OrphanContinuations), whose later lines — after the
// prefix, or after the record — are each looked at as a delimiter line before
// they are read, and only for a line proved one — its
// line ending, or the end of the input, seen after the boundary and any white
// space — so such a delimiter is never read as one of its fields and the part
// after it is not lost. White space past the reader's buffer is decided within
// the framing line budget, HeaderLimits.MaxLineBytes, and no header-only
// budget; with no line budget, a line still undecided past one buffer is this
// reader's framing refusal wrapping bufio.ErrBufferFull, which ends the
// iteration, unless HeaderLimits.UnboundedFramingLines lets it be held until
// it is decided. A line that only begins like one is read as the line it is. A
// header block with neither a malformed prefix nor an orphan record is read as
// it always was, and a missing separator there is not looked for
// (ROB-020-005).
func (p *Part) HeaderSeparatorMissing() bool {
	return p.separatorMissing
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

// BodyLen returns how many bytes of the part's body have been read so far,
// and whether the body has been read to its end — the delimiter line that
// ended it, or the end of the input. Once ended, the part's body is the n
// bytes of the reader's input that begin at BodyOffset.
func (p *Part) BodyLen() (n int64, ended bool) {
	return p.total, p.n == 0 && p.err != nil && len(p.mr.out) == 0
}

// NewMultipartReader creates a new multipart reader reading from r using the
// given MIME boundary.
//
// The boundary is usually obtained from the "boundary" parameter of
// the message's "Content-Type" header. Use mime.ParseMediaType to
// parse such headers.
func NewMultipartReader(r io.Reader, boundary string) *MultipartReader {
	src := &stickyErrorReader{r: r}
	return &MultipartReader{
		src:          src,
		bufReader:    bufio.NewReaderSize(src, peekBufferSize),
		dashBoundary: []byte("--" + boundary),
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
	// A count outside 0 to len(p) is a broken reader; the buffered reader
	// above indexes with it, so it is held to what io.Reader allows.
	n = max(0, min(n, len(p)))
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
//
// Bytes the reader has taken out of its buffer but not yet handed to a caller
// or consumed as framing — a delimiter candidate held while it is decided, or
// one that turned out to be body and is still being delivered — are not past
// the cursor either, so they are subtracted too, and so are bytes a part's
// header gave back to the reader that it has not read again yet (see
// restore).
func (r *MultipartReader) offset() int64 {
	return r.src.n - int64(r.bufReader.Buffered()) - int64(len(r.pre.b)) - int64(len(r.out)) - int64(len(r.cand.held))
}

// windowAt returns the input offset of the byte at index x of what the
// buffered reader holds.
func (r *MultipartReader) windowAt(x int) int64 {
	return r.src.n - int64(len(r.pre.b)) - int64(r.bufReader.Buffered()) + int64(x)
}

// restore gives held, bytes a part's header took out of the buffered reader
// to decide a damaged line and then did not keep (ROB-020-002; see
// headerScan.settleCR), back to this reader: it then reads held, the bytes it
// still buffered, any bytes given back before and not read yet, and then its
// source, exactly as if nothing had been taken out. The buffered bytes are
// copied out before the buffered reader is reset to read r.pre, which reads
// the sticky source itself, never the buffered reader, so no buffer is shared
// and nothing reads itself, and the bytes given back never stack: there is one
// pre, over r.src, whatever is given back how often. A failure the source
// returned stays the source's, which keeps returning it. Nothing is read, and
// offsets are unchanged: the bytes in r.pre are not past the cursor.
func (r *MultipartReader) restore(held []byte) {
	br := r.bufReader
	n := br.Buffered()
	b, _ := br.Peek(n)
	pre := make([]byte, 0, len(held)+n+len(r.pre.b))
	pre = append(append(append(pre, held...), b...), r.pre.b...)
	br.Discard(n)
	r.pre = prefixReader{b: pre, src: r.src}
	br.Reset(&r.pre)
}

func newPart(mr *MultipartReader) (*Part, error) {
	// The delimiter line that introduced this part has already been consumed,
	// so the reader is standing on the first byte of the part's header block.
	bp := &Part{mr: mr, headerOffset: mr.offset()}
	// The block is a part's header, so after a malformed prefix or an orphan
	// record it ends before a delimiter line of this reader rather than reading
	// one; see prefix.go and orphan.go.
	scan := &headerScan{lim: mr.limits, base: bp.headerOffset, parent: mr}
	if err := bp.populateHeaders(scan); err != nil {
		// Only this reader's own budgets make a HeaderLimitError. The test is
		// against the sentinel no reader can present, not against the ones it
		// can, so an input that fails with ErrHeaderTooBig of its own stays the
		// read failure it is.
		if isBudgetExhausted(err) {
			// The part is gone, so the position it had is reported with the
			// failure rather than lost with it. The refused line or field is
			// placed the same way: its offset within the block plus where the
			// block itself began. It is remembered as this reader's own, which
			// is how NextPart tells it from one an input hands back.
			refusal := scan.limitError()
			mr.refused = refusal
			return nil, refusal
		}
		return nil, err
	}
	bp.separatorMissing = scan.atDelim || scan.heal.sepMissing
	if len(scan.unread) > 0 {
		// Under recovery the block ended before a line that begins the body:
		// its bytes go back to the reader, which reads them first (see
		// recover.go and restore).
		mr.restore(scan.unread)
	}
	bp.bodyOffset = mr.offset()
	bp.r = partReader{bp}
	return bp, nil
}

func (bp *Part) populateHeaders(scan *headerScan) error {
	// Two boundary lines in a row mean an empty part with no header block. If
	// the next bytes form a whole delimiter line, return an empty header
	// instead of trying to parse that line as a header. Bytes that only begin
	// like one — "--boundary-junk" — are read as the header they are.
	if bp.mr.atDelimiter() {
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

// Read returns the part's body: every byte up to the delimiter line that ends
// it, which is not body.
//
// A delimiter line is the line ending that precedes it — "\r\n", or a bare
// '\n' or '\r', each matched as that whole ending, so "\r\r\n--b" leaves the
// first '\r' in the body — or, at the very start of a body, nothing; then
// "--" and the boundary, "--" again for the closing one, any spaces and tabs,
// and a line ending of its own, or for the closing one the end of the input.
// Endings before and after need not match each other or any other delimiter's.
// Bytes that begin like one and then do not fit — "--b-x", "--b junk", "--b"
// and white space followed by anything else — are body, every one of them,
// the line ending before them included.
//
// The decision may need bytes the reader does not have yet. Body before a
// candidate is returned at once; the candidate is held, at most one framing
// line of it, until it is decided. White space past what the reader's buffer
// holds is held outside it, bounded by the framing line budget
// (HeaderLimits.MaxLineBytes) counted on the line from its "--" up to its line
// ending, the line ending before and after excluded. A candidate whose valid
// prefix — up to the byte that decides it, which is not counted — grows past
// that budget is a framing line too long to read, however its bytes arrive and
// whatever that byte turns out to be: the body ends before it, and NextPart
// returns the reader's own *LineLimitError. With no budget a
// candidate line must fit in one buffer of the reader, ending included, as it
// always had to; one that does not ends the body the same way, and NextPart
// returns this reader's own error wrapping bufio.ErrBufferFull. With no budget
// and HeaderLimits.UnboundedFramingLines nothing bounds it: the candidate is
// held until a byte decides it, so the memory it takes is its length — the
// line ending before it, "--", the boundary, any "--" and the run of spaces
// and tabs after them — which only the length of the input bounds.
//
// At a delimiter, Read returns io.EOF, and NextPart consumes the delimiter. A
// failure of the input other than io.EOF while a candidate is undecided does
// not decide it: the candidate's bytes are body, returned with everything
// before them, and the failure is reported as for any body the input cut
// short, in errors.Join with ErrMissingBoundaryClose. The input ending with
// io.EOF decides it: a closing delimiter needs no line ending there, a
// delimiter line "--boundary" and white space is one with nothing after it
// (NextPart then reports the input's end), and anything shorter is body.
func (pr partReader) Read(d []byte) (int, error) {
	p := pr.p
	mr := p.mr
	br := mr.bufReader

	// Read into buffer until we identify some data to return,
	// or we find a reason to stop (boundary or read error).
	for len(mr.out) == 0 && p.n == 0 && p.err == nil {
		p.n, p.err = mr.scanBody(p)
		if p.n == 0 && p.err == nil && len(mr.out) == 0 {
			// Force buffered I/O to read more into buffer. The peek is for one
			// byte more than is held, never more than the whole buffer, so a
			// bufio.ErrBufferFull it returns is the input's own failure.
			if n := br.Buffered(); n < br.Size() {
				_, p.readErr = br.Peek(n + 1)
			}
		}
	}

	// A candidate that turned out to be body comes first: its bytes precede
	// everything the reader holds.
	if len(mr.out) > 0 {
		n := copy(d, mr.out)
		mr.out = mr.out[n:]
		if len(mr.out) == 0 {
			mr.out = nil
		}
		p.total += int64(n)
		return n, nil
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

// matchStatus is what is known of a delimiter candidate.
type matchStatus uint8

const (
	matchFail  matchStatus = iota // not a delimiter: its bytes are body
	matchNeed                     // undecided until more bytes arrive
	matchDelim                    // a whole delimiter line
)

// A delimMatch is the outcome of matching one candidate.
type delimMatch struct {
	status matchStatus
	close  bool

	// For matchNeed, padding reports that the candidate's name has matched and
	// k is where its white space so far ends. For matchFail after the name, k
	// is the byte that did not fit.
	padding bool
	k       int

	// For matchDelim: end is just past the delimiter line; endKind and endLen
	// are its line ending, none when the input ended it (atEOF).
	end     int
	endKind LineEnding
	endLen  int
	atEOF   bool

	// cut reports a delimiter line the end of the input cut short, read as
	// one under recovery only.
	cut bool
}

// cutDelimiterMin is how many bytes of "--boundary" a delimiter line cut by
// the end of the input must still hold to be read as one under recovery: the
// dashes and four bytes of the boundary, or all of a shorter one.
const cutDelimiterMin = 6

// match matches the candidate whose line ending before it is buf[c:c+elen]
// (elen is 0 at the start of a body). eof reports that buf is all the input
// there is, ended by io.EOF; full that buf is all the buffered reader can hold,
// so a name that does not fit in it never matches.
func (r *MultipartReader) match(buf []byte, c, elen int, eof, full bool) delimMatch {
	db := r.dashBoundary
	head := c + elen
	if len(buf)-head < len(db) {
		if eof && r.limits.Recover && len(buf)-head >= cutDelimiterMin && bytes.HasPrefix(db, buf[head:]) {
			// Under recovery, a delimiter line the end of the input cut
			// inside its name ends the part before it (ROB-060-006).
			return delimMatch{status: matchDelim, end: len(buf), atEOF: true, cut: true}
		}
		if eof || full || !bytes.HasPrefix(db, buf[head:]) {
			return delimMatch{status: matchFail}
		}
		return delimMatch{status: matchNeed}
	}
	if !bytes.Equal(buf[head:head+len(db)], db) {
		return delimMatch{status: matchFail}
	}
	k := head + len(db)
	if k == len(buf) && !eof {
		// Whether a closing "--" follows is not known yet.
		if full {
			return delimMatch{status: matchFail}
		}
		return delimMatch{status: matchNeed}
	}
	if k < len(buf) && buf[k] == '-' {
		if k+1 == len(buf) {
			if eof && r.limits.Recover {
				// "--boundary-" and the end of the input: a closing delimiter
				// cut one byte short (ROB-060-006).
				return delimMatch{status: matchDelim, close: true, end: k + 1, atEOF: true, cut: true}
			}
			if eof || full {
				return delimMatch{status: matchFail}
			}
			return delimMatch{status: matchNeed}
		}
		if buf[k+1] != '-' {
			return delimMatch{status: matchFail}
		}
		k += 2
		return matchTail(buf, k, eof, delimMatch{close: true})
	}
	return matchTail(buf, k, eof, delimMatch{})
}

// matchTail matches the white space and line ending of a candidate whose
// name has matched, from buf[k:].
func matchTail(buf []byte, k int, eof bool, m delimMatch) delimMatch {
	for k < len(buf) && (buf[k] == ' ' || buf[k] == '\t') {
		k++
	}
	if k == len(buf) {
		if eof {
			m.status, m.end, m.atEOF = matchDelim, k, true
			return m
		}
		m.status, m.padding, m.k = matchNeed, true, k
		return m
	}
	switch buf[k] {
	case '\n':
		m.endKind, m.endLen = LineEndingLF, 1
	case '\r':
		switch {
		case k+1 < len(buf) && buf[k+1] == '\n':
			m.endKind, m.endLen = LineEndingCRLF, 2
		case k+1 < len(buf) || eof:
			m.endKind, m.endLen = LineEndingCR, 1
		default:
			// The byte after the '\r' decides it.
			m.status, m.padding, m.k = matchNeed, true, k
			return m
		}
	default:
		return delimMatch{status: matchFail, padding: true, k: k}
	}
	m.status, m.end = matchDelim, k+m.endLen
	return m
}

// scanBody decides how much of what the reader holds is body of p. It returns
// how many bytes the buffered reader holds that are body, and the error that
// follows them: io.EOF at a delimiter or a refused framing attempt, the
// input's failure joined with ErrMissingBoundaryClose, or nil. (0, nil) with
// nothing in r.out asks the caller to read more.
func (r *MultipartReader) scanBody(p *Part) (int, error) {
	br := r.bufReader
	buf, _ := br.Peek(br.Buffered())
	eof := p.readErr == io.EOF
	failed := p.readErr != nil && !eof
	full := len(buf) == br.Size()

	if r.cand.active {
		return r.scanHeld(p, buf, eof, failed)
	}

	// At the very start of a body, a delimiter needs no line ending before it.
	if p.total == 0 {
		m := r.match(buf, 0, 0, eof, full)
		if done, n, err := r.decide(p, m, buf, 0, 0, failed); done {
			return n, err
		}
	}

	for from := 0; ; {
		j := bytes.Index(buf[from:], r.dashBoundary)
		if j < 0 {
			break
		}
		j += from
		from = j + 1
		c, elen := -1, 0
		switch {
		case j >= 2 && buf[j-1] == '\n' && buf[j-2] == '\r':
			c, elen = j-2, 2
		case j >= 1 && (buf[j-1] == '\n' || buf[j-1] == '\r'):
			c, elen = j-1, 1
		}
		if c < 0 {
			// Not at the start of a line, and neither is anything up to the
			// next line ending: the search goes on after that. Stepping one
			// byte at a time instead compares the whole name at every byte
			// of a run of dashes that a boundary of dashes matches
			// (ROB-060-013).
			q := bytes.IndexAny(buf[j:], "\r\n")
			if q < 0 {
				break
			}
			from = j + q + 1
			continue
		}
		// A full buffer decides a candidate only when the candidate begins it:
		// one further in is undecided until the body before it has been
		// delivered and the bytes after it have arrived. Deciding it there
		// read "\n--boundary-" at the end of a full buffer as body, and lost
		// the closing delimiter with it, depending on how the input's reads
		// filled the buffer.
		m := r.match(buf, c, elen, eof, full && c == 0)
		if done, n, err := r.decide(p, m, buf, c, elen, failed); done {
			return n, err
		}
	}

	if eof && r.limits.Recover {
		// Under recovery, a last line the end of the input cut inside
		// "--boundary" is a delimiter line (ROB-060-006): the search above
		// finds only whole names.
		if q := bytes.LastIndexAny(buf, "\r\n"); q >= 0 {
			rest := buf[q+1:]
			if len(rest) >= cutDelimiterMin && len(rest) < len(r.dashBoundary) && bytes.HasPrefix(r.dashBoundary, rest) {
				c, elen := q, 1
				if buf[q] == '\n' && q > 0 && buf[q-1] == '\r' {
					c, elen = q-1, 2
				}
				m := delimMatch{status: matchDelim, end: len(buf), atEOF: true, cut: true}
				if done, n, err := r.decide(p, m, buf, c, elen, failed); done {
					return n, err
				}
			}
		}
	}
	if eof || failed {
		return len(buf), r.bodyFailure(p)
	}
	// Hold back a line ending that could still begin a delimiter. The name
	// holds no '\r' or '\n', so only the last ending in reach can.
	h := len(buf)
	if q := bytes.LastIndexAny(buf[max(0, len(buf)-len(r.dashBoundary)-2):], "\r\n"); q >= 0 {
		q += max(0, len(buf)-len(r.dashBoundary)-2)
		start, rest := q, buf[q+1:]
		switch {
		case buf[q] == '\n' && q > 0 && buf[q-1] == '\r':
			start = q - 1
		case buf[q] == '\r' && q+1 < len(buf) && buf[q+1] == '\n':
			rest = buf[q+2:]
		}
		if bytes.HasPrefix(r.dashBoundary, rest) {
			h = start
		}
	}
	if h == 0 && full {
		// A name that cannot fit in the buffer never matches.
		h = len(buf)
	}
	return h, nil
}

// bodyFailure is the error a body the input stopped ends with.
//
// Under recovery (HeaderLimits.Recover) the end of the input, io.EOF, ends
// the last part cleanly: its body is every byte up to there, it ends with
// io.EOF, and the reader notes that it never saw a closing delimiter
// (ROB-060-005; see Framing). A failure of the input is reported as before.
func (r *MultipartReader) bodyFailure(p *Part) error {
	err := p.readErr
	if err == io.EOF {
		if r.limits.Recover {
			r.unclosed = true
			return io.EOF
		}
		err = io.ErrUnexpectedEOF
	}
	return errors.Join(err, ErrMissingBoundaryClose)
}

// decide acts on the match m of the candidate at buf[c:], whose line ending
// before it is elen bytes. It reports whether the scan is done, and what
// scanBody then returns.
func (r *MultipartReader) decide(p *Part, m delimMatch, buf []byte, c, elen int, failed bool) (bool, int, error) {
	switch m.status {
	case matchDelim:
		content := m.end - m.endLen - (c + elen)
		if err := r.checkLine(int64(content), m.endLen, true, r.windowAt(c+elen)); err != nil {
			return true, c, io.EOF
		}
		before := LineEnding(0)
		if elen > 0 {
			before = endingOf(buf[c : c+elen])
		}
		r.prove(m, before, r.windowAt(c), r.windowAt(m.end-m.endLen), m.end-c)
		return true, c, io.EOF
	case matchNeed:
		// The candidate's valid prefix is charged before anything else is
		// decided about it, as a held candidate's is as it grows, so that
		// where the bytes happen to end in the buffer cannot change whether
		// it is refused. A budget the bytes already read exceed wins over a
		// failure of the input that follows them.
		if m.padding {
			if err := r.checkLine(int64(m.k-(c+elen)), 0, false, r.windowAt(c+elen)); err != nil {
				return true, c, io.EOF
			}
		}
		if failed {
			// The input failed before the candidate was decided: it is
			// body, and the failure follows it.
			return true, len(buf), r.bodyFailure(p)
		}
		if c > 0 {
			// The body before the candidate first; it is looked at again
			// from its own first byte.
			return true, c, nil
		}
		if m.padding {
			// Hold what has matched outside the buffer, so that white space
			// longer than it can be read on.
			r.cand = heldCandidate{
				active: true,
				held:   append(r.cand.held[:0], buf[:m.k]...),
				elen:   elen,
				close:  m.close,
				start:  r.windowAt(0),
			}
			if elen > 0 {
				r.cand.before = endingOf(buf[:elen])
			}
			r.bufReader.Discard(m.k)
		}
		return true, 0, nil
	case matchFail:
		// A candidate whose name matched and whose white space went on past
		// the framing line budget before the byte that broke it was a framing
		// line too long to read, wherever that byte is: read a byte at a time
		// it is refused as soon as its valid prefix passes the budget, before
		// that byte arrives. Only the valid prefix is charged — the byte that
		// broke it is not candidate content — so a prefix of exactly the
		// budget followed by such a byte is body, as is anything that broke
		// before its name ended.
		if m.padding {
			if err := r.checkLine(int64(m.k-(c+elen)), 0, false, r.windowAt(c+elen)); err != nil {
				return true, c, io.EOF
			}
		}
	}
	return false, 0, nil
}

// scanHeld goes on deciding the held candidate with the bytes buf the reader
// now holds.
func (r *MultipartReader) scanHeld(p *Part, buf []byte, eof, failed bool) (int, error) {
	cd := &r.cand
	m := matchTail(buf, 0, eof, delimMatch{close: cd.close})
	held := len(cd.held) - cd.elen
	lineStart := cd.start + int64(cd.elen)
	switch m.status {
	case matchDelim:
		content := held + m.end - m.endLen
		if err := r.checkLine(int64(content), m.endLen, true, lineStart); err != nil {
			return 0, io.EOF
		}
		r.prove(m, cd.before, cd.start, r.windowAt(m.end-m.endLen), m.end)
		return 0, io.EOF
	}
	// The valid prefix — what is held and the white space in the reader up to
	// what decided it or the end of what is there — is charged first, as in
	// decide, whether the candidate then fails, waits, or the input failed.
	if err := r.checkLine(int64(held+m.k), 0, false, lineStart); err != nil {
		return 0, io.EOF
	}
	if m.status == matchFail {
		// Body after all, its white space in the reader included; the byte
		// that did not fit is looked at again as the start of what follows.
		r.out = cd.held
		*cd = heldCandidate{}
		return m.k, nil
	}
	if failed {
		r.out = cd.held
		*cd = heldCandidate{}
		return len(buf), r.bodyFailure(p)
	}
	cd.held = append(cd.held, buf[:m.k]...)
	r.bufReader.Discard(m.k)
	return 0, nil
}

// checkLine holds the delimiter line being decided, of content bytes from
// its "--", to the framing line budget, or with none to one buffer of the
// reader, its ending of endLen bytes included once decided says it has one —
// unless HeaderLimits.UnboundedFramingLines lifts that, when nothing is held
// to anything. A line past it is this reader's own refusal, kept for NextPart.
func (r *MultipartReader) checkLine(content int64, endLen int, decided bool, lineStart int64) error {
	if limit := r.limits.MaxLineBytes; limit > 0 {
		if content <= limit {
			return nil
		}
		r.refuseLine(newLineLimitError(limit, lineStart))
		return r.pending
	}
	if r.limits.UnboundedFramingLines {
		return nil
	}
	size := int64(r.bufReader.Size())
	if content+int64(endLen) <= size && (decided || content <= size) {
		return nil
	}
	r.refuseLine(fmt.Errorf("multipart: NextPart: %w", bufio.ErrBufferFull))
	return r.pending
}

// refuseLine records err as this reader's own refusal of a framing line, which
// NextPart returns, recognizing it by identity.
func (r *MultipartReader) refuseLine(err error) {
	r.refused = err
	r.pending = err
	r.cand = heldCandidate{}
}

// prove records the delimiter m, which began at start with the line ending
// before before (none at the start of a body) and whose own ending is at
// endAt; rest of its bytes are still in the buffered reader.
func (r *MultipartReader) prove(m delimMatch, before LineEnding, start, endAt int64, rest int) {
	if before != 0 {
		r.ends.note(before, start)
	}
	if m.endLen > 0 {
		r.ends.note(m.endKind, endAt)
	}
	r.delim = &provenDelimiter{rest: rest, close: m.close, truncated: m.atEOF && !m.close}
	if m.cut {
		r.cutClose = true
	}
}

// endingOf names the line ending e, which is "\r\n", "\n" or "\r".
func endingOf(e []byte) LineEnding {
	switch {
	case len(e) == 2:
		return LineEndingCRLF
	case e[0] == '\n':
		return LineEndingLF
	}
	return LineEndingCR
}

// atDelimiter reports whether the bytes at the reader's cursor are a whole
// delimiter line, deciding with as many bytes as it takes. A candidate whose
// white space runs past the buffer is left to the body to decide, as one at
// the start of the body; anything that is not a delimiter is left as it is.
func (r *MultipartReader) atDelimiter() bool {
	br := r.bufReader
	eof := false
	for {
		buf, _ := br.Peek(br.Buffered())
		full := len(buf) == br.Size()
		m := r.match(buf, 0, 0, eof, full)
		switch {
		case m.status == matchDelim:
			return true
		case m.status == matchFail:
			return false
		case m.padding && full:
			return true
		case full:
			return false
		}
		// Undecided: look one byte further. A failed look is the header's to
		// report; the source keeps failing, so it is not lost.
		if _, err := br.Peek(len(buf) + 1); err != nil {
			if err != io.EOF {
				return false
			}
			eof = true
		}
	}
}

// DelimiterEndings returns the line endings of every delimiter line this
// reader has proven so far: the ending before each — none for one at the
// start of the input — and each one's own. Offsets are in the reader's input,
// on the terms of Part.HeaderOffset; a closing delimiter the input ended has
// no ending of its own. A delimiter is proven, and its endings counted, when
// the part before it has been read to it — by Part.Read, which may be before
// NextPart is called — or by NextPart for the first one. It reports no
// unterminated line; the result is a snapshot, known for every reader.
func (r *MultipartReader) DelimiterEndings() LineEndings {
	e := r.ends
	e.known = true
	e.unterminated = false
	return e
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

	// refused is the budget refusal this reader itself created and returned —
	// a *HeaderLimitError for a part's header block, a *LineLimitError for a
	// framing line — or nil. NextPart recognizes its own refusal by identity
	// with it, so that a refusal an input hands back, forged or genuine but
	// made by another reader, is the ordinary read failure it is. When set it
	// holds a non-nil pointer, so comparing an error with it compares two
	// pointers, or two different types, and cannot panic.
	refused error

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

	dashBoundary []byte // "--boundary"

	// cand is the delimiter candidate held while the bytes that decide it
	// arrive: its bytes, taken out of the buffered reader, and what has been
	// matched. out is a candidate that turned out to be body, still to be
	// delivered by the part being read, ahead of anything the reader holds.
	// Neither is ever more than one framing line: see Part.Read.
	cand heldCandidate
	out  []byte

	// pre is what bufReader reads once a part's header has given bytes back
	// to it (see restore): those bytes, then src. Until then bufReader reads
	// src directly and pre is empty.
	pre prefixReader

	// delim is the delimiter the part being read ended at, proven and not yet
	// consumed; NextPart consumes it. pending is this reader's own refusal of
	// a framing attempt the part's body scan met, for NextPart to return.
	delim   *provenDelimiter
	pending error

	// ends is the line endings of every delimiter this reader has proven; see
	// DelimiterEndings. lastEnd and lastEndAt are the ending of the preamble
	// line read last, which is the ending before a delimiter that follows it.
	ends      LineEndings
	lastEnd   LineEnding
	lastEndAt int64

	// closed reports that NextPart consumed a closing delimiter line and
	// returned io.EOF for it; epilogueAt is then the offset just past that
	// line, its line ending included. Neither is set by any other end of the
	// iteration — an input that ended or failed before a closing delimiter, a
	// refusal, an empty body — and neither changes once set. epilogue is the
	// one reader Epilogue hands out, made on its first call. See Epilogue.
	closed     bool
	epilogueAt int64
	epilogue   *epilogueReader

	// Under recovery (limits.Recover): unclosed reports that the input ended
	// with no closing delimiter, and cutClose that the last delimiter line was
	// cut short by the end of the input; preamble is the bytes before the
	// first delimiter line, kept up to preambleMax, and preambleOver that
	// there were more. See Framing and Preamble.
	unclosed     bool
	cutClose     bool
	preamble     []byte
	preambleMax  int64
	preambleOver bool

	// skFailed is the last failure of a framing read; see multipart_stuck.go.
	skFailed skFramingFailure

	// resumeScanned counts the bytes ResumeAfterClose searched for line
	// endings, over every call, each once (a '\r' last in a read is looked at
	// again when the next read decides it); tests hold it linear (#71).
	resumeScanned int64
}

// defaultPreambleMax is how much of a preamble a recovering reader keeps when
// SetPreambleLimit was not called.
const defaultPreambleMax = 1 << 20

// SetPreambleLimit sets how many bytes of the preamble a reader with
// HeaderLimits.Recover keeps for Preamble; the rest is read and not kept. n of
// zero or below keeps the default of 1 MiB. It must be called before the
// first NextPart.
func (r *MultipartReader) SetPreambleLimit(n int64) {
	r.preambleMax = n
}

// Preamble returns the bytes that preceded the first delimiter line, as they
// were read, line endings included — or, when no delimiter line was found,
// every byte of the input — and whether more than the limit of
// SetPreambleLimit was there, in which case the bytes returned are the first
// of them. It is empty for a reader without HeaderLimits.Recover, which keeps
// nothing. The slice is the reader's own and must not be modified.
func (r *MultipartReader) Preamble() (b []byte, truncated bool) {
	return r.preamble, r.preambleOver
}

// keepPreamble keeps one preamble line and its line ending: as many of the
// line's bytes as the limit has room for, and then its line ending only if the
// whole of it fits, so that a line that does not fit is kept cut and with no
// line ending, whether its bytes come at once or in pieces.
func (r *MultipartReader) keepPreamble(line []byte, end LineEnding) {
	r.keepPreambleBytes(line)
	r.keepPreambleEnd(end)
}

// preambleLimit is how many preamble bytes are kept; see SetPreambleLimit.
func (r *MultipartReader) preambleLimit() int64 {
	if r.preambleMax <= 0 {
		return defaultPreambleMax
	}
	return r.preambleMax
}

// keepPreambleBytes keeps b, bytes of a preamble line's content, as far as the
// limit has room for them, and notes when it has not.
func (r *MultipartReader) keepPreambleBytes(b []byte) {
	limit := r.preambleLimit()
	room := limit - int64(len(r.preamble))
	if int64(len(b)) > room {
		r.preambleOver = true
		if room > 0 {
			r.preamble = append(r.preamble, b[:room]...)
		}
		return
	}
	r.preamble = pfGrow(r.preamble, len(b), limit)
	r.preamble = append(r.preamble, b...)
}

// keepPreambleEnd keeps a preamble line's ending, none for 0, if the whole of
// it fits, and otherwise notes that it does not.
func (r *MultipartReader) keepPreambleEnd(end LineEnding) {
	var term string
	switch end {
	case LineEndingCRLF:
		term = "\r\n"
	case LineEndingLF:
		term = "\n"
	case LineEndingCR:
		term = "\r"
	}
	if term == "" {
		return
	}
	limit := r.preambleLimit()
	if int64(len(term)) > limit-int64(len(r.preamble)) {
		r.preambleOver = true
		return
	}
	r.preamble = pfGrow(r.preamble, len(term), limit)
	r.preamble = append(r.preamble, term...)
}

// A MultipartFraming is how the iteration over a multipart has gone so far.
type MultipartFraming struct {
	// Parts is how many delimiter lines introduced a part.
	Parts int
	// Closed reports that a closing delimiter line was consumed.
	Closed bool
	// Unclosed reports, under HeaderLimits.Recover, that the input ended with
	// no closing delimiter line: the last part ran to that end.
	Unclosed bool
	// CutDelimiter reports, under HeaderLimits.Recover, that the last
	// delimiter line was cut short by the end of the input and read as one.
	CutDelimiter bool
}

// Framing reports how the iteration has gone so far; see MultipartFraming.
func (r *MultipartReader) Framing() MultipartFraming {
	return MultipartFraming{Parts: r.partsRead, Closed: r.closed, Unclosed: r.unclosed && !r.closed, CutDelimiter: r.cutClose}
}

// A heldCandidate is a delimiter candidate whose name — its line ending
// before, if any, "--", the boundary and a closing "--" — has matched, and
// whose white space continues past what the buffered reader can hold.
type heldCandidate struct {
	active bool
	held   []byte // the candidate's bytes so far, its line ending before first
	elen   int    // how many of them are that line ending
	before LineEnding
	close  bool
	start  int64 // where the candidate began in the input
}

// A provenDelimiter is a delimiter line a part's body ended at.
type provenDelimiter struct {
	rest      int  // its bytes still in the buffered reader, after any held
	close     bool // a closing delimiter
	truncated bool // "--boundary" and white space, then the end of the input
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
// HeaderLimits.UnboundedFramingLines, with no line budget, removes the bound
// altogether. A framing line before the first part — a preamble line or a
// delimiter line, however much white space pads it — is matched as its bytes
// pass and is never held, so it costs one buffer whatever its length; under
// Recover the preamble it adds is kept within SetPreambleLimit as before. A
// delimiter candidate inside a body, or after a part header's malformed
// prefix or orphan record, whose name has matched and whose run of spaces and
// tabs is still undecided, is held until a byte decides it: its memory is its
// length, and only the input's length bounds it. A line that is not a
// delimiter is never held past the byte that shows it.
//
// SetHeaderLimits must be called before the first NextPart; calling it later has
// no effect on a part that has already been read.
func (r *MultipartReader) SetHeaderLimits(limits HeaderLimits) {
	r.limits = limits
}

// NextPart returns the next part in the multipart or an error.
// When there are no more parts, the error io.EOF is returned.
//
// Once NextPart has returned an EOF-class error, subsequent calls return io.EOF
// immediately. This guards callers against infinite loops on a body that ends
// without its closing boundary, whose underlying reader stays errored. An error
// is EOF-class when io.EOF or io.ErrUnexpectedEOF itself is reachable from it
// through Unwrap() error and Unwrap() []error, so a read failure wrapped by
// fmt.Errorf with %w, joined by errors.Join or wrapped by a custom Unwrap still
// is one. The walk is the bounded one HeaderLimit describes, and no Is or As
// method is consulted: an error that claims to be io.EOF only through its Is
// method is not EOF-class, and neither is one that reaches io.EOF only deeper
// or wider than the walk's bounds.
//
// A header block or a framing line that exceeded its budget ends the iteration
// the same way, for the same reason: the reader is standing inside a part whose
// extent it never learned, or on a line it could not finish. That refusal is
// recognized by identity with the one this reader made, so a refusal an input
// hands back, forged or genuine but made elsewhere, does not end it. An
// ordinary read failure does not end it, whatever that failure carries.
func (r *MultipartReader) NextPart() (*Part, error) {
	if r.done {
		return nil, io.EOF
	}
	p, err := r.nextPart()
	if err != nil && (err == r.refused || isEOFClass(err)) {
		r.done = true
	}
	return p, err
}

// isEOFClass reports whether io.EOF or io.ErrUnexpectedEOF is reachable from
// err by identity, through the bounded walk. Both targets are pointers, so the
// comparison cannot panic whatever the dynamic type of a node is.
func isEOFClass(err error) bool {
	return walkError(err, func(node error) bool {
		return node == io.EOF || node == io.ErrUnexpectedEOF
	})
}

// readFramingLine reads the next line of multipart framing before the first
// part — a preamble line, or the first delimiter line — and returns its
// content without its line ending, which ending that was (0 when the input
// ended or failed first), and where the line began.
//
// A line ends at "\r\n", even split across reads, at a bare '\n', or at a bare
// '\r', which the byte after it decides; a '\r' that io.EOF follows is a line
// ending, one another failure follows is part of the line that failure cut.
// The buffered reader is only ever asked for one byte more than it holds, never
// more than its buffer, so a bufio.ErrBufferFull it returns is the input's own
// failure and is returned as such.
//
// With no line budget a line must end within one buffer of the reader, as it
// always had to: a line that does not fails with bufio.ErrBufferFull, the whole
// buffer consumed. With one, the line is accumulated into lineBuf until it ends
// or its content passes the budget, which returns this reader's own
// *LineLimitError saying where the line began; nothing beyond one buffer past
// the budget is retained. With HeaderLimits.UnboundedFramingLines and no
// budget, streamFramingLine reads the line instead.
//
// The returned slice is valid until the next read of the reader.
func (r *MultipartReader) readFramingLine() (line []byte, end LineEnding, start int64, err error) {
	br := r.bufReader
	start = r.offset()
	budget := r.limits.MaxLineBytes
	acc := r.lineBuf[:0]
	var data int64
	var failed error
	scanned := 0

	// take charges content to the line budget and keeps it.
	take := func(content []byte) error {
		if budget > 0 {
			if int64(len(content)) > budget-data {
				r.lineBuf = acc
				refusal := newLineLimitError(budget, start)
				r.refused = refusal
				return refusal
			}
			data += int64(len(content))
		}
		acc = append(acc, content...)
		return nil
	}

	for {
		n := br.Buffered()
		if n == 0 {
			if failed != nil {
				r.lineBuf = acc
				return acc, 0, start, failed
			}
			if _, perr := br.Peek(1); perr != nil {
				failed = perr
			}
			continue
		}
		w, _ := br.Peek(n)
		i := bytes.IndexAny(w[scanned:], "\r\n")
		if i >= 0 {
			i += scanned
		} else {
			scanned = n
		}

		var kind LineEnding
		var elen int
		switch {
		case i >= 0 && w[i] == '\n':
			kind, elen = LineEndingLF, 1
		case i >= 0 && i+1 < n && w[i+1] == '\n':
			kind, elen = LineEndingCRLF, 2
		case i >= 0 && (i+1 < n || failed == io.EOF):
			kind, elen = LineEndingCR, 1
		}
		if kind != 0 {
			if terr := take(w[:i]); terr != nil {
				return nil, 0, start, terr
			}
			br.Discard(i + elen)
			r.lineBuf = acc
			return acc, kind, start, nil
		}

		// Nothing held ends the line yet.
		if failed == nil && n < br.Size() {
			if i >= 0 {
				scanned = i
			}
			if _, perr := br.Peek(n + 1); perr != nil {
				failed = perr
			}
			continue
		}
		if failed != nil {
			// The input stopped inside the line: all of it, a trailing '\r'
			// whose next byte never came included, is what there is.
			if terr := take(w); terr != nil {
				return nil, 0, start, terr
			}
			br.Discard(n)
			r.lineBuf = acc
			return acc, 0, start, failed
		}
		// A full buffer with no line ending in it.
		if budget <= 0 {
			br.Discard(n)
			return w, 0, start, bufio.ErrBufferFull
		}
		k := n
		if i == n-1 {
			// A '\r' last: the next read decides it.
			k = n - 1
		}
		if terr := take(w[:k]); terr != nil {
			return nil, 0, start, terr
		}
		br.Discard(k)
		scanned = 0
	}
}

// delimiterLine reports whether content, a framing line without its line
// ending, is a delimiter line, and whether it is the closing one: "--", the
// boundary, "--" for the closing one, then only spaces and tabs.
func (r *MultipartReader) delimiterLine(content []byte) (ok, close bool) {
	if !bytes.HasPrefix(content, r.dashBoundary) {
		return false, false
	}
	rest := content[len(r.dashBoundary):]
	if len(rest) >= 2 && rest[0] == '-' && rest[1] == '-' {
		close = true
		rest = rest[2:]
	}
	for _, c := range rest {
		if c != ' ' && c != '\t' {
			return false, false
		}
	}
	return true, close
}

func (r *MultipartReader) nextPart() (*Part, error) {
	if r.currentPart != nil {
		r.currentPart.Close()
	}
	if string(r.dashBoundary) == "--" {
		return nil, fmt.Errorf("multipart: boundary is empty")
	}

	// A framing line the part's body scan refused is this reader's own
	// refusal, and is returned as itself: NextPart recognizes it by identity.
	if r.pending != nil {
		return nil, r.pending
	}

	// The part before ended at a delimiter its body scan proved: consume it.
	if d := r.delim; d != nil {
		r.delim = nil
		r.bufReader.Discard(d.rest)
		r.cand = heldCandidate{}
		switch {
		case d.close:
			r.markClosed()
			return nil, io.EOF
		case d.truncated:
			// "--boundary" and the end of the input: the input ended where the
			// next part should have begun. Under recovery that ends the
			// iteration as an unclosed multipart.
			if r.limits.Recover {
				r.unclosed = true
				return nil, io.EOF
			}
			return nil, fmt.Errorf("multipart: NextPart: %w", io.EOF)
		}
		return r.startPart()
	}

	// Before the first part, and after a part whose input ended or failed with
	// no delimiter, framing is read a line at a time. Before the first part, a
	// line that is not a delimiter is preamble and is skipped.
	for {
		var fl framingLine
		if r.limits.MaxLineBytes <= 0 && r.limits.UnboundedFramingLines {
			// No bound on the line at all: it is streamed and decided as it
			// goes, and keeps the preamble itself.
			fl = r.streamFramingLine()
		} else {
			line, end, start, err := r.readFramingLine()

			// A framing line past its budget is this reader's own refusal,
			// and is returned as itself rather than wrapped: it is not a
			// failure of the input, and nothing below it should be asked to
			// explain it. Identity with the refusal this reader made is what
			// identifies it, so an input that fails with ErrLineTooBig, with
			// a LineLimitError of its own — a typed nil included — or with a
			// genuine one another reader made, is the ordinary read failure
			// it is.
			if err != nil && err == r.refused {
				return nil, err
			}
			fl = framingLine{end: end, start: start, n: int64(len(line)), err: err}
			fl.ok, fl.close = r.delimiterLine(line)
			if r.limits.Recover && r.partsRead == 0 && fl.preamble() {
				r.keepPreamble(line, end)
			}
		}

		if fl.err == io.EOF && fl.ok && fl.close {
			// "--boundary--" at the end of the input needs no line ending.
			r.noteBefore(fl.start)
			r.markClosed()
			return nil, io.EOF
		}
		if fl.err != nil {
			if fl.err == io.EOF && r.limits.Recover {
				// The input ended with no closing delimiter: the iteration
				// ends, and what was read before any delimiter is kept (see
				// Framing and Preamble).
				r.unclosed = true
				return nil, io.EOF
			}
			return nil, r.skFailed.wrap(fl.err, fl.start)
		}
		if fl.ok {
			r.noteBefore(fl.start)
			r.ends.note(fl.end, fl.start+fl.n)
			if fl.close {
				r.markClosed()
				return nil, io.EOF
			}
			return r.startPart()
		}
		r.lastEnd, r.lastEndAt = fl.end, fl.start+fl.n
	}
}

// A framingLine is what one framing line read before the first part — or
// after a part the input ended — was: whether it is a delimiter line and a
// closing one, its line ending (0 when the input ended or failed first), where
// it began, how many content bytes it had, and the input's failure or io.EOF
// that ended it, if one did.
type framingLine struct {
	ok, close bool
	end       LineEnding
	start     int64
	n         int64
	err       error
}

// preamble reports whether, under recovery and before the first part, the
// line is kept as preamble: a line that is not a delimiter line, or one with
// content the end of the input cut short that is not a closing delimiter line.
// A line a failure of the input cut is not kept.
func (fl framingLine) preamble() bool {
	if fl.err == nil {
		return !fl.ok
	}
	return fl.err == io.EOF && fl.n > 0 && !(fl.ok && fl.close)
}

// streamFramingLine reads the next framing line as readFramingLine does, with
// the same line endings, failures and offsets, but without holding it: each
// byte is matched against the delimiter line grammar of delimiterLine as it
// passes and then consumed, so a line of any length costs one buffer of the
// reader. It is how a reader with HeaderLimits.UnboundedFramingLines and no
// line budget reads framing lines.
//
// Under recovery, before the first part, the line's bytes are kept as preamble
// as they pass, within SetPreambleLimit as keepPreamble keeps them, and taken
// back if the line turns out to be one framing.preamble does not keep.
func (r *MultipartReader) streamFramingLine() framingLine {
	br := r.bufReader
	fl := framingLine{start: r.offset()}
	m := delimLineMatcher{db: r.dashBoundary}
	keep := r.limits.Recover && r.partsRead == 0
	mark, overMark := len(r.preamble), r.preambleOver
	var failed error

	take := func(b []byte) {
		m.feed(b)
		fl.n += int64(len(b))
		if keep {
			r.keepPreambleBytes(b)
		}
		br.Discard(len(b))
	}

	for fl.end == 0 {
		n := br.Buffered()
		if n == 0 {
			if failed != nil {
				fl.err = failed
				break
			}
			if _, perr := br.Peek(1); perr != nil {
				failed = perr
			}
			continue
		}
		w, _ := br.Peek(n)
		i := bytes.IndexAny(w, "\r\n")
		switch {
		case i < 0:
			// No line ending here: all of it is content.
			take(w)
		case w[i] == '\n':
			take(w[:i])
			br.Discard(1)
			fl.end = LineEndingLF
		case i+1 < n && w[i+1] == '\n':
			take(w[:i])
			br.Discard(2)
			fl.end = LineEndingCRLF
		case i+1 < n || failed == io.EOF:
			take(w[:i])
			br.Discard(1)
			fl.end = LineEndingCR
		default:
			// A '\r' last: the next byte decides it. A failure other than
			// io.EOF leaves it content of the line that failure cut.
			take(w[:i])
			if failed != nil {
				b, _ := br.Peek(1)
				take(b)
				continue
			}
			if _, perr := br.Peek(2); perr != nil {
				failed = perr
			}
		}
	}

	fl.ok, fl.close = m.result()
	if keep {
		if fl.preamble() {
			r.keepPreambleEnd(fl.end)
		} else {
			r.preamble, r.preambleOver = r.preamble[:mark], overMark
		}
	}
	return fl
}

// A delimLineMatcher matches the content of a framing line, fed to it in
// pieces, against delimiterLine's grammar: "--", the boundary, "--" for the
// closing one, then only spaces and tabs. It holds no byte of the line.
type delimLineMatcher struct {
	db    []byte // "--boundary"
	i     int    // how much of db has matched, while in dlName
	state uint8
	close bool
}

const (
	dlName  uint8 = iota // matching db
	dlAfter              // db matched, nothing after it yet
	dlDash               // db and one '-'
	dlPad                // db, any "--", and spaces and tabs
	dlFail               // not a delimiter line
)

func (m *delimLineMatcher) feed(b []byte) {
	for len(b) > 0 {
		switch m.state {
		case dlFail:
			return
		case dlName:
			k := min(len(b), len(m.db)-m.i)
			if !bytes.Equal(b[:k], m.db[m.i:m.i+k]) {
				m.state = dlFail
				return
			}
			m.i += k
			b = b[k:]
			if m.i == len(m.db) {
				m.state = dlAfter
			}
		case dlAfter:
			switch b[0] {
			case '-':
				m.state = dlDash
			case ' ', '\t':
				m.state = dlPad
			default:
				m.state = dlFail
				return
			}
			b = b[1:]
		case dlDash:
			if b[0] != '-' {
				m.state = dlFail
				return
			}
			m.state, m.close = dlPad, true
			b = b[1:]
		case dlPad:
			for _, c := range b {
				if c != ' ' && c != '\t' {
					m.state = dlFail
					return
				}
			}
			return
		}
	}
}

// result reports what delimiterLine reports of the content fed so far.
func (m *delimLineMatcher) result() (ok, close bool) {
	switch m.state {
	case dlAfter:
		return true, false
	case dlPad:
		return true, m.close
	}
	return false, false
}

// noteBefore records, for a delimiter line found by reading framing lines, the
// line ending of the line before it, if a line came before it.
func (r *MultipartReader) noteBefore(start int64) {
	if r.lastEnd != 0 && r.lastEndAt < start {
		r.ends.note(r.lastEnd, r.lastEndAt)
	}
	r.lastEnd = 0
}

// markClosed records that a closing delimiter line, its line ending included,
// has just been consumed, and where the input after it begins.
func (r *MultipartReader) markClosed() {
	if !r.closed {
		r.closed = true
		r.epilogueAt = r.offset()
	}
}

// Epilogue returns the input that follows this reader's closing delimiter line
// — RFC 2046's epilogue, or whatever else follows it — together with the
// offset of its first byte, relative to the first byte of the exact input this
// reader was given on the terms of Part.HeaderOffset. ok is false, and nothing
// else is returned, until NextPart has returned io.EOF because it consumed a
// closing delimiter: "--", the boundary, "--", any spaces and tabs and the
// line's own ending — "\r\n", '\n' or '\r', the longest that matches — or the
// end of the input. An iteration that ended any other way has no epilogue: an
// input that ended or failed before a closing delimiter, whose last part runs
// to that end, a body with no delimiter at all, or a refused framing line.
//
// Calling Epilogue reads nothing and changes nothing. Every call after the
// closing delimiter returns the same reader, which is the one way to read on:
// it delivers what this reader's buffer still holds and then reads the input
// this reader was given, until that input ends. It neither interprets nor
// bounds what it delivers, and stops at nothing but the end of that input:
// when the input is a part of an enclosing multipart, that is the part's own
// end, never a byte beyond it. Errors are the input's own, returned as they
// came, with the bytes that came with them. The reader is not safe for
// concurrent use, and bytes it delivered cannot be read again.
//
// RFC 2046 says the epilogue is to be ignored, and this reader does not
// change what NextPart returns.
func (r *MultipartReader) Epilogue() (rd io.Reader, offset int64, ok bool) {
	if !r.closed {
		return nil, 0, false
	}
	if r.epilogue == nil {
		r.epilogue = &epilogueReader{mr: r}
	}
	return r.epilogue, r.epilogueAt, true
}

// An epilogueReader reads the input after a closing delimiter: the reader's
// buffer first, then its input through it. It implements io.Reader alone, so a
// copy cannot reach around it to the buffered reader's WriteTo.
type epilogueReader struct {
	mr *MultipartReader
}

// Read implements io.Reader.
func (e *epilogueReader) Read(p []byte) (int, error) {
	return e.mr.bufReader.Read(p)
}

// startPart begins the part a delimiter line just consumed introduces.
func (r *MultipartReader) startPart() (*Part, error) {
	r.partsRead++
	bp, err := newPart(r)
	if err != nil {
		return nil, err
	}
	r.currentPart = bp
	return bp, nil
}

// isBoundaryDelimiterLine reports whether line, a framing line with its line
// ending, is a delimiter line that is not the closing one.
func (mr *MultipartReader) isBoundaryDelimiterLine(line []byte) bool {
	for _, e := range []string{"\r\n", "\n", "\r"} {
		if bytes.HasSuffix(line, []byte(e)) {
			ok, close := mr.delimiterLine(line[:len(line)-len(e)])
			return ok && !close
		}
	}
	return false
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
//
// A header whose block began with a UTF-16 or UTF-32 byte order mark is refused
// before anything is written or changed, as WriteHeader refuses it; see
// Header.ByteOrderMark. The parts already written stand.
func (w *MultipartWriter) CreatePartWithBuffer(header Header, b *bytes.Buffer) (io.Writer, error) {
	if header.bom.wide() {
		return nil, errWideHeader(header.bom)
	}
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
