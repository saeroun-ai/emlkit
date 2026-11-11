package message

import (
	"mime"
	"strings"
	"unicode/utf8"
)

// The tolerant reading of a Content-Type or Content-Disposition value
// (ROB-050-002 to ROB-050-008, issues #22, #23 and #24).
//
// mime.ParseMediaType refuses a whole field over one defect. What is here
// reads the same field item by item and keeps everything that can be read:
// a base that is missing its type, its subtype or its slash, values that are
// not tokens, quoted strings that are never closed, parameters that repeat,
// RFC 2231 series with gaps, percent escapes that are not escapes, charsets
// with no decoder, and raw 8-bit bytes. It never fails, it changes nothing in
// the header, and every repair it makes is one finding that the caller turns
// into a diagnostic.
//
// # A conformant item reads as it always did
//
// A plain name=value item is first handed to mime.ParseMediaType on its own,
// and only an item that parser refuses is read by the looser rules below. An
// item the standard library accepts therefore has exactly the value it has
// always had, quoted-string escapes included. RFC 2231 items are read by
// rfc2231.go, which gives conformant utf-8 and us-ascii values the same result
// as the standard library and decodes the other charsets it leaves out.
//
// # The rules, in the order they apply to one item
//
//   - No "=", no name, or no value: the item is malformed and declares
//     nothing. A name is a token; a name with an RFC 2231 suffix that is not
//     RFC 2231 syntax is malformed too.
//   - An unquoted value that is not a token runs to the end of the item, that
//     is to the next top-level semicolon, with surrounding whitespace removed
//     (mime.parameter_unquoted_value): name=my file.pdf, boundary=--=_a/b?c=d,
//     filename==?UTF-8?B?…?=. A token followed by one stray quote, as in
//     charset=utf-8", is the token. So is a token followed by a comment, as in
//     charset=us-ascii (Plain text), except in name and filename, where a
//     parenthesis is taken for part of the name; and a charset, which is
//     always one token, ends at its first whitespace.
//   - A quoted string with no closing quote runs to the end of the field
//     (mime.parameter_unclosed_quote). The item scan reads it the same way, so
//     whatever followed is inside the value; a boundary recovered this way is
//     used only if it swallowed no semicolon.
//   - Text after the closing quote is kept as part of the value: up to the
//     last quote when the item ends in one, which is what an unescaped quote
//     inside the string looks like (mime.parameter_invalid_escape), and
//     otherwise appended as it is (mime.parameter_unquoted_value). Stray
//     quotes and nothing else after the closing one are dropped.
//   - A backslash before a character that needs no escaping is kept, as the
//     standard library keeps it: that is how filename="C:\Users\a\b.txt"
//     keeps its path (mime.parameter_invalid_escape, informational). A
//     backslash at the very end of a value is kept as well. In a string that
//     nothing closes, a backslash and quote that end it or stand before a
//     semicolon are read as a backslash and the closing quote, which is what
//     filename="C:\dir\" meant.
//
// # Which value a repeated parameter has
//
// Every occurrence is a candidate and none is discarded; see
// ParameterCandidate. One of them is chosen:
//
//   - boundary frames the body, so it is never chosen among differing
//     candidates: a boundary that repeats with different values, or beside an
//     occurrence nothing could read, has no value at all
//     (mime.parameter_duplicate), and a multipart left without one is
//     unframed (metadata_repair.go).
//   - Any other parameter takes its RFC 2231 form when it has one, as RFC 2231
//     intends for a name=fallback; name*=… pair, and otherwise its plain form.
//     Within a form the first occurrence wins, and a later one that differs
//     is reported (mime.parameter_duplicate). A candidate no charset decoded
//     is passed over while another can be read.
//   - Repeats with one value are harmless and silent, as they always were.
//
// # Bytes that are not ASCII
//
// A value holding raw UTF-8 is kept as it is (mime.parameter_raw_utf8,
// informational). One holding raw bytes that are not UTF-8 is decoded with the
// charset the entity's Content-Type declares, when there is one and it has a
// decoder; failing that with the caller's fallback charset
// (ParseOptions.FallbackCharset), when there is one and the bytes are valid in
// it, and the candidate is marked Fallback (mime.parameter_fallback_charset);
// otherwise the bytes are kept as the value and the candidate is marked
// Undecoded (mime.parameter_unknown_charset). The boundary is exempt: its
// bytes frame the body and are never rewritten.
//
// RFC 2047 encoded words in a value are decoded, as this package has always
// decoded them, and the fact is reported (mime.parameter_rfc2047,
// informational). A word whose charset has no decoder stays in the value as it
// was written, and the candidate is marked Undecoded
// (mime.parameter_unknown_charset): the caller is not handed "=?euc-kr?B?…?="
// as if it were a name.

