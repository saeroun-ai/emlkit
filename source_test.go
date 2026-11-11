package message

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Tests of the raw source and the indexed mode (ROB-110-001, -004, -005,
// ROB-080-002); see docs/robustness-notes/raw-source.md.

const srcTestClean = "MIME-Version: 1.0\r\n" +
	"From: a@example.org\r\n" +
	"Subject: =?utf-8?q?caf=C3=A9?=\r\n" +
	"Content-Type: multipart/mixed; boundary=outer\r\n" +
	"\r\n" +
	"preamble\r\n" +
	"--outer\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n" +
	"\r\n" +
	"caf=C3=A9 =\r\nau lait\r\n" +
	"--outer\r\n" +
	"Content-Type: multipart/alternative; boundary=inner\r\n" +
	"\r\n" +
	"--inner\r\n" +
	"Content-Type: text/html\r\n" +
	"\r\n" +
	"<p>hi</p>\r\n" +
	"--inner--\r\n" +
	"--outer\r\n" +
	"Content-Type: application/octet-stream\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"aGVsbG8gd29ybGQ=\r\n" +
	"--outer\r\n" +
	"Content-Type: message/rfc822\r\n" +
	"\r\n" +
	"Subject: inner\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"inner body\r\n" +
	"--outer--\r\n" +
	"epilogue\r\n"

// A damaged message: a header line with no colon, bare LF line endings,
// Base64 with junk in it, and a multipart that is never closed.
const srcTestHealed = "From: a@example.org\n" +
	"this line has no colon\n" +
	"Content-Type: multipart/mixed; boundary=B\n" +
	"\n" +
	"--B\n" +
	"Content-Type: text/plain\n" +
	"\n" +
	"one\n" +
	"--B\n" +
	"Content-Transfer-Encoding: base64\n" +
	"\n" +
	"aGVs*bG8gd29y\nbGQ\n"

// srcTestInputs returns the two messages above and every fixture of the
// regression corpus.
func srcTestInputs(t *testing.T) map[string][]byte {
	t.Helper()
	in := map[string][]byte{"clean": []byte(srcTestClean), "healed": []byte(srcTestHealed)}
	files, err := filepath.Glob(filepath.Join("testdata", "corpus", "*.eml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no corpus fixtures: %v", err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		in[filepath.Base(f)] = data
	}
	return in
}

func srcTestLeaf(p *Entity) bool {
	return p.MultipartReader() == nil && p.walkEmbedded() == nil
}

// srcTestOutcome is everything a caller can observe of a parse: every entity
// with its header, decoded body and error, the walk's error, the diagnostics
// and the completion state.
func srcTestOutcome(s *ParseSession, e *Entity, err error) []string {
	out := []string{fmt.Sprintf("parse: %v", err)}
	if e != nil {
		werr := e.Walk(func(path []int, p *Entity, perr error) error {
			var h bytes.Buffer
			textproto.WriteHeader(&h, p.Header.Header)
			mt, _, _ := p.EffectiveContentType()
			line := fmt.Sprintf("%v %s err=%v header=%q", path, mt, perr, h.String())
			if srcTestLeaf(p) {
				body, rerr := io.ReadAll(p.Body)
				line += fmt.Sprintf(" body=%q rerr=%v", body, rerr)
			}
			out = append(out, line)
			return nil
		})
		out = append(out, fmt.Sprintf("walk: %v", werr))
	}
	for _, d := range s.Diagnostics() {
		out = append(out, fmt.Sprintf("%s | %s | %s", d, d.Message, d.Action))
	}
	return append(out, "state: "+s.Completion().State.String())
}

// Parse, ParseAt and a recorded ParseSource give the same result.
func TestSrcEquivalentResults(t *testing.T) {
	for name, data := range srcTestInputs(t) {
		for _, opts := range []*ParseOptions{nil, {WalkEmbeddedMessages: true, Profile: ProfileForensic}, {Profile: ProfileStrict}} {
			want := srcTestOutcome(Parse(bytes.NewReader(data), opts))
			at := srcTestOutcome(ParseAt(bytes.NewReader(data), int64(len(data)), opts))
			if !reflect.DeepEqual(at, want) {
				t.Errorf("%s: ParseAt differs from Parse:\n got %q\nwant %q", name, at, want)
			}
			rec := NewRecordedSource(bytes.NewReader(data), nil)
			if got := srcTestOutcome(ParseSource(nil, rec, opts)); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: recorded ParseSource differs from Parse:\n got %q\nwant %q", name, got, want)
			}
			// A stream that delivers one byte at a time is recorded as it
			// is parsed, and parsed as it is without the recording.
			want = srcTestOutcome(Parse(iotest.OneByteReader(bytes.NewReader(data)), opts))
			rec = NewRecordedSource(iotest.OneByteReader(bytes.NewReader(data)), nil)
			if got := srcTestOutcome(ParseSource(nil, rec, opts)); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: recorded one-byte ParseSource differs from Parse:\n got %q\nwant %q", name, got, want)
			}
			if n, complete := rec.Size(); complete && n != int64(len(data)) {
				t.Errorf("%s: recorded %d bytes of %d", name, n, len(data))
			}
		}
	}
}

