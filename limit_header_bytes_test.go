package message

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/saeroun-ai/emlkit/textproto"
)

// The header byte budget applied to every entity of a parse. Each fixture below
// puts the block under test at a different depth and pads it to be the largest
// in its message, so that sweeping one budget across its size says something
// about that block and about no other.
//
// Every size and position is derived from the fixture's labels rather than
// counted out by hand, so a change to a fixture cannot quietly move a boundary
// these tests claim to pin.

// budgetPad returns a header field line of exactly n bytes, its terminator
// included.
func budgetPad(n int) string {
	const name = "X-Pad: "
	return name + strings.Repeat("x", n-len(name)-len("\r\n")) + "\r\n"
}

// rootBudgetFixture pads the message's own header block to be the largest.
func rootBudgetFixture() originFixture {
	return newOriginFixture(
		[2]string{"root.header", "Mime-Version: 1.0\r\n"},
		[2]string{"", "Content-Type: multipart/mixed; boundary=B\r\n"},
		[2]string{"", budgetPad(200)},
		[2]string{"", "\r\n"},
		[2]string{"root.body", "--B\r\n"},
		[2]string{"0.header", "Content-Type: text/plain\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"0.body", "zero\r\n"},
		[2]string{"", "--B--\r\n"},
	)
}

// childBudgetFixture pads the header block of the middle part, so that a
// sibling is returned before the failure and a sibling is left after it.
func childBudgetFixture() originFixture {
	return newOriginFixture(
		[2]string{"root.header", "Mime-Version: 1.0\r\n"},
		[2]string{"", "Content-Type: multipart/mixed; boundary=B\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"root.body", "--B\r\n"},
		[2]string{"0.header", "Content-Type: text/plain\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"0.body", "zero\r\n"},
		[2]string{"", "--B\r\n"},
		[2]string{"1.header", "Content-Type: text/plain\r\n"},
		[2]string{"", budgetPad(200)},
		[2]string{"", "\r\n"},
		[2]string{"1.body", "one\r\n"},
		[2]string{"", "--B\r\n"},
		[2]string{"2.header", "Content-Type: text/plain\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"2.body", "two\r\n"},
		[2]string{"", "--B--\r\n"},
	)
}

// deepBudgetFixture pads the header block of a part two levels down, so that
// the budget has to reach a child of a child and to name it correctly.
func deepBudgetFixture() originFixture {
	return newOriginFixture(
		[2]string{"root.header", "Mime-Version: 1.0\r\n"},
		[2]string{"", "Content-Type: multipart/mixed; boundary=OUTER\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"root.body", "--OUTER\r\n"},
		[2]string{"0.header", "Content-Type: multipart/alternative; boundary=INNER\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"0.body", "--INNER\r\n"},
		[2]string{"0.0.header", "Content-Type: text/plain\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"0.0.body", "deep zero\r\n"},
		[2]string{"", "--INNER\r\n"},
		[2]string{"0.1.header", "Content-Type: text/html\r\n"},
		[2]string{"", budgetPad(200)},
		[2]string{"", "\r\n"},
		[2]string{"0.1.body", "deep one\r\n"},
		[2]string{"", "--INNER--\r\n"},
		[2]string{"", "--OUTER\r\n"},
		[2]string{"1.header", "Content-Type: text/plain\r\n"},
		[2]string{"", "\r\n"},
		[2]string{"1.body", "second\r\n"},
		[2]string{"", "--OUTER--\r\n"},
	)
}

// headerBlock returns where an entity's header block begins in the input and
// how many bytes it covers, both derived from the fixture's labels.
func headerBlock(t *testing.T, f originFixture, entity string) (offset, size int64) {
	t.Helper()
	offset = f.offset(t, entity+".header")
	return offset, f.offset(t, entity+".body") - offset
}

// budgetCase is one fixture together with the entity whose header block the
// budget is swept across, and what a parse under a sufficient budget yields.
type budgetCase struct {
	name string

	// entity labels the padded header block, using the fixture's own labels:
	// "root" for the message itself, "1" for a part of it, "0.1" for a part of
	// a part.
	entity string

	// path is the MIME path of that entity, as a diagnostic reports it.
	path []int

	// all is the body of every leaf of the message, in the order a traversal
	// reaches them.
	all []string

	// upTo is what a traversal recovers before the refused header block stops
	// it.
	upTo []string

	fixture originFixture
}

func budgetCases() []budgetCase {
	return []budgetCase{
		{
			name:    "root",
			entity:  "root",
			path:    nil,
			all:     []string{"zero"},
			upTo:    nil,
			fixture: rootBudgetFixture(),
		},
		{
			name:    "child",
			entity:  "1",
			path:    []int{1},
			all:     []string{"zero", "one", "two"},
			upTo:    []string{"zero"},
			fixture: childBudgetFixture(),
		},
		{
			name:    "deep child",
			entity:  "0.1",
			path:    []int{0, 1},
			all:     []string{"deep zero", "deep one", "second"},
			upTo:    []string{"deep zero"},
			fixture: deepBudgetFixture(),
		},
	}
}

// treeResult is what reading a whole message yielded: the body of every leaf
// reached, in order, and what stopped the traversal.
type treeResult struct {
	bodies []string
	err    error
}

// readTree walks the entity's parts depth first, reading every leaf body
// whole, and stops at the first failure. It is deliberately not Walk: a
// traversal that consumes bodies is what a caller does, and it is what would
// notice a body byte lost to a header read.
func readTree(e *Entity) treeResult {
	var out treeResult

	var visit func(e *Entity) error
	visit = func(e *Entity) error {
		mr := e.MultipartReader()
		if mr == nil {
			b, err := io.ReadAll(e.Body)
			out.bodies = append(out.bodies, strings.TrimSuffix(string(b), "\r\n"))
			return err
		}
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				return nil
			}
			// A part whose transfer encoding or charset has no decoder is
			// returned with a warning and is perfectly usable, so it is read
			// rather than treated as the end of the message. Everything else is
			// what stopped the traversal.
			if err != nil && (p == nil || !(IsUnknownEncoding(err) || IsUnknownCharset(err))) {
				return err
			}
			if err := visit(p); err != nil {
				return err
			}
		}
	}

	out.err = visit(e)
	return out
}

// parseTree parses under a header budget and reads everything it can.
func parseTree(t *testing.T, text string, limit int64) (*ParseSession, treeResult, error) {
	t.Helper()

	session, e, err := Parse(strings.NewReader(text), &ParseOptions{
		Limits: Limits{MaxHeaderBytes: limit},
	})
	if e == nil {
		return session, treeResult{}, err
	}
	return session, readTree(e), err
}

// checkLimitDiagnostic pins the one diagnostic a refused header block records:
// its code, its severity, the entity it names, and the position it reports.
func checkLimitDiagnostic(t *testing.T, session *ParseSession, limit int64, path []int, offset int64) {
	t.Helper()

	diags := session.Diagnostics()
	if len(diags) != 1 {
		t.Fatalf("Diagnostics() = %v, want exactly one limit.header_bytes entry", diags)
	}
	d := diags[0]

	if d.Code != CodeLimitHeaderBytes {
		t.Errorf("Code = %q, want %q", d.Code, CodeLimitHeaderBytes)
	}
	if d.Severity != SeverityLimitExceeded {
		t.Errorf("Severity = %v, want %v", d.Severity, SeverityLimitExceeded)
	}
	if !reflect.DeepEqual(d.Path, path) {
		t.Errorf("Path = %v, want %v", d.Path, path)
	}
	if want := LocationAt(offset); d.Location != want {
		t.Errorf("Location = %v, want %v: the first byte of the refused header block", d.Location, want)
	}
	if d.Action == "" {
		t.Errorf("Action is empty, want what was done about the defect")
	}
	if !strings.Contains(d.Message, fmt.Sprint(limit)) {
		t.Errorf("Message = %q, want the budget %d in it", d.Message, limit)
	}
	if !errors.Is(d.Cause, textproto.ErrHeaderTooBig) {
		t.Errorf("Cause = %v, want the header budget failure", d.Cause)
	}
}

// checkLimitFatal pins the error a refused header block returns: fatal, wrapping
// the budget failure, and carrying the same diagnostic with the severity a
// fatal error's diagnostic always has.
func checkLimitFatal(t *testing.T, err error, path []int, offset int64) {
	t.Helper()

	if !IsFatal(err) {
		t.Fatalf("err = %v, want a FatalError", err)
	}
	if !errors.Is(err, textproto.ErrHeaderTooBig) {
		t.Errorf("err = %v, want it to wrap the header budget failure", err)
	}

	var fatal FatalError
	if !errors.As(err, &fatal) {
		t.Fatalf("err = %v, want a FatalError", err)
	}
	d, ok := fatal.Diagnostic()
	if !ok {
		t.Fatalf("FatalError carries no diagnostic, want the one that stopped the scope")
	}
	if d.Code != CodeLimitHeaderBytes {
		t.Errorf("fatal diagnostic Code = %q, want %q", d.Code, CodeLimitHeaderBytes)
	}
	if d.Severity != SeverityFatal {
		t.Errorf("fatal diagnostic Severity = %v, want %v", d.Severity, SeverityFatal)
	}
	if !reflect.DeepEqual(d.Path, path) {
		t.Errorf("fatal diagnostic Path = %v, want %v", d.Path, path)
	}
	if want := LocationAt(offset); d.Location != want {
		t.Errorf("fatal diagnostic Location = %v, want %v", d.Location, want)
	}
}

// TestParse_headerBytes_boundary sweeps the budget across the size of one
// header block at each depth a message has, and pins where the boundary falls:
// a block of exactly the budget is read, one byte more is refused, and the
// refusal names the entity it happened to and where in the input that entity
// began.
func TestParse_headerBytes_boundary(t *testing.T) {
	for _, tc := range budgetCases() {
		t.Run(tc.name, func(t *testing.T) {
			offset, size := headerBlock(t, tc.fixture, tc.entity)

			for _, delta := range []int64{-1, 0, 1} {
				t.Run(fmt.Sprintf("limit%+d", delta), func(t *testing.T) {
					limit := size + delta
					session, got, err := parseTree(t, tc.fixture.text, limit)

					if delta >= 0 {
						if err != nil {
							t.Fatalf("Parse() = %v, want a header block of %d bytes to fit a %d byte budget",
								err, size, limit)
						}
						if got.err != nil {
							t.Fatalf("reading the tree = %v", got.err)
						}
						if !reflect.DeepEqual(got.bodies, tc.all) {
							t.Errorf("bodies = %v, want %v", got.bodies, tc.all)
						}
						if d := session.Diagnostics(); d != nil {
							t.Errorf("Diagnostics() = %v, want none", d)
						}
						return
					}

					// One byte over. The scope that was reading the block ends,
					// and everything recovered before it stays valid.
					stopped := err
					if stopped == nil {
						stopped = got.err
					}
					checkLimitFatal(t, stopped, tc.path, offset)
					checkLimitDiagnostic(t, session, limit, tc.path, offset)

					if !reflect.DeepEqual(got.bodies, tc.upTo) {
						t.Errorf("bodies = %v, want %v recovered before the refusal", got.bodies, tc.upTo)
					}
				})
			}
		})
	}
}

// TestParse_headerBytes_rootRefusalYieldsNoEntity pins the one scope whose
// failure leaves nothing at all: the message's own header block.
func TestParse_headerBytes_rootRefusalYieldsNoEntity(t *testing.T) {
	f := rootBudgetFixture()
	_, size := headerBlock(t, f, "root")

	session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{
		Limits: Limits{MaxHeaderBytes: size - 1},
	})
	if e != nil {
		t.Errorf("Parse() = %v, want no entity when the message header was refused", e)
	}
	checkLimitFatal(t, err, nil, 0)
	checkLimitDiagnostic(t, session, size-1, nil, 0)
}

// TestParse_headerBytes_disabled pins that LimitDisabled turns the budget off
// at every depth, through either spelling of it.
func TestParse_headerBytes_disabled(t *testing.T) {
	for _, tc := range budgetCases() {
		for _, opts := range []struct {
			name string
			opts *ParseOptions
		}{
			{"limits", &ParseOptions{Limits: Limits{MaxHeaderBytes: LimitDisabled}}},
			{"read options", &ParseOptions{ReadOptions: ReadOptions{MaxHeaderBytes: LimitDisabled}}},
			{"both", &ParseOptions{
				Limits:      Limits{MaxHeaderBytes: LimitDisabled},
				ReadOptions: ReadOptions{MaxHeaderBytes: LimitDisabled},
			}},
		} {
			t.Run(tc.name+"/"+opts.name, func(t *testing.T) {
				session, e, err := Parse(strings.NewReader(tc.fixture.text), opts.opts)
				if err != nil {
					t.Fatalf("Parse() = %v", err)
				}
				got := readTree(e)
				if got.err != nil {
					t.Fatalf("reading the tree = %v", got.err)
				}
				if !reflect.DeepEqual(got.bodies, tc.all) {
					t.Errorf("bodies = %v, want %v", got.bodies, tc.all)
				}
				if d := session.Diagnostics(); d != nil {
					t.Errorf("Diagnostics() = %v, want none from a disabled budget", d)
				}
				if got := session.Limits().MaxHeaderBytes; got != LimitDisabled {
					t.Errorf("Limits().MaxHeaderBytes = %d, want LimitDisabled", got)
				}
			})
		}
	}
}

// TestParse_headerBytes_perEntityNotCumulative pins that the budget bounds one
// header block rather than all of them together: a message of many parts is
// read under a budget that the sum of its headers exceeds many times over.
func TestParse_headerBytes_perEntityNotCumulative(t *testing.T) {
	const parts = 50

	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=B\r\n\r\n")
	for i := 0; i < parts; i++ {
		b.WriteString("--B\r\n")
		b.WriteString("Content-Type: text/plain\r\n")
		b.WriteString(budgetPad(120))
		b.WriteString("\r\n")
		fmt.Fprintf(&b, "part %d\r\n", i)
	}
	b.WriteString("--B--\r\n")

	// The largest single header block in this message, which is what one part's
	// header costs: its content type line, its padding, and the empty line.
	size := int64(len("Content-Type: text/plain\r\n") + 120 + len("\r\n"))

	session, got, err := parseTree(t, b.String(), size)
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if got.err != nil {
		t.Fatalf("reading the tree = %v", got.err)
	}
	if len(got.bodies) != parts {
		t.Errorf("read %d parts, want %d: the budget is per entity, not cumulative", len(got.bodies), parts)
	}
	if d := session.Diagnostics(); d != nil {
		t.Errorf("Diagnostics() = %v, want none", d)
	}
}

// TestParse_headerBytes_chunkInvariance pins that the same input decides the
// same way however it arrives — one byte at a time, in chunks that fall inside
// header lines and across them, in randomly sized pieces, and from a reader
// that reports the end of the input together with its last bytes.
func TestParse_headerBytes_chunkInvariance(t *testing.T) {
	for _, tc := range budgetCases() {
		t.Run(tc.name, func(t *testing.T) {
			offset, size := headerBlock(t, tc.fixture, tc.entity)

			sources := map[string]func() io.Reader{
				"whole": func() io.Reader { return strings.NewReader(tc.fixture.text) },
				"data with eof": func() io.Reader {
					return iotest.DataErrReader(strings.NewReader(tc.fixture.text))
				},
			}
			for _, n := range []int{1, 2, 3, 7, 64, 4095, 4096} {
				sources[fmt.Sprintf("chunks of %d", n)] = func() io.Reader {
					return &chunkReader{s: tc.fixture.text, n: n}
				}
				sources[fmt.Sprintf("chunks of %d with eof", n)] = func() io.Reader {
					return iotest.DataErrReader(&chunkReader{s: tc.fixture.text, n: n})
				}
			}
			for _, seed := range []int64{1, 2, 3} {
				sources[fmt.Sprintf("random chunks seed %d", seed)] = func() io.Reader {
					return newRandomChunkReader(tc.fixture.text, 17, seed)
				}
			}

			for name, src := range sources {
				for _, delta := range []int64{-1, 0} {
					limit := size + delta

					session, e, err := Parse(src(), &ParseOptions{Limits: Limits{MaxHeaderBytes: limit}})
					var got treeResult
					if e != nil {
						got = readTree(e)
					}

					want := tc.all
					if delta < 0 {
						want = tc.upTo
					}
					if !reflect.DeepEqual(got.bodies, want) {
						t.Errorf("%s at limit%+d: bodies = %v, want %v", name, delta, got.bodies, want)
					}

					stopped := err
					if stopped == nil {
						stopped = got.err
					}
					if delta == 0 {
						if stopped != nil {
							t.Errorf("%s at the exact limit: %v", name, stopped)
						}
						if d := session.Diagnostics(); d != nil {
							t.Errorf("%s at the exact limit: Diagnostics() = %v, want none", name, d)
						}
						continue
					}
					if !IsFatal(stopped) {
						t.Errorf("%s at limit-1: err = %v, want a FatalError", name, stopped)
						continue
					}
					checkLimitDiagnostic(t, session, limit, tc.path, offset)
				}
			}
		})
	}
}

// TestParse_headerBytes_readAheadKeepsBodies pins that budgeting a header block
// takes nothing from what follows it. The parse under an exact budget is
// compared with an unbudgeted read of the same message, entity by entity: the
// same tree, the same positions, and the same bytes.
//
// The buffered reader under the header parser reads ahead of what the header
// parser consumes, so a budget enforced on that reader rather than on the block
// would shorten the body's reads or swallow its first bytes. Neither shows up
// as a failure — it shows up as a body that is quietly short — so it is checked
// against the same message read with no budget at all.
func TestParse_headerBytes_readAheadKeepsBodies(t *testing.T) {
	for _, tc := range budgetCases() {
		t.Run(tc.name, func(t *testing.T) {
			_, size := headerBlock(t, tc.fixture, tc.entity)

			unbudgeted, err := Read(strings.NewReader(tc.fixture.text))
			if err != nil {
				t.Fatalf("Read() = %v", err)
			}
			wantOrigins := collectOrigins(t, unbudgeted)

			for _, delta := range []int64{0, 1, 64} {
				_, e, err := Parse(strings.NewReader(tc.fixture.text), &ParseOptions{
					Limits: Limits{MaxHeaderBytes: size + delta},
				})
				if err != nil {
					t.Fatalf("limit%+d: Parse() = %v", delta, err)
				}

				got := readTree(e)
				if got.err != nil {
					t.Fatalf("limit%+d: reading the tree = %v", delta, got.err)
				}
				if !reflect.DeepEqual(got.bodies, tc.all) {
					t.Errorf("limit%+d: bodies = %v, want %v", delta, got.bodies, tc.all)
				}

				_, e, err = Parse(strings.NewReader(tc.fixture.text), &ParseOptions{
					Limits: Limits{MaxHeaderBytes: size + delta},
				})
				if err != nil {
					t.Fatalf("limit%+d: Parse() = %v", delta, err)
				}
				if gotOrigins := collectOrigins(t, e); !reflect.DeepEqual(gotOrigins, wantOrigins) {
					t.Errorf("limit%+d: origins =\n%v\nwant\n%v", delta, gotOrigins, wantOrigins)
				}
			}
		})
	}
}

// TestParse_headerBytes_verbatimRoundTrip pins that a message parsed under an
// exact budget still writes back out byte for byte, which is the invariant a
// DKIM signature depends on.
func TestParse_headerBytes_verbatimRoundTrip(t *testing.T) {
	for _, tc := range budgetCases() {
		t.Run(tc.name, func(t *testing.T) {
			_, size := headerBlock(t, tc.fixture, tc.entity)

			_, e, err := Parse(strings.NewReader(tc.fixture.text), &ParseOptions{
				Limits: Limits{MaxHeaderBytes: size},
			})
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}

			var b bytes.Buffer
			if err := e.WriteTo(&b); err != nil {
				t.Fatalf("WriteTo() = %v", err)
			}
			if got := b.String(); got != tc.fixture.text {
				t.Errorf("round trip changed the message:\n%q\nwant\n%q", got, tc.fixture.text)
			}
		})
	}
}

// TestParse_headerBytes_legacyChildHeadersUnbounded pins the compatibility half
// of this item: Read and ReadWithOptions resolve no configuration and budget no
// part header, so a part whose header block dwarfs the budget the message
// header was held to is still read, exactly as it always was.
func TestParse_headerBytes_legacyChildHeadersUnbounded(t *testing.T) {
	f := childBudgetFixture()
	_, rootSize := headerBlock(t, f, "root")
	_, childSize := headerBlock(t, f, "1")

	if childSize <= rootSize {
		t.Fatalf("fixture child header is %d bytes and the root's is %d; the child must be the larger",
			childSize, rootSize)
	}

	// A budget large enough for the message header and far too small for the
	// part's, which the legacy path must not apply to the part at all.
	for _, tc := range []struct {
		name string
		read func() (*Entity, error)
	}{
		{"ReadWithOptions", func() (*Entity, error) {
			return ReadWithOptions(strings.NewReader(f.text), &ReadOptions{MaxHeaderBytes: rootSize})
		}},
		{"Read", func() (*Entity, error) {
			return Read(strings.NewReader(f.text))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := tc.read()
			if err != nil {
				t.Fatalf("%s() = %v", tc.name, err)
			}
			got := readTree(e)
			if got.err != nil {
				t.Fatalf("reading the tree = %v", got.err)
			}
			want := []string{"zero", "one", "two"}
			if !reflect.DeepEqual(got.bodies, want) {
				t.Errorf("bodies = %v, want %v: part headers are not budgeted on this path",
					got.bodies, want)
			}
		})
	}
}

// TestParse_headerBytes_legacyRootFailureUnchanged pins the other half: the
// message header keeps the budget it always had on the legacy path, refuses a
// header block a byte over it, and refuses it with an ordinary error rather
// than a fatal one.
//
// What that error says is left alone rather than pinned, and deliberately so.
// The legacy path bounds the reads under the header parser instead of the block
// itself, so a budget that runs out where a line does leaves the parser looking
// at a stub of a line and reporting it as malformed. That is what it has always
// done, and this item does not change it; a parse reports the budget failure as
// the budget failure at every cut, which is what its own boundary tests pin.
func TestParse_headerBytes_legacyRootFailureUnchanged(t *testing.T) {
	f := rootBudgetFixture()
	_, size := headerBlock(t, f, "root")

	e, err := ReadWithOptions(strings.NewReader(f.text), &ReadOptions{MaxHeaderBytes: size - 1})
	if e != nil {
		t.Errorf("ReadWithOptions() = %v, want no entity", e)
	}
	if err == nil {
		t.Errorf("ReadWithOptions() = nil, want a header block a byte over the budget to be refused")
	}
	if IsFatal(err) {
		t.Errorf("ReadWithOptions() = %v, want the ordinary error it always returned", err)
	}

	// A budget that runs out inside a line is the cut that has always reported
	// the budget failure by name, and still does.
	_, err = ReadWithOptions(strings.NewReader(f.text), &ReadOptions{MaxHeaderBytes: size / 2})
	if !errors.Is(err, textproto.ErrHeaderTooBig) {
		t.Errorf("ReadWithOptions() = %v, want the header budget failure", err)
	}
	if IsFatal(err) {
		t.Errorf("ReadWithOptions() = %v, want the ordinary error it always returned", err)
	}

	// And the block of exactly the budget is still read.
	if _, err := ReadWithOptions(strings.NewReader(f.text), &ReadOptions{MaxHeaderBytes: size}); err != nil {
		t.Errorf("ReadWithOptions() at the exact budget = %v", err)
	}
}

// TestParse_headerBytes_stickyWithoutDuplicates pins what a caller who keeps
// asking is told: the identical error every time, and one diagnostic however
// many times it asks. A reader standing inside a header block it could not
// finish has nothing further to offer, and re-reading it would report the same
// defect twice.
func TestParse_headerBytes_stickyWithoutDuplicates(t *testing.T) {
	f := childBudgetFixture()
	offset, size := headerBlock(t, f, "1")

	session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{
		Limits: Limits{MaxHeaderBytes: size - 1},
	})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	mr := e.MultipartReader()
	if mr == nil {
		t.Fatal("MultipartReader() = nil, want the parts of a multipart message")
	}

	// The sibling before the refused part is returned and is complete.
	first, err := mr.NextPart()
	if err != nil {
		t.Fatalf("NextPart() = %v", err)
	}
	if got := readBodyOf(t, first); got != "zero" {
		t.Errorf("first part = %q, want %q", got, "zero")
	}

	_, want := mr.NextPart()
	checkLimitFatal(t, want, []int{1}, offset)

	for i := 0; i < 3; i++ {
		p, err := mr.NextPart()
		if p != nil {
			t.Errorf("call %d after the refusal returned %v, want no part", i, p)
		}
		if err != want {
			t.Errorf("call %d after the refusal = %v, want the identical error %v", i, err, want)
		}
	}

	checkLimitDiagnostic(t, session, size-1, []int{1}, offset)

	// The sibling handed over before the failure is still readable afterwards,
	// as is the entity whose parts these are.
	if first.Header.Get("Content-Type") != "text/plain" {
		t.Errorf("the part returned before the failure lost its header")
	}
}

// TestParse_headerBytes_walkReportsTheFailure pins that a traversal ends the
// same way an iterator does, having already visited what came before.
func TestParse_headerBytes_walkReportsTheFailure(t *testing.T) {
	f := deepBudgetFixture()
	offset, size := headerBlock(t, f, "0.1")

	session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{
		Limits: Limits{MaxHeaderBytes: size - 1},
	})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	var visited [][]int
	walkErr := e.Walk(func(path []int, part *Entity, err error) error {
		if err != nil {
			return err
		}
		visited = append(visited, path)
		return nil
	})

	checkLimitFatal(t, walkErr, []int{0, 1}, offset)
	checkLimitDiagnostic(t, session, size-1, []int{0, 1}, offset)

	want := [][]int{nil, {0}, {0, 0}}
	if !reflect.DeepEqual(visited, want) {
		t.Errorf("Walk visited %v, want %v: the parts before the refusal", visited, want)
	}
}

// TestParse_headerBytes_sinkReceivesOnce pins that a sink is told about the
// refusal exactly once, at every depth, and that a sink which retains nothing
// still receives it.
func TestParse_headerBytes_sinkReceivesOnce(t *testing.T) {
	for _, tc := range budgetCases() {
		t.Run(tc.name, func(t *testing.T) {
			offset, size := headerBlock(t, tc.fixture, tc.entity)

			var seen []Diagnostic
			session, e, err := Parse(strings.NewReader(tc.fixture.text), &ParseOptions{
				Limits: Limits{MaxHeaderBytes: size - 1},
				DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
					seen = append(seen, d)
					return nil
				}),
				DiscardDeliveredDiagnostics: true,
			})
			if e != nil {
				readTree(e)
			}
			_ = err

			if len(seen) != 1 {
				t.Fatalf("the sink received %v, want exactly one diagnostic", seen)
			}
			if seen[0].Code != CodeLimitHeaderBytes {
				t.Errorf("Code = %q, want %q", seen[0].Code, CodeLimitHeaderBytes)
			}
			if seen[0].Severity != SeverityLimitExceeded {
				t.Errorf("Severity = %v, want %v", seen[0].Severity, SeverityLimitExceeded)
			}
			if !reflect.DeepEqual(seen[0].Path, tc.path) {
				t.Errorf("Path = %v, want %v", seen[0].Path, tc.path)
			}
			if want := LocationAt(offset); seen[0].Location != want {
				t.Errorf("Location = %v, want %v", seen[0].Location, want)
			}
			if got := session.Diagnostics(); got != nil {
				t.Errorf("Diagnostics() = %v, want none retained", got)
			}
		})
	}
}

