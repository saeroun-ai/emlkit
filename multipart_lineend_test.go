package message

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

// Multipart delimiter line endings under Parse (ROB-010-002, phase 2b): what
// a parse records and refuses for them, when it observes them, and what wins
// over them. The framing itself is textproto's and is pinned there.

const mpHead = "Content-Type: multipart/mixed; boundary=B\r\n\r\n"

// mpIter iterates e's parts by hand, reading each body whole, and returns the
// bodies and the error that ended the iteration.
func mpIter(t *testing.T, e *Entity) ([]string, error) {
	t.Helper()
	mr := e.MultipartReader()
	if mr == nil {
		t.Fatal("not a multipart")
	}
	var bodies []string
	for range 16 {
		p, err := mr.NextPart()
		if err != nil {
			return bodies, err
		}
		b, berr := io.ReadAll(io.LimitReader(p.Body, 1<<16))
		if berr != nil {
			t.Fatalf("body: %v", berr)
		}
		bodies = append(bodies, string(b))
	}
	t.Fatal("more than 16 parts")
	return nil, nil
}

func span(at int64) [2]int64 { return [2]int64{at, at + 1} }

type mpFacts struct {
	code     Code
	severity Severity
	path     []int
	at       [2]int64
}

func mpFactsOf(diags []Diagnostic) []mpFacts {
	var out []mpFacts
	for _, d := range diags {
		at := [2]int64{-1, -1}
		if d.Location.HasOffset {
			at = [2]int64{d.Location.Offset, d.Location.EndOffset}
		}
		out = append(out, mpFacts{d.Code, d.Severity, d.Path, at})
	}
	return out
}

// TestParse_delimiterEndings pins each layout under every profile and
// compatibility: the parts returned, how the iteration ends, and the
// diagnostics with their exact positions.
func TestParse_delimiterEndings(t *testing.T) {
	cr := mpHead + "--B\r\r\none\r--B\r\r\ntwo\r--B--\r"
	mixed := mpHead + "--B\r\n\r\none\n--B\r\n\r\ntwo\r\n--B--\r\n"
	crClose := mpHead + "--B\r\n\r\none\r\n--B--\r"
	lf := mpHead + "--B\n\r\none\n--B\n\r\ntwo\n--B--\n"
	base := int64(len(mpHead))

	for _, tc := range []struct {
		name string
		text string
		// tolerant is Tolerant's and Forensic's result, strict Strict's.
		tolBodies, strictBodies []string
		strictRefused           bool
		tolerant, strict        []mpFacts
	}{
		{name: "cr", text: cr,
			tolBodies: []string{"one", "two"}, strictBodies: nil, strictRefused: true,
			tolerant: []mpFacts{{CodeMultipartBareCR, SeverityRecovered, nil, span(base + 3)}},
			strict:   []mpFacts{{CodeMultipartBareCR, SeverityInvalidMetadata, nil, span(base + 3)}}},
		{name: "lf and crlf", text: mixed,
			tolBodies: []string{"one", "two"}, strictBodies: []string{"one", "two"},
			tolerant: []mpFacts{
				{CodeMultipartBareLF, SeverityInfo, nil, span(base + 10)},
				{CodeMultipartMixedLineEndings, SeverityInfo, nil, span(base + 10)},
			},
			strict: []mpFacts{
				{CodeMultipartBareLF, SeverityInfo, nil, span(base + 10)},
				{CodeMultipartMixedLineEndings, SeverityInfo, nil, span(base + 10)},
			}},
		{name: "lf only", text: lf,
			tolBodies: []string{"one", "two"}, strictBodies: []string{"one", "two"},
			tolerant: []mpFacts{{CodeMultipartBareLF, SeverityInfo, nil, span(base + 3)}},
			strict:   []mpFacts{{CodeMultipartBareLF, SeverityInfo, nil, span(base + 3)}}},
		{name: "a bare cr after the closing delimiter", text: crClose,
			tolBodies: []string{"one"}, strictBodies: []string{"one"}, strictRefused: true,
			tolerant: []mpFacts{
				{CodeMultipartMixedLineEndings, SeverityRecovered, nil, span(base + 17)},
				{CodeMultipartBareCR, SeverityRecovered, nil, span(base + 17)},
			},
			strict: []mpFacts{
				{CodeMultipartMixedLineEndings, SeverityRecovered, nil, span(base + 17)},
				{CodeMultipartBareCR, SeverityInvalidMetadata, nil, span(base + 17)},
			}},
	} {
		for _, profile := range []Profile{ProfileTolerant, ProfileForensic, ProfileStrict} {
			for _, compat := range []Compatibility{CompatibilityConservative, CompatibilityAngus, CompatibilityMimeKit, CompatibilityPythonEmail} {
				label := fmt.Sprintf("%s/%v/%v", tc.name, profile, compat)
				session, e, err := Parse(strings.NewReader(tc.text), &ParseOptions{Profile: profile, Compatibility: compat})
				if err != nil {
					t.Fatalf("%s: Parse() = %v", label, err)
				}
				bodies, end := mpIter(t, e)
				wantBodies, wantFacts := tc.tolBodies, tc.tolerant
				refused := profile == ProfileStrict && tc.strictRefused
				if profile == ProfileStrict {
					wantBodies, wantFacts = tc.strictBodies, tc.strict
				}
				if !reflect.DeepEqual(bodies, wantBodies) && !(len(bodies) == 0 && len(wantBodies) == 0) {
					t.Errorf("%s: bodies %q, want %q", label, bodies, wantBodies)
				}
				if got := mpFactsOf(session.Diagnostics()); !reflect.DeepEqual(got, wantFacts) {
					t.Errorf("%s: diagnostics %+v, want %+v", label, got, wantFacts)
				}
				if e.ParseStop() != nil {
					t.Errorf("%s: ParseStop() = %v, want none", label, e.ParseStop())
				}
				if !refused {
					if end != io.EOF {
						t.Errorf("%s: iteration ended with %v, want io.EOF", label, end)
					}
					continue
				}
				checkDelimiterRefusal(t, label, end, base)
			}
		}
	}
}

