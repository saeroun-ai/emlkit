package mail

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/saeroun-ai/emlkit"
)

// A MsgIDEntry is one message identifier of a field, or one damaged attempt
// at one.
type MsgIDEntry struct {
	// Raw is the entry as it appears in the field value, with its angle
	// brackets when it has them. It is always set.
	Raw string

	// ID is the identifier without angle brackets, or empty when the entry
	// holds no usable identifier. An identifier that is usable but not valid
	// RFC 5322 syntax is kept as it was written, apart from whitespace inside
	// the angle brackets, which is removed.
	ID string
}

// A MsgIDListResult is what the tolerant parser understood of a list of
// message identifiers.
type MsgIDListResult struct {
	// Raw is the field value that was parsed.
	Raw string

	// IDs are the usable identifiers, without angle brackets, in the order
	// they appear.
	IDs []string

	// Entries are all entries in the order they appear, including those
	// without a usable identifier.
	Entries []MsgIDEntry

	// Diagnostics lists every repair and every entry that was kept raw only.
	// It is empty for a field RFC 5322 accepts as written.
	Diagnostics []message.Diagnostic
}

// A MessageIDResult is what the tolerant parser understood of a Message-ID
// field.
type MessageIDResult struct {
	// Raw is the field value that was parsed.
	Raw string

	// ID is the message identifier without angle brackets, or empty when the
	// field holds none.
	ID string

	// Diagnostics lists what was repaired, or why no identifier was found.
	Diagnostics []message.Diagnostic
}

// ParseMsgIDListTolerant parses a list of message identifiers, as found in
// In-Reply-To and References, and keeps every identifier it can still
// recognize in a damaged one.
//
// An identifier is whatever one pair of angle brackets encloses. It is kept
// as written, and reported with message_id.invalid, when it is not valid RFC
// 5322 syntax: more than one "@" or none, an empty side, characters outside
// atext, bytes that are not valid UTF-8, or whitespace inside the brackets,
// which is removed. Further repairs, each reported:
//
//   - a '<' that is never closed: the word after it is the identifier, at
//     incomplete severity when the value ends there (a truncated field);
//   - identifiers without angle brackets, when the field has none with them;
//   - commas between identifiers, and words that are no identifier, such as
//     the phrases RFC 822 allowed in In-Reply-To, which are skipped;
//   - a comment that is never closed, whose '(' is read as text so the
//     identifiers after it are still found.
//
// An entry with no usable identifier ("<>", or one holding control
// characters) is kept in Entries with its raw text and reported; the list is
// then also reported with message_id.partial_list.
//
// The work is linear in len(list): one pass, in which each angle bracket is
// looked at once. At most 16384 entries and 64 diagnostics are returned.
func ParseMsgIDListTolerant(list string) MsgIDListResult {
	p := msgIDParser{s: list}
	p.parse()
	return p.result()
}

// MsgIDListTolerant parses the named header field as a list of message
// identifiers, keeping those that can be recognized when others cannot. If
// the header field is missing or empty, it returns the zero MsgIDListResult.
// See ParseMsgIDListTolerant.
//
// This can be used on In-Reply-To and References header fields.
func (h *Header) MsgIDListTolerant(key string) MsgIDListResult {
	v := h.Get(key)
	if v == "" {
		return MsgIDListResult{}
	}
	p := msgIDParser{s: v}
	p.d.field = key
	p.parse()
	return p.result()
}

// MessageIDTolerant parses the Message-ID field as ParseMsgIDListTolerant
// parses a list, and returns its first usable identifier. A field that holds
// several identifiers, or a single word with neither angle brackets nor "@",
// is reported. If the header field is missing or empty, it returns the zero
// MessageIDResult.
func (h *Header) MessageIDTolerant() MessageIDResult {
	v := h.Get("Message-Id")
	if v == "" {
		return MessageIDResult{}
	}
	p := msgIDParser{s: v, single: true}
	p.d.field = "Message-Id"
	p.parse()
	list := p.result()
	res := MessageIDResult{Raw: v, Diagnostics: list.Diagnostics}
	if len(list.IDs) > 0 {
		res.ID = list.IDs[0]
	}
	return res
}

