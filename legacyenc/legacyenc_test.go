package legacyenc

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"slices"
	"strings"
	"testing"
	"time"
)

// -- encoders: each writes a block the way a common encoder does --

// uuEncode writes a uuencoded (or xxencoded) block. zero is how a uuencoder
// writes the value 0: '`' today, ' ' in old encoders.
func uuEncode(data []byte, name string, xx bool, zero byte) string {
	enc := func(v byte) byte {
		switch {
		case xx:
			return xxChars[v]
		case v == 0:
			return zero
		}
		return v + 0x20
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "begin 644 %s\n", name)
	for i := 0; i < len(data); i += 45 {
		chunk := data[i:min(i+45, len(data))]
		sb.WriteByte(enc(byte(len(chunk))))
		for j := 0; j < len(chunk); j += 3 {
			var g [3]byte
			copy(g[:], chunk[j:])
			sb.Write([]byte{enc(g[0] >> 2), enc((g[0]&3)<<4 | g[1]>>4), enc((g[1]&15)<<2 | g[2]>>6), enc(g[2] & 63)})
		}
		sb.WriteByte('\n')
	}
	sb.WriteByte(enc(0))
	sb.WriteString("\nend\n")
	return sb.String()
}

func b64Encode(data []byte, name string) string {
	s := base64.StdEncoding.EncodeToString(data)
	var sb strings.Builder
	fmt.Fprintf(&sb, "begin-base64 644 %s\n", name)
	for len(s) > 0 {
		n := min(60, len(s))
		sb.WriteString(s[:n] + "\n")
		s = s[n:]
	}
	sb.WriteString("====\n")
	return sb.String()
}

type ypart struct {
	part, total, begin, fileSize int
	fileCRC                      uint32
}

func yEncode(data []byte, name string, line int, p *ypart) string {
	var sb strings.Builder
	if p == nil {
		fmt.Fprintf(&sb, "=ybegin line=%d size=%d name=%s\n", line, len(data), name)
	} else {
		fmt.Fprintf(&sb, "=ybegin part=%d total=%d line=%d size=%d name=%s\n", p.part, p.total, line, p.fileSize, name)
		fmt.Fprintf(&sb, "=ypart begin=%d end=%d\n", p.begin, p.begin+len(data)-1)
	}
	col := 0
	for i, b := range data {
		o := b + 42
		esc := o == 0 || o == '\n' || o == '\r' || o == '='
		edge := col == 0 || col >= line-1 || i == len(data)-1
		if edge && (o == ' ' || o == '\t') || col == 0 && o == '.' {
			esc = true
		}
		if esc {
			sb.Write([]byte{'=', o + 64})
			col += 2
		} else {
			sb.WriteByte(o)
			col++
		}
		if col >= line {
			sb.WriteByte('\n')
			col = 0
		}
	}
	if col > 0 {
		sb.WriteByte('\n')
	}
	sum := crc32.ChecksumIEEE(data)
	if p == nil {
		fmt.Fprintf(&sb, "=yend size=%d crc32=%08x\n", len(data), sum)
	} else {
		fmt.Fprintf(&sb, "=yend size=%d part=%d pcrc32=%08x crc32=%08x\n", len(data), p.part, sum, p.fileCRC)
	}
	return sb.String()
}

type mac struct {
	name, typ, creator string
	flags              uint16
	data, rsrc         []byte
	dlen               int // declared data length when not -1
}

func crcOf(b []byte) uint16 {
	var c uint16
	for _, x := range b {
		c = crc16(c, x)
	}
	return c
}

// hqxHeader returns the header of m and its CRC.
func hqxHeader(m mac) []byte {
	h := []byte{byte(len(m.name))}
	h = append(h, m.name...)
	h = append(h, 0)
	h = append(h, m.typ...)
	h = append(h, m.creator...)
	h = binary.BigEndian.AppendUint16(h, m.flags)
	dlen := len(m.data)
	if m.dlen >= 0 {
		dlen = m.dlen
	}
	h = binary.BigEndian.AppendUint32(h, uint32(dlen))
	h = binary.BigEndian.AppendUint32(h, uint32(len(m.rsrc)))
	return binary.BigEndian.AppendUint16(h, crcOf(h))
}

