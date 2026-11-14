package message

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
)

// Regression tests of #73 (ROB-110-005): a header block a recorded Source
// kept stays served whatever the parse reads of the body after it, and the
// header of a message carried as it is needs no index of the whole input.
// See docs/robustness-notes/raw-source.md.

// srcHdrBodyLen is longer than the record limit of the tests and than the
// buffers of the parse, so the body is never kept and never read ahead whole.
const srcHdrBodyLen = 10000

// A srcHdrCase is an entity whose header block lies within the first
// srcHdrLimit bytes of the input and whose body runs past them.
type srcHdrCase struct {
	data   string
	header string // the raw header block of the entity under test
	body   string // its decoded body
	// pick returns the entity under test, and a func that reads what is left
	// of the input once its body has been read.
	pick func(t *testing.T, root *Entity) (*Entity, func())
}

const srcHdrLimit = 512

func srcHdrCases() map[string]srcHdrCase {
	x := strings.Repeat("x", srcHdrBodyLen)
	y := strings.Repeat("y", srcHdrBodyLen)
	partEnd := func(t *testing.T, mr MultipartReader) func() {
		return func() {
			if _, err := mr.NextPart(); err != io.EOF {
				t.Errorf("NextPart after the part = %v, want io.EOF", err)
			}
		}
	}
	return map[string]srcHdrCase{
		"root": {
			data:   "Subject: root\r\nContent-Type: text/plain\r\n\r\n" + x,
			header: "Subject: root\r\nContent-Type: text/plain\r\n\r\n",
			body:   x,
			pick: func(t *testing.T, root *Entity) (*Entity, func()) {
				return root, func() {}
			},
		},
		"child": {
			// The input of #73.
			data: "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nSubject: child\r\nContent-Type: text/plain\r\n\r\n" +
				x + "\r\n--b--\r\n",
			header: "Subject: child\r\nContent-Type: text/plain\r\n\r\n",
			body:   x,
			pick: func(t *testing.T, root *Entity) (*Entity, func()) {
				mr := root.MultipartReader()
				p, err := mr.NextPart()
				if err != nil {
					t.Fatalf("NextPart = %v", err)
				}
				return p, partEnd(t, mr)
			},
		},
		"embedded in a part": {
			data: "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: message/rfc822\r\n\r\n" +
				"Subject: inner\r\nContent-Type: text/plain\r\n\r\n" + y + "\r\n--b--\r\n",
			header: "Subject: inner\r\nContent-Type: text/plain\r\n\r\n",
			body:   y,
			pick: func(t *testing.T, root *Entity) (*Entity, func()) {
				mr := root.MultipartReader()
				p, err := mr.NextPart()
				if err != nil {
					t.Fatalf("NextPart = %v", err)
				}
				emb, err := p.EmbeddedMessage()
				if emb == nil {
					t.Fatalf("EmbeddedMessage = %v", err)
				}
				return emb, partEnd(t, mr)
			},
		},
		"embedded in the root": {
			data:   "Content-Type: message/rfc822\r\n\r\nSubject: inner\r\nContent-Type: text/plain\r\n\r\n" + y,
			header: "Subject: inner\r\nContent-Type: text/plain\r\n\r\n",
			body:   y,
			pick: func(t *testing.T, root *Entity) (*Entity, func()) {
				emb, err := root.EmbeddedMessage()
				if emb == nil {
					t.Fatalf("EmbeddedMessage = %v", err)
				}
				return emb, func() {}
			},
		},
	}
}

