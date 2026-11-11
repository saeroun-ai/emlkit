package mail_test

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// ROB-100-007 (issue #15): mail.Reader classifies a part from the metadata the
// parse engine interpreted it with — under the parse's parameter budgets, and
// read once — rather than by reparsing the raw header with no budget, which
// used to undo what the engine had decided. The synthetic wrapper NewReader
// builds around a single-part message also answers to the parse's stop.

// boundedLimits budgets one parameter per field, so a field with two
// parameters past its type is cut to the first and diagnosed.
func boundedLimits() message.Limits {
	return message.Limits{MaxParameterCount: 1}
}

// issue15Root is the exact reproduction from issue #15.
const issue15Root = "Content-Type: text/plain; charset=utf-8\r\nContent-Disposition: attachment; a=1; a=2\r\n\r\nbody"

// treeAround puts the given part header and body between two readable siblings.
func treeAround(header, body string) string {
	return "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nzero\r\n" +
		"--B\r\n" + header + "\r\n" + body + "\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\ndHdv\r\n" +
		"--B--\r\n"
}

// parseAll parses text under opts and reads every part through mail.Reader.
func parseAll(t *testing.T, text string, opts *message.ParseOptions) (*message.ParseSession, []readPart, error) {
	t.Helper()
	session, e, err := message.Parse(strings.NewReader(text), opts)
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	mr := mail.NewReader(e)
	defer mr.Close()
	parts, err := readAll(t, mr)
	return session, parts, err
}

func codes(diags []message.Diagnostic) []message.Code {
	var out []message.Code
	for _, d := range diags {
		out = append(out, d.Code)
	}
	return out
}

func TestReader_issue15RootClassifiedFromBoundedDisposition(t *testing.T) {
	for _, skip := range []bool{true, false} {
		t.Run(fmt.Sprintf("SkipTextAttachmentDecoding=%v", skip), func(t *testing.T) {
			session, e, err := message.Parse(strings.NewReader(issue15Root), &message.ParseOptions{
				Limits:      boundedLimits(),
				ReadOptions: message.ReadOptions{SkipTextAttachmentDecoding: skip},
			})
			if err != nil || e == nil {
				t.Fatalf("Parse() = (%v, %v)", e, err)
			}
			p, err := mail.NewReader(e).NextPart()
			if err != nil {
				t.Fatalf("NextPart() = %v", err)
			}
			ah, ok := p.Header.(*mail.AttachmentHeader)
			if !ok {
				t.Fatalf("NextPart().Header = %T, want *mail.AttachmentHeader: the bounded disposition is attachment", p.Header)
			}
			if b, _ := io.ReadAll(p.Body); string(b) != "body" {
				t.Errorf("body = %q, want %q", b, "body")
			}
			diags := session.Diagnostics()
			if got := codes(diags); !reflect.DeepEqual(got, []message.Code{message.CodeLimitParameterCount}) {
				t.Fatalf("Diagnostics() = %v, want exactly one limit.parameter_count", got)
			}
			if diags[0].Field != "Content-Disposition" || diags[0].Path != nil {
				t.Errorf("diagnostic = %+v, want the root's Content-Disposition", diags[0])
			}
			// The raw field and the standalone, unbudgeted accessor are unchanged:
			// the duplicate parameter is still a parse error there.
			if got := ah.Get("Content-Disposition"); got != "attachment; a=1; a=2" {
				t.Errorf("raw Content-Disposition = %q", got)
			}
			if disp, params, err := ah.ContentDisposition(); err == nil || disp != "attachment; a=1; a=2" || params != nil {
				t.Errorf("Header.ContentDisposition() = (%q, %v, %v), want the legacy unbudgeted parse error", disp, params, err)
			}
			if got := codes(session.Diagnostics()); len(got) != 1 {
				t.Errorf("Diagnostics() after reading the header = %v, want no new diagnostic", got)
			}
		})
	}
}

// classified is one NextPart result, with its body read whole.
type classified struct {
	attachment bool
	body       string
	err        error
	header     mail.PartHeader
}

