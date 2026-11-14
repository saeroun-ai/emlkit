# Contract for a spam-analysis adapter

Issues #44 and #45. This page is for the integrator who feeds a classifier
from emlkit. It says how to parse, how to get the whole verdict, what each
result may be relied on for, and what the adapter must do itself. Every API
named here exists in the module; the notes in `robustness-notes/` hold the
detail of each area.

`mail/example_adapter_test.go` (`ExampleReadTree_adapter`) runs the flow below
on a small damaged message and checks its output under `go test`. Read it
first.

## Recommended flow

```go
import (
    message "github.com/saeroun-ai/emlkit"
    _ "github.com/saeroun-ai/emlkit/charset" // decode legacy charsets
    "github.com/saeroun-ai/emlkit/mail"
)

session, root, err := message.Parse(r, &message.ParseOptions{
    // ProfileTolerant is the zero value: heal and report.
    WalkEmbeddedMessages: true,     // Walk and Drain look into forwarded mail
    FallbackCharset:      "euc-kr", // only where undeclared legacy text is expected
})
if root == nil {
    // No entity: the header could not be read, the options were invalid
    // (errors.Is(err, message.ErrInvalidLimits)), or a parse-wide budget
    // refused the header.
}
tree, terr := mail.ReadTree(root, &mail.TreeOptions{
    MaxBodyBytes:    256 << 10, // what the model can use per leaf
    DescendMessages: true,      // forwarded messages become subtrees
})
verdict := session.Completion()
```

`ReadTree` reads every body to its end, so `Completion` is final when it
returns nil. When it returns an error (a stop of the parse, or the error of a
`TreeOptions.Body` hook) the tree read so far comes with it and `Completion`
says what was not read. Use `root.Drain()` instead of `ReadTree` when no tree
is wanted.

## 1. Parsing

### Entry points

| Call | Input | Use it for |
| --- | --- | --- |
| `message.Parse(r, opts)` | `io.Reader`, read once | the default |
| `message.ParseContext(ctx, r, opts)` | the same, cancellable | a deadline per message |
| `message.ParseAt(ra, size, opts)` | `io.ReaderAt` of `size` bytes | a file or buffer you hold: raw bytes of every entity become readable (section 6) |
| `message.ParseSource(ctx, src, opts)` | a `*message.Source` | a stream you want recorded while it is parsed (section 6) |

All four return `(*ParseSession, *Entity, error)`. The session is never nil.
A non-nil entity is usable whatever the error says:

- `message.IsUnknownCharset(err)` / `IsUnknownEncoding(err)` with a non-nil
  entity: a warning. The stage with no decoder was skipped and the body holds
  what the other stages made of it.
- `message.IsFatal(err)`: a stop. Under `Parse` it is a budget, a stalled
  input, a `DiagnosticSink` refusal, cancellation, or a `ProfileStrict`
  refusal scoped to that entity.
- A nil entity: nothing usable. For `ParseSource`, also `ErrNoRawSource` (nil
  source) or `ErrSourceUsed` (a recorded source parsed twice).

`Read`, `ReadWithOptions`, `New` and `mail.CreateReader` keep the older
behaviour: no session, no diagnostics, and only the root header is budgeted
(1 MiB). They use the same tolerant transfer decoders, but not the header,
multipart and parameter healing of `Parse`, and they report nothing. Do not
use them for untrusted mail.

### Profiles

| `ParseOptions.Profile` | Behaviour | For a classifier |
| --- | --- | --- |
| `ProfileTolerant` (zero value) | heals damaged mail and records each repair | use this |
| `ProfileForensic` | the same, and offers the bytes after the closing delimiter through `ParseSession.TrailingInputReader` | when hidden trailing content matters |
| `ProfileStrict` | refuses the entity (or the rest of a multipart) where a repair would be needed; repairs nothing | conformance checks, not classification |

`ParseOptions.Compatibility` breaks ties. Leave it at the zero value,
`CompatibilityConservative`. Today two decisions read it: where a damaged
header line goes (`header_recover.go`) and whether Base64 data after padding
is decoded (`CompatibilityPythonEmail` stops at the padding).

### Healing policy (#62)

Recovery makes damaged mail readable; it does not establish what the sender
intended. Where the bytes establish one reading, recover that reading and
report the repair. Where alternative semantic readings remain plausible, keep
the declaration and report the conflicting evidence; the caller chooses an
alternative explicitly. `RecoverCharsetDeclarations` is the option for charset
conflicts. Report guesses belong in `StatusGuess`, `ActionGuess` and
`TypeGuess`, apart from the declared fields. Raw APIs keep the original bytes;
unfolding a header value preserves its whitespace.

Two structural exceptions are retained: bounded boundary inference and joining
an unindented continuation under the existing evidence rules. They reconstruct
structure found in the input and report the recovery without rewriting the
raw header. Inference still stops beyond eight inferred levels, and the first
`Content-Type` still wins. The reasons and limits are in
[header-recovery.md](robustness-notes/header-recovery.md) and
[multipart-recovery.md](robustness-notes/multipart-recovery.md). The classification
of every older recovery against this policy remains open under #62; this is
not a claim that the whole inventory has been audited.

### Options

| Field | Effect |
| --- | --- |
| `Limits` | resource budgets; zero fields take the defaults (below) |
| `FallbackCharset` | charset for a text body that declares none, UTF-8 or US-ASCII and whose first bytes are not UTF-8; needs a `CharsetReader` that knows it; every use is recorded (`charset.missing`, `charset.declaration_suspect`) |
| `RecoverCharsetDeclarations` | off by default; under the healing profiles a text body whose bytes contradict its declared charset is decoded as they say: by a byte order mark followed by bytes valid in its encoding (`charset.bom_mismatch`), or as UTF-8 under a legacy label (`charset.declaration_suspect`), both Recovered. ProfileStrict ignores it |
| `WalkEmbeddedMessages` | `Entity.Walk` and `Entity.Drain` descend into `message/rfc822` and `message/global` bodies |
| `RecognizeMboxEnvelope` | keeps one leading mbox `From_` line apart (`ParseSession.MboxEnvelope`) |
| `DiagnosticSink` | receives each diagnostic as it is found and may stop the parse |
| `DiscardDeliveredDiagnostics` | with a sink: the session keeps nothing it delivered |
| `ReadOptions` | the older read options, such as `SkipTextAttachmentDecoding` |

