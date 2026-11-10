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

Digests are computed while the body streams, with no second pass
(`Node.SHA256`, ROB-110-006).

## Limitations

- The spool keeps decoded bodies. Raw, undecoded bytes are reached through
  `Entity.RawSpan` and the caller's own copy of the input; there is no raw
  spool and no indexed re-parse (ROB-110-001 open).
- `Limits.MaxMemorySpoolBytes` and `MaxDiskSpoolBytes` of the parse are not
  read by the spool; it has its own options.
- Readers opened before `Close` on a temporary file keep working only as long
  as the operating system keeps an unlinked file readable.
