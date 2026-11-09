package message

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

// The input after the message (ROB-010-005). Every source here is finite,
// every read loop and traversal capped; a cap reached fails the test rather
// than cutting anything short.

// trailingTestCap bounds every byte stream a test here reads whole.
const trailingTestCap = 64 << 10

// readCapped reads r to its end in reads of size bytes, at most
// trailingTestCap bytes and 4096+trailingTestCap reads, and returns what it
// read and the error that ended it.
func readCapped(t *testing.T, r io.Reader, size int) (string, error) {
	t.Helper()
	var b strings.Builder
	buf := make([]byte, size)
	for i := 0; i < 4096+trailingTestCap; i++ {
		n, err := r.Read(buf)
		b.Write(buf[:n])
		if b.Len() > trailingTestCap {
			t.Fatalf("stream longer than the %d-byte test cap", trailingTestCap)
		}
		if err != nil {
			return b.String(), err
		}
	}
	t.Fatalf("stream did not end within the test's read cap")
	return "", nil
}

// How a test consumes the message before it looks at the trailing input.
type trailingMode int

const (
	modeWalk     trailingMode = iota // Entity.Walk, every leaf body read
	modeIterate                      // the root iterator, every part's body read
	modeSkipping                     // the root iterator, no body read
)

var trailingModes = map[string]trailingMode{"walk": modeWalk, "iterate": modeIterate, "skip bodies": modeSkipping}

// traverseRoot consumes e as mode says, at most 64 parts at depth at most 16,
// and returns what ended it: nil for an iteration that ended with io.EOF.
func traverseRoot(t *testing.T, e *Entity, mode trailingMode) error {
	t.Helper()
	if mode == modeWalk {
		parts := 0
		return e.Walk(func(path []int, part *Entity, err error) error {
			parts++
			if parts > 64 || len(path) > 16 {
				t.Fatalf("traversal past the test's cap: %d parts, path %v", parts, path)
			}
			if !part.framesParts() {
				readCapped(t, part.Body, 4096)
			}
			return nil
		})
	}
	mr := e.MultipartReader()
	if mr == nil {
		t.Fatalf("the root frames no parts")
	}
	for i := 0; ; i++ {
		if i > 64 {
			t.Fatalf("more than 64 parts")
		}
		p, err := mr.NextPart()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if mode == modeIterate {
			readCapped(t, p.Body, 7)
		}
	}
}

// A tailSource delivers s in reads of at most chunk bytes, counting the reads
// and bytes, and calls hook, when set, before each read with the number of
// reads made since it was set.
type tailSource struct {
	s     string
	chunk int
	pos   int
	reads int
	hook  func(n int)
	since int
}

func (c *tailSource) Read(p []byte) (int, error) {
	c.reads++
	if c.hook != nil {
		c.since++
		c.hook(c.since)
	}
	if c.pos >= len(c.s) {
		return 0, io.EOF
	}
	n := copy(p, c.s[c.pos:min(len(c.s), c.pos+c.chunk)])
	c.pos += n
	return n, nil
}

const trailingHead = "Content-Type: multipart/mixed; boundary=B\r\n\r\npreamble\r\n" +
	"--B\r\nContent-Type: text/plain\r\n\r\none\r\n" +
	"--B\r\n\r\ntwo\r\n"

// trailingCloses are closing delimiter lines, their line ending included.
var trailingCloses = map[string]string{
	"crlf":   "--B--\r\n",
	"lf":     "--B--\n",
	"cr":     "--B--\r",
	"padded": "--B-- \t\r\n",
}

const trailingSecond = "From: a@example.com\r\nTo: b@example.com\r\nSubject: second\r\n\r\nsecond body\r\n"

// trailingTails are what follows the closing delimiter. None begins with '\n',
// which would make a closing '\r' a "\r\n".
var trailingTails = map[string]struct {
	tail string
	ev   TrailingEvidence
}{
	"ordinary":   {"This is the epilogue.  It is also to be ignored.\r\n", TrailingEvidenceUnrecognized},
	"empty":      {"", TrailingEvidenceEmpty},
	"blank":      {"\r\n\r\n  \r\n", TrailingEvidenceBlank},
	"binary":     {"\x00\xff\xfe junk\r\n\x00", TrailingEvidenceUnrecognized},
	"message":    {trailingSecond, TrailingEvidenceMessageLike},
	"mbox":       {testFromLine + "\r\n" + trailingSecond, TrailingEvidenceMessageLike},
	"blank then": {"\r\n\r\nReceived: x\r\nFrom: y\r\n\r\nz", TrailingEvidenceMessageLike},
	"delimiters": {"--B\r\n\r\nthree\r\n--B--\r\n", TrailingEvidenceUnrecognized},
	"big":        {strings.Repeat("epilogue line\r\n", 1000), TrailingEvidenceUnrecognized},
}

func trailingDeliveries(s string) map[string]func() io.Reader {
	return map[string]func() io.Reader{
		"whole":         func() io.Reader { return strings.NewReader(s) },
		"chunks 1":      func() io.Reader { return &chunkReader{s: s, n: 1} },
		"chunks 7":      func() io.Reader { return &chunkReader{s: s, n: 7} },
		"random":        func() io.Reader { return newRandomChunkReader(s, 600, 7) },
		"data with eof": func() io.Reader { return iotest.DataErrReader(strings.NewReader(s)) },
	}
}

// withoutTrailing returns diags without input.trailing_messages.
func withoutTrailing(diags []Diagnostic) []Diagnostic {
	var out []Diagnostic
	for _, d := range diags {
		if d.Code != CodeInputTrailingMessages {
			out = append(out, d)
		}
	}
	return out
}

func trailingDiags(diags []Diagnostic) []Diagnostic {
	var out []Diagnostic
	for _, d := range diags {
		if d.Code == CodeInputTrailingMessages {
			out = append(out, d)
		}
	}
	return out
}

// checkTrailingEvidence checks the evidence of ti and the diagnostics of a
// tail that started at start against the reference, for a tail read whole.
func checkTrailingEvidence(t *testing.T, name string, ti TrailingInput, diags []Diagnostic, tail string, start int64) {
	t.Helper()
	want := refWindowed([]byte(tail), maxMboxEnvelopeContent)
	ev, loc := ti.Evidence()
	if ev != want.ev {
		t.Errorf("%s: evidence %v, want %v", name, ev, want.ev)
	}
	td := trailingDiags(diags)
	if want.ev != TrailingEvidenceMessageLike {
		if loc != (Location{}) || len(td) != 0 {
			t.Errorf("%s: %v evidence has location %v and diagnostics %v", name, ev, loc, td)
		}
		return
	}
	wantLoc := LocationRange(start+want.start, start+want.end)
	if loc != wantLoc {
		t.Errorf("%s: evidence at %v, want %v", name, loc, wantLoc)
	}
	if len(td) != 1 {
		t.Fatalf("%s: %d input.trailing_messages diagnostics, want 1", name, len(td))
	}
	d := td[0]
	if d.Severity != SeverityInfo || d.Path != nil || d.Location != wantLoc || d.Field != "" || d.Cause != nil {
		t.Errorf("%s: diagnostic %+v", name, d)
	}
	for _, s := range []string{"example.com", "MAILER", "Received"} {
		if strings.Contains(d.Message, s) || strings.Contains(d.Action, s) {
			t.Errorf("%s: the diagnostic repeats input bytes: %q / %q", name, d.Message, d.Action)
		}
	}
}

