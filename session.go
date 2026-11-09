package message

import (
	"context"
	"sync"

	"github.com/saeroun-ai/emlkit/textproto"
)

// A ParseSession collects the diagnostics observed while a message is parsed.
//
// Parsing is streaming and lazy: multipart framing, transfer encodings and
// charsets are only inspected while the caller consumes the message, so
// defects are discovered long after the top-level entity has been returned. A
// session is where the parser accumulates those observations, and it stays
// readable after parsing has finished.
//
// Diagnostics are owned by the parser. A session offers no way for a caller to
// record one, so what a session reports is always what the parser actually
// observed. A session is append-only: a recorded diagnostic is never modified,
// reordered or removed, and Diagnostics reports diagnostics in discovery
// order.
//
// A session may also deliver each diagnostic to a DiagnosticSink as it is
// discovered. A session that delivers to a sink can be configured not to
// retain what it delivers, so that a streaming caller's memory does not grow
// with the number of defects; by default, and for the zero ParseSession, every
// diagnostic is retained.
//
// Independently of retention, Limits.MaxDiagnosticCount bounds how many
// diagnostics a session records and delivers at all, the zero ParseSession
// included, which runs under DefaultLimits. The request that reaches the budget
// is replaced, in its last slot, by one terminal limit.diagnostic_count
// diagnostic, and every later request is neither retained nor delivered; what
// was recorded before is never changed. What a session reports past that point
// is therefore not everything the parser observed, and the terminal says so.
// See that field.
//
// The zero ParseSession is ready to use: it holds no diagnostics, has no sink
// and reports the default profile and compatibility policy. A nil
// *ParseSession is usable too: Diagnostics returns nil and the accessors
// report those same defaults, so a caller can pass a session around without a
// nil check on every use.
//
// A ParseSession is safe for concurrent use by multiple goroutines: the parser
// may record diagnostics while another goroutine reads them. It must not be
// copied after first use.
type ParseSession struct {
	// cfg is the settled configuration of the parse: its profile, its
	// compatibility policy, and its resolved budgets. It is written once, when
	// the session is constructed, and never afterwards, so it is read without
	// the lock.
	cfg resolvedConfig

	sink DiagnosticSink

	// noRetention is the resolved retention decision, not the requested one:
	// it is only set when a sink takes delivery of what is not retained.
	noRetention bool

	// deliver serializes record for this session, so that the order its sink
	// observes is exactly the order Diagnostics reports. It is held across the
	// sink call, which is the one place a session waits for caller code. It
	// serializes this session only: sessions that share a sink deliver to it
	// concurrently.
	//
	// Lock order is deliver, then mu. mu is never held across a sink call, so
	// a sink may read the session it is delivering for without deadlocking; it
	// must not record, which the unexported record method already prevents.
	deliver sync.Mutex

	mu    sync.Mutex
	diags []Diagnostic

	// stop is the published session-wide failure of this parse: the one
	// FatalError every scope of it reports from the moment it exists. It is
	// written exactly once, by publishStop, and is never replaced afterwards.
	//
	// That it is never replaced is the contract, not an implementation detail.
	// A caller compares the error one scope handed it against the error another
	// scope handed it, so a value that changed identity after being observed
	// would make two correct scopes disagree about the same stop. Two things can
	// cause it — a DiagnosticSink that asked the parse to stop, and a resource
	// budget whose scope is the whole parse — and which of them wins is settled
	// once, where it is published, rather than every time it is read.
	//
	// It is stored as the interface so that a nil field converts to a nil error
	// rather than to a non-nil error holding a nil pointer.
	stop FatalError

	// sinkFailure is the sticky failure of the sink itself, kept apart from stop
	// because it answers a different question: whether the sink may be called
	// again. A failed sink is never called again for this session, which is its
	// own long-standing contract. A budget that ended the parse did not break
	// the caller's sink, so it does not suppress delivery.
	//
	// It is only ever set together with a publishStop under one acquisition of
	// mu, so a non-nil sinkFailure implies a non-nil stop.
	sinkFailure FatalError

	// diagSlots is how many record requests have taken one of the
	// Limits.MaxDiagnosticCount slots, the terminal slot included. It never
	// passes the budget: the request that takes the last slot closes the
	// session's diagnostics instead of incrementing past it, so a budget of
	// math.MaxInt64 cannot overflow it. It stays zero while the budget is
	// disabled. It is written with deliver and mu both held and read with mu.
	diagSlots int64

	// diagClosed is set, once and for good, by the request that took the
	// terminal slot. From then on every record request is omitted: it is
	// neither copied, retained nor delivered. It is guarded like diagSlots,
	// and since it never goes back to false, a reader that saw it set may rely
	// on it after releasing mu.
	diagClosed bool

	// partsUsed is how many child parts this parse has discovered anywhere in
	// the message. The root entity is not one of them. It is the usage side of
	// Limits.MaxPartCount, which is one budget shared by every multipart
	// iterator of the parse rather than one budget per container.
	partsUsed int64

	// partPending is open while one caller is turning a MaxPartCount overflow
	// into this session's published stop, and nil at every other time. It is
	// closed exactly once, by that caller, when stop has been published.
	//
	// It exists because publishing that stop takes two steps that cannot happen
	// under one lock: the diagnostic has to be delivered to the sink, and only
	// the sink's answer says whether the parse ends with the budget's failure or
	// with the caller's own. Anything that asks about the session's stop in
	// between waits here rather than being shown an answer that is about to
	// change. See ParseSession.chargePart and resolvePart in limit_tree.go.
	// A shared header budget overflow uses the same transition; see claimStop
	// in limit_header_total.go.
	partPending chan struct{}

	// decodedUsed is how many bytes the leaf bodies of this parse have
	// delivered, the usage side of Limits.MaxTotalDecodedBytes. It never passes
	// that budget, and stays zero while the budget is disabled.
	decodedUsed int64

	// decodedStop is set, once, when a byte past MaxTotalDecodedBytes has been
	// proven: it is the published stop, which every leaf body reports from then
	// on instead of delivering more. It is kept apart from stop because a stop
	// published for another reason does not end reading a body already
	// returned, while this one does. See limit_body.go.
	decodedStop FatalError

	// inputStop is the exact value the input fails every read with once
	// Limits.MaxInputBytes was exceeded or the input stalled (progress.go) —
	// that stop, or a stop published before it that was adopted — and nil
	// before that. It is set once. A leaf body recognizes the input's failure
	// by this identity alone; see rawBody.note.
	inputStop error

	// headerTotal is the one budget every header block of this parse spends,
	// Limits.MaxTotalHeaderBytes and MaxTotalHeaderCount, or nil while it has
	// not been needed or both are disabled. It is created once, under mu, by
	// headerLimits; see limit_header_total.go.
	headerTotal     *textproto.HeaderBudget
	headerTotalInit bool

	// lineNotes is the set of header line-ending notes this parse has already
	// claimed, each recorded at most once per parse; see line_endings.go.
	// Guarded by mu.
	lineNotes lineNote

	// mbox is the mbox envelope line this parse recognized at the start of
	// its input, its line ending included, or nil. It is set at most once,
	// before the message's header block is read, and never modified; see
	// mbox.go. Guarded by mu.
	mbox []byte

	// trailing is the input after the closing delimiter of the message's own
	// multipart body, registered once, by the first iterator of that body to
	// reach one, under ProfileForensic; nil before that and for every other
	// parse. trailingNone is set, under ProfileForensic, for a message whose
	// root frames no parts. Both are guarded by mu, and so is the state of
	// trailing that TrailingInput reports; see trailing.go.
	trailing     *trailingTail
	trailingNone bool

	// cancelCtx and cancelDone are the context of a parse started by
	// ParseContext and the channel its Done method returned, or nil for a parse
	// that cannot be cancelled. They are written once, by setContext, before the
	// session is shared, and never afterwards, so they are read without the lock.
	// No method of cancelCtx is called with a session lock held. See cancel.go.
	cancelCtx  context.Context
	cancelDone <-chan struct{}
}

