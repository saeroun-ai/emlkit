package message

import (
	"errors"
	"io"
	"math"
	"strings"
	"testing"
)

// Boundary tests of the bounds audit of the wire and framing code
// (ROB-120-001; see docs/robustness-notes/bounds-audit-core.md). They are
// written to mean the same on a 32-bit int: every budget they set that does
// not fit in 32 bits must behave as the large budget it is.

// baMultipart is a small multipart message whose preamble is pre.
func baMultipart(pre string) string {
	return "Content-Type: multipart/mixed; boundary=b\r\n\r\n" + pre +
		"--b\r\nContent-Type: text/plain\r\n\r\none\r\n" +
		"--b  \t \r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\ndHdv\r\n" +
		"--b--\r\n"
}

// baReadParts iterates the parts of e to the end and returns their bodies.
func baReadParts(t *testing.T, e *Entity) (bodies []string, mr MultipartReader) {
	t.Helper()
	mr = e.MultipartReader()
	if mr == nil {
		t.Fatal("MultipartReader() = nil")
	}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return bodies, mr
		}
		if err != nil {
			t.Fatalf("NextPart() = %v", err)
		}
		b, err := io.ReadAll(p.Body)
		if err != nil {
			t.Fatalf("part body: %v", err)
		}
		bodies = append(bodies, string(b))
	}
}

func baHasCode(s *ParseSession, code Code) bool {
	for _, d := range s.Diagnostics() {
		if d.Code == code {
			return true
		}
	}
	return false
}

// A disabled preamble budget is no budget: textproto reads a limit of zero or
// below as its 1 MiB default, so LimitDisabled must not reach it as -1.
func TestBoundsCore_preambleLimitDisabled(t *testing.T) {
	const line = "a preamble line\r\n"
	pre := strings.Repeat(line, (1<<20)/len(line)+16) // just past 1 MiB
	for _, tc := range []struct {
		name  string
		limit int64
		cut   bool
	}{
		{"default", 0, true},
		{"disabled", LimitDisabled, false},
		{"past 32 bits", 1 << 40, false},
		{"math.MaxInt64", math.MaxInt64, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, e, err := Parse(strings.NewReader(baMultipart(pre)), &ParseOptions{Limits: Limits{MaxPreambleBytes: tc.limit}})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			bodies, mr := baReadParts(t, e)
			if len(bodies) != 2 || bodies[0] != "one" || bodies[1] != "two" {
				t.Errorf("parts = %q, want one and two", bodies)
			}
			got, truncated, ok := MultipartPreamble(mr)
			if !ok || truncated != tc.cut || (!tc.cut && len(got) != len(pre)) || (tc.cut && len(got) != 1<<20) {
				t.Errorf("MultipartPreamble() = %d bytes, truncated %v, ok %v; want cut %v of %d", len(got), truncated, ok, tc.cut, len(pre))
			}
			if baHasCode(s, CodeLimitPreambleBytes) != tc.cut {
				t.Errorf("limit.preamble_bytes recorded = %v, want %v", !tc.cut, tc.cut)
			}
		})
	}
}

// Budgets that do not fit in 32 bits, and math.MaxInt64, are ordinary large
// budgets everywhere a length is compared with one: nothing truncates them.
func TestBoundsCore_hugeBudgets(t *testing.T) {
	long := strings.Repeat("x", 5000) // longer than one 4096-byte buffer
	msg := "Subject: " + long + "\r\n" +
		baMultipart(long+"\r\n--b "+strings.Repeat(" ", 5000)+"x\r\n")
	msg = strings.Replace(msg, "\r\none\r\n", "\r\none\r\n--b"+strings.Repeat("\t", 5000)+"y\r\n", 1)
	wantFirst := "one\r\n--b" + strings.Repeat("\t", 5000) + "y"
	for _, v := range []int64{1 << 31, 1<<32 + 5, 1 << 40, math.MaxInt64} {
		l := Limits{}
		for _, f := range limitFields {
			*f.get(&l) = v
		}
		s, e, err := Parse(strings.NewReader(msg), &ParseOptions{Limits: l})
		if err != nil {
			t.Fatalf("limit %d: Parse() = %v", v, err)
		}
		if got := e.Header.Get("Subject"); got != long {
			t.Errorf("limit %d: Subject of %d bytes, want %d", v, len(got), len(long))
		}
		bodies, _ := baReadParts(t, e)
		if len(bodies) != 2 || bodies[0] != wantFirst || bodies[1] != "two" {
			t.Errorf("limit %d: parts = %.40q (%d), want 2", v, bodies, len(bodies))
		}
		for _, d := range s.Diagnostics() {
			if d.Severity == SeverityLimitExceeded || d.Severity == SeverityFatal {
				t.Errorf("limit %d: %v", v, d)
			}
		}
	}
}

// baLiar is a source that breaks the io.Reader contract: it reports more
// bytes than it was given room for, or a negative count.
type baLiar struct {
	s     string
	extra int
	calls int
}

func (r *baLiar) Read(p []byte) (int, error) {
	r.calls++
	if r.calls > 1000 {
		return 0, errors.New("baLiar: read too often")
	}
	if r.s == "" {
		return r.extra, io.EOF
	}
	n := copy(p, r.s)
	r.s = r.s[n:]
	return n + r.extra, nil
}

// A source that reports a count outside [0, len(p)] must not reach the
// buffered readers of a parse with it: bufio panics on one.
func TestBoundsCore_sourceCountOutOfRange(t *testing.T) {
	for _, extra := range []int{1, 1 << 20, -1, -(1 << 20), math.MaxInt, math.MinInt} {
		for _, legacy := range []bool{false, true} {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("extra %d, legacy %v: panicked: %v", extra, legacy, r)
					}
				}()
				src := &baLiar{s: baMultipart("pre\r\n"), extra: extra}
				var e *Entity
				if legacy {
					e, _ = Read(src)
				} else {
					_, e, _ = Parse(src, nil)
				}
				if e == nil {
					// No entity is an acceptable outcome; a panic is not.
					return
				}
				leaves := 0
				e.Walk(func(path []int, p *Entity, err error) error {
					if p.MultipartReader() == nil {
						leaves++
						io.Copy(io.Discard, io.LimitReader(p.Body, 1<<20))
					}
					return nil
				})
				// Counts above the room are the room, so nothing of the input
				// is lost to them; a negative count is no bytes, and ends in
				// the no-progress rule or the end of the input.
				if extra > 0 && leaves != 2 {
					t.Errorf("extra %d, legacy %v: %d leaves, want 2", extra, legacy, leaves)
				}
			}()
		}
	}
}
