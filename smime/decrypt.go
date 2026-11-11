package smime

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"fmt"
)

// A Decrypter decrypts the content of an EnvelopedData with keys the caller
// holds. The package holds none: decryption happens only through a Decrypter
// the caller supplies. RSADecrypter is one implementation.
//
// The plaintext of an S/MIME message is a MIME entity, often another CMS
// object (a signed message inside the envelope); parse it again.
type Decrypter interface {
	DecryptEnveloped(ed *EnvelopedData) (content []byte, err error)
}

// Decrypt has d decrypt the message's EnvelopedData or AuthEnvelopedData. It
// returns ErrNotEnveloped for any other object.
func (m *Message) Decrypt(d Decrypter) ([]byte, error) {
	if m == nil || m.Enveloped == nil {
		return nil, ErrNotEnveloped
	}
	if d == nil {
		return nil, errors.New("smime: nil Decrypter")
	}
	return d.DecryptEnveloped(m.Enveloped)
}

// An RSADecrypter is a Decrypter for the common case: a key-transport
// recipient (RSA PKCS #1 v1.5 or RSAES-OAEP with MGF1) and content under
// AES-CBC, or AES-GCM in an AuthEnvelopedData. Other recipient kinds and
// ciphers yield ErrUnsupported.
//
// Decrypting attacker-supplied AES-CBC content and reporting whether the
// padding was valid is a padding oracle if the outcome is observable to the
// sender. A classifier that never answers the sender is not one; a service
// that does must not expose the distinction.
type RSADecrypter struct {
	// Certificate, when set, selects the recipient info to use. Without it
	// the first few key-transport recipients are tried in order.
	Certificate *x509.Certificate
	// Key is the private key, usually an *rsa.PrivateKey; any
	// crypto.Decrypter that accepts *rsa.PKCS1v15DecryptOptions and
	// *rsa.OAEPOptions does.
	Key crypto.Decrypter
}

// maxBlindRecipients is how many recipients are tried without a certificate
// to choose by.
const maxBlindRecipients = 8

// DecryptEnveloped implements Decrypter.
func (d *RSADecrypter) DecryptEnveloped(ed *EnvelopedData) ([]byte, error) {
	if d == nil || d.Key == nil {
		return nil, errors.New("smime: RSADecrypter has no key")
	}
	if ed == nil || !ed.HasEncryptedContent {
		return nil, ErrNotEnveloped
	}
	if ed.ContentTruncated {
		return nil, errors.New("smime: encrypted content is truncated")
	}
	keyLen := 0
	switch ed.ContentEncryptionAlgorithm.OID {
	case oidAES128CBC, oidAES128GCM:
		keyLen = 16
	case oidAES192CBC, oidAES192GCM:
		keyLen = 24
	case oidAES256CBC, oidAES256GCM:
		keyLen = 32
	default:
		return nil, fmt.Errorf("%w: content encryption %s", ErrUnsupported, ed.ContentEncryptionAlgorithm)
	}

	err := ErrNoRecipient
	tried := 0
	for i := range ed.Recipients {
		ri := &ed.Recipients[i]
		if ri.Kind != RecipientKeyTransport {
			continue
		}
		if d.Certificate != nil && !ri.Matches(d.Certificate) {
			continue
		}
		if tried++; tried > maxBlindRecipients {
			break
		}
		var key []byte
		if key, err = d.contentKey(ri, keyLen); err != nil {
			continue
		}
		var out []byte
		if out, err = decryptContent(ed, key); err == nil {
			return out, nil
		}
	}
	return nil, err
}

