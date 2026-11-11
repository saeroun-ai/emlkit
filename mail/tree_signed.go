package mail

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/saeroun-ai/emlkit"
)

// Signed content and repairs (ROB-120-006).
//
// A signature covers bytes as they were sent: S/MIME and PGP/MIME the first
// part of a multipart/signed (RFC 1847, RFC 8551, RFC 3156), DKIM a message's
// header fields and body (RFC 6376). The tree is the parser's reading of those
// bytes, and under Parse that reading heals: a damaged header line is set
// apart, a missing boundary is inferred, junk in Base64 is skipped. Each
// repair is a diagnostic of the parse, recorded against the entity it was made
// in. Here they are mapped to the nodes of those entities, and each
// multipart/signed, and the message as a whole, is told whether its parsed
// view is still what a signature over its raw bytes covers.
//
// Which diagnostics repaired the view: every severity but two. Recovered,
// Incomplete, InvalidMetadata, LimitExceeded and Fatal say that something was
// repaired, guessed, cut, refused or lost, so the decoded view is not what
// the bytes say. SeverityInfo notes a nonstandard representation read as it
// stands, and SeverityUnsupported raw content passed through for want of a
// decoder: the bytes were not reinterpreted. limit.diagnostic_count is about
// the reporting, not the message, and is not mapped; the diagnostics it kept
// out make Tree.RepairsUnknown. For the message's own header,
// header.duplicate_singleton counts at any severity, since the parser and a
// DKIM verifier read different occurrences of a repeated field.

// Diagnostic codes of the tree (Tree.Diagnostics), both SeverityInfo: the
// repairs themselves are the parse's diagnostics, and these say what they
// mean for a signature.
const (
	// CodeSignedUnverifiable reports a multipart/signed whose parsed view
	// cannot stand for what was signed; its Message gives the reasons, which
	// Node.UnverifiableReason holds too. Path is the container's.
	CodeSignedUnverifiable message.Code = "signed.unverifiable"

	// CodeDKIMViewRepaired reports a message that carries a DKIM-Signature or
	// ARC-Message-Signature field and whose header block or body the parse
	// repaired (Tree.HeaderRepaired, Tree.BodyRepaired): a result computed
	// from the parsed view cannot be trusted. Path is the root's.
	CodeDKIMViewRepaired message.Code = "dkim.view_repaired"
)

// sgRegister records n as the node of e under the path the parse gave e.
func (b *treeBuilder) sgRegister(e *message.Entity, n *Node) {
	o, ok := e.Origin()
	if !ok {
		return
	}
	if b.sgByPath == nil {
		b.sgByPath = make(map[string]*Node)
	}
	k := sgPathKey(o.Path)
	if _, dup := b.sgByPath[k]; !dup {
		b.sgByPath[k] = n
	}
}

func sgPathKey(path []int) string {
	var s strings.Builder
	for i, p := range path {
		if i > 0 {
			s.WriteByte('.')
		}
		s.WriteString(strconv.Itoa(p))
	}
	return s.String()
}

// sgNodeAt returns the node of the entity at path, or of the nearest entity
// above it that has one — a part the parse refused has no node, and what was
// recorded against it is its container's concern — with exact reporting
// which. It returns nil for a path outside the tree.
func (b *treeBuilder) sgNodeAt(path []int) (n *Node, exact bool) {
	for i := len(path); i >= 0; i-- {
		if n := b.sgByPath[sgPathKey(path[:i])]; n != nil {
			return n, i == len(path)
		}
	}
	return nil, false
}

// sgRepairs reports whether d repaired the view of its entity; an unknown
// severity counts as a repair.
func sgRepairs(d message.Diagnostic) bool {
	return d.Code != message.CodeLimitDiagnosticCount &&
		d.Severity != message.SeverityInfo && d.Severity != message.SeverityUnsupported
}

func (n *Node) sgAddRepair(c message.Code) {
	n.Repaired = true
	for _, have := range n.RepairCodes {
		if have == c {
			return
		}
	}
	n.RepairCodes = append(n.RepairCodes, c)
}

// sgInHeader reports whether d, recorded against the root, is about its header
// block: its code is a header, header-budget, BOM or empty-input one, or it
// lies before the root's body.
func sgInHeader(d message.Diagnostic, bodyOffset int64, placed bool) bool {
	switch {
	case d.Code.Group() == "header" || d.Code == message.CodeInputBOM || d.Code == message.CodeInputEmpty:
		return true
	case strings.HasPrefix(string(d.Code), "limit.header_") || strings.HasPrefix(string(d.Code), "limit.total_header_"):
		return true
	}
	return placed && d.Location.HasOffset && d.Location.Offset < bodyOffset
}

