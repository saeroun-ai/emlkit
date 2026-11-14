package mail

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/saeroun-ai/emlkit"
)

// Reports (ROB-090-005): the machine-readable part of a multipart/report
// (RFC 6522) read into a typed model — a delivery status notification (RFC
// 3464, RFC 6533), a message disposition notification (RFC 8098) or an abuse
// feedback report (RFC 5965).
//
// Bounces are among the most damaged mail there is: written by every MTA in
// its own way, relayed through gateways, quoted and re-encoded. Each parser
// here reads what it can and says, in Diagnostics, what it repaired or could
// not read; every field stays available as it was read, in Fields, and fields
// the model does not know are kept in Unknown. A typed field holds only what
// the report gives: a value it does not give but its other fields suggest is
// in a field of its own named Guess (RecipientStatus.StatusGuess and
// ActionGuess, ReportValue.TypeGuess). See docs/robustness-notes/reports.md.

// A ReportField is one field of a report as it was read: the name as
// written and the value unfolded, the white space around it removed, its
// bytes as they were.
type ReportField struct {
	Name, Value string
}

// A ReportValue is a value of the form "type; value": an address type and an
// address (Original-Recipient, Final-Recipient), an MTA name type and a name
// (Reporting-MTA, Remote-MTA, DSN-Gateway, Received-From-MTA, MDN-Gateway),
// a diagnostic type and its text (Diagnostic-Code).
type ReportValue struct {
	// Raw is the field value as read; it is empty when the field is absent.
	Raw string
	// Type is the type the field gives, lower-cased: "rfc822", "utf-8",
	// "dns", "smtp", "x-unix"... A known type written before a colon instead
	// of the semicolon is read as the type. Type is empty when the field
	// gives none.
	Type string
	// TypeGuess is set only when Type is empty: the type the value looks
	// like — "rfc822" for an address with "@", "dns" for an MTA name that is
	// a host name, "smtp" for a diagnostic that begins with an SMTP reply
	// code. The report does not give it; the diagnostic that reports the
	// missing type names it.
	TypeGuess string
	// Value is what follows the type. For an address or a name, comments and
	// angle brackets around it are removed and Comment holds the comments'
	// text; a diagnostic's text is kept whole.
	Value   string
	Comment string
}

// A DeliveryAction is the Action of a recipient of a delivery status, one of
// the five RFC 3464 defines.
type DeliveryAction string

// The actions of RFC 3464 section 2.3.3.
const (
	ActionFailed    DeliveryAction = "failed"
	ActionDelayed   DeliveryAction = "delayed"
	ActionDelivered DeliveryAction = "delivered"
	ActionRelayed   DeliveryAction = "relayed"
	ActionExpanded  DeliveryAction = "expanded"
)

// An EnhancedStatus is an RFC 3463 status code, class.subject.detail.
type EnhancedStatus struct {
	// Raw is the Status field as read. It is empty when the field is absent,
	// and in a RecipientStatus.StatusGuess.
	Raw string
	// Class (2 success, 4 persistent transient failure, 5 permanent failure),
	// Subject and Detail are the three numbers.
	Class, Subject, Detail int
	// Valid reports that the numbers were read and the class is 2, 4 or 5.
	Valid bool
	// Inferred is set only in a RecipientStatus.StatusGuess: the code is not
	// one a Status field gives but a guess, from an SMTP reply code or from
	// the recipient's Diagnostic-Code. RecipientStatus.Status is never
	// Inferred.
	Inferred bool
	// Comment is the text of the comments around the code.
	Comment string
}

// String returns the code as "class.subject.detail", or "" when it is not
// Valid.
func (s EnhancedStatus) String() string {
	if !s.Valid {
		return ""
	}
	return strconv.Itoa(s.Class) + "." + strconv.Itoa(s.Subject) + "." + strconv.Itoa(s.Detail)
}

// Permanent reports a valid code of class 5, Transient one of class 4.
func (s EnhancedStatus) Permanent() bool { return s.Valid && s.Class == 5 }

// Transient reports a valid code of class 4.
func (s EnhancedStatus) Transient() bool { return s.Valid && s.Class == 4 }

