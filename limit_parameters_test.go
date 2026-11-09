package message

import (
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

// Limits.MaxParameterCount and Limits.MaxParameterSegments, applied where the
// parse engine reads a Content-Type or a Content-Disposition.
//
// These two budgets differ from the rest in what a refusal costs, and most of
// what is pinned here is that difference: the entity survives, its media type
// survives, the parameters within the budget survive, the body is still parsed,
// and what was dropped is said out loud exactly once.

// paramsOnly returns every budget disabled except the two parameter budgets,
// each set to what the caller asks for. A zero leaves that one disabled.
func paramsOnly(count, segments int64) Limits {
	l := allDisabled()
	if count != 0 {
		l.MaxParameterCount = count
	}
	if segments != 0 {
		l.MaxParameterSegments = segments
	}
	return l
}

// parseContentType parses a message whose Content-Type is the given value, and
// reports the media type and parameters the parse engine ended up with.
func parseContentType(t *testing.T, value string, limits Limits) (*ParseSession, string, map[string]string) {
	t.Helper()

	text := "Content-Type: " + value + "\r\n\r\nbody\r\n"
	session, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: limits})
	if e == nil {
		t.Fatalf("Parse() = (nil, %v), want a usable entity: a parameter budget must not cost the entity", err)
	}
	if err != nil {
		t.Fatalf("Parse() = %v, want no error: a parameter budget is not fatal", err)
	}

	// The body is still parsed and still readable.
	b, readErr := io.ReadAll(e.Body)
	if readErr != nil {
		t.Fatalf("reading the body = %v", readErr)
	}
	if string(b) != "body\r\n" {
		t.Errorf("body = %q, want the body to be parsed as usual", b)
	}

	return session, e.mediaType, e.mediaParams
}

// checkParameterDiagnostic pins the one diagnostic a refused parameter list
// records.
func checkParameterDiagnostic(t *testing.T, session *ParseSession, code Code, field string, limit int64) {
	t.Helper()

	diags := session.Diagnostics()
	if len(diags) != 1 {
		t.Fatalf("Diagnostics() = %v, want exactly one %s entry", codesOfDiags(diags), code)
	}
	d := diags[0]

	if d.Code != code {
		t.Errorf("Code = %q, want %q", d.Code, code)
	}
	if d.Severity != SeverityLimitExceeded {
		t.Errorf("Severity = %v, want %v", d.Severity, SeverityLimitExceeded)
	}
	if d.Field != field {
		t.Errorf("Field = %q, want %q", d.Field, field)
	}
	if d.Action == "" {
		t.Errorf("Action is empty, want what was kept and what was dropped")
	}
	if !strings.Contains(d.Message, fmt.Sprint(limit)) {
		t.Errorf("Message = %q, want the budget %d in it", d.Message, limit)
	}
}

// TestParse_parameterCount_boundary sweeps the parameter count budget across the
// exact number of occurrences a field declares: a field at the budget is parsed
// whole, and one occurrence more keeps the media type and the parameters that
// fit.
func TestParse_parameterCount_boundary(t *testing.T) {
	const value = "text/plain; a=1; b=2; c=3; d=4"

	for _, delta := range []int64{-1, 0, 1} {
		t.Run(fmt.Sprintf("limit%+d", delta), func(t *testing.T) {
			limit := int64(4) + delta
			session, mediaType, params := parseContentType(t, value, paramsOnly(limit, 0))

			if mediaType != "text/plain" {
				t.Errorf("media type = %q, want it kept whatever the budget did", mediaType)
			}

			if delta >= 0 {
				want := map[string]string{"a": "1", "b": "2", "c": "3", "d": "4"}
				if !reflect.DeepEqual(params, want) {
					t.Errorf("params = %v, want %v", params, want)
				}
				if d := session.Diagnostics(); d != nil {
					t.Errorf("Diagnostics() = %v, want none", codesOfDiags(d))
				}
				return
			}

			want := map[string]string{"a": "1", "b": "2", "c": "3"}
			if !reflect.DeepEqual(params, want) {
				t.Errorf("params = %v, want %v: the occurrences within the budget", params, want)
			}
			checkParameterDiagnostic(t, session, CodeLimitParameterCount, "Content-Type", limit)
		})
	}
}

