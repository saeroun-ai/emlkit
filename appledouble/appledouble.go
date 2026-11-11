// Package appledouble reads the AppleSingle and AppleDouble formats that
// carry a Macintosh file's metadata and resource fork (RFC 1740, and Apple's
// "AppleSingle/AppleDouble Formats for Foreign Files", versions 1 and 2).
//
// It is an optional extension of emlkit: the core does not import it, and it
// imports nothing of the core. A caller hands it the decoded bytes of a MIME
// part, typically an application/applefile part found by mail.ReadTree, and
// gets back the entries of the header and typed views of the ones a mail
// reader cares about: the real name, the file dates, the Finder info (type,
// creator, flags, and the extended attributes macOS stores after it) and the
// resource fork with its map listed.
//
// Parse heals as far as the bytes allow. Entries that overlap, run past the
// end of the input, or come in absurd numbers or sizes yield what can be read
// and an Issue each; only an input that is not AppleSingle or AppleDouble at
// all is an error. Every offset and length is checked against the input and
// the Limits before anything is sliced, and every list Parse builds is
// bounded by a Limit.
//
// The byte slices in a File alias the input: nothing is copied. Copy what is
// kept beyond the input's lifetime.
//
// Wiring to a parsed message, with mail.ReadTree:
//
//	tree, _ := mail.ReadTree(root, nil)
//	tree.Root.Walk(func(n *mail.Node) bool {
//		if appledouble.Recognize(n.MediaType) != appledouble.KindAppleDouble {
//			return true
//		}
//		types := make([]string, len(n.Children))
//		for i, c := range n.Children {
//			types[i] = c.MediaType
//		}
//		h, d := appledouble.AppleDoubleParts(types)
//		if h < 0 {
//			return true
//		}
//		var data []byte
//		if d >= 0 {
//			data = n.Children[d].Data
//		}
//		desc, err := appledouble.Combine(n.Children[h].Data, data, nil)
//		// desc.Name, desc.Type, desc.Creator, desc.Data, desc.Issues ...
//		return true
//	})
package appledouble

import (
	"cmp"
	"encoding/binary"
	"errors"
	"slices"
	"strconv"
	"strings"
)

// Format is which of the two formats an input is.
type Format uint8

const (
	// AppleSingle carries the data fork with the metadata, in one file.
	AppleSingle Format = 1
	// AppleDouble carries everything but the data fork, which travels apart.
	AppleDouble Format = 2
)

// String returns "AppleSingle" or "AppleDouble".
func (f Format) String() string {
	switch f {
	case AppleSingle:
		return "AppleSingle"
	case AppleDouble:
		return "AppleDouble"
	}
	return "Format(?)"
}

// The magic numbers and versions of the formats.
const (
	MagicSingle uint32 = 0x00051600
	MagicDouble uint32 = 0x00051607
	Version1    uint32 = 0x00010000
	Version2    uint32 = 0x00020000
)

// ErrNotAppleFile is the one error Parse returns: the input does not begin
// with the AppleSingle or AppleDouble magic number.
var ErrNotAppleFile = errors.New("appledouble: input is not AppleSingle or AppleDouble")

// An EntryID says what an entry holds.
type EntryID uint32

// The entry ids of the formats. Ids not listed are kept raw.
const (
	EntryDataFork       EntryID = 1
	EntryResourceFork   EntryID = 2
	EntryRealName       EntryID = 3
	EntryComment        EntryID = 4
	EntryIconBW         EntryID = 5
	EntryIconColor      EntryID = 6
	EntryFileInfo       EntryID = 7 // version 1 only; replaced by 8, 10, 11, 12
	EntryFileDates      EntryID = 8
	EntryFinderInfo     EntryID = 9
	EntryMacFileInfo    EntryID = 10
	EntryProDOSFileInfo EntryID = 11
	EntryMSDOSFileInfo  EntryID = 12
	EntryShortName      EntryID = 13
	EntryAFPFileInfo    EntryID = 14
	EntryDirectoryID    EntryID = 15
)

var entryNames = [...]string{
	EntryDataFork:       "data fork",
	EntryResourceFork:   "resource fork",
	EntryRealName:       "real name",
	EntryComment:        "comment",
	EntryIconBW:         "b/w icon",
	EntryIconColor:      "colour icon",
	EntryFileInfo:       "file info",
	EntryFileDates:      "file dates",
	EntryFinderInfo:     "Finder info",
	EntryMacFileInfo:    "Macintosh file info",
	EntryProDOSFileInfo: "ProDOS file info",
	EntryMSDOSFileInfo:  "MS-DOS file info",
	EntryShortName:      "short name",
	EntryAFPFileInfo:    "AFP file info",
	EntryDirectoryID:    "directory id",
}

