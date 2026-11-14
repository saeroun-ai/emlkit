package mail

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/textproto"
)

// The structure view (ROB-100-001 to ROB-100-005, ROB-090-004 to ROB-090-006,
// ROB-090-008).
//
// Reader flattens a message into text parts and attachments. ReadTree keeps
// the whole MIME tree instead: every entity is a Node at its place, with what
// it declares, how it was classified and why, and its decoded body kept up to
// a budget. The views a caller usually wants — the body in the form it
// prefers, the attachments, the resource an HTML body refers to by cid: — are
// then questions asked of the tree, and none of them discards what it does
// not choose.

// A Role is what a Node is in its message.
type Role uint8

const (
	// RoleContainer is a multipart entity: it has Children and no body.
	RoleContainer Role = iota
	// RoleBody is text meant to be read as the message, or one alternative
	// form of it.
	RoleBody
	// RoleAttachment is a part to be saved or opened apart from the message.
	RoleAttachment
	// RoleRelated is a resource of a multipart/related that its root part may
	// refer to by Content-ID or Content-Location, such as an inline image.
	RoleRelated
	// RoleMessage is an entity that carries a whole message, or the header
	// block of one. With TreeOptions.DescendMessages that message is its one
	// child.
	RoleMessage
	// RoleSignature is the signature part of a multipart/signed.
	RoleSignature
	// RoleEncrypted is a part of a multipart/encrypted: its control part or
	// its encrypted payload.
	RoleEncrypted
	// RoleReport is the machine-readable part of a multipart/report.
	RoleReport
)

// String returns the lower-case name of the role.
func (r Role) String() string {
	switch r {
	case RoleContainer:
		return "container"
	case RoleBody:
		return "body"
	case RoleAttachment:
		return "attachment"
	case RoleRelated:
		return "related"
	case RoleMessage:
		return "message"
	case RoleSignature:
		return "signature"
	case RoleEncrypted:
		return "encrypted"
	case RoleReport:
		return "report"
	}
	return fmt.Sprintf("Role(%d)", uint8(r))
}

