package mail_test

import (
	"fmt"
	"io"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
	"github.com/saeroun-ai/emlkit/textproto"
)

// Byte order marks through mail.Reader (ROB-010-003). A part whose header
// begins with a UTF-8 mark is classified by its fields as usual; a part whose
// header block began with a UTF-16 or UTF-32 mark was not read, declares
// nothing, and is an attachment whose body is its every byte from the mark on.
// Every source is a finite string.

const bomMailUTF8 = "\xef\xbb\xbf"

// bomMailUTF16LE encodes ascii as UTF-16LE after its mark.
func bomMailUTF16LE(ascii string) string {
	var b strings.Builder
	b.WriteString("\xff\xfe")
	for i := 0; i < len(ascii); i++ {
		b.WriteByte(ascii[i])
		b.WriteByte(0)
	}
	return b.String()
}

type bomMailPart struct {
	attachment bool
	mark       textproto.ByteOrderMark
	body       string
}

func readBOMMailParts(t *testing.T, label string, r *mail.Reader, refused map[int]bool) []bomMailPart {
	t.Helper()
	var parts []bomMailPart
	for i := 0; i < 8; i++ {
		p, err := r.NextPart()
		if err == io.EOF {
			return parts
		}
		if refused[i] {
			if p != nil || !message.IsFatal(err) {
				t.Errorf("%s: part %d = (%v, %v), want no part and the refusal", label, i, p != nil, err)
			}
			parts = append(parts, bomMailPart{})
			continue
		}
		if err != nil {
			t.Fatalf("%s: part %d: %v", label, i, err)
		}
		body, err := io.ReadAll(p.Body)
		if err != nil {
			t.Fatalf("%s: part %d body: %v", label, i, err)
		}
		var mp bomMailPart
		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			mp = bomMailPart{false, h.ByteOrderMark(), string(body)}
		case *mail.AttachmentHeader:
			mp = bomMailPart{true, h.ByteOrderMark(), string(body)}
		default:
			t.Fatalf("%s: part %d header %T", label, i, p.Header)
		}
		parts = append(parts, mp)
	}
	t.Fatalf("%s: more parts than the message has", label)
	return nil
}

// TestReader_byteOrderMark pins the classification of marked parts through
// NewReader over Parse under every profile, and through CreateReader.
func TestReader_byteOrderMark(t *testing.T) {
	wide := bomMailUTF16LE("Content-Type: text/html\r\n\r\nx")
	text := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\n" + bomMailUTF8 + "Content-Type: text/plain\r\n\r\none\r\n" +
		"--B\r\n" + wide + "\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nthree\r\n--B--\r\n"
	want := []bomMailPart{
		{false, textproto.BOMUTF8, "one"},
		{true, textproto.BOMUTF16LE, wide},
		{false, textproto.BOMNone, "three"},
	}

	for _, profile := range []message.Profile{message.ProfileTolerant, message.ProfileForensic, message.ProfileStrict} {
		label := fmt.Sprint(profile)
		_, e, err := message.Parse(strings.NewReader(text), &message.ParseOptions{Profile: profile})
		if err != nil {
			t.Fatalf("%s: Parse() = %v", label, err)
		}
		refused := map[int]bool{}
		expect := want
		if profile == message.ProfileStrict {
			// Strict refuses both marked parts, each with no part; the
			// iteration goes on to the third.
			refused = map[int]bool{0: true, 1: true}
			expect = []bomMailPart{{}, {}, want[2]}
		}
		got := readBOMMailParts(t, label, mail.NewReader(e), refused)
		if fmt.Sprint(got) != fmt.Sprint(expect) {
			t.Errorf("%s: parts %v, want %v", label, got, expect)
		}
	}

	r, err := mail.CreateReader(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if got := readBOMMailParts(t, "CreateReader", r, nil); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("CreateReader: parts %v, want %v", got, want)
	}

	// A message whose own header block began with a UTF-16 mark is its own one
	// part, an attachment holding every byte.
	whole := bomMailUTF16LE("From: a@example.com\r\n\r\nhi\r\n")
	r, err = mail.CreateReader(strings.NewReader(whole))
	if err != nil {
		t.Fatal(err)
	}
	if r.Header.ByteOrderMark() != textproto.BOMUTF16LE || r.Header.Len() != 0 {
		t.Errorf("root header: mark %v with %d fields, want UTF-16LE and none", r.Header.ByteOrderMark(), r.Header.Len())
	}
	if got := readBOMMailParts(t, "wide root", r, nil); fmt.Sprint(got) != fmt.Sprint([]bomMailPart{{true, textproto.BOMUTF16LE, whole}}) {
		t.Errorf("wide root: parts %v", got)
	}
}
