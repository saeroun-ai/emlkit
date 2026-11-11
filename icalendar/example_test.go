package icalendar_test

import (
	"fmt"
	"strings"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/icalendar"
	"github.com/saeroun-ai/emlkit/mail"
)

// A text/calendar part of a message, found with mail.ReadTree.
func Example_mail() {
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//EN\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\nUID:20261120-kickoff@example.com\r\nSEQUENCE:0\r\n" +
		"DTSTAMP:20261111T010000Z\r\nDTSTART;TZID=Asia/Seoul:20261120T140000\r\n" +
		"SUMMARY:분기 계획 회의\r\nORGANIZER;CN=김민수:mailto:minsu.kim@example.com\r\n" +
		"ATTENDEE;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:jieun.lee@example.com\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	msg := "From: minsu.kim@example.com\r\nTo: jieun.lee@example.com\r\n" +
		"Subject: Invitation\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nYou are invited.\r\n" +
		"--b\r\nContent-Type: text/calendar; charset=UTF-8; method=REQUEST\r\n" +
		"Content-Transfer-Encoding: 8bit\r\n\r\n" + ics + "--b--\r\n"

	_, root, err := message.Parse(strings.NewReader(msg), nil)
	if root == nil {
		fmt.Println(err)
		return
	}
	tree, err := mail.ReadTree(root, nil)
	if tree.Root == nil {
		fmt.Println(err)
		return
	}
	tree.Root.Walk(func(n *mail.Node) bool {
		if n.MediaType != "text/calendar" {
			return true
		}
		cal := icalendar.ParseBytes(n.Data, &icalendar.Options{ContentTypeMethod: n.Params["method"]})
		c := icalendar.Classify(cal, nil)
		e := c.Event
		fmt.Println(c.Intent, e.UID, e.Summary, e.Start.Time.UTC().Format("2006-01-02T15:04Z"), e.Organizer.Address, len(cal.Issues))
		return true
	})
	// Output:
	// request 20261120-kickoff@example.com 분기 계획 회의 2026-11-20T05:00Z minsu.kim@example.com 0
}
