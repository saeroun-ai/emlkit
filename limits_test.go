package message_test

import (
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/textproto"
)

// wantDefaults is the documented default of every budget, written out here
// rather than read from the implementation: these values are what the library
// promises a caller, so a test that shared the constants could not notice one
// changing.
//
// They are finite operational starting points, not a conformance boundary and
// not a measurement of anyone's caps.
var wantDefaults = map[string]int64{
	"MaxInputBytes":        256 << 20, // 256 MiB
	"MaxHeaderBytes":       1 << 20,   // 1 MiB, the long-standing header limit
	"MaxHeaderFieldBytes":  64 << 10,  // 64 KiB
	"MaxHeaderCount":       1024,
	"MaxLineBytes":         64 << 10, // 64 KiB
	"MaxTotalHeaderBytes":  32 << 20, // 32 MiB
	"MaxTotalHeaderCount":  100000,
	"MaxParameterCount":    256,
	"MaxParameterSegments": 256,
	"MaxMIMEDepth":         100,
	"MaxMessageDepth":      32,
	"MaxPartCount":         10000,
	"MaxPreambleBytes":     1 << 20,   // 1 MiB
	"MaxEpilogueBytes":     1 << 20,   // 1 MiB
	"MaxEncodedBodyBytes":  128 << 20, // 128 MiB
	"MaxDecodedBodyBytes":  128 << 20, // 128 MiB
	"MaxTotalDecodedBytes": 512 << 20, // 512 MiB
	"MaxDiagnosticCount":   1000,
	"MaxMemorySpoolBytes":  8 << 20, // 8 MiB
	"MaxDiskSpoolBytes":    1 << 30, // 1 GiB
}

// overrideValue is the value the per-field tests write into one budget. It is
// not equal to any default, so a field that was silently defaulted cannot pass,
// and it is large enough to be a workable header budget for the small fixtures
// below.
const overrideValue int64 = 1 << 19 // 512 KiB

// limitFieldNames returns the exported fields of Limits in declaration order,
// having first checked that the set of them is exactly what wantDefaults
// describes. A budget added to Limits without a documented default fails here.
func limitFieldNames(t *testing.T) []string {
	t.Helper()

	typ := reflect.TypeOf(message.Limits{})
	names := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.Int64 {
			t.Errorf("Limits.%s is %s, want int64: every budget is an int64", f.Name, f.Type)
		}
		if _, ok := wantDefaults[f.Name]; !ok {
			t.Errorf("Limits.%s has no documented default", f.Name)
		}
		names = append(names, f.Name)
	}
	if len(names) != len(wantDefaults) {
		t.Errorf("Limits has %d budgets, %d defaults are documented", len(names), len(wantDefaults))
	}
	if len(names) != 20 {
		t.Errorf("Limits has %d budgets, want the 20 the roadmap reserves", len(names))
	}
	return names
}

// limitValue reads one budget by name.
func limitValue(t *testing.T, l message.Limits, name string) int64 {
	t.Helper()
	v := reflect.ValueOf(l).FieldByName(name)
	if !v.IsValid() {
		t.Fatalf("Limits has no field %s", name)
	}
	return v.Int()
}

// withLimit returns l with one budget set by name.
func withLimit(t *testing.T, l message.Limits, name string, value int64) message.Limits {
	t.Helper()
	v := reflect.ValueOf(&l).Elem().FieldByName(name)
	if !v.IsValid() {
		t.Fatalf("Limits has no field %s", name)
	}
	v.SetInt(value)
	return l
}

// limitsMessage is a small conformant message: these tests are about
// configuration, so nothing about the input should be interesting.
const limitsMessage = "Content-Type: text/plain\r\n" +
	"\r\n" +
	"body\r\n"

