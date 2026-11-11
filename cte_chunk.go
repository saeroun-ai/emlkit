package message

// The read buffer of a tolerant transfer decoder (ROB-120-002).
//
// Each decoder used to allocate its whole read buffer, cteChunk bytes, and as
// much again for its output, at the first read of a body: 8 KiB for every
// Base64 or quoted-printable part, however small. A message may hold as many
// parts as Limits.MaxPartCount admits, 10 000 by default, and one made of
// parts of a few bytes cost 80 MiB of buffers to decode a few kilobytes. A
// decoder now starts with pfChunkFirst bytes and takes the full buffer once
// the body has shown that it is longer than that.

// pfChunkFirst is the read buffer a tolerant decoder starts with.
const pfChunkFirst = 512

// pfChunk returns the buffer for a decoder's next read: in, or one of
// cteChunk bytes once consumed bytes of the body have been read through a
// smaller one that they would have filled. What in held has been decoded by
// then, so nothing is carried over.
func pfChunk(in []byte, consumed int64) []byte {
	if len(in) < cteChunk && consumed >= int64(len(in)) {
		return make([]byte, cteChunk)
	}
	return in
}