// TestParse_parameterSegments_boundary sweeps the RFC 2231 segment budget across
// the exact number of continuation segments one parameter declares.
//
// A parameter assembled from segments is dropped whole once the budget refuses
// the series, because what an assembler would build from the segments this scan
// stopped reading is a value that looks complete and is not. Its neighbours are
// kept.
func TestParse_parameterSegments_boundary(t *testing.T) {
	const value = `text/plain; charset=utf-8; n*0="aa"; n*1="bb"; n*2="cc"`

	for _, delta := range []int64{-1, 0, 1} {
		t.Run(fmt.Sprintf("limit%+d", delta), func(t *testing.T) {
			limit := int64(3) + delta
			session, mediaType, params := parseContentType(t, value, paramsOnly(0, limit))

			if mediaType != "text/plain" {
				t.Errorf("media type = %q, want it kept", mediaType)
			}

			if delta >= 0 {
				want := map[string]string{"charset": "utf-8", "n": "aabbcc"}
				if !reflect.DeepEqual(params, want) {
					t.Errorf("params = %v, want %v", params, want)
				}
				if d := session.Diagnostics(); d != nil {
					t.Errorf("Diagnostics() = %v, want none", codesOfDiags(d))
				}
				return
			}

			want := map[string]string{"charset": "utf-8"}
			if !reflect.DeepEqual(params, want) {
				t.Errorf("params = %v, want %v: the neighbour kept, the truncated series dropped", params, want)
			}
			if got, ok := params["n"]; ok {
				t.Errorf("params[n] = %q, want it absent rather than a prefix reported as the whole value", got)
			}
			checkParameterDiagnostic(t, session, CodeLimitParameterSegments, "Content-Type", limit)
		})
	}
}

// TestParse_parameterCount_countsRawOccurrences pins what the count counts:
// occurrences as they appear, before anything is interpreted. Duplicates count
// separately, RFC 2231 segments count separately, a semicolon inside a quoted
// string separates nothing, and an empty item declares nothing.
func TestParse_parameterCount_countsRawOccurrences(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  int64 // the smallest budget the field fits
	}{
		{"none", "text/plain", 0},
		{"one", "text/plain; a=1", 1},
		{"duplicates count separately", "text/plain; a=1; a=2; a=3", 3},
		{"segments count separately", `text/plain; a*0="x"; a*1="y"`, 2},
		{"quoted semicolons separate nothing", `text/plain; a="x;y;z"; b=2`, 2},
		{"escaped quote inside a value", `text/plain; a="x\";y"; b=2`, 2},
		{"empty items declare nothing", "text/plain;;; a=1 ;;", 1},
		{"trailing semicolon", "text/plain; a=1;", 1},
		{"unterminated quote runs to the end", `text/plain; a="x; b=2`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// How many occurrences the field has, stated directly.
			_, occurrences, refusal := boundParameters(tc.value, LimitDisabled, LimitDisabled)
			if refusal != nil {
				t.Fatalf("disabled budgets refused %q", tc.value)
			}
			if got := int64(len(occurrences)); got != tc.want {
				t.Fatalf("%q has %d occurrences, want %d", tc.value, got, tc.want)
			}

			text := "Content-Type: " + tc.value + "\r\n\r\nbody\r\n"

			// At the smallest sufficient budget nothing is reported.
			at := tc.want
			if at == 0 {
				at = 1
			}
			session, _, err := Parse(strings.NewReader(text), &ParseOptions{Limits: paramsOnly(at, 0)})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			if d := session.Diagnostics(); d != nil {
				t.Errorf("a budget of %d: Diagnostics() = %v, want none", at, codesOfDiags(d))
			}

			// One below it, the field is refused — where "one below" is a budget
			// this API can express at all. Zero is not: it selects the default.
			if tc.want < 2 {
				return
			}
			session, _, err = Parse(strings.NewReader(text), &ParseOptions{Limits: paramsOnly(tc.want-1, 0)})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeLimitParameterCount}) {
				t.Errorf("a budget of %d: Diagnostics() = %v, want the count budget", tc.want-1, got)
			}
		})
	}
}

