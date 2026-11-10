package mail_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// diagCodes renders diagnostics as "severity code", in order.
func diagCodes(diags []message.Diagnostic) []string {
	var l []string
	for _, d := range diags {
		l = append(l, d.Severity.String()+" "+string(d.Code))
	}
	return l
}

// hasDiag reports whether diags holds one with this code and severity.
func hasDiag(diags []message.Diagnostic, code message.Code, severity message.Severity) bool {
	for _, d := range diags {
		if d.Code == code && d.Severity == severity {
			return true
		}
	}
	return false
}

// A wantDiag names a diagnostic a test expects by its code and severity.
type wantDiag struct {
	code     message.Code
	severity message.Severity
}

// wantsDiag reports whether want lists the code and severity of d.
func wantsDiag(want []wantDiag, d message.Diagnostic) bool {
	for _, w := range want {
		if w.code == d.Code && w.severity == d.Severity {
			return true
		}
	}
	return false
}

// actions joins the Action of every diagnostic, for substring checks.
func actions(diags []message.Diagnostic) string {
	var l []string
	for _, d := range diags {
		l = append(l, d.Action)
	}
	return strings.Join(l, " | ")
}

func formatAddrs(l []*mail.Address) []string {
	var out []string
	for _, a := range l {
		out = append(out, a.Name+"|"+a.Address)
	}
	return out
}

// TestAddressListTolerant_matchesStrict checks that a list net/mail accepts
// yields the same addresses from the tolerant parser, and no diagnostics.
func TestAddressListTolerant_matchesStrict(t *testing.T) {
	deep := strings.Repeat("(", 2000) + strings.Repeat(")", 2000)
	for _, raw := range []string{
		"a@example.org",
		"Mitsuha Miyamizu <mitsuha.miyamizu@example.org>,Han Solo <hanibunny@example.org>",
		"a@example.org (c), <b@example.org> (c)",
		"a@example.org (x (y) z)",
		"<a@example.org> " + deep + ", <b@example.org> " + deep,
		`<a@example.org> (\)\(\\), <b@example.org>`,
		"g: a@example.org, b@example.org;",
		"g:;, a@example.org",
		"undisclosed-recipients:;",
		"g: <a@example.org> " + deep + ";" + deep,
		strings.Repeat("g: <a@example.org>;, ", 50) + "<b@example.org>",
		`"Doe, John" <jd@example.org>, =?utf-8?q?J=C3=B6rg?= <j@example.org>`,
		`=?utf-8?q?J=C3=B6rg?= =?utf-8?q?_M=C3=BCller?= <j@example.org>`,
		"a@example.org,, b@example.org,",
		`"quoted@local"@example.org, user@[192.0.2.1], "a b" <c@[2001:db8::1]>`,
		"John Q. Public <jqp@example.org>",
		`"<not@an.address>" <real@example.org>, "semi;colon: here" <s@example.org>`,
		"user@xn--bcher-kva.example",
	} {
		want, err := mail.ParseAddressList(raw)
		if err != nil {
			t.Errorf("ParseAddressList(%.60q): %v; the fixture must be valid", raw, err)
			continue
		}
		got := mail.ParseAddressListTolerant(raw, nil)
		if !reflect.DeepEqual(got.Addresses, want) {
			t.Errorf("ParseAddressListTolerant(%.60q) = %v, want %v", raw, formatAddrs(got.Addresses), formatAddrs(want))
		}
		if len(got.Diagnostics) != 0 {
			t.Errorf("ParseAddressListTolerant(%.60q) reported %v for a valid list", raw, got.Diagnostics)
		}
		if got.Raw != raw {
			t.Errorf("ParseAddressListTolerant(%.60q).Raw = %.60q", raw, got.Raw)
		}
		if len(got.Entries) != len(want) {
			t.Errorf("ParseAddressListTolerant(%.60q) has %d entries, want %d", raw, len(got.Entries), len(want))
		}
	}
}