// TestParse_trailingExact reads the input after a closing delimiter of every
// line ending, after every kind of traversal, however the input and the reads
// are chunked: every byte, verbatim, at the right offset, with the evidence
// the reference gives and diagnostics otherwise exactly Tolerant's.
func TestParse_trailingExact(t *testing.T) {
	type tcase struct{ name, prefix, tail string }
	var cases []tcase
	for cname, close := range trailingCloses {
		for tname, tc := range trailingTails {
			cases = append(cases, tcase{cname + "/" + tname, trailingHead + close, tc.tail})
		}
	}
	cases = append(cases, tcase{"at eof", trailingHead + "--B--", ""}, tcase{"at eof padded", trailingHead + "--B--  ", ""},
		tcase{"cr at eof", trailingHead + "--B--\r", ""},
		tcase{"no parts", "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B--\r\n", trailingSecond})

	for _, tc := range cases {
		input := tc.prefix + tc.tail
		start := int64(len(tc.prefix))
		for mname, mode := range trailingModes {
			bs, base, err := Parse(strings.NewReader(input), &ParseOptions{Profile: ProfileTolerant})
			if err != nil {
				t.Fatal(err)
			}
			if err := traverseRoot(t, base, mode); err != nil {
				t.Fatalf("%s: tolerant traversal: %v", tc.name, err)
			}
			baseDiags := bs.Diagnostics()

			for dname, src := range trailingDeliveries(input) {
				for _, size := range []int{1, 4096} {
					name := fmt.Sprintf("%s/%s/%s/%d", tc.name, mname, dname, size)
					s, e, err := Parse(src(), &ParseOptions{Profile: ProfileForensic})
					if err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					if ti := s.TrailingInput(); ti != (TrailingInput{}) {
						t.Fatalf("%s: before the traversal: %+v", name, ti)
					}
					if r, ok := s.TrailingInputReader(); ok || r != nil {
						t.Fatalf("%s: a reader before the traversal", name)
					}
					if err := traverseRoot(t, e, mode); err != nil {
						t.Fatalf("%s: traversal: %v", name, err)
					}
					ti := s.TrailingInput()
					if at, ok := ti.Start(); ti.Status() != TrailingOpen || !ok || at != start || ti.Delivered() != 0 || ti.Location() != LocationAt(start) || ti.Err() != nil {
						t.Fatalf("%s: after the traversal %v at %d (%v), %d delivered, %v, %v; want open at %d", name, ti.Status(), at, ok, ti.Delivered(), ti.Location(), ti.Err(), start)
					}
					if ev, _ := ti.Evidence(); ev != TrailingEvidenceUndecided {
						t.Errorf("%s: evidence %v before a read", name, ev)
					}
					r, ok := s.TrailingInputReader()
					r2, _ := s.TrailingInputReader()
					if !ok || r == nil || r != r2 {
						t.Fatalf("%s: TrailingInputReader = %v, %v; again %v", name, r, ok, r2)
					}
					got, rerr := readCapped(t, r, size)
					if got != tc.tail || rerr != io.EOF {
						t.Fatalf("%s: read %s, %v; want %s, io.EOF", name, preview(got), rerr, preview(tc.tail))
					}
					ti = s.TrailingInput()
					if ti.Status() != TrailingEnded || ti.Delivered() != int64(len(tc.tail)) || ti.Location() != LocationRange(start, int64(len(input))) || ti.Err() != nil {
						t.Errorf("%s: after reading %v, %d delivered, %v, %v", name, ti.Status(), ti.Delivered(), ti.Location(), ti.Err())
					}
					checkTrailingEvidence(t, name, ti, s.Diagnostics(), tc.tail, start)
					if got := withoutTrailing(s.Diagnostics()); !reflect.DeepEqual(got, baseDiags) {
						t.Errorf("%s: diagnostics %v, Tolerant %v", name, codesOfDiags(got), codesOfDiags(baseDiags))
					}
					for _, p := range [][]byte{make([]byte, 8), nil} {
						if n, err := r.Read(p); n != 0 || err != io.EOF {
							t.Errorf("%s: a read after the end = %d, %v", name, n, err)
						}
					}
					if e.ParseStop() != nil {
						t.Errorf("%s: ParseStop %v", name, e.ParseStop())
					}
				}
			}
		}
	}
}

