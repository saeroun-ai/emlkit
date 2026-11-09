package message

import (
	"errors"
	"math"
	"testing"
)

// These tests are about arithmetic, not about mail. Their point is that a
// budget answers the same way at every boundary, including the one where the
// obvious implementation — comparing used+delta against the limit — would wrap
// to a negative number and let an exceeded budget through.

// TestWithinLimit pins the answer at limit-1, limit and limit+1 for an ordinary
// budget, for a disabled one, and for math.MaxInt64, where a computed sum
// overflows.
func TestWithinLimit(t *testing.T) {
	const max = math.MaxInt64

	for _, tc := range []struct {
		name  string
		limit int64
		used  int64
		delta int64
		want  bool
	}{
		{"nothing used, under", 10, 0, 9, true},
		{"nothing used, exactly", 10, 0, 10, true},
		{"nothing used, over", 10, 0, 11, false},
		{"partly used, under", 10, 4, 5, true},
		{"partly used, exactly", 10, 4, 6, true},
		{"partly used, over", 10, 4, 7, false},
		{"full budget, nothing more", 10, 10, 0, true},
		{"full budget, one more", 10, 10, 1, false},
		{"zero delta is always within", 10, 0, 0, true},
		{"zero limit admits nothing positive", 0, 0, 1, false},
		{"zero limit admits zero", 0, 0, 0, true},

		// A disabled budget admits everything, including amounts whose sum
		// would overflow.
		{"disabled, small", LimitDisabled, 0, 1, true},
		{"disabled, max", LimitDisabled, max, max, true},

		// math.MaxInt64 is a finite budget. used+delta overflows in every one
		// of these, so an implementation that computed it would report the
		// opposite of two of the three.
		{"max limit, boundary minus one", max, max - 2, 1, true},
		{"max limit, boundary", max, max - 1, 1, true},
		{"max limit, boundary plus one", max, max, 1, false},
		{"max limit, max delta from zero", max, 0, max, true},
		{"max limit, max delta from one", max, 1, max, false},
		{"max limit, max used and max delta", max, max, max, false},

		// A usage past the limit cannot arise through budget, but the free
		// function must still answer, and must answer closed.
		{"used past the limit", 10, 11, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := withinLimit(tc.limit, tc.used, tc.delta)
			if err != nil {
				t.Fatalf("withinLimit(%d, %d, %d) = %v", tc.limit, tc.used, tc.delta, err)
			}
			if got != tc.want {
				t.Errorf("withinLimit(%d, %d, %d) = %v, want %v", tc.limit, tc.used, tc.delta, got, tc.want)
			}
		})
	}
}

// TestWithinLimit_negative pins that a negative usage or delta is rejected
// rather than reasoned about, and that the rejection fails closed: the reported
// answer is "does not fit", so a caller that drops the error still stops.
func TestWithinLimit_negative(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int64
		used  int64
		delta int64
	}{
		{"negative delta", 10, 0, -1},
		{"negative usage", 10, -1, 0},
		{"both negative", 10, -1, -1},
		{"min int64 delta", 10, 0, math.MinInt64},
		{"negative delta on a disabled budget", LimitDisabled, 0, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := withinLimit(tc.limit, tc.used, tc.delta)
			if !errors.Is(err, errNegativeBudget) {
				t.Fatalf("withinLimit(%d, %d, %d) error = %v, want errNegativeBudget",
					tc.limit, tc.used, tc.delta, err)
			}
			if got {
				t.Errorf("withinLimit reported a fit beside its error, want it to fail closed")
			}
		})
	}
}

