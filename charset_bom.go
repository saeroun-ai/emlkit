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
// changed. By default it is not replaced at all (issues #62 and #65): where
// the bytes after the mark are valid in the mark's encoding they read two
// ways, the body is decoded as declared, and the diagnostic is SeverityInfo
// with an Action that names the other encoding. A conflict is healed — the
// body decoded as its mark says — only when all of these hold, and the
// diagnostic is then SeverityRecovered:
//
//   - the caller asked for it, ParseOptions.RecoverCharsetDeclarations, and
//     the profile is not ProfileStrict, which ignores that option;
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
// A body with no mark that declares a legacy charset is checked against its
// bytes too, by the same hold: one that is UTF-8 all the same is reported
// (charset.declaration_suspect), and read as UTF-8 only under
// ParseOptions.RecoverCharsetDeclarations. See cdDecideUTF8 for the evidence
// and cdRead for what the look costs; the look is the same either way.
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

	// The look for UTF-8 under a legacy declaration; see cdDecideUTF8.
	sniff    bool // the look is on: nothing has decided it yet
	sevenBit bool // the label names a 7-bit charset, whose shifts speak for it
	scan     cdUTF8Scan
	utf8     bool // decided: the body is read as UTF-8 from cut on
	cut      int  // where the declared decoder's input ends then: the ASCII before the first 8-bit byte
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
		if h.sniff || h.utf8 {
			if n, err, done := h.cdRead(v, p); done {
				return n, err
			}
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
	// A decoder that read its input while it was built has it unexamined.
	if !h.decided && h.class == bomClassOther && h.ctx.session.Profile() != ProfileStrict {
		h.sniff, h.sevenBit = cdSuspectLabel(charset)
	}
	return &bomBody{h: h, declared: declared}
}

// A bomBody reads its text body as declared, or, once the look has decided
// so, as its byte order mark says, or as the UTF-8 it turned out to be.
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
	if h.healed != nil {
		// The declared decoder was given nothing of the body.
		b.r = h.healed
		return b.r.Read(p)
	}
	if h.utf8 {
		// The declared decoder's input ended at the cut, and what it made of
		// the ASCII before it is delivered to its end; the rest of the body
		// is read as it is. Its input is replaced only now, since it reads
		// through the same counter.
		if h.cut > 0 && err == nil {
			return n, nil
		}
		if h.cut == 0 {
			n = 0 // it was given nothing of the body
		}
		b.r = h.ctx.cdWatchHealed(h.leaf.countCharsetInput(&bomView{h: h, off: h.cut}))
		if n > 0 {
			return n, nil
		}
		return b.r.Read(p)
	}
	if !h.sniff {
		b.r = b.declared
	}
	return n, err
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

// cdSuspectLabel reports whether a text body declared with this label, which
// names no Unicode encoding, is looked at for UTF-8 (cdDecideUTF8), and
// whether the label names an ISO 2022 charset, 7-bit text that is shifted in
// and out by control characters.
//
// UTF-7 and HZ are left alone. Their shifts are printable ASCII ("+", "~{"),
// which the look takes for text, and a UTF-7 decoder keeps UTF-8 as it is
// anyway: reading such a body as UTF-8 instead would only leave its runs
// undecoded.
func cdSuspectLabel(label string) (sniff, sevenBit bool) {
	lc := strings.ToLower(label)
	switch {
	case strings.Contains(lc, "utf-7"), strings.Contains(lc, "utf7"), strings.HasPrefix(lc, "hz"):
		return false, false
	case strings.Contains(lc, "iso-2022"), strings.Contains(lc, "iso2022"):
		return true, true
	}
	return true, false
}

