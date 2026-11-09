package message

import (
	"fmt"
	"io"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

// originFixture is a message assembled from labelled segments, so that every
// expected byte offset is derived from the fixture rather than counted out by
// hand.
type originFixture struct {
	text string
	at   map[string]int64
}

func newOriginFixture(segments ...[2]string) originFixture {
	f := originFixture{at: make(map[string]int64)}
	var b strings.Builder
	for _, s := range segments {
		if s[0] != "" {
			f.at[s[0]] = int64(b.Len())
		}
		b.WriteString(s[1])
	}
	f.text = b.String()
	return f
}

func (f originFixture) offset(t *testing.T, label string) int64 {
	t.Helper()
	off, ok := f.at[label]
	if !ok {
		t.Fatalf("fixture has no label %q", label)
	}
	return off
}

// nestedFixture is a multipart/mixed whose second part is itself a
// multipart/alternative, so that a child path is extended twice, and whose
// last part is base64 so that its recorded position is the undecoded one.
func nestedFixture() originFixture {
	return nestedFixtureEOL("\r\n")
}

// nestedFixtureEOL is nestedFixture with every line ending given explicitly,
// so that the same tree can be presented with the CRLF the RFCs require and
// with the bare LF that appears in the wild. Offsets are derived from the
// segments either way, so the expectations follow the shorter lines by
// themselves.
func nestedFixtureEOL(eol string) originFixture {
	return newOriginFixture(
		[2]string{"root.header", "Mime-Version: 1.0" + eol},
		[2]string{"", "Content-Type: multipart/mixed; boundary=OUTER" + eol},
		[2]string{"", eol},
		[2]string{"root.body", "--OUTER" + eol},
		[2]string{"0.header", "Content-Type: text/plain" + eol},
		[2]string{"", eol},
		[2]string{"0.body", "first" + eol},
		[2]string{"", "--OUTER" + eol},
		[2]string{"1.header", "Content-Type: multipart/alternative; boundary=INNER" + eol},
		[2]string{"", eol},
		[2]string{"1.body", "--INNER" + eol},
		[2]string{"1.0.header", "Content-Type: text/plain" + eol},
		[2]string{"", eol},
		[2]string{"1.0.body", "alt text" + eol},
		[2]string{"", "--INNER" + eol},
		[2]string{"1.1.header", "Content-Type: text/html" + eol},
		[2]string{"", eol},
		[2]string{"1.1.body", "<p>alt html</p>" + eol},
		[2]string{"", "--INNER--" + eol},
		[2]string{"", "--OUTER" + eol},
		[2]string{"2.header", "Content-Type: text/plain; charset=us-ascii" + eol},
		[2]string{"", "Content-Transfer-Encoding: base64" + eol},
		[2]string{"", eol},
		[2]string{"2.body", "bGFzdA==" + eol},
		[2]string{"", "--OUTER--" + eol},
	)
}

// originRecord is what a traversal observed about one entity.
type originRecord struct {
	path         []int
	known        bool
	headerOffset int64
	bodyOffset   int64
}

// originRecordOf reduces an entity to what the parser said about where it came
// from.
func originRecordOf(e *Entity) originRecord {
	return originRecord{
		path:         e.parse.origin.pathCopy(),
		known:        e.parse.origin.known,
		headerOffset: e.parse.origin.headerOffset,
		bodyOffset:   e.parse.origin.bodyOffset,
	}
}

func (r originRecord) String() string {
	if !r.known {
		return fmt.Sprintf("path=%v origin unknown", r.path)
	}
	return fmt.Sprintf("path=%v header=%d body=%d", r.path, r.headerOffset, r.bodyOffset)
}

// collectOrigins walks the whole tree depth first, in the order a caller
// consuming the message would reach each entity, and reports the origin the
// parser gave each one.
func collectOrigins(t *testing.T, e *Entity) []originRecord {
	t.Helper()

	var out []originRecord
	var visit func(e *Entity)
	visit = func(e *Entity) {
		out = append(out, originRecordOf(e))

		mr := e.MultipartReader()
		if mr == nil {
			if _, err := io.Copy(io.Discard, e.Body); err != nil {
				t.Fatalf("reading body of %v: %v", out[len(out)-1], err)
			}
			return
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				return
			}
			if err != nil {
				t.Fatalf("NextPart under %v: %v", out[len(out)-1], err)
			}
			visit(part)
		}
	}
	visit(e)
	return out
}

