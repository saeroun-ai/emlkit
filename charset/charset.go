// Package charset provides functions to decode and encode charsets.
//
// It imports all supported charsets, which adds about 1MiB to binaries size.
// Importing the package automatically sets message.CharsetReader.
//
// # Labels
//
// Mail names its charsets loosely, so a label is resolved in this order, and
// the first match wins (see Lookup):
//
//  1. encodings added with RegisterEncoding;
//  2. this package's alias table (aliases.go): vendor names, common mislabels,
//     and registered charsets golang.org/x/text has no decoder for;
//  3. the IANA registry and the WHATWG Encoding Standard labels, as
//     golang.org/x/text knows them.
//
// Matching ignores case. A label no step knows is tried once more with the
// decoration real mail puts around charset names removed: surrounding white
// space, quotes and stray punctuation, a leading "charset=", and anything
// after the name such as "; format=flowed" or an RFC 2231 "*en" language tag.
// A label that still matches nothing is an error, which message reports as an
// unknown charset while keeping the undecoded body: nothing is decoded on a
// guess.
//
// # Decoding
//
// Every decoder of this package is a streaming transformer with fixed
// buffers. A byte sequence that is not valid in the charset becomes U+FFFD and
// decoding goes on with the next byte, so neither the text before a fault nor
// the text after it is lost, and no decoder returns an error of its own.
// Output is always valid UTF-8 and at most three times as long as the input.
//
// Three choices are deliberately wider than the label, because that is what the
// label means in mail (Match.Repairs reports them):
//
//   - iso-8859-1 is decoded as windows-1252 and iso-8859-9 as windows-1254.
//     They differ only in 0x80-0x9F, which are unused control codes in the ISO
//     sets and punctuation in the Windows ones;
//   - us-ascii and labels that name no charset (unknown-8bit, x-unknown, an
//     empty label, ...) keep every valid UTF-8 sequence and read any other
//     byte above 0x7F as windows-1252;
//   - utf-16 without a byte order mark is big-endian (RFC 2781) unless its
//     first bytes say it is little-endian, which is what Windows senders
//     produce.
//
// A byte order mark is kept as U+FEFF in utf-8, utf-16le, utf-16be, utf-32le
// and utf-32be, where it is text, and consumed in utf-16 and utf-32, where it
// is the signature that selects the byte order.
//
// The decisions behind all of this are recorded in
// docs/robustness-notes/charset-plugin.md.
package charset

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/saeroun-ai/emlkit"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// charsets holds the encodings added with RegisterEncoding, by lower-case
// label. They take precedence over everything this package knows.
//
// A nil entry disables the charset.
var charsets = map[string]encoding.Encoding{}

func init() {
	message.CharsetReader = Reader
}

// A Repair says how far a label was from a plain registered charset name, or
// how far the decoder chosen for it is from the charset it names. The zero
// value means neither.
type Repair uint8

const (
	// RepairLabel means the label matched only after the decoration around it
	// was removed: white space, quotes, "charset=", trailing parameters.
	RepairLabel Repair = 1 << iota
	// RepairAlias means the label is not a name or alias the IANA registry
	// has: a vendor name, a WHATWG label or a common misspelling.
	RepairAlias
	// RepairSuperset means this package decodes with a wider charset than the
	// one the label names: windows-1252 for iso-8859-1, windows-1254 for
	// iso-8859-9, the unknown-8bit decoder for us-ascii.
	RepairSuperset
	// RepairFallback means the label names no charset at all, and the
	// unknown-8bit decoder was chosen: valid UTF-8 is kept, any other byte
	// above 0x7F is read as windows-1252.
	RepairFallback
)

// A Match is what Lookup found for a charset label.
type Match struct {
	// Encoding decodes the charset. It is never nil.
	Encoding encoding.Encoding
	// Name is the lower-case name of the charset Encoding decodes, which is
	// not always the one the label names; see Repairs. It is the label itself
	// for an encoding added with RegisterEncoding.
	Name string
	// Repairs is what had to be assumed to get from the label to Encoding.
	Repairs Repair
}

// maxLabelLen bounds the labels Lookup considers. The longest registered
// charset name has 45 characters; nothing much longer is a label.
const maxLabelLen = 128

// Lookup resolves a charset label as Reader does, and reports what it resolved
// to. The error for a label it cannot resolve is what Reader returns.
func Lookup(label string) (Match, error) {
	if len(label) > maxLabelLen {
		return Match{}, fmt.Errorf("charset %q: unknown charset", label[:maxLabelLen]+"...")
	}
	key := strings.ToLower(label)
	m, err := Match{}, errUnknown
	// The registries trim white space themselves; here that is a repair.
	if key == strings.TrimSpace(key) {
		m, err = find(key)
	}
	if err == errUnknown {
		if clean := cleanLabel(key); clean != key {
			if m, err = find(clean); err == nil {
				m.Repairs |= RepairLabel
			}
		}
	}
	if err != nil {
		return Match{}, fmt.Errorf("charset %q: %v", label, err)
	}
	return m, nil
}

var (
	errUnknown     = errors.New("unknown charset")
	errUnsupported = errors.New("unsupported charset")
	errDisabled    = errors.New("charset is disabled")
)

