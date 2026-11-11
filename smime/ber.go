package smime

import (
	"math"
	"strconv"
)

// This file is a reader of BER (X.690), the superset of DER that mail
// actually carries: Outlook and OpenSSL's streaming mode write indefinite
// lengths and split OCTET STRINGs into chunks, which encoding/asn1 rejects.
//
// The reader never copies and never allocates from a declared length. An
// element is four positions in the input. A length that runs past the bytes
// that are there yields an element clipped to them and marked truncated, so
// that a caller reads what arrived. The end of an indefinite-length element is
// found by walking its children, which is the one place the input decides how
// much work is done; depth and a count of headers read bound it.

// ASN.1 classes and the universal tags this package looks at.
const (
	classUniversal = 0
	classContext   = 2

	tagInteger         = 2
	tagOctetString     = 4
	tagNull            = 5
	tagOID             = 6
	tagSequence        = 16
	tagSet             = 17
	tagUTCTime         = 23
	tagGeneralizedTime = 24
)

// An elem is one element: where it is in the input, not what it holds.
type elem struct {
	class       uint8
	constructed bool
	indefinite  bool
	// truncated says the content was clipped: the input, or the enclosing
	// element, ended before the declared length or the end-of-contents.
	truncated bool
	tag       uint32
	// start is the identifier octet, body the first content octet, bodyEnd
	// the end of the content, and end the end of the encoding — past the
	// end-of-contents octets of an indefinite element.
	start, body, bodyEnd, end int
}

func (e elem) is(class uint8, tag uint32) bool { return e.class == class && e.tag == tag }

// isEOC reports the end-of-contents marker, 00 00.
func (e elem) isEOC() bool {
	return e.class == classUniversal && e.tag == 0 && !e.constructed && e.body == e.bodyEnd && !e.indefinite
}

type readStatus uint8

const (
	readOK  readStatus = iota
	readEnd            // the range holds no more bytes
	readBad            // no element can be read here; an issue says why
)

type issueKey struct {
	code Code
	off  int
}

type indefEnd struct {
	bodyEnd, end int
	truncated    bool
}

// A parser reads one input under one set of limits and collects its issues.
type parser struct {
	data []byte
	lim  Limits

	work      int  // element headers read so far
	exhausted bool // MaxElements was reached
	truncated bool // the input ended inside an element
	indefSeen bool

	issues     []Issue
	seen       map[issueKey]struct{}
	issuesFull bool

	// ends remembers where indefinite elements end, so that descending into
	// nested ones does not walk them again at every level.
	ends map[int]indefEnd

	// sink, when set, receives the eContent of a SignedData instead of it
	// being reassembled.
	sink    func([]byte) error
	sinkErr error
	sunk    int64

	// clips counts the places an OCTET STRING was found cut short.
	clips int
}

func newParser(data []byte, lim Limits) *parser {
	return &parser{data: data, lim: lim}
}

// issue records an observation once per code and offset, within MaxIssues.
func (p *parser) issue(code Code, sev Severity, off int, msg string) {
	if p.issuesFull {
		return
	}
	key := issueKey{code, off}
	if _, dup := p.seen[key]; dup {
		return
	}
	if len(p.issues)+1 >= p.lim.MaxIssues {
		p.issuesFull = true
		p.issues = append(p.issues, Issue{Code: CodeLimitIssues, Severity: Error, Offset: -1,
			Message: "too many issues; reporting stopped"})
		return
	}
	if p.seen == nil {
		p.seen = make(map[issueKey]struct{})
	}
	p.seen[key] = struct{}{}
	if off < 0 {
		off = -1
	}
	p.issues = append(p.issues, Issue{Code: code, Severity: sev, Offset: int64(off), Message: msg})
}

// cut reports that an element was clipped at limit: the input ended, or the
// enclosing element did.
func (p *parser) cut(pos, limit int, msg string) {
	if limit >= len(p.data) {
		p.truncated = true
		p.issue(CodeBERTruncated, Error, pos, msg)
		return
	}
	p.issue(CodeBERLength, Error, pos, msg+" (inside its enclosing element)")
}

