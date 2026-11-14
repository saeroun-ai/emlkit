package charset_test

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf16"

	"github.com/saeroun-ai/emlkit"
	_ "github.com/saeroun-ai/emlkit/charset"
)

func bomEncode16(s string, be bool) string {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		if be {
			b = append(b, byte(u>>8), byte(u))
		} else {
			b = append(b, byte(u), byte(u>>8))
		}
	}
	return string(b)
}

func bomEncode32(s string, be bool) string {
	var b []byte
	for _, r := range s {
		if be {
			b = append(b, byte(r>>24), byte(r>>16), byte(r>>8), byte(r))
		} else {
			b = append(b, byte(r), byte(r>>8), byte(r>>16), byte(r>>24))
		}
	}
	return string(b)
}

// TestParse_bomDeclarationConflicts pins ROB-080-004 with the plugin: a text
// body whose byte order mark contradicts its declared charset is reported
// once as charset.bom_mismatch, naming both. By default (#65) it is decoded as
// declared, at SeverityInfo; where the bytes after the mark are valid in its
// encoding the Action names that encoding. With
// ParseOptions.RecoverCharsetDeclarations, under the healing profiles, such a
// body is decoded as the mark says (SeverityRecovered). The rec column is
// that opt-in outcome. ProfileStrict and Read decode as declared, with the
// option or without, and a mark the declaration accepts is not reported.
func TestParse_bomDeclarationConflicts(t *testing.T) {
	const text = "Hi 안녕"
	var (
		u8   = "\xef\xbb\xbf" + text
		le16 = "\xff\xfe" + bomEncode16(text, false)
		be16 = "\xfe\xff" + bomEncode16(text, true)
		le32 = "\xff\xfe\x00\x00" + bomEncode32(text, false)
		be32 = "\x00\x00\xfe\xff" + bomEncode32(text, true)
		long = strings.Repeat("a", 1023) + "안녕" // the look ends inside 안
	)
	const (
		none = message.Severity(255) // no diagnostic
		info = message.SeverityInfo
		rec  = message.SeverityRecovered
	)
	for _, tc := range []struct {
		charset, body string
		mark          string           // the mark the diagnostic names
		sev           message.Severity // of the diagnostic, none for none
		want          string           // the healed body; "" is as declared
	}{
		// Explicit byte orders.
		{"utf-16le", be16, "UTF-16BE", rec, text},
		{"utf-16be", le16, "UTF-16LE", rec, text},
		{"utf-16le", le16, "", none, ""},
		{"UTF-16BE", be16, "", none, ""},
		{"utf-32le", be32, "UTF-32BE", rec, text},
		{"utf-32be", le32, "UTF-32LE", rec, text},
		{"utf-16le", le32, "UTF-32LE", rec, text},
		// The mark decides.
		{"utf-16", le16, "", none, ""},
		{"utf-16", be16, "", none, ""},
		{"utf-16", bomEncode16(text, false), "", none, ""},
		{"unicode", be16, "", none, ""},
		{"utf-32", le32, "", none, ""},
		{"utf-32", be32, "", none, ""},
		{"utf-32", bomEncode32(text, true), "", none, ""},
		{"utf-16", le32, "UTF-32LE", rec, text},
		{"utf-32", le16, "UTF-16LE", rec, text},
		// U+FEFF U+0000 is UTF-16LE, not a UTF-32LE mark.
		{"utf-16", "\xff\xfe\x00\x00H\x00i\x00", "", none, ""},
		// A UTF-8 mark under UTF-16 and UTF-32.
		{"utf-16", u8, "UTF-8", rec, u8},
		{"utf-16le", u8, "UTF-8", rec, u8},
		{"utf-32", u8, "UTF-8", rec, u8},
		// Legacy charsets.
		{"euc-kr", u8, "UTF-8", rec, u8},
		{"euc-kr", le16, "UTF-16LE", rec, text},
		{"iso-8859-1", u8, "UTF-8", rec, u8},
		{"iso-8859-1", le16, "UTF-16LE", rec, text},
		{"windows-1252", u8, "UTF-8", rec, u8},
		{"windows-1252", be16, "UTF-16BE", rec, text},
		{"shift_jis", u8, "UTF-8", rec, u8},
		{"shift_jis", le16, "UTF-16LE", rec, text},
		{"euc-kr", "\xef\xbb\xbf" + long, "UTF-8", rec, "\xef\xbb\xbf" + long},
		// No evidence: EUC-KR after a UTF-8 mark, a lone low surrogate.
		{"euc-kr", "\xef\xbb\xbf\xbe\xc8\xb3\xe7", "UTF-8", info, ""},
		{"windows-1252", "\xff\xfe\x00\xdcab", "UTF-16LE", info, ""},
		// UTF-8 and US-ASCII.
		{"utf-8", u8, "", none, ""},
		{"utf-8", le16, "UTF-16LE", rec, text},
		{"utf8", be16, "UTF-16BE", rec, text},
		{"utf-8", be32, "UTF-32BE", rec, text},
		{"us-ascii", u8, "UTF-8", info, ""},
		{"us-ascii", le16, "UTF-16LE", rec, text},
		// No charset: US-ASCII by default.
		{"", u8, "UTF-8", info, ""},
		{"", le16, "UTF-16LE", rec, text},
		{"", be16, "UTF-16BE", rec, text},
		{"", le32, "UTF-32LE", rec, text},
		{"", be32, "UTF-32BE", rec, text},
		{"", "\xff\xfe", "UTF-16LE", rec, ""},
		{"", "\xef\xbb", "", none, ""},
	} {
		name := tc.charset + "/" + strings.ToValidUTF8(tc.body[:min(len(tc.body), 4)], "?")
		ct := "text/plain"
		if tc.charset != "" {
			ct += "; charset=" + tc.charset
		}
		input := "Content-Type: " + ct + "\r\n\r\n" + tc.body

		// Read, as before: decoded as declared.
		e, err := message.Read(strings.NewReader(input))
		if err != nil {
			t.Fatalf("%s: Read() = %v", name, err)
		}
		b, _ := io.ReadAll(e.Body)
		declared := string(b)
		want := tc.want
		if tc.want == "" && tc.sev != rec {
			want = declared
		}

		// The encoding the mark names, without its byte order.
		family := strings.TrimSuffix(strings.TrimSuffix(tc.mark, "LE"), "BE")
		for _, p := range []message.Profile{message.ProfileTolerant, message.ProfileForensic, message.ProfileStrict} {
			for _, optIn := range []bool{false, true} {
				for _, one := range []bool{false, true} {
					var src io.Reader = strings.NewReader(input)
					if one {
						src = iotest.OneByteReader(src)
					}
					s, e, err := message.Parse(src, &message.ParseOptions{Profile: p, RecoverCharsetDeclarations: optIn})
					if err != nil {
						t.Fatalf("%s/%v: Parse() = %v", name, p, err)
					}
					b, rerr := io.ReadAll(e.Body)
					wantBody, wantSev := want, tc.sev
					// Two readings, and the declaration is followed.
					twoReadings := tc.sev == rec && (p == message.ProfileStrict || !optIn)
					if twoReadings {
						wantBody, wantSev = declared, info
					}
					if string(b) != wantBody || rerr != nil {
						t.Errorf("%s/%v/optIn=%v/one=%v: body (%q, %v), want %q", name, p, optIn, one, b, rerr, wantBody)
					}
					var got []message.Diagnostic
					for _, d := range s.Diagnostics() {
						if d.Code == message.CodeCharsetBOMMismatch {
							got = append(got, d)
						}
					}
					switch {
					case wantSev == none && len(got) != 0:
						t.Errorf("%s/%v/optIn=%v/one=%v: %v, want no charset.bom_mismatch", name, p, optIn, one, got)
					case wantSev == none:
					case len(got) != 1 || got[0].Severity != wantSev:
						t.Errorf("%s/%v/optIn=%v/one=%v: %v, want one charset.bom_mismatch at %v", name, p, optIn, one, got, wantSev)
					case !strings.Contains(got[0].Message, tc.mark) || !strings.Contains(got[0].Message, `"`+tc.charset+`"`) && tc.charset != "" || got[0].Action == "":
						t.Errorf("%s/%v/optIn=%v/one=%v: %q / %q does not name %q and the mark %s", name, p, optIn, one, got[0].Message, got[0].Action, tc.charset, tc.mark)
					case twoReadings && p != message.ProfileStrict &&
						(!strings.Contains(got[0].Action, "also valid "+family) || !strings.Contains(got[0].Action, "RecoverCharsetDeclarations")):
						t.Errorf("%s/%v/one=%v: Action %q does not name %s as the other reading", name, p, one, got[0].Action, family)
					}
				}
			}
		}
	}

	// Every mark under every kind of label, followed by nothing, by a cut
	// unit, by a lone surrogate, and by a run that ends past the look: the
	// same body and diagnostics whole and a byte at a time, by default and
	// with the recovery on.
	for _, optIn := range []bool{false, true} {
		bomChunkMatrix(t, &message.ParseOptions{RecoverCharsetDeclarations: optIn})
	}
}