func wantNestedOrigins(t *testing.T, f originFixture) []originRecord {
	t.Helper()
	return []originRecord{
		{path: nil, known: true, headerOffset: f.offset(t, "root.header"), bodyOffset: f.offset(t, "root.body")},
		{path: []int{0}, known: true, headerOffset: f.offset(t, "0.header"), bodyOffset: f.offset(t, "0.body")},
		{path: []int{1}, known: true, headerOffset: f.offset(t, "1.header"), bodyOffset: f.offset(t, "1.body")},
		{path: []int{1, 0}, known: true, headerOffset: f.offset(t, "1.0.header"), bodyOffset: f.offset(t, "1.0.body")},
		{path: []int{1, 1}, known: true, headerOffset: f.offset(t, "1.1.header"), bodyOffset: f.offset(t, "1.1.body")},
		{path: []int{2}, known: true, headerOffset: f.offset(t, "2.header"), bodyOffset: f.offset(t, "2.body")},
	}
}

// TestRead_origin_root checks the root of a parsed message: a nil path, a
// header block at the first byte of the input, and a body that starts where
// the header block ended.
func TestRead_origin_root(t *testing.T) {
	e, err := Read(strings.NewReader(testSingleText))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	if !e.parse.origin.known {
		t.Fatal("the root of a parsed message has no origin")
	}
	if e.parse.origin.path != nil {
		t.Errorf("root path = %v, want nil", e.parse.origin.path)
	}
	if e.parse.origin.headerOffset != 0 {
		t.Errorf("root headerOffset = %d, want 0", e.parse.origin.headerOffset)
	}
	want := int64(strings.Index(testSingleText, "Message body"))
	if e.parse.origin.bodyOffset != want {
		t.Errorf("root bodyOffset = %d, want %d", e.parse.origin.bodyOffset, want)
	}
}

// TestRead_origin_nestedPaths checks that a path is extended once per level of
// the tree and that every entity reports where its header and body begin in
// the original input, including a part reached through a nested multipart and
// a part whose body is transfer encoded.
func TestRead_origin_nestedPaths(t *testing.T) {
	f := nestedFixture()

	e, err := Read(strings.NewReader(f.text))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	got := collectOrigins(t, e)
	want := wantNestedOrigins(t, f)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("origins =\n%v\nwant\n%v", got, want)
	}
}

// TestRead_origin_headerOnly checks a message that is nothing but a header
// block: the body is empty and starts one past the last byte of the input,
// which is a position the parser must still be able to name. The three forms
// differ in how the header block ends — a blank line, the end of the input, or
// LF endings throughout.
func TestRead_origin_headerOnly(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"blank line", "Content-Type: text/plain\r\n\r\n"},
		{"no blank line", "Content-Type: text/plain\r\n"},
		{"lf blank line", "Content-Type: text/plain\n\n"},
		{"lf no blank line", "Content-Type: text/plain\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := Read(strings.NewReader(tc.text))
			if err != nil {
				t.Fatalf("Read() = %v", err)
			}

			want := originRecord{known: true, headerOffset: 0, bodyOffset: int64(len(tc.text))}
			if got := originRecordOf(e); !reflect.DeepEqual(got, want) {
				t.Errorf("origin = %v, want %v", got, want)
			}

			body, err := io.ReadAll(e.Body)
			if err != nil {
				t.Fatalf("reading body: %v", err)
			}
			if len(body) != 0 {
				t.Errorf("body = %q, want empty", body)
			}
		})
	}
}

// TestRead_origin_lineEndings checks that the offsets follow the input's own
// line endings rather than the CRLF the RFCs require: a message written with
// bare LF is shorter, and every position in it has to move with it.
func TestRead_origin_lineEndings(t *testing.T) {
	for _, tc := range []struct{ name, eol string }{
		{"crlf", "\r\n"},
		{"lf", "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := nestedFixtureEOL(tc.eol)

			e, err := Read(strings.NewReader(f.text))
			if err != nil {
				t.Fatalf("Read() = %v", err)
			}
			if got, want := collectOrigins(t, e), wantNestedOrigins(t, f); !reflect.DeepEqual(got, want) {
				t.Errorf("origins =\n%v\nwant\n%v", got, want)
			}
		})
	}
}

// TestRead_origin_withoutReadingBodies checks that the offsets do not depend on
// the caller having consumed anything: a caller that calls NextPart again
// without reading the part it was just given must still be told where every
// part is.
func TestRead_origin_withoutReadingBodies(t *testing.T) {
	f := nestedFixture()

	e, err := Read(strings.NewReader(f.text))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	var got []originRecord
	for _, ent := range collectEntities(t, e) {
		got = append(got, originRecordOf(ent))
	}
	if want := wantNestedOrigins(t, f); !reflect.DeepEqual(got, want) {
		t.Errorf("origins =\n%v\nwant\n%v", got, want)
	}
}