type srcTestView struct {
	whole, header, body string // raw bytes, or the error of asking
	decoded, again      string
	span                string
}

func srcTestRaw(r *io.SectionReader, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	b, rerr := io.ReadAll(r)
	if rerr != nil {
		return "read error: " + rerr.Error()
	}
	return "ok:" + string(b)
}

func srcTestAgain(p *Entity) string {
	r, err := p.DecodedBody()
	if err != nil {
		return "error: " + err.Error()
	}
	b, rerr := io.ReadAll(r)
	return fmt.Sprintf("ok:%s rerr=%v", b, rerr)
}

// srcTestViews walks e and reads, for every entity, its raw bytes and its
// decoded body, in the order named.
func srcTestViews(t *testing.T, e *Entity, order string) (map[string]*srcTestView, []*Entity) {
	t.Helper()
	views := make(map[string]*srcTestView)
	var all []*Entity
	raw := func(p *Entity, v *srcTestView) {
		v.whole = srcTestRaw(p.RawReader())
		v.header = srcTestRaw(p.RawHeader())
		v.body = srcTestRaw(p.RawBody())
		v.again = srcTestAgain(p)
	}
	e.Walk(func(path []int, p *Entity, _ error) error {
		v := &srcTestView{}
		views[fmt.Sprint(path)] = v
		all = append(all, p)
		leaf := srcTestLeaf(p)
		switch order {
		case "raw first":
			raw(p, v)
			if leaf {
				b, rerr := io.ReadAll(p.Body)
				v.decoded = fmt.Sprintf("ok:%s rerr=%v", b, rerr)
			}
		case "decoded first":
			if leaf {
				b, rerr := io.ReadAll(p.Body)
				v.decoded = fmt.Sprintf("ok:%s rerr=%v", b, rerr)
			}
			raw(p, v)
		case "raw during":
			var first [1]byte
			n := 0
			if leaf {
				n, _ = p.Body.Read(first[:])
			}
			raw(p, v)
			if leaf {
				b, rerr := io.ReadAll(p.Body)
				v.decoded = fmt.Sprintf("ok:%s%s rerr=%v", first[:n], b, rerr)
			}
		}
		if !leaf {
			v.again = ""
		}
		return nil
	})
	return views, all
}