// TestDefaultLimits pins every documented default, that each is a finite
// positive budget rather than a disabled one, and that the value returned is
// fresh.
func TestDefaultLimits(t *testing.T) {
	l := message.DefaultLimits()

	for _, name := range limitFieldNames(t) {
		got, want := limitValue(t, l, name), wantDefaults[name]
		if got != want {
			t.Errorf("DefaultLimits().%s = %d, want %d", name, got, want)
		}
		if got <= 0 {
			t.Errorf("DefaultLimits().%s = %d, want a finite positive budget: no budget is disabled by default",
				name, got)
		}
	}

	// The result is a value, so a caller may adjust it without disturbing the
	// next caller's defaults.
	l.MaxInputBytes = 1
	if message.DefaultLimits().MaxInputBytes == 1 {
		t.Errorf("modifying the result of DefaultLimits changed the defaults")
	}
}

// TestDefaultLimits_headerBytesUnchanged pins the compatibility promise of the
// defaults: the header budget is still 1 MiB, and a default Parse accepts and
// rejects exactly the message headers ReadWithOptions always has.
//
// What the two say about a header they both reject is no longer the same, and
// that is this item's doing: ReadWithOptions keeps the bare error it always
// returned, while Parse reports the budget failure as a parse failure, with the
// diagnostic that names it. The decision is what has to match, not the wording.
func TestDefaultLimits_headerBytesUnchanged(t *testing.T) {
	if got, want := message.DefaultLimits().MaxHeaderBytes, int64(1<<20); got != want {
		t.Fatalf("default MaxHeaderBytes = %d, want the existing %d", got, want)
	}

	for _, tc := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{"well under the limit", 64 << 10, false},
		{"well over the limit", 4 << 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacy, legacyErr := message.ReadWithOptions(strings.NewReader(paddedMessage(tc.size)), nil)
			session, parsed, parseErr := message.Parse(strings.NewReader(paddedMessage(tc.size)), nil)

			if (legacyErr != nil) != tc.wantErr {
				t.Fatalf("ReadWithOptions() = %v, want an error: %v", legacyErr, tc.wantErr)
			}
			if (parseErr != nil) != tc.wantErr {
				t.Fatalf("Parse() = %v, want an error: %v", parseErr, tc.wantErr)
			}
			if (legacy == nil) != (parsed == nil) {
				t.Errorf("ReadWithOptions entity = %v but Parse entity = %v", legacy, parsed)
			}
			if !tc.wantErr {
				// A padding line past 998 bytes is an observation, no budget's.
				if got := session.Diagnostics(); got != nil && (len(got) != 1 || got[0].Code != message.CodeHeaderOverlongLine) {
					t.Errorf("Diagnostics() = %v, want none for a header within the budget", codesOf(got))
				}
				return
			}

			// Both report the same underlying budget failure.
			if !errors.Is(legacyErr, textproto.ErrHeaderTooBig) {
				t.Errorf("ReadWithOptions() = %v, want the header budget failure", legacyErr)
			}
			if !errors.Is(parseErr, textproto.ErrHeaderTooBig) {
				t.Errorf("Parse() = %v, want the header budget failure", parseErr)
			}
			// Only Parse reports it as a failure of the parse.
			if message.IsFatal(legacyErr) {
				t.Errorf("ReadWithOptions() = %v, want the ordinary error it always returned", legacyErr)
			}
			if !message.IsFatal(parseErr) {
				t.Errorf("Parse() = %v, want a FatalError", parseErr)
			}
			if got := codesOf(session.Diagnostics()); !reflect.DeepEqual(got, []message.Code{message.CodeLimitHeaderBytes}) {
				t.Errorf("Diagnostics() = %v, want exactly the header budget diagnostic", got)
			}
		})
	}
}