// find resolves a lower-case label exactly as written.
func find(key string) (Match, error) {
	if enc, ok := charsets[key]; ok {
		if enc == nil {
			return Match{}, errDisabled
		}
		return Match{Encoding: enc, Name: key}, nil
	}

	// A name the IANA index knows but cannot decode comes back as (nil, nil).
	enc, err := ianaindex.MIME.Encoding(key)
	known := err == nil
	var m Match
	if !known {
		m.Repairs = RepairAlias
	}

	if a, ok := aliases[key]; ok {
		m.Encoding, m.Name, m.Repairs = a.enc, a.name, m.Repairs|a.repair
		return m, nil
	}

	// The registries, in the order this package has always asked them.
	if enc == nil {
		enc, err = ianaindex.MIME.Encoding("cs" + key)
		known = known || err == nil
	}
	if enc == nil {
		enc, err = htmlindex.Get(key)
		known = known || err == nil
	}
	// The WHATWG "replacement" encoding stands for charsets a browser refuses
	// to decode, iso-2022-cn among them: its decoder turns the whole body into
	// a single U+FFFD. An unknown charset keeps the body instead.
	if enc == nil || enc == encoding.Replacement {
		if known {
			return Match{}, errUnsupported
		}
		return Match{}, errUnknown
	}

	// What this package decodes differently from the registry, under every
	// name the registry has for it.
	m.Encoding, m.Name = enc, registryName(enc, key)
	switch enc {
	case charmap.ISO8859_1:
		m.Encoding, m.Name, m.Repairs = latin1, "windows-1252", m.Repairs|RepairSuperset
	case charmap.ISO8859_9:
		m.Encoding, m.Name, m.Repairs = latin5, "windows-1254", m.Repairs|RepairSuperset
	case registryASCII:
		m.Encoding, m.Name, m.Repairs = unknown8bit, unknown8bitName, m.Repairs|RepairSuperset
	case registryUTF16:
		m.Encoding = utf16Auto
	case japanese.ISO2022JP:
		m.Encoding = iso2022JP
	case simplifiedchinese.HZGB2312:
		m.Encoding = hzGB2312
	}
	return m, nil
}

// The registry's strict US-ASCII, which turns every byte above 0x7F into
// U+FFFD, and its utf-16, which is big-endian unless a byte order mark says
// otherwise.
var (
	registryASCII, _ = ianaindex.MIME.Encoding("us-ascii")
	registryUTF16, _ = ianaindex.MIME.Encoding("utf-16")
)

// registryName is the lower-case MIME name of an encoding a registry returned.
func registryName(enc encoding.Encoding, label string) string {
	if name, err := ianaindex.MIME.Name(enc); err == nil && name != "" {
		return strings.ToLower(name)
	}
	if name, err := htmlindex.Name(enc); err == nil && name != "" {
		return strings.ToLower(name)
	}
	return label
}

// labelJunk is what real mail wraps around a charset name.
const labelJunk = " \t\r\n\v\f\x00\xc2\xa0\"'`\\;,=<>"

// cleanLabel returns a lower-case label without the decoration around the
// charset name. Each pass removes one layer, and four passes are the limit, so
// the work is bounded whatever the label nests.
func cleanLabel(s string) string {
	for range 4 {
		before := s
		s = strings.Trim(s, labelJunk)
		if rest, ok := strings.CutPrefix(s, "charset"); ok {
			if rest = strings.TrimLeft(rest, " \t"); rest != "" && (rest[0] == '=' || rest[0] == ':') {
				s = rest[1:]
				continue
			}
		}
		// A charset name has no white space in it, and nothing after one of
		// these belongs to it: a parameter, an RFC 2231 language tag, a quote.
		if i := strings.IndexAny(s, " \t\r\n;,*'\""); i >= 0 {
			s = s[:i]
		}
		if s == before {
			break
		}
	}
	return s
}

// Reader returns an io.Reader that converts the provided charset to UTF-8.
//
// The reader decodes as it is read and never fails on its own: a byte sequence
// the charset does not allow becomes U+FFFD (see the package documentation).
// Its errors are the input's, returned unchanged after what was decoded before
// them, and io.ErrNoProgress if the input stalls (see maxEmptyReads).
func Reader(charset string, input io.Reader) (io.Reader, error) {
	m, err := Lookup(charset)
	if err != nil {
		return nil, err
	}
	return m.Encoding.NewDecoder().Reader(&stallGuard{r: input}), nil
}

// RegisterEncoding registers an encoding. This is intended to be called from
// the init function in packages that want to support additional charsets.
//
// The name is matched without regard to case, and a registered encoding takes
// precedence over the ones this package provides. A nil encoding disables the
// charset.
func RegisterEncoding(name string, enc encoding.Encoding) {
	charsets[strings.ToLower(name)] = enc
}

// maxEmptyReads is how many consecutive reads of its input a decoder accepts
// that return no bytes and no error. The golang.org/x/text reader behind every
// decoder asks again for ever, so without a limit a stalled input would hang
// the read. The limit is far above the 100 that message.Parse applies to its
// own input, which can see whether the stages below made progress: under Parse
// that rule always decides first, and this one only ends a spin nothing else
// would.
const maxEmptyReads = 1 << 16

// A stallGuard is a decoder's input under the maxEmptyReads rule. It passes
// everything else on unchanged, errors included.
type stallGuard struct {
	r     io.Reader
	empty int
}

func (g *stallGuard) Read(p []byte) (int, error) {
	n, err := g.r.Read(p)
	if n != 0 || err != nil || len(p) == 0 {
		g.empty = 0
		return n, err
	}
	if g.empty++; g.empty >= maxEmptyReads {
		return 0, io.ErrNoProgress
	}
	return 0, nil
}
