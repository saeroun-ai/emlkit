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
	}
	for _, tc := range tests {
		for name, src := range map[string]io.Reader{
			"whole":    strings.NewReader(tc.body),
			"one byte": iotest.OneByteReader(strings.NewReader(tc.body)),
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
