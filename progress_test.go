package message

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

// Tests of the no-progress rule (ROB-001-007, progress.go). Every reader these
// tests script counts its calls and returns errCallCap once it has been called
// more often than a correct parse needs, so a regression fails rather than
// spins; no reader is endless and no guard is disabled.

var errCallCap = errors.New("test reader: call cap exceeded")

// A zeroSource delivers data in reads of at most chunk bytes and, standing at
// offset k, first returns zeros[k] reads of (0, nil). final (io.EOF when nil)
// ends the data, delivered with the last bytes when eofWith is set.
type zeroSource struct {
	data    []byte
	chunk   int
	zeros   map[int]int
	final   error
	eofWith bool
	cap     int
	onZero  func(n int) // called after each (0, nil), with how many so far

	pos, calls, zeroReads, emptyRequests int
}

func newZeroSource(data string, chunk int, zeros map[int]int) *zeroSource {
	z := make(map[int]int, len(zeros))
	total := 0
	for k, v := range zeros {
		z[k] = v
		total += v
	}
	return &zeroSource{data: []byte(data), chunk: chunk, zeros: z, cap: 2*len(data) + total + 512}
}

func (s *zeroSource) Read(p []byte) (int, error) {
	s.calls++
	if s.calls > s.cap {
		return 0, errCallCap
	}
	if len(p) == 0 {
		s.emptyRequests++
		return 0, nil
	}
	if z := s.zeros[s.pos]; z > 0 {
		s.zeros[s.pos] = z - 1
		s.zeroReads++
		if s.onZero != nil {
			s.onZero(s.zeroReads)
		}
		return 0, nil
	}
	final := s.final
	if final == nil {
		final = io.EOF
	}
	if s.pos >= len(s.data) {
		return 0, final
	}
	n := min(len(p), s.chunk, len(s.data)-s.pos)
	for k, z := range s.zeros {
		if z > 0 && k > s.pos && k < s.pos+n {
			n = k - s.pos
		}
	}
	copy(p, s.data[s.pos:s.pos+n])
	s.pos += n
	if s.pos == len(s.data) && s.eofWith {
		return n, final
	}
	return n, nil
}

// A scriptReader returns its steps in order, one per call.
type scriptStep struct {
	data string
	err  error
}

type scriptReader struct {
	steps []scriptStep
	i     int
	calls int
}

func (s *scriptReader) Read(p []byte) (int, error) {
	s.calls++
	if s.i >= len(s.steps) {
		return 0, errCallCap
	}
	st := s.steps[s.i]
	s.i++
	return copy(p, st.data), st.err
}

func zeroSteps(n int) []scriptStep { return make([]scriptStep, n) }

func progressSession() *ParseSession {
	return newParseSessionWithConfig(resolvedConfig{limits: DefaultLimits()}, diagnosticOptions{})
}

// noProgressStop checks that err is a FatalError carrying code at offset, with
// io.ErrNoProgress as its cause, and returns its diagnostic.
func noProgressStop(t *testing.T, err error, code Code) Diagnostic {
	t.Helper()
	var f FatalError
	if !errors.As(err, &f) {
		t.Fatalf("error = %v, want a FatalError", err)
	}
	d, ok := f.Diagnostic()
	if !ok || d.Code != code || d.Severity != SeverityFatal {
		t.Fatalf("FatalError diagnostic = %v (%v), want %s at SeverityFatal", d, ok, code)
	}
	if !errors.Is(err, io.ErrNoProgress) {
		t.Errorf("errors.Is(%v, io.ErrNoProgress) = false", err)
	}
	return d
}

// TestProgressReader_threshold pins the count at the input: 99 consecutive
// (0, nil) reads are passed on, the 100th is the stop, the input is asked
// exactly 100 times and never again.
func TestProgressReader_threshold(t *testing.T) {
	for _, z := range []int{0, 1, 99, 100, 101} {
		t.Run(fmt.Sprint(z), func(t *testing.T) {
			s := progressSession()
			src := &scriptReader{steps: append(zeroSteps(z), scriptStep{"ab", io.EOF})}
			g := s.watchProgress(src)
			var got []byte
			var err error
			buf := make([]byte, 8)
			for range 300 {
				var n int
				n, err = g.Read(buf)
				got = append(got, buf[:n]...)
				if err != nil {
					break
				}
			}
			if z < maxConsecutiveEmptyReads {
				if string(got) != "ab" || err != io.EOF || src.calls != z+1 {
					t.Fatalf("read %q, %v after %d calls; want \"ab\", EOF after %d", got, err, src.calls, z+1)
				}
				if s.fatalErr() != nil || s.Len() != 0 {
					t.Errorf("session stop %v, %d diagnostics; want none", s.fatalErr(), s.Len())
				}
				return
			}
			if src.calls != maxConsecutiveEmptyReads || len(got) != 0 {
				t.Fatalf("input asked %d times, read %q; want exactly 100 calls and nothing", src.calls, got)
			}
			d := noProgressStop(t, err, CodeInputNoProgress)
			if !d.Location.HasOffset || d.Location.Offset != 0 || d.Path != nil {
				t.Errorf("diagnostic at %v path %v, want offset 0 and the nil path", d.Location, d.Path)
			}
			if stop := s.fatalErr(); stop != err {
				t.Errorf("session stop %v is not the identical value %v", stop, err)
			}
			if s.inputFailure() != err {
				t.Errorf("inputFailure() = %v, want the stop", s.inputFailure())
			}
			for range 3 {
				if n, again := g.Read(buf); n != 0 || again != err {
					t.Errorf("later Read() = (%d, %v), want the identical stop", n, again)
				}
				if n, again := g.Read(nil); n != 0 || again != err {
					t.Errorf("later empty Read() = (%d, %v), want the identical stop", n, again)
				}
			}
			if src.calls != maxConsecutiveEmptyReads {
				t.Errorf("input asked %d times after the stop, want no further call", src.calls)
			}
			if got := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeInputNoProgress}) {
				t.Errorf("Diagnostics() = %v, want one input.no_progress", got)
			}
		})
	}
}

