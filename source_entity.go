package message

import (
	"context"
	"io"
	"strconv"
	"sync"
)

// A srcBinding is what a session parsed from a Source keeps of it: the
// Source, and what is needed to read the same input again for the index.
type srcBinding struct {
	src  *Source
	ctx  context.Context
	opts ParseOptions

	// mu guards the index: the span of every entity of the input, by path,
	// found by one more parse of the Source the first time a span is asked
	// for that the streaming parse has not reached. built is set once that
	// parse has run, whatever it found.
	mu    sync.Mutex
	built bool
	index map[string]srcSpan
}

// A srcSpan is where an entity is in the input: its header block at
// [header, body) and its body at [body, end).
type srcSpan struct{ header, body, end int64 }

// srcPathKey is a MIME path as a map key.
func srcPathKey(path []int) string {
	b := make([]byte, 0, 4*len(path))
	for _, i := range path {
		b = strconv.AppendInt(b, int64(i), 10)
		b = append(b, '.')
	}
	return string(b)
}

// lookup returns the indexed span of the entity at o, building the index
// first when the whole input can be read. A span is reported only for an
// entity the index found at the very offsets o has.
func (b *srcBinding) lookup(o entityOrigin) (srcSpan, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.built {
		n, complete := b.src.Size()
		if !complete {
			return srcSpan{}, false
		}
		b.index = b.buildIndex(n)
		b.built = true
	}
	sp, ok := b.index[srcPathKey(o.path)]
	if !ok || sp.header != o.headerOffset || sp.body != o.bodyOffset {
		return srcSpan{}, false
	}
	return sp, true
}

// buildIndex parses the n bytes of the Source once more, under the options of
// the parse it serves, and returns the span of every entity whose end that
// parse framed: the root, the parts of every multipart of the input, and the
// message an identity-encoded part carries, with its parts. It reads every
// byte of the input once, keeps none, and records nothing in the session of
// the parse it serves. Whatever stops it leaves the spans found before.
func (b *srcBinding) buildIndex(n int64) map[string]srcSpan {
	o := b.opts
	o.DiagnosticSink, o.DiscardDeliveredDiagnostics = nil, false
	o.WalkEmbeddedMessages = true
	_, root, _ := parse(b.ctx, b.src.section(0, n), &o)
	if root == nil {
		return nil
	}

	index := make(map[string]srcSpan)
	// waiting holds the entities visited whose end is not framed yet: the
	// containers the walk is inside and the part it stands on, so no more
	// of them than the tree is deep. Nothing else of the tree is kept.
	var waiting []*srcPending
	var last *srcPending
	settle := func() {
		kept := waiting[:0]
		for _, p := range waiting {
			if !p.settle(index) {
				kept = append(kept, p)
			}
		}
		for i := len(kept); i < len(waiting); i++ {
			waiting[i] = nil
		}
		waiting = kept
	}
	// The walk reads no body: moving past a part is what frames its end.
	root.Walk(func(path []int, e *Entity, _ error) error {
		p := &srcPending{key: srcPathKey(path), e: e}
		switch po := e.parse.origin; {
		case !po.known || po.unplaced:
			p.state = -1
		case len(path) == 0:
			p.end, p.state = n, 1
			index[p.key] = srcSpan{header: po.headerOffset, body: po.bodyOffset, end: n}
		default:
			// An embedded message is visited right after its carrier,
			// and ends where the carrier does.
			if last != nil && last.e.embedded == e {
				p.carrier = last
			}
			waiting = append(waiting, p)
		}
		last = p
		settle()
		return nil
	})
	settle()
	return index
}

// A srcPending is an entity of the index parse whose end is being waited
// for: a part's until the walk has moved past it, an embedded message's
// until its carrier has one.
type srcPending struct {
	key     string
	e       *Entity
	carrier *srcPending
	end     int64
	state   int8 // 0 waiting, 1 indexed, -1 has no span
}

