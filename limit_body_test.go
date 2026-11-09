package message

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Tests for Limits.MaxEncodedBodyBytes, MaxDecodedBodyBytes and
// MaxTotalDecodedBytes (ROB-001-005). See limit_body.go.

// errCut is what a reference decode is cut with: an error that is not io.EOF,
// as the encoded limiter's refusal is not.
var errCut = errors.New("cut")

// bufferedCharset converts its input in chunks of 64 bytes, holding converted
// output it has not handed out yet, as a real charset decoder reading ahead
// does. It is greedy: it keeps reading while it holds less than 64 bytes of
// output, so the read that proves an encoded overflow happens while output made
// of admitted bytes is still waiting to be delivered. conv turns one input byte
// into its output bytes.
type bufferedCharset struct {
	r    io.Reader
	conv func(out []byte, b byte) []byte
	out  []byte
	err  error
	buf  [64]byte
}

func (c *bufferedCharset) Read(p []byte) (int, error) {
	for len(c.out) < 64 && c.err == nil {
		n, err := c.r.Read(c.buf[:])
		for _, b := range c.buf[:n] {
			c.out = c.conv(c.out, b)
		}
		c.err = err
	}
	if len(c.out) > 0 {
		n := copy(p, c.out)
		c.out = c.out[n:]
		return n, nil
	}
	return 0, c.err
}

func latin1(out []byte, b byte) []byte  { return utf8.AppendRune(out, rune(b)) }
func expand4(out []byte, b byte) []byte { return append(out, b, b, b, b) }

// maskingCharset passes its input through, but ends with mask instead of any
// error that is not io.EOF, as a careless CharsetReader might.
type maskingCharset struct {
	r    io.Reader
	mask error
}

func (m *maskingCharset) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	if err != nil && err != io.EOF {
		err = m.mask
	}
	return n, err
}

// writerToCharset passes its input through and also offers a WriteTo that
// would write without end, which no copy may be allowed to use.
type writerToCharset struct{ r io.Reader }

func (w *writerToCharset) Read(p []byte) (int, error) { return w.r.Read(p) }
func (w *writerToCharset) WriteTo(dst io.Writer) (int64, error) {
	var n int64
	for n < 1<<20 {
		m, err := dst.Write(bytes.Repeat([]byte("w"), 4096))
		n += int64(m)
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// inventingCharset passes its input through and, once its input fails, makes
// up 'z' bytes for ever: a CharsetReader whose output nothing but the decoded
// budgets can bound.
type inventingCharset struct {
	r      io.Reader
	failed bool
}

func (c *inventingCharset) Read(p []byte) (int, error) {
	if !c.failed {
		n, err := c.r.Read(p)
		if err != nil && err != io.EOF {
			c.failed = true
		}
		if n > 0 || !c.failed {
			return n, err
		}
	}
	for i := range p {
		p[i] = 'z'
	}
	return len(p), nil
}

// withTestCharsets installs the test charsets for the duration of t.
func withTestCharsets(t *testing.T) {
	t.Helper()
	old := CharsetReader
	t.Cleanup(func() { CharsetReader = old })
	CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		switch charset {
		case "x-test-latin1":
			return &bufferedCharset{r: input, conv: latin1}, nil
		case "x-test-expand":
			return &bufferedCharset{r: input, conv: expand4}, nil
		case "x-test-mask-eof":
			return &maskingCharset{r: input, mask: io.EOF}, nil
		case "x-test-mask-other":
			return &maskingCharset{r: input, mask: errors.New("hook's own error")}, nil
		case "x-test-writerto":
			return &writerToCharset{r: input}, nil
		case "x-test-invent":
			return &inventingCharset{r: input}, nil
		case "x-test-eager":
			// Reads all of its input while the entity is being built, and keeps
			// what it got whatever ended it.
			b, _ := io.ReadAll(input)
			return bytes.NewReader(b), nil
		}
		return nil, fmt.Errorf("unknown test charset %q", charset)
	}
}

type bodyFixture struct {
	name, header, wire, decoded string
}

func bodyFixtures() []bodyFixture {
	b64 := base64.StdEncoding.EncodeToString([]byte("abcdefghijk"))
	return []bodyFixture{
		{"7bit", "Content-Type: text/plain\r\n", "abcdefghijk", "abcdefghijk"},
		{"base64", "Content-Transfer-Encoding: base64\r\n", b64, "abcdefghijk"},
		{"base64 lines", "Content-Transfer-Encoding: base64\r\n", b64[:4] + "\r\n" + b64[4:8] + "\r\n" + b64[8:], "abcdefghijk"},
		{"quoted-printable", "Content-Transfer-Encoding: quoted-printable\r\n", "ab=63def=67hijk", "abcdefghijk"},
		{"latin1", "Content-Type: text/plain; charset=x-test-latin1\r\n", "caf\xe9-\xa3-fin", "café-£-fin"},
		{"expanding", "Content-Type: text/plain; charset=x-test-expand\r\n", "abc", "aaaabbbbcccc"},
		{"base64 latin1", "Content-Type: text/plain; charset=x-test-latin1\r\nContent-Transfer-Encoding: base64\r\n",
			base64.StdEncoding.EncodeToString([]byte("\xe9t\xe9 \xa3")), "été £"},
	}
}

// referenceDecode is what the decoders make of wire when it is cut with an
// error rather than ended: what a body whose encoded budget admitted exactly
// wire delivers before its failure.
func referenceDecode(t *testing.T, f bodyFixture, wire string) string {
	t.Helper()
	h, err := textprotoHeader(f.header)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := New(h, io.MultiReader(strings.NewReader(wire), iotestErrReader{errCut}))
	b, _ := io.ReadAll(e.Body)
	return string(b)
}

type iotestErrReader struct{ err error }

func (r iotestErrReader) Read([]byte) (int, error) { return 0, r.err }

func textprotoHeader(fields string) (Header, error) {
	e, err := Read(strings.NewReader(fields + "\r\n"))
	if err != nil {
		return Header{}, err
	}
	return e.Header, nil
}

// readBy reads r with a buffer of k bytes until it returns an error.
func readBy(r io.Reader, k int) ([]byte, error) {
	var out []byte
	buf := make([]byte, k)
	for range 1 << 20 {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return out, err
		}
	}
	return out, errors.New("readBy: no end")
}

// bodySources are the deliveries of the input the body budgets must not depend
// on.
func bodySources(text string) map[string]func() io.Reader {
	out := map[string]func() io.Reader{
		"whole":       func() io.Reader { return &inputSource{s: text} },
		"whole, eof":  func() io.Reader { return &inputSource{s: text, withData: true} },
		"zero reads":  func() io.Reader { return &inputSource{s: text, chunk: 2, zeros: true} },
		"random 5":    func() io.Reader { return newRandomChunkReader(text, 5, 3) },
		"chunks 1eof": func() io.Reader { return &inputSource{s: text, chunk: 1, withData: true} },
	}
	for _, n := range []int{1, 2, 3, 7} {
		out[fmt.Sprintf("chunks %d", n)] = func() io.Reader { return &inputSource{s: text, chunk: n} }
	}
	return out
}

