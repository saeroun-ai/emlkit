# TNEF (winmail.dat) extraction (ROB-140-001)

Code: `tnef/` (`tnef.go` stream and attributes, `mapi.go` property lists,
`codepage.go` text, `rtf.go` compressed RTF and what RTF encapsulates).
Tests: `tnef/tnef_test.go`, `tnef/fuzz_test.go` (`FuzzDecode`).

Outlook and Exchange can send a message's formatted body and its attachments
as one `application/ms-tnef` part, usually named `winmail.dat`. To a parser
that stops at MIME the message then has a short plain body and one opaque
attachment; the real attachments, the HTML body and any forwarded message are
inside. Package `tnef` opens that part.

## Optional, and apart from the core

Nothing in the module imports `tnef`. It imports the standard library and
`golang.org/x/text` (code pages), and no other package of the module.

It reports what it repaired as its own `tnef.Issue` values, not as
`message.Diagnostic`. Importing the root package would have been allowed (the
acceptance is that the core does not depend on the module, not the reverse),
but diagnostic codes are a closed registry in `diagnostic_code.go` with their
own severity and class rules, a TNEF stream has no MIME path or header
position to put in a `Diagnostic`, and a consumer that only wants to open a
`winmail.dat` should not have to link the MIME parser. A caller that wants one
report maps `Issue.Code` to its own scheme.

## API

- `Decode(b []byte, lim *Limits) (*Message, error)` and
  `DecodeReader(r io.Reader, lim *Limits)`. The only error of `Decode` is
  `ErrNotTNEF` (no signature). `DecodeReader` also returns the reader's error,
  together with the message decoded from what was read. `IsTNEF(b)` tests the
  signature.
- `Message`: `Subject`, `MessageClass`, `MessageID`, `InternetMessageID`,
  `From`, `DateSent`, `DateReceived`, `Body`, `BodyHTML`, `RTF`
  (`RTFTruncated`, `HTMLFromRTF`, `TextFromRTF`), `Codepage`,
  `InternetCodepage`, `Attachments`, `Recipients`, `Properties`, `Attributes`,
  `UnknownAttributes`, `Incomplete`, `Issues`, `IssuesDropped`.
- `Attachment`: `Name`, `LongName`, `DisplayName`, `Extension`, `MIMEType`,
  `ContentID`, `ContentLocation`, `Method`, `Flags`, `Hidden`, `Inline`,
  `Data`, `Size`, `Truncated`, `MetaFile`, `Created`, `Modified`, `Embedded`,
  `Properties`, `Attributes`; `Filename()` picks long, short, display name.
- Raw access: `Attribute{Level, ID, Data, Offset, Truncated, BadChecksum}` and
  `Property{ID, Type, Name, Value, Values, Offset, Truncated}` with `Tag`,
  `Int`, `Bool`, `Time`, `Text(codepage)`; `Properties.Find(id)`.
- Standalone pieces: `DecodeText(b, codepage)`, `CodepageSupported`,
  `DecompressRTF(src, maxOutput)`, `ExtractRTF(rtf, codepage, maxOutput)`.

Byte slices in the result alias the input (three-index slices, so appending to
one does not write into the input). Only decompressed RTF and decoded text are
new memory. Names are what the stream says, not safe paths: pass them through
`message.SafeFilename` before using one on disk.

## What is read

Stream (MS-OXTNEF): signature, key, then attributes — level, identifier,
length, data, 16-bit checksum.

| Attribute | Use |
| --- | --- |
| `attTnefVersion`, `attOemCodepage` | `Version`, `Codepage` |
| `attSubject`, `attBody`, `attMessageClass`, `attMessageID` | text fields, when the property form is absent |
| `attFrom` | `From` (TRP structure, `TYPE:address`) |
| `attDateSent`, `attDateRecd` | dates (DTR; no zone, given as UTC) |
| `attMAPIProps` | `Properties`, and the fields below |
| `attRecipTable` | `Recipients` |
| `attAttachRendData` | begins an attachment |
| `attAttachTitle`, `attAttachTransportFilename` | `Name` |
| `attAttachData`, `attAttachMetaFile` | `Data`, `MetaFile` |
| `attAttachCreateDate`, `attAttachModifyDate` | `Created`, `Modified` |
| `attAttachment` | attachment `Properties` |

The other attributes MS-OXTNEF defines (`attOwner`, `attPriority`, ...) are
kept raw in `Attributes`. Identifiers it does not define are skipped and
counted in `UnknownAttributes`.

Property lists: count, then tag (type, identifier), a name for identifiers of
0x8000 and above (GUID with a number or a UTF-16 string), then the value.
Fixed-size types (I2, LONG, R4, DOUBLE, CURRENCY, APPTIME, ERROR, BOOLEAN, I8,
SYSTIME, CLSID), the counted ones (STRING8, UNICODE, BINARY, OBJECT) with
their padding to four bytes, and the multi-valued form of each.

