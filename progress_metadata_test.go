package message

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Tests of the no-progress rule for the encoded words of the parameters the
// parse engine interprets (ROB-001-007, progress.go). The scripted hook readers
// are the stallingDecoder of progress_test.go, capped like every reader there.

// checkMetadataStall checks that diags hold one header.decoder_no_progress for
// field, after want, located at the entity's header with its path.
func checkMetadataStall(t *testing.T, diags []Diagnostic, field string, want ...Code) {
	t.Helper()
	want = append(want, CodeHeaderDecoderNoProgress)
	if got := codesOfDiags(diags); !reflect.DeepEqual(got, want) {
		t.Fatalf("Diagnostics() = %v, want %v", got, want)
	}
	d := diags[len(diags)-1]
	if d.Severity != SeverityInvalidMetadata || d.Field != field || d.Cause != io.ErrNoProgress || d.Action != metadataNoProgressAction {
		t.Errorf("diagnostic = %+v, want SeverityInvalidMetadata on %s with io.ErrNoProgress", d, field)
	}
}

// TestParse_metadataNoProgress pins the whole-field reading: a hook reader that
// stalls keeps the raw encoded word of that parameter while the media type,
// the other parameters and the body stay usable, under every profile.
func TestParse_metadataNoProgress(t *testing.T) {
	const word = "=?x-meta?Q?abc?="
	text := "Content-Type: text/plain; charset=utf-8; note=\"" + word + "\"\r\n\r\nbody"
	for _, k := range []int{0, 1, 99, 100, 101} {
		for _, profile := range progressProfiles {
			for lname, limits := range map[string]Limits{"default": {}, "disabled": allDisabled()} {
				t.Run(fmt.Sprintf("%d/%v/%s", k, profile, lname), func(t *testing.T) {
					built := withStallingDecoders(t, func(d *stallingDecoder) { d.zeros = k })
					got := consumeStall(strings.NewReader(text), &ParseOptions{Profile: profile, Limits: limits})
					if got.parseErr != nil || got.bodies[0] != "body" || got.entity.ParseStop() != nil {
						t.Fatalf("Parse() = %v, body %q, stop %v", got.parseErr, got.bodies[0], got.entity.ParseStop())
					}
					_, params, err := got.entity.EffectiveContentType()
					want := map[string]string{"charset": "utf-8", "note": "abc"}
					if k >= maxConsecutiveEmptyReads {
						want["note"] = word
					}
					if err != nil || !reflect.DeepEqual(params, want) {
						t.Errorf("EffectiveContentType() params %v, %v; want %v", params, err, want)
					}
					if d := (*built)[0]; d.zeroCalls != min(k, maxConsecutiveEmptyReads) {
						t.Errorf("hook reader returned %d zeros, want %d", d.zeroCalls, min(k, maxConsecutiveEmptyReads))
					}
					if k < maxConsecutiveEmptyReads {
						// The word was decoded, which ROB-050-008 reports: a
						// parameter value may not hold one.
						if got := codesOfDiags(got.session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeMIMEParameterRFC2047}) {
							t.Errorf("Diagnostics() = %v, want only the decoded word's", got)
						}
						return
					}
					checkMetadataStall(t, got.session.Diagnostics(), "Content-Type")
					if d := got.session.Diagnostics()[0]; d.Path != nil || d.Location != LocationAt(0) {
						t.Errorf("diagnostic at %v path %v, want the root header", d.Location, d.Path)
					}
				})
			}
		}
	}
}

