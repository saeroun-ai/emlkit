package mail_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

func TestListURLs(t *testing.T) {
	var (
		repaired = wantDiag{message.CodeListHeaderInvalid, message.SeverityRecovered}
		cut      = wantDiag{message.CodeListHeaderInvalid, message.SeverityIncomplete}
		unusable = wantDiag{message.CodeListHeaderInvalid, message.SeverityInvalidMetadata}
	)
	deep := strings.Repeat("(", 2000) + strings.Repeat(")", 2000)
	tests := []struct {
		name    string
		raw     string
		want    []string // parsed URLs
		invalid []string // Text of the entries without a parsed URL
		no      bool
		diags   []wantDiag // exactly these codes, in any order and number
		action  string
	}{
		// RFC 2369 as written.
		{name: "two URLs", raw: "<https://example.com/unsub>, <mailto:unsub@example.com?subject=unsubscribe>",
			want: []string{"https://example.com/unsub", "mailto:unsub@example.com?subject=unsubscribe"}},
		{name: "comments", raw: "(c) <mailto:a@example.org> (c), (c) <https://example.org/u> (c)",
			want: []string{"mailto:a@example.org", "https://example.org/u"}},
		{name: "deep comments", raw: deep + " <mailto:a@example.org> " + deep,
			want: []string{"mailto:a@example.org"}},
		{name: "RFC 2369 examples", raw: "<ftp://ftp.host.com/list.txt> (FTP),\r\n <mailto:list@host.com?subject=help>",
			want: []string{"ftp://ftp.host.com/list.txt", "mailto:list@host.com?subject=help"}},
		{name: "URL with commas", raw: "<mailto:a@example.org?subject=a,b>, <https://example.org/?q=1,2>",
			want: []string{"mailto:a@example.org?subject=a,b", "https://example.org/?q=1,2"}},
		{name: "NO", raw: "NO (posting not allowed on this list)", no: true},
		{name: "NO between comments", raw: `(\(\)) NO (\\)`, no: true},
		{name: "lower-case no", raw: "no", no: true},

		// Damaged fields.
		{name: "valid invalid valid", raw: "<https://example.com/a>, <https://example.com/%zz>, <mailto:b@example.com>",
			want: []string{"https://example.com/a", "mailto:b@example.com"}, invalid: []string{"https://example.com/%zz"}, diags: []wantDiag{unusable}, action: "continued with the next URL"},
		{name: "empty brackets", raw: "<>, <mailto:b@example.com>",
			want: []string{"mailto:b@example.com"}, invalid: []string{""}, diags: []wantDiag{unusable}},
		{name: "truncated", raw: "<https://example.com/a>, <https://example.com/he",
			want: []string{"https://example.com/a", "https://example.com/he"}, diags: []wantDiag{cut}, action: "missing '>'"},
		{name: "unclosed before the next", raw: "<https://example.com/a, <mailto:b@example.com>",
			want: []string{"https://example.com/a", "mailto:b@example.com"}, diags: []wantDiag{repaired}, action: "missing '>'"},
		{name: "no brackets", raw: "https://example.com/a, mailto:b@example.com",
			want: []string{"https://example.com/a", "mailto:b@example.com"}, diags: []wantDiag{repaired}, action: "angle brackets"},
		{name: "missing opening bracket", raw: "https://example.com/a>, <mailto:b@example.com>",
			want: []string{"https://example.com/a", "mailto:b@example.com"}, diags: []wantDiag{repaired}, action: "missing '<'"},
		{name: "whitespace inside brackets", raw: "<https://exam\r\n ple.com/a b>, < mailto:b@example.com >",
			want: []string{"https://example.com/ab", "mailto:b@example.com"}, diags: []wantDiag{repaired}, action: "whitespace"},
		{name: "no scheme", raw: "<example.com/unsub>",
			want: []string{"example.com/unsub"}, diags: []wantDiag{repaired}, action: "scheme"},
		{name: "words between URLs", raw: "<https://example.com/a> or <mailto:b@example.com>",
			want: []string{"https://example.com/a", "mailto:b@example.com"}, diags: []wantDiag{repaired}, action: "ignored that text"},
		{name: "unclosed comment", raw: "<mailto:a@example.org> (unterminated, <https://example.org/u>",
			want: []string{"mailto:a@example.org", "https://example.org/u"}, diags: []wantDiag{repaired}},
		{name: "NO with URLs", raw: "NO, <mailto:b@example.com>",
			want: []string{"mailto:b@example.com"}, no: true, diags: []wantDiag{repaired}},
		{name: "control character", raw: "<https://example.com/\x01>, <mailto:b@example.com>",
			want: []string{"mailto:b@example.com"}, invalid: []string{"https://example.com/\x01"}, diags: []wantDiag{unusable}},
		{name: "nothing usable", raw: "see the web site",
			diags: []wantDiag{repaired}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var h mail.Header
			h.Set("List-Post", test.raw)
			for name, got := range map[string]mail.ListURLsResult{
				"ParseListURLs":   mail.ParseListURLs(test.raw),
				"Header.ListURLs": h.ListURLs("List-Post"),
			} {
				var urls, invalid []string
				for _, u := range got.URLs {
					urls = append(urls, u.String())
				}
				for _, e := range got.Entries {
					if e.Raw == "" || !strings.Contains(test.raw, e.Raw) {
						t.Errorf("%s: entry Raw %q is not a part of the value", name, e.Raw)
					}
					if e.URL == nil {
						invalid = append(invalid, e.Text)
					}
				}
				if !reflect.DeepEqual(urls, test.want) {
					t.Errorf("%s: URLs = %q, want %q", name, urls, test.want)
				}
				if !reflect.DeepEqual(invalid, test.invalid) {
					t.Errorf("%s: invalid entries = %q, want %q", name, invalid, test.invalid)
				}
				if got.No != test.no {
					t.Errorf("%s: No = %v", name, got.No)
				}
				for _, d := range test.diags {
					if !hasDiag(got.Diagnostics, d.code, d.severity) {
						t.Errorf("%s: no %v %s diagnostic in %v", name, d.severity, d.code, got.Diagnostics)
					}
				}
				for _, d := range got.Diagnostics {
					if !wantsDiag(test.diags, d) {
						t.Errorf("%s: unexpected diagnostic %v", name, d)
					}
				}
				if test.action != "" && !strings.Contains(actions(got.Diagnostics), test.action) {
					t.Errorf("%s: no diagnostic action mentions %q: %q", name, test.action, actions(got.Diagnostics))
				}
				if got.Raw != test.raw {
					t.Errorf("%s: Raw = %q", name, got.Raw)
				}
			}
			// A field the strict accessor accepts gives the same URLs.
			if strict, err := h.ListCommandURLList("List-Post"); err == nil && !test.no {
				var urls []string
				for _, u := range strict {
					urls = append(urls, u.String())
				}
				if len(test.diags) == 0 && !reflect.DeepEqual(urls, test.want) {
					t.Errorf("ListCommandURLList = %q, ListURLs = %q", urls, test.want)
				}
			}
		})
	}

	var h mail.Header
	if got := h.ListURLs("List-Help"); got.No || got.Raw != "" || got.URLs != nil || got.Entries != nil || got.Diagnostics != nil {
		t.Errorf("missing field: %+v", got)
	}
	h.Set("List-Unsubscribe", "<https://example.com/%zz>")
	got := h.ListURLs("List-Unsubscribe")
	if len(got.Diagnostics) != 1 || got.Diagnostics[0].Cause == nil || got.Diagnostics[0].Field != "List-Unsubscribe" {
		t.Errorf("invalid URL diagnostics = %v", got.Diagnostics)
	}
}