// TestProgressReader_resetsAndPassThrough pins what resets the count and what is
// passed on unchanged.
func TestProgressReader_resetsAndPassThrough(t *testing.T) {
	errX := errors.New("source failure")
	read := func(g io.Reader, max int) (string, []error) {
		var got []byte
		var errs []error
		buf := make([]byte, 8)
		for range max {
			n, err := g.Read(buf)
			got = append(got, buf[:n]...)
			if err != nil {
				errs = append(errs, err)
				if err == io.EOF || IsFatal(err) || err == errCallCap {
					break
				}
			}
		}
		return string(got), errs
	}

	t.Run("99 zeros before every byte", func(t *testing.T) {
		var steps []scriptStep
		for range 5 {
			steps = append(steps, zeroSteps(99)...)
			steps = append(steps, scriptStep{"x", nil})
		}
		steps = append(steps, zeroSteps(99)...)
		steps = append(steps, scriptStep{"", io.EOF})
		s := progressSession()
		got, errs := read(s.watchProgress(&scriptReader{steps: steps}), 1000)
		if got != "xxxxx" || len(errs) != 1 || errs[0] != io.EOF || s.Len() != 0 {
			t.Errorf("read %q, %v, %d diagnostics; want every byte and io.EOF", got, errs, s.Len())
		}
	})

	t.Run("an error ends the input, whatever came before it", func(t *testing.T) {
		// Since ROB-010-006 the input's first error is final: the zeros before
		// it are passed on, the error is returned as itself, and every later read
		// returns it again without asking the source, which is never counted as
		// a stall and records nothing.
		steps := append(zeroSteps(60), scriptStep{"", errX})
		steps = append(steps, zeroSteps(40)...)
		steps = append(steps, scriptStep{"late", nil})
		s := progressSession()
		src := &scriptReader{steps: steps}
		got, errs := read(s.watchProgress(src), 300)
		if got != "" || len(errs) != 300-60 || src.calls != 61 {
			t.Fatalf("read %q, %d errors after %d calls; want 240 repeats of the source error after 61 calls", got, len(errs), src.calls)
		}
		for _, err := range errs {
			if err != errX {
				t.Fatalf("Read() = %v, want the source error itself every time", err)
			}
		}
		if s.fatalErr() != nil || s.Len() != 0 {
			t.Errorf("session stop %v, %d diagnostics; want none", s.fatalErr(), s.Len())
		}
	})

	t.Run("bytes with an error", func(t *testing.T) {
		for _, end := range []error{io.EOF, errX} {
			s := progressSession()
			g := s.watchProgress(&scriptReader{steps: append(zeroSteps(99), scriptStep{"abc", end})})
			buf := make([]byte, 8)
			for range 99 {
				if n, err := g.Read(buf); n != 0 || err != nil {
					t.Fatalf("Read() = (%d, %v), want (0, nil)", n, err)
				}
			}
			if n, err := g.Read(buf); n != 3 || err != end || string(buf[:3]) != "abc" {
				t.Errorf("Read() = (%d, %v), want (3, %v) unchanged", n, err, end)
			}
			if s.fatalErr() != nil {
				t.Errorf("session stop %v, want none", s.fatalErr())
			}
		}
	})

	t.Run("empty requests reach nothing", func(t *testing.T) {
		s := progressSession()
		src := &scriptReader{steps: zeroSteps(99)}
		g := s.watchProgress(src)
		for range 1000 {
			if n, err := g.Read(nil); n != 0 || err != nil {
				t.Fatalf("empty Read() = (%d, %v)", n, err)
			}
		}
		if src.calls != 0 {
			t.Errorf("source asked %d times for empty requests, want 0", src.calls)
		}
	})

	t.Run("a source's own io.ErrNoProgress is ordinary", func(t *testing.T) {
		var steps []scriptStep
		for range 150 {
			steps = append(steps, scriptStep{"", io.ErrNoProgress})
		}
		s := progressSession()
		g := s.watchProgress(&scriptReader{steps: steps})
		for range 150 {
			if n, err := g.Read(make([]byte, 4)); n != 0 || err != io.ErrNoProgress {
				t.Fatalf("Read() = (%d, %v), want the source's io.ErrNoProgress itself", n, err)
			}
		}
		if s.fatalErr() != nil || s.Len() != 0 {
			t.Errorf("session stop %v, %d diagnostics; want none", s.fatalErr(), s.Len())
		}
	})

	t.Run("the offset is the bytes delivered", func(t *testing.T) {
		s := progressSession()
		g := s.watchProgress(&scriptReader{steps: append([]scriptStep{{"abcd", nil}}, zeroSteps(100)...)})
		_, errs := read(g, 200)
		if d := noProgressStop(t, errs[len(errs)-1], CodeInputNoProgress); d.Location.Offset != 4 {
			t.Errorf("diagnostic at %v, want offset 4", d.Location)
		}
	})
}

// A stallOutcome is everything a consumer of a parse got.
type stallOutcome struct {
	session  *ParseSession
	entity   *Entity
	parseErr error
	bodies   []string
	bodyErrs []error
	iterErr  error
}

