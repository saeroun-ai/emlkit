package icalendar

import "strconv"

// An Intent is what a scheduling message asks for (RFC 5546).
type Intent uint8

const (
	// IntentUnknown is a METHOD this package does not know.
	IntentUnknown Intent = iota
	// IntentPublish is METHOD:PUBLISH, or no METHOD: a calendar to look at,
	// with no reply expected.
	IntentPublish
	// IntentRequest is an invitation: METHOD:REQUEST for an event the caller
	// does not know, or does not know an older revision of.
	IntentRequest
	// IntentUpdate is METHOD:REQUEST for a known event, newer than the
	// caller's copy (Compare).
	IntentUpdate
	// IntentCancel is METHOD:CANCEL, or a REQUEST or PUBLISH whose event has
	// STATUS:CANCELLED.
	IntentCancel
	// IntentReply is METHOD:REPLY: an attendee's answer.
	IntentReply
	// IntentCounter is METHOD:COUNTER: an attendee proposes a change.
	IntentCounter
	// IntentDeclineCounter is METHOD:DECLINECOUNTER.
	IntentDeclineCounter
	// IntentRefresh is METHOD:REFRESH: an attendee asks for the latest
	// revision.
	IntentRefresh
	// IntentAdd is METHOD:ADD: instances added to a recurring event.
	IntentAdd
)

func (i Intent) String() string {
	switch i {
	case IntentUnknown:
		return "unknown"
	case IntentPublish:
		return "publish"
	case IntentRequest:
		return "request"
	case IntentUpdate:
		return "update"
	case IntentCancel:
		return "cancel"
	case IntentReply:
		return "reply"
	case IntentCounter:
		return "counter"
	case IntentDeclineCounter:
		return "declinecounter"
	case IntentRefresh:
		return "refresh"
	case IntentAdd:
		return "add"
	}
	return "Intent(" + strconv.Itoa(int(i)) + ")"
}

// A Classification is what Classify found a calendar to mean.
type Classification struct {
	Intent Intent
	// Method is the calendar's method.
	Method string
	// Event is the event the message is about: the first without a
	// RECURRENCE-ID, else the first. Nil when there is no event; UID and
	// Sequence are then a to-do's, if any.
	Event    *Event
	UID      string
	Sequence int
	// Attendee is, for a REPLY or COUNTER, the attendee who answers: the
	// first whose PARTSTAT is set to anything but NEEDS-ACTION, else the
	// first. PartStat is its participation status.
	Attendee *Attendee
	PartStat string
	// Known is the caller's copy of the event, when the lookup found one.
	// Stale reports a REQUEST or CANCEL that is not newer than it: a repeat
	// or an out-of-order message.
	Known *Event
	Stale bool
	// Reason says, for a person, why the intent was chosen.
	Reason string
}