// TestParse_parameterSegments_countsContinuationsPerBaseParameter pins what a
// segment is and what it is counted against: RFC 2231 sections of one base
// parameter, matched without regard to case, and nothing else.
func TestParse_parameterSegments_countsContinuationsPerBaseParameter(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  int64 // the smallest segment budget the field fits, 0 for no segments
	}{
		{"plain parameters are not segments", "text/plain; a=1; b=2; c=3", 0},
		{"the extended form alone is not a segment", `text/plain; a*="utf-8''x"`, 0},
		{"unencoded sections", `text/plain; a*0="x"; a*1="y"`, 2},
		{"encoded sections", `text/plain; a*0*="utf-8''x"; a*1*="y"`, 2},
		{"mixed sections", `text/plain; a*0*="utf-8''x"; a*1="y"; a*2*="z"`, 3},
		{"case does not separate a parameter from itself", `text/plain; A*0="x"; a*1="y"; A*2="z"`, 3},
		{"different parameters are counted apart", `text/plain; a*0="x"; a*1="y"; b*0="p"; b*1="q"`, 2},
		{"a nonnumeric suffix is not a section", `text/plain; a*x="1"; a*y="2"`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// How many segments the field's busiest parameter has, stated
			// directly.
			_, occurrences, refusal := boundParameters(tc.value, LimitDisabled, LimitDisabled)
			if refusal != nil {
				t.Fatalf("disabled budgets refused %q", tc.value)
			}
			per := map[string]int64{}
			var most int64
			for _, o := range occurrences {
				if o.segment {
					per[o.base]++
					most = max(most, per[o.base])
				}
			}
			if most != tc.want {
				t.Fatalf("%q has %d segments in its busiest parameter, want %d", tc.value, most, tc.want)
			}

			text := "Content-Type: " + tc.value + "\r\n\r\nbody\r\n"

			at := tc.want
			if at == 0 {
				at = 1
			}
			session, _, err := Parse(strings.NewReader(text), &ParseOptions{Limits: paramsOnly(0, at)})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			if d := session.Diagnostics(); d != nil {
				t.Errorf("a budget of %d: Diagnostics() = %v, want none", at, codesOfDiags(d))
			}

			if tc.want < 2 {
				return
			}
			session, _, err = Parse(strings.NewReader(text), &ParseOptions{Limits: paramsOnly(0, tc.want-1)})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeLimitParameterSegments}) {
				t.Errorf("a budget of %d: Diagnostics() = %v, want the segment budget", tc.want-1, got)
			}
		})
	}
}

// TestParse_parameterCount_keepsRecoverableMultipart pins the case the fallback
// exists for. A multipart container whose boundary parameter arrives before the
// budget runs out keeps its children: an oversized parameter list at the end of
// the field must not hide a tree that was never in doubt.
func TestParse_parameterCount_keepsRecoverableMultipart(t *testing.T) {
	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=B")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "; x%d=%d", i, i)
	}
	b.WriteString("\r\n\r\n")
	b.WriteString("--B\r\nContent-Type: text/plain\r\n\r\nzero\r\n")
	b.WriteString("--B\r\nContent-Type: text/plain\r\n\r\none\r\n")
	b.WriteString("--B--\r\n")

	session, got, err := parseUnder(t, b.String(), paramsOnly(4, 0))
	if err != nil {
		t.Fatalf("Parse() = %v, want a parameter budget not to be fatal", err)
	}
	if got.err != nil {
		t.Fatalf("reading the tree = %v", got.err)
	}
	want := []string{"zero", "one"}
	if !reflect.DeepEqual(got.bodies, want) {
		t.Errorf("bodies = %v, want %v: the boundary arrived within the budget", got.bodies, want)
	}
	checkParameterDiagnostic(t, session, CodeLimitParameterCount, "Content-Type", 4)
}

// TestParse_parameterCount_onePerFieldPerEntity pins that the diagnostic is
// recorded once for each affected field, at first encounter, and that each
// entity of a message is judged on its own field.
func TestParse_parameterCount_onePerFieldPerEntity(t *testing.T) {
	params := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "; x%d=%d", i, i)
		}
		return b.String()
	}

	text := "Content-Type: multipart/mixed; boundary=B" + params(10) + "\r\n\r\n" +
		"--B\r\nContent-Type: text/plain" + params(10) + "\r\n\r\nzero\r\n" +
		"--B\r\nContent-Type: text/plain; charset=utf-8\r\n\r\none\r\n" +
		"--B--\r\n"

	session, got, err := parseUnder(t, text, paramsOnly(4, 0))
	if err != nil || got.err != nil {
		t.Fatalf("Parse() = %v, reading the tree = %v", err, got.err)
	}
	if want := []string{"zero", "one"}; !reflect.DeepEqual(got.bodies, want) {
		t.Errorf("bodies = %v, want %v", got.bodies, want)
	}

	diags := session.Diagnostics()
	if len(diags) != 2 {
		t.Fatalf("Diagnostics() = %v, want one per affected field", codesOfDiags(diags))
	}
	for i, want := range [][]int{nil, {0}} {
		if diags[i].Code != CodeLimitParameterCount {
			t.Errorf("diagnostic %d Code = %q, want %q", i, diags[i].Code, CodeLimitParameterCount)
		}
		if !reflect.DeepEqual(diags[i].Path, want) {
			t.Errorf("diagnostic %d Path = %v, want %v", i, diags[i].Path, want)
		}
	}

	// Reading a part's metadata again does not record the same defect again:
	// the field was parsed once, when the entity was built.
	e := mustParse(t, text, paramsOnly(4, 0))
	if _, _, err := e.Header.ContentType(); err != nil {
		t.Fatalf("ContentType() = %v", err)
	}
	if n := session.Len(); n != 2 {
		t.Errorf("Diagnostics() grew to %d", n)
	}
}

