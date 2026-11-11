package icalendar

import (
	"encoding/base64"
	"io"
	"mime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// A Calendar is the convenience view of one iCalendar object.
type Calendar struct {
	// Method is METHOD upper-cased, or the Content-Type method parameter
	// when there is no METHOD (Options.ContentTypeMethod); "" when neither.
	Method   string
	ProdID   string
	Version  string
	CalScale string

	Events    []*Event
	Todos     []*Todo
	Timezones []*Timezone

	// Raw is the VCALENDAR read, nil when the input held none. Components
	// the convenience view does not model (VJOURNAL, VFREEBUSY, X-
	// components) are among its Children.
	Raw *Component
	// Roots are all top-level components. More than one VCALENDAR is
	// reported (CodeMultipleCalendars); the events, to-dos and time zones of
	// every one are in the view, the other fields come from the first.
	Roots []*Component

	// Issues are the repairs and budgets of the whole parse, raw layer first.
	Issues []Issue
}

// An Event is a VEVENT.
type Event struct {
	UID string
	// Sequence is SEQUENCE, 0 when absent or not a number.
	Sequence    int
	Status      string // upper-cased
	Summary     string // TEXT values are unescaped
	Description string
	Location    string
	URL         string

	Organizer *Organizer
	// Attendees are the first Limits.MaxAttendees ATTENDEE properties;
	// AttendeeCount is how many there were.
	Attendees     []Attendee
	AttendeeCount int

	// Start is DTSTART, End DTEND and Duration DURATION, each as written:
	// End is not computed from Duration, nor Duration from End.
	Start    DateTime
	End      DateTime
	Duration Duration
	// AllDay reports that DTSTART is a DATE.
	AllDay       bool
	RecurrenceID DateTime
	RRule        *RRule
	ExDates      []DateTime

	Created      DateTime
	LastModified DateTime
	DTStamp      DateTime

	Attachments []Attachment
	// Alarms is the number of VALARM components.
	Alarms int
	// X are the X- properties, in order.
	X []*Property

	// Component is the VEVENT as read.
	Component *Component
}

// A Todo is a VTODO, lightly modelled.
type Todo struct {
	UID         string
	Sequence    int
	Status      string
	Summary     string
	Description string
	Start       DateTime
	Due         DateTime
	Completed   DateTime
	DTStamp     DateTime
	Organizer   *Organizer
	Component   *Component
}

// A Timezone is a VTIMEZONE: its TZID, its X-LIC-LOCATION if any, and the
// component with its STANDARD and DAYLIGHT rules.
type Timezone struct {
	ID        string
	Location  string
	Component *Component
}

// An Organizer is the ORGANIZER of an event.
type Organizer struct {
	// Address is the normalised address (NormalizeAddress) and Raw the value
	// as written.
	Address    string
	Raw        string
	CommonName string
	// SentBy is the normalised SENT-BY address, of someone acting for the
	// organizer.
	SentBy string
}

// An Attendee is one ATTENDEE. Parameter values are upper-cased and empty
// when absent; the RFC 5545 defaults (REQ-PARTICIPANT, NEEDS-ACTION,
// INDIVIDUAL) are not filled in.
type Attendee struct {
	Address    string
	Raw        string
	CommonName string
	SentBy     string
	Role       string
	PartStat   string
	CUType     string
	RSVP       bool
	// DelegatedTo, DelegatedFrom and Member are normalised addresses.
	DelegatedTo   []string
	DelegatedFrom []string
	Member        []string
}

// An RRule is an RRULE: the value as written, its parts by name, and the
// common ones split out. Recurrences are not expanded.
type RRule struct {
	Raw      string
	Freq     string // upper-cased
	Interval int    // 0 when absent, which means 1
	Count    int    // 0 when absent
	Until    DateTime
	// Parts are every NAME=VALUE part, names upper-cased; the first of a
	// repeated name.
	Parts map[string]string
}

// An Attachment is an ATTACH: a URI, or inline binary data.
type Attachment struct {
	URI        string
	FormatType string // FMTTYPE
	// Filename is the X-FILENAME, FILENAME or X-APPLE-FILENAME parameter.
	Filename string
	// Inline reports base64 data in the property itself. Data holds it
	// decoded, unless it did not decode (Invalid) or did not fit in
	// Limits.MaxAttachmentBytes (Omitted). Size is the decoded size; for an
	// omitted attachment the size the data would have, within two bytes.
	Inline  bool
	Data    []byte
	Size    int
	Omitted bool
	Invalid bool
}

// Parse reads at most Limits.MaxInputBytes from r and parses it as an
// iCalendar object. It returns a Calendar in every case; the error is only
// the reader's, other than io.EOF, with the calendar of what was read.
func Parse(r io.Reader, opts *Options) (*Calendar, error) {
	lim := opts.limits()
	data, err := io.ReadAll(io.LimitReader(r, int64(lim.MaxInputBytes)+1))
	return ParseBytes(data, opts), err
}

// ParseBytes parses data as an iCalendar object. It always returns a
// Calendar; what had to be repaired, and the budgets hit, are its Issues.
func ParseBytes(data []byte, opts *Options) *Calendar {
	p := newParser(opts)
	p.run(data)
	b := &builder{is: &p.is, lim: p.lim, attach: p.lim.MaxAttachmentBytes}
	cal := b.calendar(p.roots, opts)
	cal.Issues = p.is.list
	return cal
}

type builder struct {
	is     *issues
	lim    Limits
	zones  *zones
	attach int // inline attachment bytes left
}

func (b *builder) calendar(roots []*Component, opts *Options) *Calendar {
	cal := &Calendar{Roots: roots}
	var cals []*Component
	for _, r := range roots {
		if r.Name == "VCALENDAR" {
			cals = append(cals, r)
		}
	}
	if len(cals) == 0 {
		b.is.add(CodeNoCalendar, 0, "")
		return cal
	}
	cal.Raw = cals[0]
	if len(cals) > 1 {
		b.is.add(CodeMultipleCalendars, cals[1].Line, strconv.Itoa(len(cals))+" calendars")
	}
	var vtz []*Component
	for _, c := range cals {
		for _, ch := range c.Children {
			if ch.Name == "VTIMEZONE" {
				vtz = append(vtz, ch)
				tz := &Timezone{Component: ch}
				if p := ch.Prop("TZID"); p != nil {
					tz.ID = strings.TrimSpace(p.Value)
				}
				if p := ch.Prop("X-LIC-LOCATION"); p != nil {
					tz.Location = strings.TrimSpace(p.Value)
				}
				cal.Timezones = append(cal.Timezones, tz)
			}
		}
	}
	b.zones = newZones(b.is, vtz)

	seen := make(map[string]bool)
	for _, p := range cal.Raw.Properties {
		if !b.first(seen, p) {
			continue
		}
		switch p.Name {
		case "METHOD":
			cal.Method = strings.ToUpper(strings.TrimSpace(p.Value))
		case "PRODID":
			cal.ProdID = b.text(p)
		case "VERSION":
			cal.Version = strings.TrimSpace(p.Value)
		case "CALSCALE":
			cal.CalScale = strings.ToUpper(strings.TrimSpace(p.Value))
		}
	}
	switch {
	case cal.Version == "":
		b.is.add(CodeNoVersion, cal.Raw.Line, "")
	case cal.Version != "2.0":
		b.is.add(CodeVersion, cal.Raw.Line, cal.Version)
	}
	if opts != nil {
		if ct := strings.ToUpper(strings.TrimSpace(opts.ContentTypeMethod)); ct != "" {
			switch {
			case cal.Method == "":
				cal.Method = ct
				b.is.add(CodeMethodFromType, 0, ct)
			case cal.Method != ct:
				b.is.add(CodeMethodMismatch, 0, "METHOD:"+cal.Method+", Content-Type method="+ct)
			}
		}
	}

	for _, c := range cals {
		for _, ch := range c.Children {
			switch ch.Name {
			case "VEVENT":
				cal.Events = append(cal.Events, b.event(ch))
			case "VTODO":
				cal.Todos = append(cal.Todos, b.todo(ch))
			}
		}
	}
	return cal
}

// single are the properties an event or to-do has at most once.
var single = map[string]bool{
	"UID": true, "SEQUENCE": true, "STATUS": true, "SUMMARY": true,
	"DESCRIPTION": true, "LOCATION": true, "URL": true, "ORGANIZER": true,
	"DTSTART": true, "DTEND": true, "DURATION": true, "DUE": true,
	"RECURRENCE-ID": true, "CREATED": true, "LAST-MODIFIED": true,
	"DTSTAMP": true, "COMPLETED": true, "CLASS": true, "TRANSP": true,
	"METHOD": true, "PRODID": true, "VERSION": true, "CALSCALE": true,
}

// first reports whether p is the first of its name, reporting a repeat of a
// property that occurs once.
func (b *builder) first(seen map[string]bool, p *Property) bool {
	if !single[p.Name] {
		return true
	}
	if seen[p.Name] {
		b.is.add(CodeDuplicate, p.Line, p.Name)
		return false
	}
	seen[p.Name] = true
	return true
}

func (b *builder) event(c *Component) *Event {
	e := &Event{Component: c}
	seen := make(map[string]bool)
	hasEnd, hasDuration := false, false
	for _, p := range c.Properties {
		if !b.first(seen, p) {
			continue
		}
		switch p.Name {
		case "UID":
			e.UID = b.text(p)
		case "SEQUENCE":
			e.Sequence = b.integer(p)
		case "STATUS":
			e.Status = strings.ToUpper(strings.TrimSpace(p.Value))
		case "SUMMARY":
			e.Summary = b.text(p)
		case "DESCRIPTION":
			e.Description = b.text(p)
		case "LOCATION":
			e.Location = b.text(p)
		case "URL":
			e.URL = strings.TrimSpace(p.Value)
		case "ORGANIZER":
			e.Organizer = b.organizer(p)
		case "ATTENDEE":
			e.AttendeeCount++
			if len(e.Attendees) >= b.lim.MaxAttendees {
				if e.AttendeeCount == b.lim.MaxAttendees+1 {
					b.is.add(CodeLimitAttendees, p.Line, "")
				}
				continue
			}
			e.Attendees = append(e.Attendees, b.attendee(p))
		case "DTSTART":
			e.Start = b.dateTime(p)
			e.AllDay = e.Start.Kind == TimeDate
		case "DTEND":
			e.End = b.dateTime(p)
			hasEnd = true
		case "DURATION":
			e.Duration = b.duration(p)
			hasDuration = true
		case "RECURRENCE-ID":
			e.RecurrenceID = b.dateTime(p)
		case "RRULE":
			if e.RRule == nil {
				e.RRule = b.rrule(p)
			} else {
				b.is.add(CodeDuplicate, p.Line, p.Name)
			}
		case "EXDATE":
			e.ExDates = append(e.ExDates, b.dateTimes(p)...)
		case "CREATED":
			e.Created = b.dateTime(p)
		case "LAST-MODIFIED":
			e.LastModified = b.dateTime(p)
		case "DTSTAMP":
			e.DTStamp = b.dateTime(p)
		case "ATTACH":
			e.Attachments = append(e.Attachments, b.attachment(p))
		default:
			if strings.HasPrefix(p.Name, "X-") {
				e.X = append(e.X, p)
			}
		}
	}
	if e.UID == "" {
		b.is.add(CodeNoUID, c.Line, "VEVENT")
	}
	if hasEnd && hasDuration {
		b.is.add(CodeEndAndDuration, c.Line, "")
	}
	for _, ch := range c.Children {
		if ch.Name == "VALARM" {
			e.Alarms++
		}
	}
	return e
}

func (b *builder) todo(c *Component) *Todo {
	t := &Todo{Component: c}
	seen := make(map[string]bool)
	for _, p := range c.Properties {
		if !b.first(seen, p) {
			continue
		}
		switch p.Name {
		case "UID":
			t.UID = b.text(p)
		case "SEQUENCE":
			t.Sequence = b.integer(p)
		case "STATUS":
			t.Status = strings.ToUpper(strings.TrimSpace(p.Value))
		case "SUMMARY":
			t.Summary = b.text(p)
		case "DESCRIPTION":
			t.Description = b.text(p)
		case "DTSTART":
			t.Start = b.dateTime(p)
		case "DUE":
			t.Due = b.dateTime(p)
		case "COMPLETED":
			t.Completed = b.dateTime(p)
		case "DTSTAMP":
			t.DTStamp = b.dateTime(p)
		case "ORGANIZER":
			t.Organizer = b.organizer(p)
		}
	}
	if t.UID == "" {
		b.is.add(CodeNoUID, c.Line, "VTODO")
	}
	return t
}

// text is a TEXT value, unescaped, with invalid UTF-8 replaced.
func (b *builder) text(p *Property) string {
	return b.valid(UnescapeText(p.Value), p)
}

func (b *builder) valid(s string, p *Property) string {
	if utf8.ValidString(s) {
		return s
	}
	b.is.add(CodeInvalidUTF8, p.Line, p.Name)
	return strings.ToValidUTF8(s, replacement)
}

// param is a parameter value made valid, with an RFC 2047 encoded-word,
// which some writers put in CN, decoded.
func (b *builder) param(p *Property, name string) string {
	v := p.Param(name)
	if strings.Contains(v, "=?") && strings.Contains(v, "?=") {
		if d, err := (&mime.WordDecoder{}).DecodeHeader(v); err == nil && d != v {
			b.is.add(CodeParamEncodedWord, p.Line, name)
			v = d
		}
	}
	return b.valid(v, p)
}

func (b *builder) integer(p *Property) int {
	n, err := strconv.Atoi(strings.TrimSpace(p.Value))
	if err != nil || n < 0 {
		b.is.add(CodeBadInteger, p.Line, p.Name+":"+p.Value)
		return 0
	}
	return n
}

func (b *builder) duration(p *Property) Duration {
	d := Duration{Raw: strings.TrimSpace(p.Value)}
	d.Value, d.Valid = ParseDuration(d.Raw)
	if !d.Valid {
		b.is.add(CodeBadDuration, p.Line, d.Raw)
	}
	return d
}

func (b *builder) address(v string, p *Property) string {
	a, code := normalizeAddress(v)
	if code != "" {
		b.is.add(code, p.Line, v)
	}
	return b.valid(a, p)
}

func (b *builder) addresses(p *Property, name string) []string {
	vs := p.ParamValues(name)
	if len(vs) == 0 {
		return nil
	}
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = b.address(v, p)
	}
	return out
}

