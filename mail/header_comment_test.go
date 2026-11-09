package mail_test

import (
	"bufio"
	"bytes"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
	"github.com/saeroun-ai/emlkit/textproto"
)

// cumulativeAllocBytes reports the bytes f allocated on the heap, cumulative
// over its whole run (runtime.MemStats.TotalAlloc). Bytes freed by a collection
// during f still count, so an allocator that builds and drops a string per
// input byte is measured at its full cost, not at its live size.
func cumulativeAllocBytes(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// commentSizes are the payload sizes N, 2N, 4N, 8N the allocation tests step
// through. The largest is the 32 KiB public reproduction of issue #9.
var commentSizes = []int{4 << 10, 8 << 10, 16 << 10, 32 << 10}

// commentScanBudget is the most a skipped comment may cost, whatever its size.
// The scan itself allocates nothing; the budget leaves room for the result
// values the accessors return (a msg-id string, a list slice, a parsed URL).
const commentScanBudget = 4 << 10

// TestHeader_commentScanAllocationIsConstant is the issue #9 regression. The
// msg-id and list-command accessors used to collect every skipped comment with
// "comment += byte", which copies the whole comment so far for each byte: a
// comment of n bytes cost about n²/2 bytes of garbage, so a 32 KiB comment in
// front of a Message-Id allocated about 0.5 GiB. A skipped comment is never
// used, so it is now scanned in place, and what these accessors allocate no
// longer depends on the comment's size at all.
func TestHeader_commentScanAllocationIsConstant(t *testing.T) {
	type accessor struct {
		name  string
		key   string
		value func(comment string) string
		call  func(h *mail.Header) error
	}
	accessors := []accessor{{
		name:  "MessageID/leading",
		key:   "Message-Id",
		value: func(c string) string { return c + " <x@example.org>" },
		call: func(h *mail.Header) error {
			id, err := h.MessageID()
			if err == nil && id != "x@example.org" {
				t.Errorf("MessageID() = %q, want %q", id, "x@example.org")
			}
			return err
		},
	}, {
		name:  "MessageID/trailing",
		key:   "Message-Id",
		value: func(c string) string { return "<x@example.org> " + c },
		call: func(h *mail.Header) error {
			_, err := h.MessageID()
			return err
		},
	}, {
		name:  "MsgIDList/between",
		key:   "References",
		value: func(c string) string { return "<a@example.org> " + c + " <b@example.org>" },
		call: func(h *mail.Header) error {
			l, err := h.MsgIDList("References")
			if err == nil && !reflect.DeepEqual(l, []string{"a@example.org", "b@example.org"}) {
				t.Errorf("MsgIDList() = %q", l)
			}
			return err
		},
	}, {
		name:  "ListCommandURLList/leading",
		key:   "List-Unsubscribe",
		value: func(c string) string { return c + " <mailto:u@example.org>" },
		call: func(h *mail.Header) error {
			l, err := h.ListCommandURLList("List-Unsubscribe")
			if err == nil && (len(l) != 1 || l[0].String() != "mailto:u@example.org") {
				t.Errorf("ListCommandURLList() = %v", l)
			}
			return err
		},
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
				for _, n := range commentSizes {
					var h mail.Header
					h.Set(a.key, a.value(shape.comment(n)))
					var err error
					got := cumulativeAllocBytes(func() { err = a.call(&h) })
					if err != nil {
						t.Fatalf("n=%d: unexpected error: %v", n, err)
					}
					if got > commentScanBudget {
						t.Errorf("n=%d: allocated %d bytes, want at most %d whatever the comment size", n, got, commentScanBudget)
					}
				}
			})
		}
	}
}

// TestHeader_MessageID_issue9Repro pins the exact public reproduction from
// issue #9: the identifier after a 32 KiB comment stays usable.
func TestHeader_MessageID_issue9Repro(t *testing.T) {
	var h mail.Header
	h.Set("Message-Id", "("+strings.Repeat("a", 32768)+") <x@example.org>")
	var (
		id  string
		err error
	)
	got := cumulativeAllocBytes(func() { id, err = h.MessageID() })
	t.Logf("MessageID() after a 32 KiB comment allocated %d bytes", got)
	if err != nil || id != "x@example.org" {
		t.Fatalf("MessageID() = %q, %v; want %q, nil", id, err, "x@example.org")
	}
	if got > commentScanBudget {
		t.Errorf("MessageID() allocated %d bytes, want at most %d", got, commentScanBudget)
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
	shapes := []struct {
		name string
		raw  func(n int) string
	}{
		{"display-name comment", func(n int) string { return "a@example.org (" + strings.Repeat("a", n) + ")" }},
		{"nested display-name comment", func(n int) string {
			return "a@example.org " + strings.Repeat("(", n/2) + strings.Repeat(")", n/2)
		}},
		{"nested comment", func(n int) string {
			return "<a@example.org> " + strings.Repeat("(", n/2) + strings.Repeat(")", n/2)
		}},
		{"escaped comment", func(n int) string { return "<a@example.org> (" + strings.Repeat(`\)\(\\`, n/6) + ")" }},
		{"many comments", func(n int) string { return "<a@example.org>" + strings.Repeat(" (a)", n/4) }},
		{"group members", func(n int) string { return "g: " + strings.Repeat("<a@example.org>, ", n/17) + "<a@example.org>;" }},
		{"many groups", func(n int) string { return strings.Repeat("g: <a@example.org>;, ", n/21) + "<a@example.org>" }},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			var costs []uint64
			for _, n := range commentSizes {
				var h mail.Header
				h.Set("To", shape.raw(n))
				var err error
				costs = append(costs, cumulativeAllocBytes(func() { _, err = h.AddressList("To") }))
				if err != nil {
					t.Fatalf("n=%d: unexpected error: %v", n, err)
				}
			}
			// Over the 8x span of sizes, linear cost grows about 8x (up to
			// about 12x where buffer doubling straddles a power of two);
			// quadratic cost grows 64x. Allow up to 3x linear.
			t.Logf("bytes allocated %v at sizes %v", costs, commentSizes)
			first, last := costs[0], costs[len(costs)-1]
			ratio := float64(last) / float64(max(first, 1))
			factor := float64(commentSizes[len(commentSizes)-1]) / float64(commentSizes[0])
			if ratio > factor*3 {
				t.Errorf("allocation grew %.1fx for a %.0fx larger input (%v bytes at %v)", ratio, factor, costs, commentSizes)
			}
		})
	}
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
