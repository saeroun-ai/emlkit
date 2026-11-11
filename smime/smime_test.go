package smime

import (
	"bytes"
	"compress/zlib"
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The vectors in testdata were made once with OpenSSL; testdata/README.md has
// the commands. No test runs a subprocess.

func vector(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pemCert(t testing.TB, name string) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode(vector(t, name))
	if blk == nil {
		t.Fatalf("%s: no PEM block", name)
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// emlBody is the body of one of OpenSSL's single-part messages: what follows
// the first blank line, still in its transfer encoding.
func emlBody(t testing.TB, name string) []byte {
	t.Helper()
	b := vector(t, name)
	i := bytes.Index(b, []byte("\n\n"))
	if i < 0 {
		t.Fatalf("%s: no header end", name)
	}
	return b[i+2:]
}

// trust returns verification options that trust the test CA at a time inside
// every test certificate's validity.
func trust(t testing.TB) *VerifyOptions {
	ca := pemCert(t, "ca.pem")
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return &VerifyOptions{X509: &x509.VerifyOptions{Roots: roots, CurrentTime: ca.NotBefore.Add(24 * time.Hour)}}
}

// Minimal DER and BER builders for hand-made cases.
func tlv(tag byte, parts ...[]byte) []byte {
	body := bytes.Join(parts, nil)
	out := []byte{tag}
	switch n := len(body); {
	case n < 0x80:
		out = append(out, byte(n))
	case n < 1<<8:
		out = append(out, 0x81, byte(n))
	case n < 1<<16:
		out = append(out, 0x82, byte(n>>8), byte(n))
	default:
		out = append(out, 0x83, byte(n>>16), byte(n>>8), byte(n))
	}
	return append(out, body...)
}

func indef(tag byte, parts ...[]byte) []byte {
	out := append([]byte{tag, 0x80}, bytes.Join(parts, nil)...)
	return append(out, 0, 0)
}

var (
	derOIDData       = []byte{0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x07, 0x01}
	derOIDSigned     = []byte{0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x07, 0x02}
	derOIDCompressed = []byte{0x06, 0x0b, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x09, 0x10, 0x01, 0x09}
	derOIDZlib       = []byte{0x06, 0x0b, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x09, 0x10, 0x03, 0x08}
)

func TestClassify(t *testing.T) {
	opaque := vector(t, "opaque-rsa.der")
	stream := vector(t, "opaque-rsa-stream.der")
	p7s := vector(t, "detached-rsa.p7s")
	tests := []struct {
		name      string
		mediaType string
		params    map[string]string
		filename  string
		head      []byte
		want      Kind
		source    string
		check     func(Classification) bool
	}{
		{"detached", "multipart/signed", map[string]string{"protocol": "application/pkcs7-signature", "micalg": "sha-256"}, "", nil,
			KindDetachedSignature, "protocol", func(c Classification) bool { return c.Micalg == "sha-256" && !c.Legacy }},
		{"detached x-", "Multipart/Signed", map[string]string{"Protocol": "application/x-pkcs7-signature"}, "", nil,
			KindDetachedSignature, "protocol", func(c Classification) bool { return c.Legacy }},
		{"pgp is not smime", "multipart/signed", map[string]string{"protocol": "application/pgp-signature"}, "", nil,
			KindNone, "", func(c Classification) bool { return !c.IsSMIME() }},
		{"declared signed-data", "application/pkcs7-mime", map[string]string{"smime-type": "signed-data"}, "smime.p7m", nil,
			KindSignedData, "smime-type", nil},
		{"declared and sniffed agree", "application/pkcs7-mime", map[string]string{"smime-type": "signed-data"}, "", opaque[:80],
			KindSignedData, "smime-type", func(c Classification) bool { return !c.Mismatch && c.ContentTypeOID == OIDSignedData }},
		{"mislabelled", "application/pkcs7-mime", map[string]string{"smime-type": "enveloped-data"}, "", opaque[:80],
			KindSignedData, "content", func(c Classification) bool { return c.Mismatch && c.Declared == KindEnvelopedData }},
		{"missing smime-type, sniffed", "application/pkcs7-mime", nil, "smime.p7m", vector(t, "enveloped-aes256cbc.der")[:40],
			KindEnvelopedData, "content", func(c Classification) bool { return !c.Mismatch }},
		{"missing smime-type, BER stream", "application/pkcs7-mime", map[string]string{"name": "smime.p7m"}, "", stream[:200],
			KindSignedData, "content", nil},
		{"x- auth enveloped", "application/x-pkcs7-mime", map[string]string{"smime-type": "authEnveloped-data"}, "", vector(t, "authenveloped-aes256gcm.der"),
			KindAuthEnvelopedData, "smime-type", func(c Classification) bool { return c.Legacy }},
		{"compressed declared", "application/pkcs7-mime", map[string]string{"SMIME-Type": "Compressed-Data"}, "", nil,
			KindCompressedData, "smime-type", nil},
		{"certs-only", "application/pkcs7-mime", map[string]string{"smime-type": "certs-only"}, "", vector(t, "certs-only.p7c"),
			KindCertsOnly, "smime-type", nil},
		{"certs-only sniffed", "application/pkcs7-mime", nil, "", vector(t, "certs-only.p7c")[:60],
			KindCertsOnly, "content", nil},
		{"nothing to go by", "application/pkcs7-mime", nil, "", nil,
			KindUnknown, "media-type", nil},
		{"p7z name", "application/pkcs7-mime", nil, "a.P7Z", nil,
			KindCompressedData, "filename", nil},
		{"signature part", "application/pkcs7-signature", map[string]string{"name": "smime.p7s"}, "", p7s[:80],
			KindSignature, "media-type", nil},
		{"signature part x-", "application/x-pkcs7-signature", nil, "", nil,
			KindSignature, "media-type", func(c Classification) bool { return c.Legacy }},
		{"signature part carrying content", "application/pkcs7-signature", nil, "", opaque[:80],
			KindSignedData, "content", func(c Classification) bool { return c.Mismatch }},
		{"generic p7m", "application/octet-stream", nil, "smime.p7m", opaque,
			KindSignedData, "content", func(c Classification) bool { return c.GenericType }},
		{"generic p7m, no bytes", "application/octet-stream", nil, "smime.p7m", nil,
			KindUnknown, "filename", nil},
		{"generic p7s", "application/octet-stream", nil, "SMIME.P7S", nil,
			KindSignature, "filename", nil},
		{"generic p7c", "", nil, "chain.p7c", nil,
			KindCertsOnly, "filename", nil},
		{"generic p7z", "application/octet-stream", nil, "x.p7z ", vector(t, "compressed.der")[:30],
			KindCompressedData, "filename", nil},
		{"generic other name", "application/octet-stream", nil, "report.pdf", opaque,
			KindNone, "", nil},
		{"text is not smime", "text/plain", nil, "smime.p7m", opaque,
			KindNone, "", nil},
		{"labelled but not CMS", "application/pkcs7-mime", map[string]string{"smime-type": "signed-data"}, "", []byte("Hello, this is no CMS."),
			KindSignedData, "smime-type", func(c Classification) bool { return c.NotCMS }},
		{"base64 text head", "application/pkcs7-mime", nil, "", emlBody(t, "opaque-rsa.eml")[:300],
			KindSignedData, "content", nil},
		{"pem head", "application/pkcs7-mime", nil, "", vector(t, "opaque-rsa.pem"),
			KindSignedData, "content", nil},
	}
	for _, tc := range tests {
		c := Classify(tc.mediaType, tc.params, tc.filename, tc.head)
		if c.Kind != tc.want || c.Source != tc.source {
			t.Errorf("%s: got %v from %q, want %v from %q", tc.name, c.Kind, c.Source, tc.want, tc.source)
		}
		if tc.check != nil && !tc.check(c) {
			t.Errorf("%s: unexpected classification %+v", tc.name, c)
		}
	}
}

func TestSniff(t *testing.T) {
	tests := []struct {
		file string
		n    int
		want Kind
	}{
		{"opaque-rsa.der", 0, KindSignedData},
		{"opaque-rsa.der", 30, KindSignedData},
		{"opaque-rsa.der", 15, KindSignedData},
		{"opaque-rsa.der", 14, KindNone},
		{"opaque-rsa-stream.der", 0, KindSignedData},
		{"detached-rsa.p7s", 0, KindSignature},
		{"detached-ec.p7s", 64, KindSignature},
		{"certs-only.p7c", 0, KindCertsOnly},
		{"compressed.der", 0, KindCompressedData},
		{"enveloped-aes256cbc.der", 0, KindEnvelopedData},
		{"enveloped-aes256cbc-stream.der", 0, KindEnvelopedData},
		{"authenveloped-aes256gcm.der", 0, KindAuthEnvelopedData},
		{"content.txt", 0, KindNone},
		{"ca.pem", 0, KindNone},
	}
	for _, tc := range tests {
		b := vector(t, tc.file)
		if tc.n > 0 {
			b = b[:tc.n]
		}
		if got, _ := Sniff(b); got != tc.want {
			t.Errorf("Sniff(%s[:%d]) = %v, want %v", tc.file, tc.n, got, tc.want)
		}
	}
	if k, oid := Sniff(tlv(0x30, derOIDData, tlv(0xa0, tlv(0x04, []byte("x"))))); k != KindUnknown || oid != OIDData {
		t.Errorf("id-data: got %v %q", k, oid)
	}
}

func TestExtractSignedContent(t *testing.T) {
	b64 := func(b []byte) []byte { return []byte(base64.StdEncoding.EncodeToString(b)) }
	opaque := vector(t, "opaque-rsa.der")
	tests := []struct {
		name    string
		data    []byte
		content string
		armor   Armor
		issues  []Code
	}{
		{"der", opaque, "content.txt", ArmorNone, nil},
		{"binary, bare LF kept", vector(t, "opaque-rsa-binary.der"), "content-lf.txt", ArmorNone, nil},
		{"stream: indefinite lengths, chunked", vector(t, "opaque-rsa-stream.der"), "content-big.txt", ArmorNone,
			[]Code{CodeBERIndefinite, CodeCMSChunked}},
		{"pem", vector(t, "opaque-rsa.pem"), "content.txt", ArmorPEM, []Code{CodeArmorPEM}},
		{"eml body, base64 lines", emlBody(t, "opaque-rsa.eml"), "content.txt", ArmorBase64, []Code{CodeArmorBase64}},
		{"double base64", b64(b64(opaque)), "content.txt", ArmorBase64, []Code{CodeArmorBase64, CodeArmorNested}},
		{"base64 without padding", bytes.TrimRight(b64(opaque), "="), "content.txt", ArmorBase64, nil},
		{"base64 with trailing text", append(b64(opaque), "\r\n-- \r\nsent by a list\r\n"...), "content.txt", ArmorBase64, []Code{CodeArmorTrailing}},
		{"trailing garbage", append(append([]byte{}, opaque...), "\r\n\r\n\x00\x00junk"...), "content.txt", ArmorNone, []Code{CodeCMSTrailing}},
		{"leading blank line", append([]byte("\r\n"), opaque...), "content.txt", ArmorNone, nil},
		{"ecdsa", vector(t, "opaque-ec.der"), "content.txt", ArmorNone, nil},
		{"no signed attributes", vector(t, "opaque-rsa-noattr.der"), "content.txt", ArmorNone, nil},
	}
	for _, tc := range tests {
		want := vector(t, tc.content)
		content, info, issues, err := ExtractSignedContent(tc.data, nil)
		if err != nil {
			t.Errorf("%s: %v (%v)", tc.name, err, issues)
			continue
		}
		if !bytes.Equal(content, want) {
			t.Errorf("%s: content differs: got %d bytes, want %d", tc.name, len(content), len(want))
		}
		if !info.HasContent || info.ContentTruncated || info.ContentType != OIDData || len(info.Signers) != 1 {
			t.Errorf("%s: unexpected info %+v", tc.name, info)
		}
		for _, code := range tc.issues {
			if !HasIssue(issues, code) {
				t.Errorf("%s: no %s issue in %v", tc.name, code, issues)
			}
		}
		for _, is := range issues {
			if is.Severity == Error {
				t.Errorf("%s: unexpected %v", tc.name, is)
			}
		}
		m, err := Parse(tc.data, nil)
		if err != nil || m.Armor != tc.armor || m.Kind != KindSignedData || !bytes.Equal(m.Content(), want) {
			t.Errorf("%s: Parse: err %v armor %v kind %v", tc.name, err, m.Armor, m.Kind)
		}

		var buf bytes.Buffer
		n, sinfo, _, err := WriteSignedContent(&buf, tc.data, nil)
		if err != nil || n != int64(len(want)) || !bytes.Equal(buf.Bytes(), want) || sinfo.Content != nil || sinfo.ContentTruncated {
			t.Errorf("%s: WriteSignedContent: n %d err %v", tc.name, n, err)
		}
	}

	// A detached signature has no content; other objects are not SignedData.
	content, info, issues, err := ExtractSignedContent(vector(t, "detached-rsa.p7s"), nil)
	if err != nil || content != nil || info.HasContent || !HasIssue(issues, CodeCMSContentAbsent) {
		t.Errorf("detached: content %q err %v issues %v", content, err, issues)
	}
	if _, _, _, err := ExtractSignedContent(vector(t, "enveloped-aes256cbc.der"), nil); !errors.Is(err, ErrNotSignedData) {
		t.Errorf("enveloped: err %v", err)
	}
	for _, junk := range []string{"", "   ", "Hello world", "-----BEGIN PKCS7-----", "\xff\xff\xff", "0", "MIME-Version: 1.0\r\n\r\nbody"} {
		if _, _, _, err := ExtractSignedContent([]byte(junk), nil); !errors.Is(err, ErrNotCMS) {
			t.Errorf("%q: err %v", junk, err)
		}
	}
	// A writer's error ends the reading and comes back.
	stop := errors.New("stop")
	if _, _, _, err := WriteSignedContent(writerFunc(func([]byte) (int, error) { return 0, stop }), opaque, nil); err != stop {
		t.Errorf("writer error: got %v", err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

func TestSignedDataMetadata(t *testing.T) {
	m, err := Parse(vector(t, "opaque-rsa.der"), nil)
	if err != nil {
		t.Fatal(err)
	}
	sd := m.Signed
	if sd.Version != 1 || len(sd.DigestAlgorithms) != 1 || sd.DigestAlgorithms[0].Name != "SHA-256" {
		t.Errorf("digest algorithms: %+v", sd.DigestAlgorithms)
	}
	if len(sd.Certificates) != 2 || sd.Certificates[0].Cert == nil || sd.Certificates[1].Cert == nil {
		t.Fatalf("certificates: %+v", sd.Certificates)
	}
	si := sd.Signers[0]
	if si.IssuerString() != "CN=emlkit test CA" || si.Serial == nil || si.SubjectKeyID != nil {
		t.Errorf("identifier: %q %v", si.IssuerString(), si.Serial)
	}
	if si.DigestAlgorithm.OID != oidSHA256 || si.SignatureAlgorithm.Name != "RSA" || len(si.Signature) != 256 {
		t.Errorf("algorithms: %v %v sig %d", si.DigestAlgorithm, si.SignatureAlgorithm, len(si.Signature))
	}
	if !si.HasSignedAttributes || si.ContentType != OIDData || len(si.MessageDigest) != 32 ||
		si.SigningTime.IsZero() || len(si.SMIMECapabilities) == 0 || len(si.SignedAttributes) < 4 {
		t.Errorf("signed attributes: %+v", si)
	}
	if y := si.SigningTime.Year(); y < 2020 || y > 2100 {
		t.Errorf("signing time %v", si.SigningTime)
	}

	m, err = Parse(vector(t, "opaque-rsa-keyid.der"), nil)
	if err != nil || len(m.Signed.Signers[0].SubjectKeyID) != 20 || m.Signed.Signers[0].Serial != nil {
		t.Errorf("keyid: %v %+v", err, m.Signed.Signers[0].Identifier)
	}
	m, err = Parse(vector(t, "certs-only.p7c"), nil)
	if err != nil || m.Kind != KindCertsOnly || len(m.Signed.Certificates) != 2 || len(m.Signed.Signers) != 0 ||
		!HasIssue(m.Issues, CodeCMSNoSigners) {
		t.Errorf("certs-only: %v %+v", err, m)
	}
	m, err = Parse(vector(t, "detached-rsa.p7s"), nil)
	if err != nil || m.Kind != KindSignature {
		t.Errorf("detached: %v %v", err, m.Kind)
	}

	// A certificate that does not parse is kept raw and reported.
	der := append([]byte{}, vector(t, "opaque-rsa.der")...)
	raw := sd.Certificates[0].Raw
	i := bytes.Index(der, raw)
	der[i+10] ^= 0xff // inside the TBSCertificate's first fields
	m, err = Parse(der, nil)
	if err != nil || len(m.Signed.Certificates) != 2 || m.Signed.Certificates[0].Cert != nil ||
		m.Signed.Certificates[0].Err == nil || len(m.Signed.Certificates[0].Raw) != len(raw) ||
		!HasIssue(m.Issues, CodeCMSCertificate) {
		t.Errorf("damaged certificate: %v %+v", err, m.Issues)
	}

	for _, tc := range []struct {
		tag  uint32
		in   string
		want string
	}{
		{tagUTCTime, "261111093000Z", "2026-11-11T09:30:00Z"},
		{tagUTCTime, "9912312359Z", "1999-12-31T23:59:00Z"},
		{tagUTCTime, "500101000000Z", "1950-01-01T00:00:00Z"},
		{tagUTCTime, "261111093000+0900", "2026-11-11T00:30:00Z"},
		{tagGeneralizedTime, "20261111093000Z", "2026-11-11T09:30:00Z"},
		{tagGeneralizedTime, "20261111093000.5Z", "2026-11-11T09:30:00.5Z"},
		{tagUTCTime, "not a time at all", ""},
		{tagInteger, "261111093000Z", ""},
	} {
		got, ok := parseTime(tc.tag, []byte(tc.in))
		if s := got.UTC().Format(time.RFC3339Nano); ok != (tc.want != "") || (ok && s != tc.want) {
			t.Errorf("parseTime(%q) = %v %v, want %q", tc.in, s, ok, tc.want)
		}
	}
	if OIDName(oidAES256CBC) != "AES-256-CBC" || OIDName(oidRSAPSS) != "RSASSA-PSS" || OIDName("1.2.3") != "" {
		t.Error("OIDName")
	}
}

func TestVerify(t *testing.T) {
	opts := trust(t)
	content := vector(t, "content.txt")
	tests := []struct {
		file     string
		detached string // content file of a detached signature
		signers  int
		sigAlg   string
		digest   Status
	}{
		{"opaque-rsa.der", "", 1, "RSA", Passed},
		{"opaque-rsa-binary.der", "", 1, "RSA", Passed},
		{"opaque-rsa-stream.der", "", 1, "RSA", Passed},
		{"opaque-rsa-sha1.der", "", 1, "RSA", Passed},
		{"opaque-rsa-sha384.der", "", 1, "RSA", Passed},
		{"opaque-rsa-sha512.der", "", 1, "RSA", Passed},
		{"opaque-rsa-keyid.der", "", 1, "RSA", Passed},
		{"opaque-rsa-noattr.der", "", 1, "RSA", NotChecked},
		{"opaque-pss.der", "", 1, "RSASSA-PSS", Passed},
		{"opaque-pss-sha512.der", "", 1, "RSASSA-PSS", Passed},
		{"opaque-ec.der", "", 1, "ECDSA-SHA256", Passed},
		{"opaque-ec-sha384.der", "", 1, "ECDSA-SHA384", Passed},
		{"opaque-ed25519.der", "", 1, "Ed25519", Passed},
		{"opaque-two-signers.der", "", 2, "", Passed},
		{"opaque-rsa.pem", "", 1, "RSA", Passed},
		{"detached-rsa.p7s", "content.txt", 1, "RSA", Passed},
		{"detached-ec.p7s", "content.txt", 1, "ECDSA-SHA256", Passed},
		{"detached-rsa-binary.p7s", "content-lf.txt", 1, "RSA", Passed},
	}
	for _, tc := range tests {
		m, err := Parse(vector(t, tc.file), nil)
		if err != nil {
			t.Errorf("%s: %v", tc.file, err)
			continue
		}
		var detached []byte
		if tc.detached != "" {
			detached = vector(t, tc.detached)
		}
		res := m.Signed.Verify(detached, opts)
		if len(res) != tc.signers {
			t.Errorf("%s: %d results", tc.file, len(res))
			continue
		}
		for i, r := range res {
			if r.Digest != tc.digest || r.Signature != Passed || r.Chain != Passed || !r.ContentIntact() ||
				r.Certificate == nil || r.ContentTypeMismatch || len(r.Chains) == 0 {
				t.Errorf("%s signer %d: digest %v (%s) signature %v (%s) chain %v (%v)", tc.file, i,
					r.Digest, r.DigestNote, r.Signature, r.SignatureNote, r.Chain, r.ChainErr)
			}
			if tc.sigAlg != "" && r.Signer.SignatureAlgorithm.Name != tc.sigAlg {
				t.Errorf("%s: signature algorithm %v", tc.file, r.Signer.SignatureAlgorithm)
			}
			if len(r.Certificate.EmailAddresses) != 1 || !strings.HasSuffix(r.Certificate.EmailAddresses[0], "@example.com") {
				t.Errorf("%s: certificate addresses %v", tc.file, r.Certificate.EmailAddresses)
			}
		}
	}

	// Each fact is reported on its own.
	one := func(name string, data, detached []byte, o *VerifyOptions) SignerResult {
		t.Helper()
		m, err := Parse(data, nil)
		if err != nil || len(m.Signed.Signers) == 0 {
			t.Fatalf("%s: %v", name, err)
		}
		return m.Signed.Verify(detached, o)[0]
	}
	flip := func(data, at []byte, off int) []byte {
		t.Helper()
		i := bytes.Index(data, at)
		if i < 0 {
			t.Fatal("pattern not found")
		}
		out := append([]byte{}, data...)
		out[i+off] ^= 0x01
		return out
	}
	opaque := vector(t, "opaque-rsa.der")
	info, _ := Parse(opaque, nil)
	sig := info.Signed.Signers[0].Signature
	digestAttr := info.Signed.Signers[0].MessageDigest

	r := one("tampered content", flip(opaque, []byte("Hello from"), 0), nil, opts)
	if r.Digest != Failed || r.Signature != Passed || r.Chain != Passed || r.ContentIntact() {
		t.Errorf("tampered content: digest %v signature %v chain %v", r.Digest, r.Signature, r.Chain)
	}
	r = one("tampered signature", flip(opaque, sig, 100), nil, opts)
	if r.Digest != Passed || r.Signature != Failed || r.Chain != Passed || r.ContentIntact() {
		t.Errorf("tampered signature: digest %v signature %v chain %v", r.Digest, r.Signature, r.Chain)
	}
	// Content and digest attribute changed together: only the signature tells.
	r = one("tampered attributes", flip(opaque, digestAttr, 3), nil, opts)
	if r.Digest != Failed || r.Signature != Failed {
		t.Errorf("tampered digest attribute: digest %v signature %v", r.Digest, r.Signature)
	}
	for _, f := range []string{"opaque-ec.der", "opaque-pss.der", "opaque-ed25519.der"} {
		v := vector(t, f)
		vi, _ := Parse(v, nil)
		r = one(f, flip(v, vi.Signed.Signers[0].Signature, 9), nil, opts)
		if r.Digest != Passed || r.Signature != Failed {
			t.Errorf("%s tampered signature: digest %v signature %v", f, r.Digest, r.Signature)
		}
		r = one(f, flip(v, []byte("Second line"), 2), nil, opts)
		if r.Digest != Failed || r.Signature != Passed {
			t.Errorf("%s tampered content: digest %v signature %v", f, r.Digest, r.Signature)
		}
	}
	noattr := vector(t, "opaque-rsa-noattr.der")
	r = one("noattr tampered", flip(noattr, []byte("Hello from"), 0), nil, opts)
	if r.Digest != NotChecked || r.Signature != Failed || r.ContentIntact() {
		t.Errorf("noattr tampered content: digest %v signature %v", r.Digest, r.Signature)
	}

	// Detached: the bytes must be the ones that were signed.
	p7s := vector(t, "detached-rsa.p7s")
	r = one("detached, no content", p7s, nil, opts)
	if r.Digest != NotChecked || r.Signature != Passed || r.ContentIntact() {
		t.Errorf("detached without content: digest %v signature %v", r.Digest, r.Signature)
	}
	r = one("detached, LF", p7s, bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n")), opts)
	if r.Digest != Failed || r.Signature != Passed {
		t.Errorf("detached with LF content: digest %v signature %v", r.Digest, r.Signature)
	}

	// A valid signature is not a trusted signer.
	r = one("no roots", opaque, nil, nil)
	if r.Digest != Passed || r.Signature != Passed || r.Chain != NotChecked {
		t.Errorf("no options: digest %v signature %v chain %v", r.Digest, r.Signature, r.Chain)
	}
	r = one("no roots in options", opaque, nil, &VerifyOptions{X509: &x509.VerifyOptions{}})
	if r.Chain != NotChecked {
		t.Errorf("options without roots: chain %v", r.Chain)
	}
	other := x509.NewCertPool()
	other.AddCert(pemCert(t, "recipient-test-cert.pem"))
	r = one("other root", opaque, nil, &VerifyOptions{X509: &x509.VerifyOptions{Roots: other, CurrentTime: opts.X509.CurrentTime}})
	if r.Digest != Passed || r.Signature != Passed || r.Chain != Failed || r.ChainErr == nil {
		t.Errorf("untrusted root: digest %v signature %v chain %v", r.Digest, r.Signature, r.Chain)
	}
	expired := *opts.X509
	expired.CurrentTime = expired.CurrentTime.AddDate(30, 0, 0)
	r = one("expired", opaque, nil, &VerifyOptions{X509: &expired})
	if r.Signature != Passed || r.Chain != Failed {
		t.Errorf("expired: signature %v chain %v", r.Signature, r.Chain)
	}

	// A signature sent without certificates needs one from the caller.
	nocerts := vector(t, "opaque-rsa-nocerts.der")
	r = one("nocerts", nocerts, nil, opts)
	if r.Certificate != nil || r.Digest != Passed || r.Signature != NotChecked || r.Chain != NotChecked {
		t.Errorf("no certificates: digest %v signature %v chain %v", r.Digest, r.Signature, r.Chain)
	}
	with := *opts
	with.Certificates = []*x509.Certificate{pemCert(t, "ca.pem"), pemCert(t, "rsa.pem")}
	r = one("nocerts, supplied", nocerts, nil, &with)
	if r.Certificate == nil || r.Digest != Passed || r.Signature != Passed || r.Chain != Passed {
		t.Errorf("supplied certificate: digest %v signature %v (%s) chain %v", r.Digest, r.Signature, r.SignatureNote, r.Chain)
	}
}

func TestBEREdgeCases(t *testing.T) {
	// SignedData in BER throughout: indefinite lengths at every level and a
	// content chunked two levels deep.
	chunked := indef(0x24,
		tlv(0x04, []byte("Hel")),
		indef(0x24, tlv(0x04, []byte("lo ")), tlv(0x24, tlv(0x04, []byte("wo")), tlv(0x04, nil))),
		tlv(0x04, []byte("rld")))
	signed := func(content []byte) []byte {
		return indef(0x30, derOIDSigned, indef(0xa0, indef(0x30,
			tlv(0x02, []byte{1}), tlv(0x31),
			indef(0x30, derOIDData, indef(0xa0, content)),
			tlv(0x31))))
	}
	m, err := Parse(signed(chunked), nil)
	if err != nil || string(m.Content()) != "Hello world" || m.Signed.ContentTruncated || !m.Indefinite || m.Truncated {
		t.Fatalf("chunked: %v %q %v", err, m.Content(), m.Issues)
	}
	for _, code := range []Code{CodeBERIndefinite, CodeCMSChunked, CodeCMSNoSigners} {
		if !HasIssue(m.Issues, code) {
			t.Errorf("chunked: no %s in %v", code, m.Issues)
		}
	}
	// A chunk of another type is skipped and reported.
	m, _ = Parse(signed(indef(0x24, tlv(0x04, []byte("a")), tlv(0x02, []byte{7}), tlv(0x04, []byte("b")))), nil)
	if string(m.Content()) != "ab" || !HasIssue(m.Issues, CodeCMSChunkInvalid) {
		t.Errorf("foreign chunk: %q %v", m.Content(), m.Issues)
	}
	// PKCS #7 content that is no OCTET STRING: its content octets are kept.
	m, _ = Parse(signed(tlv(0x30, tlv(0x02, []byte{5}))), nil)
	if !bytes.Equal(m.Content(), []byte{0x02, 0x01, 0x05}) || !HasIssue(m.Issues, CodeCMSContentNotOctets) {
		t.Errorf("non-octet content: %x %v", m.Content(), m.Issues)
	}
	// A missing end-of-contents is truncation; what arrived is returned.
	whole := signed(chunked)
	m, err = Parse(whole[:len(whole)-9], nil)
	if err != nil || string(m.Content()) != "Hello world" || !m.Truncated || !HasIssue(m.Issues, CodeBERTruncated) {
		t.Errorf("missing EOC: %v %q %v", err, m.Content(), m.Issues)
	}

	deep := bytes.Repeat([]byte{0x30, 0x80}, 500)
	big := signed(indef(0x24, bytes.Repeat(tlv(0x04, bytes.Repeat([]byte("x"), 100)), 50)))
	tests := []struct {
		name   string
		data   []byte
		limits *Limits
		code   Code
		err    error
	}{
		{"length past the input", append([]byte{0x30, 0x84, 0x7f, 0xff, 0xff, 0xff}, derOIDSigned...), nil, CodeBERTruncated, nil},
		{"length of 2^64-1", append([]byte{0x30, 0x88, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, derOIDSigned...), nil, CodeBERTruncated, nil},
		{"length overflowing 64 bits", append([]byte{0x30, 0x89, 1, 0, 0, 0, 0, 0, 0, 0, 0}, derOIDSigned...), nil, CodeBERTruncated, nil},
		{"absurd inner length", tlv(0x30, derOIDSigned, tlv(0xa0, []byte{0x30, 0x84, 0xff, 0xff, 0xff, 0xff, 0x02, 0x01, 0x01}), tlv(0x05)), nil, CodeBERLength, nil},
		{"length with leading zeros", append([]byte{0x30, 0x85, 0, 0, 0, 0, byte(len(derOIDSigned))}, derOIDSigned...), nil, CodeCMSMissing, nil},
		{"reserved length octet", []byte{0x30, 0xff, 0x06, 0x01, 0x01}, nil, CodeBERLengthInvalid, ErrNotCMS},
		{"length octets cut off", []byte{0x30, 0x84, 0x01}, nil, CodeBERTruncated, ErrNotCMS},
		{"indefinite primitive", tlv(0x30, derOIDSigned, tlv(0xa0, []byte{0x04, 0x80, 0x41, 0, 0})), nil, CodeBERIndefPrimitive, nil},
		{"tag number too long", tlv(0x30, derOIDSigned, tlv(0xa0, []byte{0x3f, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f, 0x00})), nil, CodeBERTag, nil},
		{"end-of-contents in a definite element", tlv(0x30, derOIDSigned, tlv(0xa0, tlv(0x30, []byte{0, 0}, tlv(0x02, []byte{1})))), nil, CodeBERUnexpectedEOC, nil},
		{"depth, at the top", deep, nil, CodeLimitDepth, ErrNotCMS},
		{"depth, inside", tlv(0x30, derOIDSigned, tlv(0xa0, deep)), nil, CodeLimitDepth, nil},
		{"depth budget of 3", signed(chunked), &Limits{MaxDepth: 3}, CodeLimitDepth, nil},
		{"element budget", big, &Limits{MaxElements: 20}, CodeLimitElements, ErrLimitExceeded},
		{"content budget", big, &Limits{MaxContentBytes: 250}, CodeLimitContent, nil},
		{"input budget", vector(t, "opaque-rsa.der"), &Limits{MaxInputBytes: 400}, CodeLimitInput, nil},
		{"unknown content type", tlv(0x30, []byte{0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x07, 0x05}, tlv(0xa0, tlv(0x30))), nil, CodeCMSUnknownType, nil},
	}
	for _, tc := range tests {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		m, err := Parse(tc.data, tc.limits)
		runtime.ReadMemStats(&after)
		if !errors.Is(err, tc.err) || (tc.err == nil && err != nil) {
			t.Errorf("%s: err %v, want %v", tc.name, err, tc.err)
		}
		if !HasIssue(m.Issues, tc.code) {
			t.Errorf("%s: no %s in %v", tc.name, tc.code, m.Issues)
		}
		// Nothing is ever allocated from a declared length.
		if grown := after.TotalAlloc - before.TotalAlloc; grown > 1<<20 {
			t.Errorf("%s: allocated %d bytes", tc.name, grown)
		}
	}
	m, _ = Parse(big, &Limits{MaxContentBytes: 250})
	if len(m.Content()) != 250 || !m.Signed.ContentTruncated {
		t.Errorf("content budget: %d bytes, truncated %v", len(m.Content()), m.Signed.ContentTruncated)
	}
	var buf bytes.Buffer
	if n, info, _, err := WriteSignedContent(&buf, big, &Limits{MaxContentBytes: 250}); n != 250 || buf.Len() != 250 || !info.ContentTruncated || err != nil {
		t.Errorf("content budget, streamed: %d bytes, %v", n, err)
	}
	// The issue list itself is bounded.
	many := tlv(0x30, derOIDSigned, tlv(0xa0, tlv(0x30, tlv(0x02, []byte{1}), tlv(0x31), tlv(0x30, derOIDData),
		tlv(0xa0, bytes.Repeat(tlv(0xa1, []byte{1}), 500)), tlv(0x31))))
	m, _ = Parse(many, &Limits{MaxIssues: 10, MaxCertificates: 1000})
	if len(m.Issues) != 10 || m.Issues[9].Code != CodeLimitIssues {
		t.Errorf("issue budget: %d issues, last %v", len(m.Issues), m.Issues[len(m.Issues)-1])
	}
	m, _ = Parse(many, nil)
	if len(m.Signed.Certificates) != defaultMaxCerts || !HasIssue(m.Issues, CodeLimitCertificates) {
		t.Errorf("certificate budget: %d kept", len(m.Signed.Certificates))
	}

	for _, tc := range []struct {
		in   []byte
		want string
		ok   bool
	}{
		{[]byte{0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x07, 0x02}, OIDSignedData, true},
		{[]byte{0x55, 0x04, 0x03}, "2.5.4.3", true},
		{[]byte{0x88, 0x37}, "2.999", true},
		{[]byte{0x2a, 0x86}, "", false},
		{nil, "", false},
		{append([]byte{0x2a}, bytes.Repeat([]byte{0xff}, 12)...), "", false},
	} {
		if got, ok := decodeOID(tc.in); got != tc.want || ok != tc.ok {
			t.Errorf("decodeOID(%x) = %q %v", tc.in, got, ok)
		}
	}
}

// Every prefix of an object parses without a panic and yields a prefix of the
// content; only the whole object is reported whole.
func TestTruncation(t *testing.T) {
	for _, tc := range []struct {
		file, content string
		step          int
	}{
		{"opaque-rsa.der", "content.txt", 1},
		{"opaque-rsa-noattr.der", "content.txt", 3},
		{"opaque-rsa-stream.der", "content-big.txt", 5},
		{"opaque-rsa.pem", "content.txt", 7},
	} {
		data := vector(t, tc.file)
		want := vector(t, tc.content)
		sawPartial := false
		for n := 0; n <= len(data); n += tc.step {
			m, err := Parse(data[:n], nil)
			if err != nil {
				if !errors.Is(err, ErrNotCMS) || n > 48 {
					t.Fatalf("%s[:%d]: %v", tc.file, n, err)
				}
				continue
			}
			if m.Signed == nil {
				t.Fatalf("%s[:%d]: no SignedData", tc.file, n)
			}
			got := m.Signed.Content
			if !bytes.HasPrefix(want, got) {
				t.Fatalf("%s[:%d]: content is no prefix of the original", tc.file, n)
			}
			if len(got) > 0 && len(got) < len(want) {
				sawPartial = true
				if !m.Signed.ContentTruncated {
					t.Fatalf("%s[:%d]: partial content not marked truncated", tc.file, n)
				}
			}
			if tc.file != "opaque-rsa.pem" && n < len(data) && (!m.Truncated || !HasIssue(m.Issues, CodeBERTruncated)) {
				t.Fatalf("%s[:%d]: truncation not reported: %v", tc.file, n, m.Issues)
			}
			if n%40 == 0 {
				for _, r := range m.Signed.Verify(nil, nil) {
					if r.ContentIntact() && len(got) < len(want) {
						t.Fatalf("%s[:%d]: truncated content verified", tc.file, n)
					}
				}
			}
		}
		if !sawPartial {
			t.Errorf("%s: no prefix yielded partial content", tc.file)
		}
	}
	for _, file := range []string{"enveloped-aes256cbc-stream.der", "authenveloped-aes256gcm.der", "enveloped-ecdh.der", "compressed.der", "certs-only.p7c"} {
		data := vector(t, file)
		for n := 0; n < len(data); n += 3 {
			m, err := Parse(data[:n], nil)
			if err == nil && !m.Truncated {
				t.Fatalf("%s[:%d]: truncation not reported", file, n)
			}
		}
	}
}

func TestCompressedData(t *testing.T) {
	want := vector(t, "content-big.txt")
	m, err := Parse(vector(t, "compressed.der"), nil)
	if err != nil || m.Kind != KindCompressedData || !bytes.Equal(m.Content(), want) ||
		m.Compressed.ContentTruncated || m.Compressed.CompressionAlgorithm.Name != "zlib" || m.Compressed.ContentType != OIDData {
		t.Fatalf("compressed: %v %+v", err, m.Issues)
	}
	m, _ = Parse(vector(t, "compressed.der"), &Limits{MaxInflatedBytes: 1000})
	if !bytes.Equal(m.Content(), want[:1000]) || !m.Compressed.ContentTruncated || !HasIssue(m.Issues, CodeLimitInflated) {
		t.Errorf("inflate budget: %d bytes %v", len(m.Content()), m.Issues)
	}

	wrap := func(alg, stream []byte) []byte {
		return tlv(0x30, derOIDCompressed, tlv(0xa0, tlv(0x30, tlv(0x02, []byte{0}), tlv(0x30, alg),
			tlv(0x30, derOIDData, tlv(0xa0, tlv(0x04, stream))))))
	}
	// A bomb: 8 MiB of zeros in a few kilobytes inflates only to the budget.
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(make([]byte, 8<<20))
	zw.Close()
	if z.Len() > 16<<10 {
		t.Fatalf("bomb is %d bytes", z.Len())
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	m, err = Parse(wrap(derOIDZlib, z.Bytes()), &Limits{MaxInflatedBytes: 64 << 10})
	runtime.ReadMemStats(&after)
	if err != nil || len(m.Content()) != 64<<10 || !m.Compressed.ContentTruncated || !HasIssue(m.Issues, CodeLimitInflated) {
		t.Errorf("bomb: %v %d bytes %v", err, len(m.Content()), m.Issues)
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 2<<20 {
		t.Errorf("bomb: allocated %d bytes", grown)
	}
	// A stream cut short yields what inflated; a non-stream yields nothing.
	m, _ = Parse(wrap(derOIDZlib, z.Bytes()[:z.Len()/2]), nil)
	if len(m.Content()) == 0 || !m.Compressed.ContentTruncated || !HasIssue(m.Issues, CodeCMSInflate) {
		t.Errorf("cut stream: %d bytes %v", len(m.Content()), m.Issues)
	}
	m, _ = Parse(wrap(derOIDZlib, []byte("not zlib")), nil)
	if m.Content() != nil || !HasIssue(m.Issues, CodeCMSInflate) {
		t.Errorf("not zlib: %v", m.Issues)
	}
	m, _ = Parse(wrap(derOIDData, z.Bytes()), nil)
	if m.Content() != nil || len(m.Compressed.Compressed) != z.Len() || !HasIssue(m.Issues, CodeCMSCompression) {
		t.Errorf("unknown compression: %v", m.Issues)
	}
}

func TestEnvelopedData(t *testing.T) {
	blk, _ := pem.Decode(vector(t, "recipient-test-key.pem"))
	key, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		file    string
		kind    Kind
		rkind   RecipientKind
		keyAlg  string
		encAlg  string
		content string // empty: the test key is not a recipient
		chunked bool
	}{
		{"enveloped-aes256cbc.der", KindEnvelopedData, RecipientKeyTransport, "RSA", "AES-256-CBC", "content.txt", false},
		{"enveloped-aes128cbc-oaep-keyid.der", KindEnvelopedData, RecipientKeyTransport, "RSAES-OAEP", "AES-128-CBC", "content.txt", false},
		{"enveloped-aes256cbc-stream.der", KindEnvelopedData, RecipientKeyTransport, "RSA", "AES-256-CBC", "content-big.txt", true},
		{"authenveloped-aes256gcm.der", KindAuthEnvelopedData, RecipientKeyTransport, "RSA", "AES-256-GCM", "content.txt", false},
		{"enveloped-ecdh.der", KindEnvelopedData, RecipientKeyAgreement, "", "AES-256-CBC", "", false},
		{"enveloped.eml", KindEnvelopedData, RecipientKeyTransport, "RSA", "AES-256-CBC", "content.txt", false},
	}
	for _, tc := range tests {
		data := vector(t, tc.file)
		if strings.HasSuffix(tc.file, ".eml") {
			data = emlBody(t, tc.file)
		}
		m, err := Parse(data, nil)
		if err != nil || m.Kind != tc.kind || m.Enveloped == nil {
			t.Errorf("%s: %v kind %v", tc.file, err, m.Kind)
			continue
		}
		ed := m.Enveloped
		if len(ed.Recipients) != 1 || ed.Recipients[0].Kind != tc.rkind || len(ed.Recipients[0].EncryptedKey) == 0 ||
			(tc.keyAlg != "" && ed.Recipients[0].KeyEncryptionAlgorithm.Name != tc.keyAlg) ||
			(ed.Recipients[0].Serial == nil && ed.Recipients[0].SubjectKeyID == nil) {
			t.Errorf("%s: recipients %+v", tc.file, ed.Recipients)
		}
		if ed.ContentEncryptionAlgorithm.Name != tc.encAlg || !ed.HasEncryptedContent || ed.ContentTruncated ||
			len(ed.EncryptedContent) == 0 || ed.ContentType != OIDData || HasIssue(m.Issues, CodeCMSChunked) != tc.chunked {
			t.Errorf("%s: content %v, %d bytes, issues %v", tc.file, ed.ContentEncryptionAlgorithm, len(ed.EncryptedContent), m.Issues)
		}
		if m.Content() != nil {
			t.Errorf("%s: encrypted content offered as content", tc.file)
		}
		rsaDec := &RSADecrypter{Certificate: pemCert(t, "recipient-test-cert.pem"), Key: key.(crypto.Decrypter)}
		got, err := m.Decrypt(rsaDec)
		if tc.content == "" {
			if !errors.Is(err, ErrNoRecipient) {
				t.Errorf("%s: decrypt: %v", tc.file, err)
			}
			continue
		}
		if err != nil || !bytes.Equal(got, vector(t, tc.content)) {
			t.Errorf("%s: decrypt: %v, %d bytes", tc.file, err, len(got))
		}
		// Without a certificate to choose by, recipients are tried in turn.
		rsaDec.Certificate = nil
		if got, err := m.Decrypt(rsaDec); err != nil || !bytes.Equal(got, vector(t, tc.content)) {
			t.Errorf("%s: blind decrypt: %v", tc.file, err)
		}
		// Damaged ciphertext does not decrypt to something else silently.
		if tc.kind == KindAuthEnvelopedData {
			ed.EncryptedContent = append([]byte{}, ed.EncryptedContent...)
			ed.EncryptedContent[0] ^= 1
			if _, err := m.Decrypt(rsaDec); err == nil {
				t.Errorf("%s: tampered GCM content decrypted", tc.file)
			}
		}
	}
	m, _ := Parse(vector(t, "opaque-rsa.der"), nil)
	if _, err := m.Decrypt(&RSADecrypter{Key: key.(crypto.Decrypter)}); !errors.Is(err, ErrNotEnveloped) {
		t.Errorf("decrypting a SignedData: %v", err)
	}
}

// oidDER encodes a dotted object identifier as an OBJECT IDENTIFIER element.
func oidDER(t testing.TB, dotted string) []byte {
	t.Helper()
	var arcs []uint64
	for _, s := range strings.Split(dotted, ".") {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		arcs = append(arcs, v)
	}
	arcs = append([]uint64{arcs[0]*40 + arcs[1]}, arcs[2:]...)
	var body []byte
	for _, v := range arcs {
		var enc []byte
		for first := true; first || v > 0; first = false {
			b := byte(v & 0x7f)
			if !first {
				b |= 0x80
			}
			enc = append([]byte{b}, enc...)
			v >>= 7
		}
		body = append(body, enc...)
	}
	if got, ok := decodeOID(body); !ok || got != dotted {
		t.Fatalf("oidDER(%s) decodes to %q", dotted, got)
	}
	return tlv(0x06, body)
}

// handBuilt are small objects with the parts OpenSSL's vectors do not have:
// every recipient kind, unsigned attributes, a signer of a SignedData with no
// certificates. They are also FuzzParse's seeds.
func handBuilt(t testing.TB) (signed, enveloped, compressed []byte) {
	o := func(s string) []byte { return oidDER(t, s) }
	alg := func(oid string, params ...[]byte) []byte { return tlv(0x30, append([][]byte{o(oid)}, params...)...) }
	attr := func(oid string, value []byte) []byte { return tlv(0x30, o(oid), tlv(0x31, value)) }
	name := tlv(0x30, tlv(0x31, tlv(0x30, o("2.5.4.3"), tlv(0x0c, []byte("emlkit test")))))

	signer := tlv(0x30,
		tlv(0x02, []byte{1}),
		tlv(0x30, name, tlv(0x02, []byte{0x00, 0xfe})),
		alg(oidSHA256),
		tlv(0xa0,
			attr(OIDAttrContentType, o(OIDData)),
			attr(OIDAttrSigningTime, tlv(0x17, []byte("261111093000Z"))),
			attr(OIDAttrMessageDigest, tlv(0x04, bytes.Repeat([]byte{7}, 32))),
			attr(OIDAttrMessageDigest, tlv(0x04, []byte{1}))),
		alg(oidRSA, tlv(0x05)),
		tlv(0x04, bytes.Repeat([]byte{9}, 16)),
		tlv(0xa1, attr(OIDAttrCounterSignature, tlv(0x30)), attr(OIDAttrTimeStampToken, tlv(0x30))))
	signed = tlv(0x30, o(OIDSignedData), tlv(0xa0, tlv(0x30,
		tlv(0x02, []byte{1}),
		tlv(0x31, alg(oidSHA256)),
		tlv(0x30, o(OIDData), tlv(0xa0, tlv(0x04, []byte("Subject: hi\r\n\r\nhello\r\n")))),
		tlv(0xa1, tlv(0x30)),
		tlv(0x31, signer))))

	ias := tlv(0x30, name, tlv(0x02, []byte{0x80}))
	enveloped = tlv(0x30, o(OIDEnvelopedData), tlv(0xa0, tlv(0x30,
		tlv(0x02, []byte{2}),
		tlv(0xa0, tlv(0xa0)),
		tlv(0x31,
			tlv(0x30, tlv(0x02, []byte{2}), tlv(0x80, []byte("skid")), alg(oidRSA, tlv(0x05)), tlv(0x04, []byte("key0"))),
			tlv(0xa1, tlv(0x02, []byte{3}), tlv(0xa0, tlv(0x04, []byte{1})), tlv(0xa1, tlv(0x04, []byte{2})),
				alg("1.3.132.1.11.1", alg("2.16.840.1.101.3.4.1.45")),
				tlv(0x30,
					tlv(0x30, ias, tlv(0x04, []byte("key1"))),
					tlv(0x30, tlv(0xa0, tlv(0x04, []byte("rkid"))), tlv(0x04, []byte("key2"))))),
			tlv(0xa2, tlv(0x02, []byte{4}), tlv(0x30, tlv(0x04, []byte("kekid"))), alg("2.16.840.1.101.3.4.1.45"), tlv(0x04, []byte("key3"))),
			tlv(0xa3, tlv(0x02, []byte{0}), tlv(0xa0, o("1.2.840.113549.1.5.12")), alg("1.2.840.113549.1.9.16.3.9"), tlv(0x04, []byte("key4"))),
			tlv(0xa4, o("1.2.3.4"), tlv(0x04, nil))),
		tlv(0x30, o(OIDData), alg(oidAES256CBC, tlv(0x04, make([]byte, 16))),
			tlv(0xa0, tlv(0x04, make([]byte, 8)), tlv(0x04, make([]byte, 8)))))))

	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write([]byte("Subject: hi\r\n\r\nhello hello hello hello\r\n"))
	zw.Close()
	compressed = tlv(0x30, o(OIDCompressedData), tlv(0xa0, tlv(0x30, tlv(0x02, []byte{0}), alg(oidZlibCompress),
		tlv(0x30, o(OIDData), tlv(0xa0, tlv(0x04, z.Bytes()))))))
	return signed, enveloped, compressed
}

func TestHandBuilt(t *testing.T) {
	signed, enveloped, compressed := handBuilt(t)

	m, err := Parse(signed, nil)
	if err != nil || m.Kind != KindSignedData || string(m.Content()) != "Subject: hi\r\n\r\nhello\r\n" || !m.Signed.HasCRLs {
		t.Fatalf("signed: %v %+v", err, m)
	}
	si := m.Signed.Signers[0]
	if si.IssuerString() != "CN=emlkit test" || si.Serial.Int64() != 254 || len(si.SignedAttributes) != 4 ||
		len(si.MessageDigest) != 32 || si.SigningTime.Year() != 2026 || si.ContentType != OIDData ||
		!si.HasCounterSignature || !si.HasTimeStamp || len(si.UnsignedAttributes) != 2 ||
		!HasIssue(m.Issues, CodeCMSAttributeRepeat) {
		t.Errorf("signer: %+v\n%v", si, m.Issues)
	}
	// No certificate: the digest can be compared, the signature cannot.
	r := m.Signed.Verify(nil, nil)[0]
	if r.Digest != Failed || r.Signature != NotChecked || r.Certificate != nil || r.ContentIntact() {
		t.Errorf("verify: %+v", r)
	}

	m, err = Parse(enveloped, nil)
	if err != nil || m.Kind != KindEnvelopedData || !m.Enveloped.HasOriginatorInfo || m.Enveloped.Version != 2 {
		t.Fatalf("enveloped: %v %+v", err, m)
	}
	want := []struct {
		kind RecipientKind
		alg  string
		key  string
		id   string
	}{
		{RecipientKeyTransport, "RSA", "key0", "skid"},
		{RecipientKeyAgreement, "ECDH-SHA256-KDF", "key1", ""},
		{RecipientKeyAgreement, "ECDH-SHA256-KDF", "key2", "rkid"},
		{RecipientKEK, "AES-256-WRAP", "key3", "kekid"},
		{RecipientPassword, "PWRI-KEK", "key4", ""},
		{RecipientOther, "", "", ""},
	}
	rs := m.Enveloped.Recipients
	if len(rs) != len(want) {
		t.Fatalf("recipients: %+v", rs)
	}
	for i, w := range want {
		if rs[i].Kind != w.kind || rs[i].KeyEncryptionAlgorithm.Name != w.alg || string(rs[i].EncryptedKey) != w.key ||
			string(rs[i].SubjectKeyID) != w.id {
			t.Errorf("recipient %d (%v): %+v", i, w.kind, rs[i])
		}
	}
	// A negative serial is read the way x509 reads it.
	if rs[1].Serial == nil || rs[1].Serial.Int64() != -128 || rs[1].IssuerString() != "CN=emlkit test" {
		t.Errorf("key agreement identifier: %+v", rs[1].Identifier)
	}
	if len(m.Enveloped.EncryptedContent) != 16 || !HasIssue(m.Issues, CodeCMSChunked) {
		t.Errorf("encrypted content: %d bytes, %v", len(m.Enveloped.EncryptedContent), m.Issues)
	}
	if _, err := m.Decrypt(&RSADecrypter{}); err == nil {
		t.Error("decrypting without a key")
	}

	m, err = Parse(compressed, nil)
	if err != nil || !strings.HasSuffix(string(m.Content()), "hello hello hello hello\r\n") {
		t.Errorf("compressed: %v %q", err, m.Content())
	}
	m, _ = Parse(enveloped, &Limits{MaxRecipients: 2, MaxAttributes: 1, MaxSigners: 1})
	if len(m.Enveloped.Recipients) != 2 || !HasIssue(m.Issues, CodeLimitRecipients) {
		t.Errorf("recipient budget: %d kept, %v", len(m.Enveloped.Recipients), m.Issues)
	}
	m, _ = Parse(signed, &Limits{MaxAttributes: 1})
	if len(m.Signed.Signers[0].SignedAttributes) != 1 || !HasIssue(m.Issues, CodeLimitAttributes) {
		t.Errorf("attribute budget: %v", m.Issues)
	}
}

// fuzzSeeds are the vectors TestMutations damages. FuzzParse starts from
// small objects instead: the fuzzer's minimizer is quadratic in the size of an
// input, and stalls for its whole run on one of two kilobytes.
var fuzzSeeds = []string{
	"opaque-rsa.der", "opaque-rsa-noattr.der", "opaque-ec.der", "opaque-pss.der", "opaque-ed25519.der",
	"opaque-rsa-keyid.der", "detached-rsa.p7s", "certs-only.p7c", "compressed.der",
	"enveloped-aes256cbc.der", "enveloped-ecdh.der", "authenveloped-aes256gcm.der", "opaque-rsa.pem",
}

func FuzzParse(f *testing.F) {
	signed, enveloped, compressed := handBuilt(f)
	f.Add(signed)
	f.Add(enveloped)
	f.Add(compressed)
	f.Add([]byte(base64.StdEncoding.EncodeToString(compressed)))
	f.Add(indef(0x30, derOIDSigned, indef(0xa0, indef(0x30, tlv(0x02, []byte{1}), tlv(0x31),
		indef(0x30, derOIDData, indef(0xa0, indef(0x24, tlv(0x04, []byte("ab")), indef(0x24, tlv(0x04, []byte("cd")))))),
		tlv(0x31)))))
	f.Add(vector(f, "authenveloped-aes256gcm.der"))
	f.Add(bytes.Repeat([]byte{0x30, 0x80}, 40))
	f.Add([]byte("MIIB"))
	f.Fuzz(fuzzBody)
}

// fuzzBody is what FuzzParse asks of one input, of which it reads at most
// 64 KiB: no panic, bounded results, and the same content either way.
func fuzzBody(t *testing.T, data []byte) {
	lim := &Limits{MaxElements: 1 << 14, MaxContentBytes: 1 << 20, MaxInflatedBytes: 1 << 20}
	{
		if len(data) > 64<<10 {
			data = data[:64<<10]
		}
		Sniff(data)
		Classify("application/pkcs7-mime", nil, "smime.p7m", data)
		m, err := Parse(data, lim)
		if err != nil {
			if m == nil || m.Signed != nil || m.Enveloped != nil || m.Compressed != nil {
				t.Fatalf("error with a result: %v", err)
			}
			return
		}
		if len(m.Issues) > defaultMaxIssues {
			t.Fatalf("%d issues", len(m.Issues))
		}
		switch {
		case m.Signed != nil:
			// The content is made of bytes of the input, nothing more.
			if len(m.Signed.Content) > len(data) {
				t.Fatalf("content of %d bytes from %d", len(m.Signed.Content), len(data))
			}
			for _, r := range m.Signed.Verify(nil, nil) {
				_ = r.ContentIntact()
			}
			m.Signed.Verify([]byte("detached"), nil)
			for _, s := range m.Signed.Signers {
				_ = s.IssuerString()
			}
			var buf bytes.Buffer
			n, _, _, err := WriteSignedContent(&buf, data, lim)
			if err != nil || n != int64(buf.Len()) || !bytes.Equal(buf.Bytes(), m.Signed.Content) {
				t.Fatalf("streamed content differs: %d bytes against %d, %v", n, len(m.Signed.Content), err)
			}
		case m.Enveloped != nil:
			if len(m.Enveloped.EncryptedContent) > len(data) {
				t.Fatal("encrypted content larger than the input")
			}
		case m.Compressed != nil:
			if len(m.Compressed.Content) > 1<<20 {
				t.Fatal("inflated past the budget")
			}
		}
	}
}

// Deterministic mutations of the vectors: the cheap, always-on cousin of
// FuzzParse.
func TestMutations(t *testing.T) {
	rng := rand.New(rand.NewPCG(140, 4))
	interesting := []byte{0x00, 0x80, 0x81, 0x84, 0xff, 0x30, 0x24, 0xa0, 0x04, 0x7f}
	var slowest time.Duration
	var slowName string
	seeds := append([]string{"opaque-rsa-stream.der", "opaque-two-signers.der", "enveloped-aes256cbc-stream.der"}, fuzzSeeds...)
	for _, name := range seeds {
		orig := vector(t, name)
		for i := 0; i < 250; i++ {
			d := append([]byte{}, orig...)
			for k := rng.IntN(3) + 1; k > 0 && len(d) > 4; k-- {
				at := rng.IntN(len(d))
				switch rng.IntN(9) {
				case 6:
					d[at] += byte(rng.IntN(7) - 3)
				case 7:
					junk := make([]byte, rng.IntN(12)+1)
					for j := range junk {
						junk[j] = byte(rng.Uint32())
					}
					d = append(d[:at], append(junk, d[at:]...)...)
				case 8:
					other := vector(t, seeds[rng.IntN(len(seeds))])
					d = append(d[:at], other[rng.IntN(len(other)):]...)
				case 0:
					d[at] ^= 1 << rng.IntN(8)
				case 1:
					d[at] = interesting[rng.IntN(len(interesting))]
				case 2:
					d = d[:at]
				case 3:
					d = append(d[:at], d[at+min(rng.IntN(len(d)-at)+1, 64):]...)
				case 4:
					n := min(rng.IntN(len(d)-at)+1, 200)
					d = append(d[:at], append(append([]byte{}, d[at:at+n]...), d[at:]...)...)
				case 5:
					d = append(d[:at], append([]byte{0x30, 0x80, 0x24, 0x80}, d[at:]...)...)
				}
			}
			start := time.Now()
			fuzzBody(t, d)
			if el := time.Since(start); el > slowest {
				slowest, slowName = el, fmt.Sprintf("%s #%d (%d bytes)", name, i, len(d))
			}
		}
	}
	t.Logf("slowest input: %v, %s", slowest, slowName)
}
