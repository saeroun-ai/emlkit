package mail_test

import (
	"bufio"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
	"github.com/saeroun-ai/emlkit/textproto"
)

func clauseNames(r mail.Received) []string {
	var l []string
	for _, c := range r.Clauses {
		l = append(l, c.Name)
	}
	return l
}

// TestParseReceived_providers reads Received fields in the shapes common
// servers write them, none of which the RFC 5321 grammar fully describes.
func TestParseReceived_providers(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		clauses []string
		from    string
		by      string
		via     string
		with    string
		id      string
		forv    string
		date    string // RFC 3339
		comment string // a comment of the first clause
		diags   int
	}{
		{
			name:    "postfix",
			raw:     "from mail-wr1-f65.google.com (mail-wr1-f65.google.com [209.85.221.65]) by mx.example.com (Postfix) with ESMTPS id 4B1x2y3z for <user@example.com>; Wed, 22 Jan 2020 11:11:25 -0500 (EST)",
			clauses: []string{"from", "by", "with", "id", "for"},
			from:    "mail-wr1-f65.google.com", by: "mx.example.com", with: "ESMTPS", id: "4B1x2y3z", forv: "<user@example.com>",
			date: "2020-01-22T11:11:25-05:00", comment: "mail-wr1-f65.google.com [209.85.221.65]",
		},
		{
			name:    "gmail",
			raw:     "by 2002:a5d:6708:0:0:0:0:0 with SMTP id o8csp4567890wru; Wed, 22 Jan 2020 08:11:24 -0800 (PST)",
			clauses: []string{"by", "with", "id"},
			by:      "2002:a5d:6708:0:0:0:0:0", with: "SMTP", id: "o8csp4567890wru",
			date: "2020-01-22T08:11:24-08:00",
		},
		{
			name:    "exchange",
			raw:     "from AM6PR04MB1234.eurprd04.prod.outlook.com ([fe80::1]) by AM6PR04MB1234.eurprd04.prod.outlook.com ([fe80::1%4]) with mapi id 15.20.2644.027; Wed, 22 Jan 2020 16:11:25 +0000",
			clauses: []string{"from", "by", "with", "id"},
			from:    "AM6PR04MB1234.eurprd04.prod.outlook.com", by: "AM6PR04MB1234.eurprd04.prod.outlook.com", with: "mapi", id: "15.20.2644.027",
			date: "2020-01-22T16:11:25Z", comment: "[fe80::1]",
		},
		{
			name:    "exchange with a comment inside a clause",
			raw:     "from EX01.example.com (10.0.0.1) by EX02.example.com (10.0.0.2) with Microsoft SMTP Server (version=TLS1_2, cipher=TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384) id 15.1.2308.27 via Frontend Transport; Wed, 22 Jan 2020 16:11:25 +0000",
			clauses: []string{"from", "by", "with", "id", "via"},
			from:    "EX01.example.com", by: "EX02.example.com", with: "Microsoft SMTP Server", id: "15.1.2308.27", via: "Frontend Transport",
			date: "2020-01-22T16:11:25Z", comment: "10.0.0.1",
		},
		{
			name:    "sendmail",
			raw:     "from a.example.net (b.example.net [192.0.2.4])\tby c.example.org (8.14.7/8.14.7) with ESMTP id 00MGBPxx012345\t(version=TLSv1/SSLv3 cipher=DHE-RSA-AES256-SHA bits=256 verify=NO)\tfor <x@example.org>; Wed, 22 Jan 2020 11:11:25 -0500",
			clauses: []string{"from", "by", "with", "id", "for"},
			from:    "a.example.net", by: "c.example.org", with: "ESMTP", id: "00MGBPxx012345", forv: "<x@example.org>",
			date: "2020-01-22T11:11:25-05:00", comment: "b.example.net [192.0.2.4]",
		},
		{
			name:    "exim",
			raw:     "from user by host.example.org with local (Exim 4.92) (envelope-from <a@example.org>) id 1iuJ3W-0007Xm-Rc for c@example.org; Wed, 22 Jan 2020 16:11:25 +0000",
			clauses: []string{"from", "by", "with", "id", "for"},
			from:    "user", by: "host.example.org", with: "local", id: "1iuJ3W-0007Xm-Rc", forv: "c@example.org",
			date: "2020-01-22T16:11:25Z",
		},
		{
			name:    "qmail with HELO",
			raw:     "from unknown (HELO mail.example.com) (192.0.2.7) by mx.example.org with SMTP; 22 Jan 2020 16:11:25 -0000",
			clauses: []string{"from", "by", "with"},
			from:    "unknown", by: "mx.example.org", with: "SMTP",
			date: "2020-01-22T16:11:25Z", comment: "HELO mail.example.com",
		},
		{
			name:    "qmail invoked",
			raw:     "(qmail 12345 invoked by uid 1000); 22 Jan 2020 16:11:25 -0000",
			clauses: []string{""},
			date:    "2020-01-22T16:11:25Z", comment: "qmail 12345 invoked by uid 1000",
			diags: 1, // no clause keyword
		},
		{
			name:    "upper-case keywords and an obsolete zone",
			raw:     "FROM a.example BY b.example WITH SMTP ID x1 FOR <u@example.org>; Wed, 22 Jan 20 11:11:25 EST",
			clauses: []string{"from", "by", "with", "id", "for"},
			from:    "a.example", by: "b.example", with: "SMTP", id: "x1", forv: "<u@example.org>",
			date:  "2020-01-22T11:11:25-05:00",
			diags: 2, // date.obsolete_zone and the obsolete year
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := mail.ParseReceived(test.raw)
			if names := clauseNames(got); !reflect.DeepEqual(names, test.clauses) {
				t.Errorf("clauses = %q, want %q", names, test.clauses)
			}
			if got.From != test.from || got.By != test.by || got.Via != test.via || got.With != test.with || got.ID != test.id || got.For != test.forv {
				t.Errorf("from %q by %q via %q with %q id %q for %q", got.From, got.By, got.Via, got.With, got.ID, got.For)
			}
			want, err := time.Parse(time.RFC3339, test.date)
			if err != nil {
				t.Fatal(err)
			}
			if !got.HasDate || !got.Date.Equal(want) {
				t.Errorf("date = %v (%v), want %v", got.Date, got.HasDate, want)
			}
			if test.comment != "" && (len(got.Clauses) == 0 || len(got.Clauses[0].Comments) == 0 || got.Clauses[0].Comments[0] != test.comment) {
				t.Errorf("first clause = %+v, want the comment %q", got.Clauses, test.comment)
			}
			if len(got.Diagnostics) != test.diags {
				t.Errorf("diagnostics = %v, want %d", got.Diagnostics, test.diags)
			}
			checkReceivedRaw(t, test.raw, got)
		})
	}
}