// consumeStall parses r and reads it the way a caller does: the body of a leaf,
// or every part of a multipart, each body read whole.
func consumeStall(r io.Reader, opts *ParseOptions) stallOutcome {
	s, e, err := Parse(r, opts)
	out := stallOutcome{session: s, entity: e, parseErr: err}
	if e == nil {
		return out
	}
	read := func(b io.Reader) {
		got, err := io.ReadAll(b)
		out.bodies = append(out.bodies, string(got))
		out.bodyErrs = append(out.bodyErrs, err)
	}
	mr := e.MultipartReader()
	if mr == nil {
		read(e.Body)
		return out
	}
	for range 8 {
		p, err := mr.NextPart()
		if err != nil {
			out.iterErr = err
			break
		}
		read(p.Body)
	}
	return out
}

func (o stallOutcome) summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "entity=%v bodies=%q", o.entity != nil, o.bodies)
	for _, d := range o.session.Diagnostics() {
		fmt.Fprintf(&b, " %s@%v%v", d.Code, d.Location, d.Path)
	}
	return b.String()
}

func (o stallOutcome) errs() string {
	return fmt.Sprintf("%v %v %v", o.parseErr, o.bodyErrs, o.iterErr)
}

var progressProfiles = []Profile{ProfileTolerant, ProfileStrict, ProfileForensic}

// TestParse_noProgressMatrix runs runs of 0, 1, 99, 100 and 101 consecutive
// (0, nil) reads at points of the root header, root bodies of three transfer
// encodings, and the framing, headers and bodies of a multipart, across input
// chunking, EOF delivery, profiles and default or disabled limits. A run below
// 100 changes nothing; a run of 100 or more ends the parse with one
// input.no_progress at the offset the input stood at, after exactly 100 reads
// of it, keeping everything delivered before it, identically for every
// chunking.
func TestParse_noProgressMatrix(t *testing.T) {
	type message struct {
		name, text string
		offsets    []int
		leaf       bool
	}
	plain := "Subject: one\r\nContent-Type: text/plain\r\n\r\nhello body line\r\nsecond line\r\n"
	b64 := "Content-Transfer-Encoding: base64\r\n\r\nZGVjb2RlZCBiYXNlNjQgcGF5bG9hZCB0ZXh0Cg==\r\n"
	qp := "Content-Transfer-Encoding: quoted-printable\r\n\r\nsoft=\r\nbreak =3D x\r\nend\r\n"
	multi := "Content-Type: multipart/mixed; boundary=B\r\n\r\npre\r\n--B\r\nContent-Type: text/plain\r\n\r\nfirst body\r\n" +
		"--B\r\nContent-Transfer-Encoding: base64\r\n\r\nc2Vjb25k\r\n--B\r\n\r\nthird\r\n--B--\r\n"
	end := func(s string) int { return strings.Index(s, "\r\n\r\n") + 4 }
	at := func(s, sub string, d int) int { return strings.Index(s, sub) + d }
	messages := []message{
		{"plain", plain, []int{0, 5, end(plain), end(plain) + 7, len(plain)}, true},
		{"base64", b64, []int{3, end(b64), end(b64) + 6, len(b64)}, true},
		// Not inside an escape: a cut escape is kept literally, which is no
		// prefix of the decoded body.
		{"qp", qp, []int{end(qp), at(qp, "break", 2), at(qp, "end", 1), len(qp)}, true},
		{"multipart", multi, []int{0, 10, end(multi), at(multi, "first body", 3), at(multi, "Content-Transfer", 5),
			at(multi, "c2Vjb25k", 4), at(multi, "third", 2)}, false},
	}
	limitSets := map[string]Limits{"default": {}, "disabled": allDisabled()}

	for _, m := range messages {
		headerEnd := end(m.text)
		for _, eofWith := range []bool{false, true} {
			for lname, limits := range limitSets {
				for _, profile := range progressProfiles {
					opts := &ParseOptions{Profile: profile, Limits: limits}
					baseSrc := newZeroSource(m.text, 4096, nil)
					baseSrc.eofWith = eofWith
					base := consumeStall(baseSrc, opts)
					if base.session.Len() != 0 || base.session.fatalErr() != nil {
						t.Fatalf("%s: baseline has diagnostics %v", m.name, codesOfDiags(base.session.Diagnostics()))
					}
					for _, k := range m.offsets {
						if eofWith && k == len(m.text) {
							continue // never asked: io.EOF came with the last bytes
						}
						for _, z := range []int{0, 1, 99, 100, 101} {
							var first string
							for _, chunk := range []int{1, 7, 4096} {
								name := fmt.Sprintf("%s/eof=%v/%s/%v/k=%d/z=%d/chunk=%d", m.name, eofWith, lname, profile, k, z, chunk)
								src := newZeroSource(m.text, chunk, map[int]int{k: z})
								src.eofWith = eofWith
								got := consumeStall(src, opts)
								if z < maxConsecutiveEmptyReads {
									if got.summary() != base.summary() || got.errs() != base.errs() || src.zeroReads != z {
										t.Errorf("%s: got %s / %s (%d zero reads), want the baseline %s / %s",
											name, got.summary(), got.errs(), src.zeroReads, base.summary(), base.errs())
									}
									continue
								}
								checkStalledParse(t, name, got, base, src, k, headerEnd, m.leaf)
								if first == "" {
									first = got.summary()
								} else if got.summary() != first {
									t.Errorf("%s: %s differs from chunk 1's %s", name, got.summary(), first)
								}
							}
						}
					}
				}
			}
		}
	}
}

