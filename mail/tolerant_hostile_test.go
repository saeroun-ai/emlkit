package mail_test

import (
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// hostileValues are field values chosen to hit the edges of the tolerant
// scanners: constructs that open and never close, closers without openers,
// escapes at the end of the value, empty and repeated delimiters, and bytes
// that are not text.
var hostileValues = []string{
	"",
	" ",
	"\t\r\n",
	"\\",
	`\\`,
	`"`,
	`"\`,
	`"\"`,
	"(",
	")",
	"(\\",
	"()",
	"(()",
	"<",
	">",
	"<>",
	"><",
	"<<>>",
	"[",
	"]",
	"[]",
	"@",
	"@@",
	"a@",
	"@a",
	",",
	",,,",
	";",
	";;;",
	":",
	"::",
	"a:",
	":a",
	"a:;",
	"a:b:c:;;;",
	"=?",
	"=??",
	"=???",
	"=????=",
	"=?utf-8?q??=",
	"=?utf-8?b?!!!!?=",
	"=?x?x?x?= <a@b>",
	"<@>",
	"<@:>",
	"<@a,@b:>",
	"<@a,@b:c@d",
	"<a@b",
	"a@b>",
	"<a@[>",
	"a@[",
	"a@[]",
	"a@[\\]",
	`"a"@`,
	`""@b`,
	`"" <a@b>`,
	"a b c d e f g",
	"a, b, c <",
	"a, b, c <>",
	"a, b, c <d",
	"\x00",
	"\x00\x00<\x00@\x00>",
	"\xff\xfe\xfd",
	"\xff<\xff@\xff>",
	"a@b\xff, \xff@c",
	"\xe2\x80\xa8\xe2\x80\xa9\xef\xbb\xbf",
	"NO",
	"NO NO NO",
	"<NO>",
	"+",
	"-",
	"+0900",
	"--",
	"---",
	"1-2-3",
	"0000-00-00T00:00:00Z",
	"9999-99-99",
	"1 Jan",
	"Jan 1",
	"1 Jan 1",
	"1 Jan 99999999999999999999 00:00",
	"99999999999999999999 Jan 2020",
	"1 Jan 2020 99999999999999999999:00",
	"1 Jan 2020 00:00 +99999999999999999999",
	"1 Jan 2020 00:00:00.",
	"1 Jan 2020 0:0",
	"1 Jan 2020 00:00 GMT+",
	"1 Jan 2020 00:00 GMT-",
	"1 Jan 2020 00:00 +",
	"Mon",
	"Mon,",
	"Mon Mon Mon Mon Mon",
	"T",
	"1997-11-20T",
	"1997-11-20Tx",
	"from",
	"from;",
	"; ;",
	";;;;from by via with id for",
	"from by via with id for",
	"FROM ( BY",
	"from a by b; (",
	`from "a by b; 1 Jan 2020 00:00 +0000`,
	"<%>",
	"<%zz>",
	"<http://[::1>",
	"<:>",
	"<a:>",
	"a:b",
	"<mailto:>",
	"<mailto:a@b",
	strings.Repeat("(", 300),
	strings.Repeat(")", 300),
	strings.Repeat("<", 300),
	strings.Repeat(">", 300),
	strings.Repeat(`"`, 301),
	strings.Repeat(`\`, 301),
	strings.Repeat("a@b,", 300),
	strings.Repeat("g:", 300),
	strings.Repeat(";", 300),
	strings.Repeat("=?", 300),
	strings.Repeat("\xff", 300),
	strings.Repeat("\x00", 300),
}

// TestTolerantAccessors_arbitraryBytes runs every tolerant accessor over
// hostile values, over every string of up to three bytes drawn from the
// characters the scanners branch on, and over a fixed pseudo-random series of
// longer mixtures. None may panic, and every result must keep its invariants:
// the raw value, raw entries that are parts of the value, and the bounds.
func TestTolerantAccessors_arbitraryBytes(t *testing.T) {
	for _, v := range hostileValues {
		checkTolerantInvariants(t, v)
	}

	alphabet := []byte("<>()[]\",;:@\\ .=?-+aT0\x00\xff\n")
	buf := make([]byte, 0, 3)
	var generate func(depth int)
	generate = func(depth int) {
		checkTolerantInvariants(t, string(buf))
		if depth == 3 {
			return
		}
		for _, c := range alphabet {
			buf = append(buf, c)
			generate(depth + 1)
			buf = buf[:len(buf)-1]
		}
	}
	generate(0)

	fragments := append([]string{
		"a@example.org", "<b@example.org>", "Name", `"quoted, name"`, "(comment)", "g:", ";", ",", " ",
		"<", ">", "(", ")", `"`, "@", ":", "\\", "[1.2.3.4]", "=?utf-8?q?=C3=A9?=", "\xe9", "\x00",
		"Mon,", "22", "Jul", "2019", "13:57:29", "-0500", "EST", "+09:00", "PM",
		"from", "by", "with", "id", "for", "ESMTP", "NO", "mailto:u@example.org", "https://example.org/?a=1,2", "%zz",
	}, hostileValues[:40]...)
	seed := uint32(20261110)
	next := func(n int) int {
		// A fixed linear congruential series: the test is deterministic.
		seed = seed*1664525 + 1013904223
		return int(seed>>8) % n
	}
	for i := 0; i < 4000; i++ {
		var b strings.Builder
		for n := 1 + next(14); n > 0; n-- {
			b.WriteString(fragments[next(len(fragments))])
			if next(3) == 0 {
				b.WriteByte(' ')
			}
		}
		checkTolerantInvariants(t, b.String())
	}
}

