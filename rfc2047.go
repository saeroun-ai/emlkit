package message

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode/utf8"
)

// Tolerant decoding of RFC 2047 encoded words (ROB-030, issues #25 and #26).
//
// Header text is decoded to be understood, not to be judged. One pass over the
// value finds every encoded word, decodes the ones it can, and leaves the
// others exactly as they were written, so a broken word never costs its
// neighbours and no value comes back as "an error and nothing". The raw value
// is untouched in the Header; what was repaired is reported as diagnostics by
// DecodeHeaderText and Header.TextDiagnostics.
//
// What a word is. "=?" charset "?" encoding "?" text "?=", where the charset is
// a label of at most 128 bytes (an RFC 2047 token, or one with ".", ":" or
// quotes in it; it may be empty), the encoding is one letter or digit, and the
// text runs to the first "?=" after it. The text may hold white space, a bare
// "?" and 8-bit bytes, and a word may be longer than 75 bytes: mail in the wild
// does all of these. A word needs no white space around it, so one glued to
// other text or sitting in a quoted string is decoded where it stands. A word
// with no "?=" before the next "=?" or the end of the value is decoded up to
// the next white space.
//
// How it is decoded.
//
//   - B: standard base64. Text that is not canonical is repaired: white space
//     is dropped, missing, surplus and misplaced padding is accepted, the
//     URL-safe "-" and "_" are read as "+" and "/", and other bytes are
//     ignored. A word that yields no byte this way, or holds more foreign bytes
//     than base64 ones, is left verbatim.
//   - Q: "_" is a space, "=XX" a byte, in either case of hex digit. A malformed
//     escape keeps its "=" as literal text, an unencoded byte stands for
//     itself, and a line break inside the word is dropped.
//   - An unknown encoding letter leaves the word verbatim.
//   - The charset label is matched without regard to case, after an RFC 2231
//     language suffix ("utf-8*en") and surrounding quotes are removed. utf-8,
//     us-ascii and iso-8859-1 need no CharsetReader; every other label goes
//     through it. A charset it does not know leaves the word verbatim.
//   - A word labelled us-ascii, or with the placeholder unknown-8bit (RFC
//     1428), x-unknown or unknown, has no usable label once it holds 8-bit
//     bytes. They are read as UTF-8 if they are valid UTF-8, and otherwise with
//     the fallback charset if one is configured. Without one, the invalid
//     bytes of a us-ascii word become U+FFFD and a placeholder word stays
//     verbatim, an unknown charset.
//   - Adjacent words — nothing but white space, or nothing at all, between
//     them — in the same charset are joined as bytes before the charset is
//     converted, so a multi-byte character split across two words survives.
//     The white space between two decoded words is dropped; white space next to
//     plain text, or next to a word left verbatim, is kept.
//   - CR and LF in decoded text become a space (CRLF one space) and NUL becomes
//     U+FFFD, so decoded text can never be written back as a new header line.
//     Bytes that are not valid UTF-8 after conversion become U+FFFD each.
//
// Text outside encoded words. Header.Text and DecodeHeaderText keep valid
// UTF-8 as it is; text that is not valid UTF-8 is decoded with the configured
// fallback charset, or else has each invalid byte replaced by U+FFFD. The
// parameter values of Content-Type and Content-Disposition take the same word
// decoding, but everything outside their words is passed on byte for byte: a
// boundary parameter is compared with the bytes of a body.
//
// Bounds. The scan is linear in the value: the two-byte marks are found by
// cursors that only move forward, a candidate that is not a word costs no more
// than its distance to the next one, and the bytes of a word are decoded once.
// At most maxEncodedWords words are decoded per value and the rest is left
// raw. A CharsetReader's output is capped at six times its input plus 1 KiB.
// Under Header.Text, DecodeHeaderText and Parse a reader the hook returned is
// given up after 100 reads that return nothing and take nothing; see
// progress.go.

const (
	// maxEncodedWords is how many encoded words one value has decoded; the
	// rest of the value is left as it is.
	maxEncodedWords = 10000

	// maxCharsetLabelBytes bounds the charset label of an encoded word,
	// language suffix included.
	maxCharsetLabelBytes = 128

	// maxHeaderTextDiagnostics is how many diagnostics one value reports. The
	// last of them is a limit.diagnostic_count saying the reporting stopped.
	maxHeaderTextDiagnostics = 64

	// replacementChar is U+FFFD, written for a byte that is not text.
	replacementChar = string(utf8.RuneError)
)

