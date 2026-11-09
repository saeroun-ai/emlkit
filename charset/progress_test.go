package charset_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	_ "github.com/saeroun-ai/emlkit/charset"
)

// ROB-001-007 (issue #7): golang.org/x/text's transform.Reader, which this
// package installs as the charset decoder, asks its input again for ever while
// the input returns (0, nil). Under message.Parse the input's no-progress rule
// sits below it, so a stalled input ends the body after exactly 100 reads with
// the parse's stop. The source is capped so that a regression fails instead of
// spinning.

var errCharsetCallCap = errors.New("test reader: call cap exceeded")

type stallingSource struct {
	data              string
	at, zeros         int
	pos, calls, empty int
}

func (s *stallingSource) Read(p []byte) (int, error) {
	s.calls++
	if s.calls > 4*len(s.data)+1000 {
		return 0, errCharsetCallCap
	}
	if s.pos == s.at && s.zeros > 0 {
		s.zeros--
		s.empty++
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

func TestParse_charsetDecoderOverAStalledInput(t *testing.T) {
	text := "Content-Type: text/plain; charset=windows-1252\r\n\r\ncaf\xe9 body"
	for _, zeros := range []int{99, 100, 1000} {
		src := &stallingSource{data: text, at: strings.Index(text, " body"), zeros: zeros}
		_, e, err := message.Parse(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, bodyErr := io.ReadAll(e.Body)
		stop := e.ParseStop()
		if zeros < 100 {
			if string(b) != "café body" || bodyErr != nil || stop != nil {
				t.Errorf("%d zeros: body %q, %v, stop %v; want the whole body", zeros, b, bodyErr, stop)
			}
			continue
		}
		if string(b) != "café" || stop == nil || bodyErr != stop || !errors.Is(stop, io.ErrNoProgress) || src.empty != 100 {
			t.Errorf("%d zeros: body %q, %v after %d empty reads, stop %v; want the prefix and the stop after 100",
				zeros, b, bodyErr, src.empty, stop)
		}
	}
}
