# iCalendar convenience parser (ROB-140-003)

Code: `icalendar/`. Tests: `icalendar/fixtures_test.go` (invite, update,
cancel, reply, all-day, recurring, Outlook, Google), `malformed_test.go`
(damaged inputs and budgets), `fuzz_test.go` (`FuzzParse`).

An optional package for the `text/calendar` parts of mail (RFC 6047): the
format of RFC 5545 and the scheduling methods of RFC 5546. The core does not
import it, and it imports nothing of the module; it has its own `Issue` type.

## Two layers

- **Raw**, `ParseRaw(data, opts) *Document`. Lines are unfolded and split into
  name, parameters and raw value; BEGIN/END build the component tree. No value
  is interpreted. Every `Property` keeps its order, its unfolded `Raw` line
  and its physical `Line`. This is the fallback: whatever the convenience
  layer does not model is here (`Calendar.Raw`, `Event.Component`).
- **Convenience**, `ParseBytes(data, opts) *Calendar` and `Parse(r, opts)`.
  `Calendar{Method, ProdID, Version, CalScale, Events, Todos, Timezones}`;
  `Event` with UID, SEQUENCE, STATUS, the TEXT properties unescaped, URL,
  ORGANIZER and ATTENDEE (address, CN, SENT-BY, ROLE, PARTSTAT, RSVP, CUTYPE,
  DELEGATED-TO/FROM, MEMBER), DTSTART/DTEND/DURATION, all-day, RECURRENCE-ID,
  RRULE (raw, every part, and FREQ/INTERVAL/COUNT/UNTIL split), EXDATE,
  CREATED/LAST-MODIFIED/DTSTAMP, ATTACH (URI or inline base64), the number of
  VALARMs and every X- property. `Todo` is light: UID, SEQUENCE, STATUS,
  SUMMARY, DESCRIPTION, DTSTART, DUE, COMPLETED, DTSTAMP, ORGANIZER.
  `Timezone` is TZID, X-LIC-LOCATION and the raw component.

Both always return a result. `Parse` returns an error only for a reader's
failure, together with the calendar of what was read.

## Values

- TEXT: `\n` `\N` `\,` `\;` `\\` are unescaped, and `\:`, which some writers
  produce; another backslash is kept. Invalid UTF-8 is replaced
  (`value.invalid_utf8`).
- Parameters: quoted values, value lists, RFC 6868 carets (`^n`, `^^`, `^'`).
  A CN holding an RFC 2047 encoded-word is decoded (`param.encoded_word`).
- Addresses: `mailto:` in any case is removed, percent escapes decoded, a
  `?query` and angle brackets dropped; `Raw` keeps the value as written. Other
  URIs (`urn:uuid:`) stay as written. `NormalizeAddress` is exported for
  comparing with mail addresses.
- Date-times (`DateTime.Kind`): DATE, floating, UTC and TZID. `Wall` always
  holds the written fields; `Time` is set only when the instant is known
  (`Known()`). A TZID resolves, in this order, as an IANA name through
  `time.LoadLocation`; as the IANA name at the end of a `/`-prefixed global
  TZID; through a small table of Windows names ("Korea Standard Time",
  "Tokyo Standard Time", "Pacific Standard Time", ...) and Outlook display
  names ("(UTC+09:00) Seoul"); through the VTIMEZONE's X-LIC-LOCATION; or as a
  fixed zone when every STANDARD and DAYLIGHT of the VTIMEZONE has one
  TZOFFSETTO. Anything but a plain IANA name is reported
  (`datetime.zone_mapped`, with the mapping). Otherwise the value is
  `TimeUnknownZone` with its wall clock and TZID, and `datetime.unknown_tzid`:
  nothing is guessed. "Local" is refused, as it would be the host's zone.
  Zones need the host's zone database or `import _ "time/tzdata"`.
- Tolerated date-time forms (`datetime.loose`): ISO 8601 extended
  (`2026-11-20T10:00:00Z`), lower-case `t`/`z`, missing seconds, second 60.
  A DATE without `VALUE=DATE` is a date (`datetime.date_without_value`); a
  UTC value with a TZID is UTC (`datetime.tzid_with_utc`).

## Tolerance rules

| Input | Repair | Code |
| --- | --- | --- |
| UTF-8 BOM, at the start or on a line | dropped | `input.bom` |
| UTF-16 with BOM | converted to UTF-8 | `input.utf16` |
| bare LF or CR line endings | accepted | `line.bare_ending` |
| fold inside a UTF-8 sequence | rejoined byte-wise | `line.fold_splits_utf8` |
| empty lines, also inside a fold | skipped; the fold goes on after them | `line.empty` |
| first line folded | leading white space dropped | `line.leading_fold` |
| line without colon, or with a non-name before it, after a TEXT or X- property | joined to it as a new line of text | `line.unfolded_text` |
| `https://...`, `mailto:...` on a line of its own after such a property | the same | `line.unfolded_text` |
| other line without colon / with a bad name | dropped | `line.no_colon`, `line.bad_name` |
| lower- or mixed-case names | upper-cased | `line.name_case` |
| `SUMMARY :x` | name trimmed | `line.name_space` |
| unterminated quote in a parameter | read unquoted | `param.unterminated_quote` |
| text after a closing quote | kept in the value | `param.after_quote` |
| unquoted `SENT-BY=mailto:x` and other URI parameters | the colon kept in the value | `param.unquoted_uri` |
| content before the first BEGIN, after the last END | skipped, counted | `structure.before_begin`, `structure.after_end` |
| VEVENT etc. outside any VCALENDAR | a VCALENDAR supplied (`Synthesized`) | `structure.missing_vcalendar` |
| END of an outer component | the inner ones closed | `structure.missing_end` |
| BEGIN:VEVENT inside a VEVENT (or other calendar-level component) | the open one closed | `structure.missing_end` |
| BEGIN:VCALENDAR inside a calendar | everything closed; a second calendar | `structure.missing_end` |
| END of nothing open | ignored | `structure.stray_end` |
| missing ENDs at the end | closed | `structure.missing_end` |
| several VCALENDARs | events, to-dos, zones merged; other fields from the first | `structure.multiple_calendars` |
| repeated single property | the first used | `property.duplicate` |

