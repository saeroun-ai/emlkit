package textproto

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// TestMultipartReader_ResumeAfterClose reads each body to its end, reopening
// the iteration after every closing delimiter while ResumeAfterClose allows,
// and records the parts and how each resumption went.
func TestMultipartReader_ResumeAfterClose(t *testing.T) {
	tests := []struct {
		name, body string
		limit      int64
		closes     int
		want       string
	}{
		{"opening after a blank line", "--B\r\n\r\none\r\n--B--\r\n\r\n--B\r\n\r\ntwo\r\n--B--\r\n", 0, 0,
			"0@7:one; resume 19 closes=0 part=false skipped=2; 1@28:two; end epilogue=\"\""},
		{"text after the close is a part", "--B\r\n\r\none\r\n--B--\r\nX: 1\r\n\r\ntwo\r\n--B--\r\n", 0, -1,
			"0@7:one; resume 19 closes=0 part=true skipped=0; 1@27:two; end epilogue=\"\""},
		{"a further close is passed over", "--B\r\n\r\none\r\n--B--\r\n--B--\r\n--B\r\n\r\ntwo\r\n--B--\r\n", 0, 1,
			"0@7:one; resume 19 closes=1 part=false skipped=7; 1@33:two; end epilogue=\"\""},
		{"a further close past closes ends it", "--B\r\n\r\none\r\n--B--\r\n--B--\r\n--B\r\n\r\ntwo\r\n", 0, 0,
			"0@7:one; end epilogue=\"--B--\\r\\n--B\\r\\n\\r\\ntwo\\r\\n\""},
		{"text with no delimiter after it is epilogue", "--B\r\n\r\none\r\n--B--\r\ntail\r\n", 0, -1,
			"0@7:one; end epilogue=\"tail\\r\\n\""},
		{"an opening line the input ends", "--B\r\n\r\none\r\n--B--\r\n--B", 0, -1,
			"0@7:one; end epilogue=\"--B\""},
		{"past the limit", "--B\r\n\r\none\r\n--B--\r\n0123456789\r\n--B\r\n\r\ntwo\r\n", 8, -1,
			"0@7:one; end epilogue=\"0123456789\\r\\n--B\\r\\n\\r\\ntwo\\r\\n\""},
		{"a bare CR ends a blank line", "--B\r\n\r\none\r\n--B--\r\n\r--B\r\n\r\ntwo\r\n--B--\r\n", 0, -1,
			"0@7:one; resume 19 closes=0 part=false skipped=1; 1@27:two; end epilogue=\"\""},
		// The lines below are longer than a read, and their CRLF falls at
		// the end of a 4096-byte one (#71).
		{"a long line before a delimiter is a part",
			"--B\r\n\r\none\r\n--B--\r\nX: " + strings.Repeat("y", 4095-19-3) + "\r\n\r\ntwo\r\n--B--\r\n", 0, -1,
			"0@7:one; resume 19 closes=0 part=true skipped=0; 1@4099:two; end epilogue=\"\""},
		{"a long line with no delimiter after it is epilogue",
			"--B\r\n\r\none\r\n--B--\r\n" + strings.Repeat("x", 4095-19) + "\r\n" + strings.Repeat("x", 5000), 0, -1,
			fmt.Sprintf("0@7:one; end epilogue=%q", strings.Repeat("x", 4095-19)+"\r\n"+strings.Repeat("x", 5000))},
		{"a long line past the limit", "--B\r\n\r\none\r\n--B--\r\n" + strings.Repeat("x", 5000) + "\r\n--B\r\n\r\ntwo\r\n", 4096, -1,
			fmt.Sprintf("0@7:one; end epilogue=%q", strings.Repeat("x", 5000)+"\r\n--B\r\n\r\ntwo\r\n")},
	}
	for _, tc := range tests {
		for name, src := range map[string]io.Reader{
			"whole":    strings.NewReader(tc.body),
			"one byte": iotest.OneByteReader(strings.NewReader(tc.body)),
			"16 bytes": &chunkReader{s: tc.body, n: 16},
			"4096":     &chunkReader{s: tc.body, n: 4096},
		} {
			r := NewMultipartReader(src, "B")
			r.SetHeaderLimits(HeaderLimits{Recover: true})
			var got []string
			for i := 0; i < 8; i++ {
				p, err := r.NextPart()
				if err == io.EOF {
					res, ok := r.ResumeAfterClose(tc.limit, tc.closes)
					if !ok {
						rd, _, _ := r.Epilogue()
						b, _ := io.ReadAll(rd)
						got = append(got, fmt.Sprintf("end epilogue=%q", b))
						break
					}
					got = append(got, fmt.Sprintf("resume %d closes=%d part=%v skipped=%d", res.Offset, res.Closes, res.Part, res.Skipped))
					continue
				}
				if err != nil {
					t.Fatalf("%s/%s: NextPart() = %v", tc.name, name, err)
				}
				b, _ := io.ReadAll(p)
				got = append(got, fmt.Sprintf("%d@%d:%s", r.partsRead-1, p.BodyOffset(), b))
			}
			if s := strings.Join(got, "; "); s != tc.want {
				t.Errorf("%s/%s:\n got %s\nwant %s", tc.name, name, s, tc.want)
			}
		}
	}

	// Without recovery, or before the iteration ended, nothing is reopened.
	r := NewMultipartReader(strings.NewReader("--B\r\n\r\none\r\n--B--\r\n--B\r\n\r\ntwo\r\n"), "B")
	if _, ok := r.ResumeAfterClose(0, -1); ok {
		t.Error("ResumeAfterClose() before the end: ok")
	}
	r.NextPart()
	if _, err := r.NextPart(); err != io.EOF {
		t.Fatalf("NextPart() = %v", err)
	}
	if _, ok := r.ResumeAfterClose(0, -1); ok {
		t.Error("ResumeAfterClose() without recovery: ok")
	}
}

