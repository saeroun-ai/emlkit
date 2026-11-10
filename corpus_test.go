package message

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/iotest"
)

// The regression corpus (ROB-130-003, issue #11): testdata/corpus holds one
// .eml per defect and manifest.json what the parse is expected to make of it —
// the completion state, the diagnostic codes in order, and every leaf with
// its path, media type and decoded size. Every fixture is synthetic.
//
// Run with EMLKIT_UPDATE_CORPUS=1 to rewrite the expectations from the
// current behavior, and review the diff: the manifest is the record of what
// changed.

type corpusFixture struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	State       string   `json:"state,omitempty"`
	Diagnostics []string `json:"diagnostics,omitempty"`
	Leaves      []string `json:"leaves,omitempty"`
}

type corpusManifest struct {
	Provenance string          `json:"provenance"`
	Fixtures   []corpusFixture `json:"fixtures"`
}

func corpusOutcome(t *testing.T, src io.Reader) (state string, diags, leaves []string) {
	t.Helper()
	s, e, err := Parse(src, &ParseOptions{WalkEmbeddedMessages: true})
	if e == nil {
		return "no entity: " + fmt.Sprint(err), nil, nil
	}
	werr := e.Walk(func(path []int, p *Entity, _ error) error {
		if p.MultipartReader() != nil || p.walkEmbedded() != nil {
			return nil
		}
		mt, _, _ := p.EffectiveContentType()
		n, _ := io.Copy(io.Discard, p.Body)
		leaves = append(leaves, fmt.Sprintf("%v %s %d", path, mt, n))
		return nil
	})
	if werr != nil {
		leaves = append(leaves, "walk: "+werr.Error())
	}
	for _, d := range s.Diagnostics() {
		diags = append(diags, fmt.Sprintf("%s %s %v", d.Severity, d.Code, d.Path))
	}
	return s.Completion().State.String(), diags, leaves
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
		data, err := os.ReadFile(filepath.Join(dir, f.Name+".eml"))
		if err != nil {
			t.Errorf("%s: %v", f.Name, err)
			continue
		}
		state, diags, leaves := corpusOutcome(t, bytes.NewReader(data))
		// However the input is delivered, the outcome is the same.
		s2, d2, l2 := corpusOutcome(t, iotest.OneByteReader(bytes.NewReader(data)))
		if state != s2 || !reflect.DeepEqual(diags, d2) || !reflect.DeepEqual(leaves, l2) {
			t.Errorf("%s: reading a byte at a time changed the outcome", f.Name)
		}
		if update {
			f.State, f.Diagnostics, f.Leaves = state, diags, leaves
			continue
		}
		if state != f.State || !reflect.DeepEqual(diags, f.Diagnostics) || !reflect.DeepEqual(leaves, f.Leaves) {
			t.Errorf("%s (%s):\n state %q, want %q\n diagnostics %q\n        want %q\n leaves %q\n   want %q",
				f.Name, f.Description, state, f.State, diags, f.Diagnostics, leaves, f.Leaves)
		}
	}
	if update {
		out, _ := json.MarshalIndent(m, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
