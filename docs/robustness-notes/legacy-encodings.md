# Legacy inline encodings: BinHex, yEnc, uuencode, xxencode (ROB-140-006)

Code: `legacyenc/` (optional; nothing in the core imports it). Tests:
`legacyenc/legacyenc_test.go`, `legacyenc/fuzz_test.go` (`FuzzDecode`).

## Evidence

ROB-140-006 asks for BinHex and yEnc to be evaluated against measured corpus
demand. This repository has no real mail to measure. `testdata/corpus/` holds
30 fixtures, and its manifest says that every one is synthetic and "none is
derived from real mail". Each fixture was written for a defect the core heals.
Only one of them involves a legacy encoding: `uuencode-missing-end.eml`, whose
header *declares* `Content-Transfer-Encoding: x-uuencode`. No fixture holds
BinHex, yEnc or xxencode, and none holds a uuencoded block inside an ordinary
text body. Demand is therefore **unmeasured**, which is not the same as
measured to be zero. The decision below is meant to be revisited by the
counting procedure further down.

## The formats

| Format | What it looks like | Where it still appears |
|---|---|---|
| uuencode | `begin 644 name`; lines that start with a length character (`M` = 45 bytes) followed by 4 characters per 3 bytes from `' '`..`` '`' ``; a `` ` `` line; `end` | pre-MIME mail and Usenet; scripts (`uuencode f f \| mail`); old and bulk mailers; malware that puts an executable inside `text/plain`, where a scanner that only opens attachments does not look |
| `uuencode -m` | `begin-base64 644 name`, Base64 lines, `====` | GNU sharutils; rare |
| xxencode | uuencode's framing with the alphabet `+-0-9A-Za-z` (`h` = 45) | gateways that mangled uuencode's punctuation (EBCDIC); very rare |
| yEnc | `=ybegin line= size= name=`, an optional `=ypart begin= end=`, each byte +42 mod 256 with `=` escaping the critical ones, `=yend size= [part=] [pcrc32=] crc32=` | Usenet binaries, mail-to-news gateways, forwarded posts; it has no MIME label and travels in `text/plain` or `application/octet-stream` |
| BinHex 4.0 | RFC 1741, `application/mac-binhex40`. A banner `(This file must be converted with BinHex 4.0)`, then 6-bit characters between `:` and `:`. Inside: 0x90 run-length coding, then a header (name, type, creator, flags, fork lengths, CRC-16), the data fork and its CRC, the resource fork and its CRC | classic Mac OS mail (Eudora, Mac mail sent to non-Mac readers); archives of old mail; rare in current traffic |

## What the core does today

- A body that declares `x-uuencode`, or one of the aliases `canonicalCTE`
  accepts (`uuencode`, `x-uue`, `uue`, …), is decoded by `tolerantUU` in
  `cte_tolerant.go` (ROB-070-009): lines before `begin` are skipped, a missing
  `begin` or `end` is healed with a diagnostic, and short lines are padded.
  `begin-base64` framing is not recognized there.
- `application/mac-binhex40` is an ordinary attachment. Its 7bit body is
  returned as it is; nothing decodes it.
- yEnc has no transfer-encoding name. A yEnc block is whatever its part
  declares, usually `text/plain`, and is returned as text.
- Nothing looks inside a text body. To `Parse`, `mail.Reader` and
  `mail.ReadTree`, a uuencoded, xxencoded, yEnc or BinHex block inside
  `text/plain` is text. Its file is not counted as an attachment.

## Decision

1. **The core never decodes these formats on its own** and does not scan text
   bodies for them. There is no measured demand. Decoding inside a text body
   would change what "the body" is for every consumer, detection in prose can
   misfire, and BinHex's run-length stage expands 3 bytes into up to 255.
   For a spam classifier, knowing that a block is present already carries most
   of the signal.
2. **Detection is offered as a cheap signal.** `legacyenc.Sniff` reads the
   first 64 KiB and decodes nothing. `legacyenc.Scan` returns every block with
   its byte range, declared name, mode and size.
3. **Decoding is opt-in**, through `legacyenc.Decode` and `DecodeAll`, under
   budgets, and every repair is reported as an `Issue`.
4. The declared `x-uuencode` transfer encoding stays in the core as it is. It
   is a MIME declaration, not a guess.

## The package

```go
func Sniff(prefix []byte) Kind                      // first block in the first 64 KiB, or KindNone
func Scan(body []byte, lim *Limits) ([]Block, []Issue)
func Decode(body []byte, b Block, lim *Limits) Result
func DecodeAll(body []byte, lim *Limits) ([]Result, []Issue)
```

`Kind` is `KindUUEncode`, `KindXXEncode`, `KindUUBase64`, `KindYEnc` or
`KindBinHex`.

A `Block` holds:

- `Start` and `End`: the byte range in the body, from the opening line through
  the closing line and its line ending.
- `Quote`: the quoting prefix stripped from each line.
- `Name`, `Mode`, `Size`: the declared file name, the octal mode, and the
  declared decoded size.
- `Complete`, and the scan `Issues`.
- `YEnc`: every `=ybegin`/`=ypart`/`=yend` field.
- `BinHex`: the decoded header. Scan decodes only that much of the block.

A `Result` holds `Data` (for BinHex, the data fork), `Resource`, `Truncated`,
and the decoding `Issues`.

**Names, modes, sizes, CRCs, types and creators are all input and are
untrusted.** A name is not a safe path; use `message.SafeFilename`, or treat
it as a label.

### What opens a block

Each rule applies to a line after its quoting prefix, which is one or more `>`
characters with optional spaces between them (`> `, `>> `, `> > `). Lines may
end in CRLF, LF or CR.

| Kind | Opening line | Confirmed by | Closing line |
|---|---|---|---|
| BinHex | the banner, case-insensitive | the next non-blank line (at most 3 skipped) starts with `:` | the second `:` |
| yEnc | `=ybegin ` with a `size=` or `name=` field | — | `=yend` |
| begin-base64 | `begin-base64 MODE [NAME]`, MODE 1–6 octal digits | the next non-blank line is Base64 whose length is a multiple of 4 | `====` |
| uuencode / xxencode | `begin MODE [NAME]` | the next non-blank line is a *whole* uuencoded or xxencoded line, which also decides the kind | `end` |

Prose such as `begin the day`, `begin 2024 kickoff` followed by a sentence,
`=ybegin is …`, or the BinHex banner without data does not open a block.
Every line of a block must carry the opening line's quote, or the block ends
there.

A line breaks a block when it does not fit the format: a character outside
the alphabet, or a uu/xx length character that is not valid. A line that fits
the width of a whole line is the exception, since one character may have been
damaged in transit. Such a line is decoded, and the bad character is reported.
A uu/xx block that has no `end` line ends at its last *whole* line, so a
following signature such as `-- ` is not decoded as data. A yEnc block that
has no `=yend` runs until the quote changes or a new `=ybegin` starts.

### Decoders

- **uuencode / xxencode.** A line is decoded by its length character. A short
  line is padded with zeros (`short_line`). Characters past the length are
  ignored (`line_junk`). An invalid character is read as zero.
- **begin-base64.** Characters outside the alphabet are skipped. `=` ends a
  group. A group of one character holds no byte (`truncated`).
- **yEnc.** Line endings are ignored. `=` escapes the next character (minus
  64, then minus 42). The decoded length is checked against `=yend size`,
  against the `=ypart` range, and against `=ybegin size` for a single-part
  block. CRC-32 is checked against `pcrc32`, and against `crc32` when the
  block is the whole file. A mismatch is an issue and the data is still
  returned. A multi-part file reports `Part`/`Total` and its range; joining
  the parts is left to the caller.
- **BinHex.** Decoding has three stages:
  1. 6-bit decoding.
  2. Run-length expansion under the block's budget. A bomb stops at
     `MaxBlockBytes` with `budget`.
  3. Header, data fork, data CRC, resource fork, resource CRC. Each fork
     starts with a capacity no larger than the declared length, the remaining
     budget, and the characters left. CRC-16-CCITT is computed as XMODEM does
     it (polynomial 0x1021, initial 0, `binascii.crc_hqx`). Each mismatch is
     an issue.

  Input that ends early returns what was decoded, with `truncated` naming the
  section it ended in.

### Limits (a zero or negative field means the default)

| Field | Default | Bounds |
|---|---|---|
| `MaxBlocks` | 32 | blocks reported per body (`block_limit`) |
| `MaxScanBytes` | 16 MiB | bytes of the body scanned (`scan_limit`) |
| `MaxLineBytes` | 8 KiB | bytes of one line read (`line_too_long`) |
| `MaxBlockBytes` | 8 MiB | decoded bytes per block, both forks together (`budget`) |
| `MaxTotalBytes` | 32 MiB | decoded bytes per body in `DecodeAll` (`total_limit`) |

Scan is linear in the bytes it scans; every line is examined a bounded number
of times. `TestAdversarialLinear` scans 1 MiB bodies of unconfirmed openers
and one-line blocks in milliseconds. Each decoder is linear in its block.

### Issues

`missing_end`, `line_too_long`, `scan_limit`, `block_limit`, `budget`,
`total_limit`, `truncated`, `size_mismatch`, `crc_mismatch`, `invalid_char`,
`invalid_line`, `short_line`, `line_junk`, `bad_header`, `part_range`,
`dangling_escape`, `bad_run`, `trailing_data`.

Each code is reported once per block, at its first offset. CRC and size
mismatches are the exception: they are reported once for each value checked.
These are not `message.Diagnostic`s; the package has its own small type so
that it stays apart from the core.

## Using it on a text body

```go
_, e, err := message.Parse(r, nil)
if e == nil {
	return err
}
tree, _ := mail.ReadTree(e, nil)
tree.Root.Walk(func(n *mail.Node) bool {
	if n.MediaType != "text/plain" || len(n.Data) == 0 {
		return true
	}
	results, _ := legacyenc.DecodeAll(n.Data, nil)
	for _, r := range results {
		// r.Block.Kind, r.Block.Name (untrusted), r.Data, r.Resource,
		// r.Truncated, r.Block.Issues and r.Issues.
		_ = r
	}
	return true
})
```

`legacyenc.Sniff(n.Data) != legacyenc.KindNone` is the cheaper gate when only
the signal is wanted. It sees only the first 64 KiB.

Two things about `Node.Data` matter here:

- It is cut at `TreeOptions.MaxBodyBytes`. When `n.Truncated` is set, a block
  may end in `missing_end`.
- Its transfer encoding has been undone, and so has its charset unless the
  charset is utf-8, us-ascii or undeclared. uuencode, xxencode, Base64 and
  BinHex are ASCII and survive that conversion. yEnc is 8-bit and does not: a
  yEnc block in a part declared, for example, `iso-8859-1` (with the charset
  plugin or the opt-in fallback) has its high bytes rewritten, and the decoder
  then reports `crc_mismatch` / `size_mismatch`. A consumer that cares should
  decode yEnc from the transfer-decoded bytes before any charset conversion.

## What would change the decision

The decision rests on missing demand. These would change it:

- **Prevalence.** Text leaves carrying a block in more than about 0.1% of a
  deployment's messages, or in more than about 1% of its spam.
- **Verdict impact.** Blocks whose decoded content carries what the classifier
  acts on in attachments: an executable or archive magic (`MZ`, `PK\x03\x04`,
  `\x7fELF`), a macro document, or a URL. Spam that evades the classifier
  through such blocks also counts.

If either is shown, the next step is a `mail.ReadTree` option that runs `Scan`
on text leaves and reports each block as an Info-level structure diagnostic,
with an opt-in decoded view beside the attachments. The core would still
never rewrite the body.

### Measuring

Run this over a sample of real mail that keeps the spam/ham label, for
example a week of quarantine plus an equal sample of delivered mail:

1. Parse each message with `message.Parse` and read it with `mail.ReadTree`
   (default budgets).
2. For each leaf whose `MediaType` starts with `text/`, record `Sniff(n.Data)`.
   That is constant work per leaf.
3. For each leaf where Sniff found a block, run `Scan(n.Data, nil)` and record
   per block: kind, `Complete`, the issue codes, and the declared name's
   extension. Also record whether the leaf is `application/mac-binhex40` or
   `application/octet-stream`.
4. For a 1-in-100 subsample of those blocks, `Decode` and record the first 8
   bytes of `Data` (magic) and its size. Do not keep the payload.
5. Report, per kind and per label: messages with at least one block, blocks
   per message, the share complete, the top magic values, and the top
   extensions.

Count messages, not blocks, for prevalence, and keep the two labels apart.
Without that the counts cannot inform the decision.

## Fixtures

All are built in `legacyenc_test.go` by small encoders: uuencode (modern `` ` ``
and old space-for-zero), xxencode, `begin-base64`, yEnc single and multi-part
(with escapes of NUL, LF, CR, `=`, and of a leading `.`, space or tab), and
BinHex with a run-length coder. The payload has every byte value, a run of
zeros and a run of 0x90, and is 345 bytes, so its last uuencoded line starts
with `>`.