// contentKey decrypts the content-encryption key of one recipient.
func (d *RSADecrypter) contentKey(ri *RecipientInfo, keyLen int) ([]byte, error) {
	var opts crypto.DecrypterOpts
	switch ri.KeyEncryptionAlgorithm.OID {
	case oidRSA:
		// With SessionKeyLen a padding failure yields a random key in
		// constant time, and not an error to distinguish it by.
		opts = &rsa.PKCS1v15DecryptOptions{SessionKeyLen: keyLen}
	case oidRSAOAEP:
		h, mgf, _, ok := rsaParams(ri.KeyEncryptionAlgorithm.Params)
		if !ok {
			return nil, fmt.Errorf("%w: RSAES-OAEP parameters", ErrUnsupported)
		}
		opts = &rsa.OAEPOptions{Hash: h, MGFHash: mgf}
	default:
		return nil, fmt.Errorf("%w: key encryption %s", ErrUnsupported, ri.KeyEncryptionAlgorithm)
	}
	key, err := d.Key.Decrypt(rand.Reader, ri.EncryptedKey, opts)
	if err != nil {
		return nil, err
	}
	if len(key) != keyLen {
		return nil, errors.New("smime: content key has the wrong length")
	}
	return key, nil
}

var errDecrypt = errors.New("smime: content does not decrypt")

// decryptContent decrypts the content of ed with its content-encryption key.
func decryptContent(ed *EnvelopedData, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	alg := ed.ContentEncryptionAlgorithm
	ct := ed.EncryptedContent
	p := newParser(alg.Params, (*Limits)(nil).resolve())
	top := p.top()

	switch alg.OID {
	case oidAES128CBC, oidAES192CBC, oidAES256CBC:
		ive, ok := top.take(classUniversal, tagOctetString)
		iv := p.content(ive)
		if !ok || ive.truncated || len(iv) != aes.BlockSize {
			return nil, fmt.Errorf("%w: AES-CBC parameters", ErrUnsupported)
		}
		if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
			return nil, errDecrypt
		}
		out := make([]byte, len(ct))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, ct)
		pad := int(out[len(out)-1])
		if pad == 0 || pad > aes.BlockSize {
			return nil, errDecrypt
		}
		for _, c := range out[len(out)-pad:] {
			if int(c) != pad {
				return nil, errDecrypt
			}
		}
		return out[:len(out)-pad], nil
	}

	// AES-GCM: GCMParameters ::= SEQUENCE { nonce OCTET STRING, icvLen
	// INTEGER DEFAULT 12 }. The tag is the mac field, and the additional
	// data the authenticated attributes as a SET OF, when there are any.
	if !ed.Authenticated {
		return nil, fmt.Errorf("%w: AES-GCM outside AuthEnvelopedData", ErrUnsupported)
	}
	seq, ok := top.take(classUniversal, tagSequence)
	if !ok || seq.truncated {
		return nil, fmt.Errorf("%w: AES-GCM parameters", ErrUnsupported)
	}
	c := p.in(seq, 1)
	ne, ok := c.take(classUniversal, tagOctetString)
	if !ok {
		return nil, fmt.Errorf("%w: AES-GCM parameters", ErrUnsupported)
	}
	nonce := p.content(ne)
	tagLen := 12
	if n, ok := c.take(classUniversal, tagInteger); ok {
		tagLen, _ = smallInt(p.content(n))
	}
	if tagLen != len(ed.MAC) {
		return nil, errDecrypt
	}
	var aead cipher.AEAD
	switch {
	case len(nonce) == 12 && tagLen >= 12 && tagLen <= 16:
		aead, err = cipher.NewGCMWithTagSize(block, tagLen)
	case len(nonce) > 0 && len(nonce) <= 64 && tagLen == 16:
		aead, err = cipher.NewGCMWithNonceSize(block, len(nonce))
	default:
		return nil, fmt.Errorf("%w: AES-GCM nonce of %d and tag of %d bytes", ErrUnsupported, len(nonce), tagLen)
	}
	if err != nil {
		return nil, err
	}
	var aad []byte
	if ed.AuthAttrs != nil {
		aad = derSet(ed.AuthAttrs)
	}
	sealed := make([]byte, 0, len(ct)+len(ed.MAC))
	sealed = append(append(sealed, ct...), ed.MAC...)
	out, err := aead.Open(sealed[:0], nonce, sealed, aad)
	if err != nil {
		return nil, errDecrypt
	}
	return out, nil
}
