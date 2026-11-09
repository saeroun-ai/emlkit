package mail_test

import (
	"io"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// An mbox envelope line through mail.Reader (ROB-010-004). The mail package
// never recognizes one itself: a caller parses with message.Parse and
// RecognizeMboxEnvelope, keeps the session for the line, and reads the message
// through mail.NewReader as any other. The line's sender and timestamp are not
// the message's From or Date. Every source is a finite string.

const mboxMailEnv = "From envelope@example.net Thu Jan  1 00:00:00 2026\r\n"

const mboxMailMessage = "From: header@example.com\r\n" +
	"Subject: hello\r\n" +
	"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
	"--B\r\nContent-Type: text/plain\r\n\r\ntext body\r\n" +
	"--B\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=a.bin\r\n\r\nbinary\r\n" +
	"--B--\r\n"

// TestReader_mboxEnvelope pins a message read through mail.Reader after its
// envelope was kept apart: the header's own fields, both parts, and the line
// on the session, unchanged by the reading.
func TestReader_mboxEnvelope(t *testing.T) {
	session, e, err := message.Parse(strings.NewReader(mboxMailEnv+mboxMailMessage), &message.ParseOptions{RecognizeMboxEnvelope: true})
	if e == nil || err != nil {
		t.Fatalf("Parse() = (%v, %v), want the root", e != nil, err)
	}
	r := mail.NewReader(e)
	from, err := r.Header.AddressList("From")
	if err != nil || len(from) != 1 || from[0].Address != "header@example.com" {
		t.Errorf("From = (%v, %v), want the header's own address, not the envelope's", from, err)
	}
	if s, err := r.Header.Subject(); s != "hello" || err != nil {
		t.Errorf("Subject() = (%q, %v), want hello", s, err)
	}
	var got []string
	for i := 0; i < 4; i++ {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		body, err := io.ReadAll(p.Body)
		if err != nil {
			t.Fatalf("part %d body: %v", i, err)
		}
		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			got = append(got, "inline "+string(body))
		case *mail.AttachmentHeader:
			name, _ := h.Filename()
			got = append(got, "attachment "+name+" "+string(body))
		}
	}
	if want := []string{"inline text body", "attachment a.bin binary"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("parts %q, want %q", got, want)
	}
	env, ok := session.MboxEnvelope()
	if !ok || string(env.Bytes()) != mboxMailEnv || env.Location() != message.LocationRange(0, int64(len(mboxMailEnv))) {
		t.Errorf("MboxEnvelope() = (%q at %+v, %v), want %q at the start", env.Bytes(), env.Location(), ok, mboxMailEnv)
	}
}

// TestCreateReader_mboxEnvelope pins that mail.CreateReader, which has no
// options, does not recognize the line: it fails on it as it always did.
func TestCreateReader_mboxEnvelope(t *testing.T) {
	if r, err := mail.CreateReader(strings.NewReader(mboxMailEnv + mboxMailMessage)); r != nil || err == nil {
		t.Errorf("CreateReader() = (%v, %v), want the malformed header line it always reported", r != nil, err)
	}
}
