package message

import (
	"errors"
	"math"
)

// errNegativeBudget reports that a budget was asked about a negative amount.
//
// Bytes read and items parsed are never negative, so a negative usage or delta
// is a bug in the parser rather than a property of a message. It is reported
// rather than panicked on: the caller of these helpers is parser code, and a
// scope that gets this back can stop itself the same way it stops for anything
// else, without taking a process down over a message.
var errNegativeBudget = errors.New("message: negative budget amount")

// withinLimit reports whether using delta more units, on top of used already
// used, stays within limit.
//
// limit is a resolved budget: a positive count, or LimitDisabled. A disabled
// budget admits everything. math.MaxInt64 is an ordinary finite budget and is
// enforced like any other.
//
// The sum is never computed. used and delta are each at most math.MaxInt64, so
// used+delta can wrap to a negative number and make an exceeded budget look
// satisfied — which is exactly the check an attacker would aim at. Comparing
// delta against the remaining headroom instead is exact for every input in
// range, because limit-used cannot overflow when both are non-negative and
// used is not past limit.
//
// It reports errNegativeBudget for a negative used or delta, and false with it,
// so that a caller which ignores the error still fails closed.
func withinLimit(limit, used, delta int64) (bool, error) {
	if used < 0 || delta < 0 {
		return false, errNegativeBudget
	}
	if limit == LimitDisabled {
		return true, nil
	}
	if used > limit {
		// Not reachable through budget, which never lets usage past the limit,
		// but a caller tracking its own usage can ask about any state.
		return false, nil
	}
	return delta <= limit-used, nil
}

// A budget is one resolved limit together with how much of it has been used.
//
// It is the form the parser works in: a scope takes a budget from the session's
// resolved configuration, charges what it consumes as it consumes it, and stops
// the moment a charge does not fit. Charging as the bytes arrive rather than
// checking a total afterwards is what keeps a budget from being reached only
// after the allocation it was meant to prevent.
//
// No scope in this package charges one yet. The one budget enforced today,
// MaxHeaderBytes, is charged where a header block is read, which is the
// textproto layer, by the same arithmetic written there: a budget is only useful
// at the point the bytes arrive, and that point is below this package. What
// remains for this type is the budgets whose bytes this package counts itself.
//
// The zero budget has a zero limit, which no positive charge fits. A budget is
// always constructed from a resolved limit with newBudget, never left zero.
//
// A budget is a value and carries no lock. One belongs to the scope that owns
// the resource it bounds; scopes that share a resource share one through a
// pointer and serialize their own access.
type budget struct {
	// limit is a resolved budget: a positive count, or LimitDisabled.
	limit int64

	// used never exceeds limit. add is the only thing that changes it, and it
	// saturates rather than overshooting, so the remaining headroom is always
	// a non-negative number that can be computed without overflow.
	//
	// A disabled budget does not accumulate usage: there is nothing to compare
	// a total against, and a running total that nothing bounds could itself
	// overflow, which is the failure this whole type exists to avoid.
	used int64
}

// newBudget returns a budget for a resolved limit, with nothing used yet.
func newBudget(limit int64) budget {
	return budget{limit: limit}
}

// disabled reports whether this budget admits everything.
func (b budget) disabled() bool {
	return b.limit == LimitDisabled
}

// remaining reports how many further units fit. It returns math.MaxInt64 for a
// disabled budget, which is the largest amount any caller could ask about, and
// never a negative number.
func (b budget) remaining() int64 {
	if b.disabled() {
		return math.MaxInt64
	}
	if b.used > b.limit {
		return 0
	}
	return b.limit - b.used
}

// add charges delta units to the budget and reports whether they fit.
//
// When they fit, the usage grows by delta. When they do not, the budget is left
// saturated at its limit — the scope has run out, and how far past it the input
// would have gone is not a number this type promises — and add reports false so
// that the caller can stop, record the matching limit.* diagnostic, and return
// what it recovered.
//
// A negative delta is a parser bug: add changes nothing and reports false with
// errNegativeBudget, so a caller that ignores the error still fails closed.
func (b *budget) add(delta int64) (bool, error) {
	ok, err := withinLimit(b.limit, b.used, delta)
	if err != nil {
		return false, err
	}
	if !ok {
		if !b.disabled() {
			b.used = b.limit
		}
		return false, nil
	}
	if !b.disabled() {
		b.used += delta
	}
	return true, nil
}
