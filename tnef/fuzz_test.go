package tnef

import (
	"bytes"
	"testing"
	"time"
	"unicode/utf8"
)

const fuzzInputCap = 64 << 10

// fuzzLimits are small enough for a 64 KiB input to reach them.
var fuzzLimits = Limits{
	MaxAttributes:      512,
	MaxAttachments:     32,
	MaxProperties:      256,
	MaxTotalProperties: 1024,
	MaxAttachmentBytes: 16 << 10,
	MaxDecodedBytes:    96 << 10,
	MaxRTFBytes:        32 << 10,
	MaxBodyBytes:       32 << 10,
	MaxStringBytes:     1 << 10,
	MaxDepth:           3,
	MaxIssues:          64,
}

func depthOf(m *Message) int {
	d := 0
	for i := range m.Attachments {
		if e := m.Attachments[i].Embedded; e != nil {
			d = max(d, 1+depthOf(e))
		}
	}
	return d
}

// invariants checks what holds for every input.
func invariants(t *testing.T, in []byte, lim *Limits) {
	t.Helper()
	m, err := Decode(in, lim)
	if !IsTNEF(in) {
		if m != nil || err != ErrNotTNEF {
			t.Fatalf("not TNEF: %v, %v", m, err)
		}
		return
	}
	if m == nil || err != nil {
		t.Fatalf("TNEF: %v, %v", m, err)
	}
	r := lim.resolved()
	if len(m.Issues) > r.MaxIssues {
		t.Fatalf("%d issues", len(m.Issues))
	}
	if n := handed(m); int64(n) > r.MaxDecodedBytes {
		t.Fatalf("%d bytes handed out, limit %d", n, r.MaxDecodedBytes)
	}
	if d := depthOf(m); d > r.MaxDepth {
		t.Fatalf("embedded %d deep", d)
	}
	var walk func(m *Message) (atts, attrs, props int)
	walk = func(m *Message) (atts, attrs, props int) {
		if int64(len(m.RTF)) > r.MaxRTFBytes || int64(len(m.Body)) > 3*r.MaxBodyBytes || int64(len(m.BodyHTML)) > 3*r.MaxBodyBytes {
			t.Fatalf("body sizes %d %d %d", len(m.RTF), len(m.Body), len(m.BodyHTML))
		}
		for _, s := range []string{m.Subject, m.MessageClass, m.Body, m.BodyHTML, m.From.Name, m.From.Address} {
			if !utf8.ValidString(s) {
				t.Fatalf("text is not UTF-8: %q", s)
			}
		}
		attrs, props = len(m.Attributes), len(m.Properties)
		for i := range m.Recipients {
			props += len(m.Recipients[i].Properties)
		}
		for i := range m.Attachments {
			a := &m.Attachments[i]
			if int64(len(a.Data)) > r.MaxAttachmentBytes || int64(len(a.Data)) > a.Size || !utf8.ValidString(a.Filename()) {
				t.Fatalf("attachment: %d bytes of %d, %q", len(a.Data), a.Size, a.Filename())
			}
			atts, attrs, props = atts+1, attrs+len(a.Attributes), props+len(a.Properties)
			if a.Embedded != nil {
				x, y, z := walk(a.Embedded)
				atts, attrs, props = atts+x, attrs+y, props+z
			}
		}
		return atts, attrs, props
	}
	atts, attrs, props := walk(m)
	if atts > r.MaxAttachments || attrs > r.MaxAttributes || props > r.MaxTotalProperties {
		t.Fatalf("%d attachments, %d attributes, %d properties", atts, attrs, props)
	}
}

func fuzzSeeds() [][]byte {
	full := fixture().b
	seeds := [][]byte{
		full,
		full[:len(full)/2],
		inner(),
		newTW().b,
		newTW().msg(AttMAPIProps, props(pBin(PropRTFCompressed, lzfu([]byte(htmlRTF))))).b,
		newTW().msg(AttMAPIProps, props(pBin(PropRTFCompressed, cat(u32(0xFFFFFFFF), u32(0xFFFFFFFF), u32(rtfLZFu), u32(0), bytes.Repeat([]byte{0xFF, 0x0C, 0xEF}, 64))))).b,
		newTW().msg(AttRecipTable, cat(u32(2), props(pUni(PropDisplayName, "a")), props(pLong(PropRecipientType, 2)))).b,
		cat(newTW().b, bytes.Repeat([]byte{0xEE}, 32), newTW().msg(AttSubject, []byte("s\x00")).b[6:]),
		[]byte("not TNEF"),
	}
	damaged := append([]byte(nil), full...)
	for i := 40; i < len(damaged); i += 97 {
		damaged[i] ^= 0x55
	}
	return append(seeds, damaged)
}