// A Node is one entity of a message's MIME tree.
type Node struct {
	// Path is the entity's MIME path: nil for the root.
	Path []int
	// Parent is the node this one is a child of, nil for the root.
	Parent *Node
	// Children are the parts of a multipart, in order, or the one message an
	// entity carries when it was descended into.
	Children []*Node

	// Header is the entity's header, as it was read.
	Header message.Header
	// MediaType and Params are the Content-Type as the parser interpreted it,
	// lower-cased; "text/plain" for an entity that declares none.
	MediaType string
	Params    map[string]string
	// Disposition and DispositionParams are the Content-Disposition, or empty.
	Disposition       string
	DispositionParams map[string]string
	// Filename is the name the entity gives its content: the first of its
	// candidates. It is what the message says, not a safe path; see
	// message.SafeFilename. Filenames are all the candidates — the
	// disposition's filename and the content type's name, in their RFC 2231
	// and plain forms — the chosen one first; they differ when the message
	// names the content in more than one way.
	Filename  string
	Filenames []message.FilenameCandidate
	// ContentID is the Content-ID without angle brackets, and ContentLocation
	// the Content-Location, each empty when absent.
	ContentID       string
	ContentLocation string

	// Role is what the node is in its message, and Reason why it was
	// classified so.
	Role   Role
	Reason string

	// Data is the decoded body of a leaf, up to TreeOptions.MaxBodyBytes. Size
	// is how many decoded bytes the body held in all, Truncated whether Data
	// is only the first of them, and SHA256 the digest of all of them. They
	// are zero for a container, and for a carrier that was descended into.
	Data      []byte
	Size      int64
	Truncated bool
	SHA256    [sha256.Size]byte
	// DuplicateOf is an earlier leaf of the tree with the same non-empty
	// content, or nil.
	DuplicateOf *Node

	// Warning is the decoding warning the entity came with — an unknown
	// transfer encoding or charset — and Err what ended the reading of its
	// body or of its parts, other than their end. Either may be nil.
	Warning error
	Err     error

	// Preamble and Epilogue are a multipart's bytes before its first delimiter
	// line and after its closing one, as read (message.MultipartPreamble and
	// message.MultipartEpilogue), and the Truncated flags whether each was cut
	// at its Limits budget, MaxPreambleBytes or MaxEpilogueBytes, which bound
	// them; TreeOptions.MaxTotalBytes does not count them. Both are nil for a
	// leaf and for a multipart that was not read by a healing parse, the
	// preamble for one framed on a boundary inferred from its body, and the
	// epilogue for one that was never closed. Reading the epilogue of the
	// message's own multipart under ProfileForensic reads it through
	// ParseSession.TrailingInputReader, which then delivers only what follows.
	// See tree_epilogue.go.
	Preamble, Epilogue                   []byte
	PreambleTruncated, EpilogueTruncated bool

	// HasSpan reports that SpanStart and SpanEnd are the half-open range of
	// the input that holds this part as it was sent; see
	// message.Entity.RawSpan.
	HasSpan            bool
	SpanStart, SpanEnd int64

	// Signed is the multipart/signed node whose signed content this node is
	// in: set on its first part and on every node under that part, to the
	// innermost such container when signed entities nest, and nil outside any
	// signed content. A container and its signature part are not its own
	// signed content: they have the Signed of whatever they are in. See
	// tree_signed.go (ROB-120-006).
	Signed *Node

	// Repaired reports that the parse did not read this entity's bytes as they
	// stand: a diagnostic recorded against it repaired, guessed, cut, refused
	// or lost something — any severity but SeverityInfo and
	// SeverityUnsupported — or its reading ended with Err. RepairCodes are the
	// codes of those diagnostics, each once, in the order the parse found
	// them. The decoded view of a Repaired node is not what its raw bytes say.
	Repaired    bool
	RepairCodes []message.Code

	// Unverifiable is set on a multipart/signed node when its signature cannot
	// be checked against what the tree shows, and UnverifiableReason says why:
	// something within the signed content or the signature part was repaired,
	// the signed part has no raw span, the container's own framing or header
	// was repaired, its protocol or micalg is missing or disagrees with the
	// signature part, or what the parse reported is not whole. A signature is
	// verified over the raw span of Children[0], never over Data; and a
	// signature over a node that is Unverifiable, even one that checks out
	// over the raw bytes, does not vouch for the decoded view: treat the
	// content as unverified, never as verified with repairs.
	Unverifiable       bool
	UnverifiableReason string

	// budget is the parse's spool budget the node's body counts against
	// (spool_limits.go); nil for a node of an entity no session saw, or one
	// built by hand.
	budget *spoolBudget
}

// TreeOptions configures ReadTree. The zero value, and nil, are the defaults.
type TreeOptions struct {
	// MaxBodyBytes is how many decoded bytes of each leaf body are kept in
	// Node.Data. The rest is read, counted and hashed, and not kept. Zero
	// keeps 1 MiB; a negative value keeps nothing.
	MaxBodyBytes int64

	// MaxTotalBytes is how many bytes all Node.Data of one tree may hold
	// together. Zero is 64 MiB; a negative value is no limit.
	MaxTotalBytes int64

	// DescendMessages reads the message a message/rfc822 or message/global
	// entity carries as its child, with a tree of its own, instead of keeping
	// it as one leaf. Limits.MaxMessageDepth of the parse bounds the nesting.
	DescendMessages bool

	// Body, when non-nil, is given each leaf's decoded body to read instead
	// of ReadTree keeping it: Node.Data stays empty, and Size and SHA256 are
	// what the function read. What it leaves unread is discarded. An error it
	// returns ends ReadTree.
	Body func(n *Node, body io.Reader) error
}

