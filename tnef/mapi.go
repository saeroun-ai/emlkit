package tnef

import (
	"time"
	"unicode/utf8"
)

// MAPI property types. TypeMultiValue is a flag on the others.
const (
	TypeInt16      uint16 = 0x0002
	TypeInt32      uint16 = 0x0003
	TypeFloat32    uint16 = 0x0004
	TypeFloat64    uint16 = 0x0005
	TypeCurrency   uint16 = 0x0006
	TypeAppTime    uint16 = 0x0007
	TypeError      uint16 = 0x000A
	TypeBoolean    uint16 = 0x000B
	TypeObject     uint16 = 0x000D
	TypeInt64      uint16 = 0x0014
	TypeString8    uint16 = 0x001E
	TypeUnicode    uint16 = 0x001F
	TypeSysTime    uint16 = 0x0040
	TypeCLSID      uint16 = 0x0048
	TypeBinary     uint16 = 0x0102
	TypeMultiValue uint16 = 0x1000
)

// MAPI property identifiers the package reads.
const (
	PropMessageClass          uint16 = 0x001A
	PropSubject               uint16 = 0x0037
	PropClientSubmitTime      uint16 = 0x0039
	PropSenderName            uint16 = 0x0C1A
	PropSenderAddrType        uint16 = 0x0C1E
	PropSenderEmailAddress    uint16 = 0x0C1F
	PropRecipientType         uint16 = 0x0C15
	PropMessageDeliveryTime   uint16 = 0x0E06
	PropBody                  uint16 = 0x1000
	PropRTFCompressed         uint16 = 0x1009
	PropBodyHTML              uint16 = 0x1013
	PropInternetMessageID     uint16 = 0x1035
	PropDisplayName           uint16 = 0x3001
	PropAddrType              uint16 = 0x3002
	PropEmailAddress          uint16 = 0x3003
	PropCreationTime          uint16 = 0x3007
	PropLastModificationTime  uint16 = 0x3008
	PropAttachData            uint16 = 0x3701 // PR_ATTACH_DATA_BIN, PR_ATTACH_DATA_OBJ
	PropAttachExtension       uint16 = 0x3703
	PropAttachFilename        uint16 = 0x3704
	PropAttachMethod          uint16 = 0x3705
	PropAttachLongFilename    uint16 = 0x3707
	PropAttachMIMETag         uint16 = 0x370E
	PropAttachContentID       uint16 = 0x3712
	PropAttachContentLocation uint16 = 0x3713
	PropAttachFlags           uint16 = 0x3714
	PropSMTPAddress           uint16 = 0x39FE
	PropInternetCPID          uint16 = 0x3FDE
	PropMessageCodepage       uint16 = 0x3FFD
	PropSenderSMTPAddress     uint16 = 0x5D01
	PropAttachmentHidden      uint16 = 0x7FFE
)

// A PropertyName names a property whose identifier is 0x8000 or above: such
// identifiers are assigned per message, and the name says what they stand for.
type PropertyName struct {
	// GUID is the property set, as it is in the stream.
	GUID [16]byte
	// ID is the numeric name, used when IsString is false, and Name the
	// string name.
	ID       uint32
	Name     string
	IsString bool
}

// A Property is one MAPI property as it was read.
type Property struct {
	ID uint16
	// Type is the property type, with TypeMultiValue for a multi-valued one.
	Type uint16
	// Name is set for a named property.
	Name *PropertyName
	// Value is the raw bytes of the value — of the first one, for a
	// multi-valued property — without their padding. Integers are
	// little-endian; a TypeObject value begins with the 16-byte identifier of
	// its interface. It aliases the input.
	Value []byte
	// Values are all the values of a multi-valued property.
	Values [][]byte
	// Offset is where in the input Value begins.
	Offset int64
	// Truncated reports that the input ended inside the value.
	Truncated bool
}

// Tag returns the property tag: the identifier in the high word and the type
// in the low one, as MAPI writes them (PR_SUBJECT_W is 0x0037001F).
func (p *Property) Tag() uint32 { return uint32(p.ID)<<16 | uint32(p.Type) }

// Int returns the value of an integer, boolean or error-code property.
func (p *Property) Int() (int64, bool) {
	switch p.Type {
	case TypeInt16:
		if len(p.Value) >= 2 {
			return int64(int16(le16(p.Value))), true
		}
	case TypeBoolean:
		if len(p.Value) >= 2 {
			return int64(le16(p.Value)), true
		}
	case TypeInt32, TypeError:
		if len(p.Value) >= 4 {
			return int64(int32(le32(p.Value))), true
		}
	case TypeInt64:
		if len(p.Value) >= 8 {
			return int64(le64(p.Value)), true
		}
	}
	return 0, false
}

