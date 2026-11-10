package mail_test

import (
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"testing"

	"golang.org/x/text/encoding/korean"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// ROB-050-003 to ROB-050-010 through the mail layer: the filename of an
// attachment as real clients write it, broken or not, and the parts a message
// with damaged MIME metadata still yields.

// withKoreanCharsets installs a CharsetReader that decodes EUC-KR under the
// labels Korean mail uses for it, for the duration of the test.
func withKoreanCharsets(t *testing.T) {
	t.Helper()
	old := message.CharsetReader
	t.Cleanup(func() { message.CharsetReader = old })
	message.CharsetReader = func(cs string, in io.Reader) (io.Reader, error) {
		switch cs {
		case "euc-kr", "ks_c_5601-1987", "cp949":
			return korean.EUCKR.NewDecoder().Reader(in), nil
		}
		return nil, fmt.Errorf("no decoder for %q", cs)
	}
}

func eucKR(t *testing.T, s string) string {
	t.Helper()
	b, err := korean.EUCKR.NewEncoder().String(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func percent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&b, "%%%02X", s[i])
	}
	return b.String()
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestAttachmentHeader_Filename_healed(t *testing.T) {
	withKoreanCharsets(t)
	const name = "한글 보고서.hwp"
	raw := eucKR(t, name)
	for _, tc := range []struct {
		name        string
		contentType string
		disposition string
		want        string
	}{
		{name: "Apple Mail: RFC 2231 in UTF-8, name as an encoded word",
			contentType: `application/x-hwp; x-unix-mode=0644; name="=?utf-8?B?` + b64(name) + `?="`,
			disposition: "attachment; filename*=utf-8''" + percent(name),
			want:        name},
		{name: "Thunderbird: a continued series, each segment encoded",
			contentType: "application/x-hwp; name*0*=UTF-8''" + percent(name[:6]) + "; name*1*=" + percent(name[6:]),
			disposition: "attachment; filename*0*=UTF-8''" + percent(name[:6]) + "; filename*1*=" + percent(name[6:]),
			want:        name},
		{name: "a series that arrives out of order",
			disposition: "attachment; filename*1*=" + percent(name[6:]) + "; filename*0*=UTF-8''" + percent(name[:6]),
			want:        name},
		{name: "Outlook: encoded words in ks_c_5601-1987 in both fields",
			contentType: `application/octet-stream; name="=?ks_c_5601-1987?B?` + b64(raw) + `?="`,
			disposition: `attachment; filename="=?ks_c_5601-1987?B?` + b64(raw) + `?="`,
			want:        name},
		{name: "Gmail: an encoded word split in two",
			disposition: `attachment; filename="=?UTF-8?B?` + b64(name[:6]) + `?= =?UTF-8?B?` + b64(name[6:]) + `?="`,
			want:        name},
		{name: "a web mailer: RFC 2231 in EUC-KR",
			disposition: "attachment; filename*=EUC-KR''" + percent(raw),
			want:        name},
		{name: "a web mailer: raw UTF-8 between quotes",
			contentType: `application/x-hwp; name="` + name + `"`,
			disposition: `attachment; filename="` + name + `"`,
			want:        name},
		{name: "an old Korean client: raw EUC-KR beside the charset of the part",
			contentType: `text/plain; charset=euc-kr; name="` + raw + `"`,
			disposition: `attachment; filename="` + raw + `"`,
			want:        name},
		{name: "raw EUC-KR and nothing that says so stays as it is",
			contentType: "application/octet-stream",
			disposition: `attachment; filename="` + raw + `"`,
			want:        raw},
		{name: "an unquoted name with spaces",
			disposition: "attachment; filename=annual report 2026.pdf; size=3",
			want:        "annual report 2026.pdf"},
		{name: "an unquoted encoded word",
			disposition: "attachment; filename==?UTF-8?B?" + b64(name) + "?=",
			want:        name},
		{name: "a missing semicolon after the disposition",
			disposition: `attachment filename="a.pdf"`,
			want:        "a.pdf"},
		{name: "a quote that is never closed",
			disposition: `attachment; filename="a.pdf`,
			want:        "a.pdf"},
		{name: "an unescaped Windows path",
			disposition: `attachment; filename="C:\Users\kim\문서\a.pdf"`,
			want:        `C:\Users\kim\문서\a.pdf`},
		{name: "a broken parameter beside the filename",
			disposition: `attachment; filename="a;b.pdf"; broken; size=`,
			want:        "a;b.pdf"},
		{name: "only the name of a broken content type",
			contentType: "application/pdf name=a b.pdf",
			want:        "a b.pdf"},
		{name: "an undecodable filename yields to the name",
			contentType: "application/pdf; name=fallback.pdf",
			disposition: "attachment; filename*=x-no-such-charset''%C7%D1.pdf",
			want:        "fallback.pdf"},
		{name: "a series with a gap keeps what is there",
			disposition: `attachment; filename*0="part one, "; filename*2="part three.pdf"`,
			want:        "part one, part three.pdf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h mail.AttachmentHeader
			if tc.contentType != "" {
				h.Set("Content-Type", tc.contentType)
			}
			if tc.disposition != "" {
				h.Set("Content-Disposition", tc.disposition)
			}
			got, err := h.Filename()
			if err != nil || got != tc.want {
				t.Errorf("Filename() = (%q, %v), want %q", got, err, tc.want)
			}
			// The candidates are reachable from the mail header, the chosen one
			// first, and the header itself is untouched.
			if names := h.FilenameCandidates(); len(names) == 0 || names[0].Filename != tc.want {
				t.Errorf("FilenameCandidates() = %+v, want %q first", names, tc.want)
			}
			if got := h.Get("Content-Disposition"); got != tc.disposition {
				t.Errorf("Content-Disposition is now %q", got)
			}

			var inline mail.InlineAttachmentHeader
			inline.Header = h.Header
			if got, err := inline.Filename(); err != nil || got != tc.want {
				t.Errorf("inline Filename() = (%q, %v), want %q", got, err, tc.want)
			}
		})
	}
}

