package smime

import (
	"crypto"
	// The hashes Verify uses register themselves.
	_ "crypto/sha1"
	_ "crypto/sha256"
	_ "crypto/sha512"
)

// Object identifiers, in dotted form, that the package acts on.
const (
	OIDData              = "1.2.840.113549.1.7.1"
	OIDSignedData        = "1.2.840.113549.1.7.2"
	OIDEnvelopedData     = "1.2.840.113549.1.7.3"
	OIDDigestedData      = "1.2.840.113549.1.7.5"
	OIDEncryptedData     = "1.2.840.113549.1.7.6"
	OIDAuthEnvelopedData = "1.2.840.113549.1.9.16.1.23"
	OIDCompressedData    = "1.2.840.113549.1.9.16.1.9"

	OIDAttrContentType       = "1.2.840.113549.1.9.3"
	OIDAttrMessageDigest     = "1.2.840.113549.1.9.4"
	OIDAttrSigningTime       = "1.2.840.113549.1.9.5"
	OIDAttrCounterSignature  = "1.2.840.113549.1.9.6"
	OIDAttrSMIMECapabilities = "1.2.840.113549.1.9.15"
	OIDAttrTimeStampToken    = "1.2.840.113549.1.9.16.2.14"

	oidMD5    = "1.2.840.113549.2.5"
	oidSHA1   = "1.3.14.3.2.26"
	oidSHA224 = "2.16.840.1.101.3.4.2.4"
	oidSHA256 = "2.16.840.1.101.3.4.2.1"
	oidSHA384 = "2.16.840.1.101.3.4.2.2"
	oidSHA512 = "2.16.840.1.101.3.4.2.3"

	oidRSA          = "1.2.840.113549.1.1.1"
	oidRSAMD5       = "1.2.840.113549.1.1.4"
	oidRSASHA1      = "1.2.840.113549.1.1.5"
	oidRSAOAEP      = "1.2.840.113549.1.1.7"
	oidMGF1         = "1.2.840.113549.1.1.8"
	oidRSAPSS       = "1.2.840.113549.1.1.10"
	oidRSASHA256    = "1.2.840.113549.1.1.11"
	oidRSASHA384    = "1.2.840.113549.1.1.12"
	oidRSASHA512    = "1.2.840.113549.1.1.13"
	oidRSASHA224    = "1.2.840.113549.1.1.14"
	oidRSASHA1Old   = "1.3.14.3.2.29"
	oidECPublicKey  = "1.2.840.10045.2.1"
	oidECDSASHA1    = "1.2.840.10045.4.1"
	oidECDSASHA224  = "1.2.840.10045.4.3.1"
	oidECDSASHA256  = "1.2.840.10045.4.3.2"
	oidECDSASHA384  = "1.2.840.10045.4.3.3"
	oidECDSASHA512  = "1.2.840.10045.4.3.4"
	oidEd25519      = "1.3.101.112"
	oidAES128CBC    = "2.16.840.1.101.3.4.1.2"
	oidAES192CBC    = "2.16.840.1.101.3.4.1.22"
	oidAES256CBC    = "2.16.840.1.101.3.4.1.42"
	oidAES128GCM    = "2.16.840.1.101.3.4.1.6"
	oidAES192GCM    = "2.16.840.1.101.3.4.1.26"
	oidAES256GCM    = "2.16.840.1.101.3.4.1.46"
	oidZlibCompress = "1.2.840.113549.1.9.16.3.8"
)