// Bool returns the value of a boolean or integer property: whether it is not
// zero.
func (p *Property) Bool() (bool, bool) {
	v, ok := p.Int()
	return v != 0, ok
}

// Time returns the value of a TypeSysTime property, in UTC. A zero FILETIME
// is the zero Time.
func (p *Property) Time() (time.Time, bool) {
	if p.Type != TypeSysTime || len(p.Value) < 8 {
		return time.Time{}, false
	}
	return filetime(le64(p.Value)), true
}

// Text returns the value of a string property as UTF-8: a TypeUnicode value
// is UTF-16LE, a TypeString8 value is in the given Windows code page (see
// DecodeText, whose result it reports). It is not bounded by any Limits.
func (p *Property) Text(codepage int) (string, bool) {
	switch p.Type {
	case TypeUnicode:
		return decodeUTF16(terminated(p.Value, true)), true
	case TypeString8:
		return DecodeText(terminated(p.Value, false), codepage)
	}
	return "", false
}

// Properties is a list of properties in stream order.
type Properties []Property

// Find returns the first property with the identifier id, or nil. A string
// property may be present in both its forms; Find returns whichever comes
// first.
func (ps Properties) Find(id uint16) *Property {
	for i := range ps {
		if ps[i].ID == id {
			return &ps[i]
		}
	}
	return nil
}

func (ps Properties) long(id uint16) (uint32, bool) {
	if p := ps.Find(id); p != nil {
		if v, ok := p.Int(); ok {
			return uint32(v), true
		}
	}
	return 0, false
}

func (ps Properties) time(id uint16) (time.Time, bool) {
	if p := ps.Find(id); p != nil {
		if t, ok := p.Time(); ok && !t.IsZero() {
			return t, true
		}
	}
	return time.Time{}, false
}

// cursor reads a property list. Values are padded to four bytes from the
// start of the list.
type cursor struct {
	b   []byte
	pos int
}

func (c *cursor) left() int { return len(c.b) - c.pos }

func (c *cursor) u16() (uint16, bool) {
	if c.left() < 2 {
		return 0, false
	}
	v := le16(c.b[c.pos:])
	c.pos += 2
	return v, true
}

func (c *cursor) u32() (uint32, bool) {
	if c.left() < 4 {
		return 0, false
	}
	v := le32(c.b[c.pos:])
	c.pos += 4
	return v, true
}

// take returns the next n bytes, or what is left and false.
func (c *cursor) take(n uint32) ([]byte, bool) {
	if uint64(n) > uint64(c.left()) {
		v := c.b[c.pos:len(c.b):len(c.b)]
		c.pos = len(c.b)
		return v, false
	}
	end := c.pos + int(n)
	v := c.b[c.pos:end:end]
	c.pos = end
	return v, true
}

func (c *cursor) pad() {
	c.pos = (c.pos + 3) &^ 3
	if c.pos > len(c.b) {
		c.pos = len(c.b)
	}
}

// fixedSize is how many bytes a value of a fixed-size type takes in the
// stream, padding included, and how many of them are the value.
func fixedSize(typ uint16) (stored, used uint32) {
	switch typ {
	case TypeInt16, TypeBoolean:
		return 4, 2
	case TypeInt32, TypeFloat32, TypeError:
		return 4, 4
	case TypeFloat64, TypeCurrency, TypeAppTime, TypeInt64, TypeSysTime:
		return 8, 8
	case TypeCLSID:
		return 16, 16
	}
	return 0, 0
}

func variable(typ uint16) bool {
	switch typ {
	case TypeString8, TypeUnicode, TypeBinary, TypeObject:
		return true
	}
	return false
}