// TestParse_trailingAfterMbox reads the trailing input of a message that
// follows an mbox envelope line: its offset counts the line.
func TestParse_trailingAfterMbox(t *testing.T) {
	env := testFromLine + "\r\n"
	prefix := env + trailingHead + "--B--\r\n"
	s, e, err := Parse(strings.NewReader(prefix+trailingSecond), &ParseOptions{Profile: ProfileForensic, RecognizeMboxEnvelope: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.MboxEnvelope(); !ok {
		t.Fatal("no envelope recognized")
	}
	if err := traverseRoot(t, e, modeWalk); err != nil {
		t.Fatal(err)
	}
	r, _ := s.TrailingInputReader()
	if got, err := readCapped(t, r, 64); got != trailingSecond || err != io.EOF {
		t.Fatalf("read %q, %v", got, err)
	}
	ti := s.TrailingInput()
	if at, _ := ti.Start(); at != int64(len(prefix)) {
		t.Errorf("start %d, want %d", at, len(prefix))
	}
	checkTrailingEvidence(t, "mbox", ti, s.Diagnostics(), trailingSecond, int64(len(prefix)))
}

// A countingReader-like source that counts the bytes it delivered.
type byteCounter struct {
	r io.Reader
	n int64
}

func (c *byteCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// TestParse_trailingProfileParity pins that nothing changes unless the trailing
// input is read: Forensic reads the same bytes and records the same
// diagnostics as Tolerant, and no other profile offers a reader.
func TestParse_trailingProfileParity(t *testing.T) {
	for cname, close := range trailingCloses {
		for tname, tc := range trailingTails {
			input := trailingHead + close + tc.tail
			for mname, mode := range trailingModes {
				for dname, src := range trailingDeliveries(input) {
					name := cname + "/" + tname + "/" + mname + "/" + dname
					type outcome struct {
						read  int64
						diags []Diagnostic
						err   error
					}
					run := func(p Profile) (outcome, *ParseSession) {
						c := &byteCounter{r: src()}
						s, e, err := Parse(c, &ParseOptions{Profile: p})
						if err != nil {
							t.Fatalf("%s: %v: %v", name, p, err)
						}
						terr := traverseRoot(t, e, mode)
						return outcome{c.n, s.Diagnostics(), terr}, s
					}
					tol, ts := run(ProfileTolerant)
					forn, fs := run(ProfileForensic)
					if !reflect.DeepEqual(tol, forn) {
						t.Errorf("%s: Forensic %+v, Tolerant %+v", name, forn, tol)
					}
					if st := fs.TrailingInput().Status(); st != TrailingOpen {
						t.Errorf("%s: Forensic status %v", name, st)
					}
					_, ss := run(ProfileStrict)
					for _, s := range []*ParseSession{ts, ss} {
						if ti := s.TrailingInput(); ti != (TrailingInput{}) {
							t.Errorf("%s: %v: %+v", name, s.Profile(), ti)
						}
						if r, ok := s.TrailingInputReader(); ok || r != nil {
							t.Errorf("%s: %v offers a reader", name, s.Profile())
						}
					}
				}
			}
		}
	}
}

// TestParse_trailingNone pins a root that frames no parts: there is no
// trailing input, whatever its body holds, and nothing is split or flagged;
// the body is every byte after the header.
func TestParse_trailingNone(t *testing.T) {
	cases := []struct {
		name, input string
		body        int // where the body begins
	}{
		{"leaf with a message pasted in", "Content-Type: text/plain\r\n\r\nfirst body\r\n\r\n" + trailingSecond, 28},
		{"leaf with mbox concatenation", "Subject: one\r\n\r\nbody\r\n" + testFromLine + "\r\nSubject: two\r\n\r\nbody two\r\n", 16},
		{"header only", "Subject: x\r\n", 12},
		{"empty", "", 0},
		{"unframed multipart", "Content-Type: multipart/mixed; boundary=B; boundary=C\r\n\r\n--B\r\n\r\nx\r\n--B--\r\n" + trailingSecond, 57},
		{"utf-16 mark", "\xfe\xff\x00S\x00:\x00 \x00x\r\n\r\n--B--\r\n" + trailingSecond, 0},
	}
	for _, tc := range cases {
		for _, p := range []Profile{ProfileForensic, ProfileTolerant, ProfileStrict} {
			name := tc.name + "/" + p.String()
			s, e, err := Parse(strings.NewReader(tc.input), &ParseOptions{Profile: p})
			if e == nil {
				t.Fatalf("%s: no entity: %v", name, err)
			}
			want := TrailingInput{}
			if p == ProfileForensic {
				want = TrailingInput{status: TrailingNone}
			}
			body, berr := readCapped(t, e.Body, 4096)
			if p != ProfileStrict && (body != tc.input[tc.body:] || berr != io.EOF) {
				t.Errorf("%s: body %q, %v; want %q", name, body, berr, tc.input[tc.body:])
			}
			ti := s.TrailingInput()
			if ti != want {
				t.Errorf("%s: %+v, want %+v", name, ti, want)
			}
			if at, ok := ti.Start(); ok || at != 0 || ti.Location() != (Location{}) {
				t.Errorf("%s: start %d, %v; location %v", name, at, ok, ti.Location())
			}
			if r, ok := s.TrailingInputReader(); ok || r != nil {
				t.Errorf("%s: a reader", name)
			}
			if td := trailingDiags(s.Diagnostics()); len(td) != 0 {
				t.Errorf("%s: %v", name, td)
			}
		}
	}
}

// TestParse_trailingUnknown pins every way a Forensic parse does not reach a
// proven closing delimiter of the message's own body: nothing is known, no
// reader is offered and nothing is read for it.
func TestParse_trailingUnknown(t *testing.T) {
	nested := "Content-Type: multipart/mixed; boundary=O\r\n\r\n--O\r\nContent-Type: multipart/mixed; boundary=I\r\n\r\n" +
		"--I\r\n\r\nx\r\n--I--\r\n" + trailingSecond
	cases := []struct {
		name, input string
		consume     func(t *testing.T, e *Entity)
	}{
		{"missing close", trailingHead, func(t *testing.T, e *Entity) { traverseRoot(t, e, modeWalk) }},
		{"truncated", trailingHead + "--B", func(t *testing.T, e *Entity) { traverseRoot(t, e, modeWalk) }},
		{"truncated padded", trailingHead + "--B  ", func(t *testing.T, e *Entity) { traverseRoot(t, e, modeWalk) }},
		{"suffix", trailingHead + "--B--x\r\n" + trailingSecond, func(t *testing.T, e *Entity) { traverseRoot(t, e, modeWalk) }},
		{"inner close only", nested, func(t *testing.T, e *Entity) { traverseRoot(t, e, modeWalk) }},
		{"not iterated", trailingHead + "--B--\r\n" + trailingSecond, func(t *testing.T, e *Entity) {}},
		{"one part", trailingHead + "--B--\r\n" + trailingSecond, func(t *testing.T, e *Entity) { e.MultipartReader().NextPart() }},
		{"body read", trailingHead + "--B--\r\n" + trailingSecond, func(t *testing.T, e *Entity) {
			if got, _ := readCapped(t, e.Body, 64); !strings.HasSuffix(got, "--B--\r\n"+trailingSecond) {
				t.Errorf("body read directly %q", got)
			}
		}},
		{"body replaced", trailingHead + "--B--\r\n", func(t *testing.T, e *Entity) {
			e.Body = strings.NewReader("--B\r\n\r\nx\r\n--B--\r\n" + trailingSecond)
			traverseRoot(t, e, modeIterate)
		}},
	}
	for _, tc := range cases {
		s, e, err := Parse(strings.NewReader(tc.input), &ParseOptions{Profile: ProfileForensic})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		tc.consume(t, e)
		if ti := s.TrailingInput(); ti != (TrailingInput{}) {
			t.Errorf("%s: %+v", tc.name, ti)
		}
		if r, ok := s.TrailingInputReader(); ok || r != nil {
			t.Errorf("%s: a reader", tc.name)
		}
		if td := trailingDiags(s.Diagnostics()); len(td) != 0 {
			t.Errorf("%s: %v", tc.name, td)
		}
	}

	var nilSession *ParseSession
	if ti := nilSession.TrailingInput(); ti != (TrailingInput{}) {
		t.Errorf("nil session: %+v", ti)
	}
	if r, ok := nilSession.TrailingInputReader(); ok || r != nil {
		t.Errorf("nil session: a reader")
	}
	var zero TrailingInput
	if at, ok := zero.Start(); zero.Status() != TrailingUnknown || ok || at != 0 || zero.Delivered() != 0 || zero.Location() != (Location{}) || zero.Err() != nil {
		t.Errorf("zero TrailingInput: %+v", zero)
	}
	if ev, loc := zero.Evidence(); ev != TrailingEvidenceUndecided || loc != (Location{}) {
		t.Errorf("zero TrailingInput evidence %v, %v", ev, loc)
	}
}

// TestParse_trailingNested pins that only the message's own body has a
// trailing input: a nested multipart's epilogue that looks like a message is
// that part's business, and flags nothing.
func TestParse_trailingNested(t *testing.T) {
	prefix := "Content-Type: multipart/mixed; boundary=O\r\n\r\n--O\r\nContent-Type: multipart/mixed; boundary=I\r\n\r\n" +
		"--I\r\n\r\nx\r\n--I--\r\n" + strings.TrimSuffix(trailingSecond, "\r\n") + "\r\n--O--\r\n"
	tail := "root epilogue\r\n"
	for mname, mode := range trailingModes {
		// Walk iterates the inner container to its closing delimiter; the
		// other modes leave it unread, and the root's own close is the same.
		s, e, err := Parse(strings.NewReader(prefix+tail), &ParseOptions{Profile: ProfileForensic})
		if err != nil {
			t.Fatal(err)
		}
		if err := traverseRoot(t, e, mode); err != nil {
			t.Fatalf("%s: %v", mname, err)
		}
		r, ok := s.TrailingInputReader()
		if !ok {
			t.Fatalf("%s: no reader", mname)
		}
		if got, err := readCapped(t, r, 5); got != tail || err != io.EOF {
			t.Errorf("%s: read %q, %v", mname, got, err)
		}
		if at, _ := s.TrailingInput().Start(); at != int64(len(prefix)) {
			t.Errorf("%s: start %d", mname, at)
		}
		checkTrailingEvidence(t, mname, s.TrailingInput(), s.Diagnostics(), tail, int64(len(prefix)))
	}
}

// TestParse_trailingInputBudget sweeps Limits.MaxInputBytes across the closing
// delimiter and the trailing input. Reading the trailing input spends it like
// any read of the input; not reading it spends nothing.
func TestParse_trailingInputBudget(t *testing.T) {
	prefix := trailingHead + "--B--\r\n"
	input := prefix + trailingSecond
	p, l := int64(len(prefix)), int64(len(input))
	for dname, src := range trailingDeliveries(input) {
		for n := p - 3; n <= l+1; n++ {
			name := fmt.Sprintf("%s/%d", dname, n)
			s, e, err := Parse(src(), &ParseOptions{Profile: ProfileForensic, Limits: Limits{MaxInputBytes: n}})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			terr := traverseRoot(t, e, modeIterate)
			ti := s.TrailingInput()
			switch {
			case ti.Status() == TrailingUnknown:
				// Only a budget the closing delimiter itself, or the byte that
				// decides it, does not fit in ends the iteration first.
				if n > p || terr == nil || terr != e.ParseStop() {
					t.Fatalf("%s: unknown after %v (stop %v)", name, terr, e.ParseStop())
				}
				continue
			case ti.Status() != TrailingOpen || terr != nil:
				t.Fatalf("%s: %v after %v", name, ti.Status(), terr)
			}
			if n < p {
				t.Fatalf("%s: the closing delimiter was reached past the budget", name)
			}
			if e.ParseStop() != nil {
				t.Fatalf("%s: a stop before the trailing input was read: %v", name, e.ParseStop())
			}
			r, _ := s.TrailingInputReader()
			got, rerr := readCapped(t, r, 3)
			ti = s.TrailingInput()
			if n >= l {
				if got != trailingSecond || rerr != io.EOF || ti.Status() != TrailingEnded || e.ParseStop() != nil {
					t.Errorf("%s: %q, %v, %v, stop %v", name, got, rerr, ti.Status(), e.ParseStop())
				}
				continue
			}
			stop := e.ParseStop()
			if got != trailingSecond[:n-p] || stop == nil || rerr != stop || ti.Status() != TrailingStopped || ti.Err() != stop || ti.Delivered() != n-p || ti.Location() != LocationRange(p, n) {
				t.Fatalf("%s: %q, %v, %v (%v), stop %v", name, got, rerr, ti.Status(), ti.Delivered(), stop)
			}
			f, _ := stop.(FatalError)
			if d, ok := f.Diagnostic(); !ok || d.Code != CodeLimitInputBytes || d.Location.Offset != n {
				t.Errorf("%s: stop diagnostic %v", name, d)
			}
			ev, _ := ti.Evidence()
			if want := n-p >= 25; (ev == TrailingEvidenceMessageLike) != want || (ev != TrailingEvidenceMessageLike && ev != TrailingEvidenceUndecided) {
				t.Errorf("%s: evidence %v", name, ev)
			}
			if got := len(trailingDiags(s.Diagnostics())); (got == 1) != (ev == TrailingEvidenceMessageLike) {
				t.Errorf("%s: %d trailing diagnostics for %v evidence", name, got, ev)
			}
			if n2, err := r.Read(make([]byte, 4)); n2 != 0 || err != stop {
				t.Errorf("%s: a later read %d, %v", name, n2, err)
			}
		}
	}
}

// TestParse_trailingSourceError fails the input at every byte of the trailing
// input: the bytes before the failure, then the identical error, which is no
// stop; the input is never read again.
func TestParse_trailingSourceError(t *testing.T) {
	runTrailingSourceErrors(t, []error{errSourceIO, fmt.Errorf("wrapped: %w", io.EOF), io.ErrUnexpectedEOF})
}

const trailingBufferFullEnv = "EMLKIT_TRAILING_BUFFER_FULL"

// TestParse_trailingSourceBufferFull runs the same matrix with a source that
// fails with bufio.ErrBufferFull itself, in a subprocess with a deadline.
func TestParse_trailingSourceBufferFull(t *testing.T) {
	if os.Getenv(trailingBufferFullEnv) != "" {
		runTrailingSourceErrors(t, []error{bufio.ErrBufferFull})
		return
	}
	runTestInSubprocess(t, "^TestParse_trailingSourceBufferFull$", trailingBufferFullEnv, 120*time.Second)
}

func runTrailingSourceErrors(t *testing.T, fails []error) {
	prefix := trailingHead + "--B--\r\n"
	tails := []struct {
		tail    string
		decided int // bytes after which the evidence is decided, -1 never
		ev      TrailingEvidence
	}{
		// "epilogue" could still be a field name; the space after it decides.
		{"epilogue text\r\n", 9, TrailingEvidenceUnrecognized},
		{trailingSecond, 25, TrailingEvidenceMessageLike},
		{"\r\n\r\n", -1, TrailingEvidenceBlank},
	}
	for _, fail := range fails {
		for _, tt := range tails {
			input := prefix + tt.tail
			for cut := len(prefix); cut <= len(input); cut++ {
				for _, chunk := range []int{1, 7, 4096} {
					for _, withData := range []bool{true, false} {
						for _, sticky := range []bool{true, false} {
							name := fmt.Sprintf("%v/%q/cut %d/chunk %d/data %v/sticky %v", fail, tt.tail[:min(8, len(tt.tail))], cut, chunk, withData, sticky)
							src := &ioSource{data: input, cut: cut, chunk: chunk, err: fail, withData: withData, sticky: sticky}
							s, e, err := Parse(src, &ParseOptions{Profile: ProfileForensic})
							if err != nil {
								t.Fatalf("%s: %v", name, err)
							}
							if err := traverseRoot(t, e, modeIterate); err != nil {
								t.Fatalf("%s: traversal %v", name, err)
							}
							r, ok := s.TrailingInputReader()
							if !ok {
								t.Fatalf("%s: no reader", name)
							}
							got, rerr := readCapped(t, r, 5)
							n := cut - len(prefix)
							if got != tt.tail[:n] || rerr != fail {
								t.Fatalf("%s: %q, %v; want %q and the identical error", name, got, rerr, tt.tail[:n])
							}
							ti := s.TrailingInput()
							if ti.Status() != TrailingFailed || ti.Err() != fail || ti.Delivered() != int64(n) || e.ParseStop() != nil {
								t.Errorf("%s: %v, %v, %d, stop %v", name, ti.Status(), ti.Err(), ti.Delivered(), e.ParseStop())
							}
							ev, _ := ti.Evidence()
							want := TrailingEvidenceUndecided
							if tt.decided >= 0 && n >= tt.decided {
								want = tt.ev
							}
							if ev != want {
								t.Errorf("%s: evidence %v, want %v", name, ev, want)
							}
							if got := len(trailingDiags(s.Diagnostics())); (got == 1) != (want == TrailingEvidenceMessageLike) || got > 1 {
								t.Errorf("%s: %d trailing diagnostics", name, got)
							}
							if n2, err := r.Read(make([]byte, 4)); n2 != 0 || err != fail {
								t.Errorf("%s: a later read %d, %v", name, n2, err)
							}
							if src.readsAfter != 0 {
								t.Errorf("%s: the input was read %d times after its failure", name, src.readsAfter)
							}
						}
					}
				}
			}
		}
	}
}

// TestParseContext_trailingCancel cancels the parse with the trailing input
// still buffered, and in the middle of reading it from the input.
func TestParseContext_trailingCancel(t *testing.T) {
	prefix := trailingHead + "--B--\r\n"

	t.Run("buffered", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		src := &tailSource{s: prefix + trailingSecond, chunk: 4096}
		s, e, err := ParseContext(ctx, src, &ParseOptions{Profile: ProfileForensic})
		if err != nil {
			t.Fatal(err)
		}
		if err := traverseRoot(t, e, modeIterate); err != nil {
			t.Fatal(err)
		}
		r, _ := s.TrailingInputReader()
		reads := src.reads
		cancel()
		n, err := r.Read(make([]byte, 64))
		checkCancelStop(t, "first read", err, context.Canceled)
		if n != 0 || src.reads != reads {
			t.Errorf("a cancelled read delivered %d bytes and read the input %d times", n, src.reads-reads)
		}
		ti := s.TrailingInput()
		if ti.Status() != TrailingStopped || ti.Err() != err || ti.Delivered() != 0 || e.ParseStop() != err {
			t.Errorf("%v, %v, %d, stop %v", ti.Status(), ti.Err(), ti.Delivered(), e.ParseStop())
		}
		if _, again := r.Read(make([]byte, 8)); again != err {
			t.Errorf("a later read returned %v", again)
		}
		if _, again := r.Read(nil); again != err {
			t.Errorf("an empty read after the stop returned %v", again)
		}
	})

	t.Run("while reading", func(t *testing.T) {
		tail := strings.Repeat("epilogue line\r\n", 1500)
		for _, k := range []int{1, 2, 5, 9} {
			ctx, cancel := context.WithCancel(context.Background())
			src := &tailSource{s: prefix + tail, chunk: 512}
			s, e, err := ParseContext(ctx, src, &ParseOptions{Profile: ProfileForensic})
			if err != nil {
				t.Fatal(err)
			}
			if err := traverseRoot(t, e, modeSkipping); err != nil {
				t.Fatal(err)
			}
			r, _ := s.TrailingInputReader()
			cancelledAt := -1
			src.hook = func(n int) {
				if n == k {
					cancel()
					cancelledAt = src.reads
				}
			}
			got, rerr := readCapped(t, r, 100)
			checkCancelStop(t, fmt.Sprintf("k=%d", k), rerr, context.Canceled)
			if !strings.HasPrefix(tail, got) {
				t.Errorf("k=%d: delivered bytes that are not the tail's", k)
			}
			if cancelledAt < 0 || src.reads != cancelledAt {
				t.Errorf("k=%d: the input was read %d times after the cancellation (at %d)", k, src.reads-cancelledAt, cancelledAt)
			}
			ti := s.TrailingInput()
			if ti.Status() != TrailingStopped || ti.Delivered() != int64(len(got)) || ti.Err() != rerr {
				t.Errorf("k=%d: %v, %d, %v", k, ti.Status(), ti.Delivered(), ti.Err())
			}
			cancel()
		}
	})
}

// stallingTail delivers s and then reads that bring nothing, at most limit of
// them.
type stallingTail struct {
	s     string
	pos   int
	empty int
	limit int
}

func (c *stallingTail) Read(p []byte) (int, error) {
	if c.pos < len(c.s) {
		n := copy(p, c.s[c.pos:])
		c.pos += n
		return n, nil
	}
	c.empty++
	if c.empty > c.limit {
		return 0, errIOCap
	}
	return 0, nil
}

// TestParse_trailingNoProgress stalls the input after the trailing bytes: the
// no-progress rule ends the tail with the session's stop.
func TestParse_trailingNoProgress(t *testing.T) {
	prefix := trailingHead + "--B--\r\n"
	src := &stallingTail{s: prefix + trailingSecond, limit: 1000}
	s, e, err := Parse(src, &ParseOptions{Profile: ProfileForensic})
	if err != nil {
		t.Fatal(err)
	}
	if err := traverseRoot(t, e, modeIterate); err != nil {
		t.Fatal(err)
	}
	r, _ := s.TrailingInputReader()
	got, rerr := readCapped(t, r, 4096)
	if got != trailingSecond {
		t.Errorf("read %q", got)
	}
	noProgressStop(t, rerr, CodeInputNoProgress)
	if rerr != e.ParseStop() || s.TrailingInput().Status() != TrailingStopped {
		t.Errorf("%v; stop %v; %v", rerr, e.ParseStop(), s.TrailingInput().Status())
	}
	if ev, _ := s.TrailingInput().Evidence(); ev != TrailingEvidenceMessageLike {
		t.Errorf("evidence %v; proven evidence is kept after a stop", ev)
	}
}

var errTrailingStop = errors.New("test sink: stop")

// TestParse_trailingSink covers a sink that refuses the diagnostic and reads
// the snapshot while it is told, the diagnostic budget, discarding, and a stop
// published before the trailing input is read.
func TestParse_trailingSink(t *testing.T) {
	prefix := trailingHead + "--B--\r\n"
	input := prefix + trailingSecond
	start := int64(len(prefix))

	t.Run("refused", func(t *testing.T) {
		var seen TrailingInput
		calls := 0
		sink := DiagnosticSinkFunc(func(s *ParseSession, d Diagnostic) error {
			if d.Code == CodeInputTrailingMessages {
				calls++
				seen = s.TrailingInput()
				return errTrailingStop
			}
			return nil
		})
		s, e, err := Parse(strings.NewReader(input), &ParseOptions{Profile: ProfileForensic, DiagnosticSink: sink})
		if err != nil {
			t.Fatal(err)
		}
		if err := traverseRoot(t, e, modeIterate); err != nil {
			t.Fatal(err)
		}
		r, _ := s.TrailingInputReader()
		got, rerr := readCapped(t, r, 3)
		if calls != 1 || !errors.Is(rerr, errTrailingStop) || rerr != e.ParseStop() {
			t.Fatalf("calls %d, %v, stop %v", calls, rerr, e.ParseStop())
		}
		if seen.Status() != TrailingOpen || seen.Delivered() < 25 || got != trailingSecond[:seen.Delivered()] {
			t.Errorf("the sink saw %v with %d delivered; the reader delivered %q", seen.Status(), seen.Delivered(), got)
		}
		if ev, loc := seen.Evidence(); ev != TrailingEvidenceMessageLike || loc != LocationRange(start, start+25) {
			t.Errorf("the sink saw evidence %v at %v", ev, loc)
		}
		ti := s.TrailingInput()
		if ev, _ := ti.Evidence(); ti.Status() != TrailingStopped || ti.Err() != rerr || ev != TrailingEvidenceMessageLike || ti.Delivered() != int64(len(got)) {
			t.Errorf("after: %v, %v, %v, %d", ti.Status(), ti.Err(), ev, ti.Delivered())
		}
		if n, err := r.Read(make([]byte, 4)); n != 0 || err != rerr {
			t.Errorf("a later read %d, %v", n, err)
		}
		if td := trailingDiags(s.Diagnostics()); len(td) != 1 {
			t.Errorf("retained %v", td)
		}
	})

	t.Run("diagnostic budget", func(t *testing.T) {
		s, e, _ := Parse(strings.NewReader(input), &ParseOptions{Profile: ProfileForensic, Limits: Limits{MaxDiagnosticCount: 1}})
		traverseRoot(t, e, modeIterate)
		r, _ := s.TrailingInputReader()
		if got, err := readCapped(t, r, 4096); got != trailingSecond || err != io.EOF {
			t.Fatalf("%q, %v", got, err)
		}
		if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeLimitDiagnosticCount}) {
			t.Errorf("diagnostics %v", codes)
		}
		if ev, loc := s.TrailingInput().Evidence(); ev != TrailingEvidenceMessageLike || loc != LocationRange(start, start+25) {
			t.Errorf("evidence %v at %v", ev, loc)
		}
	})

	t.Run("discarded", func(t *testing.T) {
		var got []Diagnostic
		sink := DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
			got = append(got, d)
			return nil
		})
		s, e, _ := Parse(strings.NewReader(input), &ParseOptions{Profile: ProfileForensic, DiagnosticSink: sink, DiscardDeliveredDiagnostics: true})
		traverseRoot(t, e, modeIterate)
		r, _ := s.TrailingInputReader()
		readCapped(t, r, 4096)
		if s.Diagnostics() != nil || len(trailingDiags(got)) != 1 {
			t.Errorf("retained %v, delivered %v", s.Diagnostics(), codesOfDiags(got))
		}
		if ev, _ := s.TrailingInput().Evidence(); ev != TrailingEvidenceMessageLike {
			t.Errorf("evidence %v", ev)
		}
	})

	t.Run("stopped before", func(t *testing.T) {
		damaged := strings.Replace(input, "Content-Type: text/plain\r\n", "Content-Type: text/plain\r\nContent-Disposition: attachment; broken\r\n", 1)
		sink := DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
			if d.Code == CodeMIMEDispositionInvalid {
				return errTrailingStop
			}
			return nil
		})
		src := &tailSource{s: damaged, chunk: 4096}
		s, e, err := Parse(src, &ParseOptions{Profile: ProfileForensic, DiagnosticSink: sink})
		if err != nil {
			t.Fatal(err)
		}
		mr := e.MultipartReader()
		first, err := mr.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 4; i++ {
			if _, err := mr.NextPart(); err == io.EOF {
				break
			}
		}
		r, ok := s.TrailingInputReader()
		if !ok {
			t.Fatal("no reader")
		}
		// A part already returned stays the caller's; reading its disposition
		// now records a diagnostic the sink refuses, which stops the parse.
		first.EffectiveContentDisposition()
		stop := e.ParseStop()
		if !errors.Is(stop, errTrailingStop) {
			t.Fatalf("stop %v", stop)
		}
		reads := src.reads
		// A read of no bytes inspects nothing, the stop included.
		if n, err := r.Read(nil); n != 0 || err != nil || s.TrailingInput().Status() != TrailingOpen {
			t.Errorf("empty read after the stop: %d, %v, %v", n, err, s.TrailingInput().Status())
		}
		if n, err := r.Read(make([]byte, 64)); n != 0 || err != stop || src.reads != reads {
			t.Errorf("read after the stop: %d, %v, %d input reads", n, err, src.reads-reads)
		}
		if ti := s.TrailingInput(); ti.Status() != TrailingStopped || ti.Delivered() != 0 {
			t.Errorf("%v, %d", ti.Status(), ti.Delivered())
		}
		if first.Header.Get("Content-Disposition") == "" {
			t.Errorf("the returned part lost its header")
		}
	})
}

