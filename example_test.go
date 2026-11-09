package message_test

import (
	"bytes"
	"errors"
	"io"
	"log"
	"strings"

	"github.com/saeroun-ai/emlkit"
)

func ExampleRead() {
	// Let's assume r is an io.Reader that contains a message.
	var r io.Reader

	m, err := message.Read(r)
	if message.IsUnknownCharset(err) {
		// This error is not fatal
		log.Println("Unknown encoding:", err)
	} else if err != nil {
		log.Fatal(err)
	}

	if mr := m.MultipartReader(); mr != nil {
		// This is a multipart message
		log.Println("This is a multipart message containing:")
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			} else if err != nil {
				log.Fatal(err)
			}

			t, _, _ := p.Header.ContentType()
			log.Println("A part with type", t)
		}
	} else {
		t, _, _ := m.Header.ContentType()
		log.Println("This is a non-multipart message with type", t)
	}
}

func ExampleWriter() {
	var b bytes.Buffer

	var h message.Header
	h.SetContentType("multipart/alternative", nil)
	w, err := message.CreateWriter(&b, h)
	if err != nil {
		log.Fatal(err)
	}

	var h1 message.Header
	h1.SetContentType("text/html", nil)
	w1, err := w.CreatePart(h1)
	if err != nil {
		log.Fatal(err)
	}
	io.WriteString(w1, "<h1>Hello World!</h1><p>This is an HTML part.</p>")
	w1.Close()

	var h2 message.Header
	h2.SetContentType("text/plain", nil)
	w2, err := w.CreatePart(h2)
	if err != nil {
		log.Fatal(err)
	}
	io.WriteString(w2, "Hello World!\n\nThis is a text part.")
	w2.Close()

	w.Close()

	log.Println(b.String())
}

func ExampleParse() {
	// Let's assume r is an io.Reader that contains a message.
	var r io.Reader

	// Parse is the entry point that returns a parse session. Passing nil
	// options asks for the recommended configuration: the tolerant profile, the
	// conservative policy, and every diagnostic retained.
	session, e, err := message.Parse(r, nil)

	// The session is never nil, so it is always somewhere to look. The entity
	// is nil only when nothing usable could be built at all, which for Parse
	// means the header could not be read.
	switch {
	case message.IsUnknownCharset(err), message.IsUnknownEncoding(err):
		// A warning: the body is returned undecoded but readable, and the
		// session holds a diagnostic for each undecodable part.
		log.Println("undecoded content:", err)
	case err != nil:
		log.Fatal(err)
	}

	// Parse only reads the header. Multipart framing, transfer encodings and
	// charsets are inspected as the message is consumed, so most defects are
	// discovered after Parse has already returned — which is why the session
	// outlives the call.
	err = e.Walk(func(path []int, part *message.Entity, err error) error {
		if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
			return err
		}
		t, _, _ := part.Header.ContentType()
		log.Println("part", path, "has type", t)
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	for _, d := range session.Diagnostics() {
		// An entity whose transfer encoding or charset has no decoder is
		// reported here, at the part it belongs to.
		switch d.Code {
		case message.CodeCTEUnknown, message.CodeCharsetUnknown:
			log.Printf("part %v: %v (%v)", d.Path, d.Message, d.Action)
		default:
			log.Println(d)
		}
	}
}

func ExampleParseOptions() {
	// Let's assume r is an io.Reader that contains a message.
	var r io.Reader

	// A sink handles each diagnostic as it is discovered. Combined with
	// DiscardDeliveredDiagnostics, the memory a parse needs stays independent
	// of how damaged the message is.
	opts := &message.ParseOptions{
		Profile:       message.ProfileForensic,
		Compatibility: message.CompatibilityAngus,
		DiagnosticSink: message.DiagnosticSinkFunc(func(_ *message.ParseSession, d message.Diagnostic) error {
			log.Println(d)
			return nil
		}),
		DiscardDeliveredDiagnostics: true,

		// The existing read options are carried through unchanged.
		ReadOptions: message.ReadOptions{MaxHeaderBytes: 1 << 20},
	}

	session, e, err := message.Parse(r, opts)
	if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
		log.Fatal(err)
	}
	_, _ = session, e
}

func ExampleParseSession() {
	// A parse session is filled by the parser, not by the caller: it reports
	// the defects the parser observed and the repairs it applied. Let's assume
	// session is the one Parse returned for a message.
	var session *message.ParseSession

	log.Printf("parsed with the %v profile and the %v policy",
		session.Profile(), session.Compatibility())

	// Diagnostics are returned in discovery order, and the result is safe to
	// keep: neither the slice nor the paths alias the session.
	for _, d := range session.Diagnostics() {
		// Branch on the stable code and severity, never on the human-readable
		// message. Severities are categories rather than a scale, so handle
		// the ones that matter explicitly.
		switch d.Severity {
		case message.SeverityIncomplete:
			log.Println("part is truncated, data may be missing:", d)
		case message.SeverityInvalidMetadata:
			log.Println("raw content is usable but its metadata is not:", d)
		case message.SeverityFatal:
			log.Println("parser gave up on this scope:", d)
		default:
			log.Println(d)
		}

		if d.Code == message.CodeMultipartCloseNotFound {
			log.Println("the closing boundary delimiter was missing")
		}
	}
}

