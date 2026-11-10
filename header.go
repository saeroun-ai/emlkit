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

	// Text parses the value of the current field as plaintext, as Header.Text
	// does: encoded words are decoded to UTF-8, and one whose charset is
	// unknown is left as it was written while the error verifies
	// IsUnknownCharset.
	Text() (string, error)

	// TextDiagnostics parses the value of the current field as plaintext and
	// reports what was repaired, as Header.TextDiagnostics does. Field is the
	// key of the current field.
	TextDiagnostics() (string, []Diagnostic)

	headerFields()
}

type headerFields struct {
	textproto.HeaderFields
}

func (*headerFields) headerFields() {}

func (hf *headerFields) Text() (string, error) {
	return decodeHeader(hf.Value())
}

func (hf *headerFields) TextDiagnostics() (string, []Diagnostic) {
	return fieldDiagnostics(hf.Key(), hf.Value(), nil)
}

// fieldDiagnostics decodes the value v of the field named k and names that
// field in every diagnostic.
func fieldDiagnostics(k, v string, opts *HeaderTextOptions) (string, []Diagnostic) {
	s, diags := DecodeHeaderTextWithOptions(v, opts)
	for i := range diags {
		diags[i].Field = k
	}
	return s, diags
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
// It also repairs nothing. A field with a malformed media type or parameter
// fails here as it always has — the raw value, no parameters, and the error —
// even where Parse read that field for the entity; see metadata_repair.go.
// ParseMediaTypeTolerant is the reading that does not fail, for a caller that
// wants it without a parse, and FilenameCandidates reads the filename
// tolerantly.
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
// Like ContentType, it parses under no budget, repairs nothing, and reads the
// header as it is now; see there, Entity.EffectiveContentDisposition for the
// field as an entity was constructed with it, and
// ParseContentDispositionTolerant for the reading that does not fail.
func (h *Header) ContentDisposition() (disp string, params map[string]string, err error) {
	return parseHeaderWithParams(h.Get("Content-Disposition"))
}

// SetContentDisposition formats the Content-Disposition header field, as
// defined in RFC 2183.
func (h *Header) SetContentDisposition(disp string, params map[string]string) {
	h.Set("Content-Disposition", formatHeaderWithParams(disp, params))
}

// Text parses a plaintext header field. Its RFC 2047 encoded words are decoded
// to UTF-8, tolerantly: a word glued to other text, split by folding, longer
// than 75 bytes, missing its padding or its closing "?=" is still decoded, and
// adjacent words of one charset are joined before conversion, so a character
// split across two words is read whole.
//
// The result is the best decoding there is, and it is always valid UTF-8. An
// encoded word that cannot be decoded is left in it as it was written, and the
// rest is decoded around it. If that was for the word's charset — unknown to
// CharsetReader, or its reader failed — the error is the first such failure:
// for an unknown charset it verifies IsUnknownCharset, and for a reader that
// returned nothing 100 times in a row it is io.ErrNoProgress. The string
// returned with it is still the usable result. A value with a single such word
// comes back unchanged, as it did before. Nothing else is an error.
//
// A CR, LF or NUL that an encoded word decodes to is replaced (a line break by
// a space, NUL by U+FFFD), and raw text that is not valid UTF-8 has each
// invalid byte replaced by U+FFFD. The raw value stays available through Get
// and Raw. TextDiagnostics returns the same string with a report of every
// repair, and TextWithOptions adds a fallback charset for raw 8-bit text.
func (h *Header) Text(k string) (string, error) {
	return decodeHeader(h.Get(k))
}

// TextDiagnostics parses a plaintext header field as Text does and reports what
// it had to repair to do so, instead of an error. Every diagnostic names k as
// its Field and locates itself by byte offsets into the field's value, as Get
// returns it. See DecodeHeaderText for what is reported.
func (h *Header) TextDiagnostics(k string) (string, []Diagnostic) {
	return fieldDiagnostics(k, h.Get(k), nil)
}

// TextWithOptions is TextDiagnostics with options, of which a nil pointer means
// none: HeaderTextOptions.FallbackCharset names the charset of raw 8-bit text
// that is not valid UTF-8.
func (h *Header) TextWithOptions(k string, opts *HeaderTextOptions) (string, []Diagnostic) {
	return fieldDiagnostics(k, h.Get(k), opts)
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