// A Tree is the structure of one message.
type Tree struct {
	// Root is the message itself.
	Root *Node
	// Diagnostics are what reading the structure found about the shape of
	// signed, encrypted, report and unsupported entities, the
	// multipart.trailing_junk note of an epilogue that holds more than white
	// space (tree_epilogue.go), and the signed.unverifiable and
	// dkim.view_repaired notes of tree_signed.go. They are apart from the
	// parse's own diagnostics, which its ParseSession holds.
	Diagnostics []message.Diagnostic

	// HeaderRepaired reports that the message's own header block was not read
	// as it stands: a diagnostic recorded against the root changed which lines
	// form its fields, where it ends or how a field's value was read, or the
	// block repeats a field allowed once, which the parser reads from its
	// first occurrence and a DKIM verifier from its last (RFC 6376 section
	// 5.4.2). HeaderRepairFields are the fields those diagnostics name, each
	// once; a repair that names none adds nothing to it.
	//
	// BodyRepaired reports the same of the message's body: its framing or
	// decoding was repaired at the root, or anything was repaired in an entity
	// under it, or the tree was not read to its end.
	//
	// Either one means that a DKIM or ARC result computed from the parsed
	// view — header fields picked from Header, a body put back together from
	// the nodes — cannot be trusted, and that a result computed over the raw
	// input does not vouch for what the tree shows. DKIM is verified over the
	// input as it was received.
	HeaderRepaired     bool
	HeaderRepairFields []string
	BodyRepaired       bool

	// RepairsUnknown reports that the parse produced diagnostics it did not
	// keep — a DiagnosticSink with DiscardDeliveredDiagnostics, or the
	// Limits.MaxDiagnosticCount budget — among which a repair could be. Nothing
	// can then be ruled out: HeaderRepaired and BodyRepaired are set, and
	// every multipart/signed is Unverifiable.
	RepairsUnknown bool
}

const (
	defaultTreeBody  = 1 << 20
	defaultTreeTotal = 64 << 20
)

// ReadTree reads the whole MIME tree of e, consuming it, and returns its
// structure. It reads every body to its end, so that the size and digest of
// each are known and each multipart is framed to its close.
//
// A defect of one entity does not end the reading: it is kept in that node's
// Warning or Err and the siblings are read. ReadTree returns an error only
// when the parse was stopped (message.Entity.ParseStop), or TreeOptions.Body
// returned one; the tree read so far is returned with it.
//
// The parse's diagnostics, read through e.ParseSession, are mapped to the
// nodes they were recorded against: Node.Repaired, Node.Signed and, for each
// multipart/signed, Node.Unverifiable; for the message, Tree.HeaderRepaired
// and Tree.BodyRepaired. A signature is verified over the raw bytes — the
// span of a signed part, the input for DKIM — and a repaired signed view is
// unverified, never verified with repairs. An entity of Read, ReadWithOptions
// or New has no session, heals nothing and reports only Err.
func ReadTree(e *message.Entity, opts *TreeOptions) (*Tree, error) {
	var o TreeOptions
	if opts != nil {
		o = *opts
	}
	if o.MaxBodyBytes == 0 {
		o.MaxBodyBytes = defaultTreeBody
	}
	if o.MaxTotalBytes == 0 {
		o.MaxTotalBytes = defaultTreeTotal
	}
	b := &treeBuilder{opts: o, root: e, seen: make(map[[sha256.Size]byte]*Node), budget: newSpoolBudget(e)}
	t := &Tree{}
	root, err := b.read(e, nil, nil, nil)
	t.Root = root
	if root != nil {
		b.classify(root)
		b.shape(root, t)
		b.obTrailing(root, t)
		b.sgIntegrity(t, err)
	}
	return t, err
}

type treeBuilder struct {
	opts  TreeOptions
	root  *message.Entity
	total int64
	seen  map[[sha256.Size]byte]*Node
	// budget is the parse's spool budget, shared by every node of the tree.
	budget *spoolBudget
	// sgByPath is each node by the MIME path the parse gave its entity,
	// which the parse's diagnostics name; see tree_signed.go.
	sgByPath map[string]*Node
}

