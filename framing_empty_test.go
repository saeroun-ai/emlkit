package message

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Empty and header-only input (ROB-010-001). Every expectation is written out
// in the fixture — the fields with their raw bytes in document order, the body,
// where the body begins, and what WriteTo writes — and none is computed from a
// parse of the same input. The inputs use CRLF throughout; bare CR, bare LF,
// mixed endings and an unterminated last line are ROB-010-002's, and a
// whitespace-only or otherwise malformed first line is ROB-020's. Every source
// is finite and refuses to be read past a small call cap.

var errFramingCap = errors.New("test source: call cap exceeded")

// A framingSource delivers data in reads of at most chunk bytes, or, when chunk
// is zero, in the segments that splits bound. io.EOF comes together with the
// last bytes when eofWith, and on a read of its own otherwise. onEOF, when set,
// is called once, just before the first io.EOF is returned.
type framingSource struct {
	data    string
	chunk   int
	splits  []int
	eofWith bool
	onEOF   func()

	pos, calls int
}

func (s *framingSource) Read(p []byte) (int, error) {
	s.calls++
	if s.calls > len(s.data)+len(s.splits)+16 {
		return 0, errFramingCap
	}
	if s.pos >= len(s.data) {
		return 0, s.eof()
	}
	end := len(s.data)
	if s.chunk > 0 {
		end = min(end, s.pos+s.chunk)
	} else {
		for _, b := range s.splits {
			if b > s.pos {
				end = min(end, b)
				break
			}
		}
	}
	n := copy(p, s.data[s.pos:end])
	s.pos += n
	if s.pos == len(s.data) && s.eofWith {
		return n, s.eof()
	}
	return n, nil
}

func (s *framingSource) eof() error {
	if s.onEOF != nil {
		s.onEOF()
		s.onEOF = nil
	}
	return io.EOF
}

type framingField struct{ key, value, raw string }

// framingFieldsOf lists a header's fields in document order.
func framingFieldsOf(fs textproto.HeaderFields) []framingField {
	var out []framingField
	for fs.Next() {
		raw, err := fs.Raw()
		if err != nil {
			raw = []byte("raw error: " + err.Error())
		}
		out = append(out, framingField{fs.Key(), fs.Value(), string(raw)})
	}
	return out
}

const (
	framingFields = "From: a@example.com\r\nSubject: one\r\n\ttwo\r\nX-Empty:\r\n"
	framingMIME   = "MIME-Version: 1.0\r\nSubject: x\r\n"
	framingMulti  = "Content-Type: multipart/mixed; boundary=B\r\n"
)

// mimeVersionField is the field WriteTo adds at the top of a header that has
// no MIME-Version.
var mimeVersionField = framingField{"Mime-Version", "1.0", "Mime-Version: 1.0\r\n"}

// A framingForm is one empty or header-only input and everything it yields.
type framingForm struct {
	name   string
	text   string
	fields []framingField
	body   string
	bodyAt int64

	// endsAtEOF says the header block is ended by io.EOF rather than by an
	// empty line, so the read that reports io.EOF is a read of the header.
	endsAtEOF bool
	multipart bool

	// addsMIME says WriteTo adds mimeVersionField; wire is what it writes for
	// a freshly parsed entity.
	addsMIME bool
	wire     string
}

