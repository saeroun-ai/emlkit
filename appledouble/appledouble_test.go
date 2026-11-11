package appledouble

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

func codes(f *File) []Code {
	var out []Code
	for _, i := range f.Issues {
		out = append(out, i.Code)
	}
	return out
}

func mustParse(t *testing.T, b []byte, lim *Limits) *File {
	t.Helper()
	f, err := Parse(b, lim)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}

func TestAppleMailDouble(t *testing.T) {
	icon := bytes.Repeat([]byte{0xa5}, 64)
	b := build(MagicDouble, Version2, "",
		fx{EntryFinderInfo, finder("PDF ", "prvw", FlagHasBeenInited|FlagIsInvisible)},
		fx{EntryRealName, []byte("report.pdf")},
		fx{EntryResourceFork, fork(
			res{"icns", -16455, "", icon},
			res{"STR ", 128, "Note", []byte("hi")},
			res{"STR ", 129, "", nil},
		)},
	)
	f := mustParse(t, b, nil)
	if len(f.Issues) != 0 {
		t.Fatalf("issues: %v", f.Issues)
	}
	if f.Format != AppleDouble || f.Version != Version2 || f.Declared != 3 || len(f.Entries) != 3 {
		t.Fatalf("header: %v %#x %d %d", f.Format, f.Version, f.Declared, len(f.Entries))
	}
	if f.Name == nil || f.Name.Text != "report.pdf" || f.Name.Charset != "utf-8" {
		t.Fatalf("name: %+v", f.Name)
	}
	fi := f.Finder
	if fi == nil || fi.Type.String() != "PDF " || fi.Creator.String() != "prvw" || fi.Flags&FlagIsInvisible == 0 || len(fi.Attributes) != 0 {
		t.Fatalf("finder: %+v", fi)
	}
	rf := f.Resource
	if rf == nil || rf.Types != 2 || len(rf.Resources) != 3 {
		t.Fatalf("fork: %+v", rf)
	}
	want := []struct {
		typ  string
		id   int16
		name string
		data []byte
	}{{"icns", -16455, "", icon}, {"STR ", 128, "Note", []byte("hi")}, {"STR ", 129, "", nil}}
	for i, w := range want {
		r := rf.Resources[i]
		if r.Type.String() != w.typ || r.ID != w.id || r.Name != w.name || r.HasName != (w.name != "") || !bytes.Equal(r.Data, w.data) || r.Truncated {
			t.Errorf("resource %d: %+v, want %+v", i, r, w)
		}
	}
	if f.DataFork != nil || f.Entry(EntryDataFork) != nil {
		t.Fatalf("data fork in a header-only AppleDouble")
	}
}

func TestAppleSingleDataFork(t *testing.T) {
	b := build(MagicSingle, Version1, "Macintosh",
		fx{EntryRealName, []byte("hello.txt")},
		fx{EntryFileDates, datesEntry(0, -86400, DateUnknown, 1)},
		fx{EntryComment, []byte("a comment")},
		fx{99, []byte("opaque")},
		fx{EntryDataFork, []byte("Hello, world\n")},
	)
	f := mustParse(t, b, nil)
	if len(f.Issues) != 0 {
		t.Fatalf("issues: %v", f.Issues)
	}
	if f.Format != AppleSingle || f.HomeFileSystem != "Macintosh" || f.Version != Version1 {
		t.Fatalf("header: %v %q %#x", f.Format, f.HomeFileSystem, f.Version)
	}
	if string(f.DataFork) != "Hello, world\n" || string(f.Comment) != "a comment" || f.Name.Text != "hello.txt" {
		t.Fatalf("views: %q %q %+v", f.DataFork, f.Comment, f.Name)
	}
	if e := f.Entry(99); e == nil || string(e.Data) != "opaque" || e.ID.String() != "entry 99" {
		t.Fatalf("unknown entry: %+v", e)
	}
	d := f.Dates
	if !d.Created.Equal(DateEpoch) || !d.Modified.Equal(time.Date(1999, 12, 31, 0, 0, 0, 0, time.UTC)) ||
		!d.Backup.IsZero() || !d.Accessed.Equal(DateEpoch.Add(time.Second)) {
		t.Fatalf("dates: %+v", d)
	}
	desc, err := Combine(b, nil, nil)
	if err != nil || desc.Name != "hello.txt" || string(desc.Data) != "Hello, world\n" || len(desc.Issues) != 0 {
		t.Fatalf("Combine: %v %+v", err, desc)
	}
}

