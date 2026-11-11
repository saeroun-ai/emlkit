package charset_test

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

	"github.com/saeroun-ai/emlkit"
	_ "github.com/saeroun-ai/emlkit/charset"
)

// TestCorpusWithPlugin reads the regression corpus (testdata/corpus at the
// repository root, ROB-130-003) as a deployment that reads Korean mail would:
// with this package's CharsetReader installed, ParseOptions.FallbackCharset
// "euc-kr" for bodies and HeaderTextOptions.FallbackCharset "euc-kr" for
// Subject and From. The root TestCorpus cannot import this package, so it
// records every legacy charset undecoded; this is where their decoding is
// pinned.
//
// A fixture this configuration reads exactly as manifest.json records it is
// not repeated: plugin.json holds the outcome of every fixture that comes out
// differently, and only of those. EMLKIT_UPDATE_CORPUS=1 rewrites plugin.json
// from the current behavior; update manifest.json first (go test -run
// TestCorpus . with the same switch), since what differs is judged against it.
func TestCorpusWithPlugin(t *testing.T) {
	dir := filepath.Join("..", "testdata", "corpus")
	var m struct {
		Fixtures []cpFixture `json:"fixtures"`
	}
	cpReadJSON(t, filepath.Join(dir, "manifest.json"), &m)
	var p cpPluginFile
	cpReadJSON(t, filepath.Join(dir, "plugin.json"), &p)
	recorded := map[string]cpFixture{}
	for _, f := range p.Fixtures {
		recorded[f.Name] = f
	}
	update := os.Getenv("EMLKIT_UPDATE_CORPUS") != ""
	next := cpPluginFile{Configuration: cpConfiguration, Fixtures: []cpFixture{}}
	for _, f := range m.Fixtures {
		data, err := os.ReadFile(filepath.Join(dir, f.Name+".eml"))
		if err != nil {
			t.Errorf("%s: %v", f.Name, err)
			continue
		}
		got := cpFixture{Name: f.Name, cpOutcome: cpCorpusOutcome(bytes.NewReader(data))}
		// As in the root test, a byte-at-a-time outcome is recorded only where
		// it differs from reading the fixture whole.
		if got2 := cpCorpusOutcome(iotest.OneByteReader(bytes.NewReader(data))); !reflect.DeepEqual(got.cpOutcome, got2) {
			got.ByteAtATime = &got2
		}
		if !reflect.DeepEqual(got, f) {
			next.Fixtures = append(next.Fixtures, got)
		}
		if update {
			continue
		}
		want, ok := recorded[f.Name]
		if !ok {
			want = f
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s with the plugin:\n got  %s\n want %s", f.Name, cpJSON(got), cpJSON(want))
		}
	}
	if update {
		if err := os.WriteFile(filepath.Join(dir, "plugin.json"), cpJSON(next), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	for name := range recorded {
		if !slices.ContainsFunc(m.Fixtures, func(f cpFixture) bool { return f.Name == name }) {
			t.Errorf("plugin.json names %s, which manifest.json does not list", name)
		}
	}
}

const cpConfiguration = "github.com/saeroun-ai/emlkit/charset imported; ParseOptions{WalkEmbeddedMessages: true, " +
	"FallbackCharset: \"euc-kr\"}; HeaderTextOptions{FallbackCharset: \"euc-kr\"}. " +
	"Only fixtures whose outcome differs from manifest.json are listed."

type cpPluginFile struct {
	Configuration string      `json:"configuration"`
	Fixtures      []cpFixture `json:"fixtures"`
}

type cpFixture struct {
	Name string `json:"name"`
	cpOutcome
	ByteAtATime *cpOutcome `json:"byteAtATime,omitempty"`
}

func cpReadJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// The rest is a copy of the outcome in the root corpus_test.go, with this
// test's options; keep the two in step.

type cpOutcome struct {
	State       string   `json:"state,omitempty"`
	Diagnostics []string `json:"diagnostics,omitempty"`
	Leaves      []string `json:"leaves,omitempty"`
	Headers     []string `json:"headers,omitempty"`
	Text        []string `json:"text,omitempty"`
	Names       []string `json:"names,omitempty"`
}

const cpSnippetRunes = 60

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

func cpCorpusOutcome(src io.Reader) (o cpOutcome) {
	s, e, err := message.Parse(src, &message.ParseOptions{WalkEmbeddedMessages: true, FallbackCharset: "euc-kr"})
	if e == nil {
		o.State = "no entity: " + fmt.Sprint(err)
		return o
	}
	for _, k := range []string{"Subject", "From"} {
		if !e.Header.Has(k) {
			continue
		}
		v, diags := e.Header.TextWithOptions(k, &message.HeaderTextOptions{FallbackCharset: "euc-kr"})
		h := fmt.Sprintf("%s %d %s", k, len(v), cpSnippet([]byte(v)))
		for _, d := range diags {
			h += fmt.Sprintf(" (%s %s)", d.Severity, d.Code)
		}
		o.Headers = append(o.Headers, h)
	}
	werr := e.Walk(func(path []int, p *message.Entity, _ error) error {
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