// A ParameterForm says how a parameter value was written in its field.
type ParameterForm uint8

const (
	// ParameterPlain is name=value, the form of RFC 2045.
	ParameterPlain ParameterForm = iota

	// ParameterExtended is name*=charset'language'value, the extended form of
	// RFC 2231 section 4.
	ParameterExtended

	// ParameterContinued is a value cut into the numbered segments name*0,
	// name*1, … of RFC 2231 section 3, each of which may be extended.
	ParameterContinued
)

// String returns "plain", "extended" or "continued".
func (f ParameterForm) String() string {
	switch f {
	case ParameterExtended:
		return "extended"
	case ParameterContinued:
		return "continued"
	}
	return "plain"
}

// A ParameterCandidate is one value a Content-Type or Content-Disposition
// field offers for a parameter. A parameter written once has one candidate; a
// parameter that repeats, or that is given both as name and as name*, has one
// per occurrence, and a continued value has one for the whole series. None is
// discarded: the one the parse uses is marked Chosen.
type ParameterCandidate struct {
	// Name is the parameter name in lower case, without any RFC 2231 suffix.
	Name string

	// Value is the candidate's value as the tolerant reading recovered it:
	// quoting removed, RFC 2231 and RFC 2047 encodings decoded. It is UTF-8
	// unless Undecoded is set.
	Value string

	// Raw is the occurrence exactly as the field holds it, name included. For a
	// continued value it is the segments in the order they appear, joined by
	// "; ".
	Raw string

	// Form is how the value was written.
	Form ParameterForm

	// Charset and Language are what an RFC 2231 value declares, as written.
	// Charset is also set, in lower case, when raw 8-bit bytes were decoded
	// with the charset of the entity's Content-Type, or with the fallback
	// charset (see Fallback).
	Charset  string
	Language string

	// Undecoded reports that Value holds bytes no charset decoded: there was no
	// decoder for the charset declared, none was declared for raw 8-bit bytes,
	// or the bytes are not valid in it. Value is then the octets themselves and
	// may not be UTF-8, so that a caller who knows the charset can still decode
	// them. It is also set when Value holds an RFC 2047 encoded word whose
	// charset has no decoder: the word is then in Value as it was written.
	Undecoded bool

	// Fallback reports that Value was decoded with the fallback charset — the
	// parse's ParseOptions.FallbackCharset, or the HeaderTextOptions given to
	// Header.FilenameCandidatesWithOptions — because the value holds raw 8-bit
	// bytes that are not UTF-8 and nothing in the entity's header names their
	// charset. Charset is then that charset. It is a guess the caller asked
	// for, made only when the bytes decode in that charset without error.
	Fallback bool

	// Chosen reports that this candidate is the parameter's value in the
	// parameter map. A parameter has at most one chosen candidate, and has none
	// when its candidates are ambiguous in a way that is never resolved by
	// choosing, as for a boundary.
	Chosen bool
}

// A finding is everything one tolerant reading has to say under one diagnostic
// code: which parameters it concerns, how severe it is at most, and whether a
// strict profile has to refuse the field over it.
type finding struct {
	names    []string
	more     bool
	severity Severity
	refuse   bool
}

// maxDiagnosticNames caps the parameter names one diagnostic lists, so that a
// field of many defective parameters still records short diagnostics.
const maxDiagnosticNames = 8

type findings map[Code]*finding

// note records that code applies to the parameter name. The names are kept in
// first-occurrence order, once each, up to maxDiagnosticNames.
func (f findings) note(code Code, name string, severity Severity, refuse bool) {
	x := f[code]
	if x == nil {
		x = &finding{severity: severity}
		f[code] = x
	}
	if severityRank(severity) > severityRank(x.severity) {
		x.severity = severity
	}
	x.refuse = x.refuse || refuse
	for _, n := range x.names {
		if n == name {
			return
		}
	}
	if len(x.names) < maxDiagnosticNames {
		x.names = append(x.names, name)
	} else {
		x.more = true
	}
}

// severityRank orders the severities a finding can have: informational,
// unsupported, repaired.
func severityRank(s Severity) int {
	switch s {
	case SeverityInfo:
		return 0
	case SeverityUnsupported:
		return 1
	}
	return 2
}

