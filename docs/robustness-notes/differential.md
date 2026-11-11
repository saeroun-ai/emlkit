# Differential testing (ROB-130-004, ROB-130-005)

Code: `scripts/diff/py_email_dump.py` (the Python driver),
`mail/differential_test.go` (the emlkit driver, the comparison and
`TestDifferentialPython`), `mail/differential_harness_test.go` (a check of the
harness itself, run in every test run). Data: `testdata/differential/exceptions.json`
(the reviewed exceptions), `testdata/differential/fixtures/` (synthetic
fixtures for the header fields and parameters the regression corpus barely
touches; their names begin with `dx-`).

Another parser's view of a message is useful only where it differs from
emlkit's, and only if each difference is either a decision or a bug. The
harness compares the two field by field, in a schema neither parser owns, and
asks for a written reason for every difference it finds.

## Schema `emlkit-differential/1`

A driver prints one JSON object per input file, one per line, in the order
the files were given:

| Field | Meaning |
| --- | --- |
| `schema` | `"emlkit-differential/1"` |
| `parser` | the parser's name, which exceptions are matched by: `python-email`, `python-email-compat32`, `emlkit` |
| `parser_version`, `policy`, `file` | informational |
| `fixture` | the file's base name without `.eml` |
| `error` | set when the parser produced no message; the rest may then be missing |
| `subject` | the decoded text of the first Subject field, or null without one |
| `from`, `to` | the first such field as a list of `{"name", "address"}`, groups flattened; null without the field, `[]` when no address was understood |
| `date` | the Date field as a UTC instant, `YYYY-MM-DDTHH:MM:SSZ`; null when absent or unreadable |
| `root` | the root part |

A part:

| Field | Meaning |
| --- | --- |
| `path` | the MIME path as a list of indices, `[]` for the root, the convention of `Diagnostic.Path` |
| `media_type` | type/subtype, lower case, as the parser interpreted it (its default when absent or unreadable) |
| `charset` | the `charset` parameter, lower case, or null; no default is filled in |
| `disposition` | the disposition type, lower case, or null |
| `filename` | the parser's chosen filename, decoded, or null |
| `content_id` | Content-ID without white space and angle brackets, or null |
| `transfer_encoding` | the Content-Transfer-Encoding value as declared, trimmed and lower case, or null |
| `body_length`, `body_sha256` | length and SHA-256 (hex) of the body after transfer decoding and before any charset decoding; null for a container |
| `text_sha256` | SHA-256 of the part's text as UTF-8 bytes, for a `text/*` leaf whose text the driver could produce; else null |
| `children` | the parts of a container in order; null for a leaf, `[]` for a container with no part |
| `defects` | the parser's own words for what it found: Python defect class names, emlkit diagnostic codes. Informational, never compared |

A part that carries a message (`message/rfc822`) is a container whose one
child is that message's root, at the carrier's path followed by 0.

`body_sha256` is taken before charset decoding so that charset handling
cannot hide a framing or transfer-decoding difference; `text_sha256` then
isolates charset handling.

### How emlkit fills it

`message.Parse` with nil options (ProfileTolerant), then `mail.ReadTree` with
`DescendMessages`. Media type, parameters, disposition, filename and
Content-ID are the `Node`'s; `transfer_encoding` is the raw header value. The
body is `Node.Size` and `Node.SHA256`. The test sets `message.CharsetReader`
to nil for its run, so every body is the transfer-decoded bytes whatever the
charset; `text_sha256` is therefore given only for a `text/*` part whose
charset is absent, `utf-8` or `us-ascii`, where emlkit's text is those bytes.
Subject is `Header.Subject`, the addresses `Header.AddressListTolerant`
(defaults), the date `Header.DateTolerant`. Each diagnostic of the session and
of the tree is listed under the part at its path, or under the nearest part
above it with its path appended.

### How the Python driver fills it

`email.message_from_bytes` with `email.policy.default` (`--compat32` for
`email.policy.compat32`, parser name `python-email-compat32`):
`get_content_type`, `get_content_charset`, `get_content_disposition`,
`get_filename`, the raw `Content-Transfer-Encoding`, `get_payload(decode=True)`
for the body, `get_content()` for the text (compat32: the bytes decoded with
the declared charset, `us-ascii` by default, errors replaced), the header
objects' `addresses` and `datetime` (compat32: `email.utils`). It uses the
standard library only, reads only the files it is given, writes only standard
output, and bounds each input (32 MiB, `--max-bytes`), the walk depth (64) and
the parts recorded (10000).

## Comparison

Message fields first, then each part depth-first, children paired by index:
`error`, `subject`, `from`, `to`, `date`, then per part `media_type`,
`charset`, `disposition`, `filename`, `content_id`, `transfer_encoding`,
`children`, `body_length`, `body_sha256`, `text_sha256`. Values are compared
as their JSON. Two rules keep one cause from being reported many times:

- When one parser framed a part into children and the other kept it as a
  leaf, only `children` differs (count against null); the body fields follow
  from it and are not compared.
- `text_sha256` is compared only when both drivers gave one and the bodies
  are equal.

A difference is a record:

```json
{"parser": "python-email", "fixture": "qp-damaged", "path": "[]",
 "field": "body_length", "emlkit": 22, "other": 14,
 "exception": {"rationale": "...", "open": false}}
```

`path` is the part's path as Go prints it (`[]`, `[0 1]`), the form of the
corpus manifest.

## Running it

The test is skipped unless `EMLKIT_DIFFERENTIAL=1`, and skipped when
`python3` is not found. Under the bounded runner, environment variables go
through `env`:

```bash
flock -w 540 /tmp/emlkit-qa.lock rtk proxy python3 scripts/qa-bounded.py --seconds 240 -- \
  env EMLKIT_DIFFERENTIAL=1 EMLKIT_DIFFERENTIAL_OUT=/abs/path/report.json \
  go test -count=1 -timeout 180s -run 'TestDifferentialPython' -v ./mail/
```

| Variable | Effect |
| --- | --- |
| `EMLKIT_DIFFERENTIAL=1` | run the test |
| `EMLKIT_DIFFERENTIAL_OUT` | write the whole comparison — both dumps, every difference with its exception, the stale exceptions and a summary — as JSON (`emlkit-differential-report/1`); relative to `mail/`, so give an absolute path |
| `EMLKIT_DIFFERENTIAL_DRIVER` | the driver's command line, split at white space, the fixture paths appended; default `python3 -I ../scripts/diff/py_email_dump.py` |
| `EMLKIT_DIFFERENTIAL_DIR` | directories of `*.eml` to compare, separated as in `PATH`; default `testdata/corpus` and `testdata/differential/fixtures` |

The driver runs once over all fixtures as a child of the test, so inside the
runner's limits, with a 120 s deadline, 64 MiB of standard output kept and
64 KiB of standard error. A fixture name may occur once across the
directories.

The Python driver can also be run alone:
`timeout 60 python3 -I scripts/diff/py_email_dump.py testdata/corpus/*.eml`.

## Exceptions policy (ROB-130-005)

`testdata/differential/exceptions.json` (`emlkit-differential-exceptions/1`)
lists the differences that are understood. Each entry has `parser`,
`fixture`, `path` and `field` — an exact value, `*`, or a prefix followed by
`*` (`body_*` covers `body_length` and `body_sha256`) — a `rationale`
sentence, and `open: true` where emlkit may be the one that is wrong.
Unknown keys, a missing rationale or a field pattern that names no field fail
the load, in every test run (`TestDifferentialHarness`).

- A difference no exception matches fails the test, as a JSON record.
- An exception that matches nothing fails the test as stale: the difference
  it explained is gone and the entry must go too. An exception for another
  parser is not stale; nor, when `EMLKIT_DIFFERENTIAL_DIR` is set, one whose
  fixture was not compared.
- An open exception keeps the test usable and is logged on every run; it is
  listed below until it is resolved, either by changing emlkit or by
  rewriting the rationale as a decision.
- Entries are narrow: one fixture, one path, the fields that differ. A wide
  pattern would hide a regression on the same part. When emlkit heals and
  the other parser does not, the rationale names the diagnostic code that
  reports the repair.
- A new fixture (also one another change adds to `testdata/corpus`) comes
  with the review of its differences: run the test, read each record, and
  add an exception with its reason or report the bug.

## Results (2026-11-11, Python 3.14.10, policy default)

37 fixtures (30 of the corpus, 7 `dx-`), 25 differences on 18 of them, all
explained by 19 exceptions, 2 of them open. emlkit heals where Python does
not in most: inferred and mismatched boundaries, an unindented boundary
parameter, a colonless header line, a truncated closing delimiter, a
malformed transfer-encoding token, a base64 `message/rfc822`, the empty and
delimiter-less multipart, and unquoted commas in a display name. In two
cases Python loses or corrupts data: data after Base64 padding becomes
`hi\x1a\x1a`, and `binascii.a2b_qp` drops the text after a soft break ended
by a bare CR. The rest are decisions: the final line ending of a multipart
with no closing delimiter is kept, and a body with no charset or an invalid
UTF-8 one is delivered as read (reported, re-decoded only with
`FallbackCharset`) where Python substitutes U+FFFD.

Everything else agreed: RFC 2047 subjects and display names (UTF-8,
ISO-8859-1), RFC 2231 filenames with continuations and charsets, an RFC 2047
encoded filename, numeric, obsolete-zone and `-0000` dates, nested and
embedded messages, `multipart/related` with Content-IDs, quoted-printable
Latin-1 bytes, the digest default, the parts read past a reused boundary,
an inner unclosed multipart, an illegal transfer encoding on a multipart,
uuencode without `end`, junk and missing padding in Base64, an unknown
transfer encoding, conflicting filenames, a duplicate Content-Type, bare LF
line endings, a missing header separator, a part without one.

