package message

import (
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

// nbMP returns a multipart entity of media type multipart/mt and boundary b
// holding parts, each a part's header and body, closed unless open.
func nbMP(mt, b string, open bool, parts ...string) string {
	var s strings.Builder
	fmt.Fprintf(&s, "Content-Type: multipart/%s; boundary=%q\r\n\r\n", mt, b)
	for _, p := range parts {
		s.WriteString("--" + b + "\r\n" + p + "\r\n")
	}
	if !open {
		s.WriteString("--" + b + "--\r\n")
	}
	return s.String()
}

// nbLeaf returns a part of media type mt holding body.
func nbLeaf(mt, body string) string {
	return "Content-Type: " + mt + "\r\n\r\n" + body
}

// nbNestedWalk parses src, walks the whole tree and returns each leaf as
// "path type|body", followed by its header's orphan records if it has any,
// each diagnostic as "code path", and the error the walk ended with.
func nbNestedWalk(t *testing.T, src io.Reader, profile Profile) (leaves, diags []string, werr error) {
	t.Helper()
	s, e, err := Parse(src, &ParseOptions{Profile: profile})
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
		leaf := fmt.Sprintf("%v %s|%s", path, mt, b)
		for _, o := range p.Header.OrphanContinuations() {
			leaf += fmt.Sprintf(" orphan=%q", o.Bytes())
		}
		leaves = append(leaves, leaf)
		return nil
	})
	for _, d := range s.Diagnostics() {
		diags = append(diags, fmt.Sprintf("%s %v", d.Code, d.Path))
	}
	return leaves, diags, werr
}

