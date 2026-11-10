package mail_test

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"

	message "github.com/saeroun-ai/emlkit"
	"github.com/saeroun-ai/emlkit/mail"
	"github.com/saeroun-ai/emlkit/textproto"
)

// Empty and header-only messages through mail.Reader (ROB-010-001). Every
// expectation is written out; CRLF only. Every source is finite and capped.

var errEmptyMailCap = errors.New("test source: call cap exceeded")

// An emptyMailSource delivers data in reads of at most chunk bytes, with
// io.EOF together with the last bytes when eofWith.
type emptyMailSource struct {
	data    string
	chunk   int
	eofWith bool

	pos, calls int
}

func (s *emptyMailSource) Read(p []byte) (int, error) {
	s.calls++
	if s.calls > len(s.data)+16 {
		return 0, errEmptyMailCap
	}
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	n := copy(p, s.data[s.pos:min(len(s.data), s.pos+s.chunk)])
	s.pos += n
	if s.pos == len(s.data) && s.eofWith {
		return n, io.EOF
	}
	return n, nil
}

func emptyMailRaw(fs textproto.HeaderFields) []string {
	var out []string
	for fs.Next() {
		raw, err := fs.Raw()
		if err != nil {
			raw = []byte("raw error: " + err.Error())
		}
		out = append(out, string(raw))
	}
	return out
}

// TestReader_emptyAndHeaderOnly pins mail.Reader over an empty message, one
// that is only the empty line, and complete header fields ended by io.EOF or
// by the empty line, through CreateReader and through NewReader on an entity
// from message.Parse under every profile. A message that is not multipart is
// one inline part, text/plain since it declares no Content-Type, whose header
// is the message's and whose body is what follows the empty line. A multipart
// with an empty body has no part: the first NextPart returns no part and an
// error wrapping io.EOF, the next one io.EOF (ROB-060-009 owns that policy).
func TestReader_emptyAndHeaderOnly(t *testing.T) {
	const fields = "From: a@example.com\r\nSubject: x\r\n"
	const multi = "Content-Type: multipart/mixed; boundary=B\r\n"
	raw := []string{"From: a@example.com\r\n", "Subject: x\r\n"}
	cases := []struct {
		name, text string
		raw        []string
		body       string
		multipart  bool
	}{
		{"empty", "", nil, "", false},
		{"separator", "\r\n", nil, "", false},
		{"separator and a blank body line", "\r\n\r\n", nil, "\r\n", false},
		{"fields at EOF", fields, raw, "", false},
		{"fields and separator", fields + "\r\n", raw, "", false},
		{"multipart at EOF", multi, []string{multi}, "", true},
		{"multipart and separator", multi + "\r\n", []string{multi}, "", true},
	}
	type open struct {
		name string
		open func(io.Reader) (*mail.Reader, error)
	}
	opens := []open{{"CreateReader", mail.CreateReader}}
	for _, p := range []message.Profile{message.ProfileTolerant, message.ProfileStrict, message.ProfileForensic} {
		opens = append(opens, open{"Parse/" + p.String(), func(r io.Reader) (*mail.Reader, error) {
			s, e, err := message.Parse(r, &message.ParseOptions{Profile: p})
			if err != nil || e == nil {
				return nil, err
			}
			if s.Len() != 0 {
				return nil, fmt.Errorf("%d diagnostics", s.Len())
			}
			return mail.NewReader(e), nil
		}})
	}
	for _, tc := range cases {
		for _, o := range opens {
			for _, chunk := range []int{1, 4096} {
				for _, eofWith := range []bool{false, true} {
					name := fmt.Sprintf("%s/%s/chunk %d/eof with data %v", tc.name, o.name, chunk, eofWith)
					r, err := o.open(&emptyMailSource{data: tc.text, chunk: chunk, eofWith: eofWith})
					if err != nil || r == nil {
						t.Errorf("%s: (%v, %v), want a reader", name, r != nil, err)
						continue
					}
					if got := emptyMailRaw(r.Header.Fields()); !reflect.DeepEqual(got, tc.raw) {
						t.Errorf("%s: header %q, want %q", name, got, tc.raw)
					}
					p, err := r.NextPart()
					// Parse heals an empty multipart under every profile but
					// Strict: no part and io.EOF (ROB-060-009).
					healed := o.name == "Parse/tolerant" || o.name == "Parse/forensic"
					if tc.multipart && healed {
						if p != nil || err != io.EOF {
							t.Errorf("%s: NextPart() = (%v, %v), want no part and io.EOF", name, p != nil, err)
						}
					} else if tc.multipart {
						if p != nil || err == nil || err == io.EOF || !errors.Is(err, io.EOF) {
							t.Errorf("%s: NextPart() = (%v, %v), want no part and an error wrapping io.EOF", name, p != nil, err)
						}
					} else {
						if err != nil || p == nil {
							t.Errorf("%s: NextPart() = (%v, %v), want a part", name, p != nil, err)
							continue
						}
						h, inline := p.Header.(*mail.InlineHeader)
						if !inline {
							t.Errorf("%s: part header %T, want *mail.InlineHeader", name, p.Header)
							continue
						}
						if got := emptyMailRaw(h.Fields()); !reflect.DeepEqual(got, tc.raw) {
							t.Errorf("%s: part header %q, want %q", name, got, tc.raw)
						}
						if b, berr := io.ReadAll(p.Body); string(b) != tc.body || berr != nil {
							t.Errorf("%s: body (%q, %v), want (%q, nil)", name, b, berr, tc.body)
						}
					}
					if p, err := r.NextPart(); p != nil || err != io.EOF {
						t.Errorf("%s: last NextPart() = (%v, %v), want io.EOF", name, p != nil, err)
					}
				}
			}
		}
	}
}