const (
	wordEmbeddedAction     = "decoded the encoded word where it stands and kept the adjacent text"
	wordQuotedAction       = "decoded the encoded word inside the quoted string"
	wordUnterminatedAction = "decoded the encoded text up to the next white space, encoded word or end of the value"
	wordVerbatimAction     = "kept the encoded word verbatim and decoded the rest of the value"
	wordBase64Action       = "dropped white space and bytes outside the base64 alphabet, accepted missing or misplaced padding, and decoded the rest of the word"
	wordQAction            = "kept each malformed escape and unencoded byte as literal text, dropped line breaks, and decoded the rest of the word"
	wordAssumedUTF8Action  = "decoded its bytes as UTF-8"
	wordInjectionAction    = "replaced each decoded line break with a space and each decoded NUL with U+FFFD"
	wordInvalidUTF8Action  = "replaced each invalid byte of the decoded text with U+FFFD"
	wordLimitAction        = "left the rest of the value undecoded"
	rawUTF8Action          = "kept the raw text as UTF-8"
	rawReplacedAction      = "replaced each invalid byte with U+FFFD; the raw value is unchanged in the header"
	rawFallbackFailAction  = "could not apply the fallback charset and replaced each invalid byte with U+FFFD; the raw value is unchanged in the header"
	wordDiagnosticsAction  = "kept decoding and stopped reporting diagnostics for this value"
)

// errWordExpansion and errWordReader fail the conversion of one run of encoded
// words, which is then left verbatim.
var (
	errWordExpansion = errors.New("message: charset decoder output exceeded the expansion bound of an encoded word")
	errWordReader    = errors.New("message: charset decoder returned an invalid byte count")
)

// HeaderTextOptions configures DecodeHeaderTextWithOptions and
// Header.TextWithOptions. The zero value, and a nil pointer, decode as
// Header.Text does.
type HeaderTextOptions struct {
	// FallbackCharset is the charset assumed for text outside encoded words
	// that is not valid UTF-8: raw 8-bit header text, which carries no label
	// of its own. It is matched like a charset label of an encoded word and
	// converted through CharsetReader, except iso-8859-1, which needs none.
	// Each run of such text between encoded words is converted as a whole.
	//
	// When it is empty, names UTF-8 or US-ASCII, or cannot be applied, every
	// invalid byte is replaced by U+FFFD instead. Text that is valid UTF-8 is
	// never converted.
	//
	// It is also the charset of an encoded word that carries no usable label
	// of its own: one labelled us-ascii or unknown-8bit whose bytes are 8-bit
	// and not valid UTF-8. A word with a real label is never converted with
	// it, whatever its bytes.
	FallbackCharset string
}

// DecodeHeaderText decodes the RFC 2047 encoded words of a header field value
// to UTF-8 and reports everything it had to repair.
//
// It never fails: an encoded word that cannot be decoded — unknown charset,
// unknown encoding, text beyond repair — is left in the result exactly as it
// was written, and every other word is still decoded. The result is always
// valid UTF-8 and never holds a CR, LF or NUL that came out of an encoded
// word.
//
// Each diagnostic has a Code, a Severity, the Action that was taken, and a
// Location whose offsets are byte positions in s. SeverityRecovered marks a
// repair, SeverityUnsupported a word left verbatim for want of a decoder,
// SeverityInvalidMetadata text that could not be recovered, and SeverityInfo
// raw UTF-8 that was kept. A value reports at most 64 diagnostics, the last of
// them a limit.diagnostic_count; decoding is not affected by that.
//
// A conformant value yields the same string as before and no diagnostics.
func DecodeHeaderText(s string) (string, []Diagnostic) {
	return DecodeHeaderTextWithOptions(s, nil)
}