// checkStalledParse checks one parse whose input stalled at offset k.
func checkStalledParse(t *testing.T, name string, got, base stallOutcome, src *zeroSource, k, headerEnd int, leaf bool) {
	t.Helper()
	stop := got.session.fatalErr()
	if stop == nil {
		t.Errorf("%s: no stop; %s / %s", name, got.summary(), got.errs())
		return
	}
	var f FatalError
	if !errors.As(stop, &f) {
		t.Errorf("%s: stop %v is not a FatalError", name, stop)
		return
	}
	d, ok := f.Diagnostic()
	if !ok || d.Code != CodeInputNoProgress || d.Location.Offset != int64(k) || d.Path != nil || !errors.Is(stop, io.ErrNoProgress) {
		t.Errorf("%s: stop diagnostic %v, want input.no_progress at %d with the nil path", name, d, k)
	}
	if src.zeroReads != maxConsecutiveEmptyReads {
		t.Errorf("%s: %d zero reads, want exactly 100", name, src.zeroReads)
	}
	if codes := codesOfDiags(got.session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeInputNoProgress}) {
		t.Errorf("%s: Diagnostics() = %v, want one input.no_progress", name, codes)
	}
	if k < headerEnd {
		if got.entity != nil || got.parseErr != stop {
			t.Errorf("%s: Parse() = (%v, %v), want no entity and the identical stop", name, got.entity != nil, got.parseErr)
		}
		return
	}
	if got.entity == nil {
		t.Errorf("%s: no entity (%v), want the entity", name, got.parseErr)
		return
	}
	if ps := got.entity.ParseStop(); ps != stop {
		t.Errorf("%s: ParseStop() = %v, want the identical stop", name, ps)
	}
	if leaf {
		if len(got.bodyErrs) != 1 || got.bodyErrs[0] != stop {
			t.Errorf("%s: body ended with %v, want the identical stop", name, got.bodyErrs)
		}
	} else if got.iterErr != stop {
		t.Errorf("%s: iteration ended with %v, want the identical stop", name, got.iterErr)
	}
	for i, b := range got.bodies {
		if i >= len(base.bodies) || !strings.HasPrefix(base.bodies[i], b) || (i < len(got.bodies)-1 && b != base.bodies[i]) {
			t.Errorf("%s: body %d = %q, want a prefix of %q, whole before the last", name, i, b, base.bodies)
		}
	}
	calls := src.calls
	if n, err := got.entity.Body.Read(make([]byte, 16)); n != 0 || err == nil {
		t.Errorf("%s: Body.Read after the stop = (%d, %v)", name, n, err)
	}
	if src.calls != calls {
		t.Errorf("%s: the input was read again after the stop", name)
	}
}

// TestParse_noProgressPrecedence pins how a stalled input interacts with the
// other stops of a parse: a sink refusing its diagnostic, a stop published
// earlier, the diagnostic cap, sink-only delivery and cancellation.
func TestParse_noProgressPrecedence(t *testing.T) {
	const text = "Subject: x\r\n\r\nbody text"
	bodyAt := strings.Index(text, "\r\n\r\n") + 4
	sinkErr := errors.New("sink refuses")

	t.Run("a sink refusing the diagnostic", func(t *testing.T) {
		sink := DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
			if d.Code == CodeInputNoProgress {
				return sinkErr
			}
			return nil
		})
		src := newZeroSource(text, 4096, map[int]int{bodyAt + 2: 100})
		got := consumeStall(src, &ParseOptions{DiagnosticSink: sink})
		stop := got.entity.ParseStop()
		if !errors.Is(stop, sinkErr) || got.bodyErrs[0] != stop || got.bodies[0] != "bo" {
			t.Errorf("body %q ended with %v, stop %v; want the bytes before and the sink's stop", got.bodies[0], got.bodyErrs[0], stop)
		}
		if codes := codesOfDiags(got.session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeInputNoProgress}) {
			t.Errorf("Diagnostics() = %v", codes)
		}
	})

	t.Run("a stop published before the stall is adopted", func(t *testing.T) {
		unknown := "Content-Transfer-Encoding: x-unknown\r\n\r\nbody text"
		sink := DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
			if d.Code == CodeCTEUnknown {
				return sinkErr
			}
			return nil
		})
		at := strings.Index(unknown, "\r\n\r\n") + 4
		src := newZeroSource(unknown, 4096, map[int]int{at: 150})
		s, e, err := Parse(src, &ParseOptions{DiagnosticSink: sink})
		if e == nil || !errors.Is(err, sinkErr) {
			t.Fatalf("Parse() = %v", err)
		}
		_, bodyErr := io.ReadAll(e.Body)
		if bodyErr != err || e.ParseStop() != err || src.zeroReads != 100 {
			t.Errorf("body ended with %v after %d zero reads, want the sink's stop %v after 100", bodyErr, src.zeroReads, err)
		}
		if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeCTEUnknown}) {
			t.Errorf("Diagnostics() = %v, want only cte.unknown", codes)
		}
	})

	t.Run("the diagnostic cap drops the diagnostic, not the stop", func(t *testing.T) {
		src := newZeroSource(text, 4096, map[int]int{bodyAt: 100})
		got := consumeStall(src, &ParseOptions{Limits: Limits{MaxDiagnosticCount: 1}})
		noProgressStop(t, got.entity.ParseStop(), CodeInputNoProgress)
		if codes := codesOfDiags(got.session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeLimitDiagnosticCount}) {
			t.Errorf("Diagnostics() = %v, want the terminal only", codes)
		}
	})

	t.Run("delivered to a sink, not retained", func(t *testing.T) {
		var seen []Code
		sink := DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error { seen = append(seen, d.Code); return nil })
		src := newZeroSource(text, 4096, map[int]int{bodyAt: 100})
		got := consumeStall(src, &ParseOptions{DiagnosticSink: sink, DiscardDeliveredDiagnostics: true})
		if got.session.Len() != 0 || !reflect.DeepEqual(seen, []Code{CodeInputNoProgress}) {
			t.Errorf("retained %d, delivered %v", got.session.Len(), seen)
		}
	})

	t.Run("cancelled during the run", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		src := newZeroSource(text, 4096, map[int]int{bodyAt: 100})
		src.onZero = func(n int) {
			if n == 50 {
				cancel()
			}
		}
		s, e, err := ParseContext(ctx, src, nil)
		if err != nil {
			t.Fatalf("ParseContext() = %v", err)
		}
		_, bodyErr := io.ReadAll(e.Body)
		if !errors.Is(bodyErr, context.Canceled) || e.ParseStop() != bodyErr || src.zeroReads != 50 || s.Len() != 0 {
			t.Errorf("body ended with %v after %d zero reads, %d diagnostics; want the cancellation after 50 and none",
				bodyErr, src.zeroReads, s.Len())
		}
	})

	t.Run("cancelled after the stall", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		src := newZeroSource(text, 4096, map[int]int{bodyAt: 100})
		_, e, _ := ParseContext(ctx, src, nil)
		_, bodyErr := io.ReadAll(e.Body)
		stop := e.ParseStop()
		noProgressStop(t, stop, CodeInputNoProgress)
		cancel()
		if again := e.ParseStop(); again != stop || bodyErr != stop {
			t.Errorf("ParseStop() after cancel = %v, want the identical %v", again, stop)
		}
	})

	t.Run("a source error stays ordinary", func(t *testing.T) {
		errX := errors.New("source failure")
		src := newZeroSource(text, 4096, map[int]int{bodyAt: 99})
		src.final = errX
		got := consumeStall(src, nil)
		if got.bodyErrs[0] != errX || got.entity.ParseStop() != nil || got.session.Len() != 0 {
			t.Errorf("body ended with %v, stop %v, %d diagnostics; want the source error alone", got.bodyErrs[0], got.entity.ParseStop(), got.session.Len())
		}
	})
}