func mustParse(t *testing.T, text string, limits Limits) *Entity {
	t.Helper()
	_, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: limits})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	return e
}

// TestParse_parameterCount_firstEncounterWins pins that a field exceeding both
// budgets at once is reported by whichever was encountered first while the field
// was scanned, and that the choice does not depend on anything but the field.
func TestParse_parameterCount_firstEncounterWins(t *testing.T) {
	// The count runs out on the third occurrence, before the segment budget
	// could run out on the fourth.
	countFirst := `text/plain; n*0="a"; n*1="b"; n*2="c"; n*3="d"`
	session, _, _ := parseContentType(t, countFirst, paramsOnly(2, 3))
	if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeLimitParameterCount}) {
		t.Errorf("Diagnostics() = %v, want the count budget, which ran out first", got)
	}

	// The segment budget runs out first when the count is generous.
	session, _, _ = parseContentType(t, countFirst, paramsOnly(10, 2))
	if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeLimitParameterSegments}) {
		t.Errorf("Diagnostics() = %v, want the segment budget, which ran out first", got)
	}
}

// TestParse_parameters_deterministicAcrossChunking pins that the same field
// yields the same metadata and the same diagnostic however the input arrives.
// Parameters are parsed from a value the header parser has already assembled, so
// this is a regression guard rather than a hard case — and it is exactly the
// kind of thing that stops being true by accident.
func TestParse_parameters_deterministicAcrossChunking(t *testing.T) {
	text := "Content-Type: multipart/mixed; boundary=B; a=1; b=2; c=3; d=4\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nzero\r\n--B--\r\n"

	var want map[string]string
	for name, src := range chunkedSources(text) {
		session, e, err := Parse(src(), &ParseOptions{Limits: paramsOnly(3, 0)})
		if err != nil {
			t.Fatalf("%s: Parse() = %v", name, err)
		}
		if want == nil {
			want = e.mediaParams
		} else if !reflect.DeepEqual(e.mediaParams, want) {
			t.Errorf("%s: params = %v, want %v", name, e.mediaParams, want)
		}
		if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeLimitParameterCount}) {
			t.Errorf("%s: Diagnostics() = %v", name, got)
		}
		if got := readTree(e); !reflect.DeepEqual(got.bodies, []string{"zero"}) {
			t.Errorf("%s: bodies = %v", name, got.bodies)
		}
	}
}

// TestParse_parameters_disabled pins that a disabled budget refuses nothing, and
// that a field far past the defaults is parsed whole under it.
func TestParse_parameters_disabled(t *testing.T) {
	var b strings.Builder
	b.WriteString("text/plain")
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "; x%d=%d", i, i)
	}

	session, mediaType, params := parseContentType(t, b.String(), paramsOnly(LimitDisabled, LimitDisabled))
	if mediaType != "text/plain" {
		t.Errorf("media type = %q", mediaType)
	}
	if len(params) != 400 {
		t.Errorf("params has %d entries, want 400", len(params))
	}
	if d := session.Diagnostics(); d != nil {
		t.Errorf("Diagnostics() = %v, want none from disabled budgets", codesOfDiags(d))
	}
}

// TestParse_parameters_profileIndependent pins that these budgets, like every
// other, are not a matter of interpretation.
func TestParse_parameters_profileIndependent(t *testing.T) {
	const value = "text/plain; a=1; b=2; c=3; d=4"

	for _, profile := range []Profile{ProfileStrict, ProfileTolerant, ProfileForensic} {
		for _, compat := range []Compatibility{
			CompatibilityConservative, CompatibilityAngus,
			CompatibilityMimeKit, CompatibilityPythonEmail,
		} {
			text := "Content-Type: " + value + "\r\n\r\nbody\r\n"
			session, e, err := Parse(strings.NewReader(text), &ParseOptions{
				Profile:       profile,
				Compatibility: compat,
				Limits:        paramsOnly(3, 0),
			})
			if err != nil || e == nil {
				t.Fatalf("%v/%v: Parse() = (%v, %v)", profile, compat, e, err)
			}
			want := map[string]string{"a": "1", "b": "2", "c": "3"}
			if !reflect.DeepEqual(e.mediaParams, want) {
				t.Errorf("%v/%v: params = %v, want %v", profile, compat, e.mediaParams, want)
			}
			if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeLimitParameterCount}) {
				t.Errorf("%v/%v: Diagnostics() = %v", profile, compat, got)
			}
		}
	}
}

