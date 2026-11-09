package mail_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// ROB-010-006 (issue #7): an ordinary failure of the message's source reaches
// mail.Reader as itself, and a part refused by its own body budget while a
// CharsetReader read it eagerly is returned with no part, the next call
// continuing with the part after it.

var errMailIO = errors.New("test source: input/output error")

// failAfter delivers data up to cut and then fails with err on every read.
type failAfter struct {
	data       string
	cut, calls int
	pos        int
	err        error
}

func (s *failAfter) Read(p []byte) (int, error) {
	s.calls++
	if s.calls > 4*len(s.data)+64 {
		return 0, errMailCallCap
	}
	if s.pos >= s.cut {
		return 0, s.err
	}
	n := copy(p, s.data[s.pos:s.cut])
	s.pos += n
	return n, nil
}

const ioTree = "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
	"--B\r\nContent-Type: text/plain; charset=x-eager\r\n\r\nfirst part\r\n" +
	"--B\r\nContent-Type: text/plain\r\n\r\nsecond part\r\n--B--\r\n"

func withEagerHook(t *testing.T) {
	t.Helper()
	old := message.CharsetReader
	t.Cleanup(func() { message.CharsetReader = old })
	message.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) {
		b, err := io.ReadAll(in)
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(b), nil
	}
}

func TestReader_sourceErrorInPart(t *testing.T) {
	tree := strings.Replace(ioTree, "; charset=x-eager", "", 1)
	src := &failAfter{data: tree, cut: strings.Index(tree, "second part") + 3, err: errMailIO}
	_, e, err := message.Parse(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := mail.NewReader(e)
	if p, err := r.NextPart(); err != nil {
		t.Fatalf("first NextPart() = %v", err)
	} else if b, err := io.ReadAll(p.Body); string(b) != "first part" || err != nil {
		t.Fatalf("first body (%q, %v)", b, err)
	}
	p, err := r.NextPart()
	if err != nil {
		t.Fatalf("second NextPart() = %v", err)
	}
	if b, err := io.ReadAll(p.Body); string(b) != "sec" || !errors.Is(err, errMailIO) {
		t.Errorf("second body (%q, %v), want the bytes before the failure and the failure", b, err)
	}
	if p, err := r.NextPart(); p != nil || !errors.Is(err, errMailIO) || e.ParseStop() != nil {
		t.Errorf("NextPart() after the failure = (%v, %v), stop %v", p, err, e.ParseStop())
	}
}

func TestReader_eagerHookLocalRefusal(t *testing.T) {
	withEagerHook(t)
	limits := message.Limits{
		MaxEncodedBodyBytes: 3,
	}
	// The budget refuses the first part's ten bytes and admits the whole of the
	// second's two.
	tree := strings.Replace(ioTree, "second part", "ok", 1)
	_, e, err := message.Parse(strings.NewReader(tree), &message.ParseOptions{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	r := mail.NewReader(e)
	p, err := r.NextPart()
	if p != nil || !message.IsFatal(err) || message.IsUnknownCharset(err) {
		t.Fatalf("NextPart() = (%v, %v), want no part and the part's own refusal", p, err)
	}
	p, err = r.NextPart()
	if err != nil {
		t.Fatalf("NextPart() called again = %v, want the next part", err)
	}
	if b, err := io.ReadAll(p.Body); string(b) != "ok" || err != nil {
		t.Errorf("second body (%q, %v), want it whole", b, err)
	}
}