// A RecipientStatus is the per-recipient group of a delivery status (RFC
// 3464 section 2.3).
type RecipientStatus struct {
	OriginalRecipient ReportValue
	FinalRecipient    ReportValue
	// Action is the Action field normalised to one of the five actions of
	// RFC 3464, or empty when the group has no Action or it holds none of
	// them; ActionRaw is the field as read.
	Action    DeliveryAction
	ActionRaw string
	// ActionGuess is set only when the group has no Action field: the
	// action the status class suggests (failed for 5, delayed for 4,
	// delivered for 2), taken from Status, or from StatusGuess when Status is
	// not Valid. The report does not give it.
	ActionGuess DeliveryAction
	// Status is the Status field read as an RFC 3463 code. It is not Valid
	// when the group has no Status or the field holds no such code: an SMTP
	// reply code such as "550", two different codes, a class that is not 2,
	// 4 or 5.
	Status EnhancedStatus
	// StatusGuess is set only when Status is not Valid and a code can be
	// guessed: the class of a Status that is an SMTP reply code ("550" gives
	// 5.0.0) or, when the group has no Status, an RFC 3463 code found in the
	// Diagnostic-Code, else the class of its reply code. When set it is Valid
	// and Inferred, with an empty Raw. The report does not give it, and a
	// code found in free text may be a version number that looks like one.
	StatusGuess EnhancedStatus
	RemoteMTA   ReportValue
	// DiagnosticCode is the diagnostic type ("smtp", "x-unix"...) and the
	// text, usually the remote server's reply.
	DiagnosticCode  ReportValue
	LastAttemptDate DateResult
	FinalLogID      string
	WillRetryUntil  DateResult
	// Fields are every field of the group in the order read; Unknown are
	// those the model does not know, such as X-Postfix-Sender.
	Fields  []ReportField
	Unknown []ReportField
}

// Address returns the recipient's address: the Final-Recipient's, else the
// Original-Recipient's.
func (r *RecipientStatus) Address() string {
	if r.FinalRecipient.Value != "" {
		return r.FinalRecipient.Value
	}
	return r.OriginalRecipient.Value
}

// A DeliveryStatus is a message/delivery-status or
// message/global-delivery-status body (RFC 3464 section 2, RFC 6533).
type DeliveryStatus struct {
	// The per-message fields.
	ReportingMTA       ReportValue
	OriginalEnvelopeID string
	DSNGateway         ReportValue
	ReceivedFromMTA    ReportValue
	ArrivalDate        DateResult
	// Fields are every per-message field in the order read, Unknown those the
	// model does not know.
	Fields  []ReportField
	Unknown []ReportField

	// Recipients are the per-recipient groups in order.
	Recipients []RecipientStatus

	// Unparsed are lines that were neither a field nor a continuation, up to
	// 32 of them.
	Unparsed []string

	// Diagnostics say what was repaired, assumed or could not be read.
	Diagnostics []message.Diagnostic
}

// An MDNDisposition is the Disposition field of a disposition notification
// (RFC 8098 section 3.2.6), each part lower-cased.
type MDNDisposition struct {
	Raw string
	// ActionMode is "manual-action" or "automatic-action", SendingMode
	// "mdn-sent-manually" or "mdn-sent-automatically"; either is empty when
	// the field omits it.
	ActionMode, SendingMode string
	// Type is "displayed", "deleted", "dispatched" or "processed" (or the
	// RFC 3798 "denied" and "failed").
	Type string
	// Modifiers are the modifiers after the type, such as "error".
	Modifiers []string
}

// A DispositionNotification is a message/disposition-notification body
// (RFC 8098 section 3).
type DispositionNotification struct {
	// ReportingUA is the Reporting-UA field, UAName and UAProduct its two
	// parts.
	ReportingUA, UAName, UAProduct string
	MDNGateway                     ReportValue
	OriginalRecipient              ReportValue
	FinalRecipient                 ReportValue
	// OriginalMessageID is the identifier without angle brackets.
	OriginalMessageID string
	Disposition       MDNDisposition
	// Errors are the Error fields; Failures and Warnings the Failure and
	// Warning fields of RFC 3798.
	Errors, Failures, Warnings []string

	Fields      []ReportField
	Unknown     []ReportField
	Unparsed    []string
	Diagnostics []message.Diagnostic
}

