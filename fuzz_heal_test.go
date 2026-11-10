package message

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// healOutcome reads a message whole — every body, every part, every embedded
// message — and renders what the parse made of it: the tree, the diagnostic
// codes in order, and the completion state.
func healOutcome(t *testing.T, src io.Reader, profile Profile) string {
	t.Helper()
	opts := &ParseOptions{
		Profile:              profile,
		WalkEmbeddedMessages: true,
		Limits:               Limits{MaxInputBytes: 1 << 20, MaxPartCount: 256, MaxMIMEDepth: 16, MaxMessageDepth: 4},
	}
	s, e, err := Parse(src, opts)
	var b strings.Builder
	if e == nil {
		if err == nil {
			t.Fatal("no entity and no error")
		}
		fmt.Fprintf(&b, "no entity: fatal=%v\n", IsFatal(err))
		return b.String()
	}
	werr := e.Walk(func(path []int, p *Entity, err error) error {
		mt, _, _ := p.EffectiveContentType()
		if p.MultipartReader() != nil || p.walkEmbedded() != nil {
			fmt.Fprintf(&b, "%v %s\n", path, mt)
			return nil
		}
		h := sha256.New()
		n, rerr := io.Copy(h, io.LimitReader(p.Body, 2<<20))
		fmt.Fprintf(&b, "%v %s %d %x err=%v\n", path, mt, n, h.Sum(nil)[:4], rerr != nil)
		return nil
	})
	fmt.Fprintf(&b, "walk err=%v\n", werr != nil)
	for _, d := range s.Diagnostics() {
		fmt.Fprintf(&b, "%s %v\n", d.Code, d.Path)
	}
	c := s.Completion()
	fmt.Fprintf(&b, "state=%v entities=%d bodies=%d/%d multiparts=%d/%d\n", c.State, c.Entities, c.BodiesRead, c.Bodies, c.MultipartsRead, c.Multiparts)
	if c.BodiesRead > c.Bodies || c.MultipartsRead > c.Multiparts || c.Entities < 1 {
		t.Fatalf("completion counters do not add up: %+v", c)
	}
	return b.String()
}

var healSeeds = []string{
	"Subject: s\r\n\r\nbody",
	"Subject: s\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\npre\r\n--B\r\nContent-Type: text/plain\r\n\r\none\r\n--B\r\nContent-Transfer-Encoding: base64\r\n\r\naGk=\r\n--B--\r\nepi\r\n",
	"Subject: s\r\nHello Bob,\r\nbody without a separator\r\n",
	"Subject: s\r\ndamaged\r\nTo: a@example.com\r\n\r\nbody",
	"Content-Type: multipart/mixed;\r\nboundary=\"B\"\r\n\r\n--B\r\nno header\r\n--B\r\nContent-Type: text/html\r\n\r\ntwo",
	"Content-Type: multipart/mixed\r\n\r\n--X1\r\nContent-Type: text/plain\r\n\r\none\r\n--X1--\r\n",
	"Content-Type: multipart/mixed; boundary=WRONG\r\n\r\n--X1\r\n\r\none\r\n--X1\r\n\r\ntwo\r\n--X1-",
	"Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: multipart/alternative; boundary=B\r\n\r\n--B\r\n\r\nx\r\n--B--\r\n",
	"Content-Type: message/rfc822\r\nContent-Transfer-Encoding: base64\r\n\r\nU3ViamVjdDogeA0KDQpib2R5",
	"Content-Type: multipart/digest; boundary=D\r\n\r\n--D\r\n\r\nSubject: a\r\n\r\nb\r\n--D--\r\n",
	"Content-Transfer-Encoding: quoted-printable\r\n\r\na=\rb= \r\nc=ZZ=4",
	"Content-Transfer-Encoding: x-uuencode\r\n\r\nbegin 644 a\n#0V%T\n`\nend\n",
	"Content-Type: text/plain; charset=utf-8\r\n\r\n\xff\xfecaf\xe9",
	"Content-Type: text; charset=\"utf-8\r\nContent-Type: text/html\r\n\r\nx",
	"\xef\xbb\xbfSubject: bom\r\n \r\nbody",
	"From a@b Thu Jan  1 00:00:00 2026\r\nSubject: s\r\n\r\nbody",
	"A: 1\rB: 2\r\rbody",
	"Content-Type: multipart/mixed; boundary=B\n\n--B\n\none\n--B--",
}

// FuzzParse_heal checks, for arbitrary bytes, that the healing parse never
// panics or loops, that reading the message a byte at a time yields the same
// tree, diagnostics and completion as reading it at once, and that
// ProfileStrict fares no worse than ending cleanly.
func FuzzParse_heal(f *testing.F) {
	for _, s := range healSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 16<<10 {
			return
		}
		for _, profile := range []Profile{ProfileTolerant, ProfileForensic} {
			whole := healOutcome(t, bytes.NewReader(data), profile)
			bytewise := healOutcome(t, iotest.OneByteReader(bytes.NewReader(data)), profile)
			if whole != bytewise {
				t.Fatalf("%v: reading a byte at a time changed the result\nwhole:\n%s\nbytewise:\n%s", profile, whole, bytewise)
			}
		}
		healOutcome(t, bytes.NewReader(data), ProfileStrict)
	})
}

// FuzzTolerantDecoders checks that each tolerant transfer decoder ends on any
// input, yields the same bytes however the input is chunked, and never yields
// more than the input could hold.
func FuzzTolerantDecoders(f *testing.F) {
	for _, s := range []string{"aGVsbG8gd29ybGQ=", "aGk=\r\naGk", "a=\r\nb=3D= \n=", "begin 644 a\n#0V%T\n`\nend\n", "_-8=!!", strings.Repeat("=", 64)} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 16<<10 {
			return
		}
		for _, enc := range []string{"base64", "quoted-printable", "x-uuencode"} {
			var outs [2][]byte
			for i, src := range []io.Reader{bytes.NewReader(data), iotest.OneByteReader(bytes.NewReader(data))} {
				dec, err := encodingReader(enc, src)
				if err != nil {
					t.Fatal(err)
				}
				out, rerr := io.ReadAll(io.LimitReader(dec, int64(len(data))+64))
				if rerr != nil {
					t.Fatalf("%s: %v", enc, rerr)
				}
				outs[i] = out
			}
			if !bytes.Equal(outs[0], outs[1]) {
				t.Fatalf("%s: chunking changed the output: %q vs %q", enc, outs[0], outs[1])
			}
			// No decoder expands: a uuencoded line padded for stripped
			// trailing spaces yields at most one and a half times its length.
			if len(outs[0]) > len(data)*3/2+2 {
				t.Fatalf("%s: %d bytes out of %d in", enc, len(outs[0]), len(data))
			}
		}
	})
}
