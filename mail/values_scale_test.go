package mail

import (
	"testing"

	"github.com/saeroun-ai/emlkit/internal/scaletest"
)

// TestComplexityHeaderValues holds the parsers of structured header fields to
// linear cost on values that open, nest or leave unfinished one construct —
// a comment, a quoted string, an address, a group, a list item — as often as
// their length allows (ROB-120-002). A field may be 64 KiB under the default
// budgets of a parse, and a header holds many.
func TestComplexityHeaderValues(t *testing.T) {
	parsers := []struct {
		name  string
		parse func(v string)
	}{
		{"address-list", func(v string) { ParseAddressListTolerant(v, nil) }},
		{"date", func(v string) { ParseDateTolerant(v) }},
		{"message-id-list", func(v string) { ParseMsgIDListTolerant(v) }},
		{"list-urls", func(v string) { ParseListURLs(v) }},
		{"list-id", func(v string) { ParseListID(v) }},
		{"authentication-results", func(v string) { ParseAuthenticationResults("mx.example.org; " + v) }},
		{"received", func(v string) { ParseReceived("from a.example (b [192.0.2.1]) by c " + v) }},
		{"strict-accessors", func(v string) {
			var h Header
			h.Set("To", v)
			h.Set("Date", v)
			h.Set("References", v)
			h.Set("List-Unsubscribe", v)
			h.AddressList("To")
			h.Date()
			h.MsgIDList("References")
			h.ListCommandURLList("List-Unsubscribe")
		}},
	}
	const n = 2 << 10
	for _, p := range parsers {
		for _, c := range scaletest.ValueCases() {
			t.Run(p.name+"/"+c.Name, func(t *testing.T) {
				scaletest.Check(t, c.Build(n), c.Build(4*n), p.parse)
			})
		}
	}
}