// TestParse_headerBytes_sinkStopWins pins what a caller is told when its own
// sink refuses to continue while it is being told about the refused header: the
// stop it asked for, which belongs to the whole parse, rather than this scope's
// own failure. The diagnostic was delivered either way.
func TestParse_headerBytes_sinkStopWins(t *testing.T) {
	stop := errors.New("stop here")

	for _, tc := range budgetCases() {
		t.Run(tc.name, func(t *testing.T) {
			_, size := headerBlock(t, tc.fixture, tc.entity)

			var seen []Code
			session, e, err := Parse(strings.NewReader(tc.fixture.text), &ParseOptions{
				Limits: Limits{MaxHeaderBytes: size - 1},
				DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
					seen = append(seen, d.Code)
					return stop
				}),
			})

			stopped := err
			if e != nil {
				stopped = readTree(e).err
			}

			if !errors.Is(stopped, stop) {
				t.Fatalf("err = %v, want the sink's own error", stopped)
			}
			if !IsFatal(stopped) {
				t.Errorf("err = %v, want a FatalError", stopped)
			}
			var fatal FatalError
			if errors.As(stopped, &fatal) {
				if _, ok := fatal.Diagnostic(); ok {
					t.Errorf("the sink's stop carries a diagnostic, want none: it is not a defect of the message")
				}
			}
			if !reflect.DeepEqual(seen, []Code{CodeLimitHeaderBytes}) {
				t.Errorf("the sink received %v, want exactly the header budget diagnostic", seen)
			}
			if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, []Code{CodeLimitHeaderBytes}) {
				t.Errorf("Diagnostics() = %v, want the one diagnostic that was delivered", got)
			}
		})
	}
}