// read reads the element that starts at pos and lies within [pos, limit).
// depth is its nesting depth, the outermost element being at depth 1.
func (p *parser) read(pos, limit, depth int) (e elem, st readStatus) {
	if limit > len(p.data) {
		limit = len(p.data)
	}
	if pos < 0 || pos >= limit {
		return e, readEnd
	}
	if p.exhausted {
		return e, readBad
	}
	if depth > p.lim.MaxDepth {
		p.issue(CodeLimitDepth, Error, pos, "elements nest deeper than "+strconv.Itoa(p.lim.MaxDepth))
		return e, readBad
	}
	p.work++
	if p.work > p.lim.MaxElements {
		p.exhausted = true
		p.issue(CodeLimitElements, Error, pos, "more than "+strconv.Itoa(p.lim.MaxElements)+" element headers read")
		return e, readBad
	}

	d := p.data
	b := d[pos]
	e.start = pos
	e.class = b >> 6
	e.constructed = b&0x20 != 0
	e.tag = uint32(b & 0x1f)
	i := pos + 1
	if e.tag == 0x1f {
		// High tag number form: base 128, most significant group first.
		var t uint64
		for n := 0; ; n++ {
			if i >= limit {
				p.cut(pos, limit, "input ends inside a tag")
				return e, readBad
			}
			if n == 5 {
				p.issue(CodeBERTag, Error, pos, "tag number longer than five octets")
				return e, readBad
			}
			c := d[i]
			i++
			t = t<<7 | uint64(c&0x7f)
			if c&0x80 == 0 {
				break
			}
		}
		if t > math.MaxUint32 {
			p.issue(CodeBERTag, Error, pos, "tag number does not fit 32 bits")
			return e, readBad
		}
		e.tag = uint32(t)
	}
	if i >= limit {
		p.cut(pos, limit, "input ends before a length")
		return e, readBad
	}
	l := d[i]
	i++
	switch {
	case l < 0x80:
		e.body = i
		if int(l) > limit-i {
			e.truncated = true
			e.bodyEnd, e.end = limit, limit
			p.cut(pos, limit, "element declares "+strconv.Itoa(int(l))+" bytes, "+strconv.Itoa(limit-i)+" remain")
			return e, readOK
		}
		e.bodyEnd = i + int(l)
		e.end = e.bodyEnd
		return e, readOK
	case l == 0xff:
		p.issue(CodeBERLengthInvalid, Error, pos, "reserved length octet 0xFF")
		return e, readBad
	case l == 0x80:
		if !e.constructed {
			p.issue(CodeBERIndefPrimitive, Error, pos, "indefinite length on a primitive element")
			return e, readBad
		}
		e.indefinite = true
		e.body = i
		p.indefinite(&e, limit, depth)
		return e, readOK
	}
	n := int(l & 0x7f)
	if n > limit-i {
		p.cut(pos, limit, "input ends inside a length")
		return e, readBad
	}
	// The length is accumulated in 64 bits with the overflow noticed before
	// the shift, and is only ever compared with the bytes that remain.
	var v uint64
	huge := false
	for k := 0; k < n; k++ {
		if v > math.MaxUint64>>8 {
			huge = true
		}
		v = v<<8 | uint64(d[i+k])
	}
	i += n
	e.body = i
	if huge || v > uint64(limit-i) {
		e.truncated = true
		e.bodyEnd, e.end = limit, limit
		decl := "more than 2^64"
		if !huge {
			decl = strconv.FormatUint(v, 10)
		}
		p.cut(pos, limit, "element declares "+decl+" bytes, "+strconv.Itoa(limit-i)+" remain")
		return e, readOK
	}
	e.bodyEnd = i + int(v)
	e.end = e.bodyEnd
	return e, readOK
}

// indefinite finds where the indefinite-length element e ends: at the
// end-of-contents marker that follows its last child.
func (p *parser) indefinite(e *elem, limit, depth int) {
	if !p.indefSeen {
		p.indefSeen = true
		p.issue(CodeBERIndefinite, Info, e.start, "BER indefinite length")
	}
	if m, ok := p.ends[e.start]; ok && m.end <= limit {
		e.bodyEnd, e.end, e.truncated = m.bodyEnd, m.end, m.truncated
		return
	}
	cur := e.body
	for {
		c, st := p.read(cur, limit, depth+1)
		if st == readOK && c.isEOC() {
			e.bodyEnd, e.end = cur, c.end
			break
		}
		if st == readOK && !c.truncated {
			cur = c.end
			continue
		}
		if st == readEnd {
			p.cut(e.start, limit, "no end-of-contents for an indefinite length")
		}
		e.truncated = true
		e.bodyEnd, e.end = limit, limit
		break
	}
	if len(p.ends) < maxIndefiniteMemo {
		if p.ends == nil {
			p.ends = make(map[int]indefEnd)
		}
		p.ends[e.start] = indefEnd{e.bodyEnd, e.end, e.truncated}
	}
}

func (p *parser) content(e elem) []byte { return p.data[e.body:e.bodyEnd] }
func (p *parser) raw(e elem) []byte     { return p.data[e.start:e.end] }

// A cursor walks the elements of one range in order.
type cursor struct {
	p        *parser
	pos, end int
	depth    int
	peeked   bool
	pe       elem
	pst      readStatus
}

