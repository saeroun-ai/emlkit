// Package legacyenc finds and, on request, decodes the pre-MIME encodings a
// text body may carry inline: BinHex 4.0 (RFC 1741), yEnc, uuencode (also its
// "begin-base64" form) and xxencode.
//
// The core parser never calls this package. A message that says
// Content-Transfer-Encoding: x-uuencode is decoded by the core; a block of
// uuencoded lines inside an ordinary text/plain body is text to the core, and
// stays so. Sniff and Scan are a cheap signal for a consumer that wants to
// know such a block is there; Decode and DecodeAll turn a block into bytes
// within budgets. See docs/robustness-notes/legacy-encodings.md
// (ROB-140-006).
//
// Everything a block declares — a file name, a mode, a size, a CRC, a
// BinHex type and creator — comes from the input and is untrusted. A name is
// not a safe path.
//
// Nothing here panics, loops or allocates without bound on any input: Scan is
// linear in the bytes it scans, each decoder is linear in its block and keeps
// at most Limits.MaxBlockBytes, and DecodeAll at most Limits.MaxTotalBytes.
package legacyenc

import "fmt"

// A Kind is an inline encoding.
type Kind uint8

const (
	// KindNone is no block.
	KindNone Kind = iota
	// KindUUEncode is "begin MODE NAME", uuencoded lines, "end".
	KindUUEncode
	// KindXXEncode is uuencode's framing with the xxencode alphabet.
	KindXXEncode
	// KindUUBase64 is "begin-base64 MODE NAME", Base64 lines, "====".
	KindUUBase64
	// KindYEnc is "=ybegin", an optional "=ypart", yEnc lines, "=yend".
	KindYEnc
	// KindBinHex is the BinHex 4.0 banner and the data between two colons.
	KindBinHex
)

// String returns the lower-case name of the kind.
func (k Kind) String() string {
	switch k {
	case KindNone:
		return "none"
	case KindUUEncode:
		return "uuencode"
	case KindXXEncode:
		return "xxencode"
	case KindUUBase64:
		return "uuencode-base64"
	case KindYEnc:
		return "yenc"
	case KindBinHex:
		return "binhex"
	}
	return fmt.Sprintf("Kind(%d)", uint8(k))
}

// A Code names what an Issue found.
type Code uint8

const (
	// CodeMissingEnd: the block has no closing line ("end", "====", "=yend",
	// the closing colon); it runs to its last encoded line.
	CodeMissingEnd Code = iota + 1
	// CodeLineTooLong: a line of the block is longer than
	// Limits.MaxLineBytes; only that many bytes of it were read.
	CodeLineTooLong
	// CodeScanLimit: the body is longer than Limits.MaxScanBytes; the rest
	// was not scanned.
	CodeScanLimit
	// CodeBlockLimit: the body holds more blocks than Limits.MaxBlocks; the
	// rest were not reported.
	CodeBlockLimit
	// CodeBudget: the block decodes to more than its budget
	// (Limits.MaxBlockBytes, or what MaxTotalBytes left); decoding stopped
	// there.
	CodeBudget
	// CodeTotalLimit: Limits.MaxTotalBytes was spent on earlier blocks; this
	// one was not decoded.
	CodeTotalLimit
	// CodeTruncated: the encoded data ended before what it declares, or
	// inside a group.
	CodeTruncated
	// CodeSizeMismatch: the decoded size differs from a declared size.
	CodeSizeMismatch
	// CodeCRCMismatch: a CRC differs from the one declared. The data is
	// returned all the same.
	CodeCRCMismatch
	// CodeInvalidChar: a character outside the encoding's alphabet was
	// skipped (uuencode, xxencode: read as zero).
	CodeInvalidChar
	// CodeInvalidLine: a line of a uuencoded or xxencoded block has no valid
	// length character and was skipped.
	CodeInvalidLine
	// CodeShortLine: a uuencoded or xxencoded line holds fewer characters
	// than its length character needs, as when a relay strips trailing
	// spaces; it was decoded as if padded with zeros.
	CodeShortLine
	// CodeLineJunk: a uuencoded or xxencoded line holds more than its length
	// character needs (a checksum character, junk); the rest was ignored.
	CodeLineJunk
	// CodeBadHeader: a header is malformed: a yEnc "=ybegin" or "=yend"
	// field does not parse, a BinHex name length is 0 or over 63.
	CodeBadHeader
	// CodePartRange: yEnc part, total and range fields disagree.
	CodePartRange
	// CodeDanglingEscape: yEnc data ends with an escape character.
	CodeDanglingEscape
	// CodeBadRun: a BinHex run-length marker repeats nothing.
	CodeBadRun
	// CodeTrailingData: BinHex data continues after the resource fork's
	// CRC; it was ignored.
	CodeTrailingData
)