func framingForms() []framingForm {
	fields := []framingField{
		{"From", "a@example.com", "From: a@example.com\r\n"},
		{"Subject", "one two", "Subject: one\r\n\ttwo\r\n"},
		{"X-Empty", "", "X-Empty:\r\n"},
	}
	mime := []framingField{
		{"Mime-Version", "1.0", "MIME-Version: 1.0\r\n"},
		{"Subject", "x", "Subject: x\r\n"},
	}
	multi := []framingField{{"Content-Type", "multipart/mixed; boundary=B", framingMulti}}
	n, m := int64(len(framingFields)), int64(len(framingMulti))
	return []framingForm{
		{name: "empty", text: "", bodyAt: 0, endsAtEOF: true,
			addsMIME: true, wire: "Mime-Version: 1.0\r\n\r\n"},
		{name: "separator", text: "\r\n", bodyAt: 2,
			addsMIME: true, wire: "Mime-Version: 1.0\r\n\r\n"},
		{name: "separator and one blank body line", text: "\r\n\r\n", body: "\r\n", bodyAt: 2,
			addsMIME: true, wire: "Mime-Version: 1.0\r\n\r\n\r\n"},
		{name: "separator and two blank body lines", text: "\r\n\r\n\r\n", body: "\r\n\r\n", bodyAt: 2,
			addsMIME: true, wire: "Mime-Version: 1.0\r\n\r\n\r\n\r\n"},
		{name: "fields at EOF", text: framingFields, fields: fields, bodyAt: n, endsAtEOF: true,
			addsMIME: true, wire: "Mime-Version: 1.0\r\n" + framingFields + "\r\n"},
		{name: "fields and separator", text: framingFields + "\r\n", fields: fields, bodyAt: n + 2,
			addsMIME: true, wire: "Mime-Version: 1.0\r\n" + framingFields + "\r\n"},
		{name: "fields, separator and a blank body line", text: framingFields + "\r\n\r\n", fields: fields, body: "\r\n", bodyAt: n + 2,
			addsMIME: true, wire: "Mime-Version: 1.0\r\n" + framingFields + "\r\n\r\n"},
		{name: "MIME-Version at EOF", text: framingMIME, fields: mime, bodyAt: int64(len(framingMIME)), endsAtEOF: true,
			wire: framingMIME + "\r\n"},
		{name: "multipart at EOF", text: framingMulti, fields: multi, bodyAt: m, endsAtEOF: true, multipart: true,
			addsMIME: true, wire: "Mime-Version: 1.0\r\n" + framingMulti + "\r\n"},
		{name: "multipart and separator", text: framingMulti + "\r\n", fields: multi, bodyAt: m + 2, multipart: true,
			addsMIME: true, wire: "Mime-Version: 1.0\r\n" + framingMulti + "\r\n"},
	}
}

var framingCompatibilities = []Compatibility{
	CompatibilityConservative, CompatibilityAngus, CompatibilityMimeKit, CompatibilityPythonEmail,
}

// checkFramingHeader checks a freshly parsed entity against f: no error, the
// fields and their raw bytes in order, where the body begins, and, for an
// entity from Parse, an empty session and no stop.
func checkFramingHeader(t *testing.T, name string, f framingForm, s *ParseSession, e *Entity, err error) bool {
	t.Helper()
	if err != nil || e == nil {
		t.Errorf("%s: (entity %v, %v), want the entity and no error", name, e != nil, err)
		return false
	}
	if got := framingFieldsOf(e.Header.Fields()); !reflect.DeepEqual(got, f.fields) {
		t.Errorf("%s: fields %q, want %q", name, got, f.fields)
	}
	if got, want := originRecordOf(e), (originRecord{known: true, bodyOffset: f.bodyAt}); !reflect.DeepEqual(got, want) {
		t.Errorf("%s: origin %v, want %v", name, got, want)
	}
	if s != nil && (s.Len() != 0 || s.fatalErr() != nil || e.ParseStop() != nil) {
		t.Errorf("%s: diagnostics %v, stop %v; want neither", name, codesOfDiags(s.Diagnostics()), s.fatalErr())
	}
	return true
}

// checkFramingBody reads the whole body of a fresh entity.
func checkFramingBody(t *testing.T, name string, f framingForm, s *ParseSession, e *Entity) {
	t.Helper()
	b, err := io.ReadAll(e.Body)
	if string(b) != f.body || err != nil {
		t.Errorf("%s: body (%q, %v), want (%q, nil)", name, b, err, f.body)
	}
	if s != nil && s.Len() != 0 {
		t.Errorf("%s: diagnostics %v after the body", name, codesOfDiags(s.Diagnostics()))
	}
}

