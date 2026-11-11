package mail_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// fzTreeMaxBody is how much of each leaf FuzzReadTree keeps.
const fzTreeMaxBody = 4096

// fzTreeOutcome parses data, reads its whole MIME tree with embedded messages
// descended into, checks each node, and renders the tree, the views and both
// lists of diagnostics.
func fzTreeOutcome(t *testing.T, src io.Reader, data []byte) string {
	t.Helper()
	opts := &message.ParseOptions{
		WalkEmbeddedMessages: true,
		Limits:               message.Limits{MaxInputBytes: 1 << 20, MaxPartCount: 256, MaxMIMEDepth: 16, MaxMessageDepth: 4},
	}
	s, e, err := message.Parse(src, opts)
	if e == nil {
		if err == nil {
			t.Fatal("no entity and no error")
		}
		return fmt.Sprintf("no entity: fatal=%v", message.IsFatal(err))
	}
	tree, terr := mail.ReadTree(e, &mail.TreeOptions{DescendMessages: true, MaxBodyBytes: fzTreeMaxBody})
	if tree == nil || tree.Root == nil {
		t.Fatalf("ReadTree returned no tree (%v)", terr)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "tree err=%v\n", terr != nil)
	order := map[*mail.Node]int{}
	tree.Root.Walk(func(n *mail.Node) bool {
		order[n] = len(order)
		fzCheckNode(t, n, order, len(data))
		dup := "-"
		if n.DuplicateOf != nil {
			dup = fmt.Sprint(n.DuplicateOf.Path)
		}
		fmt.Fprintf(&b, "%v %s %s %q disp=%q file=%q cid=%q size=%d trunc=%v sha=%x dup=%s warn=%v err=%v span=%v:%d-%d\n",
			n.Path, n.Role, n.MediaType, n.Reason, n.Disposition, n.Filename, n.ContentID, n.Size, n.Truncated,
			n.SHA256[:4], dup, n.Warning != nil, n.Err != nil, n.HasSpan, n.SpanStart, n.SpanEnd)
		return true
	})
	for _, n := range tree.Root.Bodies() {
		fmt.Fprintf(&b, "body %v\n", n.Path)
	}
	for _, n := range tree.Root.Bodies("text/html", "TEXT/PLAIN ") {
		fmt.Fprintf(&b, "preferred body %v\n", n.Path)
	}
	for _, n := range tree.Root.Attachments() {
		if n.Role != mail.RoleAttachment && n.Role != mail.RoleMessage {
			t.Fatalf("Attachments returned %v, a %s", n.Path, n.Role)
		}
		fmt.Fprintf(&b, "attachment %v\n", n.Path)
	}
	for _, d := range tree.Diagnostics {
		fmt.Fprintf(&b, "shape %s %v\n", d.Code, d.Path)
	}
	for _, d := range s.Diagnostics() {
		fmt.Fprintf(&b, "parse %s %v\n", d.Code, d.Path)
	}
	return b.String()
}

// fzCheckNode holds one node to what Node documents: its path and parent, a
// known role, the filename its candidates give, a leaf's data within the cap
// and whole when not truncated, with its size and digest, a duplicate that is
// an earlier leaf of the same content, and a span within the input. The views
// it offers must not fail on it.
func fzCheckNode(t *testing.T, n *mail.Node, order map[*mail.Node]int, inputLen int) {
	t.Helper()
	if strings.HasPrefix(n.Role.String(), "Role(") {
		t.Fatalf("%v: role %v", n.Path, n.Role)
	}
	for i, c := range n.Children {
		if c.Parent != n || len(c.Path) != len(n.Path)+1 || c.Path[len(n.Path)] != i || fmt.Sprint(c.Path[:len(n.Path)]) != fmt.Sprint(n.Path) {
			t.Fatalf("child %d of %v has path %v", i, n.Path, c.Path)
		}
	}
	if len(n.Filenames) > 0 && n.Filename != n.Filenames[0].Filename && n.Filenames[0].Filename != "" {
		t.Fatalf("%v: Filename %q, first candidate %q", n.Path, n.Filename, n.Filenames[0].Filename)
	}
	if fzContainer(n) {
		if n.Data != nil || n.Size != 0 || n.SHA256 != ([sha256.Size]byte{}) {
			t.Fatalf("%v: a container or a carrier descended into holds %d bytes of data, size %d", n.Path, len(n.Data), n.Size)
		}
	} else {
		if len(n.Data) > fzTreeMaxBody || int64(len(n.Data)) > n.Size {
			t.Fatalf("%v: %d bytes of data, size %d", n.Path, len(n.Data), n.Size)
		}
		if !n.Truncated && (int64(len(n.Data)) != n.Size || sha256.Sum256(n.Data) != n.SHA256) {
			t.Fatalf("%v: data of %d bytes is whole but does not have size %d or its digest", n.Path, len(n.Data), n.Size)
		}
	}
	if d := n.DuplicateOf; d != nil {
		if at, ok := order[d]; !ok || at >= order[n] || fzContainer(d) || d.SHA256 != n.SHA256 || d.Size != n.Size || n.Size == 0 {
			t.Fatalf("%v: duplicate of %v, which is not an earlier leaf of the same content", n.Path, d.Path)
		}
	}
	if n.HasSpan && (n.SpanStart < 0 || n.SpanStart > n.SpanEnd || n.SpanEnd > int64(inputLen)) {
		t.Fatalf("%v: span %d-%d of an input of %d bytes", n.Path, n.SpanStart, n.SpanEnd, inputLen)
	}
	if n.ContentID != "" {
		n.Resolve("cid:" + n.ContentID)
	}
	n.Resolve(n.ContentLocation)
	n.RelatedRoot()
	n.ReportFields()
}