func TestAddressListTolerant(t *testing.T) {
	strictCommas := &mail.AddressOptions{StrictCommas: true}
	requireDomain := &mail.AddressOptions{RequireDomain: true}
	type diag = wantDiag
	recovered := func(code message.Code) diag { return diag{code, message.SeverityRecovered} }
	invalid := diag{message.CodeAddressInvalid, message.SeverityInvalidMetadata}
	partial := recovered(message.CodeAddressPartialList)
	repaired := recovered(message.CodeAddressInvalid)

	tests := []struct {
		name    string
		raw     string
		opts    *mail.AddressOptions
		want    []string // "name|address" of the usable addresses
		invalid []string // Raw of the entries without an address
		diags   []diag   // diagnostics that must be present
		action  string   // substring of some diagnostic's Action
	}{
		// ROB-040-001: partial lists.
		{name: "valid invalid valid", raw: "a@example.org, @@@, <b@example.org>",
			want: []string{"|a@example.org", "|b@example.org"}, invalid: []string{"@@@"}, diags: []diag{invalid, partial}},
		{name: "phrase without address", raw: "a@example.org, John Doe, b@example.org",
			want: []string{"|a@example.org", "|b@example.org"}, invalid: []string{"John Doe"}, diags: []diag{invalid, partial}},
		{name: "empty angle address", raw: "John <>, b@example.org",
			want: []string{"|b@example.org"}, invalid: []string{"John <>"}, diags: []diag{invalid, partial}},
		{name: "domain only and local with at", raw: "@example.org, user@, ok@example.org",
			want: []string{"|ok@example.org"}, invalid: []string{"@example.org", "user@"}, diags: []diag{invalid, partial}},
		{name: "control character in address", raw: "a\x00b@example.org, ok@example.org",
			want: []string{"|ok@example.org"}, invalid: []string{"a\x00b@example.org"}, diags: []diag{invalid, partial}},
		{name: "all invalid", raw: "<>, @",
			invalid: []string{"<>", "@"}, diags: []diag{invalid, partial}},

		// ROB-040-002: unquoted commas and local-only addresses.
		{name: "unquoted comma", raw: "Doe, John <jd@example.org>, x@example.org",
			want: []string{"Doe, John|jd@example.org", "|x@example.org"}, diags: []diag{recovered(message.CodeAddressUnquotedComma)}},
		{name: "unquoted comma suffix", raw: "Warren Worthington, Jr. <warren@example.org>",
			want: []string{"Warren Worthington, Jr.|warren@example.org"}, diags: []diag{recovered(message.CodeAddressUnquotedComma)}},
		{name: "two unquoted comma names", raw: "Smith, John <js@example.org>, Doe, Jane (ACME) <jd@example.org>",
			want: []string{"Smith, John|js@example.org", "Doe, Jane|jd@example.org"}},
		{name: "unquoted comma in group", raw: "team: Doe, John <jd@example.org>;",
			want: []string{"Doe, John|jd@example.org"}, diags: []diag{recovered(message.CodeAddressUnquotedComma)}},
		{name: "strict commas", raw: "Doe, John <jd@example.org>", opts: strictCommas,
			want: []string{"|Doe", "John|jd@example.org"}, diags: []diag{recovered(message.CodeAddressLocalOnly)}},
		{name: "local only", raw: "postmaster, b@example.org, Root <root>, <admin>",
			want: []string{"|postmaster", "|b@example.org", "Root|root", "|admin"}, diags: []diag{recovered(message.CodeAddressLocalOnly)}},
		{name: "local only refused", raw: "postmaster, b@example.org, Root <root>", opts: requireDomain,
			want: []string{"|b@example.org"}, invalid: []string{"postmaster", "Root <root>"}, diags: []diag{invalid, partial}},
		{name: "words before a bare address stay separate", raw: "alice, bob, carol@example.org",
			want: []string{"|alice", "|bob", "|carol@example.org"}, diags: []diag{recovered(message.CodeAddressLocalOnly)}},

		// ROB-040-004: obsolete and broken syntax.
		{name: "obsolete route", raw: "<@a.example,@b.example:user@example.org>, c@example.org",
			want: []string{"|user@example.org", "|c@example.org"}, diags: []diag{repaired}, action: "obsolete route"},
		{name: "obsolete route with name", raw: "Joe <@a.example:joe@example.org>",
			want: []string{"Joe|joe@example.org"}, diags: []diag{repaired}, action: "obsolete route"},
		{name: "empty group and repeated separators", raw: ",, g:;, a@example.org,,; b@example.org ;;",
			want: []string{"|a@example.org", "|b@example.org"}, diags: []diag{repaired}, action: "read ';' as a separator"},
		{name: "semicolon separators", raw: "a@example.org; B <b@example.org>; c@example.org",
			want: []string{"|a@example.org", "B|b@example.org", "|c@example.org"}, diags: []diag{repaired}},
		{name: "missing closing angle", raw: "John <a@example.org",
			want: []string{"John|a@example.org"}, diags: []diag{repaired}, action: "supplied the missing '>'"},
		{name: "missing closing angle before comma", raw: "John <a@example.org, b@example.org",
			want: []string{"John|a@example.org", "|b@example.org"}, diags: []diag{repaired}},
		{name: "doubled angles", raw: "John <<a@example.org>>, b@example.org",
			want: []string{"John|a@example.org", "|b@example.org"}, diags: []diag{repaired}, action: "repeated '<'"},
		{name: "missing opening angle", raw: "John a@example.org>",
			want: []string{"John|a@example.org"}, diags: []diag{repaired}},
		{name: "missing angles", raw: "John Doe john@example.org",
			want: []string{"John Doe|john@example.org"}, diags: []diag{repaired}, action: "display name"},
		{name: "missing comma between angle addresses", raw: "<a@example.org> B <b@example.org>",
			want: []string{"|a@example.org", "B|b@example.org"}, diags: []diag{repaired}, action: "next address"},
		{name: "missing comma between bare addresses", raw: "a@example.org b@example.org",
			want: []string{"|a@example.org", "|b@example.org"}, diags: []diag{repaired}},
		{name: "leading comment", raw: "(c) <a@example.org>, (d) b@example.org",
			want: []string{"|a@example.org", "|b@example.org"}, diags: []diag{repaired}},
		{name: "address as display name", raw: "info@example.org <info@example.org>",
			want: []string{"info@example.org|info@example.org"}, diags: []diag{repaired}, action: "unquoted '@'"},
		{name: "brackets in display name", raw: "John Doe [ACME] <jd@example.org>",
			want: []string{"John Doe [ACME]|jd@example.org"}, diags: []diag{repaired}},
		{name: "mailto prefixes", raw: "<mailto:a@example.org>, mailto:b@example.org",
			want: []string{"|a@example.org", "|b@example.org"}, diags: []diag{repaired}, action: "mailto"},
		{name: "unclosed quote", raw: `"John Doe <a@example.org>, b@example.org`,
			want: []string{"John Doe|a@example.org", "|b@example.org"}, diags: []diag{repaired}, action: "unmatched '\"'"},
		{name: "unclosed comment", raw: "a@example.org (oops, b@example.org",
			want: []string{"|a@example.org", "|b@example.org"}, diags: []diag{repaired}},
		{name: "unclosed group", raw: "team: a@example.org, b@example.org",
			want: []string{"|a@example.org", "|b@example.org"}, diags: []diag{repaired}, action: "kept the members"},
		{name: "nested group", raw: "outer: inner: a@example.org;;, b@example.org",
			want: []string{"|a@example.org", "|b@example.org"}, diags: []diag{recovered(message.CodeAddressGroupDepth)}},
		{name: "dots net/mail refuses", raw: "john..doe@example.org, .x@example.org",
			want: []string{"|john..doe@example.org", "|.x@example.org"}, diags: []diag{repaired}},
		{name: "tagged IPv6 literal net/mail refuses", raw: "user@[IPv6:2001:db8::1], N <n@[IPv6:2001:db8::2]>",
			want: []string{"|user@[IPv6:2001:db8::1]", "N|n@[IPv6:2001:db8::2]"}, diags: []diag{repaired}},
		{name: "address in front of empty angle brackets", raw: "a@example.org <>, B b@example.org <",
			want: []string{"|a@example.org", "B|b@example.org"}, diags: []diag{repaired}, action: "hold no address"},
		{name: "comma directly before an angle address separates", raw: "postmaster, <a@example.org>",
			want: []string{"|postmaster", "|a@example.org"}, diags: []diag{recovered(message.CodeAddressLocalOnly)}},
		{name: "empty group inside angle brackets", raw: "<Undisclosed-Recipient:;>, a@example.org",
			want: []string{"|a@example.org"}, diags: []diag{repaired}, action: "empty group"},
		{name: "stray closers between addresses", raw: "a@example.org, >, ), b@example.org",
			want: []string{"|a@example.org", "|b@example.org"}, diags: []diag{repaired}, action: "unmatched delimiters"},
		{name: "encoded word in an unknown charset", raw: "=?x-unknown?q?abc?= <a@example.org>, b@example.org",
			want:  []string{"=?x-unknown?q?abc?=|a@example.org", "|b@example.org"},
			diags: []diag{{message.CodeRFC2047UnknownCharset, message.SeverityUnsupported}}, action: "kept the encoded-word as written"},
		{name: "encoded word with a comma", raw: "=?utf-8?q?Doe,_John?= <jd@example.org>",
			want: []string{"Doe, John|jd@example.org"}, diags: []diag{repaired}},

		// ROB-040-005: internationalized addresses and undecodable text.
		{name: "raw UTF-8", raw: "Jörg <jörg@bücher.example>, 김철수 <user@도메인.example>",
			want:  []string{"Jörg|jörg@bücher.example", "김철수|user@도메인.example"},
			diags: []diag{{message.CodeAddressRawUTF8, message.SeverityInfo}}},
		{name: "undecodable display name", raw: "J\xf6rg <j@example.org>, \"M\xfcller\" <m@example.org>, ok@example.org",
			want:  []string{"J\xf6rg|j@example.org", "M\xfcller|m@example.org", "|ok@example.org"},
			diags: []diag{{message.CodeAddressRawUTF8, message.SeverityUnsupported}}},
		{name: "undecodable local part", raw: "caf\xe9@example.org",
			want:  []string{"|caf\xe9@example.org"},
			diags: []diag{{message.CodeAddressRawUTF8, message.SeverityUnsupported}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var h mail.Header
			h.Set("To", test.raw)
			for name, got := range map[string]mail.AddressListResult{
				"ParseAddressListTolerant":   mail.ParseAddressListTolerant(test.raw, test.opts),
				"Header.AddressListTolerant": h.AddressListTolerant("To", test.opts),
			} {
				if addrs := formatAddrs(got.Addresses); !reflect.DeepEqual(addrs, test.want) {
					t.Errorf("%s: addresses = %q, want %q", name, addrs, test.want)
				}
				var invalidRaw []string
				usable := 0
				for _, e := range got.Entries {
					if e.Raw == "" || !strings.Contains(test.raw, e.Raw) {
						t.Errorf("%s: entry Raw %q is not a part of the value", name, e.Raw)
					}
					if e.Address == nil {
						invalidRaw = append(invalidRaw, e.Raw)
						if !hasDiag(e.Diagnostics, message.CodeAddressInvalid, message.SeverityInvalidMetadata) {
							t.Errorf("%s: invalid entry %q carries %v", name, e.Raw, e.Diagnostics)
						}
					} else {
						usable++
					}
				}
				if !reflect.DeepEqual(invalidRaw, test.invalid) {
					t.Errorf("%s: invalid entries = %q, want %q", name, invalidRaw, test.invalid)
				}
				if usable != len(got.Addresses) {
					t.Errorf("%s: %d entries have an address, Addresses has %d", name, usable, len(got.Addresses))
				}
				for _, d := range test.diags {
					if !hasDiag(got.Diagnostics, d.code, d.severity) {
						t.Errorf("%s: no %v %s diagnostic in %v", name, d.severity, d.code, got.Diagnostics)
					}
				}
				if len(got.Diagnostics) == 0 {
					t.Errorf("%s: a repaired list reported nothing", name)
				}
				if test.action != "" && !strings.Contains(actions(got.Diagnostics), test.action) {
					t.Errorf("%s: no diagnostic action mentions %q: %q", name, test.action, actions(got.Diagnostics))
				}
				if got.Raw != test.raw {
					t.Errorf("%s: Raw = %q", name, got.Raw)
				}
				wantField := ""
				if name == "Header.AddressListTolerant" {
					wantField = "To"
				}
				for _, d := range got.Diagnostics {
					if d.Field != wantField {
						t.Errorf("%s: diagnostic Field = %q, want %q", name, d.Field, wantField)
					}
				}
			}
			// The strict accessor keeps refusing what it refused before.
			if _, err := h.AddressList("To"); err == nil && len(test.invalid) > 0 {
				t.Errorf("AddressList accepted a list with invalid entries")
			}
		})
	}
}

