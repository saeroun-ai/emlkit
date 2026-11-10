package message

import (
	"bytes"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

// RFC 2231 parameter values, read tolerantly (ROB-050-006, ROB-050-007).
//
// RFC 2231 gives a parameter two more spellings than name=value. An extended
// value, name*=charset'language'percent-encoded, declares the charset of its
// bytes. A continued value is cut into numbered segments, name*0, name*1, …,
// each of which may be percent-encoded on its own (name*1*); only the first
// carries the charset'language' prefix, and it applies to every encoded
// segment of the series.
//
// mime.ParseMediaType reads both, but only for utf-8 and us-ascii, only for a
// series that starts at 0 and has no gap, and it gives up on the whole value
// at the first bad percent escape. What is here reads the same conformant
// values to the same result and goes further where a message does not
// conform:
//
//   - Segments are assembled in ascending section order whatever order they
//     arrive in. A series with a gap, or one that does not start at 0, keeps
//     every segment that is present, in order; the gap is reported
//     (mime.parameter_segment_missing) and nothing is invented for it. Some
//     generators number from 1, and the alternative — the contiguous prefix
//     from 0 — would keep nothing of their names.
//   - A section repeated with a different value keeps its first occurrence
//     (mime.parameter_segment_duplicate). An identical repeat is harmless.
//   - Encoded and unencoded segments may be mixed, as RFC 2231 allows. Each
//     run of adjacent encoded segments is charset-decoded as one, so a
//     multi-byte character cut across two segments survives.
//   - A percent sign that is not followed by two hex digits is kept as a
//     literal percent sign (mime.parameter_bad_percent_escape).
//   - A charset other than utf-8 and us-ascii is decoded through the
//     configured CharsetReader, under the no-progress guard of progress.go.
//     When no decoder is available — or the bytes are not valid in the charset
//     declared — the percent-decoded bytes are kept as the value, which is
//     then not UTF-8, and the candidate is marked Undecoded
//     (mime.parameter_unknown_charset). Bytes that are all ASCII need no
//     decoder and are kept as text.
//   - A later encoded segment that repeats the first one's charset'language'
//     prefix has it removed, as some generators repeat it, and a series whose
//     first segment is not encoded takes the prefix of its first encoded one.

// maxSectionDigits bounds the digits of a section number. Nine digits cannot
// overflow an int, and no real series has a section that large.
const maxSectionDigits = 9

// splitParameterName splits a parameter name into the name itself, lower
// cased, and its RFC 2231 suffix. ok is false for a name whose suffix is not
// RFC 2231 syntax: something other than nothing, digits, or digits and a
// star after the first star.
func splitParameterName(attr string) (name string, form ParameterForm, section int, encoded, ok bool) {
	star := strings.IndexByte(attr, '*')
	if star < 0 {
		return strings.ToLower(attr), ParameterPlain, 0, false, attr != ""
	}
	name = strings.ToLower(attr[:star])
	if name == "" {
		return "", ParameterPlain, 0, false, false
	}
	rest := attr[star+1:]
	if rest == "" {
		// name*: one whole extended value.
		return name, ParameterExtended, 0, true, true
	}
	if strings.HasSuffix(rest, "*") {
		encoded = true
		rest = rest[:len(rest)-1]
	}
	if rest == "" || len(rest) > maxSectionDigits {
		return name, ParameterPlain, 0, false, false
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] < '0' || rest[i] > '9' {
			return name, ParameterPlain, 0, false, false
		}
		section = section*10 + int(rest[i]-'0')
	}
	return name, ParameterContinued, section, encoded, true
}

// splitExtendedValue splits charset'language'data. ok is false when v does not
// hold the two single quotes that delimit the prefix.
func splitExtendedValue(v string) (charset, language, data string, ok bool) {
	q1 := strings.IndexByte(v, '\'')
	if q1 < 0 {
		return "", "", v, false
	}
	q2 := strings.IndexByte(v[q1+1:], '\'')
	if q2 < 0 {
		return "", "", v, false
	}
	q2 += q1 + 1
	return v[:q1], v[q1+1 : q2], v[q2+1:], true
}