func classifyAll(t *testing.T, mr *mail.Reader) ([]classified, error) {
	t.Helper()
	var out []classified
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return out, nil
		}
		if p == nil {
			if err == nil {
				t.Fatal("NextPart() = (nil, nil)")
			}
			return out, err
		}
		b, readErr := io.ReadAll(p.Body)
		if readErr != nil {
			t.Fatalf("reading a part body = %v", readErr)
		}
		_, attachment := p.Header.(*mail.AttachmentHeader)
		out = append(out, classified{attachment: attachment, body: string(b), err: err, header: p.Header})
	}
}

// TestReader_classificationFollowsTheBoundedMetadata is the matrix: every row
// is a part between two readable siblings, read through Parse with one
// parameter per field, with and without SkipTextAttachmentDecoding. The
// charset x-made-up has no decoder, so whether the charset stage ran is
// visible as the unknown-charset warning and diagnostic: an attachment must
// never be offered as inline text after its charset was skipped, and no
// diagnostic may be recorded twice.
func TestReader_classificationFollowsTheBoundedMetadata(t *testing.T) {
	const body = "caf\xe9 =E9 raw"
	type want struct {
		attachment bool
		// charsetRan is whether the charset stage ran on the part, which shows
		// as the unknown-charset warning.
		charsetRan bool
		codes      []message.Code
	}
	for _, tc := range []struct {
		name   string
		header string
		// skip and noSkip are the outcomes with and without
		// SkipTextAttachmentDecoding.
		skip, noSkip want
	}{
		{
			name:   "duplicate disposition parameter past the count budget is an attachment",
			header: "Content-Type: text/plain; charset=x-made-up\r\nContent-Disposition: attachment; a=1; a=2\r\n",
			skip:   want{true, false, []message.Code{message.CodeLimitParameterCount}},
			noSkip: want{true, true, []message.Code{message.CodeCharsetUnknown, message.CodeLimitParameterCount}},
		},
		{
			name:   "inline disposition past the count budget stays inline",
			header: "Content-Type: text/plain; charset=x-made-up\r\nContent-Disposition: inline; a=1; b=2\r\n",
			skip:   want{false, true, []message.Code{message.CodeLimitParameterCount, message.CodeCharsetUnknown}},
			noSkip: want{false, true, []message.Code{message.CodeCharsetUnknown, message.CodeLimitParameterCount}},
		},
		{
			name:   "unterminated quote past the budget keeps the attachment before it",
			header: "Content-Type: text/plain; charset=x-made-up\r\nContent-Disposition: attachment; a=1; filename=\"cut\r\n",
			skip:   want{true, false, []message.Code{message.CodeLimitParameterCount}},
			noSkip: want{true, true, []message.Code{message.CodeCharsetUnknown, message.CodeLimitParameterCount}},
		},
		{
			name:   "segments past the segment budget keep the attachment and drop the series",
			header: "Content-Type: text/plain; charset=x-made-up\r\nContent-Disposition: attachment; filename*0=a; filename*1=b\r\n",
			skip:   want{true, false, []message.Code{message.CodeLimitParameterSegments}},
			noSkip: want{true, true, []message.Code{message.CodeCharsetUnknown, message.CodeLimitParameterSegments}},
		},
		{
			name:   "non-text media type past the count budget with no disposition is an attachment",
			header: "Content-Type: application/x-thing; a=1; b=2\r\n",
			skip:   want{true, false, []message.Code{message.CodeLimitParameterCount}},
			noSkip: want{true, false, []message.Code{message.CodeLimitParameterCount}},
		},
		{
			// The charset parameter is past the Content-Type budget, so there is
			// no charset to convert: no stage runs and nothing claims there was.
			// The body is then text with no charset, and its 8-bit bytes are
			// observed as such (charset.missing, info) where the body is watched.
			name:   "charset past the content-type budget is not applied",
			header: "Content-Type: text/plain; a=1; charset=x-made-up\r\n",
			skip:   want{false, false, []message.Code{message.CodeLimitParameterCount}},
			noSkip: want{false, false, []message.Code{message.CodeLimitParameterCount, message.CodeCharsetMissing}},
		},
	} {
		for _, skip := range []bool{true, false} {
			w := tc.noSkip
			if skip {
				w = tc.skip
			}
			t.Run(fmt.Sprintf("%s/skip=%v", tc.name, skip), func(t *testing.T) {
				// A segment budget needs its own limit; the count budget alone is
				// one per field, which a two-segment series would exceed first.
				limits := boundedLimits()
				if strings.Contains(tc.header, "filename*0") {
					limits = message.Limits{MaxParameterSegments: 1}
				}
				session, e, err := message.Parse(strings.NewReader(treeAround(tc.header, body)), &message.ParseOptions{
					Limits:      limits,
					ReadOptions: message.ReadOptions{SkipTextAttachmentDecoding: skip},
				})
				if err != nil {
					t.Fatalf("Parse() = %v", err)
				}
				mr := mail.NewReader(e)
				defer mr.Close()
				parts, err := classifyAll(t, mr)
				if err != nil {
					t.Fatalf("iteration ended with %v, want every sibling", err)
				}
				if len(parts) != 3 {
					t.Fatalf("got %d parts, want 3", len(parts))
				}
				if parts[0].attachment || parts[0].body != "zero" || parts[0].err != nil {
					t.Errorf("first sibling = %+v, want the inline %q", parts[0], "zero")
				}
				if !parts[2].attachment || parts[2].body != "two" || parts[2].err != nil {
					t.Errorf("last sibling = %+v, want the decoded attachment %q", parts[2], "two")
				}

				got := parts[1]
				if got.attachment != w.attachment {
					t.Errorf("affected part attachment = %v, want %v", got.attachment, w.attachment)
				}
				if ran := message.IsUnknownCharset(got.err); ran != w.charsetRan {
					t.Errorf("affected part err = %v, want the charset stage to have run: %v", got.err, w.charsetRan)
				}
				if message.IsFatal(got.err) {
					t.Errorf("affected part err = %v, want no stop", got.err)
				}
				// Neither stage changes these bytes: x-made-up has no decoder,
				// and no transfer encoding is declared.
				if got.body != body {
					t.Errorf("affected part body = %q, want the exact bytes %q", got.body, body)
				}
				diags := session.Diagnostics()
				if gotCodes := codes(diags); !reflect.DeepEqual(gotCodes, w.codes) {
					t.Errorf("Diagnostics() = %v, want %v", gotCodes, w.codes)
				}
				for _, d := range diags {
					if !reflect.DeepEqual(d.Path, []int{1}) {
						t.Errorf("diagnostic %v path = %v, want [1]", d.Code, d.Path)
					}
				}
			})
		}
	}
}