func (b *builder) organizer(p *Property) *Organizer {
	o := &Organizer{Raw: strings.TrimSpace(p.Value)}
	o.Address = b.address(o.Raw, p)
	o.CommonName = b.param(p, "CN")
	if s := p.Param("SENT-BY"); s != "" {
		o.SentBy = b.address(s, p)
	}
	return o
}

func (b *builder) attendee(p *Property) Attendee {
	a := Attendee{Raw: strings.TrimSpace(p.Value)}
	a.Address = b.address(a.Raw, p)
	a.CommonName = b.param(p, "CN")
	if s := p.Param("SENT-BY"); s != "" {
		a.SentBy = b.address(s, p)
	}
	a.Role = strings.ToUpper(strings.TrimSpace(p.Param("ROLE")))
	a.PartStat = strings.ToUpper(strings.TrimSpace(p.Param("PARTSTAT")))
	a.CUType = strings.ToUpper(strings.TrimSpace(p.Param("CUTYPE")))
	a.RSVP = strings.EqualFold(strings.TrimSpace(p.Param("RSVP")), "TRUE")
	a.DelegatedTo = b.addresses(p, "DELEGATED-TO")
	a.DelegatedFrom = b.addresses(p, "DELEGATED-FROM")
	a.Member = b.addresses(p, "MEMBER")
	return a
}

