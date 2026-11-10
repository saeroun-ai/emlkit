package message

import "github.com/saeroun-ai/emlkit/textproto"

// A parseContext is what the parser carries with it as it descends a message:
// the session that collects what it observes, and the origin of the entity it
// is currently working on.
//
// The two travel together because an observation is only useful with a place.
// The session says what happened; the origin says which entity it happened to
// and where in the input that entity is. Threading them as one value is what
// lets the root of a message, a direct child of it, and a child of that child
// all report into the same session while each names itself correctly, without
// any of them having to be told a path or an offset it could get wrong.
//
// The zero parseContext is usable and means "nothing is known": it has no
// session, so recording is a no-op, and an unknown origin, so it supplies no
// position. That is what an entity the caller assembled has, and it is why no
// user of this type needs a nil check.
//
// A parseContext is a value. Passing it on, or deriving a child from it, never
// disturbs the one it came from.
type parseContext struct {
	// session collects the diagnostics recorded through this context, or is
	// nil for an entity that was not parsed with one. ParseSession.record is a
	// no-op on a nil session, so nothing here special-cases that.
	session *ParseSession

	// origin is where the parser found the entity this context describes. See
	// entityOrigin: its zero value is the "not known" state, which is distinct
	// from the root entity's nil path.
	origin entityOrigin

	// depth is how many multipart containers were descended through to reach
	// this entity: zero for the root of a message, and one more for each child
	// a multipart body yielded on the way down. It is what Limits.MaxMIMEDepth
	// bounds; see limit_tree.go.
	//
	// It is kept here rather than read back from origin.path because the two
	// answer different questions. A path says where an entity sits in the
	// parsed tree, and is withdrawn when a caller replaces a body, since the
	// parts of a replacement are not the input's parts. How far the parser has
	// descended is true either way, so a budget on nesting keeps counting
	// through a detached body instead of starting over at zero.
	depth int64

	// separatorMissing reports that this entity is a part whose header block,
	// which began with a malformed prefix or held an orphan record, was ended
	// by its multipart's next
	// delimiter line rather than by an empty line (textproto
	// Part.HeaderSeparatorMissing). It is this entity's own and is never carried
	// to a child. See firstline.go.
	separatorMissing bool

	// bounds is the boundaries of the multiparts this entity is nested in,
	// innermost first, or nil. See multipart_heal.go.
	bounds *boundaryChain
}

// rootContext returns the context of the root entity of a message parsed from
// the first byte of an input, whose body begins at bodyOffset, and whose
// diagnostics belong to session. session may be nil.
func rootContext(session *ParseSession, bodyOffset int64) parseContext {
	return rootContextAt(session, 0, bodyOffset)
}

// rootContextAt is rootContext for a message whose header block begins
// headerOffset bytes into the input, after an mbox envelope line the parse
// kept apart; see rootOriginAt.
func rootContextAt(session *ParseSession, headerOffset, bodyOffset int64) parseContext {
	return parseContext{session: session, origin: rootOriginAt(headerOffset, bodyOffset)}
}

// child returns the context of the index-th part of the entity this context
// describes, whose header block and body begin headerOffset and bodyOffset
// bytes into that entity's body.
//
// The session is carried down unchanged: a defect found three levels into a
// message is the same parse's defect as one found in its top-level header. The
// origin is not, because a child's position and path are its own; see
// entityOrigin.child, including what it does for a parent whose origin is not
// known. The depth is one more than this entity's, which is the step
// Limits.MaxMIMEDepth counts, and it is counted whether or not the origin is
// known.
func (c parseContext) child(index int, headerOffset, bodyOffset int64) parseContext {
	return parseContext{
		session: c.session,
		origin:  c.origin.child(index, headerOffset, bodyOffset),
		depth:   c.depth + 1,
		bounds:  c.bounds,
	}
}

// detached returns this context with its origin withdrawn: the same parse, but
// bytes that are not the parsed input's bytes.
//
// It is what the caller replacing an entity's Body leaves behind. What is read
// out of the replacement is still part of this parse and its defects still
// belong in this session, but nothing read from it can be given a position in
// the input or a place in the parsed tree, so it is given none rather than a
// wrong one.
//
// The depth is kept. How far the parser has descended is a property of the
// descent rather than of the bytes it descended through, so a replacement body
// does not buy back the nesting budget the entity above it already spent.
func (c parseContext) detached() parseContext {
	return parseContext{session: c.session, depth: c.depth, bounds: c.bounds}
}

// fatalErr returns the sticky failure of the parse this context belongs to, or
// nil while it may continue. It is what a parse scope calls before it begins
// and at each of its safe progress boundaries; see ParseSession.fatalErr for
// what that obligation is and why the session cannot discharge it itself.
//
// It returns an error rather than a FatalError so that a scope can hold the
// result in an ordinary error variable — one that also holds what record
// returns — without a nil FatalError in it reading as a non-nil error.
//
// A context with no session belongs to no parse that could be stopped, so
// fatalErr returns nil for it.
func (c parseContext) fatalErr() error {
	if err := c.session.fatalErr(); err != nil {
		return err
	}
	return nil
}