// TestParse_metadataNoProgressBounds pins that every stalling value of a field
// is stopped on its own while the field records one diagnostic, that a reader
// taking its input while it returns nothing is not stopped, and that the
// boundary and children of a multipart survive a stalled parameter.
func TestParse_metadataNoProgressBounds(t *testing.T) {
	t.Run("two stalling values, one diagnostic", func(t *testing.T) {
		built := withStallingDecoders(t, func(d *stallingDecoder) { d.zeros = 1000 })
		text := "Content-Type: text/plain; a=\"=?x-meta?Q?one?=\"; b=\"=?x-meta?Q?two?=\"\r\n\r\nbody"
		got := consumeStall(strings.NewReader(text), nil)
		_, params, _ := got.entity.EffectiveContentType()
		if params["a"] != "=?x-meta?Q?one?=" || params["b"] != "=?x-meta?Q?two?=" || len(*built) != 2 {
			t.Errorf("params %v after %d hook readers, want both raw, each tried once", params, len(*built))
		}
		for _, d := range *built {
			if d.zeroCalls != maxConsecutiveEmptyReads {
				t.Errorf("a hook reader returned %d zeros, want 100", d.zeroCalls)
			}
		}
		checkMetadataStall(t, got.session.Diagnostics(), "Content-Type")
	})

	for _, slow := range []int{250, 1000} {
		t.Run(fmt.Sprintf("taking %d bytes while returning nothing", slow), func(t *testing.T) {
			withStallingDecoders(t, func(d *stallingDecoder) { d.slow = slow })
			content := strings.Repeat("a", 1200)
			text := "Content-Type: text/plain; note=\"=?x-meta?Q?" + content + "?=\"\r\n\r\nbody"
			got := consumeStall(strings.NewReader(text), &ParseOptions{Limits: allDisabled()})
			_, params, _ := got.entity.EffectiveContentType()
			if codes := codesOfDiags(obDropOverlong(got.session.Diagnostics())); params["note"] != content || !reflect.DeepEqual(codes, []Code{CodeMIMEParameterRFC2047}) {
				t.Errorf("note of %d bytes, %v; want it decoded, and reported as a decoded word only", len(params["note"]), codes)
			}
		})
	}

	t.Run("a multipart keeps its boundary", func(t *testing.T) {
		withStallingDecoders(t, func(d *stallingDecoder) { d.zeros = 1000 })
		text := "Content-Type: multipart/mixed; note=\"=?x-meta?Q?n?=\"; boundary=B\r\n\r\n--B\r\n\r\nchild\r\n--B--\r\n"
		got := consumeStall(strings.NewReader(text), nil)
		if !reflect.DeepEqual(got.bodies, []string{"child"}) || got.iterErr != io.EOF {
			t.Errorf("bodies %q, iteration %v; want the child", got.bodies, got.iterErr)
		}
		checkMetadataStall(t, got.session.Diagnostics(), "Content-Type")
	})

	t.Run("the hook reader's copy shortcut is not used", func(t *testing.T) {
		var wroteTo bool
		withCharsetReader(t, func(_ string, in io.Reader) (io.Reader, error) {
			return &writerToStall{wroteTo: &wroteTo}, nil
		})
		text := "Content-Type: text/plain; note=\"=?x-meta?Q?abc?=\"\r\n\r\nbody"
		got := consumeStall(strings.NewReader(text), nil)
		if wroteTo {
			t.Error("io.Copy went through the hook reader's WriteTo")
		}
		checkMetadataStall(t, got.session.Diagnostics(), "Content-Type")
	})
}

// writerToStall returns (0, nil) from Read, up to a cap, and offers a WriteTo
// that must never be used.
type writerToStall struct {
	calls   int
	wroteTo *bool
}

func (w *writerToStall) Read([]byte) (int, error) {
	w.calls++
	if w.calls > 1000 {
		return 0, errCallCap
	}
	return 0, nil
}

func (w *writerToStall) WriteTo(io.Writer) (int64, error) {
	*w.wroteTo = true
	return 0, errors.New("WriteTo must not be used")
}

