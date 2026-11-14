package message

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// Drain's verdict (#10, #70): a stop that arose while Drain read the last
// thing it reads is returned, not nil; and a second Drain finds nothing left,
// under every profile, without framing what follows a closing delimiter.

var drainProfiles = []Profile{ProfileStrict, ProfileTolerant, ProfileForensic}

// drainCarrierTail is the size of what an embedded message leaves of its
// carrier's body in the tests below: far more than any reader of the walk
// buffers ahead, so that the walk cannot have read it before Drain's own read
// of the carrier.
const drainCarrierTail = 64 << 10

// cancelAtReader cancels a context once n bytes of r have been read.
type cancelAtReader struct {
	r      io.Reader
	n, got int
	cancel context.CancelFunc
}

func (c *cancelAtReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if c.got += n; c.got >= c.n {
		c.cancel()
	}
	return n, err
}

// checkDrainStop drains e twice and checks that each returns the parse's
// stop, the identical value, of the state want.
func checkDrainStop(t *testing.T, s *ParseSession, e *Entity, want CompletionState) {
	t.Helper()
	derr := e.Drain()
	stop := e.ParseStop()
	if stop == nil {
		t.Fatalf("Drain() = %v and the parse was not stopped; the test needs a stop (state %v)", derr, s.Completion().State)
	}
	if !sameError(derr, stop) {
		t.Errorf("Drain() = %v, want the parse's stop %v", derr, stop)
	}
	if c := s.Completion(); c.State != want || !sameError(c.Stop, stop) {
		t.Errorf("Completion = %v (stop %v), want %v", c.State, c.Stop, want)
	}
	if again := e.Drain(); !sameError(again, stop) {
		t.Errorf("Drain() again = %v, want the identical stop", again)
	}
}

// TestDrain_lastBodyStop: a budget that ends the parse while Drain reads the
// last body of a message, of a multipart, or of an embedded message.
func TestDrain_lastBodyStop(t *testing.T) {
	single := "Subject: s\r\n\r\n" + strings.Repeat("x", 200)
	multi := "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\none\r\n--B\r\n\r\n" + strings.Repeat("y", 200) + "\r\n--B--\r\n"
	emb := "Content-Type: message/rfc822\r\n\r\nSubject: in\r\n\r\n" + strings.Repeat("z", 200)
	for _, tc := range []struct {
		name, text string
		limits     Limits
	}{
		{"single/input", single, Limits{MaxInputBytes: 100}},
		{"single/decoded", single, Limits{MaxTotalDecodedBytes: 50}},
		{"multipart/input", multi, Limits{MaxInputBytes: 150}},
		{"multipart/decoded", multi, Limits{MaxTotalDecodedBytes: 50}},
		{"embedded/input", emb, Limits{MaxInputBytes: 120}},
		{"embedded/decoded", emb, Limits{MaxTotalDecodedBytes: 50}},
	} {
		for _, profile := range drainProfiles {
			t.Run(tc.name+"/"+profile.String(), func(t *testing.T) {
				s, e, err := Parse(strings.NewReader(tc.text), &ParseOptions{Profile: profile, WalkEmbeddedMessages: true, Limits: tc.limits})
				if e == nil {
					t.Fatal(err)
				}
				checkDrainStop(t, s, e, CompletionLimited)
			})
		}
	}
}

