package mail

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/saeroun-ai/emlkit"
)

// The reading of a report body into groups of fields, shared by the three
// report kinds of report.go. A report body is a header block, or several
// separated by blank lines (RFC 3464 section 2.1, RFC 8098 section 3.1, RFC
// 5965 section 3.1). What bounces really contain is read with these
// tolerances, each reported:
//
//   - any of CRLF, LF and a lone CR ends a line, mixed freely;
//   - an indented line continues the field before it; an unindented line of
//     free text continues it too when that field holds free text;
//   - white space between a field name and its colon is skipped;
//   - a line that is neither a field nor a continuation is skipped and kept;
//   - a blank line made of white space is a blank line.
//
// Everything is bounded: the bytes looked at, the fields read, the lines
// kept unparsed and the diagnostics returned.
const (
	// rptMaxBytes is how many bytes of a report body are read. A report
	// holds a few hundred bytes per recipient.
	rptMaxBytes = 256 << 10
	// rptMaxFields bounds the fields read from one report.
	rptMaxFields = 4096
	// rptMaxRecipients bounds the recipient groups of one delivery status.
	rptMaxRecipients = 1024
	// rptMaxUnparsed bounds the skipped lines kept in Unparsed, and
	// rptMaxUnparsedLen the bytes kept of each.
	rptMaxUnparsed    = 32
	rptMaxUnparsedLen = 998
	// rptMaxNameLen bounds a field name; a longer "name" is not one.
	rptMaxNameLen = 128
	// rptMaxDiagnostics bounds the diagnostics of one report.
	rptMaxDiagnostics = 128
	// rptMaxList bounds the entries of a list inside one value, such as the
	// modifiers of a Disposition.
	rptMaxList = 32
)

// rptField is one field as the scanner read it: the name as written, the
// value unfolded with the white space around it removed, and the line of the
// report it began on (0 for a field taken from elsewhere).
type rptField struct {
	name, value string
	line        int
}

// rptDiags collects the diagnostics of one report, up to rptMaxDiagnostics;
// the one that reaches the bound is replaced by limit.diagnostic_count.
type rptDiags struct {
	list []message.Diagnostic
	full bool
}

func (d *rptDiags) push(x message.Diagnostic) {
	if d.full {
		return
	}
	if len(d.list) >= rptMaxDiagnostics-1 {
		d.full = true
		d.list = append(d.list, message.Diagnostic{
			Code:     message.CodeLimitDiagnosticCount,
			Severity: message.SeverityLimitExceeded,
			Message:  "mail: report reached the limit of " + strconv.Itoa(rptMaxDiagnostics) + " diagnostics",
			Action:   "the diagnostic that reached this bound and every later one of this report are omitted; reading continued",
		})
		return
	}
	d.list = append(d.list, x)
}

func (d *rptDiags) add(code message.Code, severity message.Severity, field, msg, action string) {
	d.push(message.Diagnostic{Code: code, Severity: severity, Message: msg, Field: field, Action: action})
}

// merge adds the diagnostics of a parser of this package that read the value
// of field, naming the field in each.
func (d *rptDiags) merge(field string, list []message.Diagnostic) {
	for i := range list {
		x := list[i].Clone()
		x.Field = field
		d.push(x)
	}
}

