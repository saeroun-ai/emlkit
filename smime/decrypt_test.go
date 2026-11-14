package smime

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"errors"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"
)

// The decryption tests make their recipient key when they run and keep it in
// memory only; no private key is stored in the repository (#68). The
// EnvelopedData they decrypt is built here with the standard library, covering
// what OpenSSL's vectors in testdata cover. Those vectors are read for their
// structure only: nobody holds their key any more.

type testRecipient struct {
	key  *rsa.PrivateKey
	cert *x509.Certificate
}

var (
	recipientOnce sync.Once
	recipient     testRecipient
	recipientErr  error
)

// runtimeRecipient returns an RSA key and a self-signed certificate for it,
// made once per test binary.
func runtimeRecipient(t testing.TB) testRecipient {
	t.Helper()
	recipientOnce.Do(func() {
		var key *rsa.PrivateKey
		if key, recipientErr = rsa.GenerateKey(rand.Reader, 2048); recipientErr != nil {
			return
		}
		now := time.Now()
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(0x5eed01),
			Subject:      pkix.Name{CommonName: "emlkit runtime recipient"},
			NotBefore:    now.Add(-time.Hour),
			NotAfter:     now.Add(time.Hour),
			KeyUsage:     x509.KeyUsageKeyEncipherment,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageEmailProtection},
			SubjectKeyId: []byte("emlkit runtime recipient key"),
		}
		var der []byte
		if der, recipientErr = x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key); recipientErr != nil {
			return
		}
		var cert *x509.Certificate
		if cert, recipientErr = x509.ParseCertificate(der); recipientErr != nil {
			return
		}
		recipient = testRecipient{key: key, cert: cert}
	})
	if recipientErr != nil {
		t.Fatal(recipientErr)
	}
	return recipient
}

// envelope says how buildEnvelope encrypts.
type envelope struct {
	oaep      bool        // RSAES-OAEP rather than RSA PKCS #1 v1.5
	oaepHash  crypto.Hash // SHA-256 spelled out; 0 for the defaults (SHA-1), omitted
	keyID     bool        // the recipient named by subject key identifier
	keyLen    int         // AES key size: 16, 24 or 32
	gcm       bool        // AuthEnvelopedData with AES-GCM, rather than AES-CBC
	authAttrs bool        // with authenticated attributes
	chunk     int         // > 0: BER with indefinite lengths, content in chunks
	decoy     bool        // another recipient first, which r.cert does not match
}

