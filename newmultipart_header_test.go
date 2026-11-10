package message

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit/textproto"
)

func textprotoWriteHeader(w io.Writer, h Header) error {
	return textproto.WriteHeader(w, h.Header)
}

// TestNewMultipart_headerIsolated pins issue #60: NewMultipart and the reading
// of its body never change the caller's Header, and the entity's own Header
// agrees with its body in every view.
func TestNewMultipart_headerIsolated(t *testing.T) {
	part := func() *Entity {
		var h Header
		h.Set("Content-Type", "text/plain")
		e, _ := New(h, strings.NewReader("part"))
		return e
	}
	for _, tc := range []struct {
		name, contentType, cte string
		keepsBoundary          bool
	}{
		{"no boundary", "multipart/mixed", "", false},
		{"empty boundary", `multipart/mixed; boundary=""`, "", false},
		{"overlong boundary", "multipart/mixed; boundary=" + strings.Repeat("x", 71), "", false},
		{"valid boundary", "multipart/mixed; boundary=Good", "", true},
		{"valid boundary and a transfer encoding", "multipart/mixed; boundary=Good", "base64", true},
		{"unwritable charset", "multipart/mixed; charset=windows-1252", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h Header
			h.Set("Subject", "s")
			h.Set("Content-Type", tc.contentType)
			if tc.cte != "" {
				h.Set("Content-Transfer-Encoding", tc.cte)
			}
			var before bytes.Buffer
			textprotoWrite(t, &before, h)

			e, err := NewMultipart(h, []*Entity{part()})
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(io.LimitReader(e.Body, 1<<16))
			io.ReadAll(io.LimitReader(e.Body, 16))
			if c, ok := e.Body.(io.Closer); ok {
				c.Close()
			}

			var after bytes.Buffer
			textprotoWrite(t, &after, h)
			if before.String() != after.String() || h.Get("Content-Type") != tc.contentType || h.Get("Content-Transfer-Encoding") != tc.cte {
				t.Errorf("caller's header changed:\n%q\n%q", before.String(), after.String())
			}

			// The entity's header: one boundary, the one the body uses, no
			// transfer encoding, and the same in every view.
			_, params, err := e.Header.ContentType()
			if err != nil {
				t.Fatal(err)
			}
			b := params["boundary"]
			if b == "" || (tc.keepsBoundary && b != "Good") {
				t.Errorf("entity boundary %q", b)
			}
			if tc.name != "unwritable charset" && !bytes.Contains(body, []byte("--"+b+"\r\n")) {
				t.Errorf("body %q does not use the entity's boundary %q", body, b)
			}
			if e.Header.Has("Content-Transfer-Encoding") {
				t.Error("entity header keeps a transfer encoding")
			}
			var written bytes.Buffer
			textprotoWrite(t, &written, e.Header)
			if strings.Contains(written.String(), "Content-Transfer-Encoding") || !strings.Contains(written.String(), b) {
				t.Errorf("written entity header %q disagrees with its fields", written.String())
			}
			if n := len(e.Header.Map()["Content-Type"]); n != 1 {
				t.Errorf("%d Content-Type fields in the map", n)
			}
		})
	}
}

// textprotoWrite writes h as WriteHeader writes it.
func textprotoWrite(t *testing.T, w io.Writer, h Header) {
	t.Helper()
	if err := textprotoWriteHeader(w, h); err != nil {
		t.Fatalf("WriteHeader = %v", err)
	}
}
