package message

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

const completionMessage = "Subject: s\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n" +
	"--B\r\nContent-Type: text/plain\r\n\r\none\r\n" +
	"--B\r\nContent-Type: text/plain\r\n\r\ntwo\r\n" +
	"--B--\r\n"

func TestCompletion_states(t *testing.T) {
	// Header only: nothing has been established about the body.
	s, e, err := Parse(strings.NewReader(completionMessage), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := s.Completion(); c.State != CompletionNotConsumed || c.Clean() || c.Entities != 1 || c.Multiparts != 1 {
		t.Errorf("after Parse: %+v", c)
	}

	// One part asked for and not read.
	mr := e.MultipartReader()
	p, _ := mr.NextPart()
	if c := s.Completion(); c.State != CompletionInProgress || c.Bodies != 1 || c.BodiesRead != 0 {
		t.Errorf("one part returned: %+v", c)
	}
	// Read in part.
	p.Body.Read(make([]byte, 1))
	if c := s.Completion(); c.State != CompletionInProgress || c.BodiesRead != 0 {
		t.Errorf("one byte read: %+v", c)
	}
	// The first part skipped, the rest iterated: still not a clean result.
	p2, _ := mr.NextPart()
	io.Copy(io.Discard, p2.Body)
	if _, err := mr.NextPart(); err != io.EOF {
		t.Fatal(err)
	}
	c := s.Completion()
	if c.State != CompletionInProgress || c.Bodies != 2 || c.BodiesRead != 1 || c.MultipartsRead != 1 || c.Clean() {
		t.Errorf("a part skipped: %+v", c)
	}

	// Drained: complete and clean, and stays so when drained again.
	s, e, _ = Parse(strings.NewReader(completionMessage), nil)
	for i := 0; i < 2; i++ {
		if err := e.Drain(); err != nil {
			t.Fatalf("Drain() = %v", err)
		}
		c = s.Completion()
		if c.State != CompletionComplete || !c.Clean() || c.Entities != 3 || c.BodiesRead != 2 || c.InputBytes != int64(len(completionMessage)) {
			t.Errorf("drained %d: %+v", i, c)
		}
	}

	// A single-part message.
	s, e, _ = Parse(strings.NewReader("Subject: s\r\n\r\nbody"), nil)
	if c := s.Completion(); c.State != CompletionNotConsumed {
		t.Errorf("single part, unread: %+v", c)
	}
	e.Drain()
	if c := s.Completion(); c.State != CompletionComplete || !c.Clean() {
		t.Errorf("single part, drained: %+v", c)
	}
}

func TestCompletion_defects(t *testing.T) {
	drained := func(text string, opts *ParseOptions) (Completion, error) {
		s, e, err := Parse(strings.NewReader(text), opts)
		if e == nil {
			return s.Completion(), err
		}
		derr := e.Drain()
		return s.Completion(), derr
	}

	// Lost syntax: complete reading, incomplete message.
	c, _ := drained(strings.TrimSuffix(completionMessage, "--B--\r\n"), nil)
	if c.State != CompletionIncomplete || c.Clean() || c.Diagnostics[SeverityIncomplete] != 1 {
		t.Errorf("missing close: %+v", c)
	}

	// Repaired, nothing lost: complete, not clean.
	c, _ = drained("Subject: s\r\nContent-Transfer-Encoding: base64\r\n\r\naGk!=\r\n", nil)
	if c.State != CompletionComplete || c.Clean() || c.Diagnostics[SeverityRecovered] != 1 {
		t.Errorf("repaired: %+v", c)
	}

	// Ambiguous.
	c, _ = drained("Content-Type: text/html\r\nContent-Type: text/plain\r\n\r\nx", nil)
	if c.State != CompletionComplete || !c.Ambiguous || c.Clean() {
		t.Errorf("ambiguous: %+v", c)
	}

	// A budget that ends the whole parse.
	c, derr := drained(completionMessage, &ParseOptions{Limits: Limits{MaxPartCount: 1}})
	if c.State != CompletionLimited || c.Stop == nil || !IsFatal(derr) {
		t.Errorf("part count: %+v, %v", c, derr)
	}
	// A budget that ends one body.
	c, _ = drained("Subject: s\r\n\r\n0123456789", &ParseOptions{Limits: Limits{MaxDecodedBodyBytes: 4}})
	if c.State != CompletionLimited || c.Stop != nil || c.Diagnostics[SeverityLimitExceeded] != 1 {
		t.Errorf("body budget: %+v", c)
	}
	// The diagnostic budget does not hide what was counted.
	c, _ = drained(strings.TrimSuffix(completionMessage, "--B--\r\n"), &ParseOptions{Limits: Limits{MaxDiagnosticCount: 1}})
	if c.State != CompletionIncomplete {
		t.Errorf("diagnostic budget: %+v", c)
	}

	// A sink that stops the parse.
	stop := errors.New("enough")
	c, _ = drained(strings.TrimSuffix(completionMessage, "--B--\r\n"), &ParseOptions{
		DiagnosticSink: DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error { return stop }),
	})
	if c.State != CompletionStopped || !errors.Is(c.Stop, stop) {
		t.Errorf("sink stop: %+v", c)
	}

	// The input fails.
	s, e, err := Parse(&failingSource{s: "Subject: s\r\n\r\nbody", err: errCut}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.Drain()
	if c := s.Completion(); c.State != CompletionFailed || c.InputErr != errCut || c.InputEnded {
		t.Errorf("input error: %+v", c)
	}

	// Cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	s, e, err = ParseContext(ctx, strings.NewReader(completionMessage), nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	e.Drain()
	if c := s.Completion(); c.State != CompletionCanceled || c.Stop == nil {
		t.Errorf("cancelled: %+v", c)
	}

	// No session, no claim.
	var none *ParseSession
	if c := none.Completion(); c.State != CompletionNotConsumed || c.Clean() {
		t.Errorf("nil session: %+v", c)
	}
}

func TestCompletion_embedded(t *testing.T) {
	const text = "Content-Type: message/rfc822\r\n\r\nSubject: inner\r\nContent-Type: multipart/mixed; boundary=I\r\n\r\n--I\r\n\r\nx\r\n"
	s, e, _ := Parse(strings.NewReader(text), &ParseOptions{WalkEmbeddedMessages: true})
	if err := e.Drain(); err != nil {
		t.Fatal(err)
	}
	c := s.Completion()
	if c.State != CompletionIncomplete || c.Entities != 3 || c.Bodies != c.BodiesRead || c.Multiparts != c.MultipartsRead {
		t.Errorf("embedded, unclosed inner multipart: %+v", c)
	}
}