// read builds the node of e and everything under it. warn is the warning e
// came with.
func (b *treeBuilder) read(e *message.Entity, path []int, parent *Node, warn error) (*Node, error) {
	n := &Node{Path: path, Parent: parent, Header: e.Header, Warning: warn, budget: b.budget}
	b.sgRegister(e, n)
	n.MediaType, n.Params, _ = e.EffectiveContentType()
	if n.MediaType == "" {
		n.MediaType = "text/plain"
	}
	n.Disposition, n.DispositionParams, _ = e.EffectiveContentDisposition()
	n.Disposition = strings.ToLower(n.Disposition)
	// Every name the entity gives its content, the chosen one first
	// (ROB-050-009); the plain parameters when there is no candidate list.
	n.Filenames = e.FilenameCandidates()
	if len(n.Filenames) > 0 {
		n.Filename = n.Filenames[0].Filename
	}
	if n.Filename == "" {
		n.Filename = n.DispositionParams["filename"]
	}
	if n.Filename == "" {
		n.Filename = n.Params["name"]
	}
	n.ContentID = strings.Trim(strings.TrimSpace(e.Header.Get("Content-Id")), "<>")
	n.ContentLocation = strings.TrimSpace(e.Header.Get("Content-Location"))
	if stop := b.root.ParseStop(); stop != nil {
		return n, stop
	}

	if mr := e.MultipartReader(); mr != nil {
		var last error
		fails := 0
		// A part's span is known for certain once the iterator has moved past
		// it; taken before, it is known only when a buffer happened to read
		// to the part's end, which for a multipart child depends on how the
		// input arrived. So the child read last is asked again after the call.
		var ciPrev *message.Entity
		ciSpan := func() {
			if ciPrev != nil {
				b.span(ciPrev, n.Children[len(n.Children)-1])
				ciPrev = nil
			}
		}
		for i := 0; ; i++ {
			p, err := mr.NextPart()
			ciSpan()
			if err == io.EOF {
				break
			}
			if stop := b.root.ParseStop(); stop != nil {
				return n, stop
			}
			if p == nil {
				// A part that was refused or could not be framed. The next
				// call goes on with the one after it, unless the iterator
				// has ended for good, which it says by repeating itself.
				//
				// An iterator whose source failed says so with a newly
				// wrapped error each time, which is not the same value: it
				// is known by the input having failed, and, should that not
				// be known either, the run of parts refused in a row is
				// bounded, so the loop ends whatever the iterator does.
				n.Err = err
				fails++
				if last != nil && sameErr(last, err) {
					break
				}
				if fails >= treeMaxFailedParts || (fails > 1 && b.inputFailed()) {
					break
				}
				last = err
				continue
			}
			last, fails = nil, 0
			child, cerr := b.read(p, appendPath(path, len(n.Children)), n, err)
			n.Children = append(n.Children, child)
			if cerr != nil {
				return n, cerr
			}
			ciPrev = p
		}
		if stop := b.obMultipartEnds(mr, n); stop != nil {
			return n, stop
		}
		b.span(e, n)
		return n, nil
	}

	if b.opts.DescendMessages && e.CarriesMessage() {
		emb, err := e.EmbeddedMessage()
		if stop := b.root.ParseStop(); stop != nil {
			return n, stop
		}
		if emb != nil {
			var warn error
			if message.IsUnknownCharset(err) || message.IsUnknownEncoding(err) {
				warn = err
			}
			child, cerr := b.read(emb, appendPath(path, 0), n, warn)
			n.Children = append(n.Children, child)
			// What the embedded message left of the carrier's body.
			io.Copy(io.Discard, e.Body)
			b.span(e, n)
			return n, cerr
		}
		// Refused or unreadable: it stays a leaf, with what is left of it.
		n.Err = err
	}

	h := sha256.New()
	var body io.Reader = e.Body
	if b.opts.Body != nil {
		counted := &countReader{r: io.TeeReader(body, h)}
		if err := b.opts.Body(n, counted); err != nil {
			return n, err
		}
		_, rerr := io.Copy(io.Discard, counted)
		n.Size = counted.n
		b.finishLeaf(e, n, h.Sum(nil), rerr)
		return n, nil
	}

	keep := b.opts.MaxBodyBytes
	if b.opts.MaxTotalBytes > 0 {
		keep = min(keep, b.opts.MaxTotalBytes-b.total)
	}
	var buf bytes.Buffer
	var rerr error
	scratch := pfTreeChunks.Get().(*[]byte)
	defer pfTreeChunks.Put(scratch)
	chunk := *scratch
	for {
		m, err := body.Read(chunk)
		if m > 0 {
			h.Write(chunk[:m])
			n.Size += int64(m)
			if room := keep - int64(buf.Len()); room > 0 {
				buf.Write(chunk[:min(int64(m), room)])
			}
		}
		if err != nil {
			if err != io.EOF {
				rerr = err
			}
			break
		}
	}
	n.Data = buf.Bytes()
	n.Truncated = int64(len(n.Data)) < n.Size
	b.total += int64(len(n.Data))
	b.finishLeaf(e, n, h.Sum(nil), rerr)
	if stop := b.root.ParseStop(); stop != nil {
		return n, stop
	}
	return n, nil
}

