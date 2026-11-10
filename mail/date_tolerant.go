package mail

import (
	"strconv"
	"strings"
	"time"

	"github.com/saeroun-ai/emlkit"
)

// A DateResult is what the tolerant date parser understood of a date-time.
type DateResult struct {
	// Raw is the field value that was parsed. It is kept whether or not a
	// time could be derived from it.
	Raw string

	// Time is the parsed time, in a fixed zone of the offset the value gave
	// (UTC when it gave none or gave "-0000"). It is the zero time when Valid
	// is false.
	Time time.Time

	// Valid reports whether a time was derived from Raw.
	Valid bool

	// Diagnostics lists what was repaired or assumed, or why no time could
	// be derived. It is empty for a date RFC 5322 accepts as written in its
	// current syntax.
	Diagnostics []message.Diagnostic
}

// ParseDateTolerant parses an RFC 5322 date-time and the obsolete and damaged
// forms commonly found in mail. What it accepts beyond the current syntax is
// reported in the result's diagnostics:
//
//   - obsolete zone names: UT, GMT and the North American zones of RFC 5322
//     (date.obsolete_zone, info), the military letters, which RFC 5322 reads
//     as -0000, a few further unambiguous names such as UTC, CET or KST, and
//     any other name, which is read as +0000 (date.obsolete_zone, recovered);
//   - a missing zone, read as +0000 (date.missing_zone);
//   - a weekday that does not match the date, which is ignored
//     (date.weekday_mismatch);
//   - two- and three-digit years, read as RFC 5322 section 4.3 defines them;
//     a weekday without its comma or in full, a month in full; a zone such as
//     "+09:00", "+9" or "GMT+0900"; a missing time; an AM/PM marker;
//     fractional seconds; month-first dates in the manner of asctime; ISO 8601
//     and dash-separated dates; text after the zone (date.invalid, recovered).
//
// Comments are skipped wherever they appear. A date that cannot be read — a
// field out of range, a day the month does not have, a numeric or unknown
// month — is reported with date.invalid at invalid_metadata severity; Valid is
// then false and Raw still holds the value.
//
// Only the first dozen words of the value are examined, so the work does not
// grow with text that follows a date.
func ParseDateTolerant(date string) DateResult {
	return parseDateResult("", date)
}

// DateTolerant parses the Date header field, accepting the obsolete and
// damaged forms ParseDateTolerant describes. If the header field is missing or
// empty, it returns the zero DateResult.
//
// Unlike Date, which leaves a zone name to the time package and so gives an
// unknown one a zero offset, it applies the offsets RFC 5322 defines for the
// obsolete zone names.
func (h *Header) DateTolerant() DateResult {
	v := h.Get("Date")
	if v == "" {
		return DateResult{}
	}
	return parseDateResult("Date", v)
}

func parseDateResult(field, raw string) DateResult {
	d := diagnosticList{field: field}
	t, ok := parseDateTolerant(raw, &d)
	return DateResult{Raw: raw, Time: t, Valid: ok, Diagnostics: d.list}
}

// maxDateFields is the number of words of a date value that are examined. The
// longest form read here has eight.
const maxDateFields = 12

var (
	weekdayNames = [...]string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}
	monthNames   = [...]string{"january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"}
)

// A dateZone is a zone name with a known offset. rfc marks the obsolete zones
// RFC 5322 section 4.3 defines.
type dateZone struct {
	name   string
	offset int // seconds east of UTC
	rfc    bool
}

var dateZones = [...]dateZone{
	{"UT", 0, true}, {"GMT", 0, true},
	{"EST", -5 * 3600, true}, {"EDT", -4 * 3600, true},
	{"CST", -6 * 3600, true}, {"CDT", -5 * 3600, true},
	{"MST", -7 * 3600, true}, {"MDT", -6 * 3600, true},
	{"PST", -8 * 3600, true}, {"PDT", -7 * 3600, true},
	{"UTC", 0, false}, {"Z", 0, false},
	{"AST", -4 * 3600, false}, {"ADT", -3 * 3600, false},
	{"WET", 0, false}, {"WEST", 1 * 3600, false},
	{"CET", 1 * 3600, false}, {"CEST", 2 * 3600, false},
	{"EET", 2 * 3600, false}, {"EEST", 3 * 3600, false},
	{"JST", 9 * 3600, false}, {"KST", 9 * 3600, false},
}

