package message

import "math"

// Helpers of the bounds audit of the wire and framing code (ROB-120-001; see
// docs/robustness-notes/bounds-audit-core.md).

// baPreambleLimit returns a resolved Limits.MaxPreambleBytes in the spelling
// textproto.MultipartReader.SetPreambleLimit takes. The two differ at one
// value: a resolved budget is a positive count or LimitDisabled, which is
// negative, and SetPreambleLimit reads zero or below as its own 1 MiB default
// rather than as no limit. A disabled budget is therefore passed as
// math.MaxInt64, which that reader compares with the room left and never sums.
func baPreambleLimit(resolved int64) int64 {
	if resolved <= 0 {
		return math.MaxInt64
	}
	return resolved
}

// baClampCount returns n, the count a Read of a caller's reader reported for a
// buffer of size bytes, held to what the io.Reader contract allows: 0 to size.
// A count outside it is a broken reader, not a property of the input, but the
// buffered readers of a parse index with what they are told — bufio panics on
// a negative count and slices past its buffer on one too large — so the count
// is held to the contract where the input enters the parse. A count below zero
// is no bytes; one above size is size, the most the reader can have written.
func baClampCount(n, size int) int {
	if n < 0 {
		return 0
	}
	if n > size {
		return size
	}
	return n
}
