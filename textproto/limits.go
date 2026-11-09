package textproto

import (
	"errors"
	"fmt"
	"reflect"
)

// HeaderLimits are the budgets one header block is read under.
//
// Every field is in the unit its name gives — bytes, or a count — and every one
// obeys the same rule: a positive value is the budget, and zero or below is no
// budget at all, which is what ReadHeader reads under. The zero HeaderLimits
// therefore budgets nothing, so a caller opts into each budget by naming it.
//
// The budgets are per block and are never cumulative across blocks. Each header
// block — a message's own, or one part's — is read under the whole of every
// budget here, so what one block costs is never charged to the next.
//
// This is the wire-format layer's own spelling of the budgets. The message
// layer resolves a caller's configuration into these fields; see
// message.Limits, whose LimitDisabled (-1) arrives here as an ordinary
// "zero or below", meaning no budget.
type HeaderLimits struct {
	// MaxBytes bounds the whole block as it appears in the input, from the
	// first byte of its first field through the empty line that ends it, that
	// empty line included.
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
	// having an empty name, so that a block of nothing but empty names costs
	// what a block of that many fields costs. The empty line that ends the block
	// is not a candidate and is not charged, so an empty block spends nothing.
	MaxCount int64

	// MaxLineBytes bounds one physical line of the block, not counting the
	// line terminator that ends it. A line that the input ends without
	// terminating is charged the bytes it has.
	MaxLineBytes int64
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
	}
	return fmt.Sprintf("HeaderLimitKind(%d)", int(k))
}

// unit names what the budget of this kind counts, for the error text.
func (k HeaderLimitKind) unit() string {
	switch k {
	case HeaderLimitCount:
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
	case HeaderLimitCount:
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
// typed-nil nodes never asked to unwrap themselves, and a walk bounded in nodes
// examined and children held pending so that a cyclic or enormous error graph
// terminates rather than hangs. See HeaderLimit for why classification cannot
// be left to the error under test.
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

// walkError walks the error tree rooted at err and reports whether match
// accepted a node.
//
// It is the one traversal every input-facing classification in this package
// uses, so that each of them has the same properties rather than each having
// to restate them:
//
//   - What is walked is the whole tree, not one chain of it: both
//     Unwrap() error and Unwrap() []error are followed, so a node joined with
//     others by errors.Join is found wherever in the join it sits.
//   - No Is or As method is ever called. An error is free to answer yes to any
//     question about itself, so provenance cannot be established by asking the
//     suspect; match decides by exact dynamic type and by a mark only this
//     package can set.
//   - The walk is bounded in both directions an error graph can grow, because
//     an input decides the shape of that graph: a cap on the nodes examined,
//     which is what makes a cycle terminate rather than hang, and a fixed
//     number of children held pending, which is what keeps a join of a million
//     errors from costing a million slots. What does not fit is not examined,
//     so a node buried deeper or wider than those caps is reported as absent
//     rather than searched for without limit. Nothing this package builds comes
//     close: a refusal is returned as itself, or one wrap from the top.
//   - A node that is nil, or that holds nil underneath its type, is never asked
//     to unwrap itself — what an input hands over may have methods that
//     dereference nothing.
func walkError(err error, match func(error) bool) bool {
	const (
		// maxNodes bounds the whole search, cycles included.
		maxNodes = 256

		// maxPending bounds what is held while searching. The queue is an
		// array, so this is the entire cost of the walk whatever the graph
		// does.
		maxPending = 64
	)

	var (
		queue [maxPending]error
		head  int // where the next pending node is read from
		count int // pending nodes
	)

	for nodes := 0; nodes < maxNodes; nodes++ {
		if err == nil {
			if count == 0 {
				break
			}
			err = queue[head]
			queue[head] = nil
			head = (head + 1) % len(queue)
			count--
		}

		if match(err) {
			return true
		}

		// A value that is nil underneath is a leaf. Its Unwrap may be perfectly
		// willing to dereference the nothing it was called on.
		if !isNilValue(err) {
			switch u := err.(type) {
			case interface{ Unwrap() error }:
				// Single-child first, as the errors package resolves the same
				// ambiguity, so a type that implements both is followed the way
				// the standard library follows it.
				if child := u.Unwrap(); child != nil && count < len(queue) {
					queue[(head+count)%len(queue)] = child
					count++
				}
			case interface{ Unwrap() []error }:
				for _, child := range u.Unwrap() {
					if count == len(queue) {
						// Full. Stop reading this list rather than walking a
						// hostile one to its end.
						break
					}
					if child == nil {
						continue
					}
					queue[(head+count)%len(queue)] = child
					count++
				}
			}
		}

		err = nil
	}

	return false
}

// isNilValue reports whether err holds nil underneath its type: a nil pointer,
// interface, map, slice, func or channel inside a non-nil error interface.
//
// Such a value is not the nil error, so it is delivered and examined like any
// other, but calling a method on it runs code written for a receiver that is not
// there. Whether that panics is up to the value's own author, and the value's
// own author may be whoever wrote the input.
func isNilValue(err error) bool {
	v := reflect.ValueOf(err)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice,
		reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return v.IsNil()
	}
	return false
}