// TestParse_noProgressAtTheInputBudget pins the zeros a MaxInputBytes proof
// read meets: a stall there is input.no_progress, not an overflow.
func TestParse_noProgressAtTheInputBudget(t *testing.T) {
	const text = "Subject: x\r\n\r\nbody"
	limits := Limits{MaxInputBytes: int64(len(text))}
	for _, tc := range []struct {
		name  string
		data  string
		zeros int
		want  Code // "" for no stop
	}{
		{"99 zeros then EOF", text, 99, ""},
		{"100 zeros", text, 100, CodeInputNoProgress},
		{"99 zeros then a byte past the budget", text + "X", 99, CodeLimitInputBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := newZeroSource(tc.data, 4096, map[int]int{len(text): tc.zeros})
			got := consumeStall(src, &ParseOptions{Limits: limits})
			stop := got.entity.ParseStop()
			if tc.want == "" {
				if stop != nil || got.bodies[0] != "body" || got.bodyErrs[0] != nil {
					t.Errorf("body %q, %v, stop %v; want the whole body", got.bodies[0], got.bodyErrs[0], stop)
				}
				return
			}
			var f FatalError
			if !errors.As(stop, &f) {
				t.Fatalf("stop = %v", stop)
			}
			if d, _ := f.Diagnostic(); d.Code != tc.want || d.Location.Offset != int64(len(text)) || got.bodies[0] != "body" {
				t.Errorf("stop %v at %v, body %q; want %s at %d and the whole body", d.Code, d.Location, got.bodies[0], tc.want, len(text))
			}
		})
	}
}

