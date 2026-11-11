package message

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit/internal/scaletest"
)

// pfNestWalk parses msg and returns the media type and decoded body of every
// entity in document order, and the session.
func pfNestWalk(t *testing.T, msg []byte) (types []string, bodies []string, s *ParseSession) {
	t.Helper()
	s, e, err := Parse(bytes.NewReader(msg), nil)
	if e == nil {
		t.Fatalf("Parse: %v", err)
	}
	werr := e.Walk(func(_ []int, p *Entity, _ error) error {
		mt, _, _ := p.EffectiveContentType()
		types = append(types, mt)
		if p.MultipartReader() == nil {
			b, _ := io.ReadAll(p.Body)
			bodies = append(bodies, string(b))
		}
		return nil
	})
	if werr != nil {
		t.Fatalf("Walk: %v", werr)
	}
	return types, bodies, s
}

func pfCount(s *ParseSession, code Code) (n int, last Diagnostic) {
	for _, d := range s.Diagnostics() {
		if d.Code == code {
			n++
			last = d
		}
	}
	return n, last
}

// TestNestedInferenceBound checks that boundary inference is tried for a
// multipart that up to pfInferNestMax inferred multiparts enclose, and that a
// deeper one is returned as one part holding its whole body.
func TestNestedInferenceBound(t *testing.T) {
	const text = "the innermost part of nested multiparts that declare no boundary.\r\n"

	// As deep as inference goes: every level is framed.
	msg := pfInferNested(pfInferNestMax+1, 4096)
	types, bodies, s := pfNestWalk(t, msg)
	if len(types) != pfInferNestMax+2 || types[len(types)-1] != "text/plain" {
		t.Fatalf("entities %v, want %d multiparts and the text part", types, pfInferNestMax+1)
	}
	if len(bodies) != 1 || !strings.HasPrefix(bodies[0], text) {
		t.Errorf("leaf bodies %.80q, want the innermost text", bodies)
	}
	if n, _ := pfCount(s, CodeMultipartBoundaryInferred); n != pfInferNestMax+1 {
		t.Errorf("%d boundary_inferred diagnostics, want %d", n, pfInferNestMax+1)
	}
	if n, _ := pfCount(s, CodeMultipartStartNotFound); n != 0 {
		t.Errorf("%d start_not_found diagnostics, want none", n)
	}

	// One level deeper: the last multipart is not searched, and its body is
	// one part that holds everything it held.
	msg = pfInferNested(pfInferNestMax+2, 4096)
	types, bodies, s = pfNestWalk(t, msg)
	if len(types) != pfInferNestMax+3 {
		t.Fatalf("entities %v, want %d multiparts and one unframed part", types, pfInferNestMax+2)
	}
	inner := fmt.Sprintf("--nest-%03d\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s", pfInferNestMax+1, text)
	if len(bodies) != 1 || !strings.HasPrefix(bodies[0], inner) || !strings.HasSuffix(string(msg), bodies[0]) {
		t.Errorf("unframed body begins %.120q, want the whole body of the multipart, which begins %.60q", bodies, inner)
	}
	if n, _ := pfCount(s, CodeMultipartBoundaryInferred); n != pfInferNestMax+1 {
		t.Errorf("%d boundary_inferred diagnostics, want %d", n, pfInferNestMax+1)
	}
	n, d := pfCount(s, CodeMultipartStartNotFound)
	if n != 1 || d.Severity != SeverityIncomplete || len(d.Path) != pfInferNestMax+1 {
		t.Errorf("start_not_found: %d diagnostics, the last %+v; want one, incomplete, at the multipart that was not searched", n, d)
	}
	if c := s.Completion(); c.State != CompletionIncomplete {
		t.Errorf("completion %v, want incomplete", c.State)
	}

	// Multiparts framed as they declare do not count: inference still works
	// under any number of them.
	var b bytes.Buffer
	b.WriteString(pfHead)
	for d := 0; d < 3*pfInferNestMax; d++ {
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"lvl-%03d\"\r\n\r\n--lvl-%03d\r\n", d, d)
	}
	b.WriteString("Content-Type: multipart/mixed\r\n\r\n--lost\r\nContent-Type: text/plain\r\n\r\nfound\r\n--lost--\r\n")
	for d := 3*pfInferNestMax - 1; d >= 0; d-- {
		fmt.Fprintf(&b, "\r\n--lvl-%03d--\r\n", d)
	}
	types, bodies, s = pfNestWalk(t, b.Bytes())
	if len(types) != 3*pfInferNestMax+2 || len(bodies) != 1 || bodies[0] != "found" {
		t.Errorf("under declared multiparts: %d entities, bodies %q; want the inferred part", len(types), bodies)
	}
	if n, _ := pfCount(s, CodeMultipartBoundaryInferred); n != 1 {
		t.Errorf("%d boundary_inferred diagnostics under declared multiparts, want 1", n)
	}
}

// TestNestedInferenceCost holds what a parse allocates for multiparts that
// declare no boundary, nested to the default MaxMIMEDepth, to a small multiple
// of the message: it was more than 450 times its size.
func TestNestedInferenceCost(t *testing.T) {
	msg := pfInferNested(100, 256<<10)
	sh := &pfShape{name: "nested inference"}
	pfRun(t, sh, bytes.NewReader(msg))
	cost := scaletest.Measure(1, func() { pfRun(t, sh, bytes.NewReader(msg)) })
	if limit := uint64(48 * len(msg)); cost.Bytes > limit {
		t.Errorf("allocated %d bytes for a message of %d: %.0f times its size, want at most 48", cost.Bytes, len(msg), float64(cost.Bytes)/float64(len(msg)))
	}
}

// TestSmallPartDecoderCost holds what the transfer decoder of a small part
// allocates: a decoder used to take 8 KiB of buffers for any body.
func TestSmallPartDecoderCost(t *testing.T) {
	for _, enc := range []string{"base64", "quoted-printable", "x-uuencode"} {
		t.Run(enc, func(t *testing.T) {
			build := func(encoding string) []byte {
				return pfWide(2000, true, func(b *bytes.Buffer, i int) {
					fmt.Fprintf(b, "Content-Type: application/octet-stream\r\nContent-Transfer-Encoding: %s\r\n\r\nQUJDRA==", encoding)
				})
			}
			sh := &pfShape{name: enc}
			run := func(msg []byte) uint64 {
				pfRun(t, sh, bytes.NewReader(msg))
				return scaletest.Measure(1, func() { pfRun(t, sh, bytes.NewReader(msg)) }).Bytes
			}
			// What the decoder costs is what the same parts cost beyond
			// parts that need none.
			with, without := run(build(enc)), run(build("binary"))
			if per := (int64(with) - int64(without)) / 2000; per > 2048 {
				t.Errorf("a %s part of 8 bytes allocates %d bytes more than a binary one, want at most 2048", enc, per)
			}
		})
	}
}
