# Charset plugin robustness notes (`charset/`, `utf7/`)

Scope: the optional plugin `github.com/saeroun-ai/emlkit/charset` and the IMAP
codec `utf7`. Issues #36 and #50. Nothing outside those two directories was
changed; the root hook (`message.CharsetReader`) and its contract are as they
were.

Principle applied: decode what real mail means, keep going past faults, and
refuse (so that `message` keeps the raw body) only where no decoder exists.
Text that conforms to its charset decodes exactly as before.

## Status by ledger item

| ID | Status here | Evidence |
|---|---|---|
| ROB-080-001 | Implemented | `charset/aliases.go`, `Lookup`; `registry_test.go` decodes a fixed fixture under each of 421 labels, in both cases, and fails if an alias has no fixture |
| ROB-080-005 | Implemented | fixtures per family in `registry_test.go`; `iso2022.go` and `iso2022_test.go` |
| ROB-080-006 | Implemented | `utf7_test.go` (`isNotIMAPModifiedUTF7`), `utf7/separation_test.go` |
| ROB-080-008 | Implemented, plugin side | `decode_test.go` (replacement, floods, expansion, stall), `utf7_test.go` (long runs) |
| ROB-080-004 | Implemented; healing made opt-in by #65 (independently reviewed) | `decode_test.go` (`utf16ByteOrder`); conflicts: `charset_bom.go`, `charset/bom_conflict_test.go`, `charset_bom_test.go`, `charset/declaration_optin_test.go`; see "BOM and declaration conflicts" and "Default and opt-in (#65)" |
| ROB-080-003, -007 | Refined on the root side, after the corpus review | `charset_bom.go`, `charset_monitor.go`; `charset/declared_test.go`, `charset_declared_test.go`; see "Declarations the bytes contradict" |
| ROB-080-002 | Not touched | |

The acceptance column of the roadmap asks ROB-080-001 for a provider corpus.
None was available; the evidence is the fixture table, whose bytes come from
Python's codecs, not from the decoders under test.

## Label resolution (ROB-080-001)

Order, first match wins: `RegisterEncoding` entries, the alias table, then the
IANA index, `cs`+label and the WHATWG index of `golang.org/x/text`, as before.
Matching ignores case. A label that matches nothing is tried once more after
removing decoration: surrounding white space, quotes, backslashes and stray
`; , = < >`, a leading `charset=` / `charset:`, and everything from the first
space, `;`, `,`, `*` (RFC 2231 language tag) or quote on. Labels longer than 128
bytes are unknown without being examined.

`charset.Lookup(label)` returns the decoder, its canonical name and a `Repair`
bit set: `RepairLabel` (decoration removed), `RepairAlias` (not an IANA name),
`RepairSuperset` (decoded wider than labelled), `RepairFallback` (the label
names no charset). This is the plugin's whole reporting channel: the hook
returns only a reader.

### Alias table

