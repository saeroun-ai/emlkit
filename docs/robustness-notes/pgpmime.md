# PGP/MIME and OpenPGP data (ROB-140-005)

Code: `pgpmime/` (a separate package; the core does not import it, and it
uses the standard library only). Tests: `pgpmime/*_test.go`, vectors and the
commands that made them in `pgpmime/testdata/README.md`.

## What it does

The standard library has no OpenPGP and the module takes no dependency, so
the package reads structure and metadata and does no cryptography.
Verification and decryption are interfaces the caller implements.

- **Classification** from plain values (`Part`: media type, parameters,
  disposition, file name, first body bytes). `Classify` gives one part's
  `Kind`: `signed`/`encrypted` multipart by protocol, `signature`
  (`application/pgp-signature`), `control` (`application/pgp-encrypted`),
  `keys` (`application/pgp-keys`), `armored` (text or attachment holding
  BEGIN PGP blocks, with their types), `binary` (an attachment that starts
  with a plausible OpenPGP packet, or is named `.pgp`/`.gpg`/`.sig`/`.asc`
  when no bytes are given). A part named like OpenPGP data whose bytes are
  not is reported (`classify.type_mismatch`: the `invoice.pdf.gpg` that is a
  PDF).
- **Shape** (`CheckStructure`, RFC 3156): part count and order, protocol
  missing or not matching the parts, micalg missing or not a `pgp-` hash,
  control part missing or not `Version: 1`, payload not
  `application/octet-stream`. A PGP/MIME message with a missing or wrong
  protocol is recognised by its parts. Exchange's rewrapping of
  `multipart/encrypted` as `multipart/mixed` with the control part and the
  payload as attachments (and often an empty text part) is recognised and
  reported as `structure.rewrapped`; `Control` and `Content` index the two
  parts. An S/MIME multipart is `KindNone`, without issues.
