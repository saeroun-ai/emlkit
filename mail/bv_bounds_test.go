package mail_test

import (
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// Boundary tests of the tolerant accessors (ROB-120-001; see
// docs/robustness-notes/bounds-audit-values.md). They hold on a 32-bit int as
// on a 64-bit one: run them with GOARCH=386 too.

const bvMaxEntries, bvMaxDiagnostics = 16384, 64

// TestBVAddressList_entryBoundInOneRun: addresses that no comma separates are
// read from one run of words, and the bound on the entries of a result holds
// inside that run as it does between runs.
func TestBVAddressList_entryBoundInOneRun(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		entries int
		limited bool
	}{
		{"at the bound", strings.Repeat("a@b ", bvMaxEntries), bvMaxEntries, false},
		{"one past it", strings.Repeat("a@b ", bvMaxEntries+1), bvMaxEntries, true},
		{"far past it", strings.Repeat("a@b ", 5*bvMaxEntries), bvMaxEntries, true},
		{"and another run", strings.Repeat("a@b ", bvMaxEntries+1) + ", c@d, e@f", bvMaxEntries, true},
		{"a run after separated ones", "x@y, " + strings.Repeat("a@b ", bvMaxEntries), bvMaxEntries, true},
	} {
		r := mail.ParseAddressListTolerant(tc.value, nil)
		if len(r.Entries) != tc.entries || len(r.Addresses) != tc.entries {
			t.Errorf("%s: %d entries and %d addresses, want %d", tc.name, len(r.Entries), len(r.Addresses), tc.entries)
		}
		if len(r.Diagnostics) > bvMaxDiagnostics {
			t.Errorf("%s: %d diagnostics", tc.name, len(r.Diagnostics))
		}
		limits := 0
		for _, d := range r.Diagnostics {
			if d.Code == message.CodeAddressPartialList && d.Severity == message.SeverityLimitExceeded {
				limits++
			}
		}
		// The bound is reported once, however many places notice it.
		if want := map[bool]int{true: 1}[tc.limited]; limits != want {
			t.Errorf("%s: %d limit diagnostics, want %d", tc.name, limits, want)
		}
	}
}

// TestBVMsgIDList_entryBoundWithBareWords: the identifiers without angle
// brackets, which are read when no bracketed one is usable, share the bound
// with the entries they are appended to.
func TestBVMsgIDList_entryBoundWithBareWords(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		ids   int
	}{
		{"unusable entries, then bare words", strings.Repeat("<> ", bvMaxEntries) + strings.Repeat("a@b ", 100), 0},
		{"a few unusable entries", strings.Repeat("<> ", 10) + strings.Repeat("a@b ", bvMaxEntries), bvMaxEntries - 10},
		{"bare words only", strings.Repeat("a@b ", 2*bvMaxEntries), bvMaxEntries},
	} {
		r := mail.ParseMsgIDListTolerant(tc.value)
		if len(r.Entries) != bvMaxEntries || len(r.IDs) != tc.ids {
			t.Errorf("%s: %d entries and %d identifiers, want %d and %d", tc.name, len(r.Entries), len(r.IDs), bvMaxEntries, tc.ids)
		}
		if len(r.Diagnostics) > bvMaxDiagnostics {
			t.Errorf("%s: %d diagnostics", tc.name, len(r.Diagnostics))
		}
	}
}

