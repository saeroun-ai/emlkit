package icalendar

import (
	"errors"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // zone resolution must not depend on the host's database
)

func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

// The fixtures are written as their generators write them, with CRLF line
// endings added by crlf.

var invite = crlf(`BEGIN:VCALENDAR
PRODID:-//Example Corp//Calendar 1.0//KO
VERSION:2.0
CALSCALE:GREGORIAN
METHOD:REQUEST
BEGIN:VTIMEZONE
TZID:Asia/Seoul
X-LIC-LOCATION:Asia/Seoul
BEGIN:STANDARD
TZOFFSETFROM:+0900
TZOFFSETTO:+0900
TZNAME:KST
DTSTART:19700101T000000
END:STANDARD
END:VTIMEZONE
BEGIN:VEVENT
UID:20261120-kickoff@example.com
SEQUENCE:0
DTSTAMP:20261111T010000Z
DTSTART;TZID=Asia/Seoul:20261120T140000
DTEND;TZID=Asia/Seoul:20261120T153000
SUMMARY:분기 계획 회의
LOCATION:서울 본사 3층 대회의실\, 강남구
DESCRIPTION:안건:\n1. 지난 분기 리뷰\n2. 다음 분기 목표\; 예산 검토
ORGANIZER;CN=김민수:mailto:minsu.kim@example.com
ATTENDEE;CUTYPE=INDIVIDUAL;ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;RSVP=
 TRUE;CN=이지은:mailto:jieun.lee@example.com
ATTENDEE;ROLE=OPT-PARTICIPANT;PARTSTAT=TENTATIVE;CN="Park, Joon":MAILTO:joon
 %2Bcal@example.org
STATUS:CONFIRMED
BEGIN:VALARM
ACTION:DISPLAY
TRIGGER:-PT15M
DESCRIPTION:알림
END:VALARM
END:VEVENT
END:VCALENDAR
`)

// update is invite moved by an hour, as its organizer sends it next.
var update = strings.NewReplacer(
	"SEQUENCE:0", "SEQUENCE:1",
	"DTSTAMP:20261111T010000Z", "DTSTAMP:20261112T020000Z",
	"20261120T140000", "20261120T150000",
	"20261120T153000", "20261120T163000",
).Replace(invite)

var cancel = crlf(`BEGIN:VCALENDAR
PRODID:-//Example Corp//Calendar 1.0//KO
VERSION:2.0
METHOD:CANCEL
BEGIN:VEVENT
UID:20261120-kickoff@example.com
SEQUENCE:2
DTSTAMP:20261113T000000Z
DTSTART;TZID=Asia/Seoul:20261120T150000
SUMMARY:취소됨: 분기 계획 회의
STATUS:CANCELLED
ORGANIZER;CN=김민수:mailto:minsu.kim@example.com
ATTENDEE;CN=이지은:mailto:jieun.lee@example.com
END:VEVENT
END:VCALENDAR
`)

var reply = crlf(`BEGIN:VCALENDAR
PRODID:-//Example Corp//Mail 2.0//EN
VERSION:2.0
METHOD:REPLY
BEGIN:VEVENT
UID:20261120-kickoff@example.com
SEQUENCE:1
DTSTAMP:20261112T030000Z
ORGANIZER:mailto:minsu.kim@example.com
ATTENDEE;PARTSTAT=ACCEPTED;CN=이지은:mailto:jieun.lee@example.com
REQUEST-STATUS:2.0;Success
END:VEVENT
END:VCALENDAR
`)

var allDay = crlf(`BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Example Corp//Calendar 1.0//EN
METHOD:PUBLISH
BEGIN:VEVENT
UID:holiday-2026-12-25@example.com
DTSTAMP:20261111T000000Z
DTSTART;VALUE=DATE:20261225
DTEND;VALUE=DATE:20261226
SUMMARY:성탄절
TRANSP:TRANSPARENT
END:VEVENT
END:VCALENDAR
`)