// The raw bytes of every entity are the input slice RawSpan names, whatever
// the order of the raw and the decoded reads, and DecodedBody repeats Body.
func TestSrcReadOrderIndependence(t *testing.T) {
	served := 0
	for name, data := range srcTestInputs(t) {
		opts := &ParseOptions{WalkEmbeddedMessages: true}
		var ref map[string]*srcTestView
		for _, order := range []string{"decoded first", "raw first", "raw during"} {
			_, e, _ := ParseAt(bytes.NewReader(data), int64(len(data)), opts)
			if e == nil {
				continue
			}
			views, all := srcTestViews(t, e, order)
			if ref == nil {
				ref = views
				// The reference order is checked against the input itself.
				if got := views["[]"].whole; got != "ok:"+string(data) {
					t.Errorf("%s: root raw = %q, want the input", name, got)
				}
				// A message carried as it is has its place in the input;
				// one inside Base64, and a part of an inferred boundary,
				// have none.
				for at, want := range map[string]string{
					"clean [3 0]":                         "ok:Subject: inner\r\nContent-Type: text/plain\r\n\r\ninner body",
					"clean [3 0] decoded":                 "ok:inner body rerr=<nil>",
					"embedded-message-base64.eml [0 0]":   "error: " + ErrRawSpanUnknown.Error(),
					"multipart-boundary-mismatch.eml [0]": "error: " + ErrRawSpanUnknown.Error(),
				} {
					in, path, _ := strings.Cut(at, " ")
					if in != name {
						continue
					}
					path, decoded := strings.CutSuffix(path, " decoded")
					v := views[path]
					if v == nil {
						t.Errorf("%s: no entity at %s", name, path)
					} else if got := v.whole; !decoded && got != want {
						t.Errorf("%s %s: raw = %q, want %q", name, path, got, want)
					} else if decoded && v.again != want {
						t.Errorf("%s %s: decoded again = %q, want %q", name, path, v.again, want)
					}
				}
				for _, p := range all {
					o, _ := p.Origin()
					v := views[fmt.Sprint(o.Path)]
					if strings.HasPrefix(v.whole, "ok:") {
						served++
						if v.whole != "ok:"+v.header[3:]+v.body[3:] {
							t.Errorf("%s %v: header %q and body %q are not the whole %q", name, o.Path, v.header, v.body, v.whole)
						}
						if v.again != "" && v.again != v.decoded {
							t.Errorf("%s %v: DecodedBody = %q, Body = %q", name, o.Path, v.again, v.decoded)
						}
					}
					if start, end, ok := p.RawSpan(); ok && v.whole != "ok:"+string(data[start:end]) {
						t.Errorf("%s %v: raw = %q, want input[%d:%d] = %q", name, o.Path, v.whole, start, end, data[start:end])
					}
				}
				continue
			}
			if len(views) != len(ref) {
				t.Errorf("%s: %s visited %d entities, want %d", name, order, len(views), len(ref))
			}
			for path, v := range views {
				if r := ref[path]; r == nil || *v != *r {
					t.Errorf("%s %s: %s reads\n %+v\nwant %+v", name, path, order, v, r)
				}
			}
		}
	}
	if served < 40 {
		t.Errorf("only %d entities had raw bytes: the test checks too little", served)
	}
}