// TestReader_headerOnlyClassification pins that a header-only part is
// classified as any part is: a non-text type or an attachment disposition is
// an attachment, a text type is inline. Its body is empty either way.
func TestReader_headerOnlyClassification(t *testing.T) {
	cases := []struct {
		name, header string
		attachment   bool
	}{
		{"non-text type", "Content-Type: application/octet-stream\r\n", true},
		{"attachment disposition", "Content-Type: text/plain\r\nContent-Disposition: attachment\r\n", true},
		{"text type", "Content-Type: text/html\r\n", false},
	}
	opens := map[string]func(io.Reader) (*mail.Reader, error){
		"CreateReader": mail.CreateReader,
		"Parse": func(r io.Reader) (*mail.Reader, error) {
			_, e, err := message.Parse(r, nil)
			if err != nil || e == nil {
				return nil, err
			}
			return mail.NewReader(e), nil
		},
	}
	for _, tc := range cases {
		for oname, open := range opens {
			for _, sep := range []string{"", "\r\n"} {
				name := fmt.Sprintf("%s/%s/empty line %v", tc.name, oname, sep != "")
				r, err := open(&emptyMailSource{data: tc.header + sep, chunk: 4096})
				if err != nil || r == nil {
					t.Errorf("%s: (%v, %v), want a reader", name, r != nil, err)
					continue
				}
				p, err := r.NextPart()
				if err != nil || p == nil {
					t.Errorf("%s: NextPart() = (%v, %v), want a part", name, p != nil, err)
					continue
				}
				if _, attachment := p.Header.(*mail.AttachmentHeader); attachment != tc.attachment {
					t.Errorf("%s: part header %T, want attachment %v", name, p.Header, tc.attachment)
				}
				if b, berr := io.ReadAll(p.Body); len(b) != 0 || berr != nil {
					t.Errorf("%s: body (%q, %v), want (\"\", nil)", name, b, berr)
				}
				if p, err := r.NextPart(); p != nil || err != io.EOF {
					t.Errorf("%s: last NextPart() = (%v, %v), want io.EOF", name, p != nil, err)
				}
			}
		}
	}
}
