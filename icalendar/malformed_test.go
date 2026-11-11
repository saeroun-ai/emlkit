package icalendar

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

// wrap puts event lines, LF-terminated, into a calendar with one event.
func wrap(lines string) string {
	return crlf("BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//t//t//EN\nBEGIN:VEVENT\nUID:u1\nDTSTAMP:20261111T000000Z\n" + lines + "END:VEVENT\nEND:VCALENDAR\n")
}

func utf16LE(s string) string {
	b := []byte{0xFF, 0xFE}
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u), byte(u>>8))
	}
	return string(b)
}

type malformedCase struct {
	name  string
	in    string
	opts  *Options
	codes []Code
	check func(t *testing.T, cal *Calendar)
}

func summary(want string) func(*testing.T, *Calendar) {
	return func(t *testing.T, cal *Calendar) {
		t.Helper()
		if e := one(t, cal); e.Summary != want {
			t.Errorf("summary = %q, want %q", e.Summary, want)
		}
	}
}

var malformed = []malformedCase{
	{"bom", utf8BOM + wrap("SUMMARY:x\n"), nil, []Code{CodeBOM}, summary("x")},
	{"bare lf", strings.ReplaceAll(wrap("SUMMARY:a\n b\n"), "\r\n", "\n"), nil, []Code{CodeBareLineEnding}, summary("ab")},
	{"bare cr", strings.ReplaceAll(wrap("SUMMARY:a\n b\n"), "\r\n", "\r"), nil, []Code{CodeBareLineEnding}, summary("ab")},
	{"cr cr lf", strings.ReplaceAll(wrap("SUMMARY:a\n b\n"), "\r\n", "\r\r\n"), nil, []Code{CodeBareLineEnding, CodeEmptyLine}, summary("ab")},
	{"fold splits utf-8", wrap("SUMMARY:\xED\x9A\n \x8C\xEC\x9D\x98\n"), nil, []Code{CodeFoldSplitsUTF8}, summary("회의")},
	{"tab fold and blank line in a fold", wrap("SUMMARY:a\n\n\tb\n"), nil, []Code{CodeEmptyLine}, summary("ab")},
	{"leading fold", " " + wrap("SUMMARY:x\n"), nil, []Code{CodeLeadingFold}, summary("x")},
	{"utf-16", utf16LE(wrap("SUMMARY:회의\n")), nil, []Code{CodeUTF16}, summary("회의")},
	{"missing ends", crlf("BEGIN:VCALENDAR\nVERSION:2.0\nBEGIN:VEVENT\nUID:u1\nSUMMARY:x\n"), nil, []Code{CodeMissingEnd}, func(t *testing.T, cal *Calendar) {
		if summary("x")(t, cal); len(cal.Issues) != 2 || cal.Raw.Closed {
			t.Errorf("issues = %v", cal.Issues)
		}
	}},
	{"end of the wrong component", wrap("BEGIN:VALARM\nACTION:DISPLAY\n"), nil, []Code{CodeMissingEnd}, func(t *testing.T, cal *Calendar) {
		if e := one(t, cal); e.Alarms != 1 || !e.Component.Closed || e.Component.Children[0].Closed {
			t.Errorf("event = %+v", e.Component)
		}
	}},
	{"stray end", wrap("END:VTODO\nSUMMARY:x\n"), nil, []Code{CodeStrayEnd}, summary("x")},
	{"end of the calendar inside an event", crlf("BEGIN:VCALENDAR\nVERSION:2.0\nBEGIN:VEVENT\nUID:u\nEND:VCALENDAR\n"), nil, []Code{CodeMissingEnd}, func(t *testing.T, cal *Calendar) {
		if len(cal.Issues) != 1 || !cal.Raw.Closed {
			t.Errorf("issues = %v", cal.Issues)
		}
	}},
	{"event inside an event", wrap("SUMMARY:a\nBEGIN:VEVENT\nUID:u2\nSUMMARY:b\n"), nil, []Code{CodeMissingEnd}, func(t *testing.T, cal *Calendar) {
		if len(cal.Events) != 2 || cal.Events[0].Summary != "a" || cal.Events[1].Summary != "b" {
			t.Errorf("events = %+v", cal.Events)
		}
	}},
	{"nested calendar", crlf("BEGIN:VCALENDAR\nVERSION:2.0\nBEGIN:VCALENDAR\nVERSION:2.0\nBEGIN:VEVENT\nUID:u\nEND:VEVENT\nEND:VCALENDAR\n"), nil, []Code{CodeMissingEnd, CodeMultipleCalendars}, func(t *testing.T, cal *Calendar) {
		if len(cal.Roots) != 2 || len(cal.Events) != 1 {
			t.Errorf("roots %d, events %d", len(cal.Roots), len(cal.Events))
		}
	}},
	{"content before the calendar", "Content-Type: text/calendar; method=REQUEST\r\n\r\n" + wrap("SUMMARY:x\n"), nil, []Code{CodeBeforeBegin, CodeEmptyLine}, summary("x")},
	{"content after the calendar", wrap("SUMMARY:x\n") + "--boundary42--\r\n", nil, []Code{CodeAfterEnd}, summary("x")},
	{"event without a calendar", crlf("BEGIN:VEVENT\nUID:u1\nSUMMARY:x\nEND:VEVENT\n"), nil, []Code{CodeMissingCalendar, CodeNoVersion}, func(t *testing.T, cal *Calendar) {
		if summary("x")(t, cal); !cal.Raw.Synthesized || cal.Raw.Line != 0 {
			t.Errorf("calendar = %+v", cal.Raw)
		}
	}},
	{"two calendars", wrap("SUMMARY:a\n") + wrap("SUMMARY:b\n"), nil, []Code{CodeMultipleCalendars}, func(t *testing.T, cal *Calendar) {
		if len(cal.Events) != 2 {
			t.Errorf("events = %d", len(cal.Events))
		}
	}},
	{"no calendar", "hello\r\nworld\r\n", nil, []Code{CodeBeforeBegin, CodeNoCalendar}, func(t *testing.T, cal *Calendar) {
		if cal.Raw != nil || len(cal.Events) != 0 || cal.Issues[0].Count != 2 {
			t.Errorf("calendar = %+v", cal)
		}
	}},
	{"empty input", "", nil, []Code{CodeNoCalendar}, nil},
	{"begin without a name", wrap("BEGIN:\nSUMMARY:x\n"), nil, []Code{CodeBadBegin}, summary("x")},
	{"raw line breaks in text", wrap("DESCRIPTION:line one\nline two\nJoin here: https://example.com/j/1\nhttps://example.com/j/2\nSUMMARY:x\n"), nil, []Code{CodeUnfoldedText}, func(t *testing.T, cal *Calendar) {
		e := one(t, cal)
		if want := "line one\nline two\nJoin here: https://example.com/j/1\nhttps://example.com/j/2"; e.Description != want || e.Summary != "x" {
			t.Errorf("description, summary = %q, %q", e.Description, e.Summary)
		}
		if p := e.Component.Prop("DESCRIPTION"); !strings.HasSuffix(p.Raw, "\nhttps://example.com/j/2") {
			t.Errorf("raw = %q", p.Raw)
		}
	}},
	{"no colon", wrap("DTSTART:20261120T100000Z\ngarbage\n"), nil, []Code{CodeNoColon}, func(t *testing.T, cal *Calendar) {
		if e := one(t, cal); e.Start.Kind != TimeUTC {
			t.Errorf("start = %+v", e.Start)
		}
	}},
	{"no colon after parameters", wrap("ATTENDEE;CN=x\n"), nil, []Code{CodeNoColon}, nil},
	{"bad name", wrap("DTSTART:20261120T100000Z\nNOT A NAME: x\n"), nil, []Code{CodeBadName}, nil},
	{"space around a name", wrap("SUMMARY :x\n"), nil, []Code{CodeNameSpace}, summary("x")},
	{"mixed case names", crlf("begin:vcalendar\nversion:2.0\nBegin:VEvent\nuid:u1\nSummary:x\ndtstart;tzid=Asia/Seoul:20261120T100000\nend:vevent\nend:vcalendar\n"), nil, []Code{CodeNameCase}, func(t *testing.T, cal *Calendar) {
		if e := one(t, cal); e.Summary != "x" || e.Start.Kind != TimeZoned || len(cal.Issues) != 1 {
			t.Errorf("event = %+v, issues %v", e, cal.Issues)
		}
	}},
	{"unterminated quote", wrap("ATTENDEE;CN=\"Kim:mailto:a@example.com\n"), nil, []Code{CodeParamQuote}, func(t *testing.T, cal *Calendar) {
		if a := one(t, cal).Attendees[0]; a.CommonName != "Kim" || a.Address != "a@example.com" {
			t.Errorf("attendee = %+v", a)
		}
	}},
	{"text after a quote", wrap("ATTENDEE;CN=\"Kim\"Jr:mailto:a@example.com\n"), nil, []Code{CodeParamAfterQuote}, func(t *testing.T, cal *Calendar) {
		if a := one(t, cal).Attendees[0]; a.CommonName != "KimJr" {
			t.Errorf("attendee = %+v", a)
		}
	}},
	{"unquoted uri parameter", wrap("ORGANIZER;SENT-BY=mailto:sec@example.com;CN=Boss:mailto:boss@example.com\n"), nil, []Code{CodeParamUnquotedURI}, func(t *testing.T, cal *Calendar) {
		if o := one(t, cal).Organizer; o.SentBy != "sec@example.com" || o.Address != "boss@example.com" || o.CommonName != "Boss" {
			t.Errorf("organizer = %+v", o)
		}
	}},
	{"parameter without value", wrap("SUMMARY;LANGUAGE:x\n"), nil, []Code{CodeParamNoValue}, summary("x")},
	{"encoded-word in CN", wrap("ATTENDEE;CN==?UTF-8?B?7J207KeA7J2A?=:mailto:a@example.com\n"), nil, []Code{CodeParamEncodedWord}, func(t *testing.T, cal *Calendar) {
		if a := one(t, cal).Attendees[0]; a.CommonName != "이지은" {
			t.Errorf("attendee = %+v", a)
		}
	}},
	{"tzid with utc", wrap("DTSTART;TZID=Asia/Seoul:20261120T100000Z\n"), nil, []Code{CodeTZIDWithUTC}, func(t *testing.T, cal *Calendar) {
		if d := one(t, cal).Start; d.Kind != TimeUTC || !d.Time.Equal(utc("20261120T100000Z")) {
			t.Errorf("start = %+v", d)
		}
	}},
	{"bad date", wrap("DTSTART:2026112\nDTEND:20261332T100000\n"), nil, []Code{CodeBadDateTime}, func(t *testing.T, cal *Calendar) {
		if e := one(t, cal); e.Start.Kind != TimeInvalid || e.Start.Raw != "2026112" || e.End.Kind != TimeInvalid {
			t.Errorf("start, end = %+v, %+v", e.Start, e.End)
		}
	}},
	{"extended date-time", wrap("DTSTART:2026-11-20T10:00:00Z\nDTEND:20261120t1100\n"), nil, []Code{CodeDateTimeLoose}, func(t *testing.T, cal *Calendar) {
		if e := one(t, cal); !e.Start.Time.Equal(utc("20261120T100000Z")) || e.End.Kind != TimeFloating || e.End.Wall.Hour() != 11 {
			t.Errorf("start, end = %+v, %+v", e.Start, e.End)
		}
	}},
	{"date without VALUE=DATE", wrap("DTSTART:20261120\n"), nil, []Code{CodeDateNoValue}, func(t *testing.T, cal *Calendar) {
		if e := one(t, cal); !e.AllDay {
			t.Errorf("start = %+v", e.Start)
		}
	}},
	{"floating", wrap("DTSTART:20261120T100000\n"), nil, nil, func(t *testing.T, cal *Calendar) {
		if d := one(t, cal).Start; d.Kind != TimeFloating || d.Known() || !d.Time.IsZero() {
			t.Errorf("start = %+v", d)
		}
	}},
	{"duplicate property", wrap("SUMMARY:a\nSUMMARY:b\n"), nil, []Code{CodeDuplicate}, summary("a")},
	{"no uid", crlf("BEGIN:VCALENDAR\nVERSION:2.0\nBEGIN:VEVENT\nSUMMARY:x\nEND:VEVENT\nBEGIN:VTODO\nEND:VTODO\nEND:VCALENDAR\n"), nil, []Code{CodeNoUID}, nil},
	{"bad sequence", wrap("SEQUENCE:abc\n"), nil, []Code{CodeBadInteger}, nil},
	{"end and duration", wrap("DTSTART:20261120T100000Z\nDTEND:20261120T110000Z\nDURATION:PT1H\n"), nil, []Code{CodeEndAndDuration}, nil},
	{"bad duration", wrap("DURATION:1H\n"), nil, []Code{CodeBadDuration}, nil},
	{"rrule with count and until", wrap("RRULE:FREQ=DAILY;COUNT=3;UNTIL=20261201\n"), nil, []Code{CodeRRuleCountUntil}, func(t *testing.T, cal *Calendar) {
		if r := one(t, cal).RRule; r.Count != 3 || r.Until.Kind != TimeDate || has(cal, CodeDateNoValue) {
			t.Errorf("rrule = %+v, issues %v", r, cal.Issues)
		}
	}},
	{"rrule without freq", wrap("RRULE:INTERVAL=0;BOGUS;;\n"), nil, []Code{CodeRRuleNoFreq, CodeRRuleInvalid}, nil},
	{"bad base64", wrap("ATTACH;ENCODING=BASE64;VALUE=BINARY:@@@@\n"), nil, []Code{CodeAttachmentBase64}, func(t *testing.T, cal *Calendar) {
		if a := one(t, cal).Attachments[0]; !a.Invalid || a.Data != nil {
			t.Errorf("attachment = %+v", a)
		}
	}},
	{"address without scheme", wrap("ATTENDEE:jieun@example.com\n"), nil, []Code{CodeAddressNoScheme}, func(t *testing.T, cal *Calendar) {
		if a := one(t, cal).Attendees[0]; a.Address != "jieun@example.com" {
			t.Errorf("attendee = %+v", a)
		}
	}},
	{"bad percent escape", wrap("ATTENDEE:mailto:a%zz@example.com\n"), nil, []Code{CodeAddressEscape}, nil},
	{"invalid utf-8", wrap("SUMMARY:a\xff b\n"), nil, []Code{CodeInvalidUTF8}, summary("a" + string(utf8.RuneError) + " b")},
	{"version 1.0", strings.Replace(wrap(""), "VERSION:2.0", "VERSION:1.0", 1), nil, []Code{CodeVersion}, nil},
}