Issues that can repeat on every line are recorded once and counted
(`Issue.Count`).

## Budgets

`Limits`, zero meaning the default; there is no unlimited setting. Hitting
one keeps what was read before it and records a `limit.*` issue. A cut input
does not also report the ENDs it lacks.

| Field | Default | On hitting it |
| --- | --- | --- |
| `MaxInputBytes` | 4 MiB | the rest is not read |
| `MaxLineBytes` | 1 MiB | the line is cut, `Property.Truncated`; an inline attachment on it is `Omitted` |
| `MaxLines` | 100 000 | reading stops |
| `MaxDepth` | 8 | the deeper component is skipped whole |
| `MaxComponents` | 5 000 | later components are skipped whole |
| `MaxProperties` | 1 000 per component | later properties dropped |
| `MaxParams` | 64 per property, and values per parameter | later ones dropped; also RRULE parts |
| `MaxAttendees` | 1 000 per event | later ones dropped; `AttendeeCount` has all |
| `MaxAttachmentBytes` | 1 MiB per calendar | the attachment is `Omitted`, `Size` estimated |
| `MaxIssues` | 256 | one `limit.issues`, then nothing |

Fixed: at most 64 distinct TZIDs and 64 `time.LoadLocation` calls per parse
(`limit.zones`), whatever earlier parses loaded, so results are deterministic.

## Scheduling

`Classify(cal, known)` gives the `Intent`: `REQUEST` is a request, or an
update when `known(uid)` returns an older revision; `CANCEL` (or a REQUEST or
PUBLISH with STATUS:CANCELLED) a cancel; `REPLY` a reply with the answering
attendee and their PARTSTAT; `COUNTER`, `DECLINECOUNTER`, `REFRESH`, `ADD`;
`PUBLISH` or no METHOD a publish. A REQUEST or CANCEL not newer than the
known copy is `Stale`. The event classified is the first without
RECURRENCE-ID.

`Compare(a, b)` orders two revisions by SEQUENCE, then DTSTAMP (RFC 5546,
2.1.5); `ok` is false for different UIDs or RECURRENCE-IDs.

`Options.ContentTypeMethod` takes the `method` parameter of the Content-Type:
it stands in for a missing METHOD (`calendar.method_from_content_type`), and a
METHOD that differs from it is reported (`calendar.method_mismatch`).

## From a message

`mail.ReadTree` keeps each leaf's decoded body in `Node.Data`; for a
`text/calendar` part that is UTF-8 when its charset was known (with the
`charset` plugin for non-UTF-8 ones). An `.ics` sent as
`application/octet-stream` is not charset-decoded; iCalendar is UTF-8 by
default and a UTF-16 BOM is handled here.

```go
_, root, err := message.Parse(r, nil)
if root == nil {
	return err
}
tree, err := mail.ReadTree(root, nil)
if tree.Root == nil {
	return err
}
tree.Root.Walk(func(n *mail.Node) bool {
	isICS := strings.HasSuffix(strings.ToLower(n.Filename), ".ics")
	if n.MediaType != "text/calendar" && n.MediaType != "application/ics" && !isICS {
		return true
	}
	cal := icalendar.ParseBytes(n.Data, &icalendar.Options{
		ContentTypeMethod: n.Params["method"],
	})
	if n.Truncated {
		// Data holds only TreeOptions.MaxBodyBytes of the part: the calendar
		// was cut and its last components may be missing.
	}
	c := icalendar.Classify(cal, lookupByUID)
	_ = c // c.Intent, c.UID, c.Sequence, c.PartStat, cal.Issues
	return true
})
```

`ReadTree` classifies a `text/calendar` alternative of the body as
`RoleBody`; one with a filename, or with an attachment disposition, as an
attachment. Both are calendars here.

## Not covered

- Recurrence expansion: RRULE is split, not expanded; RDATE, EXRULE and
  VALUE=PERIOD stay in the raw layer.
- VTIMEZONE rules are not evaluated, beyond the fixed-offset case.
- VFREEBUSY, VJOURNAL, VAVAILABILITY, RFC 7986 and RFC 9073 properties:
  raw layer only.
- Writing iCalendar.
- vCalendar 1.0 is read as if it were 2.0 (`calendar.version`); its quoted
  printable values are not decoded.
