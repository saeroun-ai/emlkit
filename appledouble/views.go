package appledouble

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/unicode/norm"
)

// A Name is a real name or short name entry.
type Name struct {
	// Text is the name decoded: as UTF-8 when the bytes are valid UTF-8,
	// otherwise as MacRoman, then composed to NFC (macOS stores names
	// decomposed). Trailing NULs are dropped. It is what the sender says, not
	// a safe path.
	Text string
	// Charset is "utf-8" or "macintosh", whichever decoded Text.
	Charset string
	// Raw is the entry's bytes.
	Raw []byte
}

// maxNameBytes bounds the bytes of a name that are decoded: an HFS+ name is
// at most 255 UTF-16 units, 765 bytes of UTF-8.
const maxNameBytes = 1024

func (p *parser) name(e *Entry) *Name {
	n := &Name{Raw: e.Data}
	b := e.Data
	if len(b) > maxNameBytes {
		p.issue(CodeNameLong, e.ID, int64(e.Offset), "%s of %d bytes; the first %d are decoded", e.ID, len(b), maxNameBytes)
		b = b[:maxNameBytes]
	}
	b = bytes.TrimRight(b, "\x00")
	n.Text, n.Charset = decodeName(b)
	return n
}

// decodeName decodes a name as UTF-8 when valid, otherwise MacRoman.
func decodeName(b []byte) (string, string) {
	if utf8.Valid(b) {
		return norm.NFC.String(string(b)), "utf-8"
	}
	return norm.NFC.String(macRoman(b)), "macintosh"
}

// macRoman decodes b as MacRoman, where every byte is a character.
func macRoman(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		if c < 0x80 {
			sb.WriteByte(c)
			continue
		}
		sb.WriteRune(charmap.Macintosh.DecodeByte(c))
	}
	return sb.String()
}

// An OSType is a four-character code: a file type or creator, or a resource
// type.
type OSType [4]byte

// String returns the code decoded as MacRoman.
func (t OSType) String() string { return macRoman(t[:]) }

// FileDates are the file dates entry. Each date is seconds from 2000-01-01
// 00:00:00 UTC, signed; math.MinInt32 means unknown.
type FileDates struct {
	// The dates, zero when unknown or absent from a short entry.
	Created, Modified, Backup, Accessed time.Time
	// Raw are the stored values in that order.
	Raw [4]int32
}

// DateEpoch is the zero of the file dates.
var DateEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// DateUnknown is the stored value of an unknown date.
const DateUnknown = math.MinInt32

func (p *parser) dates(e *Entry) *FileDates {
	d := &FileDates{Raw: [4]int32{DateUnknown, DateUnknown, DateUnknown, DateUnknown}}
	b := e.Data
	if len(b) < 16 {
		p.issue(CodeEntryShort, e.ID, int64(e.Offset), "file dates of %d bytes; 16 expected", len(b))
	}
	for i := 0; i < 4 && 4*i+4 <= len(b); i++ {
		d.Raw[i] = int32(binary.BigEndian.Uint32(b[4*i:]))
	}
	ts := [4]*time.Time{&d.Created, &d.Modified, &d.Backup, &d.Accessed}
	for i, v := range d.Raw {
		if v != DateUnknown {
			*ts[i] = DateEpoch.Add(time.Duration(v) * time.Second)
		}
	}
	return d
}

// Finder flags, the bits of FinderInfo.Flags.
const (
	FlagIsOnDesk      uint16 = 0x0001
	FlagColor         uint16 = 0x000e
	FlagIsShared      uint16 = 0x0040
	FlagHasNoINITs    uint16 = 0x0080
	FlagHasBeenInited uint16 = 0x0100
	FlagHasCustomIcon uint16 = 0x0400
	FlagIsStationery  uint16 = 0x0800
	FlagNameLocked    uint16 = 0x1000
	FlagHasBundle     uint16 = 0x2000
	FlagIsInvisible   uint16 = 0x4000
	FlagIsAlias       uint16 = 0x8000
)

// FinderInfo is the Finder info entry: 16 bytes of file info and 16 of
// extended file info, then, as macOS writes it, the file's extended
// attributes.
type FinderInfo struct {
	// Type and Creator are the file type and creator codes.
	Type, Creator OSType
	// Flags are the Finder flags; ExtendedFlags the extended Finder flags.
	Flags, ExtendedFlags uint16
	// Raw is the 32 bytes of Finder info, or fewer when the entry is short.
	// A short entry is read as far as it goes.
	Raw []byte
	// Attributes are the extended attributes listed after the Finder info,
	// within Limits.MaxAttributes; DeclaredAttributes is how many the
	// attribute header declares, zero when there is none.
	Attributes         []Attribute
	DeclaredAttributes int
}

// An Attribute is one extended attribute macOS stored with the Finder info,
// such as com.apple.quarantine. Its value is not interpreted.
type Attribute struct {
	// Name is the attribute's name, NUL dropped and invalid UTF-8 replaced;
	// RawName its stored bytes.
	Name    string
	RawName []byte
	Flags   uint16
	// Offset and Length are the value's declared place, an offset from the
	// start of the input.
	Offset, Length uint32
	// Value is what of it the input holds and Limits.MaxEntryBytes allow,
	// aliasing the input; Truncated reports that it is shorter than Length.
	Value     []byte
	Truncated bool
}

