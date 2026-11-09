package mail_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// ROB-001-007 (issue #7): the no-progress rule of message.Parse reaches
// mail.Reader. A stalled input is the parse's stop, which NextPart reports as
// the identical value; a body whose CharsetReader reader stalls ends alone, and
// the next part is still returned. Every reader here is capped.

var errMailCallCap = errors.New("test reader: call cap exceeded")

// stallAt delivers data, returning zeros reads of (0, nil) once it reaches at.
type stallAt struct {
	data       string
	at, zeros  int
	pos, calls int
}

func (s *stallAt) Read(p []byte) (int, error) {
	s.calls++
	if s.calls > 4*len(s.data)+1000 {
		return 0, errMailCallCap
	}
	if s.pos == s.at && s.zeros > 0 {
		s.zeros--
		return 0, nil
	}
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	end := len(s.data)
	if s.pos < s.at && s.at < end {
		end = s.at
	}
	n := copy(p, s.data[s.pos:end])
	s.pos += n
	return n, nil
}

const progressTree = "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
	"--B\r\nContent-Type: text/plain; charset=x-stall\r\n\r\nfirst part\r\n" +
	"--B\r\nContent-Type: text/plain\r\n\r\nsecond part\r\n--B--\r\n"

func TestReader_sourceNoProgress(t *testing.T) {
	tree := strings.Replace(progressTree, "; charset=x-stall", "", 1)
	src := &stallAt{data: tree, at: strings.Index(tree, "second part") + 3, zeros: 1000}
	_, e, err := message.Parse(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := mail.NewReader(e)
	p, err := r.NextPart()
	if err != nil {
		t.Fatalf("first NextPart() = %v", err)
	}
	if b, err := io.ReadAll(p.Body); string(b) != "first part" || err != nil {
		t.Fatalf("first body %q, %v", b, err)
	}
	p, err = r.NextPart()
	if err != nil {
		t.Fatalf("second NextPart() = %v", err)
	}
	b, bodyErr := io.ReadAll(p.Body)
	stop := e.ParseStop()
	if string(b) != "sec" || stop == nil || !errors.Is(bodyErr, stop) || !errors.Is(stop, io.ErrNoProgress) || !message.IsFatal(stop) {
		t.Fatalf("second body %q, %v, stop %v; want the bytes before the stall and the stop", b, bodyErr, stop)
	}
	for range 3 {
		if p, err := r.NextPart(); p != nil || err != stop {
			t.Errorf("NextPart() after the stall = (%v, %v), want the identical stop", p, err)
		}
	}
}

func TestReader_decoderNoProgressKeepsTheNextPart(t *testing.T) {
	old := message.CharsetReader
	t.Cleanup(func() { message.CharsetReader = old })
	message.CharsetReader = func(string, io.Reader) (io.Reader, error) {
		calls := 0
		return readFunc(func([]byte) (int, error) {
			calls++
			if calls > 1000 {
				return 0, errMailCallCap
			}
			return 0, nil
		}), nil
	}
	_, e, err := message.Parse(strings.NewReader(progressTree), nil)
	if err != nil {
		t.Fatal(err)
	}
	r := mail.NewReader(e)
	p, err := r.NextPart()
	if err != nil {
		t.Fatalf("first NextPart() = %v", err)
	}
	if _, err := io.ReadAll(p.Body); !message.IsFatal(err) || !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("first body ended with %v, want its local stop", err)
	}
	p, err = r.NextPart()
	if err != nil {
		t.Fatalf("second NextPart() = %v, want the next part", err)
	}
	if b, err := io.ReadAll(p.Body); string(b) != "second part" || err != nil {
		t.Errorf("second body %q, %v", b, err)
	}
	if _, err := r.NextPart(); err != io.EOF || e.ParseStop() != nil {
		t.Errorf("NextPart() = %v, stop %v; want io.EOF and no stop", err, e.ParseStop())
	}
}

type readFunc func([]byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }
