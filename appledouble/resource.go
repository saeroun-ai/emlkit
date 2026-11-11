package appledouble

import "encoding/binary"

// A ResourceFork is the resource fork entry: its bytes, and the resources
// its map lists. Nothing is interpreted.
type ResourceFork struct {
	// Data is the fork's bytes, aliasing the input.
	Data []byte
	// Types is how many resource types the map declares, and Resources the
	// resources it lists, within Limits.MaxResources. Both are empty when the
	// fork is empty, its map could not be walked, or the walk was turned
	// off.
	Types     int
	Resources []Resource
}

// A Resource is one entry of the resource map.
type Resource struct {
	Type OSType
	ID   int16
	// Name is the resource's name decoded as MacRoman, HasName whether it
	// has one.
	Name    string
	HasName bool
	// Attributes are the resource attribute bits.
	Attributes uint8
	// Offset is where the resource's data begins in the fork, after its
	// length, and Length the length declared there. Data is what of it the
	// fork holds, aliasing the input; Truncated reports that it is shorter
	// than Length. Offset, Length and Data are zero when the length itself
	// lies outside the fork.
	Offset    int64
	Length    uint32
	Data      []byte
	Truncated bool
}

const (
	forkHeaderSize = 16
	mapHeaderSize  = 28 // header copy, next map, file ref, attributes, two offsets
	typeEntrySize  = 8
	refEntrySize   = 12
)

func (p *parser) resourceFork(e *Entry) *ResourceFork {
	rf := &ResourceFork{Data: e.Data}
	if p.lim.MaxResources >= 0 && len(e.Data) > 0 {
		p.walkMap(e, rf)
	}
	return rf
}

// walkMap lists the resources of the fork in rf.Data. Every position is an
// int64 below 1<<34, checked against the fork or the map before use.
func (p *parser) walkMap(e *Entry, rf *ResourceFork) {
	d := rf.Data
	base := int64(e.Offset)
	size := int64(len(d))
	if size < forkHeaderSize {
		p.issue(CodeResourceMap, e.ID, base, "resource fork of %d bytes is shorter than its header", size)
		return
	}
	dataOff := int64(binary.BigEndian.Uint32(d))
	mapOff := int64(binary.BigEndian.Uint32(d[4:]))
	mapLen := int64(binary.BigEndian.Uint32(d[12:]))
	if mapOff >= size {
		p.issue(CodeResourceRange, e.ID, base, "resource map at %d lies past the end of the fork (%d bytes)", mapOff, size)
		return
	}
	mapEnd := mapOff + mapLen
	if mapEnd > size {
		p.issue(CodeResourceRange, e.ID, base+mapOff, "resource map runs past the end of the fork: %d of %d bytes", size-mapOff, mapLen)
		mapEnd = size
	}
	m := d[mapOff:mapEnd]
	mbase := base + mapOff
	if len(m) < mapHeaderSize {
		p.issue(CodeResourceMap, e.ID, mbase, "resource map of %d bytes is shorter than its header", len(m))
		return
	}
	ml := int64(len(m))
	typeList := int64(binary.BigEndian.Uint16(m[24:]))
	nameList := int64(binary.BigEndian.Uint16(m[26:]))
	if typeList+2 > ml {
		p.issue(CodeResourceRange, e.ID, mbase+typeList, "type list lies past the end of the map")
		return
	}
	if c := binary.BigEndian.Uint16(m[typeList:]); c != 0xffff { // count minus one
		rf.Types = int(c) + 1
	}
	for t := range rf.Types {
		te := typeList + 2 + typeEntrySize*int64(t)
		if te+typeEntrySize > ml {
			p.issue(CodeResourceRange, e.ID, mbase+te, "type list ends after %d of %d types", t, rf.Types)
			return
		}
		var typ OSType
		copy(typ[:], m[te:])
		count := int(binary.BigEndian.Uint16(m[te+4:])) + 1
		refList := typeList + int64(binary.BigEndian.Uint16(m[te+6:]))
		for r := range count {
			if len(rf.Resources) == p.lim.MaxResources {
				p.issue(CodeBudgetResources, e.ID, mbase+te, "resource map lists more than %d resources; the rest are not listed", p.lim.MaxResources)
				return
			}
			re := refList + refEntrySize*int64(r)
			if re+refEntrySize > ml {
				p.issue(CodeResourceRange, e.ID, mbase+re, "reference list of %s ends after %d of %d resources", typ, r, count)
				break
			}
			res := Resource{
				Type:       typ,
				ID:         int16(binary.BigEndian.Uint16(m[re:])),
				Attributes: m[re+4],
			}
			if no := binary.BigEndian.Uint16(m[re+2:]); no != 0xffff {
				p.resourceName(e, &res, m, mbase, nameList+int64(no))
			}
			do := int64(m[re+5])<<16 | int64(m[re+6])<<8 | int64(m[re+7])
			p.resourceData(e, &res, d, base, dataOff+do)
			rf.Resources = append(rf.Resources, res)
		}
	}
}

// resourceName reads the Pascal string at np of the map m.
func (p *parser) resourceName(e *Entry, res *Resource, m []byte, mbase, np int64) {
	ml := int64(len(m))
	if np >= ml {
		p.issue(CodeResourceRange, e.ID, mbase+np, "name of %s %d lies past the end of the map", res.Type, res.ID)
		return
	}
	end := np + 1 + int64(m[np])
	if end > ml {
		p.issue(CodeResourceRange, e.ID, mbase+np, "name of %s %d runs past the end of the map", res.Type, res.ID)
		end = ml
	}
	res.Name = macRoman(m[np+1 : end])
	res.HasName = true
}

// resourceData reads the length at dp of the fork d and slices what of the
// data the fork holds.
func (p *parser) resourceData(e *Entry, res *Resource, d []byte, base, dp int64) {
	size := int64(len(d))
	if dp+4 > size {
		p.issue(CodeResourceRange, e.ID, base+dp, "data of %s %d lies past the end of the fork", res.Type, res.ID)
		return
	}
	res.Length = binary.BigEndian.Uint32(d[dp:])
	res.Offset = dp + 4
	hi := res.Offset + int64(res.Length)
	end := min(hi, size)
	if end < hi {
		p.issue(CodeResourceRange, e.ID, base+dp, "data of %s %d runs past the end of the fork: %d of %d bytes", res.Type, res.ID, end-res.Offset, res.Length)
		res.Truncated = true
	}
	res.Data = d[res.Offset:end:end]
}