// percentDecode appends the bytes s percent-encodes to dst. A percent sign
// that two hex digits do not follow is kept as itself, and bad reports that
// there was one.
func percentDecode(dst []byte, s string) (out []byte, bad bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '%' {
			dst = append(dst, c)
			continue
		}
		if i+2 < len(s) {
			hi, okHi := unhex(s[i+1])
			lo, okLo := unhex(s[i+2])
			if okHi && okLo {
				dst = append(dst, hi<<4|lo)
				i += 2
				continue
			}
		}
		bad = true
		dst = append(dst, c)
	}
	return dst, bad
}

// A valuePiece is one piece of an RFC 2231 value in the order it is assembled
// in: an extended value is one encoded piece, a continued one a piece per
// segment.
type valuePiece struct {
	text    string // the piece with its quoting removed and nothing decoded
	encoded bool   // percent-encoded, in the charset the first piece declares
}

// An assembledValue is an RFC 2231 value put together from its pieces.
type assembledValue struct {
	value    string
	charset  string
	language string

	// undecoded reports that value holds bytes no charset decoded: it is the
	// percent-decoded octets and may not be UTF-8.
	undecoded bool

	// badPercent reports a percent sign kept literally; noPrefix an encoded
	// value whose charset is not declared where RFC 2231 has it declared — an
	// encoded first piece with no charset'language' prefix, or a prefix found
	// on a later piece instead; unknownCharset bytes left undecoded.
	badPercent     bool
	noPrefix       bool
	unknownCharset bool
}

// assemblePieces decodes pieces and joins them. dec decodes a charset other
// than utf-8 and us-ascii through the configured CharsetReader.
func assemblePieces(pieces []valuePiece, dec *metadataDecoder) assembledValue {
	var (
		out        assembledValue
		b          strings.Builder
		run        []byte
		inRun      bool
		sawEncoded bool
	)
	flush := func() {
		if !inRun {
			return
		}
		b.WriteString(decodeRun(run, dec, &out))
		run, inRun = run[:0], false
	}
	for i, p := range pieces {
		if !p.encoded {
			flush()
			b.WriteString(p.text)
			continue
		}
		data := p.text
		switch {
		case i == 0:
			if cs, lang, rest, ok := splitExtendedValue(data); ok {
				out.charset, out.language, data = cs, lang, rest
			} else {
				out.noPrefix = true
			}
		case out.charset != "":
			// A generator that repeats the prefix on every segment.
			if cs, lang, rest, ok := splitExtendedValue(data); ok &&
				strings.EqualFold(cs, out.charset) && strings.EqualFold(lang, out.language) {
				data = rest
			}
		case !sawEncoded:
			// The first segment was not encoded, so nothing has declared a
			// charset; a generator that declares it on its first encoded
			// segment instead is taken at its word when the prefix looks like
			// one.
			if cs, lang, rest, ok := splitExtendedValue(data); ok && plausiblePrefix(cs, lang) {
				out.charset, out.language, data = cs, lang, rest
				out.noPrefix = true
			}
		}
		sawEncoded = true
		var bad bool
		run, bad = percentDecode(run, data)
		out.badPercent = out.badPercent || bad
		inRun = true
	}
	flush()
	out.value = b.String()
	return out
}

