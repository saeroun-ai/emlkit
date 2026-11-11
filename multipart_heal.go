package message

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/saeroun-ai/emlkit/textproto"
)

// Multipart recovery under Parse (ROB-060-001 to ROB-060-006, ROB-060-009,
// ROB-060-011).
//
// Every profile but ProfileStrict reads a multipart body with
// textproto.HeaderLimits.Recover, and heals what its framing lacks instead of
// ending the iteration with an error:
//
//   - multipart.close_not_found: the input ended with no closing delimiter.
//     The last part runs to that end and ends with io.EOF, and NextPart
//     returns io.EOF.
//   - multipart.close_truncated: the last delimiter line was cut short by the
//     end of the input ("--boundary-", or a prefix of "--boundary" of at least
//     six bytes) and was read as a delimiter.
//   - multipart.boundary_inferred and multipart.boundary_mismatch: no line of
//     the body is a delimiter of the declared boundary — or none was declared
//     — and the body holds delimiter lines of another: the parts are framed
//     on the boundary found. multipart.boundary_ambiguous is added when more
//     than one boundary qualified.
//   - multipart.start_not_found: no delimiter line of any boundary was found.
//     The body is returned as one part with no header, so its content is not
//     lost.
//   - multipart.empty: the body holds no part at all.
//   - multipart.nested_boundary_reused: a multipart declares the boundary of
//     a multipart it is nested in, so its parts are read as parts of that one.
//
// The bytes before the first delimiter line are kept, up to
// Limits.MaxPreambleBytes (limit.preamble_bytes reports a longer one, which
// is cut there and ends nothing); MultipartPreamble returns them. They are
// also what the inference above reads, so a body with no delimiter line that
// is longer than that budget is returned as one part holding its first
// MaxPreambleBytes bytes. The bytes after the closing delimiter line are read
// only when MultipartEpilogue asks for them; see multipart_epilogue.go.
//
// ProfileStrict, and Read, ReadWithOptions and New, frame as before.

// noBoundary stands in for a boundary that was not declared, so that the
// framing reader finds no delimiter line and keeps the body for inference. It
// holds bytes no boundary parameter can.
const noBoundary = "\x00emlkit\x00no-boundary\x00"

// A boundaryChain is the boundaries of the multiparts an entity is nested in.
type boundaryChain struct {
	b  string
	up *boundaryChain

	// nbShared is the iterator of b's: how many more closing delimiter lines
	// it may read on past, -1 for any number. See multipart_nested.go.
	nbShared int
}

func (c *boundaryChain) has(b string) bool {
	for n := 0; c != nil && n < 1024; c, n = c.up, n+1 {
		if c.b == b {
			return true
		}
	}
	return false
}

const (
	closeNotFoundAction    = "ended the multipart at the end of its input: the last part runs to that end"
	closeTruncatedAction   = "read the cut line as a delimiter line and ended the multipart there"
	boundaryInferredAction = "framed the parts on the boundary found in the body"
	startNotFoundAction    = "returned the body as one part with no header"
	startCutAction         = "returned the first bytes of the body, up to Limits.MaxPreambleBytes, as one part with no header"
	emptyMultipartAction   = "returned no part"
	boundaryReusedAction   = "framed this multipart as declared: the enclosing multipart ends each of its parts at the shared delimiter, so the parts of this one are read as parts of that one"
	preambleCutAction      = "kept the first bytes of the preamble, up to the limit, and went on looking for the first delimiter line"
)

