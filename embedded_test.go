package message

import (
	"encoding/base64"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

const embeddedInner = "Subject: inner\r\nContent-Type: multipart/mixed; boundary=I\r\n\r\n" +
	"--I\r\nContent-Type: text/plain\r\n\r\ninner text\r\n" +
	"--I\r\nContent-Type: text/html\r\n\r\n<b>inner</b>\r\n--I--\r\n"

func embeddedOuter(part string) string {
	return "Subject: outer\r\nContent-Type: multipart/mixed; boundary=O\r\n\r\n" +
		"--O\r\nContent-Type: text/plain\r\n\r\nouter text\r\n" +
		"--O\r\n" + part + "\r\n--O--\r\n"
}

// walkAll walks e with embedded messages and returns "path type subject|body"
// for every entity that is neither a container nor a carrier.
func walkAll(t *testing.T, text string, opts *ParseOptions) ([]string, []Diagnostic, error) {
	t.Helper()
	s, e, err := Parse(strings.NewReader(text), opts)
	if e == nil {
		t.Fatalf("Parse() = %v", err)
	}
	var out []string
	werr := e.Walk(func(path []int, p *Entity, err error) error {
		mt, _, _ := p.EffectiveContentType()
		if p.MultipartReader() != nil || (p.CarriesMessage() && opts != nil && opts.WalkEmbeddedMessages) {
			out = append(out, fmt.Sprintf("%v %s {%s}", path, mt, p.Header.Get("Subject")))
			return nil
		}
		b, rerr := io.ReadAll(p.Body)
		if rerr != nil {
			return rerr
		}
		out = append(out, fmt.Sprintf("%v %s|%s", path, mt, b))
		return nil
	})
	return out, s.Diagnostics(), werr
}

func TestWalk_embeddedMessages(t *testing.T) {
	text := embeddedOuter("Content-Type: message/rfc822\r\n\r\n" + embeddedInner)
	got, diags, err := walkAll(t, text, &ParseOptions{WalkEmbeddedMessages: true})
	if err != nil || len(diags) != 0 {
		t.Fatalf("Walk() = %v, diagnostics %v", err, diags)
	}
	want := []string{
		"[] multipart/mixed {outer}",
		"[0] text/plain|outer text",
		"[1] message/rfc822 {}",
		"[1 0] multipart/mixed {inner}",
		"[1 0 0] text/plain|inner text",
		"[1 0 1] text/html|<b>inner</b>",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("walk %q\nwant %q", got, want)
	}

	// Without the option an embedded message is the leaf it always was.
	got, _, err = walkAll(t, text, nil)
	want = []string{"[] multipart/mixed {outer}", "[0] text/plain|outer text", "[1] message/rfc822|" + embeddedInner}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("default walk %q, %v\nwant %q", got, err, want)
	}
}

func TestWalk_embeddedMessageEncoded(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("Subject: b64 inner\r\nBroken line\r\nTo: a@example.com\r\n\r\nhello"))
	text := embeddedOuter("Content-Type: message/rfc822\r\nContent-Transfer-Encoding: base64\r\n\r\n" + enc)
	got, diags, err := walkAll(t, text, &ParseOptions{WalkEmbeddedMessages: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"[] multipart/mixed {outer}", "[0] text/plain|outer text", "[1] message/rfc822 {}", "[1 0] text/plain|hello"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("walk %q\nwant %q", got, want)
	}
	// The damaged line of the embedded header is healed and reported under the
	// embedded message's path, with no input offset: its bytes were decoded.
	var found bool
	for _, d := range diags {
		if d.Code == CodeHeaderMissingColon {
			found = true
			if !reflect.DeepEqual(d.Path, []int{1, 0}) || d.Location.HasOffset {
				t.Errorf("diagnostic %v: path %v, offset known %v; want [1 0] and no offset", d.Code, d.Path, d.Location.HasOffset)
			}
		}
	}
	if !found {
		t.Errorf("diagnostics %v, want header.missing_colon for the embedded header", codesOfDiags(diags))
	}
}

func TestEmbeddedMessage_explicit(t *testing.T) {
	text := embeddedOuter("Content-Type: message/rfc822\r\n\r\n" + embeddedInner)
	s, e, err := Parse(strings.NewReader(text), nil)
	if err != nil {
		t.Fatal(err)
	}
	mr := e.MultipartReader()
	first, _ := mr.NextPart()
	if first.CarriesMessage() {
		t.Error("text/plain part CarriesMessage")
	}
	if emb, err := first.EmbeddedMessage(); emb != nil || err != nil {
		t.Errorf("EmbeddedMessage() of a text part = (%v, %v), want nil, nil", emb != nil, err)
	}
	second, err := mr.NextPart()
	if err != nil || !second.CarriesMessage() {
		t.Fatalf("second part: %v, carries %v", err, second.CarriesMessage())
	}
	emb, err := second.EmbeddedMessage()
	if err != nil || emb == nil {
		t.Fatalf("EmbeddedMessage() = %v", err)
	}
	if again, _ := second.EmbeddedMessage(); again != emb {
		t.Error("a second call returned another entity")
	}
	if emb.Header.Get("Subject") != "inner" {
		t.Errorf("embedded subject %q", emb.Header.Get("Subject"))
	}
	imr := emb.MultipartReader()
	p, err := imr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(p.Body); string(b) != "inner text" {
		t.Errorf("inner part %q", b)
	}
	if !reflect.DeepEqual(p.parse.origin.path, []int{1, 0, 0}) {
		t.Errorf("inner part path %v, want [1 0 0]", p.parse.origin.path)
	}
	// Its offsets are the input's.
	if at := p.parse.origin.bodyOffset; !strings.HasPrefix(text[at:], "inner text") {
		t.Errorf("inner part body offset %d points at %q", at, text[at:min(int(at)+12, len(text))])
	}
	if s.Len() != 0 {
		t.Errorf("diagnostics %v", s.Diagnostics())
	}

	// An entity that belongs to no parse is read as Read reads a message.
	h, _ := textprotoHeader("Content-Type: message/rfc822")
	legacy, _ := New(h, strings.NewReader("Subject: x\r\n\r\nbody"))
	emb, err = legacy.EmbeddedMessage()
	if err != nil || emb.Header.Get("Subject") != "x" {
		t.Fatalf("New: EmbeddedMessage() = %v", err)
	}
	if b, _ := io.ReadAll(emb.Body); string(b) != "body" {
		t.Errorf("New: embedded body %q", b)
	}
}

