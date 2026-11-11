package mail

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/textproto"
)

// A ReportKind is what a report part reports.
type ReportKind uint8

const (
	// ReportUnknown is a part that reads as none of the kinds below.
	ReportUnknown ReportKind = iota
	// ReportDeliveryStatus is a delivery status notification (RFC 3464,
	// RFC 6533): message/delivery-status, message/global-delivery-status.
	ReportDeliveryStatus
	// ReportDispositionNotification is a message disposition notification
	// (RFC 8098): message/disposition-notification and its global form.
	ReportDispositionNotification
	// ReportFeedback is an abuse feedback report (RFC 5965):
	// message/feedback-report.
	ReportFeedback
)

// String returns the report-type of the kind, or "unknown".
func (k ReportKind) String() string {
	switch k {
	case ReportDeliveryStatus:
		return "delivery-status"
	case ReportDispositionNotification:
		return "disposition-notification"
	case ReportFeedback:
		return "feedback-report"
	}
	return "unknown"
}

// A Report is a multipart/report (RFC 6522) read as a whole: its
// human-readable part, its machine-readable part read into the model of its
// kind, and the original message it returns.
type Report struct {
	// Kind is what the report part was read as. Exactly one of Delivery,
	// Disposition and Feedback is set for a known kind.
	Kind        ReportKind
	Delivery    *DeliveryStatus
	Disposition *DispositionNotification
	Feedback    *FeedbackReport

	// ReportType is the report-type parameter the multipart/report declares,
	// lower-cased, or empty.
	ReportType string

	// Container is the multipart/report, nil for a report part read on its
	// own. Description is its human-readable part, Part its report part and
	// Original the message or header block it returns (message/rfc822,
	// message/global, text/rfc822-headers, message/global-headers); each is
	// nil when the report has none.
	Container   *Node
	Description *Node
	Part        *Node
	Original    *Node
	// OriginalHeader is the header of the returned message: the embedded
	// message's own when the tree descended into it, else read from
	// Original's Data. It is nil when there is none to read.
	OriginalHeader *message.Header

	// Diagnostics are what was found about the report's structure, then the
	// diagnostics of the report part's model, each with the MIME path of the
	// part it is about.
	Diagnostics []message.Diagnostic
}

// Report reads the report n is or is part of. For a multipart/report it reads
// the whole of it; for a report part — message/delivery-status,
// message/global-delivery-status, message/disposition-notification,
// message/global-disposition-notification, message/feedback-report — it reads
// that part, with the human-readable part and the returned message of the
// multipart/report it is in, if it is in one. It returns nil for any other
// node.
//
// The report part is the first part of a known report media type. When there
// is none, the second part is read by the report-type parameter, or else by
// the fields it holds, and that is reported. A report-type that disagrees
// with the report part's media type is reported, and the part is read by its
// media type.
func (n *Node) Report() *Report {
	switch {
	case n == nil:
		return nil
	case n.MediaType == "multipart/report":
		return rptBuild(n, nil)
	case rptKindOfMedia(n.MediaType) != ReportUnknown:
		if p := n.Parent; p != nil && p.MediaType == "multipart/report" {
			return rptBuild(p, n)
		}
		return rptBuild(nil, n)
	}
	return nil
}

// rptKindOfMedia returns the kind a report media type names.
func rptKindOfMedia(mediaType string) ReportKind {
	switch mediaType {
	case "message/delivery-status", "message/global-delivery-status":
		return ReportDeliveryStatus
	case "message/disposition-notification", "message/global-disposition-notification":
		return ReportDispositionNotification
	case "message/feedback-report":
		return ReportFeedback
	}
	return ReportUnknown
}

// rptKindOfType returns the kind a report-type parameter names.
func rptKindOfType(reportType string) ReportKind {
	if reportType == "" {
		return ReportUnknown
	}
	return rptKindOfMedia("message/" + reportType)
}

// rptSniff returns the kind the fields of data look like.
func rptSniff(data []byte) ReportKind {
	groups, _ := rptScan(data[:min(len(data), 16<<10)], func(string) bool { return false }, &rptDiags{})
	var disposition, delivery bool
	for _, g := range groups {
		for _, f := range g {
			switch strings.ToLower(f.name) {
			case "feedback-type":
				return ReportFeedback
			case "disposition":
				disposition = true
			case "action", "status", "reporting-mta", "diagnostic-code":
				delivery = true
			}
		}
	}
	switch {
	case delivery:
		return ReportDeliveryStatus
	case disposition:
		return ReportDispositionNotification
	}
	return ReportUnknown
}

