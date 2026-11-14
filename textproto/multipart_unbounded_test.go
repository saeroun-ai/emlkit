package textproto

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// HeaderLimits.UnboundedFramingLines (#64): with no line budget, a framing line
// of any length is read, rather than one that does not fit a buffer ending the
// iteration. The oracle is the reader under a line budget far larger than any
// line here, which accumulates every framing line whole: the unbounded reader
// must see exactly what it sees, however the input is chunked, while holding
// none of a framing line read before the first part.

// unboundedLimits is no line budget and no buffer bound.
func unboundedLimits(recover bool) HeaderLimits {
	return HeaderLimits{MaxLineBytes: -1, UnboundedFramingLines: true, Recover: recover}
}

// oracleLimits is a line budget no line here comes near.
func oracleLimits(recover bool) HeaderLimits {
	return HeaderLimits{MaxLineBytes: 1 << 30, Recover: recover}
}

// mpSnapshot is everything a caller can observe of one multipart iteration.
type mpSnapshot struct {
	parts    []string
	final    string
	preamble string
	preOver  bool
	framing  MultipartFraming
	endings  string
	epilogue string
}

func snapshotMultipart(src io.Reader, boundary string, lim HeaderLimits, preLimit int64) mpSnapshot {
	var s mpSnapshot
	r := NewMultipartReader(src, boundary)
	r.SetHeaderLimits(lim)
	if preLimit != 0 {
		r.SetPreambleLimit(preLimit)
	}
	for i := 0; ; i++ {
		if i == 64 {
			s.final = "too many parts"
			break
		}
		p, err := r.NextPart()
		if err != nil {
			s.final = err.Error()
			break
		}
		var hb bytes.Buffer
		WriteHeaderAsRead(&hb, p.Header)
		body, berr := io.ReadAll(p)
		s.parts = append(s.parts, fmt.Sprintf("h@%d b@%d sep=%v hdr=%q body=%q err=%v",
			p.HeaderOffset(), p.BodyOffset(), p.HeaderSeparatorMissing(), hb.String(), body, berr))
	}
	pre, over := r.Preamble()
	s.preamble, s.preOver = string(pre), over
	s.framing = r.Framing()
	s.endings = fmt.Sprintf("%+v", r.DelimiterEndings())
	if rd, off, ok := r.Epilogue(); ok {
		b, err := io.ReadAll(rd)
		s.epilogue = fmt.Sprintf("@%d %q %v", off, b, err)
	}
	return s
}

func (s mpSnapshot) diff(o mpSnapshot) string {
	switch {
	case len(s.parts) != len(o.parts):
		return fmt.Sprintf("parts: %d vs %d (final %q vs %q)", len(s.parts), len(o.parts), s.final, o.final)
	case s.final != o.final:
		return fmt.Sprintf("final: %q vs %q", s.final, o.final)
	case s.preamble != o.preamble || s.preOver != o.preOver:
		return fmt.Sprintf("preamble: %d bytes %v vs %d bytes %v", len(s.preamble), s.preOver, len(o.preamble), o.preOver)
	case s.framing != o.framing:
		return fmt.Sprintf("framing: %+v vs %+v", s.framing, o.framing)
	case s.endings != o.endings:
		return fmt.Sprintf("endings: %s vs %s", s.endings, o.endings)
	case s.epilogue != o.epilogue:
		return "epilogue differs"
	}
	for i := range s.parts {
		if s.parts[i] != o.parts[i] {
			return fmt.Sprintf("part %d differs", i)
		}
	}
	return ""
}

// unboundedCase is one multipart body with a framing line longer than a
// reader's buffer, and how many parts it holds.
type unboundedCase struct {
	name  string
	body  func(eol string) string
	parts int
}