// sgIntegrity maps the parse's diagnostics to the nodes of t and settles what
// they mean for signatures. readErr is what ended ReadTree, if anything.
func (b *treeBuilder) sgIntegrity(t *Tree, readErr error) {
	root := t.Root
	s := b.root.ParseSession()
	diags := s.Diagnostics()

	// A repair the session counted and did not keep cannot be placed.
	if s != nil {
		c := s.Completion()
		kept := make([]int64, len(c.Diagnostics))
		for _, d := range diags {
			if d.Code != message.CodeLimitDiagnosticCount && int(d.Severity) < len(kept) {
				kept[d.Severity]++
			}
		}
		for sev, n := range c.Diagnostics {
			if sgRepairs(message.Diagnostic{Severity: message.Severity(sev)}) && n > kept[sev] {
				t.RepairsUnknown = true
			}
		}
	}

	var bodyOffset int64
	var placed bool
	if o, ok := b.root.Origin(); ok && o.HasOffsets {
		bodyOffset, placed = o.BodyOffset, true
	}
	for _, d := range diags {
		if d.Code == message.CodeLimitDiagnosticCount {
			continue
		}
		n, exact := b.sgNodeAt(d.Path)
		if n == nil {
			continue
		}
		repair := sgRepairs(d)
		if repair {
			n.sgAddRepair(d.Code)
		}
		switch {
		case n == root && exact && sgInHeader(d, bodyOffset, placed):
			if repair || d.Code == message.CodeHeaderDuplicateSingleton {
				t.HeaderRepaired = true
				t.HeaderRepairFields = sgAddField(t.HeaderRepairFields, d.Field)
			}
		case repair:
			t.BodyRepaired = true
		}
	}
	root.walk(func(n *Node) bool {
		if n.Err != nil {
			n.Repaired = true
			t.BodyRepaired = true
		}
		return true
	})

	stopped := readErr != nil || b.root.ParseStop() != nil
	if stopped {
		t.BodyRepaired = true
	}
	if t.RepairsUnknown {
		t.HeaderRepaired, t.BodyRepaired = true, true
	}

	// One walk in document order gives each node the signed content it is
	// in, its place, and the Repaired nodes in order, so that the first
	// repair under any node is a search rather than another walk.
	w := &sgWalk{span: make(map[*Node][2]int)}
	w.visit(root, nil)
	for _, n := range w.nodes {
		if n.MediaType != "multipart/signed" {
			continue
		}
		if r := sgUnverifiable(n, t, stopped, w); len(r) > 0 {
			n.Unverifiable = true
			n.UnverifiableReason = strings.Join(r, "; ")
			t.Diagnostics = append(t.Diagnostics, message.Diagnostic{
				Code: CodeSignedUnverifiable, Severity: message.SeverityInfo,
				Message: "multipart/signed cannot be verified from the parsed view: " + n.UnverifiableReason,
				Path:    append([]int(nil), n.Path...),
				Action:  "kept the raw span of the signed part; verify over it, and treat the content as unverified",
			})
		}
	}

	if t.HeaderRepaired || t.BodyRepaired {
		for _, f := range []string{"DKIM-Signature", "ARC-Message-Signature"} {
			if root.Header.Has(f) {
				t.Diagnostics = append(t.Diagnostics, message.Diagnostic{
					Code: CodeDKIMViewRepaired, Severity: message.SeverityInfo, Field: f,
					Message: "the message carries " + f + " and the parse repaired its " + sgWhat(t),
					Path:    append([]int(nil), root.Path...),
					Action:  "kept the input as received; verify over it, and do not trust a result computed from the parsed view",
				})
				break
			}
		}
	}
}

func sgWhat(t *Tree) string {
	switch {
	case t.HeaderRepaired && t.BodyRepaired:
		return "header block and body"
	case t.HeaderRepaired:
		return "header block"
	}
	return "body"
}

func sgAddField(fields []string, f string) []string {
	if f == "" {
		return fields
	}
	for _, have := range fields {
		if strings.EqualFold(have, f) {
			return fields
		}
	}
	return append(fields, f)
}

