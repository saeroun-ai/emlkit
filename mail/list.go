package mail

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/saeroun-ai/emlkit"
)

// A ListURL is one URL of a list command field, or one damaged attempt at
// one.
type ListURL struct {
	// Raw is the entry as it appears in the field value, with its angle
	// brackets when it has them. It is always set.
	Raw string

	// Text is the URL as written between the angle brackets, with the
	// whitespace RFC 2369 tells a client to ignore removed.
	Text string

	// URL is Text parsed, or nil when Text is not a URL net/url can parse.
	URL *url.URL
}

// A ListURLsResult is what the tolerant parser understood of a list command
// field.
type ListURLsResult struct {
	// Raw is the field value that was parsed.
	Raw string

	// URLs are the parsed URLs, in the order they appear, which is the
	// sender's order of preference.
	URLs []*url.URL

	// Entries are all entries in the order they appear, including those
	// whose URL could not be parsed.
	Entries []ListURL

	// No reports that the field holds the special value "NO", which RFC 2369
	// defines for List-Post to say that posting is not allowed.
	No bool

	// Diagnostics lists every repair and every entry that was kept raw only.
	// It is empty for a field RFC 2369 accepts as written.
	Diagnostics []message.Diagnostic
}

// ParseListURLs parses the value of a list command field (RFC 2369) and keeps
// every URL it can still recognize in a damaged one.
//
// A URL is whatever one pair of angle brackets encloses; whitespace inside
// the brackets is removed, as RFC 2369 section 2 asks, and reported. Comments
// are skipped wherever they stand. The word NO, in any case, sets No rather
// than adding an entry. Further repairs, each reported with
// list_header.invalid:
//
//   - a '<' that is never closed: the word after it is the URL, at incomplete
//     severity when the value ends there;
//   - a URL without angle brackets, recognized by its scheme, and a '>'
//     without its '<';
//   - a URL without a scheme, which is kept;
//   - words that are neither a URL nor NO, which are skipped;
//   - a comment that is never closed, whose '(' is read as text.
//
// An entry net/url cannot parse, and an empty "<>", is kept in Entries with
// its text and a nil URL, and reported at invalid_metadata severity; the URLs
// around it are unaffected.
//
// The work is linear in len(value): one pass, in which each angle bracket is
// looked at once. At most 16384 entries and 64 diagnostics are returned.
func ParseListURLs(value string) ListURLsResult {
	p := listURLParser{s: value}
	p.parse()
	return p.result()
}

// ListURLs parses the named list command header field, keeping the URLs that
// can be recognized when others cannot. If the header field is missing or
// empty, it returns the zero ListURLsResult. See ParseListURLs.
//
// This can be used on List-Help, List-Unsubscribe, List-Subscribe, List-Post,
// List-Owner and List-Archive header fields. Unlike ListCommandURLList it
// reports List-Post's "NO" in ListURLsResult.No rather than as a nil URL.
func (h *Header) ListURLs(key string) ListURLsResult {
	v := h.Get(key)
	if v == "" {
		return ListURLsResult{}
	}
	p := listURLParser{s: v}
	p.d.field = key
	p.parse()
	return p.result()
}

type listURLParser struct {
	s       string
	d       diagnosticList
	entries []ListURL
	no      bool

	skipped      int
	firstSkipped string

	commentBroken bool
	limited       bool
}

func (p *listURLParser) parse() {
	s := p.s
	for i := 0; i < len(s) && !p.limited; {
		c := s[i]
		switch {
		case isWSP(c) || c == ',':
			i++
		case c == '(':
			if !p.commentBroken {
				if end, ok := scanComment(s, i); ok {
					i = end
					continue
				}
				p.commentBroken = true
				p.d.recovered(message.CodeListHeaderInvalid,
					"mail: comment is never closed",
					"read its '(' as text and looked for URLs after it")
			}
			i++
		case c == '<':
			i = p.bracketed(i)
		case c == '>' || c == ')':
			p.skip(s[i : i+1])
			i++
		default:
			j := i
			for j < len(s) && !isWSP(s[j]) && s[j] != '<' && s[j] != '>' && (s[j] != '(' || p.commentBroken) {
				j++
			}
			word := strings.TrimRight(s[i:j], ",")
			closed := j < len(s) && s[j] == '>'
			next := j
			if closed {
				next = j + 1
			}
			switch {
			case word == "":
			case strings.EqualFold(word, "NO") && !closed:
				p.no = true
			case hasURLScheme(word):
				raw := word
				note := "supplied the missing angle brackets"
				if closed {
					raw = s[i : j+1]
					note = "supplied the missing '<'"
				}
				p.entry(raw, word, []string{note}, false)
			default:
				p.skip(word)
			}
			i = next
		}
	}
}