// onlyBody returns Limits enforcing one body budget kind at limit, with the
// other two body budgets explicitly disabled.
func onlyBody(kind string, limit int64) Limits {
	l := Limits{MaxEncodedBodyBytes: LimitDisabled, MaxDecodedBodyBytes: LimitDisabled, MaxTotalDecodedBytes: LimitDisabled}
	switch kind {
	case "encoded":
		l.MaxEncodedBodyBytes = limit
	case "decoded":
		l.MaxDecodedBodyBytes = limit
	case "total":
		l.MaxTotalDecodedBytes = limit
	}
	return l
}

var bodyCodes = map[string]Code{
	"encoded": CodeLimitEncodedBodyBytes,
	"decoded": CodeLimitDecodedBodyBytes,
	"total":   CodeLimitTotalDecodedBytes,
}

// TestParse_bodyBudgetsMatrix pins each budget alone on a single body, for
// every transfer encoding and charset stage, at every boundary, however the
// input is chunked and however small the caller's buffer is.
//
// A decoded budget N delivers exactly the first N output bytes, which may end
// inside a UTF-8 sequence. An encoded budget N delivers what the decoders make
// of the first N wire bytes — all of it, including output they had buffered
// when the next wire byte was refused — and no more.
func TestParse_bodyBudgetsMatrix(t *testing.T) {
	withTestCharsets(t)
	for _, f := range bodyFixtures() {
		text := f.header + "\r\n" + f.wire
		bodyStart := int64(len(f.header) + 2)
		for _, kind := range []string{"encoded", "decoded", "total"} {
			size := int64(len(f.decoded))
			if kind == "encoded" {
				size = int64(len(f.wire))
			}
			for _, limit := range []int64{1, 2, 3, 4, size - 1, size, size + 1, LimitDisabled, math.MaxInt64} {
				if limit < 1 && limit != LimitDisabled {
					continue
				}
				over := limit != LimitDisabled && limit < size
				want := f.decoded
				switch {
				case over && kind == "encoded":
					want = referenceDecode(t, f, f.wire[:limit])
				case over:
					want = f.decoded[:limit]
				}
				for src, open := range bodySources(text) {
					for _, k := range []int{1, 2, 3, 7, 4096} {
						name := fmt.Sprintf("%s/%s=%d/%s/dst %d", f.name, kind, limit, src, k)
						s, e, err := Parse(open(), &ParseOptions{Limits: onlyBody(kind, limit)})
						if err != nil || e == nil {
							t.Fatalf("%s: Parse() = %v", name, err)
						}
						got, err := readBy(e.Body, k)
						if string(got) != want {
							t.Errorf("%s: body = %q, want %q", name, got, want)
						}
						if !over {
							if err != io.EOF || s.Len() != 0 {
								t.Errorf("%s: end = %v with %v, want io.EOF and nothing recorded", name, err, s.Diagnostics())
							}
							continue
						}
						checkBodyFailure(t, name, s, e, err, bodyCodes[kind], kind == "total")
						d := s.Diagnostics()[0]
						wantLoc := LocationAt(bodyStart)
						if kind == "encoded" {
							wantLoc = LocationAt(bodyStart + limit)
						}
						if d.Location != wantLoc || d.Path != nil {
							t.Errorf("%s: diagnostic at %v path %v, want %v and the root", name, d.Location, d.Path, wantLoc)
						}
					}
				}
			}
		}
	}
}

// checkBodyFailure pins the failure of a body read: a FatalError carrying a
// diagnostic of code, exactly one recorded, identical on every later read, and
// scoped to the body unless global.
func checkBodyFailure(t *testing.T, name string, s *ParseSession, e *Entity, err error, code Code, global bool) {
	t.Helper()
	var fatal FatalError
	if !errors.As(err, &fatal) {
		t.Errorf("%s: end = %v, want a FatalError", name, err)
		return
	}
	if d, ok := fatal.Diagnostic(); !ok || d.Code != code {
		t.Errorf("%s: FatalError carries %v, want %s", name, d, code)
	}
	if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{code}) {
		t.Errorf("%s: diagnostics = %v, want one %s", name, codes, code)
	}
	if n, again := e.Body.Read(make([]byte, 8)); n != 0 || again != err {
		t.Errorf("%s: read again = (%d, %v), want (0, the identical error)", name, n, again)
	}
	if stop := e.ParseStop(); (stop != nil) != global || (global && stop != err) {
		t.Errorf("%s: ParseStop() = %v, want global=%v", name, stop, global)
	}
}

// TestParse_bodyBudgetsHandWritten states a few of the matrix's expectations
// outright, so that they are not only what the decoders happen to do.
func TestParse_bodyBudgetsHandWritten(t *testing.T) {
	withTestCharsets(t)
	cases := []struct {
		name, header, wire string
		limits             Limits
		want               string
	}{
		// A base64 quantum the encoded budget cuts is dropped.
		{"base64 cut quantum", "Content-Transfer-Encoding: base64\r\n", "YWJjZGVm", onlyBody("encoded", 6), "abc"},
		{"base64 whole quanta", "Content-Transfer-Encoding: base64\r\n", "YWJjZGVm", onlyBody("encoded", 4), "abc"},
		// A quoted-printable escape the encoded budget cuts is kept literally by
		// the tolerant decoder, as a malformed escape would be.
		{"qp cut escape", "Content-Transfer-Encoding: quoted-printable\r\n", "ab=63de", onlyBody("encoded", 3), "ab="},
		{"qp cut escape 2", "Content-Transfer-Encoding: quoted-printable\r\n", "ab=63de", onlyBody("encoded", 4), "ab=6"},
		{"qp whole escape", "Content-Transfer-Encoding: quoted-printable\r\n", "ab=63de", onlyBody("encoded", 5), "abc"},
		// Decoded bytes, not characters: the cut falls inside "é".
		{"utf8 cut", "Content-Type: text/plain; charset=x-test-latin1\r\n", "caf\xe9", onlyBody("decoded", 4), "caf\xc3"},
		{"utf8 cut total", "Content-Type: text/plain; charset=x-test-latin1\r\n", "caf\xe9", onlyBody("total", 4), "caf\xc3"},
		// Every byte an expanding decoder made of the admitted wire bytes is
		// delivered, though it read further ahead than that.
		{"expansion of admitted", "Content-Type: text/plain; charset=x-test-expand\r\n", "abcdef", onlyBody("encoded", 2), "aaaabbbb"},
	}
	for _, tc := range cases {
		for _, k := range []int{1, 2, 3, 4096} {
			_, e, err := Parse(strings.NewReader(tc.header+"\r\n"+tc.wire), &ParseOptions{Limits: tc.limits})
			if err != nil {
				t.Fatal(err)
			}
			got, err := readBy(e.Body, k)
			if string(got) != tc.want || !IsFatal(err) {
				t.Errorf("%s/dst %d: (%q, %v), want (%q, a FatalError)", tc.name, k, got, err, tc.want)
			}
		}
	}
}

