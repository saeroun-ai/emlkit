package mail_test

import (
	netmail "net/mail"
	"strings"
	"testing"
	"time"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// TestDateTolerant_matchesStrict checks that a date in current RFC 5322
// syntax gives the instant net/mail gives, in the zone it names, and no
// diagnostics.
func TestDateTolerant_matchesStrict(t *testing.T) {
	for _, test := range []struct {
		raw    string
		strict string // the same date as net/mail reads it, when raw has CFWS net/mail refuses
	}{
		{raw: "Mon, 22 Jul 2019 13:57:29 -0500"},
		{raw: "22 Jul 2019 13:57:29 +0900"},
		{raw: "Mon, 22 Jul 2019 13:57 -0500"},
		{raw: "Mon, 22 Jul 2019 13:57:29 -0500 (GMT-05:00)"},
		{raw: "Mon, 2 Jan 2006 15:04:05 -0000"},
		{raw: "Mon, 02 Jan 2006 15:04:05 +0000"},
		{raw: "Sat, 18 Jun 2016 12:42:24 +0545"},
		{raw: "Thu, 20 Nov 1997 (comment) 09:55:06 (nested (comment)) -0600 (c3)", strict: "Thu, 20 Nov 1997 09:55:06 -0600"},
		{raw: "  Thu,\r\n 20 Nov 1997 09:55:06 -0600", strict: "Thu, 20 Nov 1997 09:55:06 -0600"},
	} {
		raw := test.raw
		if test.strict == "" {
			test.strict = raw
		}
		want, err := netmail.ParseDate(test.strict)
		if err != nil {
			t.Errorf("net/mail.ParseDate(%q): %v; the fixture must be valid", test.strict, err)
			continue
		}
		got := mail.ParseDateTolerant(raw)
		if !got.Valid || !got.Time.Equal(want) {
			t.Errorf("ParseDateTolerant(%q) = %v, %v; want %v", raw, got.Time, got.Valid, want)
		}
		if gotOffset := zoneOffset(got.Time); gotOffset != zoneOffset(want) {
			t.Errorf("ParseDateTolerant(%q) zone offset = %d, want %d", raw, gotOffset, zoneOffset(want))
		}
		if len(got.Diagnostics) != 0 {
			t.Errorf("ParseDateTolerant(%q) reported %v for a valid date", raw, got.Diagnostics)
		}
		if got.Raw != raw {
			t.Errorf("ParseDateTolerant(%q).Raw = %q", raw, got.Raw)
		}
	}
}

func zoneOffset(t time.Time) int {
	_, offset := t.Zone()
	return offset
}

func TestDateTolerant(t *testing.T) {
	type diag = wantDiag
	var (
		obsZone      = diag{message.CodeDateObsoleteZone, message.SeverityInfo}
		oddZone      = diag{message.CodeDateObsoleteZone, message.SeverityRecovered}
		missingZone  = diag{message.CodeDateMissingZone, message.SeverityRecovered}
		weekday      = diag{message.CodeDateWeekdayMismatch, message.SeverityRecovered}
		repaired     = diag{message.CodeDateInvalid, message.SeverityRecovered}
		unparseable  = diag{message.CodeDateInvalid, message.SeverityInvalidMetadata}
		onlyRepaired = []diag{repaired}
	)
	tests := []struct {
		raw    string
		want   string // RFC 3339, or empty when no time can be derived
		diags  []diag // exactly these, in any order
		action string // substring of some diagnostic's Action
	}{
		// Obsolete zones.
		{"Thu, 20 Nov 1997 09:55:06 GMT", "1997-11-20T09:55:06Z", []diag{obsZone}, "+0000"},
		{"Thu, 20 Nov 1997 09:55:06 UT", "1997-11-20T09:55:06Z", []diag{obsZone}, ""},
		{"Thu, 20 Nov 1997 09:55:06 EST", "1997-11-20T09:55:06-05:00", []diag{obsZone}, "-0500"},
		{"Thu, 20 Nov 1997 09:55:06 EDT", "1997-11-20T09:55:06-04:00", []diag{obsZone}, ""},
		{"Thu, 20 Nov 1997 09:55:06 CST", "1997-11-20T09:55:06-06:00", []diag{obsZone}, ""},
		{"Thu, 20 Nov 1997 09:55:06 MDT", "1997-11-20T09:55:06-06:00", []diag{obsZone}, ""},
		{"Thu, 20 Nov 1997 09:55:06 pst", "1997-11-20T09:55:06-08:00", []diag{obsZone}, ""},
		{"Mon, 2 Jan 06 15:04:05 MST (Some random stuff)", "2006-01-02T15:04:05-07:00", []diag{obsZone, repaired}, "obsolete year 06 as 2006"},
		{"Thu, 20 Nov 1997 09:55:06 UTC", "1997-11-20T09:55:06Z", []diag{oddZone}, ""},
		{"Thu, 20 Nov 1997 09:55:06 KST", "1997-11-20T09:55:06+09:00", []diag{oddZone}, "+0900"},
		{"Thu, 20 Nov 1997 09:55:06 CEST", "1997-11-20T09:55:06+02:00", []diag{oddZone}, ""},
		{"Thu, 20 Nov 1997 09:55:06 XYZ", "1997-11-20T09:55:06Z", []diag{oddZone}, "+0000"},
		{"Thu, 20 Nov 1997 09:55:06 A", "1997-11-20T09:55:06Z", []diag{oddZone}, "-0000"},
		{"Thu, 20 Nov 1997 09:55:06 Z", "1997-11-20T09:55:06Z", []diag{oddZone}, ""},

		// Missing zone.
		{"Thu, 20 Nov 1997 09:55:06", "1997-11-20T09:55:06Z", []diag{missingZone}, "assumed +0000"},
		{"20 Nov 1997 09:55", "1997-11-20T09:55:00Z", []diag{missingZone}, ""},

		// Weekday.
		{"Fri, 20 Nov 1997 09:55:06 -0600", "1997-11-20T09:55:06-06:00", []diag{weekday}, "ignored the weekday"},
		{"Thursday, 20 Nov 1997 09:55:06 -0600", "1997-11-20T09:55:06-06:00", onlyRepaired, "weekday"},
		{"Thu 20 Nov 1997 09:55:06 -0600", "1997-11-20T09:55:06-06:00", nil, ""},
		{"Jeu, 20 Nov 1997 09:55:06 -0600", "1997-11-20T09:55:06-06:00", onlyRepaired, "unrecognized weekday"},

		// Years.
		{"20 Nov 97 09:55 EDT", "1997-11-20T09:55:00-04:00", []diag{obsZone, repaired}, "obsolete year 97 as 1997"},
		{"20 Nov 49 09:55:06 +0000", "2049-11-20T09:55:06Z", onlyRepaired, "2049"},
		{"20 Nov 50 09:55:06 +0000", "1950-11-20T09:55:06Z", onlyRepaired, "1950"},
		{"20 Nov 100 09:55:06 +0000", "2000-11-20T09:55:06Z", onlyRepaired, "obsolete year 100 as 2000"},

		// Nonstandard zones, months, times and layouts.
		{"Thu, 20 Nov 1997 09:55:06 +09:00", "1997-11-20T09:55:06+09:00", onlyRepaired, "+0900"},
		{"Thu, 20 Nov 1997 09:55:06 +9", "1997-11-20T09:55:06+09:00", onlyRepaired, ""},
		{"Thu, 20 Nov 1997 09:55:06 GMT+0900", "1997-11-20T09:55:06+09:00", onlyRepaired, ""},
		{"Thu, 20 Nov 1997 09:55:06 UTC -05:30", "1997-11-20T09:55:06-05:30", onlyRepaired, ""},
		{"Thu, 20 Nov 1997 09:55:06+0900", "1997-11-20T09:55:06+09:00", onlyRepaired, "written against the time"},
		{"Thu, 20 Nov 1997 09:55:06 +9999", "1997-11-20T09:55:06Z", onlyRepaired, "invalid zone"},
		{"Thu, 20 Nov 1997 09:55:06 0900", "1997-11-20T09:55:06Z", []diag{missingZone, repaired}, "text after the date"},
		{"Thu, 20 November 1997 09:55:06 -0600", "1997-11-20T09:55:06-06:00", onlyRepaired, "month"},
		{"Thu, 20 Sept. 1997 09:55:06 -0600", "1997-09-20T09:55:06-06:00", []diag{repaired, weekday}, ""},
		{"Thu, 20 Nov 1997 9:55:06 PM -0600", "1997-11-20T21:55:06-06:00", onlyRepaired, "PM"},
		{"Thu, 20 Nov 1997 12:05 am -0600", "1997-11-20T00:05:00-06:00", onlyRepaired, "AM"},
		{"Thu, 20 Nov 1997 09:55:06.123 -0600", "1997-11-20T09:55:06-06:00", onlyRepaired, "fractional"},
		{"Thu, 20 Nov 1997 09.55.06 -0600", "1997-11-20T09:55:06-06:00", onlyRepaired, "'.' separators"},
		{"Thu, 20 Nov 1997 09:55:06 -0600 and more", "1997-11-20T09:55:06-06:00", onlyRepaired, "text after the date"},
		{"Thu, 20 Nov 1997 09:55:06 -0600 (unclosed", "1997-11-20T09:55:06-06:00", onlyRepaired, "never closed"},
		{"20 Nov 1997", "1997-11-20T00:00:00Z", []diag{missingZone, repaired}, "00:00:00"},
		{"Thu Nov 20 09:55:06 1997", "1997-11-20T09:55:06Z", []diag{missingZone, repaired}, "month before the day"},
		{"Thu Nov 20 09:55:06 EST 1997", "1997-11-20T09:55:06-05:00", []diag{obsZone, repaired}, ""},
		{"Nov 20, 1997 09:55:06 -0600", "1997-11-20T09:55:06-06:00", onlyRepaired, ""},
		{"1997-11-20T09:55:06Z", "1997-11-20T09:55:06Z", []diag{oddZone, repaired}, "ISO 8601"},
		{"1997-11-20 09:55:06 +0900", "1997-11-20T09:55:06+09:00", onlyRepaired, "ISO 8601"},
		{"1997-11-20T09:55:06-05:00", "1997-11-20T09:55:06-05:00", onlyRepaired, ""},
		{"20-Nov-1997 09:55:06 -0600", "1997-11-20T09:55:06-06:00", onlyRepaired, "dash-separated"},
		{"Thursday, 20-Nov-97 09:55:06 GMT", "1997-11-20T09:55:06Z", []diag{obsZone, repaired}, ""},
		{"Thu, 20 Nov 1997 23:59:60 +0000", "1997-11-21T00:00:00Z", nil, ""},

		// Dates no time can be derived from.
		{"garbage", "", []diag{unparseable}, "kept the raw value"},
		{"(only a comment)", "", []diag{unparseable}, ""},
		{"32 Jan 2020 10:00:00 +0000", "", []diag{unparseable}, ""},
		{"31 Feb 2020 10:00:00 +0000", "", []diag{unparseable}, ""},
		{"29 Feb 2019 10:00:00 +0000", "", []diag{unparseable}, ""},
		{"0 Jan 2020 10:00:00 +0000", "", []diag{unparseable}, ""},
		{"20 Foo 1997 09:55:06 +0000", "", []diag{unparseable}, ""},
		{"20 Nov 1997 25:00:00 +0000", "", []diag{unparseable}, ""},
		{"20 Nov 1997 09:61:00 +0000", "", []diag{unparseable}, ""},
		{"20 Nov 1997 09:55:61 +0000", "", []diag{unparseable}, ""},
		{"20 Nov 19970 09:55:06 +0000", "", []diag{unparseable}, ""},
		{"20 Nov 7 09:55:06 +0000", "", []diag{unparseable}, ""},
		{"20 Nov 09:55:06 +0000", "", []diag{unparseable}, ""},
		{"20/11/1997 09:55:06", "", []diag{unparseable}, ""},
		{"20 11 1997 09:55:06 +0000", "", []diag{unparseable}, ""},
		{"1997-13-20T09:55:06Z", "", []diag{unparseable}, ""},
		{"\x00\xff", "", []diag{unparseable}, ""},
		{"Thu,", "", []diag{unparseable}, ""},
	}
	for _, test := range tests {
		var h mail.Header
		h.Set("Date", test.raw)
		for name, got := range map[string]mail.DateResult{
			"ParseDateTolerant":   mail.ParseDateTolerant(test.raw),
			"Header.DateTolerant": h.DateTolerant(),
		} {
			if got.Raw != test.raw {
				t.Errorf("%s(%q).Raw = %q", name, test.raw, got.Raw)
			}
			if test.want == "" {
				if got.Valid || !got.Time.IsZero() {
					t.Errorf("%s(%q) = %v, valid %v; want no time", name, test.raw, got.Time, got.Valid)
				}
			} else {
				want, err := time.Parse(time.RFC3339, test.want)
				if err != nil {
					t.Fatal(err)
				}
				if !got.Valid || !got.Time.Equal(want) || zoneOffset(got.Time) != zoneOffset(want) {
					t.Errorf("%s(%q) = %v, valid %v; want %v", name, test.raw, got.Time.Format(time.RFC3339), got.Valid, test.want)
				}
			}
			if len(got.Diagnostics) != len(test.diags) {
				t.Errorf("%s(%q) diagnostics = %v, want %v", name, test.raw, diagCodes(got.Diagnostics), test.diags)
			}
			for _, d := range test.diags {
				if !hasDiag(got.Diagnostics, d.code, d.severity) {
					t.Errorf("%s(%q): no %v %s diagnostic in %v", name, test.raw, d.severity, d.code, diagCodes(got.Diagnostics))
				}
			}
			if test.action != "" && !strings.Contains(actions(got.Diagnostics), test.action) {
				t.Errorf("%s(%q): no diagnostic action mentions %q: %q", name, test.raw, test.action, actions(got.Diagnostics))
			}
		}
	}
}

func TestDateTolerant_missingAndBounds(t *testing.T) {
	var h mail.Header
	if got := h.DateTolerant(); got.Valid || got.Raw != "" || got.Diagnostics != nil {
		t.Errorf("missing field: %+v", got)
	}

	// The strict accessor still gives a zone name it does not know a zero
	// offset; the tolerant one applies RFC 5322's table.
	h.Set("Date", "Thu, 20 Nov 1997 09:55:06 PST")
	if got := h.DateTolerant(); zoneOffset(got.Time) != -8*3600 {
		t.Errorf("DateTolerant PST offset = %d", zoneOffset(got.Time))
	}

	// Text after a date is not examined beyond a fixed number of words, and
	// comments of any depth are skipped in one pass.
	deep := strings.Repeat("(", 100000) + strings.Repeat(")", 100000)
	for _, raw := range []string{
		"Thu, 20 Nov 1997 09:55:06 -0600 " + strings.Repeat("x ", 200000),
		deep + "Thu, 20 Nov 1997 " + deep + " 09:55:06 -0600" + deep,
		"Thu, 20 Nov 1997 09:55:06 -0600 " + strings.Repeat("(", 200000),
	} {
		got := mail.ParseDateTolerant(raw)
		want := time.Date(1997, 11, 20, 15, 55, 6, 0, time.UTC)
		if !got.Valid || !got.Time.Equal(want) {
			t.Errorf("ParseDateTolerant(%.50q...) = %v, %v", raw, got.Time, got.Valid)
		}
	}
	for _, raw := range []string{
		strings.Repeat("(", 400000),
		strings.Repeat("1 ", 200000),
		strings.Repeat("-", 400000),
		strings.Repeat(":", 400000),
		strings.Repeat("Mon, ", 100000),
		strings.Repeat("9", 400000),
	} {
		if got := mail.ParseDateTolerant(raw); got.Valid || len(got.Diagnostics) != 1 {
			t.Errorf("ParseDateTolerant(%.20q...) = valid %v, %v", raw, got.Valid, diagCodes(got.Diagnostics))
		}
	}
}
