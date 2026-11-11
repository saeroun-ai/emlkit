package mail_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// The native fuzz target of mail's structured-header parsers (ROB-130-001);
// docs/robustness-notes/fuzzing.md lists its invariants (ROB-130-002).

// fzFootprint measures how much a result holds: the bytes of its distinct
// strings and the elements of its slices and maps, followed through pointers
// and interfaces once each. A string shared by several fields counts once.
func fzFootprint(v any) int {
	w := fzWalker{ptrs: map[uintptr]bool{}, strs: map[[2]uintptr]bool{}}
	w.walk(reflect.ValueOf(v))
	return w.n
}

type fzWalker struct {
	ptrs map[uintptr]bool
	strs map[[2]uintptr]bool
	n    int
}

func (w *fzWalker) walk(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		s := v.String()
		if s == "" {
			return
		}
		k := [2]uintptr{uintptr(unsafe.Pointer(unsafe.StringData(s))), uintptr(len(s))}
		if !w.strs[k] {
			w.strs[k] = true
			w.n += len(s)
		}
	case reflect.Slice:
		if v.IsNil() {
			return
		}
		w.n += v.Len()
		for i := range v.Len() {
			w.walk(v.Index(i))
		}
	case reflect.Array:
		for i := range v.Len() {
			w.walk(v.Index(i))
		}
	case reflect.Map:
		w.n += v.Len()
		it := v.MapRange()
		for it.Next() {
			w.walk(it.Key())
			w.walk(it.Value())
		}
	case reflect.Pointer:
		if v.IsNil() || w.ptrs[v.Pointer()] {
			return
		}
		w.ptrs[v.Pointer()] = true
		w.walk(v.Elem())
	case reflect.Interface:
		if !v.IsNil() {
			w.walk(v.Elem())
		}
	case reflect.Struct:
		for i := range v.NumField() {
			w.walk(v.Field(i))
		}
	}
}

// fzCodes is every code the message package declares.
var fzCodes = sync.OnceValues(func() (map[message.Code]bool, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "../diagnostic_code.go", nil, 0)
	if err != nil {
		return nil, err
	}
	codes := map[message.Code]bool{}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			for _, val := range spec.(*ast.ValueSpec).Values {
				if lit, ok := val.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						return nil, err
					}
					codes[message.Code(s)] = true
				}
			}
		}
	}
	return codes, nil
})

// FuzzTolerantHeaderValues runs every tolerant structured-header parser of
// mail over an arbitrary value. Besides the invariants of
// TestTolerantAccessors_arbitraryBytes (no panic, raw text kept, entries are
// parts of the value, the entry and diagnostic caps, agreement with the strict
// parsers and the Header accessors), it checks that each parser is
// deterministic, holds no more than linearly much of what it was given, and
// reports only declared codes with a known severity, a message, no field, no
// location and no path; and that an Authentication-Results field is read
// within its caps, lower-cased where documented, and Trusted exactly as its
// options say.
func FuzzTolerantHeaderValues(f *testing.F) {
	for _, s := range []string{
		"Doe, John <john@example.org>, 김철수 <chulsoo@example.kr>",
		"=?UTF-8?B?7ZWc6riA?= <a@example.kr>, =?ISO-2022-JP?B?GyRCRnxLXDhsGyhC?= <b@example.jp>",
		"undisclosed-recipients:;, <@a,@b:user@example.org>, mailto:x@y",
		`"unclosed <a@b>, (comment <c@d>`,
		"Mon, 22 Jul 2019 13:57:29 -0500 (EST)",
		"2026년 11월 11일 (수) 오전 10:00 KST",
		"Thu, 1 Jan 26 25:61:00 +09:00",
		"<a@b> <c@d>, e@f <g@h",
		`Your message of "Mon" <id@host>`,
		"from mx.example.kr (mx.example.kr [192.0.2.1]) by mail.example.jp with ESMTPS id ABC for <u@example.jp>; Tue, 11 Nov 2026 09:00:00 +0900",
		"<mailto:list@example.org?subject=unsubscribe>, <https://example.org/u?a=1,2>, NO",
		"中文邮件列表 <list.example.cn>",
		`mx.example.org; spf=pass smtp.mailfrom=a@b.example; dkim=fail (bad sig) header.d=b.example header.s=s1; dmarc=none reason="no policy"`,
		"example.org 1; none",
		"\xc7\xd1\xb1\xdb <\xff@example.org>",
		strings.Repeat("(", 100) + "<a@b>",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		if len(v) > 8<<10 {
			return
		}
		checkTolerantInvariants(t, v)

		strict := &mail.AddressOptions{StrictCommas: true, RequireDomain: true}
		for _, p := range []struct {
			name string
			run  func() any
		}{
			{"ParseAddressListTolerant", func() any { return mail.ParseAddressListTolerant(v, nil) }},
			{"ParseAddressListTolerant with options", func() any { return mail.ParseAddressListTolerant(v, strict) }},
			{"ParseDateTolerant", func() any { return mail.ParseDateTolerant(v) }},
			{"ParseMsgIDListTolerant", func() any { return mail.ParseMsgIDListTolerant(v) }},
			{"ParseReceived", func() any { return mail.ParseReceived(v) }},
			{"ParseListURLs", func() any { return mail.ParseListURLs(v) }},
			{"ParseListID", func() any { return mail.ParseListID(v) }},
			{"ParseAuthenticationResults", func() any { return mail.ParseAuthenticationResults(v) }},
		} {
			a, b := p.run(), p.run()
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("%s(%q) is not deterministic:\n%+v\n%+v", p.name, v, a, b)
			}
			if fp := fzFootprint(a); fp > 32*len(v)+64<<10 {
				t.Fatalf("%s(%q): a result of footprint %d", p.name, v, fp)
			}
			diags := reflect.ValueOf(a).FieldByName("Diagnostics").Interface().([]message.Diagnostic)
			fzCheckDiagnostics(t, p.name, v, diags)
		}
		fzCheckAuthResults(t, v)
	})
}

