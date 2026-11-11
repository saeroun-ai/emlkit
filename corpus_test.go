package message

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"
)

// The regression corpus (ROB-130-003, issue #11): testdata/corpus holds
// synthetic .eml fixtures and manifest.json what the parse is expected to make
// of each — the completion state, the diagnostic codes in order, every leaf
// with its path, media type and decoded size, the decoded Subject and From
// with their size and repairs, the start of every text leaf and the first
// filename of every leaf that has one. Each fixture is in one of cpCategories. The sender-shaped fixtures are
// built by scripts/gen-corpus-fixtures.py; docs/robustness-notes/corpus.md says
// how to add one.
//
// Run with EMLKIT_UPDATE_CORPUS=1 to rewrite the expectations from the
// current behavior, and review the diff: the manifest is the record of what
// changed.
//
// This test runs without a CharsetReader — the root package cannot import the
// charset plugin — so a body or encoded word in any charset but UTF-8 and
// US-ASCII is recorded undecoded. charset/corpus_test.go reads the same
// fixtures with the plugin.

type corpusFixture struct {
	Name        string `json:"name"`
	Category    string `json:"category"`
	Description string `json:"description"`
	cpOutcome

	// ByteAtATime is the outcome of reading the fixture one byte at a time,
	// recorded only where it differs from reading it whole.
	ByteAtATime *cpOutcome `json:"byteAtATime,omitempty"`
}

type corpusManifest struct {
	Provenance string          `json:"provenance"`
	Fixtures   []corpusFixture `json:"fixtures"`
}

// cpCategories are the categories of docs/robustness-notes/corpus.md.
var cpCategories = []string{"conformant", "malformed", "truncated", "adversarial", "provider-style"}

// cpOutcome is what one parse of a fixture made of it. charset/corpus_test.go
// has a copy of it and of corpusOutcome; keep the two in step.
type cpOutcome struct {
	State       string   `json:"state,omitempty"`
	Diagnostics []string `json:"diagnostics,omitempty"`
	Leaves      []string `json:"leaves,omitempty"`
	Headers     []string `json:"headers,omitempty"`
	Text        []string `json:"text,omitempty"`
	Names       []string `json:"names,omitempty"`
}

// cpSnippetRunes is how much of a decoded text is recorded.
const cpSnippetRunes = 60

// cpSnippet is the start of a decoded text, quoted: white space runs become
// one space, at most cpSnippetRunes characters are kept (an invalid byte
// counts as one, and is shown escaped), and "…" follows a cut.
func cpSnippet(b []byte) string {
	var out []byte
	n, space := 0, false
	for len(b) > 0 && n < cpSnippetRunes {
		r, size := utf8.DecodeRune(b)
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			space = len(out) > 0
			b = b[size:]
			continue
		}
		if space {
			out = append(out, ' ')
			n++
			space = false
		}
		out = append(out, b[:size]...)
		n++
		b = b[size:]
	}
	s := strconv.QuoteToGraphic(string(out))
	if len(bytes.TrimSpace(b)) > 0 {
		s += "…"
	}
	return s
}

// cpPrefix keeps the first bytes written to it and counts them all.
type cpPrefix struct {
	b []byte
	n int64
}

func (p *cpPrefix) Write(b []byte) (int, error) {
	if room := 4096 - len(p.b); room > 0 {
		p.b = append(p.b, b[:min(room, len(b))]...)
	}
	p.n += int64(len(b))
	return len(b), nil
}

func corpusOutcome(t *testing.T, src io.Reader) (o cpOutcome) {
	t.Helper()
	s, e, err := Parse(src, &ParseOptions{WalkEmbeddedMessages: true})
	if e == nil {
		o.State = "no entity: " + fmt.Sprint(err)
		return o
	}
	for _, k := range []string{"Subject", "From"} {
		if !e.Header.Has(k) {
			continue
		}
		v, diags := e.Header.TextDiagnostics(k)
		h := fmt.Sprintf("%s %d %s", k, len(v), cpSnippet([]byte(v)))
		for _, d := range diags {
			h += fmt.Sprintf(" (%s %s)", d.Severity, d.Code)
		}
		o.Headers = append(o.Headers, h)
	}
	werr := e.Walk(func(path []int, p *Entity, _ error) error {
		if p.MultipartReader() != nil || p.CarriesMessage() {
			return nil
		}
		mt, _, _ := p.EffectiveContentType()
		var body cpPrefix
		_, _ = io.Copy(&body, p.Body)
		o.Leaves = append(o.Leaves, fmt.Sprintf("%v %s %d", path, mt, body.n))
		if strings.HasPrefix(mt, "text/") || strings.HasPrefix(mt, "message/") {
			o.Text = append(o.Text, fmt.Sprintf("%v %s", path, cpSnippet(body.b)))
		}
		if names := p.FilenameCandidates(); len(names) > 0 {
			n := fmt.Sprintf("%v %s", path, strconv.QuoteToGraphic(names[0].Filename))
			if names[0].Parameter.Undecoded {
				n += " undecoded"
			}
			o.Names = append(o.Names, n)
		}
		return nil
	})
	if werr != nil {
		o.Leaves = append(o.Leaves, "walk: "+werr.Error())
	}
	for _, d := range s.Diagnostics() {
		o.Diagnostics = append(o.Diagnostics, fmt.Sprintf("%s %s %v", d.Severity, d.Code, d.Path))
	}
	o.State = s.Completion().State.String()
	return o
}

func TestCorpus(t *testing.T) {
	dir := filepath.Join("testdata", "corpus")
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m corpusManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	update := os.Getenv("EMLKIT_UPDATE_CORPUS") != ""
	files, _ := filepath.Glob(filepath.Join(dir, "*.eml"))
	if len(files) != len(m.Fixtures) {
		t.Errorf("%d .eml files and %d manifest entries", len(files), len(m.Fixtures))
	}
	for i := range m.Fixtures {
		f := &m.Fixtures[i]
		if !slices.Contains(cpCategories, f.Category) {
			t.Errorf("%s: category %q is not one of %q", f.Name, f.Category, cpCategories)
		}
		data, err := os.ReadFile(filepath.Join(dir, f.Name+".eml"))
		if err != nil {
			t.Errorf("%s: %v", f.Name, err)
			continue
		}
		got := corpusOutcome(t, bytes.NewReader(data))
		// However the input is delivered, the outcome should be the same. Where
		// it is not, the byte-at-a-time outcome is recorded beside the other,
		// and is a finding of docs/robustness-notes/corpus.md.
		var chunked *cpOutcome
		if got2 := corpusOutcome(t, iotest.OneByteReader(bytes.NewReader(data))); !reflect.DeepEqual(got, got2) {
			chunked = &got2
		}
		if update {
			f.cpOutcome, f.ByteAtATime = got, chunked
			continue
		}
		if !reflect.DeepEqual(got, f.cpOutcome) {
			t.Errorf("%s (%s):\n got  %s\n want %s", f.Name, f.Description, cpJSON(got), cpJSON(f.cpOutcome))
		}
		if !reflect.DeepEqual(chunked, f.ByteAtATime) {
			t.Errorf("%s: read a byte at a time:\n got  %s\n want %s\n(null: the same as read whole)",
				f.Name, cpJSON(chunked), cpJSON(f.ByteAtATime))
		}
	}
	if update {
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), cpJSON(m), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// cpJSON is v as the manifest writes it: indented, with <, > and & as they
// are, since fixtures quote HTML.
func cpJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
