package smime

import "bytes"

// An Armor is the text form a CMS object arrived in.
type Armor uint8

const (
	// ArmorNone is binary BER or DER.
	ArmorNone Armor = iota
	// ArmorPEM is a "-----BEGIN ...-----" block.
	ArmorPEM
	// ArmorBase64 is bare Base64 text: a part whose transfer decoding left
	// another layer of Base64, as some mailers send.
	ArmorBase64
)

// String returns "der", "pem" or "base64".
func (a Armor) String() string {
	switch a {
	case ArmorPEM:
		return "pem"
	case ArmorBase64:
		return "base64"
	}
	return "der"
}

const (
	b64Invalid = -1
	b64Space   = -2
)

var b64Table = func() (t [256]int8) {
	for i := range t {
		t[i] = b64Invalid
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	for i := 0; i < len(alphabet); i++ {
		t[alphabet[i]] = int8(i)
	}
	for _, c := range []byte(" \t\r\n") {
		t[c] = b64Space
	}
	return t
}()

// skipBlank drops leading white space and a UTF-8 byte order mark.
func skipBlank(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		b = b[3:]
	}
	for len(b) > 0 && b64Table[b[0]] == b64Space {
		b = b[1:]
	}
	return b
}

// decodeBase64 decodes the Base64 text at the start of b, skipping white
// space, up to the padding or the first byte that is not Base64, and returns
// what follows. A final group of two or three characters is decoded as far as
// it goes. The output is never larger than the input.
func decodeBase64(b []byte) (out, rest []byte) {
	out = make([]byte, 0, len(b)/4*3+3)
	var acc uint32
	n := 0
	i := 0
	for ; i < len(b); i++ {
		v := b64Table[b[i]]
		if v == b64Space {
			continue
		}
		if v < 0 {
			break
		}
		acc = acc<<6 | uint32(v)
		n++
		if n == 4 {
			out = append(out, byte(acc>>16), byte(acc>>8), byte(acc))
			acc, n = 0, 0
		}
	}
	switch n {
	case 2:
		out = append(out, byte(acc>>4))
	case 3:
		out = append(out, byte(acc>>10), byte(acc>>2))
	}
	for i < len(b) && (b[i] == '=' || b64Table[b[i]] == b64Space) {
		i++
	}
	return out, b[i:]
}

var (
	pemBegin = []byte("-----BEGIN")
	pemEnd   = []byte("-----END")
)

// dearmor returns the BER the input holds: the input itself when it starts
// with a SEQUENCE, or what its PEM or Base64 text decodes to — more than once
// if it was encoded more than once. ok is false when no layer starts like a
// CMS object.
func (p *parser) dearmor(in []byte) (ber []byte, armor Armor, ok bool) {
	for round := 0; ; round++ {
		s := skipBlank(in)
		if len(s) == 0 {
			return nil, armor, false
		}
		if s[0] == 0x30 {
			return s, armor, true
		}
		if round == maxArmorRounds {
			return nil, armor, false
		}
		body := s
		isPEM := bytes.HasPrefix(s, pemBegin)
		if isPEM {
			nl := bytes.IndexByte(s, '\n')
			if nl < 0 {
				return nil, armor, false
			}
			body = s[nl+1:]
		} else if b64Table[s[0]] < 0 {
			return nil, armor, false
		}
		dec, rest := decodeBase64(body)
		if len(dec) == 0 {
			return nil, armor, false
		}
		rest = skipBlank(rest)
		if isPEM && bytes.HasPrefix(rest, pemEnd) {
			if nl := bytes.IndexByte(rest, '\n'); nl >= 0 {
				rest = skipBlank(rest[nl+1:])
			} else {
				rest = nil
			}
		}
		switch {
		case round > 0:
			p.issue(CodeArmorNested, Warning, -1-round, "the object was encoded as text more than once")
		case isPEM:
			armor = ArmorPEM
			p.issue(CodeArmorPEM, Info, -1, "PEM wrapper removed")
		default:
			armor = ArmorBase64
			p.issue(CodeArmorBase64, Info, -1, "Base64 text decoded")
		}
		if len(rest) > 0 {
			p.issue(CodeArmorTrailing, Warning, -1-round, "text follows the encoded object")
		}
		in = dec
	}
}
