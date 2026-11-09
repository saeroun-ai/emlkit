package textproto

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
)

// Byte order marks (ROB-010-003).
//
// A byte order mark is the encoded U+FEFF that some programs write before
// text. In front of a header block it is not part of any field: read as one,
// it would be the first bytes of the first field's name and would make that
// name invalid. So the first bytes of every header block — the message's and
// each part's, wherever ReadHeader, ReadHeaderLimit, ReadHeaderLimits or a
// MultipartReader begins one — are checked for one, and only those: a U+FEFF
// later in a block, in a value, or in a body is content, as it always was.
//
// Which mark the block begins with is decided on the longest match, so that
// FF FE 00 00 is UTF-32LE rather than UTF-16LE followed by U+0000. The decision
// is a bounded look-ahead over the block's leading bytes: at most four are
// looked at, through the scanner's peek, and a further one only while those
// held so far still begin some mark — so an input that only begins like one,
// or is a near miss, is still looked at that far before it is known to have
// none. It comes first, before anything else about the block is decided: in
// particular before a spent shared field count (HeaderBudget) refuses the
// first field candidate, which without a mark it does at the first byte, so
// the bytes looked at may be read from the source before that refusal. Like
// any peek of a bufio.Reader, looking at them may fill the reader's buffer
// from the source. io.EOF decides on the bytes before it: FF FE and FF FE 00 at
// the end of the input are UTF-16LE. A failure other than io.EOF that comes
// before the mark is decided leaves the block without one, and those bytes are
// then read as the first line the failure cut, which reports that failure as
// itself (ROB-010-006).
//
// What follows depends on the mark:
//
//   - A UTF-8 mark is followed by text that is read like any header: the mark's
//     three bytes are consumed, and the fields are read after them. The mark is
//     bytes of the block — positions, LineEndings offsets and the MaxBytes and
//     shared byte budgets count it — but it is no content of any line or field,
//     so MaxLineBytes, MaxFieldBytes and the field counts do not. The Header
//     keeps it, and WriteHeader writes it once before the fields; the raw bytes
//     of the fields never include it. Keeping the mark is not raw replay: the
//     fields are written on WriteHeader's usual terms; see
//     Header.ByteOrderMark.
//   - A UTF-16 or UTF-32 mark is followed by text in that encoding, in which
//     the ASCII characters that frame a header — field-name letters, the colon,
//     CR and LF — are each two or four bytes wide, zero bytes among them, and
//     that this package does not read: no transcoding is implemented, and
//     stripping the mark would leave those zero bytes in the field names and
//     line endings. Nothing of the block is consumed, nothing is charged to any
//     budget, and the Header has no fields: the mark and everything after it
//     are left in the reader, the body a caller reads next. WriteHeader refuses
//     such a Header. No attempt is made to tell ASCII text after such a mark
//     from text in its encoding.
//
// Only one mark is recognized. A second one, a part of one, or a near miss is
// the beginning of the first line, read as it always has been.

// A ByteOrderMark names the byte order mark a header block began with.
//
// The zero value, BOMNone, means that the block began with none, or that the
// Header was not read from a block at all.
type ByteOrderMark uint8

const (
	// BOMNone is no byte order mark.
	BOMNone ByteOrderMark = iota

	// BOMUTF8 is EF BB BF, the UTF-8 encoding of U+FEFF.
	BOMUTF8

	// BOMUTF16BE is FE FF, the UTF-16 big-endian encoding of U+FEFF.
	BOMUTF16BE

	// BOMUTF16LE is FF FE, the UTF-16 little-endian encoding of U+FEFF.
	BOMUTF16LE

	// BOMUTF32BE is 00 00 FE FF, the UTF-32 big-endian encoding of U+FEFF.
	BOMUTF32BE

	// BOMUTF32LE is FF FE 00 00, the UTF-32 little-endian encoding of U+FEFF.
	BOMUTF32LE
)

// bomBytes are the encodings of each mark.
var bomBytes = [...]string{
	BOMUTF8:    "\xef\xbb\xbf",
	BOMUTF16BE: "\xfe\xff",
	BOMUTF16LE: "\xff\xfe",
	BOMUTF32BE: "\x00\x00\xfe\xff",
	BOMUTF32LE: "\xff\xfe\x00\x00",
}

// String returns "none", "UTF-8", "UTF-16BE", "UTF-16LE", "UTF-32BE" or
// "UTF-32LE", or a "ByteOrderMark(n)" placeholder for a value that names none
// of them.
func (m ByteOrderMark) String() string {
	switch m {
	case BOMNone:
		return "none"
	case BOMUTF8:
		return "UTF-8"
	case BOMUTF16BE:
		return "UTF-16BE"
	case BOMUTF16LE:
		return "UTF-16LE"
	case BOMUTF32BE:
		return "UTF-32BE"
	case BOMUTF32LE:
		return "UTF-32LE"
	}
	return "ByteOrderMark(" + strconv.Itoa(int(m)) + ")"
}

