package message

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// Review follow-up for ROB-050-001: empty items between semicolons are
// rejected by mime.ParseMediaType but are not parameter occurrences, so the
// repair has to explain a field whose only defect is one.

// TestScanParameters_countsStrayEmptyItems pins what counts as a stray empty
// item: every empty item but one that ends the field, which mime accepts as a
// trailing semicolon. None is an occurrence.
func TestScanParameters_countsStrayEmptyItems(t *testing.T) {
	for _, tc := range []struct {
		value       string
		stray, occs int
	}{
		{"multipart/mixed; boundary=B", 0, 1},
		{"multipart/mixed; boundary=B;", 0, 1},
		{"multipart/mixed; boundary=B; ", 0, 1},
		{"multipart/mixed;; boundary=B", 1, 1},
		{"multipart/mixed; ; boundary=B", 1, 1},
		{"multipart/mixed; boundary=B;;", 1, 1},
		{"multipart/mixed; boundary=B; ;", 1, 1},
		{"text/plain;;;", 2, 0},
		{`text/plain; a=";;"; b=1`, 0, 2},
	} {
		s := scanParameters(tc.value, LimitDisabled, LimitDisabled)
		if s.strayEmpty != tc.stray || len(s.occurrences) != tc.occs {
			t.Errorf("%q: %d stray empty items, %d occurrences; want %d and %d", tc.value, s.strayEmpty, len(s.occurrences), tc.stray, tc.occs)
		}
	}
	// Cut short by a budget, the field goes on past every empty item seen, so
	// none of them is the trailing one.
	if s := scanParameters("text/plain; a=1;; b=2", 1, LimitDisabled); s.refusal == nil || s.strayEmpty != 1 {
		t.Errorf("budget cut: refusal %v, %d stray; want a refusal and 1", s.refusal, s.strayEmpty)
	}
}

func TestParse_strayEmptyItemsAreRepaired(t *testing.T) {
	t.Run("diagnostic says what was empty", func(t *testing.T) {
		session, e, err := parseWith(t, "Content-Type: multipart/mixed;; boundary=B\r\n\r\n"+repairBody, ParseOptions{})
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		d := session.Diagnostics()
		if len(d) != 1 || d[0].Code != CodeMIMEContentTypeInvalid || d[0].Severity != SeverityRecovered ||
			!strings.Contains(d[0].Message, "1 empty parameter item between semicolons") || strings.Contains(d[0].Message, "malformed") ||
			!strings.Contains(d[0].Action, "every empty item between semicolons") {
			t.Fatalf("Diagnostics() = %+v", d)
		}
		if p, err := e.MultipartReader().NextPart(); err != nil || readBodyOf(t, p) != "kept" {
			t.Errorf("NextPart() = (%v, %v), want the kept part", p, err)
		}
	})

	t.Run("budgets", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			value  string
			limits Limits
			codes  []Code
		}{
			{"both disabled", "multipart/mixed;; boundary=B", allDisabled(), []Code{CodeMIMEContentTypeInvalid}},
			{"empty items are not charged", "multipart/mixed;;;;; boundary=B", paramsOnly(1, 0), []Code{CodeMIMEContentTypeInvalid}},
			{"stray empty item before a budget cut", "multipart/mixed;; boundary=B; x=1", paramsOnly(1, 0),
				[]Code{CodeLimitParameterCount, CodeMIMEContentTypeInvalid}},
			{"only the segment budget", "multipart/mixed; boundary=B;;", paramsOnly(-1, 2), []Code{CodeMIMEContentTypeInvalid}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				session, e, err := parseWith(t, "Content-Type: "+tc.value+"\r\n\r\n"+repairBody, ParseOptions{Limits: tc.limits})
				if err != nil {
					t.Fatalf("Parse() = %v", err)
				}
				if _, params, _ := e.EffectiveContentType(); !reflect.DeepEqual(params, map[string]string{"boundary": "B"}) {
					t.Errorf("params = %v, want the boundary", params)
				}
				if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, tc.codes) {
					t.Errorf("Diagnostics() = %v, want %v", got, tc.codes)
				}
			})
		}
	})

	t.Run("disposition and the charset decision", func(t *testing.T) {
		header := "Content-Type: text/plain; charset=x-made-up\r\nContent-Disposition: attachment;; filename=a.txt\r\n\r\nraw"
		for _, profile := range profiles {
			session, e, err := parseWith(t, header, ParseOptions{Profile: profile, ReadOptions: ReadOptions{SkipTextAttachmentDecoding: true}})
			disp, params, dErr := e.EffectiveContentDisposition()
			if profile == ProfileStrict {
				if !IsFatal(err) || err != dErr || disp != "" {
					t.Errorf("strict: (%q, %v), Parse err %v; want the local refusal", disp, dErr, err)
				}
				continue
			}
			if err != nil || dErr != nil || disp != "attachment" || params["filename"] != "a.txt" {
				t.Errorf("%s: (%q, %v, %v), Parse err %v; want the repaired attachment, charset skipped", profile, disp, params, dErr, err)
			}
			if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeMIMEDispositionInvalid}) {
				t.Errorf("%s: Diagnostics() = %v", profile, got)
			}
		}
	})

	t.Run("quoted empty boundary beside a broken item is unframed", func(t *testing.T) {
		session, e, err := parseWith(t, "Content-Type: multipart/mixed; boundary=\"\"; broken\r\n\r\n"+repairBody, ParseOptions{})
		if err != nil || e.MultipartReader() != nil || rawBodyOf(t, e) != repairBody {
			t.Errorf("err %v, want an opaque body with the raw bytes", err)
		}
		if d := session.Diagnostics(); len(d) != 1 || d[0].Severity != SeverityInvalidMetadata {
			t.Errorf("Diagnostics() = %+v, want one uninterpreted-body report", d)
		}
	})

	t.Run("many empty items cost linear work", func(t *testing.T) {
		cost := func(n int) uint64 {
			text := "Content-Type: multipart/mixed" + strings.Repeat(";", n) + " boundary=B\r\n\r\n"
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			_, e, _ := Parse(strings.NewReader(text), nil)
			runtime.ReadMemStats(&after)
			if _, params, _ := e.EffectiveContentType(); params["boundary"] != "B" {
				t.Fatalf("n=%d: boundary lost", n)
			}
			return after.TotalAlloc - before.TotalAlloc
		}
		small, large := cost(4000), cost(16000)
		if ratio := float64(large) / float64(small); ratio > 4*3 {
			t.Errorf("4x the empty items cost %.1fx the bytes (%d vs %d), want about 4x", ratio, large, small)
		}
	})
}

