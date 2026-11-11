package mail_test

import (
	"fmt"
	"sort"
	"strings"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// ExampleReadTree_adapter is the flow docs/spam-adapter-contract.md describes,
// on a small damaged message: a header line with no colon, junk inside a
// Base64 body, an unclosed quote in a filename and no closing delimiter.
//
// A deployment that reads legacy charsets also imports the charset plugin
// (import _ "github.com/saeroun-ai/emlkit/charset"); this message is UTF-8
// and needs none.
func ExampleReadTree_adapter() {
	raw := "From: Billing <billing@example.com>\r\n" +
		"To: user@example.org\r\n" +
		"X-Mailer PHPMailer 5.2\r\n" +
		"Subject: =?utf-8?Q?Invoice_=E2=82=AC120_overdue?=\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"b1\"\r\n" +
		"\r\n" +
		"--b1\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"UGF5IG5v!!dyBhdCBodHRwczovL3BheS5leGFtcGxlLmNvbS8=\r\n" +
		"--b1\r\n" +
		"Content-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename=\"invoice.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"JVBERi0xLjQK\r\n"

	// Parse heals and records; nil options are the tolerant profile and the
	// default Limits.
	session, root, err := message.Parse(strings.NewReader(raw), nil)
	if root == nil {
		fmt.Println("no message:", err)
		return
	}

	// ReadTree reads every body to its end, so the verdict is final after it.
	tree, err := mail.ReadTree(root, &mail.TreeOptions{MaxBodyBytes: 256 << 10})
	if err != nil {
		fmt.Println("stopped:", err)
	}

	c := session.Completion()
	fmt.Println("completion:", c.State, "clean:", c.Clean())

	// Branch on codes and severities, never on messages.
	var codes []string
	for _, d := range session.Diagnostics() {
		codes = append(codes, fmt.Sprintf("%s (%s)", d.Code, d.Severity))
	}
	sort.Strings(codes)
	for _, code := range codes {
		fmt.Println("diagnostic:", code)
	}
	fmt.Println("header repaired:", tree.HeaderRepaired, "body repaired:", tree.BodyRepaired)

	tree.Root.Walk(func(n *mail.Node) bool {
		if len(n.Children) == 0 {
			fmt.Printf("leaf %v: %s %s, %d bytes", n.Path, n.Role, n.MediaType, n.Size)
			if n.Filename != "" {
				fmt.Printf(", file %q", message.SafeFilename(n.Filename))
			}
			fmt.Println()
		}
		return true
	})

	h := mail.Header{Header: tree.Root.Header}
	subject, _ := h.TextDiagnostics("Subject")
	fmt.Println("subject:", subject)

	// Output:
	// completion: incomplete clean: false
	// diagnostic: cte.base64_junk (recovered)
	// diagnostic: header.missing_colon (recovered)
	// diagnostic: mime.parameter_unclosed_quote (recovered)
	// diagnostic: multipart.close_not_found (incomplete)
	// header repaired: true body repaired: true
	// leaf [0]: body text/plain, 35 bytes
	// leaf [1]: attachment application/pdf, 9 bytes, file "invoice.pdf"
	// subject: Invoice €120 overdue
}
