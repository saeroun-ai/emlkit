package message

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

// ciAtMost delivers r at most n bytes a read.
type ciAtMost struct {
	r io.Reader
	n int
}

func (c *ciAtMost) Read(p []byte) (int, error) {
	if len(p) > c.n {
		p = p[:c.n]
	}
	return c.r.Read(p)
}

// ciDeliveries are the ways an input is handed to the parser.
var ciDeliveries = []struct {
	name string
	wrap func([]byte) io.Reader
}{
	{"whole", func(b []byte) io.Reader { return bytes.NewReader(b) }},
	{"1 byte", func(b []byte) io.Reader { return iotest.OneByteReader(bytes.NewReader(b)) }},
	{"7 bytes", func(b []byte) io.Reader { return &ciAtMost{bytes.NewReader(b), 7} }},
	{"509 bytes", func(b []byte) io.Reader { return &ciAtMost{bytes.NewReader(b), 509} }},
}

// ciOutcome is everything a walk of the message in src establishes: every
// entity, every leaf's size, digest and error, what each unreadable embedded
// message left of its carrier, every diagnostic with its place, in order, and
// Completion. InputBytes and InputEnded are left out: what follows a closing
// delimiter is not read, and how much of it a buffer fetched is not a result.
func ciOutcome(src io.Reader, opts ParseOptions) string {
	var b strings.Builder
	s, e, err := Parse(src, &opts)
	if e == nil {
		return fmt.Sprintf("no entity: %v", err)
	}
	read := func(r io.Reader) string {
		h := sha256.New()
		n, rerr := io.Copy(h, r)
		return fmt.Sprintf("%d %x err=%v", n, h.Sum(nil)[:6], rerr)
	}
	var carriers []*Entity
	werr := e.Walk(func(path []int, p *Entity, warn error) error {
		mt, _, _ := p.EffectiveContentType()
		switch {
		case p.MultipartReader() != nil:
			fmt.Fprintf(&b, "%v %s\n", path, mt)
		case opts.WalkEmbeddedMessages && p.CarriesMessage():
			fmt.Fprintf(&b, "%v %s carrier\n", path, mt)
			carriers = append(carriers, p)
		default:
			fmt.Fprintf(&b, "%v %s %s warn=%v\n", path, mt, read(p.Body), warn != nil)
		}
		return nil
	})
	fmt.Fprintf(&b, "walk: %v\n", werr)
	for _, c := range carriers {
		fmt.Fprintf(&b, "carrier rest %s\n", read(c.Body))
	}
	for _, d := range s.Diagnostics() {
		fmt.Fprintf(&b, "%s %s %v %+v\n", d.Severity, d.Code, d.Path, d.Location)
	}
	c := s.Completion()
	fmt.Fprintf(&b, "state=%v entities=%d bodies=%d/%d multiparts=%d/%d ambiguous=%v stop=%v\n",
		c.State, c.Entities, c.BodiesRead, c.Bodies, c.MultipartsRead, c.Multiparts, c.Ambiguous, c.Stop)
	return b.String()
}

// ciFixtures are the corpus and the inputs the soak saved.
func ciFixtures(t *testing.T) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte)
	for _, pattern := range []string{
		filepath.Join("testdata", "corpus", "*.eml"),
		filepath.Join("mail", "testdata", "soak", "*.eml"),
	} {
		files, err := filepath.Glob(pattern)
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: no fixtures (%v)", pattern, err)
		}
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			out[filepath.Base(f)] = data
		}
	}
	return out
}

// TestChunkInvariance_fixtures holds every fixture to one outcome under the
// healing profiles, however it is delivered: the entities, the bodies, the
// diagnostics in order and Completion. It pins what the corpus recorded as
// byteAtATime differences (forwarded-rfc822-attachment's Completion,
// bulk-sloppy-encodings' diagnostic order) and the soak's findings.
func TestChunkInvariance_fixtures(t *testing.T) {
	for name, data := range ciFixtures(t) {
		for _, profile := range []Profile{ProfileTolerant, ProfileForensic} {
			for _, embedded := range []bool{false, true} {
				opts := ParseOptions{Profile: profile, WalkEmbeddedMessages: embedded}
				want := ciOutcome(ciDeliveries[0].wrap(data), opts)
				for _, d := range ciDeliveries[1:] {
					if got := ciOutcome(d.wrap(data), opts); got != want {
						t.Errorf("%s, %v, embedded=%v, read %s:\n%s\nread whole:\n%s", name, profile, embedded, d.name, got, want)
					}
				}
			}
		}
	}
}