// A FeedbackReport is a message/feedback-report body (RFC 5965 section 3).
type FeedbackReport struct {
	// FeedbackType is lower-cased: "abuse", "fraud", "other", "virus",
	// "not-spam", "auth-failure"...
	FeedbackType       string
	UserAgent          string
	Version            string
	OriginalEnvelopeID string
	// OriginalMailFrom and OriginalRcptTo are the SMTP paths without angle
	// brackets.
	OriginalMailFrom string
	OriginalRcptTo   []string
	ArrivalDate      DateResult
	ReportingMTA     ReportValue
	// SourceIP is the field value without brackets or comments, SourceAddr
	// the address it parses as, zero when it does not.
	SourceIP   string
	SourceAddr netip.Addr
	// Incidents is the count the report gives, 0 when it gives none.
	Incidents       int
	ReportedDomains []string
	ReportedURIs    []string
	// AuthenticationResults are the raw values of the Authentication-Results
	// fields; see AuthResults.
	AuthenticationResults []string

	Fields      []ReportField
	Unknown     []ReportField
	Unparsed    []string
	Diagnostics []message.Diagnostic
}

// AuthResults parses the report's Authentication-Results fields with
// ParseAuthenticationResults. None of them is Trusted.
func (f *FeedbackReport) AuthResults() []AuthResult {
	out := make([]AuthResult, 0, len(f.AuthenticationResults))
	for _, v := range f.AuthenticationResults {
		out = append(out, ParseAuthenticationResults(v))
	}
	return out
}

// ParseDeliveryStatus reads the body of a message/delivery-status part: a
// per-message group of fields, then one group per recipient, separated by
// blank lines. It never fails: what it cannot read is reported in
// Diagnostics and kept in Fields, Unknown or Unparsed. UTF-8 in a field is
// reported at info severity, as RFC 3464 allows only ASCII; Node.Report
// reads a message/global-delivery-status without that.
func ParseDeliveryStatus(data []byte) *DeliveryStatus {
	return rptParseDSN(data, false, nil)
}

// ParseDispositionNotification reads the body of a
// message/disposition-notification part, one group of fields.
func ParseDispositionNotification(data []byte) *DispositionNotification {
	return rptParseMDN(data, false, nil)
}

// ParseFeedbackReport reads the body of a message/feedback-report part, one
// group of fields.
func ParseFeedbackReport(data []byte) *FeedbackReport {
	return rptParseARF(data, nil)
}

var (
	rptDSNMessageFields = map[string]bool{
		"reporting-mta": true, "original-envelope-id": true, "dsn-gateway": true,
		"received-from-mta": true, "arrival-date": true,
	}
	rptDSNRecipientFields = map[string]bool{
		"original-recipient": true, "final-recipient": true, "action": true, "status": true,
		"remote-mta": true, "diagnostic-code": true, "last-attempt-date": true,
		"final-log-id": true, "will-retry-until": true,
	}
	rptMDNFields = map[string]bool{
		"reporting-ua": true, "mdn-gateway": true, "original-recipient": true, "final-recipient": true,
		"original-message-id": true, "disposition": true, "error": true, "failure": true, "warning": true,
	}
	rptARFFields = map[string]bool{
		"feedback-type": true, "user-agent": true, "version": true, "original-envelope-id": true,
		"original-mail-from": true, "original-rcpt-to": true, "arrival-date": true, "received-date": true,
		"reporting-mta": true, "source-ip": true, "incidents": true, "reported-domain": true,
		"reported-uri": true, "authentication-results": true,
	}
)

// rptDSN assembles a delivery status from its groups.
type rptDSN struct {
	ds      *DeliveryStatus
	d       *rptDiags
	utf8OK  bool
	msgSeen map[string]bool
	seen    []map[string]bool // per recipient
	full    bool
}