// hqxRaw returns the expanded BinHex stream of m.
func hqxRaw(m mac) []byte {
	h := hqxHeader(m)
	h = append(h, m.data...)
	h = binary.BigEndian.AppendUint16(h, crcOf(m.data))
	h = append(h, m.rsrc...)
	return binary.BigEndian.AppendUint16(h, crcOf(m.rsrc))
}

// rleEncode codes runs of four or more as BinHex encoders do; every 0x90 is
// escaped on its own.
func rleEncode(b []byte) []byte {
	var out []byte
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c == 0x90 {
			out = append(out, 0x90, 0)
			continue
		}
		j := i + 1
		for j < len(b) && b[j] == c && j < i+255 {
			j++
		}
		if j-i > 3 {
			out = append(out, c, 0x90, byte(j-i))
			i = j - 1
			continue
		}
		out = append(out, c)
	}
	return out
}

// hqxText writes run-length coded bytes as a BinHex 4.0 file: the banner, a
// blank line, and 64-character lines between colons.
func hqxText(rle []byte) string {
	var chars []byte
	var acc uint32
	var bits uint
	for _, c := range rle {
		acc = acc<<8 | uint32(c)
		bits += 8
		for bits >= 6 {
			bits -= 6
			chars = append(chars, hqxChars[acc>>bits&63])
		}
		acc &= 1<<bits - 1
	}
	if bits > 0 {
		chars = append(chars, hqxChars[acc<<(6-bits)&63])
	}
	var sb strings.Builder
	sb.WriteString("(This file must be converted with BinHex 4.0)\n\n")
	line := []byte{':'}
	for _, c := range chars {
		line = append(line, c)
		if len(line) == 64 {
			sb.Write(line)
			sb.WriteByte('\n')
			line = line[:0]
		}
	}
	sb.Write(line)
	sb.WriteString(":\n")
	return sb.String()
}

func binhexEncode(m mac) string { return hqxText(rleEncode(hqxRaw(m))) }

// -- fixtures --

// payload has every byte value, runs of zeros and of 0x90, and is 45*7+30
// bytes, so that its last uuencoded line starts with '>'.
var payload = func() []byte {
	var b []byte
	for i := range 256 {
		b = append(b, byte(i))
	}
	b = append(b, make([]byte, 50)...)
	b = append(b, bytes.Repeat([]byte{0x90}, 10)...)
	return append(b, "The quick brown fox jumps ove"...)
}()

var rsrc = []byte("resource fork bytes")

var theMac = mac{name: "Read Me", typ: "TEXT", creator: "ttxt", flags: 0x0100, data: payload, rsrc: rsrc, dlen: -1}

type fixture struct {
	name       string
	text       string
	kind       Kind
	file       string
	mode       int
	size       int64
	data, rsrc []byte
}

func fixtures() []fixture {
	part := payload[100:]
	return []fixture{
		{"uuencode", uuEncode(payload, "photo.jpg", false, '`'), KindUUEncode, "photo.jpg", 0o644, -1, payload, nil},
		{"xxencode", uuEncode(payload, "photo.jpg", true, 0), KindXXEncode, "photo.jpg", 0o644, -1, payload, nil},
		{"begin-base64", b64Encode(payload, "photo.jpg"), KindUUBase64, "photo.jpg", 0o644, -1, payload, nil},
		{"yenc", yEncode(payload, "photo 1.jpg", 64, nil), KindYEnc, "photo 1.jpg", -1, int64(len(payload)), payload, nil},
		{"yenc-part", yEncode(part, "photo.jpg", 64, &ypart{2, 2, 101, len(payload), crc32.ChecksumIEEE(payload)}), KindYEnc, "photo.jpg", -1, int64(len(part)), part, nil},
		{"binhex", binhexEncode(theMac), KindBinHex, "Read Me", -1, int64(len(payload)), payload, rsrc},
	}
}

