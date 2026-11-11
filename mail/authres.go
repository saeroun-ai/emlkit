package mail

import (
	"strings"

	"github.com/saeroun-ai/emlkit"
)

// Authentication-Results and its trust boundary (issue #45).
//
// An Authentication-Results field says what some server concluded about the
// message: SPF, DKIM, DMARC, ARC. Anyone can write one. A message arrives
// carrying whatever such fields its sender, or anything on the way, chose to
// add, and only those the receiver's own border server added say anything
// the receiver can rely on (RFC 8601 sections 1.6 and 7.1).
//
// So the parser here reads every such field and trusts none by default.
// AuthResultsOptions is where the caller says what its own servers are called
// and how many Received fields they add; a field is Trusted only when both
// fit. Everything else is still returned, untrusted, because a forged claim
// is itself worth knowing about.
//
// The same holds for the rest of a header: From, Sender, Reply-To and
// Message-ID are the sender's claims; Return-Path and the Received fields
// above the border are the receiving side's; the Received fields below it
// are claims of whoever handed the message over. Nothing in this package
// upgrades a claim to a fact.

// An AuthProp is one "ptype.property=value" of a method result.
type AuthProp struct {
	// Type is the property type — "smtp", "header", "body", "policy" — and
	// Name the property, both lower-cased.
	Type, Name string
	// Value is the value as written, quotes removed.
	Value string
}

// An AuthMethod is one method result of an Authentication-Results field.
type AuthMethod struct {
	// Method is the method, lower-cased: "spf", "dkim", "dmarc", "arc",
	// "iprev", "auth", and so on. Version is its version, usually empty.
	Method, Version string
	// Result is the result, lower-cased: "pass", "fail", "none", "neutral",
	// "softfail", "policy", "temperror", "permerror".
	Result string
	// Reason is the reason given, and Comment the first comment after the
	// result, each empty when absent.
	Reason, Comment string
	// Props are the properties in order.
	Props []AuthProp
}

// Prop returns the value of the first property ptype.name, or "".
func (m AuthMethod) Prop(ptype, name string) string {
	for _, p := range m.Props {
		if p.Type == ptype && p.Name == name {
			return p.Value
		}
	}
	return ""
}

// An AuthResult is one Authentication-Results field.
type AuthResult struct {
	// Raw is the field value as it was read.
	Raw string
	// AuthServID is the identifier of the server that claims to have written
	// the field, and Version the version after it, usually empty.
	AuthServID, Version string
	// None reports the "none" form: the server performed no authentication.
	None bool
	// Methods are the method results in order.
	Methods []AuthMethod

	// Index is the field's position among all fields of the header, the
	// topmost being 0, and ReceivedAbove how many Received fields stand above
	// it. Header.AuthenticationResults sets them; ParseAuthenticationResults
	// leaves them zero.
	Index         int
	ReceivedAbove int

	// Trusted reports that the field fits the caller's AuthResultsOptions:
	// its AuthServID is one of TrustedIDs and no more than MaxReceivedAbove
	// Received fields stand above it. It is false whenever the caller named
	// no trusted identifier.
	Trusted bool

	// Diagnostics say what of the field could not be read as written.
	Diagnostics []message.Diagnostic
}

// Method returns the first result of the named method, lower-case, and
// whether there is one.
func (r AuthResult) Method(name string) (AuthMethod, bool) {
	for _, m := range r.Methods {
		if m.Method == name {
			return m, true
		}
	}
	return AuthMethod{}, false
}

// AuthResultsOptions says which Authentication-Results fields are the
// receiver's own.
type AuthResultsOptions struct {
	// TrustedIDs are the authserv-ids of the receiver's own servers, compared
	// without regard to case. With none, no field is Trusted.
	TrustedIDs []string

	// MaxReceivedAbove is how many Received fields may stand above a trusted
	// field: the hops the receiver's own servers add after the one that
	// authenticated the message, and that server's own Received field when it
	// writes it above its results. A field with more Received fields above it
	// arrived with the message and is not Trusted, whatever identifier it
	// claims. Zero means none may; a negative value does not look at the
	// position at all, which is only safe when the border server is known to
	// remove foreign fields that claim its identifier.
	MaxReceivedAbove int
}

const maxAuthMethods = 64

// ParseAuthenticationResults reads one Authentication-Results field value
// (RFC 8601 section 2.2). It reads what it can: a result it cannot read is
// reported in Diagnostics and skipped, and the others are returned.
func ParseAuthenticationResults(value string) AuthResult {
	r := AuthResult{Raw: value}
	d := &diagnosticList{field: "Authentication-Results"}
	segs := splitAuthSegments(value)
	if len(segs) == 0 || strings.TrimSpace(segs[0].text) == "" {
		d.invalid(message.CodeAuthResultsInvalid, "mail: Authentication-Results names no authserv-id", "kept the raw value only")
		r.Diagnostics = d.list
		return r
	}
	head := strings.Fields(segs[0].text)
	r.AuthServID = strings.Trim(head[0], `"`)
	if len(head) > 1 {
		r.Version = head[1]
	}
	if r.AuthServID == "" {
		d.invalid(message.CodeAuthResultsInvalid, "mail: Authentication-Results names an empty authserv-id", "read the method results; no server is named")
	}
	if len(head) > 2 || strings.ContainsAny(r.AuthServID, "=") {
		d.recovered(message.CodeAuthResultsInvalid, "mail: Authentication-Results begins with more than an authserv-id and a version", "read the first word as the authserv-id")
	}
	for _, seg := range segs[1:] {
		text := strings.TrimSpace(seg.text)
		if text == "" {
			continue
		}
		if strings.EqualFold(text, "none") {
			r.None = true
			continue
		}
		if len(r.Methods) >= maxAuthMethods {
			d.invalid(message.CodeAuthResultsInvalid, "mail: Authentication-Results holds more method results than are read", "kept the first of them")
			break
		}
		m, ok := parseAuthMethod(text, seg.comment)
		if !ok {
			d.recovered(message.CodeAuthResultsInvalid, "mail: Authentication-Results holds a result that is not method=result", "skipped it and read the others")
			continue
		}
		r.Methods = append(r.Methods, m)
	}
	r.Diagnostics = d.list
	return r
}

