package message

import (
	"io"
	"sync/atomic"
)

// Completion: what a parse has and has not established (issue #10).
//
// Parse reads the header and returns. Everything else — bodies, parts, the
// defects in them — is discovered as the caller reads, so an empty diagnostic
// list says nothing by itself: it is the list of a message nobody has read
// yet as much as that of a clean one. ParseSession.Completion says which, and
// Entity.Drain reads whatever is still unread so that the answer is final.

// A CompletionState is how far a parse has got and how it stands.
type CompletionState uint8

const (
	// CompletionNotConsumed: only the message header has been read. No body
	// was read and no part was asked for.
	CompletionNotConsumed CompletionState = iota

	// CompletionInProgress: some of the message has been read, and some has
	// not: a body or a multipart was returned to the caller and not read to
	// its end. A caller that stopped reading early, or skipped a part's
	// body, stays here; it is not a clean result.
	CompletionInProgress

	// CompletionComplete: every body and every multipart the parse returned
	// was read to its end, nothing stopped the parse, and no diagnostic
	// reports lost syntax, a budget, or a failure. The input itself need not
	// have ended: what follows a multipart's closing delimiter is not the
	// message's, and is not read.
	CompletionComplete

	// CompletionIncomplete: everything was read to its end, and at least one
	// diagnostic of SeverityIncomplete or SeverityFatal was recorded: the
	// message lacks something it should have had — a closing delimiter, the
	// end of a line — or a scope of it could not be read. What was recovered
	// is usable.
	CompletionIncomplete

	// CompletionLimited: a resource budget ended the parse or one of its
	// scopes (a limit.* diagnostic of SeverityLimitExceeded), or the input
	// stalled.
	CompletionLimited

	// CompletionCanceled: the context of ParseContext was cancelled or its
	// deadline passed.
	CompletionCanceled

	// CompletionStopped: a DiagnosticSink stopped the parse.
	CompletionStopped

	// CompletionFailed: the input returned an error other than io.EOF.
	CompletionFailed
)

// String returns the lower-case name of the state.
func (c CompletionState) String() string {
	switch c {
	case CompletionNotConsumed:
		return "not-consumed"
	case CompletionInProgress:
		return "in-progress"
	case CompletionComplete:
		return "complete"
	case CompletionIncomplete:
		return "incomplete"
	case CompletionLimited:
		return "limited"
	case CompletionCanceled:
		return "canceled"
	case CompletionStopped:
		return "stopped"
	case CompletionFailed:
		return "failed"
	}
	return "unknown"
}

// A Completion is a snapshot of what a parse has established so far.
//
// It describes the reading, not the transport: an input that ends cleanly
// where a message could end is Complete even if the message was cut in
// transit at just such a place, since nothing in the bytes says so. What the
// bytes do say — an unclosed multipart, a line with no ending, a cut Base64
// group — is reported as Incomplete.
type Completion struct {
	// State is the one-word answer; see CompletionState.
	State CompletionState

	// InputBytes is how many bytes have been read from the input, which may
	// be more than the parser has consumed: readers buffer ahead.
	InputBytes int64
	// InputEnded reports that the input returned io.EOF, and InputErr is the
	// error it returned instead, if any.
	InputEnded bool
	InputErr   error

	// Stop is the error that ended the whole parse — a budget, a
	// cancellation, a sink — or nil; see Entity.ParseStop.
	Stop error

	// Entities is how many entities the parse has returned, the message
	// itself included. Bodies and BodiesRead count the leaf bodies among
	// them and those read to their end; Multiparts and MultipartsRead the
	// multipart bodies and those iterated to their end. A body the caller
	// skipped, and a part the caller never asked for, is the difference.
	Entities       int64
	Bodies         int64
	BodiesRead     int64
	Multiparts     int64
	MultipartsRead int64

	// Diagnostics counts every diagnostic the parse produced by severity,
	// indexed by Severity, including those Limits.MaxDiagnosticCount kept out
	// of the session.
	Diagnostics [7]int64

	// Ambiguous reports that the parse had to choose between readings of the
	// message that differ in what it means: more than one boundary that
	// framed parts, conflicting repeats of Content-Type or of a parameter,
	// conflicting filenames.
	Ambiguous bool
}

// Clean reports whether the message was read whole and nothing at all was
// recorded about it but notes of SeverityInfo: State is CompletionComplete
// and no diagnostic repaired, skipped or refused anything.
func (c Completion) Clean() bool {
	if c.State != CompletionComplete || c.Ambiguous {
		return false
	}
	for sev, n := range c.Diagnostics {
		if Severity(sev) != SeverityInfo && n > 0 {
			return false
		}
	}
	return true
}