var recurring = crlf(`BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Example Corp//Calendar 1.0//EN
METHOD:REQUEST
BEGIN:VEVENT
UID:standup@example.com
DTSTAMP:20261111T000000Z
DTSTART;TZID=Asia/Seoul:20261116T100000
DURATION:PT15M
RRULE:FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,WE;UNTIL=20270331T150000Z
EXDATE;TZID=Asia/Seoul:20261123T100000,20261125T100000
EXDATE;TZID=Asia/Seoul:20261228T100000
SUMMARY:Stand-up
ORGANIZER:mailto:lead@example.com
END:VEVENT
BEGIN:VEVENT
UID:standup@example.com
RECURRENCE-ID;TZID=Asia/Seoul:20261130T100000
DTSTAMP:20261111T000000Z
DTSTART;TZID=Asia/Seoul:20261130T110000
DURATION:PT15M
SUMMARY:Stand-up (moved)
END:VEVENT
END:VCALENDAR
`)

var outlook = crlf(`BEGIN:VCALENDAR
METHOD:REQUEST
PRODID:Microsoft Exchange Server 2010
VERSION:2.0
BEGIN:VTIMEZONE
TZID:Korea Standard Time
BEGIN:STANDARD
DTSTART:16010101T000000
TZOFFSETFROM:+0900
TZOFFSETTO:+0900
END:STANDARD
BEGIN:DAYLIGHT
DTSTART:16010101T000000
TZOFFSETFROM:+0900
TZOFFSETTO:+0900
END:DAYLIGHT
END:VTIMEZONE
BEGIN:VEVENT
ORGANIZER;CN=Hong Gildong:mailto:gildong.hong@contoso.com
ATTENDEE;ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;RSVP=TRUE;CN=Kim Cheol
 su:mailto:cheolsu.kim@contoso.com
DESCRIPTION;LANGUAGE=ko-KR:주간 동기화 회의입니다.\n\n________________\n
UID:040000008200E00074C5B7101A82E0080000000010B4D8C2A2D9DA0100000000000000
 0010000000F5E1B6C4F1C2A84B8D9F3F2B1E0A7C6D
SUMMARY;LANGUAGE=ko-KR:주간 동기화
DTSTART;TZID=Korea Standard Time:20261118T100000
DTEND;TZID=Korea Standard Time:20261118T110000
CLASS:PUBLIC
PRIORITY:5
DTSTAMP:20261111T023000Z
TRANSP:OPAQUE
STATUS:CONFIRMED
SEQUENCE:0
LOCATION;LANGUAGE=ko-KR:Microsoft Teams 모임
X-MICROSOFT-CDO-APPT-SEQUENCE:0
X-MICROSOFT-CDO-OWNERAPPTID:2120834785
X-MICROSOFT-CDO-BUSYSTATUS:TENTATIVE
X-MICROSOFT-CDO-INTENDEDSTATUS:BUSY
X-MICROSOFT-CDO-ALLDAYEVENT:FALSE
X-MICROSOFT-CDO-IMPORTANCE:1
X-MICROSOFT-CDO-INSTTYPE:0
X-MICROSOFT-ONLINEMEETINGCONFLINK:conf:sip:gildong.hong@contoso.com\;gruu\;o
 paque=app:conf:focus:id:ABC123
X-MICROSOFT-DONOTFORWARDMEETING:FALSE
X-MICROSOFT-DISALLOW-COUNTER:FALSE
X-MICROSOFT-LOCATIONDISPLAYNAME:Microsoft Teams 모임
X-ALT-DESC;FMTTYPE=text/html:<html><body><p>주간 동기화 회의입니다.</p></bo
 dy></html>
BEGIN:VALARM
DESCRIPTION:REMINDER
TRIGGER;RELATED=START:-PT15M
ACTION:DISPLAY
END:VALARM
END:VEVENT
END:VCALENDAR
`)

