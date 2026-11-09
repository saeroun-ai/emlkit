package message

import (
	"io"
	"reflect"
	"strings"
	"testing"
)

// collectEntities walks the whole tree depth first, in the order a caller
// consuming the message would reach each entity, and returns the entities
// themselves so that a test can inspect what the parser attached to them.
//
// It does not read any leaf body: NextPart discards an unread one, and a test
// about what the parser recorded should not depend on the caller having
// consumed anything.
func collectEntities(t *testing.T, e *Entity) []*Entity {
	t.Helper()

	var out []*Entity
	var visit func(e *Entity)
	visit = func(e *Entity) {
		out = append(out, e)

		mr := e.MultipartReader()
		if mr == nil {
			return
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				return
			}
			if err != nil {
				t.Fatalf("NextPart under path %v: %v", e.parse.origin.pathCopy(), err)
			}
			visit(part)
		}
	}
	visit(e)
	return out
}

// nestedPaths is the tree of nestedFixture, in the order collectEntities
// reaches it.
var nestedPaths = [][]int{nil, {0}, {1}, {1, 0}, {1, 1}, {2}}

// nestedLabels names the same entities as the labels nestedFixture uses for
// their offsets.
var nestedLabels = []string{"root", "0", "1", "1.0", "1.1", "2"}

// TestReadWithOptions_session_propagates checks the seam the diagnostics work
// rests on: one session reaches the root entity, a direct child of it, and a
// child of that child, so a defect discovered at any depth — long after the
// parse call returned — is recorded against the parse it belongs to.
func TestReadWithOptions_session_propagates(t *testing.T) {
	f := nestedFixture()
	session := newParseSession(ProfileForensic, CompatibilityAngus)

	e, err := readWithOptions(strings.NewReader(f.text), nil, session)
	if err != nil {
		t.Fatalf("readWithOptions() = %v", err)
	}

	got := collectEntities(t, e)
	if len(got) != len(nestedPaths) {
		t.Fatalf("reached %d entities, want %d", len(got), len(nestedPaths))
	}
	for i, ent := range got {
		if ent.parse.session != session {
			t.Errorf("entity at path %v carries session %p, want %p",
				nestedPaths[i], ent.parse.session, session)
		}
	}
}

// TestRead_session_absent checks that the exported entry points invent no
// session: until a public Parse API exists, a parsed entity carries a nil one,
// and threading it must stay harmless.
func TestRead_session_absent(t *testing.T) {
	f := nestedFixture()

	e, err := Read(strings.NewReader(f.text))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	for i, ent := range collectEntities(t, e) {
		if ent.parse.session != nil {
			t.Errorf("entity at path %v carries session %p, want nil",
				nestedPaths[i], ent.parse.session)
		}
		// Recording through a nil session must be a no-op rather than a panic.
		if err := ent.parse.recordHeader(Diagnostic{Code: CodeHeaderMissingColon}); err != nil {
			t.Errorf("recordHeader on a session-less context = %v", err)
		}
	}
}

// recorded is one diagnostic reduced to what this file is about: which entity
// it was attributed to and where in the input it was placed.
type recorded struct {
	code     Code
	path     []int
	location Location
}

func recordedFrom(d Diagnostic) recorded {
	return recorded{code: d.Code, path: d.Path, location: d.Location}
}

// TestParseContext_record_fillsPathAndLocation checks what the seam completes:
// a diagnostic recorded on an entity is attributed to that entity's MIME path,
// and placed at the absolute offset of its header block or of its body — for
// the root, for a direct child, and for a child reached through a nested
// multipart.
func TestParseContext_record_fillsPathAndLocation(t *testing.T) {
	f := nestedFixture()
	session := newParseSession(ProfileTolerant, CompatibilityConservative)

	e, err := readWithOptions(strings.NewReader(f.text), nil, session)
	if err != nil {
		t.Fatalf("readWithOptions() = %v", err)
	}

	entities := collectEntities(t, e)
	if len(entities) != len(nestedPaths) {
		t.Fatalf("reached %d entities, want %d", len(entities), len(nestedPaths))
	}

	var want []recorded
	for i, ent := range entities {
		if err := ent.parse.recordHeader(Diagnostic{Code: CodeHeaderMissingColon}); err != nil {
			t.Fatalf("recordHeader at path %v: %v", nestedPaths[i], err)
		}
		if err := ent.parse.recordBody(Diagnostic{Code: CodeCTEBase64Truncated}); err != nil {
			t.Fatalf("recordBody at path %v: %v", nestedPaths[i], err)
		}
		want = append(want,
			recorded{
				code:     CodeHeaderMissingColon,
				path:     nestedPaths[i],
				location: LocationAt(f.offset(t, nestedLabels[i]+".header")),
			},
			recorded{
				code:     CodeCTEBase64Truncated,
				path:     nestedPaths[i],
				location: LocationAt(f.offset(t, nestedLabels[i]+".body")),
			},
		)
	}

	var got []recorded
	for _, d := range session.Diagnostics() {
		got = append(got, recordedFrom(d))
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("diagnostics =\n%v\nwant\n%v", got, want)
	}
}