// TestDrain_carrierRestStop: under ProfileStrict the walk leaves a carrier
// when its embedded multipart closes, and what follows in the carrier's body
// is read by Drain after the walk. A stop that arose there was not seen by the
// walk, and Drain returned nil. Under recovery the walk reads that rest itself
// (chunk_invariance.go), and the stop was already returned; it is checked
// here too.
func TestDrain_carrierRestStop(t *testing.T) {
	closed := "Content-Type: message/rfc822\r\n\r\nSubject: in\r\nContent-Type: multipart/mixed; boundary=I\r\n\r\n--I\r\n\r\nx\r\n--I--\r\n" +
		strings.Repeat("t", drainCarrierTail)

	t.Run("strict/input", func(t *testing.T) {
		s, e, err := Parse(strings.NewReader(closed), &ParseOptions{Profile: ProfileStrict, WalkEmbeddedMessages: true,
			Limits: Limits{MaxInputBytes: int64(len(closed)) - 100}})
		if e == nil {
			t.Fatal(err)
		}
		checkDrainStop(t, s, e, CompletionLimited)
	})
	t.Run("strict/decoded", func(t *testing.T) {
		s, e, err := Parse(strings.NewReader(closed), &ParseOptions{Profile: ProfileStrict, WalkEmbeddedMessages: true,
			Limits: Limits{MaxTotalDecodedBytes: drainCarrierTail - 100}})
		if e == nil {
			t.Fatal(err)
		}
		checkDrainStop(t, s, e, CompletionLimited)
	})
	t.Run("strict/cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		src := &cancelAtReader{r: strings.NewReader(closed), n: len(closed) - 100, cancel: cancel}
		s, e, err := ParseContext(ctx, src, &ParseOptions{Profile: ProfileStrict, WalkEmbeddedMessages: true})
		if e == nil {
			t.Fatal(err)
		}
		checkDrainStop(t, s, e, CompletionCanceled)
	})
	// A sink that stops the parse over the carrier's body budget, which the
	// tail runs into: the budget itself ends only the carrier's body, so the
	// stop is the sink's.
	enough := errors.New("enough")
	t.Run("strict/sink", func(t *testing.T) {
		s, e, err := Parse(strings.NewReader(closed), &ParseOptions{Profile: ProfileStrict, WalkEmbeddedMessages: true,
			Limits: Limits{MaxDecodedBodyBytes: drainCarrierTail - 100},
			DiagnosticSink: DiagnosticSinkFunc(func(_ *ParseSession, d Diagnostic) error {
				if d.Code == CodeLimitDecodedBodyBytes {
					return enough
				}
				return nil
			})})
		if e == nil {
			t.Fatal(err)
		}
		checkDrainStop(t, s, e, CompletionStopped)
		if c := s.Completion(); !errors.Is(c.Stop, enough) {
			t.Errorf("stop %v does not wrap the sink's error", c.Stop)
		}
	})

	// The walk ends with a refusal of ProfileStrict, an error of one entity;
	// the carrier's rest, read after it, runs into the input budget. The stop
	// of the whole parse outranks the refusal, as everywhere else.
	refused := "Content-Type: message/rfc822\r\n\r\nSubject: in\r\nContent-Type: multipart/mixed; boundary=I\r\n\r\n" +
		"--I\r\nContent-Type: text\r\n\r\nx\r\n--I--\r\n" + strings.Repeat("t", drainCarrierTail)
	t.Run("strict/refusal-then-input", func(t *testing.T) {
		s, e, err := Parse(strings.NewReader(refused), &ParseOptions{Profile: ProfileStrict, WalkEmbeddedMessages: true,
			Limits: Limits{MaxInputBytes: int64(len(refused)) - 100}})
		if e == nil {
			t.Fatal(err)
		}
		checkDrainStop(t, s, e, CompletionLimited)
	})
	// Without the budget the same walk returns the refusal itself.
	t.Run("strict/refusal", func(t *testing.T) {
		_, e, err := Parse(strings.NewReader(refused), &ParseOptions{Profile: ProfileStrict, WalkEmbeddedMessages: true})
		if e == nil {
			t.Fatal(err)
		}
		derr := e.Drain()
		if derr == nil || e.ParseStop() != nil || !IsFatal(derr) {
			t.Errorf("Drain() = %v, stop %v; want the refusal, and no stop", derr, e.ParseStop())
		}
	})

	for _, profile := range []Profile{ProfileTolerant, ProfileForensic} {
		t.Run(profile.String()+"/input", func(t *testing.T) {
			s, e, err := Parse(strings.NewReader(closed), &ParseOptions{Profile: profile, WalkEmbeddedMessages: true,
				Limits: Limits{MaxInputBytes: int64(len(closed)) - 100}})
			if e == nil {
				t.Fatal(err)
			}
			checkDrainStop(t, s, e, CompletionLimited)
		})
	}
}

// drainSnapshot is what a second Drain must leave as the first did.
type drainSnapshot struct {
	c     Completion
	diags int
}

func takeDrainSnapshot(s *ParseSession) drainSnapshot {
	c := s.Completion()
	c.InputErr, c.Stop = nil, nil
	return drainSnapshot{c: c, diags: s.Len()}
}

