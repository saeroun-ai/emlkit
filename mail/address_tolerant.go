package mail

import (
	"math"
	"mime"
	netmail "net/mail"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/saeroun-ai/emlkit"
)

// AddressOptions configures ParseAddressListTolerant and
// Header.AddressListTolerant. The zero value and a nil *AddressOptions select
// the default loose parsing, which follows MimeKit's defaults.
type AddressOptions struct {
	// StrictCommas makes every unquoted comma separate two addresses.
	//
	// By default (MimeKit's AllowUnquotedCommaInAddresses) the words in front
	// of a comma are read as part of a display name when more words and commas
	// lead up to an angle address: "Doe, John <john@example.org>" is one
	// address named "Doe, John". Such a name is reported with
	// address.unquoted_comma. With StrictCommas the same input is the
	// local-only address "Doe" followed by "John <john@example.org>".
	StrictCommas bool

	// RequireDomain reports an address without a domain as invalid.
	//
	// By default (MimeKit's AllowAddressesWithoutDomain) "postmaster" and
	// "Root <root>" are kept as addresses whose Address field is the local
	// part alone, reported with address.local_only.
	RequireDomain bool
}

// An AddressEntry is one element of an address list as the tolerant parser
// read it.
type AddressEntry struct {
	// Raw is the element exactly as it appears in the field value, without
	// the separators and whitespace around it. It is always set, so an
	// element that could not be parsed is preserved verbatim.
	Raw string

	// Address is the parsed address, or nil when the element holds no usable
	// address. Bytes that cannot be decoded (text that is neither ASCII nor
	// valid UTF-8, an encoded-word in an unknown charset) are kept in Name and
	// Address as they were written.
	Address *Address

	// Group is the display name of the RFC 5322 group the element is a member
	// of, or empty. Nested groups are flattened into the outermost one.
	Group string

	// Diagnostics are the diagnostics about this element. They are also part
	// of AddressListResult.Diagnostics.
	Diagnostics []message.Diagnostic
}

// An AddressListResult is what the tolerant address parser understood of an
// address list.
type AddressListResult struct {
	// Raw is the field value that was parsed.
	Raw string

	// Addresses are the usable addresses, in the order they appear.
	Addresses []*Address

	// Entries are all elements in the order they appear, including those
	// without a usable address.
	Entries []AddressEntry

	// Diagnostics lists every repair and every element that was kept raw
	// only, in discovery order. It is empty for a list that RFC 5322 accepts
	// as written.
	Diagnostics []message.Diagnostic
}

// ParseAddressListTolerant parses list as a list of addresses and keeps
// whatever it can still understand of a damaged one. A nil opts selects the
// defaults.
//
// An element net/mail accepts is parsed by net/mail, so a list
// ParseAddressList accepts yields the same addresses here. Any other element
// is repaired when one reading of it stands out, and kept raw with an
// address.invalid diagnostic when none does; either way its neighbours are
// unaffected. The repairs, each reported:
//
//   - a display name with unquoted commas, and an address without a domain
//     (see AddressOptions);
//   - an obsolete route ("<@a,@b:user@example.org>"), a "mailto:" prefix, a
//     comment in front of an address;
//   - missing, repeated or unmatched angle brackets, an address after its
//     display name without angle brackets, a display name holding specials
//     such as "@" without quotes;
//   - a missing comma between addresses, ';' used as a separator, a group
//     that is never closed, a group inside a group;
//   - a quoted string or comment that is never closed, whose opening
//     character is then read as literal text;
//   - text that is not valid UTF-8, which is kept byte for byte.
//
// Empty list elements are skipped, as RFC 5322 allows. Raw UTF-8 (RFC 6532)
// is accepted and reported with address.raw_utf8; an internationalized domain
// is returned as it was written, with no conversion between its A-label and
// U-label forms.
//
// The work is linear in len(list): the value is scanned once into tokens, no
// construct is rescanned more than a constant number of times, and nesting is
// counted rather than recursed into. At most 16384 entries and 64 diagnostics
// are returned; what was cut is reported.
func ParseAddressListTolerant(list string, opts *AddressOptions) AddressListResult {
	return parseAddressListTolerant("", list, opts)
}

// AddressListTolerant parses the named header field as a list of addresses,
// keeping the addresses that can be understood when others cannot. If the
// header field is missing or empty, it returns the zero AddressListResult.
// See ParseAddressListTolerant.
//
// Like AddressList it reads the first field of that name; a caller that wants
// repeated fields merged can apply ParseAddressListTolerant to each of
// Header.Values.
func (h *Header) AddressListTolerant(key string, opts *AddressOptions) AddressListResult {
	v := h.Get(key)
	if v == "" {
		return AddressListResult{}
	}
	return parseAddressListTolerant(key, v, opts)
}

func parseAddressListTolerant(field, list string, opts *AddressOptions) AddressListResult {
	p := addrListParser{s: list}
	p.diags.field = field
	p.res.Raw = list
	if opts != nil {
		p.opts = *opts
	}
	if len(list) > math.MaxInt32 {
		p.diags.add(message.CodeAddressPartialList, message.SeverityLimitExceeded,
			"mail: address list of "+strconv.Itoa(len(list))+" bytes is too large to parse",
			"kept the raw value; no address was parsed")
		p.res.Diagnostics = p.diags.list
		return p.res
	}
	p.strict = netmail.AddressParser{WordDecoder: &mime.WordDecoder{CharsetReader: message.CharsetReader}}
	p.toks = tokenizeAddresses(list)
	p.parse()
	p.res.Diagnostics = p.diags.list
	return p.res
}

type addrTokKind uint8

const (
	atAtom    addrTokKind = iota // a run of text, dots and encoded-words included
	atQuoted                     // a quoted string, with its quotes
	atComment                    // a comment, with its parentheses
	atLiteral                    // a domain literal, with its brackets
	atSpecial                    // one of < > @ , ; :
	atJunk                       // a quote, parenthesis or bracket that opens or closes nothing
)

// An addrTok is one lexical token of an address list. It is kept small because
// a hostile value can hold one token per byte.
type addrTok struct {
	kind       addrTokKind
	c          byte // the byte of an atSpecial or atJunk token
	start, end int32
}

func (t addrTok) is(c byte) bool { return t.kind == atSpecial && t.c == c }

// isWord reports whether t can be a word of a phrase or of an addr-spec.
func (t addrTok) isWord() bool {
	return t.kind == atAtom || t.kind == atQuoted || t.kind == atLiteral
}

// tokenizeAddresses splits an address list into tokens in one pass.
//
// A quoted string or comment that is never closed would otherwise swallow the
// rest of the list. Its opening character becomes an atJunk token instead and
// scanning resumes right after it; because the failed search already reached
// the end of the value, no later search of that kind is attempted, so the
// whole value is read at most three times.
func tokenizeAddresses(s string) []addrTok {
	var toks []addrTok
	quoteBroken, commentBroken := false, false
	add := func(kind addrTokKind, c byte, start, end int) {
		toks = append(toks, addrTok{kind: kind, c: c, start: int32(start), end: int32(end)})
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case isWSP(c):
			i++
		case c == '"':
			if !quoteBroken {
				if end, ok := scanQuoted(s, i); ok {
					add(atQuoted, 0, i, end)
					i = end
					continue
				}
				quoteBroken = true
			}
			add(atJunk, c, i, i+1)
			i++
		case c == '(':
			if !commentBroken {
				if end, ok := scanComment(s, i); ok {
					add(atComment, 0, i, end)
					i = end
					continue
				}
				commentBroken = true
			}
			add(atJunk, c, i, i+1)
			i++
		case c == '[':
			if end, ok := scanDomainLiteral(s, i); ok {
				add(atLiteral, 0, i, end)
				i = end
				continue
			}
			add(atJunk, c, i, i+1)
			i++
		case c == ')' || c == ']':
			add(atJunk, c, i, i+1)
			i++
		case c == '<' || c == '>' || c == '@' || c == ',' || c == ';' || c == ':':
			add(atSpecial, c, i, i+1)
			i++
		default:
			end := scanAddrAtom(s, i)
			add(atAtom, 0, i, end)
			i = end
		}
	}
	return toks
}

// scanDomainLiteral returns the index just past the ']' of the domain literal
// opened by the '[' at s[i]. As in net/mail a literal holds no whitespace, so
// a bracket that is never closed costs at most the word it starts.
func scanDomainLiteral(s string, i int) (end int, ok bool) {
	for i++; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ']':
			return i + 1, true
		case c == '[' || c == '\\' || isWSP(c) || isControl(c):
			return 0, false
		}
	}
	return 0, false
}

// scanAddrAtom returns the end of the atom that starts at s[i]: everything up
// to whitespace or a character that is a token of its own. A backslash keeps
// the byte after it in the atom. An atom that starts as an RFC 2047
// encoded-word runs to the end of that word, so an encoded-word whose text
// holds an unencoded special stays one word.
func scanAddrAtom(s string, i int) int {
	if s[i] == '=' && i+1 < len(s) && s[i+1] == '?' {
		if end := scanEncodedWord(s, i); end > 0 {
			return end
		}
	}
	for i < len(s) {
		c := s[i]
		if c == '\\' {
			i += 2
			continue
		}
		if isWSP(c) {
			break
		}
		switch c {
		case '"', '(', ')', '[', ']', '<', '>', '@', ',', ';', ':':
			return i
		}
		i++
	}
	return min(i, len(s))
}

// scanEncodedWord returns the index just past the encoded-word that starts
// with the "=?" at s[i], or -1. It stops at the third '?' after that prefix
// or at whitespace, whichever comes first.
func scanEncodedWord(s string, i int) int {
	j := i + 2
	for q := 0; q < 3; q++ {
		for j < len(s) && s[j] != '?' && !isWSP(s[j]) {
			j++
		}
		if j >= len(s) || s[j] != '?' {
			return -1
		}
		j++
	}
	if j < len(s) && s[j] == '=' {
		return j + 1
	}
	return -1
}

type addrListParser struct {
	s      string
	toks   []addrTok
	opts   AddressOptions
	strict netmail.AddressParser
	diags  diagnosticList
	res    AddressListResult

	group      string // display name of the outermost open group
	groupDepth int
	lastSemi   int // index of the last ';' token, or -1

	// noMergeBefore is the token index up to which a display name is known
	// not to continue across a comma, so the look-ahead that decides it is
	// run once per run of words rather than once per comma.
	noMergeBefore int

	invalid int

	// These repairs concern the list rather than one entry and are reported
	// once however often they occur.
	reportedSemicolon, reportedMissingComma, reportedNestedGroup, reportedStray bool
}

func (p *addrListParser) text(t addrTok) string { return p.s[t.start:t.end] }

func (p *addrListParser) parse() {
	n := len(p.toks)
	p.lastSemi = -1
	for i := n - 1; i >= 0; i-- {
		if p.toks[i].is(';') {
			p.lastSemi = i
			break
		}
	}
	for i := 0; i < n; {
		t := p.toks[i]
		if t.is(',') {
			i++
			continue
		}
		if t.is(';') {
			p.closeGroup()
			i++
			continue
		}
		if len(p.res.Entries) >= maxTolerantEntries {
			p.diags.add(message.CodeAddressPartialList, message.SeverityLimitExceeded,
				"mail: address list holds more than "+strconv.Itoa(maxTolerantEntries)+" entries",
				"kept the first "+strconv.Itoa(maxTolerantEntries)+" entries; the rest of the value was not parsed")
			break
		}
		next, split := p.parseEntry(i)
		if next <= i {
			next = i + 1
		}
		if split && !p.reportedMissingComma {
			p.reportedMissingComma = true
			p.diags.recovered(message.CodeAddressInvalid,
				"mail: addresses are not separated by a comma",
				"read the text after an angle address as the next address")
		}
		i = next
	}
	if p.groupDepth > 0 {
		p.diags.recovered(message.CodeAddressInvalid,
			"mail: group "+quoteForMessage(p.group)+" is not closed by ';'",
			"kept the members read up to the end of the value")
	}
	if p.invalid > 0 {
		p.diags.recovered(message.CodeAddressPartialList,
			"mail: "+strconv.Itoa(p.invalid)+" of "+strconv.Itoa(len(p.res.Entries))+" address list entries could not be parsed",
			"kept the usable addresses ("+strconv.Itoa(len(p.res.Addresses))+") and the raw text of the other entries")
	}
}

