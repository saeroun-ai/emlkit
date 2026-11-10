package mail

import (
	"github.com/saeroun-ai/emlkit"
)

// parseFilename returns the filename the header gives its part: the first of
// message.Header.FilenameCandidates, which reads the filename parameter of
// Content-Disposition and the discouraged name parameter of Content-Type
// tolerantly and prefers, in this order, Content-Disposition filename*,
// Content-Disposition filename, Content-Type name* and Content-Type name.
//
// A name that could be read is returned whatever was wrong with the field
// around it, so the error is always nil; it stays in the signature because it
// always was there. The name is the sender's own: it is not made safe for a
// filesystem (see message.SafeFilename), and it is not UTF-8 when its bytes
// are in a charset that is undeclared or has no decoder.
func parseFilename(h message.Header) (string, error) {
	if names := h.FilenameCandidates(); len(names) > 0 {
		return names[0].Filename, nil
	}
	return "", nil
}

// An AttachmentHeader represents an attachment's header.
type AttachmentHeader struct {
	message.Header
}

var _ PartHeader = (*AttachmentHeader)(nil)

func (*AttachmentHeader) partHeader() {}

// Filename parses the attachment's filename.
//
// It reads the header tolerantly: a filename in a field with malformed
// parameters, without quotes around its spaces, in an RFC 2231 series or
// charset the standard library does not read, or given only as the name
// parameter of Content-Type is returned rather than lost. Every candidate, with
// its raw text, is available from FilenameCandidates, which AttachmentHeader
// has through its Header. The error is always nil. The name is the sender's
// and may hold path separators; use message.SafeFilename before creating a
// file from it.
func (h *AttachmentHeader) Filename() (string, error) {
	return parseFilename(h.Header)
}

// SetFilename formats the attachment's filename.
func (h *AttachmentHeader) SetFilename(filename string) {
	dispParams := map[string]string{"filename": filename}
	h.SetContentDisposition("attachment", dispParams)
}

// An InlineAttachmentHeader represents an inlined attachment's header. An inline
// attachment is referenced from the message body (typically via a Content-Id,
// e.g. an image embedded in HTML) rather than offered as a separate download.
type InlineAttachmentHeader struct {
	message.Header
}

// Filename parses the inline attachment's filename, on the terms of
// AttachmentHeader.Filename.
func (h *InlineAttachmentHeader) Filename() (string, error) {
	return parseFilename(h.Header)
}

// SetFilename formats the inline attachment's filename.
func (h *InlineAttachmentHeader) SetFilename(filename string) {
	dispParams := map[string]string{"filename": filename}
	h.SetContentDisposition("inline", dispParams)
}
