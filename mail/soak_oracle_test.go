package mail_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/saeroun-ai/emlkit/mail"
)

// Tests of the soak's own oracle for a body the spool let go of
// (skAbandonment, skAbandonedAlike; seed 1300007, iteration 202279). The
// exception must accept only extra diagnostics of the abandoned body at the
// end of the list, and keep rejecting every other difference.

type skFakeDiag struct {
	line string
	path []int
}

// skFakePass builds a pass with head before its diagnostic lines, as check
// writes them.
func skFakePass(abandoned bool, at []int, head string, diags ...skFakeDiag) *skPass {
	p := &skPass{abandoned: abandoned, abandonedPath: at}
	p.out.WriteString(head)
	p.diagStart = p.out.Len()
	for _, d := range diags {
		p.out.WriteString(d.line + "\n")
		p.diagPaths = append(p.diagPaths, d.path)
	}
	return p
}

const (
	skFakeTree  = "tree err=true\n[] multipart/mixed container 0 00000000\n[0] text/plain attachment 0 00000000\n[1] text/plain attachment 3 a1b2c3d4\n"
	skFakeState = "state=in-progress entities=3 bodies=1/2 multiparts=0/1 ambiguous=false stop=false\n"
	skFakeHead  = skFakeTree + skFakeState
)

var (
	skDiagRoot  = skFakeDiag{"recovered header.bare_cr []", nil}
	skDiagRoot2 = skFakeDiag{"recovered header.mixed_line_endings []", nil}
	skDiagAt0   = skFakeDiag{"info cte.seven_bit_violation [0]", []int{0}}
	skDiagAt01  = skFakeDiag{"info cte.seven_bit_violation [0 1]", []int{0, 1}}
	skDiagAt1   = skFakeDiag{"info cte.seven_bit_violation [1]", []int{1}}
	skDiagAt2_1 = skFakeDiag{"info charset.invalid_sequence [2 1]", []int{2, 1}}
	skDiagRootX = skFakeDiag{"info cte.seven_bit_violation []", nil}
)