func fzCheckDiagnostics(t *testing.T, name, v string, diags []message.Diagnostic) {
	t.Helper()
	codes, err := fzCodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(diags) > 64 {
		t.Fatalf("%s(%q): %d diagnostics", name, v, len(diags))
	}
	for _, d := range diags {
		if !codes[d.Code] || d.Severity > message.SeverityFatal || d.Message == "" {
			t.Fatalf("%s(%q): diagnostic %+v has an undeclared code, an unknown severity or no message", name, v, d)
		}
		if d.Location != (message.Location{}) || d.Path != nil {
			t.Fatalf("%s(%q): diagnostic %+v has a location or a path", name, v, d)
		}
		if d.Field != "" && !(name == "ParseAuthenticationResults" && d.Field == "Authentication-Results") {
			t.Fatalf("%s(%q): diagnostic %+v names a field", name, v, d)
		}
	}
}

func fzCheckAuthResults(t *testing.T, v string) {
	t.Helper()
	r := mail.ParseAuthenticationResults(v)
	if r.Raw != v || len(r.Methods) > 64 || r.Index != 0 || r.ReceivedAbove != 0 || r.Trusted {
		t.Fatalf("ParseAuthenticationResults(%q) = %+v", v, r)
	}
	for _, m := range r.Methods {
		if m.Method == "" || m.Method != strings.ToLower(m.Method) || m.Result != strings.ToLower(m.Result) || len(m.Props) > 256 {
			t.Fatalf("ParseAuthenticationResults(%q): method %+v", v, m)
		}
		for _, p := range m.Props {
			if p.Type != strings.ToLower(p.Type) || p.Name != strings.ToLower(p.Name) {
				t.Fatalf("ParseAuthenticationResults(%q): property %+v is not lower-cased", v, p)
			}
		}
	}

	var h mail.Header
	h.Add("Authentication-Results", v)
	h.Add("Received", "from a by b; Tue, 11 Nov 2026 09:00:00 +0900")
	if h.Get("Authentication-Results") != v {
		return // the header layer changed the value; nothing to compare
	}
	all := h.AuthenticationResults(nil)
	if len(all) != 1 || all[0].Index != 1 || all[0].ReceivedAbove != 1 || all[0].Trusted ||
		all[0].AuthServID != r.AuthServID || !reflect.DeepEqual(all[0].Methods, r.Methods) {
		t.Fatalf("AuthenticationResults over %q = %+v, ParseAuthenticationResults %+v", v, all, r)
	}
	trusted := h.AuthenticationResults(&mail.AuthResultsOptions{TrustedIDs: []string{" " + strings.ToUpper(r.AuthServID) + " "}, MaxReceivedAbove: 1})
	if trusted[0].Trusted != (r.AuthServID != "" && strings.EqualFold(strings.ToUpper(r.AuthServID), r.AuthServID)) {
		t.Fatalf("AuthenticationResults over %q trusted by its own identifier: %v", v, trusted[0].Trusted)
	}
	if below := h.AuthenticationResults(&mail.AuthResultsOptions{TrustedIDs: []string{r.AuthServID}}); below[0].Trusted {
		t.Fatalf("AuthenticationResults over %q trusted below a Received field with MaxReceivedAbove 0", v)
	}
}