// A tolerantInput is one field to read tolerantly.
type tolerantInput struct {
	field string
	raw   string

	// scan is what scanParameters found in raw: the items are the occurrences
	// it retained, so a parameter budget bounds this reading as it bounds every
	// other.
	scan parameterScan

	// dec decodes encoded words and charsets through the configured
	// CharsetReader, under the no-progress guard. It is never nil.
	dec *metadataDecoder

	// poll is the parse's cancellation poll, or nil.
	poll func() bool

	// hint returns the charset to try for raw 8-bit bytes that are not UTF-8,
	// or is nil for a Content-Type, which uses its own charset parameter. It is
	// called at most once, and only when such bytes were found. noHint turns
	// the attempt off: reading bytes in a charset they do not declare is a
	// guess, and a strict profile makes none.
	hint   func() string
	noHint bool

	// fallback is the charset to try for such bytes when the hint names none,
	// or none that decodes them: the caller's fallback charset, normalized, or
	// "" for none. It is a guess too, and noHint turns it off as well.
	fallback string

	// noWords leaves RFC 2047 encoded words undecoded. It is set for a value
	// that belongs to no parse and is longer than any header field a parse
	// admits by default: the encoded-word decoder is not linear in its input,
	// and only a parse's header budgets bound what it is given.
	noWords bool
}

// A tolerantReading is what a tolerant reading of one field found.
type tolerantReading struct {
	// base is the media type or disposition. baseCode is the code of its defect
	// when it had to be repaired, baseDetail what was wrong with it, and
	// defaulted whether base is a default rather than what the field names.
	base       string
	baseCode   Code
	baseDetail string
	defaulted  bool

	params     map[string]string
	candidates []ParameterCandidate

	// The parameter names, each list in first-occurrence order: those whose
	// chosen value was read without repair, those whose chosen value needed
	// one, those left without a value because their candidates conflict, and
	// the continued ones a parameter budget dropped.
	wellFormed  []string
	repaired    []string
	conflicting []string
	dropped     []string

	// malformed counts the items that declare nothing, unclosed whether one of
	// them is an unterminated quoted string, and empty the stray empty items
	// the scan crossed.
	malformed int
	unclosed  bool
	empty     int

	truncated bool
	notes     findings
	cancelled bool
}

// needsRepair reports whether the field is not what the whole-field parser
// would have accepted as it stands: whether reading it took a repair, which is
// what a strict profile refuses.
func (t *tolerantReading) needsRepair() bool {
	if t.baseCode != "" || t.malformed > 0 || t.empty > 0 {
		return true
	}
	for _, f := range t.notes {
		if f.refuse {
			return true
		}
	}
	return false
}

// A parameterState is what a reading knows about one parameter name.
type parameterState struct {
	name string

	// all holds the indices of the name's candidates, in field order. series
	// is the index of its continued candidate, or -1, and segments that
	// candidate's segments in the order they arrived.
	all      []int
	series   int
	segments []parameterSegment

	// count is how many items name the parameter, and damaged whether one of
	// them has no value that could be read.
	count   int
	damaged bool
}

// A candidateWork is what a reading keeps about a candidate while it is being
// built and does not publish.
type candidateWork struct {
	// text is the value before encoded words are decoded.
	text string

	// repaired reports that reading the candidate took a repair. raw8 reports
	// raw bytes above 0x7f in a part of it that no charset is declared for.
	repaired bool
	raw8     bool

	// unusable marks a value that cannot be trusted to frame a body: one
	// recovered from an unterminated quote that swallowed a semicolon.
	unusable bool
}

