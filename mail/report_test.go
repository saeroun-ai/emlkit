package mail_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// The examples of RFC 3464 appendix C, RFC 8098 section 9 and RFC 5965
// appendix B, with their addresses moved under example.com, .net and .org.

const rptRFC3464Simple = "Date: Thu, 7 Jul 1994 17:16:05 -0400\r\n" +
	"From: Mail Delivery Subsystem <MAILER-DAEMON@example.com>\r\n" +
	"Message-Id: <199407072116.RAA14128@example.com>\r\n" +
	"Subject: Returned mail: Cannot send message for 5 days\r\n" +
	"To: <owner-info-mime@example.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/report; report-type=delivery-status;\r\n" +
	"    boundary=\"RAA14128.773615765/example.com\"\r\n" +
	"\r\n" +
	"--RAA14128.773615765/example.com\r\n" +
	"\r\n" +
	"The original message was received at Sat, 2 Jul 1994 17:10:28 -0400\r\n" +
	"from root@localhost\r\n" +
	"\r\n" +
	"   ----- The following addresses had delivery problems -----\r\n" +
	"<louisl@example.net>  (unrecoverable error)\r\n" +
	"\r\n" +
	"   ----- Transcript of session follows -----\r\n" +
	"<louisl@example.net>... Deferred: Connection timed out\r\n" +
	"        with example.net.\r\n" +
	"Message could not be delivered for 5 days\r\n" +
	"Message will be deleted from queue\r\n" +
	"\r\n" +
	"--RAA14128.773615765/example.com\r\n" +
	"content-type: message/delivery-status\r\n" +
	"\r\n" +
	"Reporting-MTA: dns; cs.example.com\r\n" +
	"\r\n" +
	"Original-Recipient: rfc822;louisl@example.net\r\n" +
	"Final-Recipient: rfc822;louisl@example.net\r\n" +
	"Action: failed\r\n" +
	"Status: 4.0.0\r\n" +
	"Diagnostic-Code: smtp; 426 connection timed out\r\n" +
	"Last-Attempt-Date: Thu, 7 Jul 1994 17:15:49 -0400\r\n" +
	"\r\n" +
	"--RAA14128.773615765/example.com\r\n" +
	"content-type: message/rfc822\r\n" +
	"\r\n" +
	"Subject: Re: mime info\r\n" +
	"Message-Id: <orig.1@example.com>\r\n" +
	"\r\n" +
	"[original message goes here]\r\n" +
	"\r\n" +
	"--RAA14128.773615765/example.com--\r\n"

// The per-message fields of the gateway example and the recipients of the
// multi-recipient and delayed examples, as one delivery-status body.
const rptRFC3464Body = "Reporting-MTA: dns; mta.example.com\r\n" +
	"DSN-Gateway: dns; gw.example.com\r\n" +
	"Received-From-MTA: dns; relay.example.net\r\n" +
	"Arrival-Date: Fri, 8 Jul 1994 09:21:47 -0400\r\n" +
	"Original-Envelope-Id: 0123456789\r\n" +
	"\r\n" +
	"Final-Recipient: rfc822;thomas@example.org\r\n" +
	"Status: 5.0.0 (permanent failure)\r\n" +
	"Action: failed\r\n" +
	"Diagnostic-Code: smtp; 550 'thomas@example.org' is not a registered gateway user\r\n" +
	"Remote-MTA: dns; bortas.example.org\r\n" +
	"\r\n" +
	"Final-Recipient: rfc822;mt5@example.org\r\n" +
	"Action: delayed\r\n" +
	"Status: 4.0.0\r\n" +
	"Will-Retry-Until: Fri, 15 Jul 1994 09:21:47 -0400\r\n" +
	"Final-Log-ID: 7F3A2\r\n" +
	"X-Postfix-Note: kept\r\n" +
	"\r\n" +
	"Original-Recipient: rfc822;arnold@example.org\r\n" +
	"Final-Recipient: X400; /PN=Arnold/O=example/C=org/\r\n" +
	"Action: failed\r\n" +
	"Status: 5.0.0\r\n" +
	"Diagnostic-Code: X400; 1.12 (unable to deliver)\r\n"

