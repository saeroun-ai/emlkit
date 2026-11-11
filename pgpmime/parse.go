package pgpmime

import "bytes"

// A Result is what Parse found in its input.
type Result struct {
	// Blocks are the armored blocks of a text input, in order, each with its
	// packets walked. A cleartext signature is one block whose signature
	// block is in its Cleartext.
	Blocks []*Block
	// Binary reports that the input was a binary packet stream, walked into
	// Packets.
	Binary  bool
	Packets []Packet
	// Issues are those about the input as a whole, and about the packets of
	// a binary input.
	Issues []Issue
}

// Parse reads OpenPGP data: a binary packet stream, or text holding any
// number of armored blocks and cleartext signatures among other text, such
// as a mail body or an .asc attachment. Every block is decoded and its
// packets are walked under lim, nil for the defaults. Parse does no
// cryptography and never fails: what it could not read is reported in the
// issues of the result and its blocks.
func Parse(data []byte, lim *Limits) *Result {
	return newState(lim).parse(data)
}

func (s *state) parse(data []byte) *Result {
	r := &Result{}
	if int64(len(data)) > s.lim.MaxInputBytes {
		s.add(&r.Issues, CodeInputLimit, s.lim.MaxInputBytes, "input of %d bytes is read only to %d", len(data), s.lim.MaxInputBytes)
		data = data[:s.lim.MaxInputBytes]
	}
	if sniffPackets(data) {
		r.Binary = true
		s.walk(data, 0, &r.Packets, &r.Issues)
		return r
	}
	r.Blocks = s.findBlocks(data, &r.Issues)
	for _, b := range r.Blocks {
		s.walkBlock(b)
	}
	return r
}

func (s *state) walkBlock(b *Block) {
	if b.Cleartext != nil {
		if sig := b.Cleartext.Signature; sig != nil && len(sig.Data) > 0 {
			s.walk(sig.Data, 0, &sig.Packets, &sig.Issues)
		}
		return
	}
	// The second and later parts of a split message continue its stream
	// and do not start a packet.
	if b.Type == ArmorMessagePart && b.Part > 1 {
		return
	}
	if len(b.Data) > 0 {
		s.walk(b.Data, 0, &b.Packets, &b.Issues)
	}
}

// sniffPackets reports whether b starts like a binary OpenPGP packet
// stream: a packet header of a known tag followed by a plausible first
// body byte, its version for the packets that have one.
func sniffPackets(b []byte) bool {
	h, err := parseHeader(b)
	if err != nil || !h.tag.known() || len(b) <= h.size {
		return false
	}
	v := b[h.size]
	switch h.tag {
	case TagPKESK, TagOnePass:
		return v == 3 || v == 6
	case TagSignature:
		return v >= 3 && v <= 6
	case TagSKESK:
		return v >= 4 && v <= 6
	case TagSecretKey, TagPublicKey, TagSecretSubkey, TagPublicSubkey:
		return v >= 2 && v <= 6
	case TagCompressed:
		return v <= 3
	case TagLiteral:
		return bytes.IndexByte([]byte("btulm1"), v) >= 0
	case TagSEIPD:
		return v == 1 || v == 2
	case TagAEAD:
		return v == 1
	case TagMarker:
		return v == 'P'
	case TagSED:
		return h.length >= 10 || h.partial || h.indeterminate
	}
	return false
}

// Summary is what a packet list says about a message, gathered.
type Summary struct {
	// Encrypted reports encrypted data; Protected that every encrypted data
	// packet is integrity protected.
	Encrypted, Protected bool
	// Recipients are the key IDs of the public-key session key packets, 0
	// for an anonymous one; Passphrase reports a symmetric-key one.
	Recipients []KeyID
	Passphrase bool
	// Signed reports a signature over data, binary or text, or a one-pass
	// signature; Signers are the key IDs they name. Key certifications do
	// not count.
	Signed  bool
	Signers []KeyID
	// Keys are the key IDs of the key packets, primary and subkeys; UserIDs
	// their user IDs.
	Keys    []KeyID
	UserIDs []string
	// FileNames are the file names of the literal data packets.
	FileNames []string
}

// Summary gathers the packets of the result, its blocks and their
// cleartext signatures.
func (r *Result) Summary() Summary {
	var all []Packet
	all = append(all, r.Packets...)
	for _, b := range r.Blocks {
		all = append(all, b.Packets...)
		if b.Cleartext != nil && b.Cleartext.Signature != nil {
			all = append(all, b.Cleartext.Signature.Packets...)
		}
	}
	return Summarize(all)
}

// Summarize gathers what a packet list says about a message.
func Summarize(ps []Packet) Summary {
	var s Summary
	seen := map[KeyID]bool{}
	addSigner := func(k KeyID) {
		if !seen[k] {
			seen[k] = true
			s.Signers = append(s.Signers, k)
		}
	}
	s.Protected = true
	for i := range ps {
		p := &ps[i]
		switch {
		case p.Encrypted != nil:
			s.Encrypted = true
			s.Protected = s.Protected && p.Encrypted.Protected
		case p.Tag == TagSED || p.Tag == TagSEIPD || p.Tag == TagAEAD:
			s.Encrypted = true
			s.Protected = s.Protected && p.Tag != TagSED
		case p.EncryptedKey != nil:
			s.Recipients = append(s.Recipients, p.EncryptedKey.KeyID)
		case p.SymmetricKey != nil:
			s.Passphrase = true
		case p.OnePass != nil:
			s.Signed = true
			addSigner(p.OnePass.KeyID)
		case p.Signature != nil:
			if t := p.Signature.Type; t == SigBinary || t == SigText {
				s.Signed = true
				addSigner(p.Signature.IssuerKeyID)
			}
		case p.Key != nil:
			s.Keys = append(s.Keys, p.Key.KeyID)
		case p.Tag == TagUserID && p.UserID != "":
			s.UserIDs = append(s.UserIDs, p.UserID)
		case p.Literal != nil:
			s.FileNames = append(s.FileNames, p.Literal.FileName)
		}
	}
	if !s.Encrypted {
		s.Protected = false
	}
	return s
}
