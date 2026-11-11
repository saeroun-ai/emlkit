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
	m.ctx.recordBody(Diagnostic{
		Code:     CodeCharsetInvalidSequence,
		Severity: SeverityInfo,
		Message:  "a body declared UTF-8 holds bytes that are not UTF-8",
		Field:    "Content-Type",
		Action:   "kept every byte as it was read; nothing was replaced",
	})
}

// watchUTF8 returns r with a utf8Monitor over it for a text body of this
// parse that declares the charset utf-8, and r itself otherwise.
func (c parseContext) watchUTF8(charset string, r io.Reader) io.Reader {
	if c.session == nil {
		return r
	}
	// Only the label charsetReader passes through without a decoder.
	if strings.ToLower(charset) == "utf-8" {
		return &utf8Monitor{r: r, ctx: c}
	}
	return r
}

// Fallback charset (ROB-080-007).
//
// Mail that declares no charset, or declares UTF-8 or US-ASCII and is neither,
// is common wherever a legacy charset was the local default: EUC-KR,
// Shift_JIS, windows-1252. The parser cannot know which, and does not guess.
// A caller that knows where its mail comes from can say:
// ParseOptions.FallbackCharset names the charset such a text body is decoded
// with. It is used only on evidence — the beginning of the body holds bytes
// that are not UTF-8 — and it is reported (charset.declaration_suspect, or
// charset.missing for a body that declared none), so the choice is never
// silent. A body that is valid UTF-8 as far as it was looked at is delivered
// as it is.

// charsetSniff is how many bytes of a body are looked at to decide.
const charsetSniff = 4096

const (
	fallbackDeclaredAction = "decoded the body with ParseOptions.FallbackCharset instead of the declared charset"
	fallbackMissingAction  = "decoded the body with ParseOptions.FallbackCharset"
)

// sniffFallback returns the reader of a text body that declares the charset
// declared — empty, UTF-8 or US-ASCII — for a parse that named a fallback
// charset: r itself, watched as usual, when the body's first bytes are valid
// UTF-8, and r decoded with the fallback when they are not. The decision
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

	// A sequence the look cut at its end is not evidence either way.
	probe := head
	if err == nil {
		for i := 0; i < utf8.UTFMax-1 && len(probe) > 0 && !utf8.Valid(probe); i++ {
			probe = probe[:len(probe)-1]
		}
	}
	if utf8.Valid(probe) {
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
		Message:  "a text body declared " + strconvQuote(f.declared) + " holds bytes that are not UTF-8",
		Field:    "Content-Type",
		Action:   fallbackDeclaredAction,
	}
	if f.declared == "" {
		d.Code, d.Action = CodeCharsetMissing, fallbackMissingAction
		d.Message = "a text body declares no charset and holds bytes that are not UTF-8"
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