const rptRFC8098 = "Date: Wed, 20 Sep 1995 00:19:00 (EDT) -0400\r\n" +
	"From: Joe Recipient <Joe_Recipient@example.com>\r\n" +
	"Message-Id: <199509200019.12345@example.com>\r\n" +
	"Subject: Disposition notification\r\n" +
	"To: Jane Sender <Jane_Sender@example.org>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/report; report-type=disposition-notification;\r\n" +
	"    boundary=\"RAA14128.773615765/example.com\"\r\n" +
	"\r\n" +
	"--RAA14128.773615765/example.com\r\n" +
	"\r\n" +
	"The message sent on 1995 Sep 19 at 13:30:00 (EDT) -0400 to Joe\r\n" +
	"Recipient <Joe_Recipient@example.com> with subject \"First draft of\r\n" +
	"report\" has been displayed.\r\n" +
	"This is no guarantee that the message has been read or understood.\r\n" +
	"\r\n" +
	"--RAA14128.773615765/example.com\r\n" +
	"content-type: message/disposition-notification\r\n" +
	"\r\n" +
	"Reporting-UA: joes-pc.cs.example.com; Foomail 97.1\r\n" +
	"Original-Recipient: rfc822;Joe_Recipient@example.com\r\n" +
	"Final-Recipient: rfc822;Joe_Recipient@example.com\r\n" +
	"Original-Message-ID: <199509192301.23456@example.org>\r\n" +
	"Disposition: manual-action/MDN-sent-manually; displayed\r\n" +
	"\r\n" +
	"--RAA14128.773615765/example.com\r\n" +
	"content-type: message/rfc822\r\n" +
	"\r\n" +
	"Subject: First draft of report\r\n" +
	"\r\n" +
	"[original message optionally goes here]\r\n" +
	"\r\n" +
	"--RAA14128.773615765/example.com--\r\n"

const rptRFC5965 = "From: <abusedesk@example.com>\r\n" +
	"Date: Thu, 8 Mar 2005 17:40:36 EDT\r\n" +
	"Subject: FW: Earn money\r\n" +
	"To: <abuse@example.net>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/report; report-type=feedback-report;\r\n" +
	"     boundary=\"part1_13d.2e68ed54_boundary\"\r\n" +
	"\r\n" +
	"--part1_13d.2e68ed54_boundary\r\n" +
	"Content-Type: text/plain; charset=\"US-ASCII\"\r\n" +
	"Content-Transfer-Encoding: 7bit\r\n" +
	"\r\n" +
	"This is an email abuse report for an email message received from IP\r\n" +
	"192.0.2.1 on Thu, 8 Mar 2005 14:00:00 EDT.  For more information\r\n" +
	"about this format please see http://www.mipassoc.org/arf/.\r\n" +
	"\r\n" +
	"--part1_13d.2e68ed54_boundary\r\n" +
	"Content-Type: message/feedback-report\r\n" +
	"\r\n" +
	"Feedback-Type: abuse\r\n" +
	"User-Agent: SomeGenerator/1.0\r\n" +
	"Version: 1\r\n" +
	"Original-Mail-From: <somespammer@example.net>\r\n" +
	"Original-Rcpt-To: <user@example.com>\r\n" +
	"Arrival-Date: Thu, 8 Mar 2005 14:00:00 EDT\r\n" +
	"Reporting-MTA: dns; mail.example.com\r\n" +
	"Source-IP: 192.0.2.1\r\n" +
	"Authentication-Results: mail.example.com;\r\n" +
	"               spf=fail smtp.mailfrom=example.net\r\n" +
	"Reported-Domain: example.net\r\n" +
	"Reported-Uri: http://example.net/earn_money.html\r\n" +
	"Reported-Uri: mailto:user@example.com\r\n" +
	"Removal-Recipient: user@example.com\r\n" +
	"\r\n" +
	"--part1_13d.2e68ed54_boundary\r\n" +
	"Content-Type: message/rfc822\r\n" +
	"Content-Disposition: inline\r\n" +
	"\r\n" +
	"From: <somespammer@example.net>\r\n" +
	"Received: from mailserver.example.net (mailserver.example.net\r\n" +
	"        [192.0.2.1]) by example.com with ESMTP id M63d4137594e46;\r\n" +
	"        Thu, 08 Mar 2005 14:00:00 -0400\r\n" +
	"To: <Undisclosed Recipients>\r\n" +
	"Subject: Earn money\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-type: text/plain\r\n" +
	"Message-ID: 8787KJKJ3K4J3K4J3K4J3.mail@example.net\r\n" +
	"Date: Thu, 02 Sep 2004 12:31:03 -0500\r\n" +
	"\r\n" +
	"Spam Spam Spam\r\n" +
	"--part1_13d.2e68ed54_boundary--\r\n"