// checkDelimiterRefusal pins a Strict refusal of an iterator: a FatalError
// carrying a SeverityFatal multipart.bare_cr.
func checkDelimiterRefusal(t *testing.T, label string, err error, base int64) {
	t.Helper()
	var fe FatalError
	if !errors.As(err, &fe) {
		t.Fatalf("%s: iteration ended with %v, want the iterator's FatalError", label, err)
	}
	if d, ok := fe.Diagnostic(); !ok || d.Code != CodeMultipartBareCR || d.Severity != SeverityFatal || !d.Location.HasOffset || d.Location.Offset < base {
		t.Errorf("%s: refusal carries %+v, want a SeverityFatal multipart.bare_cr", label, d)
	}
}

// TestParse_delimiterEndingsStrictIteratorIsLocal pins the scope of a Strict
// refusal: the child multipart whose delimiters use a bare CR stops returning
// parts, with the same error every time, the parts already returned stay
// readable, and the enclosing multipart goes on with its next part; Walk ends
// with the refusal, and the parse is not stopped.
func TestParse_delimiterEndingsStrictIteratorIsLocal(t *testing.T) {
	text := mpHead +
		"--B\r\nContent-Type: multipart/alternative; boundary=C\r\n\r\n" +
		"--C\r\n\r\nin one\r--C\r\n\r\nin two\r\n--C--\r\n" +
		"--B\r\n\r\nlast\r\n--B--\r\n"
	crAt := int64(strings.Index(text, "in one\r") + len("in one"))

	_, e, err := Parse(strings.NewReader(text), &ParseOptions{Profile: ProfileStrict})
	if err != nil {
		t.Fatal(err)
	}
	outer := e.MultipartReader()
	child, err := outer.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	inner := child.MultipartReader()
	p, err := inner.NextPart()
	if err != nil {
		t.Fatalf("first inner part: %v", err)
	}
	// Reading the body proves the bare-CR delimiter after it; nothing is
	// observed until the next NextPart.
	b, _ := io.ReadAll(p.Body)
	if string(b) != "in one" {
		t.Fatalf("first inner body %q", b)
	}
	_, refusal := inner.NextPart()
	var fe FatalError
	if !errors.As(refusal, &fe) {
		t.Fatalf("second inner NextPart = %v, want the refusal", refusal)
	}
	if d, _ := fe.Diagnostic(); d.Code != CodeMultipartBareCR || !reflect.DeepEqual(d.Path, []int{0}) || d.Location.Offset != crAt {
		t.Errorf("refusal carries %+v, want multipart.bare_cr of [0] at %d", d, crAt)
	}
	if _, again := inner.NextPart(); again != refusal {
		t.Errorf("third inner NextPart = %v, want the identical refusal", again)
	}
	if e.ParseStop() != nil {
		t.Errorf("ParseStop() = %v, want none", e.ParseStop())
	}
	last, err := outer.NextPart()
	if err != nil {
		t.Fatalf("outer sibling: %v", err)
	}
	if b, _ := io.ReadAll(last.Body); string(b) != "last" {
		t.Errorf("outer sibling body %q", b)
	}

	_, e, _ = Parse(strings.NewReader(text), &ParseOptions{Profile: ProfileStrict})
	var visited []string
	werr := e.Walk(func(path []int, _ *Entity, _ error) error {
		visited = append(visited, fmt.Sprint(path))
		return nil
	})
	if !errors.As(werr, &fe) || !reflect.DeepEqual(visited, []string{"[]", "[0]", "[0 0]"}) {
		t.Errorf("Walk() = %v visiting %v, want the refusal after [0 0]", werr, visited)
	}
}

