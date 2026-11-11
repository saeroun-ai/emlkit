package icalendar

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// fuzzInput caps what one fuzz input may be.
const fuzzInput = 64 << 10

// tight makes every budget small enough for fuzz inputs to reach.
var tight = &Options{Limits: Limits{
	MaxInputBytes: 8 << 10, MaxLineBytes: 256, MaxLines: 300, MaxDepth: 3,
	MaxComponents: 12, MaxProperties: 24, MaxParams: 4, MaxAttendees: 3,
	MaxAttachmentBytes: 32, MaxIssues: 24,
}}

func FuzzParse(f *testing.F) {
	for _, s := range []string{invite, update, cancel, reply, allDay, recurring, outlook, google} {
		f.Add([]byte(s))
	}
	for _, tc := range malformed {
		f.Add([]byte(tc.in))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > fuzzInput {
			data = data[:fuzzInput]
		}
		for _, opts := range []*Options{nil, tight} {
			a := ParseBytes(data, opts)
			if b := ParseBytes(data, opts); !reflect.DeepEqual(a, b) {
				t.Fatal("two parses of one input differ")
			}
			checkBounds(t, a, opts.limits())
			Classify(a, func(string) *Event {
				if len(a.Events) > 0 {
					return a.Events[len(a.Events)-1]
				}
				return nil
			})
		}
	})
}

// checkBounds checks that what a parse keeps is within its limits.
func checkBounds(t *testing.T, cal *Calendar, lim Limits) {
	t.Helper()
	if len(cal.Issues) > lim.MaxIssues+1 {
		t.Fatalf("%d issues, limit %d", len(cal.Issues), lim.MaxIssues)
	}
	n := 0
	var walk func(c *Component, depth int)
	walk = func(c *Component, depth int) {
		n++
		if depth > lim.MaxDepth || len(c.Properties) > lim.MaxProperties {
			t.Fatalf("%s at depth %d with %d properties", c.Name, depth, len(c.Properties))
		}
		for _, p := range c.Properties {
			if len(p.Params) > lim.MaxParams || len(p.Value) > lim.MaxLineBytes || len(p.Raw) > 2*lim.MaxLineBytes {
				t.Fatalf("property %s: %d params, value %d, raw %d bytes", p.Name, len(p.Params), len(p.Value), len(p.Raw))
			}
			for _, q := range p.Params {
				if len(q.Values) > lim.MaxParams {
					t.Fatalf("parameter %s with %d values", q.Name, len(q.Values))
				}
			}
		}
		for _, ch := range c.Children {
			walk(ch, depth+1)
		}
	}
	for _, r := range cal.Roots {
		walk(r, 1)
	}
	if n > lim.MaxComponents {
		t.Fatalf("%d components, limit %d", n, lim.MaxComponents)
	}
	attached := 0
	for _, e := range cal.Events {
		if len(e.Attendees) > lim.MaxAttendees {
			t.Fatalf("%d attendees, limit %d", len(e.Attendees), lim.MaxAttendees)
		}
		for _, s := range []string{e.UID, e.Summary, e.Description, e.Location} {
			if !utf8.ValidString(s) {
				t.Fatalf("invalid UTF-8 in %q", s)
			}
		}
		for _, a := range e.Attendees {
			if !utf8.ValidString(a.Address) || !utf8.ValidString(a.CommonName) {
				t.Fatalf("invalid UTF-8 in attendee %+v", a)
			}
		}
		for _, a := range e.Attachments {
			attached += len(a.Data)
		}
	}
	if attached > lim.MaxAttachmentBytes {
		t.Fatalf("%d attachment bytes, limit %d", attached, lim.MaxAttachmentBytes)
	}
}

// TestLargeInputs parses inputs of a few MiB at the default limits, shaped
// to make any quadratic path show: one text property with many joined lines,
// one line of many folds, deep nesting, many attendees and blank lines.
func TestLargeInputs(t *testing.T) {
	head := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:u\r\n"
	for name, in := range map[string]string{
		"joins":     head + "DESCRIPTION:a\r\n" + strings.Repeat("stray\r\n", 300000),
		"folds":     head + "SUMMARY:a\r\n" + strings.Repeat(" x\r\n", 600000),
		"nesting":   strings.Repeat("BEGIN:X\r\n", 300000),
		"attendees": head + strings.Repeat("ATTENDEE;CN=\"a, b\":mailto:a@x\r\n", 60000),
		"blank":     head + strings.Repeat("\r\n", 2<<20),
		"one line":  head + "SUMMARY:" + strings.Repeat("x", 3<<20),
	} {
		cal := ParseBytes([]byte(in), nil)
		checkBounds(t, cal, DefaultLimits())
		if len(cal.Issues) == 0 {
			t.Errorf("%s: no issues", name)
		}
	}
}
