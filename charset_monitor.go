package message

import (
	"bytes"
	"io"
	"strings"
	"unicode/utf8"
)

// A utf8Monitor passes a text body declared UTF-8 through unchanged and
// reports, once, a byte sequence that is not UTF-8 (charset.invalid_sequence,
// ROB-080-003). A UTF-16 or UTF-32 byte order mark at its start is looked for
// under the charset hook, before this monitor sees a byte (charset.bom_mismatch;
// see charset_bom.go).
//
// The policy for such a body is to keep it: every byte is delivered as it was
// read, the diagnostic says the text is not what it claims, and the caller
// decides whether to replace, re-decode or drop. Nothing is replaced here,
// because a replacement cannot be undone and the declaration may simply be
// wrong about a body in another charset. The optional charset plugin's
// decoders, which do convert, replace what they cannot decode with U+FFFD and
// go on.
//
// It holds at most three bytes, the beginning of a sequence a read cut.
type utf8Monitor struct {
	r   io.Reader
	ctx parseContext

	// kind says what the body declared, and so what is reported; see
	// cdMonitorKind.
	kind cdMonitorKind

	ended    bool
	invalid  bool
	carry    [utf8.UTFMax]byte
	carryLen int
}

func (m *utf8Monitor) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	if n > 0 {
		m.validate(p[:min(n, len(p))])
	}
	if err != nil && !m.ended {
		m.ended = true
		if m.carryLen > 0 && !m.invalid {
			// The body ended inside a sequence.
			m.noteInvalid()
		}
	}
	return n, err
}

// validate checks the next bytes of the body as UTF-8.
func (m *utf8Monitor) validate(b []byte) {
	if m.invalid {
		return
	}
	// A sequence the last bytes cut: complete it from these.
	for m.carryLen > 0 && len(b) > 0 {
		m.carry[m.carryLen] = b[0]
		m.carryLen++
		b = b[1:]
		if utf8.FullRune(m.carry[:m.carryLen]) {
			// A width of one is what tells a sequence that is not UTF-8 from an
			// encoded U+FFFD, which is text (ROB-120-001).
			if r, size := utf8.DecodeRune(m.carry[:m.carryLen]); r == utf8.RuneError && size <= 1 {
				m.noteInvalid()
				return
			}
			m.carryLen = 0
		} else if m.carryLen == len(m.carry) {
			m.noteInvalid()
			return
		}
	}
	for len(b) > 0 {
		if b[0] < utf8.RuneSelf {
			b = b[1:]
			continue
		}
		if !utf8.FullRune(b) {
			m.carryLen = copy(m.carry[:], b)
			return
		}
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			m.noteInvalid()
			return
		}
		b = b[size:]
	}
}

func (m *utf8Monitor) noteInvalid() {
	m.invalid = true
	m.carryLen = 0
	if m.kind != cdMonitorUTF8 && m.ctx.fatalErr() != nil {
		// The parse was stopped, and a body it cut short — in the middle of a
		// character, or of a base64 group — says nothing about its charset.
		return
	}
	d := Diagnostic{
		Code:     CodeCharsetInvalidSequence,
		Severity: SeverityInfo,
		Message:  "a body declared UTF-8 holds bytes that are not UTF-8",
		Field:    "Content-Type",
		Action:   "kept every byte as it was read; nothing was replaced",
	}
	switch m.kind {
	case cdMonitorUndeclared:
		d.Code = CodeCharsetMissing
		d.Message = "a text body declares no charset and holds bytes that are neither US-ASCII nor UTF-8"
	case cdMonitorASCII:
		d.Code = CodeCharsetDeclarationSuspect
		d.Message = "a text body declared US-ASCII holds bytes that are neither US-ASCII nor UTF-8"
	case cdMonitorHealed:
		d.Message = "a text body read as UTF-8 on the evidence of its first bytes holds bytes that are not UTF-8"
	}
	m.ctx.recordBody(d)
}

// A cdMonitorKind is which body a utf8Monitor watches. The parser delivers all
// of them as they were read, and a byte sequence that is not UTF-8 means
// something different in each:
//
//   - declared utf-8: charset.invalid_sequence, the declaration is wrong about
//     some bytes;
//   - no charset declared: charset.missing at SeverityInfo, so that a caller
//     who named no fallback charset still learns that the text is undecoded
//     bytes in a charset nothing names (with ParseOptions.FallbackCharset the
//     same code is SeverityRecovered and the body is decoded);
//   - declared us-ascii: charset.declaration_suspect at SeverityInfo, likewise;
//   - read as UTF-8 against a legacy declaration (cdDecideUTF8):
//     charset.invalid_sequence, with a message that says so.
//
// Valid UTF-8 under no declaration or under us-ascii is not reported: it is
// delivered as it was read, which is what it is.
//
// The two SeverityInfo observations are the healing profiles': ProfileStrict
// watches only a body declared utf-8, as it always did. An entity with no
// Content-Type field at all — text/plain only by default — is watched only
// when a fallback charset is configured: nothing declares its bytes to be
// text. And a parse that was stopped reports neither for the body it cut.
type cdMonitorKind uint8