// TestParseContext_record_suppliedValuesWin checks the precedence rule: the
// context supplies a default, never an override. The parser knows things the
// context does not — which field of a header block is at fault, or that an
// observation belongs to a different entity than the one being parsed — so
// anything it supplied itself has to survive.
func TestParseContext_record_suppliedValuesWin(t *testing.T) {
	// A context two levels down, so that a supplied path is visibly different
	// from the one it must not be replaced by.
	c := rootContext(nil, 100).child(1, 10, 40).child(0, 4, 12)
	if got := c.origin.pathCopy(); !reflect.DeepEqual(got, []int{1, 0}) {
		t.Fatalf("fixture context path = %v, want [1 0]", got)
	}
	headerAt := LocationAt(c.origin.headerOffset)
	bodyAt := LocationAt(c.origin.bodyOffset)

	for _, tc := range []struct {
		name string
		in   Diagnostic
		// wantPath is the path the diagnostic must end up with.
		wantPath []int
		// wantLocation is the location it must end up with, or the zero value
		// when the context is expected to supply it — which is the header or
		// the body position depending on which method was called.
		wantLocation Location
	}{{
		name:     "nothing supplied",
		in:       Diagnostic{Code: CodeHeaderMissingColon},
		wantPath: []int{1, 0},
	}, {
		name:     "path supplied",
		in:       Diagnostic{Code: CodeHeaderMissingColon, Path: []int{7, 7, 7}},
		wantPath: []int{7, 7, 7},
	}, {
		// A path of no indices is the root's path, and the root is not what a
		// caller means by leaving it out, so an empty one is still filled in.
		name:     "empty path supplied",
		in:       Diagnostic{Code: CodeHeaderMissingColon, Path: []int{}},
		wantPath: []int{1, 0},
	}, {
		name:         "location supplied",
		in:           Diagnostic{Code: CodeHeaderMissingColon, Location: LocationRange(3, 9)},
		wantPath:     []int{1, 0},
		wantLocation: LocationRange(3, 9),
	}, {
		// A location that carries only a line is still the parser's answer, so
		// it is kept whole rather than having an offset added to it.
		name:         "partial location supplied",
		in:           Diagnostic{Code: CodeHeaderMissingColon, Location: Location{Line: 9}},
		wantPath:     []int{1, 0},
		wantLocation: Location{Line: 9},
	}, {
		name:         "both supplied",
		in:           Diagnostic{Code: CodeHeaderMissingColon, Path: []int{4}, Location: LocationAt(2)},
		wantPath:     []int{4},
		wantLocation: LocationAt(2),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			for _, body := range []bool{false, true} {
				session := newParseSession(ProfileTolerant, CompatibilityConservative)
				sc := parseContext{session: session, origin: c.origin}

				want := recorded{code: tc.in.Code, path: tc.wantPath, location: tc.wantLocation}

				var err error
				if body {
					err = sc.recordBody(tc.in)
					if want.location == (Location{}) {
						want.location = bodyAt
					}
				} else {
					err = sc.recordHeader(tc.in)
					if want.location == (Location{}) {
						want.location = headerAt
					}
				}
				if err != nil {
					t.Fatalf("record: %v", err)
				}

				diags := session.Diagnostics()
				if len(diags) != 1 {
					t.Fatalf("recorded %d diagnostics, want 1", len(diags))
				}
				if got := recordedFrom(diags[0]); !reflect.DeepEqual(got, want) {
					t.Errorf("body=%v: recorded %v, want %v", body, got, want)
				}
			}
		})
	}
}

// TestParseContext_record_unknownOrigin checks that a context which cannot
// place its entity in the input says so instead of guessing. Offset zero is a
// real position and the root's nil path is a real path, so neither may stand
// in for "not known".
func TestParseContext_record_unknownOrigin(t *testing.T) {
	session := newParseSession(ProfileTolerant, CompatibilityConservative)
	c := parseContext{session: session}

	if err := c.recordHeader(Diagnostic{Code: CodeHeaderMissingColon}); err != nil {
		t.Fatalf("recordHeader: %v", err)
	}
	if err := c.recordBody(Diagnostic{Code: CodeCTEBase64Truncated}); err != nil {
		t.Fatalf("recordBody: %v", err)
	}

	diags := session.Diagnostics()
	if len(diags) != 2 {
		t.Fatalf("recorded %d diagnostics, want 2", len(diags))
	}
	for _, d := range diags {
		if d.Path != nil {
			t.Errorf("%v: path = %v, want nil", d.Code, d.Path)
		}
		if d.Location != (Location{}) {
			t.Errorf("%v: location = %v, want an unknown position", d.Code, d.Location)
		}
	}
}