func rptParseDSN(data []byte, utf8OK bool, pre []rptField) *DeliveryStatus {
	d := &rptDiags{}
	groups, unparsed := rptScan(data, func(k string) bool {
		return k == "diagnostic-code" || (!rptDSNMessageFields[k] && !rptDSNRecipientFields[k])
	}, d)
	if len(pre) > 0 {
		groups = append([][]rptField{pre}, groups...)
	}
	p := &rptDSN{ds: &DeliveryStatus{Unparsed: unparsed}, d: d, utf8OK: utf8OK, msgSeen: map[string]bool{}}
	inMessage := true
	cur := -1 // the recipient being read, or -1 at the start of a group
groups:
	for gi, g := range groups {
		if gi > 0 {
			inMessage, cur = false, -1
		}
		for _, f := range g {
			key := strings.ToLower(f.name)
			switch {
			case rptDSNMessageFields[key] && (inMessage || !p.msgSeen[key]):
				if !inMessage {
					d.add(CodeReportMisplacedField, message.SeverityRecovered, f.name,
						fmt.Sprintf("mail: report has a per-message field among a recipient's fields (line %d)", f.line), "read it as a per-message field")
				}
				p.message(key, f)
			case inMessage && !rptDSNRecipientFields[key]:
				p.message(key, f)
			default:
				if inMessage {
					inMessage = false
					if len(p.ds.Fields) > 0 {
						d.add(CodeReportMissingGroupSeparator, message.SeverityRecovered, f.name,
							fmt.Sprintf("mail: report has no blank line between the per-message fields and the first recipient's (line %d)", f.line),
							"began the first recipient's group here")
					}
				}
				if cur < 0 || p.startsRecipient(cur, key) {
					if cur >= 0 {
						d.add(CodeReportMissingGroupSeparator, message.SeverityRecovered, f.name,
							fmt.Sprintf("mail: report has no blank line between two recipients' fields (line %d)", f.line),
							"began a new recipient's group here")
					}
					if len(p.ds.Recipients) >= rptMaxRecipients {
						d.add(CodeReportLimitExceeded, message.SeverityLimitExceeded, "",
							fmt.Sprintf("mail: report has more than %d recipients", rptMaxRecipients), "read the first of them only")
						break groups
					}
					p.ds.Recipients = append(p.ds.Recipients, RecipientStatus{})
					p.seen = append(p.seen, map[string]bool{})
					cur = len(p.ds.Recipients) - 1
				}
				p.recipient(cur, key, f)
			}
		}
	}
	p.finish()
	p.ds.Diagnostics = d.list
	return p.ds
}

// startsRecipient reports whether the field key, in the group of recipient
// i, begins the next recipient's group instead: a recipient address the
// group already has.
func (p *rptDSN) startsRecipient(i int, key string) bool {
	return (key == "final-recipient" || key == "original-recipient") && p.seen[i][key]
}

func (p *rptDSN) message(key string, f rptField) {
	ds, d := p.ds, p.d
	ds.Fields = append(ds.Fields, rptReportField(f))
	if !rptDSNMessageFields[key] {
		ds.Unknown = append(ds.Unknown, rptReportField(f))
		return
	}
	if p.msgSeen[key] {
		d.add(CodeReportDuplicateField, message.SeverityRecovered, f.name,
			"mail: report repeats a per-message field", "used the first")
		return
	}
	p.msgSeen[key] = true
	v := rptClean(d, f, p.utf8OK)
	switch key {
	case "reporting-mta":
		ds.ReportingMTA = rptTyped(d, f.name, v, rptMTA)
	case "original-envelope-id":
		ds.OriginalEnvelopeID = v
	case "dsn-gateway":
		ds.DSNGateway = rptTyped(d, f.name, v, rptMTA)
	case "received-from-mta":
		ds.ReceivedFromMTA = rptTyped(d, f.name, v, rptMTA)
	case "arrival-date":
		ds.ArrivalDate = rptDate(d, f.name, v)
	}
}