// TestDrain_again: a second Drain after one that returned nil returns nil,
// reads nothing of the input, and records nothing, under every profile. The
// epilogue holds a delimiter line, which ProfileStrict's second Drain framed
// into a part, and the input is read a byte at a time, so that whatever a
// second Drain read would show in InputBytes.
func TestDrain_again(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		embedded   bool
	}{
		{"epilogue", "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\none\r\n--B--\r\n" +
			"--B\r\n\r\nphantom\r\n--B--\r\n", false},
		{"nested", "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: multipart/alternative; boundary=C\r\n\r\n" +
			"--C\r\n\r\nin\r\n--C--\r\n--C\r\n\r\nghost\r\n--B--\r\nafter\r\n", false},
		{"embedded", "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: message/rfc822\r\n\r\n" +
			"Subject: in\r\nContent-Type: multipart/mixed; boundary=I\r\n\r\n--I\r\n\r\nx\r\n--I--\r\n--I\r\n\r\nghost\r\n" +
			"--B--\r\n--B\r\n\r\nphantom\r\n", true},
	} {
		for _, profile := range drainProfiles {
			t.Run(tc.name+"/"+profile.String(), func(t *testing.T) {
				s, e, err := Parse(iotest.OneByteReader(strings.NewReader(tc.text)), &ParseOptions{Profile: profile, WalkEmbeddedMessages: tc.embedded})
				if e == nil {
					t.Fatal(err)
				}
				if err := e.Drain(); err != nil {
					t.Fatalf("Drain() = %v", err)
				}
				first := takeDrainSnapshot(s)
				if first.c.State == CompletionInProgress || first.c.BodiesRead != first.c.Bodies || first.c.MultipartsRead != first.c.Multiparts {
					t.Fatalf("after Drain: %+v", first.c)
				}
				for i := range 2 {
					if err := e.Drain(); err != nil {
						t.Errorf("Drain() %d again = %v", i+1, err)
					}
					if got := takeDrainSnapshot(s); got != first {
						t.Errorf("Drain() %d again changed the verdict:\n got %+v\nwant %+v", i+1, got, first)
					}
				}
				// Walk keeps its semantics: the parts were read, so a walk
				// visits what it is handed without framing them again.
				visited := 0
				if err := e.Walk(func([]int, *Entity, error) error { visited++; return nil }); err != nil {
					t.Errorf("Walk after Drain = %v", err)
				}
				if visited != 1 && !tc.embedded {
					t.Errorf("Walk after Drain visited %d entities, want the root alone", visited)
				}
				if got := takeDrainSnapshot(s); got != first {
					t.Errorf("Walk after Drain changed the verdict:\n got %+v\nwant %+v", got, first)
				}
			})
		}
	}
}

// TestDrain_againAfterFailure: an iterator that fails for good ends the walk
// with its failure, and a second Drain returns that identical value rather
// than nil or a fresh one.
func TestDrain_againAfterFailure(t *testing.T) {
	// ProfileStrict does not heal a multipart that declares no boundary.
	const text = "Content-Type: multipart/mixed\r\n\r\n--B\r\n\r\none\r\n--B--\r\n"
	s, e, err := Parse(strings.NewReader(text), &ParseOptions{Profile: ProfileStrict})
	if e == nil {
		t.Fatal(err)
	}
	first := e.Drain()
	if first == nil || e.ParseStop() != nil {
		t.Fatalf("Drain() = %v, stop %v; the test needs an iterator's own failure", first, e.ParseStop())
	}
	c := s.Completion()
	if c.MultipartsRead != c.Multiparts {
		t.Errorf("the failed multipart is not counted as read: %+v", c)
	}
	for i := range 2 {
		if again := e.Drain(); !sameError(again, first) {
			t.Errorf("Drain() %d again = %v, want the identical %v", i+1, again, first)
		}
	}
}

// TestDrain_replacedBody: a body the caller replaced is framed anew, as
// before, under every profile.
func TestDrain_replacedBody(t *testing.T) {
	const text = "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\n\r\none\r\n--B--\r\n"
	for _, profile := range drainProfiles {
		_, e, err := Parse(strings.NewReader(text), &ParseOptions{Profile: profile})
		if e == nil {
			t.Fatal(err)
		}
		if err := e.Drain(); err != nil {
			t.Fatalf("%v: Drain() = %v", profile, err)
		}
		e.Body = strings.NewReader("--B\r\n\r\nnew\r\n--B--\r\n")
		mr := e.MultipartReader()
		p, err := mr.NextPart()
		if err != nil {
			t.Fatalf("%v: the replaced body: %v", profile, err)
		}
		if b, _ := io.ReadAll(p.Body); string(b) != "new" {
			t.Errorf("%v: the replaced body's part = %q", profile, b)
		}
	}
}
