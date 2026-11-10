package message

import (
	"io"
	"strings"
	"unicode/utf8"
)

// A utf8Monitor passes a text body declared UTF-8 through unchanged and
// reports, once each, what contradicts the declaration (ROB-080-003,
// ROB-080-004): a byte sequence that is not UTF-8 (charset.invalid_sequence),
// and a UTF-16 or UTF-32 byte order mark at its start (charset.bom_mismatch).
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

	head     [2]byte
	headLen  int
	ended    bool
	invalid  bool
	carry    [utf8.UTFMax]byte
	carryLen int
}

func (m *utf8Monitor) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	if n > 0 {
		m.look(p[:min(n, len(p))])
	}
	if err != nil && !m.ended {
		m.ended = true
		if m.headLen < len(m.head) {
			// A body shorter than a mark.
			m.validate(m.head[:m.headLen])
		}
		if m.carryLen > 0 && !m.invalid {
			// The body ended inside a sequence.
			m.noteInvalid()
		}
	}
	return n, err
}

// look reads the next bytes of the body. The first two are held until both
// are there, so that a mark is reported before anything it makes invalid.
func (m *utf8Monitor) look(b []byte) {
	if m.headLen < len(m.head) {
		k := copy(m.head[m.headLen:], b)
		m.headLen += k
		b = b[k:]
		if m.headLen < len(m.head) {
			return
		}
		if m.head == [2]byte{0xff, 0xfe} || m.head == [2]byte{0xfe, 0xff} {
			m.ctx.recordBody(Diagnostic{
				Code:     CodeCharsetBOMMismatch,
				Severity: SeverityInfo,
				Message:  "a body declared UTF-8 begins with a UTF-16 or UTF-32 byte order mark",
				Field:    "Content-Type",
				Action:   "kept the bytes as they were read; the body is likely in the charset its mark names",
			})
		}
		m.validate(m.head[:])
	}
	m.validate(b)
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
			if r, _ := utf8.DecodeRune(m.carry[:m.carryLen]); r == utf8.RuneError {
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
