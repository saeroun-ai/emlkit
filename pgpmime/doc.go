// Package pgpmime reads the OpenPGP content of a mail message: the PGP/MIME
// structure of RFC 3156, ASCII armor, cleartext signatures and the OpenPGP
// packet stream of RFC 4880 and RFC 9580.
//
// It does no cryptography. The Go standard library has no OpenPGP, and this
// module adds no dependency, so the package reports what the data is and what
// it says about itself — signer and recipient key IDs, algorithms, literal
// file names, the signed text — and leaves verification and decryption to a
// Verifier or Decrypter the caller supplies, with the exact bytes each needs.
// Nothing it reports is authenticated: a key ID in a signature packet is a
// claim until a Verifier has checked the signature.
//
// The package works on plain values and byte slices, and the core of emlkit
// does not import it. Classify and CheckStructure take a part's media type,
// parameters, file name and first bytes, which a caller copies from a
// mail.Node or a message.Entity. Parse reads armored text or binary OpenPGP
// data. Damage is healed as far as the data allows and each repair or
// oddity is reported as an Issue; no input makes it panic, loop without end
// or allocate beyond its Limits.
package pgpmime