// TestParse_delimiterEndingsLazy pins when a delimiter a Body read proved is
// observed: by the next NextPart, not by the read — and the same whether the
// body was read or left for NextPart to skip.
func TestParse_delimiterEndingsLazy(t *testing.T) {
	text := mpHead + "--B\r\n\r\none\r--B\r\n\r\ntwo\r\n--B--\r\n"
	for _, read := range []bool{true, false} {
		session, e, _ := Parse(strings.NewReader(text), nil)
		mr := e.MultipartReader()
		p, err := mr.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		if read {
			io.ReadAll(p.Body)
			if session.Len() != 0 {
				t.Errorf("after the body read: %v, want nothing yet", codesOfDiags(session.Diagnostics()))
			}
		}
		if _, err := mr.NextPart(); err != nil {
			t.Fatalf("read %v: second NextPart = %v", read, err)
		}
		want := []Code{CodeMultipartMixedLineEndings, CodeMultipartBareCR}
		if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, want) {
			t.Errorf("read %v: diagnostics %v, want %v", read, got, want)
		}
	}
}

// TestParse_delimiterEndingsBudgetAndSink pins the diagnostic budget, a
// discarding sink, a sink that reads the session while it is told, and a sink
// that stops the parse, which then outranks the Strict refusal.
func TestParse_delimiterEndingsBudgetAndSink(t *testing.T) {
	text := mpHead + "--B\r\n\r\none\n--B\r\n\r\ntwo\r--B--\r\n"
	for _, cap := range []int64{1, 2} {
		session, e, _ := Parse(strings.NewReader(text), &ParseOptions{Profile: ProfileStrict, Limits: Limits{MaxDiagnosticCount: cap}})
		_, end := mpIter(t, e)
		var fe FatalError
		if !errors.As(end, &fe) {
			t.Fatalf("cap %d: iteration ended %v, want the refusal", cap, end)
		}
		if d, ok := fe.Diagnostic(); !ok || d.Code != CodeMultipartBareCR {
			t.Errorf("cap %d: refusal carries %+v despite the budget, want multipart.bare_cr", cap, d)
		}
		if got := int64(session.Len()); got != cap {
			t.Errorf("cap %d: %d diagnostics", cap, got)
		}
	}

	var seen []Code
	sink := DiagnosticSinkFunc(func(s *ParseSession, d Diagnostic) error {
		_ = s.Len()
		_ = s.Diagnostics()
		seen = append(seen, d.Code)
		return nil
	})
	session, e, _ := Parse(strings.NewReader(text), &ParseOptions{DiagnosticSink: sink, DiscardDeliveredDiagnostics: true})
	if _, end := mpIter(t, e); end != io.EOF {
		t.Errorf("discarding sink: iteration ended %v", end)
	}
	if want := []Code{CodeMultipartBareLF, CodeMultipartMixedLineEndings, CodeMultipartBareCR}; !reflect.DeepEqual(seen, want) || session.Diagnostics() != nil {
		t.Errorf("discarding sink saw %v, retained %v; want %v and none", seen, session.Diagnostics(), want)
	}

	errSink := errors.New("sink: enough")
	stopping := DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error { return errSink })
	session, e, _ = Parse(strings.NewReader(text), &ParseOptions{Profile: ProfileStrict, DiagnosticSink: stopping})
	_, end := mpIter(t, e)
	if !errors.Is(end, errSink) || error(session.fatalErr()) != end {
		t.Errorf("stopping sink: iteration ended %v, want the session's stop", end)
	}
}