// TestParse_bodyBudgetsCustomCharsets pins what a CharsetReader a caller
// installed can and cannot do to the budgets. Its masking of the failure is
// undone, its WriteTo is never used, and an eager read at construction is
// bounded. What it invents once its input failed is bounded only by the decoded
// budgets.
func TestParse_bodyBudgetsCustomCharsets(t *testing.T) {
	withTestCharsets(t)
	body := func(cs string) string {
		return "Content-Type: text/plain; charset=" + cs + "\r\n\r\nabcdefghij"
	}

	for _, cs := range []string{"x-test-mask-eof", "x-test-mask-other"} {
		for _, k := range []int{1, 3, 4096} {
			s, e, err := Parse(strings.NewReader(body(cs)), &ParseOptions{Limits: onlyBody("encoded", 3)})
			if err != nil {
				t.Fatal(err)
			}
			got, err := readBy(e.Body, k)
			if string(got) != "abc" {
				t.Errorf("%s/dst %d: body = %q, want %q", cs, k, got, "abc")
			}
			checkBodyFailure(t, cs, s, e, err, CodeLimitEncodedBodyBytes, false)
		}
	}

	t.Run("WriterTo", func(t *testing.T) {
		s, e, err := Parse(strings.NewReader(body("x-test-writerto")), &ParseOptions{Limits: onlyBody("decoded", 4)})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		n, err := io.Copy(&out, e.Body)
		if n != 4 || out.String() != "abcd" {
			t.Errorf("io.Copy = %d bytes %q, want the 4 admitted: the hook's WriteTo must not be used", n, out.String())
		}
		checkBodyFailure(t, "WriterTo", s, e, err, CodeLimitDecodedBodyBytes, false)
	})

	t.Run("eager", func(t *testing.T) {
		s, e, err := Parse(strings.NewReader(body("x-test-eager")), &ParseOptions{Limits: onlyBody("encoded", 3)})
		if err != nil {
			t.Fatal(err)
		}
		// The overflow was proven while the hook read at construction.
		if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeLimitEncodedBodyBytes}) {
			t.Errorf("after Parse: %v, want the encoded overflow already recorded", codes)
		}
		got, err := readBy(e.Body, 2)
		if string(got) != "abc" {
			t.Errorf("body = %q, want %q", got, "abc")
		}
		checkBodyFailure(t, "eager", s, e, err, CodeLimitEncodedBodyBytes, false)
	})

	t.Run("inventing", func(t *testing.T) {
		l := onlyBody("encoded", 3)
		l.MaxDecodedBodyBytes = 8
		s, e, err := Parse(strings.NewReader(body("x-test-invent")), &ParseOptions{Limits: l})
		if err != nil {
			t.Fatal(err)
		}
		got, err := readBy(e.Body, 3)
		if string(got) != "abczzzzz" {
			t.Errorf("body = %q, want the admitted bytes and what the hook invented up to the decoded budget", got)
		}
		// The body has one failure, the first proven: the encoded one.
		checkBodyFailure(t, "inventing", s, e, err, CodeLimitEncodedBodyBytes, false)
	})
}

// bodyTree is a message whose leaves sit first, nested, in the middle and last,
// three of them over a budget of 4.
var bodyTree = "Content-Type: multipart/mixed; boundary=O\r\n\r\n" +
	"preamble\r\n" +
	"--O\r\nContent-Type: text/plain\r\n\r\nAAAAAAAAAA\r\n" +
	"--O\r\nContent-Type: multipart/alternative; boundary=I\r\n\r\n" +
	"--I\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte("BBBBBBBBBB")) + "\r\n" +
	"--I\r\nContent-Type: text/plain\r\n\r\nccc\r\n" +
	"--I--\r\n" +
	"--O\r\nContent-Type: text/plain\r\n\r\nddd\r\n" +
	"--O\r\nContent-Type: text/plain\r\n\r\nEEEEEEEEEE\r\n" +
	"--O--\r\nepilogue\r\n"

type leafResult struct {
	body string
	err  error
}

// walkBodies walks e, reading every leaf with a buffer of k bytes, and keeps
// what each leaf delivered and how it ended. skip names leaves left unread.
func walkBodies(e *Entity, k int, skip map[string]bool) (map[string]leafResult, error) {
	out := map[string]leafResult{}
	err := e.Walk(func(path []int, part *Entity, err error) error {
		if part.MultipartReader() != nil {
			return nil
		}
		key := fmt.Sprint(path)
		if skip[key] {
			return nil
		}
		b, rerr := readBy(part.Body, k)
		out[key] = leafResult{string(b), rerr}
		return nil
	})
	return out, err
}

// TestParse_bodyBudgetsLocalFailureContinues pins that a per-body overflow ends
// that body alone: its parent drains the unread raw tail and the walk reaches
// every later sibling and every shallower part, at any depth.
func TestParse_bodyBudgetsLocalFailureContinues(t *testing.T) {
	for _, kind := range []string{"encoded", "decoded"} {
		wantB := "BBBB"
		if kind == "encoded" {
			wantB = "BBB" // what four base64 wire bytes decode to
		}
		want := map[string]string{"[0]": "AAAA", "[1 0]": wantB, "[1 1]": "ccc", "[2]": "ddd", "[3]": "EEEE"}
		fails := map[string]bool{"[0]": true, "[1 0]": true, "[3]": true}
		for src, open := range bodySources(bodyTree) {
			for _, k := range []int{1, 3, 4096} {
				name := fmt.Sprintf("%s/%s/dst %d", kind, src, k)
				s, e, err := Parse(open(), &ParseOptions{Limits: onlyBody(kind, 4)})
				if err != nil {
					t.Fatal(err)
				}
				got, err := walkBodies(e, k, nil)
				if err != nil || e.ParseStop() != nil {
					t.Fatalf("%s: Walk() = %v, ParseStop() = %v; want the walk to go on", name, err, e.ParseStop())
				}
				for key, w := range want {
					r := got[key]
					if r.body != w || IsFatal(r.err) != fails[key] || (!fails[key] && r.err != io.EOF) {
						t.Errorf("%s: leaf %s = (%q, %v), want %q failing=%v", name, key, r.body, r.err, w, fails[key])
					}
				}
				var paths [][]int
				for _, d := range s.Diagnostics() {
					if d.Code != bodyCodes[kind] {
						t.Errorf("%s: unexpected %v", name, d)
					}
					paths = append(paths, d.Path)
				}
				if !reflect.DeepEqual(paths, [][]int{{0}, {1, 0}, {3}}) {
					t.Errorf("%s: diagnostic paths = %v", name, paths)
				}
			}
		}
	}

	t.Run("unread and partly read leaves", func(t *testing.T) {
		// Skipping is not decoding: a leaf left unread, or read in part, is
		// drained raw and records nothing.
		s, e, err := Parse(strings.NewReader(bodyTree), &ParseOptions{Limits: onlyBody("decoded", 4)})
		if err != nil {
			t.Fatal(err)
		}
		mr := e.MultipartReader()
		p, _ := mr.NextPart()
		if n, _ := p.Body.Read(make([]byte, 2)); n != 2 {
			t.Fatalf("partial read = %d", n)
		}
		inner, _ := mr.NextPart() // drains the rest of [0]
		imr := inner.MultipartReader()
		imr.NextPart() // [1 0], never read
		c, err := imr.NextPart()
		if err != nil {
			t.Fatalf("NextPart() = %v", err)
		}
		if b, err := io.ReadAll(c.Body); string(b) != "ccc" || err != nil {
			t.Errorf("[1 1] = (%q, %v)", b, err)
		}
		if s.Len() != 0 {
			t.Errorf("diagnostics = %v, want none for leaves not read past the budget", s.Diagnostics())
		}
	})
}

