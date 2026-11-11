package message

import (
	"bytes"
	"fmt"
)

// A bound on nested boundary inference (ROB-120-002).
//
// A multipart whose declared boundary frames nothing has its body kept, up to
// Limits.MaxPreambleBytes, and framed again on the boundary found in it
// (multipart_heal.go). A part framed that way may be such a multipart itself,
// and so may a part of it: each level then keeps what is left of the same
// bytes and reads it once more. A message of 900 KiB nested to the default
// MaxMIMEDepth that way cost 100 copies of itself in memory at once, five
// times that in allocations, and the time to read 90 MiB.
//
// Inference is therefore tried for a multipart that at most pfInferNestMax
// multiparts framed by inference enclose. A deeper one is not searched: its
// body is returned as one part with no header, as the body of a multipart in
// which no delimiter line is found is, so nothing of it is lost, and
// multipart.start_not_found (SeverityIncomplete) says that the parts inside
// were not read. What a parse keeps for inference at one time is then at most
// pfInferNestMax+2 times MaxPreambleBytes, whatever the depth.
//
// Mail that lost one boundary parameter has one such level, and mail from a
// producer that writes none has as many as it nests multiparts, which is two
// or three. Multiparts framed as they declare do not count, however deep.

// pfInferNestMax is how many multiparts framed by inference may enclose a
// multipart that is framed by inference too.
const pfInferNestMax = 8

// pfInferredMark is the entry of a boundaryChain that marks the multipart
// below it as framed on an inferred boundary. It holds bytes no boundary a
// producer writes holds.
const pfInferredMark = "\x00emlkit\x00inferred\x00"

// pfMarkInferred returns c with a mark that the multipart it ends with was
// framed by inference, for the parts read from then on.
func pfMarkInferred(c *boundaryChain) *boundaryChain {
	return &boundaryChain{b: pfInferredMark, up: c}
}

// pfInferredAbove counts the multiparts in c that were framed by inference.
func pfInferredAbove(c *boundaryChain) int {
	count := 0
	for n := 0; c != nil && n < 1024; c, n = c.up, n+1 {
		if c.b == pfInferredMark {
			count++
		}
	}
	return count
}

// pfUnframed returns pre, the kept body of a multipart nested in too many
// inferred ones to be searched for a boundary, as one part with no header,
// and records why. over reports that the body was longer than what was kept.
func (r *multipartReader) pfUnframed(pre []byte, over bool) (*Entity, error) {
	action := startNotFoundAction
	if over {
		action = startCutAction
	}
	if fatal := r.noteFraming(CodeMultipartStartNotFound, SeverityIncomplete,
		fmt.Sprintf("no line of the multipart body is a delimiter line of a declared boundary, and %d enclosing multiparts were already framed on a boundary found in their bodies; none is looked for in this one", pfInferNestMax),
		action); fatal != nil {
		return nil, fatal
	}
	ctx := r.parent.child(r.next, 0, 0)
	if aerr := r.admitPart(ctx); aerr != nil {
		return nil, aerr
	}
	r.next++
	return newWithOptions(Header{}, bytes.NewReader(pre), r.skipTextAttachmentDecoding, ctx)
}
