# Parse completion (issue #10)

Code: `completion.go`, hooks in `session.go`, `progress.go`, `limit_body.go`,
`multipart.go`, `entity.go`. Tests: `completion_test.go`. Ledger: ROB-000-006.

## Why

`Parse` reads the header and returns. Bodies, parts and their defects are
found as the caller reads. An empty diagnostic list is therefore the list of a
message nobody has read as much as that of a clean one.

## API

- `ParseSession.Completion()` — a snapshot; reads nothing.
- `Entity.Drain()` — reads and discards whatever is still unread, so that the
  snapshot and the diagnostics are final. Safe to call again and after a
  partial read.
- `Completion.Clean()` — complete, not ambiguous, and nothing recorded above
  `SeverityInfo`.

| State | Meaning |
| --- | --- |
| `not-consumed` | only the header was read |
| `in-progress` | a body or multipart was returned and not read to its end; a skipped part stays here |
| `complete` | every body and multipart read to its end, no stop, no lost syntax, budget or failure |
| `incomplete` | all read, and a diagnostic of `SeverityIncomplete` or `SeverityFatal` exists |
| `limited` | a budget ended the parse or a scope, or the input stalled |
| `canceled` | the context of `ParseContext` was cancelled |
| `stopped` | a `DiagnosticSink` stopped the parse |
| `failed` | the input returned an error other than `io.EOF` |

Precedence, first match: canceled, stopped, limited (parse-wide stop), failed,
not-consumed, in-progress, limited (scope), incomplete, complete.

The snapshot also carries `InputBytes`, `InputEnded`, `InputErr`, `Stop`,
entity/body/multipart counts with how many were read to their end, diagnostic
counts by severity (including those `MaxDiagnosticCount` kept out of the
session), and `Ambiguous` (more than one boundary, conflicting repeats of
Content-Type or a parameter, conflicting filenames).

## What it cannot say

It describes the reading, not the transport. An input that ends where a
message could end is `complete` even if it was cut in transit at exactly such
a place. What the bytes do show — an unclosed multipart, an unended line, a
cut Base64 group — is `incomplete`.

The input need not reach `io.EOF` for `complete`: what follows a multipart's
closing delimiter is not read.

## For a spam adapter

Treat anything but `complete` as "not a verdict on the whole message".
`Clean()` is the strict test; `complete` with repairs means the message was
read whole but had to be healed, which is itself a signal.