// TestRead_origin_chunkInvariance checks that the same message produces the
// same paths and offsets however its input is chunked, including a reader that
// returns its last bytes together with io.EOF and readers whose chunk sizes
// vary from one Read to the next.
func TestRead_origin_chunkInvariance(t *testing.T) {
	f := nestedFixture()
	want := wantNestedOrigins(t, f)

	sources := map[string]func() io.Reader{
		"whole":       func() io.Reader { return strings.NewReader(f.text) },
		"one byte":    func() io.Reader { return iotest.OneByteReader(strings.NewReader(f.text)) },
		"data or err": func() io.Reader { return iotest.DataErrReader(strings.NewReader(f.text)) },
	}
	for _, n := range []int{2, 3, 7, 64, 4095, 4096} {
		sources[fmt.Sprintf("chunks of %d", n)] = func() io.Reader {
			return &chunkReader{s: f.text, n: n}
		}
	}
	// Fixed chunk sizes always split the message in the same places. Varying
	// them reaches splits inside a boundary line, a header field and a fold
	// that no single fixed size does, and seeding the generator keeps a
	// failure reproducible.
	for _, seed := range []int64{1, 2, 3, 1234567} {
		for _, max := range []int{5, 71, 4200} {
			sources[fmt.Sprintf("random chunks up to %d seed %d", max, seed)] = func() io.Reader {
				return newRandomChunkReader(f.text, max, seed)
			}
		}
	}

	for name, src := range sources {
		t.Run(name, func(t *testing.T) {
			e, err := Read(src())
			if err != nil {
				t.Fatalf("Read() = %v", err)
			}
			if got := collectOrigins(t, e); !reflect.DeepEqual(got, want) {
				t.Errorf("origins =\n%v\nwant\n%v", got, want)
			}
		})
	}
}

// TestReadWithOptions_origin_maxHeaderBytes checks that the header size budget
// does not move where the parser says anything is. The limited reader sits
// between the byte counter and the buffer, so what it shortens is an
// individual read, not a position in the input.
func TestReadWithOptions_origin_maxHeaderBytes(t *testing.T) {
	f := nestedFixture()
	want := wantNestedOrigins(t, f)
	header := f.offset(t, "root.body") // the whole top-level header block

	for _, n := range []int64{header, header + 1, 1 << 20, -1, 0} {
		t.Run(fmt.Sprintf("MaxHeaderBytes=%d", n), func(t *testing.T) {
			e, err := ReadWithOptions(strings.NewReader(f.text), &ReadOptions{MaxHeaderBytes: n})
			if err != nil {
				t.Fatalf("ReadWithOptions() = %v", err)
			}
			if got := collectOrigins(t, e); !reflect.DeepEqual(got, want) {
				t.Errorf("origins =\n%v\nwant\n%v", got, want)
			}
		})
	}
}

// TestRead_origin_matchesWalkPath checks that the path the parser assigns an
// entity is the same path Walk reports for it, so the two ways of naming a
// part cannot drift apart.
func TestRead_origin_matchesWalkPath(t *testing.T) {
	f := nestedFixture()

	e, err := Read(strings.NewReader(f.text))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	var got [][]int
	err = e.Walk(func(path []int, part *Entity, err error) error {
		if err != nil {
			return err
		}
		if !part.parse.origin.known {
			t.Errorf("entity at Walk path %v has no origin", path)
			return nil
		}
		if !reflect.DeepEqual(part.parse.origin.pathCopy(), path) {
			t.Errorf("origin path = %v, but Walk reports %v", part.parse.origin.pathCopy(), path)
		}
		got = append(got, path)
		return nil
	})
	if err != nil {
		t.Fatalf("Entity.Walk() = %v", err)
	}

	want := [][]int{nil, {0}, {1}, {1, 0}, {1, 1}, {2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Walk paths = %v, want %v", got, want)
	}
}

// TestRead_origin_partsOwnTheirPaths checks the ownership boundary: a part's
// path is its own, so neither its parent's nor a sibling's path can be
// rewritten through it, and a part returned early keeps the path it was given
// after later parts have been read.
func TestRead_origin_partsOwnTheirPaths(t *testing.T) {
	f := nestedFixture()

	e, err := Read(strings.NewReader(f.text))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}

	mr := e.MultipartReader()
	if mr == nil {
		t.Fatal("MultipartReader() = nil")
	}
	first, err := mr.NextPart()
	if err != nil {
		t.Fatalf("part 0: %v", err)
	}
	nested, err := mr.NextPart()
	if err != nil {
		t.Fatalf("part 1: %v", err)
	}

	// Writing through one part's path must not reach any other entity's, so
	// scribble over every path in sight before checking the rest.
	nested.parse.origin.path[0] = 99

	inner := nested.MultipartReader()
	if inner == nil {
		t.Fatal("nested MultipartReader() = nil")
	}
	innerFirst, err := inner.NextPart()
	if err != nil {
		t.Fatalf("part 1.0: %v", err)
	}
	innerSecond, err := inner.NextPart()
	if err != nil {
		t.Fatalf("part 1.1: %v", err)
	}
	innerSecond.parse.origin.path[1] = 99

	if got := first.parse.origin.pathCopy(); !reflect.DeepEqual(got, []int{0}) {
		t.Errorf("part 0 path = %v, want [0]", got)
	}
	if got := innerFirst.parse.origin.pathCopy(); !reflect.DeepEqual(got, []int{99, 0}) {
		// The child copied the parent's path as it was when the child was
		// made, so it carries the scribble but nothing later can change it.
		t.Errorf("part 1.0 path = %v, want [99 0]", got)
	}
	if e.parse.origin.path != nil {
		t.Errorf("root path = %v, want nil", e.parse.origin.path)
	}
}