const (
	cdMonitorUTF8 cdMonitorKind = iota
	cdMonitorUndeclared
	cdMonitorASCII
	cdMonitorHealed
)

// watchUTF8 returns r with a utf8Monitor over it for a text body of this
// parse that the parser delivers as it was read — one that declares the
// charset utf-8 or us-ascii, or none — and r itself otherwise.
func (c parseContext) watchUTF8(charset string, r io.Reader) io.Reader {
	if c.session == nil {
		return r
	}
	// Only the labels charsetReader passes through without a decoder.
	lc := strings.ToLower(charset)
	if lc == "utf-8" {
		return &utf8Monitor{r: r, ctx: c}
	}
	if c.session.Profile() == ProfileStrict {
		// ProfileStrict reports what it always did.
		return r
	}
	switch lc {
	case "":
		return &utf8Monitor{r: r, ctx: c, kind: cdMonitorUndeclared}
	case "us-ascii":
		return &utf8Monitor{r: r, ctx: c, kind: cdMonitorASCII}
	}
	return r
}

// cdWatchHealed is watchUTF8 for a body that is read as UTF-8 although it
// declares another charset.
func (c parseContext) cdWatchHealed(r io.Reader) io.Reader {
	return &utf8Monitor{r: r, ctx: c, kind: cdMonitorHealed}
}

// A cdUTF8Scan is what the beginning of a body says about being UTF-8. It is
// fed the same growing window again and again and examines each byte once, so
// that a look taken a byte at a time costs what one taken whole does.
//
// Every byte above 0x7F is either part of a well-formed multi-byte sequence —
// as unicode/utf8 defines one: no overlong form, no surrogate, nothing above
// U+10FFFF — or it is ill-formed, and each ill-formed byte counts as one.
type cdUTF8Scan struct {
	at    int  // bytes of the window examined
	lead  int  // ASCII bytes before the first byte above 0x7F, as far as examined
	multi int  // well-formed multi-byte sequences
	long  int  // those of them that are three or four bytes long
	good  int  // bytes inside well-formed multi-byte sequences
	bad   int  // ill-formed bytes
	shift bool // an ESC, SO or SI was seen: ISO 2022 shifting
}

// scan examines w from where the last call stopped. A sequence that the end
// of w cuts is left for the next call, unless final says nothing follows: then
// its bytes are ill-formed.
func (s *cdUTF8Scan) scan(w []byte, final bool) {
	i := s.at
	for i < len(w) {
		c := w[i]
		if c < utf8.RuneSelf {
			if c == 0x1b || c == 0x0e || c == 0x0f {
				s.shift = true
			}
			i++
			if s.multi == 0 && s.bad == 0 {
				s.lead = i
			}
			continue
		}
		if !final && !utf8.FullRune(w[i:]) {
			break
		}
		if r, size := utf8.DecodeRune(w[i:]); r == utf8.RuneError && size == 1 {
			s.bad++
			i++
		} else {
			s.multi++
			s.good += size
			if size >= 3 {
				s.long++
			}
			i += size
		}
	}
	s.at = i
}

// cdScanWindow scans a whole sniff window: w is the first bytes of a body, at
// most charsetSniff of them. A sequence cut at the end of a full window is no
// evidence either way; a window the body ended in has no such excuse.
func cdScanWindow(w []byte) cdUTF8Scan {
	var s cdUTF8Scan
	s.scan(w, len(w) < charsetSniff)
	return s
}

// mostlyUTF8 reports whether the window is UTF-8 as far as the fallback
// charset is concerned: it holds no ill-formed byte, or the bytes inside its
// well-formed multi-byte sequences are more than cdFallbackMajority times its
// ill-formed bytes. See "Fallback charset" below.
func (s *cdUTF8Scan) mostlyUTF8() bool {
	return s.bad == 0 || s.good > cdFallbackMajority*s.bad
}

// cdFallbackMajority is how many times the bytes of well-formed multi-byte
// UTF-8 sequences must outnumber the ill-formed bytes for a damaged body to
// stay UTF-8 when a fallback charset is configured.
const cdFallbackMajority = 2

// cdSuspectSequences is how many well-formed multi-byte UTF-8 sequences, with
// no ill-formed byte beside them, make a body declared in a legacy charset
// UTF-8 instead. One could be an accident; see cdDecideUTF8.
const cdSuspectSequences = 2

