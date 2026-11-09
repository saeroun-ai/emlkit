package mail_test

import (
	"bufio"
	"bytes"
	"fmt"
	"net/url"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
	"github.com/saeroun-ai/emlkit/textproto"
)

// allocRuns is how many back-to-back calls allocBytesPerOp measures together.
// It is fixed, so every measurement does the same finite amount of work.
const allocRuns = 32

// allocBytesPerOp reports the mean heap bytes one call of f allocates,
// cumulative over the call (runtime.MemStats.TotalAlloc). Bytes freed by a
// collection during f still count, so an allocator that builds and drops a
// string per input byte is measured at its full cost, not at its live size.
//
// TotalAlloc is process-wide: whatever else the test binary or the runtime
// allocates between the two readings is charged to f as well. A single reading
// around a single call has gone over commentScanBudget although the comment
// scan allocates nothing (issue #59; where those extra bytes came from has not
// been established). So, as testing.AllocsPerRun does for allocation counts,
// the reading is taken around runs calls in a row, with GOMAXPROCS at 1, and
// divided by runs, rounding up. A cost that every call pays, such as a copy of
// the comment, is in the mean at its full size; foreign bytes that land in the
// window are spread over all the calls.
//
// The result is a mean, not each call's own cost. Foreign bytes can only add
// to TotalAlloc, never take from it, so the result is never below the mean of
// f's own costs: noise cannot make a uniformly over-budget f pass, and making
// an in-budget f fail now takes runs times the remaining headroom in one
// window. A cost that only some calls pay is diluted by the share of calls
// that pay it: one paid once, say by the first call (there is no warm-up call,
// unlike AllocsPerRun), is charged at 1/runs. Repeated passes lower the odds
// of a noisy failure; they do not prove the window free of foreign bytes.
func allocBytesPerOp(runs int, f func()) uint64 {
	if runs < 1 {
		panic("allocBytesPerOp: runs must be positive")
	}
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range runs {
		f()
	}
	runtime.ReadMemStats(&after)
	total := after.TotalAlloc - before.TotalAlloc
	return (total + uint64(runs) - 1) / uint64(runs)
}

// allocGrowthIsLinear reports whether costs, measured at the increasing sizes,
// grew no faster than a linear cost does. Over the 8x span of commentSizes a
// linear cost grows about 8x (up to about 12x where buffer doubling straddles
// a power of two) and a quadratic one 64x, so up to three times the size
// ratio passes.
func allocGrowthIsLinear(sizes []int, costs []uint64) (ratio, factor float64, ok bool) {
	first, last := costs[0], costs[len(costs)-1]
	ratio = float64(last) / float64(max(first, 1))
	factor = float64(sizes[len(sizes)-1]) / float64(sizes[0])
	return ratio, factor, ratio <= factor*3
}

// callRecord collects, without allocating, how many measured calls ran and
// the first one that returned an error or a result other than the expected
// one, so every measured call's result is checked, not only the last one's.
// Callers compare results against expected values built before the
// measurement; nothing in a measured call formats, reflects or calls t.
type callRecord struct {
	calls     int
	firstErr  error
	errCall   int // 1-based; 0 if no call failed
	wrongCall int // 1-based; 0 if every result was the expected one
}

func (r *callRecord) note(ok bool, err error) {
	r.calls++
	if err != nil && r.errCall == 0 {
		r.firstErr, r.errCall = err, r.calls
	}
	if !ok && r.wrongCall == 0 {
		r.wrongCall = r.calls
	}
}

// check fails the test unless exactly runs calls were noted, none returned an
// error and every result was the expected one. last is the final call's
// result, shown on failure; it is formatted only here, after the measurement.
func (r *callRecord) check(t *testing.T, label string, runs int, last any) {
	t.Helper()
	if r.calls != runs {
		t.Fatalf("%s: %d calls noted, want %d", label, r.calls, runs)
	}
	if r.errCall != 0 {
		t.Fatalf("%s: call %d of %d returned an unexpected error: %v", label, r.errCall, runs, r.firstErr)
	}
	if r.wrongCall != 0 {
		t.Fatalf("%s: call %d of %d returned an unexpected result (last call returned %v)", label, r.wrongCall, runs, last)
	}
}