// checkReceivedRaw checks clause-level preservation: every clause's Raw is a
// part of the value, the clauses appear in order without overlap, and what
// lies between them is only whitespace.
func checkReceivedRaw(t *testing.T, raw string, got mail.Received) {
	t.Helper()
	if got.Raw != raw {
		t.Errorf("Raw = %q", got.Raw)
	}
	rest := raw
	for _, c := range got.Clauses {
		at := strings.Index(rest, c.Raw)
		if c.Raw == "" || at < 0 {
			t.Fatalf("clause Raw %q is not found in order in %q", c.Raw, raw)
		}
		if strings.TrimSpace(rest[:at]) != "" {
			t.Errorf("text %q between clauses is in no clause", rest[:at])
		}
		rest = rest[at+len(c.Raw):]
	}
	if got.DateRaw != "" && !strings.Contains(rest, got.DateRaw) {
		t.Errorf("DateRaw %q does not follow the clauses in %q", got.DateRaw, raw)
	}
}

func TestParseReceived_damaged(t *testing.T) {
	invalidDate := wantDiag{message.CodeDateInvalid, message.SeverityInvalidMetadata}
	repaired := wantDiag{message.CodeReceivedInvalid, message.SeverityRecovered}
	tests := []struct {
		name    string
		raw     string
		clauses []string
		values  []string // Value of each clause
		hasDate bool
		dateRaw string
		diags   []wantDiag
	}{
		{name: "no semicolon", raw: "from a.example by b.example with SMTP",
			clauses: []string{"from", "by", "with"}, values: []string{"a.example", "b.example", "SMTP"}, diags: []wantDiag{repaired}},
		{name: "no date after the semicolon", raw: "from a.example by b.example;",
			clauses: []string{"from", "by"}, values: []string{"a.example", "b.example"}, diags: []wantDiag{repaired}},
		{name: "invalid date", raw: "from a.example by b.example; not a date",
			clauses: []string{"from", "by"}, values: []string{"a.example", "b.example"}, dateRaw: "not a date", diags: []wantDiag{invalidDate}},
		{name: "text before the first clause", raw: "X-junk 1.0 from a.example by b.example; 22 Jan 2020 16:11:25 +0000",
			clauses: []string{"", "from", "by"}, values: []string{"X-junk 1.0", "a.example", "b.example"}, hasDate: true, dateRaw: "22 Jan 2020 16:11:25 +0000", diags: []wantDiag{repaired}},
		{name: "no clause", raw: "some text without keywords; 22 Jan 2020 16:11:25 +0000",
			clauses: []string{""}, values: []string{"some text without keywords"}, hasDate: true, dateRaw: "22 Jan 2020 16:11:25 +0000", diags: []wantDiag{repaired}},
		{name: "repeated and empty clauses", raw: "from from a.example by by b.example for for; 22 Jan 2020 16:11:25 +0000",
			clauses: []string{"from", "from", "by", "by", "for", "for"}, values: []string{"", "a.example", "", "b.example", "", ""}, hasDate: true, dateRaw: "22 Jan 2020 16:11:25 +0000"},
		{name: "unclosed comment", raw: "from a.example (unclosed by b.example; 22 Jan 2020 16:11:25 +0000",
			clauses: []string{"from", "by"}, values: []string{"a.example (unclosed", "b.example"}, hasDate: true, dateRaw: "22 Jan 2020 16:11:25 +0000", diags: []wantDiag{repaired}},
		{name: "semicolon inside a comment and a quoted string", raw: `from a.example (x; y) by "b;c" ; 22 Jan 2020 16:11:25 +0000`,
			clauses: []string{"from", "by"}, values: []string{"a.example", `"b;c"`}, hasDate: true, dateRaw: "22 Jan 2020 16:11:25 +0000"},
		{name: "several semicolons", raw: "from a.example; by b.example; 22 Jan 2020 16:11:25 +0000",
			clauses: []string{"from", "by"}, values: []string{"a.example;", "b.example"}, hasDate: true, dateRaw: "22 Jan 2020 16:11:25 +0000"},
		{name: "date only", raw: "; 22 Jan 2020 16:11:25 +0000",
			hasDate: true, dateRaw: "22 Jan 2020 16:11:25 +0000", diags: []wantDiag{repaired}},
		{name: "nothing", raw: " ; ",
			diags: []wantDiag{{message.CodeReceivedInvalid, message.SeverityInvalidMetadata}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := mail.ParseReceived(test.raw)
			if names := clauseNames(got); !reflect.DeepEqual(names, test.clauses) {
				t.Errorf("clauses = %q, want %q", names, test.clauses)
			}
			var values []string
			for _, c := range got.Clauses {
				values = append(values, c.Value)
			}
			if !reflect.DeepEqual(values, test.values) {
				t.Errorf("values = %q, want %q", values, test.values)
			}
			if got.HasDate != test.hasDate || got.DateRaw != test.dateRaw {
				t.Errorf("date %v, raw %q; want %v, %q", got.HasDate, got.DateRaw, test.hasDate, test.dateRaw)
			}
			if len(got.Diagnostics) != len(test.diags) {
				t.Errorf("diagnostics = %v, want %v", got.Diagnostics, test.diags)
			}
			for _, d := range got.Diagnostics {
				if !wantsDiag(test.diags, d) {
					t.Errorf("unexpected diagnostic %v", d)
				}
			}
			checkReceivedRaw(t, test.raw, got)
		})
	}
}