// checkFramingParts iterates a fresh entity. A multipart with an empty body
// has no part: the first NextPart wraps io.EOF, then io.EOF itself
// (ROB-060-009 owns that policy; this pins it).
func checkFramingParts(t *testing.T, name string, f framingForm, s *ParseSession, e *Entity) {
	t.Helper()
	mr := e.MultipartReader()
	if (mr != nil) != f.multipart {
		t.Errorf("%s: MultipartReader() != nil is %v, want %v", name, mr != nil, f.multipart)
		return
	}
	if mr == nil {
		return
	}
	p, err := mr.NextPart()
	// Every profile but Strict heals an empty multipart: no part, io.EOF, and
	// one multipart.empty diagnostic (ROB-060-009).
	healed := s != nil && s.Profile() != ProfileStrict
	if healed {
		if p != nil || err != io.EOF {
			t.Errorf("%s: first NextPart() = (%v, %v), want no part and io.EOF", name, p != nil, err)
		}
	} else if p != nil || err == nil || err == io.EOF || !errors.Is(err, io.EOF) || err.Error() != "multipart: NextPart: EOF" {
		t.Errorf("%s: first NextPart() = (%v, %v), want no part and an error wrapping io.EOF", name, p != nil, err)
	}
	if p, err = mr.NextPart(); p != nil || err != io.EOF {
		t.Errorf("%s: second NextPart() = (%v, %v), want io.EOF", name, p != nil, err)
	}
	if healed {
		if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeMultipartEmpty}) || s.fatalErr() != nil {
			t.Errorf("%s: iteration recorded %v, stop %v, want multipart.empty alone", name, codes, s.fatalErr())
		}
	} else if s != nil && (s.Len() != 0 || s.fatalErr() != nil) {
		t.Errorf("%s: iteration recorded %v, stop %v", name, codesOfDiags(s.Diagnostics()), s.fatalErr())
	}
}

// checkFramingWalk walks a fresh entity: the root alone is visited, and a
// multipart ends the walk with the error its first NextPart returns.
func checkFramingWalk(t *testing.T, name string, f framingForm, e *Entity) {
	t.Helper()
	var visits []string
	werr := e.Walk(func(path []int, part *Entity, err error) error {
		visits = append(visits, fmt.Sprintf("%v %v", path, err))
		return nil
	})
	if !reflect.DeepEqual(visits, []string{"[] <nil>"}) {
		t.Errorf("%s: Walk visited %q, want the root alone", name, visits)
	}
	healed := e.parse.session != nil && e.parse.session.Profile() != ProfileStrict
	if f.multipart && healed {
		// An empty multipart is healed: the walk ends without an error.
		if werr != nil {
			t.Errorf("%s: Walk() = %v, want nil", name, werr)
		}
	} else if f.multipart {
		if werr == nil || !errors.Is(werr, io.EOF) || werr.Error() != "multipart: NextPart: EOF" {
			t.Errorf("%s: Walk() = %v, want the error wrapping io.EOF", name, werr)
		}
	} else if werr != nil {
		t.Errorf("%s: Walk() = %v", name, werr)
	}
}

// writeFresh writes a fresh entity.
func writeFresh(t *testing.T, name string, e *Entity, err error) (string, bool) {
	t.Helper()
	if e == nil {
		t.Errorf("%s: no entity to write (%v)", name, err)
		return "", false
	}
	var w bytes.Buffer
	if werr := e.WriteTo(&w); werr != nil {
		t.Errorf("%s: WriteTo() = %v", name, werr)
		return "", false
	}
	return w.String(), true
}

// checkFramingRewrite pins what WriteTo writes for a fresh entity: the fields
// read, raw and in order, MIME-Version added at the top when there was none,
// one empty line, the body. It is not the input's bytes: an empty or
// header-only input gains that field and that line. Parsing what was written
// gives the same body and the written fields, and writing it again gives the
// same bytes.
//
// A healing parse is the exception when nothing has to be added: a header
// that says MIME-Version and was not changed is written as it was read, with
// no empty line the input did not have (ROB-020-007). Read, ReadWithOptions
// and ProfileStrict write the empty line, as they always did.
func checkFramingRewrite(t *testing.T, name string, f framingForm, parse func(io.Reader) (*ParseSession, *Entity, error), src func() io.Reader) {
	t.Helper()
	_, e, err := parse(src())
	want := f.wire
	if e != nil && !f.addsMIME && e.Header.Intact() {
		want = f.text
	}
	wire, ok := writeFresh(t, name, e, err)
	if !ok {
		return
	}
	if wire != want {
		t.Errorf("%s: WriteTo wrote %q, want %q", name, wire, want)
		return
	}

	again := f
	if f.addsMIME {
		again.fields = append([]framingField{mimeVersionField}, f.fields...)
	}
	again.bodyAt = int64(len(want) - len(f.body))
	s, e, err := parse(strings.NewReader(want))
	if checkFramingHeader(t, name+"/reparsed", again, s, e, err) {
		checkFramingBody(t, name+"/reparsed", again, s, e)
	}
	_, e, err = parse(strings.NewReader(want))
	if second, ok := writeFresh(t, name+"/rewritten", e, err); ok && second != want {
		t.Errorf("%s: second WriteTo wrote %q, want %q", name, second, want)
	}
}

