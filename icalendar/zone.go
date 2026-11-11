package icalendar

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// windowsZones maps common Windows time zone names, and the display names
// some Outlook versions write after "(UTC+hh:mm) ", lower-cased, to IANA
// zones. It is deliberately small; a name not in it stays unresolved unless
// the calendar's VTIMEZONE settles it.
var windowsZones = map[string]string{
	"korea standard time":                              "Asia/Seoul",
	"tokyo standard time":                              "Asia/Tokyo",
	"china standard time":                              "Asia/Shanghai",
	"taipei standard time":                             "Asia/Taipei",
	"singapore standard time":                          "Asia/Singapore",
	"se asia standard time":                            "Asia/Bangkok",
	"india standard time":                              "Asia/Kolkata",
	"arabian standard time":                            "Asia/Dubai",
	"israel standard time":                             "Asia/Jerusalem",
	"pacific standard time":                            "America/Los_Angeles",
	"mountain standard time":                           "America/Denver",
	"us mountain standard time":                        "America/Phoenix",
	"central standard time":                            "America/Chicago",
	"eastern standard time":                            "America/New_York",
	"alaskan standard time":                            "America/Anchorage",
	"hawaiian standard time":                           "Pacific/Honolulu",
	"atlantic standard time":                           "America/Halifax",
	"canada central standard time":                     "America/Regina",
	"central standard time (mexico)":                   "America/Mexico_City",
	"e. south america standard time":                   "America/Sao_Paulo",
	"argentina standard time":                          "America/Argentina/Buenos_Aires",
	"gmt standard time":                                "Europe/London",
	"greenwich standard time":                          "Atlantic/Reykjavik",
	"w. europe standard time":                          "Europe/Berlin",
	"romance standard time":                            "Europe/Paris",
	"central europe standard time":                     "Europe/Budapest",
	"central european standard time":                   "Europe/Warsaw",
	"fle standard time":                                "Europe/Helsinki",
	"gtb standard time":                                "Europe/Bucharest",
	"russian standard time":                            "Europe/Moscow",
	"turkey standard time":                             "Europe/Istanbul",
	"egypt standard time":                              "Africa/Cairo",
	"south africa standard time":                       "Africa/Johannesburg",
	"aus eastern standard time":                        "Australia/Sydney",
	"e. australia standard time":                       "Australia/Brisbane",
	"w. australia standard time":                       "Australia/Perth",
	"new zealand standard time":                        "Pacific/Auckland",
	"utc":                                              "UTC",
	"seoul":                                            "Asia/Seoul",
	"osaka, sapporo, tokyo":                            "Asia/Tokyo",
	"beijing, chongqing, hong kong, urumqi":            "Asia/Shanghai",
	"pacific time (us & canada)":                       "America/Los_Angeles",
	"mountain time (us & canada)":                      "America/Denver",
	"central time (us & canada)":                       "America/Chicago",
	"eastern time (us & canada)":                       "America/New_York",
	"dublin, edinburgh, lisbon, london":                "Europe/London",
	"amsterdam, berlin, bern, rome, stockholm, vienna": "Europe/Berlin",
	"brussels, copenhagen, madrid, paris":              "Europe/Paris",
}

// loaded caches the zones time.LoadLocation found, so that one name always
// gives the same *time.Location. Only names that loaded are kept, and those
// are the host's zone files: the cache is bounded by them.
var loaded sync.Map

type zoneResult struct {
	loc    *time.Location
	zone   string
	source string
}

// zones resolves the TZIDs of one calendar.
type zones struct {
	is    *issues
	vtz   map[string]*Component
	cache map[string]zoneResult
	loads int
}

func newZones(is *issues, vtimezones []*Component) *zones {
	z := &zones{is: is, vtz: make(map[string]*Component), cache: make(map[string]zoneResult)}
	for _, c := range vtimezones {
		if p := c.Prop("TZID"); p != nil {
			id := strings.TrimSpace(p.Value)
			if _, dup := z.vtz[id]; !dup {
				z.vtz[id] = c
			}
		}
	}
	return z
}

// resolve returns the zone of tzid, or nil. An unresolved TZID and one
// resolved through anything but its own IANA name are reported once.
func (z *zones) resolve(tzid string, line int) zoneResult {
	if r, ok := z.cache[tzid]; ok {
		return r
	}
	if len(z.cache) >= maxZones {
		z.is.addOnce(CodeLimitZones, line, tzid)
		return zoneResult{}
	}
	r := z.lookup(tzid)
	z.cache[tzid] = r
	switch {
	case r.loc == nil:
		z.is.add(CodeUnknownTZID, line, tzid)
	case r.source != "iana" || r.zone != strings.TrimSpace(tzid):
		z.is.add(CodeZoneMapped, line, fmt.Sprintf("%s -> %s (%s)", tzid, r.zone, r.source))
	}
	return r
}