// TestParse_bodyBudgetsContainerRawView pins that a framed multipart container's
// own body is a raw view outside the leaf budgets: read directly or copied
// verbatim, it is bounded by MaxInputBytes alone, and nothing it yields counts
// toward the total.
func TestParse_bodyBudgetsContainerRawView(t *testing.T) {
	tiny := Limits{MaxEncodedBodyBytes: 1, MaxDecodedBodyBytes: 1, MaxTotalDecodedBytes: 1}
	header, raw, _ := strings.Cut(bodyTree, "\r\n\r\n")

	s, e, err := Parse(strings.NewReader(bodyTree), &ParseOptions{Limits: tiny})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(e.Body); string(b) != raw || err != nil || s.Len() != 0 {
		t.Errorf("direct read = (%d bytes, %v), %v; want the raw body whole", len(b), err, s.Diagnostics())
	}

	s, e, err = Parse(strings.NewReader(bodyTree), &ParseOptions{Limits: tiny})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := e.WriteTo(&out); err != nil || !strings.HasSuffix(out.String(), "\r\n\r\n"+raw) || s.Len() != 0 {
		t.Errorf("WriteTo = %v, %v; want the body verbatim after the header %q", err, s.Diagnostics(), header)
	}

	// The input budget still applies to the raw view.
	l := tiny
	l.MaxInputBytes = int64(len(bodyTree) - 1)
	s, e, err = Parse(strings.NewReader(bodyTree), &ParseOptions{Limits: l})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(e.Body); err == nil || err != e.ParseStop() {
		t.Errorf("direct read past the input budget = %v, want the input stop", err)
	}
	if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeLimitInputBytes}) {
		t.Errorf("diagnostics = %v", codes)
	}
}

// TestParse_bodyBudgetsWriteTo pins the verbatim copy of a leaf: it is held to
// the encoded budget and the input budget, and not to the decoded ones.
func TestParse_bodyBudgetsWriteTo(t *testing.T) {
	const text = "Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\nYWJjZGVmZ2hpams=\r\n"
	l := onlyBody("decoded", 1)
	l.MaxTotalDecodedBytes = 1
	s, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: l})
	if err != nil {
		t.Fatal(err)
	}
	_, raw, _ := strings.Cut(text, "\r\n\r\n")
	var out bytes.Buffer
	if err := e.WriteTo(&out); err != nil || !strings.HasSuffix(out.String(), "\r\n\r\n"+raw) || s.Len() != 0 {
		t.Errorf("WriteTo under decoded budgets = (%q, %v), %v; want it verbatim", out.String(), err, s.Diagnostics())
	}

	s, e, err = Parse(strings.NewReader(text), &ParseOptions{Limits: onlyBody("encoded", 4)})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err = e.WriteTo(&out)
	if !strings.HasSuffix(out.String(), "\r\n\r\nYWJj") {
		t.Errorf("WriteTo past the encoded budget wrote %q, want the header and the 4 admitted wire bytes", out.String())
	}
	checkBodyFailure(t, "WriteTo", s, e, err, CodeLimitEncodedBodyBytes, false)
}

// TestParse_totalDecodedBytes pins the shared budget: decoded output of every
// leaf, counted once, exact at its boundary, and a stop of the whole parse that
// every leaf — including one already returned — reports from then on.
func TestParse_totalDecodedBytes(t *testing.T) {
	leaves := func(bodies ...string) string {
		s := "Content-Type: multipart/mixed; boundary=B\r\n\r\n"
		for _, b := range bodies {
			s += "--B\r\nContent-Type: text/plain\r\n\r\n" + b + "\r\n"
		}
		return s + "--B--\r\n"
	}

	t.Run("9 then 2 under 10", func(t *testing.T) {
		for src, open := range bodySources(leaves("123456789", "OK")) {
			for _, k := range []int{1, 2, 4096} {
				name := fmt.Sprintf("%s/dst %d", src, k)
				s, e, err := Parse(open(), &ParseOptions{Limits: onlyBody("total", 10)})
				if err != nil {
					t.Fatal(err)
				}
				mr := e.MultipartReader()
				first, _ := mr.NextPart()
				if b, err := readBy(first.Body, k); string(b) != "123456789" || err != io.EOF {
					t.Errorf("%s: first = (%q, %v)", name, b, err)
				}
				second, err := mr.NextPart()
				if err != nil {
					t.Fatalf("%s: NextPart() = %v", name, err)
				}
				b, err := readBy(second.Body, k)
				if string(b) != "O" {
					t.Errorf("%s: second = %q, want %q", name, b, "O")
				}
				checkBodyFailure(t, name, s, second, err, CodeLimitTotalDecodedBytes, true)
				if d := s.Diagnostics()[0]; !reflect.DeepEqual(d.Path, []int{1}) {
					t.Errorf("%s: path = %v, want the leaf that proved it", name, d.Path)
				}
				// An earlier leaf delivers nothing more, and the parse is over.
				if n, again := first.Body.Read(make([]byte, 4)); n != 0 || again != err {
					t.Errorf("%s: first leaf after the stop = (%d, %v)", name, n, again)
				}
				if _, again := mr.NextPart(); again != err {
					t.Errorf("%s: NextPart after the stop = %v", name, again)
				}
			}
		}
	})

	t.Run("exact, then an empty leaf, then one byte", func(t *testing.T) {
		s, e, err := Parse(strings.NewReader(leaves("123456789", "OK", "", "x")), &ParseOptions{Limits: onlyBody("total", 11)})
		if err != nil {
			t.Fatal(err)
		}
		got, err := walkBodies(e, 4096, nil)
		for i, want := range []string{"123456789", "OK", ""} {
			if r := got[fmt.Sprint([]int{i})]; r.body != want || r.err != io.EOF {
				t.Errorf("leaf %d = (%q, %v), want (%q, io.EOF)", i, r.body, r.err, want)
			}
		}
		if r := got["[3]"]; r.body != "" || r.err != e.ParseStop() || r.err == nil {
			t.Errorf("leaf 3 = (%q, %v), want nothing and the stop", r.body, r.err)
		}
		if err != nil && err != e.ParseStop() {
			t.Errorf("Walk() = %v", err)
		}
		if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeLimitTotalDecodedBytes}) {
			t.Errorf("diagnostics = %v", codes)
		}
	})

	t.Run("a leaf read in part before the stop", func(t *testing.T) {
		_, e, err := Parse(strings.NewReader(leaves("123456789", "OK")), &ParseOptions{Limits: onlyBody("total", 4)})
		if err != nil {
			t.Fatal(err)
		}
		mr := e.MultipartReader()
		first, _ := mr.NextPart()
		first.Body.Read(make([]byte, 3))
		second, _ := mr.NextPart()
		b, err := io.ReadAll(second.Body)
		if string(b) != "O" || err == nil || err != e.ParseStop() {
			t.Errorf("second = (%q, %v)", b, err)
		}
		if n, again := first.Body.Read(make([]byte, 4)); n != 0 || again != err {
			t.Errorf("first leaf after the stop = (%d, %v), want the stop", n, again)
		}
	})

	t.Run("total wins a tie with the decoded budget", func(t *testing.T) {
		l := onlyBody("total", 5)
		l.MaxDecodedBodyBytes = 5
		s, e, err := Parse(strings.NewReader("\r\n123456"), &ParseOptions{Limits: l})
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(e.Body)
		if string(b) != "12345" {
			t.Errorf("body = %q", b)
		}
		checkBodyFailure(t, "tie", s, e, err, CodeLimitTotalDecodedBytes, true)
	})

	t.Run("a smaller decoded budget stays local", func(t *testing.T) {
		l := onlyBody("total", 100)
		l.MaxDecodedBodyBytes = 3
		s, e, err := Parse(strings.NewReader("\r\n123456"), &ParseOptions{Limits: l})
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(e.Body)
		if string(b) != "123" {
			t.Errorf("body = %q", b)
		}
		checkBodyFailure(t, "local", s, e, err, CodeLimitDecodedBodyBytes, false)
	})
}