func TestNames(t *testing.T) {
	nfd := norm.NFD.String("보고서.pdf")
	for _, tc := range []struct {
		name, raw, text, charset string
		issue                    Code
	}{
		{"ascii", "plain.txt", "plain.txt", "utf-8", ""},
		{"utf-8 composed", "보고서.pdf", "보고서.pdf", "utf-8", ""},
		{"utf-8 decomposed", nfd, "보고서.pdf", "utf-8", ""},
		{"macroman", "R\x8esum\x8e.doc", "Résumé.doc", "macintosh", ""},
		{"macroman symbols", "\xa5 \xa9 \xd2x\xd3", "• © “x”", "macintosh", ""},
		{"trailing NULs", "a.txt\x00\x00", "a.txt", "utf-8", ""},
		{"long", strings.Repeat("a", 2000), strings.Repeat("a", maxNameBytes), "utf-8", CodeNameLong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := mustParse(t, build(MagicDouble, Version2, "", fx{EntryRealName, []byte(tc.raw)}, fx{EntryShortName, []byte(tc.raw)}), nil)
			for _, n := range []*Name{f.Name, f.ShortName} {
				if n == nil || n.Text != tc.text || n.Charset != tc.charset || string(n.Raw) != tc.raw {
					t.Fatalf("got %+v, want %q %s", n, tc.text, tc.charset)
				}
			}
			if got := codes(f); (tc.issue == "") != (len(got) == 0) || (tc.issue != "" && !slices.Contains(got, tc.issue)) {
				t.Fatalf("issues %v, want %q", got, tc.issue)
			}
		})
	}
}

func TestFinderAttributes(t *testing.T) {
	quarantine := []byte("0083;5f2b1c3a;Safari;9A1B2C3D-0000-0000-0000-000000000000")
	whereFroms := append([]byte("bplist00"), bytes.Repeat([]byte{1, 2, 3}, 20)...)
	b := macOSDouble(finder("TEXT", "ttxt", FlagHasBeenInited), []xattr{
		{"com.apple.quarantine", quarantine},
		{"com.apple.metadata:kMDItemWhereFroms", whereFroms},
		{"com.apple.lastuseddate#PS", make([]byte, 16)},
	}, fork())
	f := mustParse(t, b, nil)
	if len(f.Issues) != 0 {
		t.Fatalf("issues: %v", f.Issues)
	}
	if f.HomeFileSystem != "Mac OS X" {
		t.Fatalf("home file system %q", f.HomeFileSystem)
	}
	fi := f.Finder
	if fi.Type.String() != "TEXT" || fi.Creator.String() != "ttxt" || len(fi.Raw) != 32 || fi.DeclaredAttributes != 3 || len(fi.Attributes) != 3 {
		t.Fatalf("finder: %+v", fi)
	}
	for i, w := range []struct {
		name  string
		value []byte
	}{{"com.apple.quarantine", quarantine}, {"com.apple.metadata:kMDItemWhereFroms", whereFroms}, {"com.apple.lastuseddate#PS", make([]byte, 16)}} {
		a := fi.Attributes[i]
		if a.Name != w.name || !bytes.Equal(a.Value, w.value) || a.Truncated {
			t.Errorf("attribute %d: %q %q", i, a.Name, a.Value)
		}
	}
	if f.Resource == nil || len(f.Resource.Data) != 286 || f.Resource.Types != 0 || len(f.Resource.Resources) != 0 {
		t.Fatalf("empty fork: %d bytes, %+v", len(f.Resource.Data), f.Resource)
	}
}