func TestListID(t *testing.T) {
	repaired := wantDiag{message.CodeListHeaderInvalid, message.SeverityRecovered}
	unusable := wantDiag{message.CodeListHeaderInvalid, message.SeverityInvalidMetadata}
	tests := []struct {
		raw    string
		id     string
		desc   string
		diags  []wantDiag
		action string
	}{
		// RFC 2919 as written.
		{raw: `"The Foo List" <foo.example.com>`, id: "foo.example.com", desc: "The Foo List"},
		{raw: "List Header Mailing List <list-header.nisto.com>", id: "list-header.nisto.com", desc: "List Header Mailing List"},
		{raw: "<commonspace-users.list-id.within.com>", id: "commonspace-users.list-id.within.com"},
		{raw: `"Lena's Personal Joke List" (a comment) <lenas-jokes.da39efc25c530ad145d41b86f7420c3b.021999.localhost>`,
			id: "lenas-jokes.da39efc25c530ad145d41b86f7420c3b.021999.localhost", desc: "Lena's Personal Joke List"},
		{raw: "=?utf-8?q?Caf=C3=A9?= =?utf-8?q?_List?= <cafe.example.com>", id: "cafe.example.com", desc: "Café List"},
		{raw: `"=?utf-8?q?Caf=C3=A9?=" <cafe.example.com>`, id: "cafe.example.com", desc: "Café"},
		{raw: `"a \"quoted\" name" <q.example.com>`, id: "q.example.com", desc: `a "quoted" name`},

		// Damaged fields.
		{raw: "foo.example.com", id: "foo.example.com", diags: []wantDiag{repaired}, action: "angle brackets"},
		{raw: "Foo <foo.example.com", id: "foo.example.com", desc: "Foo", diags: []wantDiag{repaired}, action: "missing '>'"},
		{raw: "Foo < foo.exam\tple.com >", id: "foo.example.com", desc: "Foo", diags: []wantDiag{repaired}, action: "whitespace"},
		{raw: "Announce <news> list <announce.example.com>", id: "announce.example.com", desc: "Announce", diags: []wantDiag{repaired}, action: "last of 2"},
		{raw: "Foo <foo.example.com> trailing", id: "foo.example.com", desc: "Foo", diags: []wantDiag{repaired}, action: "text after"},
		{raw: "Foo <> <foo.example.com>", id: "foo.example.com", desc: "Foo", diags: []wantDiag{repaired}, action: "empty angle brackets"},
		{raw: "Foo (unclosed <foo.example.com>", id: "foo.example.com", desc: "Foo unclosed", diags: []wantDiag{repaired}},
		{raw: "Foo\x01 <foo\x00.example.com>", id: "foo.example.com", desc: "Foo", diags: []wantDiag{repaired}, action: "control characters"},
		{raw: "=?x-unknown?q?abc?= <l.example.com>", id: "l.example.com", desc: "=?x-unknown?q?abc?=",
			diags: []wantDiag{{message.CodeRFC2047UnknownCharset, message.SeverityUnsupported}}, action: "as written"},
		{raw: "Foo List", desc: "Foo List", diags: []wantDiag{unusable}, action: "kept the raw value"},
		{raw: `"Foo"`, desc: "Foo", diags: []wantDiag{unusable}},
		{raw: "<>", diags: []wantDiag{unusable}},
		{raw: "(only a comment)", diags: []wantDiag{unusable}},
	}
	for _, test := range tests {
		var h mail.Header
		h.Set("List-Id", test.raw)
		for name, got := range map[string]mail.ListIDResult{
			"ParseListID":   mail.ParseListID(test.raw),
			"Header.ListID": h.ListID(),
		} {
			if got.ID != test.id || got.Description != test.desc || got.Raw != test.raw {
				t.Errorf("%s(%q) = id %q, description %q, raw %q; want %q, %q", name, test.raw, got.ID, got.Description, got.Raw, test.id, test.desc)
			}
			if len(got.Diagnostics) != len(test.diags) {
				t.Errorf("%s(%q): diagnostics = %v, want %v", name, test.raw, got.Diagnostics, test.diags)
			}
			for _, d := range got.Diagnostics {
				if !wantsDiag(test.diags, d) {
					t.Errorf("%s(%q): unexpected diagnostic %v", name, test.raw, d)
				}
			}
			if test.action != "" && !strings.Contains(actions(got.Diagnostics), test.action) {
				t.Errorf("%s(%q): no diagnostic action mentions %q: %q", name, test.raw, test.action, actions(got.Diagnostics))
			}
		}
	}
	var h mail.Header
	if got := h.ListID(); got.ID != "" || got.Raw != "" || got.Diagnostics != nil {
		t.Errorf("missing field: %+v", got)
	}
}