// TestParse_delimiterEndingsPrecedence pins what outranks a Strict refusal: a
// cancelled parse reports its stop, and an input failure after a bare-CR
// delimiter is reported as itself, never as a format refusal — by the call
// that met it. The refusal is kept, and is what the next call returns.
func TestParse_delimiterEndingsPrecedence(t *testing.T) {
	text := mpHead + "--B\r\n\r\none\r--B\r\n\r\ntwo\r\n--B--\r\n"

	ctx, cancel := context.WithCancel(context.Background())
	_, e, err := ParseContext(ctx, strings.NewReader(text), &ParseOptions{Profile: ProfileStrict})
	if err != nil {
		t.Fatal(err)
	}
	mr := e.MultipartReader()
	p, _ := mr.NextPart()
	io.ReadAll(p.Body)
	cancel()
	_, end := mr.NextPart()
	if !errors.Is(end, context.Canceled) || e.ParseStop() == nil {
		t.Errorf("cancelled: NextPart = %v, want the stop", end)
	}

	cut := strings.Index(text, "one\r--B\r\n") + len("one\r--B\r\n")
	src := io.MultiReader(strings.NewReader(text[:cut]), iotestErrReader{errCut})
	session, e, _ := Parse(src, &ParseOptions{Profile: ProfileStrict})
	mr = e.MultipartReader()
	p, _ = mr.NextPart()
	io.ReadAll(p.Body)
	_, end = mr.NextPart()
	if !errors.Is(end, errCut) || IsFatal(end) {
		t.Errorf("input failure: NextPart = %v, want the input's failure", end)
	}
	base := int64(len(mpHead))
	want := []mpFacts{
		{CodeMultipartMixedLineEndings, SeverityRecovered, nil, span(base + 10)},
		{CodeMultipartBareCR, SeverityInvalidMetadata, nil, span(base + 10)},
	}
	if got := mpFactsOf(session.Diagnostics()); !reflect.DeepEqual(got, want) {
		t.Errorf("input failure: diagnostics %+v, want %+v", got, want)
	}
	_, refusal := mr.NextPart()
	checkDelimiterRefusal(t, "input failure, next call", refusal, base)
	if _, again := mr.NextPart(); again != refusal || e.ParseStop() != nil {
		t.Errorf("input failure: third NextPart = %v, ParseStop %v; want the identical refusal and no stop", again, e.ParseStop())
	}
}

// strictResumeSeq is what successive NextPart calls of one iterator returned:
// a part's body, or the error.
func strictResumeSeq(t *testing.T, mr MultipartReader, calls int) ([]string, []error) {
	t.Helper()
	var out []string
	var errs []error
	for range calls {
		p, err := mr.NextPart()
		errs = append(errs, err)
		if err != nil {
			out = append(out, "error")
			continue
		}
		b, berr := io.ReadAll(io.LimitReader(p.Body, 1<<16))
		if berr != nil {
			t.Fatalf("body: %v", berr)
		}
		out = append(out, string(b))
	}
	return out, errs
}

