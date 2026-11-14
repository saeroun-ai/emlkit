package message

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

// Drain after a partial read (#10, #70; drain_resume.go): the iterator a
// caller began is resumed, the part it handed out is finished first, and a
// part gone past unread never counts as read.

const resumeTwo = "Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
	"--b\r\n\r\nfirst body\r\n" +
	"--b\r\nContent-Transfer-Encoding: base64\r\n\r\nc2Vjb25k!IGJvZHk=\r\n" +
	"--b--\r\n"

// resumeNested has a multipart first part, whose second part holds base64
// junk, and a leaf after it.
const resumeNested = "Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
	"--b\r\nContent-Type: multipart/alternative; boundary=c\r\n\r\n" +
	"--c\r\n\r\ninner one\r\n" +
	"--c\r\nContent-Transfer-Encoding: base64\r\n\r\naW5uZXI!gdHdv\r\n" +
	"--c--\r\n" +
	"--b\r\n\r\nlast\r\n" +
	"--b--\r\n"

func resumeParse(t *testing.T, text string, profile Profile) (*ParseSession, *Entity) {
	t.Helper()
	s, e, err := Parse(strings.NewReader(text), &ParseOptions{Profile: profile})
	if e == nil {
		t.Fatal(err)
	}
	return s, e
}

func readOne(t *testing.T, e *Entity) {
	t.Helper()
	if n, err := e.Body.Read(make([]byte, 1)); n != 1 || err != nil {
		t.Fatalf("reading one byte: %d, %v", n, err)
	}
}

// checkComplete checks that the parse was read whole: nothing in progress,
// every body and multipart counted read.
func checkComplete(t *testing.T, s *ParseSession, entities int64) {
	t.Helper()
	c := s.Completion()
	if c.State == CompletionInProgress || c.BodiesRead != c.Bodies || c.MultipartsRead != c.Multiparts || c.Entities != entities {
		t.Errorf("not read whole: state %v, entities %d (want %d), bodies %d/%d, multiparts %d/%d",
			c.State, c.Entities, entities, c.BodiesRead, c.Bodies, c.MultipartsRead, c.Multiparts)
	}
}

// junkPaths returns the paths of the cte.base64_junk diagnostics.
func junkPaths(s *ParseSession) []string {
	var out []string
	for _, d := range s.Diagnostics() {
		if d.Code == CodeCTEBase64Junk {
			out = append(out, fmt.Sprint(d.Path))
		}
	}
	return out
}

func TestDrainResume_partialLeaf(t *testing.T) {
	for _, profile := range drainProfiles {
		// The first part, one byte read.
		t.Run("first/"+profile.String(), func(t *testing.T) {
			s, e := resumeParse(t, resumeTwo, profile)
			p, err := e.MultipartReader().NextPart()
			if err != nil {
				t.Fatal(err)
			}
			readOne(t, p)
			if err := e.Drain(); err != nil {
				t.Fatalf("Drain() = %v", err)
			}
			checkComplete(t, s, 3)
			if profile != ProfileStrict {
				if got := junkPaths(s); !slices.Equal(got, []string{"[1]"}) {
					t.Errorf("the second part's repair has paths %v, want [[1]]", got)
				}
			}
			// What the caller holds was finished: the rest of its body is
			// gone, read by Drain.
			if b, err := io.ReadAll(p.Body); len(b) != 0 || err != nil {
				t.Errorf("the first part after Drain: %q, %v", b, err)
			}
			if err := e.Drain(); err != nil {
				t.Errorf("Drain() again = %v", err)
			}
			checkComplete(t, s, 3)
		})
		// The last part, one byte read.
		t.Run("last/"+profile.String(), func(t *testing.T) {
			s, e := resumeParse(t, resumeTwo, profile)
			mr := e.MultipartReader()
			p, _ := mr.NextPart()
			io.Copy(io.Discard, p.Body)
			p, err := mr.NextPart()
			if err != nil {
				t.Fatal(err)
			}
			readOne(t, p)
			if err := e.Drain(); err != nil {
				t.Fatalf("Drain() = %v", err)
			}
			checkComplete(t, s, 3)
			if _, err := mr.NextPart(); err != io.EOF {
				t.Errorf("the caller's iterator after Drain: %v, want io.EOF", err)
			}
		})
	}
}