// healEnd is called once when the framing reader has returned io.EOF under
// recovery. It records what the framing lacked, and returns the part that
// stands in for an unframed body, or again when it replaced the framing
// reader and NextPart is to be called again.
func (r *multipartReader) healEnd() (e *Entity, err error, again bool) {
	f := r.r.Framing()
	pre, over := r.r.Preamble()
	if over && !r.inferred {
		limit := int64(len(pre))
		if fatal := r.parent.recordBody(Diagnostic{
			Code:     CodeLimitPreambleBytes,
			Severity: SeverityLimitExceeded,
			Message:  fmt.Sprintf("multipart preamble exceeds the limit of %d bytes", limit),
			Action:   preambleCutAction,
		}); fatal != nil {
			return nil, fatal, false
		}
	}

	switch {
	case f.Parts > 0 && f.Unclosed:
		return nil, r.noteFraming(CodeMultipartCloseNotFound, SeverityIncomplete,
			"multipart body ends with no closing delimiter line", closeNotFoundAction), false
	case f.Parts > 0 && f.CutDelimiter:
		return nil, r.noteFraming(CodeMultipartCloseTruncated, SeverityRecovered,
			"multipart body ends inside a delimiter line", closeTruncatedAction), false
	case f.Parts > 0:
		return nil, nil, false
	case r.inferred:
		return nil, nil, false
	}

	// No delimiter line of the declared boundary introduced a part.
	if len(bytes.TrimSpace(pre)) == 0 {
		return nil, r.noteFraming(CodeMultipartEmpty, SeverityInfo, "multipart body holds no part", emptyMultipartAction), false
	}
	if pfInferredAbove(r.parent.bounds) > pfInferNestMax {
		// Inference at every level of a deep tree keeps and reads the same
		// bytes once a level; see multipart_heal_nest.go.
		e, err = r.pfUnframed(pre, over)
		return e, err, false
	}
	if found, ambiguous := inferBoundary(pre, r.declared); found != "" && !over {
		code, msg := CodeMultipartBoundaryMismatch, fmt.Sprintf("no line of the multipart body is a delimiter of the declared boundary %q; the body holds delimiter lines of %q", r.declared, found)
		if r.declared == "" {
			code, msg = CodeMultipartBoundaryInferred, fmt.Sprintf("the multipart declares no boundary; the body holds delimiter lines of %q", found)
		}
		if fatal := r.noteFraming(code, SeverityRecovered, msg, boundaryInferredAction); fatal != nil {
			return nil, fatal, false
		}
		if ambiguous {
			if fatal := r.noteFraming(CodeMultipartBoundaryAmbiguous, SeverityInfo,
				"the multipart body holds delimiter lines of more than one boundary", fmt.Sprintf("chose %q", found)); fatal != nil {
				return nil, fatal, false
			}
		}
		r.inferred = true
		r.parent.bounds = pfMarkInferred(r.parent.bounds)
		r.endNoted = false
		r.ends = textproto.LineEndings{}
		r.r = textproto.NewMultipartReader(bytes.NewReader(pre), found)
		r.r.SetHeaderLimits(r.parent.headerLimits())
		return nil, nil, true
	}
	severity, action := SeverityRecovered, startNotFoundAction
	if over {
		severity, action = SeverityIncomplete, startCutAction
	}
	if fatal := r.noteFraming(CodeMultipartStartNotFound, severity, "multipart body holds no delimiter line", action); fatal != nil {
		return nil, fatal, false
	}
	ctx := r.parent.child(r.next, 0, 0)
	if aerr := r.admitPart(ctx); aerr != nil {
		return nil, aerr, false
	}
	r.next++
	e, err = newWithOptions(Header{}, bytes.NewReader(pre), r.skipTextAttachmentDecoding, ctx)
	return e, err, false
}

// noteFraming records one diagnostic about the framing of the multipart this
// iterator reads, at its entity's path and body, and returns the session's
// stop if recording it caused one.
func (r *multipartReader) noteFraming(code Code, severity Severity, message, action string) error {
	return r.parent.recordBody(Diagnostic{
		Code:     code,
		Severity: severity,
		Message:  message,
		Field:    "Content-Type",
		Action:   action,
	})
}

// boundaryCandidates bounds how many distinct boundaries inferBoundary tracks.
const boundaryCandidates = 64

