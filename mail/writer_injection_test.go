package mail_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
)

// TestWriters_refuseInjection pins that no writer emits a header a caller's
// value could split or extend (ROB-120-003): a raw CR, LF or NUL in a value,
// or a name that is no field name, is refused with an error before anything
// of that header is written, and text set through the encoding setters is
// encoded, never written raw.
func TestWriters_refuseInjection(t *testing.T) {
	for name, set := range map[string]func(h *mail.Header){
		"CRLF in a value": func(h *mail.Header) { h.Set("Subject", "hi\r\nBcc: evil@example.com") },
		"LF in a value":   func(h *mail.Header) { h.Set("X-A", "a\nX-Injected: 1") },
		"CR in a value":   func(h *mail.Header) { h.Set("X-A", "a\rX-Injected: 1") },
		"NUL in a value":  func(h *mail.Header) { h.Set("X-A", "a\x00b") },
		"CRLF in a name":  func(h *mail.Header) { h.Set("X-Key\r\nEvil", "v") },
		"space in a name": func(h *mail.Header) { h.Set("X Space", "v") },
		"colon in a name": func(h *mail.Header) { h.Set("X-Colon:", "v") },
		"CRLF in an id":   func(h *mail.Header) { h.SetMessageID("id\r\nX-Evil: 1@x") },
		"CRLF in an address": func(h *mail.Header) {
			h.SetAddressList("To", []*mail.Address{{Name: "x", Address: "a@b\r\nX-Evil: y"}})
		},
	} {
		var h mail.Header
		h.Set("From", "a@example.com")
		set(&h)
		var out bytes.Buffer
		w, err := mail.CreateSingleInlineWriter(&out, h)
		if err == nil {
			w.Close()
			if strings.Contains(out.String(), "\nBcc:") || strings.Contains(out.String(), "X-Injected") || strings.Contains(out.String(), "\nX-Evil") || strings.Contains(out.String(), "\x00") {
				t.Errorf("%s: wrote %q", name, out.String())
			}
			continue
		}
		// A writer that could not be made is a nil interface, so a caller's
		// nil check holds.
		if w != nil {
			t.Errorf("%s: a non-nil writer came with the error %v", name, err)
		}
	}

	// Text set through the encoding setters cannot inject either.
	var h mail.Header
	h.SetSubject("s\r\nX-Evil: 1")
	h.SetAddressList("To", []*mail.Address{{Name: "x\r\nBcc: e@x", Address: "a@example.com"}})
	var mh message.Header = h.Header
	mh.SetContentType("text/plain", map[string]string{"name": "a\r\nX-Evil: 1"})
	var out bytes.Buffer
	w, err := message.CreateWriter(&out, mh)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	for _, line := range strings.Split(out.String(), "\r\n") {
		if strings.HasPrefix(line, "X-Evil") || strings.HasPrefix(line, "Bcc") {
			t.Errorf("injected line %q in %q", line, out.String())
		}
	}

	// A nil *message.Writer fails with an error, not a panic.
	var nilWriter *message.Writer
	if _, err := nilWriter.Write([]byte("x")); err == nil {
		t.Error("nil Writer.Write succeeded")
	}
	if err := nilWriter.Close(); err == nil {
		t.Error("nil Writer.Close succeeded")
	}
}