// TestTruncatedEveryPrefix cuts fixtures at every byte: only a cut inside
// the magic is an error, every other cut is an issue, and what is read is a
// prefix of what the whole input gives.
func TestTruncatedEveryPrefix(t *testing.T) {
	for _, full := range [][]byte{
		FixtureAppleMailHeader("report.pdf"),
		macOSDouble(finder("TEXT", "ttxt", 0), []xattr{{"com.apple.quarantine", []byte("0083;00000000;Mail;")}}, fork(res{"STR ", 1, "x", []byte("abc")})),
		build(MagicSingle, Version2, "", fx{EntryRealName, []byte("a")}, fx{EntryFileDates, datesEntry(1, 2, 3, 4)}, fx{EntryDataFork, []byte("data")}),
	} {
		whole := mustParse(t, full, nil)
		for i := range len(full) {
			f, err := Parse(full[:i], nil)
			if i < 4 {
				if !errors.Is(err, ErrNotAppleFile) || f != nil {
					t.Fatalf("cut %d: %v", i, err)
				}
				continue
			}
			if err != nil || len(f.Issues) == 0 {
				t.Fatalf("cut %d: err %v, no issue", i, err)
			}
			for k, e := range f.Entries {
				w := whole.Entries[k]
				if e.ID != w.ID || !bytes.HasPrefix(w.Data, e.Data) || (len(e.Data) < len(w.Data)) != e.Truncated {
					t.Fatalf("cut %d entry %d: %+v not a prefix of %+v", i, k, e, w)
				}
			}
		}
	}
}