func (z *zones) lookup(tzid string) zoneResult {
	name := strings.TrimSpace(tzid)
	if loc := z.load(name); loc != nil {
		return zoneResult{loc, name, "iana"}
	}
	// A globally unique TZID, "/mozilla.org/20050126_1/Asia/Seoul": try the
	// IANA name at its end.
	if strings.HasPrefix(name, "/") {
		rest := name
		for range 8 {
			i := strings.IndexByte(rest, '/')
			if i < 0 {
				break
			}
			rest = rest[i+1:]
			if loc := z.load(rest); loc != nil {
				return zoneResult{loc, rest, "iana"}
			}
		}
	}
	key := strings.ToLower(name)
	if strings.HasPrefix(key, "(utc") || strings.HasPrefix(key, "(gmt") {
		if i := strings.IndexByte(key, ')'); i >= 0 {
			key = strings.TrimSpace(key[i+1:])
		}
	}
	if iana, ok := windowsZones[key]; ok {
		if loc := z.load(iana); loc != nil {
			return zoneResult{loc, iana, "windows"}
		}
	}
	c := z.vtz[name]
	if c == nil {
		return zoneResult{}
	}
	if p := c.Prop("X-LIC-LOCATION"); p != nil {
		iana := strings.TrimSpace(p.Value)
		if loc := z.load(iana); loc != nil {
			return zoneResult{loc, iana, "vtimezone"}
		}
	}
	if off, ok := fixedOffset(c); ok {
		zone := formatOffset(off)
		return zoneResult{time.FixedZone(zone, off), zone, "offset"}
	}
	return zoneResult{}
}

// load returns the IANA zone of name, or nil. Every call counts against
// maxZoneLoads, cached or not, so that what resolves does not depend on
// what was parsed before.
func (z *zones) load(name string) *time.Location {
	if !ianaName(name) || z.loads >= maxZoneLoads {
		return nil
	}
	z.loads++
	if v, ok := loaded.Load(name); ok {
		return v.(*time.Location)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil
	}
	v, _ := loaded.LoadOrStore(name, loc)
	return v.(*time.Location)
}

// ianaName reports whether name can be an IANA zone name. "Local", which
// would be the host's zone, is refused: that would be a guess.
func ianaName(name string) bool {
	if name == "" || len(name) > 64 || name == "Local" || strings.Contains(name, "..") || strings.Contains(name, "//") {
		return false
	}
	if c := name[0]; !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') || name[len(name)-1] == '/' {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '/' || c == '_' || c == '-' || c == '+') {
			return false
		}
	}
	return true
}

// fixedOffset returns the offset of a VTIMEZONE whose every STANDARD and
// DAYLIGHT has the same TZOFFSETTO: a zone without changes, which
// Outlook writes for zones such as Korea Standard Time.
func fixedOffset(c *Component) (int, bool) {
	off, n := 0, 0
	for _, ch := range c.Children {
		if ch.Name != "STANDARD" && ch.Name != "DAYLIGHT" {
			continue
		}
		p := ch.Prop("TZOFFSETTO")
		if p == nil {
			return 0, false
		}
		o, ok := parseOffset(p.Value)
		if !ok || n > 0 && o != off {
			return 0, false
		}
		off = o
		n++
	}
	return off, n > 0
}

// parseOffset reads a UTC-OFFSET: +hhmm or -hhmm, with optional seconds.
func parseOffset(v string) (int, bool) {
	s := strings.TrimSpace(v)
	if len(s) != 5 && len(s) != 7 || s[0] != '+' && s[0] != '-' {
		return 0, false
	}
	h, ok1 := digits(s[1:3])
	m, ok2 := digits(s[3:5])
	sec, ok3 := 0, true
	if len(s) == 7 {
		sec, ok3 = digits(s[5:7])
	}
	if !ok1 || !ok2 || !ok3 || h > 23 || m > 59 || sec > 59 {
		return 0, false
	}
	off := h*3600 + m*60 + sec
	if s[0] == '-' {
		off = -off
	}
	return off, true
}

func formatOffset(off int) string {
	sign := '+'
	if off < 0 {
		sign = '-'
		off = -off
	}
	return fmt.Sprintf("UTC%c%02d:%02d", sign, off/3600, off/60%60)
}