// cdRead is the declared decoder's read of its input while the look for UTF-8
// under a legacy declaration is on, and after it decided for UTF-8. done is
// false once the look is over and the body is decoded as declared: the caller
// then reads on as it always did.
//
// While the body is ASCII the decoder is handed what it asks for, as the bytes
// arrive, and the hold only keeps a copy: ASCII reads the same in the declared
// charset and in UTF-8, so nothing has to be decided, and nothing is read
// ahead. The first byte above 0x7F is held back, and from there the source is
// read ahead of the decoder — to the first ill-formed byte, the end of the
// sniff window or the end of the body — until cdDecideUTF8 can decide. Real
// legacy text ends the look within a character or two.
//
// When the body is UTF-8 the decoder's input ends at the cut, where the first
// such byte stands: it is handed the ASCII before it and then io.EOF — or
// bomRedirected when there is none, so that nothing it makes of an empty
// input is taken for text — and bomBody reads the rest as it is.
func (h *bomHold) cdRead(v *bomView, p []byte) (n int, err error, done bool) {
	for h.sniff {
		s := &h.scan
		s.scan(h.buf, false)
		if s.bad > 0 || h.sevenBit && s.shift {
			h.sniff = false
			break
		}
		if v.off < s.lead {
			// ASCII the decoder has not been handed yet.
			n = copy(p, h.buf[v.off:s.lead])
			v.off += n
			return n, nil, true
		}
		if h.err != nil || len(h.buf) >= charsetSniff {
			h.cdDecideUTF8()
			break
		}
		// More of the body: what the decoder asked for while all of it is
		// ASCII, and the rest of the window once a byte above 0x7F is held.
		end := charsetSniff
		if s.lead == len(h.buf) {
			end = min(end, len(h.buf)+len(p))
		}
		if end > cap(h.buf) {
			grown := make([]byte, len(h.buf), min(charsetSniff, max(end, 2*cap(h.buf))))
			copy(grown, h.buf)
			h.buf = grown
		}
		room := h.buf[len(h.buf):end]
		got, rerr := h.src.Read(room)
		got = max(0, min(got, len(room)))
		h.buf = h.buf[:len(h.buf)+got]
		h.err = rerr
		if got == 0 && rerr == nil {
			// The source returned nothing: so does this.
			return 0, nil, true
		}
	}
	if !h.utf8 {
		return 0, nil, false
	}
	if v.off < h.cut {
		n = copy(p, h.buf[v.off:h.cut])
		v.off += n
		return n, nil, true
	}
	if h.cut == 0 {
		return 0, bomRedirected, true
	}
	return 0, io.EOF, true
}

