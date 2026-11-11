package message

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

// The epilogue of a multipart under Parse (ROB-060-001). Every source is a
// short string; every iteration is capped.

// epiSources are the two ways a test input arrives: whole, and one byte per
// read.
var epiSources = map[string]func(string) io.Reader{
	"whole":    func(s string) io.Reader { return strings.NewReader(s) },
	"one byte": func(s string) io.Reader { return iotest.OneByteReader(strings.NewReader(s)) },
}

// epiIterate reads every part of mr and its body, at most 16 parts of at most
// 64 KiB, and returns what ended the iteration.
func epiIterate(t *testing.T, mr MultipartReader) error {
	t.Helper()
	for i := 0; i < 16; i++ {
		p, err := mr.NextPart()
		if err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, io.LimitReader(p.Body, 64<<10)); err != nil {
			return err
		}
	}
	t.Fatal("more parts than the test's cap")
	return nil
}

func TestMultipartEpilogue(t *testing.T) {
	const head = "Content-Type: multipart/mixed; boundary=B\r\n\r\n"
	const h = head + "--B\r\n\r\none\r\n--B--"
	tests := []struct {
		name, text string
		limit      int64
		want       string
		cut, ok    bool
		codes      []Code
	}{
		{"empty", h, 0, "", false, true, nil},
		{"empty after the line ending", h + "\r\n", 0, "", false, true, nil},
		{"blank", h + "\r\n\r\n \t\n", 0, "\r\n \t\n", false, true, nil},
		{"non-empty", h + "\r\nthe epilogue\r\nmore", 0, "the epilogue\r\nmore", false, true, nil},
		{"at the limit", h + "\r\n0123", 4, "0123", false, true, nil},
		{"limited", h + "\r\n0123456789", 4, "0123", true, true, []Code{CodeLimitEpilogueBytes}},
		{"disabled limit keeps the default", h + "\r\nxyz", LimitDisabled, "xyz", false, true, nil},
		{"unclosed", head + "--B\r\n\r\none\r\n", 0, "", false, false, []Code{CodeMultipartCloseNotFound}},
		{"inferred boundary", "Content-Type: multipart/mixed\r\n\r\n--X\r\n\r\none\r\n--X--\r\ntail\r\n", 0, "tail\r\n", false, true, []Code{CodeMultipartBoundaryInferred}},
	}
	for _, tc := range tests {
		for srcName, mk := range epiSources {
			name := tc.name + "/" + srcName
			s, e, err := Parse(mk(tc.text), &ParseOptions{Limits: Limits{MaxEpilogueBytes: tc.limit}})
			if err != nil {
				t.Fatalf("%s: Parse() = %v", name, err)
			}
			mr := e.MultipartReader()
			if _, _, ok := MultipartEpilogue(mr); ok {
				t.Errorf("%s: MultipartEpilogue() ok before the end of the iteration", name)
			}
			if err := epiIterate(t, mr); err != io.EOF {
				t.Fatalf("%s: iteration ended with %v, want io.EOF", name, err)
			}
			before := s.Completion()
			got, cut, ok := MultipartEpilogue(mr)
			if string(got) != tc.want || cut != tc.cut || ok != tc.ok {
				t.Errorf("%s: MultipartEpilogue() = (%q, %v, %v), want (%q, %v, %v)", name, got, cut, ok, tc.want, tc.cut, tc.ok)
			}
			// A later call reads nothing and returns the same bytes, in a copy
			// of its own.
			if len(got) > 0 {
				got[0] ^= 0xff
			}
			read := s.Completion().InputBytes
			again, cut2, ok2 := MultipartEpilogue(mr)
			if string(again) != tc.want || cut2 != tc.cut || ok2 != tc.ok {
				t.Errorf("%s: second MultipartEpilogue() = (%q, %v, %v)", name, again, cut2, ok2)
			}
			after := s.Completion()
			if after.InputBytes != read {
				t.Errorf("%s: second call read %d bytes", name, after.InputBytes-read)
			}
			// Completion is what it was, but for the limit diagnostic.
			wantState, diags := before.State, after.Diagnostics
			if tc.cut {
				wantState = CompletionLimited
				diags[SeverityLimitExceeded]--
			}
			if after.State != wantState || diags != before.Diagnostics || after.InputErr != nil || after.Stop != nil {
				t.Errorf("%s: Completion %+v, before %+v", name, after, before)
			}
			if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, tc.codes) {
				t.Errorf("%s: diagnostics %v, want %v", name, codes, tc.codes)
			}
			if tc.cut {
				d := s.Diagnostics()[len(s.Diagnostics())-1]
				if at := int64(len(h) + 2 + 4); d.Location != LocationAt(at) || d.Severity != SeverityLimitExceeded {
					t.Errorf("%s: %v at %+v, want SeverityLimitExceeded at %d", name, d.Severity, d.Location, at)
				}
			}
		}
	}
}

