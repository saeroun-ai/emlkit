package tnef

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// No winmail.dat written by Outlook is available to these tests. The fixtures
// are built here, attribute by attribute, from MS-OXTNEF.

var le = binary.LittleEndian

func u16(v uint16) []byte { return le.AppendUint16(nil, v) }
func u32(v uint32) []byte { return le.AppendUint32(nil, v) }

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func pad4(b []byte) []byte {
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// tw writes a TNEF stream. marks are the offsets after each attribute.
type tw struct {
	b     []byte
	marks []int
}

func newTW() *tw { return &tw{b: cat(u32(Signature), u16(0x1234))} }

func (w *tw) raw(level byte, id uint32, data []byte, sum uint16) *tw {
	w.b = append(w.b, level)
	w.b = append(w.b, u32(id)...)
	w.b = append(w.b, u32(uint32(len(data)))...)
	w.b = append(w.b, data...)
	w.b = append(w.b, u16(sum)...)
	w.marks = append(w.marks, len(w.b))
	return w
}

func (w *tw) attr(level byte, id uint32, data []byte) *tw {
	var sum uint16
	for _, c := range data {
		sum += uint16(c)
	}
	return w.raw(level, id, data, sum)
}

func (w *tw) msg(id uint32, data []byte) *tw { return w.attr(LevelMessage, id, data) }
func (w *tw) att(id uint32, data []byte) *tw { return w.attr(LevelAttachment, id, data) }

func props(ps ...[]byte) []byte { return cat(u32(uint32(len(ps))), cat(ps...)) }

func tag(typ, id uint16) []byte { return cat(u16(typ), u16(id)) }

func pLong(id uint16, v uint32) []byte { return cat(tag(TypeInt32, id), u32(v)) }

func pBool(id uint16, v bool) []byte {
	b := cat(tag(TypeBoolean, id), u32(0))
	if v {
		b[4] = 1
	}
	return b
}

func pTime(id uint16, t time.Time) []byte {
	ft := uint64(t.Unix()+11644473600) * 10000000
	return cat(tag(TypeSysTime, id), le.AppendUint64(nil, ft))
}

func pVar(typ, id uint16, vals ...[]byte) []byte {
	b := cat(tag(typ, id), u32(uint32(len(vals))))
	for _, v := range vals {
		b = cat(b, u32(uint32(len(v))), pad4(append([]byte(nil), v...)))
	}
	return b
}

func wide(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = le.AppendUint16(b, u)
	}
	return b
}

func pStr8(id uint16, s []byte) []byte { return pVar(TypeString8, id, cat(s, []byte{0})) }
func pUni(id uint16, s string) []byte  { return pVar(TypeUnicode, id, wide(s+"\x00")) }
func pBin(id uint16, b []byte) []byte  { return pVar(TypeBinary, id, b) }

var iidMessage = []byte{0x07, 0x03, 0x02, 0, 0, 0, 0, 0, 0xC0, 0, 0, 0, 0, 0, 0, 0x46}

func pObj(id uint16, b []byte) []byte { return pVar(TypeObject, id, cat(iidMessage, b)) }

func dtrBytes(y, mo, d, h, mi, s int) []byte {
	return cat(u16(uint16(y)), u16(uint16(mo)), u16(uint16(d)), u16(uint16(h)), u16(uint16(mi)), u16(uint16(s)), u16(0))
}

// lzfu compresses rtf with literals only, which is all a fixture needs; the
// vectors of MS-OXRTFCP in TestDecompressRTF exercise references.
func lzfu(rtf []byte) []byte {
	var body []byte
	w := len(rtfDict)
	end := func() []byte { ref := (w & 4095) << 4; return []byte{byte(ref >> 8), byte(ref)} }
	done := false
	for i := 0; i < len(rtf); i += 8 {
		chunk := rtf[i:min(i+8, len(rtf))]
		w += len(chunk)
		if len(chunk) == 8 {
			body = cat(body, []byte{0}, chunk)
			continue
		}
		body = cat(body, []byte{1 << len(chunk)}, chunk, end())
		done = true
	}
	if !done {
		body = cat(body, []byte{1}, end())
	}
	crc := ^crc32.Update(^uint32(0), crc32.IEEETable, body)
	return cat(u32(uint32(len(body)+12)), u32(uint32(len(rtf))), u32(rtfLZFu), u32(crc), body)
}

// Korean text in Windows-949, written out so that the expectation does not
// come from the decoder under test.
var (
	cp949Hangul = []byte{0xC7, 0xD1, 0xB1, 0xDB}                                     // 한글
	cp949Report = []byte{0xBA, 0xB8, 0xB0, 0xED, 0xBC, 0xAD, '.', 't', 'x', 't'}     // 보고서.txt
	cp949Hello  = []byte{0xBE, 0xC8, 0xB3, 0xE7, 0xC7, 0xCF, 0xBC, 0xBC, 0xBF, 0xE4} // 안녕하세요
)