// TestParse_parameters_legacyAccessorsUnbudgeted pins the documented
// distinction. Header.ContentType and Header.ContentDisposition parse whatever
// the field holds, under no budget, because a Header belongs to whoever holds it
// and may never have come from a parse — and the entry points that resolve no
// configuration keep parsing that way too.
func TestParse_parameters_legacyAccessorsUnbudgeted(t *testing.T) {
	var b strings.Builder
	b.WriteString("Content-Type: text/plain")
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "; x%d=%d", i, i)
	}
	b.WriteString("\r\n\r\nbody\r\n")
	text := b.String()

	// The legacy read path parses the whole field.
	e, err := Read(strings.NewReader(text))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	if len(e.mediaParams) != 400 {
		t.Errorf("Read(): params has %d entries, want all 400", len(e.mediaParams))
	}

	// And so does the accessor, even on a header that came from a budgeted
	// parse: the budget bounded the engine's own reading of the field, not the
	// field.
	_, parsed, err := Parse(strings.NewReader(text), &ParseOptions{Limits: paramsOnly(4, 0)})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if len(parsed.mediaParams) != 4 {
		t.Errorf("Parse(): params has %d entries, want the 4 within the budget", len(parsed.mediaParams))
	}
	_, direct, err := parsed.Header.ContentType()
	if err != nil {
		t.Fatalf("ContentType() = %v", err)
	}
	if len(direct) != 400 {
		t.Errorf("ContentType(): params has %d entries, want all 400", len(direct))
	}
}

// TestParse_parameters_contentDisposition pins that the budgets reach the other
// parameterized field the engine reads, and name it in the diagnostic.
//
// The engine reads Content-Disposition when it has to decide whether a text part
// is an attachment, which is what ReadOptions.SkipTextAttachmentDecoding asks
// for. Where the engine does not read the field, no budget applies to it, which
// is the same rule stated the other way round.
func TestParse_parameters_contentDisposition(t *testing.T) {
	var b strings.Builder
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Disposition: attachment")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "; x%d=%d", i, i)
	}
	b.WriteString("\r\n\r\nbody\r\n")
	text := b.String()

	limits := paramsOnly(4, 0)
	session, e, err := Parse(strings.NewReader(text), &ParseOptions{
		Limits:      limits,
		ReadOptions: ReadOptions{SkipTextAttachmentDecoding: true},
	})
	if err != nil || e == nil {
		t.Fatalf("Parse() = (%v, %v)", e, err)
	}
	if _, err := io.ReadAll(e.Body); err != nil {
		t.Fatalf("reading the body = %v", err)
	}
	checkParameterDiagnostic(t, session, CodeLimitParameterCount, "Content-Disposition", 4)

	// Without that option the engine never reads the field, so nothing about it
	// is budgeted or reported.
	session, _, err = Parse(strings.NewReader(text), &ParseOptions{Limits: limits})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if d := session.Diagnostics(); d != nil {
		t.Errorf("Diagnostics() = %v, want none: the engine did not read Content-Disposition", codesOfDiags(d))
	}
}

// TestParse_parameters_sinkStop pins that a sink refusing to continue while it
// is told about a parameter budget stops the parse, as it does for any other
// diagnostic, and that the diagnostic was delivered.
func TestParse_parameters_sinkStop(t *testing.T) {
	stop := fmt.Errorf("stop here")
	text := "Content-Type: text/plain; a=1; b=2; c=3\r\n\r\nbody\r\n"

	var seen []Code
	_, e, err := Parse(strings.NewReader(text), &ParseOptions{
		Limits: paramsOnly(2, 0),
		DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
			seen = append(seen, d.Code)
			return stop
		}),
	})
	if !IsFatal(err) {
		t.Fatalf("Parse() = %v, want the sink's stop", err)
	}
	if e == nil {
		t.Errorf("Parse() returned no entity, want the prefix recovered before the stop")
	}
	if !reflect.DeepEqual(seen, []Code{CodeLimitParameterCount}) {
		t.Errorf("the sink received %v, want the parameter budget diagnostic", seen)
	}
}

