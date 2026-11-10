package mail

import (
	"strconv"
	"strings"
	"time"

	"github.com/saeroun-ai/emlkit"
)

// A ReceivedClause is one clause of a Received field: a keyword and the words
// up to the next keyword.
type ReceivedClause struct {
	// Name is the keyword in lower case: "from", "by", "via", "with", "id"
	// or "for". It is empty for text that stands in front of the first
	// keyword, which is kept as a clause of its own.
	Name string

	// Value is the words of the clause joined by single spaces, without the
	// keyword and without comments. It is otherwise as written: an address
	// keeps its angle brackets and a domain literal its square brackets.
	Value string

	// Comments are the texts of the comments in the clause, without their
	// outer parentheses. The comment after "from" usually holds the host name
	// and address the receiving server saw.
	Comments []string

	// Raw is the clause exactly as it appears in the field value, from its
	// keyword to its last word or comment.
	Raw string
}

// A Received is one Received field as the tolerant parser read it.
type Received struct {
	// Raw is the field value that was parsed.
	Raw string

	// Clauses are the clauses in the order they appear. Clauses with a
	// repeated keyword are all kept.
	Clauses []ReceivedClause

	// From, By, Via, With, ID and For are the values of the first clause of
	// that name, or empty.
	From, By, Via, With, ID, For string

	// DateRaw is the text after the last ';', as written.
	DateRaw string

	// Date is the time DateRaw gives, read as ParseDateTolerant reads it. It
	// is the zero time when HasDate is false.
	Date time.Time

	// HasDate reports whether a time was derived from DateRaw.
	HasDate bool

	// Diagnostics lists what was repaired or could not be read, including the
	// diagnostics of the date.
	Diagnostics []message.Diagnostic
}

// receivedKeywords are the clause keywords of RFC 5321 section 4.4 and RFC
// 5322 section 3.6.7.
var receivedKeywords = [...]string{"from", "by", "via", "with", "id", "for"}

// ParseReceived parses the value of a Received field without requiring it to
// follow the grammar, which few servers do.
//
// The text after the last ';' that stands outside comments and quoted strings
// is the date. The text before it is split into words and comments; each of
// the words from, by, via, with, id and for, in any case, starts a clause that
// runs to the next of them, and the text in front of the first one is kept as
// a clause with an empty name. Nothing is dropped: every clause keeps its raw
// text, the clauses keep their order, and repeated or unknown clauses are
// kept as they come.
//
// What is reported with received.invalid: a field without a ';' or without a
// date after it, a field without any clause keyword, text in front of the
// first keyword, and a comment that is never closed, whose '(' is read as
// part of a word. A date that cannot be read is reported with date.invalid,
// and repairs to it with the codes ParseDateTolerant uses.
//
// The work is linear in len(value): the value is scanned twice. At most
// 16384 clauses and 64 diagnostics are returned.
func ParseReceived(value string) Received {
	return parseReceived("", value)
}

// Received parses every Received field of the header, in the order the fields
// appear, which puts the most recent hop first. A field that does not follow
// the grammar is still returned with whatever could be read; see
// ParseReceived. It returns nil if the header has no Received field.
func (h *Header) Received() []Received {
	values := h.Values("Received")
	if len(values) == 0 {
		return nil
	}
	l := make([]Received, len(values))
	for i, v := range values {
		l[i] = parseReceived("Received", v)
	}
	return l
}