// hasURLScheme reports whether word starts with a URL scheme and has text
// after it.
func hasURLScheme(word string) bool {
	colon := strings.IndexByte(word, ':')
	if colon < 1 || colon == len(word)-1 || !isAlpha(word[0]) {
		return false
	}
	for i := 1; i < colon; i++ {
		c := word[i]
		if !isAlpha(c) && !isDigit(c) && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

func (p *listURLParser) skip(word string) {
	if p.skipped == 0 {
		p.firstSkipped = word
	}
	p.skipped++
}

// bracketed reads the URL opened by the '<' at s[i] and returns the index to
// continue at. The search for its '>' stops at the next '<', so the regions
// searched for different URLs never overlap.
func (p *listURLParser) bracketed(i int) int {
	s := p.s
	j := i + 1
	for j < len(s) && s[j] != '>' && s[j] != '<' {
		j++
	}
	if j < len(s) && s[j] == '>' {
		text := s[i+1 : j]
		var notes []string
		if stripped := stripWSP(text); stripped != text {
			text = stripped
			notes = append(notes, "removed the whitespace inside the angle brackets")
		}
		p.entry(s[i:j+1], text, notes, false)
		return j + 1
	}
	// No '>' before the next '<' or the end of the value: the word after the
	// '<' is the URL.
	k := i + 1
	for k < j && isWSP(s[k]) {
		k++
	}
	start := k
	for k < j && !isWSP(s[k]) {
		k++
	}
	text := s[start:k]
	if k < len(s) {
		// A comma here separates this URL from the next one.
		text = strings.TrimRight(text, ",")
	}
	p.entry(s[i:k], text, []string{"supplied the missing '>'"}, j == len(s) && trimWSP(s[k:]) == "")
	return k
}

// entry records one URL. notes are the repairs already made to it; cut
// reports that the value ends inside it.
func (p *listURLParser) entry(raw, text string, notes []string, cut bool) {
	if len(p.entries) >= maxTolerantEntries {
		if !p.limited {
			p.limited = true
			p.d.add(message.CodeListHeaderInvalid, message.SeverityLimitExceeded,
				"mail: field holds more than "+strconv.Itoa(maxTolerantEntries)+" URLs",
				"kept the first "+strconv.Itoa(maxTolerantEntries)+"; the rest of the value was not parsed")
		}
		return
	}
	e := ListURL{Raw: raw, Text: text}
	if text == "" {
		p.d.invalid(message.CodeListHeaderInvalid,
			"mail: empty URL "+quoteForMessage(raw),
			"kept the raw text; continued with the next URL")
		p.entries = append(p.entries, e)
		return
	}
	u, err := url.Parse(text)
	if err != nil {
		p.d.invalid(message.CodeListHeaderInvalid,
			"mail: invalid URL "+quoteForMessage(text),
			"kept its text; continued with the next URL")
		if n := len(p.d.list); n > 0 && !p.d.full {
			p.d.list[n-1].Cause = err
		}
		p.entries = append(p.entries, e)
		return
	}
	e.URL = u
	if u.Scheme == "" {
		notes = append(notes, "kept a URL without a scheme")
	}
	if len(notes) > 0 {
		severity := message.SeverityRecovered
		msg := "mail: URL " + quoteForMessage(raw) + " is not written as RFC 2369 requires"
		if cut {
			severity = message.SeverityIncomplete
			msg = "mail: URL " + quoteForMessage(raw) + " is cut off at the end of the value"
		}
		p.d.add(message.CodeListHeaderInvalid, severity, msg, strings.Join(notes, "; "))
	}
	p.entries = append(p.entries, e)
}

func (p *listURLParser) result() ListURLsResult {
	if p.skipped > 0 {
		p.d.recovered(message.CodeListHeaderInvalid,
			"mail: field holds text that is neither a URL nor NO: "+strconv.Itoa(p.skipped)+" word(s), the first "+quoteForMessage(p.firstSkipped),
			"ignored that text")
	}
	if p.no && len(p.entries) > 0 {
		p.d.recovered(message.CodeListHeaderInvalid,
			"mail: field holds both NO and URLs",
			"kept both")
	}
	res := ListURLsResult{Raw: p.s, Entries: p.entries, No: p.no}
	for _, e := range p.entries {
		if e.URL != nil {
			res.URLs = append(res.URLs, e.URL)
		}
	}
	res.Diagnostics = p.d.list
	return res
}

// A ListIDResult is what the tolerant parser understood of a List-Id field.
type ListIDResult struct {
	// Raw is the field value that was parsed.
	Raw string

	// ID is the list identifier without angle brackets, or empty when the
	// field holds none.
	ID string

	// Description is the phrase in front of the identifier, with its quotes
	// removed and its encoded-words decoded.
	Description string

	// Diagnostics lists what was repaired, or why no identifier was found.
	Diagnostics []message.Diagnostic
}

// ParseListID parses the value of a List-Id field (RFC 2919): an optional
// phrase followed by the list identifier in angle brackets.
//
// The identifier is the content of the last pair of angle brackets, with
// whitespace removed, and the words in front of it are the description.
// Reported with list_header.invalid: an identifier without angle brackets (a
// value of one word), a '<' that is never closed, more than one identifier,
// text after the identifier, whitespace or control characters that had to be
// removed, and a value with no identifier at all, which is kept in
// Description and Raw.
//
// The work is linear in len(value): one pass.
func ParseListID(value string) ListIDResult {
	return parseListID("", value)
}

// ListID parses the List-Id header field. If the header field is missing or
// empty, it returns the zero ListIDResult. See ParseListID.
func (h *Header) ListID() ListIDResult {
	v := h.Get("List-Id")
	if v == "" {
		return ListIDResult{}
	}
	return parseListID("List-Id", v)
}

func parseListID(field, s string) ListIDResult {
	res := ListIDResult{Raw: s}
	d := diagnosticList{field: field}
	var (
		desc          strings.Builder
		prevEncoded   bool
		words         int
		lastWord      string
		ids           int
		id            string
		notes         []string
		trailing      bool
		commentBroken bool
		quoteBroken   bool
		badCharset    bool
	)
	note := func(n string) {
		for _, have := range notes {
			if have == n {
				return
			}
		}
		notes = append(notes, n)
	}
	word := func(text string, quoted bool) {
		if ids > 0 {
			trailing = true
			return
		}
		words++
		lastWord = text
		if quoted {
			text = unescape(text[1 : len(text)-1])
		}
		text, encoded, unknownCharset := decodeWord(text)
		badCharset = badCharset || unknownCharset
		if desc.Len() > 0 && !(encoded && prevEncoded) {
			desc.WriteByte(' ')
		}
		desc.WriteString(text)
		prevEncoded = encoded
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case isWSP(c):
			i++
		case c == '(' && !commentBroken:
			if end, ok := scanComment(s, i); ok {
				i = end
				continue
			}
			commentBroken = true
			note("read the '(' of a comment that is never closed as text")
			i++
		case c == '"' && !quoteBroken:
			if end, ok := scanQuoted(s, i); ok {
				word(s[i:end], true)
				i = end
				continue
			}
			quoteBroken = true
			note("ignored a '\"' that is never closed")
			i++
		case c == '<':
			j := i + 1
			for j < len(s) && s[j] != '>' && s[j] != '<' {
				j++
			}
			var text string
			if j < len(s) && s[j] == '>' {
				text = s[i+1 : j]
				i = j + 1
			} else {
				k := i + 1
				for k < j && isWSP(s[k]) {
					k++
				}
				start := k
				for k < j && !isWSP(s[k]) {
					k++
				}
				text = s[start:k]
				i = k
				note("supplied the missing '>'")
			}
			if stripped := stripWSP(text); stripped != text {
				text = stripped
				note("removed the whitespace inside the angle brackets")
			}
			if hasControl(text) {
				text = stripControl(text)
				note("removed control characters from the identifier")
			}
			if text == "" {
				note("ignored empty angle brackets")
				continue
			}
			ids++
			id = text
		case c == '>':
			note("ignored an unmatched '>'")
			i++
		default:
			j := i
			for j < len(s) && !isWSP(s[j]) && s[j] != '<' && s[j] != '>' && (s[j] != '(' || commentBroken) && (s[j] != '"' || quoteBroken) {
				j++
			}
			if j == i {
				// A '(' or '"' whose search already failed.
				j = i + 1
			} else {
				word(s[i:j], false)
			}
			i = j
		}
	}

	if badCharset {
		d.add(message.CodeRFC2047UnknownCharset, message.SeverityUnsupported,
			"mail: List-Id description holds an encoded-word in a charset that cannot be decoded",
			"kept the encoded-word as written")
	}
	description := desc.String()
	if hasControl(description) {
		description = stripControl(description)
		note("removed control characters from the description")
	}
	switch {
	case ids > 0:
		res.ID, res.Description = id, description
		if ids > 1 {
			note("kept the last of " + strconv.Itoa(ids) + " identifiers")
		}
		if trailing {
			note("ignored the text after the identifier")
		}
	case words == 1 && !isEncodedWord(lastWord) && lastWord[0] != '"' && !hasControl(lastWord):
		res.ID = lastWord
		note("supplied the missing angle brackets")
	default:
		res.Description = description
		d.invalid(message.CodeListHeaderInvalid,
			"mail: List-Id "+quoteForMessage(s)+" holds no list identifier",
			"kept the raw value and its text as the description")
		res.Diagnostics = d.list
		return res
	}
	if len(notes) > 0 {
		d.recovered(message.CodeListHeaderInvalid,
			"mail: List-Id "+quoteForMessage(s)+" is not written as RFC 2919 requires",
			strings.Join(notes, "; "))
	}
	res.Diagnostics = d.list
	return res
}