var codeNames = [...]string{
	CodeMissingEnd:     "missing_end",
	CodeLineTooLong:    "line_too_long",
	CodeScanLimit:      "scan_limit",
	CodeBlockLimit:     "block_limit",
	CodeBudget:         "budget",
	CodeTotalLimit:     "total_limit",
	CodeTruncated:      "truncated",
	CodeSizeMismatch:   "size_mismatch",
	CodeCRCMismatch:    "crc_mismatch",
	CodeInvalidChar:    "invalid_char",
	CodeInvalidLine:    "invalid_line",
	CodeShortLine:      "short_line",
	CodeLineJunk:       "line_junk",
	CodeBadHeader:      "bad_header",
	CodePartRange:      "part_range",
	CodeDanglingEscape: "dangling_escape",
	CodeBadRun:         "bad_run",
	CodeTrailingData:   "trailing_data",
}

// String returns the code's snake-case name.
func (c Code) String() string {
	if int(c) < len(codeNames) && codeNames[c] != "" {
		return codeNames[c]
	}
	return fmt.Sprintf("Code(%d)", uint8(c))
}

// An Issue is one thing scanning or decoding found wrong. Each code is
// reported once per block, at its first place, except CRC and size
// mismatches, which are reported for each value checked.
type Issue struct {
	Code Code
	// Offset is the byte offset in the body where it was found, or -1.
	Offset int
	// Detail says what was found and what was done.
	Detail string
}

func (i Issue) String() string {
	if i.Offset < 0 {
		return fmt.Sprintf("%s: %s", i.Code, i.Detail)
	}
	return fmt.Sprintf("%s at %d: %s", i.Code, i.Offset, i.Detail)
}

// Limits bounds Scan and the decoders. A field that is zero or negative takes
// its default.
type Limits struct {
	// MaxBlocks is how many blocks Scan reports per body. Default 32.
	MaxBlocks int
	// MaxScanBytes is how many bytes of a body Scan reads. Default 16 MiB.
	MaxScanBytes int
	// MaxLineBytes is how many bytes of one line are read. Default 8 KiB.
	MaxLineBytes int
	// MaxBlockBytes is how many decoded bytes one block may yield, both
	// BinHex forks together. Default 8 MiB.
	MaxBlockBytes int64
	// MaxTotalBytes is how many decoded bytes DecodeAll yields for one body.
	// Default 32 MiB.
	MaxTotalBytes int64
}

// Default limits.
const (
	DefaultMaxBlocks     = 32
	DefaultMaxScanBytes  = 16 << 20
	DefaultMaxLineBytes  = 8 << 10
	DefaultMaxBlockBytes = 8 << 20
	DefaultMaxTotalBytes = 32 << 20
)

func (l *Limits) withDefaults() Limits {
	var o Limits
	if l != nil {
		o = *l
	}
	if o.MaxBlocks <= 0 {
		o.MaxBlocks = DefaultMaxBlocks
	}
	if o.MaxScanBytes <= 0 {
		o.MaxScanBytes = DefaultMaxScanBytes
	}
	if o.MaxLineBytes <= 0 {
		o.MaxLineBytes = DefaultMaxLineBytes
	}
	if o.MaxBlockBytes <= 0 {
		o.MaxBlockBytes = DefaultMaxBlockBytes
	}
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = DefaultMaxTotalBytes
	}
	return o
}

// A Block is one encoded block Scan found in a body.
type Block struct {
	Kind Kind
	// Start and End are the half-open byte range of the body the block
	// occupies: from the start of its opening line (banner, "begin",
	// "=ybegin") to the end of its closing line and that line's ending, or,
	// with no closing line, to the end of its last encoded line.
	Start, End int
	// Quote is the quoting prefix ("> ", ">> ") the block's lines carry, which
	// was stripped from each; empty for an unquoted block.
	Quote string
	// Name is the file name the block declares, as it declares it, or empty.
	// It is untrusted input, not a safe path.
	Name string
	// Mode is the octal mode of a "begin" line, or -1.
	Mode int
	// Size is the decoded size the block declares for its own content, or -1:
	// yEnc's "=yend size", else its part range, else "=ybegin size" of a
	// single part; the data-fork length of a BinHex header.
	Size int64
	// Complete reports that the block's closing line was found.
	Complete bool
	// YEnc is the yEnc header fields, for a yEnc block.
	YEnc *YEncHeader
	// BinHex is the BinHex header, for a BinHex block whose header could be
	// decoded.
	BinHex *BinHexHeader
	// Issues are what scanning found: a missing closing line, an over-long
	// line, inconsistent yEnc fields.
	Issues []Issue

	// dataStart and dataEnd are the range of the body that holds the
	// encoded lines.
	dataStart, dataEnd int
}

// YEncHeader is what a yEnc block's "=ybegin", "=ypart" and "=yend" lines
// declare. Absent numbers are 0, absent sizes -1.
type YEncHeader struct {
	// Line is the declared line length.
	Line int64
	// Size is "=ybegin size": the whole file's size.
	Size int64
	// Part and Total number a multi-part file's parts.
	Part, Total int64
	// PartBegin and PartEnd are the "=ypart" range of the file this part
	// holds, 1-based and inclusive.
	PartBegin, PartEnd int64
	// EndSize and EndPart are "=yend size" and "=yend part".
	EndSize, EndPart int64
	// CRC32 is "=yend crc32", the whole file's CRC-32; PartCRC32 is
	// "=yend pcrc32", this part's.
	CRC32, PartCRC32       uint32
	HasCRC32, HasPartCRC32 bool
}