func srcTestHex(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

// srcTestSum is the SHA-256 of a raw reader, or the error of getting it.
func srcTestSum(r *io.SectionReader, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "read error: " + err.Error()
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// The header and body digests a DKIM verifier takes are those of the input,
// and stay so across every semantic read and a write, for a clean and for a
// healed message, seekable or recorded.
func TestSrcDigestsSurviveSemanticReads(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		split      int
		healed     bool
	}{
		{"clean", srcTestClean, strings.Index(srcTestClean, "\r\n\r\n") + 4, false},
		{"healed", srcTestHealed, strings.Index(srcTestHealed, "\n\n") + 2, true},
	} {
		data := []byte(tc.data)
		wantHeader, wantBody := srcTestHex(data[:tc.split]), srcTestHex(data[tc.split:])
		for _, mode := range []string{"seekable", "recorded"} {
			src := NewSource(bytes.NewReader(data), int64(len(data)))
			if mode == "recorded" {
				src = NewRecordedSource(bytes.NewReader(data), nil)
			}
			s, e, err := ParseSource(nil, src, nil)
			if e == nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			check := func(when string) {
				t.Helper()
				if got := srcTestSum(e.RawHeader()); got != wantHeader {
					t.Errorf("%s %s %s: header digest differs from the input's", tc.name, mode, when)
				}
				if got := srcTestSum(e.RawBody()); got != wantBody {
					t.Errorf("%s %s %s: body digest differs from the input's", tc.name, mode, when)
				}
			}
			if mode == "seekable" {
				check("before any read")
			}

			// Semantic reads: decoded header text, the tree, every decoded
			// body, the diagnostics.
			e.Header.Text("Subject")
			e.Header.Set("X-Added", "by the caller")
			leaves := 0
			if werr := e.Walk(func(_ []int, p *Entity, _ error) error {
				p.Header.ContentType()
				if srcTestLeaf(p) {
					leaves++
					io.Copy(io.Discard, p.Body)
				}
				return nil
			}); werr != nil {
				t.Errorf("%s: walk: %v", tc.name, werr)
			}
			if leaves < 2 {
				t.Errorf("%s: %d leaves", tc.name, leaves)
			}
			// What follows the last part belongs to the root's body; a
			// recorded source knows its end once it has been read.
			io.Copy(io.Discard, e.Body)
			if healed := len(s.Diagnostics()) > 0; healed != tc.healed {
				t.Errorf("%s: diagnostics %v, healed = %v", tc.name, s.Diagnostics(), tc.healed)
			}
			check("after the reads")

			// And a write of a fresh parse of the same source bytes.
			_, w, _ := ParseAt(bytes.NewReader(data), int64(len(data)), nil)
			var out bytes.Buffer
			if err := w.WriteTo(&out); err != nil {
				t.Errorf("%s: write: %v", tc.name, err)
			}
			if !tc.healed && !bytes.Equal(out.Bytes(), data) {
				t.Errorf("%s: written message differs from the input:\n%q", tc.name, out.Bytes())
			}
			if got := srcTestSum(w.RawReader()); got != srcTestHex(data) {
				t.Errorf("%s: message digest after the write differs from the input's", tc.name)
			}
			check("after the write")
		}
	}
}

// srcTestFile is a RawStore in memory that can be made to fail.
type srcTestStore struct {
	mu     sync.Mutex
	b      []byte
	failAt int
}

func (s *srcTestStore) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAt > 0 && len(s.b)+len(p) > s.failAt {
		n := s.failAt - len(s.b)
		s.b = append(s.b, p[:n]...)
		return n, errors.New("store full")
	}
	s.b = append(s.b, p...)
	return len(p), nil
}