type msgIDParser struct {
	s string
	d diagnosticList

	// single is set for the Message-ID field, which holds one identifier.
	single bool

	entries []MsgIDEntry
	bare    []MsgIDEntry // words holding "@" but no angle brackets

	skipped      int    // words that are no identifier
	firstSkipped string // the first of them
	invalid      int

	commentBroken, quoteBroken bool
	reportedComma              bool
	limited                    bool
}

func (p *msgIDParser) parse() {
	s := p.s
	for i := 0; i < len(s) && !p.limited; {
		c := s[i]
		switch {
		case isWSP(c):
			i++
		case c == ',':
			if !p.reportedComma {
				p.reportedComma = true
				p.d.recovered(message.CodeMessageIDInvalid,
					"mail: message identifiers are separated by commas",
					"ignored the commas")
			}
			i++
		case c == '(':
			if !p.commentBroken {
				if end, ok := scanComment(s, i); ok {
					i = end
					continue
				}
				p.commentBroken = true
				p.d.recovered(message.CodeMessageIDInvalid,
					"mail: comment is never closed",
					"read its '(' as text and looked for identifiers after it")
			}
			i++
		case c == '"':
			// A quoted phrase, as in the RFC 822 form
			// In-Reply-To: Your message of "Wed, 22 Jan 2020" <id@host>.
			if !p.quoteBroken {
				if end, ok := scanQuoted(s, i); ok {
					p.skip(s[i:end])
					i = end
					continue
				}
				p.quoteBroken = true
			}
			i++
		case c == '<':
			i = p.bracketed(i)
		case c == '>' || c == ')':
			p.skip(s[i : i+1])
			i++
		default:
			j := i
			for j < len(s) && !isWSP(s[j]) && !strings.ContainsRune(",<>()\"", rune(s[j])) {
				j++
			}
			word := s[i:j]
			if at := strings.IndexByte(word, '@'); at > 0 && at < len(word)-1 && !hasControl(word) && len(p.bare) < maxTolerantEntries {
				p.bare = append(p.bare, MsgIDEntry{Raw: word, ID: word})
			} else {
				p.skip(word)
			}
			i = j
		}
	}
}

func (p *msgIDParser) skip(word string) {
	if p.skipped == 0 {
		p.firstSkipped = word
	}
	p.skipped++
}

// bracketed reads the identifier opened by the '<' at s[i] and returns the
// index to continue at. The search for its '>' stops at the next '<' and
// parsing continues where the search stopped, so the regions searched for
// different identifiers never overlap. A domain literal is stepped over
// whole, as RFC 5322 lets it hold '<' and '>'.
func (p *msgIDParser) bracketed(i int) int {
	s := p.s
	if len(p.entries) >= maxTolerantEntries {
		p.limited = true
		p.d.add(message.CodeMessageIDPartialList, message.SeverityLimitExceeded,
			"mail: field holds more than "+strconv.Itoa(maxTolerantEntries)+" message identifiers",
			"kept the first "+strconv.Itoa(maxTolerantEntries)+"; the rest of the value was not parsed")
		return len(s)
	}
	j := i + 1
	for j < len(s) && s[j] != '>' && s[j] != '<' {
		if s[j] == '[' {
			if end, ok := scanDomainLiteral(s, j); ok {
				j = end
				continue
			}
		}
		j++
	}
	var (
		notes   []string
		raw, id string
		next    int
		cut     bool
	)
	if j < len(s) && s[j] == '>' {
		raw, id, next = s[i:j+1], s[i+1:j], j+1
		if stripped := stripWSP(id); stripped != id {
			id = stripped
			notes = append(notes, "removed the whitespace inside the angle brackets")
		}
	} else {
		// No '>' before the next '<' or the end of the value: the word after
		// the '<' is the identifier, and the words after that one belong to
		// no identifier.
		k := i + 1
		for k < j && isWSP(s[k]) {
			k++
		}
		start := k
		for k < j && !isWSP(s[k]) {
			k++
		}
		raw, id, next = s[i:k], s[start:k], j
		cut = j == len(s) && trimWSP(s[k:]) == ""
		notes = append(notes, "supplied the missing '>'")
		for m := k; m < j; {
			if isWSP(s[m]) {
				m++
				continue
			}
			w := m
			for m < j && !isWSP(s[m]) {
				m++
			}
			p.skip(s[w:m])
		}
	}

	if id == "" || hasControl(id) {
		why := "empty message identifier"
		if id != "" {
			why = "message identifier holds control characters"
		}
		p.d.invalid(message.CodeMessageIDInvalid,
			"mail: invalid message identifier "+quoteForMessage(raw)+": "+why,
			"kept the raw text; continued with the next identifier")
		p.entries = append(p.entries, MsgIDEntry{Raw: raw})
		p.invalid++
		return next
	}

	if strict := (headerParser{s: raw}); len(notes) == 0 {
		if got, err := strict.parseMsgID(); err == nil && strict.empty() && got == id {
			p.entries = append(p.entries, MsgIDEntry{Raw: raw, ID: id})
			return next
		}
	}
	switch at := strings.Count(id, "@"); {
	case at == 0:
		notes = append(notes, "kept an identifier without \"@\"")
	case at > 1:
		notes = append(notes, "kept an identifier with more than one \"@\"")
	case id[0] == '@' || id[len(id)-1] == '@':
		notes = append(notes, "kept an identifier with an empty side")
	}
	if !utf8.ValidString(id) {
		notes = append(notes, "kept bytes that are not valid UTF-8")
	}
	if len(notes) == 0 {
		notes = append(notes, "kept characters RFC 5322 does not allow in an identifier")
	}
	severity := message.SeverityRecovered
	msg := "mail: message identifier " + quoteForMessage(raw) + " is not valid RFC 5322 syntax"
	if cut {
		severity = message.SeverityIncomplete
		msg = "mail: message identifier " + quoteForMessage(raw) + " is cut off at the end of the value"
	}
	p.d.add(message.CodeMessageIDInvalid, severity, msg, strings.Join(notes, "; "))
	p.entries = append(p.entries, MsgIDEntry{Raw: raw, ID: id})
	return next
}

