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

## What Drain returns

- `nil` only when the walk reached the end of the message and the parse was
  not stopped when Drain looked last. A stop that arose while Drain read the
  last body, or remaining carrier bytes read after an interrupted walk, is
  returned, not `nil`.
- the parse's stop — the identical value `ParseStop` and `Completion.Stop`
  report — whenever there is one, even when an error of one entity (a
  ProfileStrict refusal, an iterator's failure) ended the walk first.
- otherwise the error that ended the walk: an iterator's failure, or a
  ProfileStrict refusal. A body that fails on its own (a body budget, a
  decoder) does not end the walk; its diagnostic is in the session.

Called again, Drain reads nothing a first Drain already read, under every
profile: a multipart whose parts were read to their end gets an iterator at
its end, so nothing after its closing delimiter is framed or read. The second
call returns `nil` after `nil`, the identical stop after a stop, and the
identical failure after an iterator that failed for good. A body the caller
replaced is framed anew. Tests: `completion_drain_test.go`.

Not promised: after a walk ended by an error that is not "for good" (for
example a Strict refusal of one part), a second Drain may iterate what that
multipart still holds; what it then returns is not pinned by a test.

## Drain after a partial read

Code: `drain_resume.go`. Tests: `completion_resume_test.go`.

`MultipartReader` used to build a new iterator on every call. A Drain after
the caller had taken a part and read some of it framed the rest of the body
anew, from inside that part, and lost what the first iterator had buffered
and the part it had handed out: Strict returned `NextPart: EOF`, the healing
profiles returned nil with the parse `in-progress` and the remaining parts
never seen.

For an entity from `Parse` whose `Body` is still the parse's:

- `MultipartReader` returns the same iterator until its parts end;
- Drain reads to its end the part that iterator last returned (its body, its
  own parts, its embedded message under `WalkEmbeddedMessages`), then lets
  the iterator go on; a Drain after any partial reading by `MultipartReader`,
  `Walk` or an earlier Drain reads the rest once, in order, with each part's
  own path, diagnostics and raw span;
- `Walk` resumes an iterator from where it stands and numbers its parts with
  their own index; it goes past the part last returned, as `NextPart` does;
- `Walk` (and so Drain) under `WalkEmbeddedMessages` reads what an embedded
  message left of its carrier before going on, under every profile, once the
  message was read to its end: its parts walked, or its body read by walkFunc
  to the carrier's end or to an error of its own. A walkFunc that skipped the
  message leaves the carrier unread, and it is abandoned (below).

Soak iteration 114063 (seed 1300007, 2026-11-14; fixture
`mail/testdata/soak/walk-carrier-rest-after-cut-leaf.eml`) showed why: a
carrier's embedded leaf ended early on a line budget, the walk read the rest
only after an embedded multipart and only under recovery, and the 7bit
carrier's reader returns its end a read later after an overlong line, so no
read-ahead reached it either. The multipart went past the carrier, which was
abandoned, and a walk consumer that read every leaf ended `in-progress`. A
ProfileStrict walk did the same for any carrier with more left than one
read-ahead. Tests: `TestWalkCarrierRest` (read, drained and skipped, every
profile, whole and in 7- and 64-byte reads), `TestSoak_walkCarrierRest`.

A part the iterator went past before it was read to its end was discarded,
as `NextPart` documents. Reading it afterwards returns what it returns
(typically nothing), but it, and everything inside it — parts a nested
iterator still frames from its own buffer, an embedded message — is
abandoned and never counted read, so the parse stays `in-progress` whatever
is read later. Before this, a skipped body read afterwards returned EOF and
counted as read, and the parse could end `complete`.

State kept: one iterator per multipart still being iterated and one part per
iterator, the path being read, bounded by `MaxMIMEDepth`; dropped when the
parts end.

Unchanged: a `Body` the caller replaced is framed anew on every call and
never kept, and the part the iterator over the original body handed out is
neither finished nor abandoned, so it stays unread (`in-progress`) unless the
caller reads it. `Read`, `ReadWithOptions` and `New` keep no iterator.

## Completion states

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
Content-Type or a parameter, an unselected multipart Content-Type whose
boundary delimits the body (`header.unselected_multipart`), conflicting
filenames).

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

## Independent review (2026-11-14)

Codex reviewed iterator ownership, active-child resumption, skipped-part accounting, stop precedence, legacy/replaced-body behavior and raw-span tests. Independent public API probes cover stop/repeated Drain in three profiles and the partial-first-part failure; all pass, with the latter ending complete at 2/2 bodies instead of losing the second part. Focused Drain/Completion tests pass. Claude's fresh full unit suite, build, vet, core/mail race and 20,000-iteration soak passed after the implementation changes. The separate preexisting spool soak failure at seed 1300007 / iteration 202279 remains tracked in soak.md until its spool review.
