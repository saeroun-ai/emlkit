package message

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

// TestParse_bomConflictWithoutDecoder pins ROB-080-004 without the charset
// plugin: a mark the declaration contradicts is still reported, at
// SeverityInfo. By default (#65) the body is decoded as declared, and the
// Action names the encoding the bytes after the mark are also valid in; with
// ParseOptions.RecoverCharsetDeclarations and no CharsetReader for the mark's
// encoding the body is kept as it was read too. Read reports nothing and
// delivers the same bytes.
func TestParse_bomConflictWithoutDecoder(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body string
		codes                   []Code
		action, optInAction     string // in the bom_mismatch Action, by default and with the option
	}{
		{"utf-16 mark, utf-8", "text/plain; charset=utf-8", "\xff\xfeh\x00i\x00", []Code{CodeCharsetBOMMismatch, CodeCharsetInvalidSequence}, "also valid UTF-16", "no CharsetReader decodes utf-16"},
		// Kept as read, like the body declared utf-8 above, and watched the same
		// way: the bytes are neither US-ASCII nor UTF-8 (charset.missing, info).
		// An entity with no Content-Type at all is not watched.
		{"utf-32 mark, no charset", "text/plain", "\x00\x00\xfe\xff\x00\x00\x00h", []Code{CodeCharsetBOMMismatch, CodeCharsetMissing}, "also valid UTF-32", "no CharsetReader decodes utf-32"},
		{"utf-16 mark, no content type", "", "\xfe\xff\x00h\x00i", []Code{CodeCharsetBOMMismatch}, "also valid UTF-16", "no CharsetReader"},
		{"utf-8 mark, us-ascii", "text/plain; charset=us-ascii", "\xef\xbb\xbfhi", []Code{CodeCharsetBOMMismatch}, "that is UTF-8", "that is UTF-8"},
		{"utf-8 mark, utf-8", "text/plain; charset=utf-8", "\xef\xbb\xbfhi", nil, "", ""},
		{"no mark", "text/plain", "hello", nil, "", ""},
		{"not text", "application/octet-stream", "\xff\xfeh\x00", nil, "", ""},
	} {
		header := ""
		if tc.contentType != "" {
			header = "Content-Type: " + tc.contentType + "\r\n"
		}
		input := header + "\r\n" + tc.body
		for name, wrap := range map[string]func(io.Reader) io.Reader{
			"whole":    func(r io.Reader) io.Reader { return r },
			"one byte": iotest.OneByteReader,
		} {
			for _, optIn := range []bool{false, true} {
				action := tc.action
				if optIn {
					action = tc.optInAction
				}
				s, e, err := Parse(wrap(strings.NewReader(input)), &ParseOptions{RecoverCharsetDeclarations: optIn})
				if err != nil {
					t.Fatal(err)
				}
				b, rerr := io.ReadAll(e.Body)
				if string(b) != tc.body || rerr != nil {
					t.Errorf("%s/%s/optIn=%v: body (%q, %v), want every byte as read", tc.name, name, optIn, b, rerr)
				}
				diags := s.Diagnostics()
				if codes := codesOfDiags(diags); !reflect.DeepEqual(codes, tc.codes) {
					t.Errorf("%s/%s/optIn=%v: diagnostics %v, want %v", tc.name, name, optIn, codes, tc.codes)
				} else if len(diags) > 0 && (diags[0].Severity != SeverityInfo || !strings.Contains(diags[0].Action, action)) {
					t.Errorf("%s/%s/optIn=%v: %v %q, want info and %q", tc.name, name, optIn, diags[0].Severity, diags[0].Action, action)
				}
			}

			e, err := Read(wrap(strings.NewReader(input)))
			if err != nil {
				t.Fatal(err)
			}
			if b, _ := io.ReadAll(e.Body); string(b) != tc.body {
				t.Errorf("%s/%s: Read body %q, want every byte as read", tc.name, name, b)
			}
		}
	}
}

// bomStall serves its text and then reads that return nothing, for ever,
// failing the test's own bound after a million of them.
type bomStall struct {
	r     io.Reader
	empty int
}

func (s *bomStall) Read(p []byte) (int, error) {
	if n, err := s.r.Read(p); n > 0 || err == nil {
		return n, err
	}
	if s.empty++; s.empty > 1e6 {
		return 0, errors.New("test: unbounded")
	}
	return 0, nil
}

// TestParse_bomLookStalled: the look at a body's first bytes passes an empty
// read on, so a source that stalls inside a mark is still ended by the parse.
func TestParse_bomLookStalled(t *testing.T) {
	for _, body := range []string{"\xff", "\xff\xfe" + "h\x00"} {
		_, e, err := Parse(&bomStall{r: strings.NewReader("Content-Type: text/plain\r\n\r\n" + body)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, rerr := io.ReadAll(e.Body)
		if rerr == nil || rerr.Error() == "test: unbounded" {
			t.Errorf("%q: ReadAll() = %v, want the parse's no-progress failure", body, rerr)
		}
	}
}