// plausiblePrefix reports whether charset and language look like the prefix
// of an extended value rather than like data that happens to hold two single
// quotes: a charset that is a short token with no percent sign, and a language
// tag of letters, digits and hyphens, or none.
func plausiblePrefix(charset, language string) bool {
	if len(charset) > 40 || !isToken(charset) || strings.IndexByte(charset, '%') >= 0 {
		return false
	}
	for i := 0; i < len(language); i++ {
		c := language[i]
		if !(c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// decodeRun returns the text of one run of percent-decoded bytes in the
// charset out declares, or the bytes themselves when nothing decodes them.
func decodeRun(run []byte, dec *metadataDecoder, out *assembledValue) string {
	cs := strings.ToLower(strings.TrimSpace(out.charset))
	switch cs {
	case "utf-8", "us-ascii", "":
		// As the whole-field parser reads them: the bytes are the text. Bytes
		// that are not UTF-8 are kept, and said to be undecoded.
		if !utf8.Valid(run) {
			out.undecoded, out.unknownCharset = true, true
		}
		return string(run)
	}
	if s, ok := dec.convertCharset(cs, run); ok {
		return s
	}
	if isASCII(run) && asciiCompatibleLabel(cs) {
		// No decoder is needed for bytes every ASCII-compatible charset reads
		// alike.
		return string(run)
	}
	out.undecoded, out.unknownCharset = true, true
	return string(run)
}

// asciiCompatibleLabel reports whether a charset label does not name one of
// the encodings in which ASCII bytes are not ASCII text.
func asciiCompatibleLabel(cs string) bool {
	for _, p := range []string{"utf-16", "utf16", "utf-32", "utf32", "utf-7", "utf7", "ucs-", "ucs2", "ucs4", "unicode", "cp037", "ibm037", "iso-2022", "hz-"} {
		if strings.HasPrefix(cs, p) {
			return false
		}
	}
	return !strings.Contains(cs, "ebcdic")
}

// convertCharset decodes b from charset to UTF-8 through the configured
// CharsetReader, with the reader it returns held to the no-progress rule as
// for an encoded word, and its output capped at a small multiple of the input:
// no charset expands a byte to more than a few bytes of UTF-8, so a reader that
// keeps delivering is not a decoder of these bytes. ok is false when there is
// no decoder, the reader failed or stalled, or the parse was cancelled.
func (d *metadataDecoder) convertCharset(charset string, b []byte) (string, bool) {
	key := "\x00charset\x00" + charset + "\x00" + string(b)
	if d.stalled[key] {
		return "", false
	}
	w := &valueDecoder{poll: d.poll}
	r, err := w.charsetReader(charset, bytes.NewReader(b))
	if err != nil || r == nil {
		return "", false
	}
	limit := int64(len(b))*8 + 64
	out, err := io.ReadAll(io.LimitReader(r, limit+1))
	if w.stalled {
		if d.stalled == nil {
			d.stalled = make(map[string]bool)
		}
		d.stalled[key] = true
		return "", false
	}
	if err != nil || int64(len(out)) > limit {
		return "", false
	}
	return string(out), true
}

// A parameterSegment is one continuation segment of a parameter, as it
// arrived.
type parameterSegment struct {
	section int
	encoded bool
	text    string
	raw     string
}

// orderSegments returns segs in ascending section order with repeated
// sections removed, the first occurrence kept. duplicate reports a section
// repeated with a different value, missing a series that does not run from 0
// without a gap. The order of arrival is kept where it is already ascending,
// which is the common case and costs one pass.
func orderSegments(segs []parameterSegment) (ordered []parameterSegment, missing, duplicate bool) {
	sorted := true
	for i := 1; i < len(segs); i++ {
		if segs[i].section <= segs[i-1].section {
			sorted = false
			break
		}
	}
	ordered = segs
	if !sorted {
		ordered = make([]parameterSegment, len(segs))
		copy(ordered, segs)
		sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].section < ordered[j].section })
		kept := ordered[:1]
		for _, s := range ordered[1:] {
			last := kept[len(kept)-1]
			if s.section == last.section {
				if s.text != last.text || s.encoded != last.encoded {
					duplicate = true
				}
				continue
			}
			kept = append(kept, s)
		}
		ordered = kept
	}
	for i, s := range ordered {
		if s.section != i {
			missing = true
			break
		}
	}
	return ordered, missing, duplicate
}
