package pgpmime_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
	"github.com/saeroun-ai/emlkit/pgpmime"
)

const (
	primaryID   = pgpmime.KeyID(0xA2227DD0135091D2)
	subkeyID    = pgpmime.KeyID(0x916B8BCC8805901D)
	primaryFPR  = "D306F995C9FCAAB40FFF7309A2227DD0135091D2"
	literalText = "압축되고 서명된 메시지\n"
	clearText   = "안녕하세요, emlkit 시험 메시지입니다.\n- 대시로 시작하는 줄\n-----BEGIN PGP MESSAGE-----\nFrom the desk of the tester   \n끝."
)

func readVec(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tags lists the packet tags, each prefixed by one '>' per nesting level.
func tags(ps []pgpmime.Packet) string {
	var out []string
	for _, p := range ps {
		out = append(out, strings.Repeat(">", p.Depth)+p.Tag.String())
	}
	return strings.Join(out, " ")
}

// packetsOf returns the packets and issues of the single block or binary
// stream of a vector.
func packetsOf(t *testing.T, r *pgpmime.Result) ([]pgpmime.Packet, []pgpmime.Issue) {
	t.Helper()
	if r.Binary {
		return r.Packets, r.Issues
	}
	if len(r.Blocks) != 1 {
		t.Fatalf("%d blocks, want 1", len(r.Blocks))
	}
	b := r.Blocks[0]
	is := append(append([]pgpmime.Issue(nil), r.Issues...), b.Issues...)
	if b.Cleartext != nil {
		if b.Cleartext.Signature == nil {
			t.Fatal("no signature block")
		}
		return b.Cleartext.Signature.Packets, append(is, b.Cleartext.Signature.Issues...)
	}
	if b.CRC != pgpmime.CRCValid && b.CRC != pgpmime.CRCAbsent {
		t.Errorf("CRC %d", b.CRC)
	}
	return b.Packets, is
}

func TestVectors(t *testing.T) {
	tests := []struct {
		file   string
		binary bool
		armor  pgpmime.ArmorType
		tags   string
		allow  pgpmime.Code
		check  func(t *testing.T, r *pgpmime.Result, ps []pgpmime.Packet)
	}{
		{"pubkey.asc", false, pgpmime.ArmorPublicKey, "public-key user-id signature public-subkey signature", "",
			func(t *testing.T, r *pgpmime.Result, ps []pgpmime.Packet) {
				k, sub := ps[0].Key, ps[3].Key
				if k.KeyID != primaryID || k.Fingerprint.String() != primaryFPR || k.Algorithm != 22 || k.Curve != "Ed25519Legacy" || k.Subkey {
					t.Errorf("primary key %+v", k)
				}
				if sub.KeyID != subkeyID || sub.Algorithm != 18 || sub.Curve != "Curve25519Legacy" || !sub.Subkey {
					t.Errorf("subkey %+v", sub)
				}
				if ps[1].UserID != "emlkit test <test@example.com>" {
					t.Errorf("user ID %q", ps[1].UserID)
				}
				if s := ps[2].Signature; s.Type != pgpmime.SigCertPositive || s.IssuerKeyID != primaryID || s.Created.Unix() != 1794360705 {
					t.Errorf("certification %+v", s)
				}
				if s := ps[4].Signature; s.Type != pgpmime.SigSubkeyBind || s.IssuerFingerprint.String() != primaryFPR {
					t.Errorf("binding %+v", s)
				}
				sum := r.Summary()
				if len(sum.Keys) != 2 || sum.Signed || len(sum.UserIDs) != 1 {
					t.Errorf("summary %+v", sum)
				}
			}},
		{"clearsigned.asc", false, pgpmime.ArmorSignedMessage, "signature", "",
			func(t *testing.T, r *pgpmime.Result, ps []pgpmime.Packet) {
				c := r.Blocks[0].Cleartext
				if string(c.Text) != clearText || len(c.Hash) != 1 || c.Hash[0] != "SHA256" {
					t.Errorf("text %q hash %q", c.Text, c.Hash)
				}
				if s := ps[0].Signature; s.Type != pgpmime.SigText || s.Hash != 8 || s.IssuerKeyID != primaryID {
					t.Errorf("signature %+v", s)
				}
			}},
		{"detached.asc", false, pgpmime.ArmorSignature, "signature", "",
			func(t *testing.T, r *pgpmime.Result, ps []pgpmime.Packet) {
				s := ps[0].Signature
				if s.Type != pgpmime.SigBinary || s.Hash.Micalg() != "pgp-sha256" || s.SignerUserID != "test@example.com" ||
					s.IssuerFingerprint.String() != primaryFPR || s.PubKeyAlgo != 22 {
					t.Errorf("signature %+v", s)
				}
			}},
		{"encrypted.asc", false, pgpmime.ArmorMessage, "pkesk seipd", "",
			func(t *testing.T, r *pgpmime.Result, ps []pgpmime.Packet) {
				if k := ps[0].EncryptedKey; k.KeyID != subkeyID || k.Algorithm != 18 || k.Version != 3 {
					t.Errorf("session key %+v", k)
				}
				sum := r.Summary()
				if !sum.Encrypted || !sum.Protected || len(sum.Recipients) != 1 || sum.Recipients[0] != subkeyID {
					t.Errorf("summary %+v", sum)
				}
			}},
		{"symmetric.asc", false, pgpmime.ArmorMessage, "skesk seipd", "",
			func(t *testing.T, r *pgpmime.Result, ps []pgpmime.Packet) {
				if k := ps[0].SymmetricKey; k.Version != 4 || k.Cipher.String() != "AES256" || k.S2K != 3 {
					t.Errorf("symmetric key %+v", k)
				}
				if sum := r.Summary(); !sum.Passphrase || !sum.Protected {
					t.Errorf("summary %+v", sum)
				}
			}},
		{"signed-zlib.asc", false, pgpmime.ArmorMessage, "compressed >one-pass-signature >literal >signature", "", checkSigned("ZLIB")},
		{"signed-bzip2.gpg", true, 0, "compressed >one-pass-signature >literal >signature", "", checkSigned("BZip2")},
		{"signed-zip.gpg", true, 0, "compressed >one-pass-signature >literal >signature", "", checkSigned("ZIP")},
		{"rfc9580-v6-cert.asc", false, pgpmime.ArmorPublicKey, "public-key signature public-subkey signature", pgpmime.CodeCRCMissing,
			func(t *testing.T, r *pgpmime.Result, ps []pgpmime.Packet) {
				const fp = "CB186C4F0609A697E4D52DFA6C722B0C1F1E27C18A56708F6525EC27BAD9ACC9"
				if k := ps[0].Key; k.Version != 6 || k.Fingerprint.String() != fp || k.KeyID != 0xCB186C4F0609A697 || k.Curve != "Ed25519" {
					t.Errorf("v6 key %+v", k)
				}
				if s := ps[1].Signature; s.Version != 6 || s.IssuerFingerprint.String() != fp || s.IssuerKeyID != 0xCB186C4F0609A697 {
					t.Errorf("v6 signature %+v", s)
				}
				if k := ps[2].Key; k.Fingerprint.String() != "12C83F1E706F6308FE151A417743A1F033790E93E9978488D1DB378DA9930885" || k.Curve != "X25519" {
					t.Errorf("v6 subkey %+v", k)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			r := pgpmime.Parse(readVec(t, tt.file), nil)
			if r.Binary != tt.binary {
				t.Fatalf("Binary = %v", r.Binary)
			}
			if !tt.binary && r.Blocks[0].Type != tt.armor {
				t.Errorf("armor %v, want %v", r.Blocks[0].Type, tt.armor)
			}
			ps, is := packetsOf(t, r)
			for _, i := range is {
				if i.Code != tt.allow {
					t.Errorf("issue %v", i)
				}
			}
			if got := tags(ps); got != tt.tags {
				t.Fatalf("packets %q, want %q", got, tt.tags)
			}
			tt.check(t, r, ps)
		})
	}
}

func checkSigned(algo string) func(t *testing.T, r *pgpmime.Result, ps []pgpmime.Packet) {
	return func(t *testing.T, r *pgpmime.Result, ps []pgpmime.Packet) {
		if c := ps[0].Compressed; c.Algorithm.String() != algo || c.Truncated || c.Size == 0 {
			t.Errorf("compressed %+v", c)
		}
		if o := ps[1].OnePass; o.KeyID != primaryID || o.Nested {
			t.Errorf("one-pass %+v", o)
		}
		if l := ps[2].Literal; l.FileName != "compressed.txt" || string(l.Data) != literalText || l.Format != 'b' || l.Truncated {
			t.Errorf("literal %+v", l)
		}
		sum := r.Summary()
		if !sum.Signed || len(sum.Signers) != 1 || sum.Signers[0] != primaryID || sum.Encrypted {
			t.Errorf("summary %+v", sum)
		}
	}
}

// edVerifier is an example Verifier, for version 4 EdDSA signatures over
// SHA-256 or SHA-512, built on crypto/ed25519 alone.
type edVerifier struct{ pub ed25519.PublicKey }

func newEdVerifier(t *testing.T) *edVerifier {
	r := pgpmime.Parse(readVec(t, "pubkey.asc"), nil)
	b := r.Blocks[0].Packets[0].Body // version, time, algorithm, OID, MPI
	mpi := b[7+int(b[6]):]
	if mpi[2] != 0x40 {
		t.Fatal("not a native Ed25519 point")
	}
	return &edVerifier{pub: ed25519.PublicKey(mpi[3:35])}
}

func (v *edVerifier) Verify(req *pgpmime.VerifyRequest) ([]pgpmime.Verification, error) {
	ps, _ := pgpmime.ParsePackets(req.Signature, nil)
	signed := req.Signed
	if req.Mode == pgpmime.ModeInline {
		for _, p := range ps {
			if p.Literal != nil {
				signed = p.Literal.Data
			}
		}
	}
	var out []pgpmime.Verification
	for _, p := range ps {
		if p.Signature != nil {
			out = append(out, v.check(p, signed))
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no signature")
	}
	return out, nil
}

func (v *edVerifier) check(p pgpmime.Packet, data []byte) pgpmime.Verification {
	res := pgpmime.Verification{KeyID: p.Signature.IssuerKeyID, Created: p.Signature.Created}
	b := p.Body
	if b[0] != 4 || b[2] != 22 {
		res.Reason = "unsupported"
		return res
	}
	var h hash.Hash
	switch b[3] {
	case 8:
		h = sha256.New()
	case 10:
		h = sha512.New()
	default:
		res.Reason = "unsupported hash"
		return res
	}
	hashedEnd := 6 + (int(b[4])<<8 | int(b[5]))
	h.Write(data)
	h.Write(b[:hashedEnd])
	h.Write([]byte{4, 0xff, byte(hashedEnd >> 24), byte(hashedEnd >> 16), byte(hashedEnd >> 8), byte(hashedEnd)})
	digest := h.Sum(nil)
	pos := hashedEnd + 2 + (int(b[hashedEnd])<<8 | int(b[hashedEnd+1]))
	if !bytes.Equal(digest[:2], b[pos:pos+2]) {
		res.Reason = "hash prefix mismatch"
		return res
	}
	pos += 2
	var sig []byte
	for range 2 {
		n := (int(b[pos])<<8 | int(b[pos+1]) + 7) / 8
		x := b[pos+2 : pos+2+n]
		sig = append(sig, make([]byte, 32-len(x))...)
		sig = append(sig, x...)
		pos += 2 + n
	}
	res.Valid = ed25519.Verify(v.pub, digest, sig)
	if !res.Valid {
		res.Reason = "bad signature"
	}
	return res
}

func verify(t *testing.T, v pgpmime.Verifier, req *pgpmime.VerifyRequest) bool {
	t.Helper()
	if req == nil {
		t.Fatal("no request")
	}
	vs, err := v.Verify(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range vs {
		if !x.Valid {
			return false
		}
	}
	return len(vs) > 0
}

// partOf is the adapter from a mail.Node to the plain values pgpmime takes.
func partOf(n *mail.Node) pgpmime.Part {
	return pgpmime.Part{MediaType: n.MediaType, Params: n.Params, Disposition: n.Disposition,
		Filename: n.Filename, Head: n.Data}
}

func childrenOf(n *mail.Node) []pgpmime.Part {
	var out []pgpmime.Part
	for _, c := range n.Children {
		out = append(out, partOf(c))
	}
	return out
}

func readTree(t *testing.T, raw []byte) *mail.Tree {
	t.Helper()
	_, e, err := message.Parse(bytes.NewReader(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := mail.ReadTree(e, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// detachedFromEML frames signed.eml with emlkit and prepares its
// verification the way an adapter would.
func detachedFromEML(t *testing.T, raw []byte) *pgpmime.VerifyRequest {
	t.Helper()
	root := readTree(t, raw).Root
	st := pgpmime.CheckStructure(partOf(root), childrenOf(root))
	if st.Kind != pgpmime.KindSigned || len(st.Issues) != 0 || st.Content != 0 || st.Signature != 1 {
		t.Fatalf("structure %+v", st)
	}
	signed, sig := root.Children[st.Content], root.Children[st.Signature]
	if !signed.HasSpan {
		t.Fatal("no span")
	}
	req, is := pgpmime.DetachedRequest(raw[signed.SpanStart:signed.SpanEnd], sig.Data, st.Micalg, nil)
	if len(is) != 0 {
		t.Fatalf("issues %v", is)
	}
	return req
}

func TestVerifyVectors(t *testing.T) {
	v := newEdVerifier(t)
	eml := readVec(t, "signed.eml")

	req := detachedFromEML(t, eml)
	if !bytes.Equal(req.Signed, []byte("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n서명된 PGP/MIME 본문입니다.\r\n- dash line\r\n")) {
		t.Fatalf("signed bytes %q", req.Signed)
	}
	if !verify(t, v, req) {
		t.Error("PGP/MIME signature does not verify")
	}
	// An MTA that stored the message with LF line endings: canonicalization
	// restores what was signed.
	if !verify(t, v, detachedFromEML(t, bytes.ReplaceAll(eml, []byte("\r\n"), []byte("\n")))) {
		t.Error("LF-only PGP/MIME signature does not verify")
	}
	if verify(t, v, detachedFromEML(t, bytes.Replace(eml, []byte("dash line"), []byte("dash lime"), 1))) {
		t.Error("tampered PGP/MIME part verifies")
	}

	clear := readVec(t, "clearsigned.asc")
	for name, in := range map[string][]byte{
		"as made": clear,
		"CRLF":    bytes.ReplaceAll(clear, []byte("\n"), []byte("\r\n")),
		"quoted":  append([]byte("> "), bytes.ReplaceAll(clear, []byte("\n"), []byte("\n> "))...),
		"in mail": append(append([]byte("Hello,\n\n"), clear...), "\n-- \nsig\n"...),
	} {
		r := pgpmime.Parse(in, nil)
		if len(r.Blocks) != 1 || r.Blocks[0].Cleartext == nil {
			t.Fatalf("%s: blocks %+v", name, r.Blocks)
		}
		req, is := pgpmime.CleartextRequest(r.Blocks[0].Cleartext)
		if len(is) != 0 {
			t.Errorf("%s: issues %v", name, is)
		}
		if !verify(t, v, req) {
			t.Errorf("%s: cleartext signature does not verify", name)
		}
	}
	tampered := pgpmime.Parse(bytes.Replace(clear, []byte("tester"), []byte("Tester"), 1), nil)
	if req, _ := pgpmime.CleartextRequest(tampered.Blocks[0].Cleartext); verify(t, v, req) {
		t.Error("tampered cleartext verifies")
	}

	for _, f := range []string{"signed-zlib.asc", "signed-bzip2.gpg", "signed-zip.gpg"} {
		req, is := pgpmime.InlineRequest(readVec(t, f), nil)
		if len(is) != 0 {
			t.Errorf("%s: issues %v", f, is)
		}
		if !verify(t, v, req) {
			t.Errorf("%s: signature does not verify", f)
		}
	}
}

type fakeDecrypter struct{ got *pgpmime.DecryptRequest }

func (d *fakeDecrypter) Decrypt(req *pgpmime.DecryptRequest) (*pgpmime.Decryption, error) {
	d.got = req
	return &pgpmime.Decryption{Plaintext: []byte("Content-Type: text/plain\r\n\r\nhello\r\n")}, nil
}

func TestEncryptedVectors(t *testing.T) {
	eml := readVec(t, "encrypted.eml")
	exchange := bytes.Replace(eml, []byte("multipart/encrypted;\r\n protocol=\"application/pgp-encrypted\"; boundary=\"enc-b\""),
		[]byte("multipart/mixed; boundary=\"enc-b\""), 1)
	exchange = bytes.Replace(exchange, []byte("--enc-b\r\n"), []byte("--enc-b\r\nContent-Type: text/plain\r\n\r\n\r\n--enc-b\r\n"), 1)
	exchange = bytes.Replace(exchange, []byte("Content-Type: application/pgp-encrypted\r\n"),
		[]byte("Content-Type: application/pgp-encrypted\r\nContent-Disposition: attachment; filename=\"PGPMIME Versions Identification\"\r\n"), 1)

	for _, tt := range []struct {
		name             string
		raw              []byte
		control, content int
		rewrapped        bool
		wantIssue        pgpmime.Code
	}{
		{"multipart/encrypted", eml, 0, 1, false, ""},
		{"exchange", exchange, 1, 2, true, pgpmime.CodeRewrapped},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := readTree(t, tt.raw).Root
			st := pgpmime.CheckStructure(partOf(root), childrenOf(root))
			if st.Kind != pgpmime.KindEncrypted || st.Control != tt.control || st.Content != tt.content || st.Rewrapped != tt.rewrapped {
				t.Fatalf("structure %+v", st)
			}
			if len(st.Issues) != 0 && (tt.wantIssue == "" || len(st.Issues) != 1 || st.Issues[0].Code != tt.wantIssue) {
				t.Errorf("issues %v", st.Issues)
			}
			if c := pgpmime.Classify(partOf(root.Children[st.Control])); c.Kind != pgpmime.KindControl || len(c.Issues) != 0 {
				t.Errorf("control %+v", c)
			}
			req, is := pgpmime.EncryptedRequest(root.Children[st.Content].Data, nil)
			if len(is) != 0 || req == nil || len(req.Recipients) != 1 || req.Recipients[0] != subkeyID || !req.Encrypted.Protected {
				t.Fatalf("request %+v issues %v", req, is)
			}
			d := &fakeDecrypter{}
			res, err := pgpmime.Decrypter(d).Decrypt(req)
			if err != nil || d.got.Message[0] != 0x84 || !bytes.HasPrefix(res.Plaintext, []byte("Content-Type")) {
				t.Errorf("decrypt %v %v", res, err)
			}
		})
	}
}