// TestParse_delimiterEndingsStrictResume pins a bare-CR delimiter met by a call
// that fails for another reason — here a malformed part header after it. That
// call reports its own failure; under ProfileStrict the refusal is recorded on
// it, and every later call returns the refusal instead of the part the
// low-level reader would resume to. Tolerant and Forensic go on to that part.
// A CRLF delimiter followed by the same failure owes nothing.
func TestParse_delimiterEndingsStrictResume(t *testing.T) {
	base := int64(len(mpHead))
	malformed := func(err error) bool {
		return err != nil && !IsFatal(err) && strings.Contains(err.Error(), "malformed MIME header line: malformed")
	}

	// The first delimiter ends with a bare CR (offset 3); the second, after
	// the malformed header, with CRLF (offset 18).
	crFirst := mpHead + "--B\rmalformed\r\n--B\r\n\r\ntwo\r\n--B--\r\n"
	// The first delimiter ends with LF (offset 3); the one a Body read proves
	// after "one" has LF before it (9) and a bare CR after it (13).
	crLater := mpHead + "--B\n\r\none\n--B\rmalformed\r\n--B\r\n\r\ntwo\r\n--B--\r\n"
	crlf := mpHead + "--B\r\nmalformed\r\n--B\r\n\r\ntwo\r\n--B--\r\n"

	for _, tc := range []struct {
		name    string
		text    string
		bad     int // the call that meets the malformed header
		heal    []string
		tolDiag []mpFacts
		strict  []mpFacts
		crAt    int64 // -1 when nothing is refused
	}{
		{name: "cr on the first delimiter", text: crFirst, bad: 0,
			heal: []string{"error", "two", "error"},
			tolDiag: []mpFacts{
				{CodeMultipartBareCR, SeverityRecovered, nil, span(base + 3)},
				{CodeMultipartMixedLineEndings, SeverityRecovered, nil, [2]int64{base + 18, base + 20}},
			},
			strict: []mpFacts{{CodeMultipartBareCR, SeverityInvalidMetadata, nil, span(base + 3)}},
			crAt:   base + 3},
		{name: "cr on a delimiter a body read proved", text: crLater, bad: 1,
			heal: []string{"one", "error", "two", "error"},
			tolDiag: []mpFacts{
				{CodeMultipartBareLF, SeverityInfo, nil, span(base + 3)},
				{CodeMultipartMixedLineEndings, SeverityRecovered, nil, span(base + 13)},
				{CodeMultipartBareCR, SeverityRecovered, nil, span(base + 13)},
			},
			strict: []mpFacts{
				{CodeMultipartBareLF, SeverityInfo, nil, span(base + 3)},
				{CodeMultipartMixedLineEndings, SeverityRecovered, nil, span(base + 13)},
				{CodeMultipartBareCR, SeverityInvalidMetadata, nil, span(base + 13)},
			},
			crAt: base + 13},
		{name: "crlf", text: crlf, bad: 0,
			heal: []string{"error", "two", "error"},
			crAt: -1},
	} {
		for _, profile := range []Profile{ProfileTolerant, ProfileForensic, ProfileStrict} {
			label := fmt.Sprintf("%s/%v", tc.name, profile)
			session, e, err := Parse(strings.NewReader(tc.text), &ParseOptions{Profile: profile})
			if err != nil {
				t.Fatalf("%s: Parse() = %v", label, err)
			}
			got, errs := strictResumeSeq(t, e.MultipartReader(), len(tc.heal)+1)
			// Every profile but Strict heals the part's header: the line with
			// no separator begins its body (ROB-060-010).
			healed := profile != ProfileStrict
			if healed {
				if errs[tc.bad] != nil {
					t.Errorf("%s: call %d = %v, want the healed part", label, tc.bad, errs[tc.bad])
				}
			} else if !malformed(errs[tc.bad]) {
				t.Errorf("%s: call %d = %v, want the malformed header, as itself", label, tc.bad, errs[tc.bad])
			}
			wantDiag := tc.tolDiag
			if profile == ProfileStrict {
				wantDiag = tc.strict
			}
			gotDiag := mpFactsOf(session.Diagnostics())
			if healed {
				var kept []mpFacts
				for _, d := range gotDiag {
					if d.code != CodeHeaderMissingBodySeparator {
						kept = append(kept, d)
					}
				}
				if len(kept) != len(gotDiag)-1 {
					t.Errorf("%s: diagnostics %+v, want one header.missing_body_separator", label, gotDiag)
				}
				gotDiag = kept
				// The healed part's body now ends at the line ending the
				// malformed line had, so that is where a mix of endings is
				// first seen; only the position of that note moves.
				wantDiag = append([]mpFacts(nil), wantDiag...)
				for i := range wantDiag {
					if wantDiag[i].code == CodeMultipartMixedLineEndings && i < len(gotDiag) && gotDiag[i].code == CodeMultipartMixedLineEndings {
						wantDiag[i].at = gotDiag[i].at
					}
				}
			}
			if !reflect.DeepEqual(gotDiag, wantDiag) {
				t.Errorf("%s: diagnostics %+v, want %+v", label, gotDiag, wantDiag)
			}
			if e.ParseStop() != nil {
				t.Errorf("%s: ParseStop() = %v, want none", label, e.ParseStop())
			}
			if profile != ProfileStrict || tc.crAt < 0 {
				want := append(append([]string(nil), tc.heal...), "error")
				if healed {
					want[tc.bad] = "malformed"
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s: calls %q, want %q", label, got, want)
				}
				if errs[len(errs)-2] != io.EOF || errs[len(errs)-1] != io.EOF {
					t.Errorf("%s: iteration ended %v, %v; want io.EOF twice", label, errs[len(errs)-2], errs[len(errs)-1])
				}
				continue
			}
			// Strict: the parts before, the failure, then the refusal for good.
			if want := padErrors(append([]string(nil), tc.heal[:tc.bad+1]...), len(got)); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: calls %q, want %q", label, got, want)
			}
			refusal := errs[tc.bad+1]
			checkDelimiterRefusal(t, label, refusal, base)
			var fe FatalError
			if errors.As(refusal, &fe) {
				if d, _ := fe.Diagnostic(); d.Location.Offset != tc.crAt {
					t.Errorf("%s: refusal at %d, want %d", label, d.Location.Offset, tc.crAt)
				}
			}
			for i := tc.bad + 2; i < len(errs); i++ {
				if errs[i] != refusal {
					t.Errorf("%s: call %d = %v, want the identical refusal", label, i, errs[i])
				}
			}
		}
	}
}