// readTolerant reads one field. See the top of this file for the rules.
func readTolerant(in tolerantInput) tolerantReading {
	r := tolerantReading{
		truncated: in.scan.refusal != nil,
		empty:     in.scan.strayEmpty,
		notes:     findings{},
		params:    map[string]string{},
	}
	var extra string
	r.base, r.baseCode, r.baseDetail, r.defaulted, extra = healBase(in.field, in.raw[:in.scan.head])

	var (
		states = map[string]*parameterState{}
		order  []*parameterState
		work   []candidateWork
	)
	state := func(name string) *parameterState {
		st := states[name]
		if st == nil {
			st = &parameterState{name: name, series: -1}
			states[name] = st
			order = append(order, st)
		}
		st.count++
		return st
	}
	add := func(item string) {
		it := readParameterItem(item)
		if it.undefined {
			return
		}
		if !it.ok {
			r.malformed++
			if hasUnclosedQuote(item) {
				r.unclosed = true
			}
			if it.name != "" {
				state(it.name).damaged = true
			}
			return
		}
		st := state(it.name)
		if it.loose {
			r.notes.note(CodeMIMEParameterUnquotedValue, it.name, SeverityRecovered, true)
		}
		if it.unclosed {
			r.notes.note(CodeMIMEParameterUnclosedQuote, it.name, SeverityRecovered, true)
		}
		switch {
		case it.badEscape:
			r.notes.note(CodeMIMEParameterInvalidEscape, it.name, SeverityRecovered, true)
		case it.literalBackslash:
			r.notes.note(CodeMIMEParameterInvalidEscape, it.name, SeverityInfo, false)
		}
		repaired := it.loose || it.unclosed || it.badEscape
		raw8 := !it.encoded && has8bit(it.text)

		if it.form == ParameterContinued {
			if st.series < 0 {
				st.series = len(r.candidates)
				st.all = append(st.all, st.series)
				r.candidates = append(r.candidates, ParameterCandidate{Name: it.name, Form: ParameterContinued})
				work = append(work, candidateWork{})
			}
			st.segments = append(st.segments, parameterSegment{
				section: it.section, encoded: it.encoded, text: it.text, raw: it.raw,
			})
			w := &work[st.series]
			w.repaired = w.repaired || repaired
			w.raw8 = w.raw8 || raw8
			return
		}
		st.all = append(st.all, len(r.candidates))
		r.candidates = append(r.candidates, ParameterCandidate{Name: it.name, Form: it.form, Raw: it.raw})
		work = append(work, candidateWork{
			text:     it.text,
			repaired: repaired,
			raw8:     raw8,
			unusable: it.unclosed && strings.IndexByte(it.text, ';') >= 0,
		})
	}

	cancelled := func() bool {
		if in.poll != nil && in.poll() {
			r.cancelled = true
		}
		return r.cancelled
	}

	// Text after the base that reads as a parameter is one: a field that left
	// out the semicolon, "text/html charset=utf-8". It is part of the base as
	// the scan saw it, so no budget is charged for it; there is at most one.
	if strings.IndexByte(extra, '=') >= 0 {
		add(extra)
	}
	for _, occ := range in.scan.occurrences {
		if cancelled() {
			return r
		}
		add(in.raw[occ.start:occ.end])
	}

	// RFC 2231 values are assembled and charset-decoded, which can call the
	// configured CharsetReader, so a parse that can be cancelled is polled
	// before each.
	for i := range r.candidates {
		c, w := &r.candidates[i], &work[i]
		if c.Form == ParameterPlain {
			continue
		}
		var pieces []valuePiece
		if c.Form == ParameterExtended {
			pieces = []valuePiece{{text: w.text, encoded: true}}
		} else {
			st := states[c.Name]
			raws := make([]string, len(st.segments))
			for j, s := range st.segments {
				raws[j] = s.raw
			}
			c.Raw = strings.Join(raws, "; ")
			if r.truncated {
				// A series a budget cut is dropped whole below, so nothing is
				// assembled or claimed about it.
				continue
			}
			segs, missing, duplicate := orderSegments(st.segments)
			if missing {
				r.notes.note(CodeMIMEParameterSegmentMissing, c.Name, SeverityRecovered, true)
			}
			if duplicate {
				r.notes.note(CodeMIMEParameterSegmentDuplicate, c.Name, SeverityRecovered, true)
			}
			w.repaired = w.repaired || missing || duplicate
			pieces = make([]valuePiece, len(segs))
			for j, s := range segs {
				pieces[j] = valuePiece{text: s.text, encoded: s.encoded}
			}
		}
		if cancelled() {
			return r
		}
		v := assemblePieces(pieces, in.dec)
		w.text = v.value
		c.Charset, c.Language, c.Undecoded = v.charset, v.language, v.undecoded
		if v.badPercent {
			r.notes.note(CodeMIMEParameterBadPercentEscape, c.Name, SeverityRecovered, true)
		}
		if v.noPrefix {
			// An extended value has to say what charset its bytes are in.
			r.notes.note(CodeMIMEParameterUnknownCharset, c.Name, SeverityRecovered, true)
		}
		if v.unknownCharset {
			r.notes.note(CodeMIMEParameterUnknownCharset, c.Name, SeverityUnsupported, false)
		}
		w.repaired = w.repaired || v.badPercent || v.noPrefix
	}

	// The charset to try for raw bytes that are not UTF-8 is asked for only if
	// there are any.
	hint := ""
	for i := range r.candidates {
		if w := &work[i]; !in.noHint && w.raw8 && r.candidates[i].Name != "boundary" && !utf8.ValidString(w.text) {
			switch {
			case in.hint != nil:
				hint = in.hint()
			case states["charset"] != nil && len(states["charset"].all) > 0:
				hint = work[states["charset"].all[0]].text
			}
			hint = strings.ToLower(strings.TrimSpace(hint))
			if hint == "utf-8" || hint == "us-ascii" {
				hint = ""
			}
			break
		}
	}

	for i := range r.candidates {
		c, w := &r.candidates[i], &work[i]
		if cancelled() {
			return r
		}
		text := w.text
		if w.raw8 && c.Name != "boundary" {
			switch {
			case utf8.ValidString(text):
				r.notes.note(CodeMIMEParameterRawUTF8, c.Name, SeverityInfo, false)
			default:
				decoded := false
				if hint != "" && c.Name != "charset" {
					if s, ok := in.dec.convertCharset(hint, []byte(text)); ok && utf8.ValidString(s) {
						text, c.Charset, decoded = s, hint, true
						r.notes.note(CodeMIMEParameterUnknownCharset, c.Name, SeverityRecovered, false)
					}
				}
				if !decoded && in.fallback != "" && in.fallback != hint && !in.noHint && c.Name != "charset" {
					// The caller's fallback charset, as for raw 8-bit header
					// text — but only when the bytes are valid in it: a name
					// with U+FFFD in it is worth less than its bytes.
					if s, ok := in.dec.convertCharset(in.fallback, []byte(text)); ok && utf8.ValidString(s) && !strings.ContainsRune(s, utf8.RuneError) {
						text, c.Charset, c.Fallback, decoded = s, in.fallback, true, true
						r.notes.note(CodeMIMEParameterFallbackCharset, c.Name, SeverityRecovered, false)
					}
				}
				if !decoded {
					c.Undecoded = true
					r.notes.note(CodeMIMEParameterUnknownCharset, c.Name, SeverityUnsupported, false)
				}
			}
		}
		c.Value = text
		if !in.noWords {
			var unknown bool
			c.Value, unknown = in.dec.cdDecodeWords(text)
			if unknown && c.Name != "boundary" {
				// An encoded word in a charset with no decoder is still in the
				// value as it was written: that is not the name.
				c.Undecoded = true
				r.notes.note(CodeMIMEParameterUnknownCharset, c.Name, SeverityUnsupported, false)
			}
		}
		if c.Value != text {
			r.notes.note(CodeMIMEParameterRFC2047, c.Name, SeverityInfo, false)
		}
	}
	if cancelled() {
		return r
	}

	for _, st := range order {
		if len(st.all) == 0 {
			// Only items that could not be read name it.
			continue
		}
		if r.truncated && st.series >= 0 {
			// The series may go on past the budget, and a value that only looks
			// complete is not handed out; a whole parameter of the same name
			// cannot be told from a conflict with it. See limit_parameters.go.
			r.dropped = append(r.dropped, st.name)
			continue
		}
		pick := chooseCandidate(st, r.candidates, work, r.notes)
		if pick < 0 {
			r.conflicting = append(r.conflicting, st.name)
			continue
		}
		r.candidates[pick].Chosen = true
		r.params[st.name] = r.candidates[pick].Value
		if work[pick].repaired {
			r.repaired = append(r.repaired, st.name)
		} else {
			r.wellFormed = append(r.wellFormed, st.name)
		}
	}
	return r
}