// TestEntityOrigin_pathCopy checks that a path handed out is a copy the caller
// owns, and that the root's nil path and an unknown origin's are alike.
func TestEntityOrigin_pathCopy(t *testing.T) {
	o := entityOrigin{known: true, path: []int{1, 0}}

	got := o.pathCopy()
	got[0] = 99
	if o.path[0] != 1 {
		t.Errorf("pathCopy() aliases the origin's own path: %v", o.path)
	}

	if got := rootOrigin(10).pathCopy(); got != nil {
		t.Errorf("root pathCopy() = %v, want nil", got)
	}
	if got := (entityOrigin{}).pathCopy(); got != nil {
		t.Errorf("unknown pathCopy() = %v, want nil", got)
	}
}

func TestEntityOrigin_child(t *testing.T) {
	root := rootOrigin(20)

	child := root.child(1, 4, 30)
	if !child.known {
		t.Fatal("child of a known origin is not known")
	}
	if !reflect.DeepEqual(child.path, []int{1}) {
		t.Errorf("child path = %v, want [1]", child.path)
	}
	// Offsets are relative to the parent's body, and absolute once resolved.
	if child.headerOffset != 24 || child.bodyOffset != 50 {
		t.Errorf("child offsets = (%d, %d), want (24, 50)", child.headerOffset, child.bodyOffset)
	}

	grandchild := child.child(0, 2, 6)
	if !reflect.DeepEqual(grandchild.path, []int{1, 0}) {
		t.Errorf("grandchild path = %v, want [1 0]", grandchild.path)
	}
	if grandchild.headerOffset != 52 || grandchild.bodyOffset != 56 {
		t.Errorf("grandchild offsets = (%d, %d), want (52, 56)", grandchild.headerOffset, grandchild.bodyOffset)
	}

	// A second child must not reach into the first one's path, whatever
	// capacity the parent's path happens to have. Spare capacity is what makes
	// this fail if children are made by appending in place: both children
	// would then be looking at the same array, and the last one made would win.
	spare := entityOrigin{known: true, path: make([]int, 1, 8)}
	spare.path[0] = 7
	first := spare.child(0, 0, 0)
	second := spare.child(1, 0, 0)
	if !reflect.DeepEqual(first.path, []int{7, 0}) {
		t.Errorf("first child path = %v after making sibling %v, want [7 0]", first.path, second.path)
	}
	if !reflect.DeepEqual(second.path, []int{7, 1}) {
		t.Errorf("second child path = %v, want [7 1]", second.path)
	}
	if !reflect.DeepEqual(spare.path, []int{7}) {
		t.Errorf("parent path = %v after making children, want [7]", spare.path)
	}

	// An unknown parent cannot place its children in the input.
	if got := (entityOrigin{}).child(0, 4, 8); got.known {
		t.Errorf("child of an unknown origin = %+v, want unknown", got)
	}
}