// TestHeader_Received reads the trace fields of a parsed header in document
// order and leaves the header writing back byte for byte.
func TestHeader_Received(t *testing.T) {
	raw := "Received: from b.example (b.example [192.0.2.2])\r\n" +
		"\tby c.example with ESMTP id 2\r\n" +
		"\tfor <u@example.org>; Wed, 22 Jan 2020 11:11:27 -0500\r\n" +
		"Subject: x\r\n" +
		"Received: garbage without structure\r\n" +
		"Received: from a.example by b.example with SMTP id 1; Wed, 22 Jan 2020 11:11:25 -0500\r\n" +
		"\r\n"
	th, err := textproto.ReadHeader(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	h := mail.Header{Header: message.Header{Header: th}}
	got := h.Received()
	if len(got) != 3 {
		t.Fatalf("Received() returned %d fields", len(got))
	}
	if got[0].ID != "2" || got[0].For != "<u@example.org>" || !got[0].HasDate || len(got[0].Diagnostics) != 0 {
		t.Errorf("first field = %+v", got[0])
	}
	if got[1].Raw != "garbage without structure" || len(got[1].Clauses) != 1 || got[1].Clauses[0].Name != "" || got[1].HasDate {
		t.Errorf("second field = %+v", got[1])
	}
	for _, d := range got[1].Diagnostics {
		if d.Field != "Received" || d.Code != message.CodeReceivedInvalid {
			t.Errorf("diagnostic %v", d)
		}
	}
	if len(got[1].Diagnostics) == 0 {
		t.Errorf("the damaged field reported nothing")
	}
	if got[2].ID != "1" || got[2].From != "a.example" {
		t.Errorf("third field = %+v", got[2])
	}
	if !got[0].Date.After(got[2].Date) {
		t.Errorf("fields are not in document order: %v, %v", got[0].Date, got[2].Date)
	}

	var out strings.Builder
	if err := textproto.WriteHeader(&out, h.Header.Header); err != nil {
		t.Fatal(err)
	}
	if out.String() != raw {
		t.Errorf("header was not written back verbatim:\n got %q\nwant %q", out.String(), raw)
	}

	var empty mail.Header
	if l := empty.Received(); l != nil {
		t.Errorf("Received() of an empty header = %v", l)
	}
}

func TestParseReceived_bounds(t *testing.T) {
	got := mail.ParseReceived(strings.Repeat("by x ", 20000) + "; 22 Jan 2020 16:11:25 +0000")
	if len(got.Clauses) != 16384 || !hasDiag(got.Diagnostics, message.CodeReceivedInvalid, message.SeverityLimitExceeded) || !got.HasDate {
		t.Errorf("clause cap: %d clauses, %v", len(got.Clauses), diagCodes(got.Diagnostics))
	}
	for name, raw := range map[string]string{
		"open comments": strings.Repeat("(", 200000) + " from a; 22 Jan 2020 16:11:25 +0000",
		"deep comment":  "from a " + strings.Repeat("(", 100000) + strings.Repeat(")", 100000) + " by b; 22 Jan 2020 16:11:25 +0000",
		"semicolons":    strings.Repeat(";", 400000),
		"quotes":        strings.Repeat(`"`, 199999),
		"backslashes":   "from " + strings.Repeat(`\`, 199999),
		"keywords":      strings.Repeat("from ", 80000),
		"one long word": strings.Repeat("x", 400000),
		"NUL bytes":     strings.Repeat("\x00", 100000) + "; \x00",
	} {
		t.Run(name, func(t *testing.T) {
			got := mail.ParseReceived(raw)
			if len(got.Diagnostics) > 64 || len(got.Clauses) > 16384 {
				t.Errorf("%d diagnostics, %d clauses", len(got.Diagnostics), len(got.Clauses))
			}
			checkReceivedRaw(t, raw, got)
		})
	}
}