func TestDrainResume_nested(t *testing.T) {
	for _, profile := range drainProfiles {
		t.Run(profile.String(), func(t *testing.T) {
			s, e := resumeParse(t, resumeNested, profile)
			outer, err := e.MultipartReader().NextPart()
			if err != nil {
				t.Fatal(err)
			}
			in, err := outer.MultipartReader().NextPart()
			if err != nil {
				t.Fatal(err)
			}
			readOne(t, in)
			if err := e.Drain(); err != nil {
				t.Fatalf("Drain() = %v", err)
			}
			checkComplete(t, s, 5)
			if profile != ProfileStrict {
				if got := junkPaths(s); !slices.Equal(got, []string{"[0 1]"}) {
					t.Errorf("the inner second part's repair has paths %v, want [[0 1]]", got)
				}
			}
		})
	}
}

// TestDrainResume_lookups: MultipartReader called again hands out the same
// iterator, unstarted or started, and it goes on where it stood.
func TestDrainResume_lookups(t *testing.T) {
	for _, profile := range drainProfiles {
		t.Run(profile.String(), func(t *testing.T) {
			s, e := resumeParse(t, resumeTwo, profile)
			a, b := e.MultipartReader(), e.MultipartReader()
			if a != b {
				t.Fatal("two lookups before any part: two iterators")
			}
			p, _ := a.NextPart()
			got, _ := io.ReadAll(p.Body)
			c := e.MultipartReader()
			if c != a {
				t.Fatal("a lookup after a part: a new iterator")
			}
			p, err := c.NextPart()
			if err != nil {
				t.Fatal(err)
			}
			second, _ := io.ReadAll(p.Body)
			if string(got) != "first body" || !strings.HasPrefix(string(second), "second") {
				t.Errorf("parts %q, %q", got, second)
			}
			if err := e.Drain(); err != nil {
				t.Fatalf("Drain() = %v", err)
			}
			checkComplete(t, s, 3)
		})
	}
	// Unstarted, then drained.
	s, e := resumeParse(t, resumeTwo, ProfileStrict)
	e.MultipartReader()
	if err := e.Drain(); err != nil {
		t.Fatalf("Drain() after an unstarted lookup = %v", err)
	}
	checkComplete(t, s, 3)
}

// TestDrainResume_walk: a Walk stopped inside a part, then resumed by another
// Walk with the parts' own indices and spans, or by Drain.
func TestDrainResume_walk(t *testing.T) {
	enough := errors.New("enough")
	stopInFirst := func(_ []int, p *Entity, _ error) error {
		if p.MultipartReader() == nil {
			readOne(t, p)
			return enough
		}
		return nil
	}

	// The paths and bodies of a walk that reads every part.
	type seen struct {
		path string
		body string
	}
	leaves := func(e *Entity) []seen {
		var out []seen
		e.Walk(func(path []int, p *Entity, _ error) error {
			if p.MultipartReader() != nil {
				return nil
			}
			b, _ := io.ReadAll(p.Body)
			out = append(out, seen{path: fmt.Sprint(path), body: string(b)})
			return nil
		})
		return out
	}

	for _, profile := range drainProfiles {
		t.Run(profile.String(), func(t *testing.T) {
			_, whole := resumeParse(t, resumeNested, profile)
			all := leaves(whole)
			if len(all) != 3 {
				t.Fatalf("a whole walk: %v", all)
			}

			s, e := resumeParse(t, resumeNested, profile)
			if err := e.Walk(stopInFirst); err != enough {
				t.Fatalf("Walk = %v", err)
			}
			// The resumed walk goes on with the root's iterator: it goes past
			// the part the walk stopped in — the nested multipart, whose
			// first part was being read — as NextPart does, and numbers what
			// follows as it is numbered.
			rest := leaves(e)
			want := all[2:]
			if fmt.Sprint(rest) != fmt.Sprint(want) {
				t.Errorf("resumed walk: %v, want %v", rest, want)
			}
			// The part gone past cannot be read whole any more.
			if c := s.Completion(); c.State != CompletionInProgress || c.BodiesRead == c.Bodies {
				t.Errorf("a part gone past unread: %+v", c)
			}

			// Resumed by Drain instead, nothing is gone past.
			s, e = resumeParse(t, resumeNested, profile)
			if err := e.Walk(stopInFirst); err != enough {
				t.Fatalf("Walk = %v", err)
			}
			if err := e.Drain(); err != nil {
				t.Fatalf("Drain() = %v", err)
			}
			checkComplete(t, s, 5)
		})
	}
}