func TestListHeaders_bounds(t *testing.T) {
	got := mail.ParseListURLs(strings.Repeat("<mailto:a@example.org>,", 20000))
	if len(got.URLs) != 16384 || !hasDiag(got.Diagnostics, message.CodeListHeaderInvalid, message.SeverityLimitExceeded) {
		t.Errorf("entry cap: %d URLs, %v", len(got.URLs), diagCodes(got.Diagnostics))
	}
	got = mail.ParseListURLs(strings.Repeat("<%zz> ", 500) + "<mailto:a@example.org>")
	if len(got.Diagnostics) != 64 || got.Diagnostics[63].Code != message.CodeLimitDiagnosticCount || len(got.URLs) != 1 || len(got.Entries) != 501 {
		t.Errorf("diagnostic cap: %d diagnostics, %d URLs, %d entries", len(got.Diagnostics), len(got.URLs), len(got.Entries))
	}
	for name, raw := range map[string]string{
		"open angles":      strings.Repeat("<", 200000),
		"open angle words": strings.Repeat("<a ", 100000),
		"close angles":     strings.Repeat(">", 200000),
		"open comments":    strings.Repeat("(", 200000) + "<mailto:a@example.org>",
		"words":            strings.Repeat("x ", 200000),
		"commas":           strings.Repeat(",", 400000),
		"schemes":          strings.Repeat("a:b ", 100000),
		"one long URL":     "<https://example.org/" + strings.Repeat("x", 400000) + ">",
		"percent signs":    "<" + strings.Repeat("%", 400000) + ">",
		"NO":               strings.Repeat("NO ", 100000),
		"quotes":           strings.Repeat(`"`, 199999),
		"NUL bytes":        strings.Repeat("<\x00>", 100000),
	} {
		t.Run(name, func(t *testing.T) {
			got := mail.ParseListURLs(raw)
			if len(got.Diagnostics) > 64 || len(got.Entries) > 16384 || len(got.URLs) > len(got.Entries) {
				t.Errorf("ParseListURLs: %d diagnostics, %d entries, %d URLs", len(got.Diagnostics), len(got.Entries), len(got.URLs))
			}
			id := mail.ParseListID(raw)
			if len(id.Diagnostics) > 1 || id.Raw != raw {
				t.Errorf("ParseListID: %d diagnostics", len(id.Diagnostics))
			}
		})
	}
}