// runFraming checks f read from src by parse, each use from a fresh parse.
func runFraming(t *testing.T, name string, f framingForm, parse func(io.Reader) (*ParseSession, *Entity, error), src func() io.Reader, full bool) {
	t.Helper()
	s, e, err := parse(src())
	if checkFramingHeader(t, name, f, s, e, err) {
		checkFramingBody(t, name, f, s, e)
	}
	if full {
		if s, e, err := parse(src()); checkFramingHeader(t, name+"/parts", f, s, e, err) {
			checkFramingParts(t, name+"/parts", f, s, e)
		}
		if s, e, err := parse(src()); checkFramingHeader(t, name+"/walk", f, s, e, err) {
			checkFramingWalk(t, name+"/walk", f, e)
		}
	}
	checkFramingRewrite(t, name, f, parse, src)
}

func framingParser(opts *ParseOptions) func(io.Reader) (*ParseSession, *Entity, error) {
	return func(r io.Reader) (*ParseSession, *Entity, error) { return Parse(r, opts) }
}

// TestParse_emptyAndHeaderOnly pins empty and header-only input under every
// profile and compatibility policy, default and disabled limits, reads of 1, 7
// and 4096 bytes, and io.EOF with the last bytes or on a read of its own: a
// usable entity with exactly the fields read, raw and in order, an empty body
// or the blank lines after the separator, no error, no diagnostic, a multipart
// with no parts, and a WriteTo that adds MIME-Version and the separator.
func TestParse_emptyAndHeaderOnly(t *testing.T) {
	for _, f := range framingForms() {
		for lname, limits := range map[string]Limits{"default": {}, "disabled": allDisabled()} {
			for _, p := range progressProfiles {
				for _, c := range framingCompatibilities {
					opts := &ParseOptions{Profile: p, Compatibility: c, Limits: limits}
					for _, chunk := range []int{1, 7, 4096} {
						for _, eofWith := range []bool{false, true} {
							name := fmt.Sprintf("%s/%s/%v/%v/chunk %d/eof with data %v", f.name, lname, p, c, chunk, eofWith)
							src := func() io.Reader { return &framingSource{data: f.text, chunk: chunk, eofWith: eofWith} }
							runFraming(t, name, f, framingParser(opts), src, true)
						}
					}
				}
			}
		}
	}
}

// TestParse_emptyAndHeaderOnlySplits delivers each input in two reads split
// at every byte, with io.EOF with the second or on a read of its own, under
// every profile.
func TestParse_emptyAndHeaderOnlySplits(t *testing.T) {
	for _, f := range framingForms() {
		for _, p := range progressProfiles {
			opts := &ParseOptions{Profile: p}
			for cut := 1; cut < len(f.text); cut++ {
				for _, eofWith := range []bool{false, true} {
					name := fmt.Sprintf("%s/%v/split %d/eof with data %v", f.name, p, cut, eofWith)
					src := func() io.Reader { return &framingSource{data: f.text, splits: []int{cut}, eofWith: eofWith} }
					runFraming(t, name, f, framingParser(opts), src, false)
				}
			}
		}
	}
}

// TestRead_emptyAndHeaderOnly pins the same results through Read and
// ReadWithOptions, which have no session.
func TestRead_emptyAndHeaderOnly(t *testing.T) {
	read := func(r io.Reader) (*ParseSession, *Entity, error) {
		e, err := Read(r)
		return nil, e, err
	}
	readWithOptions := func(r io.Reader) (*ParseSession, *Entity, error) {
		e, err := ReadWithOptions(r, &ReadOptions{MaxHeaderBytes: -1})
		return nil, e, err
	}
	for _, f := range framingForms() {
		for _, chunk := range []int{1, 7, 4096} {
			for _, eofWith := range []bool{false, true} {
				src := func() io.Reader { return &framingSource{data: f.text, chunk: chunk, eofWith: eofWith} }
				name := fmt.Sprintf("%s/chunk %d/eof with data %v", f.name, chunk, eofWith)
				runFraming(t, "Read/"+name, f, read, src, true)
				runFraming(t, "ReadWithOptions/"+name, f, readWithOptions, src, false)
			}
		}
	}
}

