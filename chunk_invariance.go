package message

import (
	"bytes"
	"io"
)

// Chunk invariance (ROB-130-007, ROB-130-003).
//
// What Parse makes of a message — the tree, the bodies, the diagnostics and
// their order, Completion — must not depend on how the input arrives: whole,
// in reads of 4096 bytes, or one byte at a time. Every stage of a body is a
// stream over the one below it, and a stage that reads ahead of what its
// consumer asked for is where the delivery shows: what it finds in the bytes
// it holds ahead was reported at once, so before or after what the stages
// above it report, depending on how much one read of the input returned. The
// rules that keep it out:
//
//   - A transfer decoder reports a repair when its consumer has read up to
//     it, not when the decoder met it: a settled note is held with the
//     position in the decoded output it was settled at, a Read never returns
//     bytes past the first note held, and the Read that begins there records
//     it. So the diagnostics of a body come in the order of their positions,
//     the decoder's among those of the stages above it (a charset monitor,
//     the parse of an embedded message), as they always did when the input
//     came a byte at a time. A consumer that stops reading before a note's
//     position does not get it, however much the decoder had read.
//   - The message a carrier holds is read from its body in whole blocks
//     (ciBlocks): the buffered reader of the embedded parse reads ahead, and
//     what the carrier's stages report during a read of it would otherwise
//     fall wherever the input's reads ended.
//   - A carrier whose embedded header could not be read gets back the bytes
//     the attempt took (ciBlocks.restore), so it is a leaf with its body
//     whole, not with whatever a buffer left of it.
//   - A walk that has left an embedded multipart reads what is left of its
//     carrier's body (embeddedReader.ciRest), so the carrier's last
//     diagnostics and its place in Completion do not depend on whether a
//     buffer had fetched those bytes.
//
// textproto holds the framing to the same rule; see scanBody there.

// ciRecord records the settled note k. A note of a decoder that holds its
// output in a buffer is held, with how much of that buffer was decoded before
// it was settled, until the consumer has read those bytes. A kind is noted
// once in a body, so it is held at most once, and nothing is allocated.
func (n *cteNotes) ciRecord(k cteNote) {
	if n.ciOut == nil {
		n.ciEmit(k)
		return
	}
	n.ciHeld |= 1 << k
	n.ciAt[k] = int32(len(*n.ciOut))
}

// ciEmit records the note k as a diagnostic of the body.
//
// A sink that stops the parse while it is told is not this body's stop; see
// settle.
func (n *cteNotes) ciEmit(k cteNote) {
	info := cteNoteInfo[k]
	n.ctx.recordBody(Diagnostic{
		Code:     info.code,
		Severity: info.severity,
		Message:  info.message,
		Field:    "Content-Transfer-Encoding",
		Action:   info.action,
		Location: n.ctx.bodyLocationAt(n.at[k]),
	})
}

// ciRelease records the notes held at or before pos, how much of the
// decoder's output has been delivered: in the order of where they were
// settled, then of where they were seen, then of kind, which is the order
// settle gave them.
func (n *cteNotes) ciRelease(pos int) {
	if n == nil {
		return
	}
	for n.ciHeld != 0 {
		first := cteNoteCount
		for k := cteNote(0); k < cteNoteCount; k++ {
			if n.ciHeld&(1<<k) == 0 || int(n.ciAt[k]) > pos {
				continue
			}
			if first == cteNoteCount || n.ciAt[k] < n.ciAt[first] || (n.ciAt[k] == n.ciAt[first] && n.at[k] < n.at[first]) {
				first = k
			}
		}
		if first == cteNoteCount {
			return
		}
		n.ciHeld &^= 1 << first
		n.ciEmit(first)
	}
}