func unboundedCases() []unboundedCase {
	pad := strings.Repeat(" \t", 5001) // 10002 bytes of transport padding
	long := strings.Repeat("p", 10000)
	tail := func(eol string) string {
		return "H: v" + eol + eol + "one" + eol + "--B--" + eol + "epi"
	}
	return []unboundedCase{
		{"long preamble line", func(e string) string { return long + e + "--B" + e + tail(e) }, 1},
		{"delimiter-like preamble line", func(e string) string { return "--B" + pad + "x" + e + "--B" + e + tail(e) }, 1},
		{"dash then padding is preamble", func(e string) string { return "--B-" + pad + e + "--B" + e + tail(e) }, 1},
		{"padded first delimiter", func(e string) string { return "--B" + pad + e + tail(e) }, 1},
		{"padded first closing delimiter", func(e string) string { return long + e + "--B--" + pad + e + "epi" }, 0},
		{"padded closing delimiter in body", func(e string) string {
			return "--B" + e + e + "one" + e + "--B--" + pad + e + "epi"
		}, 1},
		{"padded delimiter in body", func(e string) string {
			return "--B" + e + e + "one" + e + "--B" + pad + e + e + "two" + e + "--B--" + e
		}, 2},
		{"delimiter-like line in body", func(e string) string {
			return "--B" + e + e + "one" + e + "--B" + pad + "x" + e + "more" + e + "--B--" + e
		}, 1},
		{"long body lines", func(e string) string {
			return "--B" + e + e + long + e + long + e + "--B--" + e
		}, 1},
		{"long epilogue line", func(e string) string { return "--B" + e + e + "one" + e + "--B--" + e + long + e + long }, 1},
		{"padded delimiter after malformed prefix", func(e string) string {
			return "--B" + e + " bad" + e + "--B" + pad + e + e + "two" + e + "--B--" + e
		}, 2},
		{"padded delimiter cut by the end", func(e string) string { return "--B" + e + e + "one" + e + "--B" + pad }, 1},
		{"padded first delimiter cut by the end", func(e string) string { return long + e + "--B" + pad }, 0},
		{"long preamble cut by the end", func(e string) string { return long + e + long }, 0},
		{"no padding at all", func(e string) string { return "pre" + e + "--B" + e + tail(e) }, 1},
	}
}

// unboundedChunkings are the deliveries every case is read under.
func unboundedChunkings(text string) map[string]func() io.Reader {
	m := map[string]func() io.Reader{
		"whole": func() io.Reader { return strings.NewReader(text) },
	}
	for _, n := range []int{1, 2, 16, 4095, 4096, 4097} {
		m[fmt.Sprintf("chunks of %d", n)] = func() io.Reader { return &chunkReader{s: text, n: n} }
	}
	for _, seed := range []int64{1, 2} {
		m[fmt.Sprintf("random seed %d", seed)] = func() io.Reader { return newRandomChunkReader(text, 5000, seed) }
	}
	return m
}

var unboundedEOLs = []struct{ name, eol string }{{"crlf", "\r\n"}, {"lf", "\n"}, {"cr", "\r"}}

// TestMultipartReader_unboundedFramingMatchesBudget pins that the unbounded
// reader iterates every case exactly as a reader under a sufficient budget
// does — parts, offsets, headers, bodies, errors, preamble kept and cut,
// framing, line endings and epilogue — at every chunking and line ending, with
// recovery on and off, and with a preamble limit that cuts the long lines.
func TestMultipartReader_unboundedFramingMatchesBudget(t *testing.T) {
	for _, tc := range unboundedCases() {
		for _, e := range unboundedEOLs {
			text := tc.body(e.eol)
			for _, recover := range []bool{false, true} {
				for _, preLimit := range []int64{0, 7, 10001} {
					if !recover && preLimit != 0 {
						continue
					}
					want := snapshotMultipart(strings.NewReader(text), "B", oracleLimits(recover), preLimit)
					if e.eol != "\r" && len(want.parts) != tc.parts {
						t.Fatalf("%s/%s: the oracle read %d parts (%q), want %d", tc.name, e.name, len(want.parts), want.final, tc.parts)
					}
					for name, src := range unboundedChunkings(text) {
						got := snapshotMultipart(src(), "B", unboundedLimits(recover), preLimit)
						if d := got.diff(want); d != "" {
							t.Errorf("%s/%s/recover=%v/preamble %d/%s: %s", tc.name, e.name, recover, preLimit, name, d)
						}
					}
				}
			}
		}
	}
}

