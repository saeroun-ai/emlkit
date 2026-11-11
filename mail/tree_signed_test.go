package mail_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

const (
	sgPGPParams   = `; protocol="application/pgp-signature"; micalg=pgp-sha256`
	sgSMIMEParams = `; protocol="application/pkcs7-signature"; micalg=sha-256`
	sgCleanPart   = "Content-Type: text/plain\r\n\r\nsigned text"
)

// sgPGP is a PGP/MIME message: multipart/signed at the root.
func sgPGP(params, signed string) string {
	return "Content-Type: multipart/signed; boundary=S" + params + "\r\n\r\n" +
		"--S\r\n" + signed + "\r\n" +
		"--S\r\nContent-Type: application/pgp-signature\r\n\r\n" +
		"-----BEGIN PGP SIGNATURE-----\r\nc2ln\r\n-----END PGP SIGNATURE-----\r\n--S--\r\n"
}

// sgSMIME is an S/MIME message whose multipart/signed is the first part of a
// multipart/mixed.
func sgSMIME(params, signed, sig string) string {
	return "From: a@example.com\r\nContent-Type: multipart/mixed; boundary=M\r\n\r\n" +
		"--M\r\nContent-Type: multipart/signed; boundary=S" + params + "\r\n\r\n" +
		"--S\r\n" + signed + "\r\n" +
		"--S\r\nContent-Type: application/pkcs7-signature; name=smime.p7s\r\nContent-Transfer-Encoding: base64\r\n\r\n" + sig + "\r\n" +
		"--S--\r\n--M--\r\n"
}

func TestReadTree_signedRepairs(t *testing.T) {
	const unclosed = "Content-Type: multipart/alternative; boundary=A\r\n\r\n" +
		"--A\r\nContent-Type: text/plain\r\n\r\nplain\r\n--A\r\nContent-Type: text/html\r\n\r\n<p>html</p>"
	discard := &message.ParseOptions{
		DiagnosticSink:              message.DiagnosticSinkFunc(func(*message.ParseSession, message.Diagnostic) error { return nil }),
		DiscardDeliveredDiagnostics: true,
	}
	for _, tc := range []struct {
		name   string
		text   string
		signed string // the signed part as sent
		at     []int  // the multipart/signed
		opts   *message.ParseOptions
		reason string // empty: verifiable
	}{
		{"pgp clean", sgPGP(sgPGPParams, sgCleanPart), sgCleanPart, nil, nil, ""},
		{"smime clean", sgSMIME(sgSMIMEParams, sgCleanPart, "MIIBsig="), sgCleanPart, []int{0}, nil, ""},
		{"pgp damaged header in the signed part",
			sgPGP(sgPGPParams, "Content-Type: text/plain\r\nthis line has no colon\r\nContent-Language: en\r\n\r\nsigned text"),
			"Content-Type: text/plain\r\nthis line has no colon\r\nContent-Language: en\r\n\r\nsigned text", nil, nil,
			"the signed content was repaired, first at part 0 (header.missing_colon)"},
		{"smime unclosed inner multipart", sgSMIME(sgSMIMEParams, unclosed, "MIIBsig="), unclosed, []int{0}, nil,
			"the signed content was repaired, first at part 0.0 (multipart.close_not_found)"},
		{"smime damaged base64 in the signed part",
			sgSMIME(sgSMIMEParams, "Content-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\nUERG!!RkRBVEE", "MIIBsig="),
			"Content-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\nUERG!!RkRBVEE", []int{0}, nil,
			"the signed content was repaired, first at part 0.0 (cte.base64_junk, cte.base64_padding_missing)"},
		{"smime damaged signature part", sgSMIME(sgSMIMEParams, sgCleanPart, "MIIB!!sig="), sgCleanPart, []int{0}, nil,
			"the signature part was repaired, first at part 0.1 (cte.base64_junk)"},
		{"pgp missing protocol", sgPGP("; micalg=pgp-sha256", sgCleanPart), sgCleanPart, nil, nil, "it declares no protocol"},
		{"pgp missing micalg", sgPGP(`; protocol="application/pgp-signature"`, sgCleanPart), sgCleanPart, nil, nil, "it declares no micalg"},
		{"smime micalg of pgp", sgSMIME(`; protocol="application/pkcs7-signature"; micalg=pgp-sha256`, sgCleanPart, "MIIBsig="), sgCleanPart, []int{0}, nil,
			`its micalg "pgp-sha256" does not fit the protocol "application/pkcs7-signature"`},
		{"pgp clean, diagnostics discarded", sgPGP(sgPGPParams, sgCleanPart), sgCleanPart, nil, discard, ""},
		{"pgp damaged header, diagnostics discarded",
			sgPGP(sgPGPParams, "Content-Type: text/plain\r\nthis line has no colon\r\nContent-Language: en\r\n\r\nsigned text"),
			"Content-Type: text/plain\r\nthis line has no colon\r\nContent-Language: en\r\n\r\nsigned text", nil, discard,
			"the parse did not keep every diagnostic"},
	} {
		s, e, err := message.Parse(strings.NewReader(tc.text), tc.opts)
		if e == nil {
			t.Fatalf("%s: Parse() = %v", tc.name, err)
		}
		tree, err := mail.ReadTree(e, nil)
		if err != nil {
			t.Fatalf("%s: ReadTree() = %v", tc.name, err)
		}
		sn := sgChild(tree.Root, tc.at)
		if sn == nil || sn.MediaType != "multipart/signed" || len(sn.Children) != 2 {
			t.Fatalf("%s: no multipart/signed at %v: %q", tc.name, tc.at, outline(tree.Root))
		}
		first := sn.Children[0]
		// The raw representation is kept whatever was repaired.
		if !first.HasSpan || tc.text[first.SpanStart:first.SpanEnd] != tc.signed {
			t.Errorf("%s: signed span %v %q", tc.name, first.HasSpan, tc.text[first.SpanStart:first.SpanEnd])
		}
		first.Walk(func(n *mail.Node) bool {
			if n.Signed != sn {
				t.Errorf("%s: node %v is signed by %v", tc.name, n.Path, n.Signed)
			}
			return true
		})
		if sn.Signed != nil || sn.Children[1].Signed != nil {
			t.Errorf("%s: container or signature part marked as signed content", tc.name)
		}
		var noted bool
		for _, d := range tree.Diagnostics {
			if d.Code == mail.CodeSignedUnverifiable && reflect.DeepEqual(d.Path, sn.Path) && d.Severity == message.SeverityInfo {
				noted = true
			}
		}
		if want := tc.reason != ""; sn.Unverifiable != want || noted != want || !strings.Contains(sn.UnverifiableReason, tc.reason) {
			t.Errorf("%s: unverifiable %v (noted %v) %q, want %v %q\nparse: %v", tc.name, sn.Unverifiable, noted, sn.UnverifiableReason, want, tc.reason, s.Diagnostics())
		}
	}

	// Nested: S/MIME signed content inside PGP/MIME signed content. A repair
	// in the inner content leaves both unverifiable; the inner signature part
	// is the outer's content.
	inner := "Content-Type: multipart/signed; boundary=T" + sgSMIMEParams + "\r\n\r\n" +
		"--T\r\nContent-Type: text/plain\r\nno colon here\r\nContent-Language: en\r\n\r\ninner\r\n" +
		"--T\r\nContent-Type: application/pkcs7-signature\r\n\r\nMIIB\r\n--T--"
	tree, _ := readTree(t, sgPGP(sgPGPParams, inner), nil)
	outer := tree.Root
	in := outer.Children[0]
	if len(in.Children) != 2 {
		t.Fatalf("nested: %q", outline(outer))
	}
	if in.Signed != outer || in.Children[0].Signed != in || in.Children[1].Signed != outer {
		t.Errorf("nested: signed by %v, %v, %v", in.Signed, in.Children[0].Signed, in.Children[1].Signed)
	}
	if !in.Unverifiable || !outer.Unverifiable || !strings.Contains(outer.UnverifiableReason, "first at part 0.0 (header.missing_colon)") {
		t.Errorf("nested: inner %v %q, outer %v %q", in.Unverifiable, in.UnverifiableReason, outer.Unverifiable, outer.UnverifiableReason)
	}
}

