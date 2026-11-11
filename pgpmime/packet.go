package pgpmime

import (
	"errors"
	"io"
)

// A Packet is one OpenPGP packet of a stream. Only the field that fits its
// Tag is set among the decoded ones, and none when the body was too short or
// of an unknown version; Issues then say why.
type Packet struct {
	Tag Tag
	// Offset is where the packet header starts in its stream, and Depth how
	// many compressed packets the stream is inside: 0 for the stream Parse
	// was given or decoded from armor. The packets of a compressed packet
	// follow it in the list, one level deeper.
	Offset int64
	Depth  int
	// NewFormat reports an RFC 4880 new-format header. Length is the body
	// length: as declared, as the sum of the chunks of a Partial body, or to
	// the end of the stream for an Indeterminate old-format length.
	// Truncated reports that the stream ended before the body did; Length is
	// then what was present.
	NewFormat     bool
	Length        int64
	Partial       bool
	Indeterminate bool
	Truncated     bool
	// Body is the body as framed, a subslice of the stream, when it is
	// contiguous: nil for a partial body.
	Body []byte

	Signature    *Signature
	OnePass      *OnePass
	EncryptedKey *EncryptedKey
	SymmetricKey *SymmetricKey
	Encrypted    *Encrypted
	Key          *Key
	UserID       string
	Literal      *Literal
	Compressed   *Compressed
}

// header is a parsed packet header.
type header struct {
	tag           Tag
	newFormat     bool
	size          int   // header octets, the first length octets included
	length        int64 // the body length, or the first chunk of a partial one
	partial       bool
	indeterminate bool
}

var errShort = errors.New("short")

// parseHeader parses the packet header at the start of b.
func parseHeader(b []byte) (h header, err error) {
	if len(b) == 0 {
		return h, errShort
	}
	c := b[0]
	if c&0x80 == 0 {
		return h, errors.New("not a packet header")
	}
	if c&0x40 != 0 {
		h.newFormat = true
		h.tag = Tag(c & 0x3f)
		l, partial, n, ok := newLength(b[1:])
		if !ok {
			return h, errShort
		}
		h.size, h.length, h.partial = 1+n, l, partial
		return h, nil
	}
	h.tag = Tag(c >> 2 & 0x0f)
	switch c & 3 {
	case 0:
		if len(b) < 2 {
			return h, errShort
		}
		h.size, h.length = 2, int64(b[1])
	case 1:
		if len(b) < 3 {
			return h, errShort
		}
		h.size, h.length = 3, int64(be16(b[1:]))
	case 2:
		if len(b) < 5 {
			return h, errShort
		}
		h.size, h.length = 5, int64(be32(b[1:]))
	default:
		h.size, h.indeterminate = 1, true
	}
	return h, nil
}

// newLength parses a new-format body length (RFC 9580 §4.2.1).
func newLength(b []byte) (length int64, partial bool, n int, ok bool) {
	if len(b) == 0 {
		return 0, false, 0, false
	}
	switch o := b[0]; {
	case o < 192:
		return int64(o), false, 1, true
	case o < 224:
		if len(b) < 2 {
			return 0, false, 0, false
		}
		return int64(o-192)<<8 + int64(b[1]) + 192, false, 2, true
	case o < 255:
		return 1 << (o & 0x1f), true, 1, true
	default:
		if len(b) < 5 {
			return 0, false, 0, false
		}
		return int64(be32(b[1:])), false, 5, true
	}
}

// chunkReader reads a packet body from its stream, following partial body
// lengths. It never reads past the stream, so a declared length that
// overruns it only ends the body early, with trunc set.
type chunkReader struct {
	data    []byte
	pos     int
	left    int64
	partial bool
	trunc   bool
}

func newChunkReader(data []byte, bodyStart int, h header) *chunkReader {
	r := &chunkReader{data: data, pos: bodyStart, left: h.length, partial: h.partial}
	if h.indeterminate {
		r.left = int64(len(data) - bodyStart)
	}
	return r
}