// TestReader_issue15Child is the reproduction on a child: the same verdict and
// the same single diagnostic as the root, with both siblings intact.
func TestReader_issue15Child(t *testing.T) {
	for _, skip := range []bool{true, false} {
		t.Run(fmt.Sprintf("skip=%v", skip), func(t *testing.T) {
			header := "Content-Type: text/plain; charset=utf-8\r\nContent-Disposition: attachment; a=1; a=2\r\n"
			session, parts, err := parseAll(t, treeAround(header, "body"), &message.ParseOptions{
				Limits:      boundedLimits(),
				ReadOptions: message.ReadOptions{SkipTextAttachmentDecoding: skip},
			})
			if err != nil {
				t.Fatalf("iteration ended with %v", err)
			}
			var bodies []string
			var inline []bool
			for _, p := range parts {
				bodies = append(bodies, p.body)
				inline = append(inline, p.inline)
			}
			if !reflect.DeepEqual(bodies, []string{"zero", "body", "two"}) || !reflect.DeepEqual(inline, []bool{true, false, false}) {
				t.Errorf("parts = %v inline %v, want zero/body/two with only the first inline", bodies, inline)
			}
			if got := codes(session.Diagnostics()); !reflect.DeepEqual(got, []message.Code{message.CodeLimitParameterCount}) {
				t.Errorf("Diagnostics() = %v, want one limit.parameter_count", got)
			}
		})
	}
}

