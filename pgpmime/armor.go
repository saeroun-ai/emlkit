package pgpmime

import (
	"bytes"
	"encoding/base64"
	"strconv"
	"strings"
)

// An ArmorType is the kind of an ASCII-armored block (RFC 9580 §6.2).
type ArmorType uint8

// Armor types.
const (
	ArmorUnknown       ArmorType = iota // BEGIN PGP with a label this package does not know
	ArmorMessage                        // PGP MESSAGE
	ArmorSignedMessage                  // PGP SIGNED MESSAGE, a cleartext signature
	ArmorSignature                      // PGP SIGNATURE
	ArmorPublicKey                      // PGP PUBLIC KEY BLOCK
	ArmorPrivateKey                     // PGP PRIVATE KEY BLOCK, or PGP 2's SECRET KEY BLOCK
	ArmorMessagePart                    // PGP MESSAGE, PART X/Y or PART X
)

func (t ArmorType) String() string {
	switch t {
	case ArmorMessage:
		return "message"
	case ArmorSignedMessage:
		return "signed-message"
	case ArmorSignature:
		return "signature"
	case ArmorPublicKey:
		return "public-key"
	case ArmorPrivateKey:
		return "private-key"
	case ArmorMessagePart:
		return "message-part"
	}
	return "unknown"
}

// A CRCStatus is the outcome of a block's CRC-24 checksum.
type CRCStatus uint8

// CRC outcomes.
const (
	CRCAbsent   CRCStatus = iota // no checksum line
	CRCValid                     // the checksum matches the data
	CRCMismatch                  // the checksum does not match; the data is returned anyway
	CRCInvalid                   // the checksum line is malformed
)

// A Header is one armor header, such as Version, Comment, Hash or Charset.
type Header struct {
	Key, Value string
}

// A Block is one ASCII-armored block of the input.
type Block struct {
	// Type and Label are the kind of the block and the text of its BEGIN
	// line between "BEGIN " and the closing dashes, such as "PGP MESSAGE".
	Type  ArmorType
	Label string
	// Part and Total are the X and Y of a "PGP MESSAGE, PART X/Y" block;
	// Total is 0 for "PART X". Parts are not reassembled.
	Part, Total int
	// Start is the offset of the BEGIN line in the input, its quoting prefix
	// included. End is just past the END line's last character, or where the
	// block was cut when it has none.
	Start, End int64
	// Ended reports that an END line closed the block.
	Ended bool
	// Prefix is the quoting or indentation that preceded the BEGIN line,
	// such as "> ", and was removed from the block's lines.
	Prefix string
	// Headers are the armor headers in order.
	Headers []Header
	// Data is the decoded binary content: an OpenPGP packet stream. It is
	// nil for a cleartext signature, whose text and signature are in
	// Cleartext.
	Data []byte
	// CRC is the outcome of the checksum.
	CRC CRCStatus
	// Cleartext is set for a PGP SIGNED MESSAGE block.
	Cleartext *Cleartext
	// Packets are the packets of Data, walked.
	Packets []Packet
	// Issues are what was found about this block and its packets.
	Issues []Issue
}

// Header returns the value of the first armor header with the key,
// case-insensitively, or "".
func (b *Block) Header(key string) string {
	for _, h := range b.Headers {
		if strings.EqualFold(h.Key, key) {
			return h.Value
		}
	}
	return ""
}

// line returns where the line of b that starts at pos ends, and where the
// next line starts. CR LF, LF and a lone CR each end a line.
func line(b []byte, pos int) (end, next int) {
	i := bytes.IndexAny(b[pos:], "\r\n")
	if i < 0 {
		return len(b), len(b)
	}
	end = pos + i
	if b[end] == '\r' && end+1 < len(b) && b[end+1] == '\n' {
		return end, end + 2
	}
	return end, end + 1
}

// quoteLen is the length of the quoting and indentation a mail client put
// before a line: a run of '>', spaces and tabs.
func quoteLen(l []byte) int {
	i := 0
	for i < len(l) && (l[i] == '>' || l[i] == ' ' || l[i] == '\t') {
		i++
	}
	return i
}

func trimRightSpace(l []byte) []byte {
	return bytes.TrimRight(l, " \t")
}

