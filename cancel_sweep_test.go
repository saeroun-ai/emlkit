package message

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"testing"
)

// The cancellation sweep (ROB-001-006): every offset of each fixture, in reads
// of 1, 7 and 4096 bytes, with the cancelling read returning its bytes with
// nil, io.EOF or the source's own error. Each run is checked against the same
// consumption under Parse. A part whose body ended before the cancellation is
// exactly the uncut reference's. After it every endpoint reports the one stop
// and the input is never read again, and what was delivered is checked against
// the cut reference: Parse given exactly the bytes the cancelled parse took
// from its input, followed by an ordinary read failure. That is the only fair
// reference for a body the cancellation cut short. A cut can end inside a
// delimiter — "one one one\r" with the '\n' and the boundary still to come — and
// the multipart framing then rightly returns the '\r' as body, as it does for
// any input that fails there; a decoder likewise makes what it makes of a cut
// wire, quoted-printable keeping a cut escape as literal text. Neither is
// changed to suit an uncut reference.

type sweepFixture struct {
	name string
	text string

	// skip is the path of a part the consumer leaves unread, for its parent to
	// drain.
	skip string
}

func sweepFixtures() []sweepFixture {
	plain := bodyFixture{header: "Content-Type: text/plain\r\n", wire: "first line of the body\r\nsecond line\r\n"}
	b64 := bodyFixture{header: "Content-Transfer-Encoding: base64\r\n",
		wire: base64.StdEncoding.EncodeToString([]byte("decoded bytes of a base64 body, decoded bytes")) + "\r\n"}
	qp := bodyFixture{header: "Content-Transfer-Encoding: quoted-printable\r\n",
		wire: "soft=\r\nbreak =41=42 and=\r\n more=3D\r\n"}
	child64 := base64.StdEncoding.EncodeToString([]byte("second child, decoded"))
	tree := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"preamble\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\none one one\r\n" +
		"--B\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\n" + child64 + "\r\n" +
		"--B\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nchild=3Dqp soft=\r\nbreak\r\n" +
		"--B\r\nContent-Type: multipart/alternative; boundary=C\r\n\r\n" +
		"--C\r\nContent-Type: text/plain\r\n\r\nnested\r\n--C--\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nlast\r\n" +
		"--B--\r\nepilogue\r\n"
	return []sweepFixture{
		{name: "plain", text: plain.header + "\r\n" + plain.wire},
		{name: "base64", text: b64.header + "\r\n" + b64.wire},
		{name: "quoted-printable", text: qp.header + "\r\n" + qp.wire},
		{name: "tree", text: tree},
		{name: "tree skipping", text: tree, skip: "[1]"},
	}
}

type sweepPart struct {
	path      string
	body      string
	err       error // what ended the body, nil for io.EOF
	cancelled bool  // the source had cancelled when the body ended
	reader    io.Reader
}

type sweepRun struct {
	parseErr         error
	cancelledInParse bool
	entity           *Entity
	parts            []sweepPart
	walkErr          error
	session          *ParseSession
}

// sweepConsume walks e as the sweep consumer does: every leaf body read in
// reads of three bytes, containers left to the walk, the skip part unread.
func sweepConsume(e *Entity, skip string, cancelled func() bool) ([]sweepPart, error) {
	var parts []sweepPart
	err := e.Walk(func(path []int, part *Entity, err error) error {
		key := fmt.Sprint(path)
		if part.MultipartReader() != nil || key == skip {
			return nil
		}
		body, berr := readBy(part.Body, 3)
		if berr == io.EOF {
			berr = nil
		}
		parts = append(parts, sweepPart{path: key, body: string(body), err: berr, cancelled: cancelled(), reader: part.Body})
		return nil
	})
	return parts, err
}

func sweepReference(t *testing.T, f sweepFixture) []sweepPart {
	t.Helper()
	_, e, err := Parse(strings.NewReader(f.text), nil)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := sweepConsume(e, f.skip, func() bool { return false })
	if err != nil {
		t.Fatalf("%s: the uncut reference = %v", f.name, err)
	}
	return parts
}

// sweepCutReference is the consumption of Parse given exactly prefix and then
// an ordinary read failure: what a parse that was cut there, for a reason that
// is not a cancellation, delivers. It is memoized by the length of prefix.
type sweepCutReference struct {
	f     sweepFixture
	cache map[int][]sweepPart
}

func (r *sweepCutReference) at(n int) []sweepPart {
	if parts, ok := r.cache[n]; ok {
		return parts
	}
	var parts []sweepPart
	_, e, _ := Parse(io.MultiReader(strings.NewReader(r.f.text[:n]), iotestErrReader{errCut}), nil)
	if e != nil {
		parts, _ = sweepConsume(e, r.f.skip, func() bool { return false })
	}
	r.cache[n] = parts
	return parts
}