// newParseSession returns an empty session that records the profile and
// compatibility policy the message is parsed with, and retains every
// diagnostic.
//
// This constructor is unexported on purpose: sessions are created by the
// parser. Parse is what configures one and hands it back to a caller.
func newParseSession(profile Profile, compat Compatibility) *ParseSession {
	return newParseSessionWithOptions(profile, compat, diagnosticOptions{})
}

// newParseSessionWithOptions returns an empty session that delivers and
// retains diagnostics as opts describes, running under the default budgets. It
// is unexported for the same reason as newParseSession.
func newParseSessionWithOptions(profile Profile, compat Compatibility, opts diagnosticOptions) *ParseSession {
	return newParseSessionWithConfig(resolvedConfig{
		profile: profile,
		compat:  compat,
		limits:  DefaultLimits(),
	}, opts)
}

// newParseSessionWithConfig returns an empty session running under a settled
// configuration. It is what Parse uses, and the only constructor that carries
// budgets the caller chose: the configuration is resolved once, before anything
// is read, and the session holds it unchanged for the rest of the parse.
func newParseSessionWithConfig(cfg resolvedConfig, opts diagnosticOptions) *ParseSession {
	return &ParseSession{
		cfg:  cfg,
		sink: opts.sink,
		// Dropping diagnostics that nothing else receives would lose them
		// outright, so no-retention only applies once a sink takes delivery.
		noRetention: opts.noRetention && opts.sink != nil,
	}
}

