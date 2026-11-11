// Package smime reads the CMS (RFC 5652) objects S/MIME (RFC 8551) carries in
// mail: it classifies an S/MIME part, takes the content out of an opaque
// signed message so that it can be parsed as the message it is, describes
// signers, certificates and recipients, verifies signatures with the standard
// library, and offers a hook for decryption.
//
// The package is optional and stands alone: the message and mail packages do
// not import it, and it does not import them. It works on plain values — a
// media type, its parameters, a file name, the decoded bytes of a part — so a
// caller wires it to whatever read the message. docs/robustness-notes/smime.md
// shows the wiring to mail.ReadTree and message.Parse.
//
// # Damaged input
//
// Everything here reads input an attacker chose. The reader of BER underneath
// is bounded in depth, in the number of elements it visits and in what it
// reassembles, never allocates from a length the input declares, and returns
// what it could read from a truncated or malformed object together with an
// Issue for each thing it tolerated or lost. See Limits.
//
// Results alias the input: the content of a primitive OCTET STRING, raw
// certificates and raw attributes are slices of the bytes that were parsed (or
// of their decoded form, when the input was PEM or Base64). Do not modify the
// input while the results are in use.
//
// # What "verified" means
//
// SignedData.Verify reports separate facts for each signer and never one
// verdict: whether the content's digest is the one the signer signed, whether
// the signature is valid for the public key of the certificate the message
// itself names, and — only when the caller supplies roots — whether that
// certificate chains to one of them. A valid signature says that whoever holds
// that key signed these bytes. It does not say who that is: any sender can
// embed a certificate of their own making. Only the chain status, and the
// caller's comparison of the certificate's addresses with the message's
// author, say anything about identity.
package smime