// TestParse_noProgressThroughDecoders pins that the input's stop reaches the
// caller as itself through decoders that reword or loop: quoted-printable
// rewording a cut soft break, at the root and joined by the framing under a
// part; a CharsetReader that loops on its input inside one Read, as
// transform.Reader does; and an eager CharsetReader that meets the stall while
// the entity is built.
func TestParse_noProgressThroughDecoders(t *testing.T) {
	t.Run("quoted-printable at the root", func(t *testing.T) {
		text := "Content-Transfer-Encoding: quoted-printable\r\n\r\nfoo=\r"
		src := newZeroSource(text, 4096, map[int]int{len(text): 100})
		got := consumeStall(src, nil)
		stop := got.entity.ParseStop()
		noProgressStop(t, stop, CodeInputNoProgress)
		if got.bodyErrs[0] != stop {
			t.Errorf("body ended with %v, want the identical stop %v", got.bodyErrs[0], stop)
		}
	})

	t.Run("quoted-printable under a part", func(t *testing.T) {
		text := "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nfoo=\r"
		rest := "\n--B--\r\n"
		src := newZeroSource(text+rest, 4096, map[int]int{len(text): 100})
		got := consumeStall(src, nil)
		stop := got.entity.ParseStop()
		noProgressStop(t, stop, CodeInputNoProgress)
		if len(got.bodyErrs) != 1 || !errors.Is(got.bodyErrs[0], stop) || got.iterErr != stop {
			t.Errorf("part ended with %v, iteration with %v; want the stop under the framing's join and then itself", got.bodyErrs, got.iterErr)
		}
	})

	t.Run("a decoder looping on its input", func(t *testing.T) {
		var loops int
		withCharsetReader(t, func(_ string, in io.Reader) (io.Reader, error) {
			return readerFunc(func(p []byte) (int, error) {
				for {
					loops++
					if loops > 10000 {
						return 0, errCallCap
					}
					if n, err := in.Read(p); n > 0 || err != nil {
						return n, err
					}
				}
			}), nil
		})
		text := "Content-Type: text/plain; charset=x-loop\r\n\r\nbody"
		src := newZeroSource(text, 4096, map[int]int{len(text) - 2: 100})
		got := consumeStall(src, nil)
		stop := got.entity.ParseStop()
		noProgressStop(t, stop, CodeInputNoProgress)
		if got.bodies[0] != "bo" || got.bodyErrs[0] != stop || src.zeroReads != 100 {
			t.Errorf("body %q ended with %v after %d zero reads", got.bodies[0], got.bodyErrs[0], src.zeroReads)
		}
	})

	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("an eager CharsetReader, keeping its reader=%v", keep), func(t *testing.T) {
			withCharsetReader(t, func(_ string, in io.Reader) (io.Reader, error) {
				b, err := io.ReadAll(in)
				if err != nil && !keep {
					return nil, err
				}
				return bytes.NewReader(b), nil
			})
			text := "Content-Type: text/plain; charset=x-eager\r\n\r\nbody"
			src := newZeroSource(text, 4096, map[int]int{len(text) - 2: 100})
			s, e, err := Parse(src, nil)
			if e == nil {
				t.Fatalf("Parse() = (nil, %v), want the usable entity", err)
			}
			stop := s.fatalErr()
			noProgressStop(t, stop, CodeInputNoProgress)
			if err != stop {
				t.Errorf("Parse() error = %v, want the identical stop", err)
			}
			if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeInputNoProgress}) {
				t.Errorf("Diagnostics() = %v, want no charset.unknown", codes)
			}
			if _, bodyErr := io.ReadAll(e.Body); bodyErr != stop {
				t.Errorf("body ended with %v, want the identical stop", bodyErr)
			}
		})
	}
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// TestParse_noProgressInWriteTo pins that a verbatim WriteTo into a writer with
// ReadFrom — which copies around bufio's own count — meets the same stop.
func TestParse_noProgressInWriteTo(t *testing.T) {
	for _, text := range []string{
		"Subject: x\r\n\r\nbody text",
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\nxyz\r\n--B--\r\n",
	} {
		at := strings.Index(text, "\r\n\r\n") + 6
		src := newZeroSource(text, 4096, map[int]int{at: 100})
		_, e, err := Parse(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		werr := e.WriteTo(&buf)
		stop := e.ParseStop()
		noProgressStop(t, stop, CodeInputNoProgress)
		if werr != stop || src.zeroReads != 100 {
			t.Errorf("WriteTo() = %v after %d zero reads, want the identical stop after 100", werr, src.zeroReads)
		}
	}
}

// TestNewMultipart_overAStalledPart pins that the worker of a generated body
// whose part stalls ends with the stop rather than waiting.
func TestNewMultipart_overAStalledPart(t *testing.T) {
	text := "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain\r\n\r\nabcdef\r\n--B--\r\n"
	src := newZeroSource(text, 4096, map[int]int{strings.Index(text, "abcdef") + 3: 100})
	_, e, err := Parse(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	part, err := e.MultipartReader().NextPart()
	if err != nil {
		t.Fatal(err)
	}
	var h Header
	h.Set("Content-Type", "multipart/mixed; boundary=X")
	gen, err := NewMultipart(h, []*Entity{part})
	if err != nil {
		t.Fatal(err)
	}
	// The worker closes the pipe with its error just before it returns, so
	// ReadAll returning is the worker's last observable act.
	_, rerr := io.ReadAll(gen.Body)
	stop := e.ParseStop()
	noProgressStop(t, stop, CodeInputNoProgress)
	if !errors.Is(rerr, stop) {
		t.Errorf("generated body ended with %v, want the stop", rerr)
	}
}

// A stallingDecoder is a scripted CharsetReader result. It first passes prefix
// input bytes through; then makes slow reads that each take one input byte and
// return (0, nil), delivering those bytes afterwards; then returns zeros reads
// of (0, nil) taking nothing; then passes the rest through. discard drops the
// slow bytes instead of delivering them. swallow turns an input error into
// (0, nil) for ever after, maskEOF into io.EOF.
type stallingDecoder struct {
	in      io.Reader
	prefix  int
	slow    int
	discard bool
	zeros   int
	swallow bool
	maskEOF bool
	onZero  func(n int)

	held                   []byte
	failed                 bool
	calls, zeroCalls, ceil int
}

func (d *stallingDecoder) Read(p []byte) (int, error) {
	d.calls++
	if d.calls > d.ceil {
		return 0, errCallCap
	}
	if d.failed {
		return 0, nil
	}
	if d.prefix > 0 {
		n, err := d.in.Read(p[:min(len(p), d.prefix)])
		d.prefix -= n
		return n, d.inputErr(err)
	}
	if d.slow > 0 {
		var b [1]byte
		n, err := d.in.Read(b[:])
		if n > 0 {
			if !d.discard {
				d.held = append(d.held, b[0])
			}
			d.slow--
		}
		if err != nil {
			d.slow = 0
		}
		if n > 0 || err == nil {
			return 0, nil
		}
	}
	if len(d.held) > 0 {
		n := copy(p, d.held)
		d.held = d.held[n:]
		return n, nil
	}
	if d.zeros > 0 {
		d.zeros--
		d.zeroCalls++
		if d.onZero != nil {
			d.onZero(d.zeroCalls)
		}
		return 0, nil
	}
	n, err := d.in.Read(p)
	return n, d.inputErr(err)
}

func (d *stallingDecoder) inputErr(err error) error {
	if err == nil || err == io.EOF {
		return err
	}
	switch {
	case d.swallow:
		d.failed = true
		return nil
	case d.maskEOF:
		return io.EOF
	}
	return err
}

// withStallingDecoders installs a CharsetReader that builds a stallingDecoder
// with setup for every charset, and returns the decoders it built.
func withStallingDecoders(t *testing.T, setup func(d *stallingDecoder)) *[]*stallingDecoder {
	t.Helper()
	var built []*stallingDecoder
	withCharsetReader(t, func(_ string, in io.Reader) (io.Reader, error) {
		d := &stallingDecoder{in: in, ceil: 20000}
		setup(d)
		built = append(built, d)
		return d, nil
	})
	return &built
}

// TestParse_decoderNoProgress pins the body counter: a CharsetReader's reader
// that returns (0, nil) without taking input ends its body at the 100th such
// read, locally, while one that takes input as it returns nothing is never
// stopped for it.
func TestParse_decoderNoProgress(t *testing.T) {
	const body = "hello world"
	text := "Content-Type: text/plain; charset=x-stall\r\n\r\n" + body
	bodyAt := int64(len(text) - len(body))

	for _, k := range []int{0, 1, 99, 100, 101} {
		for _, profile := range progressProfiles {
			t.Run(fmt.Sprintf("%d zeros/%v", k, profile), func(t *testing.T) {
				built := withStallingDecoders(t, func(d *stallingDecoder) { d.zeros = k })
				got := consumeStall(strings.NewReader(text), &ParseOptions{Profile: profile})
				d := (*built)[0]
				if k < maxConsecutiveEmptyReads {
					if got.bodies[0] != body || got.bodyErrs[0] != nil || got.session.Len() != 0 {
						t.Errorf("body %q, %v, %v", got.bodies[0], got.bodyErrs[0], codesOfDiags(got.session.Diagnostics()))
					}
					return
				}
				diag := noProgressStop(t, got.bodyErrs[0], CodeBodyDecoderNoProgress)
				if diag.Location.Offset != bodyAt || d.zeroCalls != 100 || got.bodies[0] != "" {
					t.Errorf("diagnostic at %v after %d decoder zeros, body %q", diag.Location, d.zeroCalls, got.bodies[0])
				}
				if got.entity.ParseStop() != nil {
					t.Errorf("ParseStop() = %v, want nil for a local stall", got.entity.ParseStop())
				}
				calls := d.calls
				if n, err := got.entity.Body.Read(make([]byte, 4)); n != 0 || err != got.bodyErrs[0] || d.calls != calls {
					t.Errorf("later Read() = (%d, %v), decoder called %d more times", n, err, d.calls-calls)
				}
				if codes := codesOfDiags(got.session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeBodyDecoderNoProgress}) {
					t.Errorf("Diagnostics() = %v", codes)
				}
			})
		}
	}

	for _, slow := range []int{250, 1000} {
		t.Run(fmt.Sprintf("taking %d bytes while returning nothing", slow), func(t *testing.T) {
			long := strings.Repeat("x", 1200)
			withStallingDecoders(t, func(d *stallingDecoder) { d.slow = slow })
			got := consumeStall(strings.NewReader("Content-Type: text/plain; charset=x-slow\r\n\r\n"+long), &ParseOptions{Limits: allDisabled()})
			if got.bodies[0] != long || got.bodyErrs[0] != nil || got.session.Len() != 0 {
				t.Errorf("body of %d bytes, %v, %v; want all of it", len(got.bodies[0]), got.bodyErrs[0], codesOfDiags(got.session.Diagnostics()))
			}
		})
	}

	t.Run("a prefix, then a stall", func(t *testing.T) {
		withStallingDecoders(t, func(d *stallingDecoder) { d.prefix, d.zeros = 5, 1000 })
		got := consumeStall(strings.NewReader(text), nil)
		diag := noProgressStop(t, got.bodyErrs[0], CodeBodyDecoderNoProgress)
		if got.bodies[0] != "hello" || diag.Location.Offset != bodyAt+5 {
			t.Errorf("body %q, diagnostic at %v; want the prefix and offset %d", got.bodies[0], diag.Location, bodyAt+5)
		}
	})

	t.Run("swallowing the input's stall reports the input's stop", func(t *testing.T) {
		withStallingDecoders(t, func(d *stallingDecoder) { d.swallow = true })
		src := newZeroSource(text, 4096, map[int]int{int(bodyAt) + 2: 100})
		got := consumeStall(src, nil)
		stop := got.session.fatalErr()
		noProgressStop(t, stop, CodeInputNoProgress)
		if got.bodyErrs[0] != stop || got.bodies[0] != "he" {
			t.Errorf("body %q ended with %v, want the identical input stop %v", got.bodies[0], got.bodyErrs[0], stop)
		}
		if codes := codesOfDiags(got.session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeInputNoProgress}) {
			t.Errorf("Diagnostics() = %v, want the input's alone", codes)
		}
	})

	t.Run("masking the input's stall as io.EOF", func(t *testing.T) {
		withStallingDecoders(t, func(d *stallingDecoder) { d.maskEOF = true })
		src := newZeroSource(text, 4096, map[int]int{int(bodyAt) + 2: 100})
		got := consumeStall(src, nil)
		if stop := got.session.fatalErr(); stop == nil || got.bodyErrs[0] != stop {
			t.Errorf("body ended with %v, want the identical input stop %v", got.bodyErrs[0], stop)
		}
	})

	t.Run("swallowing a body budget's refusal reports the refusal", func(t *testing.T) {
		withStallingDecoders(t, func(d *stallingDecoder) { d.swallow = true })
		limits := allDisabled()
		limits.MaxEncodedBodyBytes = 3
		got := consumeStall(strings.NewReader(text), &ParseOptions{Limits: limits})
		var f FatalError
		if !errors.As(got.bodyErrs[0], &f) {
			t.Fatalf("body ended with %v", got.bodyErrs[0])
		}
		if d, _ := f.Diagnostic(); d.Code != CodeLimitEncodedBodyBytes {
			t.Errorf("body ended with %s, want the encoded budget's refusal", d.Code)
		}
		if codes := codesOfDiags(got.session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeLimitEncodedBodyBytes}) {
			t.Errorf("Diagnostics() = %v", codes)
		}
	})
}

