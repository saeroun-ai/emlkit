# S/MIME test vectors

Everything here was produced once, on 2026-11-11, with OpenSSL 3.5.9 by the
script below, run in an empty directory. The tests read these files and never
run OpenSSL.

The keys and certificates are throwaway ones made by the script for this
purpose only: subject `CN=emlkit test`, addresses under `example.com`, issued
by a throwaway `CN=emlkit test CA`. The CA's and the signers' private keys
were discarded. **`recipient-test-key.pem` is a test key**: the private key of
`recipient-test-cert.pem`, committed so that the decryption test can run. It
protects nothing; never use it for anything else.

| File | What it is |
| --- | --- |
| `content.txt`, `content-lf.txt`, `content-big.txt` | the signed, compressed and encrypted content: CRLF, bare LF, and 10 KiB |
| `ca.pem` | the root the signers' certificates chain to |
| `rsa.pem` | the RSA signer's certificate, for the vector that omits it |
| `recipient-test-cert.pem`, `recipient-test-key.pem` | the recipient of the enveloped vectors, and its **test key** |
| `opaque-*.der` | SignedData with the content inside: RSA PKCS #1 v1.5 (SHA-1/256/384/512), RSASSA-PSS, ECDSA P-256, Ed25519, two signers, by key identifier, without signed attributes, without certificates |
| `opaque-rsa-stream.der` | the same in BER: indefinite lengths, content in 4096-byte chunks |
| `opaque-rsa.pem`, `opaque-rsa.eml` | PEM, and a whole `application/pkcs7-mime` message |
| `detached-*.p7s`, `detached-rsa.eml` | detached signatures, and a whole `multipart/signed` message |
| `certs-only.p7c` | SignedData with certificates and no signer |
| `compressed.der` | CompressedData (zlib) |
| `enveloped-*.der`, `enveloped.eml` | EnvelopedData: RSA key transport (PKCS #1 v1.5 and OAEP), ECDH key agreement, AES-CBC, streamed BER |
| `authenveloped-aes256gcm.der` | AuthEnvelopedData, AES-256-GCM |

Every `opaque-*.der` was checked with `openssl cms -verify`, the loop at the
end of the script ("CMS Verification successful" for each).

```sh
#!/bin/bash
# Produces the S/MIME test vectors of smime/testdata. Run once, in an empty
# directory, with OpenSSL 3.x; the outputs are committed and no test runs this.
set -eu
cd "$1"
o() { timeout 60 openssl "$@"; }

cat > openssl.cnf <<'EOF'
[req]
distinguished_name = dn
prompt = no
[dn]
CN = emlkit test
[ca]
basicConstraints = critical, CA:TRUE
keyUsage = critical, keyCertSign, cRLSign
subjectKeyIdentifier = hash
[leaf_rsa]
basicConstraints = CA:FALSE
keyUsage = critical, digitalSignature, keyEncipherment
extendedKeyUsage = emailProtection
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid
subjectAltName = email:signer@example.com
[leaf_ec]
basicConstraints = CA:FALSE
keyUsage = critical, digitalSignature, keyAgreement
extendedKeyUsage = emailProtection
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid
subjectAltName = email:ec@example.com
[leaf_ed]
basicConstraints = CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = emailProtection
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid
subjectAltName = email:ed@example.com
[leaf_recipient]
basicConstraints = CA:FALSE
keyUsage = critical, keyEncipherment
extendedKeyUsage = emailProtection
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid
subjectAltName = email:recipient@example.com
EOF

# This host's crypto policy refuses to sign with SHA-1; the one SHA-1 vector is
# made, and checked, under a configuration that allows it.
cat > sha1.cnf <<'EOF'
openssl_conf = openssl_init
[openssl_init]
alg_section = evp_properties
[evp_properties]
rh-allow-sha1-signatures = yes
EOF

# Throwaway keys and certificates. Only recipient-test-key.pem is committed.
o req -config openssl.cnf -x509 -newkey rsa:2048 -nodes -keyout ca.key \
  -out ca.pem -subj "/CN=emlkit test CA" -days 3650 -extensions ca
leaf() { # name keyfile extension-section
  o req -config openssl.cnf -new -key "$2" -out "$1.csr" -subj "/CN=emlkit test"
  o x509 -req -in "$1.csr" -CA ca.pem -CAkey ca.key -CAcreateserial \
    -out "$1.pem" -days 3650 -extfile openssl.cnf -extensions "$3"
}
o genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out rsa.key
o genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out ec.key
o genpkey -algorithm ED25519 -out ed.key
o genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out recipient-test-key.pem
leaf rsa rsa.key leaf_rsa
leaf ec ec.key leaf_ec
leaf ed ed.key leaf_ed
leaf recipient recipient-test-key.pem leaf_recipient
cp recipient.pem recipient-test-cert.pem

# The signed content: a small MIME entity with CRLF line endings, the same
# entity with bare LF, and a larger one that streaming splits into chunks.
printf 'Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 7bit\r\n\r\nHello from the inside of an S/MIME blob.\r\nSecond line.\r\n' > content.txt
printf 'Content-Type: text/plain; charset=utf-8\n\nBare LF content, signed with -binary.\n' > content-lf.txt
{ printf 'Content-Type: text/plain\r\n\r\n'; i=0; while [ $i -lt 150 ]; do
    printf 'line %04d of a body long enough to be written in several chunks....\r\n' $i; i=$((i+1)); done; } > content-big.txt

S="-sign -signer rsa.pem -inkey rsa.key -certfile ca.pem"

# Opaque (encapsulated) signatures.
o cms $S -in content.txt -nodetach -noindef -outform DER -out opaque-rsa.der
o cms $S -in content-lf.txt -binary -nodetach -noindef -outform DER -out opaque-rsa-binary.der
o cms $S -in content-big.txt -binary -nodetach -stream -outform DER -out opaque-rsa-stream.der
o cms $S -in content.txt -nodetach -noindef -outform PEM -out opaque-rsa.pem
o cms $S -in content.txt -nodetach -noindef -outform SMIME -out opaque-rsa.eml \
  -from signer@example.com -to recipient@example.com -subject "opaque signed"
o cms $S -in content.txt -nodetach -noindef -noattr -outform DER -out opaque-rsa-noattr.der
o cms $S -in content.txt -nodetach -noindef -keyid -outform DER -out opaque-rsa-keyid.der
OPENSSL_CONF=$PWD/sha1.cnf o cms $S -in content.txt -nodetach -noindef -md sha1 \
  -outform DER -out opaque-rsa-sha1.der
o cms $S -in content.txt -nodetach -noindef -md sha384 -outform DER -out opaque-rsa-sha384.der
o cms $S -in content.txt -nodetach -noindef -md sha512 -outform DER -out opaque-rsa-sha512.der
o cms $S -in content.txt -nodetach -noindef -keyopt rsa_padding_mode:pss -outform DER -out opaque-pss.der
o cms $S -in content.txt -nodetach -noindef -keyopt rsa_padding_mode:pss -md sha512 \
  -keyopt rsa_pss_saltlen:32 -outform DER -out opaque-pss-sha512.der
o cms -sign -signer ec.pem -inkey ec.key -certfile ca.pem -in content.txt \
  -nodetach -noindef -outform DER -out opaque-ec.der
o cms -sign -signer ec.pem -inkey ec.key -certfile ca.pem -in content.txt \
  -nodetach -noindef -md sha384 -outform DER -out opaque-ec-sha384.der
o cms -sign -signer ed.pem -inkey ed.key -certfile ca.pem -in content.txt \
  -nodetach -noindef -md sha512 -outform DER -out opaque-ed25519.der
o cms -sign -signer rsa.pem -inkey rsa.key -signer ec.pem -inkey ec.key -certfile ca.pem \
  -in content.txt -nodetach -noindef -outform DER -out opaque-two-signers.der
o cms $S -in content.txt -nodetach -noindef -nocerts -outform DER -out opaque-rsa-nocerts.der

# Detached signatures: the multipart/signed message, and the bare .p7s.
o cms $S -in content.txt -outform SMIME -out detached-rsa.eml \
  -from signer@example.com -to recipient@example.com -subject "detached signed"
o cms $S -in content.txt -outform DER -out detached-rsa.p7s
o cms -sign -signer ec.pem -inkey ec.key -certfile ca.pem -in content.txt \
  -outform DER -out detached-ec.p7s
o cms $S -in content-lf.txt -binary -outform DER -out detached-rsa-binary.p7s

# Certificates only, compressed, enveloped.
o crl2pkcs7 -nocrl -certfile rsa.pem -certfile ca.pem -outform DER -out certs-only.p7c
o cms -compress -in content-big.txt -binary -outform DER -out compressed.der
o cms -encrypt -in content.txt -binary -aes-256-cbc -recip recipient.pem \
  -outform DER -out enveloped-aes256cbc.der
o cms -encrypt -in content.txt -binary -aes-128-cbc -recip recipient.pem -keyid \
  -keyopt rsa_padding_mode:oaep -outform DER -out enveloped-aes128cbc-oaep-keyid.der
o cms -encrypt -in content.txt -binary -aes-256-gcm -recip recipient.pem \
  -outform DER -out authenveloped-aes256gcm.der
o cms -encrypt -in content.txt -binary -aes-256-cbc -recip ec.pem \
  -outform DER -out enveloped-ecdh.der
o cms -encrypt -in content-big.txt -binary -aes-256-cbc -recip recipient.pem -stream \
  -outform DER -out enveloped-aes256cbc-stream.der
o cms -encrypt -in content.txt -aes-256-cbc -recip recipient.pem -outform SMIME \
  -out enveloped.eml -from signer@example.com -to recipient@example.com -subject "enveloped"

# OpenSSL's own verdicts, for the record. -certfile serves the vector that
# carries no certificates.
for f in opaque-*.der; do
  OPENSSL_CONF=$PWD/sha1.cnf o cms -verify -in "$f" -inform DER -CAfile ca.pem \
    -certfile rsa.pem -purpose any -binary -out /dev/null
done
```

Not committed: `ca.key`, `rsa.key`, `ec.key`, `ed.key`, the certificate
requests, `ec.pem`, `ed.pem` (both are inside the vectors that need them) and
the two configuration files. They were left in the generating session's
scratch directory and are not needed again: regenerating makes new keys and
new vectors. The verification loop at the end was run as a separate step after
the generation, with exactly that command.

The messages (`*.eml`) are as OpenSSL wrote them: their own header lines end in
bare LF while the signed part inside keeps its CRLF. That mix is deliberate
test material — the signed bytes are found by their raw span, not by
re-encoding.
