# Structure view (ROB-100-001 to -005, ROB-090-004 to -006, -008, ROB-110-005, ROB-120-005)

Issues #38, #39, #40, #42, #51. Code: `mail/tree.go`, `origin_public.go`.
Tests: `mail/tree_test.go`.

## API

`mail.ReadTree(e, opts)` reads the whole MIME tree of an entity and returns a
`*mail.Tree`: `Root` and the shape `Diagnostics`. Every entity is a `Node`:

- place: `Path`, `Parent`, `Children`
- declared: `Header`, `MediaType`, `Params`, `Disposition`,
  `DispositionParams`, `Filename`, `ContentID`, `ContentLocation`
- classified: `Role` and `Reason`
- content: `Data` (up to `MaxBodyBytes`), `Size`, `Truncated`, `SHA256`,
  `DuplicateOf`
- outcome: `Warning` (unknown encoding or charset), `Err`
- raw bytes: `HasSpan`, `SpanStart`, `SpanEnd`

`TreeOptions`: `MaxBodyBytes` (1 MiB per leaf), `MaxTotalBytes` (64 MiB per
tree), `DescendMessages`, and a `Body` hook that reads each leaf itself
instead of having it buffered.

A defect of one entity stays that node's concern. `ReadTree` returns an error
only when the parse was stopped or the `Body` hook returned one.

Also new in the root package: `Entity.Origin()` (path and input offsets) and
`Entity.RawSpan()`.

## Roles (ROB-100-002)

| Role | Rule, first match wins |
| --- | --- |
| `container` | multipart |
| `signature` | second part of `multipart/signed` |
| `encrypted` | part of `multipart/encrypted` |
| `report` | `message/*` report part of `multipart/report` |
| `message` | `message/rfc822`, `message/global`, `text/rfc822-headers`, unless `Content-Disposition: attachment` |
| `attachment` | `Content-Disposition: attachment` |
| `related` | in a `multipart/related`, outside its root part, with a Content-ID or Content-Location |
| `body` | `text/*` with no filename, or with `Content-Disposition: inline` |
| `attachment` | `text/*` with a filename and no inline disposition |
| `body` | `text/calendar` alternative with no filename |
| `related` | non-text with `Content-Disposition: inline` |
| `attachment` | anything else |

`Reason` says which rule applied.

## Views

- `Node.Bodies(prefer...)` (ROB-100-003): one form of each
  `multipart/alternative`, by the caller's order of media types, else the last
  alternative that has a body. `multipart/related` gives its root part's body,
  `multipart/signed` its first part's. No alternative is discarded.
- `Node.Attachments()`: attachments in document order; an embedded message
  counts as one, and its own attachments are asked of its node.
- `Node.RelatedRoot()` (ROB-090-004): the part the `start` parameter names,
  else the first.
- `Node.Resolve(ref)` (ROB-100-004): `cid:` URLs and bare Content-IDs by
  Content-ID; other references by Content-Location, exactly and then resolved
  against the referring part's Content-Location. Nothing is fetched
  (ROB-120-005); an unmatched reference is unresolved.
- `Node.ReportFields()` (ROB-090-005): the field groups of a delivery status,
  disposition notification or feedback report; `Data` is the raw fallback.
- `Node.Walk(fn)`.

## Raw bytes for signatures (ROB-090-006, ROB-110-005)

`SpanStart`/`SpanEnd` are the part's bytes in the input, header and body,
before any decoding or repair. A verifier that kept the input slices it there:
for `multipart/signed` that is `Children[0]`'s span. Spans are known for parts
framed by a multipart of the input; not for the root, for parts of an inferred
boundary, or for a message embedded in an encoded part.

## Shape diagnostics

`signed.structure_invalid`, `encrypted.structure_invalid`, `report.invalid`
when the part count or declared protocol/report-type does not fit, and
`feature.unsupported_media_type` for `message/partial` (not reassembled) and
`message/external-body` (never fetched) (ROB-090-008). They are in
`Tree.Diagnostics`, apart from the parse session's.

## Limitations

- Bodies are read whole to learn size and digest, so `ReadTree` costs a full
  read of the message even with `MaxBodyBytes: -1`.
- `Filename` is the first of the disposition's `filename` and the type's
  `name`; the candidate list of ROB-050-009 is not consulted here yet.
- No signature is verified and nothing is decrypted (ROB-140).
- Content-Base is not consulted for relative references.
