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

## Schema `emlkit-differential/2`

A driver prints one JSON object per input file, one per line, in the order
the files were given. Version 2 added `cc`, `reply_to` and `message_id`; a
record of another version is refused, so that a version-1 driver is not
compared as if it had found none of them.

| Field | Meaning |
| --- | --- |
| `schema` | `"emlkit-differential/2"` |
| `parser` | the parser's name, which exceptions are matched by: `python-email`, `python-email-compat32`, `emlkit` |
| `parser_version`, `policy`, `file` | informational |
| `fixture` | the file's base name without `.eml` |
| `error` | set when the parser produced no message; the rest may then be missing |
| `subject` | the decoded text of the first Subject field, or null without one |
| `from`, `to`, `cc`, `reply_to` | the first such field as a list of `{"name", "address"}`, groups flattened; null without the field, `[]` when no address was understood |
| `message_id` | the identifier of the first Message-ID field, without angle brackets; null without the field or an identifier |
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

Strings are UTF-8. A byte the parser left undecoded — a raw 8-bit header
value in no charset it could tell — is U+FFFD, one per byte: the emlkit dump
replaces each byte that is not part of a valid UTF-8 sequence, the Python
driver each surrogate escape left in a string. (Python's email package itself
mostly reads raw 8-bit header bytes as UTF-8 first, RFC 6532, with one U+FFFD
for each invalid sequence; the two counts differ only where such a sequence
is longer than one byte.) A name both parsers left undecoded therefore
compares as equal, and one only one of them decoded as different.

### How emlkit fills it

`message.Parse` with nil options (ProfileTolerant, no `FallbackCharset`),
then `mail.ReadTree` with `DescendMessages`, twice:

1. With `message.CharsetReader` nil. Every body is then the bytes after
   transfer decoding, whatever its charset: `body_length` and `body_sha256`
   are this parse's `Node.Size` and `Node.SHA256`.
2. With the charset plugin (`charset.Reader`), as a deployment that reads
   legacy-charset mail runs. Everything else is this parse's: the structure,
   media type, parameters, disposition, filename (`Node.Filename`, decoded
   with the plugin), Content-ID, the header fields, the diagnostics, and the
   text. `text_sha256` is the `Node.SHA256` of a `text/*` leaf, the body
   decoded to UTF-8 — or passed through as read, for UTF-8, US-ASCII and no
   charset — unless the part came with an unknown-charset warning
   (`Node.Warning`, or for the root the warning `Parse` returned), when the
   body is undecoded and emlkit has no text.

`dfDumpEmlkit` sets the hook for each parse and puts back the caller's value
before it returns, so the plugin is installed only while the harness parses;
the other tests of the package run with no `CharsetReader`
(`mail/soak_test.go`). Should the two parses frame a message differently, the
record's `error` says where, and the comparison reports it.

`transfer_encoding` is the raw header value. Subject is `Header.Subject`, the
addresses `Header.AddressListTolerant` (defaults), Message-ID
`Header.MessageIDTolerant().ID`, the date `Header.DateTolerant`, all read
while the plugin is installed. Each diagnostic of the session and of the tree
is listed under the part at its path, or under the nearest part above it with
its path appended.

### How the Python driver fills it

`email.message_from_bytes` with `email.policy.default` (`--compat32` for
`email.policy.compat32`, parser name `python-email-compat32`):
`get_content_type`, `get_content_charset`, `get_content_disposition`,
`get_filename`, the raw `Content-Transfer-Encoding`, `get_payload(decode=True)`
for the body, `get_content()` for the text (compat32: the bytes decoded with
the declared charset, `us-ascii` by default, errors replaced), the header
objects' `addresses` and `datetime` (compat32: `email.utils`). `message_id` is
what the first angle brackets of the field's text enclose, white space
removed, or the trimmed text without them (Python keeps a trailing comment in
the value). It uses the standard library only, reads only the files it is
given, writes only standard output, and bounds each input (32 MiB,
`--max-bytes`), the walk depth (64) and the parts recorded (10000).