// TestParse_headerBytes_sinkStoppedEarlierRecordsNothing pins that a parse
// already stopped never gets as far as the budget: the scope observes the stop
// before it reads, so no header is refused and nothing is recorded for one.
func TestParse_headerBytes_sinkStoppedEarlierRecordsNothing(t *testing.T) {
	f := childBudgetFixture()
	_, size := headerBlock(t, f, "1")

	// A message whose first part has an unknown transfer encoding, so that a
	// diagnostic is delivered — and the sink stops the parse — before the part
	// with the oversized header is ever reached.
	text := strings.Replace(f.text,
		"--B\r\nContent-Type: text/plain\r\n\r\nzero\r\n",
		"--B\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: nonsense\r\n\r\nzero\r\n", 1)
	if text == f.text {
		t.Fatal("fixture did not change; the test would not exercise an earlier diagnostic")
	}

	stop := errors.New("stop here")
	session, e, err := Parse(strings.NewReader(text), &ParseOptions{
		Limits: Limits{MaxHeaderBytes: size - 1},
		DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
			return stop
		}),
	})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	got := readTree(e)
	if !errors.Is(got.err, stop) {
		t.Fatalf("reading the tree = %v, want the sink's stop", got.err)
	}
	if codes := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(codes, []Code{CodeCTEUnknown}) {
		t.Errorf("Diagnostics() = %v, want only the diagnostic that stopped the parse", codes)
	}
}