| Decoder | Labels added | Why |
|---|---|---|
| CP949 (`korean.EUCKR`) | `cp949 x-windows-949 ms949 uhc x-uhc euc_kr x-euc-kr ks_c_5601 ks_c_5601_1987 ksc5601-1987 ksc5601_1987 ks-c-5601-1987 ks-c-5601` | Vendor and Java names; the decoder is the WHATWG one, code page 949 with extended syllables |
| ISO-2022-KR | `iso-2022-kr csiso2022kr iso2022kr iso2022-kr` | Was one U+FFFD for the whole body; see below |
| Shift_JIS (code page 932) | `cp932 x-ms-cp932 windows-932 x-ms932 sjis-win s-jis x-shift-jis x-shift_jis cswindows31j` | Microsoft, Java and PHP names |
| EUC-JP | `eucjp euc_jp x-eucjp ujis eucjp-win eucjp-ms cp51932` | |
| ISO-2022-JP | `iso2022-jp jis iso-2022-jp-1 iso-2022-jp-ms iso-2022-jp-cp932 x-windows-iso2022jp cp50220 cp50221 cp50222 windows-5022x x-windows-5022x` | The decoder reads the code page 932 table, JIS X 0212 and both katakana forms, which is what these labels add to RFC 1468 |
| GBK | `gb2312-80 gb-2312 euc-cn euccn x-euc-cn x-windows-936` | `gb2312` itself already resolved to GBK (WHATWG) |
| GB18030 | `gb-18030 gb18030-2000 gb18030-2005 cp54936 windows-54936` | |
| HZ | `hz hz-gb2312` | |
| Big5 | `x-big5 big-5 cp950 ms950 windows-950 x-windows-950 big5-eten big5hkscs` | |
| UTF-8 | `utf_8 x-utf-8 x-utf8 utf8mb3 utf8mb4 unicode-2-0-utf-8` | Misspellings and MySQL names |
| UTF-7 (RFC 2152) | `csutf7 unicode-1-1-utf-7 csunicode11utf7 utf7 x-utf-7 unicode-2-0-utf-7 x-unicode-2-0-utf-7` | Registered names x/text cannot decode, and spellings |
| UTF-16, byte order from the text | `unicode ucs-2 ucs2 iso-10646-ucs-2 csunicode unicodefeff` (and `utf-16 utf16 csutf16` through the registry) | See BOM policy |
| UTF-16LE / BE | `utf-16-le ucs-2le x-utf-16le` / `utf-16-be ucs-2be x-utf-16be` | |
| UTF-32 | `utf-32 utf32 ucs-4 ucs4 iso-10646-ucs-4 utf-32be utf-32le` and their `cs` names | Was unknown |
| windows-125x, windows-874 | `win-N`, `winN`; `tis620 cp874 ms874 x-windows-874` | `win-1251` is common in Cyrillic mail |
| ISO 8859 parts 1-11, 13-16 | `iso8859-N iso8859_N iso_8859_N 8859-N 8859_N`; `latin-1 latin_1 iso-latin-1 isolatin1 latin9 latin0 latin7` | Java and glibc spellings |
| KOI8-R/U, IBM866, Mac | `koi-8-r koi-8 x-koi8-r koi8_u koi-8-u x-koi8-u dos-866 x-cp866 x-ibm866 maccyrillic mac-cyrillic macroman mac-roman x-macroman x-macintosh` | |
| unknown-8bit | `unknown-8bit x-unknown unknown default x-default x-user-defined none null undefined binary 8bit`, the empty label; `646 usascii us_ascii` | See below |

A label is added only where it unambiguously means one charset. The near
misses pinned by ROB-080-011 (`cp-949`, `x-cp949`, `cp9490`, ...) stay unknown,
and the `cp-N` form is not accepted for any code page for the same reason.

### Deliberate widenings (reported as `RepairSuperset` / `RepairFallback`)

- **iso-8859-1 is decoded as windows-1252, iso-8859-9 as windows-1254**, under
  every name (`latin1`, `l1`, `cp819`, `latin5`, ...). They differ only in
  0x80-0x9F: C1 control codes in the ISO parts, which mail does not contain,
  and quotes, dashes and the euro sign in the code pages, which mail labelled
  with the ISO name does. This is the WHATWG and mail-client reading, and
  `iso8859-1` already resolved that way here. Bytes the code page leaves
  undefined keep their ISO control code, so every byte still maps to its own
  character and none becomes U+FFFD. Conforming ISO 8859 text is unchanged.
- **us-ascii and labels that name no charset use the unknown-8bit decoder**:
  each valid UTF-8 sequence is kept, any other byte above 0x7F is read as
  windows-1252 (as above). Valid UTF-8 passes through byte for byte; ASCII is
  unchanged. Before, `ascii` and its aliases gave U+FFFD for every such byte,
  `x-user-defined` gave private-use characters, and the others were unknown.
  `us-ascii`, `utf-8` and the empty label never reach the plugin from
  `message`, which passes those bodies through undecoded.

## Charsets (ROB-080-005)

Fixtures cover: UTF-8/7/16/32; EUC-KR/CP949, ISO-2022-KR; Shift_JIS/CP932,
EUC-JP (with JIS X 0212), ISO-2022-JP (with katakana, JIS X 0212, a code page
932 extension); GBK, GB18030 (four-byte forms), HZ, Big5 (with an ETEN
extension); KOI8-R, KOI8-U, windows-1251, ISO-8859-5, IBM866, Mac Cyrillic;
windows-1250/1252/1257, ISO-8859-2/3/4/10/13/14/15/16, Mac Roman, IBM437/850;
ISO-8859-6/7/8/8-I/9, windows-1253/1254/1255/1256/1258, windows-874 (TIS-620).

