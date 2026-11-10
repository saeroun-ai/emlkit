package message

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// ROB-050-009 and ROB-050-010: filename candidates, and the safe name derived
// from one.

type wantFilename struct {
	source FilenameSource
	name   string
}

func filenamesOf(cands []FilenameCandidate) []wantFilename {
	var out []wantFilename
	for _, c := range cands {
		out = append(out, wantFilename{c.Source, c.Filename})
	}
	return out
}

func TestFilenameCandidates(t *testing.T) {
	withKoreanCharsets(t)
	hangul := eucKR(t, "한글")
	for _, tc := range []struct {
		name        string
		contentType string
		disposition string
		want        []wantFilename
		conflict    bool
	}{
		{name: "no filename anywhere", contentType: "text/plain; charset=utf-8", disposition: "attachment; size=3"},
		{name: "no header at all"},
		{name: "disposition filename alone", disposition: "attachment; filename=a.txt",
			want: []wantFilename{{FilenameDisposition, "a.txt"}}},
		{name: "content type name alone", contentType: "application/pdf; name=a.pdf",
			want: []wantFilename{{FilenameContentType, "a.pdf"}}},
		{name: "all four sources agree",
			contentType: "application/pdf; name=a.pdf; name*=utf-8''a.pdf",
			disposition: "attachment; filename=a.pdf; filename*=utf-8''a.pdf",
			want: []wantFilename{
				{FilenameDispositionExtended, "a.pdf"}, {FilenameDisposition, "a.pdf"},
				{FilenameContentTypeExtended, "a.pdf"}, {FilenameContentType, "a.pdf"},
			}},
		{name: "all four sources differ, in order of precedence", conflict: true,
			contentType: "application/pdf; name=d.pdf; name*=utf-8''c.pdf",
			disposition: "attachment; filename=b.pdf; filename*=utf-8''a.pdf",
			want: []wantFilename{
				{FilenameDispositionExtended, "a.pdf"}, {FilenameDisposition, "b.pdf"},
				{FilenameContentTypeExtended, "c.pdf"}, {FilenameContentType, "d.pdf"},
			}},
		{name: "ASCII fallback beside the extended form", conflict: true,
			disposition: "attachment; filename=\"hangul.txt\"; filename*=UTF-8''" + percent("한글.txt"),
			want:        []wantFilename{{FilenameDispositionExtended, "한글.txt"}, {FilenameDisposition, "hangul.txt"}}},
		{name: "an empty filename yields to the name", contentType: "application/pdf; name=a.pdf", disposition: `attachment; filename=""`,
			want: []wantFilename{{FilenameContentType, "a.pdf"}, {FilenameDisposition, ""}}},
		{name: "an undecodable filename yields to a readable name", conflict: true,
			contentType: `application/x-hwp; name="=?EUC-KR?B?` + b64(hangul+".hwp") + `?="`,
			disposition: "attachment; filename*=x-none''" + percent(hangul) + ".hwp",
			want:        []wantFilename{{FilenameContentType, "한글.hwp"}, {FilenameDispositionExtended, hangul + ".hwp"}}},
		{name: "an undecodable filename is still a candidate when alone",
			disposition: "attachment; filename*=x-none''" + percent(hangul) + ".hwp",
			want:        []wantFilename{{FilenameDispositionExtended, hangul + ".hwp"}}},
		{name: "a repeated filename contributes every occurrence", conflict: true,
			disposition: "attachment; filename=a.txt; filename=b.txt",
			want:        []wantFilename{{FilenameDisposition, "a.txt"}, {FilenameDisposition, "b.txt"}}},
		{name: "raw EUC-KR read in the charset of the content type",
			contentType: `text/plain; charset=euc-kr; name="` + hangul + `.txt"`,
			disposition: `attachment; filename="` + hangul + `.txt"`,
			want:        []wantFilename{{FilenameDisposition, "한글.txt"}, {FilenameContentType, "한글.txt"}}},
		{name: "a path is kept as the sender gave it",
			disposition: `attachment; filename="..\..\C:\Windows\evil.exe"`,
			want:        []wantFilename{{FilenameDisposition, `..\..\C:\Windows\evil.exe`}}},
		{name: "damaged fields still name the file", conflict: true,
			contentType: "application/pdf name=my report.pdf",
			disposition: `attachment filename="my report (final).pdf`,
			want:        []wantFilename{{FilenameDisposition, "my report (final).pdf"}, {FilenameContentType, "my report.pdf"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h Header
			text := ""
			if tc.contentType != "" {
				h.Set("Content-Type", tc.contentType)
				text += "Content-Type: " + tc.contentType + "\r\n"
			}
			if tc.disposition != "" {
				h.Set("Content-Disposition", tc.disposition)
				text += "Content-Disposition: " + tc.disposition + "\r\n"
			}
			if got := filenamesOf(h.FilenameCandidates()); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Header.FilenameCandidates() = %+v, want %+v", got, tc.want)
			}

			session, e, _ := parseWith(t, text+"\r\nbody", ParseOptions{})
			before := session.Len()
			names := e.FilenameCandidates()
			if got := filenamesOf(names); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Entity.FilenameCandidates() = %+v, want %+v", got, tc.want)
			}
			var conflicts []Diagnostic
			for _, d := range session.Diagnostics()[before:] {
				if d.Code == CodeMIMEFilenameConflict {
					conflicts = append(conflicts, d)
				}
			}
			if (len(conflicts) == 1) != tc.conflict || len(conflicts) > 1 {
				t.Errorf("%d mime.filename_conflict diagnostics, want conflict = %v", len(conflicts), tc.conflict)
			}
			for _, d := range conflicts {
				if d.Severity != SeverityInfo || !strings.Contains(d.Action, names[0].Source.String()) || !d.Location.HasOffset {
					t.Errorf("conflict diagnostic = %+v", d)
				}
			}
			// Asking again records nothing, and the result is the caller's own.
			after := session.Len()
			if len(names) > 0 {
				names[0].Filename = "edited"
			}
			if got := filenamesOf(e.FilenameCandidates()); !reflect.DeepEqual(got, tc.want) || session.Len() != after {
				t.Errorf("second call = %+v with %d more diagnostics", got, session.Len()-after)
			}

			// A struct-literal entity reads its header as it is, and an entity
			// from Read, which repairs nothing else, names its file all the same.
			literal := &Entity{Header: h}
			if got := filenamesOf(literal.FilenameCandidates()); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("literal Entity.FilenameCandidates() = %+v, want %+v", got, tc.want)
			}
			if legacy, _ := Read(strings.NewReader(text + "\r\nbody")); legacy == nil {
				t.Error("Read() returned no entity")
			} else if got := filenamesOf(legacy.FilenameCandidates()); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Read: Entity.FilenameCandidates() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestFilenameCandidates_concurrent pins that candidates asked for from many
// goroutines at once are read once and reported once.
func TestFilenameCandidates_concurrent(t *testing.T) {
	text := "Content-Type: application/pdf; name=x.pdf\r\nContent-Disposition: attachment; filename=y.pdf; filename=z.pdf\r\n\r\nbody"
	session, e, _ := parseWith(t, text, ParseOptions{})
	want := []wantFilename{{FilenameDisposition, "y.pdf"}, {FilenameDisposition, "z.pdf"}, {FilenameContentType, "x.pdf"}}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cp := *e
			if got := filenamesOf(cp.FilenameCandidates()); !reflect.DeepEqual(got, want) {
				t.Errorf("FilenameCandidates() = %+v, want %+v", got, want)
			}
			e.ParameterCandidates("Content-Type")
			e.EffectiveContentDisposition()
		}()
	}
	wg.Wait()
	// Which of the two is recorded first depends on which goroutine got there.
	count := map[Code]int{}
	for _, d := range session.Diagnostics() {
		count[d.Code]++
	}
	if len(count) != 2 || count[CodeMIMEParameterDuplicate] != 1 || count[CodeMIMEFilenameConflict] != 1 {
		t.Errorf("Diagnostics() = %v, want the duplicate and the conflict, once each", codesOfDiags(session.Diagnostics()))
	}
}