// chooseCandidate returns the index of the candidate that is the value of the
// parameter st describes, or -1 when none may be chosen. See "Which value a
// repeated parameter has" at the top of this file.
func chooseCandidate(st *parameterState, cands []ParameterCandidate, work []candidateWork, notes findings) int {
	if st.name == "boundary" {
		first := st.all[0]
		ambiguous := st.damaged
		for _, i := range st.all {
			if work[i].unusable || cands[i].Value != cands[first].Value {
				ambiguous = true
			}
		}
		if !ambiguous {
			return first
		}
		if st.count > 1 {
			notes.note(CodeMIMEParameterDuplicate, st.name, SeverityRecovered, true)
		}
		return -1
	}

	firstExtended, firstPlain, conflict := -1, -1, false
	for _, i := range st.all {
		first := &firstPlain
		if cands[i].Form != ParameterPlain {
			first = &firstExtended
		}
		switch {
		case *first < 0:
			*first = i
		case cands[i].Value != cands[*first].Value:
			conflict = true
		}
	}
	if conflict {
		notes.note(CodeMIMEParameterDuplicate, st.name, SeverityRecovered, true)
	}
	for _, extended := range [2]bool{true, false} {
		for _, i := range st.all {
			if (cands[i].Form != ParameterPlain) == extended && !cands[i].Undecoded {
				return i
			}
		}
	}
	if firstExtended >= 0 {
		return firstExtended
	}
	return firstPlain
}

// A parameterItem is one semicolon-separated item of a parameterized field,
// read on its own.
type parameterItem struct {
	// raw is the item with surrounding whitespace removed.
	raw string

	// name is the parameter name, lower cased and without its RFC 2231 suffix.
	// It is empty when the item names nothing readable. form, section and
	// encoded describe that suffix.
	name    string
	form    ParameterForm
	section int
	encoded bool

	// text is the value with its quoting removed and nothing decoded, and ok
	// whether the item has one.
	text string
	ok   bool

	// undefined marks a well-formed item whose name has a star suffix RFC 2231
	// does not define, such as name*x. The whole-field parser accepts it and
	// makes no parameter of it, and neither does this reading.
	undefined bool

	// How the value had to be read: as a loose unquoted value, as a quoted
	// string that is never closed, with a quote or backslash that is not an
	// escape repaired, or with a backslash before an ordinary character kept.
	loose            bool
	unclosed         bool
	badEscape        bool
	literalBackslash bool
}