// padErrors extends seq with "error" to n entries.
func padErrors(seq []string, n int) []string {
	for len(seq) < n {
		seq = append(seq, "error")
	}
	return seq
}

// TestParse_delimiterEndingsStrictResumeBudgetAndStops pins the pending
// refusal of TestParse_delimiterEndingsStrictResume against the diagnostic
// budget, a discarding sink, a sink that stops the parse, a cancellation, and
// a part header budget refused on the same call.
func TestParse_delimiterEndingsStrictResumeBudgetAndStops(t *testing.T) {
	base := int64(len(mpHead))
	text := mpHead + "--B\n\r\none\n--B\rmalformed\r\n--B\r\n\r\ntwo\r\n--B--\r\n"
	strict := func(opts ParseOptions) (*ParseSession, MultipartReader, *Entity) {
		t.Helper()
		opts.Profile = ProfileStrict
		session, e, err := Parse(strings.NewReader(text), &opts)
		if err != nil {
			t.Fatal(err)
		}
		mr := e.MultipartReader()
		p, err := mr.NextPart()
		if err != nil {
			t.Fatalf("first NextPart = %v", err)
		}
		io.ReadAll(p.Body)
		return session, mr, e
	}

	// The refusal is recorded on the failing call, as the second and third
	// diagnostics; a budget that omits it does not change what is returned.
	for _, cap := range []int64{1, 2} {
		session, mr, _ := strict(ParseOptions{Limits: Limits{MaxDiagnosticCount: cap}})
		if _, err := mr.NextPart(); err == nil || IsFatal(err) {
			t.Errorf("cap %d: second NextPart = %v, want the malformed header", cap, err)
		}
		_, refusal := mr.NextPart()
		checkDelimiterRefusal(t, fmt.Sprintf("cap %d", cap), refusal, base)
		if _, again := mr.NextPart(); again != refusal {
			t.Errorf("cap %d: fourth NextPart = %v, want the identical refusal", cap, again)
		}
		if got := int64(session.Len()); got != cap {
			t.Errorf("cap %d: %d diagnostics", cap, got)
		}
	}

	var seen []Code
	sink := DiagnosticSinkFunc(func(s *ParseSession, d Diagnostic) error {
		_ = s.Len()
		seen = append(seen, d.Code)
		return nil
	})
	session, mr, _ := strict(ParseOptions{DiagnosticSink: sink, DiscardDeliveredDiagnostics: true})
	mr.NextPart()
	_, refusal := mr.NextPart()
	checkDelimiterRefusal(t, "discarding sink", refusal, base)
	if want := []Code{CodeMultipartBareLF, CodeMultipartMixedLineEndings, CodeMultipartBareCR}; !reflect.DeepEqual(seen, want) || session.Diagnostics() != nil {
		t.Errorf("discarding sink saw %v, retained %v; want %v and none", seen, session.Diagnostics(), want)
	}

	// A sink that stops the parse on the refusal's diagnostic: its stop is
	// what the failing call returns, ahead of the malformed header, and what
	// every later call returns, ahead of the refusal.
	errSink := errors.New("sink: enough")
	stopping := DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
		if d.Code == CodeMultipartBareCR {
			return errSink
		}
		return nil
	})
	session, mr, e := strict(ParseOptions{DiagnosticSink: stopping})
	for i := range 2 {
		if _, err := mr.NextPart(); !errors.Is(err, errSink) || error(session.fatalErr()) != err || e.ParseStop() == nil {
			t.Errorf("stopping sink: call %d = %v, want the session's stop", i, err)
		}
	}

	// A cancellation after the failing call outranks the refusal it left.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, ce, err := ParseContext(ctx, strings.NewReader(text), &ParseOptions{Profile: ProfileStrict})
	if err != nil {
		t.Fatal(err)
	}
	mr = ce.MultipartReader()
	p, _ := mr.NextPart()
	io.ReadAll(p.Body)
	if _, err := mr.NextPart(); err == nil || IsFatal(err) {
		t.Errorf("cancelled: second NextPart = %v, want the malformed header", err)
	}
	cancel()
	if _, end := mr.NextPart(); !errors.Is(end, context.Canceled) || ce.ParseStop() == nil {
		t.Errorf("cancelled: NextPart = %v, want the stop", end)
	}

	// A part header budget refused on the call that met the bare CR: the
	// budget refusal is returned, and stays; the bare CR is recorded first.
	budget := mpHead + "--B\rA: 1\r\nB: 2\r\n\r\ntwo\r\n--B--\r\n"
	session, be, err := Parse(strings.NewReader(budget), &ParseOptions{Profile: ProfileStrict, Limits: Limits{MaxHeaderCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	mr = be.MultipartReader()
	_, first := mr.NextPart()
	var fe FatalError
	if !errors.As(first, &fe) {
		t.Fatalf("budget: NextPart = %v, want the budget refusal", first)
	}
	if d, _ := fe.Diagnostic(); d.Code != CodeLimitHeaderCount {
		t.Errorf("budget: refusal carries %+v, want limit.header_count", d)
	}
	if _, again := mr.NextPart(); again != first {
		t.Errorf("budget: second NextPart = %v, want the identical budget refusal", again)
	}
	if got, want := codesOfDiags(session.Diagnostics()), []Code{CodeMultipartBareCR, CodeLimitHeaderCount}; !reflect.DeepEqual(got, want) {
		t.Errorf("budget: diagnostics %v, want %v", got, want)
	}
}

// cancelOnRead is a finite caller's reader that cancels ctx once it has
// handed out at bytes, and counts the reads after that.
type cancelOnRead struct {
	s            string
	pos, at, n   int
	after, calls int
	cancel       context.CancelFunc
	cancelled    bool
}

func (r *cancelOnRead) Read(p []byte) (int, error) {
	r.calls++
	if r.calls > 2*len(r.s)+100 {
		return 0, errors.New("test safety cap: read cap")
	}
	if r.cancelled {
		r.after++
	}
	if r.pos == len(r.s) {
		return 0, io.EOF
	}
	end := min(len(r.s), r.pos+r.n)
	if !r.cancelled && end > r.at {
		end = r.at
	}
	n := copy(p, r.s[r.pos:end])
	r.pos += n
	if !r.cancelled && r.pos == r.at {
		r.cancelled = true
		r.cancel()
	}
	return n, nil
}

// TestParseContext_replacedBodyFramingIsGated pins that a Body the caller
// replaced is framed through the parse's cancellation gate: once the parse is
// cancelled, neither its framing nor the framing of a multipart inside it
// reads the caller's reader again — for the message's own Body and for a
// child container's, and whether the cancellation comes before the first
// NextPart or in the middle of the walk. Body itself stays the caller's.
func TestParseContext_replacedBodyFramingIsGated(t *testing.T) {
	raw := "--O\r\nContent-Type: multipart/mixed; boundary=I\r\n\r\n--I\r\n\r\ninner" +
		strings.Repeat("a", 100) + "\r\n--I--\r\n--O\r\n\r\nlast\r\n--O--\r\n"
	walk := func(e *Entity) error {
		return e.Walk(func(_ []int, p *Entity, _ error) error {
			if p != nil && p.MultipartReader() == nil {
				io.Copy(io.Discard, io.LimitReader(p.Body, 1<<16))
			}
			return nil
		})
	}

	for _, n := range []int{1, 7, 4096} {
		for at := 0; at < len(raw); at++ {
			// The message's own Body replaced.
			ctx, cancel := context.WithCancel(context.Background())
			_, e, err := ParseContext(ctx, strings.NewReader("Content-Type: multipart/mixed; boundary=O\r\n\r\n"), nil)
			if err != nil {
				t.Fatal(err)
			}
			src := &cancelOnRead{s: raw, at: at, n: n, cancel: cancel}
			e.Body = src
			if at == 0 {
				src.cancelled = true
				cancel()
			}
			walk(e)
			if src.after != 0 {
				t.Fatalf("root replaced, chunk %d, cut %d: %d reads after the cancellation", n, at, src.after)
			}
			if e.Body != io.Reader(src) {
				t.Fatalf("root replaced: Body is no longer the caller's reader")
			}
			cancel()

			// A child container, already parsed, whose Body is replaced.
			ctx, cancel = context.WithCancel(context.Background())
			_, e, err = ParseContext(ctx, strings.NewReader(mpHead+
				"--B\r\nContent-Type: multipart/mixed; boundary=O\r\n\r\n--O\r\n\r\nx\r\n--O--\r\n--B--\r\n"), nil)
			if err != nil {
				t.Fatal(err)
			}
			child, err := e.MultipartReader().NextPart()
			if err != nil {
				t.Fatal(err)
			}
			src = &cancelOnRead{s: raw, at: at, n: n, cancel: cancel}
			child.Body = src
			if at == 0 {
				src.cancelled = true
				cancel()
			}
			walk(child)
			if src.after != 0 {
				t.Fatalf("child replaced, chunk %d, cut %d: %d reads after the cancellation", n, at, src.after)
			}
			cancel()
		}
	}
}
