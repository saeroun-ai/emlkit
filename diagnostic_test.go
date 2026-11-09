package message

import (
	"errors"
	"testing"
)

func TestSeverity_zeroValueIsInfo(t *testing.T) {
	var s Severity
	if s != SeverityInfo {
		t.Errorf("zero Severity = %v, want %v", s, SeverityInfo)
	}
}

// TestSeverity_distinct checks that the severities are distinct categories.
// Their numeric values carry no ranking, so nothing here asserts an order.
func TestSeverity_distinct(t *testing.T) {
	all := []Severity{
		SeverityInfo,
		SeverityRecovered,
		SeverityIncomplete,
		SeverityUnsupported,
		SeverityInvalidMetadata,
		SeverityLimitExceeded,
		SeverityFatal,
	}
	seen := make(map[Severity]bool, len(all))
	for _, s := range all {
		if seen[s] {
			t.Errorf("severity %v has a duplicate value %d", s, uint8(s))
		}
		seen[s] = true
	}
}

func TestSeverity_String(t *testing.T) {
	tests := []struct {
		s    Severity
		want string
	}{
		{SeverityInfo, "info"},
		{SeverityRecovered, "recovered"},
		{SeverityIncomplete, "incomplete"},
		{SeverityUnsupported, "unsupported"},
		{SeverityInvalidMetadata, "invalid_metadata"},
		{SeverityLimitExceeded, "limit_exceeded"},
		{SeverityFatal, "fatal"},
		{Severity(99), "Severity(99)"},
	}
	for _, test := range tests {
		if got := test.s.String(); got != test.want {
			t.Errorf("Severity(%d).String() = %q, want %q", uint8(test.s), got, test.want)
		}
	}
}

func TestLocation_zeroValueIsUnknown(t *testing.T) {
	var l Location
	if l.HasOffset {
		t.Error("zero Location reports a known offset")
	}
	if got := l.String(); got != "unknown" {
		t.Errorf("zero Location.String() = %q, want %q", got, "unknown")
	}
}

func TestLocation_helpers(t *testing.T) {
	// Offset 0 must be distinguishable from an unknown position.
	at := LocationAt(0)
	if !at.HasOffset || at.Offset != 0 {
		t.Errorf("LocationAt(0) = %+v, want a known offset 0", at)
	}
	if got := at.String(); got != "offset=0" {
		t.Errorf("LocationAt(0).String() = %q, want %q", got, "offset=0")
	}

	r := LocationRange(42, 50)
	if !r.HasOffset || r.Offset != 42 || r.EndOffset != 50 {
		t.Errorf("LocationRange(42, 50) = %+v", r)
	}
	if got := r.String(); got != "offset=42..50" {
		t.Errorf("LocationRange(42, 50).String() = %q, want %q", got, "offset=42..50")
	}
}

func TestLocation_String(t *testing.T) {
	tests := []struct {
		l    Location
		want string
	}{
		{Location{}, "unknown"},
		{Location{Line: 3}, "line=3"},
		{Location{Line: 3, Column: 7}, "line=3 column=7"},
		{Location{HasOffset: true, Offset: 12, Line: 3, Column: 7}, "offset=12 line=3 column=7"},
		{Location{HasOffset: true, Offset: 12, EndOffset: 12}, "offset=12"},
		{Location{HasOffset: true, Offset: 12, EndOffset: 5}, "offset=12"},
	}
	for _, test := range tests {
		if got := test.l.String(); got != test.want {
			t.Errorf("Location(%+v).String() = %q, want %q", test.l, got, test.want)
		}
	}
}

func TestDiagnostic_Clone(t *testing.T) {
	path := []int{1, 0}
	d := Diagnostic{
		Code:     CodeMultipartCloseNotFound,
		Severity: SeverityIncomplete,
		Message:  "closing delimiter not found",
		Path:     path,
	}

	clone := d.Clone()
	if clone.Path[0] != 1 || clone.Path[1] != 0 {
		t.Fatalf("Clone().Path = %v, want %v", clone.Path, path)
	}

	// Mutating either side must not be visible on the other one.
	path[0] = 99
	if clone.Path[0] != 1 {
		t.Errorf("Clone().Path aliases the original slice: %v", clone.Path)
	}
	clone.Path[1] = 5
	if d.Path[1] != 0 {
		t.Errorf("original Path was modified through the clone: %v", d.Path)
	}
}

// TestDiagnostic_Clone_retainsCause pins the documented limit of Clone: an
// arbitrary error value cannot be copied, so both copies observe the same one.
func TestDiagnostic_Clone_retainsCause(t *testing.T) {
	cause := errors.New("boom")
	clone := Diagnostic{Code: CodeInputReadError, Cause: cause}.Clone()
	if clone.Cause != cause {
		t.Errorf("Clone().Cause = %v, want the original error value", clone.Cause)
	}
	if !errors.Is(clone.Cause, cause) {
		t.Error("Clone().Cause no longer matches the original error")
	}
}

func TestDiagnostic_Clone_nilPath(t *testing.T) {
	clone := Diagnostic{Code: CodeInputEmpty}.Clone()
	if clone.Path != nil {
		t.Errorf("Clone().Path = %v, want nil", clone.Path)
	}
}

func TestDiagnostic_String(t *testing.T) {
	tests := []struct {
		name string
		d    Diagnostic
		want string
	}{
		{
			name: "zero value",
			d:    Diagnostic{},
			want: "info (no code)",
		},
		{
			name: "code and message",
			d: Diagnostic{
				Code:     CodeInputEmpty,
				Severity: SeverityIncomplete,
				Message:  "message is empty",
			},
			want: "incomplete input.empty: message is empty",
		},
		{
			name: "full",
			d: Diagnostic{
				Code:     CodeHeaderBareLF,
				Severity: SeverityRecovered,
				Message:  "header line ends with a bare LF",
				Path:     []int{1, 0},
				Field:    "Subject",
				Action:   "treated the bare LF as a line ending",
				Location: Location{HasOffset: true, Offset: 42, EndOffset: 43, Line: 3, Column: 9},
				Cause:    errors.New("boom"),
			},
			want: "recovered header.bare_lf path=1.0 offset=42..43 line=3 column=9 field=Subject: " +
				"header line ends with a bare LF (action: treated the bare LF as a line ending): boom",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.d.String(); got != test.want {
				t.Errorf("Diagnostic.String() =\n%q\nwant\n%q", got, test.want)
			}
		})
	}
}