// Fallback charset (ROB-080-007).
//
// Mail that declares no charset, or declares UTF-8 or US-ASCII and is neither,
// is common wherever a legacy charset was the local default: EUC-KR,
// Shift_JIS, windows-1252. The parser cannot know which, and does not guess.
// A caller that knows where its mail comes from can say:
// ParseOptions.FallbackCharset names the charset such a text body is decoded
// with. It is used only on evidence — the beginning of the body is not UTF-8
// — and it is reported (charset.declaration_suspect, or charset.missing for a
// body that declared none), so the choice is never silent.
//
// The evidence is the sniff window, the first charsetSniff bytes of the body
// after its transfer encoding was removed (all of it when it is shorter; a
// sequence the end of a full window cuts is left out). The body is UTF-8, and
// the fallback is not applied, when the window
//
//   - holds no ill-formed byte, which includes a body that is all ASCII, or
//   - is mostly UTF-8: the bytes inside its well-formed multi-byte sequences
//     are more than twice its ill-formed bytes (cdFallbackMajority).
//
// The second rule is for UTF-8 that something damaged on the way — a broken
// quoted-printable escape, a byte a gateway dropped: decoding all of it as
// EUC-KR for one bad byte turns every character into another. Text really in a
// legacy charset is far on the other side of that line: its bytes fall into
// well-formed UTF-8 sequences only by accident (roughly one byte in five for
// EUC-KR and GBK, fewer for Shift_JIS and the single-byte charsets), so its
// ill-formed bytes outnumber the others several times over. A damaged body
// that stays UTF-8 is delivered as it was read and watched like any other
// (charset.invalid_sequence, charset.missing or charset.declaration_suspect at
// SeverityInfo, by what it declared).

// charsetSniff is how many bytes of a body are looked at to decide.
const charsetSniff = 4096

const (
	fallbackDeclaredAction = "decoded the body with ParseOptions.FallbackCharset instead of the declared charset"
	fallbackMissingAction  = "decoded the body with ParseOptions.FallbackCharset"
)

// sniffFallback returns the reader of a text body that declares the charset
// declared — empty, UTF-8 or US-ASCII — for a parse that named a fallback
// charset: r itself, watched as usual, when the body's first bytes are UTF-8
// by the rule above, and r decoded with the fallback when they are not. The decision
// reads at most charsetSniff bytes ahead, on the first read of the body, and
// nothing before it.
func (c parseContext) sniffFallback(declared string, r io.Reader) io.Reader {
	fb := c.session.fallbackCharset()
	if fb == "" || strings.EqualFold(fb, "utf-8") || strings.EqualFold(fb, "us-ascii") {
		return c.watchUTF8(declared, r)
	}
	return &fallbackReader{ctx: c, declared: declared, fallback: fb, src: r}
}

// A fallbackReader decides, at its first read, whether its body is read as it
// is or through the fallback charset's decoder.
type fallbackReader struct {
	ctx      parseContext
	declared string
	fallback string
	src      io.Reader
	r        io.Reader // set once decided
}

func (f *fallbackReader) Read(p []byte) (int, error) {
	if f.r == nil {
		f.decide()
	}
	return f.r.Read(p)
}

func (f *fallbackReader) decide() {
	head := make([]byte, charsetSniff)
	n, err := io.ReadFull(f.src, head)
	head = head[:n]
	var rest io.Reader = f.src
	if err != nil {
		// The body ended, or failed, within the look: what was read, then
		// that end.
		if err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
		rest = &endReader{err: err}
	}
	raw := io.MultiReader(bytes.NewReader(head), rest)

	// ProfileStrict keeps the rule it had: any ill-formed byte in the window.
	scan := cdScanWindow(head)
	if scan.bad == 0 || f.ctx.session.Profile() != ProfileStrict && scan.mostlyUTF8() {
		f.r = f.ctx.watchUTF8(f.declared, raw)
		return
	}
	decoded, derr := charsetReader(f.fallback, raw)
	if derr != nil {
		// No decoder for the fallback: the body stays as it is.
		f.r = f.ctx.watchUTF8(f.declared, raw)
		return
	}
	d := Diagnostic{
		Code:     CodeCharsetDeclarationSuspect,
		Severity: SeverityRecovered,
		Message:  "a text body declared " + strconvQuote(f.declared) + " is not UTF-8",
		Field:    "Content-Type",
		Action:   fallbackDeclaredAction,
	}
	if f.declared == "" {
		d.Code, d.Action = CodeCharsetMissing, fallbackMissingAction
		d.Message = "a text body declares no charset and is not UTF-8"
	}
	f.ctx.recordBody(d)
	f.r = decoded
}

// An endReader returns its error.
type endReader struct{ err error }

func (e *endReader) Read([]byte) (int, error) { return 0, e.err }

func strconvQuote(s string) string {
	return "\"" + strings.ReplaceAll(s, "\"", "'") + "\""
}

// fallbackCharset returns ParseOptions.FallbackCharset, empty for none and
// for no session.
func (s *ParseSession) fallbackCharset() string {
	if s == nil {
		return ""
	}
	return s.cfg.fallbackCharset
}
