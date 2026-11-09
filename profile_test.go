package message

import "testing"

func TestProfile_zeroValueIsTolerant(t *testing.T) {
	var p Profile
	if p != ProfileTolerant {
		t.Errorf("zero Profile = %v, want %v", p, ProfileTolerant)
	}
}

func TestProfile_String(t *testing.T) {
	tests := []struct {
		p    Profile
		want string
	}{
		{ProfileTolerant, "tolerant"},
		{ProfileStrict, "strict"},
		{ProfileForensic, "forensic"},
		{Profile(42), "Profile(42)"},
	}
	for _, test := range tests {
		if got := test.p.String(); got != test.want {
			t.Errorf("Profile(%d).String() = %q, want %q", uint8(test.p), got, test.want)
		}
	}
}

func TestCompatibility_zeroValueIsConservative(t *testing.T) {
	var c Compatibility
	if c != CompatibilityConservative {
		t.Errorf("zero Compatibility = %v, want %v", c, CompatibilityConservative)
	}
}

func TestCompatibility_String(t *testing.T) {
	tests := []struct {
		c    Compatibility
		want string
	}{
		{CompatibilityConservative, "conservative"},
		{CompatibilityAngus, "angus"},
		{CompatibilityMimeKit, "mimekit"},
		{CompatibilityPythonEmail, "python-email"},
		{Compatibility(7), "Compatibility(7)"},
	}
	for _, test := range tests {
		if got := test.c.String(); got != test.want {
			t.Errorf("Compatibility(%d).String() = %q, want %q", uint8(test.c), got, test.want)
		}
	}
}
