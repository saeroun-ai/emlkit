package message

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

// Native fuzz targets for the value parsers of the root package (ROB-130-001):
// the tolerant media-type and disposition readers and their parameter
// candidates, the filename candidates, SafeFilename, and the RFC 2047 decoder.
// The invariants they assert are ROB-130-002's, each where the documentation
// promises it; docs/robustness-notes/fuzzing.md lists them per target.
//
// Every target caps its input, so a run is bounded in time and memory however
// long the fuzzer grows a value.

// fzMaxValue is the longest value the value targets read. It is below the
// 64 KiB past which the session-less parameter readers stop decoding encoded
// words, so the whole reading is exercised.
const fzMaxValue = 8 << 10

// fzFootprint measures how much a result holds: the bytes of its distinct
// strings and the elements of its slices and maps, followed through pointers
// and interfaces once each. A string shared by several fields (the same bytes
// at the same address) counts once, so the measure grows with what a parser
// allocated, not with how often it refers to it.
func fzFootprint(vs ...any) int {
	w := fzWalker{ptrs: map[uintptr]bool{}, strs: map[[2]uintptr]bool{}}
	for _, v := range vs {
		w.walk(reflect.ValueOf(v))
	}
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
		if fzScalar(v.Type().Elem().Kind()) {
			return
		}
		for i := range v.Len() {
			w.walk(v.Index(i))
		}
	case reflect.Array:
		if fzScalar(v.Type().Elem().Kind()) {
			return
		}
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

func fzScalar(k reflect.Kind) bool {
	return k >= reflect.Bool && k <= reflect.Complex128
}

// fzBounded fails t when a result's footprint is more than a small constant
// times its input plus a constant: no parser here may allocate more than
// linearly in what it was given.
func fzBounded(t *testing.T, what string, in int, vs ...any) {
	t.Helper()
	if fp := fzFootprint(vs...); fp > 32*in+64<<10 {
		t.Fatalf("%s: a result of footprint %d from %d input bytes", what, fp, in)
	}
}

// fzCodes is every code diagnostic_code.go declares.
var fzCodes = sync.OnceValues(func() (map[Code]bool, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "diagnostic_code.go", nil, 0)
	if err != nil {
		return nil, err
	}
	codes := map[Code]bool{}
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
					codes[Code(s)] = true
				}
			}
		}
	}
	return codes, nil
})

// fzCheckDiagnostics holds diagnostics to what every list of them promises:
// at most max of them, each with a declared code, a known severity and a
// message.
func fzCheckDiagnostics(t *testing.T, what string, diags []Diagnostic, max int) {
	t.Helper()
	codes, err := fzCodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(diags) > max {
		t.Fatalf("%s: %d diagnostics, at most %d promised", what, len(diags), max)
	}
	for _, d := range diags {
		if !codes[d.Code] {
			t.Fatalf("%s: undeclared code %q", what, d.Code)
		}
		if d.Severity > SeverityFatal || d.Message == "" {
			t.Fatalf("%s: incomplete diagnostic %+v", what, d)
		}
	}
}

var fzParameterSeeds = [][2]string{
	{"text/plain; charset=utf-8", "attachment; filename=report.pdf"},
	{`text/plain; charset="euc-kr"; name="=?EUC-KR?B?x9Gx2yDGxMDP?="`, `attachment; filename="=?EUC-KR?B?x9Gx2yDGxMDP?="`},
	{"application/octet-stream; name*=UTF-8''%ED%95%9C%EA%B8%80%2E%74%78%74", "attachment; filename*0*=UTF-8''%E6%97%A5%E6%9C%AC; filename*1*=%E8%AA%9E%2E%64%6F%63"},
	{`multipart/mixed; boundary="B"; boundary="C"`, "inline"},
	{"text", "attachment filename=a b.pdf"},
	{"/html; charset=shift_jis", `attachment; filename="C:\Users\a\b.txt"`},
	{`text/plain; name="unclosed`, `attachment; filename="my "special" file.txt"`},
	{"image/jpeg; name=\xc7\xd1\xb1\xdb.jpg; charset=euc-kr", "attachment; filename=中文 文件.txt"},
	{"text/plain; charset=gb2312; name*=gb2312''%D6%D0%CE%C4.txt", "attachment; filename*=iso-2022-jp''%1B%24%42%46%7C%4B%5C%1B%28%42.txt"},
	{"; charset=x", "attachment; filename*2=c; filename*0=a; filename*1=b"},
	{"text/plain; a=1;;; b", "=?utf-8?B?7ZWc6riA?=; filename=../../etc/passwd"},
	{"multipart/related; boundary=----=_NextPart_000_0001; type=\"text/html\"", "attachment; filename=\"CON.txt\"; filename*=UTF-8''%E2%80%AEexe.txt"},
	{"@#!", "attachment; filename=%zz%41; filename*=x'y'%zz"},
	{"text/plain; name=日本語.txt; name=\"other.txt\"", "attachment; filename=\"\\\"; filename*=utf-8'ko'%ED%95%9C"},
}