func (p *addrListParser) closeGroup() {
	if p.groupDepth > 0 {
		p.groupDepth--
		if p.groupDepth == 0 {
			p.group = ""
		}
		return
	}
	if !p.reportedSemicolon {
		p.reportedSemicolon = true
		p.diags.recovered(message.CodeAddressInvalid,
			"mail: ';' outside a group",
			"read ';' as a separator between addresses")
	}
}

// parseEntry reads the entry that starts at token start and returns the index
// of the first token after it: a separator, or the start of the next entry
// when the separator is missing, which split reports.
func (p *addrListParser) parseEntry(start int) (next int, split bool) {
	toks := p.toks
	n := len(toks)
	const (
		angleNone = iota
		angleOpen
		angleClosed
	)
	var (
		words      int
		sawAt      bool
		angle      = angleNone
		filled     bool // a word follows the '<' that is still open
		inRoute    bool
		merged     bool
		mergeUntil = -1
	)
	j := start
scan:
	for ; j < n; j++ {
		t := toks[j]
		if angle == angleClosed {
			// Only comments and surplus '>' still belong to an address whose
			// angle brackets are closed.
			if t.kind == atComment || t.is('>') {
				continue
			}
			split = !t.is(',') && !t.is(';')
			break
		}
		if t.kind != atSpecial {
			if t.isWord() {
				words++
				filled = angle == angleOpen
			}
			continue
		}
		switch t.c {
		case '<':
			if angle == angleNone {
				angle = angleOpen
				inRoute = j+1 < n && toks[j+1].is('@')
			} else if filled {
				// A '<' after the content of one that is still open starts
				// the next address: both the '>' and the comma are missing.
				split = true
				break scan
			}
		case '>':
			if angle == angleOpen || sawAt {
				angle = angleClosed
			}
		case '@':
			sawAt = true
		case ':':
			if angle == angleOpen {
				if !inRoute && !sawAt && words > 0 && toks[start].is('<') && j+1 < n && toks[j+1].is(';') {
					// "<Undisclosed-Recipient:;>", as some mailers write
					// an empty group.
					name, _ := p.phrase(toks[start+1 : j])
					p.diags.recovered(message.CodeAddressInvalid,
						"mail: empty group "+quoteForMessage(name)+" is written inside angle brackets",
						"read it as an empty group")
					next = j + 2
					if next < n && toks[next].is('>') {
						next++
					}
					return next, false
				}
				inRoute = false
				continue
			}
			if !sawAt && words > 0 {
				return p.openGroup(start, j), false
			}
		case ',':
			if angle == angleOpen {
				// A comma inside angle brackets belongs to an obsolete route
				// ("<@a,@b:user@c>"); anywhere else the '>' is missing.
				if inRoute && j+1 < n && (toks[j+1].is('@') || toks[j+1].is(',') || toks[j+1].is(':')) {
					continue
				}
				break scan
			}
			if j < mergeUntil {
				continue
			}
			if !sawAt && words > 0 && !p.opts.StrictCommas {
				if until, ok := p.mergeAhead(j); ok {
					mergeUntil = until
					merged = true
					continue
				}
			}
			break scan
		case ';':
			break scan
		}
	}
	p.interpret(start, j, merged)
	return j, split
}

// mergeAhead decides whether the words before the comma at token index comma
// continue as a display name: they do when only words, comments and commas
// separate them from the next '<' and a word stands directly in front of it
// ("Doe, John <jd@example.org>", but not "postmaster, <jd@example.org>"). It
// returns the index of that '<'.
func (p *addrListParser) mergeAhead(comma int) (until int, ok bool) {
	if comma < p.noMergeBefore {
		return 0, false
	}
	word := false // a word follows the last comma seen
	for k := comma + 1; k < len(p.toks); k++ {
		t := p.toks[k]
		if t.kind != atSpecial {
			word = word || t.isWord()
			continue
		}
		if t.c == ',' {
			word = false
			continue
		}
		if t.c == '<' && word {
			return k, true
		}
		p.noMergeBefore = k
		return 0, false
	}
	p.noMergeBefore = len(p.toks)
	return 0, false
}

