package message

import (
	"mime"

	"github.com/saeroun-ai/emlkit/textproto"
)

func parseHeaderWithParams(s string) (f string, params map[string]string, err error) {
	// Without a poll nothing is cancelled, and this is exactly the parse it
	// always was.
	f, params, _, err = parseHeaderWithParamsPoll(s, nil)
	return
}

// parseHeaderWithParamsPoll is parseHeaderWithParams for a parse that can be
// cancelled: poll, when it is not nil, is asked before the field is parsed,
// after mime.ParseMediaType returns, before each parameter value is decoded —
// a decoding that can call the configured CharsetReader — and after the last
// one. Once it reports true, nothing further is decoded and cancelled is true;
// the result is then discarded rather than returned, so the caller never
// mistakes a reading the cancellation reached for a whole one. A single call of
// mime.ParseMediaType or of a CharsetReader is not interrupted.
func parseHeaderWithParamsPoll(s string, poll func() bool) (f string, params map[string]string, cancelled bool, err error) {
	return parseHeaderWithParamsDecoder(s, poll, nil)
}

// parseHeaderWithParamsDecoder is parseHeaderWithParamsPoll with the encoded
// words of parameter values decoded by dec, which guards the CharsetReader's
// readers for the parse engine (see progress.go); a nil dec decodes as
// decodeHeader does.
func parseHeaderWithParamsDecoder(s string, poll func() bool, dec *metadataDecoder) (f string, params map[string]string, cancelled bool, err error) {
	if poll != nil && poll() {
		return "", nil, true, nil
	}
	f, params, err = mime.ParseMediaType(s)
	if err != nil {
		if poll != nil && poll() {
			return "", nil, true, nil
		}
		return s, nil, false, err
	}
	for k, v := range params {
		if poll != nil && poll() {
			return "", nil, true, nil
		}
		params[k] = dec.decode(v)
	}
	if poll != nil && poll() {
		// The last decoding may have been where it was cancelled.
		return "", nil, true, nil
	}
	return f, params, false, nil
}

func formatHeaderWithParams(f string, params map[string]string) string {
	encParams := make(map[string]string)
	for k, v := range params {
		encParams[k] = encodeHeader(v)
	}
	return mime.FormatMediaType(f, encParams)
}

// HeaderFields iterates over header fields.
type HeaderFields interface {
	textproto.HeaderFields

	// Text parses the value of the current field as plaintext. The field
	// charset is decoded to UTF-8. If the header field's charset is unknown,
	// the raw field value is returned and the error verifies IsUnknownCharset.
	Text() (string, error)

	headerFields()
}

type headerFields struct {
	textproto.HeaderFields
}

func (*headerFields) headerFields() {}

func (hf *headerFields) Text() (string, error) {
	return decodeHeader(hf.Value())
}

// A Header represents the key-value pairs in a message header.
type Header struct {
	textproto.Header
}

// HeaderFromMap creates a header from a map of header fields.
//
// This function is provided for interoperability with the standard library.
// If possible, ReadHeader should be used instead to avoid loosing information.
// The map representation looses the ordering of the fields, the capitalization
// of the header keys, and the whitespace of the original header.
func HeaderFromMap(m map[string][]string) Header {
	return Header{textproto.HeaderFromMap(m)}
}

// ContentType parses the Content-Type header field.
//
// If no Content-Type is specified, it returns "text/plain".
//
// It parses whatever the field holds, under no budget. A Header belongs to
// whoever holds it and may never have come from a parse at all, so there is no
// configuration here to hold it to; Limits.MaxParameterCount and
// Limits.MaxParameterSegments bound the parse engine's own reading of this
// field, on the entities Parse produces, and are documented there. A caller
// parsing a field of unknown provenance bounds it itself, or reaches it through
// Parse.
//
// It reads the header as it is now. Entity.EffectiveContentType is the other
// answer: the Content-Type an entity was constructed with, as its parse
// interpreted it, which is what the entity's body was decoded by.
//
// It also repairs nothing. A field with a valid media type but a malformed
// parameter fails here as it always has — the raw value, no parameters, and the
// error — even where Parse kept that media type and its well-formed parameters
// for the entity; see metadata_repair.go.
func (h *Header) ContentType() (t string, params map[string]string, err error) {
	v := h.Get("Content-Type")
	if v == "" {
		return "text/plain", nil, nil
	}
	return parseHeaderWithParams(v)
}

// SetContentType formats the Content-Type header field.
func (h *Header) SetContentType(t string, params map[string]string) {
	h.Set("Content-Type", formatHeaderWithParams(t, params))
}

// ContentDisposition parses the Content-Disposition header field, as defined in
// RFC 2183.
//
// Like ContentType, it parses under no budget and reads the header as it is
// now; see there, and Entity.EffectiveContentDisposition for the field as an
// entity was constructed with it.
func (h *Header) ContentDisposition() (disp string, params map[string]string, err error) {
	return parseHeaderWithParams(h.Get("Content-Disposition"))
}

// SetContentDisposition formats the Content-Disposition header field, as
// defined in RFC 2183.
func (h *Header) SetContentDisposition(disp string, params map[string]string) {
	h.Set("Content-Disposition", formatHeaderWithParams(disp, params))
}

// Text parses a plaintext header field. The field charset is automatically
// decoded to UTF-8. If the header field's charset is unknown, the raw field
// value is returned and the error verifies IsUnknownCharset.
func (h *Header) Text(k string) (string, error) {
	return decodeHeader(h.Get(k))
}

// SetText sets a plaintext header field.
func (h *Header) SetText(k, v string) {
	h.Set(k, encodeHeader(v))
}

// Copy creates a stand-alone copy of the header.
func (h *Header) Copy() Header {
	return Header{h.Header.Copy()}
}

// Fields iterates over all the header fields.
//
// The header may not be mutated while iterating, except using HeaderFields.Del.
func (h *Header) Fields() HeaderFields {
	return &headerFields{h.Header.Fields()}
}

// FieldsByKey iterates over all fields having the specified key.
//
// The header may not be mutated while iterating, except using HeaderFields.Del.
func (h *Header) FieldsByKey(k string) HeaderFields {
	return &headerFields{h.Header.FieldsByKey(k)}
}