// A nested multipart's epilogue ends where its enclosing part ends; the outer
// iteration goes on after it.
func TestMultipartEpilogue_nested(t *testing.T) {
	const text = "Content-Type: multipart/mixed; boundary=Out\r\n\r\n" +
		"--Out\r\nContent-Type: multipart/alternative; boundary=In\r\n\r\n" +
		"--In\r\n\r\nx\r\n--In--\r\ninner epilogue\r\n" +
		"--Out\r\n\r\nsecond\r\n--Out--\r\nouter epilogue\r\n"
	for srcName, mk := range epiSources {
		s, e, err := Parse(mk(text), nil)
		if err != nil {
			t.Fatal(err)
		}
		outer := e.MultipartReader()
		p, err := outer.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		inner := p.MultipartReader()
		if err := epiIterate(t, inner); err != io.EOF {
			t.Fatalf("%s: inner iteration ended with %v", srcName, err)
		}
		if got, cut, ok := MultipartEpilogue(inner); string(got) != "inner epilogue" || cut || !ok {
			t.Errorf("%s: inner MultipartEpilogue() = (%q, %v, %v)", srcName, got, cut, ok)
		}
		p, err = outer.NextPart()
		if err != nil {
			t.Fatalf("%s: outer NextPart() after the inner epilogue = %v", srcName, err)
		}
		if b, _ := io.ReadAll(p.Body); string(b) != "second" {
			t.Errorf("%s: second part %q", srcName, b)
		}
		if _, err := outer.NextPart(); err != io.EOF {
			t.Fatalf("%s: outer NextPart() = %v, want io.EOF", srcName, err)
		}
		if got, cut, ok := MultipartEpilogue(outer); string(got) != "outer epilogue\r\n" || cut || !ok {
			t.Errorf("%s: outer MultipartEpilogue() = (%q, %v, %v)", srcName, got, cut, ok)
		}
		if c := s.Completion(); c.State != CompletionComplete || len(s.Diagnostics()) != 0 {
			t.Errorf("%s: Completion %v, diagnostics %v", srcName, c.State, codesOfDiags(s.Diagnostics()))
		}
	}
}

