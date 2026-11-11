package appledouble

import "fmt"

// A Code names a kind of Issue. Codes are stable; branch on them, not on
// Issue.Detail.
type Code string

// The codes of the issues Parse and Combine report.
const (
	// CodeTruncatedHeader: the input ends inside the fixed header or the
	// descriptor table. What precedes the end is read.
	CodeTruncatedHeader Code = "header.truncated"
	// CodeVersion: the version is neither 1 nor 2; the input is read as
	// version 2.
	CodeVersion Code = "header.version"
	// CodeEntryRange: an entry starts or ends past the end of the input. Its
	// Data is what the input holds.
	CodeEntryRange Code = "entry.out_of_range"
	// CodeEntryOverlap: an entry overlaps another entry or the header. Each
	// keeps its declared bytes.
	CodeEntryOverlap Code = "entry.overlap"
	// CodeEntryDuplicate: an entry id is repeated. The typed view is taken
	// from the first; all stay in File.Entries.
	CodeEntryDuplicate Code = "entry.duplicate"
	// CodeEntryShort: an entry with a fixed layout is too short for it. The
	// fields it holds are read; the others are left unknown.
	CodeEntryShort Code = "entry.short"
	// CodeEntryUnexpected: an AppleDouble header carries a data fork, which
	// belongs in the separate data file. It is kept.
	CodeEntryUnexpected Code = "entry.unexpected"
	// CodeNameLong: a name entry is longer than any file system allows; only
	// its first bytes are decoded.
	CodeNameLong Code = "name.long"
	// CodeAttrHeader: the extended-attribute header after the Finder info,
	// or its table, is cut short. The attributes before the cut are listed.
	CodeAttrHeader Code = "xattr.header"
	// CodeAttrName: an attribute name is empty, not NUL-terminated or not
	// UTF-8. It is kept with invalid bytes replaced.
	CodeAttrName Code = "xattr.name"
	// CodeAttrRange: an attribute value lies past the end of the input. Its
	// Value is what the input holds.
	CodeAttrRange Code = "xattr.out_of_range"
	// CodeAttrOutside: an attribute value lies outside the Finder info
	// entry that lists it. It is read from the input all the same.
	CodeAttrOutside Code = "xattr.outside_entry"
	// CodeResourceMap: the resource fork's header or map is too short to be
	// walked. The fork's bytes are still returned.
	CodeResourceMap Code = "rsrc.map"
	// CodeResourceRange: a type list, reference, name or resource data lies
	// past the end of the fork or its map. What lies within is listed.
	CodeResourceRange Code = "rsrc.out_of_range"
	// The budget codes: a Limit was reached and the rest was not read.
	CodeBudgetEntries    Code = "budget.entries"
	CodeBudgetEntryBytes Code = "budget.entry_bytes"
	CodeBudgetTotalBytes Code = "budget.total_bytes"
	CodeBudgetAttributes Code = "budget.attributes"
	CodeBudgetResources  Code = "budget.resources"
	CodeBudgetIssues     Code = "budget.issues"
	// CodeMissingData: Combine had no data part and the header no data fork;
	// the description has no data.
	CodeMissingData Code = "combine.missing_data"
	// CodeDataConflict: Combine had a data part and the header a data fork
	// too; the data part is used.
	CodeDataConflict Code = "combine.data_conflict"
)

// An Issue is one thing found wrong with an input and how it was handled.
type Issue struct {
	Code Code
	// Entry is the entry concerned, zero when the issue is about none.
	Entry EntryID
	// Offset is the input offset the issue concerns.
	Offset int64
	// Detail describes the case for a person.
	Detail string
}

// String returns the code, offset and detail.
func (i Issue) String() string {
	return fmt.Sprintf("%s at %d: %s", i.Code, i.Offset, i.Detail)
}

// Limits bounds what Parse reads and returns. The zero value, and a nil
// *Limits, are the defaults.
type Limits struct {
	// MaxEntries is how many descriptors are read. Zero or negative is 64.
	MaxEntries int
	// MaxEntryBytes is how many bytes of one entry, or one attribute value,
	// are returned. Zero or negative is 16 MiB.
	MaxEntryBytes int64
	// MaxTotalBytes is how many entry bytes are returned in all. Zero or
	// negative is 64 MiB.
	MaxTotalBytes int64
	// MaxAttributes is how many extended attributes are listed. Zero is 128;
	// negative lists none.
	MaxAttributes int
	// MaxResources is how many resources of the resource map are listed.
	// Zero is 1024; negative does not walk the map.
	MaxResources int
	// MaxIssues is how many issues are kept. Zero or negative is 64.
	MaxIssues int
}

// The default Limits.
const (
	DefaultMaxEntries    = 64
	DefaultMaxEntryBytes = 16 << 20
	DefaultMaxTotalBytes = 64 << 20
	DefaultMaxAttributes = 128
	DefaultMaxResources  = 1024
	DefaultMaxIssues     = 64
)

func (l *Limits) resolve() Limits {
	var r Limits
	if l != nil {
		r = *l
	}
	if r.MaxEntries <= 0 {
		r.MaxEntries = DefaultMaxEntries
	}
	if r.MaxEntryBytes <= 0 {
		r.MaxEntryBytes = DefaultMaxEntryBytes
	}
	if r.MaxTotalBytes <= 0 {
		r.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if r.MaxAttributes == 0 {
		r.MaxAttributes = DefaultMaxAttributes
	}
	if r.MaxResources == 0 {
		r.MaxResources = DefaultMaxResources
	}
	if r.MaxIssues <= 0 {
		r.MaxIssues = DefaultMaxIssues
	}
	return r
}

// issue records an issue unless MaxIssues are kept already; the first one
// past the limit is replaced by a CodeBudgetIssues issue, and the rest are
// counted.
func (p *parser) issue(code Code, id EntryID, off int64, format string, args ...any) {
	f := p.f
	switch {
	case len(f.Issues) < p.lim.MaxIssues:
		f.Issues = append(f.Issues, Issue{Code: code, Entry: id, Offset: off, Detail: fmt.Sprintf(format, args...)})
	case len(f.Issues) == p.lim.MaxIssues:
		f.Issues = append(f.Issues, Issue{Code: CodeBudgetIssues, Offset: off, Detail: "further issues are counted, not kept"})
		f.DroppedIssues++
	default:
		f.DroppedIssues++
	}
}