func (s *srcTestStore) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if off >= int64(len(s.b)) {
		return 0, io.EOF
	}
	n := copy(p, s.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// A recording that reaches its limit, or whose store fails, keeps a prefix:
// the parse is unaffected, the parts inside the prefix are served, and the
// rest report ErrRawNotRecorded.
func TestSrcRecordLimit(t *testing.T) {
	data := []byte(srcTestClean)
	want := srcTestOutcome(Parse(bytes.NewReader(data), nil))
	firstEnd := int64(bytes.Index(data, []byte("\r\n--outer\r\nContent-Type: multipart/alternative")))
	limit := firstEnd + 10

	file, err := os.CreateTemp(t.TempDir(), "raw")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	for name, opts := range map[string]*RecordOptions{
		"memory limit": {MaxBytes: limit},
		"file limit":   {MaxBytes: limit, Store: file},
		"store fails":  {MaxBytes: -1, Store: &srcTestStore{failAt: int(limit)}},
	} {
		src := NewRecordedSource(bytes.NewReader(data), opts)
		s, e, err := ParseSource(nil, src, nil)
		if e == nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s.Source() != src {
			t.Errorf("%s: session does not report its source", name)
		}
		var parts []*Entity
		e.Walk(func(path []int, p *Entity, _ error) error {
			if len(path) == 1 {
				parts = append(parts, p)
			}
			return nil
		})
		io.Copy(io.Discard, e.Body)

		if n, complete := src.Size(); n != limit || complete || !src.Truncated() {
			t.Errorf("%s: Size = %d, %v, Truncated = %v; want %d, false, true", name, n, complete, src.Truncated(), limit)
		}
		if failed := src.Err() != nil; failed != (name == "store fails") {
			t.Errorf("%s: Err = %v", name, src.Err())
		}
		if len(parts) != 4 {
			t.Fatalf("%s: %d parts", name, len(parts))
		}
		start, end, _ := parts[0].RawSpan()
		if got := srcTestRaw(parts[0].RawReader()); got != "ok:"+string(data[start:end]) || end != firstEnd {
			t.Errorf("%s: first part raw = %q", name, got)
		}
		if got := srcTestAgain(parts[0]); got != "ok:café au lait rerr=<nil>" {
			t.Errorf("%s: first part decoded again = %q", name, got)
		}
		if got := srcTestRaw(e.RawHeader()); !strings.HasPrefix(got, "ok:MIME-Version: 1.0\r\nFrom: a@example.org\r\n") {
			t.Errorf("%s: root header = %q", name, got)
		}
		for what, err := range map[string]error{
			"root":        srcTestErr(e.RawReader()),
			"root body":   srcTestErr(e.RawBody()),
			"second part": srcTestErr(parts[1].RawHeader()),
			"last part":   srcTestErr(parts[3].RawReader()),
			"decoded":     func() error { _, err := parts[2].DecodedBody(); return err }(),
		} {
			if !errors.Is(err, ErrRawNotRecorded) {
				t.Errorf("%s: %s: %v, want ErrRawNotRecorded", name, what, err)
			}
		}
		p := make([]byte, 20)
		if n, err := src.ReadAt(p, limit-5); n != 5 || !errors.Is(err, ErrRawNotRecorded) || string(p[:n]) != string(data[limit-5:limit]) {
			t.Errorf("%s: ReadAt across the limit = %d, %v", name, n, err)
		}
	}

	// The parse itself never saw the limit.
	rec := NewRecordedSource(bytes.NewReader(data), &RecordOptions{MaxBytes: 1})
	if got := srcTestOutcome(ParseSource(nil, rec, nil)); !reflect.DeepEqual(got, want) {
		t.Errorf("a recording limit changed the parse:\n got %q\nwant %q", got, want)
	}
}

func srcTestErr(_ *io.SectionReader, err error) error { return err }

// srcTestBig is a multipart of 64 parts, longer than every buffer of the
// parse, so that a stream of it is not read whole before its parts are.
func srcTestBig() []byte {
	var msg bytes.Buffer
	msg.WriteString("Content-Type: multipart/mixed; boundary=B\r\n\r\n")
	for i := 0; i < 64; i++ {
		fmt.Fprintf(&msg, "--B\r\nContent-Type: text/plain\r\nX-Part: %d\r\n\r\n%s\r\n", i, strings.Repeat(fmt.Sprintf("line %d\r\n", i), 40))
	}
	msg.WriteString("--B--\r\n")
	return msg.Bytes()
}

// The policies of the entry points: which entities have a source, how often
// a source is parsed, and what a recorded source knows while it is read.
func TestSrcPolicies(t *testing.T) {
	data := []byte(srcTestClean)

	_, plain, _ := Parse(bytes.NewReader(data), nil)
	made, _ := New(Header{}, strings.NewReader("x"))
	for _, e := range []*Entity{plain, made, {}} {
		if _, err := e.RawReader(); !errors.Is(err, ErrNoRawSource) {
			t.Errorf("RawReader without a source: %v", err)
		}
		if _, err := e.DecodedBody(); !errors.Is(err, ErrNoRawSource) {
			t.Errorf("DecodedBody without a source: %v", err)
		}
	}
	if s, e, err := ParseSource(nil, nil, nil); s == nil || e != nil || !errors.Is(err, ErrNoRawSource) {
		t.Errorf("ParseSource(nil source) = %v, %v, %v", s, e, err)
	}

	// A seekable source is parsed as often as wanted; a recorded one once.
	seek := NewSource(bytes.NewReader(data), int64(len(data)))
	for i := 0; i < 2; i++ {
		if _, e, err := ParseSource(nil, seek, nil); e == nil || err != nil {
			t.Errorf("seekable parse %d: %v", i, err)
		}
	}
	rec := NewRecordedSource(bytes.NewReader(data), nil)
	_, e, _ := ParseSource(nil, rec, nil)
	if s, again, err := ParseSource(nil, rec, nil); s == nil || again != nil || !errors.Is(err, ErrSourceUsed) {
		t.Errorf("second parse of a recorded source: %v, %v", again, err)
	}

	// While a recorded source is read, a part is known once the parse has
	// moved past it, and its header as soon as it exists; the root's end is
	// known once the input has been read to its end.
	big := srcTestBig()
	rec = NewRecordedSource(bytes.NewReader(big), nil)
	_, e, _ = ParseSource(nil, rec, nil)
	mr := e.MultipartReader()
	first, _ := mr.NextPart()
	if got := srcTestRaw(first.RawHeader()); got != "ok:Content-Type: text/plain\r\nX-Part: 0\r\n\r\n" {
		t.Errorf("header of an unread part = %q", got)
	}
	if _, err := first.RawBody(); !errors.Is(err, ErrRawSpanUnknown) {
		t.Errorf("body of an unread part of a recorded source: %v", err)
	}
	if _, err := e.RawBody(); !errors.Is(err, ErrRawSpanUnknown) {
		t.Errorf("root body of a recorded source still being read: %v", err)
	}
	mr.NextPart()
	if got := srcTestRaw(first.RawBody()); got != "ok:"+strings.Repeat("line 0\r\n", 40) {
		t.Errorf("body of a passed part = %q", got)
	}
	if got := srcTestAgain(first); got != "ok:"+strings.Repeat("line 0\r\n", 40)+" rerr=<nil>" {
		t.Errorf("decoded body of a passed part = %q", got)
	}
	e.Drain()
	io.Copy(io.Discard, e.Body)
	if got := srcTestRaw(e.RawReader()); got != "ok:"+string(big) {
		t.Errorf("root of a recorded source read to its end: %d bytes, want the input", len(got))
	}

	// A decoded reader is fresh each time, and the one-shot Body is still
	// the one-shot Body.
	_, e, _ = ParseAt(bytes.NewReader(data), int64(len(data)), nil)
	first, _ = e.MultipartReader().NextPart()
	for i := 0; i < 2; i++ {
		if got := srcTestAgain(first); got != "ok:café au lait rerr=<nil>" {
			t.Errorf("decoded read %d = %q", i, got)
		}
	}
	if b, _ := io.ReadAll(first.Body); string(b) != "café au lait" {
		t.Errorf("Body after DecodedBody = %q", b)
	}
	if b, _ := io.ReadAll(first.Body); len(b) != 0 {
		t.Errorf("Body read twice = %q", b)
	}
}

// Raw readers are independent values over an io.ReaderAt: any number may be
// read at once, from any goroutine, while the parse that records the source
// goes on. Run with -race.
func TestSrcConcurrentRawReads(t *testing.T) {
	data := srcTestBig()

	for _, mode := range []string{"seekable", "recorded"} {
		src := NewSource(bytes.NewReader(data), int64(len(data)))
		if mode == "recorded" {
			src = NewRecordedSource(iotest.HalfReader(bytes.NewReader(data)), nil)
		}
		_, e, err := ParseSource(nil, src, nil)
		if e == nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make(chan string, 256)
		slots := make(chan struct{}, 8)
		mr := e.MultipartReader()
		var prev *Entity
		parts := 0
		for {
			p, err := mr.NextPart()
			// The part before this one has been passed: its raw bytes are
			// read by two goroutines, each with its own reader and through
			// ReadAt on a shared one, while the parse reads on.
			if prev != nil {
				start, end, ok := prev.RawSpan()
				own, err1 := prev.RawReader()
				shared, err2 := prev.RawReader()
				if !ok || err1 != nil || err2 != nil {
					t.Fatalf("%s: part %d: span %v, %v, %v", mode, parts, ok, err1, err2)
				}
				want := data[start:end]
				for g := 0; g < 2; g++ {
					wg.Add(1)
					slots <- struct{}{}
					go func(g int) {
						defer wg.Done()
						defer func() { <-slots }()
						got := make([]byte, len(want))
						var rerr error
						if g == 0 {
							_, rerr = io.ReadFull(own, got)
						} else {
							_, rerr = shared.ReadAt(got, 0)
						}
						if rerr != nil || !bytes.Equal(got, want) {
							errs <- fmt.Sprintf("%s: concurrent raw read: %v", mode, rerr)
						}
					}(g)
				}
			}
			if err != nil {
				break
			}
			parts++
			io.Copy(io.Discard, p.Body)
			prev = p
		}
		wg.Wait()
		close(errs)
		for msg := range errs {
			t.Error(msg)
		}
		if parts != 64 {
			t.Errorf("%s: %d parts", mode, parts)
		}
	}
}
