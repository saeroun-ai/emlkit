# Reports (ROB-090-005)

Code: `mail/report.go` (model, three parsers), `mail/report_node.go`
(`Node.Report`), `mail/report_scan.go` (field scanner, value readers),
`mail/report_code.go` (codes). Tests: `mail/report_test.go`.

The shape of a `multipart/report` is checked by `ReadTree`
(`report.invalid`, see structure-view.md). This note is about reading what
the report says.

## API

- `ParseDeliveryStatus(data)` — RFC 3464 / RFC 6533 `DeliveryStatus`:
  per-message `ReportingMTA`, `OriginalEnvelopeID`, `DSNGateway`,
  `ReceivedFromMTA`, `ArrivalDate`, then `Recipients` (`RecipientStatus`):
  `OriginalRecipient`, `FinalRecipient`, `Action` (normalised, with
  `ActionRaw`), `Status` (`EnhancedStatus`: class, subject, detail, `Raw`,
  `Valid`, `Comment`), `RemoteMTA`, `DiagnosticCode`, `LastAttemptDate`,
  `WillRetryUntil`, `FinalLogID`, and the guesses `StatusGuess` and
  `ActionGuess` (see Declared values and guesses). `Address()` gives the
  Final-Recipient, else the Original-Recipient.
- `ParseDispositionNotification(data)` — RFC 8098 `DispositionNotification`:
  `ReportingUA` (`UAName`, `UAProduct`), `MDNGateway`, `OriginalRecipient`,
  `FinalRecipient`, `OriginalMessageID`, `Disposition` (`MDNDisposition`:
  action mode, sending mode, type, modifiers), `Errors`, and RFC 3798's
  `Failures` and `Warnings`.
- `ParseFeedbackReport(data)` — RFC 5965 `FeedbackReport`: `FeedbackType`,
  `UserAgent`, `Version`, `OriginalEnvelopeID`, `OriginalMailFrom`,
  `OriginalRcptTo`, `ArrivalDate`, `ReportingMTA`, `SourceIP`/`SourceAddr`,
  `Incidents`, `ReportedDomains`, `ReportedURIs`, `AuthenticationResults`
  (raw; `AuthResults()` parses them, none trusted).
- `Node.Report()` — on a `multipart/report`, or on a report part, returns a
  `Report`: `Kind`, the typed model, `ReportType`, `Container`,
  `Description` (human-readable part), `Part`, `Original` (the returned
  `message/rfc822`, `message/global`, `text/rfc822-headers`,
  `message/global-headers`), `OriginalHeader` and `Diagnostics`. A report part
  outside a `multipart/report` is read alone. Any other node gives nil.

Every model keeps `Fields` (each field as read: name as written, value
unfolded, bytes untouched), `Unknown` (fields the model does not know, such
as `X-Postfix-Sender`, `Removal-Recipient`), `Unparsed` (skipped lines) and
`Diagnostics`. Typed values of the form `type; value` are `ReportValue`:
`Raw`, lower-cased `Type`, `TypeGuess`, `Value`, `Comment`. Dates are
`DateResult` from `ParseDateTolerant`.

### Declared values and guesses (#66)

A typed field holds only what the report gives. When the report leaves a
value out, the field stays empty and a guess, if one can be made, goes to a
field named `...Guess`. The diagnostic reports the missing or invalid field
at `invalid_metadata` and names the guess in its Action; it never reports a
recovery of a value the report does not give.

| Report gives | Declared field | Guess field |
| --- | --- | --- |
| no `Status` | `Status` not `Valid`, `Raw` empty | `StatusGuess`: an RFC 3463 code found in `Diagnostic-Code`, else the class of its reply code (`550 ...` gives 5.0.0) |
| `Status: 550` | `Status` not `Valid`, `Raw` `550` | `StatusGuess` 5.0.0 |
| `Status: 4.4.1 5.1.1` | `Status` not `Valid`, `Raw` kept | none: two readings |
| no `Action` | `Action` and `ActionRaw` empty | `ActionGuess` from the class of `Status`, else of `StatusGuess` |
| `type; value` with no type | `Type` empty | `TypeGuess`: `rfc822` for an address with `@`, `dns` for a host name, `smtp` for text starting with a reply code |

`StatusGuess`, when set, is `Valid` and `Inferred` with an empty `Raw`;
`Status` is never `Inferred`. A consumer that wants a best effort says so
(compiled as `ExampleParseDeliveryStatus`, `mail/report_test.go`):

```go
for _, r := range ds.Recipients {
	status, guessed := r.Status, false
	if !status.Valid && r.StatusGuess.Valid {
		status, guessed = r.StatusGuess, true
	}
	action := r.Action
	if action == "" && r.ActionRaw == "" {
		action = r.ActionGuess // "" when nothing suggests one
	}
	_ = guessed // a guess is not what the report says; weigh it as such
}
```

What stays a reading of the declared bytes, not a guess: a code inside a
`Status` with other text around it (`550 5.1.1 user unknown`, one code),
an Action variant mapped to its RFC 3464 word (`bounced`), a known type
before a colon (`rfc822: a@example.com`), brackets removed from an address,
a Disposition split by what it looks like, and the draft field name
`Received-Date` read as `Arrival-Date`. `Report.Kind` is what the report part
was read as, not a declared value; the declared ones are `ReportType` and the
part's media type, and a kind taken from the fields is reported
(`report.type_mismatch`).

## Tolerance