// DecodeHeaderTextWithOptions is DecodeHeaderText with a fallback charset for
// raw 8-bit text; see HeaderTextOptions.
func DecodeHeaderTextWithOptions(s string, opts *HeaderTextOptions) (string, []Diagnostic) {
	var guard valueDecoder
	d := wordScan{s: s, cr: guard.charsetReader, text: true, observe: true, maxWords: maxEncodedWords}
	if opts != nil {
		// Only a label that converts something is a fallback.
		label := normalizeCharsetLabel(opts.FallbackCharset)
		if class := classifyCharset(label); class == charsetHook || class == charsetLatin1 {
			d.fallback = label
		}
	}
	return d.decode(), d.diags
}

// decodeHeaderText is the decoding behind Header.Text: DecodeHeaderText
// without the diagnostics, with the first failure of a charset — an
// UnknownCharsetError from the hook, or whatever its reader failed with —
// returned next to the string that was decoded around it.
func decodeHeaderText(s string) (string, error) {
	if !strings.Contains(s, "=?") && utf8.ValidString(s) {
		return s, nil
	}
	var guard valueDecoder
	d := wordScan{s: s, cr: guard.charsetReader, text: true, maxWords: maxEncodedWords}
	return d.decode(), d.err
}

// decodeParameterValue decodes the encoded words of one parameter value
// through cr and passes everything outside them on byte for byte.
func decodeParameterValue(v string, cr charsetReaderFunc) string {
	if !strings.Contains(v, "=?") {
		return v
	}
	d := wordScan{s: v, cr: cr, maxWords: maxEncodedWords}
	return d.decode()
}

// A charsetReaderFunc has the signature of CharsetReader.
type charsetReaderFunc func(charset string, input io.Reader) (io.Reader, error)

// A wordScan decodes the encoded words of one value. It is used once.
type wordScan struct {
	s        string
	cr       charsetReaderFunc
	text     bool   // sanitize the text outside encoded words to UTF-8
	fallback string // normalized fallback charset of that text, or ""
	observe  bool   // collect diagnostics
	maxWords int

	out    strings.Builder
	starts finder // "=?"
	closes finder // "?="
	words  int
	run    wordRun
	err    error // the first charset failure

	diags       []Diagnostic
	full        bool  // the diagnostic budget is spent
	rawNoted    bool  // raw UTF-8 was reported once
	quoteAt     int   // quotes are counted up to here
	quoteOpen   bool  // an unescaped '"' is open at quoteAt
	provisional []int // rfc2047.illegal_location entries, kept if the quotes balance
}

// An encodedWord is one encoded word found in the value.
type encodedWord struct {
	start, end int    // the word within the value
	label      string // the charset as written
	charset    string // the charset, normalized
	enc        byte   // the encoding letter, lower case
	data       []byte // the decoded bytes, before charset conversion
	open       bool   // there was no closing "?="
	irregular  bool   // the text was repaired while decoding
	bad        bool   // the word cannot be decoded and stays verbatim
}

// A wordRun is the adjacent encoded words of one charset collected so far,
// joined as bytes and converted together.
type wordRun struct {
	active     bool
	charset    string
	label      string
	data       []byte
	start, end int // the words within the value, white space between them included
	lead       int // start of the white space dropped before the run, or -1
}

// A finder reports the first occurrence of a two-byte mark at or after a
// position. The positions it is asked about never decrease, which makes all of
// its searches together linear in the string.
type finder struct {
	s, mark string
	at      int
	state   uint8 // 0 not searched, 1 found at at, 2 no further occurrence
}

func (f *finder) next(from int) int {
	switch {
	case f.state == 2:
		return -1
	case f.state == 1 && f.at >= from:
		return f.at
	}
	if from > len(f.s) {
		from = len(f.s)
	}
	i := strings.Index(f.s[from:], f.mark)
	if i < 0 {
		f.state = 2
		return -1
	}
	f.at, f.state = from+i, 1
	return f.at
}

// decode scans the value once and returns it decoded.
func (d *wordScan) decode() string {
	s := d.s
	d.starts = finder{s: s, mark: "=?"}
	d.closes = finder{s: s, mark: "?="}
	d.out.Grow(len(s))
	pos, from := 0, 0 // the pending literal starts at pos; the search at from
	for {
		p := d.starts.next(from)
		if p < 0 {
			break
		}
		if d.words >= d.maxWords {
			if d.observe {
				d.note(CodeRFC2047WordLimit, SeverityLimitExceeded, p, len(s),
					fmt.Sprintf("the value holds more than %d encoded words", d.maxWords), wordLimitAction, nil)
			}
			break
		}
		w, resume, ok := d.parseWord(p)
		if !ok {
			from = resume
			continue
		}
		d.words++
		d.token(pos, &w)
		pos, from = w.end, w.end
	}
	d.flushRun()
	d.literal(pos, len(s), true)
	d.settle()
	return d.out.String()
}