func TestParseContext_cancellationSweep(t *testing.T) {
	for _, f := range sweepFixtures() {
		ref := sweepReference(t, f)
		cut := &sweepCutReference{f: f, cache: map[int][]sweepPart{}}
		runs := 0
		for _, chunk := range []int{1, 7, 4096} {
			for _, mode := range []cutMode{cutNil, cutEOF, cutErr} {
				for at := 1; at <= len(f.text); at++ {
					runs++
					name := fmt.Sprintf("%s chunk %d %s at %d", f.name, chunk, mode, at)
					ctx, src := newCut(t, f.text, chunk, at, mode)
					run := sweepRun{}
					run.session, run.entity, run.parseErr = ParseContext(ctx, src, nil)
					run.cancelledInParse = src.cancelled
					if run.entity != nil {
						run.parts, run.walkErr = sweepConsume(run.entity, f.skip, func() bool { return src.cancelled })
					}
					checkSweep(t, name, ref, cut, src, run)
					if t.Failed() {
						return
					}
				}
			}
		}
		if runs == 0 {
			t.Fatalf("%s: no runs", f.name)
		}
	}
}

func checkSweep(t *testing.T, name string, ref []sweepPart, cut *sweepCutReference, src *cutSource, run sweepRun) {
	t.Helper()
	if src.after != 0 {
		t.Errorf("%s: the input was read %d times after the cancelling read", name, src.after)
	}
	if run.session.Len() != 0 {
		t.Errorf("%s: diagnostics %v, want none", name, codesOfDiags(run.session.Diagnostics()))
	}

	if !src.cancelled {
		// Never reached: exactly the reference.
		if run.parseErr != nil || run.walkErr != nil || run.session.fatalErr() != nil {
			t.Errorf("%s: not cancelled, yet parse %v, walk %v, stop %v", name, run.parseErr, run.walkErr, run.session.fatalErr())
		}
		if len(run.parts) != len(ref) {
			t.Fatalf("%s: %d parts, want the reference's %d", name, len(run.parts), len(ref))
		}
		for i, p := range run.parts {
			if p.path != ref[i].path || p.body != ref[i].body || p.err != nil {
				t.Errorf("%s: part %s %q %v, want %s %q", name, p.path, p.body, p.err, ref[i].path, ref[i].body)
			}
			if n, err := p.reader.Read(nil); n != 0 || err != nil {
				t.Errorf("%s: empty read of %s = %d, %v before cancellation", name, p.path, n, err)
			}
		}
		return
	}

	stop := error(run.session.fatalErr())
	checkCancelStop(t, name, stop, context.Canceled)

	if run.cancelledInParse {
		if run.parseErr != stop {
			t.Errorf("%s: cancelled during Parse, which returned %v, want the stop", name, run.parseErr)
		}
	} else if run.parseErr != nil {
		t.Errorf("%s: Parse returned %v before the cancellation", name, run.parseErr)
	}
	if run.entity == nil {
		if !run.cancelledInParse {
			t.Errorf("%s: no entity without a cancellation during Parse", name)
		}
		return
	}
	if run.walkErr != stop {
		t.Errorf("%s: Walk returned %v, want the stop", name, run.walkErr)
	}
	if got := run.entity.ParseStop(); got != stop {
		t.Errorf("%s: ParseStop() = %v, want the stop", name, got)
	}
	var out bytes.Buffer
	if err := run.entity.WriteTo(&out); err != stop || out.Len() != 0 {
		t.Errorf("%s: WriteTo() = %v, %d bytes; want the stop and nothing written", name, err, out.Len())
	}
	if n, err := run.entity.Body.Read(make([]byte, 8)); n != 0 || err != stop {
		t.Errorf("%s: root read after cancellation = %d, %v", name, n, err)
	}

	// Every part but the last ended before the cancellation, since only one
	// read is in progress when it comes and every later read reports the stop;
	// such a part is exactly the uncut reference's. The part that ended after it,
	// if any, is the last, and is checked against the cut reference.
	cutRef := cut.at(src.pos)
	if len(run.parts) > len(ref) || len(run.parts) > len(cutRef) {
		t.Fatalf("%s: %d parts, more than the reference's %d or the cut reference's %d", name, len(run.parts), len(ref), len(cutRef))
	}
	for i, p := range run.parts {
		if p.path != ref[i].path || p.path != cutRef[i].path {
			t.Errorf("%s: part %d is %s, want %s (cut: %s)", name, i, p.path, ref[i].path, cutRef[i].path)
			continue
		}
		if !p.cancelled {
			if p.body != ref[i].body || p.err != nil {
				t.Errorf("%s: part %s ended before the cancellation with %q %v, want %q", name, p.path, p.body, p.err, ref[i].body)
			}
		} else {
			if i != len(run.parts)-1 {
				t.Errorf("%s: part %s ended after the cancellation but is not the last", name, p.path)
			}
			if p.err != stop {
				t.Errorf("%s: part %s ended with %v after the cancellation, want the stop", name, p.path, p.err)
			}
			if !strings.HasPrefix(cutRef[i].body, p.body) {
				t.Errorf("%s: part %s delivered %q, not a prefix of %q, what Parse delivers from the same %d input bytes", name, p.path, p.body, cutRef[i].body, src.pos)
			}
		}
		if n, err := p.reader.Read(nil); n != 0 || err != stop {
			t.Errorf("%s: empty read of %s after cancellation = %d, %v", name, p.path, n, err)
		}
	}
}

