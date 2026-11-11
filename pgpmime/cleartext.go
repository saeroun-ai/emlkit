package pgpmime

import (
	"bytes"
	"strings"
)

// Cleartext is a cleartext signed message (RFC 9580 §7): readable text
// followed by the signature over it.
type Cleartext struct {
	// Hash are the hash names the Hash armor headers give, in order, as
	// written. RFC 9580 makes the header optional.
	Hash []string
	// Text is the signed text as a reader sees it: the quoting prefix and
	// the dash-escaping removed, lines joined by "\n", trailing white space
	// kept as received, and no line ending after the last line.
	Text []byte
	// Signature is the PGP SIGNATURE block that follows the text, or nil
	// when it is missing.
	Signature *Block
}

// Canonical returns the bytes the signature was computed over: Text with
// the trailing spaces and tabs of every line removed and the lines joined
// by CR LF, without a line ending after the last line (RFC 9580 §7.2).
func (c *Cleartext) Canonical() []byte {
	out := make([]byte, 0, len(c.Text)+bytes.Count(c.Text, []byte{'\n'}))
	rest := c.Text
	for {
		i := bytes.IndexByte(rest, '\n')
		l := rest
		if i >= 0 {
			l = rest[:i]
		}
		out = append(out, trimRightSpace(l)...)
		if i < 0 {
			return out
		}
		out = append(out, '\r', '\n')
		rest = rest[i+1:]
	}
}

// stripPrefix removes the block's quoting prefix from a line of its text,
// or the prefix without its trailing white space, which is how clients
// quote an empty line.
func stripPrefix(l []byte, prefix string) []byte {
	if prefix == "" {
		return l
	}
	if bytes.HasPrefix(l, []byte(prefix)) {
		return l[len(prefix):]
	}
	if p := strings.TrimRight(prefix, " \t"); bytes.HasPrefix(l, []byte(p)) {
		return l[len(p):]
	}
	return l
}

// cleartext reads the Hash headers, text and signature block of the
// cleartext signed message b, whose BEGIN line ended before pos.
func (s *state) cleartext(in []byte, b *Block, pos int) int {
	c := &Cleartext{}
	b.Cleartext = c
	pos = s.headers(in, b, pos, true)
	for _, h := range b.Headers {
		if !strings.EqualFold(h.Key, "Hash") {
			continue
		}
		for _, n := range strings.Split(h.Value, ",") {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			c.Hash = append(c.Hash, n)
			if _, ok := HashByName(n); !ok {
				s.add(&b.Issues, CodeHashUnknown, -1, "Hash header names the unknown hash %q", truncate([]byte(n), 32))
			}
		}
	}
	var text []byte
	first, unescaped := true, false
	b.End = int64(pos)
	for pos < len(in) {
		end, next := line(in, pos)
		if label, _, _, ok := beginAt(in, pos, end); ok {
			if t, _, _ := armorType(label); t == ArmorSignature {
				sig := s.newBlock(in, pos, quoteLen(in[pos:end]), label, true)
				next = s.armored(in, sig, next)
				c.Signature, c.Text = sig, text
				b.End, b.Ended = sig.End, sig.Ended
				return next
			}
			c.Text = text
			s.add(&b.Issues, CodeSignatureMissing, int64(pos), "the signed text is followed by %q, not a signature", label)
			return pos
		}
		l := stripPrefix(in[pos:end], b.Prefix)
		if len(l) > 0 && l[0] == '-' {
			if len(l) > 1 && l[1] == ' ' {
				l = l[2:]
			} else if !unescaped {
				unescaped = true
				s.add(&b.Issues, CodeUnescapedDash, int64(pos), "a line of the signed text starts with an unescaped dash")
			}
		}
		if !first {
			text = append(text, '\n')
		}
		text = append(text, l...)
		first = false
		b.End = int64(end)
		pos = next
	}
	c.Text = text
	s.add(&b.Issues, CodeSignatureMissing, b.End, "the signed text has no signature block")
	return pos
}
