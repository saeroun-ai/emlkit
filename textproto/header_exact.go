package textproto

import (
	"bytes"
	"io"
	"net/textproto"
)

// Byte preservation under recovery (ROB-020-006, ROB-020-007).
//
// A block read with HeaderLimits.Recover keeps every byte it was read from,
// and an unmodified Header writes them back as they were:
//
//   - A field keeps two spellings when they differ. Raw is the one it always
//     was, every physical line ended by "\r\n", which is what a header that
//     has been changed is written with. RawAsRead is the input's own: a line
//     ended by a bare '\n' or a bare '\r' keeps that ending, and a last line
//     the input ended with io.EOF has none. Name case, white space around the
//     colon, folding, trailing white space and 8-bit bytes are the same in
//     both; only line endings ever differ. A field whose lines all ended with
//     "\r\n" keeps one spelling and costs nothing more.
//   - A line with an empty name (": value"), which is skipped without it, is
//     kept as an orphan record of one line, as a line with no colon or an
//     invalid name already was. It is no field and no lookup key: Get, Has,
//     Values, Fields, Len, Raw and Map never see it. Header.OrphanContinuations
//     returns it. It costs what it cost when it was skipped: one candidate of
//     MaxCount and of the shared count, and its bytes, as read, of
//     MaxLineBytes, MaxFieldBytes, MaxBytes and the shared bytes. What is kept
//     is those bytes, so the budgets that admitted them bound it. Unlike the
//     other records it changes nothing about how the lines after it are read.
//   - The empty line that ended the block is remembered as it was: "\r\n",
//     "\n", a bare "\r", or none, when the block ended at the end of the
//     input, before the first line of a body (BodySeparatorMissing) or at a
//     delimiter line of the part's multipart.
//
// # Writing
//
// A Header is intact (Intact) while its fields are exactly the ones read from
// such a block, in their order: nothing added, nothing deleted. Then
//
//   - WriteHeader writes the block's bytes exactly as they were read — byte
//     order mark, malformed prefix, fields and orphan records in their places,
//     and the empty line in the form it had — when the block ended with an
//     empty line ended by "\r\n" or "\n". Nothing written after such a block
//     can change how it is read. A block that had no empty line, or one ended
//     by a bare '\r', is written as it always was, every line ended by "\r\n"
//     and the empty line "\r\n": a body written after a block with none would
//     be read as header, and a '\n' written after a bare '\r' would be read
//     as the rest of that line ending.
//   - WriteHeaderAsRead writes the block's bytes exactly as they were read in
//     those cases too, with no empty line the input did not have. It is for a
//     caller that writes what followed the block in the input after it.
//
// A Header that is not intact — one changed since, one read without Recover,
// one a read failure cut, one built by hand — is written by both as it always
// was. Copy keeps a header intact.
//
// Whether a block's bytes were all kept is not assumed: when the block has
// been read, the bytes of everything kept are counted against the bytes
// consumed, and a block for which they differ is not intact. So a line this
// reader read past without keeping it — none is known under Recover — costs
// exactness, never correctness.
//
// # What is not exact
//
// A header read without Recover (ReadHeader, ReadHeaderLimits) is unchanged:
// its lines are written with "\r\n" and a line with an empty name is not
// written. A damaged line ended by a bare '\r' is decided as before, which
// under Recover too may refuse the block.

// An hpBlockTail is what a block read under recovery leaves its Header so that
// it can be written back byte for byte.
type hpBlockTail struct {
	// sep is the empty line that ended the block, as read: "\r\n", "\n" or
	// "\r", or "" for a block that ended without one.
	sep string

	// fields is how many fields the block held.
	fields int
}

// hpAllCRLF reports whether every physical line of the candidate just read
// ended with "\r\n", so that its kept bytes are its exact bytes.
func (s *headerScan) hpAllCRLF() bool {
	for i := 0; i < s.termsN; i++ {
		if s.termAt(i) != termCRLF {
			return false
		}
	}
	return true
}

// hpNoteSeparator records how the empty line just read, which ended the block,
// was ended. Nothing is recorded for a block the end of the input ended.
func (s *headerScan) hpNoteSeparator() {
	if s.termsN > 0 {
		s.hpSep = s.termAt(0).text()
	}
}

