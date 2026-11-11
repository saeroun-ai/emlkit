package mail_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// obTree parses raw with opts and reads its tree, whole or one byte at a time.
func obTree(t *testing.T, raw string, opts *message.ParseOptions, bytewise bool) (*message.ParseSession, *mail.Tree) {
	t.Helper()
	var r io.Reader = strings.NewReader(raw)
	if bytewise {
		r = iotest.OneByteReader(r)
	}
	s, e, err := message.Parse(r, opts)
	if e == nil {
		t.Fatalf("Parse() = %v", err)
	}
	tree, err := mail.ReadTree(e, nil)
	if err != nil {
		t.Fatalf("ReadTree() = %v", err)
	}
	return s, tree
}

// TestReadTree_epilogue pins the preamble and epilogue of a multipart node and
// the multipart.trailing_junk note of an epilogue that holds more than white
// space (corpus findings list-footer-after-close, nested-reused-boundary-deep).
func TestReadTree_epilogue(t *testing.T) {
	const head = "Content-Type: multipart/mixed; boundary=B\r\n\r\nThis is a multi-part message in MIME format.\r\n--B\r\nContent-Type: text/plain\r\n\r\nhello\r\n"
	tests := []struct {
		name, raw string
		path      []int  // where the note is, when there is one
		kind      string // what the note says the bytes read as
		epilogue  string
	}{
		{name: "list footer", raw: head + "--B--\r\n____\r\nTo unsubscribe write to leave@example.org\r\n",
			path: []int{}, kind: "they read as text", epilogue: "____\r\nTo unsubscribe write to leave@example.org\r\n"},
		{name: "appended message", raw: head + "--B--\r\nFrom: other@example.org\r\nSubject: second\r\n\r\nbody\r\n",
			path: []int{}, kind: "a header block", epilogue: "From: other@example.org\r\nSubject: second\r\n\r\nbody\r\n"},
		{name: "parts after the close", raw: head + "--B--\r\n\r\n--B\r\nContent-Type: application/pdf\r\n\r\nlost\r\n--B--\r\n",
			path: []int{}, kind: "further delimiter lines", epilogue: "\r\n--B\r\nContent-Type: application/pdf\r\n\r\nlost\r\n--B--\r\n"},
		{name: "binary", raw: head + "--B--\r\n\x00\x01\x02\xff", path: []int{}, kind: "binary data", epilogue: "\x00\x01\x02\xff"},
		{name: "white space only", raw: head + "--B--\r\n\r\n \t\r\n", epilogue: "\r\n \t\r\n"},
		{name: "empty", raw: head + "--B--\r\n"},
		{name: "unclosed", raw: head},
		{name: "nested", raw: "Content-Type: multipart/mixed; boundary=O\r\n\r\n--O\r\n" +
			"Content-Type: multipart/alternative; boundary=I\r\n\r\n--I\r\nContent-Type: text/plain\r\n\r\nhi\r\n--I--\r\ninner footer\r\n--O--\r\n",
			path: []int{0}, kind: "text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, tree := obTree(t, tt.raw, nil, false)
			_, tree2 := obTree(t, tt.raw, nil, true)
			var notes, notes2 []message.Diagnostic
			for _, d := range tree.Diagnostics {
				if d.Code == message.CodeMultipartTrailingJunk {
					notes = append(notes, d)
				}
			}
			for _, d := range tree2.Diagnostics {
				if d.Code == message.CodeMultipartTrailingJunk {
					notes2 = append(notes2, d)
				}
			}
			if len(notes) != len(notes2) || (len(notes) == 1 && notes[0].Message != notes2[0].Message) {
				t.Errorf("read whole %v, a byte at a time %v", notes, notes2)
			}
			if tt.path == nil {
				if len(notes) != 0 {
					t.Errorf("notes %v, want none", notes)
				}
			} else if len(notes) != 1 || notes[0].Severity != message.SeverityInfo || len(notes[0].Path) != len(tt.path) ||
				!strings.Contains(notes[0].Message, tt.kind) {
				t.Errorf("notes %v, want one at %v reading as %q", notes, tt.path, tt.kind)
			}
			if tt.name == "nested" {
				if got := string(tree.Root.Children[0].Epilogue); got != "inner footer" {
					t.Errorf("inner epilogue %q", got)
				}
				return
			}
			if got := string(tree.Root.Epilogue); got != tt.epilogue || tree.Root.EpilogueTruncated {
				t.Errorf("epilogue %q (cut %v), want %q", got, tree.Root.EpilogueTruncated, tt.epilogue)
			}
			if got := string(tree.Root.Preamble); !strings.HasPrefix(got, "This is a multi-part message in MIME format.") {
				t.Errorf("preamble %q", got)
			}
			if len(tree.Root.Children) != 1 || tree.Root.Children[0].Preamble != nil || tree.Root.Children[0].Epilogue != nil {
				t.Errorf("leaf has a preamble or epilogue: %+v", tree.Root.Children)
			}
		})
	}
}