// TestFilenameCandidates_keepsTheRawOccurrence pins that a candidate carries
// what it was read from: the raw text, the form, and what was declared.
func TestFilenameCandidates_keepsTheRawOccurrence(t *testing.T) {
	var h Header
	h.Set("Content-Disposition", `attachment; filename*1*=%EA%B8%80.txt; filename*0*=UTF-8'ko'%ED%95%9C; filename="C:\tmp\x.txt"`)
	names := h.FilenameCandidates()
	if len(names) != 2 {
		t.Fatalf("candidates = %+v", names)
	}
	first, second := names[0], names[1]
	if first.Filename != "한글.txt" || first.Parameter.Form != ParameterContinued || first.Parameter.Charset != "UTF-8" ||
		first.Parameter.Language != "ko" || first.Parameter.Raw != "filename*1*=%EA%B8%80.txt; filename*0*=UTF-8'ko'%ED%95%9C" || !first.Parameter.Chosen {
		t.Errorf("first = %+v", first)
	}
	if second.Filename != `C:\tmp\x.txt` || second.Parameter.Raw != `filename="C:\tmp\x.txt"` || second.Parameter.Chosen {
		t.Errorf("second = %+v", second)
	}
	// The original is never made safe implicitly; the safe name is derived.
	if got := SafeFilename(second.Filename); got != "x.txt" {
		t.Errorf("SafeFilename(%q) = %q", second.Filename, got)
	}
}