// TestDrainResume_spans: the raw span of each part is the same whether the
// parts were read straight through or a Drain finished them.
func TestDrainResume_spans(t *testing.T) {
	for _, profile := range drainProfiles {
		t.Run(profile.String(), func(t *testing.T) {
			_, e := resumeParse(t, resumeTwo, profile)
			mr := e.MultipartReader()
			var want [][3]int64
			for {
				p, err := mr.NextPart()
				if err != nil {
					break
				}
				io.Copy(io.Discard, p.Body)
				a, b, ok := p.RawSpan()
				want = append(want, [3]int64{a, b, map[bool]int64{true: 1}[ok]})
			}

			_, e = resumeParse(t, resumeTwo, profile)
			mr = e.MultipartReader()
			first, _ := mr.NextPart()
			readOne(t, first)
			if err := e.Drain(); err != nil {
				t.Fatal(err)
			}
			a, b, ok := first.RawSpan()
			if got := [3]int64{a, b, map[bool]int64{true: 1}[ok]}; got != want[0] {
				t.Errorf("the finished part's span %v, want %v", got, want[0])
			}
		})
	}
}

// TestDrainResume_skipped: a part the iterator went past unread is gone; read
// afterwards, it delivers what it delivers, and the parse is never complete,
// Drain or not.
func TestDrainResume_skipped(t *testing.T) {
	for _, profile := range drainProfiles {
		t.Run(profile.String(), func(t *testing.T) {
			s, e := resumeParse(t, resumeTwo, profile)
			mr := e.MultipartReader()
			skipped, _ := mr.NextPart()
			if _, err := mr.NextPart(); err != nil {
				t.Fatal(err)
			}
			io.ReadAll(skipped.Body)
			if err := e.Drain(); err != nil {
				t.Fatalf("Drain() = %v", err)
			}
			c := s.Completion()
			if c.State != CompletionInProgress || c.BodiesRead != 1 || c.Bodies != 2 || c.Clean() {
				t.Errorf("a skipped part read late: %+v", c)
			}

			// A nested multipart gone past while its parts were being read:
			// whatever its iterator still frames from its buffer is not
			// counted either.
			s, e = resumeParse(t, resumeNested, profile)
			mr = e.MultipartReader()
			outer, _ := mr.NextPart()
			imr := outer.MultipartReader()
			first, _ := imr.NextPart()
			io.Copy(io.Discard, first.Body)
			if _, err := mr.NextPart(); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				p, err := imr.NextPart()
				if err != nil {
					break
				}
				io.Copy(io.Discard, p.Body)
			}
			if err := e.Drain(); err != nil {
				t.Fatalf("Drain() = %v", err)
			}
			c = s.Completion()
			if c.State != CompletionInProgress || c.MultipartsRead == c.Multiparts || c.Clean() {
				t.Errorf("a nested multipart gone past: %+v", c)
			}
		})
	}
}