// openGroup handles the "phrase:" that tokens [start, colon) and the ':' at
// colon spell, and returns the index after the colon.
func (p *addrListParser) openGroup(start, colon int) int {
	if colon == start+1 && colon > p.lastSemi && p.toks[start].kind == atAtom && strings.EqualFold(p.text(p.toks[start]), "mailto") {
		// "mailto:" with no ';' after it is a URL scheme someone pasted, not
		// the name of a group.
		p.diags.recovered(message.CodeAddressInvalid,
			"mail: address has a \"mailto:\" prefix",
			"ignored the prefix")
		return colon + 1
	}
	name, _ := p.phrase(p.toks[start:colon])
	if p.groupDepth > 0 && !p.reportedNestedGroup {
		p.reportedNestedGroup = true
		p.diags.recovered(message.CodeAddressGroupDepth,
			"mail: group "+quoteForMessage(name)+" is nested in group "+quoteForMessage(p.group),
			"flattened the nested groups into the outermost group")
	}
	p.groupDepth++
	if p.groupDepth == 1 {
		p.group = name
	}
	return colon + 1
}

// An addrRepairs collects what was repaired in one entry, so the entry is
// reported with one address.invalid diagnostic however many repairs it took.
type addrRepairs struct {
	notes []string

	// unknownCharset is set when the display name holds an encoded-word in a
	// charset that cannot be decoded; it is reported with its own code.
	unknownCharset bool
}

func (r *addrRepairs) note(s string) {
	for _, n := range r.notes {
		if n == s {
			return
		}
	}
	r.notes = append(r.notes, s)
}

// interpret turns the tokens [start, end) of one entry into addresses.
func (p *addrListParser) interpret(start, end int, merged bool) {
	toks := p.toks[start:end]
	if len(toks) == 0 {
		return
	}
	onlyComments := true
	for _, t := range toks {
		if t.kind != atComment {
			onlyComments = false
			break
		}
	}
	if onlyComments {
		// CFWS between two separators is an empty list element.
		return
	}
	raw := p.s[toks[0].start:toks[len(toks)-1].end]
	stray := true
	for _, t := range toks {
		if t.kind != atComment && t.kind != atJunk && !t.is('>') {
			stray = false
			break
		}
	}
	if stray {
		// Closing delimiters that close nothing, and nothing else.
		if !p.reportedStray {
			p.reportedStray = true
			p.diags.recovered(message.CodeAddressInvalid,
				"mail: unmatched "+quoteForMessage(raw)+" between addresses",
				"ignored the unmatched delimiters")
		}
		return
	}

	if !merged {
		if addr, err := p.strict.Parse(raw); err == nil {
			e := AddressEntry{Raw: raw, Address: addr, Group: p.group}
			p.noteRawBytes(&e)
			p.appendEntry(e)
			return
		}
	}

	open := -1
	for k, t := range toks {
		if t.is('<') {
			open = k
			break
		}
	}
	if open >= 0 {
		p.interpretAngle(toks, open, raw, merged)
	} else {
		p.interpretBare(toks, raw, &addrRepairs{})
	}
}

