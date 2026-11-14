# Raw source and indexed mode (ROB-110-001, -004, -005, ROB-080-002)

Code: `source.go`, `source_entity.go`, `mail/rawsource.go`.
Tests: `source_test.go`, `source_header_test.go`, `mail/rawsource_test.go`.

## Model

`Parse` reads a stream once. What it hands out — `Entity.Body`, the parts — is
one-shot, and the untouched bytes of the input are gone unless the caller kept
its own copy and sliced it at `Entity.RawSpan`.

A `message.Source` is that copy made part of the parse: the input, readable at
any offset. A parse given one is still the same streaming parse — same tree,
bodies, diagnostics, errors, budgets — and every entity can in addition hand
out its wire bytes and a fresh decoded body, from the source, beside its
one-shot `Body`.

Two kinds of source:

| | made by | cost | parsed |
| --- | --- | --- | --- |
| seekable | `NewSource(io.ReaderAt, size)` | none: read in place | any number of times |
| recorded | `NewRecordedSource(io.Reader, *RecordOptions)` | the bytes the parse reads, kept as it reads them | once (`ErrSourceUsed` after) |

A recorded source keeps the input in memory, or in the caller's `RawStore`
(`io.Writer` + `io.ReaderAt`; an `*os.File` is one), up to
`RecordOptions.MaxBytes`: 8 MiB (`DefaultRecordBytes`) when zero, no limit only
when the caller writes a negative value. There is no hidden unbounded copy. It
reads nothing by itself: it sees what the parse takes from the stream.

## API

```go
session, root, err := message.ParseAt(file, size, opts)        // seekable
session, root, err := message.ParseSource(ctx, source, opts)   // either kind; ctx may be nil

r, err := entity.RawReader()    // header block + body, as sent
r, err := entity.RawHeader()    // header block, its empty line included
r, err := entity.RawBody()      // body before transfer and charset decoding
r, err := entity.DecodedBody()  // a fresh decoded body, decoded again from RawBody

session.Source()                // the Source, nil for Parse/ParseContext
source.Size()                   // bytes readable, and whether that is the whole input
source.Truncated(), source.Err()
source.ReadAt(p, off)

node.RawSection(src)            // mail.Node: its span of the caller's io.ReaderAt
```

The raw methods return a new `*io.SectionReader` per call. The spans are those
of `Entity.Origin` and `Entity.RawSpan`: a part runs from the first byte after
the delimiter line that introduced it to, and not including, the line ending
before the delimiter line that ended it; the root runs from its header block
(after an mbox envelope line kept apart) to the end of the input, epilogue
included.

Errors, all matched with `errors.Is`:

- `ErrNoRawSource` — the parse had no source (`Parse`, `Read`, `New`).
- `ErrRawSpanUnknown` — the entity's bytes in the input are not known (below).
- `ErrRawNotRecorded` — a recorded source did not keep them.

## Policies

**Read order.** Raw readers read the source, not the entity. They neither
consume nor are disturbed by `Body`, the part iterators, `Walk`, `WriteTo` or
header accessors, and can be obtained before, during and after those, any
number of times. The bytes are the input's whatever the parse healed: a
digest of `RawHeader`/`RawBody` taken before and after every semantic read is
the same, and equals the digest of the input slice (DKIM, `multipart/signed`).

**When a span is known.**

- The root: always with a seekable source; with a recorded one, once the
  stream has been read to its end. `Drain` does not read past a multipart's
  closing delimiter; reading `root.Body` to its end afterwards does.
- A header block: as soon as the entity exists, for the root, a part a
  multipart of the input framed, and a message carried as it is by one of
  those. `RawHeader` needs only the bytes up to the body's first to be
  readable: with a recorded source it is served before, while and after the
  body is read, however far past the record limit (#73). A part of an inferred
  boundary, and a message it carries, are checked against the index and have
  no span.
- A part: once the parse has moved past it (as `RawSpan`). Before that, when
  the whole input is readable — a seekable source, or a recorded one read to
  its end — the first such request parses the source one more time, privately,
  to index where every part ends. That costs one more read of the input, is
  done at most once per parse, keeps a small entry per entity and no body,
  records nothing in the session, and runs under the same options and budgets.
  A caller that reads in document order never pays it.
- A message carried by a `7bit`/`8bit`/`binary` part (`EmbeddedMessage`): its
  header block as above; the whole and its body by the index, so only when the
  whole input is readable. Its parts: like any part.

**Concurrency.** A `Source` and the readers obtained from it are safe to use
from several goroutines at once, each reader by one goroutine (`ReadAt` on a
shared reader is fine), also while the parse goes on and records into the
source. The Entity methods that *return* the readers are, like every Entity
method, for the goroutine that reads the parse. A `RawStore` never sees a
`Write` overlap another call; it may see concurrent `ReadAt`.

**Record limit.** A recorded source keeps a prefix of the input. Past
`MaxBytes`, or after its store failed (`Source.Err`), nothing more is kept;
the parse is not affected and reads on. `Truncated` reports it, spans wholly
inside the prefix are still served, and anything reaching past it returns
`ErrRawNotRecorded`. A header block is judged on its own bytes, not its
body's: a kept header block stays served once its body has run past the
prefix, and one cut by the limit is refused before and after (#73). A store failure is not a defect of the message and
records no diagnostic.

**Decoded again.** `DecodedBody` sets up the transfer decoder and charset
conversion anew over `RawBody`, under the parse's profile, fallback charset,
context and per-body budgets, following the entity's `Header` as it stands at
the call. It delivers what `Body` delivers and leaves `Body` alone. Each
reader has the total decoded budget to itself. Its diagnostics are not
recorded: the session describes the one-shot reading (`Body`, or `Drain`). A
stopped parse returns its stop. For a multipart entity it is the raw body, as
`Body` is.

**One-shot mode is unchanged.** `Parse`, `ParseContext`, `Read`,
`ReadWithOptions` and `New` allocate no source and behave as before; their
entities answer `ErrNoRawSource`.

## Not covered

- No span, so `ErrRawSpanUnknown`: a message embedded in a Base64 or
  quoted-printable part and its parts; parts of a boundary the parser inferred
  (`multipart.boundary_mismatch`, `multipart.boundary_inferred`) and the single
  part of a multipart with no delimiter; parts of a body the caller replaced.
- The index is a second parse, not a table saved from the first; there is no
  on-disk index and no seeking the streaming parse itself to a part.
- A recorded source cannot answer about bytes the parse has not read: the
  root's end needs the stream read to its end, and nothing finishes the stream
  on the caller's behalf (that would read outside the parse's input budget).
- `MaxInputBytes` bounds the parse, not raw reads: a seekable source longer
  than the budget is still readable to its declared size.
- `DecodedBody` re-reads use the header as the caller left it, and their
  diagnostics are dropped.
- `mail.ReadTree` and `mail.Spool` do not use the source themselves;
  `Node.RawSection` only slices the caller's reader at the node's span.
- A seekable source must not change while in use; nothing detects that.

## 2026-11-13 follow-up review (#73)

Codex reviewed the range check and embedded-origin propagation. Independent
public API probes pass the original 512-byte-cap case and whole headers
before and after an 8192-byte cap for root, child and both embedded forms
with one-byte input reads. An inferred outer boundary with a declared inner
boundary also returned the exact original inner header. The core/mail/charset/
utf7/internal race group, full unit suite, build, formatting and diff checks
passed. Missing spans outside this follow-up remain open.
