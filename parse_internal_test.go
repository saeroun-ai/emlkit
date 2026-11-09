package message

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// The tests here cover what the external parse tests cannot see: whether an
// entry point allocated a session at all, and the scope-entry check of entity
// construction, which Parse itself cannot reach because it always starts from a
// fresh session.

const internalUnknownEncoding = "x-emlkit-nonexistent-encoding"

// TestParse_allocatesTheSessionItReturns pins that the session a caller gets is
// the one the parsed tree carries, all the way down.
func TestParse_allocatesTheSessionItReturns(t *testing.T) {
	f := nestedFixture()

	session, e, err := Parse(strings.NewReader(f.text), nil)
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if session == nil {
		t.Fatal("Parse returned a nil session")
	}
	for i, ent := range collectEntities(t, e) {
		if ent.parse.session != session {
			t.Errorf("entity at path %v carries session %p, want %p",
				nestedPaths[i], ent.parse.session, session)
		}
	}
}

// TestLegacyEntryPoints_allocateNoSession pins the other half of the
// compatibility promise: the entry points that cannot return a session do not
// build one behind the caller's back, so they allocate nothing and expose
// nothing new.
func TestLegacyEntryPoints_allocateNoSession(t *testing.T) {
	var h Header
	h.Set("Content-Type", "text/plain; charset=x-emlkit-nonexistent-charset")
	h.Set("Content-Transfer-Encoding", internalUnknownEncoding)

	e, err := New(h, strings.NewReader("raw"))
	if !IsUnknownCharset(err) {
		t.Fatalf("New() = %v, want the charset warning", err)
	}
	if e.parse.session != nil {
		t.Errorf("New() entity carries session %p, want nil", e.parse.session)
	}

	me, err := NewMultipart(testHeader(), []*Entity{e})
	if err != nil {
		t.Fatalf("NewMultipart() = %v", err)
	}
	if me.parse.session != nil {
		t.Errorf("NewMultipart() entity carries session %p, want nil", me.parse.session)
	}

	f := nestedFixture()
	for _, tc := range []struct {
		name string
		read func() (*Entity, error)
	}{
		{"Read", func() (*Entity, error) { return Read(strings.NewReader(f.text)) }},
		{"ReadWithOptions", func() (*Entity, error) {
			return ReadWithOptions(strings.NewReader(f.text), &ReadOptions{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := tc.read()
			if err != nil {
				t.Fatalf("%s() = %v", tc.name, err)
			}
			for i, ent := range collectEntities(t, e) {
				if ent.parse.session != nil {
					t.Errorf("entity at path %v carries session %p, want nil",
						nestedPaths[i], ent.parse.session)
				}
			}
		})
	}
}

// testHeader is a minimal multipart header for NewMultipart.
func testHeader() Header {
	var h Header
	h.Set("Content-Type", "multipart/mixed; boundary=BB")
	return h
}

// stoppedSession returns a session that has already been stopped by its sink,
// together with the sticky error every scope of it must now report and a
// counter of how often the sink was consulted. The diagnostic used to trigger
// the stop is unrelated to anything the tests below parse; only the stop
// matters.
func stoppedSession(t *testing.T) (*ParseSession, error, *int) {
	t.Helper()

	stop := errors.New("caller stopped the parse")
	calls := 0
	session := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error {
			calls++
			return stop
		})})

	sticky := session.record(Diagnostic{Code: CodeInputEmpty})
	if !IsFatal(sticky) {
		t.Fatalf("record() = %v, want a fatal error", sticky)
	}
	return session, sticky, &calls
}

