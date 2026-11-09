package mail_test

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// ROB-050-001 (issue #22) through mail.Reader: a part whose Content-Type or
// Content-Disposition keeps a valid base despite malformed parameters is
// classified by the repaired metadata, not discarded for the defect of its raw
// header; under ProfileStrict such a part is refused on its own and iteration
// continues with the next one.

const issue22 = "Content-Type: multipart/mixed; boundary=B; broken\r\n\r\n--B\r\n\r\nkept\r\n--B--\r\n"

func TestReader_issue22RepairedRoot(t *testing.T) {
	session, parts, err := parseAll(t, issue22, nil)
	if err != nil {
		t.Fatalf("iteration ended with %v", err)
	}
	if len(parts) != 1 || parts[0].body != "kept" || !parts[0].inline || parts[0].err != nil {
		t.Errorf("parts = %+v, want the kept inline part", parts)
	}
	if got := codes(session.Diagnostics()); !reflect.DeepEqual(got, []message.Code{message.CodeMIMEContentTypeInvalid}) {
		t.Errorf("Diagnostics() = %v, want one mime.content_type_invalid", got)
	}

	// The legacy path keeps its long-standing failure.
	mr, err := mail.CreateReader(strings.NewReader(issue22))
	if err != nil {
		t.Fatalf("CreateReader() = %v", err)
	}
	if p, err := mr.NextPart(); p != nil || err == nil {
		t.Errorf("legacy NextPart() = (%v, %v), want the empty-boundary error", p, err)
	}
}

// repairTree puts a part with the given header between two readable siblings.
func repairTree(header string) string {
	return treeAround(header, "caf\xe9")
}

func TestReader_repairedDispositionIsAnAttachment(t *testing.T) {
	header := "Content-Type: text/plain; charset=x-made-up\r\n" +
		`Content-Disposition: attachment; filename="report;v2.txt"; broken` + "\r\n"
	for _, skip := range []bool{true, false} {
		t.Run(fmt.Sprintf("skip=%v", skip), func(t *testing.T) {
			session, e, err := message.Parse(strings.NewReader(repairTree(header)), &message.ParseOptions{
				ReadOptions: message.ReadOptions{SkipTextAttachmentDecoding: skip},
			})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			parts, err := classifyAll(t, mail.NewReader(e))
			if err != nil || len(parts) != 3 {
				t.Fatalf("parts = %+v, %v; want three", parts, err)
			}
			got := parts[1]
			if !got.attachment || got.body != "caf\xe9" {
				t.Errorf("repaired part = %+v, want the attachment with its exact bytes", got)
			}
			if ran := message.IsUnknownCharset(got.err); ran == skip {
				t.Errorf("charset stage ran = %v with skip = %v: a repaired attachment's charset is skipped", ran, skip)
			}
			// A known gap, pinned so its change is visible: the standalone
			// Filename accessor reads the raw header with no repair, so it does
			// not see the repaired filename, and quietly falls back to the
			// Content-Type name. Filename policy is ROB-050-009's.
			if name, err := got.header.(*mail.AttachmentHeader).Filename(); name != "" || err != nil {
				t.Errorf("Filename() = (%q, %v), want the legacy unrepaired (\"\", nil)", name, err)
			}
			want := []message.Code{message.CodeMIMEDispositionInvalid}
			if !skip {
				want = []message.Code{message.CodeCharsetUnknown, message.CodeMIMEDispositionInvalid}
			}
			if got := codes(session.Diagnostics()); !reflect.DeepEqual(got, want) {
				t.Errorf("Diagnostics() = %v, want %v", got, want)
			}
			if !parts[2].attachment || parts[2].body != "two" {
				t.Errorf("following sibling = %+v", parts[2])
			}
		})
	}
}