// TestParse_emptyAndHeaderOnlyEnds pins what ends such an input when it is not
// io.EOF. A header block that only io.EOF could end — an empty input, or
// complete fields with no empty line — is still being read when the input
// fails, is cancelled or stalls at its end, so that is reported by Parse with
// no entity, exactly as for any header. A block ended by its empty line gives
// the entity, and the body reports the failure after the bytes before it.
func TestParse_emptyAndHeaderOnlyEnds(t *testing.T) {
	t.Run("input error", func(t *testing.T) {
		for _, f := range framingForms() {
			for _, p := range progressProfiles {
				for _, chunk := range []int{1, 7, 4096} {
					for _, withData := range []bool{false, true} {
						name := fmt.Sprintf("%s/%v/chunk %d/with data %v", f.name, p, chunk, withData)
						src := &ioSource{data: f.text, cut: len(f.text), chunk: chunk, err: errSourceIO, withData: withData, sticky: true}
						s, e, err := Parse(src, &ParseOptions{Profile: p})
						checkFramingFailure(t, name, f, s, e, err, errSourceIO, false)
						if src.readsAfter != 0 {
							t.Errorf("%s: %d reads after the input failed", name, src.readsAfter)
						}
						if s.Len() != 0 || s.fatalErr() != nil {
							t.Errorf("%s: diagnostics %v, stop %v; an input error is neither", name, codesOfDiags(s.Diagnostics()), s.fatalErr())
						}
					}
				}
			}
		}
	})

	t.Run("cancelled before", func(t *testing.T) {
		for _, f := range framingForms() {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			src := &framingSource{data: f.text, chunk: 4096}
			s, e, err := ParseContext(ctx, src, nil)
			if src.calls != 0 || e != nil {
				t.Errorf("%s: %d reads, entity %v; want neither", f.name, src.calls, e != nil)
			}
			checkCancelStop(t, f.name, err, context.Canceled)
			if s.Len() != 0 {
				t.Errorf("%s: diagnostics %v", f.name, codesOfDiags(s.Diagnostics()))
			}
		}
	})

	t.Run("cancelled at the end", func(t *testing.T) {
		for _, f := range framingForms() {
			for _, chunk := range []int{1, 7, 4096} {
				for _, eofWith := range []bool{false, true} {
					name := fmt.Sprintf("%s/chunk %d/eof with data %v", f.name, chunk, eofWith)
					ctx, cancel := context.WithCancel(context.Background())
					src := &framingSource{data: f.text, chunk: chunk, eofWith: eofWith, onEOF: cancel}
					s, e, err := ParseContext(ctx, src, nil)
					// With io.EOF on a read of its own, the cancellation comes
					// after every byte, past an empty line that already ended the
					// header. With io.EOF delivered together with the last bytes,
					// whether the header saw it depends on when that read happened:
					// an entity built after it is returned with the stop, and its
					// body delivers nothing more once the stop has been seen.
					checkFramingFailure(t, name, f, s, e, err, nil, eofWith)
					if s.Len() != 0 {
						t.Errorf("%s: diagnostics %v", name, codesOfDiags(s.Diagnostics()))
					}
					checkCancelStop(t, name, s.fatalErr(), context.Canceled)
					cancel()
				}
			}
		}
	})

	t.Run("no progress", func(t *testing.T) {
		for _, f := range framingForms() {
			for _, p := range progressProfiles {
				for _, chunk := range []int{1, 4096} {
					for _, z := range []int{maxConsecutiveEmptyReads - 1, maxConsecutiveEmptyReads} {
						name := fmt.Sprintf("%s/%v/chunk %d/%d empty reads", f.name, p, chunk, z)
						opts := &ParseOptions{Profile: p}
						src := newZeroSource(f.text, chunk, map[int]int{len(f.text): z})
						s, e, err := Parse(src, opts)
						if z < maxConsecutiveEmptyReads {
							if checkFramingHeader(t, name, f, s, e, err) {
								checkFramingBody(t, name, f, s, e)
							}
							continue
						}
						checkFramingFailure(t, name, f, s, e, err, nil, false)
						d := noProgressStop(t, s.fatalErr(), CodeInputNoProgress)
						if d.Location.Offset != int64(len(f.text)) || src.zeroReads != maxConsecutiveEmptyReads {
							t.Errorf("%s: input.no_progress at %v after %d empty reads, want offset %d after %d",
								name, d.Location, src.zeroReads, len(f.text), maxConsecutiveEmptyReads)
						}
						if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeInputNoProgress}) {
							t.Errorf("%s: diagnostics %v, want one input.no_progress", name, codes)
						}
					}
				}
			}
		}
	})
}