func ExampleDiagnosticSink() {
	// Let's assume r is an io.Reader that contains a message.
	var r io.Reader

	// A sink handles each defect as the parser discovers it, so a caller that
	// streams a large message does not have to keep every diagnostic until the
	// parse ends. Parse accepts one through ParseOptions.
	var truncated int

	sink := message.DiagnosticSinkFunc(func(s *message.ParseSession, d message.Diagnostic) error {
		// The session is handed to the sink rather than captured from the
		// parse call, because a defect in the top-level header is delivered
		// while that call is still running. It is safe to read from here.
		log.Printf("%v profile, %d diagnostics so far: %v", s.Profile(), s.Len(), d)

		if d.Severity == message.SeverityIncomplete {
			truncated++
		}

		// Returning an error stops further delivery to this sink. The session
		// keeps this first error, never calls the sink again, and reports it
		// back to the parser as a fatal error wrapping it, so the scope that
		// was parsing stops and returns what it had already recovered.
		if truncated > 100 {
			return errors.New("too many truncated parts")
		}
		return nil
	})

	// Delivery is serialized per session, so a sink used by one session needs
	// no lock of its own. One shared by several sessions is called by them
	// concurrently and must guard what it shares. Either way it must not block
	// for long: the session waits for it to return.
	session, e, err := message.Parse(r, &message.ParseOptions{DiagnosticSink: sink})
	if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
		log.Fatal(err)
	}
	_, _ = session, e
}

func ExampleIsFatal() {
	// Let's assume r is an io.Reader that contains a multipart message.
	var r io.Reader

	e, err := message.Read(r)
	// Unknown charsets and unknown transfer encodings are warnings: Read still
	// returns a usable entity, so only anything else is worth giving up for.
	if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
		log.Fatal(err)
	}
	mr := e.MultipartReader()
	if mr == nil {
		return
	}

	for {
		part, err := mr.NextPart()

		// A result and an error are independent answers. A non-nil part is
		// what the parser recovered before it stopped; it belongs to the
		// caller and stays usable however err reads, so handle it first.
		if part != nil {
			log.Println("part:", part.Header.Get("Content-Type"))
		}

		switch {
		case errors.Is(err, io.EOF):
			// The end of the iterator is control flow, not a failure.
			return
		case message.IsFatal(err):
			// The scope that returned this error ends here. Parts already
			// returned stay valid, and how much else stops — this iterator, or
			// the whole parse — is what the API that failed says it is.
			var fatal message.FatalError
			if errors.As(err, &fatal) {
				// Not every fatal error describes a defect of the message: a
				// caller callback that refused to continue has no diagnostic.
				if d, ok := fatal.Diagnostic(); ok {
					log.Println("gave up because of:", d.Code)
				}
			}
			return
		case message.IsUnknownCharset(err), message.IsUnknownEncoding(err):
			// Warnings that accompany a usable part: the raw content is still
			// readable, so keep going.
			log.Println("undecoded content:", err)
		case err != nil:
			log.Fatal(err)
		}
	}
}

func Example_transform() {
	// Let's assume r is an io.Reader that contains a message.
	var r io.Reader

	m, err := message.Read(r)
	if message.IsUnknownCharset(err) {
		log.Println("Unknown encoding:", err)
	} else if err != nil {
		log.Fatal(err)
	}

	// We'll add "This message is powered by Go" at the end of each text entity.
	poweredBy := "\n\nThis message is powered by Go."

	var b bytes.Buffer
	w, err := message.CreateWriter(&b, m.Header)
	if err != nil {
		log.Fatal(err)
	}

	// Define a function that transforms message.
	var transform func(w *message.Writer, e *message.Entity) error
	transform = func(w *message.Writer, e *message.Entity) error {
		if mr := e.MultipartReader(); mr != nil {
			// This is a multipart entity, transform each of its parts
			for {
				p, err := mr.NextPart()
				if err == io.EOF {
					break
				} else if err != nil {
					return err
				}

				pw, err := w.CreatePart(p.Header)
				if err != nil {
					return err
				}

				if err := transform(pw, p); err != nil {
					return err
				}

				pw.Close()
			}
			return nil
		} else {
			body := e.Body
			if strings.HasPrefix(m.Header.Get("Content-Type"), "text/") {
				body = io.MultiReader(body, strings.NewReader(poweredBy))
			}
			_, err := io.Copy(w, body)
			return err
		}
	}

	if err := transform(w, m); err != nil {
		log.Fatal(err)
	}
	w.Close()

	log.Println(b.String())
}