func rptBuild(container, part *Node) *Report {
	r := &Report{Container: container, Part: part}
	d := &rptDiags{}
	var kind ReportKind
	if container != nil {
		r.ReportType = strings.ToLower(strings.TrimSpace(container.Params["report-type"]))
		declared := rptKindOfType(r.ReportType)
		if part == nil {
			for _, c := range container.Children {
				if rptKindOfMedia(c.MediaType) != ReportUnknown {
					part = c
					break
				}
			}
		}
		if part != nil {
			kind = rptKindOfMedia(part.MediaType)
			switch {
			case r.ReportType == "":
				d.add(CodeReportTypeMismatch, message.SeverityInfo, "Content-Type",
					"mail: multipart/report declares no report-type", "read the report part by its media type")
			case declared != kind:
				d.add(CodeReportTypeMismatch, message.SeverityInfo, "Content-Type",
					fmt.Sprintf("mail: multipart/report declares the report-type %q and its report part is %s", r.ReportType, part.MediaType),
					"read the report part by its media type")
			}
		} else if len(container.Children) >= 2 && rptIsLeaf(container.Children[1]) {
			// No part is labelled as a report: the second part is where RFC
			// 6522 puts it.
			c := container.Children[1]
			if declared != ReportUnknown {
				part, kind = c, declared
				d.add(CodeReportTypeMismatch, message.SeverityRecovered, "Content-Type",
					fmt.Sprintf("mail: multipart/report's second part is %s, not a report media type", c.MediaType),
					"read it as the declared report-type "+r.ReportType)
			} else if k := rptSniff(c.Data); k != ReportUnknown {
				part, kind = c, k
				d.add(CodeReportTypeMismatch, message.SeverityRecovered, "Content-Type",
					fmt.Sprintf("mail: multipart/report's second part is %s and the report-type %q names no known report", c.MediaType, r.ReportType),
					"read it as a "+k.String()+" by the fields it holds")
			}
		}
		r.Part = part
		if part == nil {
			d.add(CodeReportMissingPart, message.SeverityInvalidMetadata, "",
				"mail: multipart/report has no part that reads as a report", "kept its parts as they are")
		}
		for i, c := range container.Children {
			switch {
			case c == part:
			case i == 0 && r.Description == nil && !carriesMessage(c.MediaType):
				r.Description = c
			case r.Original == nil && carriesMessage(c.MediaType):
				r.Original = c
			}
		}
		if r.Description == nil {
			d.add(CodeReportMissingPart, message.SeverityInfo, "",
				"mail: multipart/report has no human-readable first part", "")
		}
	} else {
		kind = rptKindOfMedia(part.MediaType)
	}
	r.Kind = kind
	for i := range d.list {
		d.list[i].Path = rptPath(container)
	}
	r.Diagnostics = d.list
	r.OriginalHeader = rptOriginalHeader(r.Original)
	if part == nil {
		return r
	}

	pd := &rptDiags{}
	data := part.Data
	if part.Truncated || (len(data) == 0 && part.Size > 0) {
		// The line the cut fell in is not whole: it is not read.
		data = data[:bytes.LastIndexAny(data, "\r\n")+1]
		pd.add(CodeReportTruncated, message.SeverityIncomplete, "",
			fmt.Sprintf("mail: report part holds %d bytes and %d of them were kept", part.Size, len(part.Data)),
			"read the fields of the whole lines that were kept")
	}
	var pre []rptField
	if part.Parent != nil {
		pre = rptHeaderFields(part.Header, kind)
	}
	if len(pre) > 0 {
		pd.add(CodeReportMisplacedField, message.SeverityRecovered, pre[0].name,
			"mail: report fields begin in the report part's MIME header, with no blank line before them", "read them as report fields")
	}
	global := strings.HasPrefix(part.MediaType, "message/global-")
	var list *[]message.Diagnostic
	switch kind {
	case ReportDeliveryStatus:
		r.Delivery = rptParseDSN(data, global, pre)
		list = &r.Delivery.Diagnostics
	case ReportDispositionNotification:
		r.Disposition = rptParseMDN(data, global, pre)
		list = &r.Disposition.Diagnostics
	case ReportFeedback:
		r.Feedback = rptParseARF(data, pre)
		list = &r.Feedback.Diagnostics
	}
	if list != nil {
		*list = append(pd.list, *list...)
		for i := range *list {
			(*list)[i].Path = rptPath(part)
			r.Diagnostics = append(r.Diagnostics, (*list)[i].Clone())
		}
	}
	return r
}

func rptPath(n *Node) []int {
	if n == nil || n.Path == nil {
		return nil
	}
	return append([]int(nil), n.Path...)
}

func rptIsLeaf(n *Node) bool {
	return len(n.Children) == 0 && !strings.HasPrefix(n.MediaType, "multipart/") && !carriesMessage(n.MediaType)
}

// rptHeaderFields returns the fields of a report part's MIME header that
// belong to its report: those a part gets when its report begins with no
// blank line after the MIME header, or with no MIME header at all.
func rptHeaderFields(h message.Header, kind ReportKind) []rptField {
	var known map[string]bool
	switch kind {
	case ReportDeliveryStatus:
		known = rptDSNMessageFields
	case ReportDispositionNotification:
		known = rptMDNFields
	case ReportFeedback:
		known = rptARFFields
	default:
		return nil
	}
	var out []rptField
	fields := h.Fields()
	for fields.Next() && len(out) < rptMaxFields {
		k := fields.Key()
		lk := strings.ToLower(k)
		if known[lk] || (kind == ReportDeliveryStatus && rptDSNRecipientFields[lk]) {
			out = append(out, rptField{name: k, value: strings.TrimSpace(fields.Value())})
		}
	}
	return out
}

// rptOriginalHeader returns the header of the message n returns.
func rptOriginalHeader(n *Node) *message.Header {
	if n == nil {
		return nil
	}
	if len(n.Children) > 0 {
		h := n.Children[0].Header
		return &h
	}
	if len(n.Data) == 0 {
		return nil
	}
	data := n.Data[:min(len(n.Data), rptMaxBytes)]
	// A header cut short is still the header that was there, so an error
	// with fields read is not a reason to drop them.
	h, _, _ := textproto.ReadHeaderRecover(bufio.NewReader(bytes.NewReader(data)),
		textproto.HeaderLimits{MaxBytes: rptMaxBytes, MaxCount: rptMaxFields})
	if h.Len() == 0 {
		return nil
	}
	return &message.Header{Header: h}
}