// TestMultipartReader_ResumeAfterCloseLinear holds the bytes ResumeAfterClose
// examines for line endings to twice those after the closing delimiter, read
// whole or in 1-, 16- and 4096-byte reads (#71): a line that each read made
// longer was searched again from its start, so a 64 KiB line read a byte at a
// time cost about 2^31 bytes. The count, not a clock, is what is held. What it
// returns, and the epilogue it gives back, must not depend on the reads.
func TestMultipartReader_ResumeAfterCloseLinear(t *testing.T) {
	const head = "--B\r\n\r\none\r\n--B--\r\n"
	for _, n := range []int{1 << 12, 1 << 14, 1 << 16} {
		for _, tail := range []struct{ name, text, part string }{
			{"one line, no line ending", strings.Repeat("x", n), ""},
			{"a long field, then a part", "X: " + strings.Repeat("y", n) + "\r\n\r\ntwo\r\n--B--\r\n", "two"},
			{"a long line, then a delimiter", strings.Repeat("z", n) + "\r\n--B\r\n\r\ntwo\r\n--B--\r\n", ""},
			{"lines ended by a bare CR", strings.Repeat("x\r", n/2), ""},
			{"blank lines", strings.Repeat("\r\n", n/2), ""},
		} {
			body := head + tail.text
			var want string
			for _, size := range []int{0, 1, 16, 4096} {
				var src io.Reader = strings.NewReader(body)
				if size > 0 {
					src = &chunkReader{s: body, n: size}
				}
				r := NewMultipartReader(src, "B")
				r.SetHeaderLimits(HeaderLimits{Recover: true})
				p, err := r.NextPart()
				if err != nil {
					t.Fatalf("%s/%d/%d: NextPart() = %v", tail.name, n, size, err)
				}
				io.Copy(io.Discard, p)
				if _, err := r.NextPart(); err != io.EOF {
					t.Fatalf("%s/%d/%d: NextPart() = %v, want io.EOF", tail.name, n, size, err)
				}
				res, ok := r.ResumeAfterClose(0, -1)
				if bound := 2 * int64(len(tail.text)); r.resumeScanned > bound {
					t.Errorf("%s/%d/%d: examined %d bytes, want at most %d", tail.name, n, size, r.resumeScanned, bound)
				}
				got := fmt.Sprintf("%+v %v", res, ok)
				if ok {
					var parts []string
					for {
						p, err := r.NextPart()
						if err != nil {
							break
						}
						b, _ := io.ReadAll(p)
						parts = append(parts, string(b))
					}
					got += fmt.Sprintf(" parts=%q", parts)
					if tail.part != "" && (len(parts) == 0 || parts[0] != tail.part) {
						t.Errorf("%s/%d/%d: parts %q, want %q first", tail.name, n, size, parts, tail.part)
					}
				} else {
					rd, _, _ := r.Epilogue()
					b, _ := io.ReadAll(rd)
					if string(b) != tail.text {
						t.Errorf("%s/%d/%d: epilogue is %d bytes, not the %d after the close", tail.name, n, size, len(b), len(tail.text))
					}
				}
				if size == 0 {
					want = got
				} else if got != want {
					t.Errorf("%s/%d/%d:\n got %.200s\nwant %.200s", tail.name, n, size, got, want)
				}
			}
		}
	}
}