// TestParse_bodyBudgetsPrecedence pins which failure a body reports when more
// than one budget, or a stop, meets it.
func TestParse_bodyBudgetsPrecedence(t *testing.T) {
	t.Run("input refuses the encoded proof byte", func(t *testing.T) {
		const header = "\r\n"
		l := onlyBody("encoded", 5)
		l.MaxInputBytes = int64(len(header) + 5)
		s, e, err := Parse(strings.NewReader(header+"abcdefghij"), &ParseOptions{Limits: l})
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(e.Body)
		if string(b) != "abcde" || err == nil || err != e.ParseStop() {
			t.Errorf("body = (%q, %v), want 5 bytes and the input stop", b, err)
		}
		if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeLimitInputBytes}) {
			t.Errorf("diagnostics = %v, want only the input overflow", codes)
		}
	})

	t.Run("encoded and decoded equal on identity", func(t *testing.T) {
		l := onlyBody("encoded", 5)
		l.MaxDecodedBodyBytes = 5
		s, e, _ := Parse(strings.NewReader("\r\nabcdefghij"), &ParseOptions{Limits: l})
		b, err := io.ReadAll(e.Body)
		if string(b) != "abcde" {
			t.Errorf("body = %q", b)
		}
		checkBodyFailure(t, "equal", s, e, err, CodeLimitEncodedBodyBytes, false)
	})

	t.Run("decoded proven within the encoded budget", func(t *testing.T) {
		l := onlyBody("encoded", 5)
		l.MaxDecodedBodyBytes = 2
		s, e, _ := Parse(strings.NewReader("\r\nabcdefghij"), &ParseOptions{Limits: l})
		b, err := io.ReadAll(e.Body)
		if string(b) != "ab" {
			t.Errorf("body = %q", b)
		}
		checkBodyFailure(t, "decoded first", s, e, err, CodeLimitDecodedBodyBytes, false)
	})

	const qp = "Content-Transfer-Encoding: quoted-printable\r\n\r\n"
	longLine := strings.Repeat("a", 60) + "\r\nbb\r\n"
	t.Run("line refusal within the encoded budget", func(t *testing.T) {
		l := onlyBody("encoded", 100)
		l.MaxDecodedBodyBytes = 2
		l.MaxLineBytes = 50
		s, e, _ := Parse(strings.NewReader(qp+longLine), &ParseOptions{Limits: l})
		_, err := io.ReadAll(e.Body)
		// The line was refused before the decoders produced anything, and the
		// body keeps that one failure.
		checkBodyFailure(t, "line", s, e, err, CodeLimitLineBytes, false)
	})

	t.Run("encoded refusal before a line ends", func(t *testing.T) {
		l := onlyBody("encoded", 3)
		l.MaxLineBytes = 50
		s, e, _ := Parse(strings.NewReader(qp+longLine), &ParseOptions{Limits: l})
		b, err := io.ReadAll(e.Body)
		if string(b) != "aaa" {
			t.Errorf("body = %q", b)
		}
		checkBodyFailure(t, "encoded before line", s, e, err, CodeLimitEncodedBodyBytes, false)
	})

	sinkErr := errors.New("sink stop")
	stopFirst := DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error { return sinkErr })

	t.Run("a stop published before the refusal wins", func(t *testing.T) {
		// The sink stops the parse at the unknown transfer encoding. The body
		// that came back with the stop may still be read, as before, and the
		// later overflow reports the earlier stop and records nothing.
		for _, kind := range []string{"encoded", "decoded", "total"} {
			s, e, err := Parse(strings.NewReader("Content-Transfer-Encoding: x-unknown\r\n\r\nabcdefghij"),
				&ParseOptions{Limits: onlyBody(kind, 5), DiagnosticSink: stopFirst})
			stop := e.ParseStop()
			if stop == nil || err != stop {
				t.Fatalf("%s: Parse() = %v", kind, err)
			}
			b, err := io.ReadAll(e.Body)
			if string(b) != "abcde" || err != stop {
				t.Errorf("%s: body = (%q, %v), want the 5 admitted bytes and the earlier stop", kind, b, err)
			}
			if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeCTEUnknown}) {
				t.Errorf("%s: diagnostics = %v", kind, codes)
			}
		}
	})

	t.Run("a sink refusing the local overflow stops the parse", func(t *testing.T) {
		sink := DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
			if d.Code == CodeLimitDecodedBodyBytes {
				return sinkErr
			}
			return nil
		})
		_, e, _ := Parse(strings.NewReader("\r\nabcdef"), &ParseOptions{Limits: onlyBody("decoded", 2), DiagnosticSink: sink})
		_, err := io.ReadAll(e.Body)
		if stop := e.ParseStop(); stop == nil || err != stop || !errors.Is(err, sinkErr) {
			t.Errorf("body end = %v, ParseStop() = %v; want the sink's stop", err, stop)
		}
	})

	t.Run("a sink refusing the total overflow", func(t *testing.T) {
		sink := DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error { return sinkErr })
		_, e, _ := Parse(strings.NewReader("\r\nabcdef"), &ParseOptions{Limits: onlyBody("total", 2), DiagnosticSink: sink})
		_, err := io.ReadAll(e.Body)
		if stop := e.ParseStop(); stop == nil || err != stop || !errors.Is(err, sinkErr) {
			t.Errorf("body end = %v, ParseStop() = %v; want the sink's stop", err, stop)
		}
		if n, again := e.Body.Read(make([]byte, 1)); n != 0 || again != err {
			t.Errorf("read again = (%d, %v)", n, again)
		}
	})

	t.Run("enforced past the diagnostic cap", func(t *testing.T) {
		l := onlyBody("decoded", 2)
		l.MaxDiagnosticCount = 1
		s, e, err := Parse(strings.NewReader(bodyTree), &ParseOptions{Limits: l})
		if err != nil {
			t.Fatal(err)
		}
		got, err := walkBodies(e, 4096, nil)
		if err != nil || e.ParseStop() != nil {
			t.Fatalf("Walk() = %v", err)
		}
		for _, key := range []string{"[0]", "[1 0]", "[1 1]", "[2]", "[3]"} {
			var fatal FatalError
			if r := got[key]; len(r.body) != 2 || !errors.As(r.err, &fatal) {
				t.Errorf("leaf %s = (%q, %v), want 2 bytes and a FatalError", key, r.body, r.err)
			} else if d, _ := fatal.Diagnostic(); d.Code != CodeLimitDecodedBodyBytes {
				t.Errorf("leaf %s carries %v, want its own decoded overflow", key, d.Code)
			}
		}
		if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeLimitDiagnosticCount}) {
			t.Errorf("diagnostics = %v, want only the terminal", codes)
		}
	})
}

// failingSource delivers s and then fails with err, together with the last
// bytes when withData is set.
type failingSource struct {
	s        string
	err      error
	withData bool
}

func (f *failingSource) Read(p []byte) (int, error) {
	if f.s == "" {
		return 0, f.err
	}
	n := copy(p, f.s)
	f.s = f.s[n:]
	if f.s == "" && f.withData {
		return n, f.err
	}
	return n, nil
}

