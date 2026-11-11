package icalendar

import (
	"net/url"
	"strconv"
	"strings"
	"time"
)

// UnescapeText undoes the TEXT escapes of RFC 5545, section 3.3.11: \n and \N
// are a line break, and \, \; \\ the character after the backslash. \: is
// read as a colon, which some writers escape; any other backslash is kept.
func UnescapeText(v string) string {
	if strings.IndexByte(v, '\\') < 0 {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c == '\\' && i+1 < len(v) {
			switch v[i+1] {
			case 'n', 'N':
				b.WriteByte('\n')
				i++
				continue
			case ',', ';', '\\', ':':
				b.WriteByte(v[i+1])
				i++
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// A TimeKind says what a DateTime is.
type TimeKind uint8

const (
	// TimeNone is an absent value.
	TimeNone TimeKind = iota
	// TimeInvalid is a value that did not parse; Raw holds it.
	TimeInvalid
	// TimeDate is a DATE: Wall holds the day. It is no instant.
	TimeDate
	// TimeFloating is a local date-time without zone: the same wall clock
	// wherever it is read. Wall holds it; it is no instant.
	TimeFloating
	// TimeUTC is a date-time in UTC: Time is the instant.
	TimeUTC
	// TimeZoned is a date-time with a TZID that was resolved: Time is the
	// instant, in that zone.
	TimeZoned
	// TimeUnknownZone is a date-time with a TZID that could not be resolved.
	// Wall and TZID are as written; the instant is unknown.
	TimeUnknownZone
)

func (k TimeKind) String() string {
	switch k {
	case TimeNone:
		return "none"
	case TimeInvalid:
		return "invalid"
	case TimeDate:
		return "date"
	case TimeFloating:
		return "floating"
	case TimeUTC:
		return "utc"
	case TimeZoned:
		return "zoned"
	case TimeUnknownZone:
		return "unknown-zone"
	}
	return "TimeKind(" + strconv.Itoa(int(k)) + ")"
}

// A DateTime is a DATE or DATE-TIME value.
type DateTime struct {
	Kind TimeKind
	// Raw is the value as written.
	Raw string
	// Wall is the date and time as written, with time.UTC carrying the
	// fields. It is an instant only when Kind is TimeUTC.
	Wall time.Time
	// Time is the instant, set only when Known.
	Time time.Time
	// TZID is the TZID parameter as written.
	TZID string
	// Zone is the IANA name the TZID resolved to, or for a zone made from a
	// VTIMEZONE of fixed offset its offset, as "UTC+09:00".
	Zone string
	// ZoneSource is how the TZID was resolved: "iana", "windows", "vtimezone"
	// (its X-LIC-LOCATION) or "offset" (a VTIMEZONE of one fixed offset).
	ZoneSource string
}

// Known reports whether the value is an instant.
func (d DateTime) Known() bool { return d.Kind == TimeUTC || d.Kind == TimeZoned }

// IsZero reports whether the value is absent.
func (d DateTime) IsZero() bool { return d.Kind == TimeNone }

// A Duration is a DURATION value. Days and weeks are nominal: 24 hours and 7
// days, whatever the zone does.
type Duration struct {
	Raw   string
	Value time.Duration
	Valid bool
}

// ParseDuration parses an RFC 5545 duration such as "PT1H30M", "-P1D" or
// "P2W".
func ParseDuration(v string) (time.Duration, bool) {
	s := strings.ToUpper(strings.TrimSpace(v))
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" || s[0] != 'P' {
		return 0, false
	}
	s = s[1:]
	var total time.Duration
	inTime, seen := false, false
	for s != "" {
		if s[0] == 'T' {
			if inTime {
				return 0, false
			}
			inTime = true
			s = s[1:]
			if s == "" {
				return 0, false
			}
			continue
		}
		n := 0
		for n < len(s) && s[n] >= '0' && s[n] <= '9' {
			n++
		}
		if n == 0 || n > 9 || n == len(s) {
			return 0, false
		}
		num, _ := strconv.Atoi(s[:n])
		var unit time.Duration
		switch u := s[n]; {
		case u == 'W' && !inTime:
			unit = 7 * 24 * time.Hour
		case u == 'D' && !inTime:
			unit = 24 * time.Hour
		case u == 'H' && inTime:
			unit = time.Hour
		case u == 'M' && inTime:
			unit = time.Minute
		case u == 'S' && inTime:
			unit = time.Second
		default:
			return 0, false
		}
		d := time.Duration(num) * unit
		if d/unit != time.Duration(num) || total > (1<<62)-d {
			return 0, false
		}
		total += d
		seen = true
		s = s[n+1:]
	}
	if !seen {
		return 0, false
	}
	if neg {
		total = -total
	}
	return total, true
}

type dtParts struct {
	y, mo, d, h, mi, s  int
	hasTime, utc, loose bool
}

// scanDateTime reads YYYYMMDD and YYYYMMDDTHHMMSS[Z], and tolerates the ISO
// 8601 extended form, a lower-case t or z, and missing seconds.
func scanDateTime(v string) (dtParts, bool) {
	var r dtParts
	if strings.ContainsAny(v, "-:") {
		r.loose = true
		v = strings.Map(func(c rune) rune {
			if c == '-' || c == ':' {
				return -1
			}
			return c
		}, v)
	}
	if len(v) < 8 {
		return r, false
	}
	var ok [3]bool
	r.y, ok[0] = digits(v[0:4])
	r.mo, ok[1] = digits(v[4:6])
	r.d, ok[2] = digits(v[6:8])
	if !ok[0] || !ok[1] || !ok[2] {
		return r, false
	}
	if rest := v[8:]; rest != "" {
		if rest[0] != 'T' && rest[0] != 't' {
			return r, false
		}
		if rest[0] == 't' {
			r.loose = true
		}
		rest = rest[1:]
		if n := len(rest); n > 0 && (rest[n-1] == 'Z' || rest[n-1] == 'z') {
			r.utc = true
			r.loose = r.loose || rest[n-1] == 'z'
			rest = rest[:n-1]
		}
		var okh, okm, oks bool
		switch len(rest) {
		case 6:
			r.h, okh = digits(rest[0:2])
			r.mi, okm = digits(rest[2:4])
			r.s, oks = digits(rest[4:6])
		case 4:
			r.h, okh = digits(rest[0:2])
			r.mi, okm = digits(rest[2:4])
			oks = true
			r.loose = true
		default:
			return r, false
		}
		if !okh || !okm || !oks {
			return r, false
		}
		r.hasTime = true
	}
	if r.mo < 1 || r.mo > 12 || r.d < 1 || r.d > daysIn(r.y, r.mo) || r.h > 23 || r.mi > 59 || r.s > 60 {
		return r, false
	}
	if r.s == 60 {
		r.s = 59
		r.loose = true
	}
	return r, true
}

func digits(s string) (int, bool) {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

func daysIn(y, m int) int {
	return time.Date(y, time.Month(m)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// NormalizeAddress returns the address a cal-address value names: for a
// mailto: URI, in any case of the scheme, the address after it with percent
// escapes decoded and any "?" query dropped; for another URI, the value as
// written; for a bare address, the address.
func NormalizeAddress(v string) string {
	a, _ := normalizeAddress(v)
	return a
}

func normalizeAddress(v string) (string, Code) {
	s := trimAngle(strings.TrimSpace(v))
	scheme, rest, ok := strings.Cut(s, ":")
	if !ok || !isSchemeName(scheme) {
		if strings.Contains(s, "@") {
			return s, CodeAddressNoScheme
		}
		return s, ""
	}
	if !strings.EqualFold(scheme, "mailto") {
		return s, ""
	}
	rest = trimAngle(strings.TrimPrefix(strings.TrimSpace(rest), "//"))
	if q := strings.IndexByte(rest, '?'); q >= 0 {
		rest = rest[:q]
	}
	if strings.IndexByte(rest, '%') >= 0 {
		d, err := url.PathUnescape(rest)
		if err != nil {
			return rest, CodeAddressEscape
		}
		rest = strings.ToValidUTF8(d, replacement)
	}
	return strings.TrimSpace(rest), ""
}

func trimAngle(s string) string {
	if len(s) >= 2 && s[0] == '<' && s[len(s)-1] == '>' {
		return strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

func isSchemeName(s string) bool {
	if s == "" || !(s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}