// interpretAngle reads an entry that holds a '<': the address is what the
// angle brackets enclose and the text in front of them is the display name.
func (p *addrListParser) interpretAngle(toks []addrTok, open int, raw string, merged bool) {
	var rep addrRepairs
	k := open + 1
	for k < len(toks) && toks[k].is('<') {
		rep.note("ignored a repeated '<'")
		k++
	}
	closing := len(toks)
	for x := k; x < len(toks); x++ {
		if toks[x].is('>') {
			closing = x
			break
		}
	}
	if closing == len(toks) {
		rep.note("supplied the missing '>'")
	} else {
		for _, t := range toks[closing+1:] {
			if t.is('>') {
				rep.note("ignored a repeated '>'")
				break
			}
		}
	}
	inner := toks[k:closing]
	if len(inner) > 0 && inner[0].is('@') {
		for x, t := range inner {
			if t.is(':') {
				inner = inner[x+1:]
				rep.note("ignored the obsolete route")
				break
			}
		}
	}
	if len(inner) >= 2 && inner[0].kind == atAtom && inner[1].is(':') && strings.EqualFold(p.text(inner[0]), "mailto") {
		inner = inner[2:]
		rep.note("ignored the \"mailto:\" prefix")
	}

	spec, localOnly, ok := p.addrSpec(inner, &rep)
	if !ok {
		for _, t := range toks[:open] {
			if t.is('@') {
				// "a@example.org <>": the address stands in front of angle
				// brackets that hold none.
				bare := addrRepairs{notes: []string{"ignored the angle brackets, which hold no address"}}
				p.interpretBare(toks[:open], raw, &bare)
				return
			}
		}
		p.appendInvalid(raw, "no address between the angle brackets")
		return
	}
	name, nameRepairs := p.phrase(toks[:open])
	nameRepairs.noteTo(&rep)
	for _, t := range toks[:open] {
		if t.kind == atJunk {
			rep.note("ignored an unmatched " + strconv.QuoteRune(rune(t.c)) + " in the display name")
		} else if t.kind == atSpecial && t.c != ',' {
			rep.note("kept the unquoted " + strconv.QuoteRune(rune(t.c)) + " in the display name")
		} else if t.kind == atComment && name == "" {
			rep.note("ignored the comment in front of the address")
		}
	}
	e := AddressEntry{Raw: raw, Address: &Address{Name: name, Address: spec}, Group: p.group}
	p.finishEntry(&e, &rep, merged, localOnly)
}

// interpretBare reads an entry without angle brackets: every "local@domain"
// in it is an address, and the words in front of one are its display name.
// rep holds the repairs already made to the entry.
func (p *addrListParser) interpretBare(toks []addrTok, raw string, rep *addrRepairs) {
	type found struct {
		name       string
		spec       string
		start, end int // token range, for Raw
	}
	var (
		list    []found
		pending = 0
	)
	for k := 1; k+1 < len(toks); k++ {
		if !toks[k].is('@') || k-1 < pending {
			continue
		}
		local, domain := toks[k-1], toks[k+1]
		if (local.kind != atAtom && local.kind != atQuoted) || (domain.kind != atAtom && domain.kind != atLiteral) {
			continue
		}
		spec, ok := p.joinSpec(local, domain)
		if !ok {
			continue
		}
		name, nameRepairs := p.phrase(toks[pending : k-1])
		nameRepairs.noteTo(rep)
		if name != "" {
			rep.note("read the words in front of the address as its display name")
		}
		list = append(list, found{name: name, spec: spec, start: pending, end: k + 2})
		pending = k + 2
		k++
	}

	var words []addrTok
	comment := ""
	for _, t := range toks[pending:] {
		switch {
		case t.kind == atComment:
			if comment == "" {
				comment = commentText(p.text(t))
			}
		case t.kind == atJunk || t.is('>'):
			rep.note("ignored an unmatched " + strconv.QuoteRune(rune(t.c)))
		default:
			words = append(words, t)
		}
	}

	if len(list) == 0 {
		// No addr-spec: a single word is an address without a domain.
		if len(words) == 1 && words[0].kind == atAtom && !isEncodedWord(p.text(words[0])) {
			if local, ok := p.localPart(words[0]); ok {
				e := AddressEntry{Raw: raw, Address: &Address{Name: comment, Address: local}, Group: p.group}
				p.finishEntry(&e, rep, false, true)
				return
			}
		}
		p.appendInvalid(raw, "no address found")
		return
	}
	if len(words) > 0 {
		rep.note("ignored the text after the address")
	}
	if len(list) > 1 {
		rep.note("read " + strconv.Itoa(len(list)) + " addresses that no comma separates")
	}
	last := &list[len(list)-1]
	if last.name == "" && comment != "" {
		// As in net/mail, a comment after a bare address is its display name.
		last.name = comment
	}
	for x, f := range list {
		entryRaw := raw
		if len(list) > 1 {
			entryRaw = p.s[toks[f.start].start:toks[f.end-1].end]
		}
		e := AddressEntry{Raw: entryRaw, Address: &Address{Name: f.name, Address: f.spec}, Group: p.group}
		if x == 0 {
			p.finishEntry(&e, rep, false, false)
		} else {
			p.finishEntry(&e, &addrRepairs{notes: []string{"read an address that no comma separates from the one before it"}}, false, false)
		}
	}
}

