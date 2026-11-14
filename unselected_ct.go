package message

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"strconv"
	"strings"

	"github.com/saeroun-ai/emlkit/textproto"
)

// The Content-Type a parse did not choose (#67, a follow-up of ROB-020-008).
//
// A header block that repeats Content-Type is read by its first field: that
// policy stands, and header.duplicate_singleton reports the repeat. When a
// later field declares a multipart whose boundary really delimits lines of
// the body, the body is also readable as that multipart, and a reader that
// takes the later field — some mail clients do — sees parts this parse does
// not, or other parts than it does. Which one the sender meant is not in the
// bytes, so nothing is re-read: the fact is reported, once per entity, as
// header.unselected_multipart.
//
// The look is a passive tap on the wire bytes of the body: under the decoders
// of a leaf, under the framing of a multipart the first field selects. It
// reads no byte that the decoders or the framing do not ask for, allocates
// nothing per byte, and looks at the first ucWindow wire bytes only — the
// sniff window of the charset stage (charsetSniff) — whatever the body's
// length. A delimiter line counts only when its line ending begins inside that
// window, and, for a multipart, only when it begins before the epilogue the
// framing does not read, so how the input is delivered cannot change the
// answer. The diagnostic is recorded when a leaf body ends with io.EOF, or
// when the iteration of a multipart's parts ends with io.EOF (or its raw Body
// is read to io.EOF), after everything recorded about the body before then.

// CodeHeaderUnselectedMultipart reports, at SeverityInfo, an entity read as
// its first Content-Type while a later Content-Type of the same header block
// declares a multipart whose boundary delimits lines of the body: in the
// first ucWindow (4096) wire bytes of the body, a line is "--", that
// boundary, "--" for a closing delimiter, then only spaces and tabs before
// its line ending. The first Content-Type is still the one read. Its Message
// names the chosen media type and the media type of the unselected
// declaration, and nothing else of the header or the body; Location is where
// that line begins. It sets Completion.Ambiguous. Recorded under Parse with
// every profile but ProfileStrict, at most once per entity.
const CodeHeaderUnselectedMultipart Code = "header.unselected_multipart"

const (
	// ucWindow is how many wire bytes of a body the look reads: the charset
	// sniff window.
	ucWindow = charsetSniff

	// ucBoundaryMax is the longest boundary a delimiter line inside the
	// window can hold: "--", the boundary and at least one line-ending byte.
	ucBoundaryMax = ucWindow - 3

	// unselectedMultipartAction is the Action, given the chosen media type.
	unselectedMultipartAction = "read the entity as %s, its first Content-Type, and did not read it as the later declaration; to read it so, remove the Content-Type fields before that declaration and parse the result again (header-recovery.md, The unselected Content-Type)"
)

// A ucNode is one node of the tree of a ucScan's boundaries that share a
// form with spaces and tabs at the end removed: the root is that form, and
// each child adds a space (0) or a tab (1). decl is the first declaration of
// the boundary the node spells, an index of ucScan.types, or -1.
type ucNode struct {
	child [2]int32
	decl  int32
}

// A ucScan looks for a delimiter line of the boundaries of the unselected
// multipart declarations of one entity in the first ucWindow wire bytes of its
// body. What it keeps is bounded by the header block, which the parse already
// holds — one entry per distinct boundary and the media type of each
// declaration — and by the window: one line prefix of at most four bytes more
// than the longest boundary, which is at most ucBoundaryMax bytes.
type ucScan struct {
	ctx    parseContext
	chosen string

	// names maps a boundary, with spaces and tabs at its end removed, to the
	// root of the tree in nodes of the boundaries that read so, which spells
	// their trailing spaces and tabs; types holds the media types of the
	// declarations. Each declaration adds at most one node per byte of its
	// trailing white space, and is found again by walking that many nodes, so
	// setting up and matching cost what the header and the line hold.
	names   map[string]int32
	nodes   []ucNode
	types   []string
	longest int

	// steps counts the nodes add walked or made, for the tests.
	steps int

	seen   int64  // wire bytes looked at, never more than ucWindow
	line   []byte // the first bytes of the line being read, up to cap(line)
	start  int64  // where the line being read begins
	junk   bool   // the line can no longer be a delimiter line
	prevCR bool   // the last byte was a '\r' that ended a line

	found int   // the declaration whose delimiter line came first, or -1
	at    int64 // where that line begins

	// over reports a byte of the body past the window; eof that the body
	// ended with io.EOF, which ends its last line when nothing was over.
	over, eof bool

	held  error // the end the decoded body returned with its last bytes
	ended bool
}