## Comparison

Message fields first, then each part depth-first, children paired by index:
`error`, `subject`, `from`, `to`, `cc`, `reply_to`, `message_id`, `date`, then
per part `media_type`, `charset`, `disposition`, `filename`, `content_id`,
`transfer_encoding`, `children`, `body_length`, `body_sha256`, `text_sha256`.
Values are compared as their JSON. Two rules keep one cause from being
reported many times:

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
- An exception matches by place and field, not by value: when a change to
  emlkit alters a value that already differed, the entry still matches.
  Such a change re-reads the rationales of its fixtures.

## Results (2026-11-11, Python 3.14.10, policy default)

74 fixtures (65 of the corpus, 9 `dx-`), 44 differences on 29 of them, all
explained by 35 exceptions, 3 of them open.

emlkit heals where Python does not in most: inferred and mismatched
boundaries, an unindented boundary parameter, a colonless header line, a
truncated closing delimiter, a malformed or misspelled transfer-encoding
token (`"BASE64"; x=1`, `quoted_printable`, `base-64`), a base64
`message/rfc822`, the empty and delimiter-less multipart, a closing delimiter
an inner multipart shares with the outer one (`nested-reused-boundary-deep`:
emlkit reads on past it and keeps the outer attachment after it, which Python
drops as epilogue), and unquoted commas in a display name. In three cases
Python loses or corrupts data: data after Base64 padding becomes
`hi\x1a\x1a`, `binascii.a2b_qp` drops the text after a soft break ended by a
bare CR, and it keeps `=` followed by spaces at a line end as text where RFC
2045 makes the spaces transport padding and the `=` a soft break
(`bulk-sloppy-encodings`, "busin=   ess"). Python descends into
`message/delivery-status` and `message/disposition-notification`, which
emlkit keeps as report leaves (`Node.ReportFields`), and emlkit into
`text/rfc822-headers`, which Python keeps as text. The rest are decisions:
the final line ending of a multipart with no closing delimiter is kept, and a
body with no charset or an invalid UTF-8 one is delivered as read (reported,
re-decoded only with `FallbackCharset`) where Python substitutes U+FFFD.

Everything else agreed: RFC 2047 subjects and display names (UTF-8,
ISO-8859-1, EUC-KR, ks_c_5601-1987, ISO-2022-JP, Big5), RFC 2231 filenames
with continuations and charsets, RFC 2047 encoded filenames (also EUC-KR in a
quoted parameter), Cc and Reply-To with groups, a quoted comma and legacy
encoded words, Message-ID with a comment and without brackets, numeric,
obsolete-zone and `-0000` dates, nested and embedded messages,
`multipart/related` with Content-IDs, quoted-printable Latin-1 bytes, the
digest default, the parts read past a reused boundary, an inner unclosed
multipart, an illegal transfer encoding on a multipart, uuencode without
`end`, junk and missing padding in Base64, an unknown transfer encoding,
conflicting filenames, a duplicate Content-Type, bare LF line endings, a
missing header separator, a part without one; and the text of EUC-KR,
ks_c_5601-1987, ISO-2022-JP, Shift_JIS (with half-width katakana), Big5,
GB18030 and ISO-8859-1 bodies that keep to the strict charset.

## Charsets

With the plugin both sides decode every charset in the corpus, so the text
of each `text/*` part and every encoded word is compared. Where they still
differ, the codecs disagree on what a label means, and the plugin reads mail
as it is written:

| Label | Python | The plugin | Fixtures |
| --- | --- | --- | --- |
| `euc-kr`, `ks_c_5601-1987` | `euc_kr`: KS X 1001 only, 2,350 precomposed syllables | code page 949 (UHC), all 11,172 syllables; what Windows, Outlook and Korean webmail write under both labels | `ko-cp949-under-euc-kr`: 똠, 햏, 뷁 are U+FFFD in Python |
| `gb2312` | GB 2312-80 only | GBK (WHATWG, mail clients) | `zh-gbk-gb18030`: 喆 is U+FFFD, and Python's decoder loses its place and garbles the characters after it |