// ciTake delivers decoded bytes out[pos:] into p, up to the first note held
// past pos, after recording the notes held at pos. It returns how many.
func (n *cteNotes) ciTake(p, out []byte, pos int) int {
	if n != nil && n.ciHeld != 0 {
		n.ciRelease(pos)
		for k := cteNote(0); k < cteNoteCount && n.ciHeld != 0; k++ {
			if n.ciHeld&(1<<k) != 0 && int(n.ciAt[k]) < len(out) {
				out = out[:n.ciAt[k]]
			}
		}
	}
	return copy(p, out[pos:])
}

// ciBlockEmptyReads is how many reads in a row may return nothing before
// ciBlocks returns what it has: the count the parse calls a stall.
const ciBlockEmptyReads = maxConsecutiveEmptyReads

// ciKeepMax bounds what ciBlocks keeps of a carrier's body while the embedded
// header is read: the default header budget and one buffer. A header read
// that takes more is not given back.
const ciKeepMax = 1<<20 + 1<<16

// ciBlocks reads the body of an entity that carries a message for the parse
// of that message. Every Read fills its buffer, or ends the body: the amount
// a read of it returns, and so what the carrier's decoder and monitors have
// looked at and reported by then, depends on the bytes alone.
//
// While keep is set it also keeps what it delivers, so that a failed read of
// the embedded header can be undone.
type ciBlocks struct {
	r    io.Reader
	keep bool
	kept []byte
}

func (b *ciBlocks) Read(p []byte) (int, error) {
	total, empty := 0, 0
	for total < len(p) {
		n, err := b.r.Read(p[total:])
		n = baClampCount(n, len(p)-total)
		if b.keep && n > 0 {
			if len(b.kept)+n > ciKeepMax {
				b.keep, b.kept = false, nil
			} else {
				b.kept = append(b.kept, p[total:total+n]...)
			}
		}
		total += n
		if err != nil {
			return total, err
		}
		if n > 0 {
			empty = 0
		} else if empty++; empty >= ciBlockEmptyReads {
			break
		}
	}
	return total, nil
}

// release stops keeping: the embedded header was read.
func (b *ciBlocks) release() {
	b.keep, b.kept = false, nil
}

// ciRestore puts back the body of e, whose embedded message could not be
// read, as it was before the attempt: the bytes kept, then the rest. A parse
// that does not heal kept nothing, and neither did a header read that took
// more than ciKeepMax bytes: such a carrier stays where the attempt left it.
func (e *Entity) ciRestore(b *ciBlocks) {
	kept := b.kept
	b.release()
	if len(kept) == 0 {
		return
	}
	// The bytes given back are gated on the parse's context like any Body, so
	// a cancelled parse delivers none of them.
	e.Body = e.parse.session.gateReader(io.MultiReader(bytes.NewReader(kept), b.r))
}

// ciRest reads what the walk left of the body of an entity that carries a
// message, before it goes on to the entity's siblings: the bytes after the
// closing delimiter of an embedded multipart walked to its end, and all of
// the body when the message could not be read or was refused — walkFunc was
// given the carrier before that was known, and the multipart the carrier is
// a part of discards what is unread of it when the walk moves on. The
// carrier's decoder then reports what it finds there, in its place among the
// diagnostics, and the carrier's body counts as read, whether or not a buffer
// had fetched those bytes; read after the walk, as Drain did, the body ended
// wherever the decoder's buffer had, and its last diagnostics with it.
//
// An embedded message that is one body is left alone: reading it to its end
// reads the carrier's, and a walkFunc that did not read it keeps it. A
// walkFunc that wants the bytes of a message that cannot be read asks
// EmbeddedMessage itself and, given none, reads the carrier's Body.
func (r *embeddedReader) ciRest() {
	if r.ciRested {
		return
	}
	r.ciRested = true
	if !r.e.parse.headerLimits().Recover {
		// ProfileStrict walks as it always did.
		return
	}
	if emb := r.e.embedded; (emb == nil || emb.partsEnded) && r.e.Body != nil {
		io.Copy(io.Discard, r.e.Body)
	}
}