// rptScan reads data as groups of fields separated by blank lines. text
// reports, for a lower-case field name, whether its value is free text that an
// unindented line may continue.
func rptScan(data []byte, text func(string) bool, d *rptDiags) (groups [][]rptField, unparsed []string) {
	if len(data) > rptMaxBytes {
		cut := bytes.LastIndexAny(data[:rptMaxBytes], "\r\n") + 1
		if cut == 0 {
			cut = rptMaxBytes
		}
		data = data[:cut]
		d.add(CodeReportLimitExceeded, message.SeverityLimitExceeded, "",
			fmt.Sprintf("mail: report holds more than %d bytes", rptMaxBytes), "read the fields of the first of them only")
	}
	if bytes.HasPrefix(data, []byte("\xef\xbb\xbf")) {
		data = data[3:]
	}
	var (
		group  []rptField
		name   string
		key    string
		val    []byte
		have   bool
		fline  int
		fields int
	)
	flush := func() {
		if have {
			group = append(group, rptField{name: name, value: string(bytes.Trim(val, " \t")), line: fline})
			have = false
		}
	}
	endGroup := func() {
		flush()
		if len(group) > 0 {
			groups = append(groups, group)
			group = nil
		}
	}
	garbage := func(l []byte, line int) {
		flush()
		action := "skipped the line; it is kept in Unparsed"
		if len(unparsed) < rptMaxUnparsed {
			unparsed = append(unparsed, string(l[:min(len(l), rptMaxUnparsedLen)]))
		} else {
			action = "skipped the line"
		}
		d.add(CodeReportGarbageLine, message.SeverityRecovered, "",
			fmt.Sprintf("mail: report line %d is neither a field nor the continuation of one", line), action)
	}
	for line := 1; len(data) > 0; line++ {
		var l []byte
		if i := bytes.IndexAny(data, "\r\n"); i < 0 {
			l, data = data, nil
		} else {
			l = data[:i]
			if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
				data = data[i+2:]
			} else {
				data = data[i+1:]
			}
		}
		if len(bytes.Trim(l, " \t")) == 0 {
			endGroup()
			continue
		}
		if l[0] == ' ' || l[0] == '\t' {
			if have {
				// Unfolding removes the line break and keeps the white space.
				val = append(val, l...)
			} else {
				garbage(l, line)
			}
			continue
		}
		if nm, v, ok, spaced := rptSplitField(l); ok {
			flush()
			if fields >= rptMaxFields {
				d.add(CodeReportLimitExceeded, message.SeverityLimitExceeded, "",
					fmt.Sprintf("mail: report holds more than %d fields", rptMaxFields), "read the first of them only")
				data = nil
				break
			}
			fields++
			name, key, val, have, fline = nm, strings.ToLower(nm), append(val[:0], v...), true, line
			if spaced {
				d.add(CodeReportInvalidField, message.SeverityRecovered, nm,
					fmt.Sprintf("mail: report line %d has white space between the field name and its colon", line), "read the name without it")
			}
			continue
		}
		if have && text(key) {
			val = append(val, ' ')
			val = append(val, l...)
			d.add(CodeReportUnindentedContinuation, message.SeverityRecovered, name,
				fmt.Sprintf("mail: report line %d continues the field before it without being indented", line), "read it as part of that field's value")
			continue
		}
		garbage(l, line)
	}
	endGroup()
	return groups, unparsed
}

// rptSplitField reads l as "name: value". spaced reports white space between
// the name and the colon. A name must be printable ASCII without white space
// (RFC 5322 ftext); a "name" followed by "//" is a URL, not a field.
func rptSplitField(l []byte) (name string, value []byte, ok, spaced bool) {
	colon := bytes.IndexByte(l, ':')
	if colon <= 0 || colon > rptMaxNameLen {
		return "", nil, false, false
	}
	n := bytes.TrimRight(l[:colon], " \t")
	spaced = len(n) < colon
	if len(n) == 0 {
		return "", nil, false, false
	}
	for _, c := range n {
		if c <= ' ' || c >= 0x7f {
			return "", nil, false, false
		}
	}
	value = l[colon+1:]
	if bytes.HasPrefix(value, []byte("//")) {
		return "", nil, false, false
	}
	return string(n), value, true, spaced
}

