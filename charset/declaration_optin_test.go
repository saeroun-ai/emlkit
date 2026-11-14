package charset_test

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/charset"
)

// optInCases are bodies whose bytes read two ways: as their declared charset
// and as what their mark or their UTF-8 says (#65). healed is what
// ParseOptions.RecoverCharsetDeclarations delivers.
var optInCases = []struct {
	name, label, body, healed string
	code                      message.Code
}{
	// The independent reproduction of #65: valid Shift_JIS, ﾂ｡ﾂ｡.
	{"shift_jis C2 A1 C2 A1", "shift_jis", "\xc2\xa1\xc2\xa1", "¡¡", message.CodeCharsetDeclarationSuspect},
	{"utf-8 under euc-kr", "euc-kr", "<p>[광고] 연말 특가</p>", "<p>[광고] 연말 특가</p>", message.CodeCharsetDeclarationSuspect},
	{"utf-16le mark under euc-kr", "euc-kr", "\xff\xfe" + bomEncode16("Hi 안녕", false), "Hi 안녕", message.CodeCharsetBOMMismatch},
	{"utf-8 mark under windows-1252", "windows-1252", "\xef\xbb\xbfHi 안녕", "\xef\xbb\xbfHi 안녕", message.CodeCharsetBOMMismatch},
	{"utf-16be mark, no charset", "", "\xfe\xff" + bomEncode16("Hi", true), "Hi", message.CodeCharsetBOMMismatch},
}

func optInInput(label, body string) string {
	ct := "text/plain"
	if label != "" {
		ct += "; charset=" + label
	}
	return "Subject: s\r\nContent-Type: " + ct + "\r\n\r\n" + body
}