// Bytes returns the bytes of the mark, as a new slice on every call, or nil
// for BOMNone and for a value that names no mark.
func (m ByteOrderMark) Bytes() []byte {
	if m == BOMNone || int(m) >= len(bomBytes) {
		return nil
	}
	return []byte(bomBytes[m])
}

// wide reports whether m is a UTF-16 or UTF-32 mark: one whose block this
// package does not read.
func (m ByteOrderMark) wide() bool {
	return m >= BOMUTF16BE && m <= BOMUTF32LE
}

// ByteOrderMark returns the byte order mark the header block h was read from
// began with, or BOMNone. It is safe on a nil h.
//
// BOMUTF8 means that the mark was consumed and the fields were read after it.
// WriteHeader writes it once, before the fields, on the terms it writes any
// header: a well-formed complete block — every field read as a field, kept and
// unchanged, every line ended with CRLF, including the empty line that ends it
// — is written unchanged byte for byte, mark included; lines ended by a bare LF
// or CR are written with CRLF, a block that io.EOF ended — a mark alone, or a
// last line with no line ending or no empty line after it — gains the CRLF
// endings and the empty line WriteHeader always writes, and a line the reader
// skipped rather than kept as a field, such as one with an empty name, is not
// written. Keeping the mark is not a replay of the input.
//
// A UTF-16 or UTF-32 mark means that the block was not read: nothing of it was
// consumed, the mark and what follows it are still in the reader, and the
// Header was returned with no fields. WriteHeader refuses such a Header with
// an error before writing anything: its bytes are not the Header's to write,
// and writing the fields alone, or the mark before them, would misrepresent
// them.
//
// The mark is the Header's, unlike its LineEndings: Copy keeps it, and so do
// Add, AddRaw, Set, Del and HeaderFields.Del, which change the fields but not
// what the block began with. For a UTF-16 or UTF-32 mark that means a Header
// whose block was not read keeps saying so, and keeps being refused by
// WriteHeader, whatever fields were added to it since. There is no setter: a
// Header without the mark is built from the fields, as HeaderFromMap(h.Map())
// does, at the cost of what that representation loses — the order of the
// fields, the case of their names and their raw bytes.
func (h *Header) ByteOrderMark() ByteOrderMark {
	if h == nil {
		return BOMNone
	}
	return h.bom
}

// errWideHeader returns the error WriteHeader and MultipartWriter refuse a
// Header with whose block began with the UTF-16 or UTF-32 mark m.
func errWideHeader(m ByteOrderMark) error {
	return fmt.Errorf("textproto: cannot write a header whose block began with a %v byte order mark: the block was not read, and its bytes are not the header's to write", m)
}

// byteOrderMark decides which mark, if any, the block r stands on begins with,
// on the terms described at the top of this file. It consumes nothing.
//
// Every look goes through s.peek, so a failure is recorded as the scanner
// records any; a failure other than io.EOF before the mark is decided returns
// BOMNone, and the read of the first line that follows reports it. A wide mark
// is never returned with such a failure recorded: it is decided from bytes
// that were held, or from io.EOF.
func (s *headerScan) byteOrderMark(r *bufio.Reader) ByteOrderMark {
	// is reports whether byte i of the block is held and equals c. It looks
	// as far as byte i only, and is asked only while bytes 0 to i-1 matched.
	is := func(i int, c byte) bool {
		b, _ := s.peek(r, i+1)
		return len(b) > i && b[i] == c
	}
	// ends reports that the input ended, with io.EOF, before byte i of the
	// block.
	ends := func(i int) bool {
		b, _ := s.peek(r, i+1)
		return len(b) <= i && s.fail == io.EOF
	}

	b, _ := s.peek(r, 1)
	if len(b) == 0 {
		return BOMNone
	}
	switch b[0] {
	case 0xef:
		if is(1, 0xbb) && is(2, 0xbf) {
			return BOMUTF8
		}
	case 0xfe:
		if is(1, 0xff) {
			return BOMUTF16BE
		}
	case 0x00:
		if is(1, 0x00) && is(2, 0xfe) && is(3, 0xff) {
			return BOMUTF32BE
		}
	case 0xff:
		if !is(1, 0xfe) {
			return BOMNone
		}
		// FF FE is UTF-16LE unless 00 00 follows it, which makes it UTF-32LE.
		// Byte 2 or 3 is looked at only while that is still possible, and a
		// failure before either is known leaves the mark undecided.
		switch {
		case ends(2):
			return BOMUTF16LE
		case !is(2, 0x00):
			if b, _ := s.peek(r, 3); len(b) > 2 {
				return BOMUTF16LE
			}
			return BOMNone
		case ends(3):
			return BOMUTF16LE
		case is(3, 0x00):
			return BOMUTF32LE
		}
		if b, _ := s.peek(r, 4); len(b) > 3 {
			return BOMUTF16LE
		}
	}
	return BOMNone
}