// buildEnvelope encrypts content for r as an EnvelopedData (or, with gcm, an
// AuthEnvelopedData) ContentInfo.
func buildEnvelope(t testing.TB, r testRecipient, content []byte, e envelope) []byte {
	t.Helper()
	o := func(s string) []byte { return oidDER(t, s) }
	alg := func(oid string, params ...[]byte) []byte { return tlv(0x30, append([][]byte{o(oid)}, params...)...) }
	null := tlv(0x05)
	must := func(b []byte, err error) []byte {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	cek := must(randBytes(e.keyLen))

	// The recipient: its identifier, key encryption and encrypted key.
	version, rid := []byte{0}, tlv(0x30, r.cert.RawIssuer, must(asn1.Marshal(r.cert.SerialNumber)))
	if e.keyID {
		version, rid = []byte{2}, tlv(0x80, r.cert.SubjectKeyId)
	}
	keyAlg := alg(oidRSA, null)
	var encKey []byte
	if e.oaep {
		h, params := crypto.SHA1, tlv(0x30)
		if e.oaepHash == crypto.SHA256 {
			h = crypto.SHA256
			params = tlv(0x30, tlv(0xa0, alg(oidSHA256, null)), tlv(0xa1, alg(oidMGF1, alg(oidSHA256, null))))
		}
		keyAlg = alg(oidRSAOAEP, params)
		encKey = must(rsa.EncryptOAEP(h.New(), rand.Reader, &r.key.PublicKey, cek, nil))
	} else {
		encKey = must(rsa.EncryptPKCS1v15(rand.Reader, &r.key.PublicKey, cek))
	}
	recipients := [][]byte{tlv(0x30, tlv(0x02, version), rid, keyAlg, tlv(0x04, encKey))}
	if e.decoy {
		recipients = append([][]byte{tlv(0x30, tlv(0x02, []byte{2}), tlv(0x80, []byte("someone else")),
			alg(oidRSA, null), tlv(0x04, bytes.Repeat([]byte{0x5a}, 256)))}, recipients...)
	}

	// The content.
	block, err := aes.NewCipher(cek)
	if err != nil {
		t.Fatal(err)
	}
	var encAlg, ct, mac, attrs []byte
	if e.gcm {
		oid := map[int]string{16: oidAES128GCM, 24: oidAES192GCM, 32: oidAES256GCM}[e.keyLen]
		nonce := must(randBytes(12))
		encAlg = alg(oid, tlv(0x30, tlv(0x04, nonce), tlv(0x02, []byte{16})))
		aead, err := cipher.NewGCM(block)
		if err != nil {
			t.Fatal(err)
		}
		var aad []byte
		if e.authAttrs {
			attrs = tlv(0x30, o(OIDAttrContentType), tlv(0x31, o(OIDData)))
			aad = tlv(0x31, attrs)
		}
		sealed := aead.Seal(nil, nonce, content, aad)
		ct, mac = sealed[:len(content)], sealed[len(content):]
	} else {
		oid := map[int]string{16: oidAES128CBC, 24: oidAES192CBC, 32: oidAES256CBC}[e.keyLen]
		iv := must(randBytes(aes.BlockSize))
		encAlg = alg(oid, tlv(0x04, iv))
		pad := aes.BlockSize - len(content)%aes.BlockSize
		ct = append(bytes.Clone(content), bytes.Repeat([]byte{byte(pad)}, pad)...)
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, ct)
	}

	seq, ctx0 := func(p ...[]byte) []byte { return tlv(0x30, p...) }, func(p ...[]byte) []byte { return tlv(0xa0, p...) }
	encrypted := tlv(0x80, ct)
	if e.chunk > 0 {
		seq = func(p ...[]byte) []byte { return indef(0x30, p...) }
		ctx0 = func(p ...[]byte) []byte { return indef(0xa0, p...) }
		var chunks [][]byte
		for rest := ct; len(rest) > 0; {
			n := min(e.chunk, len(rest))
			chunks = append(chunks, tlv(0x04, rest[:n]))
			rest = rest[n:]
		}
		encrypted = indef(0xa0, chunks...)
	}
	eci := seq(o(OIDData), encAlg, encrypted)
	ri := tlv(0x31, recipients...)
	if !e.gcm {
		edVersion := []byte{0}
		if e.keyID || e.decoy {
			edVersion = []byte{2}
		}
		return seq(o(OIDEnvelopedData), ctx0(seq(tlv(0x02, edVersion), ri, eci)))
	}
	parts := [][]byte{tlv(0x02, []byte{0}), ri, eci}
	if attrs != nil {
		parts = append(parts, tlv(0xa1, attrs))
	}
	parts = append(parts, tlv(0x04, mac))
	return seq(o(OIDAuthEnvelopedData), ctx0(seq(parts...)))
}

func randBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	return b, err
}

