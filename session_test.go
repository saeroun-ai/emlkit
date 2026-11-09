package message

import (
	"sync"
	"testing"
)

func TestParseSession_zeroValue(t *testing.T) {
	var s ParseSession

	if got := s.Profile(); got != ProfileTolerant {
		t.Errorf("zero ParseSession.Profile() = %v, want %v", got, ProfileTolerant)
	}
	if got := s.Compatibility(); got != CompatibilityConservative {
		t.Errorf("zero ParseSession.Compatibility() = %v, want %v", got, CompatibilityConservative)
	}
	if got := s.Len(); got != 0 {
		t.Errorf("zero ParseSession.Len() = %d, want 0", got)
	}
	if got := s.Diagnostics(); got != nil {
		t.Errorf("zero ParseSession.Diagnostics() = %v, want nil", got)
	}

	s.record(Diagnostic{Code: CodeInputEmpty})
	if got := s.Len(); got != 1 {
		t.Errorf("ParseSession.Len() = %d, want 1 after record", got)
	}
}

func TestParseSession_nil(t *testing.T) {
	var s *ParseSession

	// Recording on a nil session is a no-op rather than a panic, so a parser
	// can carry an optional session without a nil check on every call site.
	s.record(Diagnostic{Code: CodeInputEmpty, Path: []int{0}})

	if got := s.Len(); got != 0 {
		t.Errorf("nil ParseSession.Len() = %d, want 0", got)
	}
	if got := s.Diagnostics(); got != nil {
		t.Errorf("nil ParseSession.Diagnostics() = %v, want nil", got)
	}
	if got := s.Profile(); got != ProfileTolerant {
		t.Errorf("nil ParseSession.Profile() = %v, want %v", got, ProfileTolerant)
	}
	if got := s.Compatibility(); got != CompatibilityConservative {
		t.Errorf("nil ParseSession.Compatibility() = %v, want %v", got, CompatibilityConservative)
	}
}

func TestParseSession_recordsProfileAndCompatibility(t *testing.T) {
	s := newParseSession(ProfileForensic, CompatibilityAngus)
	if got := s.Profile(); got != ProfileForensic {
		t.Errorf("Profile() = %v, want %v", got, ProfileForensic)
	}
	if got := s.Compatibility(); got != CompatibilityAngus {
		t.Errorf("Compatibility() = %v, want %v", got, CompatibilityAngus)
	}
}

func TestParseSession_preservesDiscoveryOrder(t *testing.T) {
	s := newParseSession(ProfileTolerant, CompatibilityConservative)

	want := []Code{
		CodeHeaderBareLF,
		CodeMIMEParameterUnclosedQuote,
		CodeMultipartCloseNotFound,
		CodeCTEBase64PaddingMissing,
		CodeHeaderBareLF, // a repeated code keeps its own position
	}
	for _, code := range want {
		s.record(Diagnostic{Code: code})
	}

	got := s.Diagnostics()
	if len(got) != len(want) {
		t.Fatalf("Diagnostics() returned %d diagnostics, want %d", len(got), len(want))
	}
	for i, d := range got {
		if d.Code != want[i] {
			t.Errorf("Diagnostics()[%d].Code = %q, want %q", i, d.Code, want[i])
		}
	}
	if got := s.Len(); got != len(want) {
		t.Errorf("Len() = %d, want %d", got, len(want))
	}
}

func TestParseSession_record_copiesPath(t *testing.T) {
	s := newParseSession(ProfileTolerant, CompatibilityConservative)

	path := []int{1, 2}
	s.record(Diagnostic{Code: CodeMultipartEmptyPart, Path: path})

	// A parser reusing its path buffer must not rewrite recorded history.
	path[0], path[1] = 9, 9

	got := s.Diagnostics()
	if len(got) != 1 {
		t.Fatalf("Diagnostics() returned %d diagnostics, want 1", len(got))
	}
	if got[0].Path[0] != 1 || got[0].Path[1] != 2 {
		t.Errorf("recorded Path = %v, want [1 2]", got[0].Path)
	}
}

func TestParseSession_Diagnostics_returnsDefensiveCopy(t *testing.T) {
	s := newParseSession(ProfileTolerant, CompatibilityConservative)
	s.record(Diagnostic{Code: CodeMultipartEmptyPart, Path: []int{1, 2}})

	first := s.Diagnostics()
	first[0].Code = CodeInputEmpty
	first[0].Severity = SeverityFatal
	first[0].Path[0] = 9

	second := s.Diagnostics()
	if second[0].Code != CodeMultipartEmptyPart {
		t.Errorf("Code = %q, want %q", second[0].Code, CodeMultipartEmptyPart)
	}
	if second[0].Severity != SeverityInfo {
		t.Errorf("Severity = %v, want %v", second[0].Severity, SeverityInfo)
	}
	if second[0].Path[0] != 1 || second[0].Path[1] != 2 {
		t.Errorf("Path = %v, want [1 2]", second[0].Path)
	}

	// Appending to an earlier result must not disturb a later one either.
	first = append(first, Diagnostic{Code: CodeInputEmpty})
	if len(s.Diagnostics()) != 1 {
		t.Errorf("Diagnostics() returned %d diagnostics, want 1", len(s.Diagnostics()))
	}
	_ = first
}

// TestParseSession_concurrentRecordAndRead exercises the append/read race under
// "go test -race": diagnostics are discovered from several goroutines while
// other goroutines read the session.
func TestParseSession_concurrentRecordAndRead(t *testing.T) {
	const (
		writers      = 4
		perWriter    = 64
		readers      = 2
		readsPerRead = 32
	)

	s := newParseSession(ProfileTolerant, CompatibilityConservative)

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				s.record(Diagnostic{
					Code:     CodeMultipartEmptyPart,
					Severity: SeverityInfo,
					Path:     []int{w, i},
				})
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < readsPerRead; i++ {
				for _, d := range s.Diagnostics() {
					if len(d.Path) != 2 {
						t.Errorf("read a diagnostic with Path %v, want two elements", d.Path)
						return
					}
				}
				_ = s.Len()
			}
		}()
	}
	wg.Wait()

	got := s.Diagnostics()
	if len(got) != writers*perWriter {
		t.Fatalf("Diagnostics() returned %d diagnostics, want %d", len(got), writers*perWriter)
	}

	// Interleaving between goroutines is not deterministic, but each writer's
	// own diagnostics must stay in the order it added them.
	next := make([]int, writers)
	for i, d := range got {
		w := d.Path[0]
		if w < 0 || w >= writers {
			t.Fatalf("Diagnostics()[%d] has unexpected writer index %d", i, w)
		}
		if d.Path[1] != next[w] {
			t.Fatalf("writer %d: diagnostic %d out of order, got index %d want %d", w, i, d.Path[1], next[w])
		}
		next[w]++
	}
}