// TestParse_trailingConcurrentSnapshots reads the trailing input while another
// goroutine takes snapshots; run under the race detector it checks the
// locking, and every snapshot is consistent.
func TestParse_trailingConcurrentSnapshots(t *testing.T) {
	prefix := trailingHead + "--B--\r\n"
	tail := strings.Repeat("x", 20000)
	s, e, err := Parse(strings.NewReader(prefix+tail), &ParseOptions{Profile: ProfileForensic})
	if err != nil {
		t.Fatal(err)
	}
	traverseRoot(t, e, modeIterate)
	r, _ := s.TrailingInputReader()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		last := int64(0)
		for i := 0; i < 100000; i++ {
			select {
			case <-done:
				return
			default:
			}
			ti := s.TrailingInput()
			if ti.Delivered() < last || ti.Delivered() > int64(len(tail)) {
				t.Errorf("snapshot delivered %d after %d", ti.Delivered(), last)
				return
			}
			last = ti.Delivered()
			s.TrailingInputReader()
		}
	}()
	got, rerr := readCapped(t, r, 13)
	close(done)
	wg.Wait()
	if got != tail || rerr != io.EOF {
		t.Errorf("read %d bytes, %v", len(got), rerr)
	}
}

// TestParse_trailingEmptyRead pins that a read of no bytes reads nothing and
// inspects nothing while the tail is open.
func TestParse_trailingEmptyRead(t *testing.T) {
	prefix := trailingHead + "--B--\r\n"
	src := &tailSource{s: prefix + trailingSecond, chunk: 1}
	s, e, _ := Parse(src, &ParseOptions{Profile: ProfileForensic})
	traverseRoot(t, e, modeIterate)
	r, _ := s.TrailingInputReader()
	reads := src.reads
	for _, p := range [][]byte{nil, {}} {
		if n, err := r.Read(p); n != 0 || err != nil {
			t.Errorf("empty read %d, %v", n, err)
		}
	}
	if src.reads != reads || s.TrailingInput().Status() != TrailingOpen || s.TrailingInput().Delivered() != 0 {
		t.Errorf("an empty read read the input or changed the tail")
	}
}