// AuthenticationResults returns every Authentication-Results field of the
// header, topmost first, each marked Trusted or not by opts (nil trusts
// none). See authres.go for why position matters.
func (h *Header) AuthenticationResults(opts *AuthResultsOptions) []AuthResult {
	var o AuthResultsOptions
	if opts != nil {
		o = *opts
	}
	var out []AuthResult
	received := 0
	fields := h.Fields()
	for i := 0; fields.Next(); i++ {
		switch strings.ToLower(fields.Key()) {
		case "received":
			received++
		case "authentication-results":
			r := ParseAuthenticationResults(fields.Value())
			r.Index, r.ReceivedAbove = i, received
			if r.AuthServID != "" && (o.MaxReceivedAbove < 0 || received <= o.MaxReceivedAbove) {
				for _, id := range o.TrustedIDs {
					if strings.EqualFold(strings.TrimSpace(id), r.AuthServID) {
						r.Trusted = true
						break
					}
				}
			}
			out = append(out, r)
		}
	}
	return out
}

type authSegment struct {
	text    string // the segment with comments removed
	comment string // its first comment, without the parentheses
}

// splitAuthSegments splits v at the semicolons outside quoted strings and
// comments, removing the comments. An unclosed quote or comment runs to the
// end of the value.
func splitAuthSegments(v string) []authSegment {
	var segs []authSegment
	var text, comment strings.Builder
	hasComment := false
	depth, quoted := 0, false
	flush := func() {
		segs = append(segs, authSegment{text: text.String(), comment: comment.String()})
		text.Reset()
		comment.Reset()
		hasComment = false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case depth > 0:
			switch {
			case c == '\\' && i+1 < len(v):
				i++
				if !hasComment {
					comment.WriteByte(v[i])
				}
			case c == '(':
				if depth < 64 {
					depth++
				}
				if !hasComment {
					comment.WriteByte(c)
				}
			case c == ')':
				depth--
				if depth == 0 {
					hasComment = true
					text.WriteByte(' ')
				} else if !hasComment {
					comment.WriteByte(c)
				}
			default:
				if !hasComment {
					comment.WriteByte(c)
				}
			}
		case quoted:
			switch {
			case c == '\\' && i+1 < len(v):
				i++
				text.WriteByte(v[i])
			case c == '"':
				quoted = false
				text.WriteByte(c)
			default:
				text.WriteByte(c)
			}
		case c == '"':
			quoted = true
			text.WriteByte(c)
		case c == '(':
			depth = 1
		case c == ';':
			flush()
		case c == '\r' || c == '\n':
			text.WriteByte(' ')
		default:
			text.WriteByte(c)
		}
	}
	flush()
	return segs
}

// parseAuthMethod reads "method[/version]=result [reason=value]
// *(ptype.property=value)", white space allowed around each "=".
func parseAuthMethod(text, comment string) (AuthMethod, bool) {
	pairs := authPairs(text)
	if len(pairs) == 0 || pairs[0][0] == "" || pairs[0][1] == "" {
		return AuthMethod{}, false
	}
	m := AuthMethod{Comment: strings.TrimSpace(comment)}
	m.Method = strings.ToLower(pairs[0][0])
	if i := strings.IndexByte(m.Method, '/'); i >= 0 {
		m.Method, m.Version = strings.TrimSpace(m.Method[:i]), strings.TrimSpace(m.Method[i+1:])
	}
	m.Result = strings.ToLower(pairs[0][1])
	if m.Method == "" {
		return AuthMethod{}, false
	}
	for _, kv := range pairs[1:] {
		key := strings.ToLower(kv[0])
		switch i := strings.IndexByte(key, '.'); {
		case key == "reason":
			m.Reason = kv[1]
		case i > 0:
			m.Props = append(m.Props, AuthProp{Type: key[:i], Name: key[i+1:], Value: kv[1]})
		case key != "":
			m.Props = append(m.Props, AuthProp{Name: key, Value: kv[1]})
		}
	}
	return m, true
}

// authPairs reads the "key=value" pairs of text in order. A value is a quoted
// string or runs to the next white space; a word with no "=" is skipped.
func authPairs(text string) [][2]string {
	var pairs [][2]string
	i := 0
	skipWS := func() {
		for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
			i++
		}
	}
	for i < len(text) && len(pairs) < 256 {
		skipWS()
		start := i
		for i < len(text) && text[i] != '=' && text[i] != ' ' && text[i] != '\t' {
			i++
		}
		key := text[start:i]
		skipWS()
		if i >= len(text) || text[i] != '=' {
			// A word with no value.
			continue
		}
		i++
		skipWS()
		var value string
		if i < len(text) && text[i] == '"' {
			i++
			vs := i
			for i < len(text) && text[i] != '"' {
				i++
			}
			value = text[vs:i]
			if i < len(text) {
				i++
			}
		} else {
			vs := i
			for i < len(text) && text[i] != ' ' && text[i] != '\t' {
				i++
			}
			value = text[vs:i]
		}
		pairs = append(pairs, [2]string{key, value})
	}
	return pairs
}