// TestParse_decoderNoProgressKeepsSiblings pins that a stalled body is local: the
// parts after it are returned whole, by NextPart and by Walk, under every
// profile.
func TestParse_decoderNoProgressKeepsSiblings(t *testing.T) {
	text := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=x-stall\r\n\r\nstalled part\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nnext part\r\n" +
		"--B\r\nContent-Transfer-Encoding: base64\r\n\r\nbGFzdA==\r\n--B--\r\n"
	for _, profile := range progressProfiles {
		t.Run(fmt.Sprint(profile), func(t *testing.T) {
			withStallingDecoders(t, func(d *stallingDecoder) { d.zeros = 1000 })
			got := consumeStall(strings.NewReader(text), &ParseOptions{Profile: profile})
			if !reflect.DeepEqual(got.bodies, []string{"", "next part", "last"}) || got.iterErr != io.EOF {
				t.Fatalf("bodies %q, iteration %v", got.bodies, got.iterErr)
			}
			d := noProgressStop(t, got.bodyErrs[0], CodeBodyDecoderNoProgress)
			if !reflect.DeepEqual(d.Path, []int{0}) || got.bodyErrs[1] != nil || got.bodyErrs[2] != nil || got.entity.ParseStop() != nil {
				t.Errorf("path %v, later errors %v, stop %v", d.Path, got.bodyErrs[1:], got.entity.ParseStop())
			}

			_, e, err := Parse(strings.NewReader(text), &ParseOptions{Profile: profile})
			if err != nil {
				t.Fatal(err)
			}
			var walked []string
			werr := e.Walk(func(path []int, part *Entity, _ error) error {
				if len(path) == 1 {
					b, _ := io.ReadAll(part.Body)
					walked = append(walked, string(b))
				}
				return nil
			})
			if werr != nil || !reflect.DeepEqual(walked, []string{"", "next part", "last"}) {
				t.Errorf("Walk() = %v, bodies %q", werr, walked)
			}
		})
	}
}