These are exceptions, not open differences: emlkit's text is the one the
sender's client displayed. The plugin does not report the widening
(charset-plugin.md, Limitations).

Expected differences of the same kind not yet in the corpus:

- `shift_jis`: Python's codec is JIS X 0208 with half-width katakana; the
  plugin's is code page 932, with the NEC and IBM extensions (①, Ⅱ, ㈱) that
  Windows mail uses. Python gives U+FFFD for those.
- `iso-2022-jp`: Python's `iso2022_jp` is RFC 1468; the plugin also reads the
  code page 932 table, JIS X 0212 and the katakana forms of CP50220-50222.
- `iso-8859-1`: the plugin decodes it as windows-1252, so bytes 0x80-0x9F are
  quotes, dashes and the euro sign; Python's `latin-1` gives C1 controls.

Raw 8-bit header text in no declared charset is read differently but
undecoded on both sides: emlkit keeps the bytes (U+FFFD in the dump), Python
reads them as UTF-8 and gets U+FFFD with accidental characters where a run of
EUC-KR bytes happens to be valid UTF-8. When the part's Content-Type
declares a charset, emlkit decodes a raw 8-bit filename with it
(`ko-raw-8bit-subject-filename` part [2]) and Python does not. The harness
runs without `FallbackCharset`, so an undeclared EUC-KR body or Subject is
decoded by neither (`ko-undeclared-euc-kr`; finding 9 of corpus.md, a raw
filename the fallback does not reach, is not exercised here).

## Open differences