// errorAfterReader delivers the first n bytes of s and then fails with err,
// which is how an input that fails on its own is presented to the parser.
type errorAfterReader struct {
	s   string
	n   int
	err error
}

func (r *errorAfterReader) Read(p []byte) (int, error) {
	if r.n <= 0 || len(r.s) == 0 {
		return 0, r.err
	}
	if len(p) > r.n {
		p = p[:r.n]
	}
	n := copy(p, r.s)
	r.s = r.s[n:]
	r.n -= n
	return n, nil
}

// underlyingFailures are the ways an input can fail with the very sentinel a
// budget failure carries: by returning it, and by returning something that
// wraps it.
var underlyingFailures = []struct {
	name string
	err  error
}{
	{"direct", textproto.ErrHeaderTooBig},
	{"wrapped", fmt.Errorf("reading the message: %w", textproto.ErrHeaderTooBig)},
	{"twice wrapped", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", textproto.ErrHeaderTooBig))},
}

// TestParse_headerBytes_underlyingSentinelIsNotDiagnosed pins that a budget is
// diagnosed only when a budget was actually reached.
//
// textproto.ErrHeaderTooBig is public and documented, so an io.Reader under this
// parser may fail with it for reasons of its own. Classifying by that sentinel
// would turn such a failure into a limit.header_bytes diagnostic and a
// FatalError — a report of a budget that was never approached, on a message that
// may be nowhere near it. The refusal is recognized by the error textproto
// builds for its own budget instead, so an input failure stays an input failure
// at the message header and at a part header alike.
func TestParse_headerBytes_underlyingSentinelIsNotDiagnosed(t *testing.T) {
	f := childBudgetFixture()

	// Each cut is a line boundary inside the header block named, so that the
	// input's failure is what the header parser meets next. A line cut in half
	// is reinterpreted as a malformed field long before anything classifies the
	// failure — long-standing behavior, and not what this test is about.
	for _, where := range []struct {
		name string
		// cut is how many bytes the input delivers before it fails.
		cut int
	}{
		{"in the message header", strings.Index(f.text, "\r\n") + len("\r\n")},
		{"in a part header", int(f.offset(t, "0.body")) - len("\r\n")},
	} {
		for _, tc := range underlyingFailures {
			t.Run(where.name+"/"+tc.name, func(t *testing.T) {
				src := &errorAfterReader{s: f.text, n: where.cut, err: tc.err}

				// A budget far larger than every block in the message, so
				// nothing here can be the budget's doing.
				session, e, err := Parse(src, &ParseOptions{
					Limits: Limits{MaxHeaderBytes: 1 << 20},
				})
				if e != nil {
					err = readTree(e).err
				}

				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v, want the input's own failure", err)
				}
				if IsFatal(err) {
					t.Errorf("err = %v, want an ordinary input error, not a parse failure", err)
				}
				if d := session.Diagnostics(); d != nil {
					t.Errorf("Diagnostics() = %v, want none: no budget was reached", codesOfDiags(d))
				}
			})
		}
	}
}