// addrSpec reads "local@domain", or a local part alone, from the tokens
// between angle brackets.
func (p *addrListParser) addrSpec(toks []addrTok, rep *addrRepairs) (spec string, localOnly, ok bool) {
	var parts []addrTok
	for _, t := range toks {
		switch t.kind {
		case atComment:
			continue
		case atJunk:
			rep.note("ignored an unmatched " + strconv.QuoteRune(rune(t.c)))
			continue
		}
		if t.is('<') || t.is('>') {
			continue
		}
		parts = append(parts, t)
		if len(parts) == 4 {
			break
		}
	}
	if len(parts) == 0 || (parts[0].kind != atAtom && parts[0].kind != atQuoted) {
		return "", false, false
	}
	if len(parts) == 1 || !parts[1].is('@') {
		local, ok := p.localPart(parts[0])
		if !ok {
			return "", false, false
		}
		if len(parts) > 1 {
			rep.note("ignored the text after the address")
		}
		return local, true, true
	}
	if len(parts) < 3 || (parts[2].kind != atAtom && parts[2].kind != atLiteral) {
		return "", false, false
	}
	spec, ok = p.joinSpec(parts[0], parts[2])
	if !ok {
		return "", false, false
	}
	if len(parts) > 3 {
		rep.note("ignored the text after the address")
	}
	return spec, false, true
}

func (p *addrListParser) localPart(t addrTok) (string, bool) {
	local := p.text(t)
	if t.kind == atQuoted {
		local = unescape(local[1 : len(local)-1])
	} else if strings.IndexByte(local, '\\') >= 0 {
		return "", false
	}
	if local == "" || hasControl(local) {
		return "", false
	}
	return local, true
}

func (p *addrListParser) joinSpec(local, domain addrTok) (string, bool) {
	l, ok := p.localPart(local)
	if !ok {
		return "", false
	}
	d := p.text(domain)
	if d == "" || hasControl(d) {
		return "", false
	}
	return l + "@" + d, true
}

// A phraseRepairs says what phrase had to do to a display name.
type phraseRepairs struct {
	control        bool // control characters were removed
	undecoded      bool // a malformed encoded-word was kept as written
	unknownCharset bool // an encoded-word in an unknown charset was kept as written
}

func (r phraseRepairs) noteTo(rep *addrRepairs) {
	if r.control {
		rep.note("removed control characters from the display name")
	}
	if r.undecoded {
		rep.note("kept a malformed encoded-word as written")
	}
	rep.unknownCharset = rep.unknownCharset || r.unknownCharset
}

// phrase renders the tokens of a display name. Words are joined by one space,
// except that tokens with nothing between them in the source stay joined and
// adjacent encoded-words are concatenated, as net/mail does. Comments and
// unmatched delimiters are left out.
func (p *addrListParser) phrase(toks []addrTok) (name string, repaired phraseRepairs) {
	var (
		b           strings.Builder
		prevEnd     = int32(-1)
		prevEncoded bool
	)
	for _, t := range toks {
		var (
			text    string
			encoded bool
		)
		switch t.kind {
		case atComment:
			continue
		case atJunk:
			if t.start == prevEnd {
				prevEnd = t.end
			}
			continue
		case atQuoted, atAtom:
			text = p.text(t)
			if t.kind == atQuoted {
				text = unescape(text[1 : len(text)-1])
			}
			var unknownCharset bool
			text, encoded, unknownCharset = decodeWord(text)
			switch {
			case unknownCharset:
				repaired.unknownCharset = true
			case !encoded && isEncodedWord(text):
				repaired.undecoded = true
			case !encoded && t.kind == atAtom:
				text = unescape(text)
			}
		default:
			text = p.text(t)
		}
		if b.Len() > 0 && t.start != prevEnd && !(encoded && prevEncoded) {
			b.WriteByte(' ')
		}
		b.WriteString(text)
		prevEnd, prevEncoded = t.end, encoded
	}
	name = b.String()
	if hasControl(name) {
		name, repaired.control = stripControl(name), true
	}
	return strings.TrimRight(name, ", \t"), repaired
}