// RawHeader returns the same kept bytes before, while and after the body
// past the record limit is read, any number of times; the raw reads that need
// the body fail with ErrRawNotRecorded, and Body is not disturbed.
func TestSrcRawHeaderPastRecordLimit(t *testing.T) {
	for name, c := range srcHdrCases() {
		for kind, opts := range map[string]func() *RecordOptions{
			"memory limit": func() *RecordOptions { return &RecordOptions{MaxBytes: srcHdrLimit} },
			"store fails":  func() *RecordOptions { return &RecordOptions{MaxBytes: -1, Store: &srcTestStore{failAt: srcHdrLimit}} },
		} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				src := NewRecordedSource(strings.NewReader(c.data), opts())
				_, root, err := ParseSource(context.Background(), src, nil)
				if root == nil {
					t.Fatalf("ParseSource = %v", err)
				}
				e, finish := c.pick(t, root)
				if o, _ := e.Origin(); o.BodyOffset > srcHdrLimit || !strings.HasPrefix(c.data[o.HeaderOffset:], c.header) {
					t.Fatalf("origin %+v: the header block is not where the test expects it", o)
				}
				check := func(when string) {
					t.Helper()
					for i := 0; i < 2; i++ {
						if got := srcTestRaw(e.RawHeader()); got != "ok:"+c.header {
							t.Errorf("%s, call %d: RawHeader = %.80q, want %q", when, i, got, c.header)
						}
					}
				}

				check("before the body")
				early, err := e.RawHeader()
				if err != nil {
					t.Fatalf("RawHeader = %v", err)
				}
				head := make([]byte, 100)
				n, _ := io.ReadFull(e.Body, head)
				check("while the body is read")
				rest, rerr := io.ReadAll(e.Body)
				if got := string(head[:n]) + string(rest); got != c.body || rerr != nil {
					t.Errorf("Body = %d bytes, %v; want the %d bytes of the body", len(got), rerr, len(c.body))
				}
				finish()
				if _, err := io.Copy(io.Discard, root.Body); err != nil {
					t.Errorf("reading the rest of the input: %v", err)
				}
				if !src.Truncated() {
					t.Fatal("the recording kept the whole input: the test checks nothing")
				}
				if failed := src.Err() != nil; failed != (kind == "store fails") {
					t.Errorf("Err = %v", src.Err())
				}

				check("after the body")
				// A reader handed out before the body was read reads the same
				// bytes after.
				if got := srcTestRaw(early, nil); got != "ok:"+c.header {
					t.Errorf("RawHeader taken before the body, read after = %.80q", got)
				}
				for what, err := range map[string]error{
					"RawReader":   srcTestErr(e.RawReader()),
					"RawBody":     srcTestErr(e.RawBody()),
					"DecodedBody": func() error { _, err := e.DecodedBody(); return err }(),
				} {
					if !errors.Is(err, ErrRawNotRecorded) {
						t.Errorf("%s after the body = %v, want ErrRawNotRecorded", what, err)
					}
				}
			})
		}
	}
}

// A header block the recording did not keep whole is not served, before or
// after its body is read.
func TestSrcRawHeaderCutByRecordLimit(t *testing.T) {
	for name, c := range srcHdrCases() {
		probe := NewRecordedSource(strings.NewReader(c.data), nil)
		_, root, _ := ParseSource(context.Background(), probe, nil)
		e, _ := c.pick(t, root)
		o, _ := e.Origin()

		src := NewRecordedSource(strings.NewReader(c.data), &RecordOptions{MaxBytes: o.BodyOffset - 1})
		_, root, err := ParseSource(context.Background(), src, nil)
		if root == nil {
			t.Fatalf("%s: ParseSource = %v", name, err)
		}
		e, finish := c.pick(t, root)
		if _, err := e.RawHeader(); !errors.Is(err, ErrRawNotRecorded) {
			t.Errorf("%s: RawHeader before the body = %v, want ErrRawNotRecorded", name, err)
		}
		io.Copy(io.Discard, e.Body)
		finish()
		if _, err := e.RawHeader(); !errors.Is(err, ErrRawNotRecorded) {
			t.Errorf("%s: RawHeader after the body = %v, want ErrRawNotRecorded", name, err)
		}
	}
}

