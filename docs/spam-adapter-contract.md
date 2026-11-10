# Contract for a spam-analysis adapter

Issues #44 and #45. This page says what emlkit hands an adapter that feeds a
classifier, what each piece may be relied on for, and what the adapter must
do itself.

## Recommended flow

```go
session, root, err := message.Parse(r, &message.ParseOptions{
    // ProfileTolerant is the default: heal and report.
    WalkEmbeddedMessages: true, // only if forwarded mail should be looked into
})
if root == nil {
    // No entity: the input failed or a parse-wide budget refused the header.
}
tree, terr := mail.ReadTree(root, &mail.TreeOptions{
    MaxBodyBytes:    256 << 10, // what the model can use
    DescendMessages: true,
})
verdict := session.Completion()
```

`ReadTree` reads every body to its end, so `Completion` is final afterwards.
Use `root.Drain()` instead when the tree view is not wanted.

## What to send the model

| Data | Source | Notes |
| --- | --- | --- |
| Body text | `tree.Root.Bodies("text/plain", "text/html")` | `Data` is decoded to UTF-8 and cut at `MaxBodyBytes`; `Truncated` and `Size` say so |
| Attachments | `tree.Root.Attachments()` | name, media type, size, SHA-256; `DuplicateOf` |
| Inline resources | nodes with `RoleRelated`; `Node.Resolve` for `cid:` | unresolved references are a signal |
| Subject and other text | `mail.Header.Subject`, `message.Header.TextDiagnostics` | decoded, with CR/LF/NUL neutralized |
| Addresses | `Header.AddressListTolerant` | partial lists with raw text per entry |
| Date, Message-ID, Received, List-* | the `*Tolerant` accessors, `Header.Received`, `Header.ListURLs` | raw value always kept |
| Authentication | `Header.AuthenticationResults(opts)` | only `Trusted` entries are evidence; see below |
| Defect signals | `session.Diagnostics()`, `tree.Diagnostics` | branch on `Code`, never on `Message` |
| Completeness | `session.Completion()` | see below |

Diagnostics are data for the model, not errors. Healed defects that are rare
in legitimate mail are useful features: `multipart.boundary_mismatch`,
`cte.base64_data_after_padding`, `header.duplicate_singleton` (Recovered),
`rfc2047.header_injection`, `multipart.nested_boundary_reused`,
`mime.filename_conflict`, `header.missing_body_separator`.

## Completeness

Do not treat a message as analysed unless `Completion().State` is `complete`
or `incomplete`. `not-consumed` and `in-progress` mean the adapter stopped
reading. `limited`, `canceled`, `stopped` and `failed` mean part of the
message was never seen: classify on what was read and say so, or refuse; never
report it as a clean message. `Completion().Clean()` is the strict test.

An input cut in transit exactly where a message could end is indistinguishable
from a whole one.

## Trust boundaries

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

`Header.AuthenticationResults` trusts nothing unless the adapter passes
`AuthResultsOptions.TrustedIDs` (its border servers' authserv-ids) and
`MaxReceivedAbove` (how many `Received` fields its own servers put above the
result). A field that claims a trusted id from a lower position came with the
message. emlkit does not verify DKIM, SPF, DMARC or ARC itself.

## Safety boundaries

emlkit:

- bounds every read by `Limits` (input, headers, lines, parts, depth, message
  nesting, decoded bytes, diagnostics); set them to what the service can
  afford. `mail.TreeOptions` bounds what is kept in memory.
- never opens a network connection, never reads a file, never executes
  content. `message/external-body` is reported, not fetched.
- does not unpack archives, render HTML, or follow links.
- returns filenames as the message wrote them. Use `message.SafeFilename`
  before using one as a path.
- does not decrypt or verify signatures; `Node.SpanStart`/`SpanEnd` locate the
  signed bytes for a verifier.

The adapter must:

- treat every string from a message as untrusted when logging, building
  prompts or writing files. Decoded text may hold bidirectional controls,
  zero-width characters and anything else Unicode allows.
- cap what it sends per message; `Truncated` nodes should be marked as such
  for the model.
- keep the original bytes if it needs to re-verify signatures or re-parse.
- decide what `limited` and `failed` map to. emlkit does not turn an
  incomplete message into a normal one.

## Splitting a long message for a context limit

Send structure before content: headers and the diagnostic summary first, then
the preferred body cut on a paragraph boundary, then attachment metadata. When
a body must be split across requests, score each chunk and combine with the
maximum, not a mean: one spam chunk makes a spam message.