// top is the cursor over the whole input.
func (p *parser) top() cursor { return cursor{p: p, end: len(p.data), depth: 1} }

// in is the cursor over the children of e, which is at the given depth.
func (p *parser) in(e elem, depth int) cursor {
	return cursor{p: p, pos: e.body, end: e.bodyEnd, depth: depth + 1}
}

func (c *cursor) peek() (elem, bool) {
	if !c.peeked {
		c.pe, c.pst = c.p.read(c.pos, c.end, c.depth)
		if c.pst == readOK && c.pe.isEOC() {
			c.p.issue(CodeBERUnexpectedEOC, Warning, c.pos, "end-of-contents inside a definite-length element")
			c.pst = readBad
		}
		c.peeked = true
	}
	return c.pe, c.pst == readOK
}

// next returns the next element. After a failure the cursor is at its end.
func (c *cursor) next() (elem, bool) {
	e, ok := c.peek()
	c.peeked = false
	if !ok {
		c.pos = c.end
		return e, false
	}
	c.pos = e.end
	return e, true
}

// take returns the next element when it has the given class and tag, and
// leaves the cursor where it was otherwise.
func (c *cursor) take(class uint8, tag uint32) (elem, bool) {
	e, ok := c.peek()
	if !ok || !e.is(class, tag) {
		return e, false
	}
	c.peeked = false
	c.pos = e.end
	return e, true
}

// walkOctets passes the octets of e to emit in order. e is an OCTET STRING in
// either form: primitive, or constructed of OCTET STRINGs that may themselves
// be constructed. e's own tag is not looked at, so an implicitly tagged one is
// read the same way. It returns false when emit asked to stop, and counts in
// p.clips every place the string was found cut short.
func (p *parser) walkOctets(e elem, depth int, emit func(b []byte) bool) bool {
	if e.truncated {
		p.clips++
	}
	if !e.constructed {
		return emit(p.content(e))
	}
	c := p.in(e, depth)
	for {
		ch, ok := c.next()
		if !ok {
			if c.pst == readBad {
				p.clips++
			}
			return true
		}
		if !ch.is(classUniversal, tagOctetString) {
			p.issue(CodeCMSChunkInvalid, Warning, ch.start, "a chunk of a constructed OCTET STRING is not an OCTET STRING")
			continue
		}
		if !p.walkOctets(ch, depth+1, emit) {
			return false
		}
	}
}

// collect returns the octets of e, at most budget of them. A primitive string
// is returned as a slice of the input; a constructed one is reassembled, the
// buffer growing with the bytes that are actually there. complete is false
// when the string was clipped by the input or by the budget.
func (p *parser) collect(e elem, depth, budget int, code Code) (out []byte, complete bool) {
	if !e.constructed {
		out = p.content(e)
		if len(out) > budget {
			p.issue(code, Error, e.start, "content longer than "+strconv.Itoa(budget)+" bytes; the rest is dropped")
			return out[:budget], false
		}
		return out, !e.truncated
	}
	out = []byte{}
	clips := p.clips
	complete = p.walkOctets(e, depth, func(b []byte) bool {
		if len(b) > budget-len(out) {
			out = append(out, b[:budget-len(out)]...)
			p.issue(code, Error, e.start, "content longer than "+strconv.Itoa(budget)+" bytes; the rest is dropped")
			return false
		}
		out = append(out, b...)
		return true
	})
	return out, complete && p.clips == clips
}

// decodeOID renders the content of an OBJECT IDENTIFIER in dotted form.
func decodeOID(b []byte) (string, bool) {
	if len(b) == 0 || len(b) > 64 || b[len(b)-1]&0x80 != 0 {
		return "", false
	}
	out := make([]byte, 0, 32)
	var v uint64
	first := true
	for _, c := range b {
		if v > math.MaxUint64>>7 {
			return "", false
		}
		v = v<<7 | uint64(c&0x7f)
		if c&0x80 != 0 {
			continue
		}
		if first {
			first = false
			switch {
			case v < 40:
				out = append(out, '0', '.')
			case v < 80:
				out = append(out, '1', '.')
				v -= 40
			default:
				out = append(out, '2', '.')
				v -= 80
			}
		} else {
			out = append(out, '.')
		}
		out = strconv.AppendUint(out, v, 10)
		v = 0
	}
	return string(out), true
}

// smallInt reads an INTEGER of at most four octets.
func smallInt(b []byte) (int, bool) {
	if len(b) == 0 || len(b) > 4 {
		return 0, false
	}
	v := int32(int8(b[0]))
	for _, c := range b[1:] {
		v = v<<8 | int32(c)
	}
	return int(v), true
}