func TestMalformed(t *testing.T) {
	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			cal := parseStr(t, tc.in, tc.opts)
			for _, c := range tc.codes {
				if !has(cal, c) {
					t.Errorf("no %s in %v", c, cal.Issues)
				}
			}
			if len(tc.codes) == 0 && len(cal.Issues) > 0 {
				t.Errorf("issues = %v", cal.Issues)
			}
			if tc.check != nil {
				tc.check(t, cal)
			}
		})
	}
}

func TestLimits(t *testing.T) {
	big := wrap("SUMMARY:" + strings.Repeat("x", 50) + "\n")
	many := func(n int, line string) string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, line, i)
		}
		return b.String()
	}
	for _, tc := range []struct {
		name  string
		in    string
		lim   Limits
		code  Code
		check func(t *testing.T, cal *Calendar)
	}{
		{"input", invite, Limits{MaxInputBytes: 60}, CodeLimitInput, func(t *testing.T, cal *Calendar) {
			if has(cal, CodeMissingEnd) {
				t.Errorf("a cut input reports missing ends: %v", cal.Issues)
			}
		}},
		{"line", big, Limits{MaxLineBytes: 20}, CodeLimitLine, func(t *testing.T, cal *Calendar) {
			e := one(t, cal)
			if p := e.Component.Prop("SUMMARY"); !p.Truncated || e.Summary != strings.Repeat("x", 12) {
				t.Errorf("summary = %+v", p)
			}
		}},
		{"joined line", wrap("DESCRIPTION:a\n" + strings.Repeat("stray text\n", 10)), Limits{MaxLineBytes: 40}, CodeLimitLine, func(t *testing.T, cal *Calendar) {
			if p := one(t, cal).Component.Prop("DESCRIPTION"); !p.Truncated || len(p.Value) > 40 {
				t.Errorf("description = %+v", p)
			}
		}},
		{"lines", invite, Limits{MaxLines: 5}, CodeLimitLines, nil},
		{"depth", wrap("BEGIN:VALARM\nACTION:DISPLAY\nBEGIN:X-DEEP\nEND:X-DEEP\nEND:VALARM\nSUMMARY:after\n"), Limits{MaxDepth: 2}, CodeLimitDepth, func(t *testing.T, cal *Calendar) {
			if e := one(t, cal); e.Alarms != 0 || e.Summary != "after" {
				t.Errorf("alarms %d, summary %q", e.Alarms, e.Summary)
			}
		}},
		{"components", wrap("END:VEVENT\nBEGIN:VEVENT\nUID:u2\n"), Limits{MaxComponents: 2}, CodeLimitComponents, func(t *testing.T, cal *Calendar) {
			one(t, cal)
		}},
		{"properties", wrap("SUMMARY:a\nLOCATION:b\n"), Limits{MaxProperties: 3}, CodeLimitProperties, func(t *testing.T, cal *Calendar) {
			if e := one(t, cal); e.Summary != "a" || e.Location != "" {
				t.Errorf("summary, location = %q, %q", e.Summary, e.Location)
			}
		}},
		{"params", wrap("ATTENDEE;A=1;B=2;C=3;PARTSTAT=ACCEPTED:mailto:a@example.com\n"), Limits{MaxParams: 2}, CodeLimitParams, func(t *testing.T, cal *Calendar) {
			if a := one(t, cal).Attendees[0]; a.PartStat != "" || a.Address != "a@example.com" {
				t.Errorf("attendee = %+v", a)
			}
		}},
		{"param values", wrap("ATTENDEE;DELEGATED-TO=\"mailto:a@x\",\"mailto:b@x\",\"mailto:c@x\":mailto:d@x\n"), Limits{MaxParams: 2}, CodeLimitParams, func(t *testing.T, cal *Calendar) {
			if a := one(t, cal).Attendees[0]; len(a.DelegatedTo) != 2 {
				t.Errorf("attendee = %+v", a)
			}
		}},
		{"attendees", wrap(many(3, "ATTENDEE:mailto:a%d@example.com\n")), Limits{MaxAttendees: 2}, CodeLimitAttendees, func(t *testing.T, cal *Calendar) {
			if e := one(t, cal); len(e.Attendees) != 2 || e.AttendeeCount != 3 {
				t.Errorf("attendees %d of %d", len(e.Attendees), e.AttendeeCount)
			}
		}},
		{"attachment bytes", wrap("ATTACH;ENCODING=BASE64;VALUE=BINARY:aGVsbG8gd29ybGQ=\nATTACH;ENCODING=BASE64;VALUE=BINARY:aGk=\n"), Limits{MaxAttachmentBytes: 4}, CodeLimitAttachment, func(t *testing.T, cal *Calendar) {
			at := one(t, cal).Attachments
			if !at[0].Omitted || at[0].Data != nil || at[0].Size < 11 || string(at[1].Data) != "hi" {
				t.Errorf("attachments = %+v", at)
			}
		}},
		{"issues", wrap("SUMMARY:a\nSUMMARY:b\nSUMMARY:c\nSUMMARY:d\nSUMMARY:e\n"), Limits{MaxIssues: 3}, CodeLimitIssues, func(t *testing.T, cal *Calendar) {
			if len(cal.Issues) != 4 || cal.Issues[3].Code != CodeLimitIssues {
				t.Errorf("issues = %v", cal.Issues)
			}
		}},
		{"zones", wrap(many(maxZones+2, "EXDATE;TZID=Zone %d:20261120T100000\n")), Limits{}, CodeLimitZones, func(t *testing.T, cal *Calendar) {
			if e := one(t, cal); len(e.ExDates) != maxZones+2 || e.ExDates[maxZones+1].Kind != TimeUnknownZone {
				t.Errorf("exdates = %d", len(e.ExDates))
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cal := parseStr(t, tc.in, &Options{Limits: tc.lim})
			if !has(cal, tc.code) {
				t.Errorf("no %s in %v", tc.code, cal.Issues)
			}
			if tc.check != nil {
				tc.check(t, cal)
			}
		})
	}
}