func (b *builder) dateTime(p *Property) DateTime {
	return b.parseDateTime(strings.TrimSpace(p.Value), p.Param("TZID"), p.Param("VALUE"), p.Line, true)
}

// dateTimes reads a comma-separated list, as EXDATE has.
func (b *builder) dateTimes(p *Property) []DateTime {
	tzid, vt := p.Param("TZID"), p.Param("VALUE")
	var out []DateTime
	for v := range strings.SplitSeq(p.Value, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, b.parseDateTime(v, tzid, vt, p.Line, true))
		}
	}
	return out
}

// parseDateTime reads one value. typed says that the value is a property's,
// where a DATE must say VALUE=DATE; an RRULE's UNTIL need not.
func (b *builder) parseDateTime(v, tzid, valueType string, line int, typed bool) DateTime {
	d := DateTime{Raw: v, TZID: tzid}
	r, ok := scanDateTime(v)
	if !ok {
		d.Kind = TimeInvalid
		b.is.add(CodeBadDateTime, line, v)
		return d
	}
	if r.loose {
		b.is.addOnce(CodeDateTimeLoose, line, v)
	}
	if strings.EqualFold(valueType, "DATE") && r.hasTime {
		b.is.addOnce(CodeDateTimeLoose, line, v)
		r.hasTime = false
	}
	if !r.hasTime {
		if typed && valueType == "" {
			b.is.addOnce(CodeDateNoValue, line, v)
		}
		d.Kind = TimeDate
		d.Wall = dateOf(r)
		return d
	}
	d.Wall = dateOf(r)
	if r.utc {
		if tzid != "" {
			b.is.addOnce(CodeTZIDWithUTC, line, tzid)
		}
		d.Kind = TimeUTC
		d.Time = d.Wall
		return d
	}
	if tzid == "" {
		d.Kind = TimeFloating
		return d
	}
	z := b.zones.resolve(tzid, line)
	if z.loc == nil {
		d.Kind = TimeUnknownZone
		return d
	}
	d.Kind = TimeZoned
	d.Zone, d.ZoneSource = z.zone, z.source
	d.Time = time.Date(r.y, time.Month(r.mo), r.d, r.h, r.mi, r.s, 0, z.loc)
	return d
}