**ISO-2022-JP, ISO-2022-KR and HZ are decoded by this package**
(`charset/iso2022.go`): a small state machine rewrites the 7-bit text as the
EUC form of the same set, and the x/text EUC decoder does the rest, so tables
and characters are x/text's. A generated 100 KiB differential test shows
RFC-conforming ISO-2022-JP and HZ decode identically to the x/text decoders
they replace. They differ only on broken framing, where x/text loses its place:

- a line ending (CR or LF) ends every shift state. Mail lines end in CR LF,
  which x/text reads as one double-byte character, turning the following ASCII
  lines into ideographs when an escape back to ASCII is missing or cut off;
- a control character or space inside a double-byte run is itself;
- a first byte without a valid second byte is one U+FFFD and the next byte is
  read on its own, so an escape after a truncated character still works;
- HZ: a tilde that starts no escape is a tilde outside a GB run (x/text
  swallows the next character, breaking `~user` URLs).

ISO-2022-KR is new: x/text has no decoder and the WHATWG index maps the label
to "replacement", which turned the whole body into one U+FFFD. The brief
allowed documenting it as unsupported; decoding it cost one more state machine
over the existing CP949 decoder. Bytes above 0x7F under this label are read as
CP949 (8-bit Korean mislabelled as ISO-2022-KR decodes).

**Unsupported, and an unknown-charset error rather than garbage**: ISO-2022-CN
and -CN-EXT (also "replacement" in WHATWG; now an error, was one U+FFFD),
ISO-2022-JP-2/-3/-2004, Johab, EUC-TW, VISCII, TCVN, ARMSCII-8, the other Mac
scripts, and the Microsoft `_autodetect*` labels. `message` returns the
unknown-charset warning with the raw body for all of them (`unknown_test.go`).

## UTF-7 (ROB-080-006)

`utf-7`, `unicode-1-1-utf-7` and their spellings are RFC 2152. No label reaches
the IMAP codec (`utf7` package, RFC 3501 mailbox names), and labels that ask
for it (`utf-7-imap`, `x-imap-mailbox-name`, ...) are unknown charsets. Tests
on both sides: the plugin leaves `&AOk-`, `&-` and `,` alone; the IMAP codec
leaves `+AOk-` alone and rejects RFC 2152-only syntax. The IMAP codec stays
strict (its errors are its contract); a sweep of all short inputs shows no
panic, termination, and agreement between one-shot and streamed decoding.

Standard UTF-7 decoder changes:

- a base64 run of any length decodes in fixed memory. Before, a run that did
  not fit the reader's 4096-byte buffer failed the whole body with
  `transform.ErrShortSrc`;
- a run of at most 256 base64 characters is still all-or-nothing: if it is not
  well-formed UTF-16 the `+` and the run are kept as text (existing behaviour,
  existing tests). A longer run is committed to once its first 256 characters
  decode, and later faults in it are U+FFFD;
- a `+` inside a run that failed is part of that text and no longer starts a
  new shift (`+A+AOk-` stays as it is; it used to become `+Aé`). `+` is a
  base64 character, so re-reading it made a hostile run cost up to its length
  per byte; now each byte of a run is scanned, decoded and copied once;
- an octet above 0x7F that is not UTF-8 is U+FFFD (it used to be copied
  through, producing invalid UTF-8). UTF-8 is still kept.

## Decode-error and work policy (ROB-080-008)

- **Replace and continue.** No decoder of the plugin returns an error. A
  malformed or truncated sequence is U+FFFD and decoding resumes after it; a
  first byte cut off by a line break is one U+FFFD and the line break stays.
  Tested per multi-byte family, at a line break, at the end, and between
  characters.
- **Output is always valid UTF-8**, and never more than three times the input
  (tested over every decoder with twenty adversarial byte patterns).
- **Streaming.** Every decoder is an x/text transformer behind 4096-byte
  buffers; nothing accumulates. Output does not depend on how the input is
  chunked (every decoding test runs whole and one byte at a time).
- **Linear work.** State changes consume input and emit nothing: 256 KiB
  floods of `ESC $ B`, `~{~}`, SO/SI, the ISO-2022-KR designation, `+-` and
  lone shifts decode in buffer-sized reads with the expected output size. The
  UTF-7 all-or-nothing rule looks at most 256 characters ahead and never
  re-reads a run it gave up on.