A declaration the bytes contradict is followed by default and the
contradiction reported (issues #62, #65), from the first 4096 bytes after
transfer decoding (1024 after a byte order mark): a body declared in a legacy
charset that is UTF-8, or that begins with a byte order mark of another
encoding and is valid in it, reads two ways, and is decoded as declared with
`charset.declaration_suspect` or `charset.bom_mismatch` at Info, the Action
naming the other encoding. Only with `RecoverCharsetDeclarations` is it
decoded as the bytes say (Recovered). With a `FallbackCharset`, a body
that is mostly valid UTF-8 stays UTF-8 and only one that is not is decoded
with the fallback; text with no charset and no fallback that is not UTF-8 is
kept as read and observed (`charset.missing`, Info). The evidence rules and
their known misses are in charset-plugin.md.

### The charset plugin

Without `import _ "github.com/saeroun-ai/emlkit/charset"` only UTF-8 and
US-ASCII bodies, and UTF-8, US-ASCII and ISO-8859-1 encoded words, are
decoded. A body in any other charset is delivered undecoded, with
`charset.unknown` (Unsupported) and an `IsUnknownCharset` warning on its node
(`Node.Warning`); an encoded word in any other charset is left as written
(`rfc2047.unknown_charset`). The plugin sets
`message.CharsetReader` for the whole process and adds about 1 MiB to the
binary. `FallbackCharset` does nothing without a reader for that charset.

`ParseOptions.FallbackCharset` applies to bodies only. For header text pass
the same charset to `Header.TextWithOptions(key, &message.HeaderTextOptions{FallbackCharset: ...})`.

### Limits

Every budget is an `int64`. Zero is the default, `message.LimitDisabled` (-1)
turns one off, anything below -1 is `ErrInvalidLimits`. They apply under
`Parse`, `ParseContext`, `ParseAt` and `ParseSource` only. `DefaultLimits()`
returns the defaults; `ParseSession.Limits()` the values a parse ran under.

| Field | Default | Past the budget |
| --- | --- | --- |
| `MaxInputBytes` | 256 MiB | the parse ends (`limit.input_bytes`) |
| `MaxHeaderBytes` | 1 MiB | that header block is refused (`limit.header_bytes`): for the root no entity, for a part the iteration of its multipart ends |
| `MaxHeaderFieldBytes` | 64 KiB | as above (`limit.header_field_bytes`) |
| `MaxHeaderCount` | 1024 | as above (`limit.header_count`) |
| `MaxLineBytes` | 64 KiB | as above for a header line; ends the multipart iteration for a framing line and the body for a Base64 or quoted-printable line |
| `MaxTotalHeaderBytes` | 32 MiB | the parse ends |
| `MaxTotalHeaderCount` | 100 000 | the parse ends |
| `MaxParameterCount` | 256 | parameters past it are ignored; nothing ends |
| `MaxParameterSegments` | 256 | the same, for RFC 2231 segments |
| `MaxMIMEDepth` | 100 | the multipart iterator that met it ends |
| `MaxMessageDepth` | 32 | that embedded message is not read; carrier and parse go on |
| `MaxPartCount` | 10 000 | the parse ends |
| `MaxPreambleBytes` | 1 MiB | the preamble is kept up to it; nothing ends |
| `MaxEpilogueBytes` | 1 MiB | `MultipartEpilogue` keeps up to it; `LimitDisabled` keeps the default, never more |
| `MaxEncodedBodyBytes` | 128 MiB | that body ends; siblings are read |
| `MaxDecodedBodyBytes` | 128 MiB | that body ends; siblings are read |
| `MaxTotalDecodedBytes` | 512 MiB | the parse ends |
| `MaxDiagnosticCount` | 1000 | recording stops; nothing ends |
| `MaxMemorySpoolBytes` | 8 MiB | with `ReadTree` and `Spool.Body`: a body that does not fit in memory goes to disk whole; nothing ends |
| `MaxDiskSpoolBytes` | 1 GiB | with `ReadTree` and `Spool.Body`: that body is cut (`limit.disk_spool_bytes` in `Spool.Diagnostics()`); nothing ends |

The two spool fields bound only what `mail.Spool` keeps for a tree `ReadTree`
reads from the parse. The spool applies whichever has less room, the field or
its own `SpoolOptions` quota. `LimitDisabled` leaves the `SpoolOptions` quota
alone, and a negative `SpoolOptions` quota keeps nothing (spool.md).
`MaxLineBytes: LimitDisabled` is no line limit (#64): framing lines
before the first part are streamed at any length, and a padded delimiter
candidate in a body is held until decided, so only `MaxInputBytes` bounds that
memory (bounds-audit-core.md, item (b)). `Read` keeps its 4 KiB framing.

## 2. The whole verdict

### Completion

`Parse` reads only the header. Bodies, parts and their defects are found as
the caller reads, so an empty diagnostic list after `Parse` means nothing.
Read everything first (`ReadTree` or `Entity.Drain`), then call
`ParseSession.Completion()`.

| `Completion().State` | Meaning | Treat the result as |
| --- | --- | --- |
| `not-consumed` | only the header was read | not analysed |
| `in-progress` | a body or multipart was returned and not read to its end | not analysed |
| `complete` | everything was read, nothing stopped the parse, no diagnostic of `SeverityIncomplete`, `SeverityFatal` or `SeverityLimitExceeded` | the whole message as framed; it may still have been repaired |
| `incomplete` | everything was read to its end, and a diagnostic of `SeverityIncomplete` or `SeverityFatal` says something is missing: an unclosed multipart, a line with no ending, a cut Base64 group, a body whose decoder stalled | read as far as it goes, but not a whole message; what is missing is itself a feature |
| `limited` | a budget ended the parse or a scope, a `limit.*` diagnostic was recorded, or the input stalled | part of the message may not have been seen |
| `canceled` | the context of `ParseContext` ended | part not seen |
| `stopped` | a `DiagnosticSink` stopped the parse | part not seen |
| `failed` | the input returned an error other than `io.EOF` | part not seen |

Precedence, first match: canceled, stopped, limited (parse-wide stop),
failed, not-consumed, in-progress, limited (a `limit.*` diagnostic),
incomplete, complete.

`limited` does not always mean bytes went unread: `limit.parameter_*`,
`limit.preamble_bytes` and `limit.diagnostic_count` end nothing. Look at the
`limit.*` code to tell. For `limited`, `canceled`, `stopped` and `failed`,
classify on what was read and say so, or refuse; never report it as a clean
message. `Completion().Clean()` is the strict test: `complete`, not
`Ambiguous`, and nothing recorded but `SeverityInfo`.

`Completion` also counts what was read (`Entities`, `Bodies`, `BodiesRead`,
`Multiparts`, `MultipartsRead`, `InputBytes`), the diagnostics per severity
including those `MaxDiagnosticCount` kept out (`Diagnostics`), the stop
(`Stop`), the input's error (`InputErr`), and `Ambiguous`: the parse chose
between readings that mean different things (several boundaries,
conflicting Content-Type or parameter repeats, an unselected multipart
Content-Type whose boundary delimits the body, conflicting filenames).

An input cut in transit exactly where a message could end is
indistinguishable from a whole one.

### Diagnostics

Diagnostics are data for the model, not errors. Branch on `Code` and
`Severity`, never on `Message`. They come from six places:

| Source | Holds |
| --- | --- |
| `ParseSession.Diagnostics()` | everything the parse recorded, in discovery order, each with `Path`, `Field`, `Action` and `Location` |
| `Tree.Diagnostics` | the shape of signed, encrypted and report entities, unsupported media types, `signed.unverifiable`, `dkim.view_repaired` |
| `Header.TextDiagnostics(k)` / `TextWithOptions` | RFC 2047 and raw 8-bit repairs of one field's text; `Location` is within the value |
| the `*Tolerant`, `Received`, `ListURLs`, `ListID`, `AuthenticationResults` results | repairs of structured fields (`Diagnostics` of each result) |
| `Report.Diagnostics` | what `Node.Report` found in a report |
| `Spool.Diagnostics()` | `limit.disk_spool_bytes` |

| Severity | Meaning for a classifier |
| --- | --- |
| `SeverityInfo` | nonstandard but read as it stands |
| `SeverityRecovered` | a deterministic repair was applied; the view is the parser's reading |
| `SeverityIncomplete` | syntax was lost at the end of input or a cut; what was recovered is usable |
| `SeverityUnsupported` | recognised, no decoder; the raw form is kept |
| `SeverityInvalidMetadata` | content usable, metadata could not be read; also the severity of `ProfileStrict` refusals |
| `SeverityLimitExceeded` | a budget was reached |
| `SeverityFatal` | a scope could not go on; enclosing scopes stay usable |

The session keeps at most `MaxDiagnosticCount` diagnostics; the last is then
`limit.diagnostic_count`, and `Completion().Diagnostics` still counts the
rest. A `DiagnosticSink` receives the same stream synchronously and may stop
the parse by returning an error (`CompletionStopped`). The full list of codes
is in section 12.

## 3. Structure

### ReadTree

`mail.ReadTree(root, opts)` reads the whole MIME tree and returns a
`*mail.Tree`: `Root`, `Diagnostics`, and the signature flags of section 5.
It returns an error only when the parse was stopped or `TreeOptions.Body`
returned one. A defect of one entity stays in that node's `Warning` or `Err`.
Give it an entity from `Parse`; an entity of `Read` or `New` has no session,
so no repair can be mapped to its nodes.

| `TreeOptions` | Default | |
| --- | --- | --- |
| `MaxBodyBytes` | 1 MiB | decoded bytes kept per leaf in `Node.Data`; the rest is read, counted and hashed; negative keeps nothing |
| `MaxTotalBytes` | 64 MiB | all `Node.Data` of the tree together; negative is no limit |
| `DescendMessages` | false | an embedded message becomes the carrier's one child |
| `Body` | nil | a function that reads each leaf instead of `Data` (a `Spool`, a hasher, a scanner) |

### Node

| Fields | Content |
| --- | --- |
| `Path`, `Parent`, `Children` | place; `Path` is nil for the root, as in `Diagnostic.Path` |
| `Header` | the entity's header as read |
| `MediaType`, `Params`, `Disposition`, `DispositionParams` | as the parser interpreted them, lower-cased; `text/plain` when absent |
| `Filename`, `Filenames` | the chosen filename and every candidate (`message.FilenameCandidate`) |
| `ContentID`, `ContentLocation` | without angle brackets; empty when absent |
| `Role`, `Reason` | classification and the rule that gave it |
| `Data`, `Size`, `Truncated`, `SHA256` | decoded body up to `MaxBodyBytes`, the whole decoded size, whether `Data` is a prefix, the digest of the whole body |
| `DuplicateOf` | an earlier leaf with the same non-empty content |
| `Warning`, `Err` | unknown encoding or charset; what ended the reading of the body or parts |
| `HasSpan`, `SpanStart`, `SpanEnd` | the part's bytes in the input |
| `Signed`, `Repaired`, `RepairCodes`, `Unverifiable`, `UnverifiableReason` | section 5 |

### Roles

| `Role` | Rule, first match wins |
| --- | --- |
| `RoleContainer` | a framed multipart (a multipart left unframed is a leaf) |
| `RoleSignature` | the second part of `multipart/signed` |
| `RoleEncrypted` | a part of `multipart/encrypted` |
| `RoleReport` | a `message/*` part of `multipart/report` other than the first, that carries no message |
| `RoleMessage` | `message/rfc822`, `message/global`, `message/news`, `text/rfc822-headers`, `message/rfc822-headers`, `message/global-headers`, unless `Content-Disposition: attachment` and not descended into |
| `RoleAttachment` | `Content-Disposition: attachment` |
| `RoleRelated` | in a `multipart/related`, outside its root part, with a Content-ID or Content-Location |
| `RoleBody` | `text/*` (not `text/calendar`) with no filename, or with `Content-Disposition: inline` |
| `RoleAttachment` | `text/*` (not `text/calendar`) with a filename and no inline disposition |
| `RoleBody` | `text/calendar` with no filename directly in a `multipart/alternative` |
| `RoleRelated` | a non-text part with `Content-Disposition: inline` |
| `RoleAttachment` | anything else |

### Views

| Call | Returns |
| --- | --- |
| `Node.Bodies(prefer...)` | one form of each `multipart/alternative` (the first preferred type offered, else the last alternative with a body), the root part of a `multipart/related`, the first part of a `multipart/signed`; nothing of `multipart/encrypted` or of an embedded message. It lower-cases `prefer` in place |
| `Node.Attachments()` | `RoleAttachment` nodes and, as one each, `RoleMessage` nodes, in document order; an embedded message's own attachments are asked of its node |
| `Node.RelatedRoot()` | the part `start` names, else the first part |
| `Node.Resolve(ref)` | the part a `cid:` URL, Content-ID or Content-Location names; nil when unmatched. Nothing is fetched |
| `Node.Walk(fn)` | every node in document order |
| `Node.Report()` | section 7 |
| `Node.RawSection(src)` | the node's span of your `io.ReaderAt` |

A `format=flowed` body (RFC 3676) is delivered as sent, with its soft line
breaks in place. `Node.FlowedText()` returns the unwrapped text of a
`text/plain` node that declares it, honouring `delsp`; `mail.UnwrapFlowed` is
the same for text of your own.

A multipart node carries the bytes around its parts: `Node.Preamble` and
`Node.Epilogue`, with `PreambleTruncated` and `EpilogueTruncated` when a
budget cut them.

### Filenames

`Node.Filename` is the first of `Node.Filenames`: `Content-Disposition`
`filename*`, then `filename`, then `Content-Type` `name*`, then `name`, the
first decoded non-empty one first. When the candidates name different files,
`mime.filename_conflict` (Info) is recorded and `Completion().Ambiguous` is
set. Both are what the message says, not paths: pass a name through
`message.SafeFilename` before it touches a file system. `SafeFilename` keeps
the last path component, removes control and invisible format characters
(bidirectional overrides, zero-width spaces), prefixes Windows device names,
cuts to 255 bytes and returns `unnamed` for nothing usable.

### Large bodies: Spool

`Node.Data` is capped. To keep whole bodies for a scanner, spool them:

```go
spool := mail.NewSpool(&mail.SpoolOptions{MemoryBytes: 4 << 20, DiskBytes: 256 << 20})
defer spool.Close()
tree, err := mail.ReadTree(root, &mail.TreeOptions{Body: spool.Body})
rc, ok, err := spool.Open(tree.Root.Attachments()[0]) // a fresh reader each call
```

Memory is used until `MemoryBytes` (8 MiB by default) is spent, then whole
bodies go to temporary files in `Dir` or to the caller's `BlobStore`, up to
`DiskBytes` (1 GiB by default). The parse's `MaxMemorySpoolBytes` and
`MaxDiskSpoolBytes` bound the same tree too, and the smaller room applies. A body cut by
either disk quota records `limit.disk_spool_bytes` in
`Spool.Diagnostics()`; the tree's `Size` and `SHA256` are still the whole
body's. A storage failure (a failed `Create`, `Write` or `Close`, a short
write) ends `ReadTree` with that error. `Close` removes everything the spool
made. See spool.md.

## 4. What to send the model

| Data | Source | Notes |
| --- | --- | --- |
| Body text | `tree.Root.Bodies("text/plain", "text/html")` | decoded to UTF-8 when the charset is known; `Truncated` and `Size` say what was cut; a node with `Warning` holds undecoded bytes |
| Attachments | `tree.Root.Attachments()` | `MediaType`, `Filename`, `Filenames`, `Size`, `SHA256`, `DuplicateOf`, `Role`, `Reason` |
| Inline resources | `RoleRelated` nodes; `Node.Resolve` for `cid:` | an unresolved reference is a signal |
| Subject and other text | `mail.Header.Subject`, `message.Header.TextDiagnostics` | always valid UTF-8; CR, LF and NUL decoded from encoded words are neutralised (`rfc2047.header_injection`); the raw value stays in `Get` |
| Addresses | `Header.AddressListTolerant(key, nil)` | usable addresses plus every entry's raw text |
| Date, Message-ID, References | `Header.DateTolerant`, `MessageIDTolerant`, `MsgIDListTolerant(key)` | the raw value is always kept |
| Received | `Header.Received()` | clauses and date per field, in document order |
| List-* | `Header.ListURLs(key)`, `Header.ListID()` | `List-Unsubscribe` URLs, `No` for `NO` |
| Authentication | `Header.AuthenticationResults(opts)` | only `Trusted` entries are evidence; section 5 |
| Defect signals | `session.Diagnostics()`, `tree.Diagnostics`, `Node.RepairCodes` | codes and severities |
| Completeness | `session.Completion()` | section 2 |
| Signatures | `Tree.HeaderRepaired`, `BodyRepaired`, `Node.Unverifiable` | section 5 |
| Reports | `Node.Report()` | bounces, read receipts, abuse reports |
| Hidden content | the optional packages | section 9 |

Healed defects are useful features because another reader may read the same
bytes differently: `multipart.boundary_mismatch`, `multipart.nested_boundary_reused`,
`multipart.close_shared`, `cte.base64_data_after_padding`,
`header.duplicate_singleton` (Recovered), `header.unselected_multipart`,
`rfc2047.header_injection`, `mime.parameter_duplicate`,
`mime.filename_conflict`, `header.missing_body_separator`.

## 5. What may be trusted

Everything in a message is a claim by whoever wrote it, except what the
receiver's own servers added.

| Field | Who wrote it | May be relied on for |
| --- | --- | --- |
| `From`, `Sender`, `Reply-To`, `Subject`, `Message-ID`, `Date` | the sender | what the sender wants shown |
| `Received` fields below the border | each relaying server, unverified | hints only; forgeable |
| `Received` fields at and above the border | the receiver | the connecting host and time |
| `Return-Path` | the final delivery server | the envelope sender it saw |
| `Authentication-Results` with a trusted id at a trusted position | the receiver's border | SPF, DKIM, DMARC, ARC results |
| any other `Authentication-Results`, `Received-SPF`, `X-Spam-*` | unknown | nothing; a forged one is a signal |

### Authentication-Results

`Header.AuthenticationResults(opts)` returns every field, topmost first, and
trusts none unless `AuthResultsOptions.TrustedIDs` names the border servers'
authserv-ids (compared without case). `MaxReceivedAbove` is how many
`Received` fields may stand above a trusted field; zero means none may. A
field that claims a trusted id from a lower position came with the message
and is not `Trusted`. A negative `MaxReceivedAbove` ignores the position,
which is safe only when the border removes foreign fields that claim its id.
emlkit does not verify DKIM, SPF, DMARC or ARC.

### Received

`Header.Received()` splits each field into clauses (`from`, `by`, `via`,
`with`, `id`, `for`) and reads the date after the last `;`. Nothing in a
clause is validated: hosts, addresses and IP literals are text.

### Signed content and repairs

A signature covers bytes as sent; the tree is the parser's healed reading of
them. `ReadTree` maps every parse diagnostic to the node it was recorded
against (see structure-view.md, "Signed content and repairs"):

| Field | Meaning |
| --- | --- |
| `Node.Repaired`, `RepairCodes` | a diagnostic other than Info, Unsupported and `limit.diagnostic_count` was recorded against the node, or its reading ended with `Err` |
| `Node.Signed` | the innermost `multipart/signed` whose first part this node is or is under |
| `Node.Unverifiable`, `UnverifiableReason` | on a `multipart/signed`: something in it was repaired, the signed part has no span, the container's framing or header was repaired, protocol or micalg is missing or wrong, or the tree is not whole; one `signed.unverifiable` (Info) in `Tree.Diagnostics` |
| `Tree.HeaderRepaired`, `HeaderRepairFields` | the root header block was not read as it stands, or repeats a field allowed once |
| `Tree.BodyRepaired` | anything else was repaired, an `Err`, or the tree was not read to its end |
| `Tree.RepairsUnknown` | the session counted more repairs than it kept (sink with `DiscardDeliveredDiagnostics`, or `MaxDiagnosticCount`); both flags and every `Unverifiable` are then set |

When either flag is set and the root has `DKIM-Signature` or
`ARC-Message-Signature`, `Tree.Diagnostics` holds one `dkim.view_repaired`
(Info).

Rules for the adapter:

- Verify a `multipart/signed` over `Children[0]`'s raw span
  (`input[SpanStart:SpanEnd]`), never over `Data`. Verify DKIM over the input
  as received, never over header fields or bodies taken from the tree.
- A repaired or `Unverifiable` view is unverified. A signature that checks out
  over the raw bytes does not vouch for what the tree shows: never report it
  as "verified with repairs". A failed check there means "could not check",
  not "forged".
- The core verifies and decrypts nothing. `smime` and `pgpmime` (section 9)
  verify only with keys, roots or a verifier the caller supplies, and a valid
  signature proves the key holder signed, not who the sender is.

## 6. Raw bytes

| API | Gives |
| --- | --- |
| `Entity.RawSpan()`, `Node.HasSpan`/`SpanStart`/`SpanEnd` | the half-open range of the input holding a part as sent: from the byte after its delimiter line to the line ending before the next one. Known once the part was read past; not for the root, a part of an inferred boundary, or a message inside a Base64 or quoted-printable part |
| `Entity.Origin()` | path, header and body offsets, message depth |
| `message.ParseAt(ra, size, opts)` | a seekable `Source`: raw reads cost nothing |
| `message.NewRecordedSource(r, &message.RecordOptions{...})` + `ParseSource` | the stream kept as the parse reads it, in memory or a `RawStore` (`*os.File` is one), up to `MaxBytes` (`DefaultRecordBytes`, 8 MiB, when zero; no limit only when negative) |
| `Entity.RawReader()`, `RawHeader()`, `RawBody()` | new `*io.SectionReader`s over the source: header and body, header block with its empty line, body before decoding |
| `Entity.DecodedBody()` | a fresh decoded body from `RawBody`, beside the one-shot `Body`; its diagnostics are not recorded |
| `Node.RawSection(src)` | a node's span of your `io.ReaderAt` |
| `message.MultipartPreamble(mr)`, `MultipartEpilogue(mr)` | the bytes before the first and after the closing delimiter of a multipart you iterate yourself, under a healing `Parse` (multipart-recovery.md) |
| `ParseSession.TrailingInputReader()` | under `ProfileForensic`, the input after the message's own closing delimiter, read once |

Raw readers return the input's bytes whatever the parse healed, before and
after any semantic read. Errors, matched with `errors.Is`: `ErrNoRawSource`
(no source), `ErrRawSpanUnknown` (position unknown), `ErrRawNotRecorded`
(past what a recorded source kept). `MaxInputBytes` bounds the parse, not
raw reads. See raw-source.md.

Text after a multipart's closing delimiter is not a part and no client shows
it. `ReadTree` reads it (`Node.Epilogue`) and records `multipart.trailing_junk`
in `Tree.Diagnostics` when it holds anything but white space, saying how many
bytes and whether they look like further delimiter lines, a header block,
text or binary data. Without `ReadTree`, `MultipartEpilogue` and, for the
message's own tail under `ProfileForensic`, `TrailingInputReader` read it.

## 7. Reports

`Node.Report()` reads a `multipart/report`, or a report part
(`message/delivery-status`, `message/global-delivery-status`,
`message/disposition-notification` and its global form,
`message/feedback-report`). Any other node gives nil.

| `Report` field | Content |
| --- | --- |
| `Kind` | `ReportDeliveryStatus`, `ReportDispositionNotification`, `ReportFeedback`, `ReportUnknown` |
| `Delivery`, `Disposition`, `Feedback` | the typed model of the kind (RFC 3464/6533, RFC 8098, RFC 5965) |
| `ReportType`, `Container`, `Description`, `Part`, `Original`, `OriginalHeader` | the declared report-type, the parts, and the returned message's header |
| `Diagnostics` | structure and model repairs, with the part's path |

`mail.ParseDeliveryStatus`, `ParseDispositionNotification` and
`ParseFeedbackReport` read a body directly. A feedback report's
`AuthenticationResults` are raw; `AuthResults()` parses them and trusts none.
Typed fields hold only what the report gives (#66): a recipient without a
readable `Status` has `Status.Valid == false`, one without `Action` has an
empty `Action`, a value without a type has an empty `Type`. What the other
fields suggest is in `StatusGuess`, `ActionGuess` and `TypeGuess`; classify
on a guess only knowingly, as the report does not say it.
A report part cut at `MaxBodyBytes` is read from what was kept
(`report.truncated`). Plain-text bounces that are not `multipart/report` are
not recognised. See reports.md.

## 8. Embedded messages

By default a `message/rfc822` or `message/global` part is one leaf
(`RoleMessage`) and one entry of `Attachments()`. To look inside:

- `TreeOptions.DescendMessages` for `ReadTree`;
- `ParseOptions.WalkEmbeddedMessages` for `Entity.Walk` and `Entity.Drain`;
- `Entity.EmbeddedMessage()` on request (`Entity.CarriesMessage()` tells).

The embedded message belongs to the same parse: same session, healing,
header budgets, one part of `MaxPartCount`, one level of `MaxMIMEDepth`. Its
diagnostics carry its own path (the carrier's path followed by 0).
`MaxMessageDepth` (32) bounds the nesting: a deeper message is not read
(`limit.message_depth`), the carrier stays a leaf. A message inside a Base64
or quoted-printable carrier has no input offsets and no spans. A part of a
`multipart/digest` with no Content-Type is `message/rfc822`.
`message/partial` and `message/external-body` are kept as they are
(`feature.unsupported_media_type`); nothing is reassembled or fetched. See
embedded-messages.md.

## 9. Optional packages

None of these is imported by the core, and none is called by `ReadTree`. Each
works on plain values from a node, reports its own `Issue` type (not
`message.Diagnostic`), bounds its work with its own `Limits`, and returns
names that are untrusted: pass every one through `message.SafeFilename`.

| Package | Call it for | Call | What you get |
| --- | --- | --- | --- |
| `tnef` | `application/ms-tnef`, `application/vnd.ms-tnef`, or `tnef.IsTNEF(n.Data)` (a `winmail.dat` sent as `application/octet-stream`) | `tnef.Decode(n.Data, lim)` or `DecodeReader` | the real attachments, the HTML or RTF-derived body, recipients, embedded TNEF messages |
| `smime` | `application/pkcs7-mime`, `.p7m`, and `multipart/signed` with an S/MIME protocol | `smime.Classify(n.MediaType, n.Params, n.Filename, n.Data)`, then `smime.Parse` or `ExtractSignedContent` | for opaque signed-data, the hidden MIME entity: parse it again with `message.Parse` and read its tree, bounding the depth; signer and recipient metadata; `Verify` over a raw span |
| `pgpmime` | `multipart/signed` and `multipart/encrypted` with the PGP protocol, and text bodies with `-----BEGIN PGP` blocks | `pgpmime.CheckStructure` for containers, `pgpmime.Classify` for leaves, `pgpmime.Parse(n.Data, lim)` for armored text | the text of a clearsigned message, armor blocks, packet metadata (key ids, recipients, literal file names); request builders for a caller's `Verifier` and `Decrypter` |
| `icalendar` | `text/calendar`, `application/ics`, or a `.ics` filename | `icalendar.ParseBytes(n.Data, &icalendar.Options{ContentTypeMethod: n.Params["method"]})`, then `icalendar.Classify` | method, events, organizer and attendees, URLs and attachments, the scheduling intent |
| `appledouble` | `multipart/appledouble` (`appledouble.Recognize`), `application/applefile` | `appledouble.AppleDoubleParts`, `appledouble.Combine(header, data, lim)`; `Parse` for one part | the real file name, type and creator, data and resource forks, extended attributes such as `com.apple.quarantine` |
| `legacyenc` | `text/*` leaves that may carry uuencode, xxencode, `begin-base64`, yEnc or BinHex blocks | `legacyenc.Sniff(n.Data)` as a cheap gate, `legacyenc.Scan` for blocks, `Decode` / `DecodeAll` to decode | each block's range, declared name, mode and size, and the decoded file on request |

Two things hold for all of them:

- `Node.Data` is cut at `TreeOptions.MaxBodyBytes`. When `Node.Truncated` is
  set the package sees a prefix and reports what it lacks. For whole parts,
  raise the cap or read the part through `TreeOptions.Body`.
- What they report is structure, not authentication. A TNEF sender name, a
  PGP issuer key id, an S/MIME certificate in the message and an iCalendar
  organizer are claims.

Their notes: tnef.md, smime.md, pgpmime.md, icalendar.md, appledouble.md,
legacy-encodings.md. Synthetic fixtures only: each package is tested on
inputs written for its tests or made with OpenSSL and gpg, as its note says,
not on samples of real mail.

## 10. Safety boundaries

emlkit:

- bounds every read of `Parse` by `Limits`, and what `ReadTree` keeps by
  `TreeOptions`; set them to what the service can afford;
- never opens a network connection and never executes content; it reads no
  file it was not given and writes files only as `mail.Spool`'s temporary
  files (in `SpoolOptions.Dir`, else the system's temporary directory);
  `message/external-body` is reported, not fetched;
- does not unpack archives, render HTML or follow links;
- returns filenames as the message wrote them;
- verifies no signature and decrypts nothing in the core.

The adapter must:

- treat every string from a message as untrusted when logging, building
  prompts or writing files. Decoded text may hold bidirectional controls,
  zero-width characters and anything else Unicode allows;
- cap what it sends per message, and mark `Truncated` nodes for the model;
- keep the original bytes (or parse from a `Source`) if it needs to verify
  signatures or re-parse;
- decide what `limited` and `failed` map to. emlkit does not turn an
  incomplete message into a normal one.

## 11. Splitting a long message for a context limit

Send structure before content: headers and the diagnostic summary first, then
the preferred body cut on a paragraph boundary, then attachment metadata. When
a body must be split across requests, score each chunk and combine with the
maximum, not a mean: one spam chunk makes a spam message.

## 12. Diagnostic codes

Built from `diagnostic_code.go`, `multipart_nested.go`, `mail/tree_signed.go`
and `mail/report_code.go`, with severities read from each producer. "Where"
says which list holds the code (section 2): S the session, T
`Tree.Diagnostics`, H header text, A the `mail` accessors, R
`Report.Diagnostics`, P `Spool.Diagnostics()`.

A dagger (†) marks a code that `ProfileStrict` records at
`SeverityInvalidMetadata` when it refuses the entity, or the rest of the
multipart, instead of repairing it.

### Input and limits

| Code | Severity | Where | For a classifier |
| --- | --- | --- | --- |
| `input.bom` | Recovered (UTF-8 mark); Unsupported (UTF-16/32 mark)† | S | a header block began with a byte order mark; a UTF-16/32 one leaves the entity opaque and its text unread |
| `input.no_progress` | Fatal | S | the input stalled; the parse ended (`limited`) |
| `input.trailing_messages` | Info | S | (`ProfileForensic`) the bytes after the closing delimiter begin like another message: possible hidden content |
| `limit.input_bytes` | LimitExceeded | S | message over `MaxInputBytes`; the parse ended |
| `limit.header_bytes`, `limit.header_field_bytes`, `limit.header_count` | LimitExceeded | S | one header block too large; that scope ended |
| `limit.line_bytes` | LimitExceeded | S | a header, framing, Base64 or QP line too long; that scope ended |
| `limit.total_header_bytes`, `limit.total_header_count` | LimitExceeded | S | all header blocks together too large; the parse ended |
| `limit.parameter_count`, `limit.parameter_segments` | LimitExceeded | S | parameter flood; the rest of the field ignored; parsing went on |
| `limit.mime_depth` | LimitExceeded | S | nesting deeper than `MaxMIMEDepth`; that container's parts after it were not read |
| `limit.message_depth` | LimitExceeded | S | embedded message too deep; not read |
| `limit.part_count` | LimitExceeded | S | more parts than `MaxPartCount`; the parse ended |
| `limit.preamble_bytes` | LimitExceeded | S | preamble longer than `MaxPreambleBytes`; kept in part; nothing ended |
| `limit.epilogue_bytes` | LimitExceeded | S | epilogue longer than `MaxEpilogueBytes`, when `MultipartEpilogue` read it |
| `limit.encoded_body_bytes`, `limit.decoded_body_bytes` | LimitExceeded | S | one body cut; siblings read |
| `limit.total_decoded_bytes` | LimitExceeded | S | all bodies together over budget; the parse ended |
| `limit.diagnostic_count` | LimitExceeded | S, H, A, R | recording stopped there; more defects exist (`Tree.RepairsUnknown` in a tree) |
| `limit.disk_spool_bytes` | LimitExceeded | P | a spooled body was cut at `SpoolOptions.DiskBytes` or the parse's `MaxDiskSpoolBytes`, whichever had less room; `Size` and `SHA256` are still whole |

### Header

| Code | Severity | Where | For a classifier |
| --- | --- | --- | --- |
| `header.first_line_continuation` | Recovered† | S | a header block began with indented lines, kept apart; they declare nothing |
| `header.orphan_continuation` | Recovered† | S | a damaged line and its continuation lines kept apart; they declare nothing |
| `header.missing_colon` | Recovered | S | a line with no colon inside the header, kept apart |
| `header.invalid_name` | Recovered | S | a field name holds a byte a name cannot; kept apart |
| `header.empty_name` | Recovered | S | a line with nothing before its separator; kept apart |
| `header.unindented_continuation` | Recovered | S | a line that lost its indentation was joined to the field before it |
| `header.missing_body_separator` | Recovered† | S | no empty line before the body; the first line that is no field began the body |
| `header.whitespace_body_separator` | Recovered | S | the header ended at a line of white space only |
| `header.bare_lf` | Info | S | LF line endings; common from scripts |
| `header.bare_cr` | Recovered† | S | bare CR line endings; other readers may split lines differently |
| `header.mixed_line_endings` | Info; Recovered with a bare CR | S | one header block mixes line endings: assembled from pieces |
| `header.truncated_line` | Incomplete† | S | the input ended inside a header line |
| `header.duplicate_singleton` | Info; Recovered when a repeated `Content-Type`, `Content-Transfer-Encoding` or `Content-Disposition` differs | S | a field allowed once repeats; the parser reads the first, a DKIM verifier the last; Recovered sets `Ambiguous` |
| `header.unselected_multipart` | Info | S | a later `Content-Type` of the same block declares a multipart whose boundary delimits lines (opening or closing delimiters) in the first 4096 wire bytes of the body, whether the first field made the entity a leaf or a multipart: a reader that takes the later field sees other parts than this parse (an evasion pattern). The first field is still read; the message names the two media types only; once per entity, when its body or its parts are read to the end; sets `Ambiguous`. Re-read it by removing the earlier field and parsing again (header-recovery.md) |
| `header.mbox_envelope` | Info | S | (`RecognizeMboxEnvelope`) a leading `From_` line was kept apart |
| `header.decoder_no_progress` | InvalidMetadata | S | a charset reader stalled on a parameter's encoded word; value kept raw |
| `header.invalid_utf8` | Recovered (fallback applied); Unsupported (fallback failed); InvalidMetadata (U+FFFD) | H | raw 8-bit header text that is not UTF-8 |
| `header.overlong_line` | Info | S | a header line longer than 998 octets; once per header block, naming the first such field |

### Body

| Code | Severity | Where | For a classifier |
| --- | --- | --- | --- |
| `body.decoder_no_progress` | Fatal | S | a charset reader stalled; that body ended |
| `body.nul` | Info | S | a text body under 7bit, 8bit or no transfer encoding holds NUL bytes; once per body |

### MIME types and parameters

These are recorded when the parse interprets a `Content-Type` or
`Content-Disposition` (construction, `EffectiveContentDisposition`,
`ReadTree`), and returned by `ParseMediaTypeTolerant`,
`ParseContentDispositionTolerant` and the candidate functions.

| Code | Severity | Where | For a classifier |
| --- | --- | --- | --- |
| `mime.content_type_missing_type`, `mime.content_type_missing_subtype`, `mime.content_type_missing_slash` | Recovered†; InvalidMetadata for a multipart left with no boundary | S | the media type had to be guessed |
| `mime.content_type_invalid` | Recovered†; InvalidMetadata as above | S | malformed base or parameter items |
| `mime.disposition_invalid` | Recovered† | S | malformed `Content-Disposition` |
| `mime.parameter_unquoted_value` | Recovered† | S | a value with spaces or specials was not quoted |
| `mime.parameter_unclosed_quote` | Recovered† | S | a quoted value runs to the end of the field |
| `mime.parameter_invalid_escape` | Recovered† (stray quote, trailing backslash); Info (backslash kept) | S | escaping inside a quoted value is wrong |
| `mime.parameter_duplicate` | Recovered† | S | a parameter repeats with different values; a `boundary` is then not chosen; sets `Ambiguous` |
| `mime.parameter_segment_missing`, `mime.parameter_segment_duplicate`, `mime.parameter_bad_percent_escape` | Recovered† | S | a damaged RFC 2231 value, joined as far as possible |
| `mime.parameter_unknown_charset` | Recovered (an RFC 2231 value with no charset prefix†, or raw bytes read in the entity's charset); Unsupported (bytes kept undecoded) | S | parameter bytes in an undeclared or undecodable charset; `ParameterCandidate.Undecoded` marks them |
| `mime.parameter_rfc2047` | Info | S | encoded words in a parameter (common in filenames) |
| `mime.parameter_raw_utf8` | Info | S | raw UTF-8 in a parameter |
| `mime.filename_conflict` | Info | S | the candidates name different files (`a.pdf` and `a.exe`); sets `Ambiguous` |

### Multipart

| Code | Severity | Where | For a classifier |
| --- | --- | --- | --- |
| `multipart.boundary_inferred` | Recovered | S | no boundary declared; framed on the one the body uses |
| `multipart.boundary_mismatch` | Recovered | S | the declared boundary frames nothing; another one does |
| `multipart.boundary_ambiguous` | Info | S | several boundaries could frame the body; sets `Ambiguous` |
| `multipart.start_not_found` | Recovered; Incomplete when the body was cut at `MaxPreambleBytes` or nested in more than 8 inferred multiparts | S | no delimiter line; the body is one part with no header |
| `multipart.close_not_found` | Incomplete | S | no closing delimiter: cut, or sloppy |
| `multipart.close_truncated` | Recovered | S | the input ended inside the closing delimiter |
| `multipart.empty` | Info | S | a multipart with no part |
| `multipart.nested_boundary_reused` | Recovered | S | a nested multipart declares an enclosing boundary; the tree is ambiguous |
| `multipart.nested_boundary_conflict` | Recovered | S | nested and enclosing boundaries collide through white space or `--` |
| `multipart.close_shared` | Recovered | S | the iteration read on past a shared closing line; those parts would otherwise be hidden in the epilogue |
| `multipart.illegal_transfer_encoding` | Recovered | S | a multipart declares Base64 or QP; ignored |
| `multipart.bare_lf` | Info | S | delimiter lines with LF endings |
| `multipart.bare_cr` | Recovered† | S | delimiter lines with bare CR endings |
| `multipart.mixed_line_endings` | Info; Recovered with a bare CR | S | delimiter lines mix line endings |
| `multipart.trailing_junk` | Info | T | (`ReadTree`) the epilogue holds more than white space: text no client shows |

### Transfer encodings

| Code | Severity | Where | For a classifier |
| --- | --- | --- | --- |
| `cte.invalid_token` | Recovered | S | `Content-Transfer-Encoding` written oddly (`base-64`, quotes, parameters) |
| `cte.unknown` | Unsupported | S | no decoder; body raw; `IsUnknownEncoding` warning |
| `cte.seven_bit_violation` | Info | S | a `7bit` body holds a NUL or 8-bit byte |
| `cte.seven_bit_overlong_line` | Info | S | a `7bit` body holds a line over 998 octets; once per body, at the first such line |
| `cte.base64_junk` | Recovered | S | bytes outside the alphabet were skipped |
| `cte.base64_url_alphabet` | Recovered | S | `-` and `_` read as `+` and `/` |
| `cte.base64_data_after_padding` | Recovered | S | data after `=` was decoded too; a scanner that stops at the padding sees less |
| `cte.base64_padding_invalid` | Recovered | S | misplaced `=` ignored |
| `cte.base64_padding_missing` | Recovered | S | final group unpadded; also what a cut Base64 body looks like |
| `cte.base64_length_invalid` | Incomplete | S | a single left-over character was dropped |
| `cte.qp_invalid_hex` | Recovered | S | `=` not followed by two hex digits, kept as text |
| `cte.qp_trailing_equals` | Recovered | S | the body ends with `=` |
| `cte.qp_invalid_soft_break` | Recovered | S | a soft break ended by a bare CR; white space between `=` and the line ending is transport padding and records nothing |
| `cte.base64_truncated` | Incomplete | S | a Base64 part ends inside a group because the input ended there: the attachment is cut |
| `cte.qp_control_byte` | Info | S | unencoded control byte kept |
| `cte.qp_overlong_line` | Info | S | an encoded line over 998 bytes |
| `cte.uuencode_invalid` | Recovered | S | a line that is not a whole uuencoded line |
| `cte.uuencode_missing_begin` | Recovered | S | no `begin` line |
| `cte.uuencode_missing_end` | Incomplete | S | no `end` line |

### Embedded messages

| Code | Severity | Where | For a classifier |
| --- | --- | --- | --- |
| `message.illegal_transfer_encoding` | Info | S | `message/rfc822` declared Base64 or QP; decoded as declared, so the embedded message has no input offsets |
| `message.embedded_invalid` | InvalidMetadata | S | the embedded message's header block could not be read; the carrier stays a leaf |
| `message.embedded_truncated` | Incomplete | S | the carrier holds no bytes at all (on an explicit `EmbeddedMessage` call) |

### Charsets and encoded words

| Code | Severity | Where | For a classifier |
| --- | --- | --- | --- |
| `charset.unknown` | Unsupported | S | no decoder for the body's charset; body undecoded; import the plugin |
| `charset.missing` | Recovered (decoded with `FallbackCharset`); Info (no fallback, kept as read) | S | text that declares no charset and is not UTF-8 |
| `charset.declaration_suspect` | Recovered (healed: decoded with the fallback, or, with `RecoverCharsetDeclarations`, a legacy declaration over UTF-8 read as UTF-8); Info (a legacy declaration over UTF-8 decoded as declared, the Action naming UTF-8; or US-ASCII declared, other bytes kept as read) | S, H | the declared charset is not what the bytes are |
| `mime.parameter_fallback_charset` | Recovered | S | a raw 8-bit parameter value, a filename mostly, decoded with `FallbackCharset` |
| `charset.invalid_sequence` | Info (body); Recovered (decoded word) | S, H | text declared UTF-8 is not valid UTF-8; the body keeps its bytes, a word gets U+FFFD |
| `charset.bom_mismatch` | Recovered (decoded as the mark says, only with `RecoverCharsetDeclarations`); Info (declaration kept; the Action names the mark's encoding when the bytes are also valid in it) | S | a byte order mark contradicts the declared charset |
| `charset.empty` | Recovered | H | an encoded word names no charset |
| `rfc2047.invalid_word` | Recovered (no closing `?=`); InvalidMetadata (the charset reader failed) | H | a malformed encoded word |
| `rfc2047.unknown_charset` | Unsupported; Recovered for placeholder labels such as `unknown-8bit` | H, A | an encoded word in a charset with no decoder, left as written |
| `rfc2047.unknown_encoding` | Unsupported | H | an encoding letter other than B or Q |
| `rfc2047.invalid_base64` | Recovered; InvalidMetadata (left verbatim) | H | damaged B text |
| `rfc2047.invalid_q` | Recovered | H | damaged Q text |
| `rfc2047.illegal_location` | Recovered | H | an encoded word inside a quoted string |
| `rfc2047.embedded_word` | Recovered | H | an encoded word glued to text or another word |
| `rfc2047.raw_utf8` | Info | H | raw UTF-8 outside encoded words (RFC 6532) |
| `rfc2047.header_injection` | Recovered | H | an encoded word decodes to CR, LF or NUL: neutralised; an injection attempt |
| `rfc2047.word_limit` | LimitExceeded | H | more than 10 000 words in one value; the rest left as written |

### Structured header fields

Returned in the `Diagnostics` of the `mail` accessor results, never in the
session.

| Code | Severity | Where | For a classifier |
| --- | --- | --- | --- |
| `address.invalid` | Recovered; InvalidMetadata (kept raw only) | A | an address had to be repaired or could not be read |
| `address.partial_list` | Recovered (some entries unusable); LimitExceeded (16 384 entries) | A | part of the list is unusable |
| `address.unquoted_comma` | Recovered | A | `Doe, John <jd@…>` read as one address |
| `address.local_only` | Recovered | A | an address with no domain |
| `address.group_depth` | Recovered | A | nested groups flattened |
| `address.raw_utf8` | Info (UTF-8); Unsupported (8-bit, not UTF-8) | A | non-ASCII address text |
| `date.invalid` | Recovered; InvalidMetadata (`Valid` false) | A | a date in a nonstandard form, or none readable |
| `date.obsolete_zone` | Info (RFC 5322 names); Recovered (others) | A | an obsolete or unknown zone name |
| `date.missing_zone` | Recovered | A | no zone; read as +0000 |
| `date.weekday_mismatch` | Recovered | A | the weekday does not fit the date |
| `message_id.invalid` | Recovered; Incomplete (cut at the end); InvalidMetadata | A | a malformed identifier |
| `message_id.partial_list` | Recovered; LimitExceeded | A | part of the list is unusable |
| `received.invalid` | Recovered; InvalidMetadata; LimitExceeded | A | a malformed `Received` field |
| `list_header.invalid` | Recovered; Incomplete; InvalidMetadata; LimitExceeded | A | a malformed `List-*` field |
| `authentication_results.invalid` | Recovered; InvalidMetadata | A | a malformed `Authentication-Results` field |

### Tree

| Code | Severity | Where | For a classifier |
| --- | --- | --- | --- |
| `signed.structure_invalid` | InvalidMetadata (part count); Info (protocol) | T | a `multipart/signed` of the wrong shape |
| `encrypted.structure_invalid` | InvalidMetadata (part count); Info (protocol) | T | a `multipart/encrypted` of the wrong shape |
| `report.invalid` | InvalidMetadata (part count); Info (report-type) | T | a `multipart/report` of the wrong shape |
| `feature.unsupported_media_type` | Unsupported | T | `message/partial` (not reassembled) or `message/external-body` (never fetched) |
| `signed.unverifiable` | Info | T | a `multipart/signed` whose view cannot stand for what was signed |
| `dkim.view_repaired` | Info | T | a DKIM- or ARC-signed message whose header or body was repaired |

### Reports

| Code | Severity | Where | For a classifier |
| --- | --- | --- | --- |
| `report.missing_field` | InvalidMetadata | R | a required report field is missing; a guess, if any, is in a `...Guess` field |
| `report.invalid_status` | Recovered; InvalidMetadata | R | `Status` is not a bare RFC 3463 code |
| `report.invalid_action` | Recovered; InvalidMetadata | R | `Action` is not an RFC 3464 action |
| `report.invalid_field` | Info; Recovered; InvalidMetadata | R | another field does not read as its RFC writes it |
| `report.misplaced_field` | Recovered | R | a field outside its group, read where it belongs |
| `report.duplicate_field` | Recovered | R | a field repeated where it may appear once |
| `report.missing_group_separator` | Recovered | R | no blank line between groups |
| `report.unexpected_blank_line` | Recovered | R | a blank line inside a one-group report |
| `report.garbage_line` | Recovered | R | a line that is no field, kept in `Unparsed` |
| `report.unindented_continuation` | Recovered | R | free text read as a continuation |
| `report.non_ascii` | Info; Recovered (not UTF-8) | R | 8-bit bytes in an ASCII-only report |
| `report.type_mismatch` | Info; Recovered | R | report-type and report part disagree, or the kind was inferred |
| `report.missing_part` | InvalidMetadata (no report part); Info (no human-readable part) | R | a report without the parts RFC 6522 requires |
| `report.truncated` | Incomplete | R | the report part was cut at `MaxBodyBytes` |
| `report.limit_exceeded` | LimitExceeded | R | more bytes, fields or recipients than are read |

### Reserved and not emitted

The registry declares these and no producer records them:
`input.empty`, `input.read_error`, `input.cancelled`,
`limit.memory_spool_bytes`, `header.semicolon_separator`,
`header.name_whitespace`, `header.nul`, `header.misplaced_mbox_envelope`,
`mime.parameter_missing_name`, `mime.parameter_missing_equals`,
`mime.parameter_missing_value`, `mime.parameter_segment_mixed_encoding`,
`multipart.boundary_missing`, `multipart.boundary_empty`,
`multipart.boundary_invalid`, `multipart.boundary_too_long`,
`multipart.invalid_delimiter_suffix`, `multipart.empty_part`,
`multipart.part_header_missing`, `multipart.part_separator_missing`,
`multipart.false_boundary_candidate`,
`cte.illegal_for_multipart`, `cte.illegal_for_message`,
`charset.alias_recovered`,
`charset.decode_truncated`, `address.comment_depth`. An input error and a
cancellation are errors, not diagnostics.

### Issue codes of the optional packages

Each package has its own `Issue` type; the codes are its own and may share a
spelling with a core code without sharing its meaning. Only `smime` grades
its issues (`smime.Severity`: `Info`, `Warning`, `Error`).

| Package | Defined in | Codes |
| --- | --- | --- |
| `tnef` | `tnef/tnef.go` | `stream.` truncated, damaged, resync; `attribute.` checksum, level; `attachment.no_renddata`; `property.` truncated, type; `codepage.` unknown, undeclared; `rtf.` damaged, crc, compression, extract; `embedded.not_tnef`; `limit.` input, attributes, attachments, properties, attachment_bytes, decoded_bytes, rtf_bytes, body_bytes, string_bytes, depth |
| `smime` | `smime/issue.go` | `armor.` pem, base64, nested, trailing_data; `ber.` indefinite_length, truncated, length_exceeds_input, length_invalid, tag_invalid, indefinite_primitive, unexpected_eoc; `cms.` unexpected_element, missing_field, trailing_data, unknown_content_type, content_absent, chunked_content, chunk_invalid, content_not_octet_string, oid_invalid, certificate_invalid, certificate_other, attribute_invalid, attribute_repeated, signing_time_invalid, signed_attributes_not_der, no_signers, inflate_failed, compression_unknown; `limit.` depth, elements, input_bytes, content_bytes, inflated_bytes, certificates, signers, recipients, attributes, issues, certificate_bytes |
| `pgpmime` | `pgpmime/issue.go` | `structure.` part_count, part_order, protocol_missing, protocol_mismatch, micalg_missing, micalg_invalid, control_missing, control_version, payload_type, rewrapped; `classify.type_mismatch`; `armor.` quoted, label_unknown, begin_damaged, header_invalid, blank_line_missing, end_missing, end_mismatch, end_damaged, invalid_character, padding, data_after_crc, empty, crc_missing, crc_mismatch, crc_invalid; `cleartext.` signature_missing, unescaped_dash, hash_unknown, hash_mismatch; `packet.` invalid_header, truncated, partial_not_allowed, partial_short, unknown_tag, version_unknown, malformed, legacy_encryption, compression_unknown, decompress_error; `limit.` input, blocks, packets, kept_bytes, decompressed_bytes, depth, subpackets, user_ids, armor_headers, issues; `verify.` no_signed_bytes, no_signature, signature_type, micalg_mismatch; `decrypt.not_encrypted` |
| `icalendar` | `icalendar/issue.go` | `input.` bom, utf16; `line.` bare_ending, empty, leading_fold, fold_splits_utf8, no_colon, bad_name, name_space, name_case, unfolded_text; `param.` no_value, unterminated_quote, after_quote, unquoted_uri, encoded_word; `structure.` before_begin, after_end, missing_vcalendar, missing_end, stray_end, bad_begin, no_calendar, multiple_calendars; `calendar.` no_version, version, method_mismatch, method_from_content_type; `property.duplicate`; `event.` no_uid, end_and_duration; `value.` invalid_utf8, bad_integer, bad_duration; `datetime.` invalid, loose, date_without_value, tzid_with_utc, unknown_tzid, zone_mapped; `address.` no_scheme, bad_escape; `rrule.` no_freq, invalid, count_and_until; `attachment.bad_base64`; `limit.` input_bytes, line_bytes, lines, depth, components, properties, params, attendees, attachment_bytes, zones, issues |
| `appledouble` | `appledouble/issue.go` | `header.` truncated, version; `entry.` out_of_range, overlap, duplicate, short, unexpected; `name.long`; `xattr.` header, name, out_of_range, outside_entry; `rsrc.` map, out_of_range; `budget.` entries, entry_bytes, total_bytes, attributes, resources, issues; `combine.` missing_data, data_conflict |
| `legacyenc` | `legacyenc/legacyenc.go` | missing_end, line_too_long, scan_limit, block_limit, budget, total_limit, truncated, size_mismatch, crc_mismatch, invalid_char, invalid_line, short_line, line_junk, bad_header, part_range, dangling_escape, bad_run, trailing_data |

For a classifier what these packages expose usually matters more than their
issues: TNEF attachments, the content inside opaque S/MIME, a clearsigned
text, a calendar's URLs, an AppleDouble quarantine attribute, a legacy block
in a text body. Of the issues, `tnef` `stream.resync` says that attribution
after damage is a guess, and `pgpmime` `classify.type_mismatch` marks a part
named like OpenPGP data that is not (an `invoice.pdf.gpg` that is a PDF). Each
package's note gives the rule behind every code.