func dateOf(r dtParts) time.Time {
	return time.Date(r.y, time.Month(r.mo), r.d, r.h, r.mi, r.s, 0, time.UTC)
}

func (b *builder) rrule(p *Property) *RRule {
	r := &RRule{Raw: strings.TrimSpace(p.Value), Parts: make(map[string]string)}
	n := 0
	for part := range strings.SplitSeq(r.Raw, ";") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		if n++; n > b.lim.MaxParams {
			b.is.add(CodeLimitParams, p.Line, "RRULE")
			break
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			b.is.add(CodeRRuleInvalid, p.Line, part)
			continue
		}
		k, v = strings.ToUpper(strings.TrimSpace(k)), strings.TrimSpace(v)
		if _, dup := r.Parts[k]; dup {
			b.is.add(CodeRRuleInvalid, p.Line, "repeated "+k)
			continue
		}
		r.Parts[k] = v
		switch k {
		case "FREQ":
			r.Freq = strings.ToUpper(v)
			switch r.Freq {
			case "SECONDLY", "MINUTELY", "HOURLY", "DAILY", "WEEKLY", "MONTHLY", "YEARLY":
			default:
				b.is.add(CodeRRuleInvalid, p.Line, part)
			}
		case "INTERVAL", "COUNT":
			x, err := strconv.Atoi(v)
			if err != nil || x < 1 {
				b.is.add(CodeRRuleInvalid, p.Line, part)
				continue
			}
			if k == "INTERVAL" {
				r.Interval = x
			} else {
				r.Count = x
			}
		case "UNTIL":
			r.Until = b.parseDateTime(v, "", "", p.Line, false)
		}
	}
	if r.Freq == "" {
		b.is.add(CodeRRuleNoFreq, p.Line, r.Raw)
	}
	if r.Count > 0 && !r.Until.IsZero() {
		b.is.add(CodeRRuleCountUntil, p.Line, r.Raw)
	}
	return r
}