// String returns the name of a known id, or "entry N".
func (id EntryID) String() string {
	if int(id) < len(entryNames) && entryNames[id] != "" {
		return entryNames[id]
	}
	return "entry " + strconv.FormatUint(uint64(id), 10)
}

// An Entry is one entry of the header, as declared and as read.
type Entry struct {
	ID EntryID
	// Offset and Length are what the descriptor declares.
	Offset, Length uint32
	// Data is what of the declared range the input holds and the Limits
	// allow, aliasing the input. Truncated reports that it is shorter than
	// Length.
	Data      []byte
	Truncated bool
}

// A File is a parsed AppleSingle or AppleDouble header.
type File struct {
	Format  Format
	Version uint32
	// Filler is the 16 bytes after the version. In version 1 they name the
	// home file system; HomeFileSystem is that name, trimmed, when the bytes
	// are printable ASCII ("Macintosh", or "Mac OS X" as macOS writes it in
	// version 2 files too).
	Filler         [16]byte
	HomeFileSystem string
	// Declared is the entry count the header declares; Entries are the
	// descriptors read, in their order, which may be fewer.
	Declared int
	Entries  []Entry
	// Issues are what was found wrong and how it was handled. DroppedIssues
	// counts those not kept past Limits.MaxIssues.
	Issues        []Issue
	DroppedIssues int

	// The typed views of the first entry of each kind, nil when absent.
	Name      *Name         // real name (3)
	ShortName *Name         // short name (13)
	Comment   []byte        // comment (4), raw
	Dates     *FileDates    // file dates (8)
	Finder    *FinderInfo   // Finder info (9)
	Resource  *ResourceFork // resource fork (2)
	// DataFork is the data fork entry's bytes (1), nil when there is none;
	// Entry(EntryDataFork) tells an empty fork from an absent one.
	DataFork []byte
}

// Entry returns the first entry with id, or nil.
func (f *File) Entry(id EntryID) *Entry {
	for i := range f.Entries {
		if f.Entries[i].ID == id {
			return &f.Entries[i]
		}
	}
	return nil
}

const (
	headerSize     = 26 // magic, version, filler, count
	descriptorSize = 12
)

// Parse reads an AppleSingle or AppleDouble header from b with the Limits
// lim; nil is the defaults. It returns ErrNotAppleFile, and no File, only
// when b does not begin with either magic number. Anything else wrong is an
// Issue of the File returned.
func Parse(b []byte, lim *Limits) (*File, error) {
	if len(b) < 4 {
		return nil, ErrNotAppleFile
	}
	f := &File{}
	switch binary.BigEndian.Uint32(b) {
	case MagicSingle:
		f.Format = AppleSingle
	case MagicDouble:
		f.Format = AppleDouble
	default:
		return nil, ErrNotAppleFile
	}
	p := &parser{b: b, lim: lim.resolve(), f: f}
	p.header()
	p.check()
	p.views()
	return f, nil
}

type parser struct {
	b        []byte
	lim      Limits
	f        *File
	total    int64 // entry bytes handed out
	tableEnd int64 // end of the descriptor table as declared, within b
}

func (p *parser) size() int64 { return int64(len(p.b)) }

// header reads the fixed header and the descriptors.
func (p *parser) header() {
	b, f := p.b, p.f
	if len(b) < 8 {
		p.issue(CodeTruncatedHeader, 0, p.size(), "input ends inside the version")
		return
	}
	f.Version = binary.BigEndian.Uint32(b[4:])
	if f.Version != Version1 && f.Version != Version2 {
		p.issue(CodeVersion, 0, 4, "unknown version 0x%08x, read as version 2", f.Version)
	}
	copy(f.Filler[:], b[8:])
	f.HomeFileSystem = homeFileSystem(f.Filler[:min(16, len(b)-8)])
	if len(b) < headerSize {
		p.issue(CodeTruncatedHeader, 0, p.size(), "input ends inside the header (%d of %d bytes)", len(b), headerSize)
		return
	}
	declared := int(binary.BigEndian.Uint16(b[24:]))
	f.Declared = declared
	p.tableEnd = min(int64(headerSize+descriptorSize*declared), p.size())
	want := declared
	if want > p.lim.MaxEntries {
		p.issue(CodeBudgetEntries, 0, 24, "header declares %d entries; the first %d are read", declared, p.lim.MaxEntries)
		want = p.lim.MaxEntries
	}
	if fit := (len(b) - headerSize) / descriptorSize; fit < want {
		p.issue(CodeTruncatedHeader, 0, p.size(), "input ends inside the descriptor table: %d of %d descriptors", fit, declared)
		want = fit
	}
	f.Entries = make([]Entry, 0, want)
	for i := range want {
		d := b[headerSize+descriptorSize*i:]
		e := Entry{
			ID:     EntryID(binary.BigEndian.Uint32(d)),
			Offset: binary.BigEndian.Uint32(d[4:]),
			Length: binary.BigEndian.Uint32(d[8:]),
		}
		e.Data, e.Truncated = p.entryData(e)
		f.Entries = append(f.Entries, e)
	}
}

