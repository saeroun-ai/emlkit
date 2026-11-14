package message

import (
	"bytes"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit/textproto"
)

// unfoldMessage is the folded Subject of the differential fixture
// dx-subject-folded: a fold of a tab, and two encoded words a fold separates.
const unfoldMessage = "From: sender@example.com\r\n" +
	"Subject: a long subject that is\r\n" +
	"\tfolded =?utf-8?q?with_an_encoded?=\r\n" +
	" =?utf-8?q?_word?= split\r\n" +
	"X-Ws: s \r\n \t\r\n" +
	"\r\n" +
	"body\r\n"

// TestUnfold_encodedWords pins that RFC 2047 still drops the white space
// between adjacent encoded words when unfolding keeps a fold's tabs and
// spaces, and keeps the white space of text between them.
func TestUnfold_encodedWords(t *testing.T) {
	for _, tc := range []struct{ field, raw, text string }{
		{"=?utf-8?q?a?=\r\n\t =?utf-8?q?b?=", "=?utf-8?q?a?=\t =?utf-8?q?b?=", "ab"},
		{"=?utf-8?q?a?=\r\n \r\n\t=?utf-8?q?b?=", "=?utf-8?q?a?= \t=?utf-8?q?b?=", "ab"},
		{"=?utf-8?q?a?=\r\n\tx\t=?utf-8?q?b?=", "=?utf-8?q?a?=\tx\t=?utf-8?q?b?=", "a\tx\tb"},
		{"x\r\n\t=?utf-8?q?a?=", "x\t=?utf-8?q?a?=", "x\ta"},
	} {
		for _, end := range []string{"\r\n", "\n"} {
			in := strings.ReplaceAll("Subject: "+tc.field+"\r\n\r\nbody", "\r\n", end)
			_, e, err := Parse(strings.NewReader(in), nil)
			if err != nil {
				t.Fatalf("%q: Parse() = %v", in, err)
			}
			if v := e.Header.Get("Subject"); v != tc.raw {
				t.Errorf("%q: Get() = %q, want %q", in, v, tc.raw)
			}
			if v, err := e.Header.Text("Subject"); err != nil || v != tc.text {
				t.Errorf("%q: Text() = %q, %v, want %q", in, v, err, tc.text)
			}
		}
	}
}

// TestUnfold_parseProfiles pins how each way in unfolds a header (#63): Parse
// with any profile but ProfileStrict as RFC 5322 section 3.2.2 says, removing
// the line endings alone; ProfileStrict, Read and ReadWithOptions as they
// always did, replacing each fold and the white space around it with one
// space. Adjacent encoded words a fold separates are still joined, and the
// header is written back byte for byte either way.
func TestUnfold_parseProfiles(t *testing.T) {
	const (
		rfcRaw     = "a long subject that is\tfolded =?utf-8?q?with_an_encoded?= =?utf-8?q?_word?= split"
		rfcText    = "a long subject that is\tfolded with an encoded word split"
		legacyRaw  = "a long subject that is folded =?utf-8?q?with_an_encoded?= =?utf-8?q?_word?= split"
		legacyText = "a long subject that is folded with an encoded word split"
	)
	type got struct {
		name string
		h    Header
	}
	var all []got
	for _, p := range []Profile{ProfileTolerant, ProfileForensic, ProfileStrict} {
		_, e, err := Parse(strings.NewReader(unfoldMessage), &ParseOptions{Profile: p})
		if err != nil {
			t.Fatalf("Parse(%v) = %v", p, err)
		}
		all = append(all, got{"Parse " + p.String(), e.Header})
	}
	e, err := Read(strings.NewReader(unfoldMessage))
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, got{"Read", e.Header})
	e, err = ReadWithOptions(strings.NewReader(unfoldMessage), &ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, got{"ReadWithOptions", e.Header})
	e, err = New(e.Header, strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, got{"New", e.Header})

	for _, g := range all {
		wantRaw, wantText, wantWs := legacyRaw, legacyText, "s"
		if strings.HasPrefix(g.name, "Parse") && g.name != "Parse "+ProfileStrict.String() {
			wantRaw, wantText, wantWs = rfcRaw, rfcText, "s  \t"
		}
		if v := g.h.Get("Subject"); v != wantRaw {
			t.Errorf("%s: Get() = %q, want %q", g.name, v, wantRaw)
		}
		if v, err := g.h.Text("Subject"); err != nil || v != wantText {
			t.Errorf("%s: Text() = %q, %v, want %q", g.name, v, err, wantText)
		}
		if v := g.h.Get("X-Ws"); v != wantWs {
			t.Errorf("%s: Get(X-Ws) = %q, want %q", g.name, v, wantWs)
		}
		var b bytes.Buffer
		if err := textproto.WriteHeader(&b, g.h.Header); err != nil || b.String() != unfoldMessage[:strings.Index(unfoldMessage, "\r\n\r\n")+4] {
			t.Errorf("%s: WriteHeader() = %q, %v", g.name, b.String(), err)
		}
	}
}
