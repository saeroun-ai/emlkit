package textproto

// pfGrow returns b with room for more bytes, of at most limit in all, which
// the caller has checked they fit in.
//
// The kept preamble of a multipart is appended to a line at a time and may
// reach its limit of a megabyte or more — the whole body, when no delimiter
// line is found. Left to append, a slice that long grows by a quarter at a
// time and is copied about five times over on the way; doubled here, never
// past the limit, it is copied less than twice (ROB-120-002).
func pfGrow(b []byte, more int, limit int64) []byte {
	need := len(b) + more
	if need <= cap(b) {
		return b
	}
	size := max(2*cap(b), need, 512)
	if int64(size) > limit {
		size = max(int(limit), need)
	}
	grown := make([]byte, len(b), size)
	copy(grown, b)
	return grown
}