func parseReceived(field, v string) Received {
	r := Received{Raw: v}
	d := diagnosticList{field: field}

	// The date follows the last ';' outside comments and quoted strings.
	semi := -1
	commentBroken, quoteBroken := false, false
	for i := 0; i < len(v); {
		switch v[i] {
		case '(':
			if !commentBroken {
				if end, ok := scanComment(v, i); ok {
					i = end
					continue
				}
				commentBroken = true
			}
			i++
		case '"':
			if !quoteBroken {
				if end, ok := scanQuoted(v, i); ok {
					i = end
					continue
				}
				quoteBroken = true
			}
			i++
		case '\\':
			i += 2
		case ';':
			semi = i
			i++
		default:
			i++
		}
	}
	clauses := v
	if semi >= 0 {
		clauses = v[:semi]
		r.DateRaw = trimWSP(v[semi+1:])
	}

	named, leading := parseReceivedClauses(clauses, &r, &d)
	switch {
	case len(r.Clauses) == 0 && r.DateRaw == "":
		d.invalid(message.CodeReceivedInvalid,
			"mail: Received field holds neither a clause nor a date",
			"kept the raw value")
	case !named:
		d.recovered(message.CodeReceivedInvalid,
			"mail: Received field has no from, by, via, with, id or for clause",
			"kept its text as a clause without a name")
	case leading:
		d.recovered(message.CodeReceivedInvalid,
			"mail: Received field has text in front of its first clause",
			"kept that text as a clause without a name")
	}

	switch {
	case semi < 0:
		if len(r.Clauses) > 0 {
			d.recovered(message.CodeReceivedInvalid,
				"mail: Received field has no ';' and no date",
				"kept its clauses")
		}
	case r.DateRaw == "":
		if len(r.Clauses) > 0 {
			d.recovered(message.CodeReceivedInvalid,
				"mail: Received field has no date after ';'",
				"kept its clauses")
		}
	default:
		r.Date, r.HasDate = parseDateTolerant(r.DateRaw, &d)
	}
	r.Diagnostics = d.list
	return r
}

// parseReceivedClauses splits the text in front of the date into clauses.
// named reports that a keyword was found, leading that text stands in front
// of the first one.
func parseReceivedClauses(s string, r *Received, d *diagnosticList) (named, leading bool) {
	var (
		open                       bool // a clause is being collected
		name                       string
		words                      []string
		comments                   []string
		rawStart, rawEnd           int
		commentBroken, quoteBroken bool
		limited                    bool
	)
	flush := func() {
		if !open {
			return
		}
		open = false
		c := ReceivedClause{Name: name, Comments: comments, Raw: s[rawStart:rawEnd]}
		switch len(words) {
		case 0:
		case 1:
			c.Value = words[0]
		default:
			c.Value = strings.Join(words, " ")
		}
		r.Clauses = append(r.Clauses, c)
		if c.Value != "" {
			for _, dst := range [...]struct {
				name string
				v    *string
			}{{"from", &r.From}, {"by", &r.By}, {"via", &r.Via}, {"with", &r.With}, {"id", &r.ID}, {"for", &r.For}} {
				if dst.name == name && *dst.v == "" {
					*dst.v = c.Value
				}
			}
		}
		words, comments = nil, nil
	}
	begin := func(clauseName string, at int) bool {
		flush()
		if len(r.Clauses) >= maxTolerantEntries {
			limited = true
			d.add(message.CodeReceivedInvalid, message.SeverityLimitExceeded,
				"mail: Received field holds more than "+strconv.Itoa(maxTolerantEntries)+" clauses",
				"kept the first "+strconv.Itoa(maxTolerantEntries)+"; the rest of the clauses were not parsed")
			return false
		}
		open, name, rawStart = true, clauseName, at
		return true
	}

	for i := 0; i < len(s) && !limited; {
		c := s[i]
		if isWSP(c) {
			i++
			continue
		}
		if c == '(' && !commentBroken {
			if end, ok := scanComment(s, i); ok {
				if !open && !begin("", i) {
					break
				}
				comments = append(comments, commentText(s[i:end]))
				rawEnd = end
				i = end
				continue
			}
			commentBroken = true
			d.recovered(message.CodeReceivedInvalid,
				"mail: comment in Received field is never closed",
				"read its '(' as part of a word")
		}
		// A word runs to whitespace or to the next comment; a quoted string
		// inside it is taken whole.
		j := i
		for j < len(s) {
			c := s[j]
			if isWSP(c) || (c == '(' && j > i && !commentBroken) {
				break
			}
			if c == '\\' {
				j += 2
				continue
			}
			if c == '"' && !quoteBroken {
				if end, ok := scanQuoted(s, j); ok {
					j = end
					continue
				}
				quoteBroken = true
			}
			j++
		}
		j = min(j, len(s))
		word := s[i:j]

		keyword := ""
		if len(word) <= 4 {
			for _, k := range receivedKeywords {
				if strings.EqualFold(word, k) {
					keyword = k
					break
				}
			}
		}
		switch {
		case keyword != "":
			named = true
			if !begin(keyword, i) {
				break
			}
		case !open:
			if !begin("", i) {
				break
			}
			leading = true
			words = append(words, word)
		default:
			words = append(words, word)
		}
		rawEnd = j
		i = j
	}
	flush()
	return named, leading
}