// endSource delivers s in reads of at most chunk bytes and then returns end,
// the identical value, once; every read after that is counted and fails.
type endSource struct {
	s     string
	chunk int
	end   error
	ended bool
	after int
}

var errEndSourceReread = errors.New("test source: read after its end")

func (c *endSource) Read(p []byte) (int, error) {
	if c.ended {
		c.after++
		return 0, errEndSourceReread
	}
	if c.s == "" {
		c.ended = true
		return 0, c.end
	}
	n := copy(p, c.s[:min(len(c.s), c.chunk)])
	c.s = c.s[n:]
	return n, nil
}

// TestParse_trailingCRProof pins the evidence of a trailing input whose proof
// ends at a '\r', through the public reader: a '\n' after it is part of the
// evidence, another byte follows it and is not, io.EOF itself decides it, and
// an ordinary failure — a wrapped io.EOF included — leaves it undecided, its
// bytes delivered and its error returned as itself, with no diagnostic.
func TestParse_trailingCRProof(t *testing.T) {
	prefix := "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B--\r\n"
	eio := errors.New("test source: EIO")
	wrapped := fmt.Errorf("wrapped: %w", io.EOF)
	from := "From a Mon Jan  2 03:04:05 2006"
	pad := strings.Repeat("\n", trailingEvidenceWindow-len("A: 1\r\nB:\r"))
	cases := []struct {
		tail     string
		end      error
		ev       TrailingEvidence
		from, to int
	}{
		{"A: 1\r\nB:\r", eio, TrailingEvidenceUndecided, 0, 0},
		{"A: 1\r\nB:\r", wrapped, TrailingEvidenceUndecided, 0, 0},
		{"A: 1\r\nB:\r", io.EOF, TrailingEvidenceMessageLike, 0, 9},
		{"A: 1\r\nB:\r\n", eio, TrailingEvidenceMessageLike, 0, 10},
		{"A: 1\r\nB:\r\n", io.EOF, TrailingEvidenceMessageLike, 0, 10},
		{"A: 1\r\nB:\rX", eio, TrailingEvidenceMessageLike, 0, 9},
		{"A: 1\r\nB:\n", eio, TrailingEvidenceMessageLike, 0, 9},
		{"A: 1\r\nB: ", eio, TrailingEvidenceMessageLike, 0, 9},
		{"A:\r", io.EOF, TrailingEvidenceUnrecognized, 0, 0},
		{from + "\r", eio, TrailingEvidenceUndecided, 0, 0},
		{from + "\r", io.EOF, TrailingEvidenceMessageLike, 0, len(from) + 1},
		{from + "\r\n", eio, TrailingEvidenceMessageLike, 0, len(from) + 2},
		{from + "\rX", eio, TrailingEvidenceMessageLike, 0, len(from) + 1},
		{from, io.EOF, TrailingEvidenceUnrecognized, 0, 0},
		{pad + "A: 1\r\nB:\r", eio, TrailingEvidenceUndecided, 0, 0},
		{pad + "A: 1\r\nB:\r", io.EOF, TrailingEvidenceMessageLike, len(pad), trailingEvidenceWindow},
		{pad + "A: 1\r\nB:\r\n", io.EOF, TrailingEvidenceUnrecognized, 0, 0},
		{pad + "A: 1\r\nB:\rX", eio, TrailingEvidenceUnrecognized, 0, 0},
	}
	start := int64(len(prefix))
	for i, tc := range cases {
		for _, chunk := range []int{1, 7, 4096} {
			for _, size := range []int{1, 3, 4096} {
				name := fmt.Sprintf("case %d (%s, %v)/chunk %d/read %d", i, preview(tc.tail), tc.end, chunk, size)
				src := &endSource{s: prefix + tc.tail, chunk: chunk, end: tc.end}
				s, e, err := Parse(src, &ParseOptions{Profile: ProfileForensic})
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if err := traverseRoot(t, e, modeIterate); err != nil {
					t.Fatalf("%s: traversal %v", name, err)
				}
				r, ok := s.TrailingInputReader()
				if !ok {
					t.Fatalf("%s: no reader", name)
				}
				got, rerr := readCapped(t, r, size)
				if got != tc.tail || rerr != tc.end || src.after != 0 {
					t.Fatalf("%s: read %s, %v, %d reads after the end", name, preview(got), rerr, src.after)
				}
				ti := s.TrailingInput()
				wantStatus := TrailingFailed
				if tc.end == io.EOF {
					wantStatus = TrailingEnded
				}
				if ti.Status() != wantStatus || e.ParseStop() != nil {
					t.Errorf("%s: %v, stop %v", name, ti.Status(), e.ParseStop())
				}
				ev, loc := ti.Evidence()
				if ev != tc.ev {
					t.Errorf("%s: evidence %v, want %v", name, ev, tc.ev)
					continue
				}
				td := trailingDiags(s.Diagnostics())
				if ev != TrailingEvidenceMessageLike {
					if loc != (Location{}) || len(td) != 0 {
						t.Errorf("%s: %v at %v with %d diagnostics", name, ev, loc, len(td))
					}
					continue
				}
				want := LocationRange(start+int64(tc.from), start+int64(tc.to))
				if loc != want || len(td) != 1 || td[0].Location != want {
					t.Errorf("%s: evidence at %v, %d diagnostics; want %v", name, loc, len(td), want)
				}
			}
		}
	}
}

