package mail

import (
	"strings"
	"testing"
)

// referenceSkipComment is the comment loop headerParser used before issue #9,
// kept here as the oracle for skipComment. It differs from the original only
// in no longer building the comment text, which no caller read: the loop that
// decides where the comment ends, and whether it ends, is unchanged.
func referenceSkipComment(s string) (rest string, ok bool) {
	depth := 1
	for {
		if len(s) == 0 || depth == 0 {
			break
		}
		if s[0] == '\\' && len(s) > 1 {
			s = s[1:]
		} else if s[0] == '(' {
			depth++
		} else if s[0] == ')' {
			depth--
		}
		s = s[1:]
	}
	return s, depth == 0
}

var skipCommentSeeds = []string{
	"",
	")",
	"a)",
	"a",
	"(",
	"())",
	"(()",
	`\)`,
	`\))`,
	`\`,
	`\\)`,
	`\\\)`,
	`a\(b) rest`,
	`(nested (deeper) \) still) open) tail`,
	"comment) <id@example.org> (next)",
	"\x00\xff) \r\n",
	strings.Repeat("(", 64) + strings.Repeat(")", 65) + "x",
}

// TestSkipComment_matchesReference checks skipComment against the original
// loop on escapes, nesting, closing and unterminated comments: the same rest of
// the value and the same verdict, so no accessor's result or error changes.
func TestSkipComment_matchesReference(t *testing.T) {
	for _, s := range skipCommentSeeds {
		checkSkipComment(t, s)
	}
}

func checkSkipComment(t *testing.T, s string) {
	t.Helper()
	wantRest, wantOK := referenceSkipComment(s)
	p := headerParser{s}
	gotOK := p.skipComment()
	if p.s != wantRest || gotOK != wantOK {
		t.Errorf("skipComment(%q) left %q, %v; reference left %q, %v", s, p.s, gotOK, wantRest, wantOK)
	}
}

func FuzzSkipComment(f *testing.F) {
	for _, s := range skipCommentSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		checkSkipComment(t, s)
	})
}

// TestSkipComment_noAllocation pins the scan as allocation-free for flat,
// deeply nested, escaped and unterminated comments alike.
func TestSkipComment_noAllocation(t *testing.T) {
	for _, s := range []string{
		strings.Repeat("a", 1<<16) + ")",
		strings.Repeat("(", 1<<15) + strings.Repeat(")", 1<<15+1),
		strings.Repeat(`\)`, 1<<15) + ")",
		strings.Repeat("(", 1<<16),
	} {
		allocs := testing.AllocsPerRun(10, func() {
			p := headerParser{s}
			p.skipComment()
		})
		if allocs != 0 {
			t.Errorf("skipComment(%.20q...) made %v allocations, want 0", s, allocs)
		}
	}
}