// FuzzMIMEParameters reads a Content-Type and a Content-Disposition value
// tolerantly, alone and as a header, and holds the readings to their
// contracts: they are deterministic and linear in size; a Content-Type is
// always a type and a subtype; a field's diagnostics name it, carry no
// position and no code twice; a value is UTF-8 unless its candidate is marked
// Undecoded; the parameter map holds exactly the chosen candidates; every
// candidate's raw text is in the field; and the filename candidates come in
// the documented order, each with a safe name derived from it.
func FuzzMIMEParameters(f *testing.F) {
	for _, s := range fzParameterSeeds {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, ct, cd string) {
		if len(ct)+len(cd) > fzMaxValue {
			return
		}
		fzCheckParameterField(t, "Content-Type", ct)
		fzCheckParameterField(t, "Content-Disposition", cd)

		if strings.ContainsAny(ct+cd, "\r\n") {
			// A line break reframes a header; the standalone readers above
			// took the value whole.
			return
		}
		var h Header
		h.Set("Content-Type", ct)
		h.Set("Content-Disposition", cd)
		names := h.FilenameCandidates()
		if again := h.FilenameCandidates(); !reflect.DeepEqual(names, again) {
			t.Fatalf("FilenameCandidates is not deterministic:\n%+v\n%+v", names, again)
		}
		fzCheckFilenames(t, names)
		fzBounded(t, "FilenameCandidates", len(ct)+len(cd), names)
	})
}

func fzCheckParameterField(t *testing.T, field, v string) {
	t.Helper()
	read := ParseMediaTypeTolerant
	if field == "Content-Disposition" {
		read = ParseContentDispositionTolerant
	}
	base, params, diags := read(v)
	base2, params2, diags2 := read(v)
	if base != base2 || !reflect.DeepEqual(params, params2) || !reflect.DeepEqual(diags, diags2) {
		t.Fatalf("%s %q: two readings differ", field, v)
	}
	cands := ParseParameterCandidates(field, v)
	if again := ParseParameterCandidates(strings.ToUpper(field), v); !reflect.DeepEqual(cands, again) {
		t.Fatalf("%s %q: candidates depend on the case of the field name", field, v)
	}
	if ParseParameterCandidates("X-"+field, v) != nil {
		t.Fatalf("candidates of a field that is neither Content-Type nor Content-Disposition")
	}
	what := field + " " + strconv.Quote(v)

	if strings.TrimSpace(v) == "" {
		if params != nil || diags != nil || cands != nil {
			t.Fatalf("%s: an empty value read to %v, %v, %v", what, params, diags, cands)
		}
	}
	if field == "Content-Type" {
		typ, sub, ok := strings.Cut(base, "/")
		if !ok || typ == "" || sub == "" || strings.Contains(sub, "/") {
			t.Fatalf("%s: media type %q is not a type and a subtype", what, base)
		}
	}

	fzCheckDiagnostics(t, what, diags, 64)
	seen := map[Code]bool{}
	for _, d := range diags {
		if seen[d.Code] {
			t.Fatalf("%s: code %s recorded twice", what, d.Code)
		}
		seen[d.Code] = true
		if d.Field != field || d.Location.HasOffset || d.Path != nil {
			t.Fatalf("%s: diagnostic %+v is not about the field alone", what, d)
		}
	}

	chosen := map[string]int{}
	for _, c := range cands {
		if c.Name == "" || c.Name != strings.ToLower(c.Name) || strings.Contains(c.Name, "*") {
			t.Fatalf("%s: candidate name %q", what, c.Name)
		}
		if c.Form != ParameterContinued && !strings.Contains(v, c.Raw) {
			t.Fatalf("%s: raw text %q of a candidate is not in the field", what, c.Raw)
		}
		if c.Name != "boundary" && !c.Undecoded && !utf8.ValidString(c.Value) {
			t.Fatalf("%s: candidate %q holds %q, not UTF-8, and is not marked Undecoded", what, c.Name, c.Value)
		}
		if c.Chosen {
			chosen[c.Name]++
			if got, ok := params[c.Name]; !ok || got != c.Value {
				t.Fatalf("%s: chosen candidate %q = %q, the map holds %q", what, c.Name, c.Value, got)
			}
		}
	}
	for name, n := range chosen {
		if n > 1 {
			t.Fatalf("%s: %d chosen candidates for %q", what, n, name)
		}
	}
	for name := range params {
		if chosen[name] != 1 {
			t.Fatalf("%s: parameter %q is in the map with no chosen candidate", what, name)
		}
	}
	fzBounded(t, what, len(v), base, params, diags, cands)
}