1. *Resolved, no longer open (issue #63, 2026-11-13).* **Folding white space
   was normalized, not unfolded** (`header-whitespace-separator`,
   `dx-subject-folded`, field `subject`). `textproto` built a field's value by
   trimming the white space around each line break, joining the lines with
   one space and dropping empty continuation lines (`writeContinued`,
   inherited from go-message); RFC 5322 section 3.2.2, and Python, remove the
   CRLF only. Under `Parse` with every profile but `ProfileStrict` the value
   is now unfolded as the RFC says (`unfoldValue`; header-recovery.md,
   Unfolding), so `"that is\r\n\tfolded"` is `"that is\tfolded"` and the
   white-space-only continuation gives `"s \t"`. The run reported both
   exceptions stale and they are removed. `Read`, `ReadWithOptions` and
   `ProfileStrict` keep the collapsed form.
2. *Decided, no longer open (issue #65, 2026-11-13).* **UTF-8 under a legacy
   multi-byte label is decoded as the label** (`ko-utf8-labelled-euc-kr`,
   path `[]`, field `text_sha256`). The body is valid UTF-8 HTML whose own
   meta tag says `utf-8`, declared `charset=euc-kr`. The bytes read two ways,
   and by the policy of #62 the declaration is followed by default: the
   plugin decodes CP949 mojibake ("[愿묎퀬] …" for "[광고] …") and the parse
   reports `charset.declaration_suspect` at Info, naming UTF-8 as the other
   reading; Python's KS X 1001 codec gives other mojibake with 43 U+FFFD.
   `ParseOptions.RecoverCharsetDeclarations` reads the UTF-8 (Recovered);
   the harness runs without it. The exception stays, with its rationale
   rewritten as this decision and `open` removed. Finding 6 of corpus.md.

## Expected differences not yet in the corpus

Fixtures for these would need exceptions with this reasoning, besides the
charsets above:

- Python parses every `message/*` part other than `message/delivery-status`
  as an embedded message (`message/partial`, `message/external-body`,
  `message/feedback-report`, as it does `message/disposition-notification`
  in `report-mdn-receipt`). emlkit keeps them as leaves (`ReadTree` roles
  `report`, the shape diagnostics, `ReportFields`).

## Plugging in another parser

A driver is any program that takes `.eml` paths as arguments and prints the
schema above, one object per file in argument order, with its own `parser`
name; run it with `EMLKIT_DIFFERENTIAL_DRIVER` and add exceptions under that
name. It should not touch the network or write files, should bound its
input, and is held to the test's deadline. It must follow the conventions:
lower-cased media type, charset and disposition; Content-ID and Message-ID
without brackets; the raw transfer-encoding value; transfer-decoded bytes for
the body; `null` children for a leaf and a body of `null` for a container; a
carried message as the carrier's one child; U+FFFD for an undecoded byte.
None of these is installed on this host, and nothing may be downloaded here,
so they are slots:

- **Angus Mail** (Jakarta Mail): a Java program over
  `new MimeMessage(session, in)` with `mail.mime.decodefilename` and
  `mail.mime.decodeparameters` set; walk `getContent()` as `Multipart` or
  `MimeMessage`; `ContentType.getBaseType()`, `getParameter("charset")`,
  `getDisposition()`, `getFileName()`, `getContentID()`,
  `getHeader("Content-Transfer-Encoding", null)`; the body from
  `MimeBodyPart.getInputStream()` (decoded) and the text from `getContent()`
  as a String; addresses from `getFrom()`, `getRecipients(TO)` and `(CC)`,
  `getReplyTo()`, `getMessageID()`, the date from `getSentDate()`. Needs a
  JDK and the `jakarta.mail-api` and `angus-mail` jars, vendored or from a
  local repository.
- **MimeKit**: a .NET console program over `MimeMessage.Load(stream)` with
  default `ParserOptions`; walk `Multipart` children and `MessagePart.Message`;
  `ContentType.MimeType`, `ContentType.Charset`,
  `ContentDisposition.Disposition`, `MimePart.FileName`, `ContentId`, the raw
  `Content-Transfer-Encoding` header; the body from `Content.DecodeTo`, the
  text from `TextPart.Text`; `From`, `To`, `Cc` and `ReplyTo` `.Mailboxes`,
  `MessageId`, `Date` (null for `DateTimeOffset.MinValue`). Needs the .NET
  SDK and the MimeKit package from a local feed. emlkit's tolerant address
  parser follows MimeKit's defaults, so its address differences should be
  few.
- **Mime4j** (optional): a Java program over `DefaultMessageBuilder` with a
  lenient `MimeConfig` (and a strict run as a second parser name); walk
  `Multipart.getBodyParts()` and embedded `Message` bodies; `getMimeType()`,
  `getCharset()`, `getDispositionType()`, `getFilename()`, the raw
  `Content-ID` and `Content-Transfer-Encoding` fields; the body from
  `SingleBody.getInputStream()`, the text from `TextBody.getReader()`;
  `getFrom()`, `getTo()`, `getCc()`, `getReplyTo()` flattened,
  `getMessageId()`, `getDate()`. Needs a JDK and the `apache-mime4j-core` and
  `-dom` jars.

## Limitations

- Only the Python driver exists; ROB-130-004 asks for Angus and MimeKit too.
- Top-level Subject, From, To, Cc, Reply-To, Message-ID and Date only; an
  embedded message's header is compared through its parts' fields, not its
  own Subject.
- Children are paired by index: a part one parser misses shifts every later
  sibling, and the first difference there is the one to read.
- `defects` are not compared: the vocabularies do not map one to one.
- The emlkit side runs with nil `ParseOptions`: no `FallbackCharset`, the
  default profile. Undeclared legacy text is compared as undecoded on both
  sides.
- Bodies are taken from a second parse without the plugin. The two parses
  are checked to frame the message alike, but a difference between them in a
  body of the same shape (the plugin cannot touch a non-text body, and
  passes UTF-8, US-ASCII and undeclared text through) would not be seen.