// Iterators that do not heal keep no epilogue.
func TestMultipartEpilogue_none(t *testing.T) {
	const text = "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\none\r\n--B--\r\nepilogue\r\n"
	_, e, err := Parse(strings.NewReader(text), &ParseOptions{Profile: ProfileStrict})
	if err != nil {
		t.Fatal(err)
	}
	mr := e.MultipartReader()
	if err := epiIterate(t, mr); err != io.EOF {
		t.Fatal(err)
	}
	if _, _, ok := MultipartEpilogue(mr); ok {
		t.Error("strict: MultipartEpilogue() ok")
	}

	e, err = Read(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	mr = e.MultipartReader()
	if err := epiIterate(t, mr); err != io.EOF {
		t.Fatal(err)
	}
	if _, _, ok := MultipartEpilogue(mr); ok {
		t.Error("Read: MultipartEpilogue() ok")
	}

	var h Header
	h.Set("Content-Type", "multipart/mixed; boundary=B")
	e, err = NewMultipart(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	mr = e.MultipartReader()
	if _, err := mr.NextPart(); err != io.EOF {
		t.Fatal(err)
	}
	if _, _, ok := MultipartEpilogue(mr); ok {
		t.Error("NewMultipart: MultipartEpilogue() ok")
	}
	if _, _, ok := MultipartEpilogue(nil); ok {
		t.Error("nil: MultipartEpilogue() ok")
	}
}

// Under ProfileForensic the root's epilogue is read through the trailing
// input's reader, which accounts for it and goes on after it.
func TestMultipartEpilogue_forensicRoot(t *testing.T) {
	const h = "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\none\r\n--B--\r\n"
	const tail = "From: a@example.com\r\nTo: b@example.com\r\n\r\nsecond\r\n"
	for srcName, mk := range epiSources {
		for _, limit := range []int64{0, 4} {
			s, e, err := Parse(mk(h+tail), &ParseOptions{Profile: ProfileForensic, Limits: Limits{MaxEpilogueBytes: limit}})
			if err != nil {
				t.Fatal(err)
			}
			mr := e.MultipartReader()
			if err := epiIterate(t, mr); err != io.EOF {
				t.Fatalf("%s: iteration ended with %v", srcName, err)
			}
			got, cut, ok := MultipartEpilogue(mr)
			want, wantCut, delivered, status := tail, false, int64(len(tail)), TrailingEnded
			if limit > 0 {
				want, wantCut, delivered, status = tail[:limit], true, limit+1, TrailingOpen
			}
			if string(got) != want || cut != wantCut || !ok {
				t.Errorf("%s/%d: MultipartEpilogue() = (%q, %v, %v)", srcName, limit, got, cut, ok)
			}
			if ti := s.TrailingInput(); ti.Status() != status || ti.Delivered() != delivered {
				t.Errorf("%s/%d: TrailingInput %v, %d delivered; want %v, %d", srcName, limit, ti.Status(), ti.Delivered(), status, delivered)
			}
			rd, ok := s.TrailingInputReader()
			if !ok {
				t.Fatalf("%s/%d: no trailing input reader", srcName, limit)
			}
			rest, err := readCapped(t, rd, 7)
			if rest != tail[delivered:] || err != io.EOF {
				t.Errorf("%s/%d: trailing input reader went on with (%q, %v), want %q", srcName, limit, rest, err, tail[delivered:])
			}
			ti := s.TrailingInput()
			if ev, _ := ti.Evidence(); ti.Status() != TrailingEnded || ti.Delivered() != int64(len(tail)) || ev != TrailingEvidenceMessageLike {
				t.Errorf("%s/%d: TrailingInput %v, %d delivered, %v", srcName, limit, ti.Status(), ti.Delivered(), ev)
			}
			wantCodes := []Code{CodeInputTrailingMessages}
			if limit > 0 {
				wantCodes = []Code{CodeLimitEpilogueBytes, CodeInputTrailingMessages}
			}
			if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, wantCodes) {
				t.Errorf("%s/%d: diagnostics %v, want %v", srcName, limit, codes, wantCodes)
			}
		}
	}
}

// An input error, or a stop of the parse, ends the epilogue there; the error
// stays the session's and adds no diagnostic.
func TestMultipartEpilogue_ended(t *testing.T) {
	const h = "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\none\r\n--B--\r\n"
	boom := errors.New("boom")
	s, e, err := Parse(io.MultiReader(strings.NewReader(h+"epi"), iotest.ErrReader(boom)), nil)
	if err != nil {
		t.Fatal(err)
	}
	mr := e.MultipartReader()
	if err := epiIterate(t, mr); err != io.EOF {
		t.Fatal(err)
	}
	if got, cut, ok := MultipartEpilogue(mr); string(got) != "epi" || cut || !ok {
		t.Errorf("input error: MultipartEpilogue() = (%q, %v, %v)", got, cut, ok)
	}
	if c := s.Completion(); c.InputErr != boom || len(s.Diagnostics()) != 0 {
		t.Errorf("input error: InputErr %v, diagnostics %v", c.InputErr, codesOfDiags(s.Diagnostics()))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, e, err = ParseContext(ctx, strings.NewReader(h+"epilogue"), nil)
	if err != nil {
		t.Fatal(err)
	}
	mr = e.MultipartReader()
	if err := epiIterate(t, mr); err != io.EOF {
		t.Fatal(err)
	}
	cancel()
	if got, cut, ok := MultipartEpilogue(mr); len(got) != 0 || cut || !ok {
		t.Errorf("cancelled: MultipartEpilogue() = (%q, %v, %v), want nothing read", got, cut, ok)
	}
	if c := s.Completion(); c.State != CompletionCanceled || codesOfDiags(s.Diagnostics()) != nil {
		t.Errorf("cancelled: Completion %v, diagnostics %v", c.State, codesOfDiags(s.Diagnostics()))
	}
}
