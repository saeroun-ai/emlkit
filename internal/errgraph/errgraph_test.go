package errgraph

import (
	"errors"
	"fmt"
	"io"
	"testing"
)

// fuse is how many Unwrap calls a hostile cycle answers before it gives up and
// reports no child. Find makes at most MaxNodes of them, so a fuse that blows
// means the walk was not bounded — and the fuse is also what makes such a
// regression fail here instead of spinning: every graph in this file is finite
// work whatever Find does. None of them allocates per call.
const fuse = 10000

// target is the node the tests look for. match recognizes it by exact type,
// as every classifier in the module does.
type target struct{}

func (*target) Error() string { return "target" }

func isTarget(err error) bool {
	t, ok := err.(*target)
	return ok && t != nil
}

// leaf is an ordinary error with no children.
type leaf struct{ name string }

func (l leaf) Error() string { return l.name }

// wrap has one child.
type wrap struct{ inner error }

func (w wrap) Error() string { return "wrap" }
func (w wrap) Unwrap() error { return w.inner }

// multi has many.
type multi struct{ kids []error }

func (m multi) Error() string   { return "multi" }
func (m multi) Unwrap() []error { return m.kids }

// fusedCycle unwraps to itself until its fuse blows.
type fusedCycle struct {
	calls int
	blown bool
}

func (*fusedCycle) Error() string { return "fused cycle" }
func (f *fusedCycle) Unwrap() error {
	f.calls++
	if f.calls > fuse {
		f.blown = true
		return nil
	}
	return f
}

// fusedJoin is a cycle through Unwrap() []error, with a nil child beside the
// edge back to itself. kids is built once, so a call allocates nothing.
type fusedJoin struct {
	kids  []error
	calls int
	blown bool
}

func newFusedJoin() *fusedJoin {
	f := &fusedJoin{}
	f.kids = []error{nil, f}
	return f
}

func (*fusedJoin) Error() string { return "fused join" }
func (f *fusedJoin) Unwrap() []error {
	f.calls++
	if f.calls > fuse {
		f.blown = true
		return nil
	}
	return f.kids
}

// link is one node of a ring tied after construction.
type link struct {
	next  error
	calls *int
}

func (*link) Error() string { return "link" }
func (l *link) Unwrap() error {
	*l.calls++
	if *l.calls > fuse {
		return nil
	}
	return l.next
}

// counted counts the calls of every method Find must never make.
type counted struct {
	is, as, errs *int
}

func (c counted) Error() string          { *c.errs++; return "counted" }
func (c counted) Is(error) bool          { *c.is++; return true }
func (c counted) As(any) bool            { *c.as++; return true }
func (c counted) Unwrap() error          { return nil }
func (c counted) Format(fmt.State, rune) {}

// panicky panics in Unwrap with v, which may be nil.
type panicky struct{ v any }

func (panicky) Error() string   { return "panicky" }
func (p panicky) Unwrap() error { panic(p.v) }

// panickyMany panics in Unwrap() []error with v.
type panickyMany struct{ v any }

func (panickyMany) Error() string     { return "panicky many" }
func (p panickyMany) Unwrap() []error { panic(p.v) }

// nilUnwraps counts Unwrap calls on a typed nil *nilHostile, which Find must
// never make.
var nilUnwraps int

type nilHostile struct{ inner error }

func (*nilHostile) Error() string { return "nil hostile" }
func (n *nilHostile) Unwrap() error {
	if n == nil {
		nilUnwraps++
		return nil
	}
	return n.inner
}

// nilHostileMany is the same through Unwrap() []error, as a map type.
type nilHostileMany map[int]error

func (nilHostileMany) Error() string { return "nil hostile many" }
func (n nilHostileMany) Unwrap() []error {
	if n == nil {
		nilUnwraps++
	}
	return nil
}

// Error types whose values cannot be compared: comparing two of the same type
// with == panics. Find must never do it.
type (
	sliceErr  []int
	mapErr    map[string]int
	funcErr   func()
	structErr struct{ s []int }
)

func (sliceErr) Error() string  { return "slice" }
func (mapErr) Error() string    { return "map" }
func (funcErr) Error() string   { return "func" }
func (structErr) Error() string { return "struct" }

