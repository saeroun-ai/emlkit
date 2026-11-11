package textproto

import "io"

// A Resumption is how ResumeAfterClose reopened an iteration.
type Resumption struct {
	// Offset is where the input after the closing delimiter line begins, on
	// the terms of Part.HeaderOffset.
	Offset int64
	// Closes is how many further closing delimiter lines, each alone among
	// blank lines, were passed over.
	Closes int
	// Part reports that a line that is neither blank nor a delimiter line
	// came first: the next NextPart returns the part that begins there, as if
	// the closing delimiter line had been an opening one. Otherwise the next
	// NextPart returns the part of the opening delimiter line that was found.
	Part bool
	// Skipped is how many bytes before that part, or that line, were passed
	// over: blank lines and the closing delimiter lines Closes counts.
	Skipped int64
}

// ResumeAfterClose reopens, under HeaderLimits.Recover, an iteration that
// NextPart ended at a closing delimiter line, for a caller that knows the line
// may close a nested multipart that declared the same boundary (ROB-060-011).
// It looks at the lines after it, within limit bytes (the default preamble
// limit for zero or below):
//
//   - blank lines are passed over, and so are up to closes closing delimiter
//     lines (any number for a negative closes);
//   - an opening delimiter line ended by a line ending reopens the iteration
//     there;
//   - a line that is neither reopens it there as a part with no delimiter
//     line of its own, provided a delimiter line follows within limit bytes;
//   - anything else — a further closing delimiter line past closes, the end
//     of the input, a failure of it, limit bytes — leaves the iteration ended.
//
// It reports whether it reopened. If it did not, the bytes it looked at are
// given back and Epilogue reads them as before. It must be called before
// Epilogue, and never otherwise changes what NextPart returns.
func (r *MultipartReader) ResumeAfterClose(limit int64, closes int) (Resumption, bool) {
	if !r.limits.Recover || !r.closed || r.epilogue != nil || r.delim != nil ||
		r.pending != nil || r.cand.active || len(r.out) > 0 {
		return Resumption{}, false
	}
	if limit <= 0 {
		limit = defaultPreambleMax
	}
	br := r.bufReader
	res := Resumption{Offset: r.epilogueAt}
	var kept []byte
	pos, content := 0, -1
	eof := false

	reopen := func(from int, part bool) (Resumption, bool) {
		r.restore(kept[from:])
		r.closed, r.done, r.epilogueAt, r.lastEnd = false, false, 0, 0
		if part {
			// The part begins here with no delimiter line of its own.
			r.delim = &provenDelimiter{}
		}
		res.Part, res.Skipped = part, int64(from)
		return res, true
	}
	fail := func() (Resumption, bool) {
		if len(kept) > 0 {
			r.restore(kept)
		}
		return Resumption{}, false
	}

	for {
		for {
			line, next, whole := nbLineAt(kept, pos, eof)
			if !whole {
				break
			}
			ok, close := r.delimiterLine(line)
			switch {
			case content >= 0 && ok:
				return reopen(content, true)
			case ok && !close && next == len(kept) && eof && !nbEndsLine(kept):
				// An opening delimiter line the input ended: nothing follows.
				return fail()
			case ok && !close:
				return reopen(pos, false)
			case ok:
				if closes >= 0 && res.Closes >= closes {
					return fail()
				}
				res.Closes++
			case content < 0 && !nbBlank(line):
				content = pos
			}
			pos = next
		}
		if eof || int64(len(kept)) >= limit {
			return fail()
		}
		if br.Buffered() == 0 {
			if _, err := br.Peek(1); err != nil {
				if err != io.EOF {
					return fail()
				}
				eof = true
				continue
			}
		}
		take := min(int64(br.Buffered()), limit-int64(len(kept)))
		b, _ := br.Peek(int(take))
		kept = append(kept, b...)
		br.Discard(int(take))
	}
}

// nbLineAt returns the line of b that begins at pos, without its line ending,
// where the next line begins, and whether the line is whole: its line ending
// is known, or eof says the input ends with it. A '\r' last in b is decided
// by the byte after it.
func nbLineAt(b []byte, pos int, eof bool) (line []byte, next int, whole bool) {
	if pos >= len(b) {
		return nil, pos, false
	}
	for i := pos; i < len(b); i++ {
		switch b[i] {
		case '\n':
			return b[pos:i], i + 1, true
		case '\r':
			switch {
			case i+1 < len(b) && b[i+1] == '\n':
				return b[pos:i], i + 2, true
			case i+1 < len(b) || eof:
				return b[pos:i], i + 1, true
			}
			return nil, pos, false
		}
	}
	if eof {
		return b[pos:], len(b), true
	}
	return nil, pos, false
}

// nbEndsLine reports whether b ends with a line ending.
func nbEndsLine(b []byte) bool {
	return len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r')
}

// nbBlank reports whether line holds nothing but spaces and tabs.
func nbBlank(line []byte) bool {
	for _, c := range line {
		if c != ' ' && c != '\t' {
			return false
		}
	}
	return true
}
