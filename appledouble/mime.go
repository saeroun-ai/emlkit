package appledouble

import "strings"

// A MediaKind is what a MIME media type is to this package.
type MediaKind uint8

const (
	// KindNone is a media type this package has nothing to do with.
	KindNone MediaKind = iota
	// KindAppleFile is application/applefile: an AppleSingle file on its
	// own, or the AppleDouble header part of a multipart/appledouble.
	KindAppleFile
	// KindAppleDouble is multipart/appledouble: an application/applefile
	// header part, then the data fork as a part of its own type (RFC 1740).
	KindAppleDouble
	// KindBinHex is application/mac-binhex40 (or its x- form). It is
	// recognised only; BinHex 4.0 is not decoded here.
	KindBinHex
)

// String returns the media type of the kind, or "none".
func (k MediaKind) String() string {
	switch k {
	case KindAppleFile:
		return "application/applefile"
	case KindAppleDouble:
		return "multipart/appledouble"
	case KindBinHex:
		return "application/mac-binhex40"
	}
	return "none"
}

// Recognize returns the kind of a media type, such as mail.Node.MediaType.
// Parameters after a semicolon, surrounding space and case are ignored.
func Recognize(mediaType string) MediaKind {
	mt, _, _ := strings.Cut(mediaType, ";")
	mt = strings.TrimSpace(mt)
	switch {
	case asciiEqualFold(mt, "application/applefile"):
		return KindAppleFile
	case asciiEqualFold(mt, "multipart/appledouble"):
		return KindAppleDouble
	case asciiEqualFold(mt, "application/mac-binhex40"),
		asciiEqualFold(mt, "application/x-mac-binhex40"):
		return KindBinHex
	}
	return KindNone
}

// asciiEqualFold compares under ASCII case folding only, so that no
// non-ASCII letter (the Kelvin sign, the long s) matches an ASCII one.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// AppleDoubleParts picks, from the media types of a multipart/appledouble's
// parts in order, the index of the header part — the first
// application/applefile — and of the data part — the first other part after
// it, or before it when there is none after. Either is -1 when missing.
func AppleDoubleParts(mediaTypes []string) (header, data int) {
	header, data = -1, -1
	for i, mt := range mediaTypes {
		if Recognize(mt) == KindAppleFile {
			header = i
			break
		}
	}
	for i, mt := range mediaTypes {
		if i == header || Recognize(mt) == KindAppleFile {
			continue
		}
		if data < 0 || (data < header && i > header) {
			data = i
		}
		if i > header {
			break
		}
	}
	return header, data
}

// A Description is a file as an AppleDouble pair, or an AppleSingle file,
// describes it.
type Description struct {
	// Name is the real name, empty when the header has none. It is what the
	// sender says, not a safe path.
	Name string
	// Type, Creator and Flags come from the Finder info, zero when there is
	// none.
	Type, Creator OSType
	Flags         uint16
	// Dates are the file dates, nil when absent.
	Dates *FileDates
	// Data is the data fork: the data part when one was given, otherwise the
	// header's data fork entry.
	Data []byte
	// ResourceFork is the resource fork's bytes, nil when absent.
	ResourceFork []byte
	// Attributes are the extended attributes listed with the Finder info.
	Attributes []Attribute
	// File is the parsed header, nil when it was not AppleSingle or
	// AppleDouble.
	File *File
	// Issues are the header's issues and those of combining.
	Issues []Issue
}

// Combine describes the file that the decoded bytes of an
// application/applefile part (header) and of the data part that follows it
// in a multipart/appledouble (data) make together. A nil data means there was
// no data part; an AppleSingle header then provides the data fork itself.
//
// When header is not AppleSingle or AppleDouble, Combine returns
// ErrNotAppleFile with a Description that holds data alone, still usable as
// the file's content.
func Combine(header, data []byte, lim *Limits) (*Description, error) {
	f, err := Parse(header, lim)
	if err != nil {
		return &Description{Data: data}, err
	}
	d := &Description{File: f, Data: data, Dates: f.Dates}
	if f.Name != nil {
		d.Name = f.Name.Text
	}
	if fi := f.Finder; fi != nil {
		d.Type, d.Creator, d.Flags = fi.Type, fi.Creator, fi.Flags
		d.Attributes = fi.Attributes
	}
	if f.Resource != nil {
		d.ResourceFork = f.Resource.Data
	}
	d.Issues = append(d.Issues, f.Issues...)
	fork := f.Entry(EntryDataFork)
	switch {
	case data != nil && fork != nil:
		d.Issues = append(d.Issues, Issue{Code: CodeDataConflict, Entry: EntryDataFork, Offset: int64(fork.Offset),
			Detail: "the header carries a data fork and a data part was given; the data part is used"})
	case data == nil && fork != nil:
		d.Data = f.DataFork
	case data == nil:
		d.Issues = append(d.Issues, Issue{Code: CodeMissingData, Offset: -1,
			Detail: "no data part and no data fork entry"})
	}
	return d, nil
}