// TestFilenameCandidates_strictRefusal pins that a field ProfileStrict refused
// offers no filename, while the other field still does.
func TestFilenameCandidates_strictRefusal(t *testing.T) {
	text := "Content-Type: application/pdf; name=a.pdf\r\nContent-Disposition: attachment; filename=b c.pdf\r\n\r\nbody"
	_, e, _ := parseWith(t, text, ParseOptions{Profile: ProfileStrict})
	if got := filenamesOf(e.FilenameCandidates()); !reflect.DeepEqual(got, []wantFilename{{FilenameContentType, "a.pdf"}}) {
		t.Errorf("strict FilenameCandidates() = %+v, want only the name of the field that was not refused", got)
	}
	if _, _, err := e.EffectiveContentDisposition(); !IsFatal(err) {
		t.Errorf("EffectiveContentDisposition() err = %v, want the refusal", err)
	}
}

func TestSafeFilename(t *testing.T) {
	long := strings.Repeat("가", 200) // 600 bytes
	for _, tc := range []struct{ in, want string }{
		{"report.pdf", "report.pdf"},
		{"한글 파일.hwp", "한글 파일.hwp"},
		{"", "unnamed"},
		{"   ", "unnamed"},
		{".", "unnamed"},
		{"..", "unnamed"},
		{"...", "unnamed"},
		{"../../etc/passwd", "passwd"},
		{`..\..\Windows\system32\cmd.exe`, "cmd.exe"},
		{`C:\Users\a\b.txt`, "b.txt"},
		{"/absolute/path/", "unnamed"},
		{"a/../b", "b"},
		{"dir/..", "unnamed"},
		{"a\x00b.txt", "ab.txt"},
		{"a\r\nb\tc.txt", "a  b c.txt"},
		{"\x1b[31mred\x7f.txt", "[31mred.txt"},
		{"evil\u202Egpj.exe", "evilgpj.exe"},
		{"zero\u200bwidth\ufeff.txt", "zerowidth.txt"},
		{"joined\u200dname.txt", "joined\u200dname.txt"},
		{`a<b>c:d"e|f?g*h.txt`, "a_b_c_d_e_f_g_h.txt"},
		{"file.txt:stream", "file.txt_stream"},
		{"  spaced.txt  ", "spaced.txt"},
		{"trailing.dots...", "trailing.dots"},
		{"trailing spaces and dots . . ", "trailing spaces and dots"},
		{".hidden", ".hidden"},
		{"CON", "_CON"},
		{"con.txt", "_con.txt"},
		{"Nul.tar.gz", "_Nul.tar.gz"},
		{"COM1", "_COM1"},
		{"lpt9.log", "_lpt9.log"},
		{"COM¹", "_COM¹"},
		{"aux .txt", "_aux .txt"},
		{"CONSOLE", "CONSOLE"},
		{"COM10", "COM10"},
		{"icon", "icon"},
		{"bad\xff\xfebytes.txt", "bad__bytes.txt"},
		{long + ".hwp", strings.Repeat("가", 83) + ".hwp"},
		{long, strings.Repeat("가", 85)},
		{strings.Repeat("a", 300), strings.Repeat("a", 255)},
		{"a." + strings.Repeat("e", 300), "a." + strings.Repeat("e", 253)},
		{"CON" + strings.Repeat(" ", 300) + "x", "_CON"},
	} {
		got := SafeFilename(tc.in)
		if got != tc.want {
			t.Errorf("SafeFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSafeFilename_invariants checks every hostile name, and every name made
// from the hostile fields, against what a safe name promises.
func TestSafeFilename_invariants(t *testing.T) {
	names := []string{
		"\x00", "\\", "/", "//", "\\\\server\\share\\", "a\\", "nul", "NUL.", "nul .", " nul", "..nul", "...a", "a...", ". .", "\u202e", "\ufeff\u200b",
		strings.Repeat("\xff", 400), strings.Repeat(".", 400), strings.Repeat(" ", 400), strings.Repeat("é", 400), strings.Repeat("a.", 400),
		"prn" + strings.Repeat(".", 300), strings.Repeat("가", 84) + "\U0001F600.txt", "x" + strings.Repeat("\U0001F600", 100),
	}
	names = append(names, hostileFields...)
	for _, name := range names {
		got := SafeFilename(name)
		switch {
		case got == "":
			t.Errorf("SafeFilename(%q) is empty", name)
		case len(got) > 255:
			t.Errorf("SafeFilename(%q) is %d bytes", name, len(got))
		case !utf8.ValidString(got):
			t.Errorf("SafeFilename(%q) = %q is not UTF-8", name, got)
		case strings.ContainsAny(got, "/\\\x00<>:\"|?*\r\n\t"):
			t.Errorf("SafeFilename(%q) = %q holds a separator or a reserved character", name, got)
		case got == "." || got == ".." || strings.HasSuffix(got, ".") || strings.HasSuffix(got, " ") || strings.HasPrefix(got, " "):
			t.Errorf("SafeFilename(%q) = %q is a dot name or has an edge Windows drops", name, got)
		case isWindowsDeviceName(got):
			t.Errorf("SafeFilename(%q) = %q is a device name", name, got)
		case SafeFilename(got) != got:
			t.Errorf("SafeFilename is not idempotent on %q: %q then %q", name, got, SafeFilename(got))
		}
	}
}
