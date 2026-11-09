package message

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

// With ReadOptions.SkipTextAttachmentDecoding, whether a text part's charset is
// converted depends on its Content-Disposition, so that field is read under the
// parameter budgets at the moment the charset stage is decided. A field past a
// budget records a diagnostic there, and a DiagnosticSink may stop the parse
// while it is told about it. That stop belongs to the whole session: the entity
// is returned with it, no charset decoder is set up after it, and every later
// scope reports the identical error (ROB-001-003, issue #1).

// dispositionStopLimits budgets one parameter per field, so a disposition with
// two parameters past its type is cut and diagnosed.
func dispositionStopLimits() Limits {
	l := allDisabled()
	l.MaxParameterCount = 1
	return l
}

// dispositionHeader is a text part header whose Content-Disposition is past the
// parameter budget. cte, when not empty, is a Content-Transfer-Encoding field.
func dispositionHeader(disposition, charset, cte string) string {
	h := "Content-Type: text/plain; charset=" + charset + "\r\n" +
		"Content-Disposition: " + disposition + "; a=1; b=2\r\n"
	if cte != "" {
		h += "Content-Transfer-Encoding: " + cte + "\r\n"
	}
	return h
}

// TestParse_dispositionBudgetStopIsReported pins the fix on the root entity and
// on a child of a multipart, for an inline and an attachment disposition, and
// for a sink whose error is a plain sentinel or wraps one of the two decoding
// warnings — the latter must still be reported as the fatal stop it is.
func TestParse_dispositionBudgetStopIsReported(t *testing.T) {
	sentinel := errors.New("stop requested")

	stops := []struct {
		name string
		err  error
	}{
		{"sentinel", sentinel},
		{"wrapped encoding warning", fmt.Errorf("stop: %w", UnknownEncodingError{sentinel})},
		{"wrapped charset warning", fmt.Errorf("stop: %w", UnknownCharsetError{sentinel})},
	}

	for _, disposition := range []string{"inline", "attachment"} {
		for _, stop := range stops {
			sink := DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
				if d.Code == CodeLimitParameterCount {
					return stop.err
				}
				return nil
			})
			opts := func() *ParseOptions {
				return &ParseOptions{
					Limits:         dispositionStopLimits(),
					ReadOptions:    ReadOptions{SkipTextAttachmentDecoding: true},
					DiagnosticSink: sink,
				}
			}
			checkStop := func(t *testing.T, session *ParseSession, err error) {
				t.Helper()
				if !IsFatal(err) || !errors.Is(err, sentinel) {
					t.Fatalf("err = %v, want a FatalError wrapping the sink's error", err)
				}
				if published := session.fatalErr(); published == nil || err != error(published) {
					t.Errorf("err = %v, want the identical published stop %v", err, published)
				}
				diags := session.Diagnostics()
				if got := codesOfDiags(diags); !reflect.DeepEqual(got, []Code{CodeLimitParameterCount}) {
					t.Fatalf("Diagnostics() = %v, want only the disposition's parameter budget", got)
				}
				if diags[0].Field != "Content-Disposition" {
					t.Errorf("Field = %q, want Content-Disposition", diags[0].Field)
				}
			}

			t.Run(disposition+"/"+stop.name+"/root", func(t *testing.T) {
				text := dispositionHeader(disposition, "utf-8", "") + "\r\nbody"
				session, e, err := Parse(strings.NewReader(text), opts())
				if e == nil {
					t.Fatalf("Parse() returned no entity, want the usable entity with the stop (err = %v)", err)
				}
				checkStop(t, session, err)
				if got := readBodyOf(t, e); got != "body" {
					t.Errorf("body = %q, want the recovered bytes %q", got, "body")
				}
				if got := e.Header.Get("Content-Disposition"); got != disposition+"; a=1; b=2" {
					t.Errorf("Content-Disposition = %q, want the raw field kept", got)
				}
			})

			t.Run(disposition+"/"+stop.name+"/child", func(t *testing.T) {
				text := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
					"--B\r\nContent-Type: text/plain\r\n\r\nzero\r\n" +
					"--B\r\n" + dispositionHeader(disposition, "utf-8", "") + "\r\none\r\n" +
					"--B\r\nContent-Type: text/plain\r\n\r\ntwo\r\n" +
					"--B--\r\n"
				session, e, err := Parse(strings.NewReader(text), opts())
				if err != nil {
					t.Fatalf("Parse() = %v", err)
				}
				mr := e.MultipartReader()
				zero, err := mr.NextPart()
				if err != nil {
					t.Fatalf("NextPart() = %v, want the first part", err)
				}
				if got := readBodyOf(t, zero); got != "zero" {
					t.Errorf("first part = %q, want %q", got, "zero")
				}

				one, stopErr := mr.NextPart()
				if one == nil {
					t.Fatalf("NextPart() returned no part, want the usable part with the stop (err = %v)", stopErr)
				}
				checkStop(t, session, stopErr)
				if got := readBodyOf(t, one); got != "one" {
					t.Errorf("stopped part = %q, want the recovered bytes %q", got, "one")
				}
				if p, err := mr.NextPart(); p != nil || err != stopErr {
					t.Errorf("NextPart() after the stop = (%v, %v), want (nil, %v)", p, err, stopErr)
				}
			})
		}
	}
}