// TestReadWithOptions_stoppedSessionAtScopeEntry pins the outermost scope-entry
// check. Reading a message is a scope of the parse, so it does not begin on a
// session that has already been stopped, and nothing has been recovered at that
// point — not even a header — so there is nothing to hand back with the error.
func TestReadWithOptions_stoppedSessionAtScopeEntry(t *testing.T) {
	session, sticky, calls := stoppedSession(t)

	f := nestedFixture()
	e, err := readWithOptions(strings.NewReader(f.text), nil, session)
	if e != nil {
		t.Errorf("readWithOptions() returned an entity, want nothing recovered")
	}
	if err != sticky {
		t.Errorf("readWithOptions() = %v, want the identical sticky error %v", err, sticky)
	}
	if got := sessionCodes(session); len(got) != 1 || got[0] != CodeInputEmpty {
		t.Errorf("Diagnostics() = %v, want only the diagnostic that stopped the parse", got)
	}
	if *calls != 1 {
		t.Errorf("sink was called %d times, want 1", *calls)
	}
}

// TestNewWithOptions_stoppedSessionAtScopeEntry pins the scope-entry check of
// entity construction, which is what keeps a stop observed at one decoding
// stage from letting the next one run. A stopped session means no decoder is
// set up and no defect is looked for: the header and the body as it arrived are
// what this scope recovered, and they are returned with the sticky error.
func TestNewWithOptions_stoppedSessionAtScopeEntry(t *testing.T) {
	session, sticky, calls := stoppedSession(t)

	var h Header
	h.Set("Content-Type", "text/plain; charset=x-emlkit-nonexistent-charset")
	h.Set("Content-Transfer-Encoding", internalUnknownEncoding)

	e, err := newWithOptions(h, strings.NewReader("body bytes"), false, rootContext(session, 0))
	if e == nil {
		t.Fatal("newWithOptions returned no entity, want the one it could still build")
	}
	if err != sticky {
		t.Errorf("newWithOptions() = %v, want the identical sticky error %v", err, sticky)
	}

	body, readErr := io.ReadAll(e.Body)
	if readErr != nil {
		t.Fatalf("reading body: %v", readErr)
	}
	if got, want := string(body), "body bytes"; got != want {
		t.Errorf("body = %q, want the untouched %q", got, want)
	}

	// The declared metadata is still read, because it describes the bytes
	// rather than interprets them, and WriteTo relies on it to copy them
	// through verbatim.
	if e.encoding != internalUnknownEncoding {
		t.Errorf("encoding = %q, want %q", e.encoding, internalUnknownEncoding)
	}
	if e.charset != "x-emlkit-nonexistent-charset" {
		t.Errorf("charset = %q, want the declared one", e.charset)
	}

	// Nothing was interpreted, so nothing beyond the diagnostic that stopped
	// the parse was recorded, and the failed sink was not consulted again.
	if got := sessionCodes(session); len(got) != 1 || got[0] != CodeInputEmpty {
		t.Errorf("Diagnostics() = %v, want only the diagnostic that stopped the parse", got)
	}
	if *calls != 1 {
		t.Errorf("sink was called %d times, want 1", *calls)
	}
}

// TestWalk_stoppedSessionAtScopeEntry pins that a walk begun on an already
// stopped parse visits nothing at all, not even the root entity it was called
// on, and reports the identical sticky error.
func TestWalk_stoppedSessionAtScopeEntry(t *testing.T) {
	session, sticky, _ := stoppedSession(t)

	// Build the tree first, while the session can still be read from, so that
	// the walk is the only scope under test.
	f := nestedFixture()
	e, err := readWithOptions(strings.NewReader(f.text), nil, newParseSession(ProfileTolerant, CompatibilityConservative))
	if err != nil {
		t.Fatalf("readWithOptions() = %v", err)
	}
	// Re-point the whole tree at the stopped session.
	e.parse.session = session

	visited := 0
	if err := e.Walk(func([]int, *Entity, error) error {
		visited++
		return nil
	}); err != sticky {
		t.Errorf("Walk() = %v, want the identical sticky error %v", err, sticky)
	}
	if visited != 0 {
		t.Errorf("Walk called walkFunc %d times, want 0 on an already stopped parse", visited)
	}
}
