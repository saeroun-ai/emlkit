package mail_test

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// ROB-001-008 (issue #8) through mail.Reader: reaching MaxDiagnosticCount
// closes the session's diagnostics and nothing else. Every part is still
// classified and returned, and a Strict refusal after the cap is still a
// refusal.

func TestReader_diagnosticStormKeepsEveryPart(t *testing.T) {
	const total = 1005 // past the default budget of 1000
	var raw strings.Builder
	raw.WriteString("Content-Type: multipart/mixed; boundary=B\r\n\r\n")
	for i := range total {
		fmt.Fprintf(&raw, "--B\r\nContent-Type: text/plain; broken\r\n\r\npart%d\r\n", i)
	}
	raw.WriteString("--B--\r\n")

	for _, deliverOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("deliverOnly=%v", deliverOnly), func(t *testing.T) {
			delivered := 0
			var last message.Diagnostic
			opts := &message.ParseOptions{}
			if deliverOnly {
				opts.DiscardDeliveredDiagnostics = true
				opts.DiagnosticSink = message.DiagnosticSinkFunc(func(_ *message.ParseSession, d message.Diagnostic) error {
					delivered++
					last = d
					return nil
				})
			}
			session, parts, err := parseAll(t, raw.String(), opts)
			if err != nil {
				t.Fatalf("iteration ended with %v", err)
			}
			if len(parts) != total {
				t.Fatalf("%d parts, want %d", len(parts), total)
			}
			for i, p := range parts {
				if p.body != fmt.Sprintf("part%d", i) || !p.inline || p.err != nil {
					t.Fatalf("part %d = %+v", i, p)
				}
			}
			limit := message.DefaultLimits().MaxDiagnosticCount
			if deliverOnly {
				if session.Len() != 0 || int64(delivered) != limit || last.Code != message.CodeLimitDiagnosticCount {
					t.Errorf("Len() = %d, delivered = %d, last = %v", session.Len(), delivered, last)
				}
				return
			}
			ds := session.Diagnostics()
			if int64(len(ds)) != limit || ds[len(ds)-1].Code != message.CodeLimitDiagnosticCount {
				t.Errorf("%d retained, last %v; want %d ending with the terminal", len(ds), ds[len(ds)-1], limit)
			}
		})
	}
}

// TestReader_strictRefusalsAfterDiagnosticCap pins that a Strict refusal whose
// diagnostic is the terminal, or is omitted after it, still refuses its part,
// and that the following siblings are still returned.
func TestReader_strictRefusalsAfterDiagnosticCap(t *testing.T) {
	const bad = "--B\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; broken\r\n\r\nbad\r\n"
	text := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nzero\r\n" + bad +
		"--B\r\nContent-Type: text/plain\r\n\r\ntwo\r\n" + bad +
		"--B\r\nContent-Type: text/plain\r\n\r\nfour\r\n--B--\r\n"

	session, e, err := message.Parse(strings.NewReader(text), &message.ParseOptions{
		Profile: message.ProfileStrict,
		Limits:  message.Limits{MaxDiagnosticCount: 1},
	})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	mr := mail.NewReader(e)
	var kept []string
	refusals := 0
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if !message.IsFatal(err) || e.ParseStop() != nil {
				t.Fatalf("NextPart() = %v, ParseStop() = %v; want an entity-scoped refusal", err, e.ParseStop())
			}
			// The first refusal's diagnostic became the terminal and the second's
			// was omitted, yet each refusal still carries its own.
			var f message.FatalError
			if !errors.As(err, &f) {
				t.Fatalf("NextPart() = %v, want a FatalError", err)
			}
			if d, ok := f.Diagnostic(); !ok || d.Code == message.CodeLimitDiagnosticCount || d.Path[0] != 1+2*refusals {
				t.Errorf("refusal %d carries %v, want its own diagnostic for part %d", refusals, d, 1+2*refusals)
			}
			refusals++
			continue
		}
		b, _ := io.ReadAll(p.Body)
		kept = append(kept, string(b))
	}
	if refusals != 2 || strings.Join(kept, ",") != "zero,two,four" {
		t.Errorf("refusals = %d, kept = %q; want 2 and zero,two,four", refusals, kept)
	}
	if got := codes(session.Diagnostics()); len(got) != 1 || got[0] != message.CodeLimitDiagnosticCount {
		t.Errorf("Diagnostics() = %v, want the terminal alone", got)
	}
}
