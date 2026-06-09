package mail

import (
	"github.com/saeroun-ai/emlkit"
)

// parseFilename parses the filename from the header's Content-Disposition,
// falling back to the discouraged "name" parameter of Content-Type.
func parseFilename(h message.Header) (string, error) {
	_, params, err := h.ContentDisposition()

	filename, ok := params["filename"]
	if !ok {
		// Using "name" in Content-Type is discouraged
		_, params, err = h.ContentType()
		filename = params["name"]
	}

	return filename, err
}

// An AttachmentHeader represents an attachment's header.
type AttachmentHeader struct {
	message.Header
}

var _ PartHeader = (*AttachmentHeader)(nil)

func (*AttachmentHeader) partHeader() {}

// Filename parses the attachment's filename.
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

// Filename parses the inline attachment's filename.
func (h *InlineAttachmentHeader) Filename() (string, error) {
	return parseFilename(h.Header)
}

// SetFilename formats the inline attachment's filename.
func (h *InlineAttachmentHeader) SetFilename(filename string) {
	dispParams := map[string]string{"filename": filename}
	h.SetContentDisposition("inline", dispParams)
}
