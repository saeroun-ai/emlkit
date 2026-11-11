package appledouble

import "testing"

const fuzzInputCap = 64 << 10

func FuzzParse(f *testing.F) {
	f.Add(FixtureAppleMailHeader("report.pdf"))
	f.Add(macOSDouble(finder("TEXT", "ttxt", 0), []xattr{{"com.apple.quarantine", []byte("0083;00000000;Mail;")}}, fork(res{"STR ", 1, "x", []byte("abc")})))
	f.Add(build(MagicSingle, Version1, "Macintosh", fx{EntryRealName, []byte("R\x8esum\x8e")}, fx{EntryFileDates, datesEntry(1, 2, 3, 4)}, fx{EntryDataFork, []byte("data")}))
	f.Add(buildRaw(0xffff, []desc{{EntryResourceFork, 0xfffffff0, 0xffffffff}, {EntryFinderInfo, 10, 100}}, nil))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > fuzzInputCap {
			b = b[:fuzzInputCap]
		}
		for _, lim := range []Limits{{}, {MaxEntries: 2, MaxEntryBytes: 64, MaxTotalBytes: 100, MaxAttributes: 2, MaxResources: 3, MaxIssues: 4}} {
			file, err := Parse(b, &lim)
			if err != nil {
				if file != nil {
					t.Fatal("a File with an error")
				}
				continue
			}
			checkBounds(t, file, lim.resolve(), len(b))
			if _, err := Combine(b, b[:len(b)/2], &lim); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func checkBounds(t *testing.T, f *File, lim Limits, size int) {
	t.Helper()
	if len(f.Entries) > lim.MaxEntries || len(f.Issues) > lim.MaxIssues+1 {
		t.Fatalf("%d entries, %d issues", len(f.Entries), len(f.Issues))
	}
	var total int64
	for _, e := range f.Entries {
		n := int64(len(e.Data))
		if n > int64(e.Length) || n > lim.MaxEntryBytes || (n > 0 && int64(e.Offset)+n > int64(size)) {
			t.Fatalf("entry %+v out of bounds", e)
		}
		total += n
	}
	if total > lim.MaxTotalBytes {
		t.Fatalf("%d bytes in all", total)
	}
	if fi := f.Finder; fi != nil {
		if len(fi.Attributes) > max(lim.MaxAttributes, 0) || len(fi.Raw) > 32 {
			t.Fatalf("%d attributes", len(fi.Attributes))
		}
		for _, a := range fi.Attributes {
			if int64(len(a.Value)) > lim.MaxEntryBytes || len(a.Value) > int(a.Length) {
				t.Fatalf("attribute %q of %d bytes", a.Name, len(a.Value))
			}
		}
	}
	if rf := f.Resource; rf != nil {
		if len(rf.Resources) > max(lim.MaxResources, 0) {
			t.Fatalf("%d resources", len(rf.Resources))
		}
		for _, r := range rf.Resources {
			if len(r.Data) > int(r.Length) || r.Offset+int64(len(r.Data)) > int64(len(rf.Data)) {
				t.Fatalf("resource %+v out of bounds", r)
			}
		}
	}
}