// parseDelim parses an armor line "-----BEGIN label-----" (or END, by kind)
// with its prefix and trailing white space removed. exact is false when the
// dashes are not five on each side.
func parseDelim(s []byte, kind string) (label string, exact, ok bool) {
	n := 0
	for n < len(s) && s[n] == '-' {
		n++
	}
	if n < 3 || n > 10 {
		return "", false, false
	}
	rest := s[n:]
	if len(rest) <= len(kind) || string(rest[:len(kind)]) != kind || rest[len(kind)] != ' ' {
		return "", false, false
	}
	rest = rest[len(kind)+1:]
	m := 0
	for m < len(rest) && rest[len(rest)-1-m] == '-' {
		m++
	}
	lab := bytes.TrimSpace(rest[:len(rest)-m])
	if len(lab) == 0 || len(lab) > 80 {
		return "", false, false
	}
	return string(lab), n == 5 && m == 5 && len(lab) == len(rest)-m, true
}

func isPGPLabel(label string) bool {
	return len(label) >= 4 && strings.EqualFold(label[:4], "PGP ")
}

// armorType classifies a BEGIN label.
func armorType(label string) (t ArmorType, part, total int) {
	u := strings.ToUpper(label)
	switch u {
	case "PGP MESSAGE":
		return ArmorMessage, 0, 0
	case "PGP SIGNED MESSAGE":
		return ArmorSignedMessage, 0, 0
	case "PGP SIGNATURE":
		return ArmorSignature, 0, 0
	case "PGP PUBLIC KEY BLOCK":
		return ArmorPublicKey, 0, 0
	case "PGP PRIVATE KEY BLOCK", "PGP SECRET KEY BLOCK":
		return ArmorPrivateKey, 0, 0
	}
	if rest, ok := strings.CutPrefix(u, "PGP MESSAGE, PART "); ok {
		x, y, slash := strings.Cut(rest, "/")
		p, err1 := strconv.Atoi(x)
		q, err2 := 0, error(nil)
		if slash {
			q, err2 = strconv.Atoi(y)
		}
		if err1 == nil && err2 == nil && p > 0 && p <= 1<<20 && q >= 0 && q <= 1<<20 && (!slash || q > 0) {
			return ArmorMessagePart, p, q
		}
	}
	return ArmorUnknown, 0, 0
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// beginAt reports whether the line [pos,end) of in is a BEGIN PGP line, and
// returns its label, quoting prefix length and exactness. A byte order mark
// before the line counts as part of its prefix.
func beginAt(in []byte, pos, end int) (label string, q int, exact, ok bool) {
	if bytes.HasPrefix(in[pos:end], utf8BOM) {
		q = len(utf8BOM)
	}
	q += quoteLen(in[pos+q : end])
	l := trimRightSpace(in[pos+q : end])
	if len(l) == 0 || l[0] != '-' {
		return "", 0, false, false
	}
	label, exact, ok = parseDelim(l, "BEGIN")
	if !ok || !isPGPLabel(label) {
		return "", 0, false, false
	}
	return label, q, exact, true
}

// findBlocks finds and decodes every armored block of in.
func (s *state) findBlocks(in []byte, issues *[]Issue) []*Block {
	var blocks []*Block
	pos := 0
	for pos < len(in) {
		end, next := line(in, pos)
		label, q, exact, ok := beginAt(in, pos, end)
		if !ok {
			pos = next
			continue
		}
		if len(blocks) >= s.lim.MaxBlocks {
			s.add(issues, CodeBlockLimit, int64(pos), "more than %d armored blocks; the rest are not read", s.lim.MaxBlocks)
			break
		}
		b := s.newBlock(in, pos, q, label, exact)
		if b.Type == ArmorSignedMessage {
			next = s.cleartext(in, b, next)
		} else {
			next = s.armored(in, b, next)
		}
		blocks = append(blocks, b)
		pos = next
	}
	return blocks
}

func (s *state) newBlock(in []byte, pos, q int, label string, exact bool) *Block {
	b := &Block{Label: label, Start: int64(pos), Prefix: string(bytes.TrimPrefix(in[pos:pos+q], utf8BOM))}
	b.Type, b.Part, b.Total = armorType(label)
	if b.Type == ArmorUnknown {
		s.add(&b.Issues, CodeArmorLabelUnknown, int64(pos), "unknown armor label %q", label)
	}
	if !exact {
		s.add(&b.Issues, CodeArmorBeginDamaged, int64(pos), "BEGIN line of %q has damaged dashes", label)
	}
	if b.Prefix != "" {
		s.add(&b.Issues, CodeArmorQuoted, int64(pos), "armor lines carry the prefix %q", b.Prefix)
	}
	return b
}

// parseHeaderLine splits an armor header line "Key: Value".
func parseHeaderLine(l []byte) (key, value string, ok bool) {
	i := bytes.IndexByte(l, ':')
	if i <= 0 || i > 64 {
		return "", "", false
	}
	for _, c := range l[:i] {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return "", "", false
		}
	}
	return string(l[:i]), string(bytes.TrimSpace(l[i+1:])), true
}