// TestParse_dispositionBudgetStopPrecedesTheCharsetStage pins that nothing is
// interpreted after the stop: an unknown charset is not looked up, so no charset
// diagnostic follows the stop and the body is the bytes as they arrived, while a
// transfer encoding warning discovered before the stop keeps its place in the
// order and is outranked by it.
func TestParse_dispositionBudgetStopPrecedesTheCharsetStage(t *testing.T) {
	sentinel := errors.New("stop requested")
	sink := DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
		if d.Code == CodeLimitParameterCount {
			return sentinel
		}
		return nil
	})

	for _, tc := range []struct {
		name  string
		hdr   string
		codes []Code
	}{
		{"unknown charset is not looked up", dispositionHeader("inline", "made-up-charset", ""), []Code{CodeLimitParameterCount}},
		{"earlier unknown encoding keeps its place", dispositionHeader("inline", "made-up-charset", "nonsense"), []Code{CodeCTEUnknown, CodeLimitParameterCount}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, e, err := Parse(strings.NewReader(tc.hdr+"\r\nbody"), &ParseOptions{
				Limits:         dispositionStopLimits(),
				ReadOptions:    ReadOptions{SkipTextAttachmentDecoding: true},
				DiagnosticSink: sink,
			})
			if e == nil {
				t.Fatalf("Parse() returned no entity (err = %v)", err)
			}
			if !IsFatal(err) || !errors.Is(err, sentinel) {
				t.Fatalf("err = %v, want the sink's stop rather than a warning", err)
			}
			if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, tc.codes) {
				t.Errorf("Diagnostics() = %v, want %v", got, tc.codes)
			}
			if got := readBodyOf(t, e); got != "body" {
				t.Errorf("body = %q, want the bytes as they arrived", got)
			}
		})
	}
}

// TestParse_dispositionBudgetWithoutStop is the control: a sink that does not
// stop, or no sink, leaves every existing outcome as it was — the diagnostic is
// recorded, an inline part is decoded and an attachment is not, and an unknown
// charset is reported as the warning it always was.
func TestParse_dispositionBudgetWithoutStop(t *testing.T) {
	for _, tc := range []struct {
		name    string
		hdr     string
		sink    DiagnosticSink
		codes   []Code
		warning bool
	}{
		{"inline, healthy sink", dispositionHeader("inline", "utf-8", ""),
			DiagnosticSinkFunc(func(*ParseSession, Diagnostic) error { return nil }),
			[]Code{CodeLimitParameterCount}, false},
		{"inline, no sink, unknown charset decoded", dispositionHeader("inline", "made-up-charset", ""), nil,
			[]Code{CodeLimitParameterCount, CodeCharsetUnknown}, true},
		{"attachment, no sink, unknown charset skipped", dispositionHeader("attachment", "made-up-charset", ""), nil,
			[]Code{CodeLimitParameterCount}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, e, err := Parse(strings.NewReader(tc.hdr+"\r\nbody"), &ParseOptions{
				Limits:         dispositionStopLimits(),
				ReadOptions:    ReadOptions{SkipTextAttachmentDecoding: true},
				DiagnosticSink: tc.sink,
			})
			if e == nil {
				t.Fatalf("Parse() returned no entity (err = %v)", err)
			}
			if IsFatal(err) {
				t.Fatalf("err = %v, want no stop", err)
			}
			if got := IsUnknownCharset(err); got != tc.warning {
				t.Errorf("err = %v, want unknown-charset warning %v", err, tc.warning)
			}
			if !tc.warning && err != nil {
				t.Errorf("err = %v, want nil", err)
			}
			if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, tc.codes) {
				t.Errorf("Diagnostics() = %v, want %v", got, tc.codes)
			}
			b, readErr := io.ReadAll(e.Body)
			if readErr != nil || string(b) != "body" {
				t.Errorf("body = (%q, %v), want %q", b, readErr, "body")
			}
		})
	}
}
