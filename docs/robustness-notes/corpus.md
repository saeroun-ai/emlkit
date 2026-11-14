# Regression corpus (ROB-130-003)

Issue #11. Fixtures: `testdata/corpus/*.eml`. Expectations:
`testdata/corpus/manifest.json` (no charset plugin) and
`testdata/corpus/plugin.json` (with it). Tests: `TestCorpus` in
`corpus_test.go`, `TestCorpusWithPlugin` in `charset/corpus_test.go`.
Generator: `scripts/gen-corpus-fixtures.py`.

Every fixture is synthetic: invented names (홍길동, 김철수, 山田太郎, Alice
Example …), `example.*` domains, documentation address ranges
(192.0.2.0/24, 198.51.100.0/24, 2001:db8::/32), placeholder attachment bytes
behind the right magic number. None is copied from real mail; the
sender-shaped ones model the *structure* a client or provider produces.

## Categories

Each manifest entry names one:

| Category | Meaning | Count |
|---|---|---|
| `conformant` | Valid MIME, including legacy charsets declared correctly, RFC 2231, reports and signed structures | 12 |
| `malformed` | A defect a real sender produces: wrong or missing declarations, damaged parameters, encodings or framing | 33 |
| `truncated` | The input ends early: inside a header, a base64 group, before a closing delimiter | 5 |
| `adversarial` | Built to mislead a reader: hidden text, lookalike delimiters, reused boundaries, conflicting names, oversized fields | 6 |
| `provider-style` | The shape a named kind of sender writes: Gmail, Outlook/Exchange, Apple Mail, a mobile client, Korean Outlook and webmail, list managers, calendar invitations | 9 |

The first 30 fixtures (one per defect, hand-written) predate the generator;
the 35 sender-shaped ones (`provider-*`, `ko-*`, `ja-*`, `zh-*`, `bulk-*`,
`report-*`, `signed-*`, `list-*`, `cut-*` …) are built by it.

## What is recorded

For each fixture, parsed with `Parse(r, &ParseOptions{WalkEmbeddedMessages:
true})` and walked to the end:

- `state` — `Completion().State`;
- `diagnostics` — every session diagnostic, `severity code path`, in order;
- `leaves` — every leaf: path, effective media type, decoded size;
- `headers` — Subject and From through `Header.TextDiagnostics`: decoded
  size, the first 60 characters, and the repairs it reports;
- `text` — the first 60 characters of every `text/*` and `message/*` leaf,
  white space collapsed, invalid UTF-8 shown as `\xNN`;
- `names` — the first `FilenameCandidates` entry of every leaf that names a
  file, with `undecoded` when its bytes are not UTF-8. Asking for it reads the
  Content-Disposition, so its parameter diagnostics are in `diagnostics`;
- `byteAtATime` — only where reading the input one byte at a time gives a
  different outcome. Every such entry is a finding below.

### The charset plugin

The root package cannot import `charset` (the plugin imports the root), so
`TestCorpus` runs with no `CharsetReader`. In `manifest.json` every body in
a charset other than UTF-8 and US-ASCII is therefore delivered undecoded
with `charset.unknown` (Unsupported), every such encoded word is left as
written with `rfc2047.unknown_charset`, and RFC 2231 or raw 8-bit names stay
`undecoded`. Setting the hook globally in the root test binary would change
what other root tests observe, so it is not done there.

`TestCorpusWithPlugin` reads the same fixtures in the charset package with
the plugin imported and `FallbackCharset: "euc-kr"` for bodies and for
Subject/From — the configuration of a deployment reading Korean mail.
`plugin.json` lists only the fixtures whose outcome differs from
`manifest.json`; all others must come out identical. This is where EUC-KR,
ks_c_5601-1987, CP949, ISO-2022-JP, Shift_JIS, GBK, GB18030 and Big5
decoding is pinned. Its outcome code is a copy of the root one; keep the two
in step.

## Adding a fixture