// TestParse_decoderNoProgressBufferedInput pins that the body counter sees a
// CharsetReader's reader take its own input even when a transfer decoder
// already holds that input decoded and no wire byte moves: such a reader is
// never stopped, while one that stops taking input is stopped after exactly 100
// further empty reads. Quoted-printable is the case that found it; plain and
// base64 run alongside, at every source chunking and profile.
func TestParse_decoderNoProgressBufferedInput(t *testing.T) {
	content := strings.Repeat("x", 1000)
	var qp, b64 strings.Builder
	for i := 0; i < len(content); i += 70 {
		qp.WriteString(content[i:min(i+70, len(content))])
		qp.WriteString("=\r\n")
	}
	enc := base64.StdEncoding.EncodeToString([]byte(content))
	for i := 0; i < len(enc); i += 76 {
		b64.WriteString(enc[i:min(i+76, len(enc))])
		b64.WriteString("\r\n")
	}
	bodies := map[string]string{"": content, "quoted-printable": qp.String(), "base64": b64.String()}

	for cte, body := range bodies {
		text := "Content-Type: text/plain; charset=x-consume\r\nContent-Transfer-Encoding: " + cte + "\r\n\r\n" + body
		for _, chunk := range []int{1, 7, 4096} {
			for _, profile := range progressProfiles {
				name := fmt.Sprintf("%q/chunk=%d/%v", cte, chunk, profile)
				opts := &ParseOptions{Profile: profile}

				t.Run(name+"/consuming 750 bytes one at a time", func(t *testing.T) {
					withStallingDecoders(t, func(d *stallingDecoder) { d.slow = 750 })
					got := consumeStall(newZeroSource(text, chunk, nil), opts)
					if got.bodies[0] != content || got.bodyErrs[0] != nil || got.session.Len() != 0 {
						t.Errorf("body of %d bytes, %v, %v; want all of it and no diagnostic",
							len(got.bodies[0]), got.bodyErrs[0], codesOfDiags(got.session.Diagnostics()))
					}
				})

				for _, taken := range []int{50, 300} {
					t.Run(fmt.Sprintf("%s/taking %d bytes then stalling", name, taken), func(t *testing.T) {
						built := withStallingDecoders(t, func(d *stallingDecoder) {
							d.slow, d.discard, d.zeros = taken, true, 1000
						})
						got := consumeStall(newZeroSource(text, chunk, nil), opts)
						noProgressStop(t, got.bodyErrs[0], CodeBodyDecoderNoProgress)
						if d := (*built)[0]; d.zeroCalls != maxConsecutiveEmptyReads || got.bodies[0] != "" {
							t.Errorf("stopped after %d empty reads with body %q, want exactly 100 after the input was taken", d.zeroCalls, got.bodies[0])
						}
						if got.entity.ParseStop() != nil {
							t.Errorf("ParseStop() = %v, want a local stall", got.entity.ParseStop())
						}
					})
				}
			}
		}
	}
}