// parseWord reads the candidate encoded word at p, where "=?" stands. When it
// is not a word, ok is false and resume is where the search for the next
// candidate continues; no candidate can start between p+2 and resume.
func (d *wordScan) parseWord(p int) (w encodedWord, resume int, ok bool) {
	s := d.s
	i := p + 2
	j := i
	for j < len(s) && j-i < maxCharsetLabelBytes && isCharsetLabelByte(s[j]) {
		j++
	}
	if j+2 >= len(s) || s[j] != '?' || !isAlphanumeric(s[j+1]) || s[j+2] != '?' {
		return w, p + 2, false
	}
	ts := j + 3 // the encoded text starts here
	w.start = p
	w.label = s[i:j]
	w.charset = normalizeCharsetLabel(w.label)
	w.enc = s[j+1] | 0x20

	// The text ends at the first "?=", unless another word starts before it:
	// then this one was never closed.
	te := -1
	closeAt := d.closes.next(ts)
	nested := d.starts.next(ts)
	if closeAt >= 0 && (nested < 0 || nested+1 >= closeAt) {
		te = closeAt
		w.end = closeAt + 2
	} else {
		limit := len(s)
		if nested >= 0 {
			limit = nested
		}
		te = limit
		if k := indexWhitespace(s[ts:limit]); k >= 0 {
			te = ts + k
		}
		if te == ts {
			return w, ts, false
		}
		w.end = te
		w.open = true
	}

	text := s[ts:te]
	switch w.enc {
	case 'b':
		var decoded bool
		w.data, w.irregular, decoded = decodeBWord(text)
		w.bad = !decoded
	case 'q':
		w.data, w.irregular = decodeQWord(text)
	default:
		w.bad = true
	}
	if w.bad && w.open {
		// Too little of a word to call it one.
		return w, ts, false
	}
	return w, w.end, true
}

// token adds the word w, and the literal text between pos and it, to the
// output.
func (d *wordScan) token(pos int, w *encodedWord) {
	// Nothing but white space, or nothing, lies between the run and w.
	adjacent := d.run.active && isAllWhitespace(d.s[pos:w.start])
	if adjacent && !w.bad && d.run.charset == w.charset {
		if d.observe {
			d.noteWord(w)
		}
		d.run.data = append(d.run.data, w.data...)
		d.run.end = w.end
		return
	}
	converted := d.flushRun()
	// White space between two decoded words is dropped. It is held back
	// rather than dropped here, because w may yet stay verbatim.
	hold := adjacent && converted && !w.bad
	if !hold {
		d.literal(pos, w.start, true)
	}
	if d.observe {
		d.noteWord(w)
	}
	if w.bad {
		d.literal(w.start, w.end, false)
		return
	}
	d.run = wordRun{active: true, charset: w.charset, label: w.label, data: w.data, start: w.start, end: w.end, lead: -1}
	if hold {
		d.run.lead = pos
	}
}