func (b *treeBuilder) finishLeaf(e *message.Entity, n *Node, sum []byte, rerr error) {
	copy(n.SHA256[:], sum)
	if rerr != nil && n.Err == nil {
		n.Err = rerr
	}
	if n.Size > 0 {
		if first, ok := b.seen[n.SHA256]; ok {
			n.DuplicateOf = first
		} else {
			b.seen[n.SHA256] = n
		}
	}
	b.span(e, n)
}

func (b *treeBuilder) span(e *message.Entity, n *Node) {
	n.SpanStart, n.SpanEnd, n.HasSpan = e.RawSpan()
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func appendPath(path []int, i int) []int {
	out := make([]int, len(path)+1)
	copy(out, path)
	out[len(path)] = i
	return out
}

// sameErr compares two errors without calling anything on them; values of a
// type that cannot be compared are different.
func sameErr(a, b error) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

// treeMaxFailedParts bounds how many parts in a row a multipart may refuse
// before ReadTree stops asking it for the next: the default MaxPartCount.
const treeMaxFailedParts = 10000

// inputFailed reports whether the parse's input ended with an error of its
// own, after which no iterator of it has anything more to return.
func (b *treeBuilder) inputFailed() bool {
	s := b.root.ParseSession()
	return s != nil && s.Completion().InputErr != nil
}

// classify sets the Role and Reason of n and of every node under it
// (ROB-100-002).
func (b *treeBuilder) classify(n *Node) {
	// Parents first, as roleOf reads the roles above a node. One memo of the
	// related roots serves the whole tree: see pfRelatedRoots.
	roots := make(pfRelatedRoots)
	n.walk(func(c *Node) bool {
		c.Role, c.Reason = pfRoleOf(c, roots)
		return true
	})
}

func roleOf(n *Node) (Role, string) {
	return pfRoleOf(n, nil)
}

// pfRoleOf is roleOf, looking the root part of a multipart/related up in
// roots when it is given one. It asks where n stands among its siblings by
// position, never by searching for it, so that classifying a container costs
// its parts once and not their square (ROB-120-002).
func pfRoleOf(n *Node, roots pfRelatedRoots) (Role, string) {
	// A multipart that was framed has no digest; one left unframed was read
	// as one opaque leaf, which has one even when Data was not kept (an empty
	// body, MaxBodyBytes below zero, a Body hook).
	if strings.HasPrefix(n.MediaType, "multipart/") && (len(n.Children) > 0 || n.SHA256 == [sha256.Size]byte{}) {
		return RoleContainer, "multipart entity"
	}
	if p := n.Parent; p != nil {
		switch p.MediaType {
		case "multipart/signed":
			if pfChildAt(p, 1) == n {
				return RoleSignature, "second part of multipart/signed"
			}
		case "multipart/encrypted":
			if pfChildAt(p, 0) == n {
				return RoleEncrypted, "control part of multipart/encrypted"
			}
			return RoleEncrypted, "payload of multipart/encrypted"
		case "multipart/report":
			if pfChildAt(p, 0) != n && strings.HasPrefix(n.MediaType, "message/") && !carriesMessage(n.MediaType) {
				return RoleReport, "machine-readable part of multipart/report"
			}
		}
	}
	if carriesMessage(n.MediaType) && (n.Disposition != "attachment" || len(n.Children) > 0) {
		return RoleMessage, "carries a message"
	}
	if n.Disposition == "attachment" {
		return RoleAttachment, "Content-Disposition: attachment"
	}
	if rel := relatedOf(n); rel != nil && !roots.under(rel, n) && (n.ContentID != "" || n.ContentLocation != "") {
		return RoleRelated, "resource of multipart/related with a Content-ID or Content-Location"
	}
	if strings.HasPrefix(n.MediaType, "text/") && n.MediaType != "text/calendar" {
		switch {
		case n.Filename == "":
			return RoleBody, "text with no filename and no attachment disposition"
		case n.Disposition == "inline":
			return RoleBody, "text with Content-Disposition: inline"
		}
		return RoleAttachment, "text with a filename and no inline disposition"
	}
	if n.MediaType == "text/calendar" && n.Filename == "" && n.Parent != nil && n.Parent.MediaType == "multipart/alternative" {
		return RoleBody, "calendar alternative of the body"
	}
	if n.Disposition == "inline" {
		return RoleRelated, "non-text part with Content-Disposition: inline"
	}
	return RoleAttachment, "non-text part"
}

// carriesMessage reports whether an entity of mediaType carries a message or
// the header block of one.
func carriesMessage(mediaType string) bool {
	switch mediaType {
	case "message/rfc822", "message/global", "message/news", "text/rfc822-headers", "message/rfc822-headers", "message/global-headers":
		return true
	}
	return false
}

// relatedOf returns the nearest multipart/related n is under, looking through
// the multipart/alternative and other containers between them, or nil.
func relatedOf(n *Node) *Node {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.MediaType == "multipart/related" {
			return p
		}
		if p.Role == RoleMessage || !strings.HasPrefix(p.MediaType, "multipart/") {
			return nil
		}
	}
	return nil
}