func TestFind_nil(t *testing.T) {
	calls := 0
	if Find(nil, func(error) bool { calls++; return true }) {
		t.Error("Find(nil) = true")
	}
	if calls != 0 {
		t.Errorf("match called %d times on nil", calls)
	}
}

func TestFind_genuineThroughWrapping(t *testing.T) {
	want := &target{}
	for name, err := range map[string]error{
		"itself":      want,
		"fmt %w":      fmt.Errorf("outer: %w", want),
		"two %w":      fmt.Errorf("a: %w, b: %w", errors.New("other"), want),
		"join":        errors.Join(errors.New("other"), want),
		"custom":      wrap{want},
		"custom many": multi{[]error{nil, leaf{"x"}, want}},
		"deep mix":    fmt.Errorf("x: %w", errors.Join(io.EOF, wrap{multi{[]error{want}}})),
	} {
		if !Find(err, isTarget) {
			t.Errorf("%s: target not found", name)
		}
	}
	for name, err := range map[string]error{
		"leaf":              leaf{"x"},
		"typed nil target":  (*target)(nil),
		"wrapped typed nil": fmt.Errorf("x: %w", (*target)(nil)),
		"join of others":    errors.Join(io.EOF, leaf{"x"}),
	} {
		if Find(err, isTarget) {
			t.Errorf("%s: target reported", name)
		}
	}
}

func TestFind_cyclesTerminate(t *testing.T) {
	self := &fusedCycle{}
	if Find(self, isTarget) || self.blown || self.calls > MaxNodes {
		t.Errorf("self cycle: %d Unwrap calls, blown %v", self.calls, self.blown)
	}

	join := newFusedJoin()
	if Find(join, isTarget) || join.blown || join.calls > MaxNodes {
		t.Errorf("join cycle: %d Unwrap calls, blown %v", join.calls, join.blown)
	}

	calls := 0
	a, b, c := &link{calls: &calls}, &link{calls: &calls}, &link{calls: &calls}
	a.next, b.next, c.next = b, c, a
	if Find(a, isTarget) || calls > MaxNodes {
		t.Errorf("ring: %d Unwrap calls", calls)
	}

	// A cycle beside a genuine node does not hide it: the walk is breadth
	// first, so the target is met before the cycle has used the budget.
	beside := &fusedCycle{}
	if !Find(errors.Join(beside, &target{}), isTarget) || beside.blown {
		t.Error("a cycle hid a genuine sibling")
	}
}

// TestFind_diamond pins that a graph reaching the same node along many paths —
// 2^30 here — costs no more than the node budget.
func TestFind_diamond(t *testing.T) {
	var calls int
	var d error = &target{}
	for range 30 {
		d = &link{next: multi{[]error{d, d}}, calls: &calls}
	}
	Find(d, isTarget) // whether it is found depends on the bounds; it must return
	if calls > MaxNodes {
		t.Errorf("%d Unwrap calls, want at most %d", calls, MaxNodes)
	}
}

func TestFind_nodeBound(t *testing.T) {
	chain := func(depth int) error {
		var err error = &target{}
		for range depth {
			err = wrap{err}
		}
		return err
	}
	// The root is node one, so a target at depth MaxNodes-1 is the last node
	// examined.
	if !Find(chain(MaxNodes-1), isTarget) {
		t.Error("target at the last examined depth not found")
	}
	if Find(chain(MaxNodes), isTarget) {
		t.Error("target past the node bound reported")
	}
}

func TestFind_pendingBound(t *testing.T) {
	join := func(at int) error {
		kids := make([]error, at+1)
		for i := range at {
			kids[i] = leaf{"x"}
		}
		kids[at] = &target{}
		return multi{kids}
	}
	if !Find(join(MaxPending-1), isTarget) {
		t.Error("target in the last pending slot not found")
	}
	if Find(join(MaxPending), isTarget) {
		t.Error("target past the pending bound reported")
	}
}

// TestFind_slotBound pins that nil children are charged: a long run of nils is
// not scanned to its end, which is what the pending bound alone did not
// prevent, since a nil never takes a pending slot.
func TestFind_slotBound(t *testing.T) {
	afterNils := func(n int) error {
		kids := make([]error, n+1)
		kids[n] = &target{}
		return multi{kids}
	}
	if !Find(afterNils(MaxSlots-1), isTarget) {
		t.Error("target in the last inspected slot not found")
	}
	if Find(afterNils(MaxSlots), isTarget) {
		t.Error("target past the slot bound reported: nil slots were not charged")
	}
	// A large finite run of nils returns, and reports nothing past it.
	if Find(afterNils(1<<16), isTarget) {
		t.Error("target after 65536 nils reported")
	}
}