// rptClean returns the value of f to be given a typed meaning: as it is when
// it is ASCII; as it is, reported at info severity unless utf8OK, when it is
// valid UTF-8; and with each invalid byte replaced by U+FFFD, reported, when
// it is not. The raw value stays in the report's Fields either way.
func rptClean(d *rptDiags, f rptField, utf8OK bool) string {
	v := f.value
	ascii := true
	for i := 0; i < len(v); i++ {
		if v[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	switch {
	case ascii:
	case utf8.ValidString(v):
		if !utf8OK {
			d.add(CodeReportNonASCII, message.SeverityInfo, f.name,
				"mail: report field holds UTF-8 text its RFC does not allow", "read it as UTF-8")
		}
	default:
		v = strings.ToValidUTF8(v, string(utf8.RuneError))
		d.add(CodeReportNonASCII, message.SeverityRecovered, f.name,
			"mail: report field holds bytes that are not UTF-8", "replaced them by U+FFFD in the typed value; Fields keeps them")
	}
	return v
}

// rptStripComments removes the RFC 5322 comments of s, outside quoted
// strings, and returns what is left and the text of the comments, joined by a
// space. A comment that is never closed runs to the end of s.
func rptStripComments(s string) (text, comment string) {
	if strings.IndexByte(s, '(') < 0 {
		return s, ""
	}
	var t, c strings.Builder
	depth, quoted := 0, false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case depth > 0:
			switch {
			case ch == '\\' && i+1 < len(s):
				i++
				c.WriteByte(s[i])
			case ch == '(':
				depth++
				c.WriteByte(ch)
			case ch == ')':
				depth--
				if depth > 0 {
					c.WriteByte(ch)
				}
			default:
				c.WriteByte(ch)
			}
		case quoted:
			if ch == '\\' && i+1 < len(s) {
				t.WriteByte(ch)
				i++
				ch = s[i]
			} else if ch == '"' {
				quoted = false
			}
			t.WriteByte(ch)
		case ch == '"':
			quoted = true
			t.WriteByte(ch)
		case ch == '(':
			depth = 1
			if c.Len() > 0 {
				c.WriteByte(' ')
			}
			t.WriteByte(' ')
		default:
			t.WriteByte(ch)
		}
	}
	return t.String(), strings.TrimSpace(c.String())
}

// rptValueKind says what a "type; value" field holds.
type rptValueKind uint8

const (
	rptAddress rptValueKind = iota // address-type; generic-address
	rptMTA                         // mta-name-type; mta-name
	rptText                        // diagnostic-type; *text
)

// rptTyped reads a "type; value" field (RFC 3464 sections 2.2 and 2.3). A
// known type followed by a colon in place of the semicolon is read as the
// type. A field that gives no type keeps Type empty, and the type its value
// looks like goes to TypeGuess: an address with "@" (rfc822), a host name
// (dns), text that begins with an SMTP reply code (smtp).
func rptTyped(d *rptDiags, field, v string, kind rptValueKind) ReportValue {
	r := ReportValue{Raw: v}
	rest := v
	typ, after, ok := strings.Cut(v, ";")
	if ok {
		t, _ := rptStripComments(typ)
		t = strings.TrimSpace(t)
		if rptIsAtom(t) {
			r.Type, rest = strings.ToLower(t), after
		} else {
			ok = false
		}
	}
	if !ok {
		switch t, after, colon := rptColonType(v); {
		case colon:
			r.Type, rest = t, after
			d.add(CodeReportInvalidField, message.SeverityRecovered, field,
				"mail: report field separates its type with a colon instead of a semicolon", "read the word before the colon as the type")
		// The type is missing: that is what is reported. A guess goes to
		// TypeGuess only (issue #66).
		case kind == rptAddress && strings.Contains(v, "@"):
			r.TypeGuess = "rfc822"
			d.add(CodeReportInvalidField, message.SeverityInvalidMetadata, field,
				"mail: report field gives an address with no address type", "left Type empty; TypeGuess is rfc822, as the value holds an @")
		case kind == rptMTA && rptIsHost(v):
			r.TypeGuess = "dns"
			d.add(CodeReportInvalidField, message.SeverityInvalidMetadata, field,
				"mail: report field gives an MTA name with no name type", "left Type empty; TypeGuess is dns, as the value is a host name")
		case kind == rptText && rptHasReplyCode(strings.TrimSpace(v)):
			r.TypeGuess = "smtp"
			d.add(CodeReportInvalidField, message.SeverityInvalidMetadata, field,
				"mail: report field gives a diagnostic with no diagnostic type", "left Type empty; TypeGuess is smtp, as the value begins with a reply code")
		default:
			d.add(CodeReportInvalidField, message.SeverityInvalidMetadata, field,
				"mail: report field gives no type before its value", "left Type empty; kept the value with no type")
		}
	}
	if kind == rptText {
		r.Value = strings.TrimSpace(rest)
	} else {
		text, comment := rptStripComments(rest)
		text = strings.TrimSpace(text)
		if kind == rptAddress && len(text) >= 2 && text[0] == '<' && text[len(text)-1] == '>' {
			text = strings.TrimSpace(text[1 : len(text)-1])
			d.add(CodeReportInvalidField, message.SeverityInfo, field,
				"mail: report field puts angle brackets around the address", "removed them")
		}
		r.Value, r.Comment = text, comment
	}
	if r.Value == "" {
		d.add(CodeReportInvalidField, message.SeverityInvalidMetadata, field,
			"mail: report field gives no value", "kept it empty")
	}
	return r
}