// RelatedRoot returns the root part of a multipart/related (RFC 2387,
// ROB-090-004): the child whose Content-ID the start parameter names, else
// the first child. It returns nil for any other node, and for a
// multipart/related with no part.
func (n *Node) RelatedRoot() *Node {
	if n == nil || n.MediaType != "multipart/related" || len(n.Children) == 0 {
		return nil
	}
	if start := normalizeCID(n.Params["start"]); start != "" {
		for _, c := range n.Children {
			if c.ContentID != "" && normalizeCID(c.ContentID) == start {
				return c
			}
		}
	}
	return n.Children[0]
}

// normalizeCID strips what a Content-ID, a start parameter or a cid: URL
// wraps an identifier in: white space, angle brackets, the "cid:" scheme and
// percent-encoding.
func normalizeCID(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 4 && strings.EqualFold(s[:4], "cid:") {
		s = s[4:]
		if u, err := url.PathUnescape(s); err == nil {
			s = u
		}
	}
	return strings.Trim(strings.TrimSpace(s), "<>")
}

// Resolve returns the part a reference in this node's content names
// (ROB-100-004): a "cid:" URL or a bare Content-ID is matched against the
// Content-ID of the parts of the multipart/related this node is in, and any
// other reference against their Content-Location, exactly and then with both
// resolved against this node's own Content-Location, or the nearest one
// above it. Enclosing
// multipart/related entities are searched outwards. It returns nil when
// nothing matches. Nothing is fetched: a reference that names no part of the
// message is simply unresolved (ROB-120-005).
func (n *Node) Resolve(ref string) *Node {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	isCID := len(ref) >= 4 && strings.EqualFold(ref[:4], "cid:")
	id := normalizeCID(ref)
	for rel := relatedOf(n); rel != nil; rel = relatedOf(rel) {
		var found *Node
		rel.walk(func(c *Node) bool {
			if c == rel || len(c.Children) > 0 {
				return true
			}
			switch {
			case c.ContentID != "" && normalizeCID(c.ContentID) == id:
				found = c
			case !isCID && c.ContentLocation != "" && (c.ContentLocation == ref || sameLocation(n, rel, c.ContentLocation, ref)):
				found = c
			}
			return found == nil
		})
		if found != nil {
			return found
		}
	}
	return nil
}

// sameLocation reports whether two references name the same location once
// each is resolved against the base of the referring node from: its own
// Content-Location, else that of the nearest entity above it up to rel, else
// that of rel's root part (RFC 2557 section 5).
func sameLocation(from, rel *Node, a, b string) bool {
	var base *url.URL
	for c := from; c != nil && base == nil; c = c.Parent {
		if c.ContentLocation != "" {
			if u, err := url.Parse(c.ContentLocation); err == nil && u.IsAbs() {
				base = u
			}
		}
		if c == rel {
			break
		}
	}
	if base == nil {
		root := rel.RelatedRoot()
		if root == nil || root.ContentLocation == "" {
			return false
		}
		u, err := url.Parse(root.ContentLocation)
		if err != nil {
			return false
		}
		base = u
	}
	ua, erra := base.Parse(a)
	ub, errb := base.Parse(b)
	return erra == nil && errb == nil && ua.String() == ub.String()
}