func fixtureByName(name string) fixture {
	for _, f := range fixtures() {
		if f.name == name {
			return f
		}
	}
	panic(name)
}

// -- variants --

func withCRLF(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
func withCR(s string) string   { return strings.ReplaceAll(s, "\n", "\r") }

// quoted prefixes every line with q, an empty one with q less its trailing
// space, as mail clients do.
func quoted(s, q string) string {
	var sb strings.Builder
	for _, l := range strings.SplitAfter(s, "\n") {
		switch {
		case l == "":
		case l == "\n":
			sb.WriteString(strings.TrimRight(q, " ") + l)
		default:
			sb.WriteString(q + l)
		}
	}
	return sb.String()
}

// mapLines applies fn to every line but the first (the opening line).
func mapLines(s string, fn func(i int, l string) string) string {
	ls := strings.Split(s, "\n")
	for i := 1; i < len(ls); i++ {
		ls[i] = fn(i, ls[i])
	}
	return strings.Join(ls, "\n")
}

func codes(is []Issue) []Code {
	var c []Code
	for _, i := range is {
		c = append(c, i.Code)
	}
	return c
}

// -- tests --

func TestTables(t *testing.T) {
	for _, chars := range []string{xxChars, b64Chars, hqxChars} {
		a := newAlphabet(chars)
		n := 0
		for _, v := range a {
			if v >= 0 {
				n++
			}
		}
		if len(chars) != 64 || n != 64 {
			t.Errorf("alphabet %q: %d characters, %d distinct", chars, len(chars), n)
		}
	}
	if xxChars[45] != 'h' {
		t.Errorf("xx length character for 45 = %q", xxChars[45])
	}
	// CRC-16/XMODEM check value.
	if got := crcOf([]byte("123456789")); got != 0x31c3 {
		t.Errorf("crc16(123456789) = %04x, want 31c3", got)
	}
}

func TestRoundTrip(t *testing.T) {
	variants := []struct {
		name string
		fn   func(string) string
	}{
		{"lf", func(s string) string { return s }},
		{"crlf", withCRLF},
		{"cr", withCR},
		{"quoted", func(s string) string { return quoted(s, "> ") }},
		{"quoted-twice", func(s string) string { return quoted(s, "> > ") }},
		{"quoted-crlf", func(s string) string { return withCRLF(quoted(s, ">> ")) }},
	}
	for _, f := range fixtures() {
		for _, v := range variants {
			text := v.fn(f.text)
			body := []byte("Hello,\n\nthe file is below.\n\n" + v.fn("") + text + v.fn("\nBye.\n"))
			blocks, scanIss := Scan(body, nil)
			if len(blocks) != 1 || len(scanIss) != 0 {
				t.Errorf("%s/%s: Scan = %d blocks, %v", f.name, v.name, len(blocks), scanIss)
				continue
			}
			b := blocks[0]
			if b.Kind != f.kind || b.Name != f.file || b.Mode != f.mode || b.Size != f.size || !b.Complete || len(b.Issues) != 0 {
				t.Errorf("%s/%s: block = %v %q mode %o size %d complete %v %v", f.name, v.name, b.Kind, b.Name, b.Mode, b.Size, b.Complete, b.Issues)
			}
			if string(body[b.Start:b.End]) != text {
				t.Errorf("%s/%s: block range holds %q", f.name, v.name, body[b.Start:b.End])
			}
			r := Decode(body, b, nil)
			if !bytes.Equal(r.Data, f.data) || !bytes.Equal(r.Resource, f.rsrc) || r.Truncated || len(r.Issues) != 0 {
				t.Errorf("%s/%s: Decode = %d bytes, %d resource, truncated %v, %v", f.name, v.name, len(r.Data), len(r.Resource), r.Truncated, r.Issues)
			}
			if k := Sniff(body); k != f.kind {
				t.Errorf("%s/%s: Sniff = %v", f.name, v.name, k)
			}
		}
	}
	h := fixtures()[5]
	blocks, _ := Scan([]byte(h.text), nil)
	if bh := blocks[0].BinHex; bh == nil || string(bh.Type[:]) != "TEXT" || string(bh.Creator[:]) != "ttxt" || bh.Flags != 0x0100 ||
		bh.DataLength != uint32(len(payload)) || bh.ResourceLength != uint32(len(rsrc)) || !bh.CRCValid {
		t.Errorf("BinHex header = %+v", bh)
	}
	y := fixtureByName("yenc-part")
	blocks, _ = Scan([]byte(y.text), nil)
	if h := blocks[0].YEnc; h.Part != 2 || h.Total != 2 || h.PartBegin != 101 || h.PartEnd != int64(len(payload)) || h.Size != int64(len(payload)) || !h.HasPartCRC32 {
		t.Errorf("yEnc header = %+v", h)
	}
}

func TestDamaged(t *testing.T) {
	yenc := fixtureByName("yenc").text
	ypart := fixtureByName("yenc-part").text
	uu := fixtureByName("uuencode").text
	b64 := fixtureByName("begin-base64").text
	xx := fixtureByName("xxencode").text
	part := payload[100:]
	// Header CRC wrong, data and resource CRCs wrong, declared length wrong.
	raw := hqxRaw(theMac)
	hlen := len(hqxHeader(theMac))
	flip := func(i int) string {
		r := slices.Clone(raw)
		r[i] ^= 1
		return hqxText(rleEncode(r))
	}
	long := theMac
	long.dlen = len(payload) + 5
	// A run of the escaped marker, and a run with no byte before it.
	five := mac{name: "x", typ: "BINA", creator: "????", data: bytes.Repeat([]byte{0x90}, 5), dlen: -1}
	fiveRLE := rleEncode(hqxHeader(five))
	fiveRLE = append(fiveRLE, 0x90, 0, 0x90, 5)
	fiveRLE = append(fiveRLE, rleEncode(binary.BigEndian.AppendUint16(nil, crcOf(five.data)))...)
	fiveRLE = append(fiveRLE, 0, 0)
	// old-style uuencode writes zero as ' '; a relay strips the trailing spaces.
	zeros := append(bytes.Repeat([]byte("abc"), 15), append([]byte("tail"), make([]byte, 20)...)...)
	stripped := mapLines(uuEncode(zeros, "z.bin", false, ' '), func(_ int, l string) string { return strings.TrimRight(l, " ") })

	tests := []struct {
		name      string
		text      string
		block     []Code
		decode    []Code // anyTruncation: only the first is checked
		data      []byte // nil: not checked
		rsrc      []byte
		truncated tri
	}{
		{"yenc wrong crc32", replaceField(yenc, "crc32=", "deadbeef"), nil, []Code{CodeCRCMismatch}, payload, nil, no},
		{"yenc wrong size", replaceField(yenc, "=yend size=", "999"), nil, []Code{CodeSizeMismatch}, payload, nil, yes},
		{"yenc missing =yend", dropLastLine(yenc), []Code{CodeMissingEnd}, nil, payload, nil, yes},
		{"yenc bad field", replaceField(yenc, "=yend size=", "x9"), []Code{CodeBadHeader}, nil, payload, nil, no},
		{"yenc wrong pcrc32", replaceField(ypart, "pcrc32=", "00000000"), nil, []Code{CodeCRCMismatch}, part, nil, no},
		{"yenc part range", replaceField(ypart, "begin=", "0"), []Code{CodePartRange}, nil, part, nil, no},
		{"yenc part over total", replaceField(ypart, "part=", "3"), []Code{CodePartRange}, nil, part, nil, no},
		{"yenc short part", replaceField(ypart, "end=", "300"), nil, []Code{CodeSizeMismatch}, part, nil, no},
		{"yenc dangling escape", "=ybegin line=128 size=1 name=x\n*=\n=yend size=1\n", nil, []Code{CodeDanglingEscape}, []byte{0}, nil, no},
		{"binhex header crc", flip(hlen - 1), nil, []Code{CodeCRCMismatch}, payload, rsrc, no},
		{"binhex data crc", flip(hlen + len(payload)), nil, []Code{CodeCRCMismatch}, payload, rsrc, no},
		{"binhex resource crc", flip(len(raw) - 1), nil, []Code{CodeCRCMismatch}, payload, rsrc, no},
		{"binhex declared length too long", binhexEncode(long), nil, []Code{CodeCRCMismatch, CodeTruncated}, nil, nil, yes},
		{"binhex missing colon", strings.TrimSuffix(binhexEncode(theMac), ":\n") + "\n", []Code{CodeMissingEnd}, nil, payload, rsrc, no},
		// The skipped character shifts the rest of the stream: what follows
		// varies, the first issue does not.
		{"binhex junk on a line", mapLines(binhexEncode(theMac), func(i int, l string) string {
			if i == 3 {
				return l[:10] + "~" + l[11:]
			}
			return l
		}), nil, []Code{CodeInvalidChar}, nil, nil, anyTruncation},
		{"binhex escaped marker run", hqxText(fiveRLE), nil, nil, five.data, nil, no},
		{"binhex run with no byte", hqxText(append([]byte{0x90, 5}, rleEncode(hqxRaw(theMac))...)), nil, []Code{CodeBadRun}, payload, rsrc, no},
		{"binhex trailing data", hqxText(append(rleEncode(hqxRaw(theMac)), 'x', 'y', 'z')), nil, []Code{CodeTrailingData}, payload, rsrc, no},
		{"uuencode missing end", strings.TrimSuffix(uu, "end\n"), []Code{CodeMissingEnd}, nil, payload, nil, yes},
		{"uuencode missing end before prose", strings.TrimSuffix(uu, "`\nend\n") + "-- \nAlice\n", []Code{CodeMissingEnd}, nil, payload, nil, yes},
		{"uuencode stripped trailing spaces", stripped, nil, []Code{CodeShortLine}, zeros, nil, no},
		{"uuencode checksum character", mapLines(uu, func(_ int, l string) string {
			if l == "" || l == "end" {
				return l
			}
			return l + "X"
		}), nil, []Code{CodeLineJunk}, payload, nil, no},
		{"uuencode bad character", mapLines(uu, func(i int, l string) string {
			if i == 2 {
				return l[:5] + "~" + l[6:]
			}
			return l
		}), nil, []Code{CodeInvalidChar}, nil, nil, no},
		{"xxencode missing end", strings.TrimSuffix(xx, "end\n"), []Code{CodeMissingEnd}, nil, payload, nil, yes},
		{"base64 missing ====", strings.TrimSuffix(b64, "====\n"), []Code{CodeMissingEnd}, nil, payload, nil, yes},
		// The first line confirms the block, so the damage is on the second.
		{"base64 junk", mapLines(b64, func(i int, l string) string {
			if i == 2 {
				return l[:7] + "!" + l[8:]
			}
			return l
		}), nil, []Code{CodeInvalidChar}, nil, nil, no},
	}
	for _, tc := range tests {
		body := []byte(tc.text)
		blocks, _ := Scan(body, nil)
		if len(blocks) != 1 {
			t.Errorf("%s: %d blocks", tc.name, len(blocks))
			continue
		}
		r := Decode(body, blocks[0], nil)
		if got := codes(blocks[0].Issues); !slices.Equal(got, tc.block) {
			t.Errorf("%s: block issues %v, want %v", tc.name, blocks[0].Issues, tc.block)
		}
		got := codes(r.Issues)
		if tc.truncated == anyTruncation && len(got) > 0 {
			got = got[:1]
		}
		if !slices.Equal(got, tc.decode) {
			t.Errorf("%s: decode issues %v, want %v", tc.name, r.Issues, tc.decode)
		}
		if tc.data != nil && (!bytes.Equal(r.Data, tc.data) || !bytes.Equal(r.Resource, tc.rsrc)) {
			t.Errorf("%s: decoded %d bytes and %d resource bytes, want %d and %d", tc.name, len(r.Data), len(r.Resource), len(tc.data), len(tc.rsrc))
		}
		if tc.truncated != anyTruncation && r.Truncated != (tc.truncated == yes) {
			t.Errorf("%s: truncated %v", tc.name, r.Truncated)
		}
	}
}

// tri is a want for Result.Truncated: false, true, or anyTruncation.
type tri int

const (
	no tri = iota
	yes
	anyTruncation
)

// replaceField replaces the value after the first occurrence of key.
func replaceField(s, key, val string) string {
	i := strings.Index(s, key) + len(key)
	j := i + strings.IndexAny(s[i:], " \n")
	return s[:i] + val + s[j:]
}

func dropLastLine(s string) string {
	s = strings.TrimSuffix(s, "\n")
	return s[:strings.LastIndex(s, "\n")+1]
}

// A run-length bomb: a few kilobytes that expand to a megabyte stop at the
// budget.
func bombText() string {
	m := mac{name: "bomb", typ: "BINA", creator: "????", dlen: 0x7ffffff0}
	s := rleEncode(hqxHeader(m))
	s = append(s, 'A')
	for range 4000 {
		s = append(s, 0x90, 0xff)
	}
	return hqxText(s)
}

func TestRunLengthBomb(t *testing.T) {
	body := []byte(bombText())
	results, _ := DecodeAll(body, &Limits{MaxBlockBytes: 64 << 10})
	if len(results) != 1 {
		t.Fatalf("%d results", len(results))
	}
	r := results[0]
	if len(r.Data) != 64<<10 || !r.Truncated || !slices.Equal(codes(r.Issues), []Code{CodeBudget}) {
		t.Errorf("bomb: %d bytes, truncated %v, %v", len(r.Data), r.Truncated, r.Issues)
	}
	if r.Block.Size != 0x7ffffff0 {
		t.Errorf("bomb: declared size %d", r.Block.Size)
	}
	// Without a smaller budget it still stops at what the stream holds.
	r = Decode(body, r.Block, nil)
	if len(r.Data) != 1+4000*254 || !r.Truncated || !slices.Equal(codes(r.Issues), []Code{CodeTruncated}) {
		t.Errorf("bomb, default budget: %d bytes, %v", len(r.Data), r.Issues)
	}
}

// Every cut of every fixture decodes to the original or reports why not.
func TestTruncatedEverywhere(t *testing.T) {
	for _, f := range fixtures() {
		for cut := 0; cut <= len(f.text); cut++ {
			body := []byte("Hi,\n" + f.text[:cut])
			results, _ := DecodeAll(body, nil)
			if len(results) > 1 {
				t.Fatalf("%s cut at %d: %d results", f.name, cut, len(results))
			}
			for _, r := range results {
				if r.Block.Start != 4 || r.Block.End > len(body) {
					t.Fatalf("%s cut at %d: range %d-%d", f.name, cut, r.Block.Start, r.Block.End)
				}
				exact := bytes.Equal(r.Data, f.data) && bytes.Equal(r.Resource, f.rsrc)
				if !exact && len(r.Issues)+len(r.Block.Issues) == 0 {
					t.Fatalf("%s cut at %d: %d bytes decoded and nothing reported", f.name, cut, len(r.Data))
				}
			}
			if cut == len(f.text) && (len(results) != 1 || !results[0].Block.Complete) {
				t.Fatalf("%s: whole text not one complete block", f.name)
			}
		}
	}
}

func TestTwoBlocksAndProse(t *testing.T) {
	uu, y := fixtureByName("uuencode"), fixtureByName("yenc")
	body := "Hello Bob,\n\nbegin the day with this.\n\n" + uu.text + "\nand the other:\n\n" + y.text + "\nThanks\n-- \nAlice\n"
	results, issues := DecodeAll([]byte(body), nil)
	if len(results) != 2 || len(issues) != 0 {
		t.Fatalf("%d results, %v", len(results), issues)
	}
	for i, f := range []fixture{uu, y} {
		r := results[i]
		if r.Block.Kind != f.kind || body[r.Block.Start:r.Block.End] != f.text || !bytes.Equal(r.Data, f.data) || len(r.Issues)+len(r.Block.Issues) != 0 {
			t.Errorf("block %d: %v %q, %d bytes, %v %v", i, r.Block.Kind, body[r.Block.Start:r.Block.End], len(r.Data), r.Block.Issues, r.Issues)
		}
	}
}

func TestProseIsNotABlock(t *testing.T) {
	for _, s := range []string{
		"begin the day with coffee\nend\n",
		"begin 2024 kickoff\nMeeting at ten.\nend\n",
		"Let us begin 644 tasks\nend\n",
		"begin 644 notes.txt\nsee you there\nend\n",
		"begin 644 notes.txt\n\n\n\nM\n",
		"begin\nend\n",
		"> begin the day\n> end\n",
		"=ybegin is how a yEnc post starts\n=yend\n",
		"(This file must be converted with BinHex 4.0)\nbut the attachment never arrived.\n",
		"(This file must be converted with BinHex 4.0)\n",
		"begin-base64 is an option of uuencode\n====\n",
	} {
		if blocks, _ := Scan([]byte(s), nil); len(blocks) != 0 {
			t.Errorf("%q: %d blocks, first %v", s, len(blocks), blocks[0].Kind)
		}
		if k := Sniff([]byte(s)); k != KindNone {
			t.Errorf("Sniff(%q) = %v", s, k)
		}
	}
}

func TestLimits(t *testing.T) {
	uu, y := fixtureByName("uuencode"), fixtureByName("yenc")
	two := []byte(uu.text + "\n" + y.text)

	blocks, issues := Scan(two, &Limits{MaxBlocks: 1})
	if len(blocks) != 1 || !slices.Equal(codes(issues), []Code{CodeBlockLimit}) {
		t.Errorf("MaxBlocks: %d blocks, %v", len(blocks), issues)
	}

	blocks, issues = Scan([]byte(uu.text), &Limits{MaxScanBytes: len(uu.text) / 2})
	if len(blocks) != 1 || !slices.Equal(codes(issues), []Code{CodeScanLimit}) || !slices.Equal(codes(blocks[0].Issues), []Code{CodeMissingEnd}) {
		t.Errorf("MaxScanBytes: %d blocks, %v", len(blocks), issues)
	}

	wide := []byte(yEncode(payload, "w", 128, nil))
	blocks, _ = Scan(wide, &Limits{MaxLineBytes: 64})
	if len(blocks) != 1 || !slices.Equal(codes(blocks[0].Issues), []Code{CodeLineTooLong}) {
		t.Fatalf("MaxLineBytes: %v", blocks)
	}
	if r := Decode(wide, blocks[0], &Limits{MaxLineBytes: 64}); !slices.Contains(codes(r.Issues), CodeSizeMismatch) {
		t.Errorf("MaxLineBytes: decode %v", r.Issues)
	}

	r := Decode([]byte(uu.text), mustScan(t, uu.text), &Limits{MaxBlockBytes: 10})
	if len(r.Data) != 10 || !r.Truncated || !slices.Equal(codes(r.Issues), []Code{CodeBudget}) {
		t.Errorf("MaxBlockBytes: %d bytes, %v", len(r.Data), r.Issues)
	}

	results, _ := DecodeAll(two, &Limits{MaxTotalBytes: int64(len(payload)) + 55})
	if len(results) != 2 || len(results[0].Data) != len(payload) || len(results[1].Data) != 55 ||
		!slices.Equal(codes(results[1].Issues), []Code{CodeBudget}) {
		t.Errorf("MaxTotalBytes: %v", results)
	}
	results, _ = DecodeAll(two, &Limits{MaxTotalBytes: int64(len(payload))})
	if len(results) != 2 || results[1].Data != nil || !slices.Equal(codes(results[1].Issues), []Code{CodeTotalLimit}) {
		t.Errorf("MaxTotalBytes spent: %v", results)
	}
}

func mustScan(t *testing.T, s string) Block {
	t.Helper()
	blocks, _ := Scan([]byte(s), nil)
	if len(blocks) != 1 {
		t.Fatalf("%d blocks", len(blocks))
	}
	return blocks[0]
}

func TestSniffWindow(t *testing.T) {
	uu := fixtureByName("uuencode").text
	pad := strings.Repeat("plain prose line\n", SniffBytes/17+1)
	if k := Sniff([]byte(pad + uu)); k != KindNone {
		t.Errorf("block past the window: Sniff = %v", k)
	}
	if blocks, _ := Scan([]byte(pad+uu), nil); len(blocks) != 1 {
		t.Errorf("Scan past the window: %d blocks", len(blocks))
	}
	// A prefix that stops before the confirming line reports nothing.
	if k := Sniff([]byte("begin 644 photo.jpg\n")); k != KindNone {
		t.Errorf("unconfirmed: Sniff = %v", k)
	}
}

// Bodies of a megabyte built from openers that never confirm, and from
// one-line blocks, scan in linear time: a quadratic scan would take seconds.
func TestAdversarialLinear(t *testing.T) {
	const size = 1 << 20
	rep := func(s string) []byte { return bytes.Repeat([]byte(s), size/len(s)) }
	for name, body := range map[string][]byte{
		"begin lines":      rep("begin 644 a\n"),
		"unconfirmed":      rep("begin 644 a\nM\n\n"),
		"ybegin":           append([]byte("=ybegin size=1 name=a\n"), rep("=ybegin x\n")...),
		"banners":          rep("(This file must be converted with BinHex 4.0)\n\n"),
		"quoted openers":   rep("> > > > begin 644 a\n"),
		"binhex long line": append([]byte("(This file must be converted with BinHex 4.0)\n:"), rep("!")...),
		"base64 blocks":    rep("begin-base64 644 a\n====\n"),
		"yenc data":        append([]byte("=ybegin line=128 size=9 name=a\n"), rep("abcdefgh\r\n")...),
	} {
		start := time.Now()
		results, _ := DecodeAll(body, nil)
		Sniff(body)
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("%s: %v for %d blocks", name, d, len(results))
		}
	}
}

func TestStrings(t *testing.T) {
	if KindBinHex.String() != "binhex" || Kind(99).String() != "Kind(99)" {
		t.Error("Kind.String")
	}
	if CodeCRCMismatch.String() != "crc_mismatch" || Code(99).String() != "Code(99)" {
		t.Error("Code.String")
	}
	if s := (Issue{CodeMissingEnd, 7, "x"}).String(); s != "missing_end at 7: x" {
		t.Errorf("Issue.String = %q", s)
	}
	if s := (Issue{CodeScanLimit, -1, "x"}).String(); s != "scan_limit: x" {
		t.Errorf("Issue.String = %q", s)
	}
	// A block not from Scan decodes to nothing, without a panic.
	if r := Decode([]byte("abc"), Block{Kind: KindBinHex, dataStart: 9, dataEnd: 2}, nil); len(r.Data) != 0 {
		t.Error("foreign block decoded")
	}
}