// fzContainer reports whether n is a multipart, or a carrier whose message
// was descended into: a node with no body of its own.
func fzContainer(n *mail.Node) bool {
	return n.Role == mail.RoleContainer || len(n.Children) > 0
}

var fzTreeSeeds = []string{
	"Subject: s\r\n\r\nbody",
	"Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: multipart/alternative; boundary=C\r\n\r\n--C\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n한글 본문\r\n--C\r\nContent-Type: text/html\r\n\r\n<p>本文</p>\r\n--C--\r\n--B\r\nContent-Type: application/pdf; name=\"=?UTF-8?B?7ZWc6riA?=.pdf\"\r\nContent-Disposition: attachment\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0=\r\n--B--\r\n",
	"Content-Type: multipart/related; boundary=R; start=\"<b@x>\"\r\n\r\n--R\r\nContent-ID: <a@x>\r\nContent-Type: image/png\r\n\r\npng\r\n--R\r\nContent-ID: <b@x>\r\nContent-Type: text/html\r\n\r\n<img src=\"cid:a@x\">\r\n--R--\r\n",
	"Content-Type: multipart/signed; protocol=\"application/pgp-signature\"; boundary=S\r\n\r\n--S\r\n\r\nsigned\r\n--S\r\nContent-Type: application/pgp-signature\r\n\r\nsig\r\n--S--\r\n",
	"Content-Type: multipart/report; report-type=delivery-status; boundary=D\r\n\r\n--D\r\n\r\nfailed\r\n--D\r\nContent-Type: message/delivery-status\r\n\r\nReporting-MTA: dns; mx.example.kr\r\n\r\nFinal-Recipient: rfc822; a@example.kr\r\nAction: failed\r\n--D--\r\n",
	"Content-Type: message/rfc822\r\n\r\nSubject: inner\r\nContent-Type: text/plain\r\n\r\ninner body",
	"Content-Type: message/rfc822\r\nContent-Transfer-Encoding: base64\r\n\r\nU3ViamVjdDogeA0KDQpib2R5",
	"Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\nsame\r\n--B\r\n\r\nsame\r\n--B--\r\n",
	"Content-Type: multipart/mixed; boundary=WRONG\r\n\r\n--X1\r\n\r\none\r\n--X1\r\n\r\ntwo\r\n--X1-",
	"Content-Type: text/plain; charset=iso-2022-jp\r\nContent-Disposition: attachment; filename*=UTF-8''%E6%97%A5%E6%9C%AC.txt\r\n\r\n\x1b$BF|K\\8l\x1b(B",
	"Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\nx\r\n--B--\r\n",
	"Subject: s\r\ndamaged\r\nContent-Type: multipart/alternative;\r\nboundary=\"A\"\r\n\r\n--A\r\n\r\n中文\r\n--A--",
}

// FuzzReadTree parses arbitrary bytes and reads their whole MIME tree with
// mail.ReadTree, embedded messages included, checking every node (see
// fzCheckNode) and that the tree, its views and every diagnostic are the
// same whether the input arrives whole or a byte at a time.
func FuzzReadTree(f *testing.F) {
	for _, s := range fzTreeSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 16<<10 {
			return
		}
		whole := fzTreeOutcome(t, bytes.NewReader(data), data)
		bytewise := fzTreeOutcome(t, iotest.OneByteReader(bytes.NewReader(data)), data)
		if whole != bytewise {
			t.Fatalf("reading a byte at a time changed the tree\nwhole:\n%s\nbytewise:\n%s", whole, bytewise)
		}
	})
}

// TestReadTree_unframedMultipartIsALeaf is the regression for what
// FuzzReadTree found (testdata/fuzz/FuzzReadTree/2731b81b86c2673e): a
// multipart left without a usable boundary is one opaque leaf, as mail.Reader
// reads it, whether or not ReadTree keeps its data. It used to be a container
// with no children whenever Node.Data was empty — an empty body, MaxBodyBytes
// below zero, a Body hook — so its content was in neither Bodies nor
// Attachments.
func TestReadTree_unframedMultipartIsALeaf(t *testing.T) {
	const text = "Content-Type: multipart/mixed; boundary=B\r\n\r\n--O\r\nContent-Type: multipart/mixed; boundary=B; boundary=C\r\n\r\nhidden text\r\n--O--\r\n"
	for _, tc := range []struct {
		name string
		in   string
		opts *mail.TreeOptions
	}{
		{"empty body", "Content-Type: multipart\r\n\r\n", nil},
		{"data not kept", text, &mail.TreeOptions{MaxBodyBytes: -1}},
		{"body hook", text, &mail.TreeOptions{Body: func(*mail.Node, io.Reader) error { return nil }}},
	} {
		_, e, err := message.Parse(strings.NewReader(tc.in), nil)
		if err != nil {
			t.Fatalf("%s: Parse() = %v", tc.name, err)
		}
		tree, err := mail.ReadTree(e, tc.opts)
		if err != nil {
			t.Fatalf("%s: ReadTree() = %v", tc.name, err)
		}
		var leaves []*mail.Node
		tree.Root.Walk(func(n *mail.Node) bool {
			if strings.HasPrefix(n.MediaType, "multipart/") && n.SHA256 != ([sha256.Size]byte{}) {
				leaves = append(leaves, n)
			}
			return true
		})
		if len(leaves) != 1 || leaves[0].Role != mail.RoleAttachment {
			t.Errorf("%s: %d unframed multiparts read as leaves, want one attachment", tc.name, len(leaves))
			continue
		}
		if got := tree.Root.Attachments(); len(got) != 1 || got[0] != leaves[0] {
			t.Errorf("%s: Attachments() = %v, want the unframed multipart", tc.name, got)
		}
	}
}