// adversarialFailures are errors an input can fail with that are built to be
// mistaken for a header budget refusal. None of them is one: none was produced
// by a budget refusing bytes.
//
// The forgeries are what an io.Reader under this parser can put in front of the
// message layer. A HeaderLimitError another package writes is the zero value —
// its fields are unexported, so a populated one cannot be assembled outside
// textproto at all — and a typed nil is the shape that a classification by
// errors.As would report as found and then hand over to be dereferenced.
//
// A genuine refusal captured from the parser and handed back is absent, and
// unavoidably so: nothing distinguishes a value from itself.
var adversarialFailures = []struct {
	name string
	err  error
}{
	{"match-all Is", matchAllIsError{}},
	{"wrapped match-all Is", fmt.Errorf("outer: %w", matchAllIsError{})},
	{"forged zero value", &textproto.HeaderLimitError{}},
	{"wrapped forged zero value", fmt.Errorf("outer: %w", &textproto.HeaderLimitError{})},
	{"typed nil", (*textproto.HeaderLimitError)(nil)},
	{"wrapped typed nil", fmt.Errorf("outer: %w", (*textproto.HeaderLimitError)(nil))},
}

// matchAllIsError claims to be every error it is asked about, and to be
// assignable to every target asked of it.
type matchAllIsError struct{}