func (p *rptDSN) recipient(i int, key string, f rptField) {
	r, d := &p.ds.Recipients[i], p.d
	r.Fields = append(r.Fields, rptReportField(f))
	if !rptDSNRecipientFields[key] {
		if rptDSNMessageFields[key] {
			d.add(CodeReportMisplacedField, message.SeverityRecovered, f.name,
				fmt.Sprintf("mail: report repeats a per-message field among a recipient's fields (line %d)", f.line),
				"used the first; this one is among the recipient's unknown fields")
		}
		r.Unknown = append(r.Unknown, rptReportField(f))
		return
	}
	if p.seen[i][key] {
		d.add(CodeReportDuplicateField, message.SeverityRecovered, f.name,
			fmt.Sprintf("mail: report repeats a field in one recipient's group (line %d)", f.line), "used the first")
		return
	}
	p.seen[i][key] = true
	v := rptClean(d, f, p.utf8OK)
	switch key {
	case "original-recipient":
		r.OriginalRecipient = rptTyped(d, f.name, v, rptAddress)
	case "final-recipient":
		r.FinalRecipient = rptTyped(d, f.name, v, rptAddress)
	case "action":
		r.ActionRaw = v
		r.Action = rptAction(d, f.name, v)
	case "status":
		r.Status, r.StatusGuess = rptStatus(d, f.name, v)
	case "remote-mta":
		r.RemoteMTA = rptTyped(d, f.name, v, rptMTA)
	case "diagnostic-code":
		r.DiagnosticCode = rptTyped(d, f.name, v, rptText)
	case "last-attempt-date":
		r.LastAttemptDate = rptDate(d, f.name, v)
	case "final-log-id":
		r.FinalLogID = v
	case "will-retry-until":
		r.WillRetryUntil = rptDate(d, f.name, v)
	}
}

// finish checks the fields RFC 3464 requires and guesses, apart, what can be.
func (p *rptDSN) finish() {
	ds, d := p.ds, p.d
	if !p.msgSeen["reporting-mta"] {
		d.add(CodeReportMissingField, message.SeverityInvalidMetadata, "Reporting-MTA",
			"mail: report has no Reporting-MTA", "left it empty")
	}
	if len(ds.Recipients) == 0 {
		d.add(CodeReportMissingField, message.SeverityInvalidMetadata, "Final-Recipient",
			"mail: report has no per-recipient group", "no recipient is reported")
	}
	for i := range ds.Recipients {
		r, seen := &ds.Recipients[i], p.seen[i]
		n := strconv.Itoa(i + 1)
		if !seen["final-recipient"] {
			action := "left it empty"
			if r.OriginalRecipient.Value != "" {
				action = "Address gives the Original-Recipient"
			}
			d.add(CodeReportMissingField, message.SeverityInvalidMetadata, "Final-Recipient",
				"mail: report recipient "+n+" has no Final-Recipient", action)
		}
		// A missing Status or Action is reported as missing: what can be
		// guessed goes to StatusGuess and ActionGuess, never to the fields of
		// the values the report gives (issue #66).
		if !seen["status"] {
			action := "left Status not Valid"
			if c, s, dt, ok, _ := rptFindStatus(r.DiagnosticCode.Value); ok {
				r.StatusGuess = EnhancedStatus{Class: c, Subject: s, Detail: dt, Valid: true, Inferred: true}
				action += "; StatusGuess is " + r.StatusGuess.String() + ", found in its Diagnostic-Code"
			} else if rptHasReplyCode(r.DiagnosticCode.Value) {
				r.StatusGuess = EnhancedStatus{Class: int(r.DiagnosticCode.Value[0] - '0'), Valid: true, Inferred: true}
				action += "; StatusGuess is " + r.StatusGuess.String() + ", from the class of the reply code of its Diagnostic-Code"
			}
			d.add(CodeReportMissingField, message.SeverityInvalidMetadata, "Status",
				"mail: report recipient "+n+" has no Status", action)
		}
		if !seen["action"] {
			st, from := r.Status, "Status"
			if !st.Valid {
				st, from = r.StatusGuess, "StatusGuess"
			}
			var a DeliveryAction
			switch {
			case st.Permanent():
				a = ActionFailed
			case st.Transient():
				a = ActionDelayed
			case st.Valid && st.Class == 2:
				a = ActionDelivered
			}
			action := "left Action empty"
			if a != "" {
				r.ActionGuess = a
				action += "; ActionGuess is " + string(a) + ", from the class of " + from
			}
			d.add(CodeReportMissingField, message.SeverityInvalidMetadata, "Action",
				"mail: report recipient "+n+" has no Action", action)
		}
	}
}