// TestParse_parameters_boundedWorkOnHugeField pins that the scan stops at the
// budget rather than walking a field of a hundred thousand parameters to find
// out how many there are. The proof is in what is kept, since what is kept is
// what the scan recorded.
func TestParse_parameters_boundedWorkOnHugeField(t *testing.T) {
	var b strings.Builder
	b.WriteString("text/plain")
	for i := 0; i < 100000; i++ {
		fmt.Fprintf(&b, "; x%d=%d", i, i)
	}

	session, mediaType, params := parseContentType(t, b.String(), paramsOnly(8, 0))
	if mediaType != "text/plain" {
		t.Errorf("media type = %q", mediaType)
	}
	if len(params) != 8 {
		t.Errorf("params has %d entries, want the 8 within the budget", len(params))
	}
	checkParameterDiagnostic(t, session, CodeLimitParameterCount, "Content-Type", 8)
}

// TestBoundParameters pins the scanner itself: what it treats as an occurrence,
// what base parameter each belongs to, and which occurrences are RFC 2231
// segments. These are the decisions every behavior above rests on, so they are
// pinned directly rather than only through their consequences.
func TestBoundParameters(t *testing.T) {
	type occ struct {
		text    string
		base    string
		segment bool
	}

	for _, tc := range []struct {
		name string
		raw  string
		want []occ
	}{
		{"no parameters", "text/plain", nil},
		{
			"plain",
			"text/plain; a=1; b=2",
			[]occ{{" a=1", "a", false}, {" b=2", "b", false}},
		},
		{
			"quoted semicolon",
			`text/plain; a="x;y"; b=2`,
			[]occ{{` a="x;y"`, "a", false}, {" b=2", "b", false}},
		},
		{
			"escaped quote",
			`text/plain; a="x\"; y"; b=2`,
			[]occ{{` a="x\"; y"`, "a", false}, {" b=2", "b", false}},
		},
		{
			"sections",
			`text/plain; N*0="x"; n*1*="y"; n*="z"; n*bad="w"`,
			[]occ{
				{` N*0="x"`, "n", true},
				{` n*1*="y"`, "n", true},
				{` n*="z"`, "n", false},
				{` n*bad="w"`, "n", false},
			},
		},
		{
			"an item with no value is still an occurrence",
			"text/plain; a; b=2",
			[]occ{{" a", "a", false}, {" b=2", "b", false}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, occurrences, refusal := boundParameters(tc.raw, LimitDisabled, LimitDisabled)
			if refusal != nil {
				t.Fatalf("boundParameters refused with disabled budgets: %v", refusal.code)
			}
			var got []occ
			for _, o := range occurrences {
				got = append(got, occ{tc.raw[o.start:o.end], o.base, o.segment})
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("occurrences = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRetainedParameters pins what is handed to the assembler in place of a
// field a budget refused: the media type as it was, then the occurrences that
// fit, written back exactly as they appeared, with every parameter that has a
// continuation segment among them dropped whole.
func TestRetainedParameters(t *testing.T) {
	for _, tc := range []struct {
		name  string
		raw   string
		count int64
		want  string
	}{
		{
			"prefix kept as it was",
			`text/plain; a="x;y"; b=2; c=3`,
			2,
			`text/plain; a="x;y"; b=2`,
		},
		{
			"media type survives an empty prefix",
			"multipart/mixed; a=1; b=2",
			0, // no occurrence fits
			"multipart/mixed",
		},
		{
			"a segmented parameter is dropped whole",
			`text/plain; charset=utf-8; n*0="a"; n*1="b"; z=9`,
			3,
			"text/plain; charset=utf-8",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit := tc.count
			if limit == 0 {
				// A count of zero would mean "the default", so the smallest
				// enforceable budget stands in for "nothing fits" by refusing on
				// the first occurrence.
				limit = 1
			}
			head, occurrences, refusal := boundParameters(tc.raw, limit, LimitDisabled)
			if refusal == nil {
				t.Fatalf("boundParameters accepted %q under a budget of %d", tc.raw, limit)
			}
			if tc.count == 0 {
				occurrences = nil
			}
			if got := retainedParameters(tc.raw, head, occurrences); got != tc.want {
				t.Errorf("retainedParameters = %q, want %q", got, tc.want)
			}
		})
	}
}