// properties reads one property list from c: a count, then that many
// properties. It returns what it read and whether the list was read whole.
// base is the offset in the input of c's first byte.
func (d *decoder) properties(c *cursor, base int64, depth int) (Properties, bool) {
	count, ok := c.u32()
	if !ok {
		d.issue(CodePropertyTruncated, base+int64(c.pos), depth, "property list ends inside its count")
		return nil, false
	}
	// count is the input's word: nothing is sized by it.
	var ps Properties
	cut := func(what string) (Properties, bool) {
		d.issue(CodePropertyTruncated, base+int64(c.pos), depth, "property list of %d ends inside %s of property %d", count, what, len(ps))
		return ps, false
	}
	for i := uint32(0); i < count; i++ {
		at := base + int64(c.pos)
		if len(ps) >= d.lim.MaxProperties {
			d.issue(CodeLimitProperties, at, depth, "MaxProperties reached; the rest of the list is not read")
			return ps, false
		}
		if d.props >= d.lim.MaxTotalProperties {
			d.issue(CodeLimitProperties, at, depth, "MaxTotalProperties reached; the rest of the list is not read")
			return ps, false
		}
		if c.left() < 4 {
			return cut("the tag")
		}
		d.props++
		typ, _ := c.u16()
		id, _ := c.u16()
		p := Property{ID: id, Type: typ}

		if id >= 0x8000 {
			guid, ok := c.take(16)
			if !ok {
				return cut("the name")
			}
			kind, ok := c.u32()
			if !ok {
				return cut("the name")
			}
			name := &PropertyName{}
			copy(name.GUID[:], guid)
			switch kind {
			case 0:
				if name.ID, ok = c.u32(); !ok {
					return cut("the name")
				}
			case 1:
				n, ok := c.u32()
				if !ok {
					return cut("the name")
				}
				at := base + int64(c.pos)
				w, ok := c.take(n)
				if !ok {
					return cut("the name")
				}
				c.pad()
				name.IsString = true
				name.Name = d.nameText(w, at, depth)
			default:
				d.issue(CodePropertyType, at, depth, "named property of kind %d; the rest of the list is not read", kind)
				return ps, false
			}
			p.Name = name
		}

		elem := typ &^ TypeMultiValue
		multi := typ&TypeMultiValue != 0
		stored, used := fixedSize(elem)
		switch {
		case stored != 0 && !multi:
			p.Offset = base + int64(c.pos)
			v, ok := c.take(stored)
			if !ok {
				return cut("the value")
			}
			p.Value = v[:used:used]

		case stored != 0:
			n, ok := c.u32()
			if !ok {
				return cut("the value count")
			}
			whole := true
			if have := uint32(c.left()) / stored; n > have {
				n, whole = have, false
			}
			limited := false
			if room := uint32(d.lim.MaxTotalProperties - d.props); n > room {
				n, limited = room, true
			}
			p.Offset = base + int64(c.pos)
			if n > 0 {
				p.Values = make([][]byte, n)
				for j := range p.Values {
					v, _ := c.take(stored)
					p.Values[j] = v[:used:used]
				}
				p.Value = p.Values[0]
				d.props += int(n)
			}
			if limited {
				ps = append(ps, p)
				d.issue(CodeLimitProperties, at, depth, "MaxTotalProperties reached inside a multi-valued property")
				return ps, false
			}
			if !whole {
				p.Truncated = true
				ps = append(ps, p)
				return cut("the values")
			}

		case variable(elem):
			n, ok := c.u32()
			if !ok {
				return cut("the value count")
			}
			for j := uint32(0); j < n; j++ {
				size, ok := c.u32()
				if !ok {
					p.Truncated = true
					ps = append(ps, p)
					return cut("a value length")
				}
				off := base + int64(c.pos)
				v, whole := c.take(size)
				c.pad()
				if j == 0 {
					p.Value, p.Offset = v, off
				}
				if multi {
					if d.props >= d.lim.MaxTotalProperties {
						ps = append(ps, p)
						d.issue(CodeLimitProperties, at, depth, "MaxTotalProperties reached inside a multi-valued property")
						return ps, false
					}
					d.props++
					p.Values = append(p.Values, v)
				}
				if !whole {
					p.Truncated = true
					ps = append(ps, p)
					return cut("a value")
				}
			}

		default:
			d.issue(CodePropertyType, at, depth, "property %#04x has type %#04x, which cannot be sized; the rest of the list is not read", id, typ)
			return ps, false
		}
		ps = append(ps, p)
	}
	return ps, true
}

// nameText decodes the UTF-16LE name of a named property.
func (d *decoder) nameText(w []byte, off int64, depth int) string {
	w = terminated(w, true)
	if int64(len(w)) > d.lim.MaxStringBytes {
		w = w[:d.lim.MaxStringBytes&^1]
		d.issue(CodeLimitStringBytes, off, depth, "property name longer than MaxStringBytes; cut")
	}
	if int64(len(w)) > d.left {
		w = w[:d.grant(len(w), off, depth)&^1]
		d.left += int64(len(w))
	}
	return d.charge(decodeUTF16(w), off, depth)
}

// decodeUTF16 decodes UTF-16LE. An unpaired surrogate becomes U+FFFD and an
// odd last byte is dropped.
func decodeUTF16(b []byte) string {
	out := make([]byte, 0, len(b)/2+len(b)/8)
	for i := 0; i+1 < len(b); i += 2 {
		r := rune(le16(b[i:]))
		switch {
		case r >= 0xD800 && r < 0xDC00:
			r2 := rune(0)
			if i+3 < len(b) {
				r2 = rune(le16(b[i+2:]))
			}
			if r2 >= 0xDC00 && r2 < 0xE000 {
				r = 0x10000 + (r-0xD800)<<10 + (r2 - 0xDC00)
				i += 2
			} else {
				r = utf8.RuneError
			}
		case r >= 0xDC00 && r < 0xE000:
			r = utf8.RuneError
		}
		out = utf8.AppendRune(out, r)
	}
	return string(out)
}