// The seeds, every seed cut short, and the fixture with each of its bytes
// damaged in turn hold the invariants, under the default limits and under
// small ones.
func TestInvariants(t *testing.T) {
	for _, s := range fuzzSeeds() {
		invariants(t, s, nil)
		for n := 0; n <= len(s); n += 1 + n/64 {
			invariants(t, s[:n], &fuzzLimits)
		}
	}
	b := fixture().b
	for i := range b {
		for _, x := range []byte{0x01, 0x80, 0xFF} {
			b[i] ^= x
			invariants(t, b, nil)
			invariants(t, b, &fuzzLimits)
			b[i] ^= x
		}
	}
}

// Inputs made to cost the most per byte: the work stays in proportion to the
// input. Were any of these quadratic, they would not finish in time.
func TestLargeHostileInputs(t *testing.T) {
	const size = 256 << 10
	fill := func(head, unit []byte) []byte {
		b := append([]byte(nil), head...)
		for len(b)+len(unit) <= size {
			b = append(b, unit...)
		}
		return b
	}
	empty := newTW().b
	subject := newTW().msg(AttSubject, []byte("s\x00")).b[6:]
	lying := cat([]byte{LevelMessage}, u32(AttSubject), u32(size/2)) // fits, and does not verify
	list := cat(empty, []byte{LevelMessage}, u32(AttMAPIProps), u32(0xFFFFFFF0), u32(0xFFFFFFFF))
	streams := map[string][]byte{
		"level bytes only":         fill(empty, []byte{LevelMessage}),
		"damage before each":       fill(empty, cat([]byte{0xEE}, subject)),
		"candidates that fail":     cat(fill(cat(empty, []byte{0xEE}), lying)[:size/4], make([]byte, 3*size/4)),
		"attachments":              fill(empty, newTW().att(AttAttachData, []byte("d")).b[6:]),
		"properties":               fill(list, pLong(0x6000, 1)),
		"multi-valued properties":  fill(list, cat(tag(TypeBinary|TypeMultiValue, 0x6000), u32(2), u32(0), u32(0))),
		"properties with no value": fill(list, cat(tag(TypeBinary, 0x6000), u32(0))),
	}
	open := Limits{MaxAttributes: 1 << 24, MaxAttachments: 1 << 20, MaxProperties: 1 << 24, MaxTotalProperties: 1 << 24}
	start := time.Now()
	for name, b := range streams {
		for _, lim := range []*Limits{nil, &open} {
			if m, err := Decode(b, lim); err != nil || m == nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
	}
	for _, unit := range []string{`\u1 x`, `\'c7\u1 `, `{\b `, `{\*\htmltag1 <}\htmlrtf x\htmlrtf0 `, `\bin9 `, `\`} {
		rtf := fill([]byte(`{\rtf1\fromhtml1 `), []byte(unit))
		if _, text, _ := ExtractRTF(rtf, 949, size); len(text) > size {
			t.Fatalf("%q: %d bytes", unit, len(text))
		}
		if out, _ := DecompressRTF(cat(u32(size), u32(0xFFFFFFFF), u32(rtfLZFu), u32(0), rtf), size); len(out) > size {
			t.Fatalf("%q: %d bytes decompressed", unit, len(out))
		}
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v", d)
	} else {
		t.Logf("took %v", d)
	}
}

func FuzzDecode(f *testing.F) {
	for _, s := range fuzzSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) > fuzzInputCap {
			in = in[:fuzzInputCap]
		}
		invariants(t, in, &fuzzLimits)

		// The decoders of RTF take any bytes too.
		out, _ := DecompressRTF(in, 8<<10)
		if len(out) > 8<<10 {
			t.Fatalf("DecompressRTF: %d bytes", len(out))
		}
		if _, text, _ := ExtractRTF(in, 949, 8<<10); len(text) > 8<<10 || !utf8.ValidString(text) {
			t.Fatalf("ExtractRTF: %d bytes", len(text))
		}
	})
}
