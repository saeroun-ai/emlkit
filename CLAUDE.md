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

This project has no CI configured — verification is manual. Before committing, run `go build ./...`, `go test ./...`, and `gofmt -l .` (its output must be empty: `test -z $(gofmt -l .)`).

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

## Conventions specific to this codebase

- **Error-with-usable-result pattern**: `Read`, `New`, `mail.CreateReader`, etc. may return a non-nil error that satisfies `IsUnknownCharset(err)` or `IsUnknownEncoding(err)` **together with** a fully usable `Entity`/`Reader`. Callers should treat these predicates as warnings (the raw body is still readable), not fatal errors — bail only on other errors. The predicates use `errors.As` against `UnknownCharsetError`/`UnknownEncodingError`.
- **Charset asymmetry**: non-UTF-8 charsets are decoded on read, but **writing only permits utf-8 / us-ascii** (`createWriter` rejects anything else).
- **Sealed interfaces**: `message.HeaderFields`, `textproto.HeaderFields`, and `mail.PartHeader` have unexported marker methods (`headerFields()`, `partHeader()`) so they can't be implemented outside the module. Don't try to satisfy them externally; new concrete types belong inside the module.
- **Copy-on-write for headers**: writer constructors call `header.Copy()` before mutating, so a caller's `Header` is never modified as a side effect. Preserve this when adding new writers.
- **Verbatim round-trip is a tested invariant** (see `message` test "reformatted verbatim", #187). Changes to header parsing/formatting must not break byte-exact re-serialization of unmodified fields.