// flushRun converts the collected run and writes it, or writes its words
// verbatim if the charset cannot be converted, and reports whether it was
// converted. With no run collected it reports true.
func (d *wordScan) flushRun() bool {
	if !d.run.active {
		return true
	}
	r := d.run
	d.run = wordRun{}
	converted, assumed, err := d.convert(r.charset, r.data)
	if err != nil {
		if err == errMetadataStalled {
			// The guard's own value, compared by identity: a reader of the
			// hook made no progress, or the hook is no longer asked because
			// one did. Callers get the name they know for that.
			err = io.ErrNoProgress
		}
		if d.err == nil {
			d.err = err
		}
		if d.observe {
			// An unknown charset is the UnknownCharsetError charsetReader
			// wraps the hook's refusal in; anything else is a failure of the
			// reader the hook returned, or of its guard. The error's own
			// type decides that; it is never walked.
			if isCharsetWarning(err) {
				d.note(CodeRFC2047UnknownCharset, SeverityUnsupported, r.start, r.end,
					fmt.Sprintf("no decoder for the charset %q of an encoded word", r.label), wordVerbatimAction, err)
			} else {
				d.note(CodeRFC2047InvalidWord, SeverityInvalidMetadata, r.start, r.end,
					fmt.Sprintf("the decoder of the charset %q failed on an encoded word", r.label), wordVerbatimAction, err)
			}
		}
		if r.lead >= 0 {
			d.literal(r.lead, r.start, true)
		}
		d.literal(r.start, r.end, false)
		return false
	}
	if d.observe {
		action := wordAssumedUTF8Action
		if assumed {
			action = fmt.Sprintf("decoded its bytes as %s, the configured fallback charset", d.fallback)
		}
		switch class := classifyCharset(r.charset); {
		case class == charsetPlaceholder:
			d.note(CodeRFC2047UnknownCharset, SeverityRecovered, r.start, r.end,
				fmt.Sprintf("an encoded word names the placeholder charset %q", r.label), action, nil)
		case class == charsetASCII && r.charset == "":
			d.note(CodeCharsetEmpty, SeverityRecovered, r.start, r.end,
				"an encoded word names no charset", action, nil)
		case class == charsetASCII && !isASCII(r.data):
			d.note(CodeCharsetDeclarationSuspect, SeverityRecovered, r.start, r.end,
				"an encoded word declared as US-ASCII holds 8-bit bytes", action, nil)
		}
	}
	injected, invalid := d.writeBytes(converted, true)
	if d.observe {
		if injected {
			d.note(CodeRFC2047HeaderInjection, SeverityRecovered, r.start, r.end,
				"an encoded word decodes to CR, LF or NUL", wordInjectionAction, nil)
		}
		if invalid {
			d.note(CodeCharsetInvalidSequence, SeverityRecovered, r.start, r.end,
				"the decoded text of an encoded word is not valid UTF-8", wordInvalidUTF8Action, nil)
		}
	}
	return true
}

// The ways a normalized charset label is converted.
const (
	charsetHook        = iota // by the CharsetReader
	charsetUTF8               // nothing to convert
	charsetASCII              // us-ascii, or no label: 7-bit by declaration
	charsetPlaceholder        // unknown-8bit and its kin: the producer did not know either
	charsetLatin1             // iso-8859-1, byte for code point
)

func classifyCharset(charset string) int {
	switch charset {
	case "utf-8", "utf8":
		return charsetUTF8
	case "", "us-ascii", "ascii":
		return charsetASCII
	case "unknown-8bit", "x-unknown", "unknown":
		return charsetPlaceholder
	case "iso-8859-1":
		return charsetLatin1
	}
	return charsetHook
}

// convert returns data, bytes in charset, as UTF-8. The result may be data
// itself, and is not yet known to be valid UTF-8.
//
// Bytes labelled US-ASCII, or with a placeholder such as unknown-8bit, carry no
// usable label once they are 8-bit: they are kept if they are valid UTF-8, and
// otherwise converted with the fallback charset if there is one, which assumed
// reports. Without one, US-ASCII bytes are returned as they are, for the caller
// to replace, and placeholder bytes are an unknown charset.
func (d *wordScan) convert(charset string, data []byte) (converted []byte, assumed bool, err error) {
	switch classifyCharset(charset) {
	case charsetUTF8:
		return data, false, nil
	case charsetASCII, charsetPlaceholder:
		if utf8.Valid(data) {
			return data, false, nil
		}
		if d.fallback != "" {
			// The fallback is never one of these two classes itself.
			if converted, _, err := d.convert(d.fallback, data); err == nil {
				return converted, true, nil
			}
		}
		if classifyCharset(charset) == charsetASCII {
			return data, false, nil
		}
		return nil, false, UnknownCharsetError{fmt.Errorf("message: unhandled charset %q", charset)}
	case charsetLatin1:
		if isASCII(data) {
			return data, false, nil
		}
		converted = make([]byte, 0, len(data)+len(data)/2)
		for _, c := range data {
			converted = utf8.AppendRune(converted, rune(c))
		}
		return converted, false, nil
	}
	if d.cr == nil {
		return nil, false, UnknownCharsetError{fmt.Errorf("message: unhandled charset %q", charset)}
	}
	r, err := d.cr(charset, bytes.NewReader(data))
	if err != nil {
		return nil, false, err
	}
	if r == nil {
		return nil, false, UnknownCharsetError{fmt.Errorf("message: charset reader returned neither a reader nor an error for %q", charset)}
	}
	converted, err = readConverted(r, len(data))
	return converted, false, err
}