// readParameterItem reads one raw parameter item. An item that has a name, an
// equals sign and a value is ok, whatever it took to read the value.
func readParameterItem(item string) parameterItem {
	it := parameterItem{raw: strings.TrimSpace(item)}
	s := it.raw
	eq := strings.IndexByte(s, '=')
	if eq < 0 {
		// A name alone still says which parameter the damage belongs to.
		if isToken(s) {
			it.name, _, _, _, _ = splitParameterName(s)
		}
		return it
	}
	attr, val := strings.TrimSpace(s[:eq]), strings.TrimSpace(s[eq+1:])
	if !isToken(attr) {
		return it
	}
	name, form, section, encoded, ok := splitParameterName(attr)
	if !ok {
		if val != "" && name != "" {
			if _, _, err := mime.ParseMediaType("a/b;" + s); err == nil {
				it.undefined = true
			}
		}
		return it
	}
	it.name, it.form, it.section, it.encoded = name, form, section, encoded
	if val == "" {
		return it
	}

	if form == ParameterPlain {
		// The whole-field parser's own reading of the item, so that an item it
		// accepts never reads differently here.
		if _, params, err := mime.ParseMediaType("a/b;" + s); err == nil {
			for _, v := range params {
				it.text, it.ok = v, true
			}
			if it.ok {
				it.literalBackslash = val[0] == '"' && quotedLiteralBackslash(val)
				return it
			}
		}
	}

	var f valueFlags
	it.text, f = readParameterValue(val, name == "name" || name == "filename")
	if f.loose && name == "charset" {
		// A charset is one token, so whatever follows it in a loose value is
		// not part of it: charset=utf-8 format=flowed.
		if token, _ := leadingToken(it.text); token != "" {
			it.text = token
		}
	}
	it.ok = true
	it.loose, it.unclosed, it.badEscape, it.literalBackslash = f.loose, f.unclosed, f.badEscape, f.literalBackslash
	return it
}

// isComment reports whether s is an RFC 822 comment, as far as one item shows:
// text in parentheses.
func isComment(s string) bool {
	return len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')'
}

type valueFlags struct {
	loose, unclosed, badEscape, literalBackslash bool
}