// TestChunkInvariance_notesByPosition pins the order of a body's diagnostics:
// by position, the transfer decoder's among the charset monitor's. The
// decoder used to report every repair of the bytes one read gave it before
// its consumer saw the first of them.
func TestChunkInvariance_notesByPosition(t *testing.T) {
	const head = "Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n"
	for _, tc := range []struct {
		body string
		want []Code
	}{
		// Invalid UTF-8 first, then a bad escape.
		{"caf=E9 au lait\r\nprice =ZZ\r\nend\r\n", []Code{CodeCharsetInvalidSequence, CodeCTEQPInvalidHex}},
		// The bad escape first.
		{"price =ZZ\r\ncaf=E9 au lait\r\nend\r\n", []Code{CodeCTEQPInvalidHex, CodeCharsetInvalidSequence}},
	} {
		for _, d := range ciDeliveries {
			s, e, err := Parse(d.wrap([]byte(head+tc.body)), nil)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, e.Body)
			var got []Code
			for _, diag := range s.Diagnostics() {
				got = append(got, diag.Code)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("%q read %s: %v, want %v", tc.body, d.name, got, tc.want)
			}
		}
	}
}

// TestChunkInvariance_carrierRestored: an embedded header that cannot be read
// leaves its carrier a leaf with its whole body, whatever the buffers held
// (soak seed 1300007, iterations 480, 3722, 4606 and 18589).
func TestChunkInvariance_carrierRestored(t *testing.T) {
	inner := "Subject: " + strings.Repeat("x", 600) + "\r\n\r\n" + strings.Repeat("body line\r\n", 900)
	in := []byte("Content-Type: message/rfc822\r\n\r\n" + inner)
	limits := DefaultLimits()
	limits.MaxHeaderFieldBytes = 256
	for _, d := range ciDeliveries {
		_, e, err := Parse(d.wrap(in), &ParseOptions{Limits: limits})
		if err != nil {
			t.Fatal(err)
		}
		emb, eerr := e.EmbeddedMessage()
		if emb != nil || eerr == nil {
			t.Fatalf("read %s: the embedded message was read (%v)", d.name, eerr)
		}
		rest, rerr := io.ReadAll(e.Body)
		if rerr != nil || string(rest) != inner {
			t.Errorf("read %s: the carrier keeps %d of its %d bytes (err %v)", d.name, len(rest), len(inner), rerr)
		}
	}
}

// TestChunkInvariance_unreadableCarrierWalked: a walk reads a carrier whose
// message it could not descend into to its end before it goes on, so what the
// carrier's decoder finds at its end is reported whatever the buffers held.
// Read after the walk, as Drain did, the body ended wherever the decoder's
// buffer had when the multipart moved on (soak seed 1300007, iteration 87148:
// cte.base64_length_invalid, and with it the state incomplete, came and went
// with the read size).
func TestChunkInvariance_unreadableCarrierWalked(t *testing.T) {
	inner := "Subject: " + strings.Repeat("x", 600) + "\r\n\r\n" + strings.Repeat("body line\r\n", 900)
	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: message/rfc822\r\nContent-Transfer-Encoding: base64\r\n\r\n")
	enc := base64.StdEncoding.EncodeToString([]byte(inner))
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	// One character after the padded last group: no whole byte.
	b.WriteString(enc + "\r\nQ\r\n--b\r\nContent-Type: text/plain\r\n\r\nlast\r\n--b--\r\n")
	limits := DefaultLimits()
	limits.MaxHeaderFieldBytes = 256
	for _, d := range ciDeliveries {
		s, e, err := Parse(d.wrap([]byte(b.String())), &ParseOptions{WalkEmbeddedMessages: true, Limits: limits})
		if err != nil {
			t.Fatal(err)
		}
		if derr := e.Drain(); derr != nil {
			t.Fatal(derr)
		}
		var codes []string
		for _, diag := range s.Diagnostics() {
			codes = append(codes, fmt.Sprintf("%s %v", diag.Code, diag.Path))
		}
		want := "[message.illegal_transfer_encoding [0] message.embedded_invalid [0] cte.base64_data_after_padding [0] cte.base64_length_invalid [0]]"
		if state := s.Completion().State; fmt.Sprint(codes) != want || state != CompletionIncomplete {
			t.Errorf("read %s: state %v, diagnostics %v; want incomplete, %s", d.name, state, codes, want)
		}
	}
}