func rptTree(t *testing.T, text string, opts *mail.TreeOptions) *mail.Tree {
	t.Helper()
	_, e, err := message.Parse(strings.NewReader(text), nil)
	if e == nil {
		t.Fatalf("Parse() = %v", err)
	}
	tree, err := mail.ReadTree(e, opts)
	if err != nil {
		t.Fatalf("ReadTree() = %v", err)
	}
	return tree
}

// rptCodes returns the distinct codes of list, sorted.
func rptCodes(list []message.Diagnostic) []string {
	var out []string
	for _, d := range list {
		if !slices.Contains(out, string(d.Code)) {
			out = append(out, string(d.Code))
		}
	}
	slices.Sort(out)
	return out
}

func rptWant(t *testing.T, name string, list []message.Diagnostic, want ...string) {
	t.Helper()
	slices.Sort(want)
	if got := rptCodes(list); !slices.Equal(got, want) {
		t.Errorf("%s: codes %q, want %q; diagnostics:\n%v", name, got, want, list)
	}
}

func TestReport_rfcSamples(t *testing.T) {
	t.Run("RFC 3464 multipart", func(t *testing.T) {
		r := rptTree(t, rptRFC3464Simple, nil).Root.Report()
		if r == nil || r.Kind != mail.ReportDeliveryStatus || r.Delivery == nil || r.ReportType != "delivery-status" {
			t.Fatalf("report %+v", r)
		}
		rptWant(t, "diagnostics", r.Diagnostics)
		if r.Description == nil || !strings.Contains(string(r.Description.Data), "Transcript of session") {
			t.Errorf("description %v", r.Description)
		}
		if r.Original == nil || r.OriginalHeader == nil || r.OriginalHeader.Get("Message-Id") != "<orig.1@example.com>" {
			t.Errorf("original %v, header %v", r.Original, r.OriginalHeader)
		}
		ds := r.Delivery
		if ds.ReportingMTA != (mail.ReportValue{Raw: "dns; cs.example.com", Type: "dns", Value: "cs.example.com"}) || len(ds.Recipients) != 1 {
			t.Fatalf("delivery %+v", ds)
		}
		rc := ds.Recipients[0]
		if rc.Address() != "louisl@example.net" || rc.OriginalRecipient.Type != "rfc822" || rc.Action != mail.ActionFailed ||
			rc.Status.String() != "4.0.0" || !rc.Status.Transient() || rc.DiagnosticCode.Type != "smtp" ||
			rc.DiagnosticCode.Value != "426 connection timed out" || !rc.LastAttemptDate.Valid || rc.LastAttemptDate.Time.Day() != 7 {
			t.Errorf("recipient %+v", rc)
		}
	})
	t.Run("RFC 3464 groups", func(t *testing.T) {
		ds := mail.ParseDeliveryStatus([]byte(rptRFC3464Body))
		rptWant(t, "diagnostics", ds.Diagnostics)
		if ds.DSNGateway.Value != "gw.example.com" || ds.ReceivedFromMTA.Value != "relay.example.net" ||
			!ds.ArrivalDate.Valid || ds.OriginalEnvelopeID != "0123456789" || len(ds.Fields) != 5 || len(ds.Recipients) != 3 {
			t.Fatalf("delivery %+v", ds)
		}
		r0, r1, r2 := ds.Recipients[0], ds.Recipients[1], ds.Recipients[2]
		if r0.Status.String() != "5.0.0" || r0.Status.Comment != "permanent failure" || r0.RemoteMTA.Value != "bortas.example.org" {
			t.Errorf("recipient 0 %+v", r0)
		}
		if r1.Action != mail.ActionDelayed || !r1.WillRetryUntil.Valid || r1.FinalLogID != "7F3A2" ||
			!reflect.DeepEqual(r1.Unknown, []mail.ReportField{{Name: "X-Postfix-Note", Value: "kept"}}) || len(r1.Fields) != 6 {
			t.Errorf("recipient 1 %+v", r1)
		}
		if r2.FinalRecipient.Type != "x400" || r2.FinalRecipient.Value != "/PN=Arnold/O=example/C=org/" ||
			r2.DiagnosticCode.Value != "1.12 (unable to deliver)" || r2.OriginalRecipient.Value != "arnold@example.org" {
			t.Errorf("recipient 2 %+v", r2)
		}
	})
	t.Run("RFC 8098", func(t *testing.T) {
		r := rptTree(t, rptRFC8098, nil).Root.Report()
		if r == nil || r.Kind != mail.ReportDispositionNotification || r.Disposition == nil {
			t.Fatalf("report %+v", r)
		}
		rptWant(t, "diagnostics", r.Diagnostics)
		m := r.Disposition
		want := mail.MDNDisposition{Raw: "manual-action/MDN-sent-manually; displayed", ActionMode: "manual-action", SendingMode: "mdn-sent-manually", Type: "displayed"}
		if !reflect.DeepEqual(m.Disposition, want) || m.UAName != "joes-pc.cs.example.com" || m.UAProduct != "Foomail 97.1" ||
			m.FinalRecipient.Value != "Joe_Recipient@example.com" || m.OriginalMessageID != "199509192301.23456@example.org" {
			t.Errorf("notification %+v", m)
		}
		if r.OriginalHeader == nil || r.OriginalHeader.Get("Subject") != "First draft of report" {
			t.Errorf("original header %v", r.OriginalHeader)
		}
	})
	t.Run("RFC 5965", func(t *testing.T) {
		r := rptTree(t, rptRFC5965, nil).Root.Report()
		if r == nil || r.Kind != mail.ReportFeedback || r.Feedback == nil {
			t.Fatalf("report %+v", r)
		}
		// The sample's EDT is an obsolete zone name, and 8 March 2005 was a
		// Tuesday: the date parser notes both.
		rptWant(t, "diagnostics", r.Diagnostics, "date.obsolete_zone", "date.weekday_mismatch")
		f := r.Feedback
		if f.FeedbackType != "abuse" || f.UserAgent != "SomeGenerator/1.0" || f.Version != "1" ||
			f.OriginalMailFrom != "somespammer@example.net" || !slices.Equal(f.OriginalRcptTo, []string{"user@example.com"}) ||
			!f.ArrivalDate.Valid || f.ReportingMTA.Value != "mail.example.com" || f.SourceAddr.String() != "192.0.2.1" ||
			!slices.Equal(f.ReportedDomains, []string{"example.net"}) ||
			!slices.Equal(f.ReportedURIs, []string{"http://example.net/earn_money.html", "mailto:user@example.com"}) ||
			len(f.Unknown) != 1 || f.Unknown[0].Name != "Removal-Recipient" {
			t.Errorf("feedback %+v", f)
		}
		if ar := f.AuthResults(); len(ar) != 1 || ar[0].AuthServID != "mail.example.com" || len(ar[0].Methods) != 1 || ar[0].Methods[0].Result != "fail" {
			t.Errorf("auth results %+v", ar)
		}
		if r.OriginalHeader == nil || r.OriginalHeader.Get("Subject") != "Earn money" {
			t.Errorf("original header %v", r.OriginalHeader)
		}
	})
}