Properties used: `PR_SUBJECT`, `PR_MESSAGE_CLASS`, `PR_BODY`, `PR_BODY_HTML`
(binary or string), `PR_RTF_COMPRESSED`, `PR_INTERNET_CPID`,
`PR_MESSAGE_CODEPAGE`, `PR_INTERNET_MESSAGE_ID`, `PR_CLIENT_SUBMIT_TIME`,
`PR_MESSAGE_DELIVERY_TIME`, the sender name, type and address; per attachment
`PR_ATTACH_LONG_FILENAME`, `PR_ATTACH_FILENAME`, `PR_DISPLAY_NAME`,
`PR_ATTACH_EXTENSION`, `PR_ATTACH_MIME_TAG`, `PR_ATTACH_CONTENT_ID`,
`PR_ATTACH_CONTENT_LOCATION`, `PR_ATTACH_DATA_BIN` / `PR_ATTACH_DATA_OBJ`,
`PR_ATTACH_METHOD`, `PR_ATTACH_FLAGS`, `PR_ATTACHMENT_HIDDEN`,
`PR_CREATION_TIME`, `PR_LAST_MODIFICATION_TIME`; per recipient the display
name, address type, address and `PR_RECIPIENT_TYPE`.

A string property present in both forms is read from its Unicode one. A
property is preferred to the attribute that says the same thing.

### Text

`PT_UNICODE` is UTF-16LE. `PT_STRING8` and the attribute strings are in the
stream's code page: `attOemCodepage`, else `PR_INTERNET_CPID`, else
`PR_MESSAGE_CODEPAGE`, else for an embedded message the code page of the
message around it. A binary `PR_BODY_HTML` is in `PR_INTERNET_CPID` first.

Decoded through `golang.org/x/text`: 437, 850, 852, 855, 858, 860, 862, 863,
865, 866, 874, 932, 936, 949, 950, 1250–1258, 10000, 10007, 20866, 21866,
20932/51932, 51949, 28591–28599, 28603, 28605, 38598, 50220–50222, 52936,
54936; and directly 1200, 20127, 65001.

Non-ASCII text in a code page outside that list, or with no code page
declared, is not guessed silently: it is read as UTF-8 when it is valid UTF-8
and as Windows-1252 otherwise, and `codepage.unknown` or
`codepage.undeclared` is reported once per message. The bytes stay available
in `Attributes` and `Properties`.

### Compressed RTF

`PR_RTF_COMPRESSED` (MS-OXRTFCP): the LZFu form with its CRC, and the
uncompressed "MELA" form. The two examples of the specification are test
vectors. Output is bounded by `MaxRTFBytes` and by the size the header
declares; the declared size alone sizes no allocation (LZFu cannot expand past
about nine times its input).

When the stream has no HTML body, or no text body, of its own, the RTF is
read for one (MS-OXRTFEX): `\fromhtml` gives `BodyHTML` (`HTMLFromRTF`),
`\fromtext` gives `Body` (`TextFromRTF`). Ordinary RTF gives `Body` too, as a
rough rendering: text outside font, colour, style, picture, object and other
non-content destinations, with paragraph ends as newlines.

### Embedded messages

An attachment whose `PR_ATTACH_DATA_OBJ` (or whose data, with
`PR_ATTACH_METHOD` 5) is a TNEF stream, after the 16-byte interface
identifier, is read into `Attachment.Embedded`, to `MaxDepth`. The carrier's
`Data` is kept as well. All issues, of every depth, are in the outermost
`Message.Issues`, each with its `Depth`.

## Budgets

`Limits`; a zero field is the default, a negative one allows nothing, byte
limits are capped at 2 GiB.

| Field | Default | Bounds | On reaching it |
| --- | --- | --- | --- |
| `MaxInputBytes` | 64 MiB | input read | `limit.input`, rest ignored |
| `MaxAttributes` | 16384 | attributes, all depths | `limit.attributes`, stream ends |
| `MaxAttachments` | 1024 | attachments, all depths | `limit.attachments`, later ones dropped |
| `MaxProperties` | 4096 | properties of one list | `limit.properties`, rest of the list |
| `MaxTotalProperties` | 65536 | properties, multi-values, recipient rows | `limit.properties` |
| `MaxAttachmentBytes` | 32 MiB | `Data` of one attachment | `limit.attachment_bytes`, `Truncated` |
| `MaxDecodedBytes` | 128 MiB | all content handed out | `limit.decoded_bytes`, later content cut |
| `MaxRTFBytes` | 16 MiB | one decompressed RTF | `limit.rtf_bytes`, `RTFTruncated` |
| `MaxBodyBytes` | 16 MiB | one text or HTML body, as stored | `limit.body_bytes` |
| `MaxStringBytes` | 64 KiB | one short string, as stored | `limit.string_bytes` |
| `MaxDepth` | 4 (max 32) | embedded messages | `limit.depth`, not descended |
| `MaxIssues` | 256 | `Issues` | counted in `IssuesDropped` |