// ucWatch returns the look for an entity whose header is h, read as media
// type chosen with the boundary parameter chosenBoundary, or nil when there is
// nothing to look for: no parse, the strict profile, fewer than two
// Content-Type fields, or no later field that declares a multipart with a
// boundary a line inside the window can hold.
//
// Every Content-Type field after the first is parsed, with the standard
// grammar (mime.ParseMediaType): the header block was already admitted under
// the parse's header budgets, and the work and what is kept are bounded by it.
// A field that grammar does not read, one that is not multipart/*, one with no
// boundary or an empty one, one whose boundary holds a line ending or is too
// long for a delimiter line inside the window, and one that declares the
// chosen media type and boundary again, is not looked for.
func (c parseContext) ucWatch(h Header, chosen, chosenBoundary string) *ucScan {
	if c.session == nil || c.session.Profile() == ProfileStrict {
		return nil
	}
	fields := h.FieldsByKey("Content-Type")
	if fields.Len() < 2 {
		return nil
	}
	var s *ucScan
	fields.Next() // the chosen one
	for fields.Next() {
		t, params, err := mime.ParseMediaType(fields.Value())
		if err != nil || !strings.HasPrefix(t, "multipart/") {
			continue
		}
		b := params["boundary"]
		if len(b) == 0 || len(b) > ucBoundaryMax || strings.ContainsAny(b, "\r\n") || (t == chosen && b == chosenBoundary) {
			continue
		}
		if s == nil {
			s = &ucScan{ctx: c, chosen: chosen, found: -1}
		}
		s.add(t, b)
	}
	if s != nil {
		s.prepare()
	}
	return s
}

// add keeps the declaration of media type t with the boundary b, unless an
// earlier declaration gave the same boundary, which keeps its place.
func (s *ucScan) add(t, b string) {
	if s.names == nil {
		s.names = make(map[string]int32, 2)
	}
	key := strings.TrimRight(b, " \t")
	n, ok := s.names[key]
	if !ok {
		n = int32(len(s.nodes))
		s.nodes = append(s.nodes, ucNode{decl: -1})
		s.names[key] = n
	}
	for _, c := range []byte(b[len(key):]) {
		s.steps++
		i := ucWhite(c)
		next := s.nodes[n].child[i]
		if next == 0 {
			next = int32(len(s.nodes))
			s.nodes = append(s.nodes, ucNode{decl: -1})
			s.nodes[n].child[i] = next
		}
		n = next
	}
	if s.nodes[n].decl >= 0 {
		return
	}
	s.nodes[n].decl = int32(len(s.types))
	s.types = append(s.types, t)
	s.longest = max(s.longest, len(b))
}

// ucWhite is the child index of a space (0) or a tab (1).
func ucWhite(c byte) int {
	if c == '\t' {
		return 1
	}
	return 0
}

// prepare makes the line prefix the scan keeps: "--", the longest boundary
// and "--", what a delimiter line holds before its padding.
func (s *ucScan) prepare() {
	s.line = make([]byte, 0, min(4+s.longest, ucWindow))
}

// scan looks at p, the next wire bytes of the body, as far as the window
// reaches.
func (s *ucScan) scan(p []byte) {
	for _, c := range p {
		if s.found >= 0 {
			return
		}
		if s.seen >= ucWindow {
			s.over = true
			return
		}
		at := s.seen
		s.seen++
		switch c {
		case '\n':
			if s.prevCR {
				// The '\n' of a CRLF whose '\r' ended the line.
				s.prevCR = false
				s.start = at + 1
				continue
			}
			s.endLine()
			s.start = at + 1
		case '\r':
			s.endLine()
			s.prevCR = true
			s.start = at + 1
		default:
			s.prevCR = false
			switch {
			case s.junk:
			case len(s.line) < cap(s.line):
				s.line = append(s.line, c)
				// A line that does not begin with "--" is no delimiter line.
				if len(s.line) <= 2 && c != '-' {
					s.junk = true
				}
			case c != ' ' && c != '\t':
				// Past the longest "--boundary--": only padding may follow.
				s.junk = true
			}
		}
	}
}

// endBody notes that the body ended with io.EOF. Its last line, when it has
// no line ending and lies whole inside the window, counts only as a closing
// delimiter: that is what a healing multipart accepts there with no part
// before it. An opening delimiter cut by the end of the input ends a part
// only after an earlier delimiter line, which this look would already have
// found; as the first one, the framing keeps it as preamble.
func (s *ucScan) endBody() {
	if s.eof {
		return
	}
	s.eof = true
	if s.found < 0 && !s.over && !s.prevCR && len(s.line) > 0 {
		s.decide(true)
	}
	s.line, s.junk = s.line[:0], false
}

// endLine decides the line just ended and starts the next.
func (s *ucScan) endLine() {
	s.decide(false)
	s.line, s.junk = s.line[:0], false
}

