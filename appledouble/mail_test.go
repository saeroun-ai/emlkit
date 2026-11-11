package appledouble_test

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/appledouble"
	"github.com/saeroun-ai/emlkit/mail"
)

// wrap76 base64-encodes b in lines of 76 characters.
func wrap76(b []byte) string {
	s := base64.StdEncoding.EncodeToString(b)
	var sb strings.Builder
	for len(s) > 76 {
		sb.WriteString(s[:76] + "\r\n")
		s = s[76:]
	}
	sb.WriteString(s)
	return sb.String()
}

// TestReadTreeWiring parses a message laid out as Apple Mail sends an
// attachment with Finder metadata — a multipart/appledouble inside a
// multipart/mixed — and combines its two parts as the package documentation
// shows. The header part is synthetic: no real Apple Mail sample is
// available here.
func TestReadTreeWiring(t *testing.T) {
	pdf := []byte("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n1 0 obj\n<<>>\nendobj\n")
	msg := strings.Join([]string{
		"From: a@example.com",
		"To: b@example.com",
		"Subject: report",
		"Mime-Version: 1.0 (Mac OS X Mail 16.0 \\(3774.600.62\\))",
		`Content-Type: multipart/mixed; boundary="Apple-Mail=_A"`,
		"",
		"--Apple-Mail=_A",
		"Content-Type: text/plain; charset=us-ascii",
		"",
		"See attached.",
		"--Apple-Mail=_A",
		`Content-Type: multipart/appledouble; boundary="Apple-Mail=_B"`,
		"",
		"--Apple-Mail=_B",
		"Content-Transfer-Encoding: base64",
		`Content-Type: application/applefile; name="report.pdf"`,
		"Content-Disposition: attachment; filename=report.pdf",
		"",
		wrap76(appledouble.FixtureAppleMailHeader("report.pdf")),
		"--Apple-Mail=_B",
		"Content-Disposition: attachment; filename=report.pdf",
		`Content-Type: application/pdf; x-unix-mode=0644; name="report.pdf"`,
		"Content-Transfer-Encoding: base64",
		"",
		wrap76(pdf),
		"--Apple-Mail=_B--",
		"",
		"--Apple-Mail=_A--",
		"",
	}, "\r\n")
	_, root, err := message.Parse(strings.NewReader(msg), nil)
	if root == nil {
		t.Fatalf("Parse: %v", err)
	}
	tree, err := mail.ReadTree(root, nil)
	if err != nil {
		t.Fatalf("ReadTree: %v", err)
	}

	var got []*appledouble.Description
	tree.Root.Walk(func(n *mail.Node) bool {
		if appledouble.Recognize(n.MediaType) != appledouble.KindAppleDouble {
			return true
		}
		types := make([]string, len(n.Children))
		for i, c := range n.Children {
			types[i] = c.MediaType
		}
		h, d := appledouble.AppleDoubleParts(types)
		if h < 0 {
			return true
		}
		var data []byte
		if d >= 0 {
			data = n.Children[d].Data
		}
		desc, err := appledouble.Combine(n.Children[h].Data, data, nil)
		if err != nil {
			t.Errorf("Combine: %v", err)
		}
		got = append(got, desc)
		return true
	})
	if len(got) != 1 {
		t.Fatalf("%d descriptions", len(got))
	}
	desc := got[0]
	if desc.Name != "report.pdf" || desc.Type.String() != "PDF " || desc.Creator.String() != "prvw" ||
		!bytes.Equal(desc.Data, pdf) || len(desc.Issues) != 0 {
		t.Fatalf("%+v", desc)
	}
	if r := desc.File.Resource.Resources; len(r) != 1 || r[0].Name != "Note" {
		t.Fatalf("resources %+v", r)
	}
}