func TestDamage(t *testing.T) {
	pad := bytes.Repeat([]byte{'x'}, 64)
	tableEnd := uint32(headerSize + 2*descriptorSize)
	shortAttr := append(finder("TEXT", "ttxt", 0), 0, 0)
	shortAttr = append(shortAttr, "ATTR\x00\x00"...)
	badName := macOSDouble(finder("TEXT", "ttxt", 0), []xattr{{"q", []byte("v")}}, nil)
	attrAt := headerSize + 2*descriptorSize + attrHeaderAt + attrHeaderSize
	badName[attrAt+11+1] = 'x' // overwrite the NUL
	badName[attrAt+11] = 0xff
	farValue := macOSDouble(finder("TEXT", "ttxt", 0), []xattr{{"q", []byte("v")}}, nil)
	be.PutUint32(farValue[attrAt:], 1<<20) // the value's offset
	shortFork := fork(res{"STR ", 1, "name", []byte("data")})
	cutData := slices.Clone(shortFork)
	be.PutUint32(cutData[256:], 1<<30) // the first resource's length
	for _, tc := range []struct {
		name  string
		in    []byte
		want  []Code
		check func(*testing.T, *File)
	}{
		{"overlap", buildRaw(2, []desc{{EntryRealName, tableEnd, 10}, {EntryComment, tableEnd + 5, 10}}, pad),
			[]Code{CodeEntryOverlap}, nil},
		{"overlaps header", buildRaw(1, []desc{{EntryRealName, 10, 5}}, pad), []Code{CodeEntryOverlap}, nil},
		{"starts past end", buildRaw(1, []desc{{EntryResourceFork, 1000, 10}}, pad), []Code{CodeEntryRange},
			func(t *testing.T, f *File) {
				if e := f.Entries[0]; e.Data != nil || !e.Truncated {
					t.Fatalf("%+v", e)
				}
			}},
		{"runs past end", buildRaw(1, []desc{{EntryComment, tableEnd - 12, 1<<32 - 1}}, pad), []Code{CodeEntryRange},
			func(t *testing.T, f *File) {
				if len(f.Comment) != len(pad) {
					t.Fatalf("comment %d bytes", len(f.Comment))
				}
			}},
		{"offset and length near 2^32", buildRaw(1, []desc{{EntryResourceFork, 0xfffffff0, 0xffffffff}}, nil), []Code{CodeEntryRange}, nil},
		{"absurd count", buildRaw(0xffff, []desc{{EntryRealName, 0, 0}, {EntryComment, 0, 0}}, nil),
			[]Code{CodeBudgetEntries, CodeTruncatedHeader}, func(t *testing.T, f *File) {
				if f.Declared != 0xffff || len(f.Entries) != 2 {
					t.Fatalf("%d %d", f.Declared, len(f.Entries))
				}
			}},
		{"duplicate", build(MagicDouble, Version2, "", fx{EntryRealName, []byte("first")}, fx{EntryRealName, []byte("second")}),
			[]Code{CodeEntryDuplicate}, func(t *testing.T, f *File) {
				if f.Name.Text != "first" || len(f.Entries) != 2 {
					t.Fatalf("%+v", f.Name)
				}
			}},
		{"data fork in AppleDouble", build(MagicDouble, Version2, "", fx{EntryDataFork, []byte("d")}), []Code{CodeEntryUnexpected}, nil},
		{"unknown version", build(MagicSingle, 0x00030000, "", fx{EntryRealName, []byte("n")}), []Code{CodeVersion},
			func(t *testing.T, f *File) {
				if f.Name.Text != "n" {
					t.Fatalf("%+v", f.Name)
				}
			}},
		{"header cut in filler", build(MagicDouble, Version2, "Unix")[:14], []Code{CodeTruncatedHeader},
			func(t *testing.T, f *File) {
				if f.Version != Version2 || f.HomeFileSystem != "Unix" {
					t.Fatalf("%#x %q", f.Version, f.HomeFileSystem)
				}
			}},
		{"short dates", build(MagicDouble, Version2, "", fx{EntryFileDates, []byte{0, 0, 0, 5, 0, 0}}), []Code{CodeEntryShort},
			func(t *testing.T, f *File) {
				if f.Dates.Raw[0] != 5 || f.Dates.Raw[1] != DateUnknown || !f.Dates.Modified.IsZero() {
					t.Fatalf("%+v", f.Dates)
				}
			}},
		{"short Finder info", build(MagicDouble, Version2, "", fx{EntryFinderInfo, []byte("TEX")}), []Code{CodeEntryShort}, nil},
		{"Finder info of ten bytes", build(MagicDouble, Version2, "", fx{EntryFinderInfo, finder("PDF ", "CARO", 0x0100)[:10]}), nil,
			func(t *testing.T, f *File) {
				if f.Finder.Creator.String() != "CARO" || f.Finder.Flags != 0x0100 {
					t.Fatalf("%+v", f.Finder)
				}
			}},
		{"attribute header cut", build(MagicDouble, Version2, "", fx{EntryFinderInfo, shortAttr}), []Code{CodeAttrHeader}, nil},
		{"attribute name", badName, []Code{CodeAttrName}, func(t *testing.T, f *File) {
			if a := f.Finder.Attributes[0]; !strings.HasPrefix(a.Name, string(utf8.RuneError)) || string(a.Value) != "v" {
				t.Fatalf("%q %q", a.Name, a.Value)
			}
		}},
		{"attribute value past end", farValue, []Code{CodeAttrRange, CodeAttrOutside}, func(t *testing.T, f *File) {
			if a := f.Finder.Attributes[0]; a.Value != nil || !a.Truncated {
				t.Fatalf("%+v", a)
			}
		}},
		{"fork shorter than its header", build(MagicDouble, Version2, "", fx{EntryResourceFork, []byte{1, 2, 3, 4, 5}}), []Code{CodeResourceMap},
			func(t *testing.T, f *File) {
				if len(f.Resource.Data) != 5 {
					t.Fatal(f.Resource)
				}
			}},
		{"fork map cut", build(MagicDouble, Version2, "", fx{EntryResourceFork, shortFork[:len(shortFork)-20]}), []Code{CodeResourceRange}, nil},
		{"resource data past fork", build(MagicDouble, Version2, "", fx{EntryResourceFork, cutData}), []Code{CodeResourceRange},
			func(t *testing.T, f *File) {
				if r := f.Resource.Resources[0]; !r.Truncated || r.Name != "name" || r.Length != 1<<30 {
					t.Fatalf("%+v", r)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := mustParse(t, tc.in, nil)
			got := codes(f)
			for _, w := range tc.want {
				if !slices.Contains(got, w) {
					t.Errorf("issues %v lack %s", f.Issues, w)
				}
			}
			if len(tc.want) == 0 && len(got) != 0 {
				t.Errorf("unexpected issues %v", f.Issues)
			}
			if tc.check != nil {
				tc.check(t, f)
			}
		})
	}
}

func TestNotAppleFile(t *testing.T) {
	for _, in := range [][]byte{nil, {0, 5, 0x16}, []byte("MZ\x90\x00 not apple"), {0, 5, 0x16, 0x08}} {
		if f, err := Parse(in, nil); !errors.Is(err, ErrNotAppleFile) || f != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
}

func TestBudgets(t *testing.T) {
	three := build(MagicDouble, Version2, "", fx{EntryRealName, []byte("name")}, fx{EntryComment, []byte("0123456789")}, fx{EntryShortName, []byte("short")})
	attrs := macOSDouble(finder("TEXT", "ttxt", 0), []xattr{{"a", []byte("1")}, {"b", []byte("2")}, {"c", []byte("3")}}, fork(
		res{"STR ", 1, "", []byte("a")}, res{"STR ", 2, "", []byte("b")}, res{"ICON", 3, "", []byte("c")}))
	for _, tc := range []struct {
		name  string
		in    []byte
		lim   Limits
		want  Code
		check func(*testing.T, *File)
	}{
		{"entries", three, Limits{MaxEntries: 1}, CodeBudgetEntries, func(t *testing.T, f *File) {
			if len(f.Entries) != 1 || f.Declared != 3 {
				t.Fatal(len(f.Entries))
			}
		}},
		{"entry bytes", three, Limits{MaxEntryBytes: 4}, CodeBudgetEntryBytes, func(t *testing.T, f *File) {
			if string(f.Comment) != "0123" || !f.Entries[1].Truncated {
				t.Fatalf("%q", f.Comment)
			}
		}},
		{"total bytes", three, Limits{MaxTotalBytes: 10}, CodeBudgetTotalBytes, func(t *testing.T, f *File) {
			n := 0
			for _, e := range f.Entries {
				n += len(e.Data)
			}
			if n != 10 || f.ShortName.Text != "" {
				t.Fatalf("%d bytes, short name %+v", n, f.ShortName)
			}
		}},
		{"attributes", attrs, Limits{MaxAttributes: 1}, CodeBudgetAttributes, func(t *testing.T, f *File) {
			if len(f.Finder.Attributes) != 1 || f.Finder.DeclaredAttributes != 3 {
				t.Fatal(f.Finder.Attributes)
			}
		}},
		{"attributes off", attrs, Limits{MaxAttributes: -1}, "", func(t *testing.T, f *File) {
			if f.Finder.Attributes != nil || f.Finder.DeclaredAttributes != 3 {
				t.Fatal(f.Finder.Attributes)
			}
		}},
		{"resources", attrs, Limits{MaxResources: 2}, CodeBudgetResources, func(t *testing.T, f *File) {
			if len(f.Resource.Resources) != 2 || f.Resource.Types != 2 {
				t.Fatal(f.Resource.Resources)
			}
		}},
		{"resources off", attrs, Limits{MaxResources: -1}, "", func(t *testing.T, f *File) {
			if f.Resource.Resources != nil || len(f.Resource.Data) == 0 {
				t.Fatal(f.Resource)
			}
		}},
		{"issues", buildRaw(5, []desc{{1, 900, 1}, {2, 901, 1}, {3, 902, 1}, {4, 903, 1}, {5, 904, 1}}, nil), Limits{MaxIssues: 2}, CodeBudgetIssues,
			func(t *testing.T, f *File) {
				if len(f.Issues) != 3 || f.DroppedIssues < 3 {
					t.Fatalf("%d kept, %d dropped", len(f.Issues), f.DroppedIssues)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := mustParse(t, tc.in, &tc.lim)
			got := codes(f)
			if tc.want != "" && !slices.Contains(got, tc.want) {
				t.Errorf("issues %v lack %s", f.Issues, tc.want)
			}
			if tc.want == "" && len(got) != 0 {
				t.Errorf("unexpected issues %v", f.Issues)
			}
			tc.check(t, f)
		})
	}
}

func TestRecognize(t *testing.T) {
	for in, want := range map[string]MediaKind{
		"application/applefile":                       KindAppleFile,
		" Application/AppleFile; name=\"x\" ":         KindAppleFile,
		"multipart/appledouble; boundary=b":           KindAppleDouble,
		"MULTIPART/APPLEDOUBLE":                       KindAppleDouble,
		"application/mac-binhex40":                    KindBinHex,
		"application/x-mac-binhex40; name=a":          KindBinHex,
		"application/octet-stream":                    KindNone,
		"multipart/mixed":                             KindNone,
		"":                                            KindNone,
		"multipart/appledoubl" + string(rune(0x212a)): KindNone, // Kelvin sign
		"application/applefile-but-not-really":        KindNone,
	} {
		if got := Recognize(in); got != want {
			t.Errorf("%q: %v, want %v", in, got, want)
		}
	}
}

func TestAppleDoubleParts(t *testing.T) {
	for _, tc := range []struct {
		types        []string
		header, data int
	}{
		{[]string{"application/applefile", "application/pdf"}, 0, 1},
		{[]string{"application/pdf", "application/applefile"}, 1, 0},
		{[]string{"text/plain", "application/applefile", "image/png"}, 1, 2},
		{[]string{"application/applefile"}, 0, -1},
		{[]string{"application/applefile", "application/applefile", "image/png"}, 0, 2},
		{[]string{"image/png"}, -1, 0},
		{nil, -1, -1},
	} {
		if h, d := AppleDoubleParts(tc.types); h != tc.header || d != tc.data {
			t.Errorf("%v: %d %d, want %d %d", tc.types, h, d, tc.header, tc.data)
		}
	}
}

func TestCombine(t *testing.T) {
	header := FixtureAppleMailHeader("Résumé.pdf")
	pdf := []byte("%PDF-1.4\n...")
	d, err := Combine(header, pdf, nil)
	if err != nil || d.Name != "Résumé.pdf" || d.Type.String() != "PDF " || d.Creator.String() != "prvw" ||
		!bytes.Equal(d.Data, pdf) || len(d.ResourceFork) == 0 || len(d.Issues) != 0 || d.File == nil {
		t.Fatalf("%v %+v", err, d)
	}
	if d, _ := Combine(header, nil, nil); len(d.Issues) != 1 || d.Issues[0].Code != CodeMissingData || d.Data != nil {
		t.Fatalf("missing data: %+v", d.Issues)
	}
	single := build(MagicSingle, Version2, "", fx{EntryDataFork, []byte("inside")})
	if d, _ := Combine(single, pdf, nil); len(d.Issues) != 1 || d.Issues[0].Code != CodeDataConflict || !bytes.Equal(d.Data, pdf) {
		t.Fatalf("conflict: %+v", d.Issues)
	}
	if d, err := Combine([]byte("garbage"), pdf, nil); !errors.Is(err, ErrNotAppleFile) || !bytes.Equal(d.Data, pdf) || d.File != nil {
		t.Fatalf("not apple: %v %+v", err, d)
	}
}