// TestParse_trailingTerminalFirst pins that what ended the trailing input
// first stays what every later read returns: a cancellation or a stop that
// comes after an io.EOF or an input error replaces neither, and reads nothing.
func TestParse_trailingTerminalFirst(t *testing.T) {
	prefix := trailingHead + "--B--\r\n"
	eio := errors.New("test source: EIO")
	for _, end := range []error{io.EOF, eio} {
		ctx, cancel := context.WithCancel(context.Background())
		src := &endSource{s: prefix + trailingSecond, chunk: 7, end: end}
		s, e, err := ParseContext(ctx, src, &ParseOptions{Profile: ProfileForensic})
		if err != nil {
			t.Fatal(err)
		}
		if err := traverseRoot(t, e, modeIterate); err != nil {
			t.Fatal(err)
		}
		r, _ := s.TrailingInputReader()
		if got, rerr := readCapped(t, r, 5); got != trailingSecond || rerr != end {
			t.Fatalf("%v: %s, %v", end, preview(got), rerr)
		}
		before := s.TrailingInput()
		cancel()
		if stop := e.ParseStop(); stop == nil {
			t.Fatalf("%v: no stop after the cancellation", end)
		}
		for _, p := range [][]byte{make([]byte, 8), nil} {
			if n, err := r.Read(p); n != 0 || err != end {
				t.Errorf("%v: a read after the cancellation = %d, %v", end, n, err)
			}
		}
		if after := s.TrailingInput(); after != before || src.after != 0 {
			t.Errorf("%v: %+v became %+v, %d reads after the end", end, before, after, src.after)
		}
		if ev, _ := before.Evidence(); ev != TrailingEvidenceMessageLike {
			t.Errorf("%v: evidence %v", end, ev)
		}
	}
}