// finishEntry reports what was repaired in e and appends it.
func (p *addrListParser) finishEntry(e *AddressEntry, rep *addrRepairs, merged, localOnly bool) {
	if localOnly && p.opts.RequireDomain {
		p.appendInvalid(e.Raw, "address has no domain")
		return
	}
	reported := false
	if merged {
		reported = true
		p.entryDiag(e, message.CodeAddressUnquotedComma, message.SeverityRecovered,
			"mail: display name holds an unquoted comma",
			"read "+quoteForMessage(e.Address.Name)+" as the display name of one address")
	}
	if localOnly {
		reported = true
		p.entryDiag(e, message.CodeAddressLocalOnly, message.SeverityRecovered,
			"mail: address "+quoteForMessage(e.Address.Address)+" has no domain",
			"kept the local part as the address")
	}
	if p.noteRawBytes(e) {
		reported = true
	}
	if rep.unknownCharset {
		reported = true
		p.entryDiag(e, message.CodeRFC2047UnknownCharset, message.SeverityUnsupported,
			"mail: display name holds an encoded-word in a charset that cannot be decoded",
			"kept the encoded-word as written")
	}
	if len(rep.notes) > 0 {
		p.entryDiag(e, message.CodeAddressInvalid, message.SeverityRecovered,
			"mail: address "+quoteForMessage(e.Raw)+" is not valid RFC 5322 syntax",
			strings.Join(rep.notes, "; "))
	} else if !reported {
		p.entryDiag(e, message.CodeAddressInvalid, message.SeverityRecovered,
			"mail: address "+quoteForMessage(e.Raw)+" is not valid RFC 5322 syntax",
			"kept the address as written")
	}
	p.appendEntry(*e)
}

// noteRawBytes reports the non-ASCII bytes of an entry: raw UTF-8, which RFC
// 6532 permits, or bytes in no known encoding, which are kept as they are. It
// reports whether the entry holds bytes that are not valid UTF-8.
func (p *addrListParser) noteRawBytes(e *AddressEntry) (undecodable bool) {
	if isASCII(e.Raw) {
		return false
	}
	if utf8.ValidString(e.Raw) {
		p.entryDiag(e, message.CodeAddressRawUTF8, message.SeverityInfo,
			"mail: address holds raw UTF-8 (RFC 6532)", "")
		return false
	}
	p.entryDiag(e, message.CodeAddressRawUTF8, message.SeverityUnsupported,
		"mail: address holds 8-bit text that is not valid UTF-8",
		"kept the undecodable bytes as written")
	return true
}

func (p *addrListParser) appendInvalid(raw, why string) {
	e := AddressEntry{Raw: raw, Group: p.group}
	p.entryDiag(&e, message.CodeAddressInvalid, message.SeverityInvalidMetadata,
		"mail: invalid address "+quoteForMessage(raw)+": "+why,
		"kept the raw text; continued with the next address")
	p.invalid++
	p.appendEntry(e)
}

func (p *addrListParser) appendEntry(e AddressEntry) {
	p.res.Entries = append(p.res.Entries, e)
	if e.Address != nil {
		p.res.Addresses = append(p.res.Addresses, e.Address)
	}
}

// entryDiag records a diagnostic for the list and, when the list still had
// room for it, for the entry.
func (p *addrListParser) entryDiag(e *AddressEntry, code message.Code, severity message.Severity, msg, action string) {
	before := len(p.diags.list)
	p.diags.add(code, severity, msg, action)
	if len(p.diags.list) > before && !p.diags.full {
		e.Diagnostics = append(e.Diagnostics, p.diags.list[before])
	}
}