func TestSoakOracle_abandonedAlike(t *testing.T) {
	at0 := []int{0}
	for _, c := range []struct {
		name string
		a, b *skPass
		want bool
	}{
		{"extra diagnostic at the abandoned body",
			skFakePass(true, at0, skFakeHead, skDiagRoot),
			skFakePass(true, at0, skFakeHead, skDiagRoot, skDiagAt0), true},
		{"extra diagnostic under the abandoned body",
			skFakePass(true, at0, skFakeHead, skDiagRoot),
			skFakePass(true, at0, skFakeHead, skDiagRoot, skDiagAt01), true},
		{"identical",
			skFakePass(true, at0, skFakeHead, skDiagRoot, skDiagAt0),
			skFakePass(true, at0, skFakeHead, skDiagRoot, skDiagAt0), true},
		{"root abandoned: anything in the message may follow",
			skFakePass(true, nil, skFakeHead, skDiagRoot),
			skFakePass(true, nil, skFakeHead, skDiagRoot, skDiagRootX, skDiagAt0, skDiagAt2_1), true},

		{"changed tree",
			skFakePass(true, at0, skFakeHead, skDiagRoot),
			skFakePass(true, at0, "tree err=true\n[] multipart/mixed container 0 00000000\n[0] text/html attachment 0 00000000\n[1] text/plain attachment 3 a1b2c3d4\n"+skFakeState, skDiagRoot, skDiagAt0), false},
		{"changed counters",
			skFakePass(true, at0, skFakeHead, skDiagRoot),
			skFakePass(true, at0, skFakeTree+"state=in-progress entities=3 bodies=2/2 multiparts=0/1 ambiguous=false stop=false\n", skDiagRoot, skDiagAt0), false},
		{"changed stop",
			skFakePass(true, at0, skFakeHead, skDiagRoot),
			skFakePass(true, at0, skFakeTree+"state=stopped entities=3 bodies=1/2 multiparts=0/1 ambiguous=false stop=true\n", skDiagRoot, skDiagAt0), false},
		{"changed common diagnostic",
			skFakePass(true, at0, skFakeHead, skDiagRoot),
			skFakePass(true, at0, skFakeHead, skDiagRoot2, skDiagAt0), false},
		{"changed order of common diagnostics",
			skFakePass(true, at0, skFakeHead, skDiagRoot, skDiagRoot2),
			skFakePass(true, at0, skFakeHead, skDiagRoot2, skDiagRoot, skDiagAt0), false},
		{"extra diagnostic in the middle",
			skFakePass(true, at0, skFakeHead, skDiagRoot, skDiagRoot2),
			skFakePass(true, at0, skFakeHead, skDiagRoot, skDiagAt0, skDiagRoot2), false},
		{"extra diagnostic at a sibling",
			skFakePass(true, at0, skFakeHead, skDiagRoot),
			skFakePass(true, at0, skFakeHead, skDiagRoot, skDiagAt1), false},
		{"extra diagnostic at the root above the abandoned body",
			skFakePass(true, at0, skFakeHead, skDiagRoot),
			skFakePass(true, at0, skFakeHead, skDiagRoot, skDiagRootX), false},
		{"extra diagnostic at an ancestor of a nested abandoned body",
			skFakePass(true, []int{0, 1}, skFakeHead, skDiagRoot),
			skFakePass(true, []int{0, 1}, skFakeHead, skDiagRoot, skDiagAt0), false},
		{"abandoned paths differ",
			skFakePass(true, at0, skFakeHead, skDiagRoot),
			skFakePass(true, []int{1}, skFakeHead, skDiagRoot, skDiagAt0), false},
		{"root abandoned in one reading only",
			skFakePass(true, nil, skFakeHead, skDiagRoot),
			skFakePass(true, at0, skFakeHead, skDiagRoot, skDiagAt0), false},
		{"one pass not abandoned",
			skFakePass(true, at0, skFakeHead, skDiagRoot),
			skFakePass(false, nil, skFakeHead, skDiagRoot, skDiagAt0), false},
		{"neither pass abandoned",
			skFakePass(false, nil, skFakeHead, skDiagRoot),
			skFakePass(false, nil, skFakeHead, skDiagRoot, skDiagAt0), false},
	} {
		for _, swap := range []bool{false, true} {
			a, b := c.a, c.b
			if swap {
				a, b = b, a
			}
			if got := skAbandonedAlike(a, b); got != c.want {
				t.Errorf("%s (swapped %v): skAbandonedAlike = %v, want %v", c.name, swap, got, c.want)
			}
		}
	}
}

// skUncomparableErr cannot be compared with ==; skSame must say false.
type skUncomparableErr []string

func (e skUncomparableErr) Error() string { return fmt.Sprint([]string(e)) }

func TestSoakOracle_abandonment(t *testing.T) {
	hookErr := errors.New("mail: spool: store is full")
	sameText := errors.New("mail: spool: store is full")
	stop := errors.New("parse stopped")
	part := &mail.Node{Path: []int{0, 2}}
	root := &mail.Node{}
	for _, c := range []struct {
		name          string
		node          *mail.Node
		hook, tree, s error
		want          bool
		path          []int
	}{
		{"the hook's error ended ReadTree", part, hookErr, hookErr, nil, true, []int{0, 2}},
		{"the hook failed at the root", root, hookErr, hookErr, nil, true, nil},
		{"the parse was stopped too", part, hookErr, hookErr, stop, false, nil},
		{"a parse stop with no hook error", part, nil, stop, stop, false, nil},
		{"ReadTree failed and the hook did not", nil, nil, stop, nil, false, nil},
		{"another value with the same text", part, hookErr, sameText, nil, false, nil},
		{"the hook's error wrapped by someone else", part, hookErr, fmt.Errorf("w: %w", hookErr), nil, false, nil},
		{"ReadTree returned nil", part, hookErr, nil, nil, false, nil},
		{"uncomparable values", part, skUncomparableErr{"x"}, skUncomparableErr{"x"}, nil, false, nil},
	} {
		got, path := skAbandonment(c.node, c.hook, c.tree, c.s)
		if got != c.want || !slices.Equal(path, c.path) {
			t.Errorf("%s: skAbandonment = %v %v, want %v %v", c.name, got, path, c.want, c.path)
		}
	}
}