// TestReadTree_epilogueForensic pins that under ProfileForensic the message's
// own epilogue is read through the trailing input, which accounts for it.
func TestReadTree_epilogueForensic(t *testing.T) {
	const footer = "From: other@example.org\r\nSubject: second\r\n\r\nbody\r\n"
	raw := "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\nhello\r\n--B--\r\n" + footer
	s, tree := obTree(t, raw, &message.ParseOptions{Profile: message.ProfileForensic}, false)
	if string(tree.Root.Epilogue) != footer {
		t.Fatalf("epilogue %q", tree.Root.Epilogue)
	}
	ti := s.TrailingInput()
	if ti.Status() != message.TrailingEnded || ti.Delivered() != int64(len(footer)) {
		t.Errorf("trailing input %v, %d delivered", ti.Status(), ti.Delivered())
	}
	if ev, _ := ti.Evidence(); ev != message.TrailingEvidenceMessageLike {
		t.Errorf("evidence %v", ev)
	}
	r, ok := s.TrailingInputReader()
	if !ok {
		t.Fatal("no trailing input reader")
	}
	if rest, err := io.ReadAll(r); len(rest) != 0 || err != nil {
		t.Errorf("trailing input reader delivers %q again (%v)", rest, err)
	}
	var trailing int
	for _, d := range s.Diagnostics() {
		if d.Code == message.CodeInputTrailingMessages {
			trailing++
		}
	}
	if trailing != 1 {
		t.Errorf("%d input.trailing_messages notes, want 1", trailing)
	}
}

// TestNode_BodiesKeepsPrefer pins that Bodies does not change the caller's
// slice.
func TestNode_BodiesKeepsPrefer(t *testing.T) {
	raw := "Content-Type: multipart/alternative; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain\r\n\r\nplain\r\n" +
		"--B\r\nContent-Type: text/html\r\n\r\n<p>html</p>\r\n--B--\r\n"
	_, tree := obTree(t, raw, nil, false)
	prefer := []string{" TEXT/PLAIN "}
	b := tree.Root.Bodies(prefer...)
	if len(b) != 1 || b[0].MediaType != "text/plain" {
		t.Fatalf("bodies %v", b)
	}
	if prefer[0] != " TEXT/PLAIN " {
		t.Errorf("prefer changed to %q", prefer[0])
	}
}

// TestParseAuthenticationResults_emptyID pins that an authserv-id written as
// an empty quoted string is reported.
func TestParseAuthenticationResults_emptyID(t *testing.T) {
	r := mail.ParseAuthenticationResults(`""; spf=pass smtp.mailfrom=a@example.com`)
	if r.AuthServID != "" || len(r.Methods) != 1 || len(r.Diagnostics) != 1 ||
		r.Diagnostics[0].Code != message.CodeAuthResultsInvalid || r.Diagnostics[0].Severity != message.SeverityInvalidMetadata {
		t.Errorf("result %+v", r)
	}
	h := mail.Header{}
	h.Add("Authentication-Results", `""; spf=pass`)
	if got := h.AuthenticationResults(&mail.AuthResultsOptions{TrustedIDs: []string{""}}); len(got) != 1 || got[0].Trusted {
		t.Errorf("an empty authserv-id is trusted: %+v", got)
	}
}

// TestReadTree_corpusObservations reads two corpus fixtures through the tree:
// the list footer after the closing delimiter is noted, and the flowed mobile
// reply unwraps (corpus findings list-footer-after-close and
// provider-mobile-format-flowed).
func TestReadTree_corpusObservations(t *testing.T) {
	read := func(name string) *mail.Tree {
		raw, err := os.ReadFile(filepath.Join("..", "testdata", "corpus", name+".eml"))
		if err != nil {
			t.Fatal(err)
		}
		_, tree := obTree(t, string(raw), nil, false)
		return tree
	}
	tree := read("list-footer-after-close")
	if len(tree.Diagnostics) != 1 || tree.Diagnostics[0].Code != message.CodeMultipartTrailingJunk ||
		!strings.Contains(tree.Diagnostics[0].Message, "read as text") || !bytes.Contains(tree.Root.Epilogue, []byte("unsubscribe")) {
		t.Errorf("list footer: %v, epilogue %q", tree.Diagnostics, tree.Root.Epilogue)
	}
	tree = read("provider-mobile-format-flowed")
	if text, ok := tree.Root.FlowedText(); !ok || !strings.Contains(text, "만나요.") || !strings.Contains(text, "연락 주세요.") {
		t.Errorf("flowed: %q, %v", text, ok)
	}
}