// TestParse_bodyBudgetsSourceEdges pins the io.Reader edges below the budgets:
// an input error with or without bytes, at the boundary and before it, empty
// reads, and the disabled and largest budgets.
func TestParse_bodyBudgetsSourceEdges(t *testing.T) {
	boom := errors.New("boom")
	for _, withData := range []bool{false, true} {
		for _, kind := range []string{"encoded", "decoded", "total"} {
			name := fmt.Sprintf("%s/withData %v", kind, withData)

			// The input fails exactly at the budget: no byte proves an overflow, so
			// the input's error is reported as it is and is not fatal.
			_, e, _ := Parse(&failingSource{s: "\r\nabcde", err: boom, withData: withData}, &ParseOptions{Limits: onlyBody(kind, 5)})
			b, err := io.ReadAll(e.Body)
			if string(b) != "abcde" || err != boom {
				t.Errorf("%s at the budget: (%q, %v), want all 5 bytes and the input's error", name, b, err)
			}

			// A byte past the budget arrives with the error: it proves the
			// overflow all the same.
			s, e, _ := Parse(&failingSource{s: "\r\nabcdef", err: boom, withData: withData}, &ParseOptions{Limits: onlyBody(kind, 5)})
			b, err = io.ReadAll(e.Body)
			if string(b) != "abcde" {
				t.Errorf("%s past the budget: body %q", name, b)
			}
			checkBodyFailure(t, name, s, e, err, bodyCodes[kind], kind == "total")

			// An error before the budget passes with the bytes it came with, which
			// are counted.
			_, e, _ = Parse(&failingSource{s: "\r\nabc", err: boom, withData: withData}, &ParseOptions{Limits: onlyBody(kind, 5)})
			b, err = io.ReadAll(e.Body)
			if string(b) != "abc" || err != boom {
				t.Errorf("%s before the budget: (%q, %v)", name, b, err)
			}
		}
	}

	t.Run("empty reads", func(t *testing.T) {
		for _, kind := range []string{"encoded", "decoded", "total"} {
			meter := &inputMeter{r: strings.NewReader("\r\nabcdef")}
			_, e, _ := Parse(meter, &ParseOptions{Limits: onlyBody(kind, 3)})
			read := meter.read
			if n, err := e.Body.Read(nil); n != 0 || err != nil || meter.read != read {
				t.Errorf("%s: empty read = (%d, %v), source read %d more", kind, n, err, meter.read-read)
			}
			io.ReadAll(e.Body)
			if n, err := e.Body.Read(nil); n != 0 || !IsFatal(err) {
				t.Errorf("%s: empty read after the failure = (%d, %v), want the failure", kind, n, err)
			}
		}
	})

	t.Run("disabled and math.MaxInt64", func(t *testing.T) {
		big := "\r\n" + strings.Repeat("x", 1<<16)
		for _, limit := range []int64{LimitDisabled, math.MaxInt64} {
			l := Limits{MaxEncodedBodyBytes: limit, MaxDecodedBodyBytes: limit, MaxTotalDecodedBytes: limit}
			s, e, _ := Parse(strings.NewReader(big), &ParseOptions{Limits: l})
			if b, err := io.ReadAll(e.Body); len(b) != 1<<16 || err != nil || s.Len() != 0 {
				t.Errorf("limit %d: (%d, %v)", limit, len(b), err)
			}
		}
	})
}

// TestParse_bodyBudgetsLeaves pins which entities are leaves: an unknown
// transfer encoding, a strictly refused Content-Type, a multipart kept opaque
// for want of a usable boundary, and a message/rfc822 part all are.
func TestParse_bodyBudgetsLeaves(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		profile      Profile
	}{
		{"unknown encoding", "Content-Transfer-Encoding: x-unknown\r\n", ProfileTolerant},
		{"strict refusal", "Content-Type: text/plain; a=\"b\r\n", ProfileStrict},
		{"unframed multipart", "Content-Type: multipart/mixed; boundary=B; boundary=C\r\n", ProfileTolerant},
		{"message/rfc822", "Content-Type: message/rfc822\r\n", ProfileTolerant},
	} {
		_, e, _ := Parse(strings.NewReader(tc.header+"\r\n--B\r\n\r\nabcdef"), &ParseOptions{Profile: tc.profile, Limits: onlyBody("decoded", 3)})
		if e.MultipartReader() != nil {
			t.Fatalf("%s: framed", tc.name)
		}
		b, err := io.ReadAll(e.Body)
		var fatal FatalError
		if len(b) != 3 || !errors.As(err, &fatal) {
			t.Errorf("%s: (%q, %v), want 3 bytes and the decoded overflow", tc.name, b, err)
		} else if d, _ := fatal.Diagnostic(); d.Code != CodeLimitDecodedBodyBytes {
			t.Errorf("%s: %v", tc.name, d.Code)
		}
	}
}

// TestParse_bodyBudgetsLegacyUnchanged pins that nothing changes where there
// is no parse: Read, ReadWithOptions and New wrap nothing. Under Parse every
// leaf is guarded, even with every body budget disabled — the guard is also
// what keeps a decoder from hiding the input's failure — and a Parse with every
// body budget disabled enforces none of them.
func TestParse_bodyBudgetsLegacyUnchanged(t *testing.T) {
	big := "Content-Transfer-Encoding: base64\r\n\r\n" + strings.Repeat("QUFB", 1<<14)
	for name, read := range map[string]func() (*Entity, error){
		"Read":            func() (*Entity, error) { return Read(strings.NewReader(big)) },
		"ReadWithOptions": func() (*Entity, error) { return ReadWithOptions(strings.NewReader(big), &ReadOptions{}) },
		"New": func() (*Entity, error) {
			h, _ := textprotoHeader("Content-Transfer-Encoding: base64")
			return New(h, strings.NewReader(strings.Repeat("QUFB", 1<<14)))
		},
	} {
		e, err := read()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, ok := e.Body.(*bodyGuard); ok {
			t.Errorf("%s: Body is guarded", name)
		}
		if b, err := io.ReadAll(e.Body); len(b) != 3<<14 || err != nil {
			t.Errorf("%s: (%d, %v)", name, len(b), err)
		}
	}

	s, e, err := Parse(strings.NewReader(big), &ParseOptions{Limits: onlyBody("", 0)})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := readBy(e.Body, 3); len(b) != 3<<14 || err != io.EOF || s.Len() != 0 {
		t.Errorf("Parse, body budgets disabled: (%d, %v), %v", len(b), err, s.Diagnostics())
	}

	// Without a parse, a decoder still reports a failed input in its own words,
	// as it always has.
	_, raw, _ := strings.Cut(qpCutText, "\r\n\r\n")
	h, _ := textprotoHeader("Content-Transfer-Encoding: quoted-printable")
	legacy, _ := New(h, &failingSource{s: raw[:5], err: errCut})
	if b, err := io.ReadAll(legacy.Body); string(b) != "foo" || err == errCut || IsFatal(err) {
		t.Errorf("New: (%q, %v), want the decoder's own report of the failed input", b, err)
	}
}

// qpCutText is a valid quoted-printable body whose soft line break an input
// budget can cut, which the decoder reports as invalid bytes after "=".
const qpCutText = "Content-Transfer-Encoding: quoted-printable\r\n\r\nfoo=\r\nbar"