// settle indexes p once its end is known and reports whether p is done
// with, indexed or not.
func (p *srcPending) settle(index map[string]srcSpan) bool {
	switch {
	case p.carrier != nil:
		if p.carrier.state == 0 {
			return false
		}
		if p.carrier.state < 0 {
			p.state = -1
			return true
		}
		p.end = p.carrier.end
	case p.e.wirePart != nil:
		_, end, ok := p.e.RawSpan()
		if !ok {
			return false
		}
		p.end = end
	default:
		p.state = -1
		return true
	}
	o := p.e.parse.origin
	if p.end < o.bodyOffset || o.bodyOffset < o.headerOffset {
		p.state = -1
		return true
	}
	index[p.key] = srcSpan{header: o.headerOffset, body: o.bodyOffset, end: p.end}
	p.state = 1
	return true
}

// srcPlaced reports that the entity's header block and body start where its
// origin says in the input, as the streaming parse framed them: the root of
// the input, a part a multipart of the input framed, and a message carried
// as it is by one of those (srcEmbedded). Any other placed entity — a part of
// an inferred boundary, the part standing in for an unframed body — is
// served only when the index parse framed it at the same offsets.
func (e *Entity) srcPlaced() bool {
	o := e.parse.origin
	if !o.known || o.unplaced {
		return false
	}
	return (len(o.path) == 0 && e.parse.msgDepth == 0) || e.wirePart != nil || e.srcEmbedded
}

// srcBounds returns the binding and the span of the entity in its Source.
// With needEnd false only the header block is asked for: end may be -1, and
// only the bytes up to the body's first are required to be readable, so that
// what was kept of a header block stays served however much of the body the
// parse has read since (#73).
func (e *Entity) srcBounds(needEnd bool) (*srcBinding, srcSpan, error) {
	s := e.parse.session
	if s == nil || s.raw == nil {
		return nil, srcSpan{}, ErrNoRawSource
	}
	b := s.raw
	o := e.parse.origin
	if !o.known || o.unplaced {
		return nil, srcSpan{}, ErrRawSpanUnknown
	}

	sp := srcSpan{header: o.headerOffset, body: o.bodyOffset, end: -1}
	if needEnd {
		switch {
		case len(o.path) == 0 && e.parse.msgDepth == 0:
			if n, complete := b.src.Size(); complete {
				sp.end = n
			}
		case e.wirePart != nil:
			if n, ended := e.wirePart.BodyLen(); ended {
				sp.end = o.bodyOffset + n
			}
		}
	}
	// What the streaming parse has not framed, the index may have: a part
	// not read yet, and an embedded message, whose end is its carrier's. A
	// header block needs it only when its offsets are not known to be the
	// input's.
	if (needEnd && sp.end < 0) || (!needEnd && !e.srcPlaced()) {
		found, ok := b.lookup(o)
		if !ok {
			if b.src.Truncated() {
				return nil, srcSpan{}, ErrRawNotRecorded
			}
			return nil, srcSpan{}, ErrRawSpanUnknown
		}
		if needEnd {
			sp = found
		}
	}

	if sp.header < 0 || sp.body < sp.header || (sp.end >= 0 && sp.end < sp.body) {
		return nil, srcSpan{}, ErrRawSpanUnknown
	}
	need := sp.body
	if needEnd {
		need = sp.end
	}
	if kept, _ := b.src.Size(); need > kept {
		if b.src.Truncated() {
			return nil, srcSpan{}, ErrRawNotRecorded
		}
		return nil, srcSpan{}, ErrRawSpanUnknown
	}
	return b, sp, nil
}