// TestParse_headerBytesBoundaryMatchesLegacy pins the message header budget byte
// by byte. Rather than asserting where the boundary falls, it sweeps the budget
// across it and requires Parse, through either spelling, to accept and reject
// exactly the headers ReadWithOptions does with the same number. A budget
// shifted by a byte in either direction fails here, and so does one enforced at
// a different number from the one the session reports.
//
// The two disagree about nothing but wording. Parse reports a refused header as
// a failure of the parse — a FatalError carrying one limit.header_bytes
// diagnostic — while ReadWithOptions returns the bare error it always has; both
// wrap the same underlying budget failure, which is what errors.Is finds.
func TestParse_headerBytesBoundaryMatchesLegacy(t *testing.T) {
	const fixture = "Content-Type: text/plain\r\n" +
		"X-Pad: abcdefgh\r\n" +
		"\r\n" +
		"body\r\n"
	headerBytes := int64(strings.Index(fixture, "\r\n\r\n") + len("\r\n\r\n"))

	for n := int64(1); n <= headerBytes+8; n++ {
		legacy, legacyErr := message.ReadWithOptions(
			strings.NewReader(fixture), &message.ReadOptions{MaxHeaderBytes: n})

		if message.IsFatal(legacyErr) {
			t.Errorf("MaxHeaderBytes = %d: ReadWithOptions() = %v, want the ordinary error it always returned",
				n, legacyErr)
		}

		for _, tc := range []struct {
			name string
			opts *message.ParseOptions
		}{
			{"limits", &message.ParseOptions{Limits: message.Limits{MaxHeaderBytes: n}}},
			{"read options", &message.ParseOptions{ReadOptions: message.ReadOptions{MaxHeaderBytes: n}}},
		} {
			session, parsed, parseErr := message.Parse(strings.NewReader(fixture), tc.opts)

			if (legacyErr != nil) != (parseErr != nil) {
				t.Errorf("MaxHeaderBytes = %d via %s: ReadWithOptions() = %v but Parse() = %v",
					n, tc.name, legacyErr, parseErr)
			}
			if (legacy == nil) != (parsed == nil) {
				t.Errorf("MaxHeaderBytes = %d via %s: ReadWithOptions entity = %v but Parse entity = %v",
					n, tc.name, legacy, parsed)
			}

			wantCodes := []message.Code(nil)
			if parseErr != nil {
				wantCodes = []message.Code{message.CodeLimitHeaderBytes}

				if !errors.Is(parseErr, textproto.ErrHeaderTooBig) {
					t.Errorf("MaxHeaderBytes = %d via %s: Parse() = %v, want the header budget failure",
						n, tc.name, parseErr)
				}
				if !message.IsFatal(parseErr) {
					t.Errorf("MaxHeaderBytes = %d via %s: Parse() = %v, want a FatalError", n, tc.name, parseErr)
				}
			}
			if got := codesOf(session.Diagnostics()); !reflect.DeepEqual(got, wantCodes) {
				t.Errorf("MaxHeaderBytes = %d via %s: Diagnostics() = %v, want %v", n, tc.name, got, wantCodes)
			}

			// The budget the session reports is the budget that was enforced.
			if got := session.Limits().MaxHeaderBytes; got != n {
				t.Errorf("MaxHeaderBytes = %d via %s: Limits().MaxHeaderBytes = %d", n, tc.name, got)
			}
		}
	}
}

// paddedMessage returns a message whose header block is about size bytes and
// whose only oversized quantity is that byte count.
//
// The padding field is deliberately long. A block padded out of many small
// fields is also a block with a great many fields, and would run into the field
// count budget on the way to a megabyte; padding it out of a few large ones
// leaves it comfortably inside every other default — 512 fields and 8 KiB lines
// for a 4 MiB block, against defaults of 1024 fields and 64 KiB lines and
// fields — so that what a test built on it observes is the byte budget and
// nothing else.
func paddedMessage(size int) string {
	var b strings.Builder
	b.WriteString("Content-Type: text/plain\r\n")
	const field = "X-Pad: "
	const pad = 8 << 10
	for b.Len() < size {
		b.WriteString(field)
		b.WriteString(strings.Repeat("x", pad))
		b.WriteString("\r\n")
	}
	b.WriteString("\r\nbody\r\n")
	return b.String()
}

// TestParse_limitsDefaultWhenUnset pins that an unset budget is the documented
// default, whether the caller passes nothing at all or a zero Limits.
func TestParse_limitsDefaultWhenUnset(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts *message.ParseOptions
	}{
		{"nil options", nil},
		{"zero options", &message.ParseOptions{}},
		{"zero limits", &message.ParseOptions{Limits: message.Limits{}}},
		{"defaults passed back in", &message.ParseOptions{Limits: message.DefaultLimits()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, e, err := message.Parse(strings.NewReader(limitsMessage), tc.opts)
			if err != nil || e == nil {
				t.Fatalf("Parse() = (%v, %v), want a usable entity", e, err)
			}
			if got := session.Limits(); got != message.DefaultLimits() {
				t.Errorf("Limits() =\n%+v\nwant the defaults\n%+v", got, message.DefaultLimits())
			}
		})
	}
}