func (matchAllIsError) Error() string { return "adversarial: is everything" }
func (matchAllIsError) Is(error) bool { return true }
func (matchAllIsError) As(any) bool   { return true }

// TestParse_headerBytes_forgedRefusalIsNotDiagnosed pins that a budget is
// diagnosed only when this parser's own budget refused a block — not when
// something merely presents itself as that refusal.
//
// The error type carrying a refusal is exported, so an input can hand back a
// value of that type, including a typed nil that a classification by errors.As
// would report as found and leave to be dereferenced. Classifying by shape would
// turn any of these into a limit.header_bytes diagnostic on a message nowhere
// near its budget, or into a panic inside the parser. Neither may happen, at the
// message header or at a part header.
func TestParse_headerBytes_forgedRefusalIsNotDiagnosed(t *testing.T) {
	f := childBudgetFixture()

	// Each cut is a line boundary inside the header block named, so the input's
	// failure is what the header parser meets next.
	for _, where := range []struct {
		name string
		cut  int
	}{
		{"in the message header", strings.Index(f.text, "\r\n") + len("\r\n")},
		{"in a part header", int(f.offset(t, "0.body")) - len("\r\n")},
	} {
		for _, tc := range adversarialFailures {
			t.Run(where.name+"/"+tc.name, func(t *testing.T) {
				src := &errorAfterReader{s: f.text, n: where.cut, err: tc.err}

				// A budget far larger than every block in the message, so
				// nothing here can be the budget's doing.
				session, e, err := Parse(src, &ParseOptions{
					Limits: Limits{MaxHeaderBytes: 1 << 20},
				})
				if e != nil {
					err = readTree(e).err
				}

				if err == nil {
					t.Fatal("want the input's failure")
				}
				// Formatting what arrived must not be what brings the parser
				// down either.
				_ = err.Error()

				if parserProducedFatal(err) {
					t.Errorf("err = %v, want an ordinary input error, not a parse failure", err)
				}
				if d := session.Diagnostics(); d != nil {
					t.Errorf("Diagnostics() = %v, want none: no budget was reached", codesOfDiags(d))
				}
			})
		}
	}
}