// inferBoundary looks in body, a multipart body in which no line is a
// delimiter of the declared boundary, for the boundary its delimiter lines
// actually use (ROB-060-002 to ROB-060-004). It returns the one that
// qualifies best — the earliest among equals — and whether another
// qualified, or "" when none does.
//
// A candidate is the text after "--" of a line that begins with it, of 1 to
// 70 bytes with a letter or digit among them, white space at the line's end
// dropped. It qualifies when it introduces at least one part and either a
// line "--candidate--" closes it or a part it introduces begins with a header
// field or an empty line and it is seen more than once or that field is a
// Content-* field. A candidate that differs from the declared boundary only
// in case or surrounding white space is preferred.
func inferBoundary(body []byte, declared string) (boundary string, ambiguous bool) {
	type stat struct {
		opens, closes int
		headed, mime  bool
		first         int
		order         int
	}
	stats := make(map[string]*stat)
	get := func(name []byte, at int) *stat {
		if s, ok := stats[string(name)]; ok {
			return s
		}
		if len(stats) >= boundaryCandidates {
			return nil
		}
		s := &stat{first: at, order: len(stats)}
		stats[string(name)] = s
		return s
	}
	valid := func(name []byte) bool {
		if len(name) == 0 || len(name) > 70 {
			return false
		}
		for _, c := range name {
			if c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
				return true
			}
		}
		return false
	}

	for pos := 0; pos < len(body); {
		line, next := cutLineAt(body, pos)
		at := pos
		pos = next
		if len(line) < 3 || line[0] != '-' || line[1] != '-' {
			continue
		}
		name := bytes.TrimRight(line[2:], " \t")
		if !valid(name) {
			continue
		}
		if s := get(name, at); s != nil {
			s.opens++
			// What the part it would introduce begins with.
			if after, _ := cutLineAt(body, next); next < len(body) {
				switch {
				case len(after) == 0:
					s.headed = true
				case fieldLine(after):
					s.headed = true
					if len(after) > 8 && strings.EqualFold(string(after[:8]), "content-") {
						s.mime = true
					}
				}
			}
		}
		if n := len(name); n > 2 && name[n-1] == '-' && name[n-2] == '-' && valid(name[:n-2]) {
			if s := get(name[:n-2], at); s != nil {
				s.closes++
			}
		}
	}

	want := strings.TrimSpace(declared)
	best, bestScore, qualified := "", 0, 0
	var bestStat *stat
	for name, s := range stats {
		if s.opens == 0 {
			continue
		}
		score := 0
		if s.closes > 0 {
			score += 4
		}
		if s.headed && (s.opens > 1 || s.mime) {
			score += 2
		}
		if s.mime {
			score++
		}
		if score < 2 {
			continue
		}
		if want != "" && strings.EqualFold(want, name) {
			score += 8
		}
		// "--name--" is also read as an opening of "name--"; it is not a
		// second boundary.
		if strings.HasSuffix(name, "--") {
			if o, ok := stats[name[:len(name)-2]]; ok && o.opens > 0 {
				continue
			}
		}
		qualified++
		if best == "" || score > bestScore || (score == bestScore && s.first < bestStat.first) {
			best, bestScore, bestStat = name, score, s
		}
	}
	return best, qualified > 1
}

// cutLineAt returns the line of b that begins at pos, without its line
// ending, and where the next line begins.
func cutLineAt(b []byte, pos int) (line []byte, next int) {
	if pos >= len(b) {
		return nil, len(b)
	}
	i := bytes.IndexAny(b[pos:], "\r\n")
	if i < 0 {
		return b[pos:], len(b)
	}
	end := pos + i
	next = end + 1
	if b[end] == '\r' && next < len(b) && b[next] == '\n' {
		next++
	}
	return b[pos:end], next
}

// fieldLine reports whether line begins with a field name and a colon.
func fieldLine(line []byte) bool {
	i := bytes.IndexByte(line, ':')
	if i <= 0 || i > 76 {
		return false
	}
	for _, c := range bytes.TrimRight(line[:i], " \t") {
		if c <= ' ' || c >= 0x7f {
			return false
		}
	}
	return true
}

// MultipartPreamble returns the bytes that preceded the first delimiter line
// of the multipart body mr iterates — RFC 2046's preamble — as they were
// read, and whether they were cut at Limits.MaxPreambleBytes (ROB-060-001).
// ok is false for an iterator that keeps none: one not from Parse, one under
// ProfileStrict, or one over a body assembled with NewMultipart. The preamble
// is complete once NextPart has returned its first part or its end.
func MultipartPreamble(mr MultipartReader) (preamble []byte, truncated bool, ok bool) {
	r, isReader := mr.(*multipartReader)
	if !isReader || !r.healing || r.inferred {
		return nil, false, false
	}
	b, over := r.r.Preamble()
	return bytes.Clone(b), over, true
}