// TestParseContext_detached checks what a context keeps and what it gives up
// when the bytes it describes are no longer the input's: the parse is the same
// one, so the session travels on, but nothing read from those bytes can be
// placed in the input.
func TestParseContext_detached(t *testing.T) {
	session := newParseSession(ProfileTolerant, CompatibilityConservative)
	c := rootContext(session, 20).child(1, 4, 30)

	d := c.detached()
	if d.session != session {
		t.Errorf("detached session = %p, want %p", d.session, session)
	}
	if d.origin.known {
		t.Errorf("detached origin = %+v, want unknown", d.origin)
	}
	// Children of a detached context cannot be placed either, but stay part of
	// the same parse.
	if got := d.child(0, 1, 2); got.origin.known || got.session != session {
		t.Errorf("child of a detached context = %+v, want unknown origin and the same session", got)
	}
	// Detaching is a value operation: the context it came from is untouched.
	if !c.origin.known {
		t.Error("detached() withdrew the origin of the context it was derived from")
	}
}

// TestEntity_replacedBody_keepsSession checks the same thing end to end. A
// caller that replaced a parsed entity's Body is still parsing this message,
// so defects in the replacement belong to this session; only their positions
// and paths are unknowable.
func TestEntity_replacedBody_keepsSession(t *testing.T) {
	f := nestedFixture()
	session := newParseSession(ProfileTolerant, CompatibilityConservative)

	e, err := readWithOptions(strings.NewReader(f.text), nil, session)
	if err != nil {
		t.Fatalf("readWithOptions() = %v", err)
	}
	e.Body = strings.NewReader("--OUTER\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"replaced\r\n" +
		"--OUTER--\r\n")

	mr := e.MultipartReader()
	if mr == nil {
		t.Fatal("MultipartReader() = nil")
	}
	part, err := mr.NextPart()
	if err != nil {
		t.Fatalf("NextPart: %v", err)
	}

	if part.parse.session != session {
		t.Errorf("part of a replaced body carries session %p, want %p", part.parse.session, session)
	}
	if part.parse.origin.known {
		t.Errorf("part of a replaced body has origin %+v, want unknown", part.parse.origin)
	}

	if err := part.parse.recordHeader(Diagnostic{Code: CodeHeaderMissingColon}); err != nil {
		t.Fatalf("recordHeader: %v", err)
	}
	diags := session.Diagnostics()
	if len(diags) != 1 {
		t.Fatalf("recorded %d diagnostics, want 1", len(diags))
	}
	if diags[0].Path != nil || diags[0].Location != (Location{}) {
		t.Errorf("diagnostic = %v, want no path and an unknown position", diags[0])
	}
}

// TestParseContext_record_defensiveCopies checks that nothing mutable is
// shared across the seam. The entity's own path, the diagnostic the parser
// passed in, what the session retains and what a sink was handed must each be
// modifiable without any of the others changing.
func TestParseContext_record_defensiveCopies(t *testing.T) {
	var delivered []Diagnostic
	sink := DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
		// A sink may modify what it is given, so do, and loudly.
		for i := range d.Path {
			d.Path[i] = 42
		}
		delivered = append(delivered, d)
		return nil
	})
	session := newParseSessionWithOptions(ProfileTolerant, CompatibilityConservative,
		diagnosticOptions{sink: sink})

	c := rootContext(session, 100).child(1, 10, 40).child(0, 4, 12)

	// A path the parser supplied, mutated after the call.
	supplied := []int{5, 6}
	if err := c.recordHeader(Diagnostic{Code: CodeHeaderMissingColon, Path: supplied}); err != nil {
		t.Fatalf("recordHeader: %v", err)
	}
	supplied[0] = 99

	// A path the context filled in, with the origin's own path mutated after
	// the call.
	if err := c.recordBody(Diagnostic{Code: CodeCTEBase64Truncated}); err != nil {
		t.Fatalf("recordBody: %v", err)
	}
	c.origin.path[0] = 77

	if len(delivered) != 2 {
		t.Fatalf("sink received %d diagnostics, want 2", len(delivered))
	}

	// What the session kept is its own, unreachable from any of the above.
	diags := session.Diagnostics()
	if len(diags) != 2 {
		t.Fatalf("retained %d diagnostics, want 2", len(diags))
	}
	if got := diags[0].Path; !reflect.DeepEqual(got, []int{5, 6}) {
		t.Errorf("supplied path retained as %v, want [5 6]", got)
	}
	if got := diags[1].Path; !reflect.DeepEqual(got, []int{1, 0}) {
		t.Errorf("filled path retained as %v, want [1 0]", got)
	}

	// And what Diagnostics handed out is the caller's, so scribbling on it
	// cannot reach the session either.
	diags[1].Path[0] = 13
	if got := session.Diagnostics()[1].Path; !reflect.DeepEqual(got, []int{1, 0}) {
		t.Errorf("session path after modifying a returned copy = %v, want [1 0]", got)
	}
}