// TestParse_bodyInputStopNotMasked pins that a decoder cannot hide the input
// budget's stop: the body delivers everything the decoders made of the
// admitted bytes and then reports the stop, with every body budget disabled,
// at their defaults, and with one enforced. Only a FatalError the input itself
// returned is reported this way: a plain input error stays whatever the decoder
// made of it, and a stop published for another reason is not looked up.
func TestParse_bodyInputStopNotMasked(t *testing.T) {
	header, _, _ := strings.Cut(qpCutText, "\r\n\r\n")
	header += "\r\n"
	wires := []string{"foo=6Abar", "foo=\r\nbar", "foo=\nbar", "foo= \r\nbar", "foo=\rXbar"}
	configs := map[string]Limits{
		"body budgets disabled": onlyBody("", 0),
		"defaults":              {},
		"encoded enforced":      onlyBody("encoded", 100),
	}
	for cname, base := range configs {
		for _, w := range wires {
			for cut := 1; cut < len(w); cut++ {
				for _, k := range []int{1, 2, 4096} {
					name := fmt.Sprintf("%s/%q/cut %d/dst %d", cname, w, cut, k)
					l := base
					l.MaxInputBytes = int64(len(header) + 2 + cut)
					s, e, err := Parse(strings.NewReader(header+"\r\n"+w), &ParseOptions{Limits: l})
					if err != nil {
						t.Fatalf("%s: Parse() = %v", name, err)
					}
					b, err := readBy(e.Body, k)
					f := bodyFixture{header: strings.TrimSuffix(header, "\r\n") + "\r\n"}
					if want := referenceDecode(t, f, w[:cut]); string(b) != want {
						t.Errorf("%s: body = %q, want %q", name, b, want)
					}
					if stop := e.ParseStop(); stop == nil || err != stop {
						t.Errorf("%s: end = %v, want the input stop %v", name, err, stop)
					}
					if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeLimitInputBytes}) {
						t.Errorf("%s: diagnostics = %v", name, codes)
					}
				}
			}
		}
	}

	t.Run("part", func(t *testing.T) {
		const leaf = "--B\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nfoo=\r\nbar\r\n--B--\r\n"
		text := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" + leaf
		l := onlyBody("", 0)
		l.MaxInputBytes = int64(strings.Index(text, "foo") + 5)
		_, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: l})
		if err != nil {
			t.Fatal(err)
		}
		p, err := e.MultipartReader().NextPart()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(p.Body)
		if stop := e.ParseStop(); string(b) != "foo" || stop == nil || !errors.Is(err, stop) || !IsFatal(err) {
			t.Errorf("part = (%q, %v), want %q and an error wrapping the stop %v", b, err, "foo", stop)
		}
	})

	t.Run("nested part", func(t *testing.T) {
		const inner = "--I\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nfoo=\r\nbar\r\n--I--\r\n"
		text := "Content-Type: multipart/mixed; boundary=O\r\n\r\n--O\r\nContent-Type: multipart/alternative; boundary=I\r\n\r\n" + inner + "--O--\r\n"
		for _, l := range []Limits{{}, onlyBody("", 0)} {
			l.MaxInputBytes = int64(strings.Index(text, "foo") + 5)
			_, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: l})
			if err != nil {
				t.Fatal(err)
			}
			c, _ := e.MultipartReader().NextPart()
			p, err := c.MultipartReader().NextPart()
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(p.Body)
			if stop := e.ParseStop(); string(b) != "foo" || stop == nil || !errors.Is(err, stop) || !IsFatal(err) {
				t.Errorf("nested part = (%q, %v), want %q and an error wrapping the stop %v", b, err, "foo", stop)
			}
			if n, again := p.Body.Read(make([]byte, 1)); n != 0 || again != err {
				t.Errorf("read again = (%d, %v), want the identical error", n, again)
			}
		}
	})

	t.Run("plain input error", func(t *testing.T) {
		// An ordinary input error is not a stop, but since ROB-010-006 it is
		// not the decoder's to lose either: quoted-printable's own wording of
		// the cut is kept, joined after the input's failure.
		_, raw, _ := strings.Cut(qpCutText, "\r\n\r\n")
		_, e, err := Parse(&failingSource{s: header + "\r\n" + raw[:5], err: errCut}, &ParseOptions{Limits: onlyBody("", 0)})
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(e.Body)
		if string(b) != "foo" || err == errCut || !errors.Is(err, errCut) || IsFatal(err) || e.ParseStop() != nil {
			t.Errorf("(%q, %v), want the input's failure joined with the decoder's report, and no stop", b, err)
		}
	})
}

// cyclicError unwraps to itself, so errors.Is or errors.As on it never returns.
type cyclicError struct{}

func (*cyclicError) Error() string   { return "cyclic" }
func (e *cyclicError) Unwrap() error { return e }

// trappedError panics if anything asks it As.
type trappedError struct{}

func (*trappedError) Error() string { return "trapped" }
func (*trappedError) As(any) bool   { panic("the source's As must not be called") }

// plainError is a source error a test can hand out as a typed nil.
type plainError struct{}

func (*plainError) Error() string { return "plain" }

const sourceErrorEnv = "EMLKIT_BODY_SOURCE_ERROR"

// TestParse_bodySourceErrorsUntouched pins that a leaf body hands a source's
// own error back unchanged and never asks it anything: a source error whose
// Unwrap never ends, or whose As panics, reaches the caller as the identical
// value after the bytes before it, with the body budgets at their defaults and
// disabled, for a root body, a decoded root body and a part. Each case runs in
// a subprocess with a deadline, so that a regression fails rather than hangs
// or crashes the test binary.
func TestParse_bodySourceErrorsUntouched(t *testing.T) {
	if kind := os.Getenv(sourceErrorEnv); kind != "" {
		if msg := sourceErrorCase(kind); msg != "" {
			fmt.Println("CHILD-FAIL", msg)
			os.Exit(1)
		}
		fmt.Println("CHILD-PASS")
		os.Exit(0)
	}
	for _, errKind := range []string{"cycle", "as"} {
		for _, shape := range []string{"root", "qp", "part"} {
			for _, limits := range []string{"defaults", "disabled"} {
				kind := errKind + "/" + shape + "/" + limits
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestParse_bodySourceErrorsUntouched$", "-test.count=1")
				cmd.Env = append(os.Environ(), sourceErrorEnv+"="+kind)
				out, err := cmd.CombinedOutput()
				cancel()
				if err != nil || !bytes.Contains(out, []byte("CHILD-PASS")) {
					t.Errorf("%s: subprocess = %v, deadline %v\n%s", kind, err, ctx.Err(), out)
				}
			}
		}
	}

	// A typed nil is an error like any other, and comes back as itself.
	var typedNil *plainError
	for _, l := range []Limits{{}, onlyBody("", 0)} {
		_, e, err := Parse(&failingSource{s: "\r\nabc", err: typedNil}, &ParseOptions{Limits: l})
		if err != nil {
			t.Fatal(err)
		}
		if b, err := io.ReadAll(e.Body); string(b) != "abc" || err != error(typedNil) {
			t.Errorf("typed nil: (%q, %v), want the identical value", b, err)
		}
	}
}