func TestParseDeliveryStatus_malformed(t *testing.T) {
	const rcpt = "Final-Recipient: rfc822; a@example.com\nAction: failed\n"
	many := "Reporting-MTA: dns; mx.example.com\n" + strings.Repeat("\nFinal-Recipient: rfc822; a@example.com\nAction: failed\nStatus: 5.1.1\n", 1100)
	tests := []struct {
		name  string
		body  string
		codes []string
		check func(*mail.DeliveryStatus) bool
	}{
		{"LF only, no blank lines", "Reporting-MTA: dns; mx.example.com\nFinal-Recipient: rfc822; a@example.com\nAction: failed\nStatus: 5.1.1\nFinal-Recipient: rfc822; b@example.com\nAction: delayed\nStatus: 4.4.1\n",
			[]string{"report.missing_group_separator"},
			func(ds *mail.DeliveryStatus) bool {
				return len(ds.Recipients) == 2 && ds.Recipients[1].Address() == "b@example.com" && ds.Recipients[1].Action == mail.ActionDelayed
			}},
		{"mixed endings and folding", "Reporting-MTA: dns;\r\n mx.example.com\n\r\nFinal-Recipient: rfc822; a@example.com\rAction: failed\r\nStatus: 5.1.1\nDiagnostic-Code: smtp; 550-5.1.1 The account does\r\n    not exist.\n",
			nil,
			func(ds *mail.DeliveryStatus) bool {
				return ds.ReportingMTA.Value == "mx.example.com" && len(ds.Recipients) == 1 &&
					strings.HasSuffix(ds.Recipients[0].DiagnosticCode.Value, "not exist.")
			}},
		{"status with a comment", "Reporting-MTA: dns; mx.example.com\n\n" + rcpt + "Status: 5.1.1 (bad mailbox)\n",
			nil,
			func(ds *mail.DeliveryStatus) bool {
				return ds.Recipients[0].Status.String() == "5.1.1" && ds.Recipients[0].Status.Comment == "bad mailbox"
			}},
		{"status in text", "Reporting-MTA: dns; mx.example.com\n\n" + rcpt + "Status: 550 5.1.1 user unknown\n",
			[]string{"report.invalid_status"},
			func(ds *mail.DeliveryStatus) bool { return ds.Recipients[0].Status.String() == "5.1.1" }},
		{"status as a reply code", "Reporting-MTA: dns; mx.example.com\n\n" + rcpt + "Status: 550\n",
			[]string{"report.invalid_status"},
			func(ds *mail.DeliveryStatus) bool { return ds.Recipients[0].Status.String() == "5.0.0" }},
		{"status unreadable", "Reporting-MTA: dns; mx.example.com\n\n" + rcpt + "Status: 5.1\n",
			[]string{"report.invalid_status"},
			func(ds *mail.DeliveryStatus) bool {
				return !ds.Recipients[0].Status.Valid && ds.Recipients[0].Status.Raw == "5.1"
			}},
		{"action variants", "Reporting-MTA: dns; mx.example.com\n\nFinal-Recipient: rfc822; a@example.com\nAction: FAILED\nStatus: 5.0.0\n\nFinal-Recipient: rfc822; b@example.com\nAction: Failure\nStatus: 5.0.0\n\nFinal-Recipient: rfc822; c@example.com\nAction: bogus\nStatus: 5.0.0\n",
			[]string{"report.invalid_action"},
			func(ds *mail.DeliveryStatus) bool {
				r := ds.Recipients
				return r[0].Action == mail.ActionFailed && r[1].Action == mail.ActionFailed && r[1].ActionRaw == "Failure" && r[2].Action == ""
			}},
		{"action and status inferred", "Reporting-MTA: dns; mx.example.com\n\nFinal-Recipient: rfc822; a@example.com\nDiagnostic-Code: smtp; 550 5.1.1 <a@example.com>: Recipient address rejected\n",
			[]string{"report.missing_field"},
			func(ds *mail.DeliveryStatus) bool {
				r := ds.Recipients[0]
				return r.Status.String() == "5.1.1" && r.Status.Inferred && r.Action == mail.ActionFailed && r.ActionRaw == ""
			}},
		{"untyped and bracketed values", "Reporting-MTA: mx.example.com\n\nFinal-Recipient: <a@example.com>\nOriginal-Recipient: rfc822;<a@example.com>\nAction: failed\nStatus: 5.0.0\nDiagnostic-Code: 550 user unknown\n",
			[]string{"report.invalid_field"},
			func(ds *mail.DeliveryStatus) bool {
				r := ds.Recipients[0]
				return ds.ReportingMTA.Type == "dns" && r.FinalRecipient == (mail.ReportValue{Raw: "<a@example.com>", Type: "rfc822", Value: "a@example.com"}) &&
					r.OriginalRecipient.Value == "a@example.com" && r.DiagnosticCode.Type == "smtp"
			}},
		{"colon for semicolon", "Reporting-MTA: dns; mx.example.com\n\nFinal-Recipient: RFC822: a@example.com\nAction: failed\nStatus: 5.0.0\n",
			[]string{"report.invalid_field"},
			func(ds *mail.DeliveryStatus) bool {
				return ds.Recipients[0].FinalRecipient.Type == "rfc822" && ds.Recipients[0].Address() == "a@example.com"
			}},
		{"field in the wrong group", rcpt + "Status: 5.1.1\nReporting-MTA: dns; mx.example.com\n",
			[]string{"report.misplaced_field"},
			func(ds *mail.DeliveryStatus) bool {
				return ds.ReportingMTA.Value == "mx.example.com" && len(ds.Recipients) == 1 && len(ds.Recipients[0].Unknown) == 0
			}},
		{"garbage and an unindented continuation", "Reporting-MTA: dns; mx.example.com\n----- garbage -----\n\n" + rcpt + "Status: 5.1.1\nDiagnostic-Code: smtp; 550 5.1.1 <a@example.com>: Recipient address rejected:\nUser unknown in virtual mailbox table\n",
			[]string{"report.garbage_line", "report.unindented_continuation"},
			func(ds *mail.DeliveryStatus) bool {
				return slices.Equal(ds.Unparsed, []string{"----- garbage -----"}) &&
					strings.HasSuffix(ds.Recipients[0].DiagnosticCode.Value, "rejected: User unknown in virtual mailbox table")
			}},
		{"non-ASCII diagnostic", "Reporting-MTA: dns; mx.example.com\n\n" + rcpt + "Status: 5.1.1\nDiagnostic-Code: smtp; 550 수신자 없음\n\nFinal-Recipient: rfc822; b@example.com\nAction: failed\nStatus: 5.1.1\nDiagnostic-Code: smtp; 550 \xbc\xf6\xbd\xc5\n",
			[]string{"report.non_ascii"},
			func(ds *mail.DeliveryStatus) bool {
				r0, r1 := ds.Recipients[0], ds.Recipients[1]
				return r0.DiagnosticCode.Value == "550 수신자 없음" && strings.ContainsRune(r1.DiagnosticCode.Value, utf8.RuneError) &&
					strings.Contains(r1.Fields[3].Value, "\xbc\xf6")
			}},
		{"duplicate status", "Reporting-MTA: dns; mx.example.com\n\n" + rcpt + "Status: 5.1.1\nStatus: 4.0.0\n",
			[]string{"report.duplicate_field"},
			func(ds *mail.DeliveryStatus) bool {
				return ds.Recipients[0].Status.String() == "5.1.1" && len(ds.Recipients[0].Fields) == 4
			}},
		{"empty", "",
			[]string{"report.missing_field"},
			func(ds *mail.DeliveryStatus) bool { return len(ds.Recipients) == 0 }},
		{"too many recipients", many,
			[]string{"report.limit_exceeded"},
			func(ds *mail.DeliveryStatus) bool { return len(ds.Recipients) == 1024 }},
		{"too many bytes", "Reporting-MTA: dns; mx.example.com\n\n" + rcpt + "Status: 5.1.1\n" + strings.Repeat("X-Pad: "+strings.Repeat("a", 1000)+"\n", 300),
			[]string{"report.limit_exceeded"},
			func(ds *mail.DeliveryStatus) bool {
				return len(ds.Recipients) == 1 && len(ds.Recipients[0].Unknown) < 300
			}},
		{"too many diagnostics", strings.Repeat("junk line\n", 200),
			[]string{"report.garbage_line", "limit.diagnostic_count"},
			func(ds *mail.DeliveryStatus) bool { return len(ds.Unparsed) == 32 && len(ds.Diagnostics) == 128 }},
	}
	for _, tc := range tests {
		ds := mail.ParseDeliveryStatus([]byte(tc.body))
		rptWant(t, tc.name, ds.Diagnostics, tc.codes...)
		if !tc.check(ds) {
			t.Errorf("%s: %+v", tc.name, ds)
		}
	}
}

