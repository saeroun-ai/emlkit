package message

import (
	"fmt"
	"strings"
)

// Nested multiparts whose boundaries collide (ROB-060-011). See
// docs/robustness-notes/nested-boundaries.md.
//
// A delimiter line belongs to the outermost multipart whose boundary it is a
// delimiter line of: that multipart's reader ends its part there, so nothing
// nested inside that part ever sees it. A nested multipart that declares a
// boundary whose own delimiter lines are delimiter lines of an enclosing one
// is therefore framed as declared, and its parts are read as parts of the
// enclosing one. Boundaries that are only prefixes of one another, or differ
// in case or by a trailing "-", do not collide: the wire layer tells them
// apart. Under Parse (every profile but ProfileStrict):
//
//   - multipart.nested_boundary_reused: the same boundary;
//   - multipart.nested_boundary_conflict: one that differs only by trailing
//     white space, so that its opening lines are the enclosing one's; one
//     that is the enclosing one's followed by "--", so that its opening lines
//     are the enclosing one's closing line; or one that the enclosing one's
//     is followed by "--", so that its closing line opens a part of the
//     enclosing one;
//   - multipart.close_shared: a closing delimiter line the enclosing
//     multipart met may have been the nested one's, so the iteration read on
//     past it, to the parts it would otherwise have left in its epilogue.
//
// The enclosing multipart reads on past a closing delimiter line only as
// many times as nested multiparts declared its boundary (any number of times
// when one declared it followed by "--"), and only when another delimiter
// line follows within Limits.MaxPreambleBytes.

// Codes of nested boundaries.
const (
	CodeMultipartNestedBoundaryConflict Code = "multipart.nested_boundary_conflict"
	CodeMultipartCloseShared            Code = "multipart.close_shared"
)

// A nbCollision is how the delimiter lines of a nested multipart's boundary
// meet those of an enclosing one's.
type nbCollision uint8

const (
	nbNone       nbCollision = iota
	nbSame                   // the same boundary
	nbPadded                 // the nested one's opening lines are the enclosing one's
	nbOpensClose             // the nested one's opening lines close the enclosing one
	nbCloseOpens             // the nested one's closing line opens a part of the enclosing one
)

// nbCollide reports how a nested boundary d meets an enclosing boundary a. A
// delimiter line is "--", the boundary, "--" for the closing one, and any
// spaces and tabs.
func nbCollide(a, d string) nbCollision {
	t := strings.TrimRight(d, " \t")
	switch {
	case a == "" || d == "":
		return nbNone
	case d == a:
		return nbSame
	case t == a:
		return nbPadded
	case t == a+"--":
		return nbOpensClose
	case a == d+"--":
		return nbCloseOpens
	}
	return nbNone
}

// nbCredit is called for each part this iterator frames. A part that is a
// multipart declaring a boundary whose closing delimiter line, or whose
// opening ones, close the outermost enclosing multipart it collides with lets
// that multipart read on past one closing delimiter line more — or past any
// number of them, for one whose opening lines do.
func (r *multipartReader) nbCredit(e *Entity) {
	if !r.healing || e == nil || !e.framesParts() {
		return
	}
	d := e.mediaParams["boundary"]
	var owner *boundaryChain
	unlimited := false
	for c, n := r.parent.bounds, 0; c != nil && n < 1024; c, n = c.up, n+1 {
		switch nbCollide(c.b, d) {
		case nbSame:
			owner, unlimited = c, false
		case nbOpensClose:
			owner, unlimited = c, true
		}
	}
	switch {
	case owner == nil || owner.nbShared < 0:
	case unlimited:
		owner.nbShared = -1
	default:
		owner.nbShared++
	}
}

// nbNoteConflict records, on this iterator's first call, a declared boundary
// that collides with an enclosing one's other than by being the same, which
// boundaryReusedAction already reports.
func (r *multipartReader) nbNoteConflict() error {
	if r.nbChecked || !r.healing || r.declared == "" || r.parent.bounds == nil {
		return nil
	}
	r.nbChecked = true
	for c, n := r.parent.bounds.up, 0; c != nil && n < 1024; c, n = c.up, n+1 {
		var what, action string
		switch nbCollide(c.b, r.declared) {
		case nbSame:
			return nil
		case nbPadded:
			what, action = "delimiter lines are delimiter lines", boundaryReusedAction
		case nbOpensClose:
			what, action = "delimiter lines are closing delimiter lines", nbOpensCloseAction
		case nbCloseOpens:
			what, action = "closing delimiter line is a delimiter line", nbCloseOpensAction
		default:
			continue
		}
		return r.noteFraming(CodeMultipartNestedBoundaryConflict, SeverityRecovered,
			fmt.Sprintf("multipart declares the boundary %q, whose %s of the boundary %q of a multipart it is nested in", r.declared, what, c.b), action)
	}
	return nil
}

const (
	nbOpensCloseAction = "framed this multipart as declared: each of its delimiter lines closes the enclosing multipart, which reads on past it, so the parts of this one are read as parts of that one"
	nbCloseOpensAction = "framed this multipart as declared: the enclosing multipart ends it at its closing delimiter line, which opens a part of the enclosing multipart"
	nbResumedAction    = "read on past the closing delimiter line: the parts after it are read as parts of this multipart"
	nbResumedAsPart    = "read on past the closing delimiter line: the text after it, up to the next delimiter line, is read as a part of this multipart, and the parts after that as parts of this multipart"
)

// nbResume is called when the framing reader ended this iteration. When it
// ended at a closing delimiter line a nested multipart may have shared, and
// another delimiter line follows, the iteration is reopened past it and again
// reports that NextPart is to be called again.
func (r *multipartReader) nbResume() (again bool, err error) {
	node := r.parent.bounds
	if !r.healing || r.inferred || node == nil || node.nbShared == 0 || !r.r.Framing().Closed {
		return false, nil
	}
	closes := -1
	if node.nbShared > 0 {
		closes = node.nbShared - 1
	}
	var lookahead int64
	if l, ok := r.parent.limits(); ok {
		lookahead = l.MaxPreambleBytes
	}
	res, ok := r.r.ResumeAfterClose(lookahead, closes)
	// Looking ahead reads the input, and so may meet a stop of the parse.
	if stop := r.parent.fatalErr(); stop != nil {
		return false, stop
	}
	if !ok {
		// It is not looked for again: the iteration has ended.
		node.nbShared = 0
		return false, nil
	}
	if node.nbShared > 0 {
		node.nbShared = max(node.nbShared-1-res.Closes, 0)
	}
	action := nbResumedAction
	if res.Part {
		action = nbResumedAsPart
	}
	if fatal := r.parent.recordBody(Diagnostic{
		Code:     CodeMultipartCloseShared,
		Severity: SeverityRecovered,
		Message:  fmt.Sprintf("a closing delimiter line of the boundary %q, which a nested multipart declared, is followed by another delimiter line", r.declared),
		Field:    "Content-Type",
		Action:   action,
		Location: r.parent.bodyLocationAt(res.Offset),
	}); fatal != nil {
		return false, fatal
	}
	return true, nil
}