1. Add a builder to `scripts/gen-corpus-fixtures.py` with
   `@fixture(name, category, description)`. Build bytes explicitly: `msg()`
   joins lines with CRLF (or `eol=`), `ksx1001()`/`cp949_extension()` encode
   Korean on purpose, `b64()`, `qp()`, `ew_b()`, `ew_q()`, `pct()` encode.
   Keep it to a few KiB (the script refuses more than 8 KiB). A one-off
   hand-written `.eml` is also fine; then add its manifest entry (name,
   category, description) by hand.
2. `timeout 60 python3 -I scripts/gen-corpus-fixtures.py` writes the listed
   `.eml` files and the name, category and description of their manifest
   entries, and nothing else. `--check` writes nothing and exits 1 when a file
   differs: the fixtures are reproducible from the script.
3. Record the outcome, root first, under the bounded runner of CLAUDE.md:
   `-- env EMLKIT_UPDATE_CORPUS=1 go test -run '^TestCorpus$' .`, then
   `-- env EMLKIT_UPDATE_CORPUS=1 go test -run '^TestCorpusWithPlugin$' ./charset/`.
4. Read the diff of both JSON files. Check that the content is all there,
   that legacy charsets decode in `plugin.json`, and that each diagnostic is
   the one the defect deserves. When it is not, keep the fixture and the
   recorded outcome and add a finding below; the corpus records behavior, it
   does not decide it.

Removing or renaming a generated fixture leaves its old `.eml` and manifest
entry behind; delete both. `TestCorpus` fails while the counts differ.

## Findings

Outcomes that look wrong or debatable, as recorded on 2026-11-11. An item
marked **Closed** (or "closed") was changed afterwards, and says what is
reported now. No fixture records a `byteAtATime` difference any more, and
`TestChunkInvariance_fixtures` holds every fixture to one outcome for four
ways of delivering it.

1. **forwarded-rfc822-attachment** — closed. Completion depended on delivery:
   read whole the state was `complete`, read a byte at a time `in-progress`.
   A message/rfc822 carrying a multipart, walked with `WalkEmbeddedMessages`:
   the carrier's bytes after the embedded closing delimiter counted as read
   only when a buffer had fetched them. `Walk` now reads what an embedded
   multipart left of its carrier before it goes on (embedded-messages.md);
   the state is `complete` either way.
2. **bulk-sloppy-encodings** — closed. Diagnostic order depended on delivery:
   `cte.qp_invalid_hex` before `charset.invalid_sequence` read whole, after it
   read a byte at a time. The order is now by position in the body, here
   `charset.invalid_sequence` first, since a transfer decoder records a
   repair when its consumer has read up to it (transfer-encodings.md).
3. **nested-reused-boundary-deep** — closed: the outer multipart now reads on
   past the shared closing delimiter (`multipart.close_shared`) and `a.pdf` is
   kept (nested-boundaries.md). As recorded: an inner multipart reuses the outer
   boundary; the inner closing delimiter closes the outer one, and the outer
   multipart's attachment after it (`a.pdf`) is lost as epilogue with no
   diagnostic, state `complete`. Known (multipart-recovery.md), but it hides
   an attachment from anything scanning attachments. Expected: continue the
   outer multipart after a closing delimiter that more delimiters of the same
   boundary follow, or at least report that the epilogue holds delimiter
   lines.
4. **list-footer-after-close** — text after the closing delimiter (a list
   footer with an unsubscribe address) is invisible under the default
   profile and nothing says it exists. Expected: an Info diagnostic for a
   non-empty epilogue; `multipart.trailing_junk` is defined in
   `diagnostic_code.go` but no producer emits it. (ProfileForensic's
   `TrailingInputReader` can read it.)
   **Closed:** `mail.ReadTree` keeps every multipart's `Preamble` and
   `Epilogue` on its node and records `multipart.trailing_junk` (Info) in
   `Tree.Diagnostics` for an epilogue that is not only white space, saying
   its size and whether it reads as delimiter lines, a header block, text or
   binary data (multipart-recovery.md, Observations). The session, and so
   this manifest, is unchanged.