func (r *chunkReader) Read(p []byte) (int, error) {
	for r.left == 0 {
		if !r.partial {
			return 0, io.EOF
		}
		l, partial, n, ok := newLength(r.data[r.pos:])
		if !ok {
			r.trunc = true
			return 0, io.ErrUnexpectedEOF
		}
		r.pos += n
		r.left, r.partial = l, partial
	}
	avail := len(r.data) - r.pos
	if avail == 0 {
		r.trunc = true
		return 0, io.ErrUnexpectedEOF
	}
	n := len(p)
	if int64(n) > r.left {
		n = int(r.left)
	}
	if n > avail {
		n = avail
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	r.left -= int64(n)
	return n, nil
}

// skip moves to the end of the body and returns its length.
func (r *chunkReader) skip() int64 {
	var total int64
	for {
		if r.left == 0 {
			if !r.partial {
				return total
			}
			l, partial, n, ok := newLength(r.data[r.pos:])
			if !ok {
				r.trunc = true
				return total
			}
			r.pos += n
			r.left, r.partial = l, partial
			continue
		}
		avail := int64(len(r.data) - r.pos)
		if avail == 0 {
			r.trunc = true
			return total
		}
		n := min(r.left, avail)
		r.pos += int(n)
		r.left -= n
		total += n
	}
}

// ParsePackets walks a binary OpenPGP packet stream, such as a .gpg file or
// the Data of a Block, under lim.
func ParsePackets(data []byte, lim *Limits) ([]Packet, []Issue) {
	s := newState(lim)
	var ps []Packet
	var is []Issue
	if int64(len(data)) > s.lim.MaxInputBytes {
		s.add(&is, CodeInputLimit, s.lim.MaxInputBytes, "input of %d bytes is read only to %d", len(data), s.lim.MaxInputBytes)
		data = data[:s.lim.MaxInputBytes]
	}
	s.walk(data, 0, &ps, &is)
	return ps, is
}

// walk appends the packets of the stream data to ps.
func (s *state) walk(data []byte, depth int, ps *[]Packet, is *[]Issue) {
	pos := 0
	for pos < len(data) {
		if s.packets >= s.lim.MaxPackets {
			s.add(is, CodePacketLimit, int64(pos), "more than %d packets; the rest are not read", s.lim.MaxPackets)
			return
		}
		h, err := parseHeader(data[pos:])
		if err == errShort {
			s.add(is, CodePacketTruncated, int64(pos), "the stream ends inside a packet header")
			return
		}
		if err != nil {
			s.add(is, CodePacketInvalidHeader, int64(pos), "byte 0x%02X is not a packet header; %d bytes after it are not read", data[pos], len(data)-pos)
			return
		}
		s.packets++
		p := Packet{Tag: h.tag, Offset: int64(pos), Depth: depth, NewFormat: h.newFormat,
			Partial: h.partial, Indeterminate: h.indeterminate}
		bodyStart := pos + h.size
		r := newChunkReader(data, bodyStart, h)
		p.Length = r.skip()
		p.Truncated = r.trunc
		end := r.pos
		if !h.partial {
			p.Body = data[bodyStart:end]
		}
		if p.Truncated {
			s.add(is, CodePacketTruncated, int64(pos), "%s packet is cut short: %d body bytes present", p.Tag, p.Length)
		}
		if h.partial {
			if h.length < 512 {
				s.add(is, CodePacketPartialShort, int64(pos), "first partial body chunk of %d bytes is under 512", h.length)
			}
		}
		*ps = append(*ps, p)
		idx := len(*ps) - 1
		s.decode(data, bodyStart, h, idx, depth, ps, is)
		pos = end
	}
}

// dataPacket reports whether a tag may have a partial body length.
func dataPacket(t Tag) bool {
	switch t {
	case TagCompressed, TagSED, TagLiteral, TagSEIPD, TagAEAD:
		return true
	}
	return false
}