// TestDrainResume_stop: a stop that arises while Drain finishes the part the
// caller began is returned, the identical value, by this Drain and the next.
func TestDrainResume_stop(t *testing.T) {
	text := "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\n" + strings.Repeat("x", drainCarrierTail) + "\r\n--b--\r\n"
	for _, profile := range drainProfiles {
		t.Run(profile.String(), func(t *testing.T) {
			s, e, err := Parse(strings.NewReader(text), &ParseOptions{Profile: profile,
				Limits: Limits{MaxInputBytes: int64(len(text)) - 100}})
			if e == nil {
				t.Fatal(err)
			}
			p, err := e.MultipartReader().NextPart()
			if err != nil {
				t.Fatal(err)
			}
			readOne(t, p)
			checkDrainStop(t, s, e, CompletionLimited)
		})
	}
}

// TestDrainResume_embedded: under WalkEmbeddedMessages, a message the caller
// began reading out of a carrier part is finished, and so is the rest of the
// carrier, under every profile.
func TestDrainResume_embedded(t *testing.T) {
	const text = "Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: message/rfc822\r\n\r\n" +
		"Subject: in\r\nContent-Type: multipart/mixed; boundary=i\r\n\r\n" +
		"--i\r\n\r\ninner one\r\n--i\r\n\r\ninner two\r\n--i--\r\ncarrier rest\r\n" +
		"--b\r\n\r\nlast\r\n--b--\r\n"
	for _, profile := range drainProfiles {
		opts := &ParseOptions{Profile: profile, WalkEmbeddedMessages: true}
		t.Run("begun/"+profile.String(), func(t *testing.T) {
			s, e, _ := Parse(strings.NewReader(text), opts)
			carrier, err := e.MultipartReader().NextPart()
			if err != nil {
				t.Fatal(err)
			}
			emb, err := carrier.EmbeddedMessage()
			if emb == nil {
				t.Fatal(err)
			}
			p, err := emb.MultipartReader().NextPart()
			if err != nil {
				t.Fatal(err)
			}
			readOne(t, p)
			if err := e.Drain(); err != nil {
				t.Fatalf("Drain() = %v", err)
			}
			checkComplete(t, s, 6)
		})
		// Not begun: Drain reads every carrier whole, nested ones included.
		t.Run("drained/"+profile.String(), func(t *testing.T) {
			s, e, _ := Parse(strings.NewReader(text), opts)
			if err := e.Drain(); err != nil {
				t.Fatalf("Drain() = %v", err)
			}
			checkComplete(t, s, 6)
		})
	}
}

