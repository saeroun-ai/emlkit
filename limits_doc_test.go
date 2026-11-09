package message

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The default of every budget is written down three times: in the table in the
// Limits doc comment, which is what a caller reads in godoc; in the roadmap's
// budget table, which is what a reviewer reads; and in the code. Two of those
// are prose, and prose drifts silently. These tests read both tables back and
// hold them to the code.
//
// They are deliberately tolerant about the shape of a table and strict about
// its content: a row is recognized by starting with a budget's name, and
// everything else about the surrounding document is ignored.

// docTableRow matches a row of the table in the Limits doc comment, which is an
// indented block inside the comment: a tab, the field name, whitespace, the
// value.
var docTableRow = regexp.MustCompile(`^//\t(Max[A-Za-z0-9]+)[ \t]+(.+)$`)

// roadmapTableRow matches a row of the roadmap's Markdown budget table, whose
// first two cells are the field name in backticks and the default.
var roadmapTableRow = regexp.MustCompile("^\\| *`(Max[A-Za-z0-9]+)` *\\| *([^|]+?) *\\|")

// roadmapCodeRow matches the same table's first and fourth cells: the field
// name and the diagnostic code reserved for that budget.
var roadmapCodeRow = regexp.MustCompile("^\\| *`(Max[A-Za-z0-9]+)` *\\|[^|]*\\|[^|]*\\| *`([a-z0-9_.]+)` *\\|")

// TestLimitsDocTable pins that the table in the Limits doc comment states the
// default of every budget, and states it correctly.
func TestLimitsDocTable(t *testing.T) {
	checkDocumentedDefaults(t, "limits.go", docTableRow)
}

// TestRoadmapBudgetTable pins the same for the roadmap's budget table, which is
// the ledger a reviewer checks this item against.
func TestRoadmapBudgetTable(t *testing.T) {
	checkDocumentedDefaults(t, "docs/robustness-roadmap.md", roadmapTableRow)
}

// TestRoadmapBudgetCodes pins the other half of the roadmap's budget table: it
// pairs every budget with the limit.* code reserved for it, and that pairing is
// what a later item emits a diagnostic against. A budget with no reserved code,
// a code that is not declared, and a code claimed by two budgets are all caught
// here, while ROB-001-001 itself emits none of them.
func TestRoadmapBudgetCodes(t *testing.T) {
	const path = "docs/robustness-roadmap.md"

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	declared := declaredCodes(t)
	byField := make(map[string]string, len(limitFields))
	byCode := make(map[string]string, len(limitFields))

	for _, line := range strings.Split(string(b), "\n") {
		m := roadmapCodeRow.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		name, code := m[1], m[2]

		if findLimitField(name) == nil {
			t.Errorf("%s reserves %s for %s, which is not a budget", path, code, name)
			continue
		}
		if _, ok := declared[code]; !ok {
			t.Errorf("%s reserves %s for %s, which the Code registry does not declare", path, code, name)
		}
		if Code(code).Group() != "limit" {
			t.Errorf("%s reserves %s for %s, want a code in the limit group", path, code, name)
		}
		if previous, ok := byCode[code]; ok {
			t.Errorf("%s reserves %s for both %s and %s", path, code, previous, name)
		}
		byField[name], byCode[code] = code, name
	}

	for _, f := range limitFields {
		if byField[f.name] == "" {
			t.Errorf("%s reserves no diagnostic code for %s", path, f.name)
		}
	}

	// The registry must not carry a limit.* code that no budget claims: a code
	// nothing can ever be charged against would have no way to be emitted.
	for code := range declared {
		if Code(code).Group() == "limit" && byCode[code] == "" {
			t.Errorf("%s is declared but no budget in %s reserves it", code, path)
		}
	}
}

// checkDocumentedDefaults reads every row row matches out of path and compares
// it with the resolved defaults, insisting that all eighteen budgets appear
// exactly once.
func checkDocumentedDefaults(t *testing.T, path string, row *regexp.Regexp) {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	defaults := DefaultLimits()
	documented := make(map[string]bool)

	for _, line := range strings.Split(string(b), "\n") {
		m := row.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		name, text := m[1], m[2]

		field := findLimitField(name)
		if field == nil {
			t.Errorf("%s documents %s, which is not a budget", path, name)
			continue
		}
		if documented[name] {
			t.Errorf("%s documents %s twice", path, name)
		}
		documented[name] = true

		want, err := parseDocumentedSize(text)
		if err != nil {
			t.Errorf("%s: %s = %q: %v", path, name, text, err)
			continue
		}
		if got := *field.get(&defaults); got != want {
			t.Errorf("%s documents %s as %q (%d), want %d", path, name, text, want, got)
		}
	}

	for _, f := range limitFields {
		if !documented[f.name] {
			t.Errorf("%s does not document %s", path, f.name)
		}
	}
}

// findLimitField returns the budget called name, or nil.
func findLimitField(name string) *limitField {
	for i := range limitFields {
		if limitFields[i].name == name {
			return &limitFields[i]
		}
	}
	return nil
}

// parseDocumentedSize reads a value as the tables write it: a decimal number,
// optionally grouped with spaces, and optionally followed by a binary unit.
func parseDocumentedSize(text string) (int64, error) {
	s := strings.Join(strings.Fields(text), "")

	var scale int64 = 1
	for _, unit := range []struct {
		suffix string
		scale  int64
	}{
		{"KiB", 1 << 10},
		{"MiB", 1 << 20},
		{"GiB", 1 << 30},
	} {
		if rest, ok := strings.CutSuffix(s, unit.suffix); ok {
			s, scale = rest, unit.scale
			break
		}
	}

	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return n * scale, nil
}