// optInDeclared is the body as its declared charset reads it, or as read for
// no charset.
func optInDeclared(t *testing.T, label, body string) string {
	t.Helper()
	if label == "" {
		return body
	}
	r, err := charset.Reader(label, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	return string(b)
}

// TestParse_charsetRecoveryOptIn: by default every case is decoded as
// declared with one diagnostic of its code at SeverityInfo, naming the other
// reading; with ParseOptions.RecoverCharsetDeclarations it is decoded as the
// bytes say, at SeverityRecovered. ProfileStrict ignores the option, and Read
// has none. Either way the raw source — the bytes ParseAt serves for the body
// and the whole message — is the input, unchanged.
func TestParse_charsetRecoveryOptIn(t *testing.T) {
	for _, tc := range optInCases {
		input := optInInput(tc.label, tc.body)
		declared := optInDeclared(t, tc.label, tc.body)
		other := "UTF-8"
		if tc.code == message.CodeCharsetBOMMismatch && !strings.HasPrefix(tc.body, "\xef\xbb\xbf") {
			other = "UTF-16"
		}
		for _, p := range []message.Profile{message.ProfileTolerant, message.ProfileForensic, message.ProfileStrict} {
			for _, optIn := range []bool{false, true} {
				opts := &message.ParseOptions{Profile: p, RecoverCharsetDeclarations: optIn}
				healed := optIn && p != message.ProfileStrict
				want, wantSev := declared, message.SeverityInfo
				if healed {
					want, wantSev = tc.healed, message.SeverityRecovered
				}
				for _, mode := range []string{"reader", "one byte", "source"} {
					var (
						s   *message.ParseSession
						e   *message.Entity
						err error
					)
					switch mode {
					case "reader":
						s, e, err = message.Parse(strings.NewReader(input), opts)
					case "one byte":
						s, e, err = message.Parse(iotest.OneByteReader(strings.NewReader(input)), opts)
					case "source":
						s, e, err = message.ParseAt(strings.NewReader(input), int64(len(input)), opts)
					}
					if err != nil {
						t.Fatalf("%s/%v/%v/%s: Parse() = %v", tc.name, p, optIn, mode, err)
					}
					b, rerr := io.ReadAll(e.Body)
					if string(b) != want || rerr != nil {
						t.Errorf("%s/%v/optIn=%v/%s: body (%q, %v), want %q", tc.name, p, optIn, mode, b, rerr, want)
					}
					var got []message.Diagnostic
					for _, d := range s.Diagnostics() {
						if d.Code == tc.code {
							got = append(got, d)
						}
					}
					// ProfileStrict looks for marks, not for UTF-8.
					if p == message.ProfileStrict && tc.code == message.CodeCharsetDeclarationSuspect {
						if len(got) != 0 {
							t.Errorf("%s/strict/optIn=%v/%s: %v, want nothing", tc.name, optIn, mode, got)
						}
					} else if len(got) != 1 || got[0].Severity != wantSev || got[0].Field != "Content-Type" {
						t.Errorf("%s/%v/optIn=%v/%s: %v, want one %s at %v", tc.name, p, optIn, mode, got, tc.code, wantSev)
					} else if !healed && p != message.ProfileStrict &&
						(!strings.Contains(got[0].Action, other) || !strings.Contains(got[0].Action, "RecoverCharsetDeclarations")) {
						t.Errorf("%s/%v/%s: Action %q does not name %s as the other reading", tc.name, p, mode, got[0].Action, other)
					}
					if mode != "source" {
						continue
					}
					raw, rerr := e.RawBody()
					if rerr != nil {
						t.Fatalf("%s: RawBody() = %v", tc.name, rerr)
					}
					if rb, _ := io.ReadAll(raw); string(rb) != tc.body {
						t.Errorf("%s/%v/optIn=%v: raw body %q, want the input's %q", tc.name, p, optIn, rb, tc.body)
					}
					whole, rerr := e.RawReader()
					if rerr != nil {
						t.Fatalf("%s: RawReader() = %v", tc.name, rerr)
					}
					if wb, _ := io.ReadAll(whole); string(wb) != input {
						t.Errorf("%s/%v/optIn=%v: raw message differs from the input", tc.name, p, optIn)
					}
				}
			}
		}
		e, err := message.Read(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := io.ReadAll(e.Body); string(b) != declared {
			t.Errorf("%s/Read: body %q, want %q", tc.name, b, declared)
		}
	}
}

// optInFailing serves its text and then fails with errOptInSource.
type optInFailing struct{ r io.Reader }

var errOptInSource = errors.New("test: source failed")

func (f *optInFailing) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err == io.EOF {
		err = errOptInSource
	}
	return n, err
}

// TestParse_charsetRecoveryOptInEnds: a source that fails after the body, and
// a DiagnosticSink that stops the parse at the charset diagnostic, by default
// and with the option. The source's failure is returned after what the chosen
// decoder made of the bytes before it — the reading of the mode, less at most
// a character the decoder had not finished — and the diagnostic is still
// made. A sink's stop ends the parse (CompletionStopped); the body already
// returned reads on, as any body does after a stop, in the reading of its
// mode, decided before the diagnostic was handed to the sink.
func TestParse_charsetRecoveryOptInEnds(t *testing.T) {
	for _, tc := range optInCases {
		input := optInInput(tc.label, tc.body)
		declared := optInDeclared(t, tc.label, tc.body)
		for _, optIn := range []bool{false, true} {
			want := declared
			if optIn {
				want = tc.healed
			}
			name := fmt.Sprintf("%s/optIn=%v", tc.name, optIn)

			for _, one := range []bool{false, true} {
				var src io.Reader = &optInFailing{r: strings.NewReader(input)}
				if one {
					src = iotest.OneByteReader(src)
				}
				s, e, err := message.Parse(src, &message.ParseOptions{RecoverCharsetDeclarations: optIn})
				if err != nil {
					t.Fatalf("%s: Parse() = %v", name, err)
				}
				b, rerr := io.ReadAll(e.Body)
				if !strings.HasPrefix(want, string(b)) || len(want)-len(b) > 3 || !errors.Is(rerr, errOptInSource) {
					t.Errorf("%s/one=%v failing source: (%q, %v), want %q and the source's error", name, one, b, rerr, want)
				}
				found := false
				for _, d := range s.Diagnostics() {
					found = found || d.Code == tc.code
				}
				if !found {
					t.Errorf("%s/one=%v failing source: %v, want %s", name, one, s.Diagnostics(), tc.code)
				}
			}

			stop := errors.New("test: stop")
			sink := message.DiagnosticSinkFunc(func(_ *message.ParseSession, d message.Diagnostic) error {
				if d.Code == tc.code {
					return stop
				}
				return nil
			})
			s, e, err := message.Parse(strings.NewReader(input), &message.ParseOptions{RecoverCharsetDeclarations: optIn, DiagnosticSink: sink})
			if err != nil {
				t.Fatalf("%s: Parse() = %v", name, err)
			}
			b, rerr := io.ReadAll(io.LimitReader(e.Body, 1<<20))
			if string(b) != want || rerr != nil {
				t.Errorf("%s sink stop: body (%q, %v), want %q", name, b, rerr, want)
			}
			if c := s.Completion(); c.State != message.CompletionStopped || !errors.Is(c.Stop, stop) {
				t.Errorf("%s sink stop: completion %v (%v), want stopped by the sink", name, c.State, c.Stop)
			}
		}
	}
}
