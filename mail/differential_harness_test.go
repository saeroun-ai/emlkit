package mail_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

// TestDifferentialHarness checks the differential harness itself, with no
// driver: the exceptions file is valid, the emlkit dump fills the schema, and
// the comparison and the exception matching report what they should. It runs
// in every test run, so the gated TestDifferentialPython is not the only check
// of its own logic.
func TestDifferentialHarness(t *testing.T) {
	exceptions := dfLoadExceptions(t, filepath.Join("..", "testdata", "differential", "exceptions.json"))
	if len(exceptions) == 0 {
		t.Fatal("the exceptions file holds no exception")
	}

	const raw = "From: A <a@example.com>\r\n" +
		"Subject: =?utf-8?q?hi_there?=\r\n" +
		"Date: Tue, 1 Jul 2003 10:52:37 +0200\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nhello\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=x.bin\r\n" +
		"Content-ID: <c@x>\r\nContent-Transfer-Encoding: Base64\r\n\r\naGk=\r\n" +
		"--B--\r\n"
	m := dfDumpEmlkit("self", []byte(raw))
	if m.Error != "" || m.Root == nil {
		t.Fatalf("dump: error %q, root %v", m.Error, m.Root)
	}
	if m.Subject == nil || *m.Subject != "hi there" {
		t.Errorf("subject %v", dfJSON(m.Subject))
	}
	if want := []dfAddress{{Name: "A", Address: "a@example.com"}}; !reflect.DeepEqual(m.From, want) {
		t.Errorf("from %v, want %v", m.From, want)
	}
	if m.To != nil {
		t.Errorf("to %v, want null for an absent field", m.To)
	}
	if m.Date == nil || *m.Date != "2003-07-01T08:52:37Z" {
		t.Errorf("date %v", dfJSON(m.Date))
	}
	root := m.Root
	if root.MediaType != "multipart/mixed" || len(root.Children) != 2 || root.BodyLength != nil {
		t.Fatalf("root %s", dfJSON(root))
	}
	sum := func(s string) string {
		h := sha256.Sum256([]byte(s))
		return hex.EncodeToString(h[:])
	}
	text := root.Children[0]
	if text.charset() != "utf-8" || text.BodyLength == nil || *text.BodyLength != 5 ||
		text.BodySHA256 == nil || *text.BodySHA256 != sum("hello") ||
		text.TextSHA256 == nil || *text.TextSHA256 != sum("hello") || text.Children != nil {
		t.Errorf("text part %s", dfJSON(text))
	}
	att := root.Children[1]
	if att.Filename == nil || *att.Filename != "x.bin" || att.ContentID == nil || *att.ContentID != "c@x" ||
		att.TransferEncoding == nil || *att.TransferEncoding != "base64" ||
		att.Disposition == nil || *att.Disposition != "attachment" ||
		att.BodySHA256 == nil || *att.BodySHA256 != sum("hi") || att.TextSHA256 != nil {
		t.Errorf("attachment %s", dfJSON(att))
	}
	if !reflect.DeepEqual(att.Path, []int{1}) || len(root.Defects) != 0 {
		t.Errorf("attachment path %v, root defects %v", att.Path, root.Defects)
	}

	// An identical record differs in nothing.
	var other dfMessage
	if err := json.Unmarshal([]byte(dfJSON(m)), &other); err != nil {
		t.Fatal(err)
	}
	if d := dfCompare("p", m, &other); len(d) != 0 {
		t.Fatalf("an identical record differs: %s", dfJSON(d))
	}

	// Each change is one record; a part kept whole by one parser and framed
	// by the other differs in its children alone.
	y := "y.bin"
	other.Subject = nil
	other.Root.Children[1].Filename = &y
	other.Root.Children[0].Children = []*dfPart{}
	diffs := dfCompare("p", m, &other)
	type pf struct{ Path, Field string }
	var got []pf
	for _, d := range diffs {
		got = append(got, pf{d.Path, d.Field})
	}
	want := []pf{{"[]", "subject"}, {"[0]", "children"}, {"[1]", "filename"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differences %v, want %v", got, want)
	}
	if string(diffs[2].Emlkit) != `"x.bin"` || string(diffs[2].Other) != `"y.bin"` || string(diffs[0].Other) != "null" {
		t.Errorf("difference values %s", dfJSON(diffs))
	}

	// Exceptions match by exact value, "*" and a prefix with "*".
	xs := []*dfException{
		{Parser: "p", Fixture: "self", Path: "[1]", Field: "file*", Rationale: "r"},
		{Parser: "*", Fixture: "other", Path: "*", Field: "*", Rationale: "r"},
	}
	if x := dfExplain(xs, diffs[2]); x != xs[0] {
		t.Errorf("filename difference explained by %v", dfJSON(x))
	}
	if x := dfExplain(xs, diffs[0]); x != nil {
		t.Errorf("subject difference explained by %v", dfJSON(x))
	}
	if xs[0].matched != 1 || xs[1].matched != 0 {
		t.Errorf("matched %d and %d", xs[0].matched, xs[1].matched)
	}
}