// commentPreflightSize is a small comment every allocation check measures
// first, on the same budget. Measuring the small comment costs nothing extra
// today, but a reintroduced quadratic copy fails it at about 0.5 MiB per call
// (16 MiB for the allocRuns calls) and stops the case, rather than first being
// replayed allocRuns times at commentSizes, about 16 GiB at 32 KiB.
const commentPreflightSize = 1 << 10

// commentSizes are the payload sizes N, 2N, 4N, 8N the allocation tests step
// through. The largest is the 32 KiB public reproduction of issue #9.
var commentSizes = []int{4 << 10, 8 << 10, 16 << 10, 32 << 10}

// commentScanBudget is the most one accessor call may allocate around a skipped
// comment, whatever the comment's size. The scan itself allocates nothing; the
// budget leaves room for the result values the accessors return (a msg-id
// string, a list slice, a parsed URL).
const commentScanBudget = 4 << 10

// TestHeader_commentScanAllocationIsConstant is the issue #9 regression. The
// msg-id and list-command accessors used to collect every skipped comment with
// "comment += byte", which copies the whole comment so far for each byte: a
// comment of n bytes cost about n²/2 bytes of garbage, so a 32 KiB comment in
// front of a Message-Id allocated about 0.5 GiB. A skipped comment is never
// used, so it is now scanned in place, and what these accessors allocate no
// longer depends on the comment's size at all.
//
// Each size is measured as the mean of allocRuns calls (see allocBytesPerOp),
// starting with commentPreflightSize, and the first size over budget stops
// that case: a size-dependent cost only grows at the larger sizes, and
// stopping keeps a reintroduced quadratic copy from being replayed at 32 KiB,
// where one call costs about 0.5 GiB. Every measured call's result is compared
// with the expected one (see callRecord), not only the last call's.
func TestHeader_commentScanAllocationIsConstant(t *testing.T) {
	// Expected values are built here, before any measurement; the measured
	// calls only compare against them and store their results, without
	// allocating, formatting or reflecting.
	const wantID = "x@example.org"
	wantIDs := []string{"a@example.org", "b@example.org"}
	wantURL, err := url.Parse("mailto:u@example.org")
	if err != nil {
		t.Fatal(err)
	}
	var (
		id   string
		ids  []string
		urls []*url.URL
	)
	type accessor struct {
		name  string
		key   string
		value func(comment string) string
		// call runs the accessor once and reports whether its result is
		// exactly the expected one; last returns that result for display.
		call func(h *mail.Header) (ok bool, err error)
		last func() any
	}
	accessors := []accessor{{
		name:  "MessageID/leading",
		key:   "Message-Id",
		value: func(c string) string { return c + " <x@example.org>" },
		call: func(h *mail.Header) (bool, error) {
			var err error
			id, err = h.MessageID()
			return id == wantID, err
		},
		last: func() any { return id },
	}, {
		name:  "MessageID/trailing",
		key:   "Message-Id",
		value: func(c string) string { return "<x@example.org> " + c },
		call: func(h *mail.Header) (bool, error) {
			var err error
			id, err = h.MessageID()
			return id == wantID, err
		},
		last: func() any { return id },
	}, {
		name:  "MsgIDList/between",
		key:   "References",
		value: func(c string) string { return "<a@example.org> " + c + " <b@example.org>" },
		call: func(h *mail.Header) (bool, error) {
			var err error
			ids, err = h.MsgIDList("References")
			return len(ids) == len(wantIDs) && ids[0] == wantIDs[0] && ids[1] == wantIDs[1], err
		},
		last: func() any { return ids },
	}, {
		name:  "ListCommandURLList/leading",
		key:   "List-Unsubscribe",
		value: func(c string) string { return c + " <mailto:u@example.org>" },
		call: func(h *mail.Header) (bool, error) {
			var err error
			urls, err = h.ListCommandURLList("List-Unsubscribe")
			return len(urls) == 1 && urls[0] != nil && *urls[0] == *wantURL, err
		},
		last: func() any { return urls },
	}}
	shapes := []struct {
		name    string
		comment func(n int) string
	}{
		{"flat", func(n int) string { return "(" + strings.Repeat("a", n) + ")" }},
		{"nested", func(n int) string { return strings.Repeat("(", n/2) + strings.Repeat(")", n/2) }},
		{"escaped", func(n int) string { return "(" + strings.Repeat(`\)\(\\`, n/6) + ")" }},
		{"many", func(n int) string { return strings.Repeat("(a) ", n/4) }},
	}

	for _, a := range accessors {
		for _, shape := range shapes {
			t.Run(a.name+"/"+shape.name, func(t *testing.T) {
				for _, n := range append([]int{commentPreflightSize}, commentSizes...) {
					var h mail.Header
					h.Set(a.key, a.value(shape.comment(n)))
					var rec callRecord
					got := allocBytesPerOp(allocRuns, func() { rec.note(a.call(&h)) })
					rec.check(t, fmt.Sprintf("n=%d", n), allocRuns, a.last())
					if got > commentScanBudget {
						t.Fatalf("n=%d: allocated %d bytes per call (mean of %d), want at most %d whatever the comment size", n, got, allocRuns, commentScanBudget)
					}
				}
			})
		}
	}
}

