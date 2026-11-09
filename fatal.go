package message

import "github.com/saeroun-ai/emlkit/internal/errgraph"

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
// control flow.
//
// The parser decides that an error is a warning by the error it returned
// itself, not by searching it: a part returned with an UnknownCharsetError or
// UnknownEncodingError is usable, and iteration goes on past it, whatever that
// warning's cause is. IsFatal answers a different question — whether a genuine
// fatal error is anywhere in a graph — so it is false for an ordinary decoder
// warning but true for one whose cause holds a fatal error, which a
// CharsetReader failing with a fatal error from another parse puts there. Such
// a warning still describes this part, not a stop of this parse, and the parser
// treats it as the warning it is.
//
// # Using it
//
// Test an error with IsFatal. It recognizes only a fatal error the parser
// created, wherever it sits in a graph of wrapped and joined errors within a
// fixed bound, and it never asks the error under test about itself; see
// IsFatal for exactly what it walks.
//
// FatalError has an unexported method, so a type declared elsewhere cannot
// implement it on its own. That alone does not prove a value came from the
// parser: a struct in another package that embeds a FatalError field inherits
// the method, and satisfies the interface even when the embedded value is nil.
// IsFatal does not accept such a value; satisfying the interface does not make
// one.
//
// To read the diagnostic, take the fatal error from the result a parse API
// returned directly. Such an error is either one the parser created or one the
// caller's own components failed with — the io.Reader, notably, whose read
// failures are passed on unchanged — so a type assertion on it relies on
// nothing beyond the parser and those components, and calls nothing:
//
//	if fatal, ok := err.(message.FatalError); ok {
//		if d, ok := fatal.Diagnostic(); ok {
//			log.Println(d.Code)
//		}
//	}
//
// That holds only while those components do not themselves fail with values
// that embed a FatalError; the message's bytes cannot make one, but a
// component's code can.
//
// The standard library's errors.As also finds a fatal error inside a chain of
// wrappers, but on the standard library's terms, which are not IsFatal's: it
// calls the As method of every error on its way, so an error can claim to be a
// fatal error and assign anything, a value that embeds a nil one included; and
// it follows Unwrap without any bound, so a cycle never lets it return. Use it
// only on chains whose every wrapper is trusted, such as ones your own code
// built around a parser result. IsFatal returning true for an error does not
// make errors.As safe on it: that a genuine fatal error is reachable says
// nothing about what errors.As meets on its own way there. As with any
// interface, calling a method on a nil FatalError panics.
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
	// The diagnostic is the failure's own, whether or not the session recorded
	// it: once Limits.MaxDiagnosticCount is reached the session replaces it
	// with the terminal limit.diagnostic_count diagnostic or omits it, but the
	// fatal error still carries it, so the reason a scope stopped is never lost
	// to that budget.
	//
	// The returned diagnostic is a copy whose Path is a distinct slice, so a
	// caller may retain and modify it freely. As with Diagnostic.Clone, Cause
	// is an arbitrary error value and is not copied: it is the same error the
	// parser recorded, and it is also what errors.Unwrap returns for this
	// error.
	Diagnostic() (Diagnostic, bool)

	// fatalError keeps types declared elsewhere from implementing the interface
	// by themselves. It is not proof of origin — an embedded FatalError field
	// passes it on — which is why IsFatal recognizes the parser's own concrete
	// type rather than this interface.
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
// parsed any further, as described by FatalError: whether a non-nil fatal error
// the parser created is err, or is reachable from it.
//
// A fatal error never invalidates a value already returned, nor a non-nil
// result returned alongside it, but how much of the parse ends with it depends
// on the scope that returned it. IsFatal returns false for nil, for the io.EOF
// that ends an iterator, and for the parser's decoding warnings unless their
// cause holds a genuine fatal error; see "What is not fatal" on FatalError.
//
// What it walks is err's whole error graph, not one chain of it: it follows
// Unwrap() error and Unwrap() []error, so a fatal error wrapped by fmt.Errorf
// with %w, joined by errors.Join, or held by a custom wrapper's Unwrap is
// found. It matches by the parser's own concrete type and never calls an Is,
// As or Error method, so an error cannot pass by claiming to be fatal, and a
// value of another type that embeds or exposes a FatalError — through an As
// method, for example — is not one. A nil or typed-nil error is never asked to
// unwrap itself, and an Unwrap that panics is treated as having no children.
//
// The walk is bounded in the errors it examines, in the errors it holds
// pending and in the child slots it inspects, nil ones included, so a cyclic
// or enormous graph makes it return rather than hang. A fatal error deeper or
// wider than those bounds is reported as absent; nothing the parser builds
// comes near them. It bounds how many Unwrap calls it makes, not what one does:
// an Unwrap that blocks, spins or allocates without end is the caller's code
// and is not interrupted.
//
// A genuine fatal error presented again somewhere it did not come from — by a
// reader that returns one it obtained from another parse, for example — is
// still the parser's value and is reported as fatal. Nothing in the type system
// tells a value from itself.
//
// Before ROB-120-007 this was errors.As against FatalError, which called As
// methods, followed Unwrap without a bound, and accepted any value satisfying
// the interface, an embedded nil one included.
func IsFatal(err error) bool {
	return errgraph.Find(err, isParserFatal)
}

// isParserFatal reports whether err is a non-nil fatal error this package
// created. It is a type assertion, so it calls nothing on err.
func isParserFatal(err error) bool {
	f, ok := err.(*fatalParseError)
	return ok && f != nil
}