// TestBudget_add pins the accumulating half: charges add up, the charge that
// does not fit is refused, and the budget saturates rather than overshooting so
// that its remaining headroom stays a number that can be computed safely.
func TestBudget_add(t *testing.T) {
	b := newBudget(10)
	if got := b.remaining(); got != 10 {
		t.Fatalf("remaining() = %d, want 10", got)
	}

	for _, delta := range []int64{4, 5} {
		ok, err := b.add(delta)
		if err != nil || !ok {
			t.Fatalf("add(%d) = (%v, %v), want (true, nil)", delta, ok, err)
		}
	}
	if got := b.remaining(); got != 1 {
		t.Errorf("remaining() = %d, want 1", got)
	}

	// The charge that does not fit is refused and changes the budget only by
	// saturating it.
	ok, err := b.add(2)
	if err != nil {
		t.Fatalf("add(2) = %v", err)
	}
	if ok {
		t.Errorf("add(2) = true with 1 remaining, want false")
	}
	if got := b.remaining(); got != 0 {
		t.Errorf("remaining() = %d after an over-budget charge, want 0", got)
	}
	if got := b.used; got != 10 {
		t.Errorf("used = %d, want the budget saturated at 10", got)
	}

	// Nothing more fits, and asking again does not corrupt the state.
	if ok, err := b.add(1); ok || err != nil {
		t.Errorf("add(1) on a spent budget = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := b.add(0); !ok || err != nil {
		t.Errorf("add(0) on a spent budget = (%v, %v), want (true, nil)", ok, err)
	}
}

// TestBudget_addAtMaxInt64 pins the boundary an overflowing implementation gets
// wrong: a budget of math.MaxInt64 is finite, is reached exactly, and refuses
// the byte after it without wrapping.
func TestBudget_addAtMaxInt64(t *testing.T) {
	b := newBudget(math.MaxInt64)

	if ok, err := b.add(math.MaxInt64 - 1); !ok || err != nil {
		t.Fatalf("add(MaxInt64-1) = (%v, %v), want (true, nil)", ok, err)
	}
	if got := b.remaining(); got != 1 {
		t.Errorf("remaining() = %d, want 1", got)
	}
	if ok, err := b.add(1); !ok || err != nil {
		t.Fatalf("add(1) at the boundary = (%v, %v), want (true, nil)", ok, err)
	}
	if got := b.remaining(); got != 0 {
		t.Errorf("remaining() = %d at the boundary, want 0", got)
	}
	if got := b.used; got != math.MaxInt64 {
		t.Errorf("used = %d, want MaxInt64", got)
	}
	if ok, err := b.add(1); ok || err != nil {
		t.Errorf("add(1) past the boundary = (%v, %v), want (false, nil)", ok, err)
	}
	if got := b.used; got != math.MaxInt64 {
		t.Errorf("used = %d after a refused charge, want it unchanged", got)
	}
}

// TestBudget_disabled pins that a disabled budget admits every charge without
// accumulating a total that could itself overflow.
func TestBudget_disabled(t *testing.T) {
	b := newBudget(LimitDisabled)
	if !b.disabled() {
		t.Fatalf("disabled() = false for LimitDisabled")
	}
	if got := b.remaining(); got != math.MaxInt64 {
		t.Errorf("remaining() = %d, want MaxInt64", got)
	}

	for i := 0; i < 3; i++ {
		if ok, err := b.add(math.MaxInt64); !ok || err != nil {
			t.Fatalf("add(MaxInt64) on a disabled budget = (%v, %v), want (true, nil)", ok, err)
		}
	}
	if got := b.used; got != 0 {
		t.Errorf("used = %d, want a disabled budget not to accumulate", got)
	}
	if got := b.remaining(); got != math.MaxInt64 {
		t.Errorf("remaining() = %d, want MaxInt64", got)
	}
}

// TestBudget_addNegative pins that a negative charge is a rejected parser bug:
// it fails closed and leaves the budget alone.
func TestBudget_addNegative(t *testing.T) {
	for _, limit := range []int64{10, LimitDisabled, math.MaxInt64} {
		b := newBudget(limit)
		if ok, _ := b.add(3); !ok && limit != 0 {
			// A charge of 3 fits every limit under test; this only guards the
			// state the negative charge must not disturb.
			t.Fatalf("add(3) on limit %d = false", limit)
		}
		before := b

		ok, err := b.add(-1)
		if !errors.Is(err, errNegativeBudget) {
			t.Errorf("add(-1) on limit %d error = %v, want errNegativeBudget", limit, err)
		}
		if ok {
			t.Errorf("add(-1) on limit %d = true, want it to fail closed", limit)
		}
		if b != before {
			t.Errorf("add(-1) on limit %d changed the budget: %+v, want %+v", limit, b, before)
		}
	}
}

// TestBudget_zeroValue pins what the zero budget is: a limit of zero, which no
// positive charge fits. Budgets are built from resolved limits, so this only
// records that the zero value fails closed rather than admitting everything.
func TestBudget_zeroValue(t *testing.T) {
	var b budget
	if b.disabled() {
		t.Errorf("the zero budget reports itself disabled")
	}
	if got := b.remaining(); got != 0 {
		t.Errorf("remaining() = %d, want 0", got)
	}
	if ok, err := b.add(1); ok || err != nil {
		t.Errorf("add(1) = (%v, %v), want (false, nil)", ok, err)
	}
}
