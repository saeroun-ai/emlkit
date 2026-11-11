package icalendar

// Default budgets. A field of Limits that is zero or negative takes its
// default; there is no unlimited setting.
const (
	DefaultMaxInputBytes      = 4 << 20
	DefaultMaxLineBytes       = 1 << 20
	DefaultMaxLines           = 100000
	DefaultMaxDepth           = 8
	DefaultMaxComponents      = 5000
	DefaultMaxProperties      = 1000
	DefaultMaxParams          = 64
	DefaultMaxAttendees       = 1000
	DefaultMaxAttachmentBytes = 1 << 20
	DefaultMaxIssues          = 256
)

// maxZones is how many distinct TZIDs one parse resolves, and maxZoneLoads
// how many time.LoadLocation calls it makes; both bound work an input can
// ask of the host's zone database.
const (
	maxZones     = 64
	maxZoneLoads = 64
)

// Limits bounds what one parse reads and keeps. Hitting a budget keeps what
// was read before it and records an issue with a "limit." code.
type Limits struct {
	// MaxInputBytes is how much input is read; the rest is ignored.
	MaxInputBytes int
	// MaxLineBytes is how long one content line may be after unfolding. The
	// rest of a longer line is dropped and its Property is Truncated.
	MaxLineBytes int
	// MaxLines is how many content lines are read.
	MaxLines int
	// MaxDepth is how deeply components nest, the VCALENDAR being 1. A deeper
	// component is skipped with all it contains.
	MaxDepth int
	// MaxComponents is how many components are kept; later ones are skipped.
	MaxComponents int
	// MaxProperties is how many properties one component keeps.
	MaxProperties int
	// MaxParams is how many parameters one property keeps, and how many
	// values one parameter keeps.
	MaxParams int
	// MaxAttendees is how many attendees one event keeps.
	MaxAttendees int
	// MaxAttachmentBytes is how many decoded bytes of inline attachments one
	// calendar keeps in all.
	MaxAttachmentBytes int
	// MaxIssues is how many issues are recorded; one more says that the rest
	// were not.
	MaxIssues int
}

// DefaultLimits returns the budgets used for zero fields.
func DefaultLimits() Limits {
	return Limits{
		MaxInputBytes:      DefaultMaxInputBytes,
		MaxLineBytes:       DefaultMaxLineBytes,
		MaxLines:           DefaultMaxLines,
		MaxDepth:           DefaultMaxDepth,
		MaxComponents:      DefaultMaxComponents,
		MaxProperties:      DefaultMaxProperties,
		MaxParams:          DefaultMaxParams,
		MaxAttendees:       DefaultMaxAttendees,
		MaxAttachmentBytes: DefaultMaxAttachmentBytes,
		MaxIssues:          DefaultMaxIssues,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	set := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	set(&l.MaxInputBytes, d.MaxInputBytes)
	set(&l.MaxLineBytes, d.MaxLineBytes)
	set(&l.MaxLines, d.MaxLines)
	set(&l.MaxDepth, d.MaxDepth)
	set(&l.MaxComponents, d.MaxComponents)
	set(&l.MaxProperties, d.MaxProperties)
	set(&l.MaxParams, d.MaxParams)
	set(&l.MaxAttendees, d.MaxAttendees)
	set(&l.MaxAttachmentBytes, d.MaxAttachmentBytes)
	set(&l.MaxIssues, d.MaxIssues)
	return l
}

// Options configures a parse. The zero value and nil are the defaults.
type Options struct {
	Limits Limits
	// ContentTypeMethod is the method parameter of the text/calendar
	// Content-Type the object came in (RFC 6047, section 2.4), if any. It
	// stands in for a missing METHOD, and a METHOD that differs from it is
	// reported.
	ContentTypeMethod string
}

func (o *Options) limits() Limits {
	if o == nil {
		return DefaultLimits()
	}
	return o.Limits.withDefaults()
}
