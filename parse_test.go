package message_test

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/saeroun-ai/emlkit"
)

// These are deliberately labels no decoder will ever claim, so the warnings
// under test come from the parser having no decoder rather than from a decoder
// failing. The charset package is not imported here, which is what leaves every
// non-UTF-8 charset unknown.
const (
	unknownEncoding = "x-emlkit-nonexistent-encoding"
	unknownCharset  = "x-emlkit-nonexistent-charset"
)

// A body is decoded in two stages, and each warning's Action names what its own
// stage did instead. They are spelled out rather than referenced from the
// implementation: they are part of what this item promises a caller, so a test
// that shared the constant could not notice one changing.
//
// Neither says the body is the raw bytes of the wire, because neither stage can
// promise that: skipping the charset stage leaves whatever the transfer stage
// produced, and skipping the transfer stage leaves bytes the charset stage may
// still have converted.
const (
	transferAction = "kept the original transfer encoded bytes and passed them to any later charset stage"
	charsetAction  = "kept the bytes produced by the preceding transfer decoding stage without charset conversion"
)

// diagFacts is one diagnostic reduced to the fields this item pins.
//
// Message is deliberately absent. It is documented as human-readable and not
// part of the API contract, so pinning its text here would invent a promise the
// library does not make; checkMessages asserts only that it is present and
// says something. Cause is absent too, because comparing errors structurally is
// not what a caller does with one: the tests below reach it with errors.As and
// by identity.
type diagFacts struct {
	Code     message.Code
	Severity message.Severity
	Path     []int
	Location message.Location
	Field    string
	Action   string
}

func factsOf(d message.Diagnostic) diagFacts {
	return diagFacts{
		Code:     d.Code,
		Severity: d.Severity,
		Path:     d.Path,
		Location: d.Location,
		Field:    d.Field,
		Action:   d.Action,
	}
}

// checkMessages asserts the only thing a caller may rely on about a
// human-readable message: that there is one. Its wording is free to change.
func checkMessages(t *testing.T, diags []message.Diagnostic) {
	t.Helper()
	for i, d := range diags {
		if strings.TrimSpace(d.Message) == "" {
			t.Errorf("diagnostic %d (%v) has no human-readable message", i, d.Code)
		}
	}
}

func factsOfAll(diags []message.Diagnostic) []diagFacts {
	var out []diagFacts
	for _, d := range diags {
		out = append(out, factsOf(d))
	}
	return out
}

func codesOf(diags []message.Diagnostic) []message.Code {
	var out []message.Code
	for _, d := range diags {
		out = append(out, d.Code)
	}
	return out
}

// wantEncodingFacts and wantCharsetFacts are the two diagnostics as they must
// appear for an entity at path, whose header block starts at headerOffset.
func wantEncodingFacts(path []int, headerOffset int64) diagFacts {
	return diagFacts{
		Code:     message.CodeCTEUnknown,
		Severity: message.SeverityUnsupported,
		Path:     path,
		Location: message.LocationAt(headerOffset),
		Field:    "Content-Transfer-Encoding",
		Action:   transferAction,
	}
}

func wantCharsetFacts(path []int, headerOffset int64) diagFacts {
	return diagFacts{
		Code:     message.CodeCharsetUnknown,
		Severity: message.SeverityUnsupported,
		Path:     path,
		Location: message.LocationAt(headerOffset),
		Field:    "Content-Type",
		Action:   charsetAction,
	}
}

