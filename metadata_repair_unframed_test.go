package message

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

// Review follow-ups for ROB-050-001: a repaired multipart that kept no usable
// boundary is one opaque part rather than a dead end; a strict refusal is not
// turned into success by Walk; and the item-by-item reading labels well-formed
// RFC 2231 items accurately.

// unframedInner is the body of the damaged child: multipart framing that no
// surviving boundary describes, which must come back byte for byte.
const unframedInner = "--B\r\nContent-Type: text/plain\r\n\r\ninner\r\n--B--\r\n"

func unframedTree(childType string) string {
	return "Content-Type: multipart/mixed; boundary=O\r\n\r\n" +
		"--O\r\n\r\nbefore\r\n" +
		"--O\r\nContent-Type: " + childType + "\r\n\r\n" + unframedInner + "\r\n" +
		"--O\r\n\r\nafter\r\n" +
		"--O--\r\n"
}

// TestParse_unframedMultipartIsOneOpaquePart pins the fallback: the media type
// the field declares is kept, nothing is chosen or invented, the body is one
// part with the input's bytes, and the sibling after it is reached — through
// MultipartReader and through Walk — with the raw header unchanged.
func TestParse_unframedMultipartIsOneOpaquePart(t *testing.T) {
	for _, tc := range []struct {
		name, childType string
		limits          Limits
		codes           []Code
	}{
		{"conflicting boundaries", "multipart/mixed; boundary=B; boundary=C", Limits{},
			[]Code{CodeMIMEParameterDuplicate}},
		{"damaged repeat of the boundary", `multipart/mixed; boundary=B; boundary="C`, Limits{},
			[]Code{CodeMIMEContentTypeInvalid, CodeMIMEParameterDuplicate}},
		{"unterminated quote hides the only boundary", `multipart/related; type="text/html; boundary=B`, Limits{},
			[]Code{CodeMIMEContentTypeInvalid}},
		{"boundary past the budget behind a broken item", "multipart/mixed; broken; boundary=B", paramsOnly(2, 0),
			[]Code{CodeLimitParameterCount, CodeMIMEContentTypeInvalid}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := unframedTree(tc.childType)
			if tc.name == "boundary past the budget behind a broken item" {
				// The root declares one parameter, so a budget of two leaves the
				// root whole and cuts the child after "broken".
				text = strings.Replace(text, tc.childType, tc.childType+"; x=1", 1)
				tc.limits = paramsOnly(1, 0)
			}
			session, root, err := parseWith(t, text, ParseOptions{Limits: tc.limits})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			mr := root.MultipartReader()
			before, _ := mr.NextPart()
			if readBodyOf(t, before) != "before" {
				t.Fatal("first sibling lost")
			}
			child, err := mr.NextPart()
			if err != nil || child == nil {
				t.Fatalf("NextPart() = (%v, %v), want the opaque child", child, err)
			}
			mt, _, ctErr := child.EffectiveContentType()
			if ctErr != nil || !strings.HasPrefix(mt, "multipart/") {
				t.Errorf("EffectiveContentType() = (%q, %v), want the declared multipart type", mt, ctErr)
			}
			if child.MultipartReader() != nil {
				t.Error("MultipartReader() != nil, want no framing without a usable boundary")
			}
			if got := rawBodyOf(t, child); got != unframedInner {
				t.Errorf("opaque body = %q, want the raw bytes %q", got, unframedInner)
			}
			checkRawHeader(t, child, "Content-Type: "+strings.SplitN(strings.SplitN(text, "--O\r\nContent-Type: ", 2)[1], "\r\n", 2)[0]+"\r\n")
			after, err := mr.NextPart()
			if err != nil || readBodyOf(t, after) != "after" {
				t.Errorf("following sibling = (%v, %v)", after, err)
			}

			diags := session.Diagnostics()
			if got := codesOfDiags(diags); !reflect.DeepEqual(got, tc.codes) {
				t.Errorf("Diagnostics() = %v, want %v", got, tc.codes)
			}
			for _, d := range diags {
				if strings.HasPrefix(string(d.Code), "limit.") {
					continue
				}
				if d.Severity != SeverityInvalidMetadata || !strings.Contains(d.Action, "opaque, uninterpreted part") {
					t.Errorf("%v: severity %v action %q, want an uninterpreted-body report", d.Code, d.Severity, d.Action)
				}
			}

			// Walk reaches all three as leaves.
			_, root, _ = parseWith(t, text, ParseOptions{Limits: tc.limits})
			var leaves []string
			if err := root.Walk(func(path []int, e *Entity, err error) error {
				if e.MultipartReader() == nil {
					leaves = append(leaves, rawBodyOf(t, e))
				}
				return err
			}); err != nil {
				t.Fatalf("Walk() = %v", err)
			}
			if want := []string{"before", unframedInner, "after"}; !reflect.DeepEqual(leaves, want) {
				t.Errorf("Walk leaves = %q, want %q", leaves, want)
			}
		})
	}

	t.Run("root", func(t *testing.T) {
		_, e, err := parseWith(t, "Content-Type: multipart/mixed; boundary=B; boundary=C\r\n\r\n"+unframedInner, ParseOptions{})
		if err != nil || e.MultipartReader() != nil || rawBodyOf(t, e) != unframedInner {
			t.Errorf("root: err %v, want an opaque body with the raw bytes", err)
		}
	})

	// No repair, no fallback: a valid multipart without a boundary keeps its
	// existing behavior, which ROB-060-002 owns.
	t.Run("missing boundary without a repair is unchanged", func(t *testing.T) {
		session, e, _ := parseWith(t, "Content-Type: multipart/mixed\r\n\r\n"+unframedInner, ParseOptions{})
		mr := e.MultipartReader()
		if mr == nil {
			t.Fatal("MultipartReader() = nil, want the existing iterator")
		}
		if _, err := mr.NextPart(); err == nil {
			t.Error("NextPart() succeeded, want the existing empty-boundary error")
		}
		if session.Diagnostics() != nil {
			t.Errorf("Diagnostics() = %v, want none", codesOfDiags(session.Diagnostics()))
		}
	})

	// Strict refuses instead, as before; the fallback is a tolerant repair.
	t.Run("strict still refuses", func(t *testing.T) {
		_, root, _ := parseWith(t, unframedTree("multipart/mixed; boundary=B; boundary=C"), ParseOptions{Profile: ProfileStrict})
		mr := root.MultipartReader()
		mr.NextPart()
		child, err := mr.NextPart()
		if child == nil || !IsFatal(err) || root.ParseStop() != nil {
			t.Fatalf("strict child = (%v, %v), want the entity with its local refusal", child, err)
		}
		if after, err := mr.NextPart(); err != nil || readBodyOf(t, after) != "after" {
			t.Errorf("following sibling = (%v, %v)", after, err)
		}
	})
}

