package mail_test

import (
	"fmt"
	"io"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// Malformed prefixes through mail.Reader (ROB-020-001). A part whose header
// begins with lines of white space keeps them as its malformed prefix, is
// classified by the fields after them, and the parts after it are read. Every
// source is a finite string.

type firstLineMailPart struct {
	attachment bool
	prefix     string
	subject    string
	body       string
}

func readFirstLineMailParts(t *testing.T, label string, r *mail.Reader, refused map[int]bool) []firstLineMailPart {
	t.Helper()
	var parts []firstLineMailPart
	for i := range 8 {
		p, err := r.NextPart()
		if err == io.EOF {
			return parts
		}
		if refused[i] {
			if p != nil || !message.IsFatal(err) {
				t.Errorf("%s: part %d = (%v, %v), want no part and the refusal", label, i, p != nil, err)
			}
			parts = append(parts, firstLineMailPart{})
			continue
		}
		if err != nil {
			t.Fatalf("%s: part %d: %v", label, i, err)
		}
		body, err := io.ReadAll(p.Body)
		if err != nil {
			t.Fatalf("%s: part %d body: %v", label, i, err)
		}
		var mp firstLineMailPart
		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			mp = firstLineMailPart{false, string(h.MalformedPrefix().Bytes()), h.Get("Subject"), string(body)}
		case *mail.AttachmentHeader:
			mp = firstLineMailPart{true, string(h.MalformedPrefix().Bytes()), h.Get("Subject"), string(body)}
		default:
			t.Fatalf("%s: part %d header %T", label, i, p.Header)
		}
		parts = append(parts, mp)
	}
	t.Fatalf("%s: more parts than the message has", label)
	return nil
}

// TestReader_firstLine pins the parts of a multipart whose middle part's
// header begins with a prefix, an indented Content-Type among it, through
// NewReader over Parse under every profile and through CreateReader: the part
// is inline text, its prefix kept, and the last part follows. Strict refuses
// the middle part alone.
func TestReader_firstLine(t *testing.T) {
	text := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nSubject: first\r\n\r\none\r\n" +
		"--B\r\n Content-Type: application/octet-stream\r\nSubject: middle\r\n\r\ntwo\r\n" +
		"--B\r\nSubject: last\r\n\r\nthree\r\n--B--\r\n"
	want := []firstLineMailPart{
		{false, "", "first", "one"},
		{false, " Content-Type: application/octet-stream\r\n", "middle", "two"},
		{false, "", "last", "three"},
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
			refused = map[int]bool{1: true}
			expect = []firstLineMailPart{want[0], {}, want[2]}
		}
		if got := readFirstLineMailParts(t, label, mail.NewReader(e), refused); fmt.Sprint(got) != fmt.Sprint(expect) {
			t.Errorf("%s: parts %v, want %v", label, got, expect)
		}
	}

	r, err := mail.CreateReader(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if got := readFirstLineMailParts(t, "CreateReader", r, nil); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("CreateReader: parts %v, want %v", got, want)
	}

	// A message whose own header begins with a prefix is its own one part,
	// whose header is the message's.
	r, err = mail.CreateReader(strings.NewReader(" Subject: shifted\r\nSubject: kept\r\n\r\nhi"))
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Header.MalformedPrefix().Bytes()) != " Subject: shifted\r\n" || r.Header.Get("Subject") != "kept" {
		t.Errorf("root header: prefix %q, Subject %q", r.Header.MalformedPrefix().Bytes(), r.Header.Get("Subject"))
	}
	if got := readFirstLineMailParts(t, "root", r, nil); fmt.Sprint(got) != fmt.Sprint([]firstLineMailPart{{false, " Subject: shifted\r\n", "kept", "hi"}}) {
		t.Errorf("root: parts %v", got)
	}
}
