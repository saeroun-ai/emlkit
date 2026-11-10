package message

import (
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

// treeOf parses text, walks it, and returns each leaf as "path type|body",
// the diagnostic codes recorded, and the error the walk ended with.
func treeOf(t *testing.T, src io.Reader, opts *ParseOptions) (leaves []string, codes []Code, werr error) {
	t.Helper()
	s, e, err := Parse(src, opts)
	if e == nil {
		t.Fatalf("Parse() = %v", err)
	}
	werr = e.Walk(func(path []int, p *Entity, err error) error {
		if err != nil {
			return err
		}
		if p.MultipartReader() != nil {
			return nil
		}
		mt, _, _ := p.EffectiveContentType()
		b, rerr := io.ReadAll(p.Body)
		if rerr != nil {
			return rerr
		}
		leaves = append(leaves, fmt.Sprintf("%v %s|%s", path, mt, b))
		return nil
	})
	return leaves, codesOfDiags(s.Diagnostics()), werr
}

func TestParse_multipartRecovery(t *testing.T) {
	const h = "Content-Type: multipart/mixed; boundary=Bound\r\n\r\n"
	const p1 = "--Bound\r\nContent-Type: text/plain\r\n\r\none\r\n"
	const p2 = "--Bound\r\nContent-Type: text/html\r\n\r\ntwo\r\n"
	tests := []struct {
		name, text string
		leaves     []string
		codes      []Code
	}{
		{"conformant", h + "preamble\r\n" + p1 + p2 + "--Bound--\r\nepilogue\r\n",
			[]string{"[0] text/plain|one", "[1] text/html|two"}, nil},
		{"missing close", h + p1 + p2,
			[]string{"[0] text/plain|one", "[1] text/html|two\r\n"}, []Code{CodeMultipartCloseNotFound}},
		{"missing close and final line ending", h + p1 + "--Bound\r\nContent-Type: text/html\r\n\r\ntwo",
			[]string{"[0] text/plain|one", "[1] text/html|two"}, []Code{CodeMultipartCloseNotFound}},
		{"input ends after an opening delimiter", h + p1 + "--Bound\r\n",
			[]string{"[0] text/plain|one", "[1] text/plain|"}, []Code{CodeMultipartCloseNotFound}},
		{"input ends at an opening delimiter", h + p1 + "--Bound",
			[]string{"[0] text/plain|one"}, []Code{CodeMultipartCloseNotFound}},
		{"close cut by one byte", h + p1 + p2 + "--Bound-",
			[]string{"[0] text/plain|one", "[1] text/html|two"}, []Code{CodeMultipartCloseTruncated}},
		{"delimiter cut inside the boundary", h + p1 + p2 + "--Boun",
			[]string{"[0] text/plain|one", "[1] text/html|two"}, []Code{CodeMultipartCloseNotFound}},
		{"short cut is body", h + p1 + p2 + "--Bo",
			[]string{"[0] text/plain|one", "[1] text/html|two\r\n--Bo"}, []Code{CodeMultipartCloseNotFound}},
		{"no delimiter at all", h + "just text\r\nno parts\r\n",
			[]string{"[0] text/plain|just text\r\nno parts\r\n"}, []Code{CodeMultipartStartNotFound}},
		{"empty body", h, nil, []Code{CodeMultipartEmpty}},
		{"only a close", h + "--Bound--\r\n", nil, []Code{CodeMultipartEmpty}},
		{"boundary not declared", "Content-Type: multipart/mixed\r\n\r\n--X1\r\nContent-Type: text/plain\r\n\r\none\r\n--X1\r\n\r\ntwo\r\n--X1--\r\n",
			[]string{"[0] text/plain|one", "[1] text/plain|two"}, []Code{CodeMultipartBoundaryInferred}},
		{"boundary declared empty", "Content-Type: multipart/mixed; boundary=\"\"\r\n\r\n--X1\r\n\r\none\r\n--X1--\r\n",
			[]string{"[0] text/plain|one"}, []Code{CodeMultipartBoundaryInferred}},
		{"boundary mismatch", "Content-Type: multipart/mixed; boundary=WRONG\r\n\r\npre\r\n--X1\r\nContent-Type: text/plain\r\n\r\none\r\n--X1\r\n\r\ntwo\r\n--X1--\r\n",
			[]string{"[0] text/plain|one", "[1] text/plain|two"}, []Code{CodeMultipartBoundaryMismatch}},
		{"mismatch with an unclosed body", "Content-Type: multipart/mixed; boundary=WRONG\r\n\r\n--X1\r\nContent-Type: text/plain\r\n\r\none\r\n--X1\r\nContent-Type: text/html\r\n\r\ntwo",
			[]string{"[0] text/plain|one", "[1] text/html|two"}, []Code{CodeMultipartBoundaryMismatch, CodeMultipartCloseNotFound}},
		{"dashes in text are no boundary", h + "a line\r\n----------\r\n\r\n-- \r\nsig\r\n",
			[]string{"[0] text/plain|a line\r\n----------\r\n\r\n-- \r\nsig\r\n"}, []Code{CodeMultipartStartNotFound}},
		{"inner multipart with no close", h + "--Bound\r\nContent-Type: multipart/alternative; boundary=In\r\n\r\n--In\r\n\r\ninner\r\n--Bound\r\n\r\nouter\r\n--Bound--\r\n",
			[]string{"[0 0] text/plain|inner", "[1] text/plain|outer"}, []Code{CodeMultipartCloseNotFound}},
		{"inner multipart reusing the boundary", h + "--Bound\r\nContent-Type: multipart/alternative; boundary=Bound\r\n\r\n--Bound\r\n\r\ninner\r\n--Bound--\r\n",
			[]string{"[1] text/plain|inner"}, []Code{CodeMultipartNestedBoundaryReused, CodeMultipartEmpty}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, profile := range []Profile{ProfileTolerant, ProfileForensic} {
				for name, src := range map[string]io.Reader{
					"whole":    strings.NewReader(tc.text),
					"one byte": iotest.OneByteReader(strings.NewReader(tc.text)),
				} {
					leaves, codes, werr := treeOf(t, src, &ParseOptions{Profile: profile})
					if werr != nil {
						t.Errorf("%v/%s: Walk() = %v, want the healed tree", profile, name, werr)
					}
					if !reflect.DeepEqual(leaves, tc.leaves) {
						t.Errorf("%v/%s: leaves %q\nwant %q", profile, name, leaves, tc.leaves)
					}
					if !reflect.DeepEqual(codes, tc.codes) {
						t.Errorf("%v/%s: diagnostics %v, want %v", profile, name, codes, tc.codes)
					}
				}
			}
		})
	}
}

