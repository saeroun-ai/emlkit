package mail_test

import (
	"io"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// A node of a tree read from ParseAt gives its raw bytes beside its decoded
// data, as often as asked.
func TestNodeRawSection(t *testing.T) {
	const text = "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\naGVsbG8=\r\n" +
		"--B--\r\n"
	src := strings.NewReader(text)
	_, e, err := message.ParseAt(src, int64(len(text)), nil)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := mail.ReadTree(e, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tree.Root.RawSection(src); ok {
		t.Error("the root has a span")
	}
	leaf := tree.Root.Children[0]
	for i := 0; i < 2; i++ {
		r, ok := leaf.RawSection(src)
		if !ok {
			t.Fatal("no raw section")
		}
		raw, _ := io.ReadAll(r)
		const want = "Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\naGVsbG8="
		if string(raw) != want || string(leaf.Data) != "hello" {
			t.Errorf("raw = %q, decoded = %q", raw, leaf.Data)
		}
	}
}