// decide looks at the line held. The rule is the one textproto frames by:
// "--", the boundary, "--" for a closing delimiter, then only spaces and
// tabs. A line that reads as a delimiter of more than one declared boundary —
// "--x--" closes x and opens "x--", "--x  " opens "x" and "x " — names the
// declaration given first. closeOnly leaves opening delimiters out.
func (s *ucScan) decide(closeOnly bool) {
	if s.junk || len(s.line) <= 2 {
		return
	}
	content := s.line[2:]
	t := bytes.TrimRight(content, " \t")
	best := int32(-1)
	pick := func(d int32) {
		if d >= 0 && (best < 0 || d < best) {
			best = d
		}
	}
	// An opening delimiter: a boundary that is t and a prefix of the
	// padding after it.
	if n, ok := s.names[string(t)]; ok && !closeOnly {
		pick(s.nodes[n].decl)
		for _, c := range content[len(t):] {
			if n = s.nodes[n].child[ucWhite(c)]; n == 0 {
				break
			}
			pick(s.nodes[n].decl)
		}
	}
	// A closing delimiter: the boundary and "--", then padding.
	if len(t) > 2 && t[len(t)-1] == '-' && t[len(t)-2] == '-' {
		b := t[:len(t)-2]
		key := bytes.TrimRight(b, " \t")
		if n, ok := s.names[string(key)]; ok {
			for _, c := range b[len(key):] {
				if n = s.nodes[n].child[ucWhite(c)]; n == 0 {
					break
				}
			}
			if n != 0 || len(b) == len(key) {
				pick(s.nodes[n].decl)
			}
		}
	}
	if best >= 0 {
		s.found, s.at = int(best), s.start
	}
}

// end records the observation for a body that ended with err: only io.EOF,
// since a body that failed, or that a stop ended, was not read to its end.
// Every byte the look saw is the body's.
func (s *ucScan) end(err error) {
	if err == io.EOF {
		_ = s.finish(-1)
		return
	}
	s.ended = true
}

// closeParts records the observation for a multipart whose iteration, framed
// by mr, ended with io.EOF. A line at or past the end of its closing delimiter
// line is epilogue, which the framing reads only as far as its buffer went,
// and is not counted.
//
// It returns the session's stop when a DiagnosticSink stopped the parse on the
// observation, for the iterator to return in place of io.EOF.
func (s *ucScan) closeParts(mr *textproto.MultipartReader) error {
	limit := int64(-1)
	if _, at, ok := mr.Epilogue(); ok {
		limit = at
	}
	return s.finish(limit)
}

// finish records the observation once, when a delimiter line was found that
// begins before limit (any, for a negative limit), and returns the session's
// stop if recording it caused one.
func (s *ucScan) finish(limit int64) error {
	if s.ended {
		return nil
	}
	s.ended = true
	if s.found < 0 || (limit >= 0 && s.at >= limit) {
		return nil
	}
	chosen := "a media type it does not read"
	if s.chosen != "" {
		chosen = strconv.Quote(s.chosen)
	}
	// A sink that stops the parse here is the session's stop. A body has
	// ended either way and reports its own end, as obNULMonitor's does; an
	// iteration returns the stop (closeParts).
	return s.ctx.recordBody(Diagnostic{
		Code:     CodeHeaderUnselectedMultipart,
		Severity: SeverityInfo,
		Message: fmt.Sprintf("the entity is read as %s, its first Content-Type; a later Content-Type declares %q, and a line of the body is a delimiter line of its boundary",
			chosen, s.types[s.found]),
		Field:    "Content-Type",
		Action:   fmt.Sprintf(unselectedMultipartAction, chosen),
		Location: s.ctx.bodyLocationAt(s.at),
	})
}

// A ucTap passes the wire bytes of a body through unchanged and shows them to
// its look.
type ucTap struct {
	r io.Reader
	s *ucScan
}

func (t *ucTap) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	t.s.scan(p[:max(0, min(n, len(p)))])
	if err == io.EOF {
		t.s.endBody()
	}
	return n, err
}

// A ucEnd passes a body through unchanged and records the look's observation
// when it ends. An end its source returns together with bytes is held for the
// next read, as obNULMonitor holds one, so that the observation follows
// whatever the readers above it record about the body's bytes.
type ucEnd struct {
	r io.Reader
	s *ucScan
}

func (e *ucEnd) Read(p []byte) (int, error) {
	if e.s.held != nil {
		e.s.end(e.s.held)
		return 0, e.s.held
	}
	n, err := e.r.Read(p)
	if err != nil {
		if n > 0 {
			e.s.held = err
			return n, nil
		}
		e.s.end(err)
	}
	return n, err
}