func TestEntityOrigin_locations(t *testing.T) {
	o := entityOrigin{known: true, headerOffset: 12, bodyOffset: 40}

	if got, want := o.location(), LocationAt(12); got != want {
		t.Errorf("location() = %v, want %v", got, want)
	}
	if got, want := o.bodyLocation(), LocationAt(40); got != want {
		t.Errorf("bodyLocation() = %v, want %v", got, want)
	}

	// An unknown origin reports an unknown position rather than offset zero,
	// which is a real position in the input.
	var unknown entityOrigin
	if got := unknown.location(); got.HasOffset {
		t.Errorf("unknown location() = %v, want an unknown position", got)
	}
	if got := unknown.bodyLocation(); got.HasOffset {
		t.Errorf("unknown bodyLocation() = %v, want an unknown position", got)
	}
}

// TestEntity_origin_notParsed checks that entities the caller assembled carry
// no origin: nothing about them can be attributed to a parsed input.
func TestEntity_origin_notParsed(t *testing.T) {
	if e := testMakeEntity(); e.parse.origin.known {
		t.Errorf("New() entity origin = %+v, want unknown", e.parse.origin)
	}

	e := testMakeMultipart()
	if e.parse.origin.known {
		t.Errorf("NewMultipart() entity origin = %+v, want unknown", e.parse.origin)
	}
	for _, got := range collectOrigins(t, e) {
		if got.known {
			t.Errorf("part of a NewMultipart() entity has origin %v, want unknown", got)
		}
	}
}

// TestEntity_origin_replacedBody checks that replacing a parsed entity's Body
// withdraws the origin of what is read from it: those parts are not the
// input's parts, so neither their positions nor their paths describe it.
func TestEntity_origin_replacedBody(t *testing.T) {
	f := nestedFixture()

	e, err := Read(strings.NewReader(f.text))
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	e.Body = strings.NewReader("--OUTER\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"replaced\r\n" +
		"--OUTER--\r\n")

	// The entity itself was still parsed from the input, so it keeps its own
	// origin; only what is read out of the replacement loses one.
	if !e.parse.origin.known {
		t.Error("replacing the body withdrew the entity's own origin")
	}

	mr := e.MultipartReader()
	if mr == nil {
		t.Fatal("MultipartReader() = nil")
	}
	part, err := mr.NextPart()
	if err != nil {
		t.Fatalf("NextPart: %v", err)
	}
	if part.parse.origin.known {
		t.Errorf("part read from a replaced body has origin %+v, want unknown", part.parse.origin)
	}
}

// TestReadWithOptions_origin_skipTextAttachmentDecoding checks that an option
// that changes how a body is decoded does not change where the parser says it
// is: offsets describe the input, not the decoded view.
func TestReadWithOptions_origin_skipTextAttachmentDecoding(t *testing.T) {
	f := newOriginFixture(
		[2]string{"root.header", "Content-Type: multipart/mixed; boundary=BB\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"root.body", "--BB\r\n"},
		[2]string{"0.header", "Content-Type: text/plain; charset=utf-8\r\n"},
		[2]string{"", "Content-Disposition: attachment; filename=a.txt\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"0.body", "hello\r\n"},
		[2]string{"", "--BB--\r\n"},
	)

	want := []originRecord{
		{path: nil, known: true, headerOffset: f.offset(t, "root.header"), bodyOffset: f.offset(t, "root.body")},
		{path: []int{0}, known: true, headerOffset: f.offset(t, "0.header"), bodyOffset: f.offset(t, "0.body")},
	}

	for _, skip := range []bool{false, true} {
		t.Run(fmt.Sprintf("skip=%v", skip), func(t *testing.T) {
			e, err := ReadWithOptions(strings.NewReader(f.text), &ReadOptions{SkipTextAttachmentDecoding: skip})
			if err != nil {
				t.Fatalf("ReadWithOptions() = %v", err)
			}
			if got := collectOrigins(t, e); !reflect.DeepEqual(got, want) {
				t.Errorf("origins =\n%v\nwant\n%v", got, want)
			}
		})
	}
}

// chunkReader hands out at most n bytes per Read, so that a test can vary how
// the input is delivered without varying the input itself.
type chunkReader struct {
	s string
	n int
}

func (c *chunkReader) Read(p []byte) (int, error) {
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

// randomChunkReader hands out between 1 and max bytes per Read, choosing how
// many from a seeded generator so that a run that fails can be repeated
// exactly.
type randomChunkReader struct {
	s   string
	max int
	rnd *rand.Rand
}

func newRandomChunkReader(s string, max int, seed int64) *randomChunkReader {
	return &randomChunkReader{s: s, max: max, rnd: rand.New(rand.NewSource(seed))}
}

func (c *randomChunkReader) Read(p []byte) (int, error) {
	if len(c.s) == 0 {
		return 0, io.EOF
	}
	n := 1 + c.rnd.Intn(c.max)
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