// TestParse_strictRefusalIsNotSuccessForWalk pins that Walk reports a strict
// refusal it can see — the root's Content-Type, or a disposition already read —
// without reading anything for the purpose, that a session stop outranks it,
// and that it does not stop the parse.
func TestParse_strictRefusalIsNotSuccessForWalk(t *testing.T) {
	strict := ParseOptions{Profile: ProfileStrict}

	t.Run("root content type", func(t *testing.T) {
		_, e, err := parseWith(t, issue22, strict)
		calls := 0
		walkErr := e.Walk(func([]int, *Entity, error) error { calls++; return nil })
		if !IsFatal(err) || walkErr != err || calls != 0 || e.ParseStop() != nil {
			t.Errorf("Walk() = %v after %d callbacks, want the identical refusal %v and no stop", walkErr, calls, err)
		}
		// A copy is the same entity, refusal included, and the raw body stays
		// readable for salvage.
		cp := *e
		if cp.Walk(func([]int, *Entity, error) error { return nil }) != err {
			t.Error("a copy walked to success")
		}
		if rawBodyOf(t, e) != strings.SplitN(issue22, "\r\n\r\n", 2)[1] {
			t.Error("raw body lost")
		}
	})

	const lazyDisposition = "Content-Type: text/plain\r\nContent-Disposition: attachment; broken\r\n\r\nbody"

	t.Run("disposition read before the walk", func(t *testing.T) {
		_, e, err := parseWith(t, lazyDisposition, strict)
		if err != nil {
			t.Fatalf("Parse() = %v, want nothing yet: the disposition is read lazily", err)
		}
		_, _, dErr := e.EffectiveContentDisposition()
		if !IsFatal(dErr) {
			t.Fatalf("EffectiveContentDisposition() err = %v, want the refusal", dErr)
		}
		if walkErr := e.Walk(func([]int, *Entity, error) error { return nil }); walkErr != dErr {
			t.Errorf("Walk() = %v, want the identical refusal", walkErr)
		}
	})

	t.Run("disposition read during the walk", func(t *testing.T) {
		_, e, _ := parseWith(t, lazyDisposition, strict)
		var seen error
		walkErr := e.Walk(func(_ []int, p *Entity, _ error) error {
			_, _, seen = p.EffectiveContentDisposition()
			return nil
		})
		if !IsFatal(seen) || walkErr != seen {
			t.Errorf("Walk() = %v, want the refusal the callback just observed", walkErr)
		}
	})

	t.Run("disposition nobody read is not read for the walk", func(t *testing.T) {
		session, e, _ := parseWith(t, lazyDisposition, strict)
		if walkErr := e.Walk(func([]int, *Entity, error) error { return nil }); walkErr != nil {
			t.Errorf("Walk() = %v, want nil: nothing observed a refusal", walkErr)
		}
		if session.Diagnostics() != nil {
			t.Errorf("Diagnostics() = %v, want none: Walk must not read the field", codesOfDiags(session.Diagnostics()))
		}
	})

	t.Run("disposition read by the charset stage", func(t *testing.T) {
		text := "Content-Type: text/plain; charset=utf-8\r\nContent-Disposition: attachment; broken\r\n\r\nbody"
		_, e, err := parseWith(t, text, ParseOptions{Profile: ProfileStrict, ReadOptions: ReadOptions{SkipTextAttachmentDecoding: true}})
		if !IsFatal(err) {
			t.Fatalf("Parse() = %v, want the refusal", err)
		}
		if walkErr := e.Walk(func([]int, *Entity, error) error { return nil }); walkErr != err {
			t.Errorf("Walk() = %v, want the identical refusal", walkErr)
		}
	})

	t.Run("a sink stop outranks the refusal", func(t *testing.T) {
		sentinel := errors.New("stop")
		_, e, err := parseWith(t, issue22, ParseOptions{
			Profile:        ProfileStrict,
			DiagnosticSink: DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error { return sentinel }),
		})
		if !errors.Is(err, sentinel) || err != e.ParseStop() {
			t.Fatalf("Parse() = %v, want the sink stop", err)
		}
		if walkErr := e.Walk(func([]int, *Entity, error) error { return nil }); walkErr != err {
			t.Errorf("Walk() = %v, want the identical stop", walkErr)
		}
	})

	t.Run("a child refusal ends only the walk", func(t *testing.T) {
		text := unframedTree("text/plain; broken")
		_, root, _ := parseWith(t, text, strict)
		if walkErr := root.Walk(func([]int, *Entity, error) error { return nil }); !IsFatal(walkErr) || root.ParseStop() != nil {
			t.Errorf("Walk() = %v, ParseStop() = %v; want the local refusal and no stop", walkErr, root.ParseStop())
		}
		_, root, _ = parseWith(t, text, strict)
		mr := root.MultipartReader()
		var got []string
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if p != nil && err == nil {
				got = append(got, readBodyOf(t, p))
			}
		}
		if !reflect.DeepEqual(got, []string{"before", "after"}) {
			t.Errorf("siblings through the iterator = %q", got)
		}
	})
}