// readParameterValue reads a non-empty parameter value that has no
// surrounding whitespace, by the rules at the top of this file. filename says
// that the value names a file, in which a parenthesis is far more likely part
// of the name — "report (1).pdf" — than the start of a comment.
func readParameterValue(val string, filename bool) (string, valueFlags) {
	var f valueFlags
	if val[0] != '"' {
		if isToken(val) {
			return val, f
		}
		f.loose = true
		if i := strings.IndexByte(val, '('); !filename && i > 0 && isComment(val[i:]) {
			// A comment after the value, which RFC 2045 allows and the
			// whole-field parser does not read: charset=us-ascii (Plain text).
			if token := strings.TrimRight(val[:i], " \t"); isToken(token) {
				return token, f
			}
		}
		// A quote opened in the middle of the value and never closed ran to
		// the end of the field, as the item scan reads it.
		f.unclosed = hasUnclosedQuote(val)
		if q := strings.IndexByte(val, '"'); f.unclosed && isToken(val[:q]) {
			if after := strings.TrimSpace(val[q+1:]); after == "" || after[0] == ';' {
				// A token with one stray quote after it: charset=utf-8". The
				// token is the value. Whatever the quote swallowed after a
				// semicolon is not read, and is reported as the unclosed quote
				// it is.
				f.unclosed = after != ""
				return val[:q], f
			}
		}
		return val, f
	}

	end := closingQuote(val)
	if end < 0 {
		f.unclosed = true
		body := strings.TrimRight(val[1:], " \t\r\n")
		if cut := escapedClosingQuote(body); cut >= 0 {
			// A Windows path that ends in a backslash, "C:\dir\": the backslash
			// was read as an escape of the closing quote. Read as a backslash
			// and the closing quote it stands before, the string is closed;
			// what follows it is not read.
			f.badEscape = true
			f.unclosed = strings.TrimSpace(body[cut+2:]) != ""
			return unquote(body[:cut], &f) + `\`, f
		}
		return unquote(body, &f), f
	}
	rest := strings.TrimRight(val[end+1:], " \t\r\n")
	if strings.TrimSpace(rest) == "" {
		return unquote(val[1:end], &f), f
	}
	if strings.Trim(rest, `"`) == "" {
		// Nothing but stray quotes after the closing one: charset="utf-8"".
		f.badEscape = true
		return unquote(val[1:end], &f), f
	}
	if !filename && isComment(strings.TrimSpace(rest)) {
		// A comment after the quoted string: charset="us-ascii" (Plain text).
		f.loose = true
		return unquote(val[1:end], &f), f
	}
	if rest[len(rest)-1] == '"' {
		// A quote inside the string that should have been escaped: the string
		// runs to the last quote of the item.
		f.badEscape = true
		return unquote(val[1:end+len(rest)], &f), f
	}
	f.loose = true
	return unquote(val[1:end], &f) + rest, f
}

// closingQuote returns the index of the quote that closes the quoted string
// val opens with, or -1 when nothing closes it. A backslash escapes the byte
// after it, as the parameter scan reads it.
func closingQuote(val string) int {
	for i := 1; i < len(val); i++ {
		switch val[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// escapedClosingQuote looks, in the content of a quoted string that nothing
// closes, for a backslash and quote that would close it if the backslash were
// a backslash: an escaped quote that ends the content or stands before a
// semicolon. It returns the index of that backslash, or -1.
func escapedClosingQuote(body string) int {
	for i := 0; i+1 < len(body); i++ {
		if body[i] != '\\' {
			continue
		}
		if body[i+1] == '"' {
			if after := strings.TrimSpace(body[i+2:]); after == "" || after[0] == ';' {
				return i
			}
		}
		i++
	}
	return -1
}

// unquote returns the content of a quoted string with its escapes resolved as
// mime.ParseMediaType resolves them: a backslash before a character that has
// to be escaped stands for that character, and one before any other character
// is kept, so that an unescaped Windows path survives. A quote in s is literal,
// a line break is dropped, and a backslash that ends s is kept.
func unquote(s string, f *valueFlags) string {
	if strings.IndexByte(s, '\\') < 0 && strings.IndexByte(s, '\r') < 0 && strings.IndexByte(s, '\n') < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 >= len(s):
			f.badEscape = true
			b.WriteByte(c)
		case c == '\\' && isTSpecial(s[i+1]):
			i++
			b.WriteByte(s[i])
		case c == '\\':
			f.literalBackslash = true
			b.WriteByte(c)
		case c == '\r' || c == '\n':
			f.badEscape = true
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// quotedLiteralBackslash reports whether the quoted string val opens with
// holds a backslash before a character that needs no escaping, which
// mime.ParseMediaType keeps as a backslash.
func quotedLiteralBackslash(val string) bool {
	for i := 1; i < len(val); i++ {
		switch val[i] {
		case '"':
			return false
		case '\\':
			if i+1 < len(val) && !isTSpecial(val[i+1]) {
				return true
			}
			i++
		}
	}
	return false
}

// isTSpecial reports whether c is one of the tspecials of RFC 2045, which a
// token may not hold.
func isTSpecial(c byte) bool {
	return strings.IndexByte(`()<>@,;:\"/[]?=`, c) >= 0
}

// isToken reports whether s is an RFC 2045 token.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f || isTSpecial(c) {
			return false
		}
	}
	return true
}

// leadingToken splits s into the token it starts with and what follows.
func leadingToken(s string) (token, rest string) {
	i := 0
	for i < len(s) && s[i] > ' ' && s[i] < 0x7f && !isTSpecial(s[i]) {
		i++
	}
	return s[:i], s[i:]
}

func has8bit(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return true
		}
	}
	return false
}

// healBase reads the base of a field — its media type or its disposition —
// from head, the field up to its first top-level semicolon.
//
// A base that is valid on its own is returned as the whole-field parser
// normalizes it, with no code. Otherwise code says what was wrong, detail says
// it in words, defaulted reports that base is a default rather than something
// the field names, and extra is text after the base that may be a parameter the
// field forgot to separate.
func healBase(field, head string) (base string, code Code, detail string, defaulted bool, extra string) {
	if b, ok := validBase(field, head); ok {
		return b, "", "", false, ""
	}
	s := strings.TrimSpace(head)
	if n := len(s); n > 2 && s[0] == '"' && s[n-1] == '"' {
		// A base that is valid but for the quotes around it: "text/plain".
		if b, ok := validBase(field, s[1:n-1]); ok {
			return b, fieldInvalidCode(field), "has its " + baseNoun(field) + " " + quote(b) + " in quotes", false, ""
		}
	}
	if field == "Content-Disposition" {
		return healDisposition(s)
	}
	return healMediaType(s)
}