// checkFramingFailure checks an input that ended with something other than
// io.EOF where io.EOF would have been: fail, the input's own error, or, when
// fail is nil, the session's stop. A block only io.EOF could end yields no
// entity and that identical error. A block its empty line ended yields the
// entity and no error, and the body reports the failure after every byte
// before it. loose admits the other outcome of a cancellation delivered with
// the last bytes: Parse returns the entity with the stop, and the body is then
// a prefix.
func checkFramingFailure(t *testing.T, name string, f framingForm, s *ParseSession, e *Entity, err, fail error, loose bool) {
	t.Helper()
	ended := func() error {
		if fail != nil {
			return fail
		}
		return s.fatalErr()
	}
	if f.endsAtEOF {
		if want := ended(); e != nil || want == nil || err != want {
			t.Errorf("%s: Parse() = (entity %v, %v), want no entity and the identical %v", name, e != nil, err, want)
		}
		return
	}
	if e == nil {
		t.Errorf("%s: no entity (%v); the empty line ended the header", name, err)
		return
	}
	if err != nil && !(loose && fail == nil && err == ended()) {
		t.Errorf("%s: Parse() error %v, want nil", name, err)
	}
	if got := framingFieldsOf(e.Header.Fields()); !reflect.DeepEqual(got, f.fields) {
		t.Errorf("%s: fields %q, want %q", name, got, f.fields)
	}
	if f.multipart && fail != nil {
		// The parts' framing reports the input's error, wrapped.
		p, perr := e.MultipartReader().NextPart()
		if p != nil || !errors.Is(perr, fail) {
			t.Errorf("%s: NextPart() = (%v, %v), want the input's error", name, p != nil, perr)
		}
		return
	}
	b, berr := io.ReadAll(e.Body)
	if want := ended(); want == nil || berr != want {
		t.Errorf("%s: body ended with %v, want the identical %v", name, berr, want)
	}
	if loose {
		if !strings.HasPrefix(f.body, string(b)) {
			t.Errorf("%s: body %q, want a prefix of %q", name, b, f.body)
		}
	} else if string(b) != f.body {
		t.Errorf("%s: body %q, want %q before the failure", name, b, f.body)
	}
}

// TestParse_emptyAndHeaderOnlyBudgets pins the budgets that apply: a block or
// an input of exactly the budget is read, one byte over is refused. A block
// ended by io.EOF is its fields alone; the empty line that ends one counts
// toward MaxHeaderBytes and toward no field.
func TestParse_emptyAndHeaderOnlyBudgets(t *testing.T) {
	for _, f := range framingForms() {
		exact := Limits{MaxInputBytes: max(1, int64(len(f.text))), MaxHeaderCount: max(1, int64(len(f.fields)))}
		if f.bodyAt > 0 {
			exact.MaxHeaderBytes = f.bodyAt
		}
		// The input alone: what WriteTo writes is longer than these budgets.
		if s, e, err := Parse(strings.NewReader(f.text), &ParseOptions{Limits: exact}); checkFramingHeader(t, f.name+"/exact budgets", f, s, e, err) {
			checkFramingBody(t, f.name+"/exact budgets", f, s, e)
		}

		if f.bodyAt > 0 {
			s, e, err := Parse(strings.NewReader(f.text), &ParseOptions{Limits: Limits{MaxHeaderBytes: f.bodyAt - 1}})
			checkFramingRefusal(t, f.name+"/header bytes", s, e, err, CodeLimitHeaderBytes, 0)
		}
		if f.endsAtEOF && len(f.text) > 0 {
			over := int64(len(f.text)) - 1
			s, e, err := Parse(strings.NewReader(f.text), &ParseOptions{Limits: Limits{MaxInputBytes: over}})
			checkFramingRefusal(t, f.name+"/input bytes", s, e, err, CodeLimitInputBytes, over)
		}
	}
}

