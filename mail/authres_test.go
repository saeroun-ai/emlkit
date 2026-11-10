package mail_test

import (
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

func TestParseAuthenticationResults(t *testing.T) {
	r := mail.ParseAuthenticationResults(`mx.example.com 1;
	 dkim=pass (2048-bit key; "quoted; semi") header.d=sender.example header.s=sel header.b="AbC+/=";
	 spf = softfail (domain of transitioning) smtp.mailfrom=bounce@sender.example;
	 dmarc=fail reason="p=reject (policy)" header.from=sender.example;
	 arc/1=none`)
	if r.AuthServID != "mx.example.com" || r.Version != "1" || r.None || len(r.Methods) != 4 || len(r.Diagnostics) != 0 {
		t.Fatalf("%+v", r)
	}
	dkim, _ := r.Method("dkim")
	if dkim.Result != "pass" || dkim.Prop("header", "d") != "sender.example" || dkim.Prop("header", "b") != "AbC+/=" || dkim.Comment != `2048-bit key; "quoted; semi"` {
		t.Errorf("dkim %+v", dkim)
	}
	spf, _ := r.Method("spf")
	if spf.Result != "softfail" || spf.Prop("smtp", "mailfrom") != "bounce@sender.example" {
		t.Errorf("spf %+v", spf)
	}
	dmarc, _ := r.Method("dmarc")
	if dmarc.Result != "fail" || dmarc.Reason != "p=reject (policy)" || dmarc.Prop("header", "from") != "sender.example" {
		t.Errorf("dmarc %+v", dmarc)
	}
	if arc, ok := r.Method("arc"); !ok || arc.Version != "1" || arc.Result != "none" {
		t.Errorf("arc %+v", arc)
	}

	for value, want := range map[string]struct {
		id      string
		none    bool
		methods int
		diags   int
	}{
		"mx.example.com; none":                   {"mx.example.com", true, 0, 0},
		"mx.example.com":                         {"mx.example.com", false, 0, 0},
		"":                                       {"", false, 0, 1},
		"; spf=pass":                             {"", false, 0, 1},
		"mx.example.com; garbage here; spf=pass": {"mx.example.com", false, 1, 1},
		"mx.example.com; =pass; dkim=; spf=pass": {"mx.example.com", false, 1, 2},
		"mx.example.com; spf=pass (unclosed; dkim=fail": {"mx.example.com", false, 1, 0},
		`mx.example.com; dkim=pass header.b="unclosed`:  {"mx.example.com", false, 1, 0},
	} {
		r := mail.ParseAuthenticationResults(value)
		if r.AuthServID != want.id || r.None != want.none || len(r.Methods) != want.methods || len(r.Diagnostics) != want.diags {
			t.Errorf("%q: id %q none %v methods %d diagnostics %v", value, r.AuthServID, r.None, len(r.Methods), r.Diagnostics)
		}
		for _, d := range r.Diagnostics {
			if d.Code != message.CodeAuthResultsInvalid {
				t.Errorf("%q: diagnostic %v", value, d.Code)
			}
		}
	}

	// Hostile values end, without a panic.
	for _, v := range []string{strings.Repeat("(", 100000), strings.Repeat(";", 100000), strings.Repeat("a=b ", 100000), strings.Repeat(`"\`, 50000), "\x00\xff;=;=\"", strings.Repeat("x=y;", 100000)} {
		mail.ParseAuthenticationResults(v)
	}
}

func TestHeader_AuthenticationResultsTrust(t *testing.T) {
	const raw = "Received: by internal.example.org\r\n" +
		"Authentication-Results: mx.example.org; spf=pass smtp.mailfrom=a@sender.example\r\n" +
		"Received: from sender.example by mx.example.org\r\n" +
		"Authentication-Results: other.example.net; dkim=pass\r\n" +
		"Received: from somewhere by sender.example\r\n" +
		"Authentication-Results: mx.example.org; spf=pass; dkim=pass; dmarc=pass\r\n" +
		"From: a@sender.example\r\n\r\n"
	e, err := message.Read(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	h := mail.Header{Header: e.Header}

	trust := func(opts *mail.AuthResultsOptions) []bool {
		var out []bool
		for _, r := range h.AuthenticationResults(opts) {
			out = append(out, r.Trusted)
		}
		return out
	}
	eq := func(a, b []bool) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	all := h.AuthenticationResults(nil)
	if len(all) != 3 || all[0].ReceivedAbove != 1 || all[1].ReceivedAbove != 2 || all[2].ReceivedAbove != 3 || all[0].Index != 1 {
		t.Fatalf("%+v", all)
	}
	// Nothing is trusted unless the caller names its servers.
	if got := trust(nil); !eq(got, []bool{false, false, false}) {
		t.Errorf("no options: %v", got)
	}
	// Named, with the position checked: the forged copy below the border is
	// not trusted though it claims the receiver's identifier.
	if got := trust(&mail.AuthResultsOptions{TrustedIDs: []string{"MX.example.org"}, MaxReceivedAbove: 1}); !eq(got, []bool{true, false, false}) {
		t.Errorf("id and position: %v", got)
	}
	if got := trust(&mail.AuthResultsOptions{TrustedIDs: []string{"mx.example.org"}}); !eq(got, []bool{false, false, false}) {
		t.Errorf("id, nothing may stand above: %v", got)
	}
	// Position not looked at: the forged copy passes, which is why this is
	// only for a border that strips foreign fields.
	if got := trust(&mail.AuthResultsOptions{TrustedIDs: []string{"mx.example.org"}, MaxReceivedAbove: -1}); !eq(got, []bool{true, false, true}) {
		t.Errorf("id only: %v", got)
	}
}
