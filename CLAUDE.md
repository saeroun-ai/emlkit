# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

`github.com/saeroun-ai/emlkit` is a Go library for the Internet Message Format (email and MIME). It implements RFC 5322 (message format), RFC 2045–2047 (MIME), and RFC 2183 (Content-Disposition). The API is streaming-first (everything is built on `io.Reader`/`io.Writer`) and "DKIM-friendly": parsed messages can be re-serialized byte-for-byte verbatim.

Module: Go 1.26+, single external dependency `golang.org/x/text`.

## Commands

```bash
go build -v ./...                                          # build all packages
go test ./...                                              # run all tests
go test ./mail/                                            # test one package
go test -run TestName ./...                                # run a single test by name
go test -bench=. ./textproto/                             # run benchmarks (bench_test.go lives here)
go test -coverprofile=coverage.txt -covermode=atomic ./... # coverage profile
gofmt -l .                                                 # list unformatted files; must be empty before committing
gofmt -w .                                                 # format in place
```

A CI workflow exists in `.github/workflows/ci.yml` (gofmt, build, vet, test, race, fuzz smoke) and runs on every push and pull request; its first run, on 2026-11-11, was green. It does not replace local verification: before committing, run `go build ./...`, `go test ./...`, and `gofmt -l .` (its output must be empty: `test -z $(gofmt -l .)`). `go vet` is clean only with `-composites=false -stdmethods=false`.

## Host-safe verification

All local Go builds, tests, fuzzing, benchmarks, and parser QA programs must run
inside a bounded process group. On this Linux host use:

```bash
rtk proxy python3 scripts/qa-bounded.py --seconds 180 -- go test -timeout 120s ./...
rtk proxy python3 scripts/qa-bounded.py --seconds 600 -- go test -race -timeout 480s . ./mail/ ./charset/ ./utf7/ ./internal/...
rtk proxy python3 scripts/qa-bounded.py --seconds 600 -- go test -race -timeout 480s ./textproto/
rtk proxy python3 scripts/qa-bounded.py --seconds 400 -- go test -race -timeout 360s ./tnef/ ./smime/ ./pgpmime/ ./icalendar/ ./appledouble/ ./legacyenc/
rtk proxy python3 scripts/qa-bounded.py --seconds 120 -- go test -run '^$' -fuzz '^TARGET$' -fuzztime 30s -parallel 2 .
```

Run the race detector in those three groups, not as `./...`: building every
race test binary at once passes the 1.5 GiB limit and is OOM-killed during the
build, before any test runs (seen 2026-11-11).

The runner requires an available systemd user manager. It fails closed if the
service or limits cannot be established: 1.5 GiB RAM, no swap, CPU quota of two
cores, 128 tasks, a runtime deadline, and whole-group termination on OOM.
`GOMAXPROCS=2`, `GOMEMLIMIT=512MiB`, and `GOFLAGS=-p=2` apply inside the service.
Run one QA job at a time; do not start parallel bounded services to multiply the
host load. Do not rerun a memory-killed test without investigating and bounding
its input first. On another host, establish and verify equivalent enforced
limits before execution; do not silently fall back to an unbounded run.

Every endless/adversarial test source must have a finite byte/work cap and every
hang-prone probe a subprocess deadline. A Go test timeout alone does not bound
memory. Do not run mutation tests that disable guards against an unbounded
source, and never leave a deliberately disabled guard in the shared working
tree. Use exit statuses directly; piping tests through `tail`/`grep` must not
hide failure. No-tests-to-run output is not validation.