// TestMultipartReader_unboundedFramingReadsWhatLegacyRefuses pins the
// difference itself: the cases a reader with no line budget and no flag refuses
// at its buffer are read whole with the flag.
func TestMultipartReader_unboundedFramingReadsWhatLegacyRefuses(t *testing.T) {
	refusedSomewhere := false
	for _, tc := range unboundedCases() {
		text := tc.body("\r\n")
		legacy := snapshotMultipart(strings.NewReader(text), "B", HeaderLimits{MaxLineBytes: -1}, 0)
		if strings.Contains(legacy.final, bufio.ErrBufferFull.Error()) {
			refusedSomewhere = true
		}
		got := snapshotMultipart(strings.NewReader(text), "B", unboundedLimits(true), 0)
		if strings.Contains(got.final, bufio.ErrBufferFull.Error()) || len(got.parts) != tc.parts {
			t.Errorf("%s: unbounded read %d parts, final %q; want %d and no buffer failure", tc.name, len(got.parts), got.final, tc.parts)
		}
	}
	if !refusedSomewhere {
		t.Fatal("no case is refused by the legacy reader; the cases say nothing about #64")
	}
}

// genSource delivers head, then n copies of fill, then tail, generated as it
// is read, so a line of any length costs the test nothing to hold.
type genSource struct {
	head, tail string
	fill       byte
	n          int64
	pos        int64
	read       int64
}

func (g *genSource) Read(p []byte) (int, error) {
	total := int64(len(g.head)) + g.n + int64(len(g.tail))
	if g.pos >= total {
		return 0, io.EOF
	}
	k := 0
	for k < len(p) && g.pos < total {
		switch {
		case g.pos < int64(len(g.head)):
			c := copy(p[k:], g.head[g.pos:])
			k += c
			g.pos += int64(c)
		case g.pos < int64(len(g.head))+g.n:
			c := min(int64(len(p)-k), int64(len(g.head))+g.n-g.pos)
			for range c {
				p[k] = g.fill
				k++
			}
			g.pos += c
		default:
			off := g.pos - int64(len(g.head)) - g.n
			c := copy(p[k:], g.tail[off:])
			k += c
			g.pos += int64(c)
		}
	}
	g.read += int64(k)
	return k, nil
}

// TestMultipartReader_unboundedFramingHoldsNothing pins, by what the reader
// holds rather than by timing, that a framing line before the first part is
// streamed: after a 4 MiB preamble line and a 4 MiB padded delimiter line the
// reader holds its one buffer, no line accumulator, no candidate, and a kept
// preamble no larger than its limit.
func TestMultipartReader_unboundedFramingHoldsNothing(t *testing.T) {
	const n = 4 << 20
	for _, tc := range []struct {
		name string
		src  *genSource
	}{
		{"preamble line", &genSource{fill: 'p', n: n, tail: "\r\n--B\r\n\r\none\r\n--B--\r\n"}},
		{"padded delimiter", &genSource{head: "--B", fill: ' ', n: n, tail: "\r\n\r\none\r\n--B--\r\n"}},
		{"delimiter-like preamble line", &genSource{head: "--B", fill: '\t', n: n, tail: "x\r\n--B\r\n\r\none\r\n--B--\r\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewMultipartReader(tc.src, "B")
			r.SetHeaderLimits(unboundedLimits(true))
			r.SetPreambleLimit(1024)
			p, err := r.NextPart()
			if err != nil {
				t.Fatalf("NextPart() = %v", err)
			}
			if got := readPartBody(t, p); got != "one" {
				t.Errorf("body = %q, want %q", got, "one")
			}
			if cap(r.lineBuf) != 0 || r.bufReader.Size() != peekBufferSize || r.cand.held != nil || r.out != nil {
				t.Errorf("reader holds lineBuf %d, buffer %d, candidate %d, out %d; want one buffer and nothing else",
					cap(r.lineBuf), r.bufReader.Size(), cap(r.cand.held), cap(r.out))
			}
			if len(r.preamble) > 1024 {
				t.Errorf("kept %d preamble bytes, want at most the limit", len(r.preamble))
			}
			if _, err := r.NextPart(); err != io.EOF {
				t.Errorf("NextPart() = %v, want io.EOF", err)
			}
		})
	}
}