// readConverted reads everything r delivers for in bytes of input. Output past
// six times the input plus 1 KiB fails it, which no charset conversion comes
// near. A read that returns nothing is asked again, as io.Copy does; the
// callers that must not wait for ever hand in a reader that gives up.
func readConverted(r io.Reader, in int) ([]byte, error) {
	limit := math.MaxInt
	if in < (math.MaxInt-1024)/6 {
		limit = 6*in + 1024
	}
	buf := make([]byte, 0, in+in/2+16)
	for {
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)]
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		if n < 0 || n > cap(buf)-len(buf) {
			return nil, errWordReader
		}
		buf = buf[:len(buf)+n]
		if len(buf) > limit {
			return nil, errWordExpansion
		}
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// literal writes s[start:end], text that is not decoded. plain marks the text
// between encoded words, which is where raw 8-bit text is diagnosed and the
// fallback charset applies; a word left verbatim is not plain.
func (d *wordScan) literal(start, end int, plain bool) {
	if start >= end {
		return
	}
	seg := d.s[start:end]
	if !d.text {
		d.out.WriteString(seg)
		return
	}
	first := indexNonASCII(seg)
	if first < 0 {
		d.out.WriteString(seg)
		return
	}
	if utf8.ValidString(seg[first:]) {
		d.out.WriteString(seg)
		if plain && d.observe && !d.rawNoted {
			d.rawNoted = true
			d.note(CodeRFC2047RawUTF8, SeverityInfo, start+first, end,
				"the value holds raw UTF-8 text outside encoded words", rawUTF8Action, nil)
		}
		return
	}
	var cause error
	if plain && d.fallback != "" {
		converted, _, err := d.convert(d.fallback, []byte(seg))
		if err == nil {
			d.writeBytes(converted, false)
			if d.observe {
				d.note(CodeHeaderInvalidUTF8, SeverityRecovered, start+first, end,
					"the value holds raw 8-bit text that is not valid UTF-8",
					fmt.Sprintf("decoded the raw text as %s, the configured fallback charset", d.fallback), nil)
			}
			return
		}
		cause = err
	}
	d.writeBytes([]byte(seg), false)
	if plain && d.observe {
		if cause != nil {
			d.note(CodeHeaderInvalidUTF8, SeverityUnsupported, start+first, end,
				"the value holds raw 8-bit text that is not valid UTF-8", rawFallbackFailAction, cause)
		} else {
			d.note(CodeHeaderInvalidUTF8, SeverityInvalidMetadata, start+first, end,
				"the value holds raw 8-bit text that is not valid UTF-8", rawReplacedAction, nil)
		}
	}
}

// writeBytes writes b with every byte that is not part of valid UTF-8 replaced
// by U+FFFD. With neutralize it also replaces CR and LF by a space, a CRLF
// pair by one, and NUL by U+FFFD. It reports what it had to replace.
func (d *wordScan) writeBytes(b []byte, neutralize bool) (injected, invalid bool) {
	clean := true
	for _, c := range b {
		if c >= utf8.RuneSelf || neutralize && (c == '\r' || c == '\n' || c == 0) {
			clean = false
			break
		}
	}
	if clean {
		d.out.Write(b)
		return false, false
	}
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c >= utf8.RuneSelf:
			r, size := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && size == 1 {
				d.out.WriteString(replacementChar)
				invalid = true
			} else {
				d.out.Write(b[i : i+size])
			}
			i += size
			continue
		case !neutralize:
			d.out.WriteByte(c)
		case c == '\r':
			if i+1 < len(b) && b[i+1] == '\n' {
				i++
			}
			d.out.WriteByte(' ')
			injected = true
		case c == '\n':
			d.out.WriteByte(' ')
			injected = true
		case c == 0:
			d.out.WriteString(replacementChar)
			injected = true
		default:
			d.out.WriteByte(c)
		}
		i++
	}
	return injected, invalid
}