// TestParse_nestedBoundaries walks multiparts nested 2 to 4 deep whose
// boundaries are prefixes of one another, differ by a little, or are the same
// (ROB-060-011). No body is lost: where a nested multipart's delimiter lines
// are an enclosing one's, its parts are read as that one's, reported.
func TestParse_nestedBoundaries(t *testing.T) {
	plain, html, pdf := nbLeaf("text/plain", "plain"), nbLeaf("text/html", "html"), nbLeaf("application/pdf", "pdf")
	png := nbLeaf("image/png", "png")
	two := func(out, in string) string {
		return nbMP("mixed", out, false, nbMP("alternative", in, false, plain, html), pdf)
	}
	framed := []string{"[0 0] text/plain|plain", "[0 1] text/html|html", "[1] application/pdf|pdf"}
	flat := []string{"[1] text/plain|plain", "[2] text/html|html", "[3] application/pdf|pdf"}
	reused := []string{"multipart.nested_boundary_reused [0]", "multipart.empty [0]"}
	tests := []struct {
		name, text string
		leaves     []string
		diags      []string
	}{
		{"distinct", two("OUT", "IN"), framed, nil},
		{"inner a prefix of the outer", two("BB", "B"), framed, nil},
		{"outer a prefix of the inner", two("B", "BB"), framed, nil},
		{"inner adds a dash", two("B", "B-"), framed, nil},
		{"outer adds a dash", two("B-", "B"), framed, nil},
		{"case differs", two("b", "B"), framed, nil},
		{"outer adds white space", two("B ", "B"), framed, nil},
		{"4 levels, each a prefix of the next",
			nbMP("mixed", "BBBB", false, nbMP("mixed", "BBB", false, nbMP("mixed", "BB", false, nbMP("alternative", "B", false, plain, html), pdf))),
			[]string{"[0 0 0 0] text/plain|plain", "[0 0 0 1] text/html|html", "[0 0 1] application/pdf|pdf"}, nil},
		// The input ends with an inner opening line, which is also the
		// outer delimiter cut short: the outer reads it as its own, cut, and
		// the inner part before it runs to the end of the outer one.
		{"inner a prefix, input ends at an inner delimiter",
			"Content-Type: multipart/mixed; boundary=abcdefgh\r\n\r\n--abcdefgh\r\n" +
				"Content-Type: multipart/alternative; boundary=abcdef\r\n\r\n--abcdef\r\n" + plain + "\r\n--abcdef",
			[]string{"[0 0] text/plain|plain"},
			[]string{"multipart.close_not_found [0]", "multipart.close_not_found []"}},

		// The inner opening lines are the outer's, padded: its parts are the
		// outer's, and its closing line, which the outer does not read as
		// one, stays in the last of them.
		{"inner adds white space", two("B", "B "),
			[]string{"[1] text/plain|plain", "[2] text/html|html\r\n--B --\r\n", "[3] application/pdf|pdf"},
			[]string{"multipart.nested_boundary_conflict [0]", "multipart.empty [0]"}},
		// Each inner opening line closes the outer, which reads on past it.
		{"inner adds --", two("B", "B--"),
			[]string{"[1] text/plain|plain", "[2] text/html|html\r\n--B----\r\n", "[3] application/pdf|pdf"},
			[]string{"multipart.nested_boundary_conflict [0]", "multipart.empty [0]", "multipart.close_shared []", "multipart.close_shared []"}},
		// The inner closing line opens an outer part, which is empty.
		{"outer adds --", two("B--", "B"),
			[]string{"[0 0] text/plain|plain", "[0 1] text/html|html", "[1] text/plain|", "[2] application/pdf|pdf"},
			[]string{"multipart.nested_boundary_conflict [0]", "multipart.close_not_found [0]"}},

		{"same boundary, 2 levels", two("B", "B"), flat, append(reused, "multipart.close_shared []")},
		{"same boundary, 3 levels",
			nbMP("mixed", "B", false, nbMP("mixed", "B", false, nbMP("alternative", "B", false, plain, html), png), pdf),
			[]string{"[2] text/plain|plain", "[3] text/html|html", "[4] image/png|png", "[5] application/pdf|pdf"},
			append(reused, "multipart.nested_boundary_reused [1]", "multipart.empty [1]", "multipart.close_shared []", "multipart.close_shared []")},
		// The middle multipart is cut at the first shared delimiter; what
		// follows the inner close, its own last part, is read as an outer one.
		{"same boundary across a level",
			nbMP("mixed", "B", false, nbMP("mixed", "C", false, nbMP("alternative", "B", false, plain, html), png), pdf),
			[]string{"[1] text/plain|plain", "[2] text/html|html", "[3] image/png|png\r\n--C--\r\n orphan=\"--C\\r\\n\"", "[4] application/pdf|pdf"},
			[]string{"multipart.nested_boundary_reused [0 0]", "multipart.empty [0 0]", "multipart.close_not_found [0]", "multipart.close_shared []", "header.missing_colon [3]"}},

		{"inner close missing",
			nbMP("mixed", "OUT", false, nbMP("alternative", "IN", true, plain, html), pdf),
			[]string{"[0 0] text/plain|plain", "[0 1] text/html|html\r\n", "[1] application/pdf|pdf"},
			[]string{"multipart.close_not_found [0]"}},
		{"inner close missing, same boundary",
			nbMP("mixed", "B", false, nbMP("alternative", "B", true, plain, html), pdf),
			[]string{"[1] text/plain|plain", "[2] text/html|html\r\n", "[3] application/pdf|pdf"}, reused},
		{"outer close missing",
			nbMP("mixed", "OUT", true, nbMP("alternative", "IN", false, plain, html), pdf),
			[]string{"[0 0] text/plain|plain", "[0 1] text/html|html", "[1] application/pdf|pdf\r\n"},
			[]string{"multipart.close_not_found []"}},
		{"outer close missing, same boundary",
			nbMP("mixed", "B", true, nbMP("alternative", "B", false, plain, html), pdf),
			[]string{"[1] text/plain|plain", "[2] text/html|html", "[3] application/pdf|pdf\r\n"},
			append(reused, "multipart.close_shared []", "multipart.close_not_found []")},
		// Nothing follows the shared close but the outer one and an epilogue:
		// the epilogue is not read as a part.
		{"same boundary, epilogue after",
			nbMP("mixed", "B", false, nbMP("alternative", "B", false, plain, html)) + "--B\r\nnot a part\r\n",
			[]string{"[1] text/plain|plain", "[2] text/html|html"}, reused},

		// An outer delimiter line in an inner part legitimately ends the
		// whole inner multipart; the text after it begins an outer part,
		// whose damaged lines are kept as orphan records.
		{"text holds the outer delimiter",
			nbMP("mixed", "OUT", false, nbMP("alternative", "IN", false, nbLeaf("text/plain", "a\r\n--OUT\r\nb"), html), pdf),
			[]string{"[0 0] text/plain|a", "[1] text/html|html\r\n--IN--\r\n orphan=\"b\\r\\n\" orphan=\"--IN\\r\\n\"", "[2] application/pdf|pdf"},
			[]string{"multipart.close_not_found [0]", "header.missing_colon [1]"}},
		{"text holds a sibling's delimiter",
			nbMP("mixed", "OUT", false, nbMP("alternative", "IN1", false, nbLeaf("text/plain", "a\r\n--IN2\r\nb")), nbMP("alternative", "IN2", false, html)),
			[]string{"[0 0] text/plain|a\r\n--IN2\r\nb", "[1 0] text/html|html"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, profile := range []Profile{ProfileTolerant, ProfileForensic} {
				for name, src := range map[string]io.Reader{
					"whole":    strings.NewReader(tc.text),
					"one byte": iotest.OneByteReader(strings.NewReader(tc.text)),
				} {
					leaves, diags, werr := nbNestedWalk(t, src, profile)
					if werr != nil {
						t.Errorf("%v/%s: Walk() = %v", profile, name, werr)
					}
					if !reflect.DeepEqual(leaves, tc.leaves) {
						t.Errorf("%v/%s: leaves %q\nwant %q", profile, name, leaves, tc.leaves)
					}
					if !reflect.DeepEqual(diags, tc.diags) {
						t.Errorf("%v/%s: diagnostics %q\nwant %q", profile, name, diags, tc.diags)
					}
				}
			}
		})
	}
}