// rptColonType reads "type: value" where a known type, or an "x-" one, is
// followed by a colon in place of the semicolon.
func rptColonType(v string) (typ, rest string, ok bool) {
	t, rest, found := strings.Cut(v, ":")
	if !found {
		return "", "", false
	}
	t = strings.ToLower(strings.TrimSpace(t))
	switch {
	case t == "rfc822", t == "utf-8", t == "x400", t == "unknown", t == "dns", t == "smtp":
	case strings.HasPrefix(t, "x-") && rptIsAtom(t):
	default:
		return "", "", false
	}
	return t, rest, true
}

// rptIsAtom reports whether s is a non-empty run of the letters, digits and
// punctuation a type name is made of.
func rptIsAtom(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isAlpha(c) && !isDigit(c) && c != '-' && c != '.' && c != '_' && c != '+' {
			return false
		}
	}
	return true
}

// rptIsHost reports whether v, comments removed, is one word with a dot or a
// bracketed address literal.
func rptIsHost(v string) bool {
	t, _ := rptStripComments(v)
	t = strings.TrimSpace(t)
	if t == "" || strings.ContainsAny(t, " \t@;") {
		return false
	}
	return strings.Contains(t, ".") || strings.HasPrefix(t, "[")
}

// rptHasReplyCode reports whether s begins with a three-digit SMTP reply code
// of class 2, 4 or 5.
func rptHasReplyCode(s string) bool {
	return len(s) >= 3 && (s[0] == '2' || s[0] == '4' || s[0] == '5') && isDigit(s[1]) && isDigit(s[2]) &&
		(len(s) == 3 || !isDigit(s[3]))
}

// rptTriple reads w as an RFC 3463 code: one digit, a dot, one to three
// digits, a dot, one to three digits.
func rptTriple(w string) (class, subject, detail int, ok bool) {
	parts := strings.Split(w, ".")
	if len(parts) != 3 || len(parts[0]) != 1 {
		return 0, 0, 0, false
	}
	var n [3]int
	for i, p := range parts {
		if p == "" || len(p) > 3 || !allDigits(p) {
			return 0, 0, 0, false
		}
		n[i], _ = strconv.Atoi(p)
	}
	return n[0], n[1], n[2], true
}

// rptFindStatus looks for an RFC 3463 code of class 2, 4 or 5 among the first
// 64 words of text, trimmed of the punctuation real bounces put around one,
// and returns the first. conflict reports that those words also hold a
// different such code.
func rptFindStatus(text string) (class, subject, detail int, ok, conflict bool) {
	words := 0
	for w := range strings.FieldsFuncSeq(text, func(r rune) bool {
		return r == ' ' || r == '\t' || r == ',' || r == ';' || r == '=' || r == '\r' || r == '\n'
	}) {
		if words++; words > 64 {
			break
		}
		w = strings.Trim(w, "#<>[]()\"'.:")
		// "550-5.1.1": the reply code and its continuation mark.
		if len(w) > 4 && rptHasReplyCode(w) && (w[3] == '-' || w[3] == ' ') {
			w = w[4:]
		}
		c, s, dt, found := rptTriple(w)
		if !found || (c != 2 && c != 4 && c != 5) {
			continue
		}
		if !ok {
			class, subject, detail, ok = c, s, dt, true
		} else if c != class || s != subject || dt != detail {
			return class, subject, detail, true, true
		}
	}
	return class, subject, detail, ok, false
}

