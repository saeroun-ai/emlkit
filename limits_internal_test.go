package message

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

// The tests here cover the resolver itself, field by field, which the external
// tests can only reach through Parse. Their point is that no budget is special:
// one rule is applied to all eighteen of them, and a field that is added to
// Limits without being declared to the resolver is caught here rather than
// silently defaulting to zero.

// TestLimitFields_coverEveryField pins that the table the resolver reads
// describes exactly the fields Limits declares, in the same order, with no
// duplicates. Everything else in this file trusts that table, so it is checked
// against the type itself rather than against another list.
func TestLimitFields_coverEveryField(t *testing.T) {
	typ := reflect.TypeOf(Limits{})

	if got, want := len(limitFields), typ.NumField(); got != want {
		t.Fatalf("limitFields has %d entries, Limits has %d fields", got, want)
	}

	seen := make(map[string]bool, len(limitFields))
	for i, f := range limitFields {
		if got, want := f.name, typ.Field(i).Name; got != want {
			t.Errorf("limitFields[%d] is %q, want %q: the table is in declaration order", i, got, want)
		}
		if seen[f.name] {
			t.Errorf("limitFields declares %s twice", f.name)
		}
		seen[f.name] = true

		if f.def <= 0 {
			t.Errorf("%s has default %d, want a finite positive budget", f.name, f.def)
		}

		// The accessor must reach the field the entry names, not a neighbour.
		var l Limits
		*f.get(&l) = 1
		if got := reflect.ValueOf(l).Field(i).Int(); got != 1 {
			t.Errorf("limitFields[%d] (%s) does not address field %d", i, f.name, i)
		}
	}
}

// TestLimits_resolveEveryField applies the one rule to every budget: zero
// selects the default, LimitDisabled stays disabled, a positive value is kept
// as it is — including math.MaxInt64, which is a finite budget and not a
// synonym for disabled — and anything below LimitDisabled is rejected.
func TestLimits_resolveEveryField(t *testing.T) {
	for _, f := range limitFields {
		t.Run(f.name, func(t *testing.T) {
			for _, tc := range []struct {
				name  string
				set   int64
				want  int64
				valid bool
			}{
				{"zero selects the default", 0, f.def, true},
				{"disabled", LimitDisabled, LimitDisabled, true},
				{"one", 1, 1, true},
				{"below the default", f.def - 1, f.def - 1, true},
				{"above the default", f.def + 1, f.def + 1, true},
				{"max int64 is finite", math.MaxInt64, math.MaxInt64, true},
				{"below disabled", LimitDisabled - 1, 0, false},
				{"far below disabled", -1000, 0, false},
				{"min int64", math.MinInt64, 0, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var in Limits
					*f.get(&in) = tc.set

					got, err := in.resolve()
					if !tc.valid {
						if !errors.Is(err, ErrInvalidLimits) {
							t.Fatalf("resolve() = %v, want one verifying ErrInvalidLimits", err)
						}
						if got != (Limits{}) {
							t.Errorf("resolve() = %+v, want the zero value beside its error", got)
						}
						return
					}
					if err != nil {
						t.Fatalf("resolve() = %v, want nil", err)
					}
					if v := *f.get(&got); v != tc.want {
						t.Errorf("%s = %d, want %d", f.name, v, tc.want)
					}

					// Setting one budget must not disturb another: every other
					// field was zero, so every other field is its default.
					want := DefaultLimits()
					*f.get(&want) = tc.want
					if got != want {
						t.Errorf("resolve() =\n%+v\nwant\n%+v", got, want)
					}
				})
			}
		})
	}
}

// TestLimits_resolveNamesTheOffendingField pins that an invalid value is
// reported with the field it was written in, so a caller can find it. The
// wording is not part of the contract; the field name appearing in it is what
// makes the error usable.
func TestLimits_resolveNamesTheOffendingField(t *testing.T) {
	for _, f := range limitFields {
		var in Limits
		*f.get(&in) = -2

		_, err := in.resolve()
		if err == nil {
			t.Fatalf("%s = -2 resolved without error", f.name)
		}
		if !strings.Contains(err.Error(), f.name) {
			t.Errorf("error for %s = %q, want it to name the field", f.name, err)
		}
		if !strings.Contains(err.Error(), "-2") {
			t.Errorf("error for %s = %q, want it to report the value", f.name, err)
		}
	}
}