func TestParseDispositionNotification_malformed(t *testing.T) {
	const fr = "Final-Recipient: rfc822; a@example.com\n"
	tests := []struct {
		name  string
		body  string
		codes []string
		check func(*mail.DispositionNotification) bool
	}{
		{"no semicolon", fr + "Disposition: manual-action/MDN-sent-manually displayed\n",
			[]string{"report.invalid_field"},
			func(m *mail.DispositionNotification) bool {
				return m.Disposition.ActionMode == "manual-action" && m.Disposition.Type == "displayed"
			}},
		{"no mode", fr + "Disposition: Displayed\n",
			[]string{"report.invalid_field"},
			func(m *mail.DispositionNotification) bool {
				return m.Disposition.Type == "displayed" && m.Disposition.ActionMode == ""
			}},
		{"modifiers and RFC 3798 fields", fr + "Disposition: automatic-action/MDN-sent-automatically; deleted/error, expired\nError: e1\nFailure: disk full\nWarning: w1\nX-Note: n\n",
			nil,
			func(m *mail.DispositionNotification) bool {
				return slices.Equal(m.Disposition.Modifiers, []string{"error", "expired"}) && slices.Equal(m.Errors, []string{"e1"}) &&
					slices.Equal(m.Failures, []string{"disk full"}) && slices.Equal(m.Warnings, []string{"w1"}) && len(m.Unknown) == 1
			}},
		{"blank line and no final recipient", "Reporting-UA: ua.example.com; Mailer 1.0\n\nOriginal-Recipient: rfc822;a@example.com\nDisposition: manual-action/MDN-sent-manually; displayed\n",
			[]string{"report.unexpected_blank_line", "report.missing_field"},
			func(m *mail.DispositionNotification) bool {
				return m.UAName == "ua.example.com" && m.UAProduct == "Mailer 1.0" && m.OriginalRecipient.Value == "a@example.com"
			}},
		{"unknown mode and type", fr + "Disposition: manual/sent; read\n",
			[]string{"report.invalid_field"},
			func(m *mail.DispositionNotification) bool { return m.Disposition.Type == "read" }},
	}
	for _, tc := range tests {
		m := mail.ParseDispositionNotification([]byte(tc.body))
		rptWant(t, tc.name, m.Diagnostics, tc.codes...)
		if !tc.check(m) {
			t.Errorf("%s: %+v", tc.name, m)
		}
	}
}