var oidNames = map[string]string{
	OIDData:                      "data",
	OIDSignedData:                "signedData",
	OIDEnvelopedData:             "envelopedData",
	"1.2.840.113549.1.7.4":       "signedAndEnvelopedData",
	OIDDigestedData:              "digestedData",
	OIDEncryptedData:             "encryptedData",
	OIDAuthEnvelopedData:         "authEnvelopedData",
	OIDCompressedData:            "compressedData",
	"1.2.840.113549.1.9.16.1.1":  "receipt",
	"1.2.840.113549.1.9.16.1.2":  "authData",
	"1.2.840.113549.1.9.16.1.4":  "TSTInfo",
	OIDAttrContentType:           "contentType",
	OIDAttrMessageDigest:         "messageDigest",
	OIDAttrSigningTime:           "signingTime",
	OIDAttrCounterSignature:      "counterSignature",
	OIDAttrSMIMECapabilities:     "smimeCapabilities",
	OIDAttrTimeStampToken:        "timeStampToken",
	"1.2.840.113549.1.9.16.2.11": "encrypKeyPref",
	"1.2.840.113549.1.9.16.2.12": "signingCertificate",
	"1.2.840.113549.1.9.16.2.47": "signingCertificateV2",
	"1.2.840.113549.1.9.52":      "cmsAlgorithmProtection",
	"1.3.6.1.4.1.311.16.4":       "msEncrypKeyPref",

	oidMD5:                    "MD5",
	oidSHA1:                   "SHA-1",
	oidSHA224:                 "SHA-224",
	oidSHA256:                 "SHA-256",
	oidSHA384:                 "SHA-384",
	oidSHA512:                 "SHA-512",
	"2.16.840.1.101.3.4.2.8":  "SHA3-256",
	"2.16.840.1.101.3.4.2.9":  "SHA3-384",
	"2.16.840.1.101.3.4.2.10": "SHA3-512",

	oidRSA:                   "RSA",
	oidRSAMD5:                "MD5-RSA",
	oidRSASHA1:               "SHA1-RSA",
	oidRSASHA1Old:            "SHA1-RSA",
	oidRSAOAEP:               "RSAES-OAEP",
	oidMGF1:                  "MGF1",
	oidRSAPSS:                "RSASSA-PSS",
	oidRSASHA224:             "SHA224-RSA",
	oidRSASHA256:             "SHA256-RSA",
	oidRSASHA384:             "SHA384-RSA",
	oidRSASHA512:             "SHA512-RSA",
	oidECPublicKey:           "EC",
	oidECDSASHA1:             "ECDSA-SHA1",
	oidECDSASHA224:           "ECDSA-SHA224",
	oidECDSASHA256:           "ECDSA-SHA256",
	oidECDSASHA384:           "ECDSA-SHA384",
	oidECDSASHA512:           "ECDSA-SHA512",
	"1.2.840.10040.4.1":      "DSA",
	"1.2.840.10040.4.3":      "DSA-SHA1",
	"2.16.840.1.101.3.4.3.2": "DSA-SHA256",
	"1.3.101.110":            "X25519",
	"1.3.101.111":            "X448",
	oidEd25519:               "Ed25519",
	"1.3.101.113":            "Ed448",

	"1.2.840.113549.3.2":         "RC2-CBC",
	"1.2.840.113549.3.4":         "RC4",
	"1.2.840.113549.3.7":         "3DES-CBC",
	"1.3.14.3.2.7":               "DES-CBC",
	oidAES128CBC:                 "AES-128-CBC",
	oidAES192CBC:                 "AES-192-CBC",
	oidAES256CBC:                 "AES-256-CBC",
	oidAES128GCM:                 "AES-128-GCM",
	oidAES192GCM:                 "AES-192-GCM",
	oidAES256GCM:                 "AES-256-GCM",
	"2.16.840.1.101.3.4.1.5":     "AES-128-WRAP",
	"2.16.840.1.101.3.4.1.25":    "AES-192-WRAP",
	"2.16.840.1.101.3.4.1.45":    "AES-256-WRAP",
	"1.2.840.113549.1.9.16.3.6":  "3DES-WRAP",
	"1.2.840.113549.1.9.16.3.18": "ChaCha20-Poly1305",
	"1.2.840.113549.1.9.16.3.9":  "PWRI-KEK",
	"1.2.840.113549.1.5.12":      "PBKDF2",
	oidZlibCompress:              "zlib",

	"1.3.133.16.840.63.0.2":     "ECDH-SHA1-KDF",
	"1.3.132.1.11.0":            "ECDH-SHA224-KDF",
	"1.3.132.1.11.1":            "ECDH-SHA256-KDF",
	"1.3.132.1.11.2":            "ECDH-SHA384-KDF",
	"1.3.132.1.11.3":            "ECDH-SHA512-KDF",
	"1.3.133.16.840.63.0.3":     "ECDH-COFACTOR-SHA1-KDF",
	"1.3.132.1.14.1":            "ECDH-COFACTOR-SHA256-KDF",
	"1.2.840.113549.1.9.16.3.5": "ESDH",
}

// OIDName returns the common name of an object identifier in dotted form:
// "SHA-256", "RSASSA-PSS", "AES-256-CBC", "signedData". It returns the empty
// string for one the package has no name for.
func OIDName(oid string) string { return oidNames[oid] }

// hashOf maps a digest algorithm to the standard library's hash, when Verify
// supports it. MD5 is recognised by name and not verified.
func hashOf(oid string) (crypto.Hash, bool) {
	switch oid {
	case oidSHA1:
		return crypto.SHA1, true
	case oidSHA256:
		return crypto.SHA256, true
	case oidSHA384:
		return crypto.SHA384, true
	case oidSHA512:
		return crypto.SHA512, true
	}
	return 0, false
}