// walk calls fn for n and every node under it, in document order, until fn
// returns false.
func (n *Node) walk(fn func(*Node) bool) bool {
	if !fn(n) {
		return false
	}
	for _, c := range n.Children {
		if !c.walk(fn) {
			return false
		}
	}
	return true
}

// Walk calls fn for n and every node under it, in document order. It stops
// when fn returns false.
func (n *Node) Walk(fn func(*Node) bool) {
	if n != nil && fn != nil {
		n.walk(fn)
	}
}

// Bodies returns the parts to read as the message's body (ROB-100-003). For
// each multipart/alternative it chooses one form: the first media type of
// prefer that some alternative offers — the last such alternative, which is
// the richest in the sender's order — or, with no preference or none offered,
// the last alternative that has a body at all. A multipart/related gives the
// body of its root part, a multipart/signed that of its first part, and any
// other multipart those of all its parts in order. An embedded message's body
// is its own and is not included; ask its node.
//
// Nothing is discarded: every alternative stays in the tree.
func (n *Node) Bodies(prefer ...string) []*Node {
	// The caller's slice is left as it was.
	norm := make([]string, len(prefer))
	for i, p := range prefer {
		norm[i] = strings.ToLower(strings.TrimSpace(p))
	}
	return n.bodies(norm)
}

func (n *Node) bodies(prefer []string) []*Node {
	if n == nil {
		return nil
	}
	if n.Role == RoleBody {
		if len(prefer) == 0 {
			return []*Node{n}
		}
		for _, p := range prefer {
			if n.MediaType == p {
				return []*Node{n}
			}
		}
		return nil
	}
	if n.Role != RoleContainer {
		return nil
	}
	switch n.MediaType {
	case "multipart/alternative":
		for _, p := range prefer {
			for i := len(n.Children) - 1; i >= 0; i-- {
				if b := n.Children[i].bodies([]string{p}); len(b) > 0 {
					return b
				}
			}
		}
		for i := len(n.Children) - 1; i >= 0; i-- {
			if b := n.Children[i].bodies(nil); len(b) > 0 {
				return b
			}
		}
		return nil
	case "multipart/related":
		return n.RelatedRoot().bodies(prefer)
	case "multipart/signed":
		if len(n.Children) > 0 {
			return n.Children[0].bodies(prefer)
		}
		return nil
	case "multipart/encrypted":
		return nil
	}
	var out []*Node
	for _, c := range n.Children {
		out = append(out, c.bodies(prefer)...)
	}
	if len(out) == 0 && len(prefer) > 0 {
		// A message that offers none of the preferred forms still has a body.
		for _, c := range n.Children {
			out = append(out, c.bodies(nil)...)
		}
	}
	return out
}

// Attachments returns every attachment under n, in document order: the
// RoleAttachment nodes and, as whole messages, the RoleMessage ones. The
// attachments inside an embedded message are that message's, and are not
// included; ask its node.
func (n *Node) Attachments() []*Node {
	var out []*Node
	var visit func(c *Node, top bool)
	visit = func(c *Node, top bool) {
		switch c.Role {
		case RoleAttachment:
			out = append(out, c)
		case RoleMessage:
			if !top {
				out = append(out, c)
				return
			}
		}
		for _, k := range c.Children {
			visit(k, false)
		}
	}
	if n != nil {
		visit(n, true)
	}
	return out
}