// rptOneGroup flattens the groups of a report that is one group of fields,
// reporting the blank lines inside it.
func rptOneGroup(groups [][]rptField, d *rptDiags) []rptField {
	if len(groups) <= 1 {
		if len(groups) == 1 {
			return groups[0]
		}
		return nil
	}
	var all []rptField
	for i, g := range groups {
		if i > 0 && len(g) > 0 {
			d.add(CodeReportUnexpectedBlankLine, message.SeverityRecovered, g[0].name,
				fmt.Sprintf("mail: report has a blank line before line %d inside its one group of fields", g[0].line),
				"read the fields after it as part of the same group")
		}
		all = append(all, g...)
	}
	return all
}

func rptParseMDN(data []byte, utf8OK bool, pre []rptField) *DispositionNotification {
	d := &rptDiags{}
	groups, unparsed := rptScan(data, func(k string) bool {
		return k == "error" || k == "failure" || k == "warning" || !rptMDNFields[k]
	}, d)
	if len(pre) > 0 {
		if len(groups) == 0 {
			groups = [][]rptField{nil}
		}
		groups[0] = append(append([]rptField(nil), pre...), groups[0]...)
	}
	m := &DispositionNotification{Unparsed: unparsed}
	seen := map[string]bool{}
	for _, f := range rptOneGroup(groups, d) {
		key := strings.ToLower(f.name)
		m.Fields = append(m.Fields, rptReportField(f))
		if !rptMDNFields[key] {
			m.Unknown = append(m.Unknown, rptReportField(f))
			continue
		}
		v := rptClean(d, f, utf8OK)
		switch key {
		case "error":
			m.Errors = append(m.Errors, v)
			continue
		case "failure":
			m.Failures = append(m.Failures, v)
			continue
		case "warning":
			m.Warnings = append(m.Warnings, v)
			continue
		}
		if seen[key] {
			d.add(CodeReportDuplicateField, message.SeverityRecovered, f.name, "mail: report repeats a field", "used the first")
			continue
		}
		seen[key] = true
		switch key {
		case "reporting-ua":
			m.ReportingUA = v
			name, product, _ := strings.Cut(v, ";")
			m.UAName, m.UAProduct = strings.TrimSpace(name), strings.TrimSpace(product)
		case "mdn-gateway":
			m.MDNGateway = rptTyped(d, f.name, v, rptMTA)
		case "original-recipient":
			m.OriginalRecipient = rptTyped(d, f.name, v, rptAddress)
		case "final-recipient":
			m.FinalRecipient = rptTyped(d, f.name, v, rptAddress)
		case "original-message-id":
			ids := ParseMsgIDListTolerant(v)
			d.merge(f.name, ids.Diagnostics)
			if len(ids.IDs) > 0 {
				m.OriginalMessageID = ids.IDs[0]
			} else {
				m.OriginalMessageID = rptUnbracket(v)
			}
		case "disposition":
			m.Disposition = rptDisposition(d, f.name, v)
		}
	}
	if !seen["final-recipient"] {
		action := "left it empty"
		if m.OriginalRecipient.Value != "" {
			action = "the Original-Recipient is the only recipient given"
		}
		d.add(CodeReportMissingField, message.SeverityInvalidMetadata, "Final-Recipient", "mail: report has no Final-Recipient", action)
	}
	if !seen["disposition"] {
		d.add(CodeReportMissingField, message.SeverityInvalidMetadata, "Disposition", "mail: report has no Disposition", "left it empty")
	}
	m.Diagnostics = d.list
	return m
}

