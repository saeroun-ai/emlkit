// Package errgraph is the one bounded traversal of an error graph that every
// classification in this module uses (ROB-120-007).
//
// An error the parser classifies may have come from a caller's io.Reader, a
// CharsetReader or a DiagnosticSink, so its dynamic type, its methods and the
// shape of the graph its Unwrap methods describe are not the parser's. The
// errors package asks such an error about itself — Is and As — and follows its
// Unwrap without a bound, so an error can claim any identity, and one that
// unwraps to itself never lets errors.Is or errors.As return. Find answers the
// same kind of question without either weakness.
package errgraph

import "reflect"

const (
	// MaxNodes bounds the errors Find examines, the root included. It is what
	// makes a cycle — an error that unwraps to itself, or a ring of them, or a
	// diamond that reaches one node many times — terminate rather than hang.
	MaxNodes = 256

	// MaxPending bounds the errors held waiting to be examined. The queue is a
	// fixed array, so this is the whole memory a walk costs whatever the graph
	// does.
	MaxPending = 64

	// MaxSlots bounds the child slots Find inspects across the whole walk: one
	// for every Unwrap() error call and one for every element of an
	// Unwrap() []error result it looks at, whether that element is an error,
	// nil or a typed nil. Without it a join whose children are a long run of
	// nils would be scanned to its end, since a nil child never takes a pending
	// slot.
	MaxSlots = 256
)

// Find reports whether match accepts err or an error reachable from it.
//
// The walk is breadth first from err, over both Unwrap() error and
// Unwrap() []error, and has these properties:
//
//   - match is called on every node examined, including nil-valued ones, and
//     must decide by exact dynamic type, by fields only this module can set,
//     or by == against a pointer-typed target. match is this module's code; it
//     must never call a method of the node.
//   - No Error, Is, As or Format method of any node is called. Identity is
//     never established by asking the node.
//   - A node whose value is nil underneath its type — a typed nil pointer, map,
//     slice, func, channel — is never asked to unwrap itself.
//   - An Unwrap method that panics, with any value including panic(nil), is
//     treated as having no children. Nothing it returned is used.
//   - The walk stops after MaxNodes nodes, holds at most MaxPending, and
//     inspects at most MaxSlots child slots. What does not fit is not
//     examined, so a match buried deeper or wider than those bounds is
//     reported as absent rather than searched for without limit. Nothing this
//     module builds comes near them.
//   - No node is hashed or compared with another, so a value of a
//     non-comparable type cannot make the walk panic.
//
// Find bounds the number of Unwrap calls it makes, not what one call does. An
// Unwrap that blocks, loops, allocates without end, calls runtime.Goexit or
// exhausts the stack is code the caller supplied, running on the caller's
// goroutine, and no in-process walk can interrupt it.
func Find(err error, match func(error) bool) bool {
	var (
		queue [MaxPending]error
		head  int // where the next pending node is read from
		count int // pending nodes
		slots int // child slots inspected so far
	)

	for nodes := 0; nodes < MaxNodes; nodes++ {
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

		// A value that is nil underneath is a leaf: its Unwrap may well
		// dereference the nothing it would be called on.
		if slots < MaxSlots && !isNil(err) {
			switch u := err.(type) {
			case interface{ Unwrap() error }:
				slots++
				if child := unwrapOne(u); child != nil && count < len(queue) {
					queue[(head+count)%len(queue)] = child
					count++
				}
			case interface{ Unwrap() []error }:
				children := unwrapMany(u)
				// Both bounds are checked before an element is read, so a
				// hostile list is never read past them, not even by one.
				for i := 0; i < len(children) && slots < MaxSlots && count < len(queue); i++ {
					slots++
					child := children[i]
					if child != nil {
						queue[(head+count)%len(queue)] = child
						count++
					}
				}
			}
		}

		err = nil
	}

	return false
}

// unwrapOne calls u.Unwrap, and reports no child if it panics.
//
// The recovery is unconditional, so it also ends a panic(nil) in a program
// running with GODEBUG=panicnil=1, where recover returns nil. A call that
// panicked never assigned child, which is therefore nil.
func unwrapOne(u interface{ Unwrap() error }) (child error) {
	defer func() { _ = recover() }()
	return u.Unwrap()
}

// unwrapMany calls u.Unwrap, and reports no children if it panics, on the same
// terms as unwrapOne.
func unwrapMany(u interface{ Unwrap() []error }) (children []error) {
	defer func() { _ = recover() }()
	return u.Unwrap()
}

// isNil reports whether err holds nil underneath its type: a nil pointer, map,
// slice, func or channel inside a non-nil error interface. It inspects the
// value through reflect and calls none of its methods.
func isNil(err error) bool {
	v := reflect.ValueOf(err)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice,
		reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return v.IsNil()
	}
	return false
}