// TestReader_withoutBudgetsKeepsLegacyClassification pins the legacy path and
// how Parse differs from it. Read and CreateReader have no budgets and no
// repair, so the duplicate parameter is a parse error there, as it is for
// Header.ContentDisposition, and the part stays inline text. Under Parse with
// budgets the field fits within, the whole field fails to parse and is then
// repaired by ROB-050-001 (#22): its valid base "attachment" is kept, the
// conflicting duplicate a takes its first value by the precedence of
// ROB-050-005 (before it, both were dropped), one mime.parameter_duplicate is
// recorded, and the part is the attachment its base declares.
func TestReader_withoutBudgetsKeepsLegacyClassification(t *testing.T) {
	t.Run("CreateReader", func(t *testing.T) {
		mr, err := mail.CreateReader(strings.NewReader(issue15Root))
		if err != nil {
			t.Fatalf("CreateReader() = %v", err)
		}
		parts, err := classifyAll(t, mr)
		if err != nil || len(parts) != 1 || parts[0].attachment || parts[0].body != "body" {
			t.Errorf("parts = %+v, %v; want the one inline part", parts, err)
		}
	})
	t.Run("Parse within the budget", func(t *testing.T) {
		session, e, err := message.Parse(strings.NewReader(issue15Root), &message.ParseOptions{
			ReadOptions: message.ReadOptions{SkipTextAttachmentDecoding: true},
		})
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		disp, params, dispErr := e.EffectiveContentDisposition()
		if dispErr != nil || disp != "attachment" || !reflect.DeepEqual(params, map[string]string{"a": "1"}) {
			t.Errorf("EffectiveContentDisposition() = (%q, %v, %v), want the repaired attachment with the first of the conflicting a", disp, params, dispErr)
		}
		parts, err := classifyAll(t, mail.NewReader(e))
		if err != nil || len(parts) != 1 || !parts[0].attachment || parts[0].body != "body" {
			t.Errorf("parts = %+v, %v; want the one attachment", parts, err)
		}
		if got := codes(session.Diagnostics()); !reflect.DeepEqual(got, []message.Code{message.CodeMIMEParameterDuplicate}) {
			t.Errorf("Diagnostics() = %v, want one mime.parameter_duplicate and no budget diagnostic", got)
		}
		if _, _, err := e.Header.ContentDisposition(); err == nil {
			t.Error("Header.ContentDisposition() = nil error, want the legacy unbudgeted parse error")
		}
	})
}

// TestReader_stoppedParseStaysStopped covers a DiagnosticSink that stops the
// parse on the budget diagnostic, wherever it is recorded: while the root is
// constructed (the charset stage reads the disposition), while mail.Reader
// classifies the root (nothing read it before), and while it classifies a
// child. In every case NextPart returns the identical published stop and no
// part, on every retry; one diagnostic is recorded; and a stop wrapping a
// decoding warning is still the stop.
func TestReader_stoppedParseStaysStopped(t *testing.T) {
	warnings := realWarnings(t)
	for _, stopErr := range []struct {
		name string
		err  error
	}{
		{"sentinel", errors.New("stop requested")},
		{"wrapping an encoding warning", fmt.Errorf("stop: %w", warnings["unknown encoding"])},
		{"wrapping a charset warning", fmt.Errorf("stop: %w", warnings["unknown charset"])},
	} {
		sink := message.DiagnosticSinkFunc(func(_ *message.ParseSession, d message.Diagnostic) error {
			if d.Code == message.CodeLimitParameterCount {
				return stopErr.err
			}
			return nil
		})
		for _, tc := range []struct {
			name string
			text string
			skip bool
			// parseStops is whether Parse itself already returns the stop.
			parseStops bool
			// before is how many parts NextPart returns before the stop.
			before []string
		}{
			{"root, stopped during Parse", issue15Root, true, true, nil},
			{"root, stopped while classified", issue15Root, false, false, nil},
			// The charset stage reads the disposition only for a text part that
			// declares a charset, so that is what puts the stop inside the
			// multipart iterator.
			{"child, stopped during iteration", treeAround("Content-Type: text/plain; charset=utf-8\r\nContent-Disposition: attachment; a=1; a=2\r\n", "body"), true, false, []string{"zero"}},
			{"child without charset, stopped while classified", treeAround("Content-Type: text/plain\r\nContent-Disposition: attachment; a=1; a=2\r\n", "body"), true, false, []string{"zero"}},
			{"child, stopped while classified", treeAround("Content-Type: text/plain; charset=utf-8\r\nContent-Disposition: attachment; a=1; a=2\r\n", "body"), false, false, []string{"zero"}},
		} {
			t.Run(stopErr.name+"/"+tc.name, func(t *testing.T) {
				session, e, err := message.Parse(strings.NewReader(tc.text), &message.ParseOptions{
					Limits:         boundedLimits(),
					ReadOptions:    message.ReadOptions{SkipTextAttachmentDecoding: tc.skip},
					DiagnosticSink: sink,
				})
				if e == nil {
					t.Fatalf("Parse() returned no entity (err = %v)", err)
				}
				if tc.parseStops != message.IsFatal(err) {
					t.Fatalf("Parse() err = %v, want a stop: %v", err, tc.parseStops)
				}
				if tc.parseStops && err != e.ParseStop() {
					t.Errorf("Parse() err = %v, want the identical published stop %v", err, e.ParseStop())
				}

				mr := mail.NewReader(e)
				defer mr.Close()
				parts, stop := classifyAll(t, mr)
				if !message.IsFatal(stop) || !errors.Is(stop, stopErr.err) {
					t.Fatalf("iteration ended with %v, want the sink's stop", stop)
				}
				if stop != e.ParseStop() {
					t.Errorf("iteration ended with %v, want the identical published stop %v", stop, e.ParseStop())
				}
				var bodies []string
				for _, p := range parts {
					bodies = append(bodies, p.body)
				}
				if !reflect.DeepEqual(bodies, tc.before) {
					t.Errorf("parts before the stop = %q, want %q", bodies, tc.before)
				}
				for i := range 3 {
					if p, err := mr.NextPart(); p != nil || err != stop {
						t.Errorf("NextPart() retry %d = (%v, %v), want (nil, the identical stop)", i, p, err)
					}
				}
				if got := codes(session.Diagnostics()); !reflect.DeepEqual(got, []message.Code{message.CodeLimitParameterCount}) {
					t.Errorf("Diagnostics() = %v, want one limit.parameter_count", got)
				}
				// The stopped root keeps its header and, read directly, its body.
				if got := mr.Header.Get("Content-Type"); got == "" {
					t.Error("Reader.Header lost the root header")
				}
			})
		}
	}
}

