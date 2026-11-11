package message

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Byte order marks against the declared charset (ROB-080-004).
//
// A text body that begins with a byte order mark names its own encoding, and
// sometimes that is not the charset its Content-Type declares: UTF-16 sent
// with charset=utf-8 or with the sender's legacy charset, UTF-8 with a BOM
// labelled euc-kr or utf-16, little-endian UTF-16 labelled utf-16be. Under
// Parse the first bytes of every text body whose charset stage runs — one that
// declares a charset the CharsetReader accepted, or none at all — are looked
// at for a mark when the decoder the declaration chose first reads them,
// before anything is delivered, and a mark the declaration contradicts is
// reported once as charset.bom_mismatch, naming the declared charset and the
// mark found. The CharsetReader is not asked about the declaration: the
// parser knows the labels of the Unicode encodings (bomClassLabels), and any
// other label is a charset without a mark of its own.
//
// What agrees with its declaration is not reported: a UTF-8 mark under
// utf-8, either UTF-16 mark under utf-16 (which consumes it), the mark of the
// declared byte order under utf-16le, utf-16be, utf-32le and utf-32be (where
// it is text, U+FEFF), and either UTF-32 mark under utf-32. Neither is a body
// without a mark.
//
// The declaration is never silently replaced, and the header is never
// changed. A conflict is healed — the body decoded as its mark says — only
// when all of these hold, and the diagnostic is then SeverityRecovered:
//
//   - the profile is not ProfileStrict;
//   - the bytes after the mark, as far as bomWindow of them, are valid in the
//     mark's encoding (a cut sequence at the end of that window is no evidence
//     either way). For FF FE 00 00, which begins both UTF-32LE and UTF-16LE
//     text that starts with U+0000, that is what decides which mark it is;
//   - there is a decoder: UTF-8 is delivered by the parser itself, as it
//     delivers a body declared utf-8 (the mark kept, the bytes watched for
//     invalid sequences); UTF-16 and UTF-32 are decoded by the CharsetReader
//     under the labels utf-16 and utf-32, which with the charset plugin use
//     the mark for the byte order and consume it.
//
// Otherwise the body is decoded as declared, mark included, and the
// diagnostic is SeverityInfo with an Action that says why. A UTF-8 mark under
// us-ascii or no charset at all is that too: the parser delivers such a body as
// it was read, which already is UTF-8, so there is nothing to heal.
//
// Read, ReadWithOptions and New look at nothing and report nothing, as before.
// A CharsetReader that reads its input while it builds its decoder gets that
// input unexamined: nothing is held back from it, and nothing is reported.
// Under ReadOptions.SkipTextAttachmentDecoding a text body that declares no
// charset is looked at only when ParseOptions.FallbackCharset is set, so that
// whether it is an attachment is not asked for this alone.

// bomWindow is how many bytes after a mark the declaration contradicts are
// looked at, before anything is delivered, for evidence that the body is in
// the mark's encoding.
const bomWindow = 1024

// A bomClass is what a declared charset label says about byte order marks.
type bomClass uint8

const (
	bomClassOther bomClass = iota // a charset with no mark of its own
	bomClassASCII                 // no charset, or us-ascii: delivered as read
	bomClassUTF8
	bomClassUTF16 // byte order from the mark, or from the text
	bomClassUTF16LE
	bomClassUTF16BE
	bomClassUTF32 // byte order from the mark, else big-endian
	bomClassUTF32LE
	bomClassUTF32BE
)

// bomClassLabels are the labels of the Unicode encodings, as the charset
// plugin spells them. Any other label is bomClassOther.
var bomClassLabels = map[string]bomClass{}

func init() {
	for class, labels := range map[bomClass]string{
		bomClassUTF8:    "utf-8 utf8 utf_8 x-utf-8 x-utf8 x-utf_8j utf8mb3 utf8mb4 unicode-1-1-utf-8 unicode-2-0-utf-8 csutf8",
		bomClassUTF16:   "utf-16 utf16 csutf16 unicode ucs-2 ucs2 iso-10646-ucs-2 csunicode unicodefeff",
		bomClassUTF16LE: "utf-16le utf16le utf-16-le ucs-2le x-utf-16le csutf16le",
		bomClassUTF16BE: "utf-16be utf16be utf-16-be ucs-2be x-utf-16be csutf16be",
		bomClassUTF32:   "utf-32 utf32 csutf32 ucs-4 ucs4 iso-10646-ucs-4 csucs4",
		bomClassUTF32LE: "utf-32le utf32le csutf32le",
		bomClassUTF32BE: "utf-32be utf32be csutf32be",
	} {
		for _, l := range strings.Fields(labels) {
			bomClassLabels[l] = class
		}
	}
}

