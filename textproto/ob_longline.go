package textproto

// Overlong header lines (ROB-130-003, corpus finding subject-5000).
//
// RFC 5322 section 2.1.1 says a line holds at most 998 characters before its
// CRLF. Nothing in this package refuses a longer one but Limits.MaxLineBytes;
// LongLine says where the first longer line of a block was, so that the parse
// can report it.

// LongLine returns the first physical line of the block h was read from that
// holds more than limit bytes before its line ending: the key of the field it
// belongs to, or "" for a line of the malformed prefix or of an orphan record,
// and its offset and length, the line ending excluded, relative to the first
// byte of the block on the terms of LineEndings. A line ends at "\r\n", at a
// bare '\n' and at a bare '\r', a '\r' that is content of a value included,
// so a line is never counted longer than the input had it.
//
// ok is false when no line is longer, for a header that was not read whole by
// this package or has been changed since (LineEndings would report nothing),
// and for a limit that is not positive. It reads only what h keeps.
func (h *Header) LongLine(limit int) (key string, start, length int64, ok bool) {
	if h == nil || h.ends == nil || limit <= 0 {
		return "", 0, 0, false
	}
	at := int64(len(h.bom.Bytes()))
	if p := h.prefix; p != nil {
		if s, n, found := obLongLineIn(p.b, p.start, limit); found {
			return "", s, n, true
		}
		at = p.start + int64(len(p.b))
	}
	orphans := h.orphans
	orphan := func(o *orphanRecord) (int64, int64, bool) {
		at = o.start + int64(len(o.b))
		return obLongLineIn(o.b, o.start, limit)
	}
	// h.l is in reverse order; the fields read from the block have ascending
	// seq in document order, and each orphan record stands before the first
	// field whose seq is greater than its after.
	for i := len(h.l) - 1; i >= 0; i-- {
		f := h.l[i]
		for len(orphans) > 0 && orphans[0].after < f.seq {
			if s, n, found := orphan(orphans[0]); found {
				return "", s, n, true
			}
			orphans = orphans[1:]
		}
		b := f.hpBytes()
		if s, n, found := obLongLineIn(b, at, limit); found {
			return f.k, s, n, true
		}
		at += int64(len(b))
	}
	for _, o := range orphans {
		if s, n, found := orphan(o); found {
			return "", s, n, true
		}
	}
	return "", 0, 0, false
}

// obLongLineIn returns the offset and length of the first line of b, which
// begins at offset base, that holds more than limit bytes before its line
// ending.
func obLongLineIn(b []byte, base int64, limit int) (start, length int64, ok bool) {
	lineStart := 0
	for i := 0; i <= len(b); i++ {
		if i < len(b) && b[i] != '\n' && b[i] != '\r' {
			continue
		}
		if n := i - lineStart; n > limit {
			return base + int64(lineStart), int64(n), true
		}
		if i < len(b) && b[i] == '\r' && i+1 < len(b) && b[i+1] == '\n' {
			i++
		}
		lineStart = i + 1
	}
	return 0, 0, false
}
