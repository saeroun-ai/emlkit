package pgpmime_test

import (
	"bytes"
	"fmt"
	"os"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
	"github.com/saeroun-ai/emlkit/pgpmime"
)

// Example wires pgpmime to the tree of a parsed message: each multipart is
// checked, and a PGP/MIME signed one is prepared for a Verifier from the
// raw bytes of its first part.
func Example() {
	input, err := os.ReadFile("testdata/signed.eml")
	if err != nil {
		panic(err)
	}
	_, e, err := message.Parse(bytes.NewReader(input), nil)
	if err != nil {
		panic(err)
	}
	tree, err := mail.ReadTree(e, nil)
	if err != nil {
		panic(err)
	}
	part := func(n *mail.Node) pgpmime.Part {
		return pgpmime.Part{MediaType: n.MediaType, Params: n.Params,
			Disposition: n.Disposition, Filename: n.Filename, Head: n.Data}
	}
	tree.Root.Walk(func(n *mail.Node) bool {
		if len(n.Children) == 0 {
			if c := pgpmime.Classify(part(n)); c.Kind != pgpmime.KindNone {
				fmt.Println("part", n.Path, c.Kind, c.Armor)
			}
			return true
		}
		var children []pgpmime.Part
		for _, c := range n.Children {
			children = append(children, part(c))
		}
		st := pgpmime.CheckStructure(part(n), children)
		if st.Kind != pgpmime.KindSigned || st.Content < 0 || st.Signature < 0 {
			return true
		}
		signed, sig := n.Children[st.Content], n.Children[st.Signature]
		var raw []byte // nil, and so unverifiable, without a span
		if signed.HasSpan {
			raw = input[signed.SpanStart:signed.SpanEnd]
		}
		req, issues := pgpmime.DetachedRequest(raw, sig.Data, st.Micalg, nil)
		fmt.Println(st.Kind, st.Micalg, len(st.Issues), len(issues))
		if req != nil {
			fmt.Println("issuer", req.Signatures[0].IssuerKeyID, "signed bytes", len(req.Signed))
			// verifications, err := verifier.Verify(req)
		}
		return true
	})
	// Output:
	// signed pgp-sha256 0 0
	// issuer A2227DD0135091D2 signed bytes 126
	// part [1] signature [signature]
}