- **Errors are the input's.** An input error is returned unchanged after what
  was decoded before it.
- **Stalled input.** The x/text reader asks again for ever when its input
  returns `(0, nil)`. The plugin now ends that with `io.ErrNoProgress` after
  65536 consecutive empty reads. The bound is far above the 100 of
  `message.Parse` on purpose: Parse can see progress below the decoder and
  always decides first; this only ends a spin on the entry points that have no
  such rule (`Read`, `New`, direct use of `charset.Reader`).

## Byte order marks (ROB-080-004, plugin side)

- `utf-8` and its spellings: the mark is **kept** as U+FEFF, as `message` does
  for the `utf-8` it passes through itself. Stripping it only under some
  spellings would be inconsistent, and the root is not this item's to change.
- `utf-16` (and `unicode`, `ucs-2`, ...), `utf-32`: a mark decides the byte
  order and is **consumed**.
- `utf-16le`, `utf-16be`, `utf-32le`, `utf-32be`: the label is taken at its
  word, never examined, and a leading U+FEFF is text (RFC 2781).
- Unmarked `utf-16`: the first 128 bytes are examined. The byte position with
  clearly fewer distinct values is the high byte; failing that, NUL bytes
  beside ASCII decide; failing that, the label's default applies: **big-endian
  for `utf-16`** (RFC 2781, and the previous behaviour), little-endian for the
  Windows names `unicode`/`ucs-2` (WHATWG, and the previous behaviour). This is
  what makes .NET mail readable (`charset=utf-16`, little-endian, no mark).
  Unmarked `utf-32` is big-endian without examination.

## Declarations the bytes contradict (root side)

Where: `charset_bom.go` (`cdRead`, `cdDecideUTF8`), `charset_monitor.go`
(`cdUTF8Scan`, `fallbackReader`, `utf8Monitor`), under `Parse` with
ProfileTolerant and ProfileForensic. ProfileStrict, `Read`,
`ReadWithOptions` and `New` decode as declared and report what they always
did. Tests: `charset/declared_test.go`, `charset_declared_test.go`,
`charset/fallback_test.go`, `charset/declaration_optin_test.go`. The plugin
is unchanged.

### Default and opt-in (#65)

Policy #62: bytes that read two ways are read as declared, and the
contradiction is reported as a fact; the other reading is the caller's
choice. Since #65 (option (a), 2026-11-13) that applies to both heals of
this section and of "BOM and declaration conflicts":