// Profile returns the profile the message was parsed with. It returns
// ProfileTolerant for a nil session.
func (s *ParseSession) Profile() Profile {
	if s == nil {
		return ProfileTolerant
	}
	return s.cfg.profile
}

// Compatibility returns the compatibility policy the message was parsed with.
// It returns CompatibilityConservative for a nil session.
func (s *ParseSession) Compatibility() Compatibility {
	if s == nil {
		return CompatibilityConservative
	}
	return s.cfg.compat
}

// Limits returns the resource budgets the message was parsed under, fully
// resolved: no field is zero, every one is either a positive budget or
// LimitDisabled, and reading one needs no knowledge of what an unset field
// would have meant.
//
// It reports DefaultLimits for a nil session and for the zero ParseSession,
// which are the budgets a parse configured with nothing runs under, and for the
// session of a configuration Parse rejected, whose requested budgets were never
// settled.
//
// Limits is a value, so what a caller receives is a copy: modifying it cannot
// change what the parse ran under, and the parser cannot change what a caller
// is holding.
func (s *ParseSession) Limits() Limits {
	if s == nil {
		return DefaultLimits()
	}
	// A resolved configuration has no zero field, so an all-zero value can only
	// be the zero session's: one that no parse ever configured.
	if s.cfg.limits == (Limits{}) {
		return DefaultLimits()
	}
	return s.cfg.limits
}

// enforcedLimits returns the budgets of this parse, which are the budgets
// Limits reports and no others — what a session says it ran under is what its
// parse enforced — together with whether there is a parse to enforce them.
//
// It reports false for a nil session. A nil session is not a parse whose
// budgets were resolved but the absence of one, which is what the entry points
// that allocate no session pass down, and their long-standing behavior — an
// unbudgeted header for every entity below the message, and no other budget
// anywhere — is kept exactly by budgeting nothing.
func (s *ParseSession) enforcedLimits() (Limits, bool) {
	if s == nil {
		return Limits{}, false
	}
	return s.Limits(), true
}

// headerLimits returns the budgets one header block of this parse is read
// under, in the wire-format layer's own spelling.
//
// The translation is the identity on values and on meaning: a resolved budget
// is a positive count or LimitDisabled, and LimitDisabled is negative, which is
// exactly what textproto reads as no budget. A parse with no session budgets
// nothing, so every field stays zero.
func (s *ParseSession) headerLimits() textproto.HeaderLimits {
	l, ok := s.enforcedLimits()
	if !ok {
		return textproto.HeaderLimits{}
	}
	return textproto.HeaderLimits{
		MaxBytes:      l.MaxHeaderBytes,
		MaxFieldBytes: l.MaxHeaderFieldBytes,
		MaxCount:      l.MaxHeaderCount,
		MaxLineBytes:  l.MaxLineBytes,
		Total:         s.headerBudget(l),
	}
}