// TestFind_singleUnwrapCharged pins that an Unwrap() error call costs a slot
// as well, so the budget is shared by the two forms.
func TestFind_singleUnwrapCharged(t *testing.T) {
	const nils = 200
	build := func(depth int) error {
		var chain error = &target{}
		for range depth {
			chain = wrap{chain}
		}
		kids := make([]error, nils+1)
		kids[nils] = chain
		return multi{kids}
	}
	// The root charges nils+1 slots; each wrap one more.
	if !Find(build(MaxSlots-nils-1), isTarget) {
		t.Error("target within the shared slot budget not found")
	}
	if Find(build(MaxSlots-nils), isTarget) {
		t.Error("target past the shared slot budget reported: Unwrap() error was not charged")
	}
}

func TestFind_panickingUnwrap(t *testing.T) {
	for name, err := range map[string]error{
		"single":        panicky{"boom"},
		"single nil":    panicky{nil},
		"single error":  panicky{errors.New("boom")},
		"many":          panickyMany{"boom"},
		"many nil":      panickyMany{nil},
		"wrapped":       fmt.Errorf("x: %w", panicky{"boom"}),
		"inside a join": errors.Join(panickyMany{nil}, panicky{nil}),
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("Find panicked: %v", p)
				}
			}()
			if Find(err, isTarget) {
				t.Error("target reported")
			}
			// A sibling of a panicking node is still examined.
			if !Find(errors.Join(err, &target{}), isTarget) {
				t.Error("target beside a panicking Unwrap not found")
			}
		})
	}
}

func TestFind_typedNilsNotUnwrapped(t *testing.T) {
	nilUnwraps = 0
	var matched int
	for _, err := range []error{
		(*nilHostile)(nil),
		nilHostileMany(nil),
		fmt.Errorf("x: %w", (*nilHostile)(nil)),
		errors.Join((*nilHostile)(nil), nilHostileMany(nil)),
		multi{[]error{(*nilHostile)(nil), nilHostileMany(nil)}},
	} {
		Find(err, func(node error) bool { matched++; return false })
	}
	if nilUnwraps != 0 {
		t.Errorf("Unwrap called %d times on a typed nil", nilUnwraps)
	}
	if matched == 0 {
		t.Error("match was never called")
	}
	// A typed nil is matched like any node; it is only never unwrapped.
	seen := false
	Find((*nilHostile)(nil), func(node error) bool {
		_, seen = node.(*nilHostile)
		return false
	})
	if !seen {
		t.Error("a typed nil was not offered to match")
	}
}

func TestFind_neverAsksTheNode(t *testing.T) {
	var is, as, errs int
	c := counted{&is, &as, &errs}
	for _, err := range []error{
		c,
		fmt.Errorf("x: %w", c),
		errors.Join(c, c),
		multi{[]error{c, wrap{c}}},
	} {
		if Find(err, isTarget) {
			t.Error("a node that claims every identity was taken for the target")
		}
		Find(err, func(node error) bool { return node == io.EOF })
	}
	if is != 0 || as != 0 || errs != 0 {
		t.Errorf("Is called %d, As %d, Error %d times; want none", is, as, errs)
	}
}

// TestFind_nonComparableNodes pins that identity matches against a pointer
// target, which is what the module's classifiers use, cannot panic on a node
// whose type is not comparable, and that the walk itself compares nothing.
func TestFind_nonComparableNodes(t *testing.T) {
	odd := []error{
		sliceErr{1},
		mapErr{"a": 1},
		funcErr(func() {}),
		structErr{[]int{1}},
	}
	graph := multi{append(append([]error{}, odd...), wrap{multi{odd}}, io.EOF)}
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("panicked: %v", p)
		}
	}()
	if !Find(graph, func(node error) bool { return node == io.EOF || node == io.ErrUnexpectedEOF }) {
		t.Error("io.EOF beside non-comparable nodes not found")
	}
	if Find(multi{odd}, func(node error) bool { return node == io.EOF }) {
		t.Error("io.EOF reported among nodes that are not it")
	}
}