const (
	longName = "아주 긴 한글 파일 이름 — 분기 보고서 최종본 (수정) 2026년 11월.xlsx"
	htmlRTF  = `{\rtf1\ansi\ansicpg949\fromhtml1 \deff0{\fonttbl{\f0\fswiss Arial;}}
{\*\htmltag19 <html>}{\*\htmltag34 <head>}{\*\htmltag241 <title>}t{\*\htmltag249 </title>}{\*\htmltag41 </head>}
{\*\htmltag50 <body>}\htmlrtf {\f0 \htmlrtf0 {\*\htmltag64 <p>}\htmlrtf {\htmlrtf0 \'c7\'d1\'b1\'db \u-10179?\u-8704? a\{b\}\htmlrtf\par}\htmlrtf0
{\*\htmltag72 </p>}{\*\htmltag58 </body>}{\*\htmltag27 </html>}}`
	htmlWant = "<html><head><title>t</title></head><body><p>한글 😀 a{b}</p></body></html>"
)

var sent = time.Date(2026, 11, 11, 9, 30, 0, 0, time.UTC)

func inner() []byte {
	return newTW().
		msg(AttTnefVersion, u32(0x00010000)).
		msg(AttMAPIProps, props(pUni(PropSubject, "안쪽 메시지"), pStr8(PropMessageClass, []byte("IPM.Note")))).
		att(AttAttachRendData, make([]byte, 14)).
		att(AttAttachTitle, []byte("inner.bin\x00")).
		att(AttAttachData, []byte{1, 2, 3}).b
}

// fixture is a message with every attribute the package reads, two
// attachments and an embedded message.
func fixture() *tw {
	guid := bytes.Repeat([]byte{0xAB}, 16)
	from := cat(u16(4), u16(0), u16(4), u16(14), []byte("Kim\x00"), []byte("SMTP:k@x.test\x00"))
	return newTW().
		msg(AttTnefVersion, u32(0x00010000)).
		msg(AttOemCodepage, cat(u32(949), u32(0))).
		msg(AttMessageClass, []byte("IPM.Microsoft Mail.Note\x00")).
		msg(AttFrom, from).
		msg(AttSubject, cat(cp949Hello, []byte{0})).
		msg(AttDateSent, dtrBytes(2026, 11, 11, 18, 30, 0)).
		msg(AttMessageID, []byte("0123456789ABCDEF\x00")).
		msg(AttBody, cat([]byte("plain "), cp949Hangul, []byte{0})).
		msg(0x00061234, []byte("not an attribute MS-OXTNEF defines")).
		msg(AttMAPIProps, props(
			pStr8(PropMessageClass, []byte("IPM.Note")),
			pLong(PropInternetCPID, 949),
			pTime(PropClientSubmitTime, sent),
			pBin(PropBodyHTML, cat([]byte("<p>"), cp949Hangul, []byte("</p>"))),
			pBin(PropRTFCompressed, lzfu([]byte(htmlRTF))),
			cat(tag(TypeInt32, 0x8001), guid, u32(0), u32(0x8501), u32(7)),
			cat(tag(TypeUnicode, 0x8002), guid, u32(1), u32(12), pad4(wide("Label\x00")), u32(1), u32(4), wide("v\x00")),
			cat(tag(TypeInt32|TypeMultiValue, 0x6001), u32(3), u32(10), u32(20), u32(30)),
			pVar(TypeUnicode|TypeMultiValue, 0x6002, wide("a\x00"), wide("bc\x00")),
		)).
		msg(AttRecipTable, cat(u32(1), props(
			pUni(PropDisplayName, "받는 사람"), pStr8(PropEmailAddress, []byte("r@x.test")), pLong(PropRecipientType, 1)))).
		att(AttAttachRendData, make([]byte, 14)).
		att(AttAttachTitle, cat(cp949Report, []byte{0})).
		att(AttAttachCreateDate, dtrBytes(2026, 1, 2, 3, 4, 5)).
		att(AttAttachModifyDate, dtrBytes(2026, 13, 2, 3, 4, 5)).
		att(AttAttachMetaFile, []byte("wmf")).
		att(AttAttachData, []byte("hello")).
		att(AttAttachment, props(
			pUni(PropAttachLongFilename, longName),
			pStr8(PropAttachExtension, []byte(".xlsx")),
			pStr8(PropAttachMIMETag, []byte("application/vnd.ms-excel")),
			pStr8(PropAttachContentID, []byte("img1@x.test")),
			pLong(PropAttachMethod, uint32(AttachByValue)),
			pLong(PropAttachFlags, AttachFlagMHTMLRef),
			pBool(PropAttachmentHidden, true),
		)).
		att(AttAttachRendData, make([]byte, 14)).
		att(AttAttachment, props(
			pLong(PropAttachMethod, uint32(AttachEmbeddedMsg)),
			pUni(PropDisplayName, "전달된 메시지"),
			pObj(PropAttachData, inner()),
		))
}