func checkTolerantInvariants(t *testing.T, v string) {
	t.Helper()
	const maxDiagnostics, maxEntries = 64, 16384
	checkDiags := func(name string, diags []message.Diagnostic) {
		t.Helper()
		if len(diags) > maxDiagnostics {
			t.Errorf("%s(%q): %d diagnostics", name, v, len(diags))
		}
		for _, d := range diags {
			if d.Code == "" || d.Message == "" {
				t.Errorf("%s(%q): incomplete diagnostic %+v", name, v, d)
			}
		}
	}
	part := func(name, raw string) {
		t.Helper()
		if raw == "" || !strings.Contains(v, raw) {
			t.Errorf("%s(%q): raw text %q is not a part of the value", name, v, raw)
		}
	}

	for _, opts := range []*mail.AddressOptions{nil, {StrictCommas: true, RequireDomain: true}} {
		a := mail.ParseAddressListTolerant(v, opts)
		checkDiags("ParseAddressListTolerant", a.Diagnostics)
		usable := 0
		for _, e := range a.Entries {
			part("ParseAddressListTolerant", e.Raw)
			if e.Address != nil {
				usable++
				if e.Address.Address == "" {
					t.Errorf("ParseAddressListTolerant(%q): empty address for %q", v, e.Raw)
				}
				if opts != nil && !strings.Contains(e.Address.Address, "@") {
					t.Errorf("ParseAddressListTolerant(%q): RequireDomain kept %q", v, e.Address.Address)
				}
			} else if len(e.Diagnostics) == 0 && len(a.Diagnostics) < maxDiagnostics {
				t.Errorf("ParseAddressListTolerant(%q): invalid entry %q has no diagnostic", v, e.Raw)
			}
		}
		if a.Raw != v || usable != len(a.Addresses) || len(a.Entries) > maxEntries {
			t.Errorf("ParseAddressListTolerant(%q): raw %q, %d usable entries, %d addresses", v, a.Raw, usable, len(a.Addresses))
		}
		if opts == nil {
			// A list net/mail accepts gives the same addresses and nothing
			// to report beyond raw UTF-8.
			if strict, err := mail.ParseAddressList(v); err == nil {
				if len(strict) != len(a.Addresses) {
					t.Errorf("ParseAddressListTolerant(%q) = %d addresses, ParseAddressList %d", v, len(a.Addresses), len(strict))
				}
				for i := range min(len(strict), len(a.Addresses)) {
					if *strict[i] != *a.Addresses[i] {
						t.Errorf("ParseAddressListTolerant(%q)[%d] = %v, ParseAddressList %v", v, i, a.Addresses[i], strict[i])
					}
				}
				for _, d := range a.Diagnostics {
					if d.Code != message.CodeAddressRawUTF8 {
						t.Errorf("ParseAddressListTolerant(%q): %v for a list net/mail accepts", v, d)
					}
				}
			}
		}
	}

	d := mail.ParseDateTolerant(v)
	checkDiags("ParseDateTolerant", d.Diagnostics)
	if d.Raw != v || (!d.Valid && !d.Time.IsZero()) {
		t.Errorf("ParseDateTolerant(%q): raw %q, valid %v, time %v", v, d.Raw, d.Valid, d.Time)
	}
	if !d.Valid && len(d.Diagnostics) != 1 {
		t.Errorf("ParseDateTolerant(%q): invalid date with diagnostics %v", v, d.Diagnostics)
	}

	m := mail.ParseMsgIDListTolerant(v)
	checkDiags("ParseMsgIDListTolerant", m.Diagnostics)
	ids := 0
	for _, e := range m.Entries {
		part("ParseMsgIDListTolerant", e.Raw)
		if e.ID != "" {
			ids++
		}
	}
	if m.Raw != v || ids != len(m.IDs) || len(m.Entries) > maxEntries {
		t.Errorf("ParseMsgIDListTolerant(%q): raw %q, %d usable entries, %d identifiers", v, m.Raw, ids, len(m.IDs))
	}

	r := mail.ParseReceived(v)
	checkDiags("ParseReceived", r.Diagnostics)
	if r.Raw != v || len(r.Clauses) > maxEntries {
		t.Errorf("ParseReceived(%q): raw %q, %d clauses", v, r.Raw, len(r.Clauses))
	}
	for _, c := range r.Clauses {
		part("ParseReceived", c.Raw)
	}
	if len(r.Clauses) == 0 && !r.HasDate && len(r.Diagnostics) == 0 {
		t.Errorf("ParseReceived(%q): nothing read and nothing reported", v)
	}

	l := mail.ParseListURLs(v)
	checkDiags("ParseListURLs", l.Diagnostics)
	urls := 0
	for _, e := range l.Entries {
		part("ParseListURLs", e.Raw)
		if e.URL != nil {
			urls++
		}
	}
	if l.Raw != v || urls != len(l.URLs) || len(l.Entries) > maxEntries {
		t.Errorf("ParseListURLs(%q): raw %q, %d parsed entries, %d URLs", v, l.Raw, urls, len(l.URLs))
	}

	id := mail.ParseListID(v)
	checkDiags("ParseListID", id.Diagnostics)
	if id.Raw != v || (id.ID == "" && len(id.Diagnostics) == 0) {
		t.Errorf("ParseListID(%q): raw %q, id %q, %v", v, id.Raw, id.ID, id.Diagnostics)
	}

	// The header accessors read the same value and must agree with the
	// functions; the strict accessors must still not panic on it.
	if v == "" {
		return
	}
	var h mail.Header
	for _, k := range []string{"To", "Date", "Message-Id", "References", "Received", "List-Post", "List-Id"} {
		h.Set(k, v)
	}
	if got := h.AddressListTolerant("To", nil); len(got.Entries) != len(mail.ParseAddressListTolerant(v, nil).Entries) {
		t.Errorf("AddressListTolerant(%q) differs from ParseAddressListTolerant", v)
	}
	if got := h.DateTolerant(); got.Valid != d.Valid || !got.Time.Equal(d.Time) {
		t.Errorf("DateTolerant(%q) differs from ParseDateTolerant", v)
	}
	if got := h.MsgIDListTolerant("References"); len(got.IDs) != len(m.IDs) {
		t.Errorf("MsgIDListTolerant(%q) differs from ParseMsgIDListTolerant", v)
	}
	if got := h.MessageIDTolerant(); got.Raw != v || len(got.Diagnostics) > maxDiagnostics {
		t.Errorf("MessageIDTolerant(%q): raw %q", v, got.Raw)
	}
	if got := h.Received(); len(got) != 1 || len(got[0].Clauses) != len(r.Clauses) {
		t.Errorf("Received(%q) differs from ParseReceived", v)
	}
	if got := h.ListURLs("List-Post"); len(got.URLs) != len(l.URLs) || got.No != l.No {
		t.Errorf("ListURLs(%q) differs from ParseListURLs", v)
	}
	if got := h.ListID(); got.ID != id.ID {
		t.Errorf("ListID(%q) differs from ParseListID", v)
	}
	h.AddressList("To")
	h.Date()
	h.MessageID()
	h.MsgIDList("References")
	h.ListCommandURLList("List-Post")

	// A field MsgIDList accepts gives the same identifiers.
	if strict, err := h.MsgIDList("References"); err == nil {
		if len(strict) != len(m.IDs) {
			t.Errorf("ParseMsgIDListTolerant(%q) = %q, MsgIDList %q", v, m.IDs, strict)
		}
		for i := range min(len(strict), len(m.IDs)) {
			if strict[i] != m.IDs[i] {
				t.Errorf("ParseMsgIDListTolerant(%q) = %q, MsgIDList %q", v, m.IDs, strict)
			}
		}
	}
}