// TestChunkInvariance_carrierRead: a walk that leaves an embedded multipart
// reads what is left of its carrier, so Completion does not depend on whether
// a buffer had fetched the bytes after the embedded closing delimiter
// (corpus fixture forwarded-rfc822-attachment), and what the carrier's
// decoder finds there is reported.
func TestChunkInvariance_carrierRead(t *testing.T) {
	inner := "Content-Type: multipart/mixed; boundary=in\r\n\r\n--in\r\n\r\npart\r\n--in--\r\n" +
		strings.Repeat("epilogue line\r\n", 700) + "bad escape =ZZ\r\nend\r\n"
	in := []byte("Content-Type: message/rfc822\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" + inner)
	for _, d := range ciDeliveries {
		s, e, err := Parse(d.wrap(in), &ParseOptions{WalkEmbeddedMessages: true})
		if err != nil {
			t.Fatal(err)
		}
		if werr := e.Walk(func(_ []int, p *Entity, _ error) error {
			if p.MultipartReader() == nil && !p.CarriesMessage() {
				io.Copy(io.Discard, p.Body)
			}
			return nil
		}); werr != nil {
			t.Fatal(werr)
		}
		var codes []Code
		for _, diag := range s.Diagnostics() {
			codes = append(codes, diag.Code)
		}
		want := []Code{CodeMessageIllegalTransferEncoding, CodeCTEQPInvalidHex}
		if state := s.Completion().State; state != CompletionComplete || fmt.Sprint(codes) != fmt.Sprint(want) {
			t.Errorf("read %s: state %v, diagnostics %v; want complete, %v", d.name, state, codes, want)
		}
	}
}

// TestChunkInvariance_legacyBase64 records where delivery still shows, and is
// left to: the standard library's streaming Base64 decoder, which
// ProfileStrict keeps, accepts data after padding when a read ended at the
// padding and refuses it otherwise. Read, ReadWithOptions and New decode
// with the tolerant decoder, and so does every healing profile: they must not
// differ.
func TestChunkInvariance_legacyBase64(t *testing.T) {
	for _, name := range []string{"base64-data-after-padding", "base64-junk-and-missing-padding"} {
		data, err := os.ReadFile(filepath.Join("testdata", "corpus", name+".eml"))
		if err != nil {
			t.Fatal(err)
		}
		body := func(e *Entity, err error) string {
			if e == nil {
				return fmt.Sprintf("no entity: %v", err)
			}
			b, rerr := io.ReadAll(e.Body)
			return fmt.Sprintf("%x err=%v", sha256.Sum256(b), rerr)
		}
		whole, small := body(Read(bytes.NewReader(data))), body(Read(iotest.OneByteReader(bytes.NewReader(data))))
		if whole != small {
			t.Errorf("%s, Read: a byte at a time %s, whole %s", name, small, whole)
		}
		strict := func(src io.Reader) string {
			_, e, err := Parse(src, &ParseOptions{Profile: ProfileStrict})
			return body(e, err)
		}
		if whole, small := strict(bytes.NewReader(data)), strict(iotest.OneByteReader(bytes.NewReader(data))); whole != small {
			t.Logf("%s, ProfileStrict (the legacy decoder, left as it is): a byte at a time %s, whole %s", name, small, whole)
		}
	}
}