// TestParse_repairLabelsRFC2231ItemsAccurately pins that a well-formed RFC 2231
// item that yields no value on its own — a later continuation segment, or an
// extended value whose charset has no decoder — is not counted as malformed,
// while the deferred policy still drops it.
func TestParse_repairLabelsRFC2231ItemsAccurately(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		params      map[string]string
		message     string
		action      string
		codes       []Code
	}{
		{"well-formed series beside a broken item", "text/plain; filename*0=a; filename*1=b; broken; format=flowed",
			map[string]string{"format": "flowed"}, "1 malformed parameter item", "dropped the RFC 2231 continued filename",
			[]Code{CodeMIMEContentTypeInvalid}},
		{"lone later segment beside a broken item", "text/plain; filename*1=b; broken",
			map[string]string{}, "1 malformed parameter item", "dropped the RFC 2231 continued filename",
			[]Code{CodeMIMEContentTypeInvalid}},
		{"damaged segment drops its family", "text/plain; filename*0=a; filename*1=b c; format=flowed",
			map[string]string{"format": "flowed"}, "1 malformed parameter item", "dropped the RFC 2231 continued filename",
			[]Code{CodeMIMEContentTypeInvalid}},
		{"undecodable extended value is not malformed", "text/plain; name*=iso-8859-1''caf%E9; broken; format=flowed",
			map[string]string{"format": "flowed"}, "1 malformed parameter item", "kept no value for the well-formed name",
			[]Code{CodeMIMEContentTypeInvalid}},
		{"undecodable repeat makes a plain value ambiguous", "text/plain; name=x; name*=iso-8859-1''y; broken",
			map[string]string{}, "1 malformed parameter item", "dropped every occurrence of the conflicting name",
			[]Code{CodeMIMEContentTypeInvalid, CodeMIMEParameterDuplicate}},
		{"names are reported in first-occurrence order", "text/plain; zeta=1; alpha=2; broken",
			map[string]string{"zeta": "1", "alpha": "2"}, "1 malformed parameter item", "well-formed parameters zeta, alpha",
			[]Code{CodeMIMEContentTypeInvalid}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, e, _ := parseWith(t, "Content-Type: "+tc.value+"\r\n\r\nbody", ParseOptions{})
			if _, params, err := e.EffectiveContentType(); err != nil || !reflect.DeepEqual(params, tc.params) {
				t.Errorf("params = %v, %v; want %v", params, err, tc.params)
			}
			diags := session.Diagnostics()
			if got := codesOfDiags(diags); !reflect.DeepEqual(got, tc.codes) {
				t.Fatalf("Diagnostics() = %v, want %v", got, tc.codes)
			}
			if !strings.Contains(diags[0].Message, tc.message) || !strings.Contains(diags[0].Action, tc.action) {
				t.Errorf("diagnostic = %q / %q, want %q and %q", diags[0].Message, diags[0].Action, tc.message, tc.action)
			}
		})
	}
}

// rawBodyOf reads e.Body whole and returns it untrimmed, for the checks that
// the bytes are exactly the input's.
func rawBodyOf(t *testing.T, e *Entity) string {
	t.Helper()
	b, err := io.ReadAll(e.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return string(b)
}