// sgWalk is the tree in document order: nodes, each node's range of indices
// in it — its own and those of the nodes under it — and the indices of the
// Repaired nodes, ascending.
type sgWalk struct {
	nodes    []*Node
	span     map[*Node][2]int
	repaired []int
}

// visit sets n.Signed to signed and goes on with n's children: the first part
// of a multipart/signed is its signed content, and every other child is in
// whatever n is in.
func (w *sgWalk) visit(n, signed *Node) {
	i := len(w.nodes)
	w.nodes = append(w.nodes, n)
	n.Signed = signed
	if n.Repaired {
		w.repaired = append(w.repaired, i)
	}
	for k, c := range n.Children {
		cs := signed
		if k == 0 && n.MediaType == "multipart/signed" {
			cs = n
		}
		w.visit(c, cs)
	}
	w.span[n] = [2]int{i, len(w.nodes)}
}

// firstRepaired returns the first Repaired node at or under n, or nil.
func (w *sgWalk) firstRepaired(n *Node) *Node {
	s := w.span[n]
	if j := sort.SearchInts(w.repaired, s[0]); j < len(w.repaired) && w.repaired[j] < s[1] {
		return w.nodes[w.repaired[j]]
	}
	return nil
}

// sgUnverifiable returns why the multipart/signed n cannot be verified from
// the parsed view, or nothing.
func sgUnverifiable(n *Node, t *Tree, stopped bool, w *sgWalk) []string {
	var r []string
	if stopped {
		r = append(r, "the tree was not read to its end")
	}
	if t.RepairsUnknown {
		r = append(r, "the parse did not keep every diagnostic, so a repair cannot be ruled out")
	}
	if n.Repaired {
		r = append(r, "its own framing or header was repaired"+sgCodes(n.RepairCodes))
	}
	if len(n.Children) != 2 {
		r = append(r, fmt.Sprintf("it has %d parts, not a signed part and a signature", len(n.Children)))
	}
	if len(n.Children) > 0 {
		first := n.Children[0]
		if !first.HasSpan {
			r = append(r, "the signed part has no raw span")
		}
		if at := w.firstRepaired(first); at != nil {
			r = append(r, "the signed content was repaired, first at "+sgPathString(at.Path)+sgCodes(at.RepairCodes))
		}
	}
	if len(n.Children) > 1 {
		if at := w.firstRepaired(n.Children[1]); at != nil {
			r = append(r, "the signature part was repaired, first at "+sgPathString(at.Path)+sgCodes(at.RepairCodes))
		}
	}

	// RFC 1847 section 2.1: protocol and micalg are both required, and the
	// protocol is the media type of the signature part.
	proto := strings.ToLower(strings.TrimSpace(n.Params["protocol"]))
	switch {
	case proto == "":
		r = append(r, "it declares no protocol")
	case len(n.Children) > 1 && proto != n.Children[1].MediaType:
		r = append(r, fmt.Sprintf("its protocol %q is not the media type %q of its signature part", proto, n.Children[1].MediaType))
	}
	micalg := strings.ToLower(strings.TrimSpace(n.Params["micalg"]))
	switch {
	case micalg == "":
		r = append(r, "it declares no micalg")
	case !sgMicalgFits(proto, micalg):
		r = append(r, fmt.Sprintf("its micalg %q does not fit the protocol %q", micalg, proto))
	}
	return r
}

// sgMicalgFits reports whether every value of a micalg list fits the
// protocol: "pgp-" names for PGP/MIME (RFC 3156 section 5), and none of
// those for S/MIME (RFC 8551 section 3.5.3.2). Any other protocol takes any
// value.
func sgMicalgFits(proto, micalg string) bool {
	for _, v := range strings.Split(micalg, ",") {
		v = strings.TrimSpace(v)
		pgp := strings.HasPrefix(v, "pgp-")
		switch proto {
		case "application/pgp-signature":
			if !pgp {
				return false
			}
		case "application/pkcs7-signature", "application/x-pkcs7-signature":
			if pgp || v == "" {
				return false
			}
		}
	}
	return true
}

func sgCodes(codes []message.Code) string {
	if len(codes) == 0 {
		return ""
	}
	s := make([]string, len(codes))
	for i, c := range codes {
		s[i] = string(c)
	}
	return " (" + strings.Join(s, ", ") + ")"
}

func sgPathString(path []int) string {
	if len(path) == 0 {
		return "the root"
	}
	return "part " + sgPathKey(path)
}