// Classify says what a calendar asks for. known, when not nil, looks up the
// caller's copy of an event by UID, and returns nil for an unknown one; it
// tells an update from a new invitation.
func Classify(cal *Calendar, known func(uid string) *Event) Classification {
	c := Classification{Method: cal.Method}
	for _, e := range cal.Events {
		if e.RecurrenceID.IsZero() {
			c.Event = e
			break
		}
	}
	if c.Event == nil && len(cal.Events) > 0 {
		c.Event = cal.Events[0]
	}
	cancelled := false
	switch {
	case c.Event != nil:
		c.UID, c.Sequence = c.Event.UID, c.Event.Sequence
		cancelled = c.Event.Status == "CANCELLED"
	case len(cal.Todos) > 0:
		c.UID, c.Sequence = cal.Todos[0].UID, cal.Todos[0].Sequence
		cancelled = cal.Todos[0].Status == "CANCELLED"
	}
	if known != nil && c.Event != nil && c.UID != "" {
		c.Known = known(c.UID)
	}
	newer := func() bool {
		if c.Known == nil {
			return true
		}
		order, ok := Compare(c.Event, c.Known)
		c.Stale = ok && order <= 0
		return ok && order > 0
	}

	switch c.Method {
	case "REQUEST":
		switch {
		case cancelled:
			c.Intent, c.Reason = IntentCancel, "REQUEST with STATUS:CANCELLED"
			newer()
		case c.Known == nil:
			c.Intent, c.Reason = IntentRequest, "REQUEST for an unknown event"
		case newer():
			c.Intent, c.Reason = IntentUpdate, "REQUEST newer than the known event"
		default:
			c.Intent, c.Reason = IntentRequest, "REQUEST not newer than the known event"
		}
	case "CANCEL":
		c.Intent, c.Reason = IntentCancel, "CANCEL"
		newer()
	case "REPLY", "COUNTER":
		c.Intent, c.Reason = IntentReply, "REPLY"
		if c.Method == "COUNTER" {
			c.Intent, c.Reason = IntentCounter, "COUNTER"
		}
		if c.Event != nil {
			c.Attendee = replier(c.Event)
		}
		if c.Attendee != nil {
			c.PartStat = c.Attendee.PartStat
		}
	case "DECLINECOUNTER":
		c.Intent, c.Reason = IntentDeclineCounter, "DECLINECOUNTER"
	case "REFRESH":
		c.Intent, c.Reason = IntentRefresh, "REFRESH"
	case "ADD":
		c.Intent, c.Reason = IntentAdd, "ADD"
	case "PUBLISH", "":
		c.Intent, c.Reason = IntentPublish, "PUBLISH"
		if c.Method == "" {
			c.Reason = "no METHOD"
		}
		if cancelled {
			c.Intent, c.Reason = IntentCancel, c.Reason+" with STATUS:CANCELLED"
		}
	default:
		c.Intent, c.Reason = IntentUnknown, "unknown METHOD"
	}
	return c
}

func replier(e *Event) *Attendee {
	for i := range e.Attendees {
		if s := e.Attendees[i].PartStat; s != "" && s != "NEEDS-ACTION" {
			return &e.Attendees[i]
		}
	}
	if len(e.Attendees) > 0 {
		return &e.Attendees[0]
	}
	return nil
}

// Compare orders two revisions of one event as RFC 5546, section 2.1.5,
// does: the higher SEQUENCE is newer and, at equal SEQUENCE, the later
// DTSTAMP. It returns 1 when a is newer than b, -1 when it is older and 0
// when neither is, which includes DTSTAMPs that cannot be compared. ok is
// false, and order 0, when a and b are not revisions of one event: their
// UIDs differ, or their RECURRENCE-IDs do.
func Compare(a, b *Event) (order int, ok bool) {
	if a == nil || b == nil || a.UID != b.UID || !sameInstant(a.RecurrenceID, b.RecurrenceID) {
		return 0, false
	}
	switch {
	case a.Sequence > b.Sequence:
		return 1, true
	case a.Sequence < b.Sequence:
		return -1, true
	}
	x, y := a.DTStamp, b.DTStamp
	var after, before bool
	switch {
	case x.Known() && y.Known():
		after, before = x.Time.After(y.Time), x.Time.Before(y.Time)
	case x.Kind == y.Kind && (x.Kind == TimeFloating || x.Kind == TimeDate):
		after, before = x.Wall.After(y.Wall), x.Wall.Before(y.Wall)
	}
	switch {
	case after:
		return 1, true
	case before:
		return -1, true
	}
	return 0, true
}

// sameInstant reports whether two RECURRENCE-IDs name the same instance.
func sameInstant(x, y DateTime) bool {
	switch {
	case x.IsZero() || y.IsZero():
		return x.IsZero() && y.IsZero()
	case x.Known() && y.Known():
		return x.Time.Equal(y.Time)
	}
	return x.Kind == y.Kind && x.Wall.Equal(y.Wall) && x.TZID == y.TZID
}