func TestParseFeedbackReport_malformed(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		codes []string
		check func(*mail.FeedbackReport) bool
	}{
		{"bracketed IP and the draft date name", "Feedback-Type: Abuse\nUser-Agent: X/1\nVersion: 1\nSource-IP: [192.0.2.1]\nReceived-Date: Tue, 8 Mar 2005 14:00:00 -0500\n",
			[]string{"report.invalid_field"},
			func(f *mail.FeedbackReport) bool {
				return f.FeedbackType == "abuse" && f.SourceAddr.String() == "192.0.2.1" && f.ArrivalDate.Valid
			}},
		{"missing and invalid fields", "Feedback-Type: spam-ish\nSource-IP: nope\nIncidents: many\nReported-URI: <http://example.net/a>\nReported-URI: http://example.net/b\n",
			[]string{"report.invalid_field", "report.missing_field"},
			func(f *mail.FeedbackReport) bool {
				return f.SourceIP == "nope" && !f.SourceAddr.IsValid() && f.Incidents == 0 &&
					slices.Equal(f.ReportedURIs, []string{"http://example.net/a", "http://example.net/b"})
			}},
		{"duplicate and an unindented continuation", "Feedback-Type: abuse\nFeedback-Type: fraud\nUser-Agent: X/1\nVersion: 0.1\nIncidents: 3\nAuthentication-Results: mx.example.com;\nspf=fail smtp.mailfrom=example.net\n",
			[]string{"report.duplicate_field", "report.invalid_field", "report.unindented_continuation"},
			func(f *mail.FeedbackReport) bool {
				ar := f.AuthResults()
				return f.FeedbackType == "abuse" && f.Incidents == 3 && len(ar) == 1 && len(ar[0].Methods) == 1 && ar[0].Methods[0].Method == "spf"
			}},
	}
	for _, tc := range tests {
		f := mail.ParseFeedbackReport([]byte(tc.body))
		rptWant(t, tc.name, f.Diagnostics, tc.codes...)
		if !tc.check(f) {
			t.Errorf("%s: %+v", tc.name, f)
		}
	}
}