// hpTailOf returns the tail of the block just read whole into h, whose fields
// began base bytes into it, or nil when it was not read under recovery or the
// bytes h keeps are not all of the bytes the block consumed.
func (s *headerScan) hpTailOf(h *Header, base int64) *hpBlockTail {
	if !s.lim.Recover {
		return nil
	}
	n := base + int64(len(s.hpSep))
	if h.prefix != nil {
		n += int64(len(h.prefix.b))
	}
	for _, f := range h.l {
		n += int64(len(f.hpBytes()))
	}
	for _, o := range h.orphans {
		n += int64(len(o.b))
	}
	if n != s.consumed {
		return nil
	}
	return &hpBlockTail{sep: s.hpSep, fields: len(h.l)}
}

// hpMaterialize gives a field read under recovery exact bytes of its own,
// before a line whose ending was not "\r\n" is joined to it.
func (p *pendingField) hpMaterialize() {
	if p.hpExact && p.raw == nil {
		p.raw = bytes.Clone(p.kv)
		p.rawLast = p.lastLine
	}
}

// hpBytes returns the field's bytes as they were read, for a field read from a
// block; nil for a field that was added.
func (f *headerField) hpBytes() []byte {
	if f.orig != nil {
		return f.orig
	}
	return f.b
}

// hpReplays reports whether writing h replays the block it was read from:
// whether it is intact, and, unless asRead, whether that block ended with an
// empty line that nothing written after it can change.
func (h *Header) hpReplays(asRead bool) bool {
	t := h.hpTail
	if t == nil || len(h.l) != t.fields {
		return false
	}
	if !asRead && t.sep != "\r\n" && t.sep != "\n" {
		return false
	}
	// Fields are stored last first, and a field read from a block is never
	// modified: the ones of this block, all of them and in order, count from
	// its length down to 1. A field added since counts 0.
	for i, f := range h.l {
		if f.seq != len(h.l)-i {
			return false
		}
	}
	return true
}

// hpReplay writes the block h, which is intact, was read from.
func (h *Header) hpReplay(w io.Writer) error {
	if h.bom == BOMUTF8 {
		if _, err := io.WriteString(w, bomBytes[BOMUTF8]); err != nil {
			return err
		}
	}
	if h.prefix != nil {
		if _, err := w.Write(h.prefix.b); err != nil {
			return err
		}
	}
	next := 0
	for i := len(h.l) - 1; i >= 0; i-- {
		f := h.l[i]
		for ; next < len(h.orphans) && h.orphans[next].after < f.seq; next++ {
			if _, err := w.Write(h.orphans[next].b); err != nil {
				return err
			}
		}
		if _, err := w.Write(f.hpBytes()); err != nil {
			return err
		}
	}
	for ; next < len(h.orphans); next++ {
		if _, err := w.Write(h.orphans[next].b); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, h.hpTail.sep)
	return err
}

// Intact reports whether h is exactly a block read with HeaderLimits.Recover:
// read whole, every byte of it kept, and no field added or deleted since. Such
// a header is written back byte for byte; see WriteHeaderAsRead and
// WriteHeader. A Copy of an intact header is intact. It is false for a nil h.
func (h *Header) Intact() bool {
	return h != nil && h.hpReplays(true)
}

// RawAsRead returns the first field associated with the given key as Raw
// does, but with the line endings the input had: for a field read with
// HeaderLimits.Recover, each physical line ends as it ended in the input —
// "\r\n", a bare '\n' or a bare '\r' — and a last line the input ended with
// io.EOF has no line ending. For any other field it is Raw.
//
// The returned slice should not be modified and becomes invalid when the
// header is updated.
func (h *Header) RawAsRead(k string) ([]byte, error) {
	fields := h.m[textproto.CanonicalMIMEHeaderKey(k)]
	if len(fields) == 0 {
		return nil, nil
	}
	f := fields[len(fields)-1]
	if f.orig != nil {
		return f.orig, nil
	}
	return f.raw()
}

// WriteHeaderAsRead writes h to w as WriteHeader does, except that an intact
// header (see Header.Intact) whose block ended without an empty line — at the
// end of the input, before the first line of a body that no empty line
// separated from it, or at a delimiter line — or with one ended by a bare
// '\r' is written exactly as it was read too: no empty line added, a last
// line the input did not end left unended, a bare '\r' left bare. What it
// writes for an intact header is the block's bytes, all of them and nothing
// else.
//
// It is for replaying an input: the caller writes after it what followed the
// block there. Anything else written after such a block may be read back as
// part of the header, which is why WriteHeader does not write it this way.
func WriteHeaderAsRead(w io.Writer, h Header) error {
	if h.bom.wide() {
		return errWideHeader(h.bom)
	}
	if h.hpReplays(true) {
		return h.hpReplay(w)
	}
	return WriteHeader(w, h)
}