// TestReader_unknownDecodersStayWarningsUnderParse keeps issue #1's contract on
// the new classification path: a root with an unknown transfer encoding or
// charset, parsed under budgets, is still one usable part, its warning came
// from Parse, and nothing is stopped.
func TestReader_unknownDecodersStayWarningsUnderParse(t *testing.T) {
	for _, header := range []string{unknownCTE, unknownCharset, unknownBoth} {
		session, e, err := message.Parse(strings.NewReader(header+"Content-Disposition: attachment; a=1; b=2\r\n\r\nraw=XX"), &message.ParseOptions{
			Limits: boundedLimits(),
		})
		if e == nil || message.IsFatal(err) || !(message.IsUnknownEncoding(err) || message.IsUnknownCharset(err)) {
			t.Fatalf("Parse(%q) = (%v, %v), want a usable entity and a warning", header, e, err)
		}
		mr := mail.NewReader(e)
		parts, err := classifyAll(t, mr)
		if err != nil || len(parts) != 1 || !parts[0].attachment || parts[0].body != "raw=XX" {
			t.Errorf("%q: parts = %+v, %v; want the one attachment with its raw body", header, parts, err)
		}
		if got := codes(session.Diagnostics()); got[len(got)-1] != message.CodeLimitParameterCount {
			t.Errorf("%q: Diagnostics() = %v, want the decoder warnings then the disposition budget", header, got)
		}
	}
}