// bomClassOf returns the class of a declared charset label, empty for none.
// Only what the parser itself delivers as read — no label, and us-ascii — is
// bomClassASCII. Decoration the charset plugin also ignores — case, quotes,
// white space, and anything from a ";", ",", "*" or space on — is ignored.
func bomClassOf(label string) bomClass {
	lc := strings.ToLower(label)
	if lc == "" || lc == "us-ascii" {
		return bomClassASCII
	}
	lc = strings.Trim(lc, " \t\"'")
	if i := strings.IndexAny(lc, " \t;,*\"'"); i >= 0 {
		lc = lc[:i]
	}
	return bomClassLabels[lc]
}

// accepts reports whether a body declared with this class may begin with m.
func (c bomClass) accepts(m textproto.ByteOrderMark) bool {
	switch c {
	case bomClassUTF8:
		return m == textproto.BOMUTF8
	case bomClassUTF16:
		return m == textproto.BOMUTF16LE || m == textproto.BOMUTF16BE
	case bomClassUTF16LE:
		return m == textproto.BOMUTF16LE
	case bomClassUTF16BE:
		return m == textproto.BOMUTF16BE
	case bomClassUTF32:
		return m == textproto.BOMUTF32LE || m == textproto.BOMUTF32BE
	case bomClassUTF32LE:
		return m == textproto.BOMUTF32LE
	case bomClassUTF32BE:
		return m == textproto.BOMUTF32BE
	}
	return false
}

// bomMarks are the marks, longest first, so that FF FE 00 00 is read as
// UTF-32LE before it is read as UTF-16LE.
var bomMarks = [...]struct {
	m textproto.ByteOrderMark
	b string
}{
	{textproto.BOMUTF32BE, "\x00\x00\xfe\xff"},
	{textproto.BOMUTF32LE, "\xff\xfe\x00\x00"},
	{textproto.BOMUTF8, "\xef\xbb\xbf"},
	{textproto.BOMUTF16BE, "\xfe\xff"},
	{textproto.BOMUTF16LE, "\xff\xfe"},
}

// bomLeading returns the mark b begins with, reading FF FE 00 00 as UTF-32LE.
func bomLeading(b []byte) textproto.ByteOrderMark {
	for _, k := range bomMarks {
		if len(b) >= len(k.b) && string(b[:len(k.b)]) == k.b {
			return k.m
		}
	}
	return textproto.BOMNone
}

// bomLen returns the length of the mark m, or zero for BOMNone.
func bomLen(m textproto.ByteOrderMark) int {
	for _, k := range bomMarks {
		if k.m == m {
			return len(k.b)
		}
	}
	return 0
}

// bomValid reports whether rest, the bytes after the mark m, are valid in m's
// encoding as far as they go. A unit or sequence cut at their end is not
// counted against them.
func bomValid(m textproto.ByteOrderMark, rest []byte) bool {
	switch m {
	case textproto.BOMUTF8:
		for i := 1; i < utf8.UTFMax && i <= len(rest); i++ {
			if utf8.RuneStart(rest[len(rest)-i]) {
				if !utf8.FullRune(rest[len(rest)-i:]) {
					rest = rest[:len(rest)-i]
				}
				break
			}
		}
		return utf8.Valid(rest)
	case textproto.BOMUTF16BE, textproto.BOMUTF16LE:
		unit := func(i int) uint16 {
			if m == textproto.BOMUTF16BE {
				return uint16(rest[i])<<8 | uint16(rest[i+1])
			}
			return uint16(rest[i+1])<<8 | uint16(rest[i])
		}
		for i := 0; i+1 < len(rest); i += 2 {
			switch u := unit(i); {
			case u >= 0xdc00 && u <= 0xdfff:
				return false // a low surrogate alone
			case u >= 0xd800 && u <= 0xdbff:
				if i+3 >= len(rest) {
					return true // the pair is cut
				}
				if v := unit(i + 2); v < 0xdc00 || v > 0xdfff {
					return false
				}
				i += 2
			}
		}
		return true
	case textproto.BOMUTF32BE, textproto.BOMUTF32LE:
		for i := 0; i+3 < len(rest); i += 4 {
			r := uint32(rest[i])<<24 | uint32(rest[i+1])<<16 | uint32(rest[i+2])<<8 | uint32(rest[i+3])
			if m == textproto.BOMUTF32LE {
				r = uint32(rest[i+3])<<24 | uint32(rest[i+2])<<16 | uint32(rest[i+1])<<8 | uint32(rest[i])
			}
			if r > utf8.MaxRune || r >= 0xd800 && r <= 0xdfff {
				return false
			}
		}
		return true
	}
	return false
}

