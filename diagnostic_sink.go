package message

// A DiagnosticSink receives diagnostics as the parser discovers them.
//
// A sink lets a streaming caller act on each defect as it is observed — log
// it, count it, or ask for the parse to stop — instead of waiting for the
// parse to finish and reading the whole list back. Together with a session
// that does not retain diagnostics, it keeps the memory a parse needs
// independent of how damaged the message is.
//
// A sink is called synchronously, from whichever goroutine discovered the
// diagnostic. Delivery is serialized per session: one session has at most one
// Diagnose call in flight, and that call returns before the next one for the
// same session starts. Serialization does not extend further than that. One
// sink instance may be configured on several sessions, and those sessions
// deliver concurrently, so a sink that keeps state shared between sessions
// must synchronize it itself.
//
// A sink must not block indefinitely: the session that is delivering cannot
// make progress while its Diagnose call runs.
type DiagnosticSink interface {
	// Diagnose handles one diagnostic discovered by session.
	//
	// The session is passed rather than captured by the sink because the
	// parser delivers diagnostics before the caller can hold the session: a
	// defect in the top-level header is observed and delivered while the parse
	// call that would return the session is still running. It is never nil.
	//
	// Diagnose may read that session: Diagnostics, Len, Profile and
	// Compatibility are safe to call from inside Diagnose and do not deadlock.
	// When the session retains diagnostics, the diagnostic being delivered is
	// already included in what those accessors report.
	//
	// The diagnostic is a copy the sink may retain and modify freely; it
	// shares nothing mutable with the session or with the parser, except
	// Diagnostic.Cause, which is the error value the parser recorded.
	//
	// Returning a non-nil error stops further delivery to this sink: the
	// session keeps the first error, never calls the sink again for that
	// session, and reports it back to the parser as a FatalError wrapping it,
	// so that errors.Is against the returned error still matches what the sink
	// returned.
	//
	// Stopping is deliberately session-wide rather than local to the entity
	// that happened to be parsing: a sink that refuses to continue is the
	// caller ending this parse, so no scope of that session starts or continues
	// once it observes the stop. A scope already in progress may still return
	// what it recovered before that point, and values already returned to the
	// caller stay valid; see FatalError.
	Diagnose(session *ParseSession, d Diagnostic) error
}

// A DiagnosticSinkFunc adapts an ordinary function to a DiagnosticSink.
type DiagnosticSinkFunc func(session *ParseSession, d Diagnostic) error

// Diagnose calls f.
func (f DiagnosticSinkFunc) Diagnose(session *ParseSession, d Diagnostic) error {
	return f(session, d)
}

// diagnosticOptions configures how a session delivers and keeps diagnostics.
//
// It is the internal form of what ParseOptions carries. It stays unexported so
// that a session is configured only where one is created, which is Parse.
//
// The zero value is the default: no sink, and every diagnostic retained.
type diagnosticOptions struct {
	// sink receives every diagnostic as it is discovered, or is nil when
	// diagnostics are only collected.
	sink DiagnosticSink

	// noRetention asks the session not to keep the diagnostics it delivers,
	// for a caller that handles them in its sink as they arrive. It is
	// ignored while sink is nil: diagnostics are never dropped without being
	// delivered somewhere.
	noRetention bool
}