const maxHeaderValue = 4096

// headers reads the armor headers of b from pos and returns where its body
// starts. In a cleartext signature only the headers RFC 4880 and its
// predecessors used there count, so that a missing blank line does not turn
// the first line of text into a header.
func (s *state) headers(in []byte, b *Block, pos int, clear bool) int {
	limited := false
	for pos < len(in) {
		end, next := line(in, pos)
		q := quoteLen(in[pos:end])
		l := trimRightSpace(in[pos+q : end])
		if len(l) == 0 {
			return next
		}
		if l[0] == '-' {
			if _, _, ok := parseDelim(l, "END"); ok {
				return pos
			}
			if _, _, ok := parseDelim(l, "BEGIN"); ok {
				return pos
			}
		}
		key, value, ok := parseHeaderLine(l)
		if ok && clear {
			switch strings.ToLower(key) {
			case "hash", "charset", "version", "comment":
			default:
				ok = false
			}
		}
		if !ok {
			s.add(&b.Issues, CodeArmorBlankLineMissing, int64(pos), "no blank line after the armor headers")
			return pos
		}
		if len(value) > maxHeaderValue {
			s.add(&b.Issues, CodeArmorHeaderInvalid, int64(pos), "armor header %q is longer than %d bytes and was cut", key, maxHeaderValue)
			value = value[:maxHeaderValue]
		}
		if len(b.Headers) < s.lim.MaxArmorHeaders {
			b.Headers = append(b.Headers, Header{Key: key, Value: value})
		} else if !limited {
			limited = true
			s.add(&b.Issues, CodeArmorHeaderLimit, int64(pos), "more than %d armor headers; the rest are not kept", s.lim.MaxArmorHeaders)
		}
		pos = next
	}
	return pos
}

var b64Alphabet = func() (t [256]bool) {
	for _, c := range []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/") {
		t[c] = true
	}
	return t
}()