// TestParse_nestedBoundariesUnchanged pins that ProfileStrict does not read
// past a shared closing delimiter, and that under ProfileForensic the input
// after the last closing delimiter is still the trailing input when nothing
// was read past it.
func TestParse_nestedBoundariesUnchanged(t *testing.T) {
	plain := nbLeaf("text/plain", "plain")
	text := nbMP("mixed", "B", false, nbMP("alternative", "B", false, plain), nbLeaf("application/pdf", "pdf"))
	s, e, _ := Parse(strings.NewReader(text), &ParseOptions{Profile: ProfileStrict})
	mr := e.MultipartReader()
	var types []string
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		mt, _, _ := p.EffectiveContentType()
		types = append(types, mt)
	}
	if want := []string{"multipart/alternative", "text/plain"}; !reflect.DeepEqual(types, want) || len(s.Diagnostics()) != 0 {
		t.Errorf("strict: parts %q, diagnostics %v; want %q and none", types, codesOfDiags(s.Diagnostics()), want)
	}

	text = nbMP("mixed", "B", false, nbMP("alternative", "B", false, plain)) + "tail\r\n"
	s, e, err := Parse(strings.NewReader(text), &ParseOptions{Profile: ProfileForensic})
	if err != nil {
		t.Fatal(err)
	}
	if werr := e.Walk(func([]int, *Entity, error) error { return nil }); werr != nil {
		t.Fatal(werr)
	}
	rd, ok := s.TrailingInputReader()
	if !ok {
		t.Fatal("TrailingInputReader() not ok")
	}
	if b, _ := io.ReadAll(rd); string(b) != "\r\n--B--\r\ntail\r\n" {
		t.Errorf("trailing input %q", b)
	}
}