// The header block of a message carried as it is is served as soon as the
// message exists, from a recorded Source that has not been read to its end,
// with no index of the whole input; its body still waits for the end.
func TestSrcRawHeaderEmbeddedBeforeEnd(t *testing.T) {
	for _, name := range []string{"embedded in a part", "embedded in the root"} {
		c := srcHdrCases()[name]
		src := NewRecordedSource(strings.NewReader(c.data), &RecordOptions{MaxBytes: -1})
		_, root, err := ParseSource(context.Background(), src, nil)
		if root == nil {
			t.Fatalf("%s: ParseSource = %v", name, err)
		}
		e, finish := c.pick(t, root)
		if _, complete := src.Size(); complete {
			t.Fatalf("%s: the input was read to its end before the body: the test checks nothing", name)
		}
		if got := srcTestRaw(e.RawHeader()); got != "ok:"+c.header {
			t.Errorf("%s: RawHeader before the end = %.80q, want %q", name, got, c.header)
		}
		if _, err := e.RawBody(); !errors.Is(err, ErrRawSpanUnknown) {
			t.Errorf("%s: RawBody before the end = %v, want ErrRawSpanUnknown", name, err)
		}
		if got, _ := io.ReadAll(e.Body); string(got) != c.body {
			t.Errorf("%s: Body = %d bytes, want %d", name, len(got), len(c.body))
		}
		finish()
		io.Copy(io.Discard, root.Body)
		if got := srcTestRaw(e.RawHeader()); got != "ok:"+c.header {
			t.Errorf("%s: RawHeader after the end = %.80q", name, got)
		}
		if got := srcTestRaw(e.RawBody()); got != "ok:"+c.body {
			t.Errorf("%s: RawBody after the end = %.80q", name, got)
		}
	}
}

// Entities whose offsets are not the input's keep no header block: a message
// inside Base64, a part of an inferred boundary and a message it carries.
func TestSrcRawHeaderNotPlaced(t *testing.T) {
	inner := base64.StdEncoding.EncodeToString([]byte("Subject: inner\r\n\r\nhi\r\n"))
	inputs := map[string]string{
		"base64 embedded": "Content-Type: message/rfc822\r\nContent-Transfer-Encoding: base64\r\n\r\n" + inner + "\r\n",
		"inferred boundary": "Content-Type: multipart/mixed; boundary=wrong\r\n\r\n--X\r\nContent-Type: message/rfc822\r\n\r\n" +
			"Subject: inner\r\n\r\nhi\r\n--X--\r\n",
	}
	for name, data := range inputs {
		for _, kind := range []string{"seekable", "recorded"} {
			var root *Entity
			if kind == "seekable" {
				_, root, _ = ParseAt(strings.NewReader(data), int64(len(data)), nil)
			} else {
				_, root, _ = ParseSource(context.Background(), NewRecordedSource(strings.NewReader(data), nil), nil)
			}
			if root == nil {
				t.Fatalf("%s %s: no root", name, kind)
			}
			var unplaced []*Entity
			carrier := root
			mr := root.MultipartReader()
			if mr != nil {
				p, err := mr.NextPart()
				if p == nil {
					t.Fatalf("%s %s: NextPart = %v", name, kind, err)
				}
				unplaced = append(unplaced, p)
				carrier = p
			}
			emb, err := carrier.EmbeddedMessage()
			if emb == nil {
				t.Fatalf("%s %s: EmbeddedMessage = %v", name, kind, err)
			}
			unplaced = append(unplaced, emb)
			for _, e := range unplaced {
				o, _ := e.Origin()
				if _, err := e.RawHeader(); !errors.Is(err, ErrRawSpanUnknown) {
					t.Errorf("%s %s %v: RawHeader = %v, want ErrRawSpanUnknown", name, kind, o.Path, err)
				}
			}
			io.Copy(io.Discard, emb.Body)
			if mr != nil {
				for i := 0; i < 8; i++ {
					if _, err := mr.NextPart(); err != nil {
						break
					}
				}
			}
			io.Copy(io.Discard, root.Body)
			for _, e := range unplaced {
				o, _ := e.Origin()
				if _, err := e.RawHeader(); !errors.Is(err, ErrRawSpanUnknown) {
					t.Errorf("%s %s %v: RawHeader at the end = %v, want ErrRawSpanUnknown", name, kind, o.Path, err)
				}
			}
			// The root is the input's.
			got := srcTestRaw(root.RawHeader())
			if h, ok := strings.CutPrefix(got, "ok:"); !ok || !strings.HasPrefix(data, h) || !bytes.HasSuffix([]byte(h), []byte("\r\n\r\n")) {
				t.Errorf("%s %s: root RawHeader = %q", name, kind, got)
			}
		}
	}
}