// TestReader_classifiesTheConstructionSnapshot pins the mutation contract.
// Editing Entity.Header after Parse or New does not reclassify the entity —
// its body was decoded by the header as it was — while the edit is visible
// through the header itself and its standalone accessors. A struct-literal
// Entity has no snapshot and is classified by its current header, as before.
func TestReader_classifiesTheConstructionSnapshot(t *testing.T) {
	t.Run("Parse", func(t *testing.T) {
		_, e, err := message.Parse(strings.NewReader(issue15Root), &message.ParseOptions{
			Limits:      boundedLimits(),
			ReadOptions: message.ReadOptions{SkipTextAttachmentDecoding: true},
		})
		if err != nil {
			t.Fatalf("Parse() = %v", err)
		}
		e.Header.Set("Content-Disposition", "inline")
		e.Header.Set("Content-Type", "image/png")
		parts, err := classifyAll(t, mail.NewReader(e))
		if err != nil || len(parts) != 1 || !parts[0].attachment || parts[0].body != "body" {
			t.Fatalf("parts = %+v, %v; want the attachment it was parsed as", parts, err)
		}
		if disp, _, err := parts[0].header.(*mail.AttachmentHeader).ContentDisposition(); err != nil || disp != "inline" {
			t.Errorf("Header.ContentDisposition() = (%q, %v), want the edited value", disp, err)
		}
	})
	t.Run("New", func(t *testing.T) {
		var h message.Header
		h.Set("Content-Type", "text/plain")
		h.Set("Content-Disposition", "attachment")
		e, err := message.New(h, strings.NewReader("body"))
		if err != nil {
			t.Fatalf("New() = %v", err)
		}
		e.Header.Set("Content-Disposition", "inline")
		parts, err := classifyAll(t, mail.NewReader(e))
		if err != nil || len(parts) != 1 || !parts[0].attachment {
			t.Errorf("parts = %+v, %v; want the attachment it was constructed as", parts, err)
		}
	})
	t.Run("struct literal", func(t *testing.T) {
		var h message.Header
		h.Set("Content-Type", "text/plain")
		h.Set("Content-Disposition", "attachment")
		e := &message.Entity{Header: h, Body: strings.NewReader("body")}
		parts, err := classifyAll(t, mail.NewReader(e))
		if err != nil || len(parts) != 1 || !parts[0].attachment || parts[0].body != "body" {
			t.Errorf("parts = %+v, %v; want the attachment its header declares", parts, err)
		}
	})
}

// TestReader_nestedChunkedInputKeepsRawBytes reads a nested tree one byte at a
// time: the affected part sits inside a multipart/alternative inside the root,
// and must be classified from its bounded metadata with its diagnostic at the
// right path, its siblings intact, and every header written back byte for byte.
func TestReader_nestedChunkedInputKeepsRawBytes(t *testing.T) {
	affected := "Content-Type: text/plain; charset=utf-8\r\nContent-Disposition: attachment; a=1; a=2\r\n"
	text := "Content-Type: multipart/mixed; boundary=OUT\r\n\r\n" +
		"--OUT\r\nContent-Type: multipart/alternative; boundary=IN\r\n\r\n" +
		"--IN\r\nContent-Type: text/plain\r\n\r\nplain\r\n" +
		"--IN\r\n" + affected + "\r\nbody\r\n" +
		"--IN--\r\n" +
		"--OUT\r\nContent-Type: text/html\r\n\r\n<p>after</p>\r\n" +
		"--OUT--\r\n"
	for _, skip := range []bool{true, false} {
		t.Run(fmt.Sprintf("skip=%v", skip), func(t *testing.T) {
			session, e, err := message.Parse(iotest.OneByteReader(strings.NewReader(text)), &message.ParseOptions{
				Limits:      boundedLimits(),
				ReadOptions: message.ReadOptions{SkipTextAttachmentDecoding: skip},
			})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			mr := mail.NewReader(e)
			var bodies []string
			var attachments []bool
			var affectedHeader *mail.AttachmentHeader
			for {
				p, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("NextPart() = %v", err)
				}
				// Read the body in small chunks too.
				b, readErr := io.ReadAll(iotest.HalfReader(p.Body))
				if readErr != nil {
					t.Fatalf("reading a body = %v", readErr)
				}
				bodies = append(bodies, string(b))
				ah, ok := p.Header.(*mail.AttachmentHeader)
				attachments = append(attachments, ok)
				if ok {
					affectedHeader = ah
				}
			}
			if !reflect.DeepEqual(bodies, []string{"plain", "body", "<p>after</p>"}) || !reflect.DeepEqual(attachments, []bool{false, true, false}) {
				t.Fatalf("parts = %q attachments %v", bodies, attachments)
			}
			diags := session.Diagnostics()
			if len(diags) != 1 || diags[0].Code != message.CodeLimitParameterCount || !reflect.DeepEqual(diags[0].Path, []int{0, 1}) {
				t.Errorf("Diagnostics() = %+v, want one limit.parameter_count at [0 1]", diags)
			}
			var raw strings.Builder
			for fields := affectedHeader.Fields(); fields.Next(); {
				b, err := fields.Raw()
				if err != nil {
					t.Fatal(err)
				}
				raw.Write(b)
			}
			if raw.String() != affected {
				t.Errorf("affected header written back as %q, want the raw bytes %q", raw.String(), affected)
			}
		})
	}
}