func (p *msgIDParser) result() MsgIDListResult {
	usable := 0
	for _, e := range p.entries {
		if e.ID != "" {
			usable++
		}
	}
	switch {
	case usable == 0 && len(p.bare) > 0:
		// No identifier in angle brackets: the words that look like one are
		// the identifiers.
		p.entries = append(p.entries, p.bare...)
		p.d.recovered(message.CodeMessageIDInvalid,
			"mail: message identifier "+quoteForMessage(p.bare[0].Raw)+" has no angle brackets",
			"read the words holding \"@\" ("+strconv.Itoa(len(p.bare))+") as identifiers")
	case usable == 0 && p.single && p.skipped == 1 && len(p.entries) == 0 && !hasControl(p.firstSkipped):
		p.entries = append(p.entries, MsgIDEntry{Raw: p.firstSkipped, ID: p.firstSkipped})
		p.skipped = 0
		p.d.recovered(message.CodeMessageIDInvalid,
			"mail: message identifier "+quoteForMessage(p.firstSkipped)+" has neither angle brackets nor \"@\"",
			"kept the word as the identifier")
	default:
		p.skipped += len(p.bare)
		if p.firstSkipped == "" && len(p.bare) > 0 {
			p.firstSkipped = p.bare[0].Raw
		}
	}
	if p.skipped > 0 {
		p.d.recovered(message.CodeMessageIDInvalid,
			"mail: field holds text that is not a message identifier: "+strconv.Itoa(p.skipped)+" word(s), the first "+quoteForMessage(p.firstSkipped),
			"ignored that text")
	}

	res := MsgIDListResult{Raw: p.s, Entries: p.entries}
	for _, e := range p.entries {
		if e.ID != "" {
			res.IDs = append(res.IDs, e.ID)
		}
	}
	if p.invalid > 0 {
		p.d.recovered(message.CodeMessageIDPartialList,
			"mail: "+strconv.Itoa(p.invalid)+" of "+strconv.Itoa(len(p.entries))+" message identifiers are not usable",
			"kept the usable identifiers ("+strconv.Itoa(len(res.IDs))+") and the raw text of the other entries")
	}
	if p.single && len(res.IDs) > 1 {
		p.d.recovered(message.CodeMessageIDInvalid,
			"mail: field holds "+strconv.Itoa(len(res.IDs))+" message identifiers",
			"kept the first one")
	}
	res.Diagnostics = p.d.list
	return res
}