// cdDecideUTF8 decides whether a body with no byte order mark that declares a
// legacy charset — any label the CharsetReader accepted that names no Unicode
// encoding: euc-kr, ks_c_5601-1987, shift_jis, gb2312, big5, iso-2022-*,
// iso-8859-1, windows-1252 … — is UTF-8 all the same, and sets h.utf8 and
// h.cut when it is to be read so. Senders that write UTF-8 under the charset
// their software was configured with years ago are common, and the declared
// decoder turns every character of such a body into another without a single
// error.
//
// The bytes then read two ways, so by default the declaration is followed and
// the evidence only reported: charset.declaration_suspect at SeverityInfo,
// naming UTF-8. Under ParseOptions.RecoverCharsetDeclarations the body is read
// as UTF-8 instead, at SeverityRecovered; the evidence and its known false
// positives below are the same.
//
// The evidence is the sniff window: the first charsetSniff bytes of the body
// after its transfer encoding was removed, or all of it when it is shorter. A
// sequence the end of a full window cuts is left out. The body is UTF-8 by
// this rule when all of these hold:
//
//  1. The window holds no ill-formed byte: every byte above 0x7F is part of a
//     well-formed UTF-8 sequence.
//  2. It holds at least cdSuspectSequences (two) multi-byte sequences. One
//     can be an accident.
//  3. At least one of them is three or four bytes long — or, when all are two
//     bytes long, the declared decoder itself shows the bytes are not its
//     own: asked to decode the window, it returns U+FFFD, or one character
//     for every byte, which is how a single-byte charset reads anything.
//  4. Under an ISO 2022 label the window holds no ESC, SO or SI: text that
//     shifts is what it says it is.
//
// Why this is enough. Text in a double-byte charset is ill-formed UTF-8
// almost at once: of the Hangul syllables of EUC-KR about one in ten is a
// well-formed two-byte sequence by accident, and a three-byte one needs a
// hanja followed by particular syllables, so a window of real EUC-KR, GBK,
// Big5 or Shift_JIS text passes rule 1 only when it holds a handful of
// characters, all of the accidental kind. Those few — "화환" in EUC-KR is
// C8 AD C8 AF, which is also "ȭȯ" in UTF-8 — are two-byte sequences, and rule
// 3 keeps them: the declared decoder reads them as pairs and without error.
// A single-byte charset maps every byte to something, so there the UTF-8
// evidence has to stand alone, and it does: an accented letter followed by
// the one kind of symbol that completes a sequence ("Ã©"), twice, with no
// other accented letter anywhere in the window, is what UTF-8 read as
// windows-1252 looks like and not what anybody writes.
//
// What is delivered under the option: the ASCII before the first byte above
// 0x7F as the declared decoder read it — which is as anyone reads ASCII — and everything
// from that byte on as it is, watched for bytes that are not UTF-8 after all
// (charset.invalid_sequence). The header is not changed.
//
// What it costs: see cdRead. The decision is a function of the window's
// bytes alone, however the input was cut into reads. The declared decoder is
// asked about the window only for rule 3's second arm, under the no-progress
// and output bounds of a parameter value.
//
// ProfileStrict, Read, ReadWithOptions and New decode as declared and look at
// nothing, with the option or without. So does a body whose byte order mark was reported
// (charset.bom_mismatch), which has had its look.
func (h *bomHold) cdDecideUTF8() {
	h.sniff = false
	w := h.buf
	s := &h.scan
	s.scan(w, len(w) < charsetSniff)
	if s.bad > 0 || s.multi < cdSuspectSequences || h.sevenBit && s.shift {
		return
	}
	if s.long == 0 && !h.cdDeclaredMisreads(w[:s.at]) {
		return
	}
	d := Diagnostic{
		Code:     CodeCharsetDeclarationSuspect,
		Severity: SeverityInfo,
		Message: fmt.Sprintf("a text body declared %q is well-formed UTF-8: %d multi-byte sequences and no ill-formed byte in its first %d bytes",
			h.charset, s.multi, s.at),
		Field: "Content-Type",
		Action: fmt.Sprintf("decoded the body as declared, %q; its bytes also read as UTF-8, as which ParseOptions.RecoverCharsetDeclarations would decode them",
			h.charset),
	}
	if h.ctx.session.recoverCharset() {
		h.utf8, h.cut = true, s.lead
		d.Severity = SeverityRecovered
		d.Action = "decoded the body as UTF-8, not as declared; the Content-Type is left as it was"
	}
	h.ctx.recordBody(d)
}

// cdDeclaredMisreads reports whether the declared charset's decoder reads w —
// well-formed UTF-8 whose multi-byte sequences are all two bytes long — as
// bytes that are not its own: with a U+FFFD, or as one character for every
// byte. A decoder that fails, stalls or overruns says nothing, and the body
// is then decoded as declared.
func (h *bomHold) cdDeclaredMisreads(w []byte) bool {
	dec := &metadataDecoder{poll: h.ctx.cancelPoll()}
	s, ok := dec.convertCharset(strings.ToLower(h.charset), w)
	if !ok {
		return false
	}
	return utf8.RuneCountInString(s) == len(w) || strings.ContainsRune(s, utf8.RuneError)
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
		// With no mark, a legacy declaration is still checked against the
		// bytes as they are read; see cdRead.
		return
	}
	h.sniff = false
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
	case !h.ctx.session.recoverCharset():
		// Two readings, and the declaration is the one followed (#62, #65).
		d.Action = asDeclared + "; the bytes after the mark are also valid " + encoding +
			", as which ParseOptions.RecoverCharsetDeclarations would decode them"
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