// TestParse_multipartRecoveryStrictUnchanged pins that ProfileStrict and Read
// frame as before: an unclosed multipart ends its last part with the
// missing-close error, and a body with no delimiter yields no part.
func TestParse_multipartRecoveryStrictUnchanged(t *testing.T) {
	const text = "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\none\r\n--B\r\n\r\ntwo"
	_, _, werr := treeOf(t, strings.NewReader(text), &ParseOptions{Profile: ProfileStrict})
	if werr == nil || !strings.Contains(werr.Error(), "missing closing boundary") {
		t.Errorf("strict: Walk() = %v, want the missing closing boundary", werr)
	}
	e, err := Read(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	mr := e.MultipartReader()
	mr.NextPart()
	p, _ := mr.NextPart()
	if _, rerr := io.ReadAll(p.Body); rerr == nil || !strings.Contains(rerr.Error(), "missing closing boundary") {
		t.Errorf("Read: body = %v, want the missing closing boundary", rerr)
	}
}

func TestMultipartPreamble(t *testing.T) {
	const text = "Content-Type: multipart/mixed; boundary=B\r\n\r\nThis is a preamble.\r\nTwo lines.\r\n--B\r\n\r\none\r\n--B--\r\n"
	_, e, err := Parse(strings.NewReader(text), nil)
	if err != nil {
		t.Fatal(err)
	}
	mr := e.MultipartReader()
	if _, err := mr.NextPart(); err != nil {
		t.Fatal(err)
	}
	pre, cut, ok := MultipartPreamble(mr)
	if string(pre) != "This is a preamble.\r\nTwo lines.\r\n" || cut || !ok {
		t.Errorf("MultipartPreamble() = (%q, %v, %v)", pre, cut, ok)
	}

	// A preamble past MaxPreambleBytes is cut there, reported, and ends
	// nothing: the parts after it are still read.
	long := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" + strings.Repeat("0123456789\r\n", 100) + "--B\r\n\r\none\r\n--B--\r\n"
	s, e, err := Parse(strings.NewReader(long), &ParseOptions{Limits: Limits{MaxPreambleBytes: 64}})
	if err != nil {
		t.Fatal(err)
	}
	mr = e.MultipartReader()
	p, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(p.Body); string(b) != "one" {
		t.Errorf("part body %q, want one", b)
	}
	if _, err := mr.NextPart(); err != io.EOF {
		t.Errorf("NextPart() = %v, want io.EOF", err)
	}
	pre, cut, ok = MultipartPreamble(mr)
	if len(pre) != 64 || !cut || !ok {
		t.Errorf("MultipartPreamble() = (%d bytes, %v, %v), want 64 bytes, cut", len(pre), cut, ok)
	}
	if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeLimitPreambleBytes}) {
		t.Errorf("diagnostics %v, want limit.preamble_bytes", codes)
	}

	// Strict and Read keep none.
	_, e, _ = Parse(strings.NewReader(text), &ParseOptions{Profile: ProfileStrict})
	if _, _, ok := MultipartPreamble(e.MultipartReader()); ok {
		t.Error("strict: MultipartPreamble() ok, want none kept")
	}
}