// parserProducedFatal reports whether err is a fatal error this parser built.
//
// IsFatal cannot answer that about the inputs above. It is errors.As, and one of
// them answers yes to every As question, so IsFatal would report a fatal error
// that was never made — as any errors.As predicate in any program would, and as
// the caller's own error asked it to. The concrete type the parser returns is
// what an input cannot present, so that is what is asked about here.
func parserProducedFatal(err error) bool {
	_, ok := err.(*fatalParseError)
	return ok
}

// TestParse_headerBytes_genuineRefusalStillClassified is the control for the
// test above: the same two header blocks, refused by a budget that really is
// too small, are still diagnosed and still fatal. A fix that stopped mistaking
// input failures for refusals by no longer recognizing refusals would pass there
// and fail here.
func TestParse_headerBytes_genuineRefusalStillClassified(t *testing.T) {
	f := childBudgetFixture()

	for _, tc := range []struct {
		name   string
		entity string
		path   []int
	}{
		{"the message header", "root", nil},
		{"a part header", "1", []int{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			offset, size := headerBlock(t, f, tc.entity)

			session, got, err := parseTree(t, f.text, size-1)
			stopped := err
			if stopped == nil {
				stopped = got.err
			}

			checkLimitFatal(t, stopped, tc.path, offset)
			checkLimitDiagnostic(t, session, size-1, tc.path, offset)
		})
	}
}