// TestLimits_resolveIsIdempotent pins that resolving an already resolved
// configuration changes nothing. The parser resolves once, but a caller may
// well pass DefaultLimits back in, and that must not shift a budget.
func TestLimits_resolveIsIdempotent(t *testing.T) {
	once, err := DefaultLimits().resolve()
	if err != nil {
		t.Fatalf("resolve() = %v", err)
	}
	if once != DefaultLimits() {
		t.Errorf("resolve(DefaultLimits()) =\n%+v\nwant\n%+v", once, DefaultLimits())
	}

	twice, err := once.resolve()
	if err != nil {
		t.Fatalf("second resolve() = %v", err)
	}
	if twice != once {
		t.Errorf("resolving twice changed the configuration")
	}
}

// TestLimits_resolveDoesNotTouchTheCaller pins that resolution works on a copy.
// Limits is a value type, but the resolver writes through pointers into it, so
// it is worth pinning that those pointers reach its own copy.
func TestLimits_resolveDoesNotTouchTheCaller(t *testing.T) {
	in := Limits{MaxPartCount: 7}
	if _, err := in.resolve(); err != nil {
		t.Fatalf("resolve() = %v", err)
	}
	if in != (Limits{MaxPartCount: 7}) {
		t.Errorf("resolve() modified the caller's value: %+v", in)
	}
}

