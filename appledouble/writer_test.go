package appledouble

import "encoding/binary"

// A small writer for the fixtures. No real Apple Mail sample is available
// here; these follow the published layouts (RFC 1740, Apple's AppleSingle/
// AppleDouble specification, and the ._ layout of xnu's vfs_xattr.c).

var be = binary.BigEndian

type fx struct {
	id   EntryID
	data []byte
}

// build lays out a header whose entries follow the descriptor table one
// after another, in order.
func build(magic, version uint32, filler string, entries ...fx) []byte {
	var b []byte
	b = be.AppendUint32(b, magic)
	b = be.AppendUint32(b, version)
	var fill [16]byte
	copy(fill[:], filler)
	b = append(b, fill[:]...)
	b = be.AppendUint16(b, uint16(len(entries)))
	off := headerSize + descriptorSize*len(entries)
	for _, e := range entries {
		b = be.AppendUint32(b, uint32(e.id))
		b = be.AppendUint32(b, uint32(off))
		b = be.AppendUint32(b, uint32(len(e.data)))
		off += len(e.data)
	}
	for _, e := range entries {
		b = append(b, e.data...)
	}
	return b
}

type desc struct {
	id          EntryID
	off, length uint32
}

// buildRaw writes an AppleDouble header with the descriptors as given and
// payload after the table.
func buildRaw(declared int, descs []desc, payload []byte) []byte {
	var b []byte
	b = be.AppendUint32(b, MagicDouble)
	b = be.AppendUint32(b, Version2)
	b = append(b, make([]byte, 16)...)
	b = be.AppendUint16(b, uint16(declared))
	for _, d := range descs {
		b = be.AppendUint32(b, uint32(d.id))
		b = be.AppendUint32(b, d.off)
		b = be.AppendUint32(b, d.length)
	}
	return append(b, payload...)
}

// finder returns 32 bytes of Finder info.
func finder(typ, creator string, flags uint16) []byte {
	b := make([]byte, 32)
	copy(b, typ)
	copy(b[4:], creator)
	be.PutUint16(b[8:], flags)
	return b
}

func datesEntry(v ...int32) []byte {
	var b []byte
	for _, x := range v {
		b = be.AppendUint32(b, uint32(x))
	}
	return b
}

type res struct {
	typ  string
	id   int16
	name string
	data []byte
}

// fork builds a resource fork: header, the reserved area to 256, the data,
// then the map, resources grouped by type in order of first appearance. With
// no resources it is the 286-byte empty fork macOS writes.
func fork(rs ...res) []byte {
	var types []string
	byType := map[string][]res{}
	for _, r := range rs {
		if _, ok := byType[r.typ]; !ok {
			types = append(types, r.typ)
		}
		byType[r.typ] = append(byType[r.typ], r)
	}
	var data, refs, names []byte
	tl := be.AppendUint16(nil, uint16(len(types)-1))
	refBase, count := 2+8*len(types), 0
	for _, t := range types {
		group := byType[t]
		tl = append(tl, t[:4]...)
		tl = be.AppendUint16(tl, uint16(len(group)-1))
		tl = be.AppendUint16(tl, uint16(refBase+12*count))
		for _, r := range group {
			no := uint16(0xffff)
			if r.name != "" {
				no = uint16(len(names))
				names = append(names, byte(len(r.name)))
				names = append(names, r.name...)
			}
			do := len(data)
			data = be.AppendUint32(data, uint32(len(r.data)))
			data = append(data, r.data...)
			refs = be.AppendUint16(refs, uint16(r.id))
			refs = be.AppendUint16(refs, no)
			refs = append(refs, 0, byte(do>>16), byte(do>>8), byte(do))
			refs = be.AppendUint32(refs, 0)
			count++
		}
	}
	mapLen := mapHeaderSize + len(tl) + len(refs) + len(names)
	var hdr []byte
	hdr = be.AppendUint32(hdr, 256)
	hdr = be.AppendUint32(hdr, uint32(256+len(data)))
	hdr = be.AppendUint32(hdr, uint32(len(data)))
	hdr = be.AppendUint32(hdr, uint32(mapLen))
	out := append([]byte{}, hdr...)
	reserved := make([]byte, 240)
	if len(rs) == 0 {
		copy(reserved, "This resource fork intentionally left blank   ")
	}
	out = append(out, reserved...)
	out = append(out, data...)
	out = append(out, hdr...)
	out = append(out, make([]byte, 8)...) // next map, file ref, attributes
	out = be.AppendUint16(out, mapHeaderSize)
	out = be.AppendUint16(out, uint16(mapHeaderSize+len(tl)+len(refs)))
	out = append(out, tl...)
	out = append(out, refs...)
	return append(out, names...)
}

type xattr struct {
	name  string
	value []byte
}

// macOSDouble builds a ._ file as macOS writes it: Finder info at 50 with
// the extended attributes after it, then the resource fork.
func macOSDouble(fi []byte, attrs []xattr, rsrc []byte) []byte {
	const finfoAt = headerSize + 2*descriptorSize
	var table, values []byte
	tableAt := finfoAt + attrHeaderAt + attrHeaderSize
	size := 0
	for _, a := range attrs {
		size += (attrEntryFixed + len(a.name) + 1 + 3) &^ 3
	}
	dataStart := tableAt + size
	for _, a := range attrs {
		e := be.AppendUint32(nil, uint32(dataStart+len(values)))
		e = be.AppendUint32(e, uint32(len(a.value)))
		e = be.AppendUint16(e, 0)
		e = append(e, byte(len(a.name)+1))
		e = append(e, a.name...)
		e = append(e, 0)
		for len(e)%4 != 0 {
			e = append(e, 0)
		}
		table = append(table, e...)
		values = append(values, a.value...)
	}
	total := dataStart + len(values)
	var b []byte
	b = be.AppendUint32(b, MagicDouble)
	b = be.AppendUint32(b, Version2)
	b = append(b, "Mac OS X        "...)
	b = be.AppendUint16(b, 2)
	b = be.AppendUint32(b, uint32(EntryFinderInfo))
	b = be.AppendUint32(b, finfoAt)
	b = be.AppendUint32(b, uint32(total-finfoAt))
	b = be.AppendUint32(b, uint32(EntryResourceFork))
	b = be.AppendUint32(b, uint32(total))
	b = be.AppendUint32(b, uint32(len(rsrc)))
	b = append(b, fi[:32]...)
	b = append(b, 0, 0)
	b = append(b, "ATTR"...)
	b = be.AppendUint32(b, 0) // debug tag
	b = be.AppendUint32(b, uint32(total))
	b = be.AppendUint32(b, uint32(dataStart))
	b = be.AppendUint32(b, uint32(len(values)))
	b = append(b, make([]byte, 12)...)
	b = be.AppendUint16(b, 0)
	b = be.AppendUint16(b, uint16(len(attrs)))
	b = append(b, table...)
	b = append(b, values...)
	return append(b, rsrc...)
}

// FixtureAppleMailHeader is the application/applefile part of an Apple Mail
// multipart/appledouble, for the tests of the external package.
func FixtureAppleMailHeader(name string) []byte {
	return build(MagicDouble, Version2, "",
		fx{EntryFinderInfo, finder("PDF ", "prvw", FlagHasBeenInited)},
		fx{EntryRealName, []byte(name)},
		fx{EntryResourceFork, fork(res{"STR ", 128, "Note", []byte("hi")})},
	)
}