// A bomHold is the input of a text body's charset stage under Parse. The
// look happens when the decoder the declaration chose first reads its input,
// so a decoder that reads nothing makes the look take nothing either: the
// first bytes are read into the hold — at most 4+bomWindow of them — and
// decide whether the body is decoded as declared, from those bytes again, or
// as its mark says.
type bomHold struct {
	ctx     parseContext
	leaf    *leafBody // whose counter the charset stage reads through, or nil
	charset string
	class   bomClass

	src     io.Reader // the body's bytes before the charset stage
	view    *bomView  // what the declared decoder reads
	buf     []byte    // the bytes looked at, read again by whichever decoder reads on
	err     error     // what src returned while they were read, returned after them
	armed   bool      // the body has been read: the look may be taken
	decided bool
	healed  io.Reader // the body decoded as its mark says, when it is
}

// bomRedirected is what the declared decoder's input returns once the body
// is decoded as its mark says instead; that decoder is not read again.
var bomRedirected = errors.New("message: the body is decoded as its byte order mark says")

// A bomView reads a bomHold from its beginning.
type bomView struct {
	h        *bomHold
	declared bool // the declared decoder's input, whose first read looks
	off      int
}

func (v *bomView) Read(p []byte) (int, error) {
	h := v.h
	if v.declared {
		if !h.decided {
			if !h.armed {
				// Read while the decoder was built, before the body was:
				// nothing is held back from it, and nothing is looked at.
				h.decided = true
			} else if !h.look(len(p)) {
				// The source returned nothing: so does this.
				return 0, nil
			}
		}
		if h.healed != nil {
			return 0, bomRedirected
		}
	}
	if v.off < len(h.buf) {
		n := copy(p, h.buf[v.off:])
		v.off += n
		return n, nil
	}
	if h.err != nil {
		return 0, h.err
	}
	return h.src.Read(p)
}

// holdBOM returns the input of a text body's charset stage: r, held for the
// look at its first bytes and counted for leaf, and the hold to build the body
// with. Outside Parse it is r counted, and no hold.
func (c parseContext) holdBOM(leaf *leafBody, r io.Reader) (io.Reader, *bomHold) {
	if c.session == nil {
		return leaf.countCharsetInput(r), nil
	}
	h := &bomHold{ctx: c, leaf: leaf, src: r}
	h.view = &bomView{h: h, declared: true}
	return leaf.countCharsetInput(h.view), h
}

// body returns the reader of a text body that declares charset (empty for
// none) and whose charset stage reads h: declared, the reader that stage
// built, unless the body's first bytes say otherwise. A nil h returns
// declared.
func (h *bomHold) body(charset string, declared io.Reader) io.Reader {
	if h == nil {
		return declared
	}
	h.charset, h.class = charset, bomClassOf(charset)
	return &bomBody{h: h, declared: declared}
}

// A bomBody reads its text body as declared, or, once the look has decided
// so, as its byte order mark says.
type bomBody struct {
	h        *bomHold
	declared io.Reader
	r        io.Reader // set once decided
}

func (b *bomBody) Read(p []byte) (int, error) {
	if b.r != nil {
		return b.r.Read(p)
	}
	h := b.h
	h.armed = true
	n, err := b.declared.Read(p)
	if !h.decided {
		return n, err
	}
	if h.healed == nil {
		b.r = b.declared
		return n, err
	}
	// The declared decoder was given nothing of the body.
	b.r = h.healed
	return b.r.Read(p)
}