// sourceErrorCase runs one case of TestParse_bodySourceErrorsUntouched in the
// subprocess and returns what went wrong, or "".
func sourceErrorCase(kind string) string {
	parts := strings.Split(kind, "/")
	var fail error = &cyclicError{}
	if parts[0] == "as" {
		fail = &trappedError{}
	}
	var l Limits
	if parts[2] == "disabled" {
		l = onlyBody("", 0)
	}
	text := "Subject: qa\r\n\r\nabc"
	switch parts[1] {
	case "qp":
		text = "Content-Transfer-Encoding: quoted-printable\r\n\r\nabc"
	case "part":
		text = "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\nabc"
	}
	_, e, err := Parse(&failingSource{s: text, err: fail}, &ParseOptions{Limits: l})
	if err != nil || e == nil {
		return fmt.Sprintf("Parse() = %v", err)
	}
	body := e
	if parts[1] == "part" {
		if body, err = e.MultipartReader().NextPart(); err != nil {
			return fmt.Sprintf("NextPart() = %v", err)
		}
	}
	b, err := io.ReadAll(body.Body)
	if string(b) != "abc" {
		return fmt.Sprintf("body = %q", b)
	}
	if parts[1] != "part" {
		if err != fail {
			return fmt.Sprintf("end = %v, want the source's error", err)
		}
		return ""
	}
	// Under a part the framing joins the source's error with its own; only
	// the standard library's join is looked into, to find it first.
	if reflect.TypeOf(err) != joinErrorType {
		return fmt.Sprintf("end = %T %v, want the framing's join", err, err)
	}
	if errs := err.(interface{ Unwrap() []error }).Unwrap(); len(errs) == 0 || errs[0] != fail {
		return fmt.Sprintf("end = %v, want the source's error first", err)
	}
	return ""
}

// TestParse_totalStopAfterLocalFailure pins that the total stop outranks a
// local failure a body already reported: the error the caller holds stays
// valid, but a later read of that body returns the parse's stop, without
// reading the input or recording anything more.
func TestParse_totalStopAfterLocalFailure(t *testing.T) {
	const text = "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\n12345\r\n--B\r\n\r\nOK\r\n--B--\r\n"
	l := onlyBody("decoded", 3)
	l.MaxTotalDecodedBytes = 4
	meter := &inputMeter{r: strings.NewReader(text)}
	s, e, err := Parse(meter, &ParseOptions{Limits: l})
	if err != nil {
		t.Fatal(err)
	}
	mr := e.MultipartReader()
	first, _ := mr.NextPart()
	b, local := io.ReadAll(first.Body)
	if string(b) != "123" || e.ParseStop() != nil {
		t.Fatalf("first = (%q, %v)", b, local)
	}
	second, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	b, global := io.ReadAll(second.Body)
	if string(b) != "O" || global == nil || global != e.ParseStop() {
		t.Fatalf("second = (%q, %v), want %q and the total stop", b, global, "O")
	}

	read, diags := meter.read, s.Len()
	if n, again := first.Body.Read(make([]byte, 4)); n != 0 || again != global {
		t.Errorf("first again = (%d, %v), want the total stop", n, again)
	}
	var fatal FatalError
	if !errors.As(local, &fatal) {
		t.Fatalf("the local error held = %v", local)
	}
	if d, _ := fatal.Diagnostic(); d.Code != CodeLimitDecodedBodyBytes {
		t.Errorf("the local error held now carries %v", d.Code)
	}
	if meter.read != read || s.Len() != diags || diags != 2 {
		t.Errorf("after the retry: %d more input bytes, %d diagnostics; want none more and 2", meter.read-read, s.Len())
	}
}

// TestParse_bodyBudgetsRealDefaults pins which budget a single large body meets
// first when nothing is configured: MaxEncodedBodyBytes, at 128 MiB, before
// MaxInputBytes at 256 MiB.
func TestParse_bodyBudgetsRealDefaults(t *testing.T) {
	if testing.Short() {
		t.Skip("reads 128 MiB")
	}
	const header = "Subject: x\r\n\r\n"
	size := int64(len(header)) + defaultMaxEncodedBodyBytes + 1<<20
	s, e, err := Parse(&repeatReader{header: header, size: size}, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, e.Body)
	if n != defaultMaxEncodedBodyBytes {
		t.Errorf("copied %d, want %d", n, defaultMaxEncodedBodyBytes)
	}
	checkBodyFailure(t, "defaults", s, e, err, CodeLimitEncodedBodyBytes, false)
}

// endless hands out bytes for ever.
type endless struct{}

func (endless) Read(p []byte) (int, error) { return len(p), nil }

// TestBodyBudgets_acceptedPathAllocs pins that a read within every budget costs
// no allocation.
func TestBodyBudgets_acceptedPathAllocs(t *testing.T) {
	s := newParseSessionWithConfig(resolvedConfig{limits: DefaultLimits()}, diagnosticOptions{})
	ctx := parseContext{session: s}
	r, slot := ctx.limitBody(endless{})
	g := ctx.guardBody(r, slot)
	buf := make([]byte, 512)
	if allocs := testing.AllocsPerRun(200, func() { g.Read(buf) }); allocs != 0 {
		t.Errorf("a read within the budgets allocates %v times", allocs)
	}
}

// FuzzParse_bodyBudgets checks properties every parse under small body budgets
// keeps: no leaf delivers more than its decoded budget, the leaves together no
// more than the total, a failure is repeated identically, and nothing panics.
func FuzzParse_bodyBudgets(f *testing.F) {
	f.Add([]byte(bodyTree), uint8(4), uint8(4), uint8(9), uint8(3), uint8(1))
	f.Add([]byte("Content-Transfer-Encoding: quoted-printable\r\n\r\nab=63=\r\nde=6"), uint8(3), uint8(0), uint8(0), uint8(7), uint8(2))
	f.Add([]byte("Content-Transfer-Encoding: base64\r\n\r\nYWJj\r\nZGVm"), uint8(5), uint8(2), uint8(3), uint8(1), uint8(5))
	f.Fuzz(func(t *testing.T, msg []byte, enc, dec, total, chunk, dst uint8) {
		budget := func(v uint8) int64 {
			if v == 0 {
				return LimitDisabled
			}
			return int64(v)
		}
		l := Limits{MaxEncodedBodyBytes: budget(enc), MaxDecodedBodyBytes: budget(dec), MaxTotalDecodedBytes: budget(total)}
		_, e, err := Parse(&inputSource{s: string(msg), chunk: int(chunk%8) + 1}, &ParseOptions{Limits: l})
		if e == nil {
			return
		}
		var sum int64
		e.Walk(func(_ []int, part *Entity, _ error) error {
			if part.MultipartReader() != nil {
				return nil
			}
			b, err := readBy(part.Body, int(dst%9)+1)
			sum += int64(len(b))
			if l.MaxDecodedBodyBytes > 0 && int64(len(b)) > l.MaxDecodedBodyBytes {
				t.Fatalf("leaf delivered %d bytes past a decoded budget of %d", len(b), l.MaxDecodedBodyBytes)
			}
			if IsFatal(err) {
				if n, again := part.Body.Read(make([]byte, 4)); n != 0 || again != err {
					t.Fatalf("read after %v = (%d, %v)", err, n, again)
				}
			}
			return nil
		})
		if l.MaxTotalDecodedBytes > 0 && sum > l.MaxTotalDecodedBytes {
			t.Fatalf("leaves delivered %d bytes past a total of %d", sum, l.MaxTotalDecodedBytes)
		}
		_ = err
	})
}