// headerBudget returns the budget every header block of this parse shares,
// creating it on first use, or nil when both of its budgets are disabled. Every
// call returns the same value, so the message's own header and every part's at
// any depth spend one budget. LimitDisabled is negative, which is what textproto
// reads as one budget being off.
func (s *ParseSession) headerBudget(l Limits) *textproto.HeaderBudget {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.headerTotalInit {
		s.headerTotalInit = true
		if l.MaxTotalHeaderBytes != LimitDisabled || l.MaxTotalHeaderCount != LimitDisabled {
			s.headerTotal = textproto.NewHeaderBudget(l.MaxTotalHeaderBytes, l.MaxTotalHeaderCount)
		}
	}
	return s.headerTotal
}

// record adds a diagnostic to the session and delivers it to the configured
// sink. It is the only way to add one, and it is unexported so that a session
// can only report what the parser observed.
//
// record stores a copy whose Path is a distinct slice, and hands the sink a
// further copy, so neither the parser's path buffer nor the retained history
// can be disturbed by the other. Diagnostic.Cause is retained as given.
//
// record returns a FatalError once a sink has failed. A sink error is not a
// defect of the message but a caller asking the parse to stop, so it is
// reported as a fatal error that wraps the sink's own error — errors.Is still
// matches it — and that carries no diagnostic. The first such error is sticky:
// it is wrapped once, kept, and returned unchanged by every later record, and
// the failed sink is never called again for this session.
//
// What record returns is the session's published stop rather than the error it
// happened to build, and the two differ in exactly one case: a resource budget
// whose scope is the whole parse got there first. Reporting what is published
// is what keeps every scope of a parse holding one error value; see publishStop.
// A budget stop does not suppress delivery — it did not break the caller's sink
// — so only a failed sink stops the sink being called.
//
// The scope of that error is the session, not one entity: a caller that asked
// the parse to stop asked for all of it, so no scope of this session may start
// or continue once it observes the stop, and each returns what it had already
// recovered. Observing it is the scope's job, through fatalErr; record is the
// backstop for a scope that reaches this point anyway. Values already returned
// stay valid, as they do for any fatal error. Later diagnostics are still
// recorded, and still retained when the session retains them, leaving the
// record of what happened after the failure readable, up to
// Limits.MaxDiagnosticCount.
//
// Every request takes one slot of Limits.MaxDiagnosticCount while that budget is
// positive, whether or not the session retains and whether or not it has a sink,
// so the budget bounds what is delivered and what is kept alike. The last slot
// is reserved: the request that takes it is not recorded itself, and the one
// limit.diagnostic_count diagnostic built by diagnosticCountTerminal is recorded
// and delivered in its place, exactly as an ordinary diagnostic would be. Every
// later request is omitted before anything is copied: it is not retained, the
// sink is not called, and it returns what an ordinary request returns from then
// on — the published stop once the sink has failed, nil otherwise. The terminal
// is built here rather than recorded through a second call, so it cannot recurse
// and cannot be counted twice.
//
// Closing the diagnostics ends nothing by itself. The request the terminal
// replaces returns nil, as for any diagnostic, unless the sink refuses the
// terminal or had already failed. Every budget, Strict refusal and sink failure
// is decided by its own code, independently of whether its diagnostic was kept,
// and the request it was recorded with may itself be one of them, so none of
// them is lost to this one. An omitted request returns exactly what an ordinary
// request returns once its sink is no longer called, so it can neither replace
// nor hide a stop already published: a budget's stop is read from fatalErr as it
// always was, and a failed sink's is returned here as it always was.
//
// Once the context of a parse started by ParseContext is done, record omits
// every request — nothing is retained and the sink is not called — and returns
// the session's stop: the caller ended the parse, so nothing more is reported
// to it. That is checked before the delivery lock is taken and again once it
// is held, before a slot is claimed, so a request that waited behind a slow
// sink while the context was cancelled is omitted too. A delivery already
// running when the context is cancelled is left as it was: its diagnostic is
// recorded and its sink call runs to its end. See cancel.go.
//
// record is a no-op on a nil session, and returns nil.
//
// Concurrency: record may be called from several goroutines. Calls on one
// session are serialized by its delivery lock, so its sink sees diagnostics in
// exactly the order Diagnostics reports them. Concurrent calls are ordered by
// whichever acquires that lock first: a session cannot order events the parser
// did not order. Each goroutine's own diagnostics keep their relative order.
// Different sessions are independent and deliver concurrently, including to a
// shared sink.
func (s *ParseSession) record(d Diagnostic) error {
	if s == nil {
		return nil
	}

	// A parse whose context has been cancelled records nothing further: the
	// caller ended it, so no diagnostic is retained or delivered to a sink from
	// then on, and the request returns the session's stop. A diagnostic
	// recorded before this point stays recorded. See cancel.go.
	//
	// It is asked twice. First before waiting for the delivery lock, where the
	// stop can be built at once. Then again once the lock is held, under mu and
	// before a slot is claimed or anything is retained: a request that waited
	// behind a delivery to a slow sink may find the context cancelled meanwhile,
	// and is then omitted like any other. That second look only receives from
	// the Done channel; the stop is built after both locks are released, since
	// building it calls the context's methods. A delivery already running when
	// the context is cancelled is not interrupted.
	if stop := s.cancelStop(); stop != nil {
		return stop
	}

	s.deliver.Lock()

	s.mu.Lock()
	if s.cancelSignalled() {
		s.mu.Unlock()
		s.deliver.Unlock()
		return s.cancelStop()
	}
	defer s.deliver.Unlock()
	sink, failed := s.sink, s.sinkFailure
	// A failed sink implies a published stop, so this is the identity every
	// other scope of the parse is reporting.
	published := s.stop
	slot := s.claimDiagnosticSlot()
	if slot == diagnosticOmitted {
		s.mu.Unlock()
		if failed != nil {
			return published
		}
		return nil
	}
	var c Diagnostic
	if slot == diagnosticTerminal {
		c = s.diagnosticCountTerminal(d)
	} else {
		c = d.Clone()
	}
	retained := !s.noRetention
	if retained {
		s.diags = append(s.diags, c)
	}
	s.mu.Unlock()

	if failed != nil {
		return published
	}
	if sink == nil {
		return nil
	}

	if retained {
		// The session owns c now, so the sink gets its own copy.
		c = c.Clone()
	}
	// The session is passed to the sink rather than left to be captured: the
	// parser delivers diagnostics it discovers before it has handed the
	// session to the caller.
	if err := sink.Diagnose(s, c); err != nil {
		// deliver serializes record for this session and a failed sink is
		// never called again, so this is the first sink error.
		fatal := newFatalError("diagnostic sink stopped the parse", err)
		s.mu.Lock()
		s.sinkFailure = fatal
		// Ordinarily this is what the parse stops with. It is published rather
		// than assigned so that the one case where something else got there
		// first — a budget failure already published — keeps the identity every
		// scope is already reporting, instead of a second one appearing here.
		published := s.publishStop(fatal)
		s.mu.Unlock()
		return published
	}
	return nil
}