// fzCheckFilenames holds filename candidates to the order FilenameCandidates
// documents and derives a safe name from each.
func fzCheckFilenames(t *testing.T, names []FilenameCandidate) {
	t.Helper()
	usable, nonEmpty := -1, -1
	for i, n := range names {
		if n.Filename != n.Parameter.Value {
			t.Fatalf("candidate %d: Filename %q, its parameter %q", i, n.Filename, n.Parameter.Value)
		}
		want, extended := "filename", n.Source == FilenameDispositionExtended || n.Source == FilenameContentTypeExtended
		switch n.Source {
		case FilenameDispositionExtended, FilenameDisposition:
		case FilenameContentTypeExtended, FilenameContentType:
			want = "name"
		default:
			t.Fatalf("candidate %d: source %v", i, n.Source)
		}
		if n.Parameter.Name != want || (n.Parameter.Form != ParameterPlain) != extended {
			t.Fatalf("candidate %d from %v is parameter %q in form %v", i, n.Source, n.Parameter.Name, n.Parameter.Form)
		}
		if i > 1 && names[i-1].Source > n.Source {
			t.Fatalf("candidates after the first are not in order of precedence: %v before %v", names[i-1].Source, n.Source)
		}
		if n.Filename != "" {
			if nonEmpty < 0 {
				nonEmpty = i
			}
			if usable < 0 && !n.Parameter.Undecoded {
				usable = i
			}
		}
		fzCheckSafeFilename(t, n.Filename)
	}
	if (usable >= 0 && usable != 0) || (usable < 0 && nonEmpty > 0) {
		t.Fatalf("the first candidate is not the one to use: %+v", names)
	}
}

// fzCheckSafeFilename holds SafeFilename(name) to its contract: one path
// component, never empty, never "." or "..", valid UTF-8 with no control,
// invisible or reserved character, no leading space and no trailing space or
// dot, no Windows device stem, at most 255 bytes, and idempotent.
func fzCheckSafeFilename(t *testing.T, name string) {
	t.Helper()
	s := SafeFilename(name)
	bad := func(why string) {
		t.Helper()
		t.Fatalf("SafeFilename(%q) = %q: %s", name, s, why)
	}
	switch {
	case s == "":
		bad("empty")
	case strings.Trim(s, ".") == "":
		bad("only dots")
	case strings.ContainsAny(s, `/\<>:"|?*`):
		bad("a separator or a reserved character")
	case !utf8.ValidString(s):
		bad("not UTF-8")
	case len(s) > maxSafeFilenameBytes:
		bad("longer than 255 bytes")
	case s[0] == ' ' || s[len(s)-1] == ' ' || s[len(s)-1] == '.':
		bad("a leading space, or a trailing space or dot")
	case isWindowsDeviceName(s):
		bad("a Windows device name")
	}
	for _, r := range s {
		if unicode.IsControl(r) || isInvisibleRune(r) || (unicode.IsSpace(r) && r != ' ') {
			bad("a control, invisible or non-space white space character")
		}
	}
	if again := SafeFilename(s); again != s {
		t.Fatalf("SafeFilename is not idempotent: %q, then %q, then %q", name, s, again)
	}
}

// FuzzSafeFilename holds SafeFilename to its contract over arbitrary names.
func FuzzSafeFilename(f *testing.F) {
	for _, s := range []string{
		"../../etc/passwd", `C:\Users\a\b.txt`, "CON", "con.txt ", "...", "", "\x00a\x01b",
		"\xe2\x80\xaetxt.exe", "한글 파일.hwp", "日本語.doc", "中文 文件.pdf", strings.Repeat("가", 100) + ".txt",
		"a<b>c:d\"e|f?g*h", "\xff\xfe", "COM¹.txt", "  .  ", "x\xe2\x80\x8b\xe2\x80\x8dy", "lpt9 .tar.gz",
		strings.Repeat(" ", 300) + "CON" + strings.Repeat(" ", 300) + ".txt",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if len(name) > fzMaxValue {
			return
		}
		fzCheckSafeFilename(t, name)
	})
}