// rptStatus reads a Status field (RFC 3464 section 2.3.4). Comments are
// allowed around the code; other text around one code is reported and the
// code read. A field that holds no code, or two different ones, is not Valid;
// when it is an SMTP reply code, its class goes to guess (issue #66).
func rptStatus(d *rptDiags, field, v string) (s, guess EnhancedStatus) {
	s = EnhancedStatus{Raw: v}
	text, comment := rptStripComments(v)
	s.Comment = comment
	text = strings.TrimSpace(text)
	if c, sub, det, ok := rptTriple(text); ok {
		s.Class, s.Subject, s.Detail = c, sub, det
		s.Valid = c == 2 || c == 4 || c == 5
		if !s.Valid {
			d.add(CodeReportInvalidStatus, message.SeverityInvalidMetadata, field,
				fmt.Sprintf("mail: report status %q has the class %d, not 2, 4 or 5", text, c), "kept the numbers; the status is not Valid")
		}
		return s, guess
	}
	if c, sub, det, ok, conflict := rptFindStatus(text); ok {
		if conflict {
			first := EnhancedStatus{Class: c, Subject: sub, Detail: det, Valid: true}
			d.add(CodeReportInvalidStatus, message.SeverityInvalidMetadata, field,
				"mail: report status holds more than one status code, the first "+first.String()+", and they differ",
				"left Status not Valid; Raw keeps the field")
			return s, guess
		}
		s.Class, s.Subject, s.Detail, s.Valid = c, sub, det, true
		d.add(CodeReportInvalidStatus, message.SeverityRecovered, field,
			"mail: report status holds more than a status code", "read the code "+s.String()+" out of it")
		return s, guess
	}
	if rptHasReplyCode(text) {
		guess = EnhancedStatus{Class: int(text[0] - '0'), Valid: true, Inferred: true}
		d.add(CodeReportInvalidStatus, message.SeverityInvalidMetadata, field,
			"mail: report status is an SMTP reply code, not an RFC 3463 code",
			"left Status not Valid; StatusGuess is "+guess.String()+", from the class of the reply code")
		return s, guess
	}
	d.add(CodeReportInvalidStatus, message.SeverityInvalidMetadata, field,
		"mail: report status is not a status code", "left Status not Valid; Raw keeps the field")
	return s, guess
}

// rptAction reads an Action field (RFC 3464 section 2.3.3).
func rptAction(d *rptDiags, field, v string) DeliveryAction {
	text, _ := rptStripComments(v)
	words := strings.Fields(text)
	if len(words) == 0 {
		d.add(CodeReportInvalidAction, message.SeverityInvalidMetadata, field, "mail: report action is empty", "kept it empty")
		return ""
	}
	w := strings.ToLower(strings.Trim(words[0], ".,;:"))
	var a DeliveryAction
	exact := true
	switch w {
	case "failed", "delayed", "delivered", "relayed", "expanded":
		a = DeliveryAction(w)
	case "fail", "failure", "bounce", "bounced":
		a, exact = ActionFailed, false
	case "delay", "deferred", "defer":
		a, exact = ActionDelayed, false
	case "deliver", "success", "succeeded", "sent":
		a, exact = ActionDelivered, false
	case "relay":
		a, exact = ActionRelayed, false
	case "expand", "expansion":
		a, exact = ActionExpanded, false
	default:
		d.add(CodeReportInvalidAction, message.SeverityInvalidMetadata, field,
			fmt.Sprintf("mail: report action %q is not one RFC 3464 defines", w), "kept the raw value only")
		return ""
	}
	switch {
	case !exact:
		d.add(CodeReportInvalidAction, message.SeverityRecovered, field,
			fmt.Sprintf("mail: report action %q is a variant of %q", w, a), "read it as "+string(a))
	case len(words) > 1:
		d.add(CodeReportInvalidAction, message.SeverityRecovered, field,
			"mail: report action is followed by other text", "read the first word as the action")
	}
	return a
}

// rptDate reads a date field with ParseDateTolerant and reports what it
// repaired under the field's name.
func rptDate(d *rptDiags, field, v string) DateResult {
	r := ParseDateTolerant(v)
	for i := range r.Diagnostics {
		r.Diagnostics[i].Field = field
	}
	d.merge(field, r.Diagnostics)
	return r
}

// rptUnbracket removes comments, white space and one pair of angle brackets
// around a value such as an SMTP path.
func rptUnbracket(v string) string {
	t, _ := rptStripComments(v)
	t = strings.TrimSpace(t)
	if len(t) >= 2 && t[0] == '<' && t[len(t)-1] == '>' {
		t = strings.TrimSpace(t[1 : len(t)-1])
	}
	return t
}

// rptReportField converts a scanned field to a ReportField.
func rptReportField(f rptField) ReportField {
	return ReportField{Name: f.name, Value: f.value}
}
