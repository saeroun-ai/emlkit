package message

import "errors"

// A FatalError reports that the parser cannot make safe forward progress in
// the scope that returned it.
//
// # Scope
//
// The scope is the API boundary that produced the error, and it is exactly as
// large as that boundary: one entity, one iterator over parts, or the whole
// parse and the session behind it. A fatal error is a statement about that
// scope and about nothing larger.
//
// Two consequences follow, and only these two:
//
//   - Values already returned are never invalidated. Entities, parts and bytes
//     a caller is already holding stay valid and usable, whatever fails
//     afterwards.
//   - A fatal error confined to a child scope does not by itself end the
//     scopes around it. A part that cannot be parsed any further does not
//     invalidate its parent or the siblings already returned.
//
// What it does not mean is that every fatal error is small. Some scopes are
// the entire parse: when a DiagnosticSink stops the parse, the failure belongs
// to the session, and no scope of that session starts or continues once it
// observes the stop — though one already in progress still returns what it
// recovered before that point. Which scope ended is decided by the API that
// returned the error, so a caller reads it there rather than inferring it from
// the error.
//
// # Fatal errors and results
//
// A scope that stops fatally still hands back whatever it recovered first, so
// the error and the result answer two different questions:
//
//	part, err := mr.NextPart()
//	if part != nil {
//		// Usable and owned by the caller, whatever err says.
//	}
//	if message.IsFatal(err) {
//		// This iterator ends here; the parts it already returned remain valid.
//	}
//
// A non-nil result is the recovered prefix: as much as the parser could safely
// make of the scope before it stopped, owned by the caller, and not
// invalidated by the error. A nil result means no safe result could be
// constructed at all.
//
// Only the returned value can say which of the two happened, so that is where
// a caller looks. The result is deliberately not carried inside the error, and
// a FatalError has no "partial" flag: a flag on the error could not describe a
// result the error does not hold, and would be wrong as soon as the two
// disagreed.
//
// # What is not fatal
//
// UnknownCharsetError and UnknownEncodingError accompany a usable result and
// are warnings, not failures; the io.EOF that ends an iterator is ordinary
// control flow. None of them satisfies IsFatal, and their behavior is
// unchanged.
//
// # Using it
//
// FatalError is a sealed interface: its unexported method can only be
// implemented inside this package, so a value that satisfies it was produced
// by the parser and its invariants hold. Test it with IsFatal, or take it out
// of an error chain the ordinary way:
//
//	var fatal message.FatalError
//	if errors.As(err, &fatal) {
//		if d, ok := fatal.Diagnostic(); ok {
//			log.Println(d.Code)
//		}
//	}
//
// The parser never returns a nil FatalError, and errors.As only assigns a
// non-nil one; as with any interface, calling a method on a nil value panics.
//
// A fatal error wraps the error that caused it, so errors.Is, errors.As and
// errors.Unwrap reach that cause through it, including when the fatal error
// has been combined with other errors by errors.Join.
type FatalError interface {
	error

	// Diagnostic returns the diagnostic describing why the scope stopped, and
	// whether there is one. Not every fatal error has one: a diagnostic
	// reports a defect observed in the message, so a failure that is not a
	// property of the message — a caller's callback refusing to continue, for
	// example — has none, and Diagnostic reports that explicitly with a false
	// second result rather than by returning a zero Diagnostic that could be
	// mistaken for a real one.
	//
	// The returned diagnostic is a copy whose Path is a distinct slice, so a
	// caller may retain and modify it freely. As with Diagnostic.Clone, Cause
	// is an arbitrary error value and is not copied: it is the same error the
	// parser recorded, and it is also what errors.Unwrap returns for this
	// error.
	Diagnostic() (Diagnostic, bool)

	// fatalError seals the interface to this package, which is the only place
	// that can decide a scope of a message cannot be parsed any further, and
	// the only place that can establish the invariants a fatal error carries,
	// such as the severity of its diagnostic.
	fatalError()
}

// fatalParseError is the parser's implementation of FatalError. It is
// unexported, along with its constructors, so that a fatal error can only come
// from the parser.
type fatalParseError struct {
	// what names the situation that ended the scope, for example the
	// diagnostic sink asking the parse to stop. It is empty when the cause or
	// the diagnostic already says everything.
	what string

	// diag describes the defect that stopped the scope, and is only meaningful
	// when hasDiag is true. It is cloned on the way in and on the way out, so
	// neither the parser nor a caller can reach the other's copy.
	diag    Diagnostic
	hasDiag bool

	// cause is the underlying error, or nil when there is none. For a
	// diagnostic-backed fatal error it is that diagnostic's own Cause, so
	// unwrapping the error and reading the diagnostic lead to the same value.
	cause error
}

var _ FatalError = (*fatalParseError)(nil)

// newFatalError returns a fatal error caused by cause, with no diagnostic
// attached. what is a short description of the situation that ended the scope
// and may be empty.
//
// It returns the interface rather than the concrete type so that a nil result
// cannot be mistaken for a non-nil error by a caller storing it.
func newFatalError(what string, cause error) FatalError {
	return &fatalParseError{what: what, cause: cause}
}

// newFatalDiagnostic returns a fatal error described by d.
//
// It stores a clone of d whose Severity is SeverityFatal, which is the
// invariant that keeps a fatal error and its diagnostic from disagreeing, and
// one of the reasons FatalError is sealed to this package. The diagnostic's
// Cause becomes the error's cause.
func newFatalDiagnostic(d Diagnostic) FatalError {
	c := d.Clone()
	c.Severity = SeverityFatal
	return &fatalParseError{diag: c, hasDiag: true, cause: c.Cause}
}

// Error implements the error interface. The text is for humans: program logic
// should use IsFatal, errors.Is, errors.As, or the diagnostic's Code.
func (e *fatalParseError) Error() string {
	s := "message: fatal parse error"
	if e.what != "" {
		s += ": " + e.what
	}
	switch {
	case e.hasDiag:
		// The diagnostic renders its own cause, so the cause is not repeated.
		s += ": " + e.diag.String()
	case e.cause != nil:
		s += ": " + e.cause.Error()
	}
	return s
}

// Unwrap returns the error that caused the scope to stop, or nil when the
// parser recorded none. It is what makes errors.Is and errors.As see through a
// fatal error to the original failure. It is deliberately not part of the
// FatalError interface: errors.Unwrap and the errors package find it on the
// concrete type.
func (e *fatalParseError) Unwrap() error {
	return e.cause
}

// Diagnostic implements FatalError.
func (e *fatalParseError) Diagnostic() (Diagnostic, bool) {
	if !e.hasDiag {
		return Diagnostic{}, false
	}
	return e.diag.Clone(), true
}

// fatalError implements FatalError.
func (e *fatalParseError) fatalError() {}

// IsFatal reports whether err says that a scope of the message could not be
// parsed any further, as described by FatalError. It finds a fatal error
// anywhere in err's chain, including inside an error built by errors.Join.
//
// A fatal error never invalidates a value already returned, nor a non-nil
// result returned alongside it, but how much of the parse ends with it depends
// on the scope that returned it. IsFatal returns false for nil, for the
// warnings reported by IsUnknownCharset and IsUnknownEncoding, and for the
// io.EOF that ends an iterator.
func IsFatal(err error) bool {
	var fatal FatalError
	return errors.As(err, &fatal)
}