Every length and count the input declares is compared with the input that is
left, as an unsigned 64-bit value, before it is converted or used; none of
them sizes an allocation. Resynchronisation after damage spends a checksum
budget of four times the input. RTF group nesting is an explicit stack capped
at 1024.

## Tolerance

| Damage | Issue | Result |
| --- | --- | --- |
| stream ends inside an attribute | `stream.truncated` | the partial data is used: a cut attachment keeps its beginning (`Truncated`, `Size` as declared), a cut property list keeps its whole properties and the beginning of a cut value |
| checksum mismatch | `attribute.checksum` | data used, `Attribute.BadChecksum` |
| level byte wrong, attribute otherwise verifies | `attribute.level` | read by its identifier |
| bytes that are no attribute | `stream.resync` | skipped to the next position with a level, a known identifier, a fitting length and a matching checksum |
| the same, nothing verifies after | `stream.damaged` | stream ends, `Incomplete` |
| attachment attributes with no `attAttachRendData`, or a repeated one | `attachment.no_renddata` | an attachment is begun |
| property list ends early | `property.truncated` | properties before it kept |
| property type or name kind that cannot be sized | `property.type` | rest of that list not read |
| compressed RTF cut, bad CRC, unknown type | `rtf.damaged`, `rtf.crc`, `rtf.compression` | what decompresses is kept; unknown type gives none |
| RTF content only partly extracted | `rtf.extract` | the part is kept |
| method 5 and no TNEF stream | `embedded.not_tnef` | `Data` kept, no `Embedded` |

After `stream.resync` the attributes that follow are attributed by best
guess: if the damage hid the header of an attachment that itself carries a
TNEF stream, the inner attributes are read as outer ones. A consumer that
must not be confused by that treats a message with `stream.resync` as
structurally uncertain.

## Wiring it to a MIME part

The core does not detect or open TNEF. A consumer does, on the decoded body of
the part, for instance from `mail.ReadTree`:

```go
// tree, _ := mail.ReadTree(entity, nil)
// for each leaf n of the tree:
//
//	if n.MediaType == "application/ms-tnef" ||
//		n.MediaType == "application/vnd.ms-tnef" ||
//		tnef.IsTNEF(n.Data) { // winmail.dat sent as application/octet-stream
//		tm, err := tnef.Decode(n.Data, &tnef.Limits{MaxDepth: 2})
//		if err != nil {
//			// tnef.ErrNotTNEF: keep n as the ordinary attachment it is.
//			continue
//		}
//		// n.Truncated (TreeOptions.MaxBodyBytes) shows up here as
//		// tm.Incomplete and a stream.truncated issue: what was kept
//		// is still read.
//		for i := range tm.Attachments {
//			a := &tm.Attachments[i]
//			name := message.SafeFilename(a.Filename()) // not a.Filename() itself
//			// classify a.Data as any other attachment; a.Embedded
//			// is a forwarded message with attachments of its own.
//		}
//		// tm.BodyHTML / tm.Body are the body Outlook meant: prefer
//		// them to the placeholder text part beside winmail.dat.
//	}
```

`Node.Data` holds at most `TreeOptions.MaxBodyBytes` (1 MiB by default), so a
consumer that wants whole attachments out of a large `winmail.dat` raises it
or streams the part through `TreeOptions.Body` into `tnef.DecodeReader`.

## Not covered

- **No real-world fixtures.** Every test stream is built by the in-test
  writer from the specifications; only the two MS-OXRTFCP examples come from
  outside. Samples written by Outlook and Exchange (several versions, Korean
  and Japanese locales, meeting requests, embedded messages, OLE attachments)
  are still needed before this is called done, and are the likeliest source
  of surprises in attribute order and property typing.
- A corrupt length on a known attribute that still fits the stream swallows
  the attributes after it; only the checksum mismatch shows it.
- RTF: characters are read in the document's code page (`\ansicpg`), not per
  font (`\fcharset`); fields lose their instruction (a hyperlink keeps its
  text and loses its URL); no conversion of ordinary RTF to HTML.
- OLE attachments (`PR_ATTACH_METHOD` 6) are returned as their storage bytes;
  the compound file is not opened. `attAttachMetaFile` is returned as it is.
- Meeting and task content (`attDateStart`, `attOwner`, the named
  appointment properties) is available raw only; nothing builds an iCalendar
  object from it (ROB-140-003).
- Code pages outside the list above, among them 1201 (UTF-16BE), 1361
  (Johab), 65000 (UTF-7) and the EBCDIC ones.
- Writing TNEF.