// TestParse_trailingRepeatedEOF asks an ended iterator for parts again: it
// keeps answering io.EOF, the reader registered first stays the one offered,
// and nothing is read.
func TestParse_trailingRepeatedEOF(t *testing.T) {
	prefix := trailingHead + "--B--\r\n"
	src := &tailSource{s: prefix + trailingSecond, chunk: 1}
	s, e, _ := Parse(src, &ParseOptions{Profile: ProfileForensic})
	mr := e.MultipartReader()
	for i := 0; i < 8; i++ {
		if _, err := mr.NextPart(); err == io.EOF {
			break
		}
	}
	r, ok := s.TrailingInputReader()
	if !ok {
		t.Fatal("no reader")
	}
	reads := src.reads
	for i := 0; i < 16; i++ {
		if _, err := mr.NextPart(); err != io.EOF {
			t.Fatalf("NextPart %d after the end = %v", i, err)
		}
	}
	if r2, _ := s.TrailingInputReader(); r2 != r || src.reads != reads {
		t.Errorf("another reader, or %d reads", src.reads-reads)
	}
	if ti := s.TrailingInput(); ti.Status() != TrailingOpen || ti.Delivered() != 0 {
		t.Errorf("%v, %d", ti.Status(), ti.Delivered())
	}
	if got, err := readCapped(t, r, 4096); got != trailingSecond || err != io.EOF {
		t.Errorf("read %s, %v", preview(got), err)
	}
}

// TestParse_trailingWriteTo pins that writing a Forensic message, before it is
// iterated, is unchanged and makes nothing available: WriteTo copies the
// multipart body verbatim, the bytes after its closing delimiter included,
// which consumes them as body and is no inspection.
func TestParse_trailingWriteTo(t *testing.T) {
	input := trailingHead + "--B--\r\n" + trailingSecond
	var out [2]strings.Builder
	for i, p := range []Profile{ProfileTolerant, ProfileForensic} {
		s, e, err := Parse(strings.NewReader(input), &ParseOptions{Profile: p})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.WriteTo(&out[i]); err != nil {
			t.Fatal(err)
		}
		if ti := s.TrailingInput(); ti != (TrailingInput{}) {
			t.Errorf("%v: %+v", p, ti)
		}
	}
	if out[0].String() != out[1].String() {
		t.Errorf("Forensic wrote %s, Tolerant %s", preview(out[1].String()), preview(out[0].String()))
	}
	if !strings.HasSuffix(out[1].String(), "--B--\r\n"+trailingSecond) {
		t.Errorf("WriteTo did not copy the bytes after the closing delimiter: %s", preview(out[1].String()))
	}
}

// TestTrailing_fixtures pins the evidence each trailing fixture is meant to
// show, so that the reference the other tests compare with cannot drift from
// it unnoticed.
func TestTrailing_fixtures(t *testing.T) {
	for name, tc := range trailingTails {
		if got := refWindowed([]byte(tc.tail), maxMboxEnvelopeContent).ev; got != tc.ev {
			t.Errorf("%s: the reference says %v, the fixture %v", name, got, tc.ev)
		}
	}
}

func TestTrailing_strings(t *testing.T) {
	for s, want := range map[TrailingStatus]string{TrailingUnknown: "unknown", TrailingNone: "none", TrailingOpen: "open",
		TrailingEnded: "ended", TrailingFailed: "failed", TrailingStopped: "stopped", 9: "TrailingStatus(9)"} {
		if s.String() != want {
			t.Errorf("%d: %q", s, s.String())
		}
	}
	for e, want := range map[TrailingEvidence]string{TrailingEvidenceUndecided: "undecided", TrailingEvidenceEmpty: "empty",
		TrailingEvidenceBlank: "blank", TrailingEvidenceUnrecognized: "unrecognized", TrailingEvidenceMessageLike: "message-like", 9: "TrailingEvidence(9)"} {
		if e.String() != want {
			t.Errorf("%d: %q", e, e.String())
		}
	}
}