// armored reads the headers, base64 body, checksum and END line of b, whose
// BEGIN line ended before pos, decodes it and returns where scanning resumes.
func (s *state) armored(in []byte, b *Block, pos int) int {
	pos = s.headers(in, b, pos, false)
	var b64 []byte
	crcPos, crcNext := -1, 0
	var crcText []byte
	dirtyPos, dirtyLen, invalid, afterCRC := -1, 0, 0, 0
	cut := len(in)
scan:
	for pos < len(in) {
		end, next := line(in, pos)
		q := quoteLen(in[pos:end])
		l := trimRightSpace(in[pos+q : end])
		switch {
		case len(l) == 0:
		case l[0] == '-':
			if label, exact, ok := parseDelim(l, "END"); ok {
				b.Ended, b.End = true, int64(end)
				if !exact {
					s.add(&b.Issues, CodeArmorEndDamaged, int64(pos), "END line of %q has damaged dashes", label)
				}
				if !strings.EqualFold(label, b.Label) {
					s.add(&b.Issues, CodeArmorEndMismatch, int64(pos), "block %q ends with END %q", b.Label, label)
				}
				pos = next
				break scan
			}
			if _, _, _, ok := beginAt(in, pos, end); ok {
				cut = pos
				break scan
			}
			invalid += len(l)
			if dirtyPos < 0 {
				dirtyPos, dirtyLen = pos, len(b64)
			}
		case l[0] == '=' && len(bytes.TrimLeft(l, "=")) > 0:
			if crcPos < 0 {
				crcPos, crcNext, crcText = pos, next, l[1:]
			} else {
				afterCRC++
			}
		case crcPos >= 0:
			afterCRC++
		default:
			start := len(b64)
			dirty := false
			for _, c := range l {
				if b64Alphabet[c] || c == '=' {
					b64 = append(b64, c)
				} else {
					dirty = true
					invalid++
				}
			}
			if dirty && dirtyPos < 0 {
				dirtyPos, dirtyLen = pos, start
			}
		}
		pos = next
	}
	resume := pos
	if !b.Ended {
		// Without an END line the block ends after its checksum line, or
		// before the first line that is not base64, or where scanning
		// stopped.
		resume = cut
		if dirtyPos >= 0 && (crcPos < 0 || dirtyPos < crcPos) {
			b64, resume, invalid, crcPos = b64[:dirtyLen], dirtyPos, 0, -1
		} else if crcPos >= 0 {
			resume = crcNext
		}
		b.End = int64(resume)
		s.add(&b.Issues, CodeArmorEndMissing, b.End, "block %q has no END line", b.Label)
	} else if afterCRC > 0 {
		s.add(&b.Issues, CodeArmorDataAfterCRC, -1, "%d lines after the checksum line were ignored", afterCRC)
	}
	if invalid > 0 {
		s.add(&b.Issues, CodeArmorInvalidCharacter, -1, "%d characters outside base64 were skipped", invalid)
	}
	data, damaged := decodeArmorBase64(b64)
	if damaged {
		s.add(&b.Issues, CodeArmorPadding, -1, "misplaced base64 padding or an incomplete final group")
	}
	b.Data = data
	if len(data) == 0 {
		s.add(&b.Issues, CodeArmorEmpty, b.Start, "block %q holds no data", b.Label)
	}
	switch {
	case crcPos < 0:
		b.CRC = CRCAbsent
		if len(data) > 0 {
			s.add(&b.Issues, CodeCRCMissing, -1, "block %q has no checksum", b.Label)
		}
	default:
		want, ok := parseCRC(crcText)
		switch {
		case !ok:
			b.CRC = CRCInvalid
			s.add(&b.Issues, CodeCRCInvalid, int64(crcPos), "checksum line %q is malformed", truncate(crcText, 16))
		case want != crc24(data):
			b.CRC = CRCMismatch
			s.add(&b.Issues, CodeCRCMismatch, int64(crcPos), "checksum %06X does not match the data's %06X", want, crc24(data))
		default:
			b.CRC = CRCValid
		}
	}
	return resume
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}

// decodeArmorBase64 decodes base64 text that may have lost or misplaced its
// padding: each run of '=' ends a segment, and each segment is decoded on
// its own. A final lone character, which encodes no whole byte, is dropped.
func decodeArmorBase64(src []byte) (out []byte, damaged bool) {
	out = make([]byte, base64.RawStdEncoding.DecodedLen(len(src)))
	n := 0
	for i := 0; i < len(src); {
		j := i
		for j < len(src) && src[j] != '=' {
			j++
		}
		k := j
		for k < len(src) && src[k] == '=' {
			k++
		}
		seg := src[i:j]
		if len(seg)%4 == 1 {
			seg = seg[:len(seg)-1]
			damaged = true
		}
		m, err := base64.RawStdEncoding.Decode(out[n:], seg)
		n += m
		if err != nil {
			damaged = true
		}
		if j < len(src) && k < len(src) {
			damaged = true
		}
		i = k
	}
	return out[:n], damaged
}

func parseCRC(t []byte) (uint32, bool) {
	t = bytes.TrimSpace(t)
	if len(t) != 4 {
		return 0, false
	}
	var v [3]byte
	if n, err := base64.StdEncoding.Decode(v[:], t); err != nil || n != 3 {
		return 0, false
	}
	return uint32(v[0])<<16 | uint32(v[1])<<8 | uint32(v[2]), true
}

var crc24Table = func() (t [256]uint32) {
	for i := range t {
		c := uint32(i) << 16
		for j := 0; j < 8; j++ {
			c <<= 1
			if c&0x1000000 != 0 {
				c ^= 0x1864CFB
			}
		}
		t[i] = c & 0xFFFFFF
	}
	return t
}()

// crc24 is the armor checksum of RFC 9580 §6.1.
func crc24(data []byte) uint32 {
	c := uint32(0xB704CE)
	for _, b := range data {
		c = (c<<8 ^ crc24Table[byte(c>>16)^b]) & 0xFFFFFF
	}
	return c
}