// RawReader returns a reader of the entity exactly as the input holds it:
// its header block and its body, the bytes Entity.RawSpan names, untouched by
// any decoding or repair. For the root that is the whole message, from the
// first byte of its header block — after an mbox envelope line the parse kept
// apart — to the end of the input.
//
// Every call returns a new, independent reader positioned at the start. It
// reads the Source of the parse, not the entity: it does not consume Body,
// is not disturbed by Body or the parts being read, and may be obtained and
// read before, while and after they are, in any order, any number of times
// (ROB-110-004, ROB-110-005, ROB-080-002). The reader may be read from
// another goroutine while the parse goes on; the call that returns it is,
// like every method of an Entity, for the goroutine that reads the parse.
//
// The error is ErrNoRawSource for an entity whose parse was given no Source,
// ErrRawSpanUnknown when the entity's bytes are not known, and
// ErrRawNotRecorded when a recorded Source did not keep them all.
//
// With a seekable Source, and with a recorded one that has been read to its
// end, the bytes of a part are known before the parse has read the part: the
// first such call parses the Source once more to find where every part ends.
// With a recorded Source still being read, a part is known once the parse
// has moved past it, as for RawSpan, and the root once the input has been
// read to its end.
func (e *Entity) RawReader() (*io.SectionReader, error) {
	b, sp, err := e.srcBounds(true)
	if err != nil {
		return nil, err
	}
	return b.src.section(sp.header, sp.end-sp.header), nil
}

// RawHeader returns a reader of the entity's header block as the input holds
// it: every byte from the first of the block to the first of the body, the
// empty line that ends the block included when the input has one. It is
// known as soon as the entity exists — for the root, a part of a multipart
// of the input and a message carried as it is by one of those — and, with a
// recorded Source, served as long as the block lies within what was kept,
// before, while and after the body is read. See RawReader for the terms.
func (e *Entity) RawHeader() (*io.SectionReader, error) {
	b, sp, err := e.srcBounds(false)
	if err != nil {
		return nil, err
	}
	return b.src.section(sp.header, sp.body-sp.header), nil
}

// RawBody returns a reader of the entity's body as the input holds it, before
// transfer decoding and charset conversion: for a part, up to and not
// including the line ending before the delimiter line that ended it; for the
// root, to the end of the input. See RawReader for the terms.
func (e *Entity) RawBody() (*io.SectionReader, error) {
	b, sp, err := e.srcBounds(true)
	if err != nil {
		return nil, err
	}
	return b.src.section(sp.body, sp.end-sp.body), nil
}

// DecodedBody returns a new reader of the entity's decoded body, read again
// from its raw bytes: the transfer decoding and charset conversion Body
// applies, set up afresh over RawBody. It delivers what Body delivers, does
// not consume or disturb Body, and may be called any number of times, in any
// order with reads of Body (ROB-110-004). For a multipart entity it is the
// raw body, as Body is.
//
// The decoding follows the entity's Header as it stands when DecodedBody is
// called, under the profile and the per-body budgets of the parse; each
// reader has the parse's total decoded budget to itself. What a repeated
// read observes is not recorded: the session's diagnostics are those of the
// one-shot reading, so read Body, or call Drain, to have them. A parse that
// has been stopped returns its stop.
//
// The errors of RawBody apply.
func (e *Entity) DecodedBody() (io.Reader, error) {
	b, sp, err := e.srcBounds(true)
	if err != nil {
		return nil, err
	}
	if stop := e.parse.fatalErr(); stop != nil {
		return nil, stop
	}
	// A session of its own takes what this reading observes, on the terms
	// of the parse: its configuration, its context, its Source.
	scratch := newParseSessionWithConfig(e.parse.session.cfg, diagnosticOptions{})
	scratch.setContext(b.ctx)
	scratch.raw = b
	ctx := e.parse
	ctx.session = scratch
	again, _ := newWithOptions(e.Header.Copy(), b.src.section(sp.body, sp.end-sp.body), e.skipTextAttachmentDecoding, ctx)
	if again == nil || again.Body == nil {
		return nil, ErrRawSpanUnknown
	}
	return again.Body, nil
}
