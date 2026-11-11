# pgpmime test vectors

All vectors except `rfc9580-v6-cert.asc` were made on 2026-11-11 with GnuPG
2.4.9 and a throwaway key generated for them in a temporary `GNUPGHOME`,
which was deleted afterwards. Only the public key is kept here. The key has
no passphrase and no other use.

| Key | Fingerprint | Key ID |
| --- | --- | --- |
| primary, Ed25519 (EdDSA legacy, algorithm 22) | `D306F995C9FCAAB40FFF7309A2227DD0135091D2` | `A2227DD0135091D2` |
| subkey, Curve25519 (ECDH, algorithm 18) | `2322AA96A15FAF9E638A79AD916B8BCC8805901D` | `916B8BCC8805901D` |

User ID: `emlkit test <test@example.com>`.

## Plaintext inputs

```sh
printf '안녕하세요, emlkit 시험 메시지입니다.\n- 대시로 시작하는 줄\n-----BEGIN PGP MESSAGE-----\nFrom the desk of the tester   \n끝.\n' > cleartext.txt
printf 'Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n서명된 PGP/MIME 본문입니다.\r\n- dash line\r\n' > signed-part.txt
printf 'Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n암호화된 비밀 메시지입니다.\r\n' > encrypted-part.txt
printf '압축되고 서명된 메시지\n' > compressed.txt
```

`cleartext.txt` has a line that starts with a dash, an armor line and a
`From ` line (gpg dash-escapes all three) and trailing spaces (kept in the
output, not signed).

## Key and vectors

```sh
export GNUPGHOME=$(mktemp -d)        # a fresh, empty home
G="timeout 60 gpg --batch --yes --pinentry-mode loopback --passphrase="
$G --quick-gen-key "emlkit test <test@example.com>" ed25519 sign never
$G --quick-add-key D306F995C9FCAAB40FFF7309A2227DD0135091D2 cv25519 encr never
KEY=test@example.com

$G --armor --export $KEY > pubkey.asc
$G --local-user $KEY --digest-algo SHA256 --clearsign -o clearsigned.asc cleartext.txt
$G --local-user $KEY --digest-algo SHA256 --armor --detach-sign -o detached.asc signed-part.txt
$G --trust-model always --recipient $KEY --armor --encrypt -o encrypted.asc encrypted-part.txt
$G --local-user $KEY --digest-algo SHA256 --compress-algo zlib --armor --sign -o signed-zlib.asc compressed.txt
$G --local-user $KEY --digest-algo SHA512 --compress-algo bzip2 --sign -o signed-bzip2.gpg compressed.txt
$G --local-user $KEY --compress-algo zip --sign -o signed-zip.gpg compressed.txt
timeout 60 gpg --batch --yes --pinentry-mode loopback --passphrase test --symmetric --armor -o symmetric.asc compressed.txt
```

`symmetric.asc` is encrypted with the passphrase `test`.

## PGP/MIME messages

`signed.eml` is a `multipart/signed` (RFC 3156 §5) whose first part is
`signed-part.txt` byte for byte, followed by the CR LF that belongs to the
next delimiter, and whose second part is `detached.asc` with CR LF line
endings. `encrypted.eml` is a `multipart/encrypted` (RFC 3156 §4) with the
`Version: 1` control part and `encrypted.asc` with CR LF line endings as its
payload. Both were assembled with `printf` and `sed 's/$/\r/'`; every header
line is in the files.

## RFC 9580 sample

`rfc9580-v6-cert.asc` is the sample version 6 certificate of RFC 9580
Appendix A.3 (Ed25519 primary key
`CB186C4F0609A697E4D52DFA6C722B0C1F1E27C18A56708F6525EC27BAD9ACC9`, X25519
subkey `12C83F1E706F6308FE151A417743A1F033790E93E9978488D1DB378DA9930885`).

## What the tests check

The tests verify the Ed25519 signatures of `clearsigned.asc`, `detached.asc`
(over the first part of `signed.eml`, as the parser frames it),
`signed-zlib.asc`, `signed-bzip2.gpg` and `signed-zip.gpg` with
`crypto/ed25519` and the key in `pubkey.asc`, as an example Verifier. Nothing
is decrypted: that would need the secret key, which is not kept.