// TestBridgeMaxHeaderBytes pins how the two spellings of the header budget are
// reconciled, including the case the item exists for: two values that disagree
// are an error rather than a silent choice.
func TestBridgeMaxHeaderBytes(t *testing.T) {
	const other = 4096

	for _, tc := range []struct {
		name       string
		fromLimits int64
		fromRead   int64
		want       int64
		valid      bool
	}{
		{"neither set", 0, 0, defaultMaxHeaderBytes, true},
		{"limits only", other, 0, other, true},
		{"limits disabled only", LimitDisabled, 0, LimitDisabled, true},
		{"read only", 0, other, other, true},
		{"read unlimited only", 0, LimitDisabled, LimitDisabled, true},
		{"both agree", other, other, other, true},
		{"both disabled", LimitDisabled, LimitDisabled, LimitDisabled, true},
		{"read explicitly at the default", 0, defaultMaxHeaderBytes, defaultMaxHeaderBytes, true},
		{"both explicitly at the default", defaultMaxHeaderBytes, defaultMaxHeaderBytes, defaultMaxHeaderBytes, true},
		{"both set and disagree", other, other * 2, 0, false},
		{"limits disabled, read finite", LimitDisabled, other, 0, false},
		{"limits finite, read disabled", other, LimitDisabled, 0, false},
		{"read below disabled", 0, -2, 0, false},
		{"read at min int64", 0, math.MinInt64, 0, false},
		{"read below disabled with limits set", other, -2, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// resolved is what Limits.MaxHeaderBytes already resolved to, which
			// is what the resolver would hand the bridge.
			resolved := tc.fromLimits
			if resolved == 0 {
				resolved = defaultMaxHeaderBytes
			}

			got, err := bridgeMaxHeaderBytes(tc.fromLimits, tc.fromRead, resolved)
			if !tc.valid {
				if !errors.Is(err, ErrInvalidLimits) {
					t.Fatalf("bridgeMaxHeaderBytes() = (%d, %v), want an ErrInvalidLimits error", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("bridgeMaxHeaderBytes() = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("bridgeMaxHeaderBytes() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestResolveConfig pins what one parse is configured with: the profile and
// policy as asked for, budgets resolved, and the read options the existing read
// path is handed carrying the same header budget in its own encoding.
func TestResolveConfig(t *testing.T) {
	for _, tc := range []struct {
		name       string
		opts       ParseOptions
		wantHeader int64
	}{
		{"defaults", ParseOptions{}, defaultMaxHeaderBytes},
		{"from limits", ParseOptions{Limits: Limits{MaxHeaderBytes: 99}}, 99},
		{"from read options", ParseOptions{ReadOptions: ReadOptions{MaxHeaderBytes: 99}}, 99},
		{
			"disabled through both",
			ParseOptions{
				Limits:      Limits{MaxHeaderBytes: LimitDisabled},
				ReadOptions: ReadOptions{MaxHeaderBytes: LimitDisabled},
			},
			LimitDisabled,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Profile = ProfileForensic
			tc.opts.Compatibility = CompatibilityAngus
			tc.opts.ReadOptions.SkipTextAttachmentDecoding = true

			cfg, read, err := resolveConfig(tc.opts)
			if err != nil {
				t.Fatalf("resolveConfig() = %v", err)
			}
			if cfg.profile != ProfileForensic || cfg.compat != CompatibilityAngus {
				t.Errorf("cfg = %v/%v, want the requested profile and policy", cfg.profile, cfg.compat)
			}
			if cfg.limits.MaxHeaderBytes != tc.wantHeader {
				t.Errorf("cfg.limits.MaxHeaderBytes = %d, want %d", cfg.limits.MaxHeaderBytes, tc.wantHeader)
			}
			if read.MaxHeaderBytes != tc.wantHeader {
				t.Errorf("read.MaxHeaderBytes = %d, want %d", read.MaxHeaderBytes, tc.wantHeader)
			}
			if !read.SkipTextAttachmentDecoding {
				t.Errorf("resolveConfig dropped an unrelated read option")
			}

			// Every other budget is resolved too, whichever spelling supplied
			// the header one.
			want := DefaultLimits()
			want.MaxHeaderBytes = tc.wantHeader
			if cfg.limits != want {
				t.Errorf("cfg.limits =\n%+v\nwant\n%+v", cfg.limits, want)
			}
		})
	}
}

// TestResolveConfig_invalid pins that a rejected configuration yields nothing
// usable rather than a half-resolved one.
func TestResolveConfig_invalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts ParseOptions
	}{
		{"invalid budget", ParseOptions{Limits: Limits{MaxPartCount: -2}}},
		{"invalid legacy header budget", ParseOptions{ReadOptions: ReadOptions{MaxHeaderBytes: -2}}},
		{
			"conflicting header budgets",
			ParseOptions{
				Limits:      Limits{MaxHeaderBytes: 10},
				ReadOptions: ReadOptions{MaxHeaderBytes: 20},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, read, err := resolveConfig(tc.opts)
			if !errors.Is(err, ErrInvalidLimits) {
				t.Fatalf("resolveConfig() = %v, want one verifying ErrInvalidLimits", err)
			}
			if cfg != (resolvedConfig{}) {
				t.Errorf("resolveConfig() = %+v, want the zero configuration", cfg)
			}
			if read != (ReadOptions{}) {
				t.Errorf("resolveConfig() read options = %+v, want the zero value", read)
			}
		})
	}
}

// TestParseSession_carriesResolvedLimits pins the storage half of the accessor:
// the session Parse builds holds what resolveConfig settled, and the sessions
// the older constructors build hold the defaults.
func TestParseSession_carriesResolvedLimits(t *testing.T) {
	cfg, _, err := resolveConfig(ParseOptions{Limits: Limits{MaxPartCount: 3}})
	if err != nil {
		t.Fatalf("resolveConfig() = %v", err)
	}
	s := newParseSessionWithConfig(cfg, diagnosticOptions{})
	if got := s.cfg.limits.MaxPartCount; got != 3 {
		t.Errorf("stored MaxPartCount = %d, want 3", got)
	}
	if got := s.Limits(); got != cfg.limits {
		t.Errorf("Limits() =\n%+v\nwant\n%+v", got, cfg.limits)
	}

	for name, built := range map[string]*ParseSession{
		"newParseSession":            newParseSession(ProfileTolerant, CompatibilityConservative),
		"newParseSessionWithOptions": newParseSessionWithOptions(ProfileStrict, CompatibilityAngus, diagnosticOptions{}),
	} {
		if got := built.Limits(); got != DefaultLimits() {
			t.Errorf("%s: Limits() =\n%+v\nwant the defaults\n%+v", name, got, DefaultLimits())
		}
	}
}