// TestReader_strictRefusesOnePartAndContinues pins the strict profile's local
// failure: the refused part is reported without a part, the parse is not
// stopped, and the next call continues with the following sibling.
func TestReader_strictRefusesOnePartAndContinues(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		skip         bool
	}{
		{"content type", "Content-Type: multipart/mixed; boundary=B; broken\r\n", false},
		{"disposition read for the charset stage", "Content-Type: text/plain; charset=utf-8\r\nContent-Disposition: attachment; broken\r\n", true},
		{"disposition read while classified", "Content-Type: text/plain\r\nContent-Disposition: attachment; broken\r\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, e, err := message.Parse(strings.NewReader(repairTree(tc.header)), &message.ParseOptions{
				Profile:     message.ProfileStrict,
				ReadOptions: message.ReadOptions{SkipTextAttachmentDecoding: tc.skip},
			})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			mr := mail.NewReader(e)
			first, err := mr.NextPart()
			if err != nil || first == nil {
				t.Fatalf("first NextPart() = (%v, %v)", first, err)
			}
			b, _ := io.ReadAll(first.Body)
			p, refused := mr.NextPart()
			if p != nil || !message.IsFatal(refused) {
				t.Fatalf("refused part: NextPart() = (%v, %v), want (nil, an entity-scoped FatalError)", p, refused)
			}
			if e.ParseStop() != nil {
				t.Errorf("ParseStop() = %v, want nil: a strict refusal is not a stop", e.ParseStop())
			}
			after, err := mr.NextPart()
			if err != nil || after == nil {
				t.Fatalf("NextPart() after the refusal = (%v, %v), want the following sibling", after, err)
			}
			if rest, _ := io.ReadAll(after.Body); string(rest) != "two" || string(b) != "zero" {
				t.Errorf("siblings = %q, %q; want zero and two", b, rest)
			}
			if _, err := mr.NextPart(); err != io.EOF {
				t.Errorf("NextPart() at the end = %v, want io.EOF", err)
			}
		})
	}

	t.Run("root", func(t *testing.T) {
		_, e, err := message.Parse(strings.NewReader(issue22), &message.ParseOptions{Profile: message.ProfileStrict})
		if e == nil || !message.IsFatal(err) || e.ParseStop() != nil {
			t.Fatalf("Parse() = (%v, %v), want the entity with its own refusal", e, err)
		}
		mr := mail.NewReader(e)
		if p, got := mr.NextPart(); p != nil || got != err {
			t.Errorf("NextPart() = (%v, %v), want (nil, the identical refusal)", p, got)
		}
		if _, got := mr.NextPart(); got != io.EOF {
			t.Errorf("NextPart() after the refusal = %v, want io.EOF", got)
		}
	})
}

// TestReader_sinkStopOnARepairWins pins that a sink stopping on the repair
// diagnostic recorded while mail.Reader classifies a part is the stop, on every
// retry, under every profile.
func TestReader_sinkStopOnARepairWins(t *testing.T) {
	sentinel := errors.New("stop on repair")
	header := "Content-Type: text/plain\r\nContent-Disposition: attachment; broken\r\n"
	for _, profile := range []message.Profile{message.ProfileTolerant, message.ProfileForensic, message.ProfileStrict} {
		t.Run(profile.String(), func(t *testing.T) {
			session, e, err := message.Parse(strings.NewReader(repairTree(header)), &message.ParseOptions{
				Profile: profile,
				DiagnosticSink: message.DiagnosticSinkFunc(func(_ *message.ParseSession, d message.Diagnostic) error {
					if d.Code == message.CodeMIMEDispositionInvalid {
						return sentinel
					}
					return nil
				}),
			})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			mr := mail.NewReader(e)
			parts, stop := classifyAll(t, mr)
			if len(parts) != 1 || parts[0].body != "zero" {
				t.Errorf("parts before the stop = %+v", parts)
			}
			if !errors.Is(stop, sentinel) || stop != e.ParseStop() {
				t.Fatalf("iteration ended with %v, want the published sink stop", stop)
			}
			for i := range 2 {
				if p, err := mr.NextPart(); p != nil || err != stop {
					t.Errorf("retry %d = (%v, %v), want the identical stop", i, p, err)
				}
			}
			if got := codes(session.Diagnostics()); !reflect.DeepEqual(got, []message.Code{message.CodeMIMEDispositionInvalid}) {
				t.Errorf("Diagnostics() = %v, want the one repair diagnostic", got)
			}
		})
	}
}