// TestParse_headerBytes_discoveryOrder pins where the refusal lands among the
// other things a parse observes: after what was found before it, because that is
// when it was found.
func TestParse_headerBytes_discoveryOrder(t *testing.T) {
	f := childBudgetFixture()
	_, size := headerBlock(t, f, "1")

	// The part before the oversized one declares an encoding nothing decodes,
	// so a diagnostic is recorded before the budget is ever reached.
	text := strings.Replace(f.text,
		"--B\r\nContent-Type: text/plain\r\n\r\nzero\r\n",
		"--B\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: nonsense\r\n\r\nzero\r\n", 1)
	if text == f.text {
		t.Fatal("fixture did not change; the test would not exercise an earlier diagnostic")
	}

	var delivered []Code
	session, e, err := Parse(strings.NewReader(text), &ParseOptions{
		Limits: Limits{MaxHeaderBytes: size - 1},
		DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
			delivered = append(delivered, d.Code)
			return nil
		}),
	})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if got := readTree(e); !IsFatal(got.err) {
		t.Fatalf("reading the tree = %v, want the refusal", got.err)
	}

	want := []Code{CodeCTEUnknown, CodeLimitHeaderBytes}
	if got := codesOfDiags(session.Diagnostics()); !reflect.DeepEqual(got, want) {
		t.Errorf("Diagnostics() = %v, want %v in discovery order", got, want)
	}
	if !reflect.DeepEqual(delivered, want) {
		t.Errorf("the sink received %v, want %v in discovery order", delivered, want)
	}
}

// TestParse_headerBytes_otherFailuresAreNotDiagnosed pins that only the budget
// is translated: a part header that is malformed rather than too large fails
// the way it always has, with nothing recorded, because nothing diagnoses that
// malformed line yet. The line here has neither a colon nor a semicolon
// (ROB-020-003's); a part header that begins with a continuation line is no
// longer such a failure but a malformed prefix (ROB-020-001), pinned in
// framing_firstline_test.go.
func TestParse_headerBytes_otherFailuresAreNotDiagnosed(t *testing.T) {
	const text = "Content-Type: multipart/mixed; boundary=B\r\n" +
		"\r\n" +
		"--B\r\n" +
		"this part header line has no separator\r\n" +
		"\r\n" +
		"body\r\n" +
		"--B--\r\n"

	// ProfileStrict, which heals no header line; the other profiles begin the
	// part's body with such a line (ROB-060-010).
	session, e, err := Parse(strings.NewReader(text), &ParseOptions{Profile: ProfileStrict})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	got := readTree(e)
	if got.err == nil {
		t.Fatal("reading the tree succeeded, want the malformed part header to fail")
	}
	if IsFatal(got.err) {
		t.Errorf("err = %v, want the ordinary error of a malformed header", got.err)
	}
	if errors.Is(got.err, textproto.ErrHeaderTooBig) {
		t.Errorf("err = %v, want it not to be reported as a budget failure", got.err)
	}
	if d := session.Diagnostics(); d != nil {
		t.Errorf("Diagnostics() = %v, want none: a line without a separator is not diagnosed yet", d)
	}
}

// TestParse_headerBytes_replacedBodyIsStillBudgeted pins the budget where the
// position is not known. A caller that swaps an entity's Body after parsing is
// iterating bytes that are not the input's, so a part read out of them has no
// place in the parsed tree and no position in the input — but it is still part
// of this parse, so its header block is still held to this parse's budget, and
// the refusal is still recorded and still ends the iteration. What it does not
// do is claim a path or an offset it cannot know.
func TestParse_headerBytes_replacedBodyIsStillBudgeted(t *testing.T) {
	f := childBudgetFixture()
	_, size := headerBlock(t, f, "1")

	session, e, err := Parse(strings.NewReader(f.text), &ParseOptions{
		Limits: Limits{MaxHeaderBytes: size - 1},
	})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	// The same multipart body, from somewhere else entirely.
	e.Body = strings.NewReader(f.text[f.offset(t, "root.body"):])

	mr := e.MultipartReader()
	if mr == nil {
		t.Fatal("MultipartReader() = nil")
	}
	if _, err := mr.NextPart(); err != nil {
		t.Fatalf("NextPart() = %v, want the part before the oversized one", err)
	}

	_, err = mr.NextPart()
	if !IsFatal(err) {
		t.Fatalf("NextPart() = %v, want a FatalError", err)
	}
	if !errors.Is(err, textproto.ErrHeaderTooBig) {
		t.Errorf("NextPart() = %v, want it to wrap the header budget failure", err)
	}

	diags := session.Diagnostics()
	if len(diags) != 1 {
		t.Fatalf("Diagnostics() = %v, want exactly one", diags)
	}
	if diags[0].Code != CodeLimitHeaderBytes {
		t.Errorf("Code = %q, want %q", diags[0].Code, CodeLimitHeaderBytes)
	}
	if diags[0].Path != nil {
		t.Errorf("Path = %v, want none: these bytes have no place in the parsed tree", diags[0].Path)
	}
	if diags[0].Location != (Location{}) {
		t.Errorf("Location = %v, want unknown: these bytes have no position in the input", diags[0].Location)
	}
}

// readBodyOf reads an entity's body whole, with its trailing line ending
// removed.
func readBodyOf(t *testing.T, e *Entity) string {
	t.Helper()
	b, err := io.ReadAll(e.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return strings.TrimSuffix(string(b), "\r\n")
}

// codesOfDiags reduces diagnostics to their codes, which is what these tests
// compare when the rest of a diagnostic is checked elsewhere.
func codesOfDiags(diags []Diagnostic) []Code {
	var out []Code
	for _, d := range diags {
		out = append(out, d.Code)
	}
	return out
}