func has(m *Message, code Code) bool {
	for _, i := range m.Issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

func mustDecode(t *testing.T, b []byte, lim *Limits) *Message {
	t.Helper()
	m, err := Decode(b, lim)
	if err != nil || m == nil {
		t.Fatalf("Decode: %v, %v", m, err)
	}
	return m
}

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestDecodeFixture(t *testing.T) {
	m := mustDecode(t, fixture().b, nil)
	for _, i := range m.Issues {
		t.Errorf("unexpected issue: %v", i)
	}
	eq(t, "Key", m.Key, 0x1234)
	eq(t, "Version", m.Version, 0x00010000)
	eq(t, "Codepage", m.Codepage, 949)
	eq(t, "InternetCodepage", m.InternetCodepage, 949)
	eq(t, "Subject", m.Subject, "안녕하세요")
	eq(t, "MessageClass", m.MessageClass, "IPM.Note")
	eq(t, "MessageID", m.MessageID, "0123456789ABCDEF")
	eq(t, "From", m.From, Address{Name: "Kim", Address: "k@x.test", Type: "SMTP"})
	eq(t, "DateSent", m.DateSent, sent)
	eq(t, "Body", m.Body, "plain 한글")
	eq(t, "BodyHTML", m.BodyHTML, "<p>한글</p>")
	eq(t, "RTF", string(m.RTF), htmlRTF)
	eq(t, "HTMLFromRTF", m.HTMLFromRTF, false)
	eq(t, "UnknownAttributes", m.UnknownAttributes, 1)
	eq(t, "Incomplete", m.Incomplete, false)
	eq(t, "Attributes", len(m.Attributes), 11)
	eq(t, "raw subject", string(m.Attributes[4].Data), string(cp949Hello)+"\x00")

	eq(t, "Properties", len(m.Properties), 9)
	if p := m.Properties.Find(0x8001); p == nil || p.Name == nil || p.Name.ID != 0x8501 || p.Name.GUID[0] != 0xAB {
		t.Errorf("named property by id: %+v", p)
	} else if v, _ := p.Int(); v != 7 {
		t.Errorf("named property value = %d", v)
	}
	if p := m.Properties.Find(0x8002); p == nil || p.Name == nil || !p.Name.IsString || p.Name.Name != "Label" {
		t.Errorf("named property by name: %+v", p)
	} else if s, _ := p.Text(0); s != "v" {
		t.Errorf("named property text = %q", s)
	}
	if p := m.Properties.Find(0x6001); p == nil || len(p.Values) != 3 || le32(p.Values[2]) != 30 {
		t.Errorf("multi-valued integers: %+v", p)
	}
	if p := m.Properties.Find(0x6002); p == nil || len(p.Values) != 2 || string(p.Values[1]) != string(wide("bc\x00")) {
		t.Errorf("multi-valued strings: %+v", p)
	}
	if p := m.Properties.Find(PropSubject); p != nil {
		t.Errorf("Find of an absent property: %+v", p)
	}
	eq(t, "Tag", m.Properties[1].Tag(), 0x3FDE0003)

	if len(m.Recipients) != 1 {
		t.Fatalf("Recipients = %d", len(m.Recipients))
	}
	eq(t, "Recipient", m.Recipients[0].Address, Address{Name: "받는 사람", Address: "r@x.test"})
	eq(t, "Recipient kind", m.Recipients[0].Kind, 1)

	if len(m.Attachments) != 2 {
		t.Fatalf("Attachments = %d", len(m.Attachments))
	}
	a := &m.Attachments[0]
	eq(t, "Name", a.Name, "보고서.txt")
	eq(t, "LongName", a.LongName, longName)
	eq(t, "Filename", a.Filename(), longName)
	eq(t, "Extension", a.Extension, ".xlsx")
	eq(t, "MIMEType", a.MIMEType, "application/vnd.ms-excel")
	eq(t, "ContentID", a.ContentID, "img1@x.test")
	eq(t, "Method", a.Method, AttachByValue)
	eq(t, "Hidden", a.Hidden, true)
	eq(t, "Inline", a.Inline, true)
	eq(t, "Data", string(a.Data), "hello")
	eq(t, "Size", a.Size, 5)
	eq(t, "Truncated", a.Truncated, false)
	eq(t, "MetaFile", string(a.MetaFile), "wmf")
	eq(t, "Created", a.Created, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	eq(t, "Modified of a month 13", a.Modified.IsZero(), true)
	eq(t, "attachment attributes", len(a.Attributes), 7)

	e := &m.Attachments[1]
	eq(t, "embedded display name", e.Filename(), "전달된 메시지")
	eq(t, "embedded inline", e.Inline, false)
	if e.Embedded == nil {
		t.Fatal("no embedded message")
	}
	eq(t, "embedded Subject", e.Embedded.Subject, "안쪽 메시지")
	eq(t, "embedded data", bytes.Equal(e.Data, inner()), true)
	if len(e.Embedded.Attachments) != 1 || string(e.Embedded.Attachments[0].Data) != "\x01\x02\x03" ||
		e.Embedded.Attachments[0].Name != "inner.bin" {
		t.Errorf("embedded attachments: %+v", e.Embedded.Attachments)
	}

	r, err := DecodeReader(bytes.NewReader(fixture().b), nil)
	if err != nil || r.Subject != m.Subject || len(r.Attachments) != 2 {
		t.Errorf("DecodeReader: %v, %v", r, err)
	}
}

func TestNotTNEF(t *testing.T) {
	for _, b := range [][]byte{nil, {0x78}, {0x78, 0x9F, 0x3E}, []byte("From: a@b\r\n\r\n"), {0x22, 0x3E, 0x9F, 0x78}} {
		if m, err := Decode(b, nil); m != nil || !errors.Is(err, ErrNotTNEF) {
			t.Errorf("Decode(%x) = %v, %v", b, m, err)
		}
	}
	if _, err := DecodeReader(strings.NewReader("nope"), nil); !errors.Is(err, ErrNotTNEF) {
		t.Errorf("DecodeReader = %v", err)
	}
	// The signature alone is a stream, cut inside its header.
	if m := mustDecode(t, u32(Signature), nil); !m.Incomplete || !has(m, CodeTruncated) {
		t.Errorf("signature only: %+v", m)
	}
	// With its key it is an empty one.
	if m := mustDecode(t, newTW().b, nil); m.Incomplete || len(m.Issues) != 0 {
		t.Errorf("empty stream: %+v", m)
	}
}

func attributes(m *Message) int {
	n := len(m.Attributes)
	for i := range m.Attachments {
		n += len(m.Attachments[i].Attributes)
	}
	return n
}

// Cut at every byte: nothing panics, a cut at an attribute boundary is a
// clean, shorter stream, a cut anywhere else is reported, and what came
// before the cut is kept.
func TestTruncation(t *testing.T) {
	w := fixture()
	full := w.b
	boundary := map[int]int{6: 0}
	for i, at := range w.marks {
		boundary[at] = i + 1
	}
	for n := 0; n <= len(full); n++ {
		m, err := Decode(full[:n], nil)
		if n < 4 {
			if err == nil {
				t.Fatalf("cut at %d: no error", n)
			}
			continue
		}
		if err != nil || m == nil {
			t.Fatalf("cut at %d: %v", n, err)
		}
		if k, ok := boundary[n]; ok {
			if m.Incomplete || len(m.Issues) != 0 || attributes(m) != k {
				t.Errorf("cut at boundary %d: incomplete=%v attributes=%d want %d issues=%v", n, m.Incomplete, attributes(m), k, m.Issues)
			}
		} else if !m.Incomplete || !has(m, CodeTruncated) {
			t.Errorf("cut at %d: incomplete=%v issues=%v", n, m.Incomplete, m.Issues)
		}
		if n >= w.marks[4] && m.Subject != "안녕하세요" {
			t.Errorf("cut at %d: Subject = %q", n, m.Subject)
		}
		if n >= w.marks[17] && (len(m.Attachments) == 0 || m.Attachments[0].LongName != longName) {
			t.Errorf("cut at %d: first attachment lost", n)
		}
	}

	// A cut inside the data of an attachment keeps the beginning of it.
	cut := newTW().att(AttAttachRendData, make([]byte, 14)).att(AttAttachData, []byte("0123456789")).b
	m := mustDecode(t, cut[:len(cut)-6], nil)
	if len(m.Attachments) != 1 {
		t.Fatalf("attachments = %d", len(m.Attachments))
	}
	a := m.Attachments[0]
	if string(a.Data) != "012345" || !a.Truncated || a.Size != 10 {
		t.Errorf("cut attachment: %q truncated=%v size=%d", a.Data, a.Truncated, a.Size)
	}
}

func TestDamage(t *testing.T) {
	subject := func() *tw { return newTW().msg(AttTnefVersion, u32(0x00010000)) }

	t.Run("checksum", func(t *testing.T) {
		b := subject().raw(LevelMessage, AttSubject, []byte("hello\x00"), 0xBEEF).msg(AttBody, []byte("b\x00")).b
		m := mustDecode(t, b, nil)
		if !has(m, CodeChecksum) || m.Subject != "hello" || m.Body != "b" || !m.Attributes[1].BadChecksum || m.Incomplete {
			t.Errorf("%+v", m)
		}
	})
	t.Run("resync", func(t *testing.T) {
		w := subject()
		w.b = append(w.b, 0xEE, 0x01, 0x02, 0xEE, 0xEE, 0x00, 0xFF)
		m := mustDecode(t, w.msg(AttSubject, []byte("after\x00")).b, nil)
		if !has(m, CodeResync) || m.Subject != "after" || m.Incomplete {
			t.Errorf("%+v", m)
		}
	})
	t.Run("unknown attribute with an absurd length", func(t *testing.T) {
		w := subject()
		w.b = cat(w.b, []byte{LevelMessage}, u32(0x00FF00FF), u32(0xFFFFFFF0), []byte("xx"))
		m := mustDecode(t, w.msg(AttSubject, []byte("after\x00")).b, nil)
		if !has(m, CodeResync) || m.Subject != "after" {
			t.Errorf("%+v", m)
		}
	})
	t.Run("level", func(t *testing.T) {
		b := subject().attr(0x07, AttSubject, []byte("lvl\x00")).att(AttBody, []byte("b\x00")).b
		m := mustDecode(t, b, nil)
		if !has(m, CodeLevel) || m.Subject != "lvl" || m.Body != "b" || len(m.Attachments) != 0 {
			t.Errorf("%+v", m)
		}
	})
	t.Run("no attribute after damage", func(t *testing.T) {
		w := subject().msg(AttSubject, []byte("kept\x00"))
		w.b = append(w.b, bytes.Repeat([]byte{0xEE}, 40)...)
		m := mustDecode(t, w.b, nil)
		if !has(m, CodeDamaged) || m.Subject != "kept" || !m.Incomplete {
			t.Errorf("%+v", m)
		}
	})
	t.Run("lost attAttachRendData", func(t *testing.T) {
		b := subject().
			att(AttAttachTitle, []byte("a\x00")).att(AttAttachData, []byte("A")).
			att(AttAttachTitle, []byte("b\x00")).att(AttAttachData, []byte("B")).b
		m := mustDecode(t, b, nil)
		if !has(m, CodeNoRendData) || len(m.Attachments) != 2 || m.Attachments[1].Name != "b" || string(m.Attachments[1].Data) != "B" {
			t.Errorf("%+v", m)
		}
	})
	t.Run("embedded message that is not TNEF", func(t *testing.T) {
		b := subject().att(AttAttachRendData, nil).att(AttAttachment, props(
			pLong(PropAttachMethod, uint32(AttachEmbeddedMsg)), pObj(PropAttachData, []byte("not a stream")))).b
		m := mustDecode(t, b, nil)
		if !has(m, CodeEmbeddedNotTNEF) || m.Attachments[0].Embedded != nil || string(m.Attachments[0].Data) != "not a stream" {
			t.Errorf("%+v", m)
		}
	})
}

func TestCodepages(t *testing.T) {
	high := []byte{'c', 'a', 'f', 0xE9, 0}
	tests := []struct {
		name    string
		cp      uint32
		subject []byte
		want    string
		issue   Code
	}{
		{"cp949", 949, cat(cp949Hangul, []byte{0}), "한글", ""},
		{"cp1252", 1252, high, "café", ""},
		{"latin-1", 28591, high, "café", ""},
		{"cp1251", 1251, []byte{0xCF, 0xF0, 0xE8, 0}, "При", ""},
		{"shift_jis", 932, []byte{0x93, 0xFA, 0x96, 0x7B, 0}, "日本", ""},
		{"gbk", 936, []byte{0xD6, 0xD0, 0xCE, 0xC4, 0}, "中文", ""},
		{"big5", 950, []byte{0xA4, 0xA4, 0xA4, 0xE5, 0}, "中文", ""},
		{"utf-8", 65001, []byte("한글\x00"), "한글", ""},
		{"utf-8, damaged", 65001, []byte{'a', 0xFF, 0}, "a\uFFFD", ""},
		{"us-ascii", 20127, high, "caf\uFFFD", ""},
		{"unknown, ascii text", 12345, []byte("plain\x00"), "plain", ""},
		{"unknown", 12345, high, "café", CodeCodepageUnknown},
		{"unknown, utf-8 text", 12345, []byte("한글\x00"), "한글", CodeCodepageUnknown},
		{"undeclared", 0, high, "café", CodeCodepageUndeclared},
	}
	for _, tt := range tests {
		w := newTW()
		if tt.cp != 0 {
			w.msg(AttOemCodepage, cat(u32(tt.cp), u32(0)))
		}
		m := mustDecode(t, w.msg(AttSubject, tt.subject).b, nil)
		if m.Subject != tt.want {
			t.Errorf("%s: Subject = %q, want %q", tt.name, m.Subject, tt.want)
		}
		if tt.issue == "" && len(m.Issues) != 0 || tt.issue != "" && !has(m, tt.issue) {
			t.Errorf("%s: issues = %v, want %q", tt.name, m.Issues, tt.issue)
		}
		if raw := m.Attributes[len(m.Attributes)-1].Data; !bytes.Equal(raw, tt.subject) {
			t.Errorf("%s: raw bytes = %x", tt.name, raw)
		}
	}

	// PR_INTERNET_CPID stands in when attOemCodepage is absent, and the
	// Unicode form of a property is preferred to its 8-bit one.
	m := mustDecode(t, newTW().msg(AttMAPIProps, props(
		pStr8(PropSubject, cp949Hangul), pLong(PropInternetCPID, 949),
		pStr8(PropAttachFilename, []byte("x")), pUni(PropBody, "본문"), pStr8(PropBody, []byte("8-bit")))).b, nil)
	if m.Subject != "한글" || m.Codepage != 949 || m.Body != "본문" || len(m.Issues) != 0 {
		t.Errorf("%+v", m)
	}
	if s, ok := m.Properties[0].Text(949); s != "한글" || !ok {
		t.Errorf("Property.Text = %q, %v", s, ok)
	}

	// Attachment names as 8-bit strings in the stream's code page.
	m = mustDecode(t, newTW().msg(AttOemCodepage, cat(u32(949), u32(0))).
		att(AttAttachRendData, make([]byte, 14)).
		att(AttAttachment, props(pStr8(PropAttachLongFilename, cp949Report), pStr8(PropAttachFilename, cp949Report[:6]))).b, nil)
	a := m.Attachments[0]
	if a.LongName != "보고서.txt" || a.Name != "보고서" || len(m.Issues) != 0 ||
		!bytes.Equal(a.Properties.Find(PropAttachLongFilename).Value, cat(cp949Report, []byte{0})) {
		t.Errorf("%+v", a)
	}
}

func TestBodies(t *testing.T) {
	rtf := func(s string) []byte { return pBin(PropRTFCompressed, lzfu([]byte(s))) }
	tests := []struct {
		name       string
		props      []byte
		html, text string
		fromRTF    bool
	}{
		{"html, utf-8 binary", props(pLong(PropInternetCPID, 65001), pBin(PropBodyHTML, []byte("<b>한글</b>"))), "<b>한글</b>", "", false},
		{"html, unicode", props(pUni(PropBodyHTML, "<b>한글</b>")), "<b>한글</b>", "", false},
		{"html in rtf", props(rtf(htmlRTF)), htmlWant, "", true},
		{"text in rtf", props(rtf(`{\rtf1\ansi\ansicpg1252\fromtext \deff0{\fonttbl{\f0 Arial;}}\pard caf\'e9\par line 2\tab x}`)), "", "café\nline 2\tx", true},
		{"plain rtf", props(rtf(`{\rtf1\ansi{\fonttbl{\f0 Arial;}}{\colortbl;\red0\green0\blue0;}{\*\generator Riched20;}\pard\b Bold\b0  and \u8364? 5\par}`)), "", "Bold and € 5\n", true},
	}
	for _, tt := range tests {
		m := mustDecode(t, newTW().msg(AttMAPIProps, tt.props).b, nil)
		if m.BodyHTML != tt.html || m.Body != tt.text || (m.HTMLFromRTF || m.TextFromRTF) != tt.fromRTF || len(m.Issues) != 0 {
			t.Errorf("%s: html=%q text=%q fromRTF=%v/%v issues=%v", tt.name, m.BodyHTML, m.Body, m.HTMLFromRTF, m.TextFromRTF, m.Issues)
		}
	}
}

func TestDecompressRTF(t *testing.T) {
	// The two examples of MS-OXRTFCP section 4.
	vectors := []struct{ in, want string }{
		{"\x2d\x00\x00\x00\x2b\x00\x00\x00\x4c\x5a\x46\x75\xf1\xc5\xc7\xa7\x03\x00\x0a\x00" +
			"\x72\x63\x70\x67\x31\x32\x35\x42\x32\x0a\xf3\x20\x68\x65\x6c\x09\x00\x20\x62\x77" +
			"\x05\xb0\x6c\x64\x7d\x0a\x80\x0f\xa0",
			"{\\rtf1\\ansi\\ansicpg1252\\pard hello world}\r\n"},
		{"\x1a\x00\x00\x00\x1c\x00\x00\x00\x4c\x5a\x46\x75\xe2\xd4\x4b\x51\x41\x00\x04\x20" +
			"\x57\x58\x59\x5a\x0d\x6e\x7d\x01\x0e\xb0",
			"{\\rtf1 WXYZWXYZWXYZWXYZWXYZ}"},
	}
	for i, v := range vectors {
		out, err := DecompressRTF([]byte(v.in), 1<<20)
		if err != nil || string(out) != v.want {
			t.Errorf("vector %d: %q, %v", i, out, err)
		}
	}
	eq(t, "dictionary", len(rtfDict), 207)

	doc := strings.Repeat(`{\rtf1 some text} `, 40)
	good := lzfu([]byte(doc))
	if out, err := DecompressRTF(good, 1<<20); err != nil || string(out) != doc {
		t.Errorf("round trip: %d bytes, %v", len(out), err)
	}
	mela := cat(u32(uint32(len(doc)+12)), u32(uint32(len(doc))), u32(rtfMELA), u32(0), []byte(doc))
	if out, err := DecompressRTF(mela, 1<<20); err != nil || string(out) != doc {
		t.Errorf("MELA: %d bytes, %v", len(out), err)
	}

	// A stream of nothing but the longest references: the most LZFu can
	// expand. The declared size is a lie and the limit holds.
	bomb := cat(u32(0xFFFFFFFF), u32(0xFFFFFFFF), u32(rtfLZFu), u32(0))
	for i, w := 0, len(rtfDict); i < 2000; i++ {
		bomb = append(bomb, 0xFF)
		for j := 0; j < 8; j, w = j+1, w+17 {
			ref := ((w-1)&4095)<<4 | 15 // the byte before, seventeen times
			bomb = append(bomb, byte(ref>>8), byte(ref))
		}
	}
	damaged := func(f func(b []byte) []byte) []byte { return f(append([]byte(nil), good...)) }
	tests := []struct {
		name    string
		in      []byte
		max     int
		wantLen int
		status  rtfStatus
	}{
		{"short header", good[:15], 1 << 20, 0, rtfShort},
		{"unknown type", damaged(func(b []byte) []byte { b[8] = 'X'; return b }), 1 << 20, 0, rtfUnknown},
		{"bad crc", damaged(func(b []byte) []byte { b[12] ^= 1; return b }), 1 << 20, len(doc), rtfBadCRC},
		{"cut", good[:len(good)-100], 1 << 20, -1, rtfCut | rtfBadCRC},
		{"limit", good, 100, 100, rtfLimit},
		{"zero limit", good, 0, 0, rtfLimit},
		{"MELA cut", mela[:len(mela)-10], 1 << 20, len(doc) - 10, rtfCut},
		{"MELA limit", mela, 7, 7, rtfLimit},
		{"bomb", bomb, 4096, 4096, rtfLimit | rtfBadCRC | rtfCut},
		{"bomb, no limit to speak of", bomb, 1 << 30, 2000 * 8 * 17, rtfBadCRC | rtfCut},
	}
	for _, tt := range tests {
		out, st := decompressRTF(tt.in, tt.max)
		if tt.wantLen >= 0 && len(out) != tt.wantLen || len(out) > tt.max {
			t.Errorf("%s: %d bytes, want %d", tt.name, len(out), tt.wantLen)
		}
		if st != tt.status {
			t.Errorf("%s: status %05b, want %05b", tt.name, st, tt.status)
		}
		if _, err := DecompressRTF(tt.in, tt.max); err == nil {
			t.Errorf("%s: no error", tt.name)
		}
	}
}

func TestExtractRTF(t *testing.T) {
	tests := []struct {
		name, in string
		cp, max  int
		kind     RTFKind
		want     string
		complete bool
	}{
		{"html", htmlRTF, 0, 1 << 20, RTFHTML, htmlWant, true},
		{"html, cut", htmlRTF, 0, 12, RTFHTML, "<html><head>", false},
		{"text", `{\rtf1\fromtext a\line b\\c}`, 0, 99, RTFText, "a\nb\\c", true},
		{"code page of the caller", `{\rtf1 \'c7\'d1\'b1\'db}`, 949, 99, RTFPlain, "한글", true},
		{"cut inside a character", `{\rtf1\ansicpg949 \'c7\'d1\'b1\'db}`, 0, 4, RTFPlain, "한", false},
		{"lone surrogates", `{\rtf1\uc0 \u-10179 x\u-8704 }`, 0, 99, RTFPlain, "\uFFFDx\uFFFD", true},
		{"quotes and dashes", `{\rtf1 \lquote a\rquote\~\emdash\_\bullet}`, 0, 99, RTFPlain, "‘a’\u00A0—-•", true},
		{"binary data", `{\rtf1 a{\pict\bin4 }}}}}b}`, 0, 99, RTFPlain, "ab", true},
		{"unbalanced", `}}{\rtf1 a}}}{{{b`, 0, 99, RTFPlain, "ab", true},
		{"too deep", strings.Repeat("{", maxRTFDepth+5) + "x", 0, 99, RTFPlain, "", false},
		{"empty", ``, 0, 99, RTFPlain, "", true},
		{"backslash at the end", `{\rtf1 a\`, 0, 99, RTFPlain, "a", true},
		{"the word in the text is not the header's", `{\rtf1 see \\fromhtml1 and \fromhtml1 x}`, 0, 99, RTFPlain, `see \fromhtml1 and x`, true},
		{"nothing allowed", `{\rtf1\fromtext abc}`, 0, 0, RTFText, "", false},
		{"nothing allowed, nothing there", `{\rtf1\fromhtml1}`, 0, -1, RTFHTML, "", true},
	}
	for _, tt := range tests {
		kind, got, complete := ExtractRTF([]byte(tt.in), tt.cp, tt.max)
		if kind != tt.kind || got != tt.want || complete != tt.complete {
			t.Errorf("%s: %v %q %v, want %v %q %v", tt.name, kind, got, complete, tt.kind, tt.want, tt.complete)
		}
	}
}

// Length fields that claim far more than the stream holds: what is there is
// read, the rest is reported, and nothing is allocated to their measure.
func TestAbsurdLengths(t *testing.T) {
	const huge = 0xFFFFFFFF
	guid := make([]byte, 16)
	open := func(id uint32, data []byte) []byte { // an attribute with a lying length
		w := newTW().msg(AttTnefVersion, u32(0x00010000))
		return cat(w.b, []byte{LevelMessage}, u32(id), u32(huge), data)
	}
	list := func(data ...[]byte) []byte { return newTW().msg(AttMAPIProps, cat(data...)).b }
	tests := []struct {
		name  string
		in    []byte
		issue Code
		check func(m *Message) bool
	}{
		{"attribute", open(AttBody, []byte("partial")), CodeTruncated,
			func(m *Message) bool { return m.Body == "partial" && m.Incomplete }},
		{"property count", list(u32(huge), pLong(PropInternetCPID, 949)), CodePropertyTruncated,
			func(m *Message) bool { return m.Codepage == 949 }},
		{"string length", list(u32(1), tag(TypeString8, PropSubject), u32(1), u32(huge), []byte("abc")), CodePropertyTruncated,
			func(m *Message) bool { return m.Subject == "abc" && m.Properties[0].Truncated }},
		{"value count", list(u32(1), tag(TypeBinary, 0x6000), u32(huge), u32(2), []byte("hi\x00\x00")), CodePropertyTruncated,
			func(m *Message) bool { return string(m.Properties[0].Value) == "hi" }},
		{"multi-value count", list(u32(1), tag(TypeInt32|TypeMultiValue, 0x6000), u32(huge), u32(1), u32(2)), CodePropertyTruncated,
			func(m *Message) bool { return len(m.Properties[0].Values) == 2 }},
		{"name length", list(u32(1), tag(TypeInt32, 0x8000), guid, u32(1), u32(huge), []byte("ab")), CodePropertyTruncated,
			func(m *Message) bool { return len(m.Properties) == 0 }},
		{"name kind", list(u32(1), tag(TypeInt32, 0x8000), guid, u32(9), u32(1)), CodePropertyType,
			func(m *Message) bool { return len(m.Properties) == 0 }},
		{"property type", list(u32(2), pLong(PropInternetCPID, 949), tag(0x0777, 0x6000), u32(1)), CodePropertyType,
			func(m *Message) bool { return len(m.Properties) == 1 }},
		{"recipient rows", newTW().msg(AttRecipTable, cat(u32(huge), props(pUni(PropDisplayName, "r")))).b, CodePropertyTruncated,
			func(m *Message) bool { return len(m.Recipients) == 1 && m.Recipients[0].Name == "r" }},
		{"rtf sizes", list(props(pBin(PropRTFCompressed, cat(u32(huge), u32(huge), u32(rtfLZFu), u32(0), []byte{0}, []byte(`{\rtf1 a`))))), CodeRTFDamaged,
			func(m *Message) bool { return string(m.RTF) == `{\rtf1 a` && m.RTFTruncated && m.Body == "a" }},
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for _, tt := range tests {
		m := mustDecode(t, tt.in, nil)
		if !has(m, tt.issue) || !tt.check(m) {
			t.Errorf("%s: issues=%v message=%+v", tt.name, m.Issues, m)
		}
	}
	runtime.ReadMemStats(&after)
	if n := after.TotalAlloc - before.TotalAlloc; n > 1<<20 {
		t.Errorf("%d bytes allocated for a few hundred bytes of input", n)
	}
}

func TestLimits(t *testing.T) {
	full := fixture().b
	tests := []struct {
		name  string
		lim   Limits
		issue Code
		check func(m *Message) bool
	}{
		{"input", Limits{MaxInputBytes: 200}, CodeLimitInput,
			func(m *Message) bool {
				return m.Incomplete && m.Subject == "안녕하세요" && len(m.Attachments) == 0
			}},
		{"no input", Limits{MaxInputBytes: -1}, CodeLimitInput,
			func(m *Message) bool { return m.Incomplete && len(m.Attributes) == 0 }},
		{"attributes", Limits{MaxAttributes: 5}, CodeLimitAttributes,
			func(m *Message) bool { return m.Incomplete && attributes(m) == 5 && m.Subject == "안녕하세요" }},
		{"attachments", Limits{MaxAttachments: 1}, CodeLimitAttachments,
			func(m *Message) bool { return len(m.Attachments) == 1 && m.Attachments[0].LongName == longName }},
		{"properties of a list", Limits{MaxProperties: 2}, CodeLimitProperties,
			func(m *Message) bool { return len(m.Properties) == 2 && len(m.Attachments[0].Properties) == 2 }},
		{"properties in all", Limits{MaxTotalProperties: 7}, CodeLimitProperties,
			func(m *Message) bool { return len(m.Properties) == 7 && len(m.Attachments[0].Properties) == 0 }},
		{"attachment bytes", Limits{MaxAttachmentBytes: 3}, CodeLimitAttachmentBytes,
			func(m *Message) bool {
				a := m.Attachments[0]
				return string(a.Data) == "hel" && a.Truncated && a.Size == 5 && m.Attachments[1].Embedded != nil
			}},
		{"decoded bytes", Limits{MaxDecodedBytes: 40}, CodeLimitDecodedBytes,
			func(m *Message) bool { return handed(m) <= 40 && m.Subject == "안녕하세요" }},
		{"rtf bytes", Limits{MaxRTFBytes: 16}, CodeLimitRTFBytes,
			func(m *Message) bool { return len(m.RTF) == 16 && m.RTFTruncated }},
		{"depth", Limits{MaxDepth: -1}, CodeLimitDepth,
			func(m *Message) bool { return m.Attachments[1].Embedded == nil && len(m.Attachments[1].Data) > 0 }},
		{"string bytes", Limits{MaxStringBytes: 6}, CodeLimitStringBytes,
			func(m *Message) bool { return m.Subject == "안녕하" && m.Body == "plain 한글" }},
		{"body bytes", Limits{MaxBodyBytes: 5}, CodeLimitBodyBytes,
			func(m *Message) bool {
				return m.Body == "plain" && m.BodyHTML == "<p>한" && m.Subject == "안녕하세요"
			}},
	}
	for _, tt := range tests {
		m := mustDecode(t, full, &tt.lim)
		if !has(m, tt.issue) || !tt.check(m) {
			t.Errorf("%s: issues=%v", tt.name, m.Issues)
		}
	}

	m := mustDecode(t, full[:len(full)-3], &Limits{MaxIssues: -1})
	if len(m.Issues) != 0 || m.IssuesDropped == 0 {
		t.Errorf("issues: %v, %d dropped", m.Issues, m.IssuesDropped)
	}

	// Messages nested deeper than the default allows.
	nested := newTW().msg(AttSubject, []byte("level 9\x00")).b
	for i := 8; i >= 0; i-- {
		nested = newTW().msg(AttSubject, []byte{'l', byte('0' + i), 0}).
			att(AttAttachRendData, nil).att(AttAttachment, props(pObj(PropAttachData, nested))).b
	}
	m = mustDecode(t, nested, nil)
	depth := 0
	for len(m.Attachments) == 1 && m.Attachments[0].Embedded != nil {
		m, depth = m.Attachments[0].Embedded, depth+1
	}
	if depth != 4 || m.Subject != "l4" {
		t.Errorf("read %d levels deep, to %q", depth, m.Subject)
	}
}

// handed is how many bytes of content a message hands its caller, counted as
// Limits.MaxDecodedBytes counts them.
func handed(m *Message) int {
	strs := func(ss ...string) (n int) {
		for _, s := range ss {
			n += len(s)
		}
		return n
	}
	names := func(ps Properties) (n int) {
		for i := range ps {
			if ps[i].Name != nil {
				n += len(ps[i].Name.Name)
			}
		}
		return n
	}
	n := strs(m.Subject, m.MessageClass, m.MessageID, m.InternetMessageID, m.From.Name, m.From.Address, m.From.Type,
		m.Body, m.BodyHTML) + len(m.RTF) + names(m.Properties)
	for i := range m.Recipients {
		r := &m.Recipients[i]
		n += strs(r.Name, r.Address.Address, r.Type) + names(r.Properties)
	}
	for i := range m.Attachments {
		a := &m.Attachments[i]
		n += strs(a.Name, a.LongName, a.DisplayName, a.Extension, a.MIMEType, a.ContentID, a.ContentLocation)
		n += len(a.Data) + len(a.MetaFile) + names(a.Properties)
		if a.Embedded != nil {
			n += handed(a.Embedded)
		}
	}
	return n
}