// noteWord reports what is irregular about one encoded word.
func (d *wordScan) noteWord(w *encodedWord) {
	s := d.s
	d.countQuotes(w.start)
	d.quoteAt = w.end
	switch {
	case d.quoteOpen:
		if i := d.note(CodeRFC2047IllegalLocation, SeverityRecovered, w.start, w.end,
			"an encoded word appears inside a quoted string", wordQuotedAction, nil); i >= 0 {
			d.provisional = append(d.provisional, i)
		}
	case w.start > 0 && !isWordBoundary(s[w.start-1], '('), w.end < len(s) && !isWordBoundary(s[w.end], ')'):
		d.note(CodeRFC2047EmbeddedWord, SeverityRecovered, w.start, w.end,
			"an encoded word is not separated from the adjacent text by white space", wordEmbeddedAction, nil)
	}
	if w.open {
		d.note(CodeRFC2047InvalidWord, SeverityRecovered, w.start, w.end,
			"an encoded word has no closing \"?=\"", wordUnterminatedAction, nil)
	}
	switch {
	case w.bad && w.enc != 'b':
		d.note(CodeRFC2047UnknownEncoding, SeverityUnsupported, w.start, w.end,
			fmt.Sprintf("an encoded word names the unknown encoding %q", string(s[w.start+len(w.label)+3])), wordVerbatimAction, nil)
		return
	case w.bad:
		d.note(CodeRFC2047InvalidBase64, SeverityInvalidMetadata, w.start, w.end,
			"the base64 text of an encoded word cannot be decoded", wordVerbatimAction, nil)
		return
	case w.irregular && w.enc == 'b':
		d.note(CodeRFC2047InvalidBase64, SeverityRecovered, w.start, w.end,
			"the base64 text of an encoded word is malformed", wordBase64Action, nil)
	case w.irregular:
		d.note(CodeRFC2047InvalidQ, SeverityRecovered, w.start, w.end,
			"the Q text of an encoded word is malformed", wordQAction, nil)
	}
}

// countQuotes moves the quote count up to end. An encoded word is skipped by
// its caller, so only text outside words is counted, and a backslash escapes
// the byte after it.
func (d *wordScan) countQuotes(end int) {
	s := d.s
	for i := d.quoteAt; i < end; i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			d.quoteOpen = !d.quoteOpen
		}
	}
	if end > d.quoteAt {
		d.quoteAt = end
	}
}

// settle decides the provisional diagnostics once the whole value is seen: a
// word counts as inside a quoted string only if the quotes of the value
// balance. An unbalanced value is no evidence of a quoted string at all.
func (d *wordScan) settle() {
	if !d.observe || len(d.provisional) == 0 {
		return
	}
	d.countQuotes(len(d.s))
	if !d.quoteOpen {
		return
	}
	drop := d.provisional
	kept := d.diags[:0]
	for i, diag := range d.diags {
		if len(drop) > 0 && drop[0] == i {
			drop = drop[1:]
			continue
		}
		kept = append(kept, diag)
	}
	d.diags = kept
}

// note records one diagnostic located at s[start:end] and returns its index,
// or -1 once the value's diagnostic budget is spent.
func (d *wordScan) note(code Code, severity Severity, start, end int, message, action string, cause error) int {
	if d.full {
		return -1
	}
	if len(d.diags) == maxHeaderTextDiagnostics-1 {
		d.full = true
		d.diags = append(d.diags, Diagnostic{
			Code:     CodeLimitDiagnosticCount,
			Severity: SeverityLimitExceeded,
			Message:  fmt.Sprintf("the value has more than %d diagnostics", maxHeaderTextDiagnostics-1),
			Action:   wordDiagnosticsAction,
			Location: LocationRange(int64(start), int64(end)),
		})
		return -1
	}
	d.diags = append(d.diags, Diagnostic{
		Code:     code,
		Severity: severity,
		Message:  message,
		Action:   action,
		Location: LocationRange(int64(start), int64(end)),
		Cause:    cause,
	})
	return len(d.diags) - 1
}