func (b *builder) attachment(p *Property) Attachment {
	a := Attachment{FormatType: strings.TrimSpace(p.Param("FMTTYPE"))}
	for _, n := range []string{"X-FILENAME", "FILENAME", "X-APPLE-FILENAME"} {
		if f := b.param(p, n); f != "" {
			a.Filename = f
			break
		}
	}
	if !strings.EqualFold(p.Param("ENCODING"), "BASE64") && !strings.EqualFold(p.Param("VALUE"), "BINARY") {
		a.URI = strings.TrimSpace(p.Value)
		return a
	}
	a.Inline = true
	v := p.Value
	if strings.ContainsAny(v, " \t\r\n") {
		v = strings.Map(func(r rune) rune {
			if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, v)
	}
	// An upper bound whether or not the data is padded.
	a.Size = base64.RawStdEncoding.DecodedLen(len(v))
	if p.Truncated {
		a.Omitted = true
		return a
	}
	if a.Size > b.attach {
		a.Omitted = true
		b.is.add(CodeLimitAttachment, p.Line, strconv.Itoa(a.Size)+" bytes")
		return a
	}
	data, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(v, "="))
	}
	if err != nil {
		a.Invalid = true
		a.Size = 0
		b.is.add(CodeAttachmentBase64, p.Line, err.Error())
		return a
	}
	b.attach -= len(data)
	a.Data, a.Size = data, len(data)
	return a
}