// TestReader_healedMetadata reads one message whose every part has damaged
// MIME metadata, through message.Parse and mail.Reader: each part is returned,
// classified and named, and what was repaired is in the session.
func TestReader_healedMetadata(t *testing.T) {
	withKoreanCharsets(t)
	const name = "한글.hwp"
	text := "Content-Type: multipart boundary=--=_b 1\r\n\r\n" +
		"----=_b 1\r\nContent-Type: text; charset=euc-kr\r\n\r\n" + eucKR(t, "본문") + "\r\n" +
		"----=_b 1\r\nContent-Type: /pdf; name=annual report.pdf\r\nContent-Disposition: attachment filename=\"annual report.pdf\r\n\r\npdf\r\n" +
		"----=_b 1\r\nContent-Type: application/x-hwp\r\nContent-Disposition: attachment; filename*1*=" + percent(eucKR(t, "글")) +
		".hwp; filename*0*=euc-kr''" + percent(eucKR(t, "한")) + "\r\n\r\nhwp\r\n" +
		"----=_b 1--\r\n"

	session, e, err := message.Parse(strings.NewReader(text), nil)
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	parts, err := classifyAll(t, mail.NewReader(e))
	if err != nil || len(parts) != 3 {
		t.Fatalf("parts = %+v, %v; want three", parts, err)
	}
	if parts[0].attachment || parts[0].body != "본문" || parts[0].err != nil {
		t.Errorf("first part = %+v, want the inline text decoded from EUC-KR", parts[0])
	}
	for i, want := range []string{"annual report.pdf", name} {
		p := parts[i+1]
		if !p.attachment || p.err != nil {
			t.Errorf("part %d = %+v, want an attachment", i+1, p)
			continue
		}
		if got, err := p.header.(*mail.AttachmentHeader).Filename(); err != nil || got != want {
			t.Errorf("part %d: Filename() = (%q, %v), want %q", i+1, got, err, want)
		}
	}
	if parts[1].body != "pdf" || parts[2].body != "hwp" {
		t.Errorf("attachment bodies = %q, %q", parts[1].body, parts[2].body)
	}

	seen := map[message.Code]bool{}
	for _, d := range session.Diagnostics() {
		seen[d.Code] = true
	}
	for _, code := range []message.Code{
		message.CodeMIMEContentTypeMissingSlash, message.CodeMIMEContentTypeMissingType,
		message.CodeMIMEParameterUnquotedValue, message.CodeMIMEDispositionInvalid, message.CodeMIMEParameterUnclosedQuote,
	} {
		if !seen[code] {
			t.Errorf("Diagnostics() = %v, want %v among them", codes(session.Diagnostics()), code)
		}
	}

	// Strict refuses each damaged part on its own and goes on.
	_, e, err = message.Parse(strings.NewReader(text), &message.ParseOptions{Profile: message.ProfileStrict})
	if e == nil || !message.IsFatal(err) || e.ParseStop() != nil {
		t.Errorf("strict Parse() = %v, stop %v; want the root refused on its own", err, e.ParseStop())
	}
}
