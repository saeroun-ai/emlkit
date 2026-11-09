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

// ROB-010-002 through mail.Reader: a multipart delimiter line with a bare CR
// line ending is refused under ProfileStrict for that multipart's iteration,
// not for one part. Every source is a finite string, and every iteration is
// a fixed number of calls.

// mailCalls makes n NextPart calls and returns, for each, the part's body or
// "error", and the error.
func mailCalls(t *testing.T, mr *mail.Reader, n int) ([]string, []error) {
	t.Helper()
	var out []string
	var errs []error
	for range n {
		p, err := mr.NextPart()
		errs = append(errs, err)
		if err != nil {
			if p != nil {
				t.Errorf("NextPart returned a part with %v", err)
			}
			out = append(out, "error")
			continue
		}
		b, berr := io.ReadAll(io.LimitReader(p.Body, 1<<16))
		if berr != nil {
			t.Fatalf("body: %v", berr)
		}
		out = append(out, string(b))
	}
	return out, errs
}

// checkBareCRRefusal reports whether err is the Strict refusal of a delimiter.
func checkBareCRRefusal(t *testing.T, label string, err error) {
	t.Helper()
	var fe message.FatalError
	if !errors.As(err, &fe) {
		t.Fatalf("%s: %v, want the iterator's FatalError", label, err)
	}
	if d, ok := fe.Diagnostic(); !ok || d.Code != message.CodeMultipartBareCR || d.Severity != message.SeverityFatal {
		t.Errorf("%s: refusal carries %+v, want a SeverityFatal multipart.bare_cr", label, d)
	}
}

// TestReader_strictDelimiterRefusalIsSticky pins the refusal: no Part, and
// the same error on every later call — after a malformed part header the
// call that met the delimiter reported first, and inside a nested multipart,
// whose enclosing multipart's later part is not reached through the Reader.
// Tolerant goes on in both.
func TestReader_strictDelimiterRefusalIsSticky(t *testing.T) {
	const head = "Content-Type: multipart/mixed; boundary=B\r\n\r\n"
	flat := head + "--B\n\r\none\n--B\rmalformed\r\n--B\r\n\r\ntwo\r\n--B--\r\n"
	nested := head +
		"--B\r\nContent-Type: multipart/alternative; boundary=C\r\n\r\n" +
		"--C\r\n\r\nin one\r--C\r\n\r\nin two\r\n--C--\r\n" +
		"--B\r\n\r\nlast\r\n--B--\r\n"

	for _, tc := range []struct {
		name      string
		text      string
		tolerant  []string
		strict    []string
		malformed int // the call that meets a malformed header, or -1
		refusal   int // the first call Strict refuses
	}{
		{name: "flat, after a malformed header", text: flat,
			tolerant:  []string{"one", "error", "two", "error"},
			strict:    []string{"one", "error", "error", "error"},
			malformed: 1, refusal: 2},
		{name: "nested", text: nested,
			tolerant:  []string{"in one", "in two", "last", "error"},
			strict:    []string{"in one", "error", "error", "error"},
			malformed: -1, refusal: 1},
	} {
		for _, profile := range []message.Profile{message.ProfileTolerant, message.ProfileStrict} {
			label := fmt.Sprintf("%s/%v", tc.name, profile)
			_, e, err := message.Parse(strings.NewReader(tc.text), &message.ParseOptions{Profile: profile})
			if err != nil {
				t.Fatalf("%s: Parse() = %v", label, err)
			}
			mr := mail.NewReader(e)
			got, errs := mailCalls(t, mr, 4)
			mr.Close()
			want := tc.tolerant
			if profile == message.ProfileStrict {
				want = tc.strict
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s: calls %q, want %q", label, got, want)
			}
			if tc.malformed >= 0 {
				if err := errs[tc.malformed]; err == nil || message.IsFatal(err) || !strings.Contains(err.Error(), "malformed MIME header line") {
					t.Errorf("%s: call %d = %v, want the malformed header, as itself", label, tc.malformed, err)
				}
			}
			if e.ParseStop() != nil {
				t.Errorf("%s: ParseStop() = %v, want none", label, e.ParseStop())
			}
			if profile != message.ProfileStrict {
				if errs[3] != io.EOF {
					t.Errorf("%s: iteration ended %v, want io.EOF", label, errs[3])
				}
				continue
			}
			refusal := errs[tc.refusal]
			checkBareCRRefusal(t, label, refusal)
			for i := tc.refusal + 1; i < len(errs); i++ {
				if errs[i] != refusal {
					t.Errorf("%s: call %d = %v, want the identical refusal", label, i, errs[i])
				}
			}
		}
	}
}