5. **bulk-duplicate-content-type-multipart** — `Content-Type: text/plain`
   followed by `Content-Type: multipart/mixed; boundary=…`: the first wins
   (the documented policy), so the HTML part and the zip attachment are one
   `text/plain` leaf of MIME source, with only `header.duplicate_singleton`.
   Debatable: when a later Content-Type is multipart and its boundary frames
   the body, reading it as such recovers the parts.
   **Changed by #67 (2026-11-14)**: the policy stands and the leaf is
   unchanged; `header.unselected_multipart` (Info) now says that the later
   `multipart/mixed` declaration's boundary delimits the body, and
   `Completion.Ambiguous` is set (it already was, through the Recovered
   `header.duplicate_singleton`). Reading the other declaration is a header
   rewrite and a second parse (header-recovery.md, The unselected
   Content-Type).
6. **ko-utf8-labelled-euc-kr** (plugin) — valid UTF-8 HTML declared
   `charset=euc-kr` is decoded as CP949 into mojibake, with no diagnostic.
   Expected: an observation such as `charset.declaration_suspect` when a body
   declared in a legacy multi-byte charset is valid non-ASCII UTF-8, and
   arguably decoding it as UTF-8.
   **Closed**: read as UTF-8 with `charset.declaration_suspect` (Recovered)
   under the healing profiles; the evidence rule is in charset-plugin.md,
   "UTF-8 under a legacy declaration".
   **Changed by #65 (2026-11-13)**: by default the body is decoded as
   declared again, and `charset.declaration_suspect` is reported at Info,
   naming UTF-8 (`plugin.json`: `info`, CP949 text). The UTF-8 reading is
   `ParseOptions.RecoverCharsetDeclarations` (Recovered).
7. **bulk-sloppy-encodings** (plugin, FallbackCharset euc-kr) — one damaged
   quoted-printable escape makes a UTF-8 body invalid UTF-8, so the whole
   body is re-decoded as EUC-KR: "Café" becomes "Caf챕" and "€" two U+FFFD
   (`charset.declaration_suspect`). Expected: a body that is overwhelmingly
   valid UTF-8 stays UTF-8; the fallback applies to bodies that are mostly
   not, or only to the invalid runs.
   **Closed**: a body whose sniff window is mostly UTF-8 (bytes in
   well-formed multi-byte sequences more than twice the ill-formed bytes)
   stays UTF-8 and records `charset.invalid_sequence` (Info), as without the
   fallback; charset-plugin.md, "The fallback charset and damaged UTF-8".
8. **ko-filename-encoded-word-quoted** (no plugin) — an `euc-kr` encoded word
   inside a quoted filename, with no decoder for the charset, is returned as
   the literal `=?euc-kr?B?…?=` text with no diagnostic and `Undecoded`
   false, while the UTF-8 one beside it reports `mime.parameter_rfc2047`.
   Expected: as for an RFC 2231 value in an unknown charset,
   `mime.parameter_unknown_charset` (Unsupported) and `Undecoded`. With the
   plugin both names decode.
   **Closed**: exactly that, for every parameter but `boundary`
   (mime-parameters.md, "Fallback charset and undecodable words").
9. **ko-raw-8bit-subject-filename** (plugin, FallbackCharset euc-kr) — a raw
   EUC-KR filename on a part whose Content-Type has no charset stays
   undecoded (`mime.parameter_unknown_charset`, Unsupported) although the
   parse was given `FallbackCharset` and the Subject decodes with the header
   fallback. The part whose Content-Type says `charset=euc-kr` decodes.
   Expected: parameter decoding to fall back to `ParseOptions.FallbackCharset`
   too.
   **Closed**: it does, when the bytes are valid in the fallback charset:
   `mime.parameter_fallback_charset` (Recovered), `ParameterCandidate.Fallback`.
   A header read with no parse takes `Header.FilenameCandidatesWithOptions`.
