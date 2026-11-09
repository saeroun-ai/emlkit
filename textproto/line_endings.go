package textproto

import (
	"math"
	"strconv"
)

// A LineEnding names one way a physical line of a header block ended.
//
// The zero value names none of them: it is not a line ending, and every
// LineEndings method given it, or any value other than the three constants
// below, reports nothing.
type LineEnding uint8

const (
	// LineEndingCRLF is "\r\n", the line ending RFC 5322 requires. A "\r" and
	// a "\n" that arrive in different reads are still one CRLF.
	LineEndingCRLF LineEnding = iota + 1

	// LineEndingLF is a "\n" with no "\r" before it.
	LineEndingLF

	// LineEndingCR is a "\r" that is not followed by "\n" and that ended a
	// line; see ReadHeader for when a "\r" ends a line and when it is kept as
	// content.
	LineEndingCR
)

// String returns "CRLF", "LF" or "CR", or a "LineEnding(n)" placeholder for a
// value that names none of them.
func (k LineEnding) String() string {
	switch k {
	case LineEndingCRLF:
		return "CRLF"
	case LineEndingLF:
		return "LF"
	case LineEndingCR:
		return "CR"
	}
	return "LineEnding(" + strconv.Itoa(int(k)) + ")"
}

// index returns k's slot in the per-kind arrays, and whether k names a line
// ending at all.
func (k LineEnding) index() (int, bool) {
	if k < LineEndingCRLF || k > LineEndingCR {
		return 0, false
	}
	return int(k - LineEndingCRLF), true
}

// LineEndings describes the physical line endings of one header block exactly
// as it was read from its input: how many lines ended each way, where the
// first of each kind began, and whether the input ended inside the block's
// last line, before any line ending at all. MultipartReader.DelimiterEndings
// reports the line endings of a multipart's delimiter lines with the same
// type, on the terms it states there; what follows describes a header block.
//
// It is an immutable snapshot. Header.LineEndings returns a copy of it, which
// stays what it was whatever is done afterwards: changing that copy changes no
// Header, and changing the Header changes no copy already returned. Changing
// the Header — see Header.LineEndings — does make every later call report an
// unknown LineEndings, since the header is then no longer the block read. It
// describes the input, not the fields: the raw bytes a Header stores end every
// physical line with "\r\n" whatever ended it in the input, so they cannot say
// this themselves.
//
// The zero LineEndings is unknown: Known reports false and every other method
// reports nothing. That is what a Header reports that this package did not
// read whole, or that was changed after it was read.
//
// Offsets are relative to the first byte of the block, which is where
// ReadHeaderLimits's reader stood when the call began, and Part.HeaderOffset
// for a part's header. The empty line that ends a block is a line of it, and
// its line ending is counted.
type LineEndings struct {
	known bool

	count [3]int64
	first [3]int64

	unterminated           bool
	untermStart, untermEnd int64
}

// Known reports whether l describes a header block this package read whole.
func (l LineEndings) Known() bool {
	return l.known
}

// Count returns how many lines of the block ended with k. It returns 0 for a
// value of k that names no line ending, and for an unknown l.
func (l LineEndings) Count(k LineEnding) int64 {
	i, ok := k.index()
	if !ok || !l.known {
		return 0
	}
	return l.count[i]
}

// First returns the offset of the first byte of the first line ending of kind
// k in the block, and whether there is one.
func (l LineEndings) First(k LineEnding) (offset int64, ok bool) {
	i, valid := k.index()
	if !valid || !l.known || l.count[i] == 0 {
		return 0, false
	}
	return l.first[i], true
}

// Unterminated reports whether the input ended, with io.EOF, inside the
// block's last line, before any line ending: the half-open range [start, end)
// is that line as it was read, which the block kept with "\r\n" supplied. A
// line that ends with a "\r" right before io.EOF is not unterminated; that "\r"
// is its line ending.
func (l LineEndings) Unterminated() (start, end int64, ok bool) {
	if !l.known || !l.unterminated {
		return 0, 0, false
	}
	return l.untermStart, l.untermEnd, true
}

// note records one line ending of kind k that began at offset. Counts
// saturate rather than wrap.
func (l *LineEndings) note(k LineEnding, offset int64) {
	i, ok := k.index()
	if !ok {
		return
	}
	if l.count[i] == 0 || offset < l.first[i] {
		l.first[i] = offset
	}
	if l.count[i] < math.MaxInt64 {
		l.count[i]++
	}
}

// LineEndings returns the line endings of the header block h was read from.
//
// It is known only for a Header that ReadHeader, ReadHeaderLimit,
// ReadHeaderLimits or a MultipartReader's part read whole, and for a Copy of
// one. Add, AddRaw, Set, Del and HeaderFields.Del make it unknown, because the
// header no longer is the block that was read; a Header built any other way —
// the zero value, HeaderFromMap — has none. It is safe on a nil h.
func (h *Header) LineEndings() LineEndings {
	if h == nil || h.ends == nil {
		return LineEndings{}
	}
	return *h.ends
}
