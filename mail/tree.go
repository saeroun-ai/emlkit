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
	// Filename is the name the entity gives its content: the disposition's
	// filename, else the content type's name. It is what the message says,
	// not a safe path.
	Filename string
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

	// HasSpan reports that SpanStart and SpanEnd are the half-open range of
	// the input that holds this part as it was sent; see
	// message.Entity.RawSpan.
	HasSpan            bool
	SpanStart, SpanEnd int64
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
	// signed, encrypted, report and unsupported entities. They are apart from
	// the parse's own diagnostics, which its ParseSession holds.
	Diagnostics []message.Diagnostic
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
	b := &treeBuilder{opts: o, root: e, seen: make(map[[sha256.Size]byte]*Node)}
	t := &Tree{}
	root, err := b.read(e, nil, nil, nil)
	t.Root = root
	if root != nil {
		b.classify(root)
		b.shape(root, t)
	}
	return t, err
}

type treeBuilder struct {
	opts  TreeOptions
	root  *message.Entity
	total int64
	seen  map[[sha256.Size]byte]*Node
}

// read builds the node of e and everything under it. warn is the warning e
// came with.
func (b *treeBuilder) read(e *message.Entity, path []int, parent *Node, warn error) (*Node, error) {
	n := &Node{Path: path, Parent: parent, Header: e.Header, Warning: warn}
	n.MediaType, n.Params, _ = e.EffectiveContentType()
	if n.MediaType == "" {
		n.MediaType = "text/plain"
	}
	n.Disposition, n.DispositionParams, _ = e.EffectiveContentDisposition()
	n.Disposition = strings.ToLower(n.Disposition)
	n.Filename = n.DispositionParams["filename"]
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
		for i := 0; ; i++ {
			p, err := mr.NextPart()
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
				n.Err = err
				if last != nil && sameErr(last, err) {
					break
				}
				last = err
				continue
			}
			last = nil
			child, cerr := b.read(p, appendPath(path, len(n.Children)), n, err)
			n.Children = append(n.Children, child)
			if cerr != nil {
				return n, cerr
			}
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
	chunk := make([]byte, 32<<10)
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

// classify sets the Role and Reason of n and of every node under it
// (ROB-100-002).
func (b *treeBuilder) classify(n *Node) {
	n.Role, n.Reason = roleOf(n)
	for _, c := range n.Children {
		b.classify(c)
	}
}

func roleOf(n *Node) (Role, string) {
	if strings.HasPrefix(n.MediaType, "multipart/") && (len(n.Children) > 0 || n.Data == nil) {
		return RoleContainer, "multipart entity"
	}
	if p := n.Parent; p != nil {
		idx := indexOf(p, n)
		switch p.MediaType {
		case "multipart/signed":
			if idx == 1 {
				return RoleSignature, "second part of multipart/signed"
			}
		case "multipart/encrypted":
			if idx == 0 {
				return RoleEncrypted, "control part of multipart/encrypted"
			}
			return RoleEncrypted, "payload of multipart/encrypted"
		case "multipart/report":
			if idx >= 1 && strings.HasPrefix(n.MediaType, "message/") && !carriesMessage(n.MediaType) {
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
	if rel := relatedOf(n); rel != nil && !underRoot(rel, n) && (n.ContentID != "" || n.ContentLocation != "") {
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

// underRoot reports whether n is the root part of the multipart/related rel,
// or a part of it.
func underRoot(rel, n *Node) bool {
	root := rel.RelatedRoot()
	for c := n; c != nil && c != rel; c = c.Parent {
		if c == root {
			return true
		}
	}
	return false
}

func indexOf(p, n *Node) int {
	for i, c := range p.Children {
		if c == n {
			return i
		}
	}
	return -1
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
	for i, p := range prefer {
		prefer[i] = strings.ToLower(strings.TrimSpace(p))
	}
	return n.bodies(prefer)
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
