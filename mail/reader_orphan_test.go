package mail_test

import (
	"fmt"
	"io"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// Orphan continuation lines through mail.Reader (ROB-020-002). A part whose
// header holds a damaged line and continuation lines after it keeps them as an
// orphan record, is classified by its fields alone — an indented
// Content-Disposition or Content-Type in the record decides nothing — and the
// parts after it are read. Every source is a finite string.

type orphanMailPart struct {
	attachment bool
	records    string
	subject    string
	body       string
}

func readOrphanMailParts(t *testing.T, label string, r *mail.Reader, refused map[int]bool) []orphanMailPart {
	t.Helper()
	var parts []orphanMailPart
	for i := range 8 {
		p, err := r.NextPart()
		if err == io.EOF {
			return parts
		}
		if refused[i] {
			if p != nil || !message.IsFatal(err) {
				t.Errorf("%s: part %d = (%v, %v), want no part and the refusal", label, i, p != nil, err)
			}
			parts = append(parts, orphanMailPart{})
			continue
		}
		if err != nil {
			t.Fatalf("%s: part %d: %v", label, i, err)
		}
		body, err := io.ReadAll(p.Body)
		if err != nil {
			t.Fatalf("%s: part %d body: %v", label, i, err)
		}
		var mp orphanMailPart
		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			mp = orphanMailPart{false, recordsText(h.OrphanContinuations()), h.Get("Subject"), string(body)}
		case *mail.AttachmentHeader:
			mp = orphanMailPart{true, recordsText(h.OrphanContinuations()), h.Get("Subject"), string(body)}
		default:
			t.Fatalf("%s: part %d header %T", label, i, p.Header)
		}
		parts = append(parts, mp)
	}
	t.Fatalf("%s: more parts than the message has", label)
	return nil
}

func recordsText[T interface{ Bytes() []byte }](records []T) string {
	var b strings.Builder
	for _, r := range records {
		b.Write(r.Bytes())
	}
	return b.String()
}

// TestReader_orphanContinuation pins the parts of a multipart whose middle
// part's header holds a record, an indented Content-Disposition and
// Content-Type among it, through NewReader over Parse under every profile and
// through CreateReader: the part is inline text, its record kept, and the last
// part follows. Strict refuses the middle part alone.
func TestReader_orphanContinuation(t *testing.T) {
	const record = "broken\r\n Content-Disposition: attachment; filename=x.bin\r\n Content-Type: application/octet-stream\r\n"
	text := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nSubject: first\r\n\r\none\r\n" +
		"--B\r\nSubject: middle\r\n" + record + "\r\ntwo\r\n" +
		"--B\r\nSubject: last\r\n\r\nthree\r\n--B--\r\n"
	want := []orphanMailPart{
		{false, "", "first", "one"},
		{false, record, "middle", "two"},
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
			expect = []orphanMailPart{want[0], {}, want[2]}
		}
		if got := readOrphanMailParts(t, label, mail.NewReader(e), refused); fmt.Sprint(got) != fmt.Sprint(expect) {
			t.Errorf("%s: parts %v, want %v", label, got, expect)
		}
	}

	r, err := mail.CreateReader(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if got := readOrphanMailParts(t, "CreateReader", r, nil); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("CreateReader: parts %v, want %v", got, want)
	}

	// A message whose own header holds a record is its own one part, whose
	// header is the message's.
	r, err = mail.CreateReader(strings.NewReader("Subject: kept\r\n: x\r\n Subject: shifted\r\n\r\nhi"))
	if err != nil {
		t.Fatal(err)
	}
	if got := recordsText(r.Header.OrphanContinuations()); got != ": x\r\n Subject: shifted\r\n" || r.Header.Get("Subject") != "kept" {
		t.Errorf("root header: records %q, Subject %q", got, r.Header.Get("Subject"))
	}
	if got := readOrphanMailParts(t, "root", r, nil); fmt.Sprint(got) != fmt.Sprint([]orphanMailPart{{false, ": x\r\n Subject: shifted\r\n", "kept", "hi"}}) {
		t.Errorf("root: parts %v", got)
	}
}