func checkFramingRefusal(t *testing.T, name string, s *ParseSession, e *Entity, err error, code Code, offset int64) {
	t.Helper()
	var fe FatalError
	if e != nil || !errors.As(err, &fe) {
		t.Errorf("%s: Parse() = (entity %v, %v), want no entity and a FatalError", name, e != nil, err)
		return
	}
	if d, ok := fe.Diagnostic(); !ok || d.Code != code || d.Location.Offset != offset {
		t.Errorf("%s: diagnostic %v (%v), want %s at offset %d", name, d, ok, code, offset)
	}
	if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{code}) {
		t.Errorf("%s: diagnostics %v, want one %s", name, codes, code)
	}
}

// TestParse_headerOnlyKeepsInterpretation pins that ending a header at io.EOF,
// or having no body, neither adds to nor hides what its fields declare: the
// result is the one the same field gives with an empty line and no body. An
// unknown transfer encoding or charset is still a warning with its diagnostic,
// under ProfileStrict too; a multipart whose conflicting boundaries the repair
// dropped is still an opaque leaf with its diagnostic; ProfileStrict still
// refuses that field with an entity-scoped FatalError that does not stop the
// parse. The body is empty throughout.
func TestParse_headerOnlyKeepsInterpretation(t *testing.T) {
	old := CharsetReader
	CharsetReader = nil
	t.Cleanup(func() { CharsetReader = old })

	const (
		unknownCTE     = "Content-Transfer-Encoding: x-emlkit-unknown\r\n"
		unknownCharset = "Content-Type: text/plain; charset=x-emlkit-unknown\r\n"
		conflicting    = "Content-Type: multipart/mixed; boundary=B; boundary=C\r\n"
	)
	cte := framingField{"Content-Transfer-Encoding", "x-emlkit-unknown", unknownCTE}
	charset := framingField{"Content-Type", "text/plain; charset=x-emlkit-unknown", unknownCharset}
	boundaries := framingField{"Content-Type", "multipart/mixed; boundary=B; boundary=C", conflicting}
	noError := func(err error) bool { return err == nil }
	cases := []struct {
		name    string
		field   framingField
		profile Profile
		err     func(error) bool
		codes   []Code
	}{
		{"unknown encoding", cte, ProfileTolerant, IsUnknownEncoding, []Code{CodeCTEUnknown}},
		{"unknown encoding", cte, ProfileStrict, IsUnknownEncoding, []Code{CodeCTEUnknown}},
		{"unknown charset", charset, ProfileTolerant, IsUnknownCharset, []Code{CodeCharsetUnknown}},
		{"unknown charset", charset, ProfileStrict, IsUnknownCharset, []Code{CodeCharsetUnknown}},
		{"conflicting boundaries", boundaries, ProfileTolerant, noError, []Code{CodeMIMEParameterDuplicate}},
		{"conflicting boundaries", boundaries, ProfileStrict, IsFatal, []Code{CodeMIMEParameterDuplicate}},
	}
	for _, tc := range cases {
		for _, sep := range []string{"", "\r\n"} {
			name := fmt.Sprintf("%s/%v/empty line %v", tc.name, tc.profile, sep != "")
			s, e, err := Parse(strings.NewReader(tc.field.raw+sep), &ParseOptions{Profile: tc.profile})
			if e == nil || !tc.err(err) {
				t.Errorf("%s: Parse() = (entity %v, %v)", name, e != nil, err)
				continue
			}
			if got := framingFieldsOf(e.Header.Fields()); !reflect.DeepEqual(got, []framingField{tc.field}) {
				t.Errorf("%s: fields %q, want %q", name, got, tc.field)
			}
			if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, tc.codes) {
				t.Errorf("%s: diagnostics %v, want %v", name, codes, tc.codes)
			}
			if e.ParseStop() != nil || e.MultipartReader() != nil {
				t.Errorf("%s: ParseStop() = %v, MultipartReader() != nil is %v; want neither", name, e.ParseStop(), e.MultipartReader() != nil)
			}
			if b, berr := io.ReadAll(e.Body); len(b) != 0 || berr != nil {
				t.Errorf("%s: body (%q, %v), want (\"\", nil)", name, b, berr)
			}
		}
	}
}