func TestEmbeddedMessage_depthBudget(t *testing.T) {
	// A message in a message in a message.
	level2 := "Subject: two\r\n\r\ndeepest"
	level1 := "Subject: one\r\nContent-Type: message/rfc822\r\n\r\n" + level2
	text := "Subject: zero\r\nContent-Type: message/rfc822\r\n\r\n" + level1

	got, diags, err := walkAll(t, text, &ParseOptions{WalkEmbeddedMessages: true})
	want := []string{"[] message/rfc822 {zero}", "[0] message/rfc822 {one}", "[0 0] text/plain|deepest"}
	if err != nil || len(diags) != 0 || !reflect.DeepEqual(got, want) {
		t.Errorf("unbounded: %q, %v, %v\nwant %q", got, diags, err, want)
	}

	got, diags, err = walkAll(t, text, &ParseOptions{WalkEmbeddedMessages: true, Limits: Limits{MaxMessageDepth: 1}})
	want = []string{"[] message/rfc822 {zero}", "[0] message/rfc822 {one}"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("depth 1: %q, %v\nwant %q", got, err, want)
	}
	if codes := codesOfDiags(diags); !reflect.DeepEqual(codes, []Code{CodeLimitMessageDepth}) {
		t.Errorf("depth 1: diagnostics %v, want limit.message_depth", codes)
	}

	// Asked for explicitly, the refusal is the entity's own FatalError and the
	// parse goes on.
	_, e, _ := Parse(strings.NewReader(text), &ParseOptions{Limits: Limits{MaxMessageDepth: 1}})
	one, err := e.EmbeddedMessage()
	if err != nil {
		t.Fatal(err)
	}
	two, err := one.EmbeddedMessage()
	if two != nil || !IsFatal(err) || e.ParseStop() != nil {
		t.Errorf("EmbeddedMessage() past the budget = (%v, %v), stop %v; want an entity-scoped FatalError", two != nil, err, e.ParseStop())
	}
	if b, _ := io.ReadAll(one.Body); string(b) != level2 {
		t.Errorf("the refused carrier's body %q, want it untouched", b)
	}
}

func TestParse_digestDefaultType(t *testing.T) {
	const text = "Content-Type: multipart/digest; boundary=D\r\n\r\n" +
		"--D\r\n\r\nSubject: first\r\n\r\none\r\n" +
		"--D\r\nContent-Type: text/plain\r\n\r\nplain\r\n" +
		"--D\r\nContent-Type: message/rfc822\r\n\r\nSubject: third\r\n\r\nthree\r\n--D--\r\n"
	got, diags, err := walkAll(t, text, &ParseOptions{WalkEmbeddedMessages: true})
	want := []string{
		"[] multipart/digest {}",
		"[0] message/rfc822 {}", "[0 0] text/plain|one",
		"[1] text/plain|plain",
		"[2] message/rfc822 {}", "[2 0] text/plain|three",
	}
	if err != nil || len(diags) != 0 || !reflect.DeepEqual(got, want) {
		t.Errorf("walk %q, %v, %v\nwant %q", got, diags, err, want)
	}
}

func TestEmbeddedMessage_headersOnlyAndEmpty(t *testing.T) {
	_, e, _ := Parse(strings.NewReader("Content-Type: text/rfc822-headers\r\n\r\nSubject: bounced\r\nTo: a@example.com\r\n"), nil)
	emb, err := e.EmbeddedMessage()
	if err != nil || emb.Header.Get("Subject") != "bounced" || emb.Header.Get("To") != "a@example.com" {
		t.Errorf("text/rfc822-headers: %v", err)
	}

	s, e, _ := Parse(strings.NewReader("Content-Type: message/rfc822\r\n\r\n"), nil)
	emb, err = e.EmbeddedMessage()
	if err != nil || emb == nil || emb.Header.Len() != 0 {
		t.Errorf("empty: (%v, %v)", emb != nil, err)
	}
	if codes := codesOfDiags(s.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeMessageEmbeddedTruncated}) {
		t.Errorf("empty: diagnostics %v, want message.embedded_truncated", codes)
	}

	// A walkFunc that read the carrier's body leaves nothing to descend into,
	// and that is no defect.
	s, e, _ = Parse(strings.NewReader("Content-Type: message/rfc822\r\n\r\nSubject: x\r\n\r\nbody"), &ParseOptions{WalkEmbeddedMessages: true})
	visits := 0
	werr := e.Walk(func(path []int, p *Entity, err error) error {
		visits++
		io.Copy(io.Discard, p.Body)
		return nil
	})
	if werr != nil || visits != 1 || s.Len() != 0 {
		t.Errorf("consumed carrier: %d visits, %v, %v", visits, werr, s.Diagnostics())
	}
}
