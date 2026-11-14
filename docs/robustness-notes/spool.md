# Spooling bodies for repeated reads (ROB-110-002 to -004, -006, -007)

Issues #41, #43. Code: `mail/spool.go`. Tests: `mail/spool_test.go`.

A body is a stream and is read once. `mail.Spool` keeps the decoded bodies of
one message so they can be read again:

```go
spool := mail.NewSpool(&mail.SpoolOptions{MemoryBytes: 4 << 20, DiskBytes: 256 << 20})
defer spool.Close()
tree, err := mail.ReadTree(root, &mail.TreeOptions{Body: spool.Body})
rc, ok, err := spool.Open(tree.Root.Attachments()[0])
```

- Bodies stay in memory until `MemoryBytes` (8 MiB) is spent; a body that
  would pass it goes whole to a temporary file in `Dir`, or to the caller's
  `BlobStore`.
- `DiskBytes` (1 GiB) bounds what is written outside memory. A body that
  would pass it is cut there and the rest is read and dropped; one
  `limit.disk_spool_bytes` diagnostic per such body is in
  `Spool.Diagnostics()`. The tree's `Size` and `SHA256` are still those of the
  whole body.
- `Spool.Open(node)` returns an independent reader each time. `Kept` and
  `Usage` report what is held.
- `Spool.Close()` drops the memory and removes every temporary file or stored
  blob it created, complete or not. It is idempotent and safe in a `defer`
  placed right after `NewSpool`. A storage failure ends `ReadTree` with that
  error; `Close` still removes what was made.

`BlobStore` is three methods — `Create`, `Open`, `Delete` — for a caller with
its own storage.

## The parse's spool budgets (#43, #70)

`Limits.MaxMemorySpoolBytes` and `MaxDiskSpoolBytes` reach the spool through
`ReadTree` (`mail/spool_limits.go`). `ReadTree` reads them from
`e.ParseSession().Limits()` and gives every node of the tree, embedded
messages under `DescendMessages` included, one private budget that counts what
was kept for that tree. `Spool.Body` uses the smaller room of that budget and
of its own `SpoolOptions` quota:

| Parse budget | `SpoolOptions` quota | Applied |
| --- | --- | --- |
| positive (zero resolves to 8 MiB / 1 GiB) | positive (zero is 8 MiB / 1 GiB) | the smaller room left |
| `LimitDisabled` | positive | the spool's quota |
| any | negative | nothing kept there |
| none: `Read`, `ReadWithOptions`, `New`, or a hand-built `Node` | any | the spool's quota, as before |

- The budget is per tree, the spool's quotas per spool: one spool used for two
  messages gives each its own parse budget, and its own quotas count both.
- A body that does not fit in the memory left goes to disk whole. Nothing is
  lost and nothing is reported; `limit.memory_spool_bytes` stays unemitted.
- A body cut on disk records `limit.disk_spool_bytes`; its message names
  `Limits.MaxDiskSpoolBytes` when the parse's budget was the smaller room, and
  the spool's quota otherwise. `Size` and `SHA256` stay those of the whole body.
- What else these budgets do not touch: `Node.Data` is bounded by
  `TreeOptions.MaxBodyBytes`/`MaxTotalBytes`, a recorded `Source` by its
  `RecordOptions`, and the parse retains nothing under them.

## Storage failures

- The writer's `Close` decides whether a blob is kept: a failing `Close` ends
  `ReadTree` even for a body the quota cut or whose read failed (before, the
  error was dropped and the body was reported kept).
- A `Write` that takes fewer bytes than given with no error is
  `io.ErrShortWrite`; one that reports a negative count or more than it was
  given is `invalid write result`. Neither keeps a body with a hole.
- The spool's own quota result is known by identity. An error from the
  caller's writer is wrapped once and never looked into: no `errors.Is`, no
  `Unwrap` (the bounded error graph rule).
- `Close` deletes every blob or file created, including the one being
  written when the failure happened. Pinned by `mail/spool_limits_test.go`.

Digests are computed while the body streams, with no second pass
(`Node.SHA256`, ROB-110-006).

## Limitations

- The spool keeps decoded bodies. Raw, undecoded bytes come from a
  `message.Source`: `ParseAt` reads a seekable input in place, and a
  recorded source (`NewRecordedSource`, `ParseSource`) keeps the stream in
  memory or a caller's `RawStore`, with a private re-parse to index parts not
  yet reached (raw-source.md, ROB-110-001). The spool does not use a source,
  and a source does not keep decoded bodies.
- The parse's budgets apply only through `ReadTree` with `Spool.Body`. A
  caller who feeds `Spool.Body` from its own walk gets `SpoolOptions` alone.
- `Spool.Body` reads up to the memory room plus one byte before it knows
  whether a body needs storage, so when the store fails, that much of the body
  has been read. Diagnostics from that body's decoders then depend on read
  sizes (seed 1300007, iteration 202279; `TestSoak_abandonedSpoolBody`). Read
  to its end, the body records the same diagnostics either way.
- Readers opened before `Close` on a temporary file keep working only as long
  as the operating system keeps an unlinked file readable.
