package mail_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// TestMsgIDListTolerant_matchesStrict checks that a field MsgIDList accepts
// yields the same identifiers from the tolerant parser, and no diagnostics.
func TestMsgIDListTolerant_matchesStrict(t *testing.T) {
	deep := strings.Repeat("(", 2000) + strings.Repeat(")", 2000)
	for _, raw := range []string{
		"<123@asdf>",
		"  \t <DM6PR09MB253761A38B42C713082A7CE2C60C0@DM6PR09MB2537.namprd09.prod.outlook.com>",
		`<20200122161125.7enac4n5rsxfnhg7@example.com> (Christopher Wellons's message of "Wed, 22 Jan 2020 11:11:25 -0500")`,
		"<87pnfb69f3.fsf@bernat.ch>  \t <20200122161125.7enac4n5rsxfnhg7@nullprogram.com>",
		"<87pnfb69f3.fsf@bernat.ch> (a comment) \t <20200122161125.7enac4n5rsxfnhg7@nullprogram.com> (another comment)",
		"<123@[2001:db8:85a3:8d3:1319:8a2e:370:7348]>",
		deep + "<a@example.org>" + deep + "<b@example.org>" + deep,
		`<a@example.org> (\)) <b@example.org>`,
		`(<fake@example.org>) <a@example.org>`,
		"<jörg@bücher.example>",
		"<a@[x>y<z]> <b@example.org>",
	} {
		var h mail.Header
		h.Set("References", raw)
		want, err := h.MsgIDList("References")
		if err != nil {
			t.Errorf("MsgIDList(%.60q): %v; the fixture must be valid", raw, err)
			continue
		}
		got := h.MsgIDListTolerant("References")
		if !reflect.DeepEqual(got.IDs, want) {
			t.Errorf("MsgIDListTolerant(%.60q) = %q, want %q", raw, got.IDs, want)
		}
		if len(got.Diagnostics) != 0 {
			t.Errorf("MsgIDListTolerant(%.60q) reported %v for a valid field", raw, got.Diagnostics)
		}
		if got.Raw != raw || len(got.Entries) != len(want) {
			t.Errorf("MsgIDListTolerant(%.60q): Raw %.60q, %d entries", raw, got.Raw, len(got.Entries))
		}
	}
}