func TestNode_Report(t *testing.T) {
	report := func(params, part string) string {
		return "Content-Type: multipart/report; boundary=S" + params + "\r\n\r\n" +
			"--S\r\nContent-Type: text/plain\r\n\r\nNot delivered.\r\n" +
			"--S\r\n" + part + "\r\n--S--\r\n"
	}
	const dsn = "Reporting-MTA: dns; mx.example.com\r\n\r\nFinal-Recipient: rfc822; a@example.com\r\nAction: failed\r\nStatus: 5.1.1\r\n"
	tests := []struct {
		name  string
		text  string
		opts  *mail.TreeOptions
		kind  mail.ReportKind
		codes []string
		check func(*mail.Report) bool
	}{
		{"report-type disagrees", report("; report-type=delivery-status", "Content-Type: message/feedback-report\r\n\r\nFeedback-Type: abuse\r\nUser-Agent: X/1\r\nVersion: 1\r\n"),
			nil, mail.ReportFeedback, []string{"report.type_mismatch"},
			func(r *mail.Report) bool { return r.Feedback.FeedbackType == "abuse" && r.Description != nil }},
		{"report part labelled text/plain", report("; report-type=delivery-status", "Content-Type: text/plain\r\n\r\n"+dsn),
			nil, mail.ReportDeliveryStatus, []string{"report.type_mismatch"},
			func(r *mail.Report) bool { return r.Delivery.Recipients[0].Status.String() == "5.1.1" }},
		{"report kind read from its fields", report("; report-type=x-odd", "Content-Type: application/octet-stream\r\n\r\nFeedback-Type: fraud\r\nUser-Agent: X/1\r\nVersion: 1\r\n"),
			nil, mail.ReportFeedback, []string{"report.type_mismatch"},
			func(r *mail.Report) bool { return r.Feedback.FeedbackType == "fraud" }},
		{"fields in the part header", report("; report-type=delivery-status", "Content-Type: message/delivery-status\r\nReporting-MTA: dns; mx.example.com\r\n\r\nFinal-Recipient: rfc822; a@example.com\r\nAction: failed\r\nStatus: 5.1.1\r\n"),
			nil, mail.ReportDeliveryStatus, []string{"report.misplaced_field"},
			func(r *mail.Report) bool {
				return r.Delivery.ReportingMTA.Value == "mx.example.com" && len(r.Delivery.Recipients) == 1 &&
					reflect.DeepEqual(r.Diagnostics[0].Path, []int{1})
			}},
		{"truncated part", report("; report-type=delivery-status", "Content-Type: message/delivery-status\r\n\r\n"+dsn),
			&mail.TreeOptions{MaxBodyBytes: 40}, mail.ReportDeliveryStatus, []string{"report.truncated", "report.missing_field"},
			func(r *mail.Report) bool { return r.Delivery.ReportingMTA.Value == "mx.example.com" }},
		{"no report part", "Content-Type: multipart/report; boundary=S; report-type=delivery-status\r\n\r\n--S\r\n\r\nonly text\r\n--S--\r\n",
			nil, mail.ReportUnknown, []string{"report.missing_part"},
			func(r *mail.Report) bool { return r.Part == nil && r.Description != nil }},
		{"global report with returned headers", report("; report-type=global-delivery-status",
			"Content-Type: message/global-delivery-status\r\n\r\nReporting-MTA: dns; mx.example.com\r\n\r\nFinal-Recipient: utf-8; 사용자@example.com\r\nAction: failed\r\nStatus: 5.1.1\r\n"+
				"--S\r\nContent-Type: text/rfc822-headers\r\n\r\nSubject: original\r\nMessage-Id: <o@example.com>\r\n"),
			nil, mail.ReportDeliveryStatus, nil,
			func(r *mail.Report) bool {
				return r.Delivery.Recipients[0].Address() == "사용자@example.com" && r.OriginalHeader != nil && r.OriginalHeader.Get("Subject") == "original"
			}},
		{"descended original", rptRFC3464Simple, &mail.TreeOptions{DescendMessages: true}, mail.ReportDeliveryStatus, nil,
			func(r *mail.Report) bool {
				return r.OriginalHeader != nil && r.OriginalHeader.Get("Subject") == "Re: mime info"
			}},
	}
	for _, tc := range tests {
		tree := rptTree(t, tc.text, tc.opts)
		r := tree.Root.Report()
		if r == nil || r.Kind != tc.kind {
			t.Errorf("%s: report %+v", tc.name, r)
			continue
		}
		rptWant(t, tc.name, r.Diagnostics, tc.codes...)
		if !tc.check(r) {
			t.Errorf("%s: %+v", tc.name, r)
		}
		// Asked of the report part, the same report.
		if r.Part != nil && strings.HasPrefix(r.Part.MediaType, "message/") {
			if p := r.Part.Report(); p == nil || p.Container != tree.Root || p.Kind != r.Kind {
				t.Errorf("%s: Report of the part %+v", tc.name, p)
			}
		}
	}
	if r := rptTree(t, "Subject: plain\r\n\r\nbody\r\n", nil).Root.Report(); r != nil {
		t.Errorf("plain message: report %+v", r)
	}
	// A report part read on its own, outside a multipart/report.
	r := rptTree(t, "Content-Type: message/delivery-status\r\n\r\n"+dsn, nil).Root.Report()
	if r == nil || r.Container != nil || r.Delivery == nil || len(r.Delivery.Recipients) != 1 {
		t.Errorf("lone part: report %+v", r)
	}
}

// FuzzReportParsers checks that no input makes a report parser panic or
// return more diagnostics than its bound.
func FuzzReportParsers(f *testing.F) {
	for _, s := range []string{
		rptRFC3464Body,
		"Status: 5.1.1 (unclosed\nAction: (\n\tcontinued\nFinal-Recipient: ;\n",
		"Disposition: ;/,\nReporting-UA: ;\nOriginal-Message-ID: <\n",
		"Feedback-Type: abuse\n\n\nSource-IP: [::1]\nIncidents: -1\n",
		"\xef\xbb\xbfReporting-MTA : x400: \xff\r\rFinal-Recipient: rfc822; <>\r\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		lists := [][]message.Diagnostic{
			mail.ParseDeliveryStatus(data).Diagnostics,
			mail.ParseDispositionNotification(data).Diagnostics,
			mail.ParseFeedbackReport(data).Diagnostics,
		}
		for _, list := range lists {
			if len(list) > 128 {
				t.Fatalf("%d diagnostics", len(list))
			}
		}
	})
}