// bomChunkMatrix parses every mark under every kind of label whole and a byte
// at a time under opts and compares the outcomes.
func bomChunkMatrix(t *testing.T, opts *message.ParseOptions) {
	t.Helper()
	outcome := func(r io.Reader) string {
		s, e, err := message.Parse(r, opts)
		if err != nil {
			return err.Error()
		}
		b, rerr := io.ReadAll(e.Body)
		out := string(b)
		if rerr != nil {
			out += " / " + rerr.Error()
		}
		for _, d := range s.Diagnostics() {
			out += " / " + string(d.Code) + " " + d.Severity.String()
		}
		return out
	}
	for _, label := range []string{"", "utf-8", "us-ascii", "utf-16", "utf-16le", "utf-16be", "utf-32", "utf-32le", "euc-kr", "x-unknown"} {
		for _, mark := range []string{"\xef\xbb\xbf", "\xfe\xff", "\xff\xfe", "\x00\x00\xfe\xff", "\xff\xfe\x00\x00"} {
			for _, tail := range []string{"", "A", "\x00\xdc\x00\xdc", "\xd8\x00", strings.Repeat("\x00A", 600)} {
				ct := "text/plain"
				if label != "" {
					ct += "; charset=" + label
				}
				input := "Content-Type: " + ct + "\r\n\r\n" + mark + tail
				whole := outcome(strings.NewReader(input))
				if bytewise := outcome(iotest.OneByteReader(strings.NewReader(input))); whole != bytewise {
					t.Errorf("%s %q+%q optIn=%v: whole %q, a byte at a time %q", label, mark, tail[:min(len(tail), 4)], opts.RecoverCharsetDeclarations, whole, bytewise)
				}
			}
		}
	}
}