// look reads the first bytes of the body into the hold and decides. Until a
// mark is possible it asks the source for as many bytes as the decoder asked
// it for, want, and at least the four a mark needs, so that what the source
// hands on in one read is taken whole, and no more than that is taken ahead
// of the decoder. It returns false when a read of the source returned nothing
// and no error.
func (h *bomHold) look(want int) bool {
	if h.buf == nil {
		h.buf = make([]byte, 0, 4+bomWindow)
	}
	for h.err == nil && !h.enough() {
		end := cap(h.buf)
		if len(h.buf) < 4 {
			end = min(end, max(4, len(h.buf)+want))
		}
		p := h.buf[len(h.buf):end]
		n, err := h.src.Read(p)
		n = max(0, min(n, len(p)))
		h.buf = h.buf[:len(h.buf)+n]
		h.err = err
		if n == 0 && err == nil {
			return false
		}
	}
	h.decided = true
	h.decide()
	return true
}

// enough reports whether the hold has what the decision needs: four bytes,
// enough for any mark, and when the declaration does not accept the mark they
// begin with, bomWindow bytes after it.
func (h *bomHold) enough() bool {
	if len(h.buf) == cap(h.buf) {
		return true
	}
	if len(h.buf) < 4 {
		return false
	}
	m := bomLeading(h.buf)
	return m == textproto.BOMNone || h.class.accepts(m)
}

// decide sets h.healed when the body is to be decoded as its mark says, and
// reports a mark its declaration contradicts.
func (h *bomHold) decide() {
	buf := h.buf
	m := bomLeading(buf)
	if m == textproto.BOMUTF32LE && !h.class.accepts(m) && !bomValid(m, buf[4:]) {
		// U+FEFF U+0000 in UTF-16LE rather than a UTF-32LE mark.
		m = textproto.BOMUTF16LE
	}
	if m == textproto.BOMNone || h.class.accepts(m) {
		return
	}
	valid := bomValid(m, buf[bomLen(m):])

	declared := fmt.Sprintf("%q", h.charset)
	d := Diagnostic{
		Code:     CodeCharsetBOMMismatch,
		Severity: SeverityInfo,
		Message:  fmt.Sprintf("a text body declared %s begins with a %v byte order mark", declared, m),
		Field:    "Content-Type",
	}
	if h.charset == "" {
		declared = "US-ASCII"
		d.Message = fmt.Sprintf("a text body that declares no charset, so US-ASCII, begins with a %v byte order mark", m)
	}
	encoding, label := "UTF-8", "utf-8"
	switch m {
	case textproto.BOMUTF16BE, textproto.BOMUTF16LE:
		encoding, label = "UTF-16", "utf-16"
	case textproto.BOMUTF32BE, textproto.BOMUTF32LE:
		encoding, label = "UTF-32", "utf-32"
	}
	asDeclared := "decoded the body as " + declared + ", the mark included"

	switch {
	case h.class == bomClassASCII && m == textproto.BOMUTF8:
		// Delivered as read, which is how UTF-8 is delivered too.
		d.Action = "delivered the bytes as they were read, as for " + declared + ", the mark included: that is UTF-8"
		if !valid {
			d.Action = "delivered the bytes as they were read, as for " + declared + ", the mark included; the bytes after the mark are not UTF-8"
		}
	case h.ctx.session.Profile() == ProfileStrict:
		d.Action = asDeclared + ": ProfileStrict does not decode by the mark"
	case !valid:
		d.Action = asDeclared + ": the bytes after the mark are not " + encoding
	case m == textproto.BOMUTF8:
		h.healed = h.ctx.watchUTF8(label, h.leaf.countCharsetInput(&bomView{h: h}))
		d.Severity = SeverityRecovered
		d.Action = "decoded the body as UTF-8, as its mark says, not as " + declared + "; the mark is kept, and the Content-Type is left as it was"
	default:
		healed, err := charsetReader(label, h.leaf.countCharsetInput(&bomView{h: h}))
		if err != nil || healed == nil {
			// The declared decoder reads on, through the counter again.
			h.leaf.countCharsetInput(h.view)
			d.Action = asDeclared + ": no CharsetReader decodes " + label
			break
		}
		h.healed = healed
		d.Severity = SeverityRecovered
		d.Action = "decoded the body as " + encoding + " in the byte order of its mark, not as " + declared + "; the mark was consumed, and the Content-Type is left as it was"
	}
	h.ctx.recordBody(d)
}