func TestInferBoundary(t *testing.T) {
	tests := []struct {
		name, body, declared, want string
		ambiguous                  bool
	}{
		{"closed", "--A1\r\n\r\nx\r\n--A1--\r\n", "", "A1", false},
		{"mime headed, unclosed", "--A1\r\nContent-Type: text/plain\r\n\r\nx\r\n", "", "A1", false},
		{"two openings with headers", "--A1\r\nX: 1\r\n\r\nx\r\n--A1\r\n\r\ny\r\n", "", "A1", false},
		{"single opening followed by text", "--A1\r\nhello\r\n", "", "", false},
		{"dashes only", "----------\r\n\r\ntext\r\n----------\r\n", "", "", false},
		{"signature separator", "text\r\n-- \r\nname\r\n", "", "", false},
		{"case differs from the declared one", "--a1\r\n\r\nx\r\n--a1--\r\n--Z9\r\n\r\ny\r\n--Z9--\r\n", "A1", "a1", true},
		{"two boundaries, earliest wins", "--Z9\r\n\r\ny\r\n--Z9--\r\n--A1\r\n\r\nx\r\n--A1--\r\n", "", "Z9", true},
		{"nested: the outer one comes first", "--Out\r\nContent-Type: multipart/alternative; boundary=In\r\n\r\n--In\r\n\r\nx\r\n--In--\r\n--Out--\r\n", "", "Out", true},
		{"too long", "--" + strings.Repeat("x", 71) + "\r\n\r\nx\r\n--" + strings.Repeat("x", 71) + "--\r\n", "", "", false},
	}
	for _, tc := range tests {
		got, ambiguous := inferBoundary([]byte(tc.body), tc.declared)
		if got != tc.want || ambiguous != tc.ambiguous {
			t.Errorf("%s: inferBoundary() = (%q, %v), want (%q, %v)", tc.name, got, ambiguous, tc.want, tc.ambiguous)
		}
	}
	// Many candidates and odd bytes are handled within the bound.
	var many strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&many, "--cand%d\r\n\r\n", i)
	}
	inferBoundary([]byte(many.String()), "")
	inferBoundary([]byte("--\x00\xff\r\n--\r\n----\r\n"), "\x00")
}