// TestParse_limitsPerFieldOverride pins that each budget can be set on its own:
// the session reports the value that was asked for, and no other budget moves.
func TestParse_limitsPerFieldOverride(t *testing.T) {
	for _, name := range limitFieldNames(t) {
		t.Run(name, func(t *testing.T) {
			opts := &message.ParseOptions{
				Limits: withLimit(t, message.Limits{}, name, overrideValue),
			}

			session, e, err := message.Parse(strings.NewReader(limitsMessage), opts)
			if err != nil || e == nil {
				t.Fatalf("Parse() = (%v, %v), want a usable entity", e, err)
			}

			want := withLimit(t, message.DefaultLimits(), name, overrideValue)
			if got := session.Limits(); got != want {
				t.Errorf("Limits() =\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

// TestParse_limitsDisable pins that any single budget can be turned off, and
// that doing so leaves every other one at its default.
func TestParse_limitsDisable(t *testing.T) {
	for _, name := range limitFieldNames(t) {
		t.Run(name, func(t *testing.T) {
			opts := &message.ParseOptions{
				Limits: withLimit(t, message.Limits{}, name, message.LimitDisabled),
			}

			session, e, err := message.Parse(strings.NewReader(limitsMessage), opts)
			if err != nil || e == nil {
				t.Fatalf("Parse() = (%v, %v), want a usable entity", e, err)
			}

			want := withLimit(t, message.DefaultLimits(), name, message.LimitDisabled)
			if got := session.Limits(); got != want {
				t.Errorf("Limits() =\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

// TestParse_limitsDisableEverything pins that a caller may turn every budget
// off at once, which is the configuration a caller bounding its input some
// other way would use.
func TestParse_limitsDisableEverything(t *testing.T) {
	all := message.Limits{}
	for _, name := range limitFieldNames(t) {
		all = withLimit(t, all, name, message.LimitDisabled)
	}

	session, e, err := message.Parse(strings.NewReader(limitsMessage), &message.ParseOptions{Limits: all})
	if err != nil || e == nil {
		t.Fatalf("Parse() = (%v, %v), want a usable entity", e, err)
	}
	if got := session.Limits(); got != all {
		t.Errorf("Limits() =\n%+v\nwant every budget disabled\n%+v", got, all)
	}
}

// TestParse_limitsMaxInt64IsFinite pins that math.MaxInt64 is an ordinary
// budget and not a second spelling of LimitDisabled. The two are reported
// differently, because a later release enforces one and not the other.
func TestParse_limitsMaxInt64IsFinite(t *testing.T) {
	for _, name := range limitFieldNames(t) {
		opts := &message.ParseOptions{
			Limits: withLimit(t, message.Limits{}, name, math.MaxInt64),
		}
		session, _, err := message.Parse(strings.NewReader(limitsMessage), opts)
		if err != nil {
			t.Fatalf("%s = MaxInt64: Parse() = %v", name, err)
		}
		if got := limitValue(t, session.Limits(), name); got != math.MaxInt64 {
			t.Errorf("%s = %d, want MaxInt64 kept as a finite budget", name, got)
		}
	}
}

// TestParse_invalidLimits pins the whole contract for a configuration Parse
// cannot resolve. It is the caller's error, not the message's, so: no entity,
// no byte of input read, an error verifying ErrInvalidLimits that is not fatal,
// and a session that still exists, still reports the requested profile and
// policy, and holds no diagnostic — there is nothing about the message to
// report.
func TestParse_invalidLimits(t *testing.T) {
	for _, name := range limitFieldNames(t) {
		for _, value := range []int64{message.LimitDisabled - 1, -1000, math.MinInt64} {
			t.Run(name, func(t *testing.T) {
				opts := &message.ParseOptions{
					Profile:       message.ProfileForensic,
					Compatibility: message.CompatibilityAngus,
					Limits:        withLimit(t, message.Limits{}, name, value),
				}

				r := &untouchedReader{t: t, s: limitsMessage}
				session, e, err := message.Parse(r, opts)

				if !errors.Is(err, message.ErrInvalidLimits) {
					t.Fatalf("%s = %d: Parse() = %v, want one verifying ErrInvalidLimits", name, value, err)
				}
				if message.IsFatal(err) {
					t.Errorf("Parse() = %v, want a configuration error rather than a fatal one", err)
				}
				if e != nil {
					t.Errorf("Parse() returned an entity for an invalid configuration")
				}
				if r.reads != 0 {
					t.Errorf("Parse read the input %d times, want 0 for an invalid configuration", r.reads)
				}
				if session == nil {
					t.Fatal("Parse returned a nil session for an invalid configuration")
				}
				if got := session.Profile(); got != message.ProfileForensic {
					t.Errorf("Profile() = %v, want the requested %v", got, message.ProfileForensic)
				}
				if got := session.Compatibility(); got != message.CompatibilityAngus {
					t.Errorf("Compatibility() = %v, want the requested %v", got, message.CompatibilityAngus)
				}
				if got := session.Limits(); got != message.DefaultLimits() {
					t.Errorf("Limits() =\n%+v\nwant the defaults, since the requested budgets never resolved", got)
				}
				if got := session.Diagnostics(); got != nil {
					t.Errorf("Diagnostics() = %v, want none: a caller's configuration error is not a defect of the message",
						codesOf(got))
				}
				if !strings.Contains(err.Error(), name) {
					t.Errorf("Parse() = %q, want it to name the offending field %s", err, name)
				}
			})
		}
	}
}

// TestParse_headerBytesBridge pins that the two spellings of the header budget
// are one budget: whichever one is set is the one enforced, and the session
// reports it either way.
func TestParse_headerBytesBridge(t *testing.T) {
	// The fixture's header is a few kilobytes, so a budget of 128 bytes stops
	// the parse and a generous one does not.
	const small = 128
	fixture := paddedMessage(4 << 10)

	for _, tc := range []struct {
		name       string
		limits     message.Limits
		read       message.ReadOptions
		wantLimit  int64
		wantHeader bool // whether the header budget stops the parse
	}{
		{"neither set", message.Limits{}, message.ReadOptions{}, 1 << 20, false},
		{"limits only", message.Limits{MaxHeaderBytes: small}, message.ReadOptions{}, small, true},
		{"read options only", message.Limits{}, message.ReadOptions{MaxHeaderBytes: small}, small, true},
		{
			"both set and equal",
			message.Limits{MaxHeaderBytes: small},
			message.ReadOptions{MaxHeaderBytes: small},
			small, true,
		},
		{
			"both set and generous",
			message.Limits{MaxHeaderBytes: 1 << 20},
			message.ReadOptions{MaxHeaderBytes: 1 << 20},
			1 << 20, false,
		},
		{
			"disabled through limits",
			message.Limits{MaxHeaderBytes: message.LimitDisabled},
			message.ReadOptions{},
			message.LimitDisabled, false,
		},
		{
			"disabled through read options",
			message.Limits{},
			message.ReadOptions{MaxHeaderBytes: message.LimitDisabled},
			message.LimitDisabled, false,
		},
		{
			"disabled through both",
			message.Limits{MaxHeaderBytes: message.LimitDisabled},
			message.ReadOptions{MaxHeaderBytes: message.LimitDisabled},
			message.LimitDisabled, false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, e, err := message.Parse(strings.NewReader(fixture), &message.ParseOptions{
				Limits:      tc.limits,
				ReadOptions: tc.read,
			})

			if errors.Is(err, message.ErrInvalidLimits) {
				t.Fatalf("Parse() = %v, want the configuration accepted", err)
			}
			if tc.wantHeader {
				if err == nil || e != nil {
					t.Errorf("Parse() = (%v, %v), want the header budget to stop the parse", e, err)
				}
			} else if err != nil || e == nil {
				t.Errorf("Parse() = (%v, %v), want a usable entity", e, err)
			}

			if got := session.Limits().MaxHeaderBytes; got != tc.wantLimit {
				t.Errorf("Limits().MaxHeaderBytes = %d, want %d", got, tc.wantLimit)
			}
			// Nothing else moved.
			want := message.DefaultLimits()
			want.MaxHeaderBytes = tc.wantLimit
			if got := session.Limits(); got != want {
				t.Errorf("Limits() =\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

// TestParse_headerBytesConflict pins the case this bridge exists for. Two
// budgets for the same bytes that disagree are refused rather than silently
// resolved in favour of one of them: dropping a limit a caller asked for is the
// one outcome a safety budget must never have.
func TestParse_headerBytesConflict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits int64
		read   int64
	}{
		{"different finite budgets", 4096, 8192},
		{"finite against disabled", 4096, message.LimitDisabled},
		{"disabled against finite", message.LimitDisabled, 4096},
		{"the default against something else", 1 << 20, 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &untouchedReader{t: t, s: limitsMessage}
			session, e, err := message.Parse(r, &message.ParseOptions{
				Limits:      message.Limits{MaxHeaderBytes: tc.limits},
				ReadOptions: message.ReadOptions{MaxHeaderBytes: tc.read},
			})

			if !errors.Is(err, message.ErrInvalidLimits) {
				t.Fatalf("Parse() = %v, want one verifying ErrInvalidLimits", err)
			}
			if message.IsFatal(err) {
				t.Errorf("Parse() = %v, want a configuration error rather than a fatal one", err)
			}
			if e != nil {
				t.Errorf("Parse() returned an entity for a conflicting configuration")
			}
			if r.reads != 0 {
				t.Errorf("Parse read the input %d times, want 0", r.reads)
			}
			if session == nil {
				t.Fatal("Parse returned a nil session")
			}
			if got := session.Limits(); got != message.DefaultLimits() {
				t.Errorf("Limits() = %+v, want the defaults", got)
			}
			// The error has to say what disagreed, or the caller cannot fix it.
			if !strings.Contains(err.Error(), "MaxHeaderBytes") {
				t.Errorf("Parse() = %q, want it to name the conflicting budget", err)
			}
		})
	}
}

// TestParse_legacyHeaderBytesBelowDisabled pins the one place Parse departs
// from the legacy reading. ReadOptions documents any negative value as "no
// limit"; Parse resolves it like every other budget, so a value below
// LimitDisabled is a configuration error there rather than an accident that
// quietly removes a limit.
func TestParse_legacyHeaderBytesBelowDisabled(t *testing.T) {
	for _, value := range []int64{-2, -1000, math.MinInt64} {
		r := &untouchedReader{t: t, s: limitsMessage}
		session, e, err := message.Parse(r, &message.ParseOptions{
			ReadOptions: message.ReadOptions{MaxHeaderBytes: value},
		})
		if !errors.Is(err, message.ErrInvalidLimits) {
			t.Fatalf("ReadOptions.MaxHeaderBytes = %d: Parse() = %v, want ErrInvalidLimits", value, err)
		}
		if e != nil || session == nil || r.reads != 0 {
			t.Errorf("ReadOptions.MaxHeaderBytes = %d: Parse() = (%v, %v) after %d reads, want no entity and no read",
				value, e, err, r.reads)
		}
	}
}

// TestReadWithOptions_legacyHeaderBytesBelowDisabled pins the other half: the
// entry points that resolve nothing keep their long-standing behavior exactly,
// so an existing caller passing a negative value still gets no limit at all.
func TestReadWithOptions_legacyHeaderBytesBelowDisabled(t *testing.T) {
	// A header far past the 1 MiB default, so that "no limit" is observable.
	big := paddedMessage(2 << 20)

	for _, value := range []int64{-1, -2, -1000, math.MinInt64} {
		e, err := message.ReadWithOptions(strings.NewReader(big), &message.ReadOptions{MaxHeaderBytes: value})
		if err != nil || e == nil {
			t.Errorf("MaxHeaderBytes = %d: ReadWithOptions() = (%v, %v), want the legacy unlimited behavior",
				value, e, err)
		}
	}

	// And the default still applies when nothing is asked for.
	if _, err := message.ReadWithOptions(strings.NewReader(big), nil); err == nil {
		t.Errorf("ReadWithOptions() with default options accepted a 2 MiB header")
	}
}

// TestParseSession_limits pins the accessor: a nil session and the zero session
// report the defaults, and what it returns is a copy a caller may keep.
func TestParseSession_limits(t *testing.T) {
	var nilSession *message.ParseSession
	if got := nilSession.Limits(); got != message.DefaultLimits() {
		t.Errorf("nil session Limits() =\n%+v\nwant the defaults", got)
	}

	zero := &message.ParseSession{}
	if got := zero.Limits(); got != message.DefaultLimits() {
		t.Errorf("zero session Limits() =\n%+v\nwant the defaults", got)
	}
	// The zero session's other accessors keep reporting their own defaults.
	if got := zero.Profile(); got != message.ProfileTolerant {
		t.Errorf("zero session Profile() = %v, want %v", got, message.ProfileTolerant)
	}
	if got := zero.Compatibility(); got != message.CompatibilityConservative {
		t.Errorf("zero session Compatibility() = %v, want %v", got, message.CompatibilityConservative)
	}

	// A caller may modify what it received; the session is unaffected, and so
	// is the next reader of it.
	session, _, err := message.Parse(strings.NewReader(limitsMessage), &message.ParseOptions{
		Limits: message.Limits{MaxPartCount: 7},
	})
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}

	held := session.Limits()
	held.MaxPartCount = 999
	held.MaxInputBytes = 999
	if got := session.Limits().MaxPartCount; got != 7 {
		t.Errorf("MaxPartCount = %d after the caller modified its copy, want 7", got)
	}
	if got := session.Limits(); got.MaxInputBytes != message.DefaultLimits().MaxInputBytes {
		t.Errorf("MaxInputBytes = %d after the caller modified its copy, want the default", got.MaxInputBytes)
	}
}

// TestParse_optionsCopySemantics pins that options are read once, when Parse is
// called. A caller that reuses and edits its options value cannot reach into a
// parse that already started, and Parse does not edit what it was given.
func TestParse_optionsCopySemantics(t *testing.T) {
	opts := &message.ParseOptions{
		Limits:      message.Limits{MaxPartCount: 7},
		ReadOptions: message.ReadOptions{MaxHeaderBytes: 4096},
	}
	before := *opts

	session, e, err := message.Parse(strings.NewReader(limitsMessage), opts)
	if err != nil || e == nil {
		t.Fatalf("Parse() = (%v, %v), want a usable entity", e, err)
	}

	// Parse resolves into its own configuration and leaves the caller's alone.
	if *opts != before {
		t.Errorf("Parse modified the options it was given:\n%+v\nwant\n%+v", *opts, before)
	}

	// Editing them afterwards changes nothing about the parse that ran.
	opts.Limits.MaxPartCount = 999
	opts.Limits.MaxInputBytes = 1
	opts.ReadOptions.MaxHeaderBytes = 1

	want := message.DefaultLimits()
	want.MaxPartCount = 7
	want.MaxHeaderBytes = 4096
	if got := session.Limits(); got != want {
		t.Errorf("Limits() =\n%+v\nwant\n%+v", got, want)
	}

	// Reading the body after the options changed still works: the parse held on
	// to nothing of them.
	if got, want := readBody(t, e), "body\r\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// untouchedReader fails the test if it is read at all. It is what proves that a
// configuration error costs no input: a rejected parse must not have consumed a
// byte a caller may want to hand to something else.
type untouchedReader struct {
	t     *testing.T
	s     string
	reads int
}

func (r *untouchedReader) Read(p []byte) (int, error) {
	r.t.Helper()
	r.reads++
	r.t.Errorf("the input was read for a configuration that was rejected")
	if len(r.s) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.s)
	r.s = r.s[n:]
	return n, nil
}
