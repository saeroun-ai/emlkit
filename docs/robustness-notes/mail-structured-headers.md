# Tolerant structured headers in `mail/` (ROB-040)

Implementation notes for ROB-040-001, -002, -004 … -009 (issues #27, #28, #47,
#48). ROB-040-003 was already done and is unchanged; its one-pass comment
scanner is reused.

## Shape of the API

Every strict accessor is unchanged, in signature and in behavior: `AddressList`,
`Date`, `MessageID`, `MsgIDList`, `ListCommandURLList`, `ParseAddress` and
`ParseAddressList` still return what they returned and refuse what they refused.
Each has a tolerant sibling that never returns an error. A sibling returns a
result struct holding

- `Raw`, the field value that was parsed;
- the typed values that could be understood, in order;
- for lists, `Entries`: every element including the unusable ones, each with
  the exact text it was read from (`Raw`), so nothing damaged is lost;
- `Diagnostics []message.Diagnostic`, with `Field` set by the `Header` methods
  and empty for the `Parse*` functions. `Location` and `Path` are not set: a
  standalone accessor knows neither the offset of the field nor its entity.

| Strict | Tolerant | Result |
|---|---|---|
| `Header.AddressList(key)`, `ParseAddressList(s)` | `Header.AddressListTolerant(key, *AddressOptions)`, `ParseAddressListTolerant(s, *AddressOptions)` | `AddressListResult` (`Addresses`, `Entries []AddressEntry`) |
| `Header.Date()` | `Header.DateTolerant()`, `ParseDateTolerant(s)` | `DateResult` (`Time`, `Valid`) |
| `Header.MessageID()` | `Header.MessageIDTolerant()` | `MessageIDResult` (`ID`) |
| `Header.MsgIDList(key)` | `Header.MsgIDListTolerant(key)`, `ParseMsgIDListTolerant(s)` | `MsgIDListResult` (`IDs`, `Entries []MsgIDEntry`) |
| — | `Header.Received()`, `ParseReceived(s)` | `[]Received` / `Received` (`Clauses []ReceivedClause`, `From`, `By`, `Via`, `With`, `ID`, `For`, `Date`, `DateRaw`, `HasDate`) |
| `Header.ListCommandURLList(key)` | `Header.ListURLs(key)`, `ParseListURLs(s)` | `ListURLsResult` (`URLs`, `Entries []ListURL`, `No`) |
| — | `Header.ListID()`, `ParseListID(s)` | `ListIDResult` (`ID`, `Description`) |

The `Header` methods return the zero result for a missing or empty field.

## Diagnostics

No code was added to the registry. Within the reserved structured-metadata
codes the severity carries the outcome:

| Severity | Meaning here |
|---|---|
| `recovered` | the syntax was wrong, one reading stood out, a typed value was kept; `Action` says what was done |
| `invalid_metadata` | no typed value; the raw text is kept (`Entries[i].Raw`, `Raw`, `DateRaw`) |
| `incomplete` | the value ends inside an identifier or URL (`<id@host` at the end of the field) |
| `info` | obsolete or extended syntax with a defined meaning: RFC 5322 obsolete zone names, raw UTF-8 (RFC 6532) |
| `unsupported` | 8-bit text in no known encoding, kept byte for byte (`address.raw_utf8`) |
| `limit_exceeded` | a structural bound was reached (below) |

`address.invalid`, `message_id.invalid`, `list_header.invalid`,
`received.invalid` and `date.invalid` are therefore used both for a repaired
value (`recovered`) and for one kept raw only (`invalid_metadata`); callers that
need the difference branch on code and severity together.
`address.partial_list` / `message_id.partial_list` are reported once per list
when at least one entry is unusable. One code from outside the group is used:
`rfc2047.unknown_charset` (`unsupported`) for an encoded-word in a display name
or List-Id description whose charset cannot be decoded; the word is kept as
written. `address.comment_depth` is not emitted:
comments have no depth limit.

## Bounds

The accessors belong to no `ParseSession` and take no `Limits`. They are bounded
structurally:

- every scanner is one pass over the value with counters, never recursion;
- a quoted string, comment or bracket that is never closed is searched for once
  and then read as literal text, so no search is repeated over the same bytes
  (address lists: at most three reads of the value; message identifiers and
  list URLs: the regions searched for a closing `>` never overlap);
- the unquoted-comma look-ahead is decided once per run of words;
- a date examines at most 12 words of the value, plus comments it skips;
- one call returns at most 16384 entries (addresses, identifiers, URLs,
  Received clauses; `maxTolerantEntries`) and 64 diagnostics
  (`maxTolerantDiagnostics`). The 64th is `limit.diagnostic_count`, as with
  `Limits.MaxDiagnosticCount`; text after the 16384th entry is not parsed and
  reported at `limit_exceeded` severity with the list's own code. Both are
  constants, not options.

Memory is linear in the value (an address list is tokenized at 12 bytes per
token). As for ROB-040-003, callers of the standalone functions bound the input
size themselves; `Parse` already bounds header fields.

## Per item

### ROB-040-001 — partial address lists: done

`ParseAddressListTolerant` / `Header.AddressListTolerant`. The list is split
into elements; an element `net/mail` accepts is parsed by `net/mail`, so a list
`ParseAddressList` accepts gives identical addresses and no diagnostics beyond
`address.raw_utf8` (checked over about 18,000 generated values). Any other element is repaired or kept raw
with `address.invalid` (`invalid_metadata`), and its neighbours are unaffected.
`AddressEntry` carries `Raw`, `Address` (nil when unusable), `Group` and the
diagnostics about that element.

### ROB-040-002 — unquoted commas and local-only addresses: done

Defaults follow MimeKit (`AllowUnquotedCommaInAddresses`,
`AllowAddressesWithoutDomain`); `AddressOptions{StrictCommas, RequireDomain}`
turn them off.

- `Doe, John <jd@example.org>` is one address named `Doe, John`
  (`address.unquoted_comma`). The words before a comma join the display name
  only when words and commas alone lead to an angle address with a word directly
  in front of it; `alice, bob, carol@example.org` stays three addresses and
  `postmaster, <a@example.org>` two.
- `postmaster`, `Root <root>` and `<admin>` are kept with the local part as
  `Address.Address` (`address.local_only`). A lone quoted string or a phrase of
  several words is not an address.

Limitation: with `StrictCommas`, MimeKit still joins a multi-word phrase across
a comma; here every unquoted comma then separates.

### ROB-040-003 — bounded comments and groups: unchanged (already done)

### ROB-040-004 — obsolete and broken address syntax: done

Repaired, each reported with `address.invalid` (`recovered`) unless noted:
obsolete routes (`<@a,@b:user@example.org>`); `mailto:` prefixes; a comment in
front of an address; missing `>`; repeated or unmatched angle brackets; an
address after its display name without angle brackets (`John Doe
john@example.org`); unquoted specials in a display name (`info@example.org
<info@example.org>`, `Name [ACME] <…>`); a missing comma between addresses; `;`
as a separator; a group never closed; nested groups, flattened into the
outermost (`address.group_depth`); an empty group inside angle brackets
(`<Undisclosed-Recipient:;>`); quotes, comments and brackets never closed;
dotted local parts and tagged IPv6 literals that `net/mail` refuses; an
encoded-word holding an unencoded comma. Empty list elements and empty groups
are valid (obsolete) syntax and are skipped silently.

Limitations: words after an angle address (`<a@example.org> Name`) are read as
a separate, unusable entry rather than as its name; after one comment that is
never closed, later parentheses in the same value are literal text.

### ROB-040-005 — internationalized addresses: done, without IDNA conversion

Raw UTF-8 in local parts, domains and display names is accepted (`net/mail`
already reads it) and reported with `address.raw_utf8` (`info`). Text that is
neither ASCII nor valid UTF-8 is kept byte for byte in `Name` / `Address` and
reported at `unsupported` severity; an encoded-word in an unknown charset is
kept as written and reported with `rfc2047.unknown_charset` (the strict
accessor fails the whole list on one). Limitation: an internationalized domain is returned as it was
written. A-labels (`xn--…`) are not converted to U-labels or back, and nothing
is normalized, because the module has no IDNA dependency.

### ROB-040-006 — obsolete and invalid dates: done

`ParseDateTolerant` / `Header.DateTolerant` use their own parser, not
`net/mail.ParseDate`, so an obsolete zone gets the offset RFC 5322 defines for
it. (`Header.Date` is unchanged and still lets the `time` package give an
unknown zone name a zero offset.)

- `date.obsolete_zone`: UT, GMT, EST/EDT, CST/CDT, MST/MDT, PST/PDT (`info`);
  UTC, Z, AST/ADT, WET/WEST, CET/CEST, EET/EEST, JST, KST, the military
  letters (read as -0000) and any unknown name (read as +0000) (`recovered`).
- `date.missing_zone`: read as +0000.
- `date.weekday_mismatch`: the weekday is ignored.
- `date.invalid` (`recovered`): 2- and 3-digit years per RFC 5322 §4.3; full
  weekday and month names; zones `+09:00`, `+9`, `GMT+0900`; a zone or AM/PM
  marker written against the time; a missing time (00:00:00); fractional
  seconds; `.` as time separator; month-first (asctime) dates; ISO 8601 and
  `dd-Mon-yy` dates; text after the date; a comment never closed.
- `date.invalid` (`invalid_metadata`): `Valid` is false and `Raw` keeps the
  value — out-of-range fields, a day the month does not have, numeric or
  unknown months, 1- or 5-digit years.

Accepted silently because RFC 5322 allows them: missing seconds, comments, a
missing weekday, a second of 60. Also silent: a weekday without its comma.

### ROB-040-007 — partial Message-ID / In-Reply-To / References: done

`ParseMsgIDListTolerant`, `Header.MsgIDListTolerant`,
`Header.MessageIDTolerant`. A field `MsgIDList` accepts gives identical
identifiers and no diagnostics. Otherwise an identifier is what one pair of
angle brackets holds, kept as written (whitespace inside the brackets removed)
with `message_id.invalid` when it has several `@`, none, an empty side,
characters outside atext or invalid UTF-8. Also repaired: a `<` never closed
(`incomplete` at the end of the value), identifiers without angle brackets when
the field has none with them, commas, RFC 822 phrases (`Your message of "…"
<id@host>`), a comment never closed. `<>` and identifiers holding control
characters stay in `Entries` as unusable, and the list reports
`message_id.partial_list`. `MessageIDTolerant` returns the first identifier,
reports extra ones, and accepts a single bare word.

### ROB-040-008 — Received: done

`ParseReceived` / `Header.Received()` (all Received fields, in document order).
The date is the text after the last `;` outside comments and quoted strings,
read by the tolerant date parser. The rest is split into clauses at the words
`from`, `by`, `via`, `with`, `id`, `for` (any case). Each `ReceivedClause` has
`Name`, `Value` (its words, comments left out), `Comments` and `Raw`; text
before the first keyword is a clause with an empty name; repeated and empty
clauses are kept in order. `received.invalid` reports a missing `;` or date, no
keyword at all, leading text, and a comment never closed.

Limitations: clause values are not parsed further (`For` keeps its angle
brackets, no address or domain validation); a keyword-like word inside a clause
value starts a new clause; clause keywords beyond the six of RFC 5321/5322 are
not recognized as clause names.

### ROB-040-009 — List-* headers: done

`ParseListURLs` / `Header.ListURLs(key)` and `ParseListID` / `Header.ListID()`.
URLs are what angle brackets hold, with whitespace inside removed as RFC 2369
asks. `NO` (any case) sets `No` instead of adding an entry. Multiple URLs are
kept in order; a URL `net/url` cannot parse, and `<>`, stays in `Entries` with
its text and a nil `URL` (`list_header.invalid`, `invalid_metadata`, with the
`net/url` error as `Cause`) and the URLs around it are kept. Also repaired:
missing `>` (`incomplete` at the end of the value), URLs without angle
brackets, a URL without a scheme, words between URLs, a comment never closed.
`ListID` returns the content of the last angle brackets and the decoded phrase
in front of it; a single bare word is taken as the identifier; a value with no
identifier is `invalid_metadata` with its text in `Description`.

Limitations: no per-header semantics (for example `List-Unsubscribe-Post` /
RFC 8058 one-click is not interpreted); bare URLs joined by a comma without a
space are read as one URL.

## Tests

`mail/address_tolerant_test.go`, `date_tolerant_test.go`,
`msgid_tolerant_test.go`, `received_test.go`, `list_test.go`: table tests per
feature, strict/tolerant differentials on valid input, the two caps, and
adversarial depth and length cases (up to 400 KiB). `tolerant_hostile_test.go`
runs every accessor over a fixed set of hostile values, every string of up to
three bytes from the characters the scanners branch on, and 4,000 fixed
pseudo-random mixtures, checking the invariants (raw text preserved, entries
are parts of the value, bounds, strict equivalence). No fuzz target was added.

## Observations

ROB-130-003. `mail.ParseAuthenticationResults` reports an authserv-id written
as an empty quoted string (`""`) as `authentication_results.invalid`
(InvalidMetadata) where it reported nothing; the method results are still
read, and `AuthenticationResults` never trusts the field, as before. Test:
`TestParseAuthenticationResults_emptyID` in `mail/ob_tree_test.go`.