// TestReader_stopWhileReadingTheRootBodyIsNotAnEOF covers the single-part
// wrapper after its one part was handed out: a sink that stops the parse while
// the root body is being read — here on its line budget — must be what the
// next NextPart reports, not an io.EOF that would present the stopped message
// as complete.
func TestReader_stopWhileReadingTheRootBodyIsNotAnEOF(t *testing.T) {
	sentinel := errors.New("stop on the body")
	text := "Content-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		"dHdv\r\n" + strings.Repeat("QUFB", 64) + "\r\n"
	session, e, err := message.Parse(strings.NewReader(text), &message.ParseOptions{
		Limits: message.Limits{MaxLineBytes: 80},
		DiagnosticSink: message.DiagnosticSinkFunc(func(_ *message.ParseSession, d message.Diagnostic) error {
			if d.Code == message.CodeLimitLineBytes {
				return sentinel
			}
			return nil
		}),
	})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	mr := mail.NewReader(e)
	p, err := mr.NextPart()
	if err != nil {
		t.Fatalf("NextPart() = %v, want the root part", err)
	}
	b, readErr := io.ReadAll(p.Body)
	if readErr == nil {
		t.Fatalf("reading the body = %q, nil; want the line budget to refuse the long line", b)
	}
	if !strings.HasPrefix(string(b), "two") {
		t.Errorf("body before the refusal = %q, want the bytes decoded before it", b)
	}
	for i := range 2 {
		if p, err := mr.NextPart(); p != nil || !message.IsFatal(err) || !errors.Is(err, sentinel) || err != e.ParseStop() {
			t.Errorf("NextPart() %d after the stop = (%v, %v), want (nil, the identical stop)", i, p, err)
		}
	}
	if got := codes(session.Diagnostics()); !reflect.DeepEqual(got, []message.Code{message.CodeLimitLineBytes}) {
		t.Errorf("Diagnostics() = %v, want one limit.line_bytes", got)
	}
}

// TestReader_sinkReadingThePartWhileItIsClassified covers the review finding
// through mail.Reader: classifying the root reads its disposition for the first
// time, the sink told about the budget reads the same entity back, and then
// stops the parse. NextPart must not block, must return the identical stop and
// no part on every retry, and the diagnostic is recorded once.
func TestReader_sinkReadingThePartWhileItIsClassified(t *testing.T) {
	sentinel := errors.New("stop after reading")
	var (
		target *message.Entity
		seen   []string
	)
	session, e, err := message.Parse(strings.NewReader(issue15Root), &message.ParseOptions{
		Limits: boundedLimits(),
		DiagnosticSink: message.DiagnosticSinkFunc(func(_ *message.ParseSession, d message.Diagnostic) error {
			if target == nil || d.Code != message.CodeLimitParameterCount {
				return nil
			}
			disp, params, _ := target.EffectiveContentDisposition()
			mt, _, _ := target.EffectiveContentType()
			seen = append(seen, fmt.Sprintf("%s %v %s %v", disp, params, mt, target.ParseStop()))
			return sentinel
		}),
	})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	target = e

	mr := mail.NewReader(e)
	var (
		p    *mail.Part
		stop error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p, stop = mr.NextPart()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("NextPart() did not return: the sink reading the part blocked its own delivery")
	}
	if p != nil || !message.IsFatal(stop) || !errors.Is(stop, sentinel) || stop != e.ParseStop() {
		t.Fatalf("NextPart() = (%v, %v), want (nil, the published stop)", p, stop)
	}
	if want := []string{"attachment map[a:1] text/plain <nil>"}; !reflect.DeepEqual(seen, want) {
		t.Errorf("sink saw %q, want %q", seen, want)
	}
	for i := range 3 {
		if p, err := mr.NextPart(); p != nil || err != stop {
			t.Errorf("NextPart() retry %d = (%v, %v), want (nil, the identical stop)", i, p, err)
		}
	}
	if disp, _, _ := e.EffectiveContentDisposition(); disp != "attachment" {
		t.Errorf("EffectiveContentDisposition() after the stop = %q, want the cached attachment", disp)
	}
	if got := codes(session.Diagnostics()); !reflect.DeepEqual(got, []message.Code{message.CodeLimitParameterCount}) {
		t.Errorf("Diagnostics() = %v, want one limit.parameter_count", got)
	}
}
