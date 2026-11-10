package mail_test

import (
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

const treeMessage = "Subject: tree\r\nContent-Type: multipart/mixed; boundary=M\r\n\r\n" +
	"--M\r\nContent-Type: multipart/related; boundary=R; start=\"<root@x>\"; type=\"multipart/alternative\"\r\n\r\n" +
	"--R\r\nContent-Type: image/png\r\nContent-ID: <logo@x>\r\nContent-Disposition: inline; filename=logo.png\r\n\r\nPNGDATA\r\n" +
	"--R\r\nContent-Type: multipart/alternative; boundary=A\r\nContent-ID: <root@x>\r\n\r\n" +
	"--A\r\nContent-Type: text/plain\r\n\r\nplain body\r\n" +
	"--A\r\nContent-Type: text/html\r\nContent-Location: http://example.com/mail/index.html\r\n\r\n<img src=\"cid:logo@x\"><img src=\"pics/a.gif\">\r\n" +
	"--A--\r\n" +
	"--R\r\nContent-Type: image/gif\r\nContent-Location: http://example.com/mail/pics/a.gif\r\n\r\nGIFDATA\r\n" +
	"--R--\r\n" +
	"--M\r\nContent-Type: application/pdf; name=report.pdf\r\nContent-Disposition: attachment; filename=report.pdf\r\n\r\nPDFDATA\r\n" +
	"--M\r\nContent-Type: text/plain; name=notes.txt\r\n\r\nnotes\r\n" +
	"--M\r\nContent-Type: application/octet-stream\r\n\r\nPDFDATA\r\n" +
	"--M\r\nContent-Type: message/rfc822\r\n\r\nSubject: fwd\r\nContent-Type: multipart/mixed; boundary=F\r\n\r\n" +
	"--F\r\n\r\nforwarded text\r\n--F\r\nContent-Type: application/zip\r\n\r\nZIP\r\n--F--\r\n" +
	"--M--\r\n"

func readTree(t *testing.T, text string, opts *mail.TreeOptions) (*mail.Tree, *message.ParseSession) {
	t.Helper()
	s, e, err := message.Parse(strings.NewReader(text), nil)
	if e == nil {
		t.Fatalf("Parse() = %v", err)
	}
	tree, err := mail.ReadTree(e, opts)
	if err != nil {
		t.Fatalf("ReadTree() = %v", err)
	}
	return tree, s
}

func outline(n *mail.Node) []string {
	var out []string
	n.Walk(func(c *mail.Node) bool {
		out = append(out, fmt.Sprintf("%v %s %s", c.Path, c.MediaType, c.Role))
		return true
	})
	return out
}

func TestReadTree_structure(t *testing.T) {
	tree, s := readTree(t, treeMessage, &mail.TreeOptions{DescendMessages: true})
	if s.Len() != 0 || len(tree.Diagnostics) != 0 {
		t.Fatalf("diagnostics %v, %v", s.Diagnostics(), tree.Diagnostics)
	}
	want := []string{
		"[] multipart/mixed container",
		"[0] multipart/related container",
		"[0 0] image/png related",
		"[0 1] multipart/alternative container",
		"[0 1 0] text/plain body",
		"[0 1 1] text/html body",
		"[0 2] image/gif related",
		"[1] application/pdf attachment",
		"[2] text/plain attachment",
		"[3] application/octet-stream attachment",
		"[4] message/rfc822 message",
		"[4 0] multipart/mixed container",
		"[4 0 0] text/plain body",
		"[4 0 1] application/zip attachment",
	}
	if got := outline(tree.Root); !reflect.DeepEqual(got, want) {
		t.Errorf("outline %q\nwant %q", got, want)
	}

	root := tree.Root
	// Bodies: one form of the alternative, by preference, nothing discarded.
	for _, tc := range []struct {
		prefer []string
		want   string
	}{
		{nil, "<img src=\"cid:logo@x\"><img src=\"pics/a.gif\">"},
		{[]string{"text/html", "text/plain"}, "<img src=\"cid:logo@x\"><img src=\"pics/a.gif\">"},
		{[]string{"text/plain"}, "plain body"},
		{[]string{"text/enriched", "TEXT/PLAIN "}, "plain body"},
		{[]string{"text/enriched"}, "<img src=\"cid:logo@x\"><img src=\"pics/a.gif\">"},
	} {
		b := root.Bodies(tc.prefer...)
		if len(b) != 1 || string(b[0].Data) != tc.want {
			t.Errorf("Bodies(%q) = %d nodes %q, want %q", tc.prefer, len(b), firstData(b), tc.want)
		}
	}

	// Attachments: the outer message's own, the embedded message as one.
	var names []string
	for _, a := range root.Attachments() {
		names = append(names, fmt.Sprintf("%v %s %q", a.Path, a.MediaType, a.Filename))
	}
	wantAtt := []string{`[1] application/pdf "report.pdf"`, `[2] text/plain "notes.txt"`, `[3] application/octet-stream ""`, `[4] message/rfc822 ""`}
	if !reflect.DeepEqual(names, wantAtt) {
		t.Errorf("attachments %q\nwant %q", names, wantAtt)
	}
	fwd := root.Children[4].Children[0]
	if a := fwd.Attachments(); len(a) != 1 || a[0].MediaType != "application/zip" {
		t.Errorf("embedded message attachments: %v", outline(fwd))
	}
	if b := fwd.Bodies(); len(b) != 1 || string(b[0].Data) != "forwarded text" {
		t.Errorf("embedded message body: %q", firstData(b))
	}

	// multipart/related: the root by its start parameter, and references.
	rel := root.Children[0]
	if r := rel.RelatedRoot(); r == nil || r.MediaType != "multipart/alternative" {
		t.Errorf("RelatedRoot() = %v, want the part start names", r)
	}
	html := rel.Children[1].Children[1]
	for ref, want := range map[string]string{
		"cid:logo@x":                           "image/png",
		"CID:logo%40x":                         "image/png",
		"<logo@x>":                             "image/png",
		"pics/a.gif":                           "image/gif",
		"http://example.com/mail/pics/a.gif":   "image/gif",
		"cid:missing@x":                        "",
		"http://tracker.example.net/pixel.gif": "",
		"":                                     "",
	} {
		got := ""
		if n := html.Resolve(ref); n != nil {
			got = n.MediaType
		}
		if got != want {
			t.Errorf("Resolve(%q) = %q, want %q", ref, got, want)
		}
	}

	// Sizes, digests and duplicates.
	pdf, dup := root.Children[1], root.Children[3]
	if pdf.Size != 7 || pdf.Truncated || dup.DuplicateOf != pdf || pdf.DuplicateOf != nil || pdf.SHA256 != dup.SHA256 {
		t.Errorf("pdf size %d, duplicate of %v", pdf.Size, dup.DuplicateOf)
	}
	// The span of a part is its bytes in the input, header and body.
	if !pdf.HasSpan || !strings.HasPrefix(treeMessage[pdf.SpanStart:pdf.SpanEnd], "Content-Type: application/pdf") ||
		!strings.HasSuffix(treeMessage[pdf.SpanStart:pdf.SpanEnd], "\r\n\r\nPDFDATA") {
		t.Errorf("span %v [%d,%d) = %q", pdf.HasSpan, pdf.SpanStart, pdf.SpanEnd, treeMessage[pdf.SpanStart:pdf.SpanEnd])
	}
	if !rel.HasSpan || !strings.HasSuffix(treeMessage[rel.SpanStart:rel.SpanEnd], "--R--") {
		t.Errorf("container span = %q", treeMessage[rel.SpanStart:rel.SpanEnd])
	}
}

func firstData(b []*mail.Node) string {
	if len(b) == 0 {
		return ""
	}
	return string(b[0].Data)
}

func TestReadTree_embeddedIsALeafByDefault(t *testing.T) {
	tree, _ := readTree(t, treeMessage, nil)
	carrier := tree.Root.Children[4]
	if carrier.Role != mail.RoleMessage || len(carrier.Children) != 0 || !strings.HasPrefix(string(carrier.Data), "Subject: fwd\r\n") {
		t.Errorf("carrier role %v, %d children, data %q", carrier.Role, len(carrier.Children), carrier.Data)
	}
}

func TestReadTree_budgetsAndHook(t *testing.T) {
	tree, _ := readTree(t, treeMessage, &mail.TreeOptions{MaxBodyBytes: 4})
	pdf := tree.Root.Children[1]
	if string(pdf.Data) != "PDFD" || pdf.Size != 7 || !pdf.Truncated {
		t.Errorf("data %q size %d truncated %v", pdf.Data, pdf.Size, pdf.Truncated)
	}
	tree, _ = readTree(t, treeMessage, &mail.TreeOptions{MaxBodyBytes: -1})
	if pdf := tree.Root.Children[1]; len(pdf.Data) != 0 || pdf.Size != 7 {
		t.Errorf("keep nothing: data %q size %d", pdf.Data, pdf.Size)
	}
	tree, _ = readTree(t, treeMessage, &mail.TreeOptions{MaxTotalBytes: 10})
	var kept int
	tree.Root.Walk(func(n *mail.Node) bool { kept += len(n.Data); return true })
	if kept > 10 {
		t.Errorf("%d bytes kept, want at most 10", kept)
	}

	var seen []string
	tree, _ = readTree(t, treeMessage, &mail.TreeOptions{Body: func(n *mail.Node, r io.Reader) error {
		head := make([]byte, 3)
		m, _ := io.ReadFull(r, head)
		seen = append(seen, string(head[:m]))
		return nil
	}})
	if pdf := tree.Root.Children[1]; len(pdf.Data) != 0 || pdf.Size != 7 {
		t.Errorf("hook: data %q size %d, want nothing kept and the whole size", pdf.Data, pdf.Size)
	}
	if len(seen) != 8 || seen[4] != "PDF" {
		t.Errorf("hook saw %q", seen)
	}
}

func TestReadTree_shapes(t *testing.T) {
	signed := "Content-Type: multipart/signed; boundary=S; protocol=\"application/pgp-signature\"; micalg=pgp-sha256\r\n\r\n" +
		"--S\r\nContent-Type: text/plain\r\n\r\nsigned text\r\n" +
		"--S\r\nContent-Type: application/pgp-signature\r\n\r\nSIG\r\n--S--\r\n"
	tree, _ := readTree(t, signed, nil)
	if got := outline(tree.Root); !reflect.DeepEqual(got, []string{"[] multipart/signed container", "[0] text/plain body", "[1] application/pgp-signature signature"}) {
		t.Errorf("signed: %q", got)
	}
	if len(tree.Diagnostics) != 0 {
		t.Errorf("signed: diagnostics %v", tree.Diagnostics)
	}
	// What a verifier hashes: the first part exactly as sent.
	first := tree.Root.Children[0]
	if !first.HasSpan || signed[first.SpanStart:first.SpanEnd] != "Content-Type: text/plain\r\n\r\nsigned text" {
		t.Errorf("signed part span %q", signed[first.SpanStart:first.SpanEnd])
	}
	if b := tree.Root.Bodies(); len(b) != 1 || string(b[0].Data) != "signed text" {
		t.Errorf("signed body %q", firstData(b))
	}

	for name, tc := range map[string]struct {
		text string
		code message.Code
	}{
		"signed with one part":       {"Content-Type: multipart/signed; boundary=S; protocol=\"application/pgp-signature\"\r\n\r\n--S\r\n\r\nx\r\n--S--\r\n", message.CodeSignedStructureInvalid},
		"signed protocol mismatch":   {"Content-Type: multipart/signed; boundary=S; protocol=\"application/pkcs7-signature\"\r\n\r\n--S\r\n\r\nx\r\n--S\r\nContent-Type: application/pgp-signature\r\n\r\ns\r\n--S--\r\n", message.CodeSignedStructureInvalid},
		"encrypted with three parts": {"Content-Type: multipart/encrypted; boundary=S; protocol=\"application/pgp-encrypted\"\r\n\r\n--S\r\nContent-Type: application/pgp-encrypted\r\n\r\nVersion: 1\r\n--S\r\nContent-Type: application/octet-stream\r\n\r\nx\r\n--S\r\n\r\ny\r\n--S--\r\n", message.CodeEncryptedStructureInvalid},
		"report with one part":       {"Content-Type: multipart/report; boundary=S; report-type=delivery-status\r\n\r\n--S\r\n\r\nx\r\n--S--\r\n", message.CodeReportInvalid},
		"partial":                    {"Content-Type: message/partial; id=\"a@b\"; number=1; total=2\r\n\r\nSubject: x\r\n\r\nfragment", message.CodeFeatureUnsupportedMediaType},
		"external body":              {"Content-Type: message/external-body; access-type=URL; URL=\"http://example.com/x\"\r\n\r\nContent-Type: image/png\r\n\r\n", message.CodeFeatureUnsupportedMediaType},
	} {
		tree, _ := readTree(t, tc.text, nil)
		if len(tree.Diagnostics) != 1 || tree.Diagnostics[0].Code != tc.code {
			t.Errorf("%s: diagnostics %v, want %s", name, tree.Diagnostics, tc.code)
		}
	}
}

func TestReadTree_report(t *testing.T) {
	const text = "Content-Type: multipart/report; boundary=S; report-type=delivery-status\r\n\r\n" +
		"--S\r\nContent-Type: text/plain\r\n\r\nYour message could not be delivered.\r\n" +
		"--S\r\nContent-Type: message/delivery-status\r\n\r\n" +
		"Reporting-MTA: dns; mx.example.com\r\n\r\n" +
		"Final-Recipient: rfc822; a@example.com\r\nAction: failed\r\nStatus: 5.1.1\r\n\r\n" +
		"Final-Recipient: rfc822; b@example.com\r\nAction: delayed\r\nStatus: 4.2.2\r\n" +
		"--S\r\nContent-Type: text/rfc822-headers\r\n\r\nSubject: original\r\n--S--\r\n"
	tree, _ := readTree(t, text, nil)
	want := []string{"[] multipart/report container", "[0] text/plain body", "[1] message/delivery-status report", "[2] text/rfc822-headers message"}
	if got := outline(tree.Root); !reflect.DeepEqual(got, want) || len(tree.Diagnostics) != 0 {
		t.Fatalf("outline %q, diagnostics %v", got, tree.Diagnostics)
	}
	groups := tree.Root.Children[1].ReportFields()
	if len(groups) != 3 {
		t.Fatalf("%d groups, want 3", len(groups))
	}
	if groups[0].Get("Reporting-MTA") != "dns; mx.example.com" || groups[1].Get("Status") != "5.1.1" || groups[2].Get("Action") != "delayed" {
		t.Errorf("groups %v", groups)
	}
	if b := tree.Root.Bodies(); len(b) != 1 || !strings.HasPrefix(string(b[0].Data), "Your message") {
		t.Errorf("report body %q", firstData(b))
	}
}

// TestReadTree_damaged pins that a damaged message still yields its whole
// tree: a part with a damaged header, an unclosed inner multipart and an
// unknown transfer encoding each stay one node's concern.
func TestReadTree_damaged(t *testing.T) {
	const text = "Content-Type: multipart/mixed; boundary=M\r\n\r\n" +
		"--M\r\nContent-Type: text/plain\r\nno separator\r\n" +
		"--M\r\nContent-Type: multipart/alternative; boundary=A\r\n\r\n--A\r\n\r\ninner\r\n" +
		"--M\r\nContent-Transfer-Encoding: x-odd\r\n\r\nraw\r\n" +
		"--M\r\nContent-Type: application/pdf\r\nContent-Transfer-Encoding: base64\r\n\r\nUERG!!RkRBVEE\r\n"
	tree, s := readTree(t, text, nil)
	want := []string{
		"[] multipart/mixed container",
		"[0] text/plain body",
		"[1] multipart/alternative container",
		"[1 0] text/plain body",
		"[2] text/plain body",
		"[3] application/pdf attachment",
	}
	if got := outline(tree.Root); !reflect.DeepEqual(got, want) {
		t.Errorf("outline %q\nwant %q", got, want)
	}
	if n := tree.Root.Children[2]; !message.IsUnknownEncoding(n.Warning) || string(n.Data) != "raw" {
		t.Errorf("unknown encoding node: warning %v, data %q", n.Warning, n.Data)
	}
	if n := tree.Root.Children[3]; string(n.Data) != "PDFFDATA" {
		t.Errorf("damaged base64 node: %q", n.Data)
	}
	if s.Len() == 0 {
		t.Error("no diagnostics for a damaged message")
	}
}