var google = crlf(`BEGIN:VCALENDAR
PRODID:-//Google Inc//Google Calendar 70.9054//EN
VERSION:2.0
CALSCALE:GREGORIAN
METHOD:REQUEST
BEGIN:VTIMEZONE
TZID:America/Los_Angeles
X-LIC-LOCATION:America/Los_Angeles
BEGIN:DAYLIGHT
TZOFFSETFROM:-0800
TZOFFSETTO:-0700
TZNAME:PDT
DTSTART:19700308T020000
RRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU
END:DAYLIGHT
BEGIN:STANDARD
TZOFFSETFROM:-0700
TZOFFSETTO:-0800
TZNAME:PST
DTSTART:19701101T020000
RRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU
END:STANDARD
END:VTIMEZONE
BEGIN:VEVENT
DTSTART;TZID=America/Los_Angeles:20261203T090000
DTEND;TZID=America/Los_Angeles:20261203T093000
DTSTAMP:20261111T180000Z
ORGANIZER;CN=alex@example.com:mailto:alex@example.com
UID:7kq3m9v2c1h8s0d4f6g5j2l1p0@google.com
ATTENDEE;CUTYPE=INDIVIDUAL;ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED;RSVP=TRUE
 ;CN=alex@example.com;X-NUM-GUESTS=0:mailto:alex@example.com
ATTENDEE;CUTYPE=INDIVIDUAL;ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;RSVP=
 TRUE;CN=sam@example.net;X-NUM-GUESTS=0:mailto:sam@example.net
X-GOOGLE-CONFERENCE:https://meet.google.com/abc-defg-hij
CREATED:20261111T175900Z
DESCRIPTION:Weekly sync\n\n-::~:~::~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~:~
 :~:~:~:~:~:~:~:~::~:~::-\nJoin with Google Meet: https://meet.google.com/a
 bc-defg-hij\n\nLearn more about Meet at: https://support.google.com/a/user
 s/answer/9282720
LAST-MODIFIED:20261111T180000Z
LOCATION:
SEQUENCE:0
STATUS:CONFIRMED
SUMMARY:Sync
TRANSP:OPAQUE
BEGIN:VALARM
ACTION:EMAIL
DESCRIPTION:This is an event reminder
SUMMARY:Alarm notification
ATTENDEE:mailto:alex@example.com
TRIGGER:-P0DT0H30M0S
END:VALARM
BEGIN:VALARM
ACTION:DISPLAY
DESCRIPTION:This is an event reminder
TRIGGER:-P0DT0H10M0S
END:VALARM
END:VEVENT
END:VCALENDAR
`)

func parseStr(t *testing.T, s string, opts *Options) *Calendar {
	t.Helper()
	return ParseBytes([]byte(s), opts)
}

