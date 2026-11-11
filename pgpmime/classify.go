package pgpmime

import (
	"bytes"
	"fmt"
	"strings"
)

// A Part is what classification needs to know of a MIME entity, as plain
// values a caller copies from a mail.Node or a message.Entity.
type Part struct {
	// MediaType and Params are the Content-Type; parameter names are
	// matched case-insensitively.
	MediaType string
	Params    map[string]string
	// Disposition and Filename are the Content-Disposition and the name the
	// part gives its content.
	Disposition string
	Filename    string
	// Head is the start of the decoded body, or all of it, and may be nil.
	// Without it a part is classified by its type and name alone.
	Head []byte
}

func (p Part) param(name string) string {
	if v, ok := p.Params[name]; ok {
		return strings.TrimSpace(v)
	}
	for k, v := range p.Params {
		if strings.EqualFold(k, name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func (p Part) mediaType() string { return strings.ToLower(strings.TrimSpace(p.MediaType)) }

// A Kind is what a part is in OpenPGP terms.
type Kind uint8

// Kinds.
const (
	KindNone      Kind = iota
	KindSigned         // multipart/signed with the PGP protocol (RFC 3156 §5)
	KindEncrypted      // multipart/encrypted with the PGP protocol (RFC 3156 §4)
	KindSignature      // an application/pgp-signature part
	KindControl        // an application/pgp-encrypted control part
	KindKeys           // an application/pgp-keys part
	KindArmored        // a text or attachment that carries armored blocks
	KindBinary         // an attachment of binary OpenPGP data
)

func (k Kind) String() string {
	switch k {
	case KindSigned:
		return "signed"
	case KindEncrypted:
		return "encrypted"
	case KindSignature:
		return "signature"
	case KindControl:
		return "control"
	case KindKeys:
		return "keys"
	case KindArmored:
		return "armored"
	case KindBinary:
		return "binary"
	}
	return "none"
}

// A Classification is what Classify found a part to be.
type Classification struct {
	Kind   Kind
	Reason string
	// Armor are the types of the armored blocks Head holds, each once, in
	// order of first appearance.
	Armor  []ArmorType
	Issues []Issue
}

// HasArmor reports whether Head holds a block of the type.
func (c Classification) HasArmor(t ArmorType) bool {
	for _, a := range c.Armor {
		if a == t {
			return true
		}
	}
	return false
}

// armorTypes lists the types of the BEGIN PGP lines of b.
func armorTypes(b []byte) []ArmorType {
	var out []ArmorType
	if !bytes.Contains(b, []byte("BEGIN PGP")) {
		return nil
	}
	for pos := 0; pos < len(b); {
		end, next := line(b, pos)
		if label, _, _, ok := beginAt(b, pos, end); ok {
			t, _, _ := armorType(label)
			seen := false
			for _, a := range out {
				seen = seen || a == t
			}
			if !seen {
				out = append(out, t)
			}
		}
		pos = next
	}
	return out
}

// pgpExt returns the OpenPGP file name extension of name, or "".
func pgpExt(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, e := range []string{".asc", ".pgp", ".gpg", ".sig"} {
		if strings.HasSuffix(n, e) {
			return e
		}
	}
	return ""
}

// hasVersion1 reports whether a control part says "Version: 1".
func hasVersion1(b []byte) bool {
	for pos := 0; pos < len(b); {
		end, next := line(b, pos)
		if k, v, ok := parseHeaderLine(bytes.TrimSpace(b[pos:end])); ok && strings.EqualFold(k, "Version") && v == "1" {
			return true
		}
		pos = next
	}
	return false
}

func addIssue(dst *[]Issue, code Code, format string, args ...any) {
	*dst = append(*dst, Issue{Code: code, Offset: -1, Message: fmt.Sprintf(format, args...)})
}

// Classify says what one part is in OpenPGP terms, from its type,
// parameters, name and first bytes. A multipart is classified by its
// declared protocol only; CheckStructure checks its parts.
func Classify(p Part) Classification {
	mt := p.mediaType()
	c := Classification{Armor: armorTypes(p.Head)}
	binary := len(c.Armor) == 0 && sniffPackets(p.Head)
	ext := pgpExt(p.Filename)
	switch mt {
	case "multipart/signed":
		if strings.EqualFold(p.param("protocol"), "application/pgp-signature") {
			c.Kind, c.Reason = KindSigned, "multipart/signed with protocol application/pgp-signature"
		}
		return c
	case "multipart/encrypted":
		if strings.EqualFold(p.param("protocol"), "application/pgp-encrypted") {
			c.Kind, c.Reason = KindEncrypted, "multipart/encrypted with protocol application/pgp-encrypted"
		}
		return c
	case "application/pgp-signature":
		c.Kind, c.Reason = KindSignature, "application/pgp-signature"
		if len(p.Head) > 0 && !c.HasArmor(ArmorSignature) && !binary {
			addIssue(&c.Issues, CodeTypeMismatch, "application/pgp-signature holds no signature")
		}
		return c
	case "application/pgp-encrypted":
		c.Kind, c.Reason = KindControl, "application/pgp-encrypted"
		if len(p.Head) > 0 && !hasVersion1(p.Head) {
			addIssue(&c.Issues, CodeControlVersion, "control part does not say \"Version: 1\"")
		}
		return c
	case "application/pgp-keys":
		c.Kind, c.Reason = KindKeys, "application/pgp-keys"
		if len(p.Head) > 0 && !c.HasArmor(ArmorPublicKey) && !c.HasArmor(ArmorPrivateKey) && !binary {
			addIssue(&c.Issues, CodeTypeMismatch, "application/pgp-keys holds no key block")
		}
		return c
	}
	where := "attachment"
	if strings.HasPrefix(mt, "text/") && ext == "" && !strings.EqualFold(p.Disposition, "attachment") {
		where = "text"
	}
	switch {
	case len(c.Armor) > 0:
		c.Kind, c.Reason = KindArmored, where+" carries armored OpenPGP blocks"
	case binary:
		c.Kind, c.Reason = KindBinary, where+" starts with an OpenPGP packet"
	case ext != "" && len(p.Head) == 0:
		c.Kind, c.Reason = KindBinary, "file name ends in "+ext
		if ext == ".asc" {
			c.Kind = KindArmored
		}
	case ext != "":
		addIssue(&c.Issues, CodeTypeMismatch, "%q is named like OpenPGP data and its content is not", p.Filename)
	}
	return c
}

// A Structure is what CheckStructure found a multipart to be.
type Structure struct {
	// Kind is KindSigned, KindEncrypted or KindNone.
	Kind Kind
	// Protocol and Micalg are the declared parameters, lower-cased.
	Protocol, Micalg string
	// Content is the index of the signed part, or of the encrypted payload;
	// Signature of the signature part; Control of the control part. Each is
	// -1 when absent.
	Content, Signature, Control int
	// Rewrapped reports a multipart/mixed that carries an encrypted message
	// as attachments (CodeRewrapped).
	Rewrapped bool
	Issues    []Issue
}

// CheckStructure checks a multipart and its parts against RFC 3156: the
// declared protocol and micalg, the part count, order and types, and the
// control part's version. It recognises a PGP/MIME message whose protocol
// is missing or wrong by its parts, and the Exchange rewrapping of an
// encrypted message as multipart/mixed. Kind is KindNone for a multipart
// that is not PGP/MIME, an S/MIME one included.
func CheckStructure(p Part, children []Part) Structure {
	st := Structure{Content: -1, Signature: -1, Control: -1,
		Protocol: strings.ToLower(p.param("protocol")), Micalg: strings.ToLower(p.param("micalg"))}
	find := func(mt string) int {
		for i, ch := range children {
			if ch.mediaType() == mt {
				return i
			}
		}
		return -1
	}
	switch p.mediaType() {
	case "multipart/signed":
		checkSigned(&st, children, find("application/pgp-signature"))
	case "multipart/encrypted":
		checkEncrypted(&st, children, find("application/pgp-encrypted"))
	case "multipart/mixed":
		checkRewrapped(&st, children, find("application/pgp-encrypted"))
	}
	return st
}

// protocol decides whether a multipart is PGP/MIME from its protocol and
// whether a part of the PGP type is present.
func protocol(st *Structure, want string, found bool) bool {
	switch {
	case st.Protocol == want:
		return true
	case !found:
		return false
	case st.Protocol == "":
		addIssue(&st.Issues, CodeProtocolMissing, "no protocol declared; a part is %s", want)
	default:
		addIssue(&st.Issues, CodeProtocolMismatch, "protocol %q declared; a part is %s", st.Protocol, want)
	}
	return true
}

func checkSigned(st *Structure, children []Part, sig int) {
	if !protocol(st, "application/pgp-signature", sig >= 0) {
		return
	}
	st.Kind = KindSigned
	if len(children) != 2 {
		addIssue(&st.Issues, CodePartCount, "multipart/signed has %d parts, not 2", len(children))
	}
	switch {
	case sig < 0:
		addIssue(&st.Issues, CodeProtocolMismatch, "no part is application/pgp-signature")
		if len(children) >= 2 {
			st.Content, st.Signature = 0, 1
		} else if len(children) == 1 {
			st.Content = 0
		}
	default:
		st.Signature = sig
		if sig != 1 {
			addIssue(&st.Issues, CodePartOrder, "the signature is part %d, not the second", sig+1)
		}
		if sig != 0 {
			st.Content = 0
		} else if len(children) > 1 {
			st.Content = 1
		}
	}
	switch {
	case st.Micalg == "":
		addIssue(&st.Issues, CodeMicalgMissing, "no micalg declared")
	case !strings.HasPrefix(st.Micalg, "pgp-"):
		addIssue(&st.Issues, CodeMicalgInvalid, "micalg %q is not a pgp- hash", st.Micalg)
	default:
		if _, ok := HashByName(st.Micalg); !ok {
			addIssue(&st.Issues, CodeMicalgInvalid, "micalg %q names no known hash", st.Micalg)
		}
	}
}

func checkEncrypted(st *Structure, children []Part, ctl int) {
	if !protocol(st, "application/pgp-encrypted", ctl >= 0) {
		return
	}
	st.Kind = KindEncrypted
	if len(children) != 2 {
		addIssue(&st.Issues, CodePartCount, "multipart/encrypted has %d parts, not 2", len(children))
	}
	if ctl < 0 {
		addIssue(&st.Issues, CodeControlMissing, "no application/pgp-encrypted control part")
	} else {
		st.Control = ctl
		if ctl != 0 {
			addIssue(&st.Issues, CodePartOrder, "the control part is part %d, not the first", ctl+1)
		}
		if h := children[ctl].Head; len(h) > 0 && !hasVersion1(h) {
			addIssue(&st.Issues, CodeControlVersion, "control part does not say \"Version: 1\"")
		}
	}
	st.Content = payload(children, ctl)
	if st.Content >= 0 && children[st.Content].mediaType() != "application/octet-stream" {
		addIssue(&st.Issues, CodePayloadType, "payload is %q, not application/octet-stream", children[st.Content].MediaType)
	}
}

// payload picks the encrypted payload: the first application/octet-stream
// part other than the control part, else the first part after it that
// carries an armored message, else the part after it.
func payload(children []Part, ctl int) int {
	for i, ch := range children {
		if i != ctl && ch.mediaType() == "application/octet-stream" {
			return i
		}
	}
	for i, ch := range children {
		if i != ctl && Classify(ch).HasArmor(ArmorMessage) {
			return i
		}
	}
	if ctl+1 < len(children) {
		return ctl + 1
	}
	return -1
}

func checkRewrapped(st *Structure, children []Part, ctl int) {
	if ctl < 0 || ctl+1 >= len(children) {
		return
	}
	pay := children[ctl+1]
	if pay.mediaType() != "application/octet-stream" && !Classify(pay).HasArmor(ArmorMessage) &&
		!strings.EqualFold(pay.Filename, "encrypted.asc") {
		return
	}
	st.Kind, st.Rewrapped, st.Control, st.Content = KindEncrypted, true, ctl, ctl+1
	addIssue(&st.Issues, CodeRewrapped, "multipart/mixed carries a PGP/MIME encrypted message as attachments")
	if h := children[ctl].Head; len(h) > 0 && !hasVersion1(h) {
		addIssue(&st.Issues, CodeControlVersion, "control part does not say \"Version: 1\"")
	}
	extra := 0
	for i, ch := range children {
		if i == ctl || i == ctl+1 {
			continue
		}
		if ch.mediaType() == "text/plain" && len(bytes.TrimSpace(ch.Head)) == 0 {
			continue
		}
		extra++
	}
	if extra > 0 {
		addIssue(&st.Issues, CodePartCount, "%d parts beside the control part and the payload", extra)
	}
}