// preview renders s for a failure message: its length and at most its first
// 64 bytes, so that a long epilogue does not flood the log. A fuzz failure
// keeps the whole input in its own artifact.
func preview(s string) string {
	if len(s) <= 64 {
		return fmt.Sprintf("%d bytes %q", len(s), s)
	}
	return fmt.Sprintf("%d bytes %q...", len(s), s[:64])
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// diagKeys projects diagnostics onto what the fuzz target compares: code,
// severity, path, location and field.
func diagKeys(diags []Diagnostic) []string {
	var out []string
	for _, d := range diags {
		out = append(out, fmt.Sprintf("%s %v %v %+v %q", d.Code, d.Severity, d.Path, d.Location, d.Field))
	}
	return out
}

// A trailingRun is what FuzzParse_trailing observes of one parse: the framing
// and raw bytes of the walk, and then the trailing input.
type trailingRun struct {
	noEntity    bool
	parseErr    string
	visits      []string // each entity walked: its path, and a leaf's raw body and how it ended
	walkErr     string
	before      TrailingStatus // after the walk, before the trailing input is read
	diagsBefore []string

	status    TrailingStatus // after it is read
	start     int64
	startOK   bool
	tail      string
	tailEOF   bool
	tailErr   string
	delivered int64
	evidence  TrailingEvidence
	evLoc     Location
	diags     []string
}

func (r trailingRun) summary() string {
	return fmt.Sprintf("entity %v (%s), %d visits, walk %q, %v then %v at %d (%v), tail %s eof %v %q, %d delivered, %v at %v, diagnostics %v then %v",
		!r.noEntity, r.parseErr, len(r.visits), r.walkErr, r.before, r.status, r.start, r.startOK, preview(r.tail), r.tailEOF, r.tailErr,
		r.delivered, r.evidence, r.evLoc, r.diagsBefore, r.diags)
}

// FuzzParse_trailing parses any input of at most 16 KiB under Forensic and
// Tolerant, walks it under explicit tree budgets, draining every leaf's raw,
// undecoded body rather than decoding it (decoder parity, #34, is outside this
// item), and under Forensic reads the trailing input when there is one. It
// checks the framing and the trailing input only: the walk — entities, raw
// bytes, how each read and the walk ended, diagnostics — is the same under
// both profiles and both deliveries; Tolerant offers nothing; Forensic's
// trailing input, once read, ended with io.EOF and is exactly the input from
// its start, with the reference's evidence and at most one diagnostic for it.
func FuzzParse_trailing(f *testing.F) {
	for _, tc := range trailingTails {
		f.Add([]byte(trailingHead+"--B--\r\n"+tc.tail), int64(1))
	}
	f.Add([]byte("Content-Type: multipart/mixed; boundary=B\r\n\r\n--B--"), int64(2))
	f.Add([]byte("Subject: x\r\n\r\nbody\r\n"+trailingSecond), int64(3))
	f.Add([]byte(trailingHead+"--B--\r\nA: 1\r\nB:\r"), int64(4))
	f.Fuzz(func(t *testing.T, data []byte, seed int64) {
		if len(data) > 16<<10 {
			return
		}
		input := string(data)
		limits := Limits{MaxPartCount: 64, MaxMIMEDepth: 16}
		run := func(p Profile, src io.Reader) trailingRun {
			s, e, err := Parse(src, &ParseOptions{Profile: p, Limits: limits})
			o := trailingRun{parseErr: errText(err)}
			if e == nil {
				o.noEntity = true
				o.diagsBefore, o.diags = diagKeys(s.Diagnostics()), diagKeys(s.Diagnostics())
				return o
			}
			parts := 0
			werr := e.Walk(func(path []int, part *Entity, err error) error {
				parts++ // the root and at most MaxPartCount parts
				if parts > 65 || len(path) > 16 {
					t.Fatalf("walk past the budgets: %d parts, path %v", parts, path)
				}
				visit := fmt.Sprintf("%v %q", path, errText(err))
				if !part.framesParts() {
					raw, rerr := readCapped(t, part.originalBody, 4096)
					visit += fmt.Sprintf(" %q %q", raw, errText(rerr))
				}
				o.visits = append(o.visits, visit)
				return nil
			})
			o.walkErr = errText(werr)
			o.before = s.TrailingInput().Status()
			o.diagsBefore = diagKeys(s.Diagnostics())
			if r, ok := s.TrailingInputReader(); ok {
				tail, terr := readCapped(t, r, 1+int(uint64(seed)%4096))
				o.tail, o.tailEOF, o.tailErr = tail, terr == io.EOF, errText(terr)
			}
			ti := s.TrailingInput()
			o.status = ti.Status()
			o.start, o.startOK = ti.Start()
			o.delivered = ti.Delivered()
			o.evidence, o.evLoc = ti.Evidence()
			o.diags = diagKeys(s.Diagnostics())
			return o
		}
		whole := run(ProfileForensic, strings.NewReader(input))
		chunked := run(ProfileForensic, newRandomChunkReader(input, 97, seed))
		if !reflect.DeepEqual(whole, chunked) {
			t.Fatalf("deliveries differ:\n%s\n%s", whole.summary(), chunked.summary())
		}
		tol := run(ProfileTolerant, strings.NewReader(input))
		if tol.before != TrailingUnknown || tol.status != TrailingUnknown || tol.tail != "" || !slices.Equal(tol.diags, tol.diagsBefore) {
			t.Fatalf("Tolerant offered a trailing input: %s", tol.summary())
		}
		if tol.noEntity != whole.noEntity || tol.parseErr != whole.parseErr || !slices.Equal(tol.visits, whole.visits) ||
			tol.walkErr != whole.walkErr || !slices.Equal(tol.diagsBefore, whole.diagsBefore) {
			t.Fatalf("the walk differs:\nTolerant %s\nForensic %s", tol.summary(), whole.summary())
		}

		switch whole.before {
		case TrailingUnknown, TrailingNone:
			if whole.status != whole.before || whole.startOK || whole.tail != "" || !slices.Equal(whole.diags, whole.diagsBefore) {
				t.Fatalf("%v with a trailing input: %s", whole.before, whole.summary())
			}
			return
		case TrailingOpen:
		default:
			t.Fatalf("status %v before the trailing input was read", whole.before)
		}
		if whole.status != TrailingEnded || !whole.tailEOF || !whole.startOK || whole.start > int64(len(input)) ||
			whole.tail != input[whole.start:] || whole.delivered != int64(len(whole.tail)) {
			t.Fatalf("trailing input not exactly the input from its start: %s; input from there %s", whole.summary(), preview(input[min(whole.start, int64(len(input))):]))
		}
		want := refWindowed([]byte(whole.tail), maxMboxEnvelopeContent)
		if whole.evidence != want.ev {
			t.Fatalf("evidence %v, reference %v: %s", whole.evidence, want.ev, whole.summary())
		}
		if want.ev == TrailingEvidenceMessageLike && whole.evLoc != LocationRange(whole.start+want.start, whole.start+want.end) {
			t.Fatalf("evidence at %v, reference [%d, %d) from %d", whole.evLoc, want.start, want.end, whole.start)
		}
		// Reading it appends one input.trailing_messages for message-like
		// evidence and nothing otherwise. The earlier diagnostics are compared
		// by content: a parse that had none before has a nil list, and the
		// empty prefix of the list after it is not nil.
		if len(whole.diags) < len(whole.diagsBefore) || !slices.Equal(whole.diags[:len(whole.diagsBefore)], whole.diagsBefore) {
			t.Fatalf("reading the trailing input changed earlier diagnostics: %s", whole.summary())
		}
		added := whole.diags[len(whole.diagsBefore):]
		wantAdded := 0
		if want.ev == TrailingEvidenceMessageLike {
			wantAdded = 1
		}
		if len(added) != wantAdded || (wantAdded == 1 && !strings.HasPrefix(added[0], string(CodeInputTrailingMessages)+" ")) {
			t.Fatalf("reading the trailing input recorded %v for %v evidence", added, want.ev)
		}
	})
}