func has(cal *Calendar, code Code) bool {
	for _, i := range cal.Issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

func clean(t *testing.T, cal *Calendar) {
	t.Helper()
	for _, i := range cal.Issues {
		t.Errorf("unexpected issue %v", i)
	}
}

func one(t *testing.T, cal *Calendar) *Event {
	t.Helper()
	if len(cal.Events) != 1 {
		t.Fatalf("got %d events, want 1; issues %v", len(cal.Events), cal.Issues)
	}
	return cal.Events[0]
}

func utc(s string) time.Time {
	t, err := time.Parse("20060102T150405Z", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestInvite(t *testing.T) {
	cal := parseStr(t, invite, nil)
	clean(t, cal)
	if cal.Method != "REQUEST" || cal.Version != "2.0" || cal.CalScale != "GREGORIAN" || cal.ProdID != "-//Example Corp//Calendar 1.0//KO" {
		t.Errorf("calendar = %q %q %q %q", cal.Method, cal.Version, cal.CalScale, cal.ProdID)
	}
	if len(cal.Timezones) != 1 || cal.Timezones[0].ID != "Asia/Seoul" || cal.Timezones[0].Location != "Asia/Seoul" {
		t.Errorf("timezones = %+v", cal.Timezones)
	}
	e := one(t, cal)
	if e.UID != "20261120-kickoff@example.com" || e.Sequence != 0 || e.Status != "CONFIRMED" {
		t.Errorf("event = %q %d %q", e.UID, e.Sequence, e.Status)
	}
	if e.Summary != "분기 계획 회의" || e.Location != "서울 본사 3층 대회의실, 강남구" {
		t.Errorf("summary, location = %q, %q", e.Summary, e.Location)
	}
	if want := "안건:\n1. 지난 분기 리뷰\n2. 다음 분기 목표; 예산 검토"; e.Description != want {
		t.Errorf("description = %q, want %q", e.Description, want)
	}
	if e.Start.Kind != TimeZoned || !e.Start.Time.Equal(utc("20261120T050000Z")) || e.Start.Zone != "Asia/Seoul" || e.Start.ZoneSource != "iana" {
		t.Errorf("start = %+v", e.Start)
	}
	if !e.End.Time.Equal(utc("20261120T063000Z")) || e.AllDay {
		t.Errorf("end = %+v, all day %v", e.End, e.AllDay)
	}
	if !e.DTStamp.Time.Equal(utc("20261111T010000Z")) || e.DTStamp.Kind != TimeUTC {
		t.Errorf("dtstamp = %+v", e.DTStamp)
	}
	if o := e.Organizer; o == nil || o.Address != "minsu.kim@example.com" || o.CommonName != "김민수" || o.Raw != "mailto:minsu.kim@example.com" {
		t.Errorf("organizer = %+v", e.Organizer)
	}
	if len(e.Attendees) != 2 || e.AttendeeCount != 2 {
		t.Fatalf("attendees = %+v", e.Attendees)
	}
	a := e.Attendees[0]
	if a.Address != "jieun.lee@example.com" || a.CommonName != "이지은" || !a.RSVP || a.Role != "REQ-PARTICIPANT" || a.PartStat != "NEEDS-ACTION" || a.CUType != "INDIVIDUAL" {
		t.Errorf("attendee 0 = %+v", a)
	}
	a = e.Attendees[1]
	if a.Address != "joon+cal@example.org" || a.Raw != "MAILTO:joon%2Bcal@example.org" || a.CommonName != "Park, Joon" || a.RSVP || a.Role != "OPT-PARTICIPANT" {
		t.Errorf("attendee 1 = %+v", a)
	}
	if e.Alarms != 1 {
		t.Errorf("alarms = %d", e.Alarms)
	}
	if c := Classify(cal, nil); c.Intent != IntentRequest || c.UID != e.UID {
		t.Errorf("classify = %+v", c)
	}
}

func TestUpdateCancelReply(t *testing.T) {
	inv := one(t, parseStr(t, invite, nil))
	upCal := parseStr(t, update, nil)
	clean(t, upCal)
	up := one(t, upCal)
	if up.Sequence != 1 || !up.Start.Time.Equal(utc("20261120T060000Z")) {
		t.Errorf("update = %d %v", up.Sequence, up.Start.Time)
	}
	lookup := func(e *Event) func(string) *Event {
		return func(uid string) *Event {
			if e != nil && uid == e.UID {
				return e
			}
			return nil
		}
	}
	if c := Classify(upCal, lookup(inv)); c.Intent != IntentUpdate || c.Stale || c.Known != inv {
		t.Errorf("update classified %+v", c)
	}
	if c := Classify(parseStr(t, invite, nil), lookup(up)); c.Intent != IntentRequest || !c.Stale {
		t.Errorf("an older invite after the update classified %+v", c)
	}

	canCal := parseStr(t, cancel, nil)
	clean(t, canCal)
	can := one(t, canCal)
	if can.Status != "CANCELLED" || can.Summary != "취소됨: 분기 계획 회의" || can.Start.Kind != TimeZoned {
		t.Errorf("cancel = %q %q %v", can.Status, can.Summary, can.Start.Kind)
	}
	if c := Classify(canCal, lookup(up)); c.Intent != IntentCancel || c.Stale || c.Sequence != 2 {
		t.Errorf("cancel classified %+v", c)
	}

	repCal := parseStr(t, reply, nil)
	clean(t, repCal)
	c := Classify(repCal, lookup(up))
	if c.Intent != IntentReply || c.PartStat != "ACCEPTED" || c.Attendee == nil || c.Attendee.Address != "jieun.lee@example.com" {
		t.Errorf("reply classified %+v", c)
	}
	if one(t, repCal).Start.Kind != TimeNone {
		t.Errorf("reply start = %+v", one(t, repCal).Start)
	}
}

func TestAllDay(t *testing.T) {
	cal := parseStr(t, allDay, nil)
	clean(t, cal)
	e := one(t, cal)
	if !e.AllDay || e.Start.Kind != TimeDate || e.Start.Known() || e.End.Kind != TimeDate {
		t.Fatalf("start, end = %+v, %+v", e.Start, e.End)
	}
	if e.Start.Wall != time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC) || e.End.Wall != time.Date(2026, 12, 26, 0, 0, 0, 0, time.UTC) {
		t.Errorf("wall = %v, %v", e.Start.Wall, e.End.Wall)
	}
	if c := Classify(cal, nil); c.Intent != IntentPublish {
		t.Errorf("classify = %+v", c)
	}
}

func TestRecurring(t *testing.T) {
	cal := parseStr(t, recurring, nil)
	clean(t, cal)
	if len(cal.Events) != 2 {
		t.Fatalf("events = %d", len(cal.Events))
	}
	e := cal.Events[0]
	r := e.RRule
	if r == nil || r.Freq != "WEEKLY" || r.Interval != 2 || r.Count != 0 || r.Parts["BYDAY"] != "MO,WE" || r.Raw != "FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,WE;UNTIL=20270331T150000Z" {
		t.Fatalf("rrule = %+v", r)
	}
	if r.Until.Kind != TimeUTC || !r.Until.Time.Equal(utc("20270331T150000Z")) {
		t.Errorf("until = %+v", r.Until)
	}
	if len(e.ExDates) != 3 || !e.ExDates[0].Time.Equal(utc("20261123T010000Z")) || !e.ExDates[2].Time.Equal(utc("20261228T010000Z")) {
		t.Errorf("exdates = %+v", e.ExDates)
	}
	if !e.Duration.Valid || e.Duration.Value != 15*time.Minute || e.Duration.Raw != "PT15M" {
		t.Errorf("duration = %+v", e.Duration)
	}
	o := cal.Events[1]
	if o.RecurrenceID.Kind != TimeZoned || !o.RecurrenceID.Time.Equal(utc("20261130T010000Z")) {
		t.Errorf("recurrence-id = %+v", o.RecurrenceID)
	}
	if c := Classify(cal, nil); c.Event != e {
		t.Errorf("classify chose %+v, want the master", c.Event)
	}
	if _, ok := Compare(e, o); ok {
		t.Error("Compare treats an override as a revision of the master")
	}
}

func TestOutlook(t *testing.T) {
	cal := parseStr(t, outlook, nil)
	e := one(t, cal)
	// The Windows zone name is resolved, and said to be.
	if len(cal.Issues) != 1 || cal.Issues[0].Code != CodeZoneMapped {
		t.Errorf("issues = %v, want only %s", cal.Issues, CodeZoneMapped)
	}
	if e.Start.Kind != TimeZoned || e.Start.Zone != "Asia/Seoul" || e.Start.ZoneSource != "windows" || e.Start.TZID != "Korea Standard Time" || !e.Start.Time.Equal(utc("20261118T010000Z")) {
		t.Errorf("start = %+v", e.Start)
	}
	if want := "040000008200E00074C5B7101A82E0080000000010B4D8C2A2D9DA01000000000000000010000000F5E1B6C4F1C2A84B8D9F3F2B1E0A7C6D"; e.UID != want {
		t.Errorf("uid = %q", e.UID)
	}
	if e.Summary != "주간 동기화" || e.Location != "Microsoft Teams 모임" || !strings.HasPrefix(e.Description, "주간 동기화 회의입니다.\n\n") {
		t.Errorf("text = %q %q %q", e.Summary, e.Location, e.Description)
	}
	if a := e.Attendees[0]; a.CommonName != "Kim Cheolsu" || a.Address != "cheolsu.kim@contoso.com" {
		t.Errorf("attendee = %+v", a)
	}
	if len(e.X) != 12 || e.X[0].Name != "X-MICROSOFT-CDO-APPT-SEQUENCE" || e.X[11].Name != "X-ALT-DESC" {
		t.Errorf("x = %d properties", len(e.X))
	}
	if x := e.X[11]; x.Param("FMTTYPE") != "text/html" || !strings.HasSuffix(x.Value, "</body></html>") {
		t.Errorf("X-ALT-DESC = %+v", x)
	}
	if e.Alarms != 1 || Classify(cal, nil).Intent != IntentRequest {
		t.Errorf("alarms %d", e.Alarms)
	}
}

func TestGoogle(t *testing.T) {
	cal := parseStr(t, google, nil)
	clean(t, cal)
	e := one(t, cal)
	if !e.Start.Time.Equal(utc("20261203T170000Z")) || e.Start.Zone != "America/Los_Angeles" {
		t.Errorf("start = %+v", e.Start)
	}
	if len(e.Attendees) != 2 || e.Attendees[0].PartStat != "ACCEPTED" || !e.Attendees[1].RSVP || e.Attendees[1].Address != "sam@example.net" {
		t.Errorf("attendees = %+v", e.Attendees)
	}
	if e.Alarms != 2 || e.Location != "" || !strings.Contains(e.Description, "\nJoin with Google Meet: https://meet.google.com/abc-defg-hij\n") {
		t.Errorf("alarms %d, location %q, description %q", e.Alarms, e.Location, e.Description)
	}
	if len(e.X) != 1 || e.X[0].Value != "https://meet.google.com/abc-defg-hij" {
		t.Errorf("x = %+v", e.X)
	}
	if !e.Created.Time.Equal(utc("20261111T175900Z")) || !e.LastModified.Time.Equal(utc("20261111T180000Z")) {
		t.Errorf("created, last-modified = %v, %v", e.Created, e.LastModified)
	}
}

func TestRawLayer(t *testing.T) {
	doc := ParseRaw([]byte(crlf(`BEGIN:VCALENDAR
BEGIN:VEVENT
ATTENDEE;DELEGATED-TO="mailto:a@x","mailto:b@x";CN="Doe; Jane, Q: Esq":mailto:c@x
ATTENDEE;CN="George ^'Babe^' Ruth^nJr";X-EMPTY=:mailto:d@x
SUMMARY:a\, b
END:VEVENT
END:VCALENDAR
`)), nil)
	if len(doc.Issues) != 0 || len(doc.Components) != 1 {
		t.Fatalf("doc = %+v", doc)
	}
	ev := doc.Components[0].Children[0]
	if ev.Name != "VEVENT" || !ev.Closed || ev.Line != 2 || len(ev.Properties) != 3 {
		t.Fatalf("event = %+v", ev)
	}
	p := ev.Properties[0]
	if got := p.ParamValues("delegated-to"); len(got) != 2 || got[1] != "mailto:b@x" {
		t.Errorf("DELEGATED-TO = %q", got)
	}
	if p.Param("CN") != "Doe; Jane, Q: Esq" || p.Value != "mailto:c@x" || p.Line != 3 || !strings.HasPrefix(p.Raw, "ATTENDEE;DELEGATED-TO=") {
		t.Errorf("property = %+v", p)
	}
	if q := ev.Properties[1]; q.Param("CN") != "George \"Babe\" Ruth\nJr" || len(q.Params[1].Values) != 1 || q.Params[1].Values[0] != "" {
		t.Errorf("caret, empty = %+v", q.Params)
	}
	if s := ev.Prop("summary"); s == nil || s.Value != `a\, b` || UnescapeText(s.Value) != "a, b" {
		t.Errorf("summary = %+v", s)
	}
}

func TestZones(t *testing.T) {
	fixed := "BEGIN:VTIMEZONE\nTZID:Customized Time Zone\nBEGIN:STANDARD\nDTSTART:16010101T000000\nTZOFFSETFROM:+0530\nTZOFFSETTO:+0530\nEND:STANDARD\nEND:VTIMEZONE\n"
	dst := "BEGIN:VTIMEZONE\nTZID:Customized Time Zone\nBEGIN:STANDARD\nTZOFFSETTO:+0100\nEND:STANDARD\nBEGIN:DAYLIGHT\nTZOFFSETTO:+0200\nEND:DAYLIGHT\nEND:VTIMEZONE\n"
	lic := "BEGIN:VTIMEZONE\nTZID:Paris custom\nX-LIC-LOCATION:Europe/Paris\nEND:VTIMEZONE\n"
	for _, tc := range []struct {
		tzid, vtz  string
		kind       TimeKind
		zone, from string
		instant    string
	}{
		{"Asia/Seoul", "", TimeZoned, "Asia/Seoul", "iana", "20261120T010000Z"},
		{"/mozilla.org/20050126_1/Asia/Seoul", "", TimeZoned, "Asia/Seoul", "iana", "20261120T010000Z"},
		{"Pacific Standard Time", "", TimeZoned, "America/Los_Angeles", "windows", "20261120T180000Z"},
		{"tokyo standard time", "", TimeZoned, "Asia/Tokyo", "windows", "20261120T010000Z"},
		{"(UTC+09:00) Seoul", "", TimeZoned, "Asia/Seoul", "windows", "20261120T010000Z"},
		{"Customized Time Zone", fixed, TimeZoned, "UTC+05:30", "offset", "20261120T043000Z"},
		{"Paris custom", lic, TimeZoned, "Europe/Paris", "vtimezone", "20261120T090000Z"},
		{"Customized Time Zone", dst, TimeUnknownZone, "", "", ""},
		{"Mars Standard Time", "", TimeUnknownZone, "", "", ""},
		{"Local", "", TimeUnknownZone, "", "", ""},
		{"../../etc/localtime", "", TimeUnknownZone, "", "", ""},
	} {
		in := "BEGIN:VCALENDAR\nVERSION:2.0\n" + tc.vtz + "BEGIN:VEVENT\nUID:z\nDTSTART;TZID=\"" + tc.tzid + "\":20261120T100000\nEND:VEVENT\nEND:VCALENDAR\n"
		cal := parseStr(t, crlf(in), nil)
		d := one(t, cal).Start
		if d.Kind != tc.kind || d.Zone != tc.zone || d.ZoneSource != tc.from || d.TZID != tc.tzid {
			t.Errorf("%s: %+v", tc.tzid, d)
			continue
		}
		if tc.instant != "" && !d.Time.Equal(utc(tc.instant)) {
			t.Errorf("%s: instant %v, want %s", tc.tzid, d.Time, tc.instant)
		}
		if d.Wall != time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC) {
			t.Errorf("%s: wall %v", tc.tzid, d.Wall)
		}
		mapped := tc.from != "iana" || tc.zone != tc.tzid
		switch {
		case tc.kind == TimeUnknownZone && !has(cal, CodeUnknownTZID):
			t.Errorf("%s: no %s in %v", tc.tzid, CodeUnknownTZID, cal.Issues)
		case tc.kind == TimeZoned && mapped != has(cal, CodeZoneMapped):
			t.Errorf("%s: %s reported %v, want %v", tc.tzid, CodeZoneMapped, !mapped, mapped)
		}
	}
}

func TestValues(t *testing.T) {
	for in, want := range map[string]string{
		`a\nb\Nc`: "a\nb\nc", `a\,b\;c\\d`: `a,b;c\d`, `x\:y`: "x:y", `odd\q`: `odd\q`, `end\`: `end\`,
	} {
		if got := UnescapeText(in); got != want {
			t.Errorf("UnescapeText(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]time.Duration{
		"PT1H30M": 90 * time.Minute, "-PT15M": -15 * time.Minute, "P2W": 14 * 24 * time.Hour,
		"P1DT2H": 26 * time.Hour, "-P0DT0H30M0S": -30 * time.Minute, "+PT10S": 10 * time.Second,
	} {
		if got, ok := ParseDuration(in); !ok || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "P", "PT", "P1H", "PT1D", "PT1.5H", "P1", "1H", "PT1H1", "P9999999999W", "PTT1H"} {
		if _, ok := ParseDuration(in); ok {
			t.Errorf("ParseDuration(%q) accepted", in)
		}
	}
	for in, want := range map[string]string{
		"mailto:a@x": "a@x", "MAILTO:A@X": "A@X", " mailto: <a@x> ": "a@x", "mailto:a%40b@x": "a@b@x",
		"mailto:a@x?subject=hi": "a@x", "a@x": "a@x", "urn:uuid:1234": "urn:uuid:1234", "mailto://a@x": "a@x",
	} {
		if got := NormalizeAddress(in); got != want {
			t.Errorf("NormalizeAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompare(t *testing.T) {
	stamp := func(s string) DateTime { return DateTime{Kind: TimeUTC, Time: utc(s)} }
	ev := func(uid string, seq int, st string) *Event {
		return &Event{UID: uid, Sequence: seq, DTStamp: stamp(st)}
	}
	a := ev("u", 1, "20261111T000000Z")
	for _, tc := range []struct {
		b     *Event
		order int
		ok    bool
	}{
		{ev("u", 0, "20261112T000000Z"), 1, true},
		{ev("u", 2, "20261101T000000Z"), -1, true},
		{ev("u", 1, "20261110T000000Z"), 1, true},
		{ev("u", 1, "20261112T000000Z"), -1, true},
		{ev("u", 1, "20261111T000000Z"), 0, true},
		{&Event{UID: "u", Sequence: 1}, 0, true},
		{ev("v", 0, "20261111T000000Z"), 0, false},
		{nil, 0, false},
	} {
		if order, ok := Compare(a, tc.b); order != tc.order || ok != tc.ok {
			t.Errorf("Compare(a, %+v) = %d, %v, want %d, %v", tc.b, order, ok, tc.order, tc.ok)
		}
	}
}

func TestClassifyMethods(t *testing.T) {
	cal := func(method, extra string) *Calendar {
		m := ""
		if method != "" {
			m = "METHOD:" + method + "\n"
		}
		return parseStr(t, crlf("BEGIN:VCALENDAR\nVERSION:2.0\n"+m+"BEGIN:VEVENT\nUID:u\nDTSTAMP:20261111T000000Z\n"+extra+"END:VEVENT\nEND:VCALENDAR\n"), nil)
	}
	for _, tc := range []struct {
		method, extra string
		want          Intent
	}{
		{"REQUEST", "STATUS:CANCELLED\n", IntentCancel},
		{"PUBLISH", "STATUS:CANCELLED\n", IntentCancel},
		{"COUNTER", "ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:a@x\nATTENDEE;PARTSTAT=TENTATIVE:mailto:b@x\n", IntentCounter},
		{"DECLINECOUNTER", "", IntentDeclineCounter},
		{"REFRESH", "", IntentRefresh},
		{"ADD", "", IntentAdd},
		{"", "", IntentPublish},
		{"FOO", "", IntentUnknown},
	} {
		c := Classify(cal(tc.method, tc.extra), nil)
		if c.Intent != tc.want {
			t.Errorf("%s %q: %v, want %v", tc.method, tc.extra, c.Intent, tc.want)
		}
		if tc.want == IntentCounter && (c.Attendee == nil || c.Attendee.Address != "b@x" || c.PartStat != "TENTATIVE") {
			t.Errorf("counter attendee = %+v", c.Attendee)
		}
	}
	empty := Classify(&Calendar{Method: "REQUEST"}, func(string) *Event { t.Fatal("lookup without an event"); return nil })
	if empty.Intent != IntentRequest || empty.Event != nil {
		t.Errorf("empty = %+v", empty)
	}
}

func TestContentTypeMethod(t *testing.T) {
	cal := parseStr(t, invite, &Options{ContentTypeMethod: "cancel"})
	if cal.Method != "REQUEST" || !has(cal, CodeMethodMismatch) {
		t.Errorf("mismatch: %q %v", cal.Method, cal.Issues)
	}
	noMethod := strings.Replace(invite, "METHOD:REQUEST\r\n", "", 1)
	cal = parseStr(t, noMethod, &Options{ContentTypeMethod: "request"})
	if cal.Method != "REQUEST" || !has(cal, CodeMethodFromType) {
		t.Errorf("from type: %q %v", cal.Method, cal.Issues)
	}
}

type failingReader struct{ data string }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.data == "" {
		return 0, errors.New("connection reset")
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestParseReader(t *testing.T) {
	cal, err := Parse(strings.NewReader(allDay), nil)
	if err != nil || len(cal.Events) != 1 {
		t.Fatalf("Parse = %v, %v", cal, err)
	}
	cal, err = Parse(&failingReader{data: allDay[:strings.Index(allDay, "DTSTART")]}, nil)
	if err == nil || cal == nil || len(cal.Events) != 1 || cal.Events[0].UID == "" {
		t.Errorf("Parse of a failing reader = %+v, %v", cal, err)
	}
}

func TestAttachments(t *testing.T) {
	in := crlf("BEGIN:VCALENDAR\nVERSION:2.0\nBEGIN:VEVENT\nUID:u\n" +
		"ATTACH;FMTTYPE=application/pdf:https://example.com/agenda.pdf\n" +
		"ATTACH;FMTTYPE=text/plain;ENCODING=BASE64;VALUE=BINARY;X-FILENAME=a.txt:aGVsbG8g\n d29ybGQ=\n" +
		"ATTACH;ENCODING=BASE64;VALUE=BINARY:aGk\n" +
		"END:VEVENT\nEND:VCALENDAR\n")
	cal := parseStr(t, in, nil)
	clean(t, cal)
	at := one(t, cal).Attachments
	if len(at) != 3 || at[0].URI != "https://example.com/agenda.pdf" || at[0].Inline || at[0].FormatType != "application/pdf" {
		t.Fatalf("attachments = %+v", at)
	}
	if !at[1].Inline || string(at[1].Data) != "hello world" || at[1].Size != 11 || at[1].Filename != "a.txt" {
		t.Errorf("inline = %+v", at[1])
	}
	if string(at[2].Data) != "hi" {
		t.Errorf("unpadded = %+v", at[2])
	}
}
