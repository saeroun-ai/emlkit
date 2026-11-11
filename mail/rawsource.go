package mail

import "io"

// RawSection returns a new reader of the node's bytes in src — its header
// block and body exactly as the input held them, before any decoding or
// repair — where src is the input the tree was parsed from: what
// message.ParseAt was given, or the caller's own copy. It is the raw view
// beside the decoded Data, and may be asked for any number of times
// (ROB-080-002). ok is false for a node with no span: see HasSpan.
//
// The tree's root has no span; its bytes are all of src.
func (n *Node) RawSection(src io.ReaderAt) (r *io.SectionReader, ok bool) {
	if n == nil || src == nil || !n.HasSpan || n.SpanStart < 0 || n.SpanEnd < n.SpanStart {
		return nil, false
	}
	return io.NewSectionReader(src, n.SpanStart, n.SpanEnd-n.SpanStart), true
}