// TestHeader_MessageID_issue9Repro pins the exact public reproduction from
// issue #9: the identifier after a 32 KiB comment stays usable. The same shape
// is measured first with a commentPreflightSize comment, on the same budget,
// so that a reintroduced quadratic copy fails there instead of being replayed
// allocRuns times at 32 KiB; this does not rely on any other test failing
// first. The 32 KiB measurement itself is unchanged.
func TestHeader_MessageID_issue9Repro(t *testing.T) {
	const wantID = "x@example.org"
	for _, n := range []int{commentPreflightSize, 32768} {
		var h mail.Header
		h.Set("Message-Id", "("+strings.Repeat("a", n)+") <x@example.org>")
		var (
			id  string
			rec callRecord
		)
		got := allocBytesPerOp(allocRuns, func() {
			var err error
			id, err = h.MessageID()
			rec.note(id == wantID, err)
		})
		t.Logf("MessageID() after a %d-byte comment allocated %d bytes per call (mean of %d)", n, got, allocRuns)
		rec.check(t, fmt.Sprintf("MessageID() after a %d-byte comment", n), allocRuns, id)
		if got > commentScanBudget {
			t.Fatalf("MessageID() after a %d-byte comment allocated %d bytes per call, want at most %d", n, got, commentScanBudget)
		}
	}
}

