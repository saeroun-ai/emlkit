package message

import "io"

// An entityOrigin is what the parser knows about where an entity came from:
// its position in the MIME tree of the message, and the best known byte
// positions of its header block and its body in the original input.
//
// It is owned by the parser. An entity the caller assembled with New or
// NewMultipart, and an entity whose Body the caller replaced after parsing,
// have no origin: their bytes are not the input's bytes, so any position in it
// would be a guess. The zero entityOrigin is that "not known" state, and it is
// why known exists — the root of a parsed message has a nil path, which is a
// meaningful value and has to stay distinguishable from the absent one.
//
// An origin is threaded down the tree as the parser reaches each entity, which
// for a multipart body is while the caller iterates it rather than when its
// parent was returned.
type entityOrigin struct {
	// known reports whether the parser assigned this origin. Every other field
	// is only meaningful when it is true.
	known bool

	// path is the MIME path of the entity as zero-based part indices, using
	// the same convention as WalkFunc: nil for the root entity, and the
	// parent's path followed by the entity's own index otherwise.
	//
	// The slice belongs to this entity. It is never appended to in place, so
	// no entity can reach another's path, and it leaves the entity only as a
	// copy; see pathCopy and child.
	path []int

	// headerOffset is the absolute, zero-based offset of the first byte of the
	// entity's header block in the original input. It is the position of the
	// entity itself: for a part, the first byte after the boundary delimiter
	// line that introduced it.
	headerOffset int64

	// bodyOffset is the absolute, zero-based offset of the first byte of the
	// entity's body. It describes the input as it was read, before any
	// transfer encoding or charset was decoded, and it is also the point the
	// offsets of this entity's own parts are measured from.
	bodyOffset int64

	// unplaced reports that the path is known but the offsets are not: the
	// entity was read out of bytes a transfer decoder produced — a message
	// embedded in a Base64 part — so it has a place in the tree and none in
	// the input. Its parts are unplaced too. See embedded.go.
	unplaced bool
}

// rootOrigin returns the origin of the root entity of a parsed message: a nil
// path, a header block starting at the first byte of the input, and a body
// starting at bodyOffset.
func rootOrigin(bodyOffset int64) entityOrigin {
	return rootOriginAt(0, bodyOffset)
}

// rootOriginAt returns the origin of the root entity of a parsed message whose
// header block begins headerOffset bytes into the input, after an mbox
// envelope line the parse kept apart (see mbox.go), and whose body begins at
// bodyOffset. Both are absolute positions in the input.
func rootOriginAt(headerOffset, bodyOffset int64) entityOrigin {
	return entityOrigin{known: true, headerOffset: headerOffset, bodyOffset: bodyOffset}
}

// child returns the origin of the index-th part of the entity this origin
// describes, whose header block and body begin headerOffset and bodyOffset
// bytes into that entity's body.
//
// The child's path is a fresh slice rather than an append to the parent's.
// Appending could write into an array that the parent, or a sibling already
// returned to the caller, still refers to, so each entity owns the path it was
// given and no later part can rewrite an earlier one's history.
//
// An unknown origin has unknown children: if the body being iterated is not
// known to be the input's bytes, then neither the position of what it contains
// nor its place in the parsed tree can be attributed to that input.
func (o entityOrigin) child(index int, headerOffset, bodyOffset int64) entityOrigin {
	if !o.known {
		return entityOrigin{}
	}

	path := make([]int, len(o.path)+1)
	copy(path, o.path)
	path[len(o.path)] = index

	return entityOrigin{
		known:        true,
		path:         path,
		headerOffset: o.bodyOffset + headerOffset,
		bodyOffset:   o.bodyOffset + bodyOffset,
		unplaced:     o.unplaced,
	}
}

// pathCopy returns the entity's MIME path as a slice the caller owns, so that
// what it does with it cannot reach the entity's own path.
//
// It returns nil for the root of a parsed message and for an unknown origin
// alike: a path of no indices is nil either way, and the two are told apart by
// known rather than by the path.
func (o entityOrigin) pathCopy() []int {
	if len(o.path) == 0 {
		return nil
	}
	path := make([]int, len(o.path))
	copy(path, o.path)
	return path
}

// location returns the position of the entity — the first byte of its header
// block — for a Diagnostic observed on it. It returns the zero Location, which
// reports an unknown position, for an unknown origin.
func (o entityOrigin) location() Location {
	if !o.known || o.unplaced {
		return Location{}
	}
	return LocationAt(o.headerOffset)
}

// bodyLocation returns the position of the first byte of the entity's body,
// for a Diagnostic observed while decoding it rather than on the entity
// itself. It returns the zero Location for an unknown origin.
func (o entityOrigin) bodyLocation() Location {
	if !o.known || o.unplaced {
		return Location{}
	}
	return LocationAt(o.bodyOffset)
}

// A countingReader counts the bytes read through it.
//
// It sits under the buffered reader the header parser uses, so it counts what
// that reader pulled in, which runs ahead of what the parser consumed. The
// parser subtracts what is still buffered to get its own position. Counting
// here rather than at the parse cursor is what keeps that position independent
// of how the input chunks its reads.
type countingReader struct {
	r io.Reader
	n int64 // bytes read from r so far
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