// TestReader_unframedMultipartIsAnOpaquePart is the review regression for a
// repaired multipart that kept no usable boundary: mail.Reader returns its body
// as one raw, uninterpreted part and then reaches the sibling after it, with
// the declared media type kept and nothing chosen or invented.
func TestReader_unframedMultipartIsAnOpaquePart(t *testing.T) {
	inner := "--B\r\nContent-Type: text/plain\r\n\r\ninner\r\n--B--\r\n"
	for _, childType := range []string{
		"multipart/mixed; boundary=B; boundary=C",
		"multipart/related; type=\"text/html; boundary=B",
	} {
		for _, nested := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/nested=%v", childType, nested), func(t *testing.T) {
				child := "--O\r\nContent-Type: " + childType + "\r\n\r\n" + inner + "\r\n"
				text := "Content-Type: multipart/mixed; boundary=O\r\n\r\n--O\r\n\r\nbefore\r\n" + child + "--O\r\n\r\nafter\r\n--O--\r\n"
				if nested {
					text = "Content-Type: multipart/mixed; boundary=TOP\r\n\r\n--TOP\r\n" + text + "\r\n--TOP\r\n\r\nlast\r\n--TOP--\r\n"
				}
				session, parts, err := parseAll(t, text, nil)
				if err != nil {
					t.Fatalf("iteration ended with %v", err)
				}
				var bodies []string
				for _, p := range parts {
					bodies = append(bodies, p.body)
				}
				want := []string{"before", inner, "after"}
				if nested {
					want = append(want, "last")
				}
				if !reflect.DeepEqual(bodies, want) {
					t.Fatalf("bodies = %q, want %q", bodies, want)
				}
				if parts[1].inline || parts[1].err != nil {
					t.Errorf("opaque part = %+v, want an attachment with no error", parts[1])
				}
				diags := session.Diagnostics()
				if len(diags) == 0 {
					t.Fatal("no diagnostic for the uninterpreted body")
				}
				for _, d := range diags {
					if d.Severity != message.SeverityInvalidMetadata {
						t.Errorf("%v severity %v, want invalid_metadata: the parts inside were not read", d.Code, d.Severity)
					}
				}
			})
		}
	}
}

// TestReader_strayEmptyParameterItems is the review regression for fields
// whose only defect is an empty item between semicolons: the boundary is kept,
// the parts are read, and the legacy path keeps its long-standing failure.
func TestReader_strayEmptyParameterItems(t *testing.T) {
	for _, ct := range []string{
		"multipart/mixed;; boundary=B",
		"multipart/mixed; ; boundary=B",
		"multipart/mixed; boundary=B;;",
		"multipart/mixed; boundary=B; ;",
	} {
		body := "--B\r\nContent-Type: text/plain\r\n\r\nkept\r\n--B--\r\n"
		t.Run(ct, func(t *testing.T) {
			session, parts, err := parseAll(t, "Content-Type: "+ct+"\r\n\r\n"+body, nil)
			if err != nil || len(parts) != 1 || parts[0].body != "kept" {
				t.Errorf("root: parts = %+v, %v; want the kept part", parts, err)
			}
			if got := codes(session.Diagnostics()); !reflect.DeepEqual(got, []message.Code{message.CodeMIMEContentTypeInvalid}) {
				t.Errorf("root: Diagnostics() = %v", got)
			}
			// treeAround frames the parent with B, so the child uses I.
			inner := strings.ReplaceAll(body, "--B", "--I")
			_, parts, err = parseAll(t, treeAround("Content-Type: "+strings.Replace(ct, "boundary=B", "boundary=I", 1)+"\r\n", inner), nil)
			var bodies []string
			for _, p := range parts {
				bodies = append(bodies, p.body)
			}
			if err != nil || !reflect.DeepEqual(bodies, []string{"zero", "kept", "two"}) {
				t.Errorf("child: bodies = %q, %v", bodies, err)
			}
			mr, err := mail.CreateReader(strings.NewReader("Content-Type: " + ct + "\r\n\r\n" + body))
			if err != nil {
				t.Fatalf("CreateReader() = %v", err)
			}
			if p, err := mr.NextPart(); p != nil || err == nil {
				t.Errorf("legacy NextPart() = (%v, %v), want the empty-boundary error", p, err)
			}
		})
	}
}