// TestHeader_commentsAroundIDsAndListCommands covers how comments, escapes and
// nesting are skipped around msg-ids and list commands, and what the existing
// accessors return when a comment is never closed. A partial list keeps the
// entries before the damage; the damaged entry itself is not reported as
// parsed, and the error is always returned with it.
func TestHeader_commentsAroundIDsAndListCommands(t *testing.T) {
	deep := strings.Repeat("(", 100000) + strings.Repeat(")", 100000)

	t.Run("MessageID", func(t *testing.T) {
		tests := []struct {
			raw     string
			want    string
			wantErr bool
		}{
			{"(c) <a@example.org>", "a@example.org", false},
			{"<a@example.org> (c)", "a@example.org", false},
			{"(c1) (c2)\t(c3)<a@example.org>(c4) (c5)", "a@example.org", false},
			{"(outer (inner (innermost)) tail) <a@example.org>", "a@example.org", false},
			{deep + " <a@example.org> " + deep, "a@example.org", false},
			{`(escaped \) paren) <a@example.org>`, "a@example.org", false},
			{`(escaped \( paren) <a@example.org>`, "a@example.org", false},
			{`(backslash \\) <a@example.org>`, "a@example.org", false},
			{`(\\\)\\) <a@example.org>`, "a@example.org", false},
			{`(<fake@example.org>) <a@example.org>`, "a@example.org", false},
			{`("quoted ) paren") <a@example.org>`, "", true},
			{"(unterminated <a@example.org>", "", true},
			{"((one close only) <a@example.org>", "", true},
			{`(trailing backslash \`, "", true},
			{`(escaped close \)`, "", true},
			{strings.Repeat("(", 100000) + " <a@example.org>", "", true},
			{"<a@example.org> (unterminated", "", true},
			{"<a@example.org> (", "", true},
			{"(", "", true},
			{")", "", true},
			{"<a@example.org> )", "a@example.org", false},
		}
		for _, test := range tests {
			var h mail.Header
			h.Set("Message-Id", test.raw)
			got, err := h.MessageID()
			if (err != nil) != test.wantErr || got != test.want {
				t.Errorf("MessageID(%.60q) = %q, %v; want %q, error %v", test.raw, got, err, test.want, test.wantErr)
			}
		}
	})

	t.Run("MsgIDList", func(t *testing.T) {
		tests := []struct {
			raw     string
			want    []string
			wantErr bool
		}{
			{"(c) <a@example.org> (c) <b@example.org> (c)", []string{"a@example.org", "b@example.org"}, false},
			{deep + "<a@example.org>" + deep + "<b@example.org>" + deep, []string{"a@example.org", "b@example.org"}, false},
			{`<a@example.org> (\)) <b@example.org>`, []string{"a@example.org", "b@example.org"}, false},
			// The comment that is never closed swallows <b@example.org>; only
			// the entry before it survives and the damage is reported.
			{"<a@example.org> <b@example.org> (unterminated <c@example.org>", []string{"a@example.org"}, true},
			{"<a@example.org> (unterminated <b@example.org>", nil, true},
			{"(unterminated <a@example.org>", nil, true},
		}
		for _, test := range tests {
			var h mail.Header
			h.Set("References", test.raw)
			got, err := h.MsgIDList("References")
			if (err != nil) != test.wantErr || !reflect.DeepEqual(got, test.want) {
				t.Errorf("MsgIDList(%.60q) = %q, %v; want %q, error %v", test.raw, got, err, test.want, test.wantErr)
			}
		}
	})

	t.Run("ListCommandURLList", func(t *testing.T) {
		tests := []struct {
			raw     string
			want    []string
			wantErr bool
		}{
			{"(c) <mailto:a@example.org> (c), (c) <https://example.org/u> (c)", []string{"mailto:a@example.org", "https://example.org/u"}, false},
			{deep + " <mailto:a@example.org> " + deep, []string{"mailto:a@example.org"}, false},
			{`(\(\)) NO (\\)`, []string{""}, false},
			{"<mailto:a@example.org>, <https://example.org/u> (unterminated", []string{"mailto:a@example.org"}, true},
			{"(unterminated <mailto:a@example.org>", nil, true},
		}
		for _, test := range tests {
			var h mail.Header
			h.Set("List-Post", test.raw)
			l, err := h.ListCommandURLList("List-Post")
			var got []string
			for _, u := range l {
				if u == nil {
					got = append(got, "")
				} else {
					got = append(got, u.String())
				}
			}
			if (err != nil) != test.wantErr || !reflect.DeepEqual(got, test.want) {
				t.Errorf("ListCommandURLList(%.60q) = %q, %v; want %q, error %v", test.raw, got, err, test.want, test.wantErr)
			}
		}
	})
}

// TestHeader_AddressList_commentsAndGroups pins what Header.AddressList,
// ParseAddress and ParseAddressList get from net/mail (Go 1.26), which they
// call unchanged.
//
// What net/mail guarantees for this item: it skips a comment iteratively with
// a depth counter and a strings.Builder, so any nesting depth is handled in
// linear time and memory with no recursion, and it does not nest groups — a
// group member is parsed with groups disabled, so the call depth stays fixed
// however many groups or members a field holds, and a group inside a group is
// refused. Nothing there needs a local depth bound.
//
// What it does not do, and this item leaves alone: an address list is
// all-or-nothing, so one damaged entry fails the whole list with no partial
// result, and a comment before an address ("(c) <a@example.org>") is refused
// even though RFC 5322 allows CFWS there. Those are recovery and conformance
// gaps rather than resource bounds; ROB-040-001 and ROB-040-004 own them, and
// the cases below record today's behavior so that work shows up as a change.
func TestHeader_AddressList_commentsAndGroups(t *testing.T) {
	deep := strings.Repeat("(", 100000) + strings.Repeat(")", 100000)
	a := &mail.Address{Address: "a@example.org"}
	b := &mail.Address{Address: "b@example.org"}

	tests := []struct {
		name    string
		raw     string
		want    []*mail.Address
		wantErr bool
	}{
		{"trailing comments", "a@example.org (c), <b@example.org> (c)", []*mail.Address{{Name: "c", Address: "a@example.org"}, b}, false},
		{"nested display-name comment", "a@example.org (x (y) z)", []*mail.Address{{Name: "x (y) z", Address: "a@example.org"}}, false},
		{"deep comment after angle-addr", "<a@example.org> " + deep + ", <b@example.org> " + deep, []*mail.Address{a, b}, false},
		{"escaped delimiters", `<a@example.org> (\)\(\\), <b@example.org>`, []*mail.Address{a, b}, false},
		{"group", "g: a@example.org, b@example.org;", []*mail.Address{a, b}, false},
		{"empty group", "g:;, a@example.org", []*mail.Address{a}, false},
		{"group with deep comments", "g: <a@example.org> " + deep + ";" + deep, []*mail.Address{a}, false},
		{"many groups", strings.Repeat("g: <a@example.org>;, ", 1000) + "<b@example.org>", append(repeatAddress(a, 1000), b), false},
		{"nested group refused", "outer: inner: a@example.org;;", nil, true},
		{"deeply nested group refused", strings.Repeat("g:", 100000) + " a@example.org" + strings.Repeat(";", 100000), nil, true},
		{"unterminated group", "g: a@example.org, b@example.org", nil, true},
		{"unterminated comment", "a@example.org, <b@example.org> (unterminated", nil, true},
		{"unterminated deep comment", "<a@example.org> " + strings.Repeat("(", 100000), nil, true},
		{"unterminated comment in group", "g: <a@example.org> (unterminated;", nil, true},
		// Refused by net/mail today although RFC 5322 allows them (ROB-040-004).
		{"leading comment refused", "(c) <a@example.org>", nil, true},
		{"leading deep comment refused", deep + " <a@example.org>", nil, true},
		{"leading comment in group refused", "g: (c) <a@example.org>;", nil, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var h mail.Header
			h.Set("To", test.raw)
			for name, parse := range map[string]func() ([]*mail.Address, error){
				"Header.AddressList": func() ([]*mail.Address, error) { return h.AddressList("To") },
				"ParseAddressList":   func() ([]*mail.Address, error) { return mail.ParseAddressList(test.raw) },
			} {
				got, err := parse()
				if (err != nil) != test.wantErr || !reflect.DeepEqual(got, test.want) {
					t.Errorf("%s = %v, %v; want %v, error %v", name, got, err, test.want, test.wantErr)
				}
			}
		})
	}

	// ParseAddress refuses a group of more than one address, or one nested in
	// another, and still skips a deep comment around a single mailbox.
	if got, err := mail.ParseAddress("<a@example.org> " + deep); err != nil || !reflect.DeepEqual(got, a) {
		t.Errorf("ParseAddress(deep comments) = %v, %v; want %v, nil", got, err, a)
	}
	if got, err := mail.ParseAddress("g: a@example.org;"); err != nil || !reflect.DeepEqual(got, a) {
		t.Errorf("ParseAddress(single-member group) = %v, %v; want %v, nil", got, err, a)
	}
	for _, raw := range []string{"g: a@example.org, b@example.org;", "outer: inner: a@example.org;;", strings.Repeat("(", 100000)} {
		if got, err := mail.ParseAddress(raw); err == nil {
			t.Errorf("ParseAddress(%.40q) = %v, nil; want an error", raw, got)
		}
	}
}

func repeatAddress(a *mail.Address, n int) []*mail.Address {
	l := make([]*mail.Address, n)
	for i := range l {
		l[i] = a
	}
	return l
}

// TestHeader_AddressList_allocationIsLinear checks that the work net/mail does
// for comments and groups grows linearly. It keeps a display-name comment as a
// string (that comment becomes Address.Name), so its cost is proportional to
// the input rather than constant; what must not happen is the quadratic growth
// the msg-id parser had, which would multiply the cost by 64 at eight times the
// size instead of by about 8.
func TestHeader_AddressList_allocationIsLinear(t *testing.T) {
	one := func(int) int { return 1 }
	shapes := []struct {
		name  string
		raw   func(n int) string
		count func(n int) int // how many addresses raw(n) holds, all a@example.org
	}{
		{"display-name comment", func(n int) string { return "a@example.org (" + strings.Repeat("a", n) + ")" }, one},
		{"nested display-name comment", func(n int) string {
			return "a@example.org " + strings.Repeat("(", n/2) + strings.Repeat(")", n/2)
		}, one},
		{"nested comment", func(n int) string {
			return "<a@example.org> " + strings.Repeat("(", n/2) + strings.Repeat(")", n/2)
		}, one},
		{"escaped comment", func(n int) string { return "<a@example.org> (" + strings.Repeat(`\)\(\\`, n/6) + ")" }, one},
		{"many comments", func(n int) string { return "<a@example.org>" + strings.Repeat(" (a)", n/4) }, one},
		{"group members", func(n int) string { return "g: " + strings.Repeat("<a@example.org>, ", n/17) + "<a@example.org>;" }, func(n int) int { return n/17 + 1 }},
		{"many groups", func(n int) string { return strings.Repeat("g: <a@example.org>;, ", n/21) + "<a@example.org>" }, func(n int) int { return n/21 + 1 }},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			var costs []uint64
			for _, n := range commentSizes {
				var h mail.Header
				h.Set("To", shape.raw(n))
				want := shape.count(n)
				var (
					l   []*mail.Address
					rec callRecord
				)
				costs = append(costs, allocBytesPerOp(allocRuns, func() {
					var err error
					l, err = h.AddressList("To")
					rec.note(addressesAre(l, want, "a@example.org"), err)
				}))
				rec.check(t, fmt.Sprintf("n=%d", n), allocRuns, fmt.Sprintf("%d addresses, want %d", len(l), want))
			}
			t.Logf("bytes allocated per call %v at sizes %v", costs, commentSizes)
			if ratio, factor, ok := allocGrowthIsLinear(commentSizes, costs); !ok {
				t.Errorf("allocation grew %.1fx for a %.0fx larger input (%v bytes at %v)", ratio, factor, costs, commentSizes)
			}
		})
	}
}

// addressesAre reports, without allocating, whether l holds exactly n
// addresses and each of them is addr.
func addressesAre(l []*mail.Address, n int, addr string) bool {
	if len(l) != n {
		return false
	}
	for _, a := range l {
		if a == nil || a.Address != addr {
			return false
		}
	}
	return true
}

// Package-level sinks for the measurement controls: what is stored here
// outlives the call, so the compiler must put it on the heap where TotalAlloc
// sees it.
var (
	byteSink    []byte
	commentSink string
	commentOK   bool
)

// preIssue9MaxInput caps the whole input of preIssue9ConsumeComment, comment,
// closing ')' and anything after it included.
const preIssue9MaxInput = 1 << 10

// preIssue9ConsumeComment is the comment loop the msg-id and list-command
// parsers used before issue #9 (headerParser.consumeComment, replaced in
// 01cfdf5), replayed on a plain string whose opening '(' is already consumed.
// It collects the comment with "comment += byte", copying everything collected
// so far for each byte, so n bytes cost about n²/2 bytes of garbage.
//
// It exists only as a positive control for the allocation checks. Its whole
// input is capped at preIssue9MaxInput bytes, checked before anything is
// copied, so one call costs at most about 0.5 MiB of garbage; the fixtures
// stay at or under 512 comment bytes (about 130 KiB per call). An input over
// the cap, such as one of commentSizes, where one call would cost up to
// 0.5 GiB, panics instead of being copied.
func preIssue9ConsumeComment(s string) (comment string, ok bool) {
	if len(s) > preIssue9MaxInput {
		panic(fmt.Sprintf("preIssue9ConsumeComment: %d-byte input over the %d-byte cap", len(s), preIssue9MaxInput))
	}
	depth := 1
	for len(s) > 0 && depth > 0 {
		if s[0] == '\\' && len(s) > 1 {
			s = s[1:]
		} else if s[0] == '(' {
			depth++
		} else if s[0] == ')' {
			depth--
		}
		if depth > 0 {
			comment += s[:1]
		}
		s = s[1:]
	}
	return comment, depth == 0
}

// TestAllocBytesPerOp_controls checks the measurement and both gates the
// regression tests above use against finite allocations of known shape: the
// averaging must still charge a per-call cost over commentScanBudget, and the
// pre-#9 comment copy, in full, while a cost that is only over budget summed
// over all allocRuns calls, or that grows linearly, must pass. Every payload
// is small and fixed; nothing here runs on commentSizes, and the pre-#9 copy
// refuses any input over preIssue9MaxInput.
func TestAllocBytesPerOp_controls(t *testing.T) {
	const quadraticComment = 256 // about 33 KiB of garbage per call
	flat := func(n int) string { return strings.Repeat("a", n) + ") <x@example.org>" }

	t.Run("budget", func(t *testing.T) {
		// Built outside the measured calls, so only the copy is charged.
		comment := flat(quadraticComment)
		tests := []struct {
			name     string
			f        func()
			atLeast  uint64 // what one call allocates at the very least
			wantOver bool
		}{
			{"nothing", func() {}, 0, false},
			// Over budget only summed over the runs, never per call.
			{"1 KiB per call", func() { byteSink = make([]byte, 1<<10) }, 1 << 10, false},
			{"one byte over budget per call", func() { byteSink = make([]byte, commentScanBudget+1) }, commentScanBudget + 1, true},
			{"pre-#9 comment copy", func() { commentSink, commentOK = preIssue9ConsumeComment(comment) }, commentScanBudget + 1, true},
		}
		for _, test := range tests {
			got := allocBytesPerOp(allocRuns, test.f)
			t.Logf("%s: %d bytes per call", test.name, got)
			if got < test.atLeast {
				t.Errorf("%s: measured %d bytes per call, want at least %d", test.name, got, test.atLeast)
			}
			if over := got > commentScanBudget; over != test.wantOver {
				t.Errorf("%s: %d bytes per call over the %d budget = %v, want %v", test.name, got, commentScanBudget, over, test.wantOver)
			}
		}
		if commentSink != strings.Repeat("a", quadraticComment) || !commentOK {
			t.Errorf("preIssue9ConsumeComment collected %d bytes, closed %v; want the %d-byte comment, closed", len(commentSink), commentOK, quadraticComment)
		}
	})

	t.Run("replica cap", func(t *testing.T) {
		// One byte over the cap, not commentSizes: if the cap were lost, this
		// would still copy only about 0.5 MiB.
		over := strings.Repeat("a", preIssue9MaxInput) + ")"
		defer func() {
			if recover() == nil {
				t.Errorf("preIssue9ConsumeComment accepted a %d-byte input over the %d-byte cap", len(over), preIssue9MaxInput)
			}
		}()
		preIssue9ConsumeComment(over)
	})

	t.Run("growth", func(t *testing.T) {
		// The same 8x span as commentSizes, scaled down to keep the pre-#9
		// copy at most 512 bytes.
		sizes := []int{64, 128, 256, 512}
		tests := []struct {
			name       string
			f          func(n int) func()
			wantLinear bool
		}{
			{"64 bytes per input byte", func(n int) func() {
				return func() { byteSink = make([]byte, 64*n) }
			}, true},
			{"pre-#9 comment copy", func(n int) func() {
				s := flat(n)
				return func() { commentSink, commentOK = preIssue9ConsumeComment(s) }
			}, false},
		}
		for _, test := range tests {
			var costs []uint64
			for _, n := range sizes {
				costs = append(costs, allocBytesPerOp(allocRuns, test.f(n)))
			}
			ratio, factor, linear := allocGrowthIsLinear(sizes, costs)
			t.Logf("%s: %v bytes per call at sizes %v, grew %.1fx", test.name, costs, sizes, ratio)
			if linear != test.wantLinear {
				t.Errorf("%s: growth of %.1fx over a %.0fx span judged linear = %v, want %v", test.name, ratio, factor, linear, test.wantLinear)
			}
		}
		if commentSink != strings.Repeat("a", sizes[len(sizes)-1]) || !commentOK {
			t.Errorf("preIssue9ConsumeComment collected %d bytes, closed %v; want the %d-byte comment, closed", len(commentSink), commentOK, sizes[len(sizes)-1])
		}
	})
}

// TestHeader_commentAccessorsKeepRawHeader checks that reading comments,
// msg-ids, list commands and address groups out of a parsed header leaves the
// raw field bytes untouched, so the header still writes back byte for byte.
func TestHeader_commentAccessorsKeepRawHeader(t *testing.T) {
	deep := strings.Repeat("(", 2000) + strings.Repeat(")", 2000)
	raw := "Message-Id: (c\\)) " + deep + "\r\n <a@example.org> (unterminated\r\n" +
		"References: <a@example.org> (c) <b@example.org>\r\n" +
		"List-Post: (c) NO (c)\r\n" +
		"List-Help: " + deep + " <mailto:h@example.org> (\r\n" +
		"To: g: (c) <a@example.org>, " + deep + " b@example.org;, outer: inner: c@example.org;;\r\n" +
		"\r\n"
	th, err := textproto.ReadHeader(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	h := mail.Header{Header: message.Header{Header: th}}

	if _, err := h.MessageID(); err == nil {
		t.Error("MessageID: want an error for the unterminated trailing comment")
	}
	if got, err := h.MsgIDList("References"); err != nil || !reflect.DeepEqual(got, []string{"a@example.org", "b@example.org"}) {
		t.Errorf("MsgIDList = %q, %v", got, err)
	}
	if got, err := h.ListCommandURLList("List-Post"); err != nil || len(got) != 1 || got[0] != nil {
		t.Errorf("ListCommandURLList(List-Post) = %v, %v", got, err)
	}
	if _, err := h.ListCommandURLList("List-Help"); err == nil {
		t.Error("ListCommandURLList(List-Help): want an error for the unterminated comment")
	}
	if _, err := h.AddressList("To"); err == nil {
		t.Error("AddressList: want an error for the nested group")
	}

	var out bytes.Buffer
	if err := textproto.WriteHeader(&out, h.Header.Header); err != nil {
		t.Fatal(err)
	}
	if out.String() != raw {
		t.Errorf("header was not written back verbatim:\n got %q\nwant %q", out.String(), raw)
	}
}

// FuzzHeaderCommentAccessors runs every accessor that skips comments over an
// arbitrary field value: none may panic, and reading the parsed header must
// leave it writing back byte for byte. It is a focused target for this item,
// not the general header fuzzing of ROB-130.
func FuzzHeaderCommentAccessors(f *testing.F) {
	for _, s := range []string{
		"<a@example.org>",
		"(c) <a@example.org> (c)",
		`(\)\(\\) <a@example.org>`,
		"(unterminated <a@example.org>",
		"((()) <a@example.org>",
		`(\`,
		"<a@example.org> (c) <b@example.org>",
		"(c) NO (c)",
		"<mailto:a@example.org>, (c) <https://example.org/u> (",
		"g: (c) <a@example.org>, b@example.org (x (y) z);",
		"outer: inner: a@example.org;;",
		strings.Repeat("(", 512) + strings.Repeat(")", 512) + " <a@example.org>",
	} {
		f.Add(s)
	}
	keys := []string{"Message-Id", "References", "List-Post", "To"}
	f.Fuzz(func(t *testing.T, value string) {
		if strings.ContainsAny(value, "\r\n") {
			// Line breaks reframe the header; that is ROB-010/ROB-020 ground,
			// not the comment scanner's.
			return
		}
		var raw strings.Builder
		for _, k := range keys {
			raw.WriteString(k)
			raw.WriteString(": ")
			raw.WriteString(value)
			raw.WriteString("\r\n")
		}
		raw.WriteString("\r\n")
		th, err := textproto.ReadHeader(bufio.NewReader(strings.NewReader(raw.String())))
		if err != nil {
			return // not a single well-framed field; header framing is not this target's concern
		}
		h := mail.Header{Header: message.Header{Header: th}}

		h.MessageID()
		h.MsgIDList("References")
		h.ListCommandURLList("List-Post")
		h.AddressList("To")

		var out bytes.Buffer
		if err := textproto.WriteHeader(&out, h.Header.Header); err != nil {
			t.Fatal(err)
		}
		if out.String() != raw.String() {
			t.Errorf("header was not written back verbatim:\n got %q\nwant %q", out.String(), raw.String())
		}
	})
}
