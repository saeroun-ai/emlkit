package mail_test

import (
	"io"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// The input after the message through mail.Reader (ROB-010-005). mail.Reader
// iterates the message's own multipart body like any caller, so under
// ProfileForensic its end at the closing delimiter makes the trailing input
// available on the session; the reader itself never reads it. Every source is
// a finite string and every loop capped.

const trailingMailMessage = "From: header@example.com\r\n" +
	"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
	"--B\r\nContent-Type: text/plain\r\n\r\ntext body\r\n" +
	"--B\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=a.bin\r\n\r\nbinary\r\n" +
	"--B--\r\n"

const trailingMailSecond = "From: second@example.com\r\nTo: x@example.com\r\n\r\nsecond body\r\n"

// readAllCapped reads r whole, failing the test past 64 KiB.
func readAllCapped(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(io.LimitReader(r, 64<<10+1))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(b) > 64<<10 {
		t.Fatalf("stream longer than the test cap")
	}
	return string(b)
}

// readMailParts reads every part of r, at most 8, and returns how many.
func readMailParts(t *testing.T, r *mail.Reader) int {
	t.Helper()
	for i := 0; i < 8; i++ {
		p, err := r.NextPart()
		if err == io.EOF {
			return i
		}
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		readAllCapped(t, p.Body)
	}
	t.Fatalf("more than 8 parts")
	return 0
}

func TestReader_trailingInput(t *testing.T) {
	session, e, err := message.Parse(strings.NewReader(trailingMailMessage+trailingMailSecond), &message.ParseOptions{Profile: message.ProfileForensic})
	if e == nil || err != nil {
		t.Fatalf("Parse() = (%v, %v)", e != nil, err)
	}
	r := mail.NewReader(e)
	if n := readMailParts(t, r); n != 2 {
		t.Fatalf("%d parts, want the first message's two", n)
	}
	tr, ok := session.TrailingInputReader()
	if !ok {
		t.Fatal("no trailing input after the reader's end")
	}
	if got := readAllCapped(t, tr); got != trailingMailSecond {
		t.Errorf("trailing input %q", got)
	}
	ti := session.TrailingInput()
	if at, _ := ti.Start(); ti.Status() != message.TrailingEnded || at != int64(len(trailingMailMessage)) {
		t.Errorf("%v at %d", ti.Status(), at)
	}
	if ev, _ := ti.Evidence(); ev != message.TrailingEvidenceMessageLike {
		t.Errorf("evidence %v", ev)
	}
	found := 0
	for _, d := range session.Diagnostics() {
		if d.Code == message.CodeInputTrailingMessages {
			found++
		}
	}
	if found != 1 {
		t.Errorf("%d input.trailing_messages diagnostics", found)
	}
	// The reader has nothing more, and reading the trailing input took nothing
	// from it.
	if _, err := r.NextPart(); err != io.EOF {
		t.Errorf("NextPart after the trailing input = %v", err)
	}
}

// TestReader_trailingInputTolerant pins that mail.Reader under the default
// profile is unchanged: no trailing input is offered.
func TestReader_trailingInputTolerant(t *testing.T) {
	session, e, _ := message.Parse(strings.NewReader(trailingMailMessage+trailingMailSecond), nil)
	readMailParts(t, mail.NewReader(e))
	if r, ok := session.TrailingInputReader(); ok || r != nil {
		t.Errorf("a trailing input reader under the default profile")
	}
	if st := session.TrailingInput().Status(); st != message.TrailingUnknown {
		t.Errorf("status %v", st)
	}
}

// TestReader_trailingInputSinglePart pins a single-part message read through
// mail.Reader: a message pasted into its body stays body, and there is no
// trailing input.
func TestReader_trailingInputSinglePart(t *testing.T) {
	head := "From: header@example.com\r\nContent-Type: text/plain\r\n\r\n"
	body := "first body\r\n\r\n" + trailingMailSecond
	session, e, err := message.Parse(strings.NewReader(head+body), &message.ParseOptions{Profile: message.ProfileForensic})
	if e == nil || err != nil {
		t.Fatalf("Parse() = (%v, %v)", e != nil, err)
	}
	r := mail.NewReader(e)
	p, err := r.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if got := readAllCapped(t, p.Body); got != body {
		t.Errorf("body %q", got)
	}
	if _, err := r.NextPart(); err != io.EOF {
		t.Errorf("a second part: %v", err)
	}
	if st := session.TrailingInput().Status(); st != message.TrailingNone {
		t.Errorf("status %v", st)
	}
	if _, ok := session.TrailingInputReader(); ok {
		t.Errorf("a trailing input reader for a single-part message")
	}
}