// sessionProgress is what a session counts for Completion. The counters are
// atomic so that the readers of a parse can update them without the session's
// lock.
type sessionProgress struct {
	inputBytes     atomic.Int64
	entities       atomic.Int64
	bodies         atomic.Int64
	bodiesRead     atomic.Int64
	multiparts     atomic.Int64
	multipartsRead atomic.Int64
	touched        atomic.Bool
	ambiguous      atomic.Bool
	severities     [7]atomic.Int64
}

// noteDiagnostic counts d, whether or not the session keeps it.
func (p *sessionProgress) noteDiagnostic(d Diagnostic) {
	if int(d.Severity) < len(p.severities) {
		p.severities[d.Severity].Add(1)
	}
	switch d.Code {
	case CodeMultipartBoundaryAmbiguous, CodeMIMEParameterDuplicate, CodeMIMEFilenameConflict:
		p.ambiguous.Store(true)
	case CodeHeaderDuplicateSingleton:
		if d.Severity == SeverityRecovered {
			p.ambiguous.Store(true)
		}
	}
}

// noteInput records what a read of the input returned.
func (s *ParseSession) noteInput(n int, err error) {
	if s == nil {
		return
	}
	if n > 0 {
		s.prog.inputBytes.Add(int64(n))
	}
	if err != nil {
		s.mu.Lock()
		if s.inputEnd == nil {
			s.inputEnd = err
		}
		s.mu.Unlock()
	}
}

// Completion reports what the parse has established so far. It reads nothing
// and may be called at any time; the answer is final once nothing of the
// message is left unread, which Entity.Drain makes sure of.
//
// A nil session reports the zero Completion, CompletionNotConsumed.
func (s *ParseSession) Completion() Completion {
	if s == nil {
		return Completion{}
	}
	p := &s.prog
	c := Completion{
		InputBytes:     p.inputBytes.Load(),
		Entities:       p.entities.Load(),
		Bodies:         p.bodies.Load(),
		BodiesRead:     p.bodiesRead.Load(),
		Multiparts:     p.multiparts.Load(),
		MultipartsRead: p.multipartsRead.Load(),
		Ambiguous:      p.ambiguous.Load(),
	}
	for i := range c.Diagnostics {
		c.Diagnostics[i] = p.severities[i].Load()
	}

	s.mu.Lock()
	inputEnd := s.inputEnd
	var stop error
	if s.stop != nil {
		stop = s.stop
	}
	sinkStopped := s.sinkFailure != nil
	s.mu.Unlock()

	c.Stop = stop
	switch {
	case inputEnd == io.EOF:
		c.InputEnded = true
	case inputEnd != nil && (stop == nil || inputEnd != stop):
		c.InputErr = inputEnd
	}

	read := c.BodiesRead == c.Bodies && c.MultipartsRead == c.Multiparts
	switch {
	case s.cancelErr() != nil:
		c.State = CompletionCanceled
	case sinkStopped:
		c.State = CompletionStopped
	case stop != nil:
		c.State = CompletionLimited
	case c.InputErr != nil:
		c.State = CompletionFailed
	case !p.touched.Load():
		c.State = CompletionNotConsumed
	case !read:
		c.State = CompletionInProgress
	case c.Diagnostics[SeverityLimitExceeded] > 0:
		c.State = CompletionLimited
	case c.Diagnostics[SeverityIncomplete] > 0 || c.Diagnostics[SeverityFatal] > 0:
		c.State = CompletionIncomplete
	default:
		c.State = CompletionComplete
	}
	return c
}

// Drain reads everything of the entity that has not been read yet and
// discards it: every body to its end, every multipart to its close, and,
// under ParseOptions.WalkEmbeddedMessages, every embedded message. It is how
// a caller that wants the parse's whole verdict — ParseSession.Completion and
// every diagnostic — gets it without wanting the bytes.
//
// It returns nil when the walk ended at the end of the message, and
// otherwise what ended it: the parse's stop, or an error of an iterator. A
// body that ends with an error of its own does not end the walk; the session
// has its diagnostic. Drain may be called again, and after a partial read; it
// reads what is left.
func (e *Entity) Drain() error {
	return e.Walk(func(_ []int, p *Entity, _ error) error {
		if p.MultipartReader() != nil || p.walkEmbedded() != nil {
			return nil
		}
		if p.Body != nil {
			io.Copy(io.Discard, p.Body)
		}
		return nil
	})
}