| | Default | `ParseOptions.RecoverCharsetDeclarations` | ProfileStrict |
|---|---|---|---|
| UTF-8 under a legacy label, by the rule below | as declared; `charset.declaration_suspect` **Info**, Action names UTF-8 and the option | UTF-8, Recovered (as before #65) | as declared, silent (option ignored) |
| a byte order mark the label does not accept, bytes after it valid in its encoding | as declared, mark included; `charset.bom_mismatch` **Info**, Action names the mark's encoding and the option | as the mark says, Recovered (as before #65) | as declared; Info (option ignored) |
| the same, bytes after the mark not valid | as declared; Info | as declared; Info | as declared; Info |

Under the healing profiles the look is the same with the option and without:
the same bounded hold (at most 4096 bytes, 1028 for a mark), read only when
the declared decoder first reads, chunk-invariant, with the same evidence.
Default mode only skips the redirect, so a body that the option would have
read as UTF-8 or as its mark is what the declared decoder makes of it, and
the UTF-8 monitor of a healed body (`charset.invalid_sequence` for a healed
body) runs only with the option. The header and the raw bytes (`RawBody`,
`RawReader` of a parse with a source) are untouched either way; a source
that fails, and a sink that stops the parse at the diagnostic, end the body
the same way in both modes (tested). `FallbackCharset` is a separate option
and unchanged.

Valid Shift_JIS `C2 A1 C2 A1` (`ﾂ｡ﾂ｡`) was the independent reproduction:
`Parse` now gives `ﾂ｡ﾂ｡` like `Read`, with `charset.declaration_suspect` at
Info; only the option gives `¡¡`. The known misses below are thus false
reports at Info by default, and false heals only with the option.

Not part of #65: an entity with no Content-Type field at all is still not
watched for a missing charset unless a fallback is configured (see "Text
with no charset and no fallback").

All three rules below read the same evidence, the **sniff window**: the first
4096 bytes (`charsetSniff`) of a text body after its transfer encoding was
removed, or the whole body when it is shorter. Every byte above 0x7F in it is
either inside a well-formed UTF-8 multi-byte sequence (no overlong form, no
surrogate, nothing above U+10FFFF) or it is one *ill-formed byte*. A sequence
cut by the end of a full window is left out; one cut by the end of the body
is ill-formed. The decision depends on those bytes only, never on how the
input was cut into reads (tested whole and a byte at a time).

### UTF-8 under a legacy declaration

A body that declares a charset the `CharsetReader` accepted and that names no
Unicode encoding (`euc-kr`, `ks_c_5601-1987`, `shift_jis`, `gb2312`, `big5`,
`iso-2022-jp`/`-kr`, `iso-8859-1`, `windows-1252`, `windows-1251`, ...) and
begins with no byte order mark is UTF-8 by the evidence — reported as
`charset.declaration_suspect`, at Info by default and Recovered, read as
UTF-8, with `RecoverCharsetDeclarations` — when all of these hold:

1. the window holds **no ill-formed byte**;
2. it holds **at least two** multi-byte sequences (`cdSuspectSequences`);
3. **one of them is three or four bytes long** — or, when all are two bytes
   long, the declared decoder itself shows the bytes are not its own: asked
   to decode the window, it returns U+FFFD, or one character for every byte
   (which is how a single-byte charset reads anything);
4. under an ISO 2022 label the window holds no ESC, SO or SI.

Otherwise the body is decoded as declared, silently, as before.

Why the rule is this one:

- Text really in a double-byte charset is ill-formed UTF-8 almost at once.
  About one EUC-KR Hangul syllable in ten is a well-formed *two-byte*
  sequence by accident (lead C2-C8, trail A1-BF); a three-byte one needs a
  hanja followed by particular syllables. So a real EUC-KR, GBK, Big5 or
  Shift_JIS body passes rule 1 only when it is a handful of characters, all
  of the accidental kind — `화환` is `C8 AD C8 AF`, also `ȭȯ` in UTF-8. Those
  are two-byte sequences, and rule 3 keeps them: the declared decoder reads
  them as pairs, without error.
- A single-byte charset maps every byte to something, so there the UTF-8
  evidence stands alone, which is why it asks for two sequences and a clean
  window: an accented capital followed by the one kind of symbol that
  completes a sequence (`Ã©`), twice, with no other accented letter in 4 KiB,
  is what UTF-8 read as windows-1252 looks like and not what people write.
- Rule 3's second arm is the only place the hook is asked (one decode of at
  most 4096 bytes, under the no-progress guard and the 8x output cap of a
  parameter value). It is what lets UTF-8 Cyrillic or Latin under
  `windows-1251`, `koi8-r` or `iso-8859-1` be healed — the classic
  `РџСЂРёРІРµС‚` — without also "healing" `화환`.

What is delivered with the option (by default, the declared decoder's
text): the ASCII before the first byte above 0x7F as the declared
decoder read it, and everything from that byte on as it is, watched for bytes
that turn out not to be UTF-8 after the window (`charset.invalid_sequence`,
Info). The header is not changed and the raw bytes are the input's.

What it costs: nothing is read ahead while the body is ASCII — the decoder is
handed what it asks for and the hold keeps a copy of at most 4096 bytes. From
the first byte above 0x7F the source is read ahead of the decoder to the
first ill-formed byte, the end of the window or the end of the body; real
legacy text ends the look within a character or two.

Not examined: `utf-7` and `hz` labels (their shifts are printable ASCII, and
a UTF-7 decoder keeps UTF-8 anyway); a label the hook refuses
(`charset.unknown`, body raw — which for a UTF-8 body is already UTF-8); a
body whose byte order mark was reported; a `CharsetReader` that read its
input while it built its decoder.

Known misses, on purpose (by default a false positive is an Info report, and
a false heal needs the option): a body with a single multi-byte character; UTF-8
made only of two-byte sequences under a double-byte label when the declared
decoder happens to read every pair (`café déjà` under `euc-kr` decodes to
valid Hangul and stays that); text beyond the window; Shift_JIS half-width
katakana that pair up as UTF-8 (`ﾃｽ` is `ý`), which the second arm of rule 3
takes for a single-byte reading — two or more such pairs and nothing else
8-bit in the window.

### The fallback charset and damaged UTF-8

`ParseOptions.FallbackCharset` is for bodies that declare no charset, UTF-8
or US-ASCII and **are not UTF-8**. Before, one ill-formed byte in the window
— a broken quoted-printable escape — sent the whole body through the
fallback decoder (`Café` became `Caf챕`). Now the body stays UTF-8 when the
window

- holds no ill-formed byte, or
- is **mostly UTF-8**: the bytes inside its well-formed multi-byte sequences
  are more than twice its ill-formed bytes (`good > 2 × bad`,
  `cdFallbackMajority`).

Otherwise it is decoded with the fallback (`charset.missing` or
`charset.declaration_suspect`, Recovered), as before. Real legacy text is far
on the other side of that line: about one byte in five of EUC-KR or GBK text
falls into a well-formed sequence by accident, fewer for Shift_JIS and the
single-byte charsets, so `good` is a fraction of `bad`, not twice it. A tie
goes to the fallback (`Caf\xc3\xa9 \xb0`: two good bytes, one bad). A damaged
body that stays UTF-8 is delivered as it was read — nothing is replaced — and
is watched like any other body, so its damage is still reported at Info.
ProfileStrict keeps the old rule (any ill-formed byte).

### Text with no charset and no fallback

A text body that the parser delivers as read — declared `utf-8`, declared
`us-ascii`, or a Content-Type with no charset — is watched for bytes that are
not UTF-8, and the first is reported once at Info, nothing replaced:

| Declared | Code |
|---|---|
| `utf-8` | `charset.invalid_sequence` (as before) |
| no charset | `charset.missing` |
| `us-ascii` | `charset.declaration_suspect` |

`charset.missing` thus means "a text body declares no charset and is not
US-ASCII or UTF-8" in both of its severities: Info when the bytes were kept
(no fallback, or none that applies), Recovered when
`ParseOptions.FallbackCharset` decoded them. Valid UTF-8 under no charset or
`us-ascii` is not reported. An entity with **no Content-Type field at all**
is watched only when a fallback charset is configured: it is text/plain only
by default, nothing declares its bytes to be text, and the transfer-encoding
tests pin such bodies as silent. A parse that was stopped does not report the
body it cut. ProfileStrict watches only `utf-8`, as before.

### Parameters and filenames

See mime-parameters.md, "Fallback charset and undecodable words": raw 8-bit
parameter values fall back to `ParseOptions.FallbackCharset`
(`mime.parameter_fallback_charset`), and an encoded word in a charset with no
decoder marks its candidate `Undecoded` (`mime.parameter_unknown_charset`).

## Limitations and open points

- **No diagnostics from the hook.** `func(charset, io.Reader) (io.Reader,
  error)` cannot report a repaired label, a widened decoder or a replaced
  sequence. `Lookup` exposes the label-level part; replaced sequences are only
  visible as U+FFFD. `message` cannot import `charset` (the import runs the
  other way), so surfacing this as diagnostics needs a root-side hook.
- **BOM/declaration conflicts** are detected and healed by `message`, not by
  the plugin; see "BOM and declaration conflicts" below. The plugin itself
  still never overrides an explicit byte order.
- **Unmarked UTF-16 can be guessed wrong** when it is a few characters of one
  non-Latin script with no ASCII, or one or two characters whose low byte is
  zero: the label default applies. Nothing is delivered until 128 bytes or the
  end of input have been seen, so an input error inside the first 128 bytes of
  unmarked UTF-16 loses that prefix.
- **Encoded words in headers**: Go's `mime.WordDecoder` decodes `iso-8859-1`,
  `us-ascii` and `utf-8` words itself, strictly, and never asks the plugin, so
  the two widenings above apply to bodies and to other charsets' words only.
- **ISO-2022-JP**: bytes above 0x7F are U+FFFD, as before (not guessed to be
  EUC-JP, Shift_JIS or UTF-8); ISO-2022-JP-2's other national sets are not
  decoded. A line break resets the state, which mis-reads a double-byte run
  that a gateway wrapped without closing it; x/text already did this for LF.
- **Decoded-size budgets** are the root's (ROB-001-005, ROB-070-010); the
  plugin only guarantees the threefold bound and fixed memory.
- A reader or `CharsetReader` that blocks inside `Read` cannot be interrupted.

## API and behaviour changes to review

Added: `charset.Lookup`, `charset.Match`, `charset.Repair` and its four
constants. `RegisterEncoding` now lower-cases the name it stores (lookups were
already lower-case, so a mixed-case registration could never match).

Changed for input that was already accepted: 0x80-0x9F under the ISO-8859-1 and
-9 labels; bytes above 0x7F under `ascii` and its aliases; `x-user-defined`;
unmarked `utf-16` that looks little-endian; marked or big-endian-looking text
under `unicode`/`ucs-2` (was always little-endian with the mark kept);
`iso-2022-kr` (decoded, was one U+FFFD); `iso-2022-cn`, `replacement` (unknown
charset, was one U+FFFD); broken framing in ISO-2022-JP and HZ; non-UTF-8
octets and runs over 256 characters in UTF-7; a stalled input ends. Error texts
for unknown labels changed (`unknown charset` / `unsupported charset`).

## BOM and declaration conflicts (ROB-080-004)

Where: the root, `charset_bom.go`, not the plugin. The hook returns only a
reader and `message` cannot import `charset`, but the decision needs nothing
the plugin knows: the declared label and the body's first bytes, which the
root sees before the hook does. Under `Parse` the input of every text body's
charset stage — a declared charset the `CharsetReader` accepted, us-ascii,
utf-8, or no charset at all — is held for one look when the declared decoder
first reads it. The root keeps the plugin's spellings of the UTF-8, UTF-16
and UTF-32 labels (`bomClassLabels`); every other label is a charset without
a mark of its own. The plugin is unchanged.

What happened before (base `e58d480`, plugin imported, `Parse`, text
`Hi 안녕` behind each mark) and what ROB-080-004 made of it under
ProfileTolerant and ProfileForensic. Since #65 the "Now" column holds only
with `ParseOptions.RecoverCharsetDeclarations`; by default every Recovered
row below is decoded as declared, mark included, and reported at Info with
the mark's encoding named in the Action (see "Default and opt-in (#65)").
The rows that report no conflict, or Info, are the same in both modes.

| Declared | Body begins with | Before | Now | `charset.bom_mismatch` |
|---|---|---|---|---|
| utf-16le / utf-16be | the other UTF-16 mark | U+FFFE and byte-swapped text, silent | UTF-16 in the mark's order, mark consumed | Recovered |
| utf-16le / utf-16be / utf-32le / utf-32be | its own mark | U+FEFF kept as text | unchanged | none |
| utf-16 (`unicode`, `ucs-2`, ...) | either UTF-16 mark, or none | the mark decides and is consumed; unmarked text is sniffed | unchanged | none |
| utf-16 / utf-16le | UTF-32LE mark, valid UTF-32 after it | U+0000 between the characters, silent | UTF-32 | Recovered |
| utf-32le / utf-32be | the other UTF-32 mark | all U+FFFD, silent | UTF-32 in the mark's order | Recovered |
| utf-32 | either UTF-32 mark, or none | the mark decides; unmarked is big-endian | unchanged | none |
| utf-32 | a UTF-16 mark | garbage, silent | UTF-16 | Recovered |
| utf-16, utf-16le, utf-32 | UTF-8 mark | mojibake (U+EFBB ...) or U+FFFD, silent | UTF-8, mark kept | Recovered |
| euc-kr, iso-8859-1, windows-1252, shift_jis | UTF-8 mark | mojibake (`ï»¿Hi ì•ˆë…•`), silent | UTF-8, mark kept | Recovered |
| the same | a UTF-16 mark | `ÿþH\0i\0...` or U+FFFD, silent | UTF-16 | Recovered |
| utf-8 | UTF-8 mark | U+FEFF kept | unchanged | none |
| utf-8 | a UTF-16 or UTF-32 mark | bytes kept; `bom_mismatch` (info) and `invalid_sequence` | UTF-16 / UTF-32 | Recovered |
| utf8 (a spelling the plugin decodes) | a UTF-16 mark | U+FFFD for each invalid byte, silent | UTF-16 | Recovered |
| us-ascii, or no charset | UTF-8 mark | the bytes as read, which is UTF-8, silent | unchanged | Info |
| us-ascii, or no charset | a UTF-16 or UTF-32 mark | the raw UTF-16/32 bytes, silent | UTF-16 / UTF-32 | Recovered |

Policy:

- **The declaration is never silently replaced.** The header is not changed,
  the raw bytes of a parse with a source (`ParseAt`, `RawBody`, `RawReader`)
  are the input's, with the option and without, and every conflict is
  reported once, at the body, with `Field: "Content-Type"`, a message naming
  the declared charset (quoted, or "declares no charset, so US-ASCII") and
  the mark (`UTF-8`, `UTF-16LE`, ...), and an Action that says how the body
  was decoded.
- **Heal only when asked (#65), and only on evidence.** Without
  `RecoverCharsetDeclarations` nothing is healed: a conflict whose bytes are
  valid in the mark's encoding is decoded as declared at SeverityInfo, the
  Action naming that encoding. With it, the body is decoded as its mark says — UTF-8 by
  the parser itself as for a body declared utf-8 (mark kept, watched for
  invalid sequences), UTF-16 and UTF-32 by the `CharsetReader` under the
  labels `utf-16` and `utf-32` (mark decides the order and is consumed) — only
  when the up to 1024 bytes after the mark are valid in the mark's encoding
  (no stray surrogate, no code point above U+10FFFF or in the surrogate
  range, valid UTF-8; a unit cut at the end of the look does not count), the
  profile is not ProfileStrict, and there is a decoder. That is
  SeverityRecovered. `FF FE 00 00` is a UTF-32LE mark only when what follows
  is valid UTF-32LE; otherwise it is a UTF-16LE mark followed by U+0000.
- **Otherwise keep the declaration**, mark included, at SeverityInfo, with the
  reason in the Action: the bytes after the mark are not in its encoding (for
  example EUC-KR after a UTF-8 mark), there is no decoder (no plugin), or
  ProfileStrict. A UTF-8 mark under us-ascii or no charset is SeverityInfo
  too: such a body is delivered as read, which already is UTF-8.
- **ProfileStrict, `Read`, `ReadWithOptions`, `New`** decode exactly as
  before. ProfileStrict reports a conflict at SeverityInfo (before, only a
  UTF-16/32 mark under the exact label utf-8 was reported); the other entry
  points have no session and report nothing.
- **What the look costs.** It reads when the declared decoder first reads its
  input, as much as that read asks for and at least four bytes; only after a
  mark the declaration does not accept does it read ahead, up to 1028 bytes,
  before anything is delivered. A decoder that reads nothing makes the look
  read nothing, so the no-progress counts of the body guard are unchanged,
  and an empty read of the source is passed on as one. Whole input and one
  byte at a time give the same body and diagnostics (tested for every mark
  under ten labels).

The UTF-8 monitor (`charset_monitor.go`) no longer reports the mark itself;
its earlier `charset.bom_mismatch` (message "a body declared UTF-8 begins
with a UTF-16 or UTF-32 byte order mark") is now this one.

Limitations:

- A label the `CharsetReader` refuses (unknown charset) is not examined: the
  body stays raw with `charset.unknown`, whatever its mark says.
- A single-byte text that really begins with `ÿþ` or `þÿ` (or `ï»¿`) and
  whose next bytes happen to be valid UTF-16 (UTF-8) is reported at Info by
  default, and decoded as the mark says only with
  `RecoverCharsetDeclarations`; real text does not begin that way, and the
  diagnostic says what was done.
- A `CharsetReader` that reads its input while it builds its decoder gets the
  input unexamined, and nothing is reported.
- Under `ReadOptions.SkipTextAttachmentDecoding` a text body without a charset
  is examined only when `ParseOptions.FallbackCharset` is set, so that its
  Content-Disposition is not interpreted for this alone.
- The label table in the root has to follow the plugin's aliases for the
  Unicode encodings; a new spelling added to `charset/aliases.go` reads as a
  charset without a mark until it is added there too.
- The UTF-7 (`+/v8`) and GB18030 (`84 31 95 33`) marks are not recognized.

## 2026-11-13 follow-up review (#65)

Codex reviewed the option plumbing, both decoder decisions and changed
expectations. The original Shift_JIS reproduction and 60 independent
profile/chunk/raw-source scenarios pass. Focused root/charset race tests,
full fresh unit tests, build, formatting and diff checks pass. The Python
comparison has 74 fixtures, 44 explained differences, two open folding
differences (#63), and no unexplained or stale exceptions. The default
change and opt-in API are accepted; real-provider corpus work remains open.