// publishStop publishes f as this session's one session-wide failure and returns
// the failure that is published, which is f only when nothing was published
// before it.
//
// It is the single place a session-wide stop is decided, and that is the whole
// point of it. Both of the things that can end a whole parse — a sink refusing
// to continue, and a resource budget whose scope is the parse — go through here,
// so the precedence between them is settled once, at the moment of publication,
// and never re-derived on a later read. Whatever is published first is what
// every caller receives from then on.
//
// It must be called with s.mu held, and f must not be nil.
func (s *ParseSession) publishStop(f FatalError) FatalError {
	if s.stop == nil {
		s.stop = f
	}
	return s.stop
}

// fatalErr returns the session's sticky fatal error, or nil while parsing may
// continue. It returns the identical value on every call, so a scope can
// propagate what it observed and a caller can compare what it received.
//
// This accessor is what makes a session-wide stop real, and it is the parser's
// obligation, not the session's: a session cannot interrupt a scan that never
// looks at it. A parse scope must call fatalErr before it begins, and again at
// each safe progress boundary — a point where it can return what it has
// recovered without leaving a half-built result — and then stop as FatalError
// describes, returning that error together with whatever it recovered up to
// that point.
//
// A session only reaches the scopes of a parse started by Parse. Read and
// ReadWithOptions carry no session, so there is nothing to stop what they
// return, and Entity.Walk and mail.Reader stop only for an entity that came from
// Parse; Entity.ParseStop is how a layer outside this package, such as
// mail.Reader, asks. The stickiness of record
// remains the backstop for a scope that does not check: it is handed the same
// fatal error back rather than being allowed to continue quietly.
//
// It is deliberately unexported, and offers no way to request or clear a stop.
// A session reports what the parser observed, so a caller can neither stop a
// parse through it nor clear a stop; a caller asks for a stop through a
// DiagnosticSink, or by cancelling the context given to ParseContext. The one
// write it can make is taking up such a cancellation, described below: it
// publishes the cancellation when it observes the context done and nothing was
// published first. Entity.ParseStop is the one exported reading of it, on the
// same terms.
//
// Two failures can end a whole session — a sink that asked the parse to stop,
// and a resource budget whose scope is the parse — and which of them a session
// ends with is decided where it is published rather than here; see publishStop.
// What this reports is that one published value, unchanged, for ever. A sink
// stop outranks a budget failure, because it is the caller ending this parse
// rather than a defect of the message, and publishStop is where that ordering
// lives.
//
// fatalErr waits, at most once and only while a MaxPartCount or shared header
// budget overflow is being turned into that published value. Deciding it means delivering a diagnostic to
// the caller's sink, and only the sink's answer says which of the two failures
// the parse ends with; a scope that asked in the middle of that would otherwise
// have to be given an answer that was about to change. Waiting is safe under the
// existing synchronous sink contract, which already blocks the parse for as long
// as the sink blocks: no session lock is held while waiting, and the caller
// finishing the transition never waits here itself, so a sink may read the
// session it is delivering for exactly as it always could. See
// ParseSession.resolvePart.
//
// For a parse started by ParseContext, fatalErr is also where a scope sees that
// the caller cancelled the parse's context: once the context is done, it
// publishes the cancellation as the stop if nothing was published before it,
// and returns the published stop. That is the one way it writes anything, and
// it is the caller's own request being taken up, not a stop a caller could
// make or clear through the session. While it waits for a transition it also
// wakes when the context is done, so a sink blocked in the delivery that
// decides the transition cannot keep a cancelled parse's other scopes waiting.
// The context's methods are called with no session lock held. See cancel.go.
//
// fatalErr is safe for concurrent use, and returns nil for a nil session.
func (s *ParseSession) fatalErr() FatalError {
	if s == nil {
		return nil
	}
	for {
		s.mu.Lock()
		published, pending := s.stop, s.partPending
		s.mu.Unlock()

		if published != nil {
			return published
		}
		if stop := s.cancelStop(); stop != nil {
			return stop
		}
		if pending == nil {
			return nil
		}
		// The transition publishes before it closes this, so the next pass
		// finds the published value rather than looping. A nil cancelDone is a
		// case that never fires.
		select {
		case <-pending:
		case <-s.cancelDone:
		}
	}
}

// Len returns the number of retained diagnostics without copying them. It
// returns 0 for a nil session, and for a session that delivers to a sink
// without retaining.
func (s *ParseSession) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.diags)
}

// Diagnostics returns the diagnostics recorded so far, in discovery order. It
// holds at most Limits.MaxDiagnosticCount entries, and the terminal
// limit.diagnostic_count diagnostic is the last of them when that budget was
// reached.
//
// The returned slice and the Path of every returned diagnostic are freshly
// allocated, so the caller may retain and modify them without affecting the
// session, and a concurrent record cannot change an already returned result.
// Diagnostic.Cause is not copied: it is the same error value the parser
// recorded, so anything it refers to is shared. Diagnostics returns nil when
// nothing has been retained, for a nil session, and for a session that
// delivers to a sink without retaining.
func (s *ParseSession) Diagnostics() []Diagnostic {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.diags) == 0 {
		return nil
	}
	out := make([]Diagnostic, len(s.diags))
	for i, d := range s.diags {
		out[i] = d.Clone()
	}
	return out
}