- **ASCII armor** (`Parse`, RFC 9580 §6): every block of a text, among other
  text: `PGP MESSAGE`, `SIGNED MESSAGE`, `SIGNATURE`, `PUBLIC KEY BLOCK`,
  `PRIVATE KEY BLOCK` (and PGP 2's `SECRET KEY BLOCK`), `MESSAGE, PART X/Y`
  and `PART X`; armor headers; the base64 body; the CRC-24.
- **Cleartext signatures** (RFC 9580 §7): `Cleartext.Text` is the signed
  text as a reader sees it (dash-escaping and quoting removed, lines joined
  by LF, trailing white space kept, no final line ending), `Hash` the Hash
  header names, `Signature` the following signature block.
  `Cleartext.Canonical()` is what was hashed: trailing spaces and tabs
  removed from every line, CR LF between lines, none after the last. This
  gives the classifier the text of a clearsigned mail, which is not a MIME
  body of its own.
- **Packet walk** (`ParsePackets`, and every block's `Packets`): old and new
  headers, every length form including partial body lengths and the
  indeterminate old-format length; each `Packet` has its tag, offset, depth,
  length, and `Body` when contiguous. Decoded: signatures v3–v6 (type,
  algorithms, creation and expiry, issuer key ID and fingerprint, signer's
  user ID, hash prefix), one-pass signatures v3/v6, PKESK v3/v6 (recipient
  key ID or fingerprint, anonymous recipients), SKESK v4–v6, encrypted data
  (SED tag 9 reported as unprotected; SEIPD v1/v2 and the LibrePGP AEAD
  packet with cipher, AEAD mode and chunk size), keys v2–v6 (algorithm,
  creation, bits or curve, fingerprint and key ID: SHA-1 for v4, SHA-256 for
  v5/v6, the modulus for RSA v3; the public part of secret keys), user IDs,
  literal data (format, file name, date, content), compressed data
  (uncompressed, ZIP, ZLIB, BZip2, inflated and walked one level deeper).
  `Result.Summary()`/`Summarize` gather recipients, signers, keys, user IDs
  and literal file names.
- **Integration points**: `Verifier` and `Decrypter`, with
  `DetachedRequest`, `CleartextRequest`, `InlineRequest` and
  `EncryptedRequest` preparing the exact bytes each needs (below).

## Structure only: what may be trusted

Nothing the package reports is authenticated. A signature packet's issuer is
a claim; a cleartext signed text is just text until a Verifier has checked
it; the recipients of an encrypted message are what its session key packets
say. A spam classifier may use them as features (a signature by a key ID, an
encrypted mail to an anonymous recipient, a literal file name, an armored
private key in a mail body) but not as proof of origin. Integrity of the
encrypted data (SEIPD versus SED) is only declared until decryption checks
it.

## Verification and decryption

`DetachedRequest(signed, signature, micalg, lim)` is for PGP/MIME signed
messages. `signed` must be the first part exactly as transmitted, header and
body, from the byte after its delimiter line to the byte before the line
ending of the next one: `input[n.SpanStart:n.SpanEnd]` of the `mail.Node`
(`message.Entity.RawSpan`). The bytes are canonicalised to CR LF line
endings (`Canonicalize`), as RFC 3156 §5 requires of a receiver, and nothing
else: trailing white space and the transfer encoding are signed as sent.
`signature` is the decoded body of the second part, armored or binary.
`micalg` is compared with the signature's hash (`verify.micalg_mismatch`).

A part without a span (`HasSpan` false: the root, a part of an inferred
boundary, a message inside an encoded part) gives `verify.no_signed_bytes`
and no request. A part the parser repaired — any diagnostic with an `Action`
whose `Path` lies under the `multipart/signed` node — is unverifiable: its
bytes may not be what was signed, so a failed verification there is no
evidence of forgery. The same holds for a message re-encoded in transit (8bit
to quoted-printable), the classic cause of broken PGP/MIME signatures.

`CleartextRequest(c)` signs `c.Canonical()`; `InlineRequest(message, lim)` is
for `gpg --sign` output, where the signature travels with the literal data
(the Verifier gets the whole message). `EncryptedRequest(payload, lim)`
dearmors the payload of a `multipart/encrypted` or an encrypted attachment
and lists its recipients; the Decrypter returns the literal content, which
for PGP/MIME is a MIME entity to parse again (RFC 3156 §4), and the verdicts
of signatures inside it (§6.2).

The tests carry an example Verifier over `crypto/ed25519` that verifies the
gpg vectors: the PGP/MIME signature over the span emlkit framed (also after
the message's line endings were changed to LF), the cleartext signature
(also quoted with `> ` and with CR LF), and the three compressed inline
signatures. Nothing is decrypted, since the secret key is not kept.

## Budgets

`Limits`; a zero or negative field takes its default, and every budget is
shared by the whole call, nested packets included.

| Field | Default | When reached |
| --- | --- | --- |
| `MaxInputBytes` | 32 MiB | the rest of the input is ignored (`limit.input`) |
| `MaxBlocks` | 64 | later blocks are not read |
| `MaxArmorHeaders` | 32 per block | later headers are not kept |
| `MaxPackets` | 4096 | the rest of the stream is not walked |
| `MaxKeptBytes` | 4 MiB | literal content, user IDs and signer IDs are cut; `Literal.Truncated` |
| `MaxDecompressedBytes` | 32 MiB | inflating stops; `Compressed.Truncated` |
| `MaxDepth` | 8 | a deeper compressed packet is not inflated |
| `MaxSubpackets` | 64 per signature | the rest of the area is not read |
| `MaxUserIDs` | 64 | later user IDs are not kept |
| `MaxIssues` | 256 | one `limit.issues` marks the rest |

No allocation is sized from a declared length: packet bodies are subslices
of the stream, a length that overruns the stream only ends the body
(`packet.truncated`), partial chunks are read in place, literal content is
copied up to the bytes present and the kept budget, inflation reads into a
buffer capped at the remaining budget, and four-octet lengths are compared
as 64-bit values before slicing. Decoded block data and cleartext are bounded
by the input. Scanning is linear: a block ends at the next BEGIN line, so
text is scanned at most twice.

## Tolerance

- Line endings CR LF, LF or lone CR, freely mixed.
- Quoting and indentation (`> `, `> > `, spaces, tabs) before the BEGIN line
  and the lines after it are removed (`armor.quoted`); in a cleartext the
  BEGIN line's prefix, or that prefix without its trailing space (how
  clients quote an empty line), is removed from each text line. A UTF-8 byte
  order mark before a BEGIN line is ignored.
- Trailing white space on armor lines is ignored.
- BEGIN and END lines with three to ten dashes (`armor.begin_damaged`,
  `armor.end_damaged`); an END that names another type ends the block
  (`armor.end_mismatch`).
- Headers not followed by a blank line: the body starts at the first line
  that is not `Key: value` (`armor.blank_line_missing`). In a cleartext only
  `Hash`, `Charset`, `Version` and `Comment` count as headers, so that a
  text line like `Subject: x` is not swallowed.
- No END line: the block ends after its checksum line, else before the first
  line holding a character outside base64 (prose after a cut-off block),
  else at the next BEGIN line or the end of the input (`armor.end_missing`).
- Characters outside base64 in a closed block are skipped
  (`armor.invalid_character`); missing padding is accepted, misplaced
  padding splits the data into segments decoded separately, and a final lone
  character is dropped (`armor.padding`); lines after the checksum are
  ignored (`armor.data_after_crc`).
- CRC-24: absent (`armor.crc_missing`, informational, as RFC 9580 makes it
  optional), malformed (`armor.crc_invalid`) or wrong (`armor.crc_mismatch`);
  the data is returned in every case.
- Cleartext: a line starting with an unescaped dash is kept as it is
  (`cleartext.unescaped_dash`); the text ends at the signature's BEGIN line,
  or at another block's BEGIN line or the end of the input
  (`cleartext.signature_missing`). Unknown Hash names are reported.
- Packets: an unknown or reserved tag is listed and skipped; a known packet
  of an unknown version is listed undecoded (`packet.version_unknown`); a
  body shorter than its fields is listed undecoded (`packet.malformed`); a
  partial length on a non-data packet stops its decoding
  (`packet.partial_not_allowed`); a first partial chunk under 512 octets is
  accepted (`packet.partial_short`); a byte that is not a packet header ends
  the walk (`packet.invalid_header`); corrupt compressed data is walked as
  far as it inflated (`packet.decompress_error`).

## Not covered

- No signature is verified and nothing is decrypted without a caller's
  Verifier or Decrypter; no keyring, trust model or key expiry check.
- `MESSAGE, PART X/Y` blocks are recorded, not reassembled; only part 1 is
  walked.
- Version 5 signatures are read as LibrePGP writes them, with two-octet
  subpacket area lengths. PKESK version 5, user attribute packets, notation
  data, embedded signatures and key flags are not decoded.
- The MIME classification does not recognise a `multipart/signed` that a
  gateway rewrapped as `multipart/mixed` (indistinguishable from a signature
  sent as an attachment), nor the pre-RFC 3156 `application/pgp;
  x-action=...` beyond the armor it carries.
- Fingerprints of version 3 keys (MD5) are not computed.

## Wiring to `mail.ReadTree`

Keep the input (for spans), parse it, read the tree, and copy each node's
values into `pgpmime.Part`. Multiparts go to `CheckStructure`, leaves to
`Classify`; an `armored` text body goes to `Parse` for its blocks and its
cleartext text. `pgpmime/example_test.go` runs this against
`testdata/signed.eml`; in outline:

```go
input, _ := io.ReadAll(r)
sess, e, err := message.Parse(bytes.NewReader(input), nil)
tree, err := mail.ReadTree(e, nil)

part := func(n *mail.Node) pgpmime.Part {
	return pgpmime.Part{MediaType: n.MediaType, Params: n.Params,
		Disposition: n.Disposition, Filename: n.Filename, Head: n.Data}
}
tree.Root.Walk(func(n *mail.Node) bool {
	if len(n.Children) == 0 {
		if c := pgpmime.Classify(part(n)); c.Kind == pgpmime.KindArmored {
			res := pgpmime.Parse(n.Data, nil) // blocks, cleartext, packets
			_ = res.Summary()
		}
		return true
	}
	var kids []pgpmime.Part
	for _, c := range n.Children {
		kids = append(kids, part(c))
	}
	st := pgpmime.CheckStructure(part(n), kids)
	switch {
	case st.Kind == pgpmime.KindSigned && st.Content >= 0 && st.Signature >= 0:
		signed := n.Children[st.Content]
		var raw []byte
		if signed.HasSpan && !repairedUnder(sess.Diagnostics(), n.Path) {
			raw = input[signed.SpanStart:signed.SpanEnd]
		}
		req, issues := pgpmime.DetachedRequest(raw, n.Children[st.Signature].Data, st.Micalg, nil)
		// req == nil: unverifiable; else verifier.Verify(req)
	case st.Kind == pgpmime.KindEncrypted && st.Content >= 0:
		req, issues := pgpmime.EncryptedRequest(n.Children[st.Content].Data, nil)
		// req.Recipients are features; decrypter.Decrypt(req) if one is configured
	}
	return true
})
```

`repairedUnder` is the caller's test for a diagnostic with an `Action` whose
`Path` has the multipart's path as a prefix. `mail.ReadTree` now marks such
a container itself: `Node.Unverifiable` and `UnverifiableReason` on the
`multipart/signed` node (structure-view.md, "Signed content and repairs"). `Node.Data` is capped by
`TreeOptions.MaxBodyBytes` (`Node.Truncated` says so); a signature or an
encrypted payload larger than that cap must be read with the `Body` hook
instead.