This guard was added after an unbounded preflight mutation test caused a kernel
OOM on 2026-11-09. It supports local QA and is not a claim that CI (#14) is done.

## Architecture

The library is layered. Each layer adds semantics on top of the one below; pick the lowest layer that does what you need.

### `textproto/` — wire format only (bottom layer)
Pure RFC 5322 / MIME wire format with **no** decoding of values. `ReadHeader`/`WriteHeader`, `Header`, and the `MultipartReader`/`MultipartWriter` live here. This layer is what enables verbatim round-tripping:

- `headerField` stores the **original raw bytes** (`b []byte`, including whitespace/folding) alongside the parsed key/value. `raw()` returns those bytes unchanged if the field was never modified, and `WriteHeader` re-emits them — so a parsed-then-written header is byte-identical (critical for DKIM signatures). Only mutated/synthesized fields get reformatted via `formatHeaderField` (line folding).
- `Header.l` stores fields **in reverse order** so prepending (the common case when adding received/trace headers) is cheap. `WriteHeader` iterates back-to-front to restore document order.
- `Header.AddRaw([]byte)` injects a fully pre-formatted field (including trailing CRLF); `Add(k, v)` formats and folds for you.

### root package `message` — MIME semantics (middle layer)
Wraps `textproto` to add transfer-encoding and charset handling. Core type is `Entity` (`Header` + decoded `Body io.Reader`).

- `Read`/`ReadWithOptions` parse only the header eagerly; the body stays a lazy stream. **An `Entity` can be consumed only once** — reading its `Body` (or calling `Walk`) exhausts it.
- `New` auto-decodes the body to UTF-8: applies the `Content-Transfer-Encoding` reader (`encoding.go`) then, for `text/*`, the charset reader (`charset.go`).
- Multipart is a recursive tree: `Entity.MultipartReader()` yields child `*Entity`s; `Walk` traverses the whole tree depth-first. `NewMultipart`/`Writer.CreatePart` build trees for writing.
- `charset.go` exposes the `CharsetReader` hook (see charset layer); `encoding.go` implements quoted-printable / base64 / 7bit / 8bit / binary plus the `lineWrapper` (wraps at 76 cols for base64, 998 for 7/8bit) and `whitespaceReplacingReader` (tolerates non-spec indented base64).

### `mail/` — mail convenience API (top layer)
Models a message as **one-or-more text parts + zero-or-more attachments**, hiding the multipart tree. `Reader.NextPart` flattens nested multiparts and classifies each leaf as `*InlineHeader` or `*AttachmentHeader` (based on Content-Disposition / content type). `Writer` builds the corresponding `multipart/mixed` → `multipart/alternative` structure and auto-sets sensible `Content-Transfer-Encoding` (quoted-printable for text, base64 otherwise). `header.go` adds typed accessors (addresses via `net/mail`, `Date`, `Message-Id`, etc.).

### `charset/` — optional charset plugin
Importing `_ "github.com/saeroun-ai/emlkit/charset"` (side-effect only) sets `message.CharsetReader` to decode ~all common charsets via `golang.org/x/text`. Adds ~1 MiB to binaries, so it's opt-in. Without it, non-UTF-8 charsets produce `IsUnknownCharset` errors. The package also carries a quirks table for charsets `ianaindex` doesn't handle.

### Optional modules
`tnef/` (winmail.dat), `smime/` (S/MIME structure, opaque signed content, verification), `pgpmime/` (PGP/MIME structure, armor, packets), `icalendar/`, `appledouble/` and `legacyenc/` (BinHex, yEnc, inline uuencode) are standalone packages with their own `Issue` and `Limits` types. The core never imports them and never calls them on its own; a consumer feeds them the bytes of a part (`mail.Node.Data`, or the raw span for signatures). Each has a note under `docs/robustness-notes/`. Keep them free of imports from the core outside tests.

### Healing under `Parse`
`Parse` (every profile but `ProfileStrict`) heals damaged mail and reports each repair as a `Diagnostic`; `Read`, `ReadWithOptions`, `New` and `ProfileStrict` keep the earlier strict behavior, so both paths must be kept working. What is healed, and the policy for each case, is written per area in `docs/robustness-notes/` — read the relevant note before changing header recovery (`textproto/recover.go`, `header_recover.go`), multipart recovery (`multipart_heal.go`), transfer decoding (`cte_tolerant.go`), RFC 2047 (`rfc2047.go`), MIME parameters (`mediatype_tolerant.go`, `rfc2231.go`), embedded messages (`embedded.go`), completion (`completion.go`) or the mail views (`mail/tree.go`, `mail/spool.go`, `mail/*_tolerant.go`). Opt-in checks: `EMLKIT_SOAK=30s` (seeded soak, `mail/soak_test.go`), `EMLKIT_DIFFERENTIAL=1` (comparison with Python's `email`, `mail/differential_test.go`), `EMLKIT_UPDATE_CORPUS=1` (rewrites `testdata/corpus` expectations for review). `docs/robustness-roadmap.md` is the ledger of items and their status; `docs/spam-adapter-contract.md` says what a consumer may rely on.

Healing policy (#62): recovery makes damaged mail readable; it does not establish the sender's intent. When the bytes establish one reading, recover it and report the repair. When multiple readings remain plausible, keep the declared reading and report the conflicting evidence; alternative semantic decoding must be explicitly enabled by the caller (for example `RecoverCharsetDeclarations`). Preserve original bytes in the raw APIs, and put inferred values in separately named guess fields rather than declared-value fields. Unfolding must preserve the whitespace that is part of the field value.

Two existing structural recoveries remain deliberate exceptions under healing profiles: boundary inference only under the evidence and bounded search in `docs/robustness-notes/multipart-recovery.md`, and unindented continuation joining only under the grammar in `docs/robustness-notes/header-recovery.md`. They restore structure present in the bytes, retain the original header, and report the choice; neither proves sender intent. Inferred multipart nesting stays capped at eight, and duplicate Content-Type selection stays first-wins. Do not broaden these exceptions without documenting the evidence and ambiguity.

## Conventions specific to this codebase

- **Error-with-usable-result pattern**: `Read`, `New`, `mail.CreateReader`, etc. may return a top-level `UnknownCharsetError` or `UnknownEncodingError` together with a usable result. Keep that result and its undecoded body. A `FatalError` wrapping a warning is still a stop; the public predicates search graphs and may report multiple classes. Internal warning decisions require a non-nil result and the actual top-level warning, and session stops take precedence.
- **Error classification**: `IsFatal`, `IsUnknownCharset` and `IsUnknownEncoding` use `internal/errgraph`, not `errors.As`. The warning predicates match the exact value type with a non-nil private cause; `IsFatal` matches a non-nil `*fatalParseError`, whose cause may be nil. The bounded walker follows `%w`, joins and custom `Unwrap`, with node/pending/child-slot caps, without calling `Is`, `As`, `Error` or `Format`. Other internal decisions use known top-level types or reader/session identity. The deliberate exception is textproto's EOF classification: it follows foreign `Unwrap` within these bounds to find `io.EOF`/`io.ErrUnexpectedEOF`. This limits calls, not execution inside a caller's method; formatting an error elsewhere has its own behavior. Use `errgraph.Find` for new graph classifications, never unbounded `errors.Is`/`errors.As` on errors supplied by a reader, charset hook or sink.
- **Charset asymmetry**: non-UTF-8 charsets are decoded on read, but **writing only permits utf-8 / us-ascii** (`createWriter` rejects anything else).
- **Sealed interfaces**: `message.HeaderFields`, `textproto.HeaderFields`, and `mail.PartHeader` have unexported marker methods (`headerFields()`, `partHeader()`) so they can't be implemented outside the module. Don't try to satisfy them externally; new concrete types belong inside the module.
- **Copy-on-write for headers**: writer constructors call `header.Copy()` before mutating, so a caller's `Header` is never modified as a side effect. Preserve this when adding new writers.
- **Verbatim round-trip is a tested invariant** (see `message` test "reformatted verbatim", #187). Changes to header parsing/formatting must not break byte-exact re-serialization of unmodified fields.
