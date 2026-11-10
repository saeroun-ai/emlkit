package textproto

import (
	"errors"
	"fmt"

	"github.com/saeroun-ai/emlkit/internal/errgraph"
)

// HeaderLimits are the budgets one header block is read under.
//
// Two kinds of field are here. The four Max fields are per-block budgets: each
// is in the unit its name gives — bytes, or a count — and obeys the same rule,
// a positive value is the budget and zero or below is no budget at all. They
// are never cumulative: each header block — a message's own, or one part's —
// is read under the whole of every one of them, so what one block costs is
// never charged to the next.
//
// Total is the other kind: not a number but a pointer to a HeaderBudget, which
// every block read with the same pointer spends together, on top of the
// per-block budgets. nil is no shared budget.
//
// The zero HeaderLimits budgets nothing, which is what ReadHeader reads under,
// so a caller opts into each budget by naming it.
//
// This is the wire-format layer's own spelling of the budgets. The message
// layer resolves a caller's configuration into these fields; see
// message.Limits, whose LimitDisabled (-1) arrives here as an ordinary
// "zero or below", meaning no budget.
type HeaderLimits struct {
	// MaxBytes bounds the whole block as it appears in the input, from the
	// first byte of its first field through the empty line that ends it, that
	// empty line included. A UTF-8 byte order mark the block begins with is
	// bytes of the block and is counted here, and by the shared budget's
	// bytes; it is no line's and no field's, so no other budget counts it. A
	// block that begins with a UTF-16 or UTF-32 mark is not read and costs
	// nothing. See ReadHeader.
	MaxBytes int64

	// MaxFieldBytes bounds one logical header field: its first physical line,
	// every continuation line that belongs to it, and the line terminators of
	// each, exactly as they appear in the input.
	//
	// The empty line that ends the block is not part of any field and is not
	// charged here. MaxBytes counts it, because it is part of the block; this
	// budget counts fields, and a block that holds no field spends none of it.
	// So a block that is nothing but that empty line is read under any budget at
	// all, down to one byte.
	MaxFieldBytes int64

	// MaxCount bounds how many logical fields one block may contain. Every
	// field candidate is charged, including one that is later skipped for
	// having an empty name, or kept as a malformed prefix or an orphan record
	// rather than a field, so that a block of nothing but empty names costs
	// what a block of that many fields costs. The empty line that ends the block
	// is not a candidate and is not charged, so an empty block spends nothing.
	MaxCount int64

	// MaxLineBytes bounds one physical line of the block, not counting the
	// line terminator that ends it. A line that the input ends without
	// terminating is charged the bytes it has.
	MaxLineBytes int64

	// Total, when non-nil, is a budget several blocks spend together, on top
	// of the per-block budgets above. It is the one field here that is
	// cumulative; see HeaderBudget. nil is no shared budget.
	Total *HeaderBudget

	// Recover asks the reader to heal a damaged line of the block that no
	// continuation line follows, rather than fail the block with it: to join
	// it to the field before it, keep it apart, or end the block before it as
	// the first line of the body. It is no budget and is off by default. See
	// recover.go, and ReadHeaderRecover for the bytes a block ended that way
	// hands back.
	Recover bool
}

// A HeaderLimitKind names which budget of a header block was exceeded.
//
// The zero value is HeaderLimitBytes, which is the budget that existed before
// the others and the one a HeaderLimitError reports when nothing else is said.
type HeaderLimitKind int

const (
	// HeaderLimitBytes is HeaderLimits.MaxBytes: the whole block.
	HeaderLimitBytes HeaderLimitKind = iota

	// HeaderLimitFieldBytes is HeaderLimits.MaxFieldBytes: one logical field.
	HeaderLimitFieldBytes

	// HeaderLimitCount is HeaderLimits.MaxCount: the number of fields.
	HeaderLimitCount

	// HeaderLimitLineBytes is HeaderLimits.MaxLineBytes: one physical line of
	// the block.
	HeaderLimitLineBytes

	// HeaderLimitTotalBytes is the byte budget of HeaderLimits.Total: the wire
	// bytes of every block that shares it.
	HeaderLimitTotalBytes

	// HeaderLimitTotalCount is the field budget of HeaderLimits.Total: the
	// field candidates of every block that shares it.
	HeaderLimitTotalCount
)

// String returns a short name for the kind, for humans and tests.
func (k HeaderLimitKind) String() string {
	switch k {
	case HeaderLimitBytes:
		return "header bytes"
	case HeaderLimitFieldBytes:
		return "header field bytes"
	case HeaderLimitCount:
		return "header field count"
	case HeaderLimitLineBytes:
		return "header line bytes"
	case HeaderLimitTotalBytes:
		return "total header bytes"
	case HeaderLimitTotalCount:
		return "total header field count"
	}
	return fmt.Sprintf("HeaderLimitKind(%d)", int(k))
}

// unit names what the budget of this kind counts, for the error text.
func (k HeaderLimitKind) unit() string {
	switch k {
	case HeaderLimitCount, HeaderLimitTotalCount:
		return "fields"
	}
	return "bytes"
}

// sentinel returns the public error this kind's refusal unwraps to, so that
// errors.Is finds a budget failure through a HeaderLimitError of any kind.
func (k HeaderLimitKind) sentinel() error {
	switch k {
	case HeaderLimitFieldBytes:
		return ErrHeaderFieldTooBig
	case HeaderLimitCount, HeaderLimitTotalCount:
		return ErrTooManyHeaderFields
	case HeaderLimitLineBytes:
		return ErrLineTooBig
	}
	return ErrHeaderTooBig
}

