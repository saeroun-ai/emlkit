package message

import "github.com/saeroun-ai/emlkit/textproto"

// An EntityOrigin is where a parse found an entity: its place in the tree, and
// its position in the input when that is known.
type EntityOrigin struct {
	// Path is the entity's MIME path, as Walk reports it: nil for the root,
	// the parent's path followed by the entity's index otherwise. An embedded
	// message is at its carrier's path followed by 0. The slice is the
	// caller's.
	Path []int

	// HasOffsets reports whether HeaderOffset and BodyOffset are positions in
	// the input. They are not for an entity read out of bytes a transfer
	// decoder produced, such as a message embedded in a Base64 part.
	HasOffsets bool

	// HeaderOffset and BodyOffset are the absolute offsets of the first byte
	// of the entity's header block and of its body, as the input held them,
	// before any decoding.
	HeaderOffset, BodyOffset int64

	// MessageDepth is how many messages the entity's own message is embedded
	// in: zero for the message that was parsed and its parts.
	MessageDepth int
}

// Origin reports where the parser found the entity. ok is false for an entity
// that was not parsed from an input — one made by New or NewMultipart — and
// for the parts of a body the caller replaced.
func (e *Entity) Origin() (o EntityOrigin, ok bool) {
	c := e.parse
	if !c.origin.known {
		return EntityOrigin{}, false
	}
	return EntityOrigin{
		Path:         c.origin.pathCopy(),
		HasOffsets:   !c.origin.unplaced,
		HeaderOffset: c.origin.headerOffset,
		BodyOffset:   c.origin.bodyOffset,
		MessageDepth: int(c.msgDepth),
	}, true
}

// RawSpan returns the half-open range [start, end) of the input that holds
// this part exactly as it was sent: its header block from the first byte
// after the delimiter line that introduced it, and its body up to, and not
// including, the line ending before the delimiter line that ended it. A
// caller that kept the input can slice it there to hash or verify the bytes
// untouched by any decoding or repair, which is what a multipart/signed
// first part needs (ROB-090-006, ROB-110-005).
//
// ok is false until the part's body has been read to its end — reading its
// Body, or moving its multipart's iterator past it, does that — and for the
// root entity, whose span is the whole input, for an entity with no known
// position, and for one not framed by a multipart of the input.
func (e *Entity) RawSpan() (start, end int64, ok bool) {
	c := e.parse
	if e.wirePart == nil || !c.origin.known || c.origin.unplaced {
		return 0, 0, false
	}
	n, ended := e.wirePart.BodyLen()
	if !ended {
		return 0, 0, false
	}
	return c.origin.headerOffset, c.origin.bodyOffset + n, true
}

// setWirePart records the framing part an entity was built from.
func (e *Entity) setWirePart(p *textproto.Part) {
	if e != nil {
		e.wirePart = p
	}
}