// shape looks at the entities whose structure RFCs fix and records where a
// message departs from it. Nothing is changed by it.
func (b *treeBuilder) shape(root *Node, t *Tree) {
	note := func(n *Node, code message.Code, severity message.Severity, msg, action string) {
		t.Diagnostics = append(t.Diagnostics, message.Diagnostic{
			Code: code, Severity: severity, Message: msg, Path: append([]int(nil), n.Path...),
			Field: "Content-Type", Action: action,
		})
	}
	root.walk(func(n *Node) bool {
		switch n.MediaType {
		case "multipart/signed":
			// RFC 1847 section 2.1: exactly two parts, the second of the
			// declared protocol.
			proto := strings.ToLower(n.Params["protocol"])
			switch {
			case len(n.Children) != 2:
				note(n, message.CodeSignedStructureInvalid, message.SeverityInvalidMetadata,
					fmt.Sprintf("multipart/signed has %d parts, not a signed part and a signature", len(n.Children)),
					"kept the parts as they are; the signature cannot be checked against this structure")
			case proto == "":
				note(n, message.CodeSignedStructureInvalid, message.SeverityInfo,
					"multipart/signed declares no protocol", "read the second part as the signature")
			case proto != n.Children[1].MediaType:
				note(n, message.CodeSignedStructureInvalid, message.SeverityInfo,
					fmt.Sprintf("multipart/signed declares the protocol %q and its second part is %q", proto, n.Children[1].MediaType),
					"read the second part as the signature")
			}
		case "multipart/encrypted":
			// RFC 1847 section 2.2: a control part of the declared protocol
			// and an application/octet-stream payload.
			proto := strings.ToLower(n.Params["protocol"])
			switch {
			case len(n.Children) != 2:
				note(n, message.CodeEncryptedStructureInvalid, message.SeverityInvalidMetadata,
					fmt.Sprintf("multipart/encrypted has %d parts, not a control part and a payload", len(n.Children)),
					"kept the parts as they are")
			case proto != "" && proto != n.Children[0].MediaType:
				note(n, message.CodeEncryptedStructureInvalid, message.SeverityInfo,
					fmt.Sprintf("multipart/encrypted declares the protocol %q and its first part is %q", proto, n.Children[0].MediaType),
					"read the first part as the control part")
			}
		case "multipart/report":
			// RFC 6522: a human-readable part, then a message/<report-type>.
			rt := strings.ToLower(n.Params["report-type"])
			switch {
			case len(n.Children) < 2:
				note(n, message.CodeReportInvalid, message.SeverityInvalidMetadata,
					fmt.Sprintf("multipart/report has %d parts, not a description and a report", len(n.Children)),
					"kept the parts as they are")
			case rt == "":
				note(n, message.CodeReportInvalid, message.SeverityInfo, "multipart/report declares no report-type", "read its parts by their own media types")
			case n.Children[1].MediaType != "message/"+rt && n.Children[1].MediaType != "message/global-"+rt:
				note(n, message.CodeReportInvalid, message.SeverityInfo,
					fmt.Sprintf("multipart/report declares the report-type %q and its second part is %q", rt, n.Children[1].MediaType),
					"read its parts by their own media types")
			}
		case "message/partial":
			note(n, message.CodeFeatureUnsupportedMediaType, message.SeverityUnsupported,
				"message/partial is one fragment of a message sent in pieces", "kept the fragment as it is; fragments are not reassembled")
		case "message/external-body":
			note(n, message.CodeFeatureUnsupportedMediaType, message.SeverityUnsupported,
				"message/external-body refers to content outside the message", "kept the reference as it is; nothing is fetched")
		}
		return true
	})
}

// ReportFields reads the body of a RoleReport node — a message/delivery-status,
// message/disposition-notification or message/feedback-report — as the groups
// of header fields it is made of: the per-message group first, then one per
// recipient where the report has them (ROB-090-005). A group that cannot be
// read ends the list; the raw body is Data either way. It returns nil for a
// node whose Data was not kept whole.
func (n *Node) ReportFields() []message.Header {
	if n == nil || n.Truncated || len(n.Data) == 0 {
		return nil
	}
	br := bufio.NewReader(bytes.NewReader(n.Data))
	var groups []message.Header
	for len(groups) < 1024 {
		// Blank lines between groups.
		for {
			c, err := br.Peek(1)
			if err != nil || (c[0] != '\r' && c[0] != '\n') {
				break
			}
			br.Discard(1)
		}
		if _, err := br.Peek(1); err != nil {
			break
		}
		h, _, err := textproto.ReadHeaderRecover(br, textproto.HeaderLimits{MaxBytes: 1 << 20, MaxCount: 4096})
		if err != nil || h.Len() == 0 {
			break
		}
		groups = append(groups, message.Header{Header: h})
	}
	return groups
}
