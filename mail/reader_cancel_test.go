package mail_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// ROB-001-006 (issue #6): the cancellation of a message.ParseContext reaches
// mail.Reader through the parse it reads. NextPart returns the parse's stop,
// and a Part.Body already returned reports it from its next read, while the
// bytes already read stay the caller's. The source is never closed by either.

func TestReader_cancellation(t *testing.T) {
	text := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nzero zero zero\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n\r\none\r\n" +
		"--B--\r\n"
	for _, single := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		in := text
		if single {
			in = "Content-Type: text/plain\r\n\r\nzero zero zero"
		}
		_, e, err := message.ParseContext(ctx, strings.NewReader(in), nil)
		if err != nil {
			t.Fatal(err)
		}
		r := mail.NewReader(e)
		p, err := r.NextPart()
		if err != nil {
			t.Fatalf("NextPart() = %v", err)
		}
		buf := make([]byte, 4)
		if _, err := io.ReadFull(p.Body, buf); err != nil || string(buf) != "zero" {
			t.Fatalf("first bytes = %q, %v", buf, err)
		}
		cancel()
		stop := e.ParseStop()
		if stop == nil || !message.IsFatal(stop) || !errors.Is(stop, context.Canceled) {
			t.Fatalf("ParseStop() = %v, want the cancellation", stop)
		}
		if n, err := p.Body.Read(buf); n != 0 || err != stop {
			t.Errorf("single %v: part read after cancellation = %d, %v; want the stop", single, n, err)
		}
		for range 2 {
			if p, err := r.NextPart(); p != nil || err != stop {
				t.Errorf("single %v: NextPart() = %v, %v; want the identical stop", single, p, err)
			}
		}
	}
}