// TestWalkCarrierRest: a carrier inside a multipart, whose embedded message
// ends before the carrier does, followed by far more than any read-ahead.
// Walked with the embedded message read to its end, the walk reads the rest of
// the carrier before its multipart goes past it, under every profile and read
// size, and the parse is read whole. Walked with the embedded message's body
// skipped, the carrier is gone past unread and abandoned: reading it later,
// or a Drain, cannot make the parse complete.
func TestWalkCarrierRest(t *testing.T) {
	rest := strings.Repeat("rest of the carrier\r\n", drainCarrierTail/21)
	shapes := []struct {
		name, text string
		limits     Limits
		entities   int64
	}{
		// The embedded message is a leaf whose base64 body a line budget ends
		// early (soak iteration 114063).
		{"cut-leaf", "Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
			"--b\r\nContent-Type: message/rfc822\r\nContent-Transfer-Encoding: 7bit\r\n\r\n" +
			"Subject: in\r\nContent-Transfer-Encoding: base64\r\n\r\n" + strings.Repeat("QUFB", 80) + "\r\n" + rest +
			"--b\r\n\r\nlast\r\n--b--\r\n", Limits{MaxLineBytes: 215}, 4},
		// The embedded message is a multipart closed before the carrier ends.
		{"closed-multipart", "Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
			"--b\r\nContent-Type: message/rfc822\r\n\r\n" +
			"Subject: in\r\nContent-Type: multipart/mixed; boundary=i\r\n\r\n" +
			"--i\r\n\r\ninner\r\n--i--\r\n" + rest +
			"--b\r\n\r\nlast\r\n--b--\r\n", Limits{}, 5},
	}
	type reading struct {
		name string
		src  func(string) io.Reader
	}
	readings := []reading{
		{"whole", func(s string) io.Reader { return strings.NewReader(s) }},
		{"7-byte", func(s string) io.Reader { return &chunkReader{s: s, n: 7} }},
		{"64-byte", func(s string) io.Reader { return &chunkReader{s: s, n: 64} }},
	}
	walk := func(e *Entity, readEmbedded bool) error {
		var carriers []*Entity
		err := e.Walk(func(_ []int, p *Entity, _ error) error {
			switch {
			case p.MultipartReader() != nil:
			case p.CarriesMessage():
				carriers = append(carriers, p)
			case readEmbedded || p.parse.msgDepth == 0:
				io.Copy(io.Discard, p.Body)
			}
			return nil
		})
		for _, c := range carriers {
			io.Copy(io.Discard, c.Body)
		}
		return err
	}
	for _, sh := range shapes {
		for _, profile := range drainProfiles {
			for _, rd := range readings {
				name := sh.name + "/" + profile.String() + "/" + rd.name
				opts := &ParseOptions{Profile: profile, WalkEmbeddedMessages: true, Limits: sh.limits}
				t.Run("read/"+name, func(t *testing.T) {
					s, e, _ := Parse(rd.src(sh.text), opts)
					if err := walk(e, true); err != nil {
						t.Fatalf("Walk = %v", err)
					}
					checkComplete(t, s, sh.entities)
				})
				t.Run("drained/"+name, func(t *testing.T) {
					s, e, _ := Parse(rd.src(sh.text), opts)
					if err := e.Drain(); err != nil {
						t.Fatalf("Drain() = %v", err)
					}
					checkComplete(t, s, sh.entities)
				})
				t.Run("skipped/"+name, func(t *testing.T) {
					s, e, _ := Parse(rd.src(sh.text), opts)
					if err := walk(e, false); err != nil {
						t.Fatalf("Walk = %v", err)
					}
					e.Drain()
					c := s.Completion()
					if c.State != CompletionInProgress || c.BodiesRead == c.Bodies || c.Clean() {
						t.Errorf("the embedded message skipped: %+v", c)
					}
				})
			}
		}
	}
}

// TestDrainResume_replacedBody: a body the caller replaced is framed anew on
// every lookup and is not resumed; the part the original iterator handed out
// is neither finished nor abandoned, and stays unread.
func TestDrainResume_replacedBody(t *testing.T) {
	for _, profile := range drainProfiles {
		t.Run(profile.String(), func(t *testing.T) {
			s, e := resumeParse(t, resumeTwo, profile)
			p, _ := e.MultipartReader().NextPart()
			readOne(t, p)
			e.Body = strings.NewReader("--b\r\n\r\nnew\r\n--b--\r\n")
			if e.MultipartReader() == e.MultipartReader() {
				t.Error("a replaced body is kept as one iterator")
			}
			if err := e.Drain(); err != nil {
				t.Fatalf("Drain() = %v", err)
			}
			if c := s.Completion(); c.State != CompletionInProgress {
				t.Errorf("the original part left unread: %+v", c)
			}
			// The part is still the caller's and reads on.
			if b, err := io.ReadAll(p.Body); string(b) != "irst body" || err != nil {
				t.Errorf("the original part: %q, %v", b, err)
			}
		})
	}
}

// TestDrainResume_legacy: an entity from Read keeps no iterator; every lookup
// frames the body anew, as before.
func TestDrainResume_legacy(t *testing.T) {
	e, err := Read(strings.NewReader(resumeTwo))
	if err != nil {
		t.Fatal(err)
	}
	if e.MultipartReader() == e.MultipartReader() {
		t.Error("Read keeps one iterator")
	}
}