// TestParse_walkStopsAtAContainerRefusedMidWalk pins that a refusal of a
// container observed after the walk descended into it — here by walkFunc
// reading the root's disposition while it handles the first child — ends the
// walk before another part of that container is read.
func TestParse_walkStopsAtAContainerRefusedMidWalk(t *testing.T) {
	text := "Content-Type: multipart/mixed; boundary=O\r\nContent-Disposition: inline; broken\r\n\r\n" +
		"--O\r\n\r\nfirst\r\n--O\r\n\r\nsecond\r\n--O--\r\n"
	session, root, err := parseWith(t, text, ParseOptions{Profile: ProfileStrict})
	if err != nil {
		t.Fatalf("Parse() = %v, want nothing yet: the disposition is read lazily", err)
	}
	var seen []string
	var refusal error
	walkErr := root.Walk(func(path []int, e *Entity, err error) error {
		seen = append(seen, fmt.Sprint(path))
		if len(path) == 1 && path[0] == 0 {
			_, _, refusal = root.EffectiveContentDisposition()
		}
		return nil
	})
	if !IsFatal(refusal) || walkErr != refusal {
		t.Fatalf("Walk() = %v, want the refusal observed mid-walk %v", walkErr, refusal)
	}
	if want := []string{"[]", "[0]"}; !reflect.DeepEqual(seen, want) {
		t.Errorf("walked %v, want %v: no part of the refused container after the refusal", seen, want)
	}
	if root.ParseStop() != nil || len(session.Diagnostics()) != 1 {
		t.Errorf("ParseStop() = %v, %d diagnostics; want no stop and the one refusal", root.ParseStop(), len(session.Diagnostics()))
	}

	// The same walk with the root read before descending ends after the root's
	// own callback, before any child.
	_, root, _ = parseWith(t, text, ParseOptions{Profile: ProfileStrict})
	seen = nil
	walkErr = root.Walk(func(path []int, e *Entity, err error) error {
		seen = append(seen, fmt.Sprint(path))
		e.EffectiveContentDisposition()
		return nil
	})
	if !IsFatal(walkErr) || !reflect.DeepEqual(seen, []string{"[]"}) {
		t.Errorf("Walk() = %v after %v, want the refusal before any child", walkErr, seen)
	}
}