// rptDisposition reads "action-mode/sending-mode; type[/modifier,...]" (RFC
// 8098 section 3.2.6). A missing semicolon, a missing mode and unknown words
// are reported.
func rptDisposition(d *rptDiags, field, v string) MDNDisposition {
	r := MDNDisposition{Raw: v}
	text, _ := rptStripComments(v)
	text = strings.TrimSpace(text)
	mode, typ, semi := strings.Cut(text, ";")
	if !semi {
		words := strings.Fields(text)
		switch {
		case len(words) >= 2 && rptLooksLikeMode(words[0]):
			mode, typ = words[0], strings.Join(words[1:], " ")
			d.add(CodeReportInvalidField, message.SeverityRecovered, field,
				"mail: report disposition has no semicolon between its mode and its type", "read the first word as the mode")
		case len(words) == 1 && rptLooksLikeMode(words[0]):
			mode, typ = words[0], ""
		default:
			mode, typ = "", text
			d.add(CodeReportInvalidField, message.SeverityRecovered, field,
				"mail: report disposition has no mode", "read the value as the disposition type")
		}
	}
	if mode = strings.TrimSpace(mode); mode != "" {
		am, sm, _ := strings.Cut(mode, "/")
		r.ActionMode, r.SendingMode = strings.ToLower(strings.TrimSpace(am)), strings.ToLower(strings.TrimSpace(sm))
		if (r.ActionMode != "manual-action" && r.ActionMode != "automatic-action") ||
			(r.SendingMode != "mdn-sent-manually" && r.SendingMode != "mdn-sent-automatically") {
			d.add(CodeReportInvalidField, message.SeverityInfo, field,
				fmt.Sprintf("mail: report disposition mode %q is not one RFC 8098 defines", mode), "kept it as written, lower-cased")
		}
	}
	t, mods, _ := strings.Cut(typ, "/")
	r.Type = strings.ToLower(strings.TrimSpace(t))
	for m := range strings.SplitSeq(mods, ",") {
		if m = strings.ToLower(strings.TrimSpace(m)); m != "" && len(r.Modifiers) < rptMaxList {
			r.Modifiers = append(r.Modifiers, m)
		}
	}
	switch r.Type {
	case "displayed", "deleted", "dispatched", "processed", "denied", "failed":
	case "":
		d.add(CodeReportInvalidField, message.SeverityInvalidMetadata, field, "mail: report disposition has no type", "left it empty")
	default:
		d.add(CodeReportInvalidField, message.SeverityInfo, field,
			fmt.Sprintf("mail: report disposition type %q is not one RFC 8098 defines", r.Type), "kept it as written, lower-cased")
	}
	return r
}

func rptLooksLikeMode(w string) bool {
	w = strings.ToLower(w)
	return strings.Contains(w, "-action") || strings.Contains(w, "mdn-sent")
}

