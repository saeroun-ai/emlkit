package smime_test

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
	"github.com/saeroun-ai/emlkit/smime"
)

// These tests wire the package to the parser the way a consumer does; the
// parser itself knows nothing of S/MIME. inner and repaired are the functions
// docs/robustness-notes/smime.md shows.

// inner returns the message hidden in an S/MIME node, or nil, and the results
// of verifying a detached signature. raw is the input the tree was parsed
// from.
func inner(sess *message.ParseSession, raw []byte, n *mail.Node, depth int, opts *smime.VerifyOptions) (*mail.Tree, []smime.SignerResult) {
	if depth > 4 {
		return nil, nil // layers within layers: stop
	}
	c := smime.Classify(n.MediaType, n.Params, n.Filename, n.Data)
	switch c.Kind {
	case smime.KindSignedData, smime.KindCompressedData:
		m, err := smime.Parse(n.Data, nil)
		if err != nil || len(m.Content()) == 0 {
			return nil, nil
		}
		var res []smime.SignerResult
		if m.Signed != nil {
			res = m.Signed.Verify(nil, opts)
		}
		isess, e, err := message.Parse(bytes.NewReader(m.Content()), nil)
		if err != nil {
			return nil, res
		}
		t, _ := mail.ReadTree(e, nil)
		if t != nil && t.Root != nil {
			if deeper, _ := inner(isess, m.Content(), t.Root, depth+1, opts); deeper != nil {
				return deeper, res
			}
		}
		return t, res

	case smime.KindDetachedSignature:
		if len(n.Children) != 2 {
			return nil, nil
		}
		signed, sig := n.Children[0], n.Children[1]
		m, err := smime.Parse(sig.Data, nil)
		if err != nil || m.Signed == nil {
			return nil, nil
		}
		if !signed.HasSpan || signed.Err != nil || repaired(sess, signed) {
			// Unverifiable: the signed bytes cannot be identified.
			return nil, nil
		}
		return nil, m.Signed.Verify(raw[signed.SpanStart:signed.SpanEnd], opts)
	}
	return nil, nil
}

// repaired reports a parse diagnostic above info at or under the node.
func repaired(sess *message.ParseSession, n *mail.Node) bool {
	for _, d := range sess.Diagnostics() {
		if d.Severity > message.SeverityInfo && len(d.Path) >= len(n.Path) &&
			slices.Equal(d.Path[:len(n.Path)], n.Path) {
			return true
		}
	}
	return false
}

func readTree(t testing.TB, raw []byte) (*message.ParseSession, *mail.Tree) {
	t.Helper()
	sess, e, err := message.Parse(bytes.NewReader(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := mail.ReadTree(e, nil)
	if err != nil {
		t.Fatal(err)
	}
	return sess, tree
}

func testRoots(t testing.TB) *smime.VerifyOptions {
	t.Helper()
	b, err := os.ReadFile("testdata/ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	ca, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return &smime.VerifyOptions{X509: &x509.VerifyOptions{Roots: roots, CurrentTime: ca.NotBefore.Add(24 * time.Hour)}}
}

// A detached signature verifies over the raw span of the first part, and over
// nothing else.
func TestDetachedOverRawSpan(t *testing.T) {
	raw, err := os.ReadFile("testdata/detached-rsa.eml")
	if err != nil {
		t.Fatal(err)
	}
	sess, tree := readTree(t, raw)
	root := tree.Root
	c := smime.Classify(root.MediaType, root.Params, root.Filename, nil)
	if c.Kind != smime.KindDetachedSignature || c.Micalg != "sha-256" || len(root.Children) != 2 {
		t.Fatalf("classification %+v, %d children", c, len(root.Children))
	}
	signed, sig := root.Children[0], root.Children[1]
	if sc := smime.Classify(sig.MediaType, sig.Params, sig.Filename, sig.Data); sc.Kind != smime.KindSignature {
		t.Fatalf("signature part: %+v", sc)
	}
	if !signed.HasSpan || repaired(sess, signed) {
		t.Fatalf("the signed part has no usable raw span: %v", sess.Diagnostics())
	}
	span := raw[signed.SpanStart:signed.SpanEnd]
	want, _ := os.ReadFile("testdata/content.txt")
	if !bytes.Equal(span, want) {
		t.Fatalf("raw span is not what was signed:\n%q\n%q", span, want)
	}

	_, res := inner(sess, raw, root, 0, testRoots(t))
	if len(res) != 1 || res[0].Digest != smime.Passed || res[0].Signature != smime.Passed || res[0].Chain != smime.Passed {
		t.Errorf("raw span: %+v", res)
	}
	// The decoded body is not the signed bytes: the part's header is missing.
	m, err := smime.Parse(sig.Data, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := m.Signed.Verify(signed.Data, testRoots(t))[0]
	if r.Digest != smime.Failed || r.Signature != smime.Passed || r.ContentIntact() {
		t.Errorf("decoded body: digest %v signature %v", r.Digest, r.Signature)
	}
}

// An opaque-signed message hides its body in the CMS object; extracting it
// gives the parser the real message.
func TestOpaqueReparse(t *testing.T) {
	raw, err := os.ReadFile("testdata/opaque-rsa.eml")
	if err != nil {
		t.Fatal(err)
	}
	sess, tree := readTree(t, raw)
	root := tree.Root
	c := smime.Classify(root.MediaType, root.Params, root.Filename, root.Data)
	if c.Kind != smime.KindSignedData || c.Mismatch || c.Source != "smime-type" {
		t.Fatalf("classification %+v", c)
	}
	in, res := inner(sess, raw, root, 0, testRoots(t))
	if in == nil || in.Root.MediaType != "text/plain" || !strings.HasPrefix(string(in.Root.Data), "Hello from the inside") {
		t.Fatalf("inner message: %+v", in)
	}
	if len(res) != 1 || !res[0].ContentIntact() || res[0].Chain != smime.Passed {
		t.Errorf("verify: %+v", res)
	}
}

func ExampleExtractSignedContent() {
	raw, err := os.ReadFile("testdata/opaque-rsa.eml")
	if err != nil {
		panic(err)
	}
	_, outer, err := message.Parse(bytes.NewReader(raw), nil)
	if err != nil {
		panic(err)
	}
	mediaType, params, _ := outer.Header.ContentType()
	body, _ := io.ReadAll(io.LimitReader(outer.Body, 32<<20)) // transfer decoding applied

	c := smime.Classify(mediaType, params, "", body)
	fmt.Println("kind:", c.Kind)
	if c.Kind != smime.KindSignedData {
		return
	}
	content, info, issues, err := smime.ExtractSignedContent(body, nil)
	if err != nil {
		panic(err)
	}
	fmt.Println("issues:", len(issues), "signers:", len(info.Signers), "digest:", info.Signers[0].DigestAlgorithm)

	// The content is the real message's MIME entity: parse it like one.
	_, inner, err := message.Parse(bytes.NewReader(content), nil)
	if err != nil {
		panic(err)
	}
	innerType, _, _ := inner.Header.ContentType()
	text, _ := io.ReadAll(inner.Body)
	fmt.Println(innerType)
	fmt.Print(strings.ReplaceAll(string(text), "\r\n", "\n"))

	// Verification states facts; without roots it says nothing of trust.
	r := info.Verify(nil, nil)[0]
	fmt.Println("digest:", r.Digest, "signature:", r.Signature, "chain:", r.Chain)
	// Output:
	// kind: signed-data
	// issues: 0 signers: 1 digest: SHA-256
	// text/plain
	// Hello from the inside of an S/MIME blob.
	// Second line.
	// digest: passed signature: passed chain: not-checked
}