- Round trip: each kind × LF, CRLF, CR, `> `, `> > `, `>> ` with CRLF. The
  checks are the range, kind, name, mode, size and data, with no issues, and
  Sniff giving the same kind.
- yEnc: wrong `crc32`, wrong `pcrc32`, wrong `=yend size`, a field that does
  not parse, a missing `=yend`, a bad `=ypart` range, part over total, a part
  range that disagrees with the data, a dangling escape.
- BinHex:
  - wrong header, data fork and resource fork CRC;
  - a declared data length past the data;
  - a missing closing colon;
  - a damaged character on a full-width line;
  - a run of an escaped 0x90;
  - a run marker with no byte before it;
  - data after the last CRC;
  - a run-length bomb: about 10 KB of characters that expand to 1 MB, stopped
    at a 64 KiB budget.
- uuencode / xxencode / base64:
  - missing `end`, `====` or the closing line, including before a signature;
  - stripped trailing spaces;
  - a checksum character on each line;
  - a damaged character.
- Every fixture cut at every byte offset: the result is either exact or has an
  issue.
- Two blocks in one body among prose. Prose that looks similar is not a
  block. The limits: `MaxBlocks`, `MaxScanBytes`, `MaxLineBytes`,
  `MaxBlockBytes`, `MaxTotalBytes`. The Sniff window.
- `FuzzDecode`: bodies capped at 64 KiB. It checks that there is no panic,
  that blocks are inside the body and in order, that no block exceeds its
  budget and no body its total, and that Sniff agrees with Scan.

## Limitations

- BinHex is found only by its banner. Bare `:`-framed data, BinHex 5.0,
  MacBinary and AppleSingle/AppleDouble (ROB-140-002) are not handled.
- Quoted blocks that a mail client re-wrapped, or whose quote depth changes
  inside the block, end at the first changed line.
- A yEnc block that has no `=yend` decodes the prose after it until the quote
  changes or the body ends. The size and CRC checks are what tell.
- The `begin` line must start the line, after its quote. `  begin 644 x`
  indented by spaces is not found.
- Multi-part yEnc or uuencode files spread over several messages are reported
  per part and not joined.
- No real-mail fixture exists. The fixtures are synthetic, like the rest of
  `testdata/corpus/`.