// dateFields splits a date value into its words: runs of bytes separated by
// whitespace, commas and comments. It stops after maxDateFields words (more
// reports that) or at a comment that is never closed (openComment reports
// that), so it reads past the date only to skip comments.
func dateFields(s string) (fields []string, more, openComment bool) {
	fields = make([]string, 0, maxDateFields)
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case isWSP(c) || c == ',':
			i++
		case c == '(':
			end, ok := scanComment(s, i)
			if !ok {
				return fields, false, true
			}
			i = end
		default:
			j := i
			for j < len(s) && !isWSP(s[j]) && s[j] != ',' && s[j] != '(' {
				j++
			}
			if len(fields) == maxDateFields {
				return fields, true, false
			}
			fields = append(fields, s[i:j])
			i = j
		}
	}
	return fields, false, false
}

// lookupDateName finds word, without a trailing dot, among the lower-case
// English names: it matches a name it is a prefix of when it has at least
// three letters. standard reports the three-letter form RFC 5322 uses.
func lookupDateName(word string, names []string) (index int, standard, ok bool) {
	word = strings.TrimSuffix(word, ".")
	if len(word) < 3 {
		return 0, false, false
	}
	for i, name := range names {
		if len(word) <= len(name) && strings.EqualFold(name[:len(word)], word) {
			return i, len(word) == 3, true
		}
	}
	return 0, false, false
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// smallNumber parses s as a number of one to maxDigits digits.
func smallNumber(s string, maxDigits int) (int, bool) {
	if len(s) > maxDigits || !allDigits(s) {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func parseDateTolerant(raw string, d *diagnosticList) (time.Time, bool) {
	var notes []string
	fail := func(why string) (time.Time, bool) {
		d.invalid(message.CodeDateInvalid,
			"mail: invalid date "+quoteForMessage(raw)+": "+why,
			"kept the raw value; no time was derived")
		return time.Time{}, false
	}

	fields, more, openComment := dateFields(raw)
	if openComment {
		notes = append(notes, "ignored a comment that is never closed and the text in it")
	}
	if len(fields) == 0 {
		return fail("no date")
	}

	// Weekday.
	weekday := -1
	if isAlpha(fields[0][0]) {
		if wd, standard, ok := lookupDateName(fields[0], weekdayNames[:]); ok {
			weekday = wd
			if !standard {
				notes = append(notes, "read the weekday "+quoteForMessage(fields[0]))
			}
			fields = fields[1:]
		} else if _, _, isMonth := lookupDateName(fields[0], monthNames[:]); !isMonth && len(fields) > 3 {
			notes = append(notes, "ignored the unrecognized weekday "+quoteForMessage(fields[0]))
			fields = fields[1:]
		}
	}
	if len(fields) == 0 {
		return fail("no day of month")
	}

	// Day and month, and for a date written as one word the year too.
	var (
		dayS, monthS, yearS string
		numericMonth        bool
	)
	switch first := fields[0]; {
	case strings.Count(first, "-") >= 2:
		parts := strings.SplitN(first, "-", 3)
		fields = fields[1:]
		if len(parts[0]) == 4 && allDigits(parts[0]) && allDigits(parts[1]) {
			// ISO 8601: yyyy-mm-dd, optionally followed by 'T' and the time.
			yearS, monthS, numericMonth = parts[0], parts[1], true
			n := 0
			for n < len(parts[2]) && isDigit(parts[2][n]) {
				n++
			}
			dayS = parts[2][:n]
			if rest := parts[2][n:]; rest != "" {
				if (rest[0] != 'T' && rest[0] != 't') || len(rest) == 1 {
					return fail("unrecognized date " + quoteForMessage(first))
				}
				fields = append([]string{rest[1:]}, fields...)
			}
			notes = append(notes, "read the ISO 8601 date "+quoteForMessage(first))
		} else {
			dayS, monthS, yearS = parts[0], parts[1], parts[2]
			notes = append(notes, "read the dash-separated date "+quoteForMessage(first))
		}
	case isAlpha(first[0]):
		if len(fields) < 2 {
			return fail("no day of month")
		}
		monthS, dayS = first, fields[1]
		fields = fields[2:]
		notes = append(notes, "read a date that puts the month before the day")
	default:
		if len(fields) < 2 {
			return fail("no month")
		}
		dayS, monthS = first, fields[1]
		fields = fields[2:]
	}

	day, ok := smallNumber(dayS, 2)
	if !ok || day < 1 {
		return fail("invalid day of month " + quoteForMessage(dayS))
	}
	var month time.Month
	if numericMonth {
		m, ok := smallNumber(monthS, 2)
		if !ok || m < 1 || m > 12 {
			return fail("invalid month " + quoteForMessage(monthS))
		}
		month = time.Month(m)
	} else {
		m, standard, ok := lookupDateName(monthS, monthNames[:])
		if !ok || !isAlpha(monthS[0]) {
			return fail("unrecognized month " + quoteForMessage(monthS))
		}
		if !standard {
			notes = append(notes, "read the month "+quoteForMessage(monthS))
		}
		month = time.Month(m + 1)
	}

	// Year and time, in either order.
	if yearS == "" && len(fields) > 0 && !looksLikeClock(fields[0]) {
		yearS, fields = fields[0], fields[1:]
	}
	var hour, minute, second int
	if len(fields) > 0 && looksLikeClock(fields[0]) {
		var (
			rest string
			note string
		)
		hour, minute, second, rest, note, ok = parseClock(fields[0])
		if !ok {
			return fail("invalid time " + quoteForMessage(fields[0]))
		}
		if note != "" {
			notes = append(notes, note)
		}
		if rest != "" {
			// A zone or AM/PM marker written against the time, as in
			// "10:00:00+0900".
			fields[0] = rest
			notes = append(notes, "split "+quoteForMessage(rest)+", written against the time, from it")
		} else {
			fields = fields[1:]
		}
	} else {
		notes = append(notes, "assumed 00:00:00 for the missing time")
	}
	if yearS == "" {
		// The year after the time, and possibly after the zone, as asctime
		// and date(1) write it.
		for k := 0; k < len(fields) && k < 2; k++ {
			if n := len(fields[k]); allDigits(fields[k]) && (n == 4 || (n == 2 && k == 0)) {
				yearS = fields[k]
				fields = append(fields[:k:k], fields[k+1:]...)
				break
			}
		}
		if yearS == "" {
			return fail("no year")
		}
	}
	year, ok := smallNumber(yearS, 4)
	if !ok {
		return fail("invalid year " + quoteForMessage(yearS))
	}
	switch len(yearS) {
	case 4:
	case 3:
		year += 1900
		notes = append(notes, "read the obsolete year "+yearS+" as "+strconv.Itoa(year))
	case 2:
		if year < 50 {
			year += 2000
		} else {
			year += 1900
		}
		notes = append(notes, "read the obsolete year "+yearS+" as "+strconv.Itoa(year))
	default:
		return fail("invalid year " + quoteForMessage(yearS))
	}
	if year < 1 {
		return fail("invalid year " + quoteForMessage(yearS))
	}

	// AM/PM marker.
	if len(fields) > 0 && (strings.EqualFold(fields[0], "AM") || strings.EqualFold(fields[0], "PM")) {
		pm := fields[0][0] == 'P' || fields[0][0] == 'p'
		if hour >= 1 && hour <= 12 {
			if pm && hour < 12 {
				hour += 12
			} else if !pm && hour == 12 {
				hour = 0
			}
			notes = append(notes, "applied the "+strings.ToUpper(fields[0])+" marker")
		} else {
			notes = append(notes, "ignored the "+strings.ToUpper(fields[0])+" marker of a 24-hour time")
		}
		fields = fields[1:]
	}

	// The day has to exist in its month.
	if date := time.Date(year, month, day, 0, 0, 0, 0, time.UTC); date.Day() != day || date.Month() != month {
		return fail("day " + strconv.Itoa(day) + " does not exist in that month")
	} else if weekday >= 0 && int(date.Weekday()) != weekday {
		d.recovered(message.CodeDateWeekdayMismatch,
			"mail: date is a "+date.Weekday().String()+", not a "+time.Weekday(weekday).String(),
			"ignored the weekday")
	}

	loc, used := parseDateZone(fields, d, &notes)
	fields = fields[used:]
	if len(fields) > 0 || more {
		notes = append(notes, "ignored the text after the date")
	}
	if len(notes) > 0 {
		d.recovered(message.CodeDateInvalid,
			"mail: date "+quoteForMessage(raw)+" is not in current RFC 5322 syntax",
			strings.Join(notes, "; "))
	}
	return time.Date(year, month, day, hour, minute, second, 0, loc), true
}

// looksLikeClock reports whether word starts a time of day: digits, then ':'
// or '.', then a digit.
func looksLikeClock(word string) bool {
	i := 0
	for i < len(word) && isDigit(word[i]) {
		i++
	}
	return i >= 1 && i <= 2 && i+1 < len(word) && (word[i] == ':' || word[i] == '.') && isDigit(word[i+1])
}

// parseClock parses "hh:mm", "hh:mm:ss" and "hh:mm:ss.fraction", with '.'
// accepted in place of ':'. rest is what follows the time in the same word.
func parseClock(word string) (hour, minute, second int, rest, note string, ok bool) {
	number := func() (int, bool) {
		n := 0
		for n < len(word) && n < 2 && isDigit(word[n]) {
			n++
		}
		if n == 0 || (n < len(word) && isDigit(word[n])) {
			return 0, false
		}
		v, _ := strconv.Atoi(word[:n])
		word = word[n:]
		return v, true
	}
	if hour, ok = number(); !ok || word == "" {
		return 0, 0, 0, "", "", false
	}
	sep := word[0]
	word = word[1:]
	if minute, ok = number(); !ok {
		return 0, 0, 0, "", "", false
	}
	if sep == '.' {
		note = "read a time written with '.' separators"
	}
	if len(word) > 1 && word[0] == sep && isDigit(word[1]) {
		word = word[1:]
		if second, ok = number(); !ok {
			return 0, 0, 0, "", "", false
		}
		if len(word) > 1 && (word[0] == '.' || word[0] == ',') && isDigit(word[1]) {
			n := 1
			for n < len(word) && isDigit(word[n]) {
				n++
			}
			word = word[n:]
			note = "dropped the fractional seconds"
		}
	}
	// A second of 60 is a leap second; time.Date carries it into the next
	// minute.
	if hour > 23 || minute > 59 || second > 60 {
		return 0, 0, 0, "", "", false
	}
	return hour, minute, second, word, note, true
}

// parseDateZone reads the zone from the words after the time and returns the
// location for it and the number of words it used. A zone that is missing or
// cannot be read is +0000, and reported.
func parseDateZone(fields []string, d *diagnosticList, notes *[]string) (loc *time.Location, used int) {
	if len(fields) == 0 {
		d.recovered(message.CodeDateMissingZone, "mail: date has no zone", "assumed +0000")
		return time.UTC, 0
	}
	word := fields[0]
	if word[0] == '+' || word[0] == '-' {
		offset, standard, ok := parseZoneOffset(word)
		if !ok {
			*notes = append(*notes, "assumed +0000 for the invalid zone "+quoteForMessage(word))
			return time.UTC, 1
		}
		if !standard {
			*notes = append(*notes, "read the zone "+quoteForMessage(word)+" as "+formatZoneOffset(offset))
		}
		return fixedZone("", offset), 1
	}
	if !isAlpha(word[0]) {
		d.recovered(message.CodeDateMissingZone, "mail: date has no zone", "assumed +0000")
		return time.UTC, 0
	}

	n := 0
	for n < len(word) && isAlpha(word[n]) {
		n++
	}
	name, tail := strings.ToUpper(word[:n]), word[n:]
	if name == "GMT" || name == "UTC" || name == "UT" {
		// "GMT+0900" in one word, or "GMT +0900" in two.
		offsetWord, words := tail, 1
		if tail == "" && len(fields) > 1 {
			offsetWord, words = fields[1], 2
		}
		if offsetWord != "" && (offsetWord[0] == '+' || offsetWord[0] == '-') {
			if offset, _, ok := parseZoneOffset(offsetWord); ok {
				*notes = append(*notes, "read the zone "+quoteForMessage(name+offsetWord)+" as "+formatZoneOffset(offset))
				return fixedZone("", offset), words
			}
		}
	}
	if tail != "" {
		*notes = append(*notes, "assumed +0000 for the invalid zone "+quoteForMessage(word))
		return time.UTC, 1
	}

	for _, z := range dateZones {
		if z.name != name {
			continue
		}
		if z.rfc {
			d.add(message.CodeDateObsoleteZone, message.SeverityInfo,
				"mail: date uses the obsolete zone name "+name,
				"read it as "+formatZoneOffset(z.offset))
		} else {
			d.recovered(message.CodeDateObsoleteZone,
				"mail: date uses the zone name "+name+", which RFC 5322 does not define",
				"read it as "+formatZoneOffset(z.offset))
		}
		return fixedZone(name, z.offset), 1
	}
	if len(name) == 1 && name != "J" {
		d.recovered(message.CodeDateObsoleteZone,
			"mail: date uses the military zone "+name,
			"read it as -0000, as RFC 5322 section 4.3 requires")
		return time.UTC, 1
	}
	d.recovered(message.CodeDateObsoleteZone,
		"mail: date uses the unknown zone name "+quoteForMessage(word[:n]),
		"read it as +0000")
	return time.UTC, 1
}

// parseZoneOffset parses a signed zone offset. standard reports the "+hhmm"
// form; "+hh:mm", "+hmm" and "+h" to "+hh" are read as well.
func parseZoneOffset(word string) (offset int, standard, ok bool) {
	sign := 1
	if word[0] == '-' {
		sign = -1
	}
	body := word[1:]
	var hh, mm string
	switch {
	case len(body) == 5 && body[2] == ':':
		hh, mm = body[:2], body[3:]
	case len(body) == 4:
		hh, mm, standard = body[:2], body[2:], true
	case len(body) == 3:
		hh, mm = body[:1], body[1:]
	case len(body) == 1 || len(body) == 2:
		hh, mm = body, "00"
	default:
		return 0, false, false
	}
	h, okH := smallNumber(hh, 2)
	m, okM := smallNumber(mm, 2)
	if !okH || !okM || h > 23 || m > 59 {
		return 0, false, false
	}
	return sign * (h*3600 + m*60), standard, true
}

func formatZoneOffset(offset int) string {
	sign := byte('+')
	if offset < 0 {
		sign, offset = '-', -offset
	}
	h, m := offset/3600, offset%3600/60
	return string([]byte{sign, byte('0' + h/10), byte('0' + h%10), byte('0' + m/10), byte('0' + m%10)})
}

func fixedZone(name string, offset int) *time.Location {
	if offset == 0 && name == "" {
		return time.UTC
	}
	return time.FixedZone(name, offset)
}
