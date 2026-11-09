package mail_test

import (
	"io"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// ROB-001-010 (issue #5): a shared header total ends mail.Reader with the
// parse's one stop, while every part before it stays the caller's.
func TestReader_totalHeaderStop(t *testing.T) {
	const root = "Content-Type: multipart/mixed; boundary=B\r\n\r\n"
	const first = "Content-Type: text/plain\r\n\r\n"
	text := root + "--B\r\n" + first + "zero\r\n--B\r\nContent-Type: text/plain\r\n\r\none\r\n--B--\r\n"

	for _, l := range []message.Limits{
		{MaxTotalHeaderBytes: int64(len(root) + len(first))},
		{MaxTotalHeaderCount: 2},
	} {
		session, e, err := message.Parse(strings.NewReader(text), &message.ParseOptions{Limits: l})
		if err != nil {
			t.Fatal(err)
		}
		r := mail.NewReader(e)
		p, err := r.NextPart()
		if err != nil {
			t.Fatalf("first = %v", err)
		}
		if b, err := io.ReadAll(p.Body); err != nil || string(b) != "zero" {
			t.Fatalf("first body = (%q, %v)", b, err)
		}
		for range 2 {
			p, err := r.NextPart()
			if stop := e.ParseStop(); p != nil || stop == nil || err != stop || !message.IsFatal(err) {
				t.Errorf("%+v: NextPart() = (%v, %v), want the identical stop %v", l, p, err, stop)
			}
		}
		if d := session.Diagnostics(); len(d) != 1 || d[0].Code.Group() != "limit" {
			t.Errorf("Diagnostics() = %v", d)
		}
	}
}
