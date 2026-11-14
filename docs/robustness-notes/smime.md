# S/MIME (ROB-140-004)

Code: `smime/` (`classify.go`, `ber.go`, `armor.go`, `cms.go`, `verify.go`,
`decrypt.go`, `oid.go`, `limits.go`, `issue.go`). Tests: `smime/smime_test.go`,
`smime/decrypt_test.go`, `smime/wiring_test.go`. Vectors: `smime/testdata/`
(made with OpenSSL; the commands are in its README). No private key is stored:
the decryption tests make theirs at run time (#68).

`smime` is an optional package. Nothing in `message`, `mail` or `textproto`
imports it, it imports none of them, and it uses the standard library only. It
has its own `Issue` type instead of `message.Diagnostic`. It works on plain
values, so the caller does the wiring (see the end of this note).

## Why it exists

An opaque-signed message (`application/pkcs7-mime; smime-type=signed-data`)
carries its whole body inside a CMS blob. To the MIME parser it is one Base64
attachment named `smime.p7m`; the text, links and attachments a classifier
wants are inside. `ExtractSignedContent` returns the inner MIME entity so that
it can be parsed like any message. That is the main use. Describing signers and
recipients comes second, verifying third, and decryption is only a hook.

## API

| Call | Result |
| --- | --- |
| `Classify(mediaType, params, filename, head)` | `Classification`: `Kind`, what decided it, `Mismatch`, `Legacy`, `NotCMS`, `Micalg` |
| `Sniff(head)` | the kind and CMS content type the first bytes declare |
| `Parse(data, limits)` | `*Message`: `Kind`, `Signed`, `Enveloped`, `Compressed`, `Issues`, `Truncated`, `Armor` |
| `ExtractSignedContent(data, limits)` | the encapsulated content, the `*SignedData`, issues |
| `WriteSignedContent(w, data, limits)` | the same, content written to `w` chunk by chunk |
| `(*SignedData).Verify(content, opts)` | one `SignerResult` per signer |
| `(*Message).Decrypt(Decrypter)` | plaintext of an EnvelopedData, through the caller's keys |
| `OIDName(oid)` | "SHA-256", "RSASSA-PSS", "AES-256-CBC", ... |

### Classification

| Input | Kind |
| --- | --- |
| `multipart/signed`, protocol `application/pkcs7-signature` or `x-pkcs7-signature` | `KindDetachedSignature` |
| `multipart/signed`, any other protocol (PGP/MIME) | `KindNone` |
| `application/pkcs7-signature`, `x-pkcs7-signature`, or a generic type named `.p7s` | `KindSignature` |
| `application/pkcs7-mime` or `x-pkcs7-mime` with `smime-type` | `signed-data`, `enveloped-data`, `authEnveloped-data`, `compressed-data`, `certs-only` |
| the same without `smime-type` | by the bytes; else by name (`.p7c`, `.p7z`); else `KindUnknown` |
| `application/octet-stream` (or no type) named `.p7m` / `.p7s` / `.p7c` / `.p7z` | by the bytes, else by name; `GenericType` set |

When bytes are given they decide: `Sniff` reads the CMS content type, and for
a SignedData looks far enough to tell opaque from detached from certs-only. A
label that disagrees with the bytes sets `Mismatch` (a part labelled
`enveloped-data` that is a readable SignedData is worth a feature of its own).
Bytes that are no CMS object at all set `NotCMS` and leave the labels' kind.
`x-` forms set `Legacy`.

### What is read from CMS (RFC 5652, 5083, 3274)

- **SignedData**: version, digest algorithms, encapsulated content type and
  content, certificates (parsed with `x509.ParseCertificate`; the raw bytes
  are always kept, and one that does not parse gets `Err` and an issue), CRL
  presence, and per signer: identifier (issuer and serial, or subject key
  id), digest and signature algorithms, signature, the signed attributes
  (content type, message digest, signing time, S/MIME capabilities raw, and
  the full list), unsigned attributes with flags for a counter-signature and
  a timestamp token.
- **EnvelopedData / AuthEnvelopedData**: recipients — key transport (issuer
  and serial or key id, algorithm, encrypted key), key agreement (one entry
  per recipient key), KEK, password, other — the content encryption algorithm
  with its parameters, the ciphertext if present, the MAC and authenticated
  attributes.
- **CompressedData**: algorithm, and the content inflated (zlib).
- Anything else (`digestedData`, `encryptedData`, ...): `KindUnknown` and an
  issue with the type.

## Tolerance rules

| Input | What happens | Issue |
| --- | --- | --- |
| BER indefinite lengths (Outlook, `openssl -stream`) | read | `ber.indefinite_length` (info) |
| content as a constructed OCTET STRING in chunks, nested or not | joined | `cms.chunked_content` (info) |
| a chunk that is not an OCTET STRING | skipped | `cms.chunk_invalid` |
| PEM instead of DER | decoded | `armor.pem` (info) |
| Base64 text instead of DER (a mailer encoded twice) | decoded, up to three layers | `armor.base64`, `armor.nested` |
| Base64 without padding, with line breaks, with text after it | decoded up to the first non-Base64 byte | `armor.trailing_data` |
| bytes after the object | ignored | `cms.trailing_data` |
| input ends inside the object | every field that arrived is returned; content so far with `ContentTruncated` | `ber.truncated` |
| a length larger than what remains | element clipped to what is there | `ber.truncated`, or `ber.length_exceeds_input` inside a parent |
| PKCS #7 content that is not an OCTET STRING | its content octets kept | `cms.content_not_octet_string` |
| certificate that does not parse | kept raw | `cms.certificate_invalid` |
| attribute or other certificate format | kept raw, `Other` | `cms.certificate_other` (info) |
| signed attributes not in DER | verified as sent | `cms.signed_attributes_not_der` |
| a required field absent | the rest is read | `cms.missing_field` |
| zlib stream damaged | what inflated is kept, `ContentTruncated` | `cms.inflate_failed` |

`Parse` returns an error only for input that is no CMS object (`ErrNotCMS`)
or when the element budget ran out before the outer structure was read
(`ErrLimitExceeded`). Everything else is a result plus issues.

## Budgets

The BER reader is the package's own (`encoding/asn1` reads DER only). An
element is four offsets into the input; nothing is copied and nothing is ever
allocated from a declared length. Lengths are accumulated in 64 bits with the
overflow caught before the shift, and only compared with the bytes that
remain. The one input-driven loop is finding the end of an indefinite-length
element; depth and a count of element headers bound it.

| `Limits` field | Default | Bounds |
| --- | --- | --- |
| `MaxInputBytes` | 64 MiB | input read; the rest is cut off and reported |
| `MaxDepth` | 32 (cap 128) | nesting of elements, and so the recursion |
| `MaxElements` | 1,048,576 | element headers read in one call (total work) |
| `MaxContentBytes` | 64 MiB | reassembled eContent or ciphertext |
| `MaxInflatedBytes` | 16 MiB | output of zlib (the bomb guard) |
| `MaxCertificates`, `MaxCertificateBytes` | 64, 64 KiB | certificates kept, and the size past which one is not parsed |
| `MaxSigners`, `MaxRecipients`, `MaxAttributes` | 16, 256, 64 | list lengths |
| `MaxIssues` | 100 | issues; the last slot becomes `limit.issues` |

Zero or negative selects the default; there is no unlimited setting. Reaching
a budget is an issue (`limit.*`) and a truncated result, not an error.

Memory: a primitive OCTET STRING is returned as a slice of the input. A
chunked one is copied into a buffer that grows with the bytes actually
present, so it is never larger than the input. Inflation grows the same way up
to `MaxInflatedBytes`. Results alias the input; do not modify it while they
are in use.

## Trust model: what "verified" means

`Verify` returns three separate statuses per signer, each `Passed`, `Failed`
or `NotChecked` (with a note saying why), and never one combined verdict:

| Field | Says | Does not say |
| --- | --- | --- |
| `Digest` | the content hashes to the signer's message-digest attribute | that anyone signed it |
| `Signature` | the signature over the signed attributes (or over the content, when there are none) is valid for the key of `Certificate` | whose key that is |
| `Chain` | `Certificate` chains to a root the caller supplied | that the signer is the message's author |

`ContentIntact()` is `Signature` passed and `Digest` passed (or not
applicable): the content is what the holder of that key signed. It is not
trust. The certificate comes from the message itself, and any sender can mint
one that says anything. A spam message with a perfectly valid self-made
signature is ordinary.

Identity needs both of the following, and the second is the caller's job:

1. `Chain == Passed`. Chains are built only when `VerifyOptions.X509` is given
   with `Roots`; the system trust store is never consulted implicitly. The
   certificates in the message are offered as intermediates. An empty
   `KeyUsages` means `ExtKeyUsageEmailProtection`. `CurrentTime` is the
   caller's; the signer's `SigningTime` is its own claim and is never used.
2. The certificate's addresses (`Certificate.EmailAddresses`) match the
   message's `From` or `Sender`. The package does not see the message header.

Not checked at all: revocation (CRLs in the message are noted, not read;
no OCSP), counter-signatures, timestamp tokens, `signingCertificate`
attributes, and whether the digest algorithm is one a policy still accepts
(SHA-1 verifies; the algorithm is reported so the caller can decide).

Algorithms: RSA PKCS #1 v1.5, RSASSA-PSS (MGF1 with the same hash) and ECDSA,
each with SHA-1, SHA-256, SHA-384 and SHA-512, and Ed25519. MD5, DSA, GOST and
anything else are named and reported `NotChecked`. Go refuses RSA keys under
1024 bits; such a signature is `Failed` with the library's reason.

RSASSA-PSS is held to the salt length its parameters declare (absent: 20).
A declared length of 0 means an empty salt, but Go's verifier has no option
for exactly zero — its 0 is "detect the length", which accepts any. So for a
declared 0, a signature that no salt length verifies is `Failed`, and one
that some salt length verifies is `NotChecked` ("cannot be checked exactly"),
never `Passed`: an honest zero-salt signature and one whose real salt is 32
bytes cannot be told apart through that API (#72). OpenSSL accepts the
first and refuses the second. `Digest` and `Chain` are not affected.

A signer certificate is found by the signer identifier among the message's
certificates and `VerifyOptions.Certificates`. Without one, `Signature` is
`NotChecked`.

### Detached signatures need the raw bytes

A `multipart/signed` signature covers the first part exactly as transmitted:
its header block and body, CRLF line endings, up to but not including the
line break before the boundary. Decoded text is not that. Pass the slice of
the original input given by `message.Entity.RawSpan`, or by
`mail.Node.SpanStart`/`SpanEnd` when `HasSpan` is set.

If the parser had to repair that part — a diagnostic of the parse lies at or
under its path with a severity above info, or the node has `Err` set, or there
is no span — the bytes the sender signed can no longer be identified with
certainty. Report such a part as **unverifiable**. A `Digest` failure there
means "could not check", not "forged". The same holds for a message that
passed through a gateway that re-wrapped lines or rewrote the part's header.

## Decryption

No key is built in and nothing is decrypted by `Parse`. `Decrypter` is the
hook: `Message.Decrypt(d)` hands the `EnvelopedData` to the caller's
implementation. `RSADecrypter` is a small one for the common case — RSA key
transport (PKCS #1 v1.5 with the session-key form that does not reveal padding
failures, or OAEP with MGF1) and AES-CBC, or AES-GCM in an AuthEnvelopedData.
Key agreement (ECDH), KEK and password recipients, 3DES and RC2 are described
but not decrypted.

Tests decrypt EnvelopedData and AuthEnvelopedData that `decrypt_test.go`
builds with the standard library for a key it makes at run time (PKCS #1 v1.5
and OAEP, AES-CBC also as streamed BER, AES-GCM with and without
authenticated attributes, a second recipient chosen by certificate). OpenSSL's
enveloped vectors are read for structure only: their key is not kept.

AES-CBC content has no integrity of its own. A service that tells a sender
whether a ciphertext decrypted cleanly is a padding oracle; a classifier that
never answers is not.

## Wiring to `mail.ReadTree` and `message.Parse`

The parser gives the node; the node gives plain values; `smime` gives bytes
back; the parser reads those bytes as a message. Layers nest (compressed
inside signed, signed inside enveloped), so bound the loop.

```go
import (
	"bytes"
	"slices"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
	"github.com/saeroun-ai/emlkit/smime"
)

// inner returns the message hidden in an S/MIME node, or nil. raw is the
// input the tree was parsed from.
func inner(sess *message.ParseSession, raw []byte, n *mail.Node, depth int) *mail.Tree {
	if depth > 4 {
		return nil // layers within layers: stop
	}
	c := smime.Classify(n.MediaType, n.Params, n.Filename, n.Data)
	switch c.Kind {
	case smime.KindSignedData, smime.KindCompressedData:
		if n.Truncated {
			// Node.Data is only the first TreeOptions.MaxBodyBytes; the
			// result below will say ContentTruncated.
		}
		m, err := smime.Parse(n.Data, nil)
		if err != nil || len(m.Content()) == 0 {
			return nil
		}
		// m.Issues, c.Mismatch, m.Signed.Signers are features in themselves.
		_, e, err := message.Parse(bytes.NewReader(m.Content()), nil)
		if err != nil {
			return nil
		}
		t, _ := mail.ReadTree(e, nil)
		if t != nil && t.Root != nil {
			if deeper := inner(sess, m.Content(), t.Root, depth+1); deeper != nil {
				return deeper
			}
		}
		return t

	case smime.KindDetachedSignature:
		if len(n.Children) != 2 {
			return nil
		}
		signed, sig := n.Children[0], n.Children[1]
		m, err := smime.Parse(sig.Data, nil)
		if err != nil || m.Signed == nil {
			return nil
		}
		if !signed.HasSpan || signed.Err != nil || repaired(sess, signed) {
			// Unverifiable: the signed bytes cannot be identified.
			return nil
		}
		for _, r := range m.Signed.Verify(raw[signed.SpanStart:signed.SpanEnd], nil) {
			_ = r.Digest    // the part is what was signed
			_ = r.Signature // ... by the key of r.Certificate
			_ = r.Chain     // NotChecked: no roots were given
		}
	}
	return nil
}

// repaired reports a parse diagnostic above info at or under the node.
func repaired(sess *message.ParseSession, n *mail.Node) bool {
	for _, d := range sess.Diagnostics() {
		if d.Severity > message.SeverityInfo && len(d.Path) >= len(n.Path) &&
			slices.Equal(d.Path[:len(n.Path)], n.Path) {
			return true
		}
	}
	return false
}
```

`smime/wiring_test.go` holds these two functions (returning the results
instead of dropping them) and runs them against OpenSSL's own messages: the
detached signature verifies over the raw span and fails over the decoded body,
and the opaque message re-parses to its text part.

`mail.ReadTree` now makes the `repaired` test itself: a `multipart/signed`
node is `Unverifiable`, with the reasons in `UnverifiableReason`, when a
repair lies in its signed part or signature part, its own framing or header
was repaired, it has no span, or its protocol or micalg does not fit
(structure-view.md, "Signed content and repairs"). Its rule differs from
the helper above: it also counts repairs of the signature part and of the
container's own framing and header, and it does not count Unsupported
diagnostics, which mark raw content passed through unread.

For an opaque part larger than `TreeOptions.MaxBodyBytes`, read the part with
`TreeOptions.Body` into a bounded buffer of your own, or use
`WriteSignedContent` to stream the content out without a second copy.

## Limitations

- The input is one byte slice. `WriteSignedContent` streams the output, not
  the input.
- Signed attributes are verified as transmitted; a signer that sent them in
  non-DER form and signed their DER re-encoding fails `Signature`.
- An RSASSA-PSS signature with a declared salt length of zero is at best
  `NotChecked` (see the trust model).
- `Sniff` reads the first 8 KiB.
- No signing, no encryption, no writing of CMS.
- A blob with more chunks than `MaxElements` allows is refused whole
  (`ErrLimitExceeded`) when its outer lengths are indefinite, because its end
  cannot be found within the budget.
- Not covered: CRL and OCSP checks, counter-signature and timestamp
  verification, signed receipts (RFC 2634), triple wrapping as a unit,
  `digestedData`, `encryptedData`, `authenticatedData`, MD5 and DSA
  signatures, ECDH/KEK/password decryption, 3DES/RC2. PGP/MIME is the
  `pgpmime` package's (pgpmime.md, ROB-140-005).

## 2026-11-13 follow-up review (#72, #68)

Codex reviewed the implementation and runtime-generated envelope tests,
independently reproduced the repaired salt-length result, and compared the
original, mismatched, genuine-zero and omitted-default signatures with
OpenSSL. The S/MIME race test, full unit suite, build, formatting and diff
checks passed on Linux amd64. No PEM private-key block remains; the literal
PGP private-key armor type names are parser syntax and remain in pgpmime.
Real-client fixtures (#69) remain unverified; ROB-140-004 stays Review.
