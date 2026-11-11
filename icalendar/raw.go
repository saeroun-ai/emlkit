package icalendar

import (
	"bytes"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// A Param is one property parameter.
type Param struct {
	// Name is upper-cased.
	Name string
	// Values are the parameter's values with quotes removed and RFC 6868
	// caret escapes applied; nil for a parameter written without "=".
	Values []string
}

// A Property is one content line.
type Property struct {
	// Name is upper-cased.
	Name   string
	Params []Param
	// Value is the value as written, unfolded but not unescaped.
	Value string
	// Raw is the content line as read, unfolded. Lines the parser joined to it
	// (CodeUnfoldedText) follow it, each after a "\n".
	Raw string
	// Line is the physical line, from 1, where the content line begins.
	Line int
	// Truncated reports that the line was longer than Limits.MaxLineBytes and
	// was cut.
	Truncated bool
}

// Param returns the first value of the named parameter, or "".
func (p *Property) Param(name string) string {
	for i := range p.Params {
		if strings.EqualFold(p.Params[i].Name, name) {
			if len(p.Params[i].Values) > 0 {
				return p.Params[i].Values[0]
			}
			return ""
		}
	}
	return ""
}

// ParamValues returns all values of the named parameter, of each occurrence.
func (p *Property) ParamValues(name string) []string {
	var vs []string
	for i := range p.Params {
		if strings.EqualFold(p.Params[i].Name, name) {
			vs = append(vs, p.Params[i].Values...)
		}
	}
	return vs
}

// A Component is a BEGIN/END block with its properties, in order, and the
// components inside it.
type Component struct {
	// Name is upper-cased: VCALENDAR, VEVENT, VALARM, ...
	Name       string
	Properties []*Property
	Children   []*Component
	// Line is the physical line of the BEGIN; 0 for a component the parser
	// supplied (Synthesized).
	Line int
	// Synthesized reports that the input had no BEGIN for this component: a
	// VCALENDAR made to hold components found outside one.
	Synthesized bool
	// Closed reports that the component's own END closed it.
	Closed bool

	overflow bool
}

// Prop returns the first property of the name, or nil.
func (c *Component) Prop(name string) *Property {
	for _, p := range c.Properties {
		if strings.EqualFold(p.Name, name) {
			return p
		}
	}
	return nil
}

// Props returns every property of the name, in order.
func (c *Component) Props(name string) []*Property {
	var ps []*Property
	for _, p := range c.Properties {
		if strings.EqualFold(p.Name, name) {
			ps = append(ps, p)
		}
	}
	return ps
}

// A Document is the raw result of a parse: the top-level components, normally
// a single VCALENDAR, and the repairs made to read them.
type Document struct {
	Components []*Component
	Issues     []Issue
}

// ParseRaw reads the content lines and the component tree of data, without
// interpreting any value. It always returns a Document; what could not be
// read is reported in its Issues.
func ParseRaw(data []byte, opts *Options) *Document {
	p := newParser(opts)
	p.run(data)
	return &Document{Components: p.roots, Issues: p.is.list}
}

const utf8BOM = "\xEF\xBB\xBF"

// calendarLevel are the components that belong directly in a VCALENDAR.
var calendarLevel = map[string]bool{
	"VEVENT": true, "VTODO": true, "VJOURNAL": true, "VFREEBUSY": true,
	"VTIMEZONE": true, "VAVAILABILITY": true,
}

type parser struct {
	lim Limits
	is  issues

	roots      []*Component
	stack      []*Component
	components int
	skip       int  // depth of a component being skipped by a budget
	seenRoot   bool // a top-level BEGIN was seen
	cut        bool // a budget ended the reading early

	// last is the property most recently read, to which a stray text line
	// may be joined; while pending, its joined value is built in lastVal and
	// lastRaw.
	last    *Property
	pending bool
	lastVal []byte
	lastRaw []byte

	buf []byte
}

func newParser(opts *Options) *parser {
	lim := opts.limits()
	return &parser{lim: lim, is: issues{max: lim.MaxIssues}}
}

func (p *parser) run(data []byte) {
	data = p.prepare(data)
	u := unfolder{data: data, next: 1, p: p}
	lines := 0
	for {
		text, no, trunc, ok := u.logical()
		if !ok {
			break
		}
		lines++
		if lines > p.lim.MaxLines {
			p.is.add(CodeLimitLines, no, "")
			p.cut = true
			break
		}
		p.line(text, no, trunc)
	}
	p.flush()
	for i := len(p.stack) - 1; i >= 0; i-- {
		// A cut input ends inside components, and a synthesized one has no
		// END to miss: neither is reported.
		if !p.cut && !p.stack[i].Synthesized {
			p.is.add(CodeMissingEnd, 0, p.stack[i].Name+" open at the end of the input")
		}
	}
	p.stack = nil
}

// prepare applies the input budget and removes a byte order mark.
func (p *parser) prepare(data []byte) []byte {
	if len(data) > p.lim.MaxInputBytes {
		data = data[:p.lim.MaxInputBytes]
		p.is.add(CodeLimitInput, 0, "")
		p.cut = true
	}
	switch {
	case bytes.HasPrefix(data, []byte(utf8BOM)):
		p.is.addOnce(CodeBOM, 1, "")
		data = data[len(utf8BOM):]
	case len(data) >= 2 && (data[0] == 0xFF && data[1] == 0xFE || data[0] == 0xFE && data[1] == 0xFF):
		p.is.add(CodeUTF16, 0, "")
		data = fromUTF16(data[2:], data[0] == 0xFE)
	}
	return data
}

func fromUTF16(b []byte, bigEndian bool) []byte {
	u := make([]uint16, len(b)/2)
	for i := range u {
		if bigEndian {
			u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		} else {
			u[i] = uint16(b[2*i+1])<<8 | uint16(b[2*i])
		}
	}
	out := make([]byte, 0, len(b))
	for _, r := range utf16.Decode(u) {
		out = utf8.AppendRune(out, r)
	}
	return out
}

// unfolder splits the input into physical lines and joins folded ones.
type unfolder struct {
	data []byte
	pos  int
	next int // number of the next physical line
	p    *parser
}

// physical returns the next physical line without its ending.
func (u *unfolder) physical() ([]byte, int, bool) {
	if u.pos >= len(u.data) {
		return nil, 0, false
	}
	no := u.next
	u.next++
	start := u.pos
	i := bytes.IndexAny(u.data[start:], "\r\n")
	if i < 0 {
		u.pos = len(u.data)
		return u.data[start:], no, true
	}
	end := start + i
	if u.data[end] == '\r' && end+1 < len(u.data) && u.data[end+1] == '\n' {
		u.pos = end + 2
	} else {
		u.pos = end + 1
		u.p.is.addOnce(CodeBareLineEnding, no, "")
	}
	return u.data[start:end], no, true
}

// logical returns the next unfolded content line, the number of its first
// physical line and whether it was cut at Limits.MaxLineBytes.
func (u *unfolder) logical() (string, int, bool, bool) {
	p := u.p
	for {
		b, no, ok := u.physical()
		if !ok {
			return "", 0, false, false
		}
		if len(b) == 0 {
			p.is.addOnce(CodeEmptyLine, no, "")
			continue
		}
		if b[0] == ' ' || b[0] == '\t' {
			// Only the first line can be a fold with nothing to continue.
			p.is.add(CodeLeadingFold, no, "")
			b = bytes.TrimLeft(b, " \t")
			if len(b) == 0 {
				continue
			}
		}
		limit := p.lim.MaxLineBytes
		buf, trunc := appendCapped(p.buf[:0], b, limit, false)
		for {
			pos, next := u.pos, u.next
			nb, nno, ok := u.physical()
			if !ok {
				break
			}
			if len(nb) == 0 {
				// An empty line inside a folded line: skip it and see whether
				// the fold goes on after it.
				p.is.addOnce(CodeEmptyLine, nno, "")
				continue
			}
			if nb[0] != ' ' && nb[0] != '\t' {
				u.pos, u.next = pos, next
				break
			}
			nb = nb[1:]
			if len(buf) > 0 && len(nb) > 0 && buf[len(buf)-1] >= 0x80 && nb[0]&0xC0 == 0x80 {
				p.is.addOnce(CodeFoldSplitsUTF8, nno, "")
			}
			buf, trunc = appendCapped(buf, nb, limit, trunc)
		}
		p.buf = buf
		if trunc {
			p.is.add(CodeLimitLine, no, "")
		}
		return string(buf), no, trunc, true
	}
}

func appendCapped(buf, b []byte, limit int, trunc bool) ([]byte, bool) {
	if room := limit - len(buf); len(b) > room {
		return append(buf, b[:max(room, 0)]...), true
	}
	return append(buf, b...), trunc
}

type lineKind uint8

const (
	lineOK lineKind = iota
	lineNoColon
	lineBadName
)

type contentLine struct {
	kind   lineKind
	name   string
	params []Param
	value  string
}

// line handles one unfolded content line.
func (p *parser) line(text string, no int, trunc bool) {
	if strings.HasPrefix(text, utf8BOM) {
		p.is.addOnce(CodeBOM, no, "")
		text = text[len(utf8BOM):]
	}
	if p.skip > 0 {
		if c := p.parse(text, no, true); c.kind == lineOK {
			switch c.name {
			case "BEGIN":
				p.skip++
			case "END":
				p.skip--
			}
		}
		return
	}
	c := p.parse(text, no, len(p.stack) == 0)
	if len(p.stack) == 0 {
		if c.kind == lineOK && c.name == "BEGIN" {
			if !strings.HasPrefix(text, "BEGIN") {
				p.is.addOnce(CodeNameCase, no, text[:5])
			}
			p.begin(c.value, no)
			return
		}
		if p.seenRoot {
			p.is.addOnce(CodeAfterEnd, no, text)
		} else {
			p.is.addOnce(CodeBeforeBegin, no, text)
		}
		return
	}
	if c.kind != lineOK || p.isURIText(c) {
		if p.join(text) {
			p.is.addOnce(CodeUnfoldedText, no, text)
			return
		}
		if c.kind == lineBadName {
			p.is.add(CodeBadName, no, text)
		} else {
			p.is.add(CodeNoColon, no, text)
		}
		return
	}
	p.flush()
	switch c.name {
	case "BEGIN":
		p.begin(c.value, no)
	case "END":
		p.end(c.value, no)
	default:
		top := p.stack[len(p.stack)-1]
		if len(top.Properties) >= p.lim.MaxProperties {
			if !top.overflow {
				top.overflow = true
				p.is.add(CodeLimitProperties, no, top.Name)
			}
			p.last = nil
			return
		}
		prop := &Property{Name: c.name, Params: c.params, Value: c.value, Raw: text, Line: no, Truncated: trunc}
		top.Properties = append(top.Properties, prop)
		p.last = prop
	}
}

// parse splits a content line into name, parameters and value. quiet keeps
// it from reporting, for lines that are skipped.
func (p *parser) parse(s string, no int, quiet bool) contentLine {
	report := func(code Code, detail string, once bool) {
		switch {
		case quiet:
		case once:
			p.is.addOnce(code, no, detail)
		default:
			p.is.add(code, no, detail)
		}
	}
	i := 0
	for i < len(s) && s[i] != ';' && s[i] != ':' {
		i++
	}
	if i == len(s) {
		return contentLine{kind: lineNoColon}
	}
	name := strings.TrimSpace(s[:i])
	if !isName(name) {
		return contentLine{kind: lineBadName}
	}
	if name != s[:i] {
		report(CodeNameSpace, s[:i], true)
	}
	up := strings.ToUpper(name)
	if up != name {
		report(CodeNameCase, name, true)
	}
	var params []Param
	over := false
	for i < len(s) && s[i] == ';' {
		i++
		j := i
		for j < len(s) && s[j] != '=' && s[j] != ';' && s[j] != ':' {
			j++
		}
		if j == len(s) {
			return contentLine{kind: lineNoColon}
		}
		pname := strings.ToUpper(strings.TrimSpace(s[i:j]))
		var vals []string
		if s[j] != '=' {
			report(CodeParamNoValue, s[i:j], false)
			i = j
		} else {
			i = j + 1
			for {
				v, ni, ok := p.paramValue(s, i, pname, report)
				if !ok {
					return contentLine{kind: lineNoColon}
				}
				i = ni
				if len(vals) < p.lim.MaxParams {
					vals = append(vals, v)
				} else if !over {
					over = true
					report(CodeLimitParams, pname, false)
				}
				if s[i] != ',' {
					break
				}
				i++
			}
			if pname == "" {
				report(CodeParamNoValue, "a parameter without a name", false)
				continue
			}
		}
		if pname == "" {
			continue
		}
		if len(params) >= p.lim.MaxParams {
			if !over {
				over = true
				report(CodeLimitParams, name, false)
			}
			continue
		}
		params = append(params, Param{Name: pname, Values: vals})
	}
	if i >= len(s) {
		return contentLine{kind: lineNoColon}
	}
	return contentLine{kind: lineOK, name: up, params: params, value: s[i+1:]}
}

// uriParams are the parameters whose value is a URI and must be quoted.
var uriParams = map[string]bool{
	"SENT-BY": true, "DELEGATED-TO": true, "DELEGATED-FROM": true,
	"MEMBER": true, "DIR": true, "ALTREP": true,
}

// paramValue reads one parameter value at s[i]. It returns the value, the
// index of the delimiter after it (",", ";" or ":"), and false when the line
// ends first.
func (p *parser) paramValue(s string, i int, pname string, report func(Code, string, bool)) (string, int, bool) {
	if i < len(s) && s[i] == '"' {
		k := strings.IndexByte(s[i+1:], '"')
		if k >= 0 {
			v := s[i+1 : i+1+k]
			i += k + 2
			if j := delim(s, i); j > i {
				report(CodeParamAfterQuote, s[i:j], false)
				v += s[i:j]
				i = j
			}
			return caret(v), i, i < len(s)
		}
		report(CodeParamQuote, pname, false)
		i++
	}
	j := delim(s, i)
	if j < len(s) && s[j] == ':' && uriParams[pname] && isScheme(s[i:j]) {
		report(CodeParamUnquotedURI, pname, true)
		j = delim(s, j+1)
	}
	return caret(s[i:j]), j, j < len(s)
}

// delim returns the index of the first ",", ";" or ":" at or after i.
func delim(s string, i int) int {
	for i < len(s) && s[i] != ',' && s[i] != ';' && s[i] != ':' {
		i++
	}
	return i
}

func isScheme(s string) bool {
	switch strings.ToLower(s) {
	case "mailto", "http", "https", "ldap", "cid":
		return true
	}
	return false
}

// caret applies the RFC 6868 escapes: ^n, ^^ and ^'.
func caret(v string) string {
	if strings.IndexByte(v, '^') < 0 {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); i++ {
		if v[i] == '^' && i+1 < len(v) {
			switch v[i+1] {
			case 'n', 'N':
				b.WriteByte('\n')
				i++
				continue
			case '^':
				b.WriteByte('^')
				i++
				continue
			case '\'':
				b.WriteByte('"')
				i++
				continue
			}
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

// isName reports whether s is an iana-token or x-name: letters, digits and
// hyphens.
func isName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// textual reports whether a stray line after a property of this name is
// likely to be the rest of its text, broken by a raw line break.
func textual(name string) bool {
	switch name {
	case "SUMMARY", "DESCRIPTION", "LOCATION", "COMMENT", "CONTACT", "RESOURCES", "CATEGORIES":
		return true
	}
	return strings.HasPrefix(name, "X-")
}

// isURIText reports whether a well-formed line is really a URI on a line of
// its own, inside text broken by a raw line break: "https://..." reads as a
// property named HTTPS.
func (p *parser) isURIText(c contentLine) bool {
	if c.kind != lineOK || len(c.params) > 0 || p.last == nil || !textual(p.last.Name) {
		return false
	}
	switch c.name {
	case "HTTP", "HTTPS", "FTP":
		return strings.HasPrefix(c.value, "//")
	case "MAILTO", "TEL", "SIP":
		return true
	}
	return false
}

// join appends a stray line to the text property before it, as a new line
// of its text.
func (p *parser) join(text string) bool {
	if p.last == nil || !textual(p.last.Name) {
		return false
	}
	if !p.pending {
		p.pending = true
		p.lastVal = append(p.lastVal[:0], p.last.Value...)
		p.lastRaw = append(p.lastRaw[:0], p.last.Raw...)
	}
	room := p.lim.MaxLineBytes - len(p.lastVal) - 2
	if room < len(text) {
		if !p.last.Truncated {
			p.last.Truncated = true
			p.is.add(CodeLimitLine, p.last.Line, p.last.Name)
		}
		text = text[:max(room, 0)]
	}
	if text != "" || room >= 0 {
		p.lastVal = append(append(p.lastVal, `\n`...), text...)
		p.lastRaw = append(append(p.lastRaw, '\n'), text...)
	}
	return true
}

// flush ends the joining of lines to the last property.
func (p *parser) flush() {
	if p.pending {
		p.last.Value = string(p.lastVal)
		p.last.Raw = string(p.lastRaw)
		p.pending = false
	}
}

// begin opens a component.
func (p *parser) begin(v string, no int) {
	name := p.compName(v, no)
	if name == "" {
		p.is.add(CodeBadBegin, no, "BEGIN without a name")
		return
	}
	p.last = nil
	if len(p.stack) == 0 {
		if name != "VCALENDAR" && calendarLevel[name] {
			p.is.add(CodeMissingCalendar, no, name)
			if !p.push(&Component{Name: "VCALENDAR", Synthesized: true}, no) {
				return
			}
		}
	} else if name == "VCALENDAR" {
		p.popTo(0, no)
	} else if calendarLevel[name] && p.stack[len(p.stack)-1].Name != "VCALENDAR" {
		for i := len(p.stack) - 1; i >= 0; i-- {
			if p.stack[i].Name == "VCALENDAR" {
				p.popTo(i+1, no)
				break
			}
		}
	}
	p.push(&Component{Name: name, Line: no}, no)
}

// push adds c under the innermost open component, within the budgets; when
// a budget refuses it, everything up to its END is skipped.
func (p *parser) push(c *Component, no int) bool {
	if p.components >= p.lim.MaxComponents {
		p.is.addOnce(CodeLimitComponents, no, c.Name)
		p.skip = 1
		return false
	}
	if len(p.stack) >= p.lim.MaxDepth {
		p.is.add(CodeLimitDepth, no, c.Name)
		p.skip = 1
		return false
	}
	p.components++
	if len(p.stack) == 0 {
		p.roots = append(p.roots, c)
		p.seenRoot = true
	} else {
		parent := p.stack[len(p.stack)-1]
		parent.Children = append(parent.Children, c)
	}
	p.stack = append(p.stack, c)
	return true
}

// end closes the named component and any left open inside it.
func (p *parser) end(v string, no int) {
	p.last = nil
	name := p.compName(v, no)
	if name == "" {
		p.is.add(CodeBadBegin, no, "END without a name")
		return
	}
	for i := len(p.stack) - 1; i >= 0; i-- {
		if p.stack[i].Name == name {
			p.popTo(i+1, no)
			p.stack[i].Closed = true
			p.stack = p.stack[:i]
			return
		}
	}
	p.is.add(CodeStrayEnd, no, name)
}

// popTo closes, without their END, the components above depth n.
func (p *parser) popTo(n, no int) {
	for len(p.stack) > n {
		if c := p.stack[len(p.stack)-1]; !c.Synthesized {
			p.is.add(CodeMissingEnd, no, c.Name)
		}
		p.stack = p.stack[:len(p.stack)-1]
	}
}

func (p *parser) compName(v string, no int) string {
	name := strings.TrimSpace(v)
	up := strings.ToUpper(name)
	if up != name {
		p.is.addOnce(CodeNameCase, no, name)
	}
	return up
}