// TestParseContext_cutBeforeDelimiterKeepsCR pins the case the cut reference
// exists for: a cancellation that leaves the input ending between the '\r' and
// the '\n' of a part's closing delimiter line. The '\r' is body as far as the
// framing could tell — Parse delivers it too, from the same bytes followed by
// an ordinary failure — and the cancelled body delivers it and ends with the
// stop, rather than a complete body the input never proved.
func TestParseContext_cutBeforeDelimiterKeepsCR(t *testing.T) {
	text := sweepFixtures()[3].text
	at := strings.Index(text, "one one one\r\n") + len("one one one\r")
	_, e, err := Parse(io.MultiReader(strings.NewReader(text[:at]), iotestErrReader{errCut}), nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.MultipartReader().NextPart()
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := io.ReadAll(p.Body)
	if string(ref) != "one one one\r" {
		t.Fatalf("Parse of the cut input delivered %q, want the body and its unproven '\\r'", ref)
	}

	ctx, src := newCut(t, text, 1, at, cutNil)
	_, e, err = ParseContext(ctx, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err = e.MultipartReader().NextPart()
	if err != nil {
		t.Fatal(err)
	}
	got, err := readBy(p.Body, 3)
	if string(got) != string(ref) || err == nil || err != e.ParseStop() {
		t.Errorf("cancelled body = %q, %v; want %q and the stop", got, err, ref)
	}
	if src.after != 0 {
		t.Errorf("%d reads after the cancelling read", src.after)
	}
}

// TestParseContext_containerRawSweep reads the raw view of a multipart
// container — and the raw copy WriteTo makes of it — under cancellation: what
// either delivers is an exact prefix of the input's bytes, even of bytes the
// buffered reader already held, and it ends with the stop.
func TestParseContext_containerRawSweep(t *testing.T) {
	f := sweepFixtures()[3]
	header := f.text[:strings.Index(f.text, "\r\n\r\n")+4]
	raw := f.text[len(header):]
	for _, chunk := range []int{1, 7, 4096} {
		for _, mode := range []cutMode{cutNil, cutEOF, cutErr} {
			for at := 1; at <= len(f.text); at++ {
				name := fmt.Sprintf("chunk %d %s at %d", chunk, mode, at)

				ctx, src := newCut(t, f.text, chunk, at, mode)
				_, e, err := ParseContext(ctx, src, nil)
				if e == nil {
					if !src.cancelled {
						t.Fatalf("%s: no entity, %v", name, err)
					}
					continue
				}
				body, berr := readBy(e.Body, 5)
				if !strings.HasPrefix(raw, string(body)) {
					t.Fatalf("%s: raw view %q is not a prefix of the input", name, body)
				}
				switch {
				case src.cancelled:
					if stop := e.ParseStop(); stop == nil || berr != stop {
						t.Fatalf("%s: raw view ended with %v, want the stop %v", name, berr, stop)
					}
				case berr != io.EOF || string(body) != raw:
					t.Fatalf("%s: raw view %q, %v without a cancellation", name, body, berr)
				}
				if src.after != 0 {
					t.Fatalf("%s: %d reads after the cancelling read", name, src.after)
				}

				// The verbatim copy.
				ctx, src = newCut(t, f.text, chunk, at, mode)
				_, e, err = ParseContext(ctx, src, nil)
				if e == nil {
					continue
				}
				var out bytes.Buffer
				werr := e.WriteTo(&out)
				switch {
				case src.cancelled && err != nil:
					// Cancelled before WriteTo began: nothing is written.
					if werr != err || out.Len() != 0 {
						t.Fatalf("%s: WriteTo() = %v, %d bytes; want the stop and nothing", name, werr, out.Len())
					}
				case src.cancelled:
					stop := e.ParseStop()
					if werr != stop || stop == nil {
						t.Fatalf("%s: WriteTo() = %v, want the stop %v", name, werr, stop)
					}
					w := out.String()
					cut := strings.Index(w, "\r\n\r\n")
					if cut < 0 || !strings.HasPrefix(raw, w[cut+4:]) {
						t.Fatalf("%s: WriteTo() wrote %q, whose body is not a prefix of the input", name, w)
					}
				case werr != nil:
					t.Fatalf("%s: WriteTo() = %v without a cancellation", name, werr)
				}
				if src.after != 0 {
					t.Fatalf("%s: %d reads after the cancelling read", name, src.after)
				}
			}
		}
	}
}