10. **ko-undeclared-euc-kr** (no fallback) — a text body with no charset and
    8-bit bytes that are not UTF-8 records nothing at all; a body declared
    UTF-8 with the same bytes records `charset.invalid_sequence` (Info).
    Expected: an Info observation here too, so a caller that set no fallback
    learns the text is undecoded bytes. With the fallback it is
    `charset.missing`, as designed.
    **Closed**: `charset.missing` at Info, bytes kept (and
    `charset.declaration_suspect` at Info under a declared `us-ascii`). An
    entity with no Content-Type field at all is still silent without a
    fallback; charset-plugin.md, "Text with no charset and no fallback".
11. **nul-in-text** — NUL bytes in an 8bit text body: no diagnostic (there is
    `header.nul` for headers and `cte.qp_control_byte` for quoted-printable).
    RFC 2045 forbids NUL in 8bit data, and text hidden behind NULs is a
    signal. Expected: an Info observation.
    **Closed:** `body.nul` (Info), once per body (transfer-encodings.md,
    Observations).
12. **subject-5000** — a 5000-octet header line, five times RFC 5322's 998:
    no diagnostic (nothing is lost; the Subject decodes whole). Debatable:
    an Info observation for lines over 998 octets.
    **Closed:** `header.overlong_line` (Info), once per header block, with
    the field in `Field` (header-recovery.md, Observations).
13. **bulk-sloppy-encodings** — `=` followed by spaces before CRLF is
    reported `cte.qp_invalid_soft_break` (Recovered). RFC 2045 section 6.7
    rule 3 makes trailing white space transport padding to be deleted, which
    leaves a valid soft break. Debatable: Info, or nothing. One diagnostic per
    code per body, so the genuinely invalid bare-LF soft break in the same
    body is reported by that same entry.
    **Closed:** white space between `=` and the line ending is transport
    padding and records nothing. The bare-LF soft break (`12=\n.50`) never
    recorded anything — a bare LF ends a quoted-printable line like CRLF —
    so the body now records no `cte.qp_invalid_soft_break`; a soft break
    ended by a bare CR still does (transfer-encodings.md, Observations).
14. **cut-in-base64-attachment** — the cut inside a base64 group is reported
    `cte.base64_padding_missing` (Recovered), not `cte.base64_truncated`;
    the message is still `incomplete` through `multipart.close_not_found`.
    Debatable: an unpadded final group in a part that the end of input cut
    off is more likely truncation than a sender omitting padding.
    **Closed:** a group cut where the input ends inside a multipart part
    records `cte.base64_truncated` (Incomplete). The root body of a message
    that frames no parts always ends where the input ends, so there the cut
    cannot be told from missing padding and stays
    `cte.base64_padding_missing` (transfer-encodings.md, Observations).
15. **mbox-from-line** — without `RecognizeMboxEnvelope` (the corpus default)
    Thunderbird's `From - <date>` first line is `header.invalid_name`, kept
    apart, and everything else is read. Debatable: a pointer to the option
    (the line fits its grammar) would say more than "invalid name".
    **Closed:** the code and what is parsed are unchanged; the diagnostic's
    Message says the line is an mbox From_ line and its Action names
    `ParseOptions.RecognizeMboxEnvelope` (header-recovery.md, Observations).
16. **provider-mobile-format-flowed** — not a defect: bodies are delivered as
    sent, so `format=flowed; delsp=yes` is not unwrapped and a word the client
    split ("만나 ⏎요") stays split. A tokenizing consumer needs RFC 3676
    unwrapping, which no API offers.
    **Closed:** `mail.UnwrapFlowed` and `Node.FlowedText` unwrap it; bodies
    are still delivered as sent (structure-view.md, Observations).

Decoded correctly with the plugin, for the record: EUC-KR and ks_c_5601-1987
bodies and B/Q words (including two folded Q words), CP949 extension
syllables (똠, 햏, 뷁) under an `euc-kr` label, RFC 2231 continuations that
split a double-byte character across segments (EUC-KR and UTF-8), encoded
words inside quoted filename parameters, ISO-2022-JP, Shift_JIS with
half-width katakana, GBK under `gb2312`, GB18030 four-byte sequences, Big5.
The plugin reports nothing about a widened label (CP949 under `euc-kr`); see
charset-plugin.md, Limitations.
