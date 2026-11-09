package textproto

import "sync"

// A HeaderBudget is a byte and field budget that several header blocks spend
// together, set through HeaderLimits.Total. Every per-block budget of
// HeaderLimits still applies to each block on its own; this one bounds what
// all the blocks that share it cost in sum.
//
// # What is charged
//
// Bytes are the wire bytes MaxBytes counts — every line of a block, folds,
// line terminators and the empty line that ends it — and fields are the
// candidates MaxCount counts, a candidate with an empty name included and the
// empty line that ends a block excluded. Only bytes a header reader consumes
// into a block are charged: nothing it merely peeks at or buffers ahead, and
// nothing outside a block, such as multipart framing or a part's body. A part
// with no header block at all charges nothing.
//
// Each read is checked against the room that is left before any of it is kept,
// after the per-block line, field and block budgets have admitted it, so those
// smaller budgets win a read that would exceed both. A read that does not fit
// is refused whole and charges nothing; what earlier reads admitted stays
// charged. A read is what the header reader takes from its bufio.Reader at once:
// one line, or one buffer of a longer line, so how much a refused block leaves
// charged depends on that buffer's size and never on how the underlying input
// chunks its reads. Nothing is ever refunded, including for a block that a
// per-block budget later refuses.
//
// Fields are counted when a field has been read whole. In addition, once every
// field the budget allows has been charged, a block is refused at the first
// byte of the next candidate, before any of that candidate is read, so that an
// exhausted count bounds a candidate however long it is. A line that is the
// empty line ending the block, and an input with no byte left, are not
// candidates. A block whose very first line begins with white space is
// malformed and ends with that error; that line is not a field candidate, and
// only the byte budgets bound its length.
//
// # Refusal
//
// A refusal is a *HeaderLimitError of kind HeaderLimitTotalBytes or
// HeaderLimitTotalCount. Its UnitOffset is, for bytes, the first byte past
// the budget — so usage stays below the budget by what the refused read held
// below that byte — and, for fields, the first byte of the refused candidate.
//
// Every charge, and Refusal, holds the budget's lock, so they happen in one
// order. The first charge in that order that does not fit decides and records
// the refusal in the same critical section, with that charge's own kind,
// budget and offsets, in the input of the reader that made it. Refusal reports
// nil if it takes the lock before that charge and that value if after, even
// before the charge's caller has received it; it is never replaced. Every
// later charge, of either kind and from any reader, is refused with that
// identical value, so a block another reader is still reading is refused at
// its next charge and keeps what it had already been charged.
//
// A HeaderBudget is safe for concurrent use. Its zero value budgets nothing;
// use NewHeaderBudget.
type HeaderBudget struct {
	mu sync.Mutex

	// maxBytes and maxCount are the budgets; zero or below is none.
	maxBytes, maxCount int64

	// usedBytes and usedCount are what has been admitted. Each never passes its
	// budget, and stays zero while that budget is off.
	usedBytes, usedCount int64

	// refusal is the first refusal, built by the charge that did not fit and
	// never replaced. A non-nil refusal refuses every later charge.
	refusal *HeaderLimitError
}

// NewHeaderBudget returns a budget of maxBytes wire bytes and maxCount field
// candidates shared by every block read with it. A value of zero or below
// turns that one budget off; math.MaxInt64 is an ordinary finite budget.
func NewHeaderBudget(maxBytes, maxCount int64) *HeaderBudget {
	return &HeaderBudget{maxBytes: maxBytes, maxCount: maxCount}
}

// Refusal returns the first refusal this budget recorded, or nil if, in the
// order its lock imposes, no charge has failed yet. Once recorded the value
// never changes, and every refusal the budget makes is that same value, so
// comparing an error with it tells whether this budget refused — without
// asking the error anything. It is safe on a nil budget.
func (b *HeaderBudget) Refusal() *HeaderLimitError {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.refusal
}

// The charges below are made by a header scanner for a block that begins at
// base in its reader's input. Each returns nil when the charge is admitted, and
// otherwise the budget's one refusal: the existing one, or a new one built from
// the arguments, at unit, a position relative to base.

// refuseLocked returns the budget's refusal, recording a new one of kind at
// base+unit first if there is none. b.mu must be held.
func (b *HeaderBudget) refuseLocked(kind HeaderLimitKind, limit, base, unit int64) *HeaderLimitError {
	if b.refusal == nil {
		b.refusal = newHeaderLimitError(kind, limit, base, base+unit)
	}
	return b.refusal
}

// chargeBytes admits n wire bytes read at consumed bytes into the block, or
// refuses them all. A new refusal is placed at the first byte past the budget.
func (b *HeaderBudget) chargeBytes(n, base, consumed int64) *HeaderLimitError {
	if n <= 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.refusal != nil {
		return b.refusal
	}
	if b.maxBytes <= 0 {
		return nil
	}
	// Compared with the room left rather than summed, so math.MaxInt64 is
	// exact. usedBytes never passes maxBytes, so this cannot overflow.
	if left := b.maxBytes - b.usedBytes; n > left {
		return b.refuseLocked(HeaderLimitTotalBytes, b.maxBytes, base, consumed+left)
	}
	b.usedBytes += n
	return nil
}

// chargeField admits one field candidate that began at fieldStart, or refuses
// it.
func (b *HeaderBudget) chargeField(base, fieldStart int64) *HeaderLimitError {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.refusal != nil {
		return b.refusal
	}
	if b.maxCount <= 0 {
		return nil
	}
	if b.usedCount >= b.maxCount {
		return b.refuseLocked(HeaderLimitTotalCount, b.maxCount, base, fieldStart)
	}
	b.usedCount++
	return nil
}

// preflightField refuses a candidate known to exist, beginning at start, before
// any of it is read, when the budget has already refused or its field budget
// has no room left. It charges nothing: a candidate that passes is charged by
// chargeField once it has been read whole. A field budget that is off never
// refuses here, and a budget that refused for bytes returns that refusal.
func (b *HeaderBudget) preflightField(base, start int64) *HeaderLimitError {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.refusal != nil {
		return b.refusal
	}
	if b.maxCount > 0 && b.usedCount >= b.maxCount {
		return b.refuseLocked(HeaderLimitTotalCount, b.maxCount, base, start)
	}
	return nil
}