func sgChild(n *mail.Node, path []int) *mail.Node {
	for _, i := range path {
		if n == nil || i >= len(n.Children) {
			return nil
		}
		n = n.Children[i]
	}
	return n
}

func TestReadTree_dkimView(t *testing.T) {
	const sig = "DKIM-Signature: v=1; a=rsa-sha256; d=example.com; s=sel; h=from:subject; bh=AAAA; b=BBBB\r\n"
	for _, tc := range []struct {
		name         string
		text         string
		header, body bool
		fields       []string
	}{
		{"clean", sig + "From: a@example.com\r\nSubject: hi\r\n\r\nbody\r\n", false, false, nil},
		{"damaged header", sig + "From: a@example.com\r\nthis line has no colon\r\nFrom: b@example.net\r\nSubject: hi\r\n\r\nbody\r\n", true, false, []string{"From"}},
		{"unclosed body", sig + "From: a@example.com\r\nContent-Type: multipart/mixed; boundary=M\r\n\r\n--M\r\nContent-Type: text/plain\r\n\r\nx\r\n", false, true, nil},
	} {
		tree, s := readTree(t, tc.text, nil)
		var noted bool
		for _, d := range tree.Diagnostics {
			noted = noted || d.Code == mail.CodeDKIMViewRepaired && d.Field == "DKIM-Signature"
		}
		if tree.HeaderRepaired != tc.header || tree.BodyRepaired != tc.body || !reflect.DeepEqual(tree.HeaderRepairFields, tc.fields) ||
			noted != (tc.header || tc.body) || tree.RepairsUnknown {
			t.Errorf("%s: header %v %q body %v noted %v unknown %v, want %v %q %v\nparse: %v",
				tc.name, tree.HeaderRepaired, tree.HeaderRepairFields, tree.BodyRepaired, noted, tree.RepairsUnknown,
				tc.header, tc.fields, tc.body, s.Diagnostics())
		}
		if tree.Root.Repaired != (tc.header || tc.body) {
			t.Errorf("%s: root repaired %v %v", tc.name, tree.Root.Repaired, tree.Root.RepairCodes)
		}
	}
}