## Open differences

1. **Folding white space is normalized, not unfolded**
   (`header-whitespace-separator`, `dx-subject-folded`, field `subject`).
   `textproto` builds a field's value by trimming the white space around each
   line break, joining the lines with one space and dropping empty
   continuation lines (`writeContinued`, inherited from go-message). RFC 5322
   section 3.2.2 unfolds by removing the CRLF only, so `"that is\r\n\tfolded"`
   is `"that is\tfolded"` and a white-space-only continuation stays part of
   the value; Python follows the RFC. The raw field is kept, so round-trips
   are unaffected, but every decoded header value — Subject and the
   structured fields read from `Header.Get` — differs from the RFC unfolding
   in white space. For classification this is mostly harmless; it does
   matter where white space is the signal (tab runs, padding used to push
   text out of view). Changing it touches `textproto/header.go` and every
   test that expects the collapsed form, so it is left to a decision.

## Expected differences not yet in the corpus

Fixtures for these would need exceptions with this reasoning:

- Python parses every `message/*` part other than `message/delivery-status`
  as an embedded message (`message/partial`, `message/external-body`,
  `message/disposition-notification`, `message/feedback-report`), and a
  `message/delivery-status` as one child per field group. emlkit keeps them as
  leaves (`ReadTree` roles `report`, the shape diagnostics, `ReportFields`).
- An encoded word in a charset other than UTF-8, US-ASCII and ISO-8859-1 is
  left as written by the emlkit driver, which runs without the charset plugin;
  Python decodes it. That is a limit of the harness, not of emlkit.

## Plugging in another parser

A driver is any program that takes `.eml` paths as arguments and prints the
schema above, one object per file in argument order, with its own `parser`
name; run it with `EMLKIT_DIFFERENTIAL_DRIVER` and add exceptions under that
name. It should not touch the network or write files, should bound its
input, and is held to the test's deadline. It must follow the conventions:
lower-cased media type, charset and disposition; Content-ID without brackets;
the raw transfer-encoding value; transfer-decoded bytes for the body; `null`
children for a leaf and a body of `null` for a container; a carried message
as the carrier's one child. None of these is installed on this host, and
nothing may be downloaded here, so they are slots:

- **Angus Mail** (Jakarta Mail): a Java program over
  `new MimeMessage(session, in)` with `mail.mime.decodefilename` and
  `mail.mime.decodeparameters` set; walk `getContent()` as `Multipart` or
  `MimeMessage`; `ContentType.getBaseType()`, `getParameter("charset")`,
  `getDisposition()`, `getFileName()`, `getContentID()`,
  `getHeader("Content-Transfer-Encoding", null)`; the body from
  `MimeBodyPart.getInputStream()` (decoded) and the text from `getContent()`
  as a String; addresses from `getFrom()`/`getRecipients(TO)`, the date from
  `getSentDate()`. Needs a JDK and the `jakarta.mail-api` and `angus-mail`
  jars, vendored or from a local repository.
- **MimeKit**: a .NET console program over `MimeMessage.Load(stream)` with
  default `ParserOptions`; walk `Multipart` children and `MessagePart.Message`;
  `ContentType.MimeType`, `ContentType.Charset`,
  `ContentDisposition.Disposition`, `MimePart.FileName`, `ContentId`, the raw
  `Content-Transfer-Encoding` header; the body from `Content.DecodeTo`, the
  text from `TextPart.Text`; `From.Mailboxes`, `To.Mailboxes`, `Date` (null
  for `DateTimeOffset.MinValue`). Needs the .NET SDK and the MimeKit package
  from a local feed. emlkit's tolerant address parser follows MimeKit's
  defaults, so its address differences should be few.
- **Mime4j** (optional): a Java program over `DefaultMessageBuilder` with a
  lenient `MimeConfig` (and a strict run as a second parser name); walk
  `Multipart.getBodyParts()` and embedded `Message` bodies; `getMimeType()`,
  `getCharset()`, `getDispositionType()`, `getFilename()`, the raw
  `Content-ID` and `Content-Transfer-Encoding` fields; the body from
  `SingleBody.getInputStream()`, the text from `TextBody.getReader()`;
  `getFrom()`, `getTo()` flattened, `getDate()`. Needs a JDK and the
  `apache-mime4j-core` and `-dom` jars.

## Limitations

- Only the Python driver exists; ROB-130-004 asks for Angus and MimeKit too.
- Top-level Subject, From, To and Date only; an embedded message's header is
  compared through its parts' fields, not its own Subject.
- Children are paired by index: a part one parser misses shifts every later
  sibling, and the first difference there is the one to read.
- `defects` are not compared: the vocabularies do not map one to one.
- The emlkit side runs without a CharsetReader (see above), so text is
  compared only for UTF-8, US-ASCII and undeclared text.
