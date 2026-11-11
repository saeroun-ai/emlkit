# Structure view (ROB-100-001 to -005, ROB-090-004 to -006, -008, ROB-110-005, ROB-120-005, -006)

Issues #38, #39, #40, #42, #51. Code: `mail/tree.go`, `mail/tree_signed.go`,
`origin_public.go`, `entity_session.go`. Tests: `mail/tree_test.go`,
`mail/tree_signed_test.go`.

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
- `Filename` is the chosen candidate of ROB-050-009; `Filenames` holds all of
  them. Neither is a safe path: use `message.SafeFilename`.
- No signature is verified and nothing is decrypted (ROB-140).
- Content-Base is not consulted for relative references.

## Signed content and repairs (ROB-120-006)

A signature covers bytes as sent; the tree is the parser's healed reading of
them. `ReadTree` now reads the parse's diagnostics through the new
`Entity.ParseSession()` and maps each to the node of the entity it names by
`Path` (a refused part's to its container). A diagnostic **repaired** the view
when its severity is Recovered, Incomplete, InvalidMetadata, LimitExceeded or
Fatal (or unknown). Info and Unsupported are observations: the bytes were read
as they stand, or passed through raw. `limit.diagnostic_count` is not mapped.

Per node:

- `Repaired`, `RepairCodes`: the node's own repairs, each code once; also set
  when its reading ended with `Err`.
- `Signed`: the innermost `multipart/signed` whose first part this node is or
  is under. A container and its signature part carry the `Signed` of what they
  are in, not their own.

Per `multipart/signed`: `Unverifiable` and `UnverifiableReason` (all reasons,
`; `-joined), and one `signed.unverifiable` (Info) in `Tree.Diagnostics`, when

- a node at or under the signed part, or under the signature part, is
  `Repaired` (the reason names the first one and its codes);
- the signed part has no raw span;
- the container itself is `Repaired` — its framing or its own header (for a
  root container that is the message header, which declares the boundary);
- it does not have exactly two parts;
- `protocol` is missing or is not the signature part's media type, or
  `micalg` is missing, or is not `pgp-*` for `application/pgp-signature`, or
  is `pgp-*` for `application/pkcs7-signature` / `x-pkcs7-signature`;
- the tree was not read to its end (a stop, or the `Body` hook failed), or
  `RepairsUnknown`.

Verify over `Children[0]`'s raw span, never over `Data`. A signature that
checks out over the raw bytes does not vouch for an `Unverifiable` view: treat
it as unverified, never as verified with repairs. Opaque S/MIME
(`application/pkcs7-mime`) is a leaf; its own `Repaired` says whether its
decoded DER was healed.

Per message (DKIM, ARC), on `Tree`:

- `HeaderRepaired`: a repair recorded against the root that is a `header.*`,
  `input.bom`, `input.empty` or header-budget code or lies before the root's
  body, or a `header.duplicate_singleton` at any severity (the parser reads the
  first occurrence, a DKIM verifier the last). `HeaderRepairFields` are the
  fields those diagnostics name; most header repairs name none.
- `BodyRepaired`: any other repair, at the root's body or anywhere below, an
  `Err`, or a tree not read to its end.
- `RepairsUnknown`: the session counted (`Completion.Diagnostics`) more
  repairs than it kept — a sink with `DiscardDeliveredDiagnostics`, or the
  diagnostic budget. Both flags are then set and every signed container is
  unverifiable.
- one `dkim.view_repaired` (Info, `Field` the signature field) when either flag
  is set and the root header has `DKIM-Signature` or `ARC-Message-Signature`.

A DKIM result computed from the parsed view cannot be trusted when either flag
is set; verify DKIM over the input as received.

The pass is one walk in document order plus a binary search per signed
container, so nested signed entities cost no more than the tree.

Changed expectation: `TestReadTree_shapes` — the two `multipart/signed` shape
defects are now followed by `signed.unverifiable`; the test drops that note
before counting.

Open: `Read`/`ReadWithOptions`/`New` entities have no session, so only `Err`
is known (they heal nothing that is reported). A repair outside a signed
container — in an enclosing multipart's framing — is not counted against it
when the container's own framing and span are intact. `multipart/encrypted`
payloads are not marked. A header repair that names no field cannot be tied
to one: `HeaderRepairFields` does not map locations to fields.

## Observations

ROB-130-003, corpus findings list-footer-after-close and
provider-mobile-format-flowed. Code: `mail/tree_epilogue.go`,
`mail/flowed.go`, a few lines in `mail/tree.go`. Tests:
`mail/ob_tree_test.go`, `mail/flowed_test.go`.

- `Node.Preamble`, `Node.Epilogue`, `PreambleTruncated`, `EpilogueTruncated`:
  a multipart's bytes before its first and after its closing delimiter line;
  an epilogue that is not only white space adds `multipart.trailing_junk`
  (Info) to `Tree.Diagnostics`. See multipart-recovery.md, Observations.
- `Node.Bodies(prefer...)` no longer lower-cases and trims the caller's slice
  in place; it works on a copy.
- `mail.UnwrapFlowed(text, delSp)` unwraps an RFC 3676 format=flowed body:
  quote depth per line, one space of space-stuffing removed, a line ending in
  a space joined to the next line of the same depth (its space deleted with
  `delSp`), a flowed line before another depth or the end treated as fixed,
  and the `-- ` signature separator never flowed. Each paragraph is written
  with its quote marks and one space, and the line ending of its last line.
  `Node.FlowedText()` applies it, with the `delsp` parameter, to the `Data`
  of a `text/plain` node whose Content-Type says `format=flowed`, and reports
  false for any other node. Bodies are still delivered as sent.