// decodeBWord decodes the base64 text of an encoded word. Canonical text is
// decoded as it is. Otherwise white space is dropped, "-" and "_" are read as
// the URL-safe "+" and "/", padding ends a group wherever it stands, and every
// other byte is ignored; repaired reports that this was needed. ok is false
// when that yields no byte, or when more bytes were ignored than decoded.
func decodeBWord(text string) (data []byte, repaired, ok bool) {
	if b, err := base64.StdEncoding.DecodeString(text); err == nil {
		return b, false, true
	}
	data = make([]byte, 0, len(text)/4*3+3)
	var acc uint32
	bits, valid, junk := 0, 0, 0
	for i := 0; i < len(text); i++ {
		c := text[i]
		var v uint32
		switch {
		case 'A' <= c && c <= 'Z':
			v = uint32(c - 'A')
		case 'a' <= c && c <= 'z':
			v = uint32(c-'a') + 26
		case '0' <= c && c <= '9':
			v = uint32(c-'0') + 52
		case c == '+' || c == '-':
			v = 62
		case c == '/' || c == '_':
			v = 63
		case c == '=':
			acc, bits = 0, 0
			continue
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			continue
		default:
			junk++
			continue
		}
		valid++
		acc = acc<<6 | v
		bits += 6
		if bits >= 8 {
			bits -= 8
			data = append(data, byte(acc>>bits))
			acc &= 1<<bits - 1
		}
	}
	if len(data) == 0 || junk > valid {
		return nil, true, false
	}
	return data, true, true
}

// decodeQWord decodes the Q text of an encoded word. It cannot fail: a
// malformed escape keeps its "=" as text, a byte that should have been encoded
// stands for itself, and CR and LF are dropped; irregular reports any of
// these. Space and tab inside the text are kept and not reported.
func decodeQWord(text string) (data []byte, irregular bool) {
	data = make([]byte, 0, len(text))
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '_':
			data = append(data, ' ')
		case c == '=':
			if i+2 < len(text) {
				hi, ok1 := unhex(text[i+1])
				lo, ok2 := unhex(text[i+2])
				if ok1 && ok2 {
					data = append(data, hi<<4|lo)
					i += 2
					continue
				}
			}
			irregular = true
			data = append(data, c)
		case c == '\r' || c == '\n':
			irregular = true
		case c == ' ' || c == '\t':
			data = append(data, c)
		case c < ' ' || c > '~' || c == '?':
			irregular = true
			data = append(data, c)
		default:
			data = append(data, c)
		}
	}
	return data, irregular
}

func unhex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}

// normalizeCharsetLabel returns a charset label as it is matched: without an
// RFC 2231 language suffix, without surrounding quotes or white space, in
// lower case.
func normalizeCharsetLabel(label string) string {
	if i := strings.IndexByte(label, '*'); i >= 0 {
		label = label[:i]
	}
	return strings.ToLower(strings.Trim(label, "\"' \t"))
}

// isCharsetLabelByte reports whether c may appear in the charset of an encoded
// word: every byte of an RFC 2047 token, which covers the "*" of a language
// suffix, and also the ".", ":" and '"' that registered labels and careless
// producers use. White space, controls, 8-bit bytes and the other specials are
// not among them. Neither are "=" and "?", which is what keeps a chain of "=?"
// from being read as one label.
func isCharsetLabelByte(c byte) bool {
	if c <= ' ' || c >= 0x7f {
		return false
	}
	switch c {
	case '(', ')', '<', '>', '@', ',', ';', '/', '[', ']', '\\', '?', '=':
		return false
	}
	return true
}

func isAlphanumeric(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

func isLinearWhitespace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// isWordBoundary reports whether c next to an encoded word separates it from
// other text: white space, or the parenthesis of a comment the word is in.
func isWordBoundary(c, paren byte) bool {
	return isLinearWhitespace(c) || c == paren
}

func isAllWhitespace(s string) bool {
	return indexNonWhitespace(s) < 0
}

func indexNonWhitespace(s string) int {
	for i := 0; i < len(s); i++ {
		if !isLinearWhitespace(s[i]) {
			return i
		}
	}
	return -1
}

func indexWhitespace(s string) int {
	for i := 0; i < len(s); i++ {
		if isLinearWhitespace(s[i]) {
			return i
		}
	}
	return -1
}

func indexNonASCII(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return i
		}
	}
	return -1
}

func isASCII(b []byte) bool {
	for _, c := range b {
		if c >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
