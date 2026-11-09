package mail_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// ROB-001-009 (issue #3): Limits.MaxInputBytes reaches mail.Reader through the
// parse it reads, so a part past the budget ends the reader with the parse's
// one stop, while every part and byte before it stays the caller's.

func parseUnderInput(t *testing.T, text string, limit int64) (*message.ParseSession, *message.Entity) {
	t.Helper()
	session, e, err := message.Parse(strings.NewReader(text), &message.ParseOptions{
		Limits: message.Limits{MaxInputBytes: limit},
	})
	if err != nil || e == nil {
		t.Fatalf("Parse() = (%v, %v), want the header", e, err)
	}
	return session, e
}

func onlyInputDiagnostic(t *testing.T, session *message.ParseSession, limit int64) {
	t.Helper()
	diags := session.Diagnostics()
	if len(diags) != 1 || diags[0].Code != message.CodeLimitInputBytes || diags[0].Location != message.LocationAt(limit) {
		t.Errorf("Diagnostics() = %v, want one limit.input_bytes at offset %d", diags, limit)
	}
}

func TestReader_inputBytesMultipart(t *testing.T) {
	text := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nzero\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n\r\n" + strings.Repeat("1", 200) + "\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\ntwo\r\n" +
		"--B--\r\n"
	start := int64(strings.Index(text, "111"))
	limit := start + 50

	for _, skip := range []bool{false, true} {
		session, e := parseUnderInput(t, text, limit)
		r := mail.NewReader(e)

		p, err := r.NextPart()
		if err != nil {
			t.Fatalf("NextPart() = %v", err)
		}
		if b, err := io.ReadAll(p.Body); err != nil || string(b) != "zero" {
			t.Fatalf("first body = (%q, %v)", b, err)
		}

		p, err = r.NextPart()
		if err != nil {
			t.Fatalf("NextPart() = %v, want the attachment: its header fits", err)
		}
		if _, ok := p.Header.(*mail.AttachmentHeader); !ok {
			t.Errorf("second part is %T, want an attachment", p.Header)
		}
		if !skip {
			b, err := io.ReadAll(p.Body)
			if want := text[start:limit]; string(b) != want {
				t.Errorf("attachment = %q, want the admitted %q", b, want)
			}
			if stop := e.ParseStop(); stop == nil || !errors.Is(err, stop) || !message.IsFatal(err) {
				t.Errorf("attachment error = %v, want the stop %v", err, stop)
			}
		}

		// Read or skipped, the rest of the message is past the budget.
		for range 2 {
			p, err := r.NextPart()
			if stop := e.ParseStop(); p != nil || stop == nil || err != stop {
				t.Errorf("skip %v: NextPart() = (%v, %v), want the identical stop %v", skip, p, err, stop)
			}
		}
		onlyInputDiagnostic(t, session, limit)
	}
}

func TestReader_inputBytesSinglePart(t *testing.T) {
	const header = "Content-Type: text/plain\r\n\r\n"
	text := header + strings.Repeat("s", 100)
	limit := int64(len(header) + 40)

	session, e := parseUnderInput(t, text, limit)
	r := mail.NewReader(e)
	p, err := r.NextPart()
	if err != nil {
		t.Fatalf("NextPart() = %v", err)
	}
	b, err := io.ReadAll(p.Body)
	stop := e.ParseStop()
	if string(b) != strings.Repeat("s", 40) || stop == nil || err != stop {
		t.Errorf("body = (%d bytes, %v), want 40 and the identical stop %v", len(b), err, stop)
	}
	if p, err := r.NextPart(); p != nil || err != stop {
		t.Errorf("NextPart() = (%v, %v), want the stop", p, err)
	}
	onlyInputDiagnostic(t, session, limit)
}
