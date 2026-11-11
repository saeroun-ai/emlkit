package message

// CodeMIMEParameterFallbackCharset reports that a Content-Type or
// Content-Disposition parameter value held raw 8-bit bytes that are not
// UTF-8, that nothing in the entity's header names their charset, and that
// they were decoded with the caller's fallback charset
// (ParseOptions.FallbackCharset), in which they are valid. It is
// SeverityRecovered, and the candidate is marked ParameterCandidate.Fallback.
// Bytes the fallback charset does not decode cleanly stay as they are, under
// mime.parameter_unknown_charset.
const CodeMIMEParameterFallbackCharset Code = "mime.parameter_fallback_charset"