// TestMultipartReader_unboundedCandidateIsHeldToItsLength pins the one place
// that still holds bytes, and that it is honest about it: a padded delimiter
// candidate inside a body is held until it is decided, so what is held grows
// with its padding — and only with it, the candidate's own length — and
// whichever way it is decided it is then released.
func TestMultipartReader_unboundedCandidateIsHeldToItsLength(t *testing.T) {
	const n = 1 << 20
	for _, tc := range []struct {
		name, tail string
		parts      int
		body       string
	}{
		{"a delimiter", "\r\n\r\ntwo\r\n--B--\r\n", 2, "one"},
		{"body after all", "x\r\n--B--\r\n", 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &genSource{head: "--B\r\n\r\none\r\n--B", fill: ' ', n: n, tail: tc.tail}
			r := NewMultipartReader(src, "B")
			r.SetHeaderLimits(unboundedLimits(true))
			p, err := r.NextPart()
			if err != nil {
				t.Fatalf("NextPart() = %v", err)
			}
			buf := make([]byte, 512)
			var body int64
			maxHeld := 0
			for {
				k, err := p.Read(buf)
				body += int64(k)
				maxHeld = max(maxHeld, len(r.cand.held), len(r.out))
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("Read() = %v", err)
				}
			}
			// The last buffer of padding may still be the buffered reader's
			// when the byte after it decides the candidate.
			if maxHeld < n-peekBufferSize || maxHeld > n+len("\r\n--B")+peekBufferSize {
				t.Errorf("held at most %d bytes, want the candidate's length, %d and its name", maxHeld, n)
			}
			parts := 1
			for {
				p, err := r.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("NextPart() = %v", err)
				}
				io.Copy(io.Discard, p)
				parts++
			}
			if parts != tc.parts {
				t.Errorf("read %d parts, want %d", parts, tc.parts)
			}
			if r.cand.held != nil || r.out != nil {
				t.Errorf("the candidate is still held after it was decided")
			}
		})
	}
}

// TestMultipartReader_unboundedFramingInputFailure pins that a failure of the
// input inside a long framing line is reported as the unbounded reader's
// oracle reports it, the failure itself and not a refusal, at every cut.
func TestMultipartReader_unboundedFramingInputFailure(t *testing.T) {
	cause := errors.New("input broke")
	for _, tc := range unboundedCases() {
		text := tc.body("\r\n")
		for _, cut := range []int{1, 3, 4096, 5000, 9999, 10002, 10005, len(text) - 1} {
			if cut >= len(text) {
				continue
			}
			for _, recover := range []bool{false, true} {
				mk := func() io.Reader { return &failAtSource{data: text, cut: cut, chunk: 1000, err: cause, sticky: true} }
				want := snapshotMultipart(mk(), "B", oracleLimits(recover), 0)
				got := snapshotMultipart(mk(), "B", unboundedLimits(recover), 0)
				if d := got.diff(want); d != "" {
					t.Errorf("%s/cut %d/recover=%v: %s", tc.name, cut, recover, d)
				}
				if strings.Contains(got.final, bufio.ErrBufferFull.Error()) || strings.Contains(got.final, ErrLineTooBig.Error()) {
					t.Errorf("%s/cut %d: final %q, want no refusal", tc.name, cut, got.final)
				}
			}
		}
	}
}

// TestDelimLineMatcher_matchesDelimiterLine pins the streaming matcher against
// delimiterLine on every short line over the bytes the grammar turns on, fed
// whole and split at every position.
func TestDelimLineMatcher_matchesDelimiterLine(t *testing.T) {
	alphabet := []byte{'-', 'B', ' ', '\t', 'x'}
	for _, boundary := range []string{"B", "-", "B-", "BB"} {
		r := NewMultipartReader(strings.NewReader(""), boundary)
		var line []byte
		var walk func(depth int)
		walk = func(depth int) {
			wok, wclose := r.delimiterLine(line)
			for split := 0; split <= len(line); split++ {
				m := delimLineMatcher{db: r.dashBoundary}
				m.feed(line[:split])
				m.feed(line[split:])
				if ok, close := m.result(); ok != wok || close != wclose {
					t.Fatalf("boundary %q line %q split %d: matcher (%v, %v), delimiterLine (%v, %v)", boundary, line, split, ok, close, wok, wclose)
				}
			}
			if depth == 0 {
				return
			}
			for _, c := range alphabet {
				line = append(line, c)
				walk(depth - 1)
				line = line[:len(line)-1]
			}
		}
		walk(7)
	}
}