// entryData returns what of e's declared range may be handed out.
func (p *parser) entryData(e Entry) ([]byte, bool) {
	lo := int64(e.Offset)
	hi := lo + int64(e.Length) // no overflow: both are below 1<<32
	if e.Length == 0 {
		return nil, false
	}
	if lo >= p.size() {
		p.issue(CodeEntryRange, e.ID, lo, "%s of %d bytes starts at %d, past the end of the input (%d bytes)", e.ID, e.Length, lo, p.size())
		return nil, true
	}
	end := hi
	if end > p.size() {
		end = p.size()
		p.issue(CodeEntryRange, e.ID, lo, "%s runs past the end of the input: %d of %d bytes", e.ID, end-lo, e.Length)
	}
	if end-lo > p.lim.MaxEntryBytes {
		end = lo + p.lim.MaxEntryBytes
		p.issue(CodeBudgetEntryBytes, e.ID, lo, "%s cut at %d of %d bytes", e.ID, end-lo, e.Length)
	}
	if left := p.lim.MaxTotalBytes - p.total; end-lo > left {
		end = lo + left
		p.issue(CodeBudgetTotalBytes, e.ID, lo, "%s cut at %d of %d bytes: the total is spent", e.ID, end-lo, e.Length)
	}
	p.total += end - lo
	return p.b[lo:end:end], end < hi
}

// check reports entries that overlap one another or the header, and
// repeated ids.
func (p *parser) check() {
	type span struct {
		lo, hi int64
		i      int
	}
	spans := make([]span, 0, len(p.f.Entries))
	seen := make(map[EntryID]bool, len(p.f.Entries))
	for i, e := range p.f.Entries {
		if seen[e.ID] {
			p.issue(CodeEntryDuplicate, e.ID, int64(e.Offset), "%s repeated; the first is used", e.ID)
		}
		seen[e.ID] = true
		if e.ID == EntryDataFork && p.f.Format == AppleDouble {
			p.issue(CodeEntryUnexpected, e.ID, int64(e.Offset), "an AppleDouble header carries a data fork")
		}
		lo := int64(e.Offset)
		hi := min(lo+int64(e.Length), p.size())
		if lo >= hi {
			continue
		}
		if lo < p.tableEnd {
			p.issue(CodeEntryOverlap, e.ID, lo, "%s at %d overlaps the header, which ends at %d", e.ID, lo, p.tableEnd)
		}
		spans = append(spans, span{lo, hi, i})
	}
	slices.SortStableFunc(spans, func(a, b span) int { return cmp.Compare(a.lo, b.lo) })
	reach := -1 // the span reaching farthest so far
	for k, s := range spans {
		if reach >= 0 && s.lo < spans[reach].hi {
			o := p.f.Entries[spans[reach].i]
			e := p.f.Entries[s.i]
			p.issue(CodeEntryOverlap, e.ID, s.lo, "%s at %d overlaps %s at %d", e.ID, s.lo, o.ID, o.Offset)
		}
		if reach < 0 || s.hi > spans[reach].hi {
			reach = k
		}
	}
}

// views fills the typed views from the first entry of each kind.
func (p *parser) views() {
	f := p.f
	done := make(map[EntryID]bool, len(f.Entries))
	for i := range f.Entries {
		e := &f.Entries[i]
		if done[e.ID] {
			continue
		}
		done[e.ID] = true
		switch e.ID {
		case EntryDataFork:
			f.DataFork = e.Data
			if f.DataFork == nil {
				f.DataFork = []byte{}
			}
		case EntryResourceFork:
			f.Resource = p.resourceFork(e)
		case EntryRealName:
			f.Name = p.name(e)
		case EntryShortName:
			f.ShortName = p.name(e)
		case EntryComment:
			f.Comment = e.Data
		case EntryFileDates:
			f.Dates = p.dates(e)
		case EntryFinderInfo:
			f.Finder = p.finderInfo(e)
		}
	}
}

// homeFileSystem returns the filler as a name when it is printable ASCII
// padded with spaces or NULs.
func homeFileSystem(b []byte) string {
	s := strings.TrimRight(string(b), " \x00")
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return ""
		}
	}
	return s
}