func TestAddressListTolerant_groups(t *testing.T) {
	got := mail.ParseAddressListTolerant("a@example.org, team: b@example.org, C <c@example.org>;, d@example.org", nil)
	var groups []string
	for _, e := range got.Entries {
		groups = append(groups, e.Group)
	}
	if want := []string{"", "team", "team", ""}; !reflect.DeepEqual(groups, want) {
		t.Errorf("groups = %q, want %q", groups, want)
	}
	if len(got.Diagnostics) != 0 {
		t.Errorf("diagnostics = %v", got.Diagnostics)
	}

	var h mail.Header
	if got := h.AddressListTolerant("To", nil); got.Raw != "" || got.Addresses != nil || got.Entries != nil || got.Diagnostics != nil {
		t.Errorf("missing field: %+v", got)
	}
}

// TestAddressListTolerant_bounds checks the structural bounds: the cap on
// entries, the cap on diagnostics, and that deep or unbalanced nesting is
// handled in one pass without a panic.
func TestAddressListTolerant_bounds(t *testing.T) {
	t.Run("diagnostic cap", func(t *testing.T) {
		got := mail.ParseAddressListTolerant(strings.Repeat("<>, ", 200)+"a@example.org", nil)
		if len(got.Entries) != 201 || len(got.Addresses) != 1 {
			t.Fatalf("entries = %d, addresses = %d", len(got.Entries), len(got.Addresses))
		}
		if len(got.Diagnostics) != 64 {
			t.Fatalf("diagnostics = %d, want 64", len(got.Diagnostics))
		}
		last := got.Diagnostics[63]
		if last.Code != message.CodeLimitDiagnosticCount || last.Severity != message.SeverityLimitExceeded {
			t.Errorf("last diagnostic = %v", last)
		}
	})
	t.Run("entry cap", func(t *testing.T) {
		got := mail.ParseAddressListTolerant(strings.Repeat("a@example.org,", 20000), nil)
		if len(got.Entries) != 16384 || len(got.Addresses) != 16384 {
			t.Fatalf("entries = %d, addresses = %d", len(got.Entries), len(got.Addresses))
		}
		if !hasDiag(got.Diagnostics, message.CodeAddressPartialList, message.SeverityLimitExceeded) {
			t.Errorf("diagnostics = %v", got.Diagnostics)
		}
	})
	one := []string{"|a@example.org"}
	for name, test := range map[string]struct {
		raw  string
		want []string
	}{
		"unclosed comments":      {strings.Repeat("(", 100000) + "a@example.org", one},
		"surplus closers":        {"a@example.org" + strings.Repeat(")", 100000), one},
		"nested groups":          {strings.Repeat("g:", 50000) + "a@example.org" + strings.Repeat(";", 50000), one},
		"angle brackets":         {strings.Repeat("<", 50000) + "a@example.org" + strings.Repeat(">", 50000), one},
		"unclosed angles":        {strings.Repeat("<a@example.org ", 3), []string{"|a@example.org", "|a@example.org", "|a@example.org"}},
		"many unclosed angles":   {strings.Repeat("<a@example.org ", 5000), nil},
		"quotes":                 {strings.Repeat(`"`, 99999) + " <a@example.org>", nil},
		"encoded-word openers":   {strings.Repeat("=?", 50000) + " <a@example.org", nil},
		"brackets":               {strings.Repeat("[", 50000) + "a@example.org", one},
		"backslashes":            {strings.Repeat(`\`, 99999), nil},
		"phrase words":           {strings.Repeat("word, ", 20000) + "x@example.org", nil},
		"phrase words to angle":  {strings.Repeat("word, ", 20000) + "<x@example.org>", nil},
		"route commas":           {"<" + strings.Repeat("@a,", 30000) + ":x@example.org>", nil},
		"at signs":               {strings.Repeat("@", 100000), nil},
		"colons":                 {strings.Repeat("a:", 50000), nil},
		"long local-only run":    {strings.Repeat("a ", 50000), nil},
		"NUL bytes":              {strings.Repeat("\x00", 50000) + "<a@example.org>", nil},
		"invalid UTF-8":          {strings.Repeat("\xff", 50000) + " <a@example.org>", nil},
		"unclosed quote at end":  {strings.Repeat("a@example.org, ", 2000) + `"`, nil},
		"alternating delimiters": {strings.Repeat(`"(<[`, 20000), nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := mail.ParseAddressListTolerant(test.raw, nil)
			if test.want != nil && !reflect.DeepEqual(formatAddrs(got.Addresses), test.want) {
				t.Errorf("addresses = %.200q, want %q", formatAddrs(got.Addresses), test.want)
			}
			if len(got.Diagnostics) > 64 || len(got.Entries) > 16384 {
				t.Errorf("%d diagnostics, %d entries", len(got.Diagnostics), len(got.Entries))
			}
			if len(got.Diagnostics) == 0 {
				t.Errorf("a damaged list reported nothing")
			}
			for _, e := range got.Entries {
				if e.Raw == "" {
					t.Fatalf("an entry has no Raw text")
				}
			}
		})
	}
}