func TestMsgIDListTolerant(t *testing.T) {
	type diag = wantDiag
	var (
		repaired = diag{message.CodeMessageIDInvalid, message.SeverityRecovered}
		cut      = diag{message.CodeMessageIDInvalid, message.SeverityIncomplete}
		unusable = diag{message.CodeMessageIDInvalid, message.SeverityInvalidMetadata}
		partial  = diag{message.CodeMessageIDPartialList, message.SeverityRecovered}
	)
	tests := []struct {
		name    string
		raw     string
		want    []string // usable identifiers
		invalid []string // Raw of the entries without one
		diags   []diag   // exactly these codes, in any order and number
		action  string
	}{
		{name: "valid invalid valid", raw: "<a@example.org> <> <b@example.org>",
			want: []string{"a@example.org", "b@example.org"}, invalid: []string{"<>"}, diags: []diag{unusable, partial}},
		{name: "control characters", raw: "<a@example.org> <\x00bad@example.org> <b@example.org>",
			want: []string{"a@example.org", "b@example.org"}, invalid: []string{"<\x00bad@example.org>"}, diags: []diag{unusable, partial}},
		{name: "more than one at", raw: "<a@b@example.org>",
			want: []string{"a@b@example.org"}, diags: []diag{repaired}, action: "more than one"},
		{name: "no at", raw: "<20200122.abc> <b@example.org>",
			want: []string{"20200122.abc", "b@example.org"}, diags: []diag{repaired}, action: "without \"@\""},
		{name: "empty side", raw: "<@example.org>",
			want: []string{"@example.org"}, diags: []diag{repaired}, action: "empty side"},
		{name: "characters outside atext", raw: "<a,b(c)@example.org>",
			want: []string{"a,b(c)@example.org"}, diags: []diag{repaired}, action: "does not allow"},
		{name: "whitespace inside brackets", raw: "< a@example.org > <b@exam\t ple.org>",
			want: []string{"a@example.org", "b@example.org"}, diags: []diag{repaired}, action: "whitespace"},
		{name: "invalid UTF-8", raw: "<caf\xe9@example.org>",
			want: []string{"caf\xe9@example.org"}, diags: []diag{repaired}, action: "UTF-8"},
		{name: "missing brackets", raw: "a@example.org b@example.org",
			want: []string{"a@example.org", "b@example.org"}, diags: []diag{repaired}, action: "as identifiers"},
		{name: "truncated", raw: "<a@example.org> <b@exam",
			want: []string{"a@example.org", "b@exam"}, diags: []diag{cut}, action: "missing '>'"},
		{name: "truncated before trailing whitespace", raw: "<a@example.org \t",
			want: []string{"a@example.org"}, diags: []diag{cut}},
		{name: "unclosed before the next", raw: "<a@example.org <b@example.org>",
			want: []string{"a@example.org", "b@example.org"}, diags: []diag{repaired}, action: "missing '>'"},
		{name: "unclosed trailing comment", raw: "<a@example.org> (unterminated",
			want: []string{"a@example.org"}, diags: []diag{repaired}, action: "'('"},
		{name: "unclosed comment before an identifier", raw: "<a@example.org> (unterminated <b@example.org>",
			want: []string{"a@example.org", "b@example.org"}, diags: []diag{repaired}},
		{name: "commas", raw: "<a@example.org>, <b@example.org>,<c@example.org>",
			want: []string{"a@example.org", "b@example.org", "c@example.org"}, diags: []diag{repaired}, action: "commas"},
		{name: "RFC 822 phrase", raw: `Your message of "Wed, 22 Jan 2020 11:11:25 -0500" <a@example.org>`,
			want: []string{"a@example.org"}, diags: []diag{repaired}, action: "ignored that text"},
		{name: "address in a phrase is not an identifier", raw: "message from john@example.org <a@example.org>",
			want: []string{"a@example.org"}, diags: []diag{repaired}},
		{name: "stray closers", raw: "> <a@example.org> ) >",
			want: []string{"a@example.org"}, diags: []diag{repaired}},
		{name: "nothing usable", raw: "<> ()",
			invalid: []string{"<>"}, diags: []diag{unusable, partial}},
		{name: "words only", raw: "no identifiers here",
			diags: []diag{repaired}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var h mail.Header
			h.Set("References", test.raw)
			for name, got := range map[string]mail.MsgIDListResult{
				"ParseMsgIDListTolerant":   mail.ParseMsgIDListTolerant(test.raw),
				"Header.MsgIDListTolerant": h.MsgIDListTolerant("References"),
			} {
				if !reflect.DeepEqual(got.IDs, test.want) {
					t.Errorf("%s: IDs = %q, want %q", name, got.IDs, test.want)
				}
				var invalid []string
				for _, e := range got.Entries {
					if e.Raw == "" || !strings.Contains(test.raw, e.Raw) {
						t.Errorf("%s: entry Raw %q is not a part of the value", name, e.Raw)
					}
					if e.ID == "" {
						invalid = append(invalid, e.Raw)
					}
				}
				if !reflect.DeepEqual(invalid, test.invalid) {
					t.Errorf("%s: invalid entries = %q, want %q", name, invalid, test.invalid)
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
		})
	}
}

func TestMessageIDTolerant(t *testing.T) {
	tests := []struct {
		raw   string
		want  string
		diags int
	}{
		{"<123@asdf>", "123@asdf", 0},
		{"(c) <a@example.org> (c)", "a@example.org", 0},
		{"<a@example.org> (unterminated", "a@example.org", 2},
		{"a@example.org", "a@example.org", 1},
		{"abc123", "abc123", 1},
		{"<a@b@c>", "a@b@c", 1},
		{"<a@example.org> <b@example.org>", "a@example.org", 1},
		{"<a@example.org", "a@example.org", 1},
		{"two words", "", 1},
		{"<>", "", 2},
	}
	for _, test := range tests {
		var h mail.Header
		h.Set("Message-Id", test.raw)
		got := h.MessageIDTolerant()
		if got.ID != test.want || got.Raw != test.raw || len(got.Diagnostics) != test.diags {
			t.Errorf("MessageIDTolerant(%q) = %q, raw %q, %v; want %q and %d diagnostics", test.raw, got.ID, got.Raw, got.Diagnostics, test.want, test.diags)
		}
		for _, d := range got.Diagnostics {
			if d.Field != "Message-Id" || d.Code.Group() != "message_id" {
				t.Errorf("MessageIDTolerant(%q): diagnostic %v", test.raw, d)
			}
		}
	}
	var h mail.Header
	if got := h.MessageIDTolerant(); got.ID != "" || got.Raw != "" || got.Diagnostics != nil {
		t.Errorf("missing field: %+v", got)
	}
	if got := h.MsgIDListTolerant("References"); got.IDs != nil || got.Entries != nil || got.Diagnostics != nil {
		t.Errorf("missing field: %+v", got)
	}
}

func TestMsgIDListTolerant_bounds(t *testing.T) {
	got := mail.ParseMsgIDListTolerant(strings.Repeat("<a@example.org> ", 20000))
	if len(got.IDs) != 16384 || !hasDiag(got.Diagnostics, message.CodeMessageIDPartialList, message.SeverityLimitExceeded) {
		t.Errorf("entry cap: %d identifiers, %v", len(got.IDs), diagCodes(got.Diagnostics))
	}
	got = mail.ParseMsgIDListTolerant(strings.Repeat("<> ", 500) + "<a@example.org>")
	if len(got.Diagnostics) != 64 || got.Diagnostics[63].Code != message.CodeLimitDiagnosticCount || !reflect.DeepEqual(got.IDs, []string{"a@example.org"}) {
		t.Errorf("diagnostic cap: %d diagnostics, IDs %q", len(got.Diagnostics), got.IDs)
	}
	for name, raw := range map[string]string{
		"open angles":                  strings.Repeat("<", 200000),
		"open angle words":             strings.Repeat("<a ", 100000),
		"hidden angles":                "<" + strings.Repeat(` [<] "<"`, 50000),
		"comments closing in literals": "<a " + strings.Repeat("(c [x)<] ", 40000),
		"close angles":                 strings.Repeat(">", 200000),
		"open comments":                strings.Repeat("(", 200000) + "<a@example.org>",
		"quotes":                       strings.Repeat(`"`, 199999),
		"backslashes":                  strings.Repeat(`\`, 199999),
		"bare identifiers":             strings.Repeat("a@b ", 100000),
		"words":                        strings.Repeat("x ", 200000),
		"commas":                       strings.Repeat(",", 400000),
		"one long word":                strings.Repeat("x", 400000),
		"one long id":                  "<" + strings.Repeat("x", 400000) + "@y>",
		"alternating":                  strings.Repeat(`<"(>`, 100000),
		"NUL identifiers":              strings.Repeat("<\x00>", 100000),
		"at signs in angle":            "<" + strings.Repeat("@", 400000) + ">",
	} {
		t.Run(name, func(t *testing.T) {
			got := mail.ParseMsgIDListTolerant(raw)
			if len(got.Diagnostics) > 64 || len(got.Entries) > 16384 || len(got.IDs) > len(got.Entries) {
				t.Errorf("%d diagnostics, %d entries, %d identifiers", len(got.Diagnostics), len(got.Entries), len(got.IDs))
			}
			if len(got.Diagnostics) == 0 && name != "one long id" {
				t.Errorf("a damaged field reported nothing")
			}
		})
	}
}