func TestDecryptRuntime(t *testing.T) {
	r := runtimeRecipient(t)
	small, large := vector(t, "content.txt"), vector(t, "content-big.txt")
	tests := []struct {
		name    string
		e       envelope
		content []byte
		kind    Kind
		keyAlg  string
		encAlg  string
		armor   bool // sent as Base64 text, as an S/MIME message body is
	}{
		{"PKCS #1 v1.5, AES-256-CBC", envelope{keyLen: 32}, small, KindEnvelopedData, "RSA", "AES-256-CBC", false},
		{"OAEP defaults, key id, AES-128-CBC", envelope{oaep: true, keyID: true, keyLen: 16}, small, KindEnvelopedData, "RSAES-OAEP", "AES-128-CBC", false},
		{"OAEP SHA-256, AES-192-CBC", envelope{oaep: true, oaepHash: crypto.SHA256, keyLen: 24}, small, KindEnvelopedData, "RSAES-OAEP", "AES-192-CBC", false},
		{"streamed BER, AES-256-CBC", envelope{keyLen: 32, chunk: 4096}, large, KindEnvelopedData, "RSA", "AES-256-CBC", false},
		{"AES-256-GCM", envelope{keyLen: 32, gcm: true}, small, KindAuthEnvelopedData, "RSA", "AES-256-GCM", false},
		{"AES-128-GCM, authenticated attributes, OAEP", envelope{oaep: true, keyLen: 16, gcm: true, authAttrs: true}, small, KindAuthEnvelopedData, "RSAES-OAEP", "AES-128-GCM", false},
		{"Base64 message body", envelope{keyLen: 32}, small, KindEnvelopedData, "RSA", "AES-256-CBC", true},
		{"second of two recipients", envelope{keyLen: 32, decoy: true}, small, KindEnvelopedData, "RSA", "AES-256-CBC", false},
	}
	for _, tc := range tests {
		data := buildEnvelope(t, r, tc.content, tc.e)
		if tc.armor {
			var b strings.Builder
			s := base64.StdEncoding.EncodeToString(data)
			for len(s) > 64 {
				b.WriteString(s[:64])
				b.WriteString("\r\n")
				s = s[64:]
			}
			b.WriteString(s)
			b.WriteString("\r\n")
			data = []byte(b.String())
		}
		m, err := Parse(data, nil)
		if err != nil || m.Kind != tc.kind || m.Enveloped == nil {
			t.Errorf("%s: %v kind %v", tc.name, err, m.Kind)
			continue
		}
		ed := m.Enveloped
		ri := ed.Recipients[len(ed.Recipients)-1]
		if ri.Kind != RecipientKeyTransport || ri.KeyEncryptionAlgorithm.Name != tc.keyAlg || !ri.Matches(r.cert) ||
			ed.ContentEncryptionAlgorithm.Name != tc.encAlg || ed.ContentTruncated ||
			HasIssue(m.Issues, CodeCMSChunked) != (tc.e.chunk > 0) || (tc.armor != (m.Armor == ArmorBase64)) {
			t.Errorf("%s: recipient %+v, content %v, armor %v, issues %v", tc.name, ri, ed.ContentEncryptionAlgorithm, m.Armor, m.Issues)
		}
		dec := &RSADecrypter{Certificate: r.cert, Key: r.key}
		if got, err := m.Decrypt(dec); err != nil || !bytes.Equal(got, tc.content) {
			t.Errorf("%s: decrypt: %v, %d bytes", tc.name, err, len(got))
		}
		// Without a certificate to choose by, recipients are tried in turn.
		// The decoy is skipped here: a wrong PKCS #1 v1.5 key turns into a
		// random content key, which CBC padding rejects only most of the time.
		if !tc.e.decoy {
			if got, err := m.Decrypt(&RSADecrypter{Key: r.key}); err != nil || !bytes.Equal(got, tc.content) {
				t.Errorf("%s: blind decrypt: %v", tc.name, err)
			}
		}
		// Damaged GCM content or attributes do not decrypt silently.
		if tc.e.gcm {
			saved := ed.EncryptedContent
			ed.EncryptedContent = bytes.Clone(saved)
			ed.EncryptedContent[0] ^= 1
			if _, err := m.Decrypt(dec); err == nil {
				t.Errorf("%s: tampered GCM content decrypted", tc.name)
			}
			ed.EncryptedContent = saved
			if ed.AuthAttrs != nil {
				ed.AuthAttrs = bytes.Clone(ed.AuthAttrs)
				ed.AuthAttrs[len(ed.AuthAttrs)-1] ^= 1
				if _, err := m.Decrypt(dec); err == nil {
					t.Errorf("%s: tampered authenticated attributes decrypted", tc.name)
				}
			}
		}
		// A certificate that names no recipient selects none.
		if _, err := m.Decrypt(&RSADecrypter{Certificate: pemCert(t, "recipient-test-cert.pem"), Key: r.key}); !errors.Is(err, ErrNoRecipient) {
			t.Errorf("%s: decrypt for another certificate: %v", tc.name, err)
		}
	}

	// Damaged CBC content: a partial last block does not decrypt.
	m, _ := Parse(buildEnvelope(t, r, small, envelope{keyLen: 32}), nil)
	m.Enveloped.EncryptedContent = m.Enveloped.EncryptedContent[:len(m.Enveloped.EncryptedContent)-1]
	if _, err := m.Decrypt(&RSADecrypter{Certificate: r.cert, Key: r.key}); !errors.Is(err, errDecrypt) {
		t.Errorf("truncated CBC content: %v", err)
	}
	m, _ = Parse(vector(t, "opaque-rsa.der"), nil)
	if _, err := m.Decrypt(&RSADecrypter{Key: r.key}); !errors.Is(err, ErrNotEnveloped) {
		t.Errorf("decrypting a SignedData: %v", err)
	}
}