// TestParse_metadataNoProgressPaths pins the reading of a field that needs
// repair, of a retained prefix, and of a lazily read disposition.
func TestParse_metadataNoProgressPaths(t *testing.T) {
	const word = "=?x-meta?Q?abc?="

	t.Run("repaired item by item", func(t *testing.T) {
		for _, profile := range progressProfiles {
			withStallingDecoders(t, func(d *stallingDecoder) { d.zeros = 1000 })
			text := "Content-Type: text/plain; note=\"" + word + "\"; broken\r\n\r\nbody"
			s, e, err := Parse(strings.NewReader(text), &ParseOptions{Profile: profile})
			checkMetadataStall(t, s.Diagnostics(), "Content-Type", CodeMIMEContentTypeInvalid)
			if e == nil || e.ParseStop() != nil {
				t.Fatalf("%v: Parse() = %v", profile, err)
			}
			if profile == ProfileStrict {
				if !IsFatal(err) {
					t.Errorf("strict: Parse() = %v, want the entity-scoped refusal", err)
				}
				continue
			}
			if _, params, perr := e.EffectiveContentType(); err != nil || perr != nil || params["note"] != word {
				t.Errorf("%v: Parse() = %v, params %v, %v; want the raw word kept", profile, err, params, perr)
			}
		}
	})

	t.Run("the retained prefix of a field past its budget", func(t *testing.T) {
		withStallingDecoders(t, func(d *stallingDecoder) { d.zeros = 1000 })
		text := "Content-Type: text/plain; note=\"" + word + "\"; a=1; b=2\r\n\r\nbody"
		s, e, err := Parse(strings.NewReader(text), &ParseOptions{Limits: paramsOnly(1, 0)})
		if err != nil {
			t.Fatal(err)
		}
		checkMetadataStall(t, s.Diagnostics(), "Content-Type", CodeLimitParameterCount)
		if _, params, _ := e.EffectiveContentType(); !reflect.DeepEqual(params, map[string]string{"note": word}) {
			t.Errorf("params %v", params)
		}
	})

	t.Run("a lazily read disposition, read back by the sink", func(t *testing.T) {
		built := withStallingDecoders(t, func(d *stallingDecoder) { d.zeros = 1000 })
		var ent *Entity
		var fromSink string
		sink := DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
			if d.Code == CodeHeaderDecoderNoProgress {
				_, params, _ := ent.EffectiveContentDisposition()
				fromSink = params["filename"]
			}
			return nil
		})
		text := "Content-Type: text/plain\r\nContent-Disposition: attachment; filename=\"" + word + "\"\r\n\r\nbody"
		s, e, err := Parse(strings.NewReader(text), &ParseOptions{DiagnosticSink: sink})
		if err != nil || s.Len() != 0 {
			t.Fatalf("Parse() = %v, %v", err, codesOfDiags(s.Diagnostics()))
		}
		ent = e
		done := make(chan map[string]string, 1)
		go func() {
			_, params, _ := e.EffectiveContentDisposition()
			done <- params
		}()
		var params map[string]string
		select {
		case params = <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("EffectiveContentDisposition() did not return: the sink waited on the interpretation it is told about")
		}
		if params["filename"] != word || fromSink != word {
			t.Errorf("filename %q, from the sink %q; want the raw word", params["filename"], fromSink)
		}
		checkMetadataStall(t, s.Diagnostics(), "Content-Disposition")
		e.EffectiveContentDisposition()
		if s.Len() != 1 || len(*built) != 1 {
			t.Errorf("a second reading recorded %d diagnostics and built %d hook readers, want 1 and 1", s.Len(), len(*built))
		}
	})

	t.Run("a disposition read by the charset stage", func(t *testing.T) {
		withStallingDecoders(t, func(d *stallingDecoder) { d.zeros = 1000 })
		text := "Content-Type: text/plain; charset=utf-8\r\nContent-Disposition: attachment; filename=\"" + word + "\"\r\n\r\nbody"
		s, e, err := Parse(strings.NewReader(text), &ParseOptions{ReadOptions: ReadOptions{SkipTextAttachmentDecoding: true}})
		if err != nil || e == nil {
			t.Fatalf("Parse() = %v", err)
		}
		checkMetadataStall(t, s.Diagnostics(), "Content-Disposition")
		if disp, _, _ := e.EffectiveContentDisposition(); disp != "attachment" {
			t.Errorf("disposition %q, want attachment", disp)
		}
	})
}

// TestParse_metadataNoProgressIsOurs pins what is not a stall: a hook reader's
// own io.ErrNoProgress and a hook that fails stay the ordinary undecoded value
// with no diagnostic, a cancellation reached inside the copy discards the
// reading as ParseContext does, and the legacy path is unchanged.
func TestParse_metadataNoProgressIsOurs(t *testing.T) {
	const word = "=?x-meta?Q?abc?="
	text := "Content-Type: text/plain; note=\"" + word + "\"\r\n\r\nbody"

	t.Run("a hook reader's own io.ErrNoProgress", func(t *testing.T) {
		withCharsetReader(t, func(string, io.Reader) (io.Reader, error) {
			return readerFunc(func([]byte) (int, error) { return 0, io.ErrNoProgress }), nil
		})
		got := consumeStall(strings.NewReader(text), nil)
		if _, params, _ := got.entity.EffectiveContentType(); params["note"] != word || got.session.Len() != 0 {
			t.Errorf("note %q, %v; want the raw word and no diagnostic", params["note"], codesOfDiags(got.session.Diagnostics()))
		}
	})

	t.Run("a hook that fails", func(t *testing.T) {
		withCharsetReader(t, func(string, io.Reader) (io.Reader, error) { return nil, errors.New("no such charset") })
		got := consumeStall(strings.NewReader(text), nil)
		if _, params, _ := got.entity.EffectiveContentType(); params["note"] != word || got.session.Len() != 0 {
			t.Errorf("note %q, %v; want the raw word and no diagnostic", params["note"], codesOfDiags(got.session.Diagnostics()))
		}
	})

	t.Run("cancelled inside the copy", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		built := withStallingDecoders(t, func(d *stallingDecoder) {
			d.zeros = 1000
			d.onZero = func(n int) {
				if n == 50 {
					cancel()
				}
			}
		})
		s, _, err := ParseContext(ctx, strings.NewReader(text), nil)
		if !errors.Is(err, context.Canceled) || s.Len() != 0 || (*built)[0].zeroCalls != 50 {
			t.Errorf("ParseContext() = %v, %v, %d hook zeros; want the cancellation, nothing recorded, 50",
				err, codesOfDiags(s.Diagnostics()), (*built)[0].zeroCalls)
		}
	})

	t.Run("Read is unchanged", func(t *testing.T) {
		withStallingDecoders(t, func(d *stallingDecoder) { d.zeros = 150 })
		e, err := Read(strings.NewReader(text))
		if err != nil {
			t.Fatal(err)
		}
		if _, params, _ := e.EffectiveContentType(); params["note"] != "abc" {
			t.Errorf("note %q, want the legacy path to wait out 150 empty reads as before", params["note"])
		}
	})
}