// TestBVDate_numericBoundaries: every number of a date at and past the end of
// its range. A number too wide for its field is no number, so none is ever
// wide enough to overflow.
func TestBVDate_numericBoundaries(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
		unix  int64
	}{
		{"1 Jan 1970 00:00:00 +0000", true, 0},
		{"1 Jan 1970 00:00:00 -2359", true, 23*3600 + 59*60},
		{"1 Jan 1970 00:00:00 +2359", true, -(23*3600 + 59*60)},
		{"31 Dec 1969 23:59:60 +0000", true, 0},
		{"1 Jan 70 00:00 +0000", true, 0},
		{"1 Jan 1970 00:00:00 +2400", true, 0},
		{"1 Jan 1970 00:00:00 +99999999999999999999", true, 0},
		{"1 Jan 1970 00:00:00.99999999999999999999 +0000", true, 0},
		{"1970-01-01T00:00:00Z", true, 0},
		{"19 Jan 2038 03:14:08 +0000", true, 1 << 31},
		{"31 Dec 9999 23:59:59 +0000", true, 253402300799},
		{"1 Jan 0000 00:00 +0000", false, 0},
		{"1 Jan 10000 00:00 +0000", false, 0},
		{"1 Jan 99999999999999999999 00:00 +0000", false, 0},
		{"99999999999999999999 Jan 1970 00:00 +0000", false, 0},
		{"0 Jan 1970 00:00 +0000", false, 0},
		{"32 Jan 1970 00:00 +0000", false, 0},
		{"30 Feb 1970 00:00 +0000", false, 0},
		{"1 Jan 1970 24:00 +0000", false, 0},
		{"1 Jan 1970 00:60 +0000", false, 0},
		{"1 Jan 1970 00:00:61 +0000", false, 0},
		// Three digits are no hour, so no time is read and the text is ignored.
		{"1 Jan 1970 999:00 +0000", true, 0},
		{"1970-13-01", false, 0},
		{"1970-00-01", false, 0},
		{"1970-01-", false, 0},
		{"0-0-0", false, 0},
		{"--", false, 0},
		{"-", false, 0},
		{"+", false, 0},
		{"T", false, 0},
		{":", false, 0},
	} {
		r := mail.ParseDateTolerant(tc.value)
		if r.Valid != tc.valid || r.Valid && r.Time.Unix() != tc.unix {
			t.Errorf("ParseDateTolerant(%q): valid %v at %d, want %v at %d", tc.value, r.Valid, r.Time.Unix(), tc.valid, tc.unix)
		}
		if len(r.Diagnostics) > bvMaxDiagnostics {
			t.Errorf("ParseDateTolerant(%q): %d diagnostics", tc.value, len(r.Diagnostics))
		}
	}
}

// TestBVTolerantAccessors_floods: a value longer than the default budget of
// one header field, made of one construct repeated, through every tolerant
// accessor. None may panic, and each keeps the bounds it documents on what it
// returns, whatever the construct.
func TestBVTolerantAccessors_floods(t *testing.T) {
	units := []string{
		"a@b ", "a@b,", "<a@b>", "<a@b> ", "<> ", "<", ">", "a ", "a,", "x:", ";", ",", "@", "\"", "\\", "(", ")", "()", "(c) ",
		"[", "]", "=?", "?=", "from x ", "by ", "id", "<u:1> ", "<u:1>,", "u:1 ", "x=y ", "x=y;", "x.y=z ", "=", "\xff", "\x00", "\r\n ",
		"1 ", "1:", "+", "-", "Jan ",
	}
	for _, unit := range units {
		v := strings.Repeat(unit, (70<<10)/len(unit))
		for _, v := range []string{v, "s.example; spf=pass " + v} {
			bound := func(name string, entries, diags int) {
				t.Helper()
				if entries > bvMaxEntries || diags > bvMaxDiagnostics {
					t.Errorf("%s over %q: %d entries, %d diagnostics", name, unit, entries, diags)
				}
			}
			for _, opts := range []*mail.AddressOptions{nil, {StrictCommas: true, RequireDomain: true}} {
				a := mail.ParseAddressListTolerant(v, opts)
				bound("ParseAddressListTolerant", len(a.Entries), len(a.Diagnostics))
			}
			m := mail.ParseMsgIDListTolerant(v)
			bound("ParseMsgIDListTolerant", len(m.Entries), len(m.Diagnostics))
			l := mail.ParseListURLs(v)
			bound("ParseListURLs", len(l.Entries), len(l.Diagnostics))
			r := mail.ParseReceived(v)
			bound("ParseReceived", len(r.Clauses), len(r.Diagnostics))
			bound("ParseListID", 0, len(mail.ParseListID(v).Diagnostics))
			bound("ParseDateTolerant", 0, len(mail.ParseDateTolerant(v).Diagnostics))

			a := mail.ParseAuthenticationResults(v)
			bound("ParseAuthenticationResults", 0, len(a.Diagnostics))
			if len(a.Methods) > 64 {
				t.Errorf("ParseAuthenticationResults over %q: %d methods", unit, len(a.Methods))
			}
			for _, m := range a.Methods {
				if len(m.Props) > 256 {
					t.Errorf("ParseAuthenticationResults over %q: %d properties of one method", unit, len(m.Props))
				}
			}
		}
	}
}