func rptParseARF(data []byte, pre []rptField) *FeedbackReport {
	d := &rptDiags{}
	groups, unparsed := rptScan(data, func(k string) bool {
		return k == "authentication-results" || !rptARFFields[k]
	}, d)
	if len(pre) > 0 {
		if len(groups) == 0 {
			groups = [][]rptField{nil}
		}
		groups[0] = append(append([]rptField(nil), pre...), groups[0]...)
	}
	r := &FeedbackReport{Unparsed: unparsed}
	seen := map[string]bool{}
	var receivedDate *rptField
	for _, f := range rptOneGroup(groups, d) {
		key := strings.ToLower(f.name)
		r.Fields = append(r.Fields, rptReportField(f))
		if !rptARFFields[key] {
			r.Unknown = append(r.Unknown, rptReportField(f))
			continue
		}
		// RFC 5965 is written in ASCII; Feedback reports carry UTF-8 rarely
		// enough that it is worth a note.
		v := rptClean(d, f, false)
		switch key {
		case "original-rcpt-to":
			if len(r.OriginalRcptTo) < rptMaxFields {
				r.OriginalRcptTo = append(r.OriginalRcptTo, rptUnbracket(v))
			}
			continue
		case "reported-domain":
			r.ReportedDomains = append(r.ReportedDomains, rptUnbracket(v))
			continue
		case "reported-uri":
			r.ReportedURIs = append(r.ReportedURIs, rptUnbracket(v))
			continue
		case "authentication-results":
			r.AuthenticationResults = append(r.AuthenticationResults, v)
			continue
		case "received-date":
			if receivedDate == nil {
				receivedDate = &f
			}
			continue
		}
		if seen[key] {
			d.add(CodeReportDuplicateField, message.SeverityRecovered, f.name, "mail: report repeats a field", "used the first")
			continue
		}
		seen[key] = true
		switch key {
		case "feedback-type":
			t, _ := rptStripComments(v)
			r.FeedbackType = strings.ToLower(strings.TrimSpace(t))
			switch r.FeedbackType {
			case "abuse", "fraud", "other", "virus", "not-spam", "auth-failure":
			default:
				d.add(CodeReportInvalidField, message.SeverityInfo, f.name,
					fmt.Sprintf("mail: report feedback type %q is not a registered one", r.FeedbackType), "kept it as written, lower-cased")
			}
		case "user-agent":
			r.UserAgent = v
		case "version":
			t, _ := rptStripComments(v)
			r.Version = strings.TrimSpace(t)
			if r.Version != "1" {
				d.add(CodeReportInvalidField, message.SeverityInfo, f.name,
					fmt.Sprintf("mail: report version %q is not 1", r.Version), "read the report as version 1")
			}
		case "original-envelope-id":
			r.OriginalEnvelopeID = v
		case "original-mail-from":
			r.OriginalMailFrom = rptUnbracket(v)
		case "arrival-date":
			r.ArrivalDate = rptDate(d, f.name, v)
		case "reporting-mta":
			r.ReportingMTA = rptTyped(d, f.name, v, rptMTA)
		case "source-ip":
			r.SourceIP, r.SourceAddr = rptSourceIP(d, f.name, v)
		case "incidents":
			t, _ := rptStripComments(v)
			n, err := strconv.Atoi(strings.TrimSpace(t))
			if err != nil || n < 0 {
				d.add(CodeReportInvalidField, message.SeverityInvalidMetadata, f.name,
					"mail: report incidents is not a count", "left it 0")
			} else {
				r.Incidents = n
			}
		}
	}
	if receivedDate != nil && !seen["arrival-date"] {
		seen["arrival-date"] = true
		v := rptClean(d, *receivedDate, false)
		r.ArrivalDate = rptDate(d, receivedDate.name, v)
		d.add(CodeReportInvalidField, message.SeverityInfo, receivedDate.name,
			"mail: report gives Received-Date, the draft name of Arrival-Date", "read it as the Arrival-Date")
	}
	for _, need := range [...]struct{ key, name string }{{"feedback-type", "Feedback-Type"}, {"user-agent", "User-Agent"}, {"version", "Version"}} {
		if !seen[need.key] {
			d.add(CodeReportMissingField, message.SeverityInvalidMetadata, need.name, "mail: report has no "+need.name, "left it empty")
		}
	}
	r.Diagnostics = d.list
	return r
}

// rptSourceIP reads a Source-IP value: an IPv4 or IPv6 address, tolerating
// brackets, an "IPv6:" prefix and comments around it.
func rptSourceIP(d *rptDiags, field, v string) (string, netip.Addr) {
	t, _ := rptStripComments(v)
	t = strings.TrimSpace(t)
	s := t
	if len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']' {
		s = s[1 : len(s)-1]
	}
	if len(s) > 5 && strings.EqualFold(s[:5], "ipv6:") {
		s = s[5:]
	}
	a, err := netip.ParseAddr(s)
	switch {
	case err != nil:
		d.add(CodeReportInvalidField, message.SeverityInvalidMetadata, field, "mail: report source IP is not an IP address", "kept the text only")
		return t, netip.Addr{}
	case s != t:
		d.add(CodeReportInvalidField, message.SeverityRecovered, field,
			"mail: report source IP is wrapped in brackets or a prefix", "read the address inside")
	}
	return s, a
}