// The sentinels a refused budget unwraps to. Match them with errors.Is; see
// ErrHeaderTooBig, whose terms these share.
//
// Like ErrHeaderTooBig, each is the sentinel to test for and not the sentinel
// to produce. A refusal is recognized by the error this package builds for it —
// HeaderLimit for a header block, LineLimit for a framing line — so an
// io.Reader that returns one of these values, or wraps it, is an ordinary read
// failure and is reported as one.
var (
	// ErrHeaderFieldTooBig reports that one logical header field did not fit
	// in the byte budget it was read under.
	ErrHeaderFieldTooBig = errors.New("message: header field exceeds maximum size")

	// ErrTooManyHeaderFields reports that a header block held more fields than
	// the budget it was read under allowed.
	ErrTooManyHeaderFields = errors.New("message: header has too many fields")

	// ErrLineTooBig reports that one physical line did not fit in the byte
	// budget it was read under. It covers a line of a header block and a
	// multipart framing line alike, which are the lines this package has to
	// materialize as lines.
	ErrLineTooBig = errors.New("message: line exceeds maximum size")
)

// A LineLimitError reports that a physical line outside a header block — a
// multipart preamble line, or a delimiter line — exceeded the byte budget it
// was read under, and says which budget and where in the reader's input that
// line began.
//
// # Provenance
//
// It carries the same guarantee a HeaderLimitError carries, for the same
// reason and by the same means: the fields that make it a refusal are
// unexported, so a value another package writes is the zero value and refuses
// nothing. LineLimit is the predicate that tells the two apart, and it is what
// a caller classifying a refused line should use. As there, the one case that
// cannot be told apart is a genuine error obtained from this package and
// presented again somewhere it did not come from.
//
// # Reading one
//
// Limit and Offset report what was exceeded and where. It wraps ErrLineTooBig,
// so errors.Is finds the budget failure through it.
type LineLimitError struct {
	// fromParser marks a value this package created, and is set in exactly one
	// place.
	fromParser bool

	// limit is the budget that was exceeded, in bytes, not counting the line
	// terminator.
	limit int64

	// offset is where the refused line began.
	offset int64
}

// newLineLimitError returns the refusal of a line that began at offset and did
// not fit in limit bytes. It is the only place a genuine LineLimitError is
// made.
func newLineLimitError(limit, offset int64) *LineLimitError {
	return &LineLimitError{fromParser: true, limit: limit, offset: offset}
}

// Limit returns the budget that was exceeded, in bytes, not counting the line
// terminator. It returns 0 for a nil error.
func (e *LineLimitError) Limit() int64 {
	if e == nil {
		return 0
	}
	return e.limit
}

// Offset returns the offset of the first byte of the refused line, relative to
// the first byte of the exact input the reader was given, on the same terms as
// Part.HeaderOffset. It is a real position: the reader had already reached the
// line before it read a byte of it. It returns 0 for a nil error.
func (e *LineLimitError) Offset() int64 {
	if e == nil {
		return 0
	}
	return e.offset
}

// Error implements error. It is safe on a nil receiver, because a typed nil
// inside a non-nil error interface is something an adversarial input can
// present, and formatting what arrived must not be what brings a parser down.
func (e *LineLimitError) Error() string {
	if e == nil {
		return "multipart: nil line limit error"
	}
	return fmt.Sprintf("multipart: line at offset %d exceeds the limit of %d bytes", e.offset, e.limit)
}

// Unwrap reports the budget failure this error describes, so that
// errors.Is(err, ErrLineTooBig) holds for it.
func (e *LineLimitError) Unwrap() error {
	return ErrLineTooBig
}

// LineLimit returns the framing line budget refusal in err's chain, and whether
// there is a genuine one.
//
// It is to LineLimitError what HeaderLimit is to HeaderLimitError, with the
// same guarantees and the same bounds: exact dynamic type and the parser's own
// mark, no Is or As method ever called, both Unwrap forms followed, nil and
// typed-nil nodes never asked to unwrap themselves, an Unwrap that panics
// treated as having no children, and a walk bounded in nodes examined, children
// held pending and child slots inspected so that a cyclic or enormous error
// graph terminates rather than hangs. See HeaderLimit for why classification
// cannot be left to the error under test.
func LineLimit(err error) (*LineLimitError, bool) {
	var found *LineLimitError
	walkError(err, func(node error) bool {
		if e, ok := node.(*LineLimitError); ok && e != nil && e.fromParser {
			found = e
			return true
		}
		return false
	})
	return found, found != nil
}

// walkError walks the error graph rooted at err and reports whether match
// accepted a node. It is errgraph.Find, the one traversal every classification
// in this module uses, with its bounds: nodes examined, children held pending
// and child slots inspected, so that a cycle terminates and a join of a million
// errors, or of a million nils, costs what a small one does. No Is, As or Error
// method is called; a nil-valued node is never asked to unwrap itself; and an
// Unwrap that panics is treated as having no children. What lies past the
// bounds is reported as absent. Nothing this package builds comes close: a
// refusal is returned as itself.
func walkError(err error, match func(error) bool) bool {
	return errgraph.Find(err, match)
}