// BinHexHeader is the header of a BinHex 4.0 stream.
type BinHexHeader struct {
	// Name is the declared Macintosh file name; untrusted.
	Name    string
	Version byte
	// Type and Creator are the Finder type and creator codes.
	Type, Creator [4]byte
	Flags         uint16
	// DataLength and ResourceLength are the declared fork lengths.
	DataLength, ResourceLength uint32
	// CRC is the declared header CRC; CRCValid reports that it matches.
	CRC      uint16
	CRCValid bool
}

// A Result is one decoded block.
type Result struct {
	// Block is the block as Scan found it; for BinHex, Block.BinHex is the
	// header the decoder read.
	Block Block
	// Data is the decoded content; for BinHex, the data fork.
	Data []byte
	// Resource is the BinHex resource fork; nil for other kinds.
	Resource []byte
	// Truncated reports that Data (and Resource) is less than the block
	// declares or holds: a budget stopped decoding, or the input ended
	// first.
	Truncated bool
	// Issues are what decoding found. The block's own scanning issues are in
	// Block.Issues.
	Issues []Issue
}

// SniffBytes is how much of its argument Sniff reads.
const SniffBytes = 64 << 10

// Sniff reports the kind of the first block in prefix — the start of a text
// body — or KindNone. It reads at most SniffBytes and decodes nothing. It
// reports a block exactly when Scan would: an opening line confirmed by the
// line after it, so the prefix must reach that line.
func Sniff(prefix []byte) Kind {
	if len(prefix) > SniffBytes {
		prefix = prefix[:SniffBytes]
	}
	lim := (*Limits)(nil).withDefaults()
	s := &scanner{b: prefix, lim: lim, sniff: true}
	for pos := 0; pos < len(s.b); {
		end, next := s.line(pos)
		if blk, _, ok := s.block(pos, end, next); ok {
			return blk.Kind
		}
		pos = next
	}
	return KindNone
}

// Scan finds the encoded blocks in a text body, in order, within lim (nil for
// the defaults). The second result holds what limited the scan itself.
//
// A block opens on a line, after any quoting prefix, that is a BinHex 4.0
// banner followed by a line starting with ':'; "=ybegin" with a size or name;
// "begin-base64 MODE NAME" followed by a Base64 line; or "begin MODE NAME"
// followed by a whole uuencoded or xxencoded line. Prose that merely begins
// with "begin" is not a block. Lines may end in CRLF, LF or CR.
func Scan(body []byte, lim *Limits) ([]Block, []Issue) {
	l := lim.withDefaults()
	return scan(body, l)
}

// Decode decodes block b of body, keeping at most lim.MaxBlockBytes. b must
// come from Scan of the same body.
func Decode(body []byte, b Block, lim *Limits) Result {
	l := lim.withDefaults()
	return decode(body, b, l, l.MaxBlockBytes)
}

// DecodeAll scans body and decodes each block it finds, keeping at most
// lim.MaxBlockBytes per block and lim.MaxTotalBytes in all. It returns one
// Result per block, and the scan's own issues.
func DecodeAll(body []byte, lim *Limits) ([]Result, []Issue) {
	l := lim.withDefaults()
	blocks, issues := scan(body, l)
	results := make([]Result, 0, len(blocks))
	left := l.MaxTotalBytes
	for _, b := range blocks {
		if left <= 0 {
			r := Result{Block: b, Truncated: true}
			r.Issues = append(r.Issues, Issue{CodeTotalLimit, b.Start, fmt.Sprintf("the body's %d decoded bytes were spent on earlier blocks; not decoded", l.MaxTotalBytes)})
			results = append(results, r)
			continue
		}
		r := decode(body, b, l, min(l.MaxBlockBytes, left))
		left -= int64(len(r.Data) + len(r.Resource))
		results = append(results, r)
	}
	return results, issues
}

func decode(body []byte, b Block, l Limits, budget int64) Result {
	r := Result{Block: b}
	start, end := max(0, min(b.dataStart, len(body))), max(0, min(b.dataEnd, len(body)))
	if end < start {
		end = start
	}
	d := &decoder{
		body:   body,
		quote:  []byte(b.Quote),
		pos:    start,
		end:    end,
		maxLn:  l.MaxLineBytes,
		budget: budget,
		res:    &r,
	}
	d.iss.list = &r.Issues
	switch b.Kind {
	case KindUUEncode:
		d.uu(&uuAlphabet)
	case KindXXEncode:
		d.uu(&xxAlphabet)
	case KindUUBase64:
		d.base64()
	case KindYEnc:
		d.yenc()
	case KindBinHex:
		d.binhex()
	}
	return r
}