// readBody drains an entity's body, which for an undecoded one is the raw
// bytes of the input.
func readBody(t *testing.T, e *message.Entity) string {
	t.Helper()
	b, err := io.ReadAll(e.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return string(b)
}

// offsetOf returns the position of a unique marker in a fixture, so that an
// expected offset is derived from the message rather than counted out by hand.
func offsetOf(t *testing.T, text, marker string) int64 {
	t.Helper()
	i := strings.Index(text, marker)
	if i < 0 {
		t.Fatalf("fixture does not contain %q", marker)
	}
	if strings.Contains(text[i+1:], marker) {
		t.Fatalf("marker %q is not unique in the fixture", marker)
	}
	return int64(i)
}

// rootEncodingMessage declares a transfer encoding nothing can decode, at the
// top level of the message.
const rootEncodingMessage = "Content-Type: text/plain\r\n" +
	"Content-Transfer-Encoding: " + unknownEncoding + "\r\n" +
	"\r\n" +
	"raw body bytes\r\n"

// rootBothMessage declares a transfer encoding and a charset that are both
// unknown, on one entity.
const rootBothMessage = "Content-Type: text/plain; charset=" + unknownCharset + "\r\n" +
	"Content-Transfer-Encoding: " + unknownEncoding + "\r\n" +
	"\r\n" +
	"raw body bytes\r\n"

// nestedWarningMessage puts both unknowns on a part two levels down, so that a
// diagnostic recorded while the caller iterates has to name a path the parse
// call never saw.
const nestedWarningMessage = "Content-Type: multipart/mixed; boundary=BB\r\n" +
	"\r\n" +
	"--BB\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"fine\r\n" +
	"--BB\r\n" +
	"Content-Type: multipart/alternative; boundary=CC\r\n" +
	"\r\n" +
	"--CC\r\n" +
	"Content-Type: text/plain; charset=" + unknownCharset + "\r\n" +
	"Content-Transfer-Encoding: " + unknownEncoding + "\r\n" +
	"\r\n" +
	"deep\r\n" +
	"--CC--\r\n" +
	"--BB--\r\n"

// nestedWarningMarker is the first line of the header block of the part at path
// {1, 0}, and is unique in nestedWarningMessage.
const nestedWarningMarker = "Content-Type: text/plain; charset="

// siblingWarningMessage puts a warning on the first of two parts, so a test can
// ask what happens to the sibling after a sink stops the parse.
const siblingWarningMessage = "Content-Type: multipart/mixed; boundary=BB\r\n" +
	"\r\n" +
	"--BB\r\n" +
	"Content-Transfer-Encoding: " + unknownEncoding + "\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"first\r\n" +
	"--BB\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"second\r\n" +
	"--BB--\r\n"

// walkAll consumes the whole tree, which is what makes the parser reach the
// parts whose defects are only discovered lazily. Unknown encodings and
// charsets are warnings, so they do not stop the walk.
func walkAll(t *testing.T, e *message.Entity) {
	t.Helper()
	err := e.Walk(func(path []int, part *message.Entity, err error) error {
		if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
}

// TestParse_rootUnknownEncoding pins the whole contract for one warning on the
// root entity: the exact code, severity, path, offset, field, action and cause,
// the legacy error, and a body that still yields the raw bytes.
func TestParse_rootUnknownEncoding(t *testing.T) {
	session, e, err := message.Parse(strings.NewReader(rootEncodingMessage), nil)

	if session == nil {
		t.Fatal("Parse returned a nil session")
	}
	if e == nil {
		t.Fatal("Parse returned no entity for a warning")
	}
	if !message.IsUnknownEncoding(err) {
		t.Fatalf("Parse error = %v, want one that verifies IsUnknownEncoding", err)
	}
	if message.IsFatal(err) {
		t.Errorf("Parse error = %v, want a warning rather than a fatal error", err)
	}
	if got, want := readBody(t, e), "raw body bytes\r\n"; got != want {
		t.Errorf("body = %q, want the raw undecoded %q", got, want)
	}

	diags := session.Diagnostics()
	if len(diags) != 1 {
		t.Fatalf("Diagnostics() = %v, want exactly one", codesOf(diags))
	}
	// The root's header block starts at the first byte of the input.
	want := wantEncodingFacts(nil, 0)
	if got := factsOf(diags[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("diagnostic =\n%+v\nwant\n%+v", got, want)
	}
	checkMessages(t, diags)

	// The legacy warning is the diagnostic's cause, so a caller reaches the
	// same error through either.
	if diags[0].Cause != err {
		t.Errorf("Cause = %v, want the identical returned warning %v", diags[0].Cause, err)
	}
	var unknown message.UnknownEncodingError
	if !errors.As(diags[0].Cause, &unknown) {
		t.Errorf("errors.As(Cause, &UnknownEncodingError{}) = false, want true")
	}
}

// TestParse_rootUnknownCharset is the same for the charset warning, whose
// affected field is the Content-Type the charset parameter came from.
func TestParse_rootUnknownCharset(t *testing.T) {
	const text = "Content-Type: text/plain; charset=" + unknownCharset + "\r\n" +
		"\r\n" +
		"raw body bytes\r\n"

	session, e, err := message.Parse(strings.NewReader(text), nil)
	if e == nil {
		t.Fatal("Parse returned no entity for a warning")
	}
	if !message.IsUnknownCharset(err) {
		t.Fatalf("Parse error = %v, want one that verifies IsUnknownCharset", err)
	}
	if got, want := readBody(t, e), "raw body bytes\r\n"; got != want {
		t.Errorf("body = %q, want the raw undecoded %q", got, want)
	}

	diags := session.Diagnostics()
	if len(diags) != 1 {
		t.Fatalf("Diagnostics() = %v, want exactly one", codesOf(diags))
	}
	want := wantCharsetFacts(nil, 0)
	if got := factsOf(diags[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("diagnostic =\n%+v\nwant\n%+v", got, want)
	}
	checkMessages(t, diags)
	if diags[0].Cause != err {
		t.Errorf("Cause = %v, want the identical returned warning %v", diags[0].Cause, err)
	}
	var unknown message.UnknownCharsetError
	if !errors.As(diags[0].Cause, &unknown) {
		t.Errorf("errors.As(Cause, &UnknownCharsetError{}) = false, want true")
	}
}

// TestParse_bothWarningsOnOneEntity pins that neither warning hides the other.
// One entity can declare an encoding and a charset that are both unknown, and
// the returned error can only name one of them, so the session has to hold both
// — in the order they were discovered.
func TestParse_bothWarningsOnOneEntity(t *testing.T) {
	session, e, err := message.Parse(strings.NewReader(rootBothMessage), nil)
	if e == nil {
		t.Fatal("Parse returned no entity for a warning")
	}
	if got, want := readBody(t, e), "raw body bytes\r\n"; got != want {
		t.Errorf("body = %q, want the raw undecoded %q", got, want)
	}

	// The returned error keeps its long-standing precedence: the charset is
	// examined last, so it is what the single returned warning reports.
	if !message.IsUnknownCharset(err) {
		t.Errorf("Parse error = %v, want the charset warning", err)
	}
	if message.IsUnknownEncoding(err) {
		t.Errorf("Parse error = %v, want it not to report the encoding as well", err)
	}

	diags := session.Diagnostics()
	want := []diagFacts{wantEncodingFacts(nil, 0), wantCharsetFacts(nil, 0)}
	if got := factsOfAll(diags); !reflect.DeepEqual(got, want) {
		t.Fatalf("diagnostics =\n%+v\nwant\n%+v", got, want)
	}
	checkMessages(t, diags)

	// The warning the returned error does not carry is not lost: it is the
	// cause of the diagnostic that does.
	var unknownEnc message.UnknownEncodingError
	if !errors.As(diags[0].Cause, &unknownEnc) {
		t.Errorf("the encoding warning is not reachable through the first diagnostic")
	}
	if diags[1].Cause != err {
		t.Errorf("second Cause = %v, want the identical returned warning %v", diags[1].Cause, err)
	}
}

// TestParse_nestedWarnings pins that a defect discovered while the caller walks
// the tree — long after Parse returned — is attributed to the part it belongs
// to, with that part's absolute position in the input.
func TestParse_nestedWarnings(t *testing.T) {
	session, e, err := message.Parse(strings.NewReader(nestedWarningMessage), nil)
	if err != nil {
		t.Fatalf("Parse() = %v, want nil: the root itself is sound", err)
	}
	if got := session.Diagnostics(); len(got) != 0 {
		t.Fatalf("Diagnostics() = %v before the tree was walked, want none", codesOf(got))
	}

	walkAll(t, e)

	at := offsetOf(t, nestedWarningMessage, nestedWarningMarker)
	want := []diagFacts{
		wantEncodingFacts([]int{1, 0}, at),
		wantCharsetFacts([]int{1, 0}, at),
	}
	if got := factsOfAll(session.Diagnostics()); !reflect.DeepEqual(got, want) {
		t.Errorf("diagnostics =\n%+v\nwant\n%+v", got, want)
	}
	checkMessages(t, session.Diagnostics())
}

// TestParse_nestedWarningPartIsUsable pins that the warning still accompanies a
// usable part when the caller iterates by hand rather than walking.
func TestParse_nestedWarningPartIsUsable(t *testing.T) {
	session, e, err := message.Parse(strings.NewReader(siblingWarningMessage), nil)
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	mr := e.MultipartReader()
	if mr == nil {
		t.Fatal("MultipartReader() = nil")
	}

	first, err := mr.NextPart()
	if first == nil {
		t.Fatal("NextPart returned no part for a warning")
	}
	if !message.IsUnknownEncoding(err) {
		t.Fatalf("NextPart error = %v, want one that verifies IsUnknownEncoding", err)
	}
	if got, want := readBody(t, first), "first"; got != want {
		t.Errorf("body = %q, want the raw undecoded %q", got, want)
	}

	second, err := mr.NextPart()
	if err != nil {
		t.Fatalf("NextPart for the sibling = %v, want nil", err)
	}
	if got, want := readBody(t, second), "second"; got != want {
		t.Errorf("sibling body = %q, want %q", got, want)
	}

	at := offsetOf(t, siblingWarningMessage, "Content-Transfer-Encoding: ")
	want := []diagFacts{wantEncodingFacts([]int{0}, at)}
	if got := factsOfAll(session.Diagnostics()); !reflect.DeepEqual(got, want) {
		t.Errorf("diagnostics =\n%+v\nwant\n%+v", got, want)
	}
	checkMessages(t, session.Diagnostics())
}

// TestParse_sinkReceivesWarnings pins that both warnings reach a sink in
// discovery order, and that a session told not to retain what it delivers keeps
// nothing.
func TestParse_sinkReceivesWarnings(t *testing.T) {
	for _, discard := range []bool{false, true} {
		t.Run(fmt.Sprintf("discard=%v", discard), func(t *testing.T) {
			var (
				delivered []message.Diagnostic
				sessions  []*message.ParseSession
			)
			opts := &message.ParseOptions{
				DiagnosticSink: message.DiagnosticSinkFunc(func(s *message.ParseSession, d message.Diagnostic) error {
					sessions = append(sessions, s)
					delivered = append(delivered, d)
					return nil
				}),
				DiscardDeliveredDiagnostics: discard,
			}

			session, e, err := message.Parse(strings.NewReader(rootBothMessage), opts)
			if !message.IsUnknownCharset(err) {
				t.Fatalf("Parse error = %v, want the charset warning", err)
			}
			if got, want := readBody(t, e), "raw body bytes\r\n"; got != want {
				t.Errorf("body = %q, want the raw undecoded %q", got, want)
			}

			want := []diagFacts{wantEncodingFacts(nil, 0), wantCharsetFacts(nil, 0)}
			if got := factsOfAll(delivered); !reflect.DeepEqual(got, want) {
				t.Errorf("sink received\n%+v\nwant\n%+v", got, want)
			}
			checkMessages(t, delivered)
			for i, s := range sessions {
				if s != session {
					t.Errorf("delivery %d named session %p, want %p", i, s, session)
				}
			}

			if discard {
				if got := session.Diagnostics(); got != nil {
					t.Errorf("Diagnostics() = %v, want nil without retention", codesOf(got))
				}
				if got := session.Len(); got != 0 {
					t.Errorf("Len() = %d, want 0 without retention", got)
				}
			} else if got := factsOfAll(session.Diagnostics()); !reflect.DeepEqual(got, want) {
				t.Errorf("Diagnostics() =\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

// TestParse_sinkWithoutRetentionStillDelivers pins that asking for no retention
// without a sink does not drop diagnostics: they are never discarded without
// being delivered somewhere.
func TestParse_sinkWithoutRetentionStillDelivers(t *testing.T) {
	session, _, err := message.Parse(strings.NewReader(rootEncodingMessage),
		&message.ParseOptions{DiscardDeliveredDiagnostics: true})
	if !message.IsUnknownEncoding(err) {
		t.Fatalf("Parse error = %v, want the encoding warning", err)
	}
	if got := codesOf(session.Diagnostics()); len(got) != 1 {
		t.Errorf("Diagnostics() = %v, want the diagnostic retained when no sink takes delivery", got)
	}
}

// TestParse_sinkStopReturnsUsableResult pins the sink-failure contract: the
// session comes back, so does the usable entity built before the stop, and the
// error is a fatal error wrapping what the sink returned. The stop is observed
// before any further semantic work, so the charset the same entity also cannot
// decode is never examined and never diagnosed.
func TestParse_sinkStopReturnsUsableResult(t *testing.T) {
	stop := errors.New("caller stopped the parse")
	calls := 0

	session, e, err := message.Parse(strings.NewReader(rootBothMessage), &message.ParseOptions{
		DiagnosticSink: message.DiagnosticSinkFunc(func(*message.ParseSession, message.Diagnostic) error {
			calls++
			return stop
		}),
	})

	if session == nil {
		t.Fatal("Parse returned a nil session")
	}
	if e == nil {
		t.Fatal("Parse returned no entity, want the prefix recovered before the stop")
	}
	if got, want := readBody(t, e), "raw body bytes\r\n"; got != want {
		t.Errorf("body = %q, want the raw undecoded %q", got, want)
	}

	if !message.IsFatal(err) {
		t.Fatalf("Parse error = %v, want a fatal error", err)
	}
	if !errors.Is(err, stop) {
		t.Errorf("errors.Is(%v, stop) = false, want true", err)
	}

	var fatal message.FatalError
	if !errors.As(err, &fatal) {
		t.Fatalf("errors.As(%v, &FatalError) = false, want true", err)
	}
	// A sink refusing to continue is not a defect of the message, so no
	// diagnostic is invented to describe it.
	if d, ok := fatal.Diagnostic(); ok {
		t.Errorf("fatal error carries diagnostic %v, want none for a callback failure", d)
	}

	if calls != 1 {
		t.Errorf("sink was called %d times, want 1: a failed sink is not called again", calls)
	}
	if got, want := codesOf(session.Diagnostics()), []message.Code{message.CodeCTEUnknown}; !reflect.DeepEqual(got, want) {
		t.Errorf("Diagnostics() = %v, want %v: the stop is observed before the charset is examined", got, want)
	}
}

// TestParse_sinkStopEndsLaterScopes pins that the stop is session-wide: the part
// being built when the sink failed is still returned, and the iterator that
// would produce its sibling recovers nothing and reports the identical sticky
// error.
func TestParse_sinkStopEndsLaterScopes(t *testing.T) {
	stop := errors.New("caller stopped the parse")

	session, e, err := message.Parse(strings.NewReader(siblingWarningMessage), &message.ParseOptions{
		DiagnosticSink: message.DiagnosticSinkFunc(func(*message.ParseSession, message.Diagnostic) error {
			return stop
		}),
	})
	if err != nil {
		t.Fatalf("Parse() = %v, want nil: the root is sound and nothing has been diagnosed yet", err)
	}

	mr := e.MultipartReader()
	if mr == nil {
		t.Fatal("MultipartReader() = nil")
	}

	first, firstErr := mr.NextPart()
	if first == nil {
		t.Fatal("NextPart returned no part, want the one recovered before the stop")
	}
	if !message.IsFatal(firstErr) || !errors.Is(firstErr, stop) {
		t.Fatalf("NextPart error = %v, want a fatal error wrapping the sink's own", firstErr)
	}
	if got, want := readBody(t, first), "first"; got != want {
		t.Errorf("recovered part body = %q, want %q", got, want)
	}

	second, secondErr := mr.NextPart()
	if second != nil {
		t.Errorf("NextPart after the stop returned a part, want nothing recovered")
	}
	if secondErr != firstErr {
		t.Errorf("NextPart after the stop = %v, want the identical sticky error %v", secondErr, firstErr)
	}

	// What was returned before the stop stays usable, which is the whole point
	// of handing it back.
	if first.Header.Get("Content-Type") != "text/plain" {
		t.Errorf("part returned before the stop is no longer usable")
	}

	// The one diagnostic that triggered the stop is still readable afterwards,
	// and nothing was recorded for the scopes that never ran.
	at := offsetOf(t, siblingWarningMessage, "Content-Transfer-Encoding: ")
	want := []diagFacts{wantEncodingFacts([]int{0}, at)}
	if got := factsOfAll(session.Diagnostics()); !reflect.DeepEqual(got, want) {
		t.Errorf("diagnostics =\n%+v\nwant\n%+v", got, want)
	}
	checkMessages(t, session.Diagnostics())
}

// TestParse_walkAfterRootStopVisitsNothing pins that a walk is a scope of the
// parse like any other. A sink that stopped the parse while the root entity was
// being built has ended it, so a walk started afterwards reports the identical
// fatal error and never reaches walkFunc — not even for the root, which needs no
// traversal to produce.
func TestParse_walkAfterRootStopVisitsNothing(t *testing.T) {
	stop := errors.New("caller stopped the parse")

	session, e, err := message.Parse(strings.NewReader(rootBothMessage), &message.ParseOptions{
		DiagnosticSink: message.DiagnosticSinkFunc(func(*message.ParseSession, message.Diagnostic) error {
			return stop
		}),
	})
	if e == nil {
		t.Fatal("Parse returned no entity, want the prefix recovered before the stop")
	}
	if !message.IsFatal(err) {
		t.Fatalf("Parse error = %v, want a fatal error", err)
	}

	visited := 0
	walkErr := e.Walk(func([]int, *message.Entity, error) error {
		visited++
		return nil
	})

	if visited != 0 {
		t.Errorf("Walk called walkFunc %d times, want 0 on a stopped parse", visited)
	}
	if walkErr != err {
		t.Errorf("Walk() = %v, want the identical error Parse returned %v", walkErr, err)
	}
	if !errors.Is(walkErr, stop) {
		t.Errorf("errors.Is(%v, stop) = false, want true", walkErr)
	}

	// The entity handed back before the walk is untouched by the walk refusing
	// to run, and its diagnostic is still readable.
	if got := e.Header.Get("Content-Transfer-Encoding"); got != unknownEncoding {
		t.Errorf("entity returned before the stop is no longer usable: %q", got)
	}
	if got, want := codesOf(session.Diagnostics()), []message.Code{message.CodeCTEUnknown}; !reflect.DeepEqual(got, want) {
		t.Errorf("Diagnostics() = %v, want %v", got, want)
	}
}

// TestParse_walkStopsPartwayThrough pins the other half: a walk already in
// progress when the sink stops observes the stop at its next boundary, returns
// the fatal error, and does not hand walkFunc anything further. What walkFunc
// was already given stays valid.
func TestParse_walkStopsPartwayThrough(t *testing.T) {
	stop := errors.New("caller stopped the parse")

	// The root and the first part are sound, so the walk runs normally until it
	// reaches the part whose transfer encoding has no decoder.
	session, e, err := message.Parse(strings.NewReader(siblingWarningMessage), &message.ParseOptions{
		DiagnosticSink: message.DiagnosticSinkFunc(func(*message.ParseSession, message.Diagnostic) error {
			return stop
		}),
	})
	if err != nil {
		t.Fatalf("Parse() = %v, want nil: nothing has been diagnosed yet", err)
	}

	var visited [][]int
	var kept []*message.Entity
	walkErr := e.Walk(func(path []int, part *message.Entity, err error) error {
		visited = append(visited, path)
		kept = append(kept, part)
		return nil
	})

	if !message.IsFatal(walkErr) || !errors.Is(walkErr, stop) {
		t.Fatalf("Walk() = %v, want a fatal error wrapping the sink's own", walkErr)
	}

	// Only the root was visited: the first part is where the sink failed, so it
	// is not delivered, and neither is its sibling.
	want := [][]int{nil}
	if !reflect.DeepEqual(visited, want) {
		t.Errorf("Walk visited %v, want %v", visited, want)
	}

	// What walkFunc was already handed stays valid and usable after the stop.
	if len(kept) != 1 {
		t.Fatalf("walkFunc kept %d entities, want 1", len(kept))
	}
	if got := kept[0].Header.Get("Content-Type"); got != "multipart/mixed; boundary=BB" {
		t.Errorf("entity handed to walkFunc before the stop is no longer usable: %q", got)
	}

	// The walk reports the same sticky error every other scope of this parse
	// does, and a walk started after the stop visits nothing at all rather than
	// handing out the root once more.
	revisited := 0
	again := e.Walk(func([]int, *message.Entity, error) error {
		revisited++
		return nil
	})
	if again != walkErr {
		t.Errorf("second Walk() = %v, want the identical sticky error %v", again, walkErr)
	}
	if revisited != 0 {
		t.Errorf("second Walk called walkFunc %d times, want 0 on a stopped parse", revisited)
	}
	if got, want := codesOf(session.Diagnostics()), []message.Code{message.CodeCTEUnknown}; !reflect.DeepEqual(got, want) {
		t.Errorf("Diagnostics() = %v, want %v", got, want)
	}
}

// TestParse_walkStopsBeforeAWarnedChildWhoseStopWrapsTheWarning pins the case
// TestParse_walkStopsPartwayThrough cannot reach: a sink that stops the parse
// with an error that is, or wraps, the warning it was told about.
//
// NextPart then returns the usable child together with the session's stop, and
// that stop answers to IsUnknownEncoding or IsUnknownCharset because the sink
// chose to carry the cause. It is still the caller ending the parse, not a
// warning about a part, so Walk must return it — the identical value every other
// scope reports — before walkFunc sees the stopped child, while every sibling
// handed over before it stays as it was. A sink that does not stop leaves the
// ordinary warning callback exactly as it always was, which is the control.
func TestParse_walkStopsBeforeAWarnedChildWhoseStopWrapsTheWarning(t *testing.T) {
	const (
		unknownCTE     = "Content-Type: text/plain\r\nContent-Transfer-Encoding: nonsense\r\n"
		unknownCharset = "Content-Type: text/plain; charset=made-up-charset\r\n"
	)
	message3 := func(warned string) string {
		return "Content-Type: multipart/mixed; boundary=BB\r\n\r\n" +
			"--BB\r\nContent-Type: text/plain\r\n\r\nzero\r\n" +
			"--BB\r\n" + warned + "\r\none\r\n" +
			"--BB\r\nContent-Type: text/plain\r\n\r\ntwo\r\n" +
			"--BB--\r\n"
	}

	for _, child := range []struct {
		name    string
		header  string
		code    message.Code
		warning func(error) bool
	}{
		{"unknown transfer encoding", unknownCTE, message.CodeCTEUnknown, message.IsUnknownEncoding},
		{"unknown charset", unknownCharset, message.CodeCharsetUnknown, message.IsUnknownCharset},
	} {
		for _, sink := range []struct {
			name string
			stop func(message.Diagnostic) error
		}{
			{"sink returns the cause", func(d message.Diagnostic) error { return d.Cause }},
			{"sink wraps the cause", func(d message.Diagnostic) error {
				return fmt.Errorf("caller stopped at %s: %w", d.Code, d.Cause)
			}},
		} {
			t.Run(child.name+"/"+sink.name, func(t *testing.T) {
				session, e, err := message.Parse(strings.NewReader(message3(child.header)), &message.ParseOptions{
					DiagnosticSink: message.DiagnosticSinkFunc(func(_ *message.ParseSession, d message.Diagnostic) error {
						return sink.stop(d)
					}),
				})
				if err != nil {
					t.Fatalf("Parse() = %v, want nil: nothing has been diagnosed yet", err)
				}

				var visited [][]int
				var bodies []string
				walkErr := e.Walk(func(path []int, part *message.Entity, err error) error {
					visited = append(visited, path)
					if part == nil {
						t.Errorf("walkFunc was handed a nil entity at %v", path)
						return err
					}
					if part.MultipartReader() == nil {
						b, readErr := io.ReadAll(part.Body)
						if readErr != nil {
							t.Errorf("reading %v = %v", path, readErr)
						}
						bodies = append(bodies, strings.TrimSuffix(string(b), "\r\n"))
					}
					// The ordinary shape of a walkFunc: a warning is noted and the
					// walk goes on, so only Walk itself can stop at the stop.
					if child.warning(err) {
						return nil
					}
					return err
				})

				if !message.IsFatal(walkErr) {
					t.Fatalf("Walk() = %v, want the sink's FatalError", walkErr)
				}
				// The precondition that makes this test mean something: the stop
				// really answers to the warning predicate Walk forwards.
				if !child.warning(walkErr) {
					t.Fatalf("Walk() = %v, want a stop that wraps the %s warning", walkErr, child.name)
				}

				// Identity: the walk reports the one stop the session published, and
				// so does a walk started afterwards.
				again := e.Walk(func([]int, *message.Entity, error) error {
					t.Error("a walk of a stopped parse called walkFunc")
					return nil
				})
				if again != walkErr {
					t.Errorf("second Walk() = %v, want the identical stop %v", again, walkErr)
				}

				// No callback for the stopped child or anything after it; the root and
				// the sibling before it were handed over, and that sibling's body is
				// intact.
				if want := [][]int{nil, {0}}; !reflect.DeepEqual(visited, want) {
					t.Errorf("Walk visited %v, want %v", visited, want)
				}
				if want := []string{"zero"}; !reflect.DeepEqual(bodies, want) {
					t.Errorf("bodies = %v, want %v", bodies, want)
				}
				if got, want := codesOf(session.Diagnostics()), []message.Code{child.code}; !reflect.DeepEqual(got, want) {
					t.Errorf("Diagnostics() = %v, want %v", got, want)
				}
			})
		}

		t.Run(child.name+"/sink does not stop", func(t *testing.T) {
			session, e, err := message.Parse(strings.NewReader(message3(child.header)), &message.ParseOptions{
				DiagnosticSink: message.DiagnosticSinkFunc(func(*message.ParseSession, message.Diagnostic) error {
					return nil
				}),
			})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}

			var visited [][]int
			var warned [][]int
			var bodies []string
			walkErr := e.Walk(func(path []int, part *message.Entity, err error) error {
				visited = append(visited, path)
				if err != nil {
					if !child.warning(err) || message.IsFatal(err) {
						t.Errorf("walkFunc at %v got %v, want only the %s warning", path, err, child.name)
					}
					warned = append(warned, path)
				}
				if part.MultipartReader() == nil {
					b, _ := io.ReadAll(part.Body)
					bodies = append(bodies, strings.TrimSuffix(string(b), "\r\n"))
				}
				return nil
			})
			if walkErr != nil {
				t.Fatalf("Walk() = %v, want nil", walkErr)
			}
			if want := [][]int{nil, {0}, {1}, {2}}; !reflect.DeepEqual(visited, want) {
				t.Errorf("Walk visited %v, want %v", visited, want)
			}
			if want := [][]int{{1}}; !reflect.DeepEqual(warned, want) {
				t.Errorf("warnings forwarded at %v, want %v", warned, want)
			}
			if want := []string{"zero", "one", "two"}; !reflect.DeepEqual(bodies, want) {
				t.Errorf("bodies = %v, want %v", bodies, want)
			}
			if got, want := codesOf(session.Diagnostics()), []message.Code{child.code}; !reflect.DeepEqual(got, want) {
				t.Errorf("Diagnostics() = %v, want %v", got, want)
			}
		})
	}
}

// TestWalk_withoutSessionIsUnchanged pins that a walk over an entity that was
// not parsed with a session behaves exactly as it always has: there is no parse
// that could be stopped, so nothing new can interrupt it.
func TestWalk_withoutSessionIsUnchanged(t *testing.T) {
	e, err := message.Read(strings.NewReader(nestedWarningMessage))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	var visited [][]int
	walkErr := e.Walk(func(path []int, part *message.Entity, err error) error {
		if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
			return err
		}
		visited = append(visited, path)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("Walk() = %v, want nil", walkErr)
	}

	want := [][]int{nil, {0}, {1}, {1, 0}}
	if !reflect.DeepEqual(visited, want) {
		t.Errorf("Walk visited %v, want %v", visited, want)
	}
}

// TestParse_headerFailureStillReturnsSession pins that the session survives a
// failure that leaves no entity at all, and pins the limit of what that
// guarantees. A session is always returned, but it holds what some producer was
// implemented to record, and no producer diagnoses header framing yet — so a
// malformed header block is carried by the returned error alone. An empty
// session therefore does not mean a clean parse, and a caller has to read both
// results.
//
// The header byte budget is no longer such a failure: it is diagnosed, and its
// boundary tests own it. This one keeps to what is still undiagnosed.
func TestParse_headerFailureStillReturnsSession(t *testing.T) {
	session, e, err := message.Parse(strings.NewReader("this line has no field separator\r\n\r\nbody\r\n"), nil)

	if session == nil {
		t.Fatal("Parse returned a nil session for a header failure")
	}
	if err == nil {
		t.Fatal("Parse() = nil error, want the header failure")
	}
	if e != nil {
		t.Errorf("Parse returned entity %v, want nil when the header could not be read", e)
	}
	// The header limit is not a stop asked for by the caller.
	if message.IsFatal(err) {
		t.Errorf("Parse error = %v, want an ordinary error", err)
	}
	// The session is empty because nothing diagnoses header framing yet, not
	// because the parse went well. The error is the only report of this
	// failure, which is exactly what the documentation must not overstate.
	if got := session.Diagnostics(); got != nil {
		t.Errorf("Diagnostics() = %v, want none: no producer diagnoses header framing yet", codesOf(got))
	}
	if session.Len() != 0 {
		t.Errorf("Len() = %d, want 0", session.Len())
	}
}

// TestParse_zeroAndNilOptions pins the defaults. Passing nothing must mean the
// recommended configuration rather than an unconfigured one.
func TestParse_zeroAndNilOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts *message.ParseOptions
	}{
		{"nil options", nil},
		{"zero options", &message.ParseOptions{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, e, err := message.Parse(strings.NewReader(rootEncodingMessage), tc.opts)
			if !message.IsUnknownEncoding(err) {
				t.Fatalf("Parse error = %v, want the encoding warning", err)
			}
			if got, want := readBody(t, e), "raw body bytes\r\n"; got != want {
				t.Errorf("body = %q, want %q", got, want)
			}

			if got := session.Profile(); got != message.ProfileTolerant {
				t.Errorf("Profile() = %v, want %v", got, message.ProfileTolerant)
			}
			if got := session.Compatibility(); got != message.CompatibilityConservative {
				t.Errorf("Compatibility() = %v, want %v", got, message.CompatibilityConservative)
			}
			// Diagnostics are retained by default.
			if got := session.Len(); got != 1 {
				t.Errorf("Len() = %d, want 1: diagnostics are retained by default", got)
			}
		})
	}
}

// TestParse_optionsReachTheSession pins that a configured profile and policy are
// what the session reports, and that the read options are the existing ones.
func TestParse_optionsReachTheSession(t *testing.T) {
	session, e, err := message.Parse(strings.NewReader(rootEncodingMessage), &message.ParseOptions{
		Profile:       message.ProfileForensic,
		Compatibility: message.CompatibilityAngus,
		ReadOptions:   message.ReadOptions{MaxHeaderBytes: -1},
	})
	if !message.IsUnknownEncoding(err) {
		t.Fatalf("Parse error = %v, want the encoding warning", err)
	}
	if e == nil {
		t.Fatal("Parse returned no entity")
	}
	if got := session.Profile(); got != message.ProfileForensic {
		t.Errorf("Profile() = %v, want %v", got, message.ProfileForensic)
	}
	if got := session.Compatibility(); got != message.CompatibilityAngus {
		t.Errorf("Compatibility() = %v, want %v", got, message.CompatibilityAngus)
	}
}

// TestParse_skipTextAttachmentDecoding pins that the existing option still
// suppresses charset decoding, and that a decoding step never taken is not
// diagnosed: nothing was interpreted, so there is nothing to report.
func TestParse_skipTextAttachmentDecoding(t *testing.T) {
	const text = "Content-Type: text/plain; charset=" + unknownCharset + "\r\n" +
		"Content-Disposition: attachment; filename=a.txt\r\n" +
		"\r\n" +
		"raw body bytes\r\n"

	session, e, err := message.Parse(strings.NewReader(text), &message.ParseOptions{
		ReadOptions: message.ReadOptions{SkipTextAttachmentDecoding: true},
	})
	if err != nil {
		t.Fatalf("Parse() = %v, want nil when the charset is never consulted", err)
	}
	if got, want := readBody(t, e), "raw body bytes\r\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if got := session.Diagnostics(); got != nil {
		t.Errorf("Diagnostics() = %v, want none", codesOf(got))
	}
}

// TestParse_chunkInvariance pins that the diagnostics do not depend on how the
// input arrives: the same message must produce the same codes, paths and
// offsets however it is chunked, including from a reader that returns its last
// bytes together with io.EOF.
func TestParse_chunkInvariance(t *testing.T) {
	at := offsetOf(t, nestedWarningMessage, nestedWarningMarker)
	want := []diagFacts{
		wantEncodingFacts([]int{1, 0}, at),
		wantCharsetFacts([]int{1, 0}, at),
	}

	sources := map[string]func() io.Reader{
		"whole":       func() io.Reader { return strings.NewReader(nestedWarningMessage) },
		"one byte":    func() io.Reader { return iotest.OneByteReader(strings.NewReader(nestedWarningMessage)) },
		"data or err": func() io.Reader { return iotest.DataErrReader(strings.NewReader(nestedWarningMessage)) },
	}
	for _, n := range []int{2, 3, 7, 64, 4095, 4096} {
		sources[fmt.Sprintf("chunks of %d", n)] = func() io.Reader {
			return &parseChunkReader{s: nestedWarningMessage, n: n}
		}
	}

	for name, src := range sources {
		t.Run(name, func(t *testing.T) {
			session, e, err := message.Parse(src(), nil)
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			walkAll(t, e)
			if got := factsOfAll(session.Diagnostics()); !reflect.DeepEqual(got, want) {
				t.Errorf("diagnostics =\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

// TestParse_legacyEntryPointsUnchanged pins the compatibility promise. Read,
// ReadWithOptions and New keep returning exactly what they returned before, so
// bridging the warnings into diagnostics cannot be observed through them.
func TestParse_legacyEntryPointsUnchanged(t *testing.T) {
	// Read and Parse must agree on the entity and on the error, including its
	// text: existing callers may well be matching on it.
	_, parsed, parseErr := message.Parse(strings.NewReader(rootBothMessage), nil)
	read, readErr := message.Read(strings.NewReader(rootBothMessage))
	if readErr == nil || parseErr == nil {
		t.Fatalf("Read() = %v and Parse() = %v, want both to warn", readErr, parseErr)
	}
	if readErr.Error() != parseErr.Error() {
		t.Errorf("Read() error = %q, Parse() error = %q, want the same", readErr, parseErr)
	}
	if !message.IsUnknownCharset(readErr) || message.IsUnknownEncoding(readErr) {
		t.Errorf("Read() error = %v, want only the charset warning", readErr)
	}
	if got, want := readBody(t, read), readBody(t, parsed); got != want {
		t.Errorf("Read() body = %q, Parse() body = %q", got, want)
	}

	// ReadWithOptions is the same entry point with the same options.
	withOpts, err := message.ReadWithOptions(strings.NewReader(rootEncodingMessage), &message.ReadOptions{})
	if !message.IsUnknownEncoding(err) {
		t.Errorf("ReadWithOptions() error = %v, want the encoding warning", err)
	}
	if got, want := readBody(t, withOpts), "raw body bytes\r\n"; got != want {
		t.Errorf("ReadWithOptions() body = %q, want %q", got, want)
	}

	// New builds an entity for a caller and has no parse to attach anything to.
	var h message.Header
	h.Set("Content-Type", "text/plain; charset="+unknownCharset)
	h.Set("Content-Transfer-Encoding", unknownEncoding)
	e, err := message.New(h, strings.NewReader("raw"))
	if e == nil {
		t.Fatal("New returned no entity for a warning")
	}
	if !message.IsUnknownCharset(err) || message.IsUnknownEncoding(err) {
		t.Errorf("New() error = %v, want only the charset warning", err)
	}
	if got := readBody(t, e); got != "raw" {
		t.Errorf("New() body = %q, want %q", got, "raw")
	}

	// And the multipart iterator still hands back a usable part beside its
	// warning.
	legacy, err := message.Read(strings.NewReader(siblingWarningMessage))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	mr := legacy.MultipartReader()
	if mr == nil {
		t.Fatal("MultipartReader() = nil")
	}
	part, err := mr.NextPart()
	if part == nil || !message.IsUnknownEncoding(err) {
		t.Fatalf("NextPart() = (%v, %v), want a usable part and the encoding warning", part, err)
	}
	if got := readBody(t, part); got != "first" {
		t.Errorf("NextPart() body = %q, want %q", got, "first")
	}
}

// TestNewMultipart_noDecodingWarning pins what the NewMultipart documentation
// now says in place of a warning it cannot produce. Neither decoding stage
// applies to a multipart entity — its body is not transfer decoded, and a
// charset applies only to text/* — so a conforming header yields no warning
// even when it carries labels that would warn on any other entity.
func TestNewMultipart_noDecodingWarning(t *testing.T) {
	var child message.Header
	child.Set("Content-Type", "text/plain")
	part, err := message.New(child, strings.NewReader("hi"))
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	var h message.Header
	h.Set("Content-Type", "multipart/mixed; boundary=BB; charset="+unknownCharset)
	h.Set("Content-Transfer-Encoding", unknownEncoding)

	e, err := message.NewMultipart(h, []*message.Entity{part})
	if err != nil {
		t.Errorf("NewMultipart() = %v, want nil: neither decoding stage applies to multipart", err)
	}
	if e == nil {
		t.Fatal("NewMultipart returned no entity")
	}
	if message.IsUnknownEncoding(err) || message.IsUnknownCharset(err) {
		t.Errorf("NewMultipart() = %v, want no decoding warning", err)
	}
}

// TestParse_errorsIsAndAs pins that the errors this item produces behave the
// ordinary way for the errors package, at every layer a caller may unwrap.
func TestParse_errorsIsAndAs(t *testing.T) {
	session, _, err := message.Parse(strings.NewReader(rootBothMessage), nil)

	// The returned warning is reachable as its concrete type, and so is the
	// warning behind the diagnostic the returned error does not carry.
	var charsetErr message.UnknownCharsetError
	if !errors.As(err, &charsetErr) {
		t.Errorf("errors.As(err, &UnknownCharsetError{}) = false, want true")
	}
	var encodingErr message.UnknownEncodingError
	if errors.As(err, &encodingErr) {
		t.Errorf("errors.As(err, &UnknownEncodingError{}) = true, want false for the charset warning")
	}

	diags := session.Diagnostics()
	if len(diags) != 2 {
		t.Fatalf("Diagnostics() = %v, want two", codesOf(diags))
	}
	if !errors.As(diags[0].Cause, &encodingErr) {
		t.Errorf("errors.As(first Cause, &UnknownEncodingError{}) = false, want true")
	}
	if !errors.As(diags[1].Cause, &charsetErr) {
		t.Errorf("errors.As(second Cause, &UnknownCharsetError{}) = false, want true")
	}
	// A warning is not a fatal error, however it is reached.
	if message.IsFatal(err) || message.IsFatal(diags[0].Cause) {
		t.Errorf("a warning satisfied IsFatal")
	}

	// A sink stop wraps the sink's own error, so errors.Is finds it through the
	// fatal error, and errors.As finds the fatal error itself.
	stop := errors.New("caller stopped the parse")
	_, _, fatalErr := message.Parse(strings.NewReader(rootBothMessage), &message.ParseOptions{
		DiagnosticSink: message.DiagnosticSinkFunc(func(*message.ParseSession, message.Diagnostic) error {
			return stop
		}),
	})
	if !errors.Is(fatalErr, stop) {
		t.Errorf("errors.Is(%v, stop) = false, want true", fatalErr)
	}
	var fatal message.FatalError
	if !errors.As(fatalErr, &fatal) {
		t.Errorf("errors.As(%v, &FatalError) = false, want true", fatalErr)
	}
	if errors.As(fatalErr, &charsetErr) {
		t.Errorf("the fatal error reported a charset warning it was never given")
	}
}

// parseChunkReader hands out at most n bytes per Read, so a test can vary how
// the input is delivered without varying the input itself.
type parseChunkReader struct {
	s string
	n int
}

func (c *parseChunkReader) Read(p []byte) (int, error) {
	if len(c.s) == 0 {
		return 0, io.EOF
	}
	n := c.n
	if n > len(p) {
		n = len(p)
	}
	if n > len(c.s) {
		n = len(c.s)
	}
	n = copy(p[:n], c.s[:n])
	c.s = c.s[n:]
	return n, nil
}