| Damage | Reading | Code |
| --- | --- | --- |
| LF, lone CR, mixed line ends | each ends a line | none |
| folded field | unfolded | none |
| unindented line of free text after Diagnostic-Code, Error, Authentication-Results or an unknown field | continuation | `report.unindented_continuation` |
| any other line that is no field (`name:` with an ftext name; a `name://` is a URL) | skipped, kept in `Unparsed` | `report.garbage_line` |
| `Name : value` | name without the space | `report.invalid_field` |
| no blank line between per-message and recipient fields | recipient group begins at the first recipient field | `report.missing_group_separator` |
| no blank line between recipients | new group where Final-Recipient or Original-Recipient repeats (Postfix writes Final before Original, so the order alone is no boundary) | `report.missing_group_separator` |
| per-message field among a recipient's | moved to the per-message group; a repeat stays in the recipient's `Unknown` | `report.misplaced_field` |
| report fields in the report part's MIME header (no blank line after Content-Type) | read as report fields | `report.misplaced_field` |
| blank line inside an MDN or ARF | one group | `report.unexpected_blank_line` |
| field repeated where it may appear once | first used | `report.duplicate_field` |
| `Status: 5.1.1 (bad mailbox)` | comments allowed | none |
| `Status: 550 5.1.1 user unknown`, `5.1.1.` | the one code read out of the text | `report.invalid_status` recovered |
| `Status: 4.4.1 5.1.1` (two different codes) | not `Valid`, `Raw` kept, no guess | `report.invalid_status` invalid_metadata |
| `Status: 550` | not `Valid`, `Raw` kept; `StatusGuess` 5.0.0 | `report.invalid_status` invalid_metadata |
| `Status: 5.1`, class not 2/4/5 | not `Valid`, `Raw` kept | `report.invalid_status` invalid_metadata |
| no Status | not `Valid`; `StatusGuess` from an RFC 3463 code in Diagnostic-Code, else its reply code | `report.missing_field` invalid_metadata |
| `Action: FAILED (x)` | case and comments ignored | none |
| `Action: failure`, `bounced`, `deferred`, `success`... | mapped to the RFC 3464 action | `report.invalid_action` recovered |
| unknown action | empty, `ActionRaw` kept | `report.invalid_action` |
| no Action | empty, `ActionRaw` empty; `ActionGuess` from the class of `Status`, else `StatusGuess` | `report.missing_field` invalid_metadata |
| `type; value` with no type | `Type` empty; `TypeGuess` `rfc822` for an address with `@`, `dns` for a host name, `smtp` for text starting with a reply code | `report.invalid_field` invalid_metadata |
| `rfc822: addr` | colon read as the separator | `report.invalid_field` recovered |
| `rfc822; <addr>` | brackets removed | `report.invalid_field` info |
| Disposition without `;`, without a mode, unknown mode or type | split by what it looks like, lower-cased | `report.invalid_field` |
| unknown Feedback-Type, Version not 1, `Received-Date` | kept / read as Arrival-Date | `report.invalid_field` info |
| `Source-IP: [192.0.2.1]`, `IPv6:` prefix | address inside | `report.invalid_field` recovered |
| unreadable Source-IP or Incidents | text kept, zero value | `report.invalid_field` |
| mandatory field missing | left empty | `report.missing_field` |
| UTF-8 in a report that is ASCII-only (not `message/global-*`) | read as UTF-8 | `report.non_ascii` info |
| bytes that are not UTF-8 | U+FFFD in the typed value, bytes kept in `Fields` | `report.non_ascii` recovered |
| report-type missing or of another kind than the report part | part read by its media type | `report.type_mismatch` info |
| no part labelled as a report | second part read by the report-type, else by its fields | `report.type_mismatch` recovered |
| no report part / no human-readable part | | `report.missing_part` |
| report part cut at `MaxBodyBytes` | whole lines kept are read | `report.truncated` |

Diagnostics of the parse functions carry no path; `Node.Report` sets the
MIME path of the part (structure diagnostics carry the `multipart/report`'s).

## Bounds

256 KiB of a report body are read (cut at a line end), 4096 fields, 1024
recipients, 32 unparsed lines of up to 998 bytes, 128 diagnostics (the last
is `limit.diagnostic_count`). Beyond a bound: `report.limit_exceeded`. Every
scan is one pass; comments are counted, not recursed into.

## Limitations

- xtext and the RFC 6533 `utf-8-addr-xtext` / `unitext` encodings of
  addresses are not decoded; the value is as written.
- Bounces that are not `multipart/report` at all (plain-text bounces of old
  MTAs) are not recognised; that is heuristics, not this model.
- Extension fields of RFC 6591 (auth-failure) and others are in `Unknown`,
  not typed.
- The status triple found inside free text may be a version number that
  looks like one. Found in a `Diagnostic-Code` it is only ever a
  `StatusGuess`; found in a `Status` field it is read only when it is the one
  code there, and that reading is always reported.
- An Action variant such as `sent` is mapped by a fixed table (`delivered`);
  it is a reading of the declared word, reported, not a guess field.

## Independent #66 review (2026-11-14)

Codex reviewed the DSN/MDN/ARF typed-value paths, status ambiguity handling, raw-value preservation, example and fuzz invariants. Six public-API scenarios and focused report tests under the race detector passed independently. Claude's fresh full unit suite, vet and 30-second report fuzz run (512,544 executions) passed. Missing values and SMTP reply-code guesses now require consumers to read the explicit Guess fields; the documented normalization table remains unchanged.