// healMediaType recovers a media type (ROB-050-002). RFC 2045 section 5.2
// makes text/plain the type of an entity that declares none, and RFC 2046 makes
// an unrecognized subtype of text text/plain, one of multipart multipart/mixed,
// and anything else application/octet-stream; a type or subtype that is missing
// is read the way an unrecognized one is:
//
//	""  ";charset=x"  "charset=x"   text/plain: nothing names a type
//	"text"  "text/"                 text/plain
//	"multipart"  "multipart/"       multipart/mixed
//	"image"  "image/"  "foo"        application/octet-stream
//	"text plain"                    text/plain: whitespace for the slash
//	"/html"  "/mixed"  "/pdf"       text/html, multipart/mixed, application/pdf
//	"text/html charset=x"           text/html, and the rest read as a parameter
//	"@#!"                           application/octet-stream: nothing readable
func healMediaType(s string) (base string, code Code, detail string, defaulted bool, extra string) {
	typ, rest := leadingToken(s)
	after := strings.TrimSpace(rest)
	typ = strings.ToLower(typ)
	switch {
	case s == "":
		return "text/plain", CodeMIMEContentTypeMissingType, "names no media type", true, ""
	case typ == "" && strings.HasPrefix(after, "/"):
		sub, tail := leadingToken(strings.TrimSpace(after[1:]))
		if sub == "" {
			break
		}
		sub = strings.ToLower(sub)
		return typeOfSubtype(sub) + "/" + sub, CodeMIMEContentTypeMissingType,
			"names the subtype " + quote(sub) + " but no type", true, strings.TrimSpace(tail)
	case typ == "":
		// Nothing readable where the type should be.
	case strings.HasPrefix(after, "="):
		// The field starts with a parameter: nothing names a type.
		return "text/plain", CodeMIMEContentTypeMissingType, "names no media type", true, s
	case strings.HasPrefix(after, "/"):
		sub, tail := leadingToken(strings.TrimSpace(after[1:]))
		tail = strings.TrimSpace(tail)
		if sub == "" {
			return defaultSubtype(typ), CodeMIMEContentTypeMissingSubtype,
				"names the type " + quote(typ) + " but no subtype", true, tail
		}
		base = typ + "/" + strings.ToLower(sub)
		return base, CodeMIMEContentTypeInvalid,
			"has a media type " + quote(base) + " that is not written as one token pair or is followed by unexpected text", false, tail
	default:
		if sub, tail := leadingToken(after); sub != "" && rest != after && !strings.HasPrefix(strings.TrimSpace(tail), "=") {
			base = typ + "/" + strings.ToLower(sub)
			return base, CodeMIMEContentTypeMissingSlash,
				"separates the type and subtype of " + quote(base) + " by whitespace instead of a slash", false, strings.TrimSpace(tail)
		}
		return defaultSubtype(typ), CodeMIMEContentTypeMissingSlash,
			"names " + quote(typ) + " with no slash and no subtype", true, after
	}
	return "application/octet-stream", CodeMIMEContentTypeInvalid, "has no readable media type", true, ""
}

// defaultSubtype returns the media type a bare top-level type is read as.
func defaultSubtype(typ string) string {
	switch typ {
	case "text":
		return "text/plain"
	case "multipart":
		return "multipart/mixed"
	}
	return "application/octet-stream"
}

// typeOfSubtype returns the top-level type a bare subtype is read under: the
// one it belongs to for the few subtypes that leave no doubt, and application
// otherwise, which is how an unrecognized type is treated.
func typeOfSubtype(sub string) string {
	switch sub {
	case "plain", "html", "csv", "calendar", "enriched", "richtext":
		return "text"
	case "mixed", "alternative", "related", "digest", "parallel", "report", "signed", "encrypted":
		return "multipart"
	case "jpeg", "jpg", "png", "gif", "bmp", "tiff", "webp":
		return "image"
	}
	return "application"
}

// healDisposition recovers a disposition. RFC 2183 section 2.8 has an
// unrecognized disposition treated as attachment, and a field with parameters
// and no readable type is read the same way; a type followed by unexpected
// text keeps the type, and the text is read as a parameter when it is one:
// "attachment filename=a.pdf".
func healDisposition(s string) (base string, code Code, detail string, defaulted bool, extra string) {
	code = CodeMIMEDispositionInvalid
	typ, rest := leadingToken(s)
	after := strings.TrimSpace(rest)
	switch {
	case typ == "" || strings.HasPrefix(after, "="):
		if typ == "" {
			s = ""
		}
		return "attachment", code, "names no readable disposition type", true, s
	}
	base = strings.ToLower(typ)
	return base, code, "has unexpected text after the disposition " + quote(base), false, after
}

func quote(s string) string {
	return `"` + s + `"`
}