// withSalvageCharsets installs a CharsetReader whose x-stall readers return
// (0, nil) without taking input and whose other readers pass their input
// through, and returns how many readers it built per charset.
func withSalvageCharsets(t *testing.T) map[string]int {
	t.Helper()
	built := map[string]int{}
	withCharsetReader(t, func(cs string, in io.Reader) (io.Reader, error) {
		built[cs]++
		d := &stallingDecoder{in: in, ceil: 20000}
		if cs == "x-stall" {
			d.zeros = 1000
		}
		return d, nil
	})
	return built
}

// TestParse_metadataNoProgressSalvage pins that a stalled value never costs the
// field's other values: whatever order a field's parameters are visited in, a
// stalled value stays raw, every other value decodes, a boundary decoded from an
// encoded word still frames the children, and the field records one
// diagnostic — on the whole-field reading, the repair and the retained prefix.
// Each case is parsed repeatedly, since parameters are visited in map order.
func TestParse_metadataNoProgressSalvage(t *testing.T) {
	const (
		stalled = "=?x-stall?Q?a?="
		good    = "=?x-good?Q?GOOD?="
	)
	pair := "a=\"" + stalled + "\"; z=\"" + good + "\""
	want := map[string]string{"a": stalled, "z": "GOOD"}
	cases := []struct {
		name   string
		text   string
		limits Limits
		codes  []Code // before the one header.decoder_no_progress
		stalls int    // x-stall readers built per parse
	}{
		// The word of z decodes, which is reported (ROB-050-008); the word of a
		// stalls and is not.
		{"whole field", "Content-Type: text/plain; " + pair + "\r\n\r\nbody", Limits{},
			[]Code{CodeMIMEParameterRFC2047}, 1},
		{"repaired", "Content-Type: text/plain; " + pair + "; broken\r\n\r\nbody", Limits{},
			[]Code{CodeMIMEContentTypeInvalid, CodeMIMEParameterRFC2047}, 1},
		{"retained prefix", "Content-Type: text/plain; " + pair + "; b=1; c=2\r\n\r\nbody", paramsOnly(2, 0),
			[]Code{CodeLimitParameterCount, CodeMIMEParameterRFC2047}, 1},
		// A field with a stray empty item before the cut is read item by item;
		// the value that stalled is handed to the hook once.
		{"retained prefix, then repaired", "Content-Type: text/plain; a=\"" + stalled + "\";; z=\"" + good + "\"; b=1\r\n\r\nbody",
			paramsOnly(2, 0), []Code{CodeLimitParameterCount, CodeMIMEContentTypeInvalid, CodeMIMEParameterRFC2047}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := range 32 {
				built := withSalvageCharsets(t)
				s, e, err := Parse(strings.NewReader(tc.text), &ParseOptions{Limits: tc.limits})
				if err != nil || e == nil {
					t.Fatalf("parse %d: Parse() = %v", i, err)
				}
				if _, params, _ := e.EffectiveContentType(); !reflect.DeepEqual(params, want) {
					t.Fatalf("parse %d: params %v, want %v", i, params, want)
				}
				if built["x-stall"] != tc.stalls {
					t.Errorf("parse %d: %d x-stall readers, want %d", i, built["x-stall"], tc.stalls)
				}
				checkMetadataStall(t, s.Diagnostics(), "Content-Type", tc.codes...)
			}
		})
	}

	t.Run("a boundary after a stalled value", func(t *testing.T) {
		text := "Content-Type: multipart/mixed; note=\"" + stalled + "\"; boundary=\"=?x-good?Q?B?=\"\r\n\r\n" +
			"--B\r\n\r\nchild\r\n--B--\r\n"
		for i := range 32 {
			withSalvageCharsets(t)
			got := consumeStall(strings.NewReader(text), nil)
			if !reflect.DeepEqual(got.bodies, []string{"child"}) || got.iterErr != io.EOF {
				t.Fatalf("parse %d: bodies %q, iteration %v; want the child framed by the decoded boundary", i, got.bodies, got.iterErr)
			}
			_, params, _ := got.entity.EffectiveContentType()
			if params["boundary"] != "B" || params["note"] != stalled {
				t.Fatalf("parse %d: params %v", i, params)
			}
			// The boundary is itself an encoded word, decoded and reported.
			checkMetadataStall(t, got.session.Diagnostics(), "Content-Type", CodeMIMEParameterRFC2047)
		}
	})
}