const (
	finderInfoSize = 32
	attrHeaderAt   = 34 // after the Finder info and two bytes of padding
	attrHeaderSize = 36
	attrEntryFixed = 11 // offset, length, flags, name length
)

func (p *parser) finderInfo(e *Entry) *FinderInfo {
	b := e.Data
	fi := &FinderInfo{Raw: b[:min(finderInfoSize, len(b))]}
	if len(b) < 8 {
		p.issue(CodeEntryShort, e.ID, int64(e.Offset), "Finder info of %d bytes holds no type and creator", len(b))
	}
	if len(b) >= 4 {
		copy(fi.Type[:], b)
	}
	if len(b) >= 8 {
		copy(fi.Creator[:], b[4:])
	}
	if len(b) >= 10 {
		fi.Flags = binary.BigEndian.Uint16(b[8:])
	}
	if len(b) >= 26 {
		fi.ExtendedFlags = binary.BigEndian.Uint16(b[24:])
	}
	if len(b) >= attrHeaderAt+4 && string(b[attrHeaderAt:attrHeaderAt+4]) == "ATTR" {
		p.attributes(e, fi)
	}
	return fi
}

// attributes lists the extended attributes of the header that begins at
// attrHeaderAt of the Finder info entry e.
func (p *parser) attributes(e *Entry, fi *FinderInfo) {
	b := e.Data
	base := int64(e.Offset)
	if len(b) < attrHeaderAt+attrHeaderSize {
		p.issue(CodeAttrHeader, e.ID, base+attrHeaderAt, "attribute header cut short at %d of %d bytes", len(b)-attrHeaderAt, attrHeaderSize)
		return
	}
	n := int(binary.BigEndian.Uint16(b[attrHeaderAt+34:]))
	fi.DeclaredAttributes = n
	if p.lim.MaxAttributes < 0 {
		return
	}
	entryLo := int64(e.Offset)
	entryHi := entryLo + int64(e.Length)
	pos := attrHeaderAt + attrHeaderSize
	for i := range n {
		if i == p.lim.MaxAttributes {
			p.issue(CodeBudgetAttributes, e.ID, base+int64(pos), "%d attributes declared; the first %d are listed", n, i)
			return
		}
		if len(b)-pos < attrEntryFixed {
			p.issue(CodeAttrHeader, e.ID, base+int64(pos), "attribute table ends after %d of %d attributes", i, n)
			return
		}
		a := Attribute{
			Offset: binary.BigEndian.Uint32(b[pos:]),
			Length: binary.BigEndian.Uint32(b[pos+4:]),
			Flags:  binary.BigEndian.Uint16(b[pos+8:]),
		}
		nl := int(b[pos+10])
		if len(b)-pos-attrEntryFixed < nl {
			p.issue(CodeAttrHeader, e.ID, base+int64(pos), "attribute table ends inside the name of attribute %d", i)
			return
		}
		a.RawName = b[pos+attrEntryFixed : pos+attrEntryFixed+nl]
		a.Name = p.attrName(e, base+int64(pos), a.RawName)
		a.Value, a.Truncated = p.attrValue(e, a, entryLo, entryHi)
		fi.Attributes = append(fi.Attributes, a)
		pos += (attrEntryFixed + nl + 3) &^ 3
	}
}

func (p *parser) attrName(e *Entry, off int64, raw []byte) string {
	name, _, terminated := bytes.Cut(raw, []byte{0})
	switch {
	case len(name) == 0:
		p.issue(CodeAttrName, e.ID, off, "attribute with an empty name")
	case !terminated:
		p.issue(CodeAttrName, e.ID, off, "attribute name is not NUL-terminated")
	}
	if !utf8.Valid(name) {
		p.issue(CodeAttrName, e.ID, off, "attribute name is not UTF-8")
		return strings.ToValidUTF8(string(name), string(utf8.RuneError))
	}
	return string(name)
}

func (p *parser) attrValue(e *Entry, a Attribute, entryLo, entryHi int64) ([]byte, bool) {
	if a.Length == 0 {
		return nil, false
	}
	lo := int64(a.Offset)
	hi := lo + int64(a.Length)
	if lo < entryLo || hi > entryHi {
		p.issue(CodeAttrOutside, e.ID, lo, "value of %q lies outside the Finder info entry", a.Name)
	}
	if lo >= p.size() {
		p.issue(CodeAttrRange, e.ID, lo, "value of %q starts past the end of the input", a.Name)
		return nil, true
	}
	end := min(hi, p.size())
	if end < hi {
		p.issue(CodeAttrRange, e.ID, lo, "value of %q runs past the end of the input: %d of %d bytes", a.Name, end-lo, a.Length)
	}
	if end-lo > p.lim.MaxEntryBytes {
		end = lo + p.lim.MaxEntryBytes
		p.issue(CodeBudgetEntryBytes, e.ID, lo, "value of %q cut at %d of %d bytes", a.Name, end-lo, a.Length)
	}
	return p.b[lo:end:end], end < hi
}