// FuzzDecodeHeaderText decodes arbitrary header text and holds the result to
// what DecodeHeaderText promises: deterministic, valid UTF-8, no line break or
// NUL that the raw value did not hold, valid UTF-8 without encoded words kept
// as it is, at most three bytes out per byte in, at most 64 diagnostics with
// the 64th a limit.diagnostic_count, each placed within the value. The same
// value decoded with a fallback charset, for Header.Text and as a parameter
// value is held to the same bounds.
func FuzzDecodeHeaderText(f *testing.F) {
	for _, s := range []string{
		"=?UTF-8?B?7ZWc6riA?= =?UTF-8?B?7YWM7Iqk7Yq4?=",
		"=?ISO-2022-JP?B?GyRCRnxLXDhsGyhC?=",
		"=?GB2312?B?1tDOxA==?= and =?big5?B?wWPF6Q==?=",
		"=?EUC-KR?Q?=C7=D1=B1=DB?= =?ks_c_5601-1987?B?x9Gx2yDGxMDP?=",
		"abc=?utf-8?q?x?=def",
		"=?utf-8?B?broken base64!!?=",
		"=?utf-8?Q?a=ZZ_b?",
		`"=?utf-8?q?quoted?="`,
		"raw 한글 utf-8 and \xc7\xd1\xb1\xdb euc-kr bytes",
		"=?us-ascii?Q?=FF=FE?=",
		"=?utf-8?q?line=0D=0Abreak=00nul?=",
		"=?=?=?=?x?B?=?",
		"=?unknown-8bit?B?x9Gx2w==?=",
		"=?utf-8*ko?B?7ZWc?=",
		"=?iso-8859-1?q?caf=E9?= =?utf-8?b?7ZW?= =?utf-8?b?c6riA?=",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > fzMaxValue {
			return
		}
		out, diags := DecodeHeaderText(s)
		out2, diags2 := DecodeHeaderText(s)
		if out != out2 || !reflect.DeepEqual(diags, diags2) {
			t.Fatalf("DecodeHeaderText(%q) is not deterministic", s)
		}
		fzCheckHeaderText(t, "DecodeHeaderText", s, out, diags)
		if !strings.Contains(s, "=?") && utf8.ValidString(s) {
			if out != s {
				t.Fatalf("DecodeHeaderText(%q) = %q, changed UTF-8 text without encoded words", s, out)
			}
			if fzASCII(s) && len(diags) != 0 {
				t.Fatalf("DecodeHeaderText(%q): diagnostics %v for plain ASCII", s, diags)
			}
		}
		if text, _ := decodeHeaderText(s); text != out {
			t.Fatalf("Header.Text decodes %q to %q, DecodeHeaderText to %q", s, text, out)
		}

		fb, fbDiags := DecodeHeaderTextWithOptions(s, &HeaderTextOptions{FallbackCharset: "iso-8859-1"})
		fzCheckHeaderText(t, "DecodeHeaderTextWithOptions", s, fb, fbDiags)
		if !strings.Contains(s, "=?") && utf8.ValidString(s) && fb != s {
			t.Fatalf("a fallback charset converted valid UTF-8 %q to %q", s, fb)
		}

		var guard valueDecoder
		pv := decodeParameterValue(s, guard.charsetReader)
		if !strings.Contains(s, "=?") && pv != s {
			t.Fatalf("a parameter value without encoded words %q became %q", s, pv)
		}
		if len(pv) > 3*len(s)+16 {
			t.Fatalf("parameter value %q decoded to %d bytes", s, len(pv))
		}
	})
}

func fzCheckHeaderText(t *testing.T, what, s, out string, diags []Diagnostic) {
	t.Helper()
	what += "(" + strconv.Quote(s) + ")"
	if !utf8.ValidString(out) {
		t.Fatalf("%s = %q, not UTF-8", what, out)
	}
	if !strings.ContainsAny(s, "\r\n\x00") && strings.ContainsAny(out, "\r\n\x00") {
		t.Fatalf("%s = %q, a line break or NUL the value did not hold", what, out)
	}
	if len(out) > 3*len(s)+16 {
		t.Fatalf("%s: %d bytes out of %d", what, len(out), len(s))
	}
	fzCheckDiagnostics(t, what, diags, maxHeaderTextDiagnostics)
	if len(diags) == maxHeaderTextDiagnostics && diags[len(diags)-1].Code != CodeLimitDiagnosticCount {
		t.Fatalf("%s: the 64th diagnostic is %s", what, diags[len(diags)-1].Code)
	}
	for _, d := range diags {
		l := d.Location
		if d.Field != "" || (l.HasOffset && (l.Offset < 0 || l.Offset > int64(len(s)) || l.EndOffset > int64(len(s)))) {
			t.Fatalf("%s: diagnostic %+v is not placed within the value", what, d)
		}
	}
}

func fzASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