// headerLimits returns the budgets the parse this context belongs to puts on
// the header block of one entity, or no budgets at all for a parse that budgets
// nothing.
//
// It is asked of the context rather than read from a limits value threaded
// separately, for the same reason the session is carried here at all: the
// entity being descended into is reached long after Parse returned, and the
// configuration it must be held to is the one its own parse settled.
//
// A context with no session belongs to no parse that resolved a configuration,
// which is what every entry point but Parse produces, and it budgets nothing.
func (c parseContext) headerLimits() textproto.HeaderLimits {
	return c.session.headerLimits()
}

// limits returns the resolved budgets of the parse this context belongs to, and
// whether there is a parse enforcing any. See ParseSession.enforcedLimits.
func (c parseContext) limits() (Limits, bool) {
	return c.session.enforcedLimits()
}

// recordHeader records a diagnostic observed on the entity itself — on its
// header block, or on the metadata parsed out of it. The position it supplies
// is where that header block starts.
//
// See record for how a caller-supplied Path and Location are treated.
func (c parseContext) recordHeader(d Diagnostic) error {
	return c.record(d, c.origin.location())
}

// completeHeader returns d filled in from this context as recordHeader would
// fill it in, without recording it.
//
// It exists for the diagnostic that both ends a scope and is recorded: a
// FatalError carries its own copy of the diagnostic that stopped the scope, and
// that copy has to name the same entity and the same position as the one the
// session holds. Completing the diagnostic once and handing the same value to
// both is what keeps them from disagreeing.
//
// Completing a diagnostic twice is harmless — the second pass finds nothing
// left to fill in — so a completed diagnostic may still be passed to
// recordHeader.
func (c parseContext) completeHeader(d Diagnostic) Diagnostic {
	return c.complete(d, c.origin.location())
}

// headerLocationAt returns the position of a byte delta bytes into this
// entity's header block, for a diagnostic about one line or one field of it
// rather than about the block as a whole.
//
// It returns the zero Location — "position unknown" — for an entity whose own
// position is not known, so that a diagnostic about bytes the parser cannot
// place says so rather than claiming a position derived from nothing. record
// then leaves the location alone, since what the context would supply instead
// is also unknown.
func (c parseContext) headerLocationAt(delta int64) Location {
	if !c.origin.known {
		return Location{}
	}
	return LocationAt(c.origin.headerOffset + delta)
}

// bodyLocationAt returns the position of a byte delta bytes into this entity's
// body, on the same terms as headerLocationAt: the input as it was read, before
// any transfer encoding or charset was decoded, and the zero Location for an
// entity whose position is not known.
func (c parseContext) bodyLocationAt(delta int64) Location {
	if !c.origin.known {
		return Location{}
	}
	return LocationAt(c.origin.bodyOffset + delta)
}

// recordBody records a diagnostic observed while reading or decoding the
// entity's body rather than on the entity itself. The position it supplies is
// where that body starts, in the input as it was read, before any transfer
// encoding or charset was decoded.
//
// See record for how a caller-supplied Path and Location are treated.
func (c parseContext) recordBody(d Diagnostic) error {
	return c.record(d, c.origin.bodyLocation())
}

// completeBody returns d filled in from this context as recordBody would fill
// it in, without recording it. It is completeHeader for a defect of the body;
// see there for why a diagnostic that both ends a scope and is recorded has to
// be completed once and handed to both.
func (c parseContext) completeBody(d Diagnostic) Diagnostic {
	return c.complete(d, c.origin.bodyLocation())
}

// record completes d from the context and hands it to the session.
//
// What the context supplies is a default, never an override. The parser knows
// things the context cannot: a defect in one header field of a part is at that
// field, not at the top of the part's header block, and a defect the parser
// attributes to a different entity than the one being parsed carries that
// entity's path. So:
//
//   - a Path with at least one index is kept as it is, and an absent or empty
//     one is filled with this entity's path;
//   - any Location that is not the zero value is kept whole, including one
//     that carries only a line or only a column, and only the zero Location —
//     which means "position unknown" — is replaced by at.
//
// An unknown origin supplies neither: the path stays nil and the location
// stays the zero value, so a diagnostic about bytes the parser cannot place in
// the input says so rather than claiming offset zero, which is a real
// position. The diagnostic is still recorded — the defect is real even when
// its position is not known.
//
// The path this fills in is a copy, so the entity's own path is not reachable
// through the diagnostic. Beyond that, defensive copying is the session's
// existing job and is not repeated here: ParseSession.record clones what it
// retains and clones again for its sink, so neither the caller's diagnostic
// nor this entity's path can be disturbed through what the session holds, and
// vice versa.
//
// Once the session omits every further diagnostic under
// Limits.MaxDiagnosticCount, d is handed on as it is: completing it would only
// copy a path the session discards unread, and the session still answers the
// request exactly as it answers any other.
//
// record returns what the session returns: nil, or the sticky FatalError of a
// sink that asked the parse to stop. A nil session records nothing and returns
// nil.
func (c parseContext) record(d Diagnostic, at Location) error {
	if c.session.omittingDiagnostics() {
		return c.session.record(d)
	}
	return c.session.record(c.complete(d, at))
}

// complete fills in from this context what a diagnostic did not carry, on the
// terms record describes: an absent path becomes this entity's path, and an
// unknown position becomes at.
func (c parseContext) complete(d Diagnostic, at Location) Diagnostic {
	if len(d.Path) == 0 {
		d.Path = c.origin.pathCopy()
	}
	if d.Location == (Location{}) {
		d.Location = at
	}
	return d
}
