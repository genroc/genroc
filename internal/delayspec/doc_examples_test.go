package delayspec

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// specs/delay-syntax.md carries the syntax reference — every accepted spelling in one place,
// and a table of the ones that are turned away. A reference nobody executes is a reference
// that rots, so this reads the file and runs both halves through the parser.
//
// It is deliberately literal-minded: it extracts what the document *shows the reader*, so a
// spelling that stops working, or one the document forgot to update, fails here rather than
// in someone's definition.
const syntaxDoc = "../../specs/delay-syntax.md"

// The heading that splits the file: everything above shows syntax that must parse,
// the table below it shows syntax that must not.
const rejectedHeading = "## What is rejected"

// A slot assignment as the document writes it: for: "2h30m", until: 1789000000000, tz: "UTC".
var slotPattern = regexp.MustCompile(`\b(for|until|tz): (?:"([^"]*)"|(\d+))`)

func TestDocExamples_AcceptedAndRejectedSpellings(t *testing.T) {
	body, err := os.ReadFile(syntaxDoc)
	if err != nil {
		t.Fatalf("read %s: %v", syntaxDoc, err)
	}
	accepted, rejected, ok := strings.Cut(string(body), rejectedHeading)
	if !ok {
		t.Fatalf("%s has no %q section; this test keys on it to tell the two halves apart", syntaxDoc, rejectedHeading)
	}
	// The rejected table ends at the next heading.
	if end := strings.Index(rejected, "\n## "); end >= 0 {
		rejected = rejected[:end]
	}

	for _, ex := range slotExamples(accepted) {
		if err := parseSlot(ex, true); err != nil {
			t.Errorf("%s shows %s: %s as valid, but it does not parse: %v", syntaxDoc, ex.slot, ex.value, err)
		}
	}
	for _, ex := range slotExamples(rejected) {
		if err := parseSlot(ex, false); err == nil {
			t.Errorf("%s lists %s: %s as rejected, but it parses", syntaxDoc, ex.slot, ex.value)
		}
	}

	// The clock-field table writes its examples bare, without a slot name — pick them out of
	// the first column. Rows describing a field's *shape* rather than a whole clock ("08",
	// "*", "base/step") have no colon, which is what separates them.
	for _, clock := range firstColumnCells(sectionOf(accepted, "#### Clock fields")) {
		if !strings.Contains(clock, ":") {
			continue
		}
		if _, err := ParseInstant(clock); err != nil {
			t.Errorf("%s shows the clock pattern %q, which does not parse: %v", syntaxDoc, clock, err)
		}
	}

	// A reference with no examples in it would pass everything above in silence.
	if n := len(slotExamples(accepted)); n < 25 {
		t.Errorf("only %d accepted examples found in %s; the extraction has probably drifted from the document", n, syntaxDoc)
	}
}

// bare records that the document wrote the value without quotes — the JSON-number form,
// which is milliseconds by definition and has no grammar to check. A *quoted* "5000" is a
// different thing entirely, and one the reference lists as rejected.
type slotExample struct {
	slot, value string
	bare        bool
}

func slotExamples(section string) []slotExample {
	var out []slotExample
	for _, m := range slotPattern.FindAllStringSubmatch(section, -1) {
		value, bare := m[2], m[3] != ""
		if bare {
			value = m[3]
		}
		out = append(out, slotExample{slot: m[1], value: value, bare: bare})
	}
	return out
}

// parseSlot routes an example to the grammar its slot uses.
//
// skipExpressions is set for the accepted half only. The expression forms belong to the
// validator rather than to this package, so an accepted "$:" example has nothing to check
// here — while the rejected table's "${ }" row is a claim about the grammar, and is checked.
func parseSlot(ex slotExample, skipExpressions bool) error {
	if ex.bare {
		return nil // a JSON number is milliseconds by definition; no grammar involved
	}
	if skipExpressions && (strings.HasPrefix(strings.TrimSpace(ex.value), "$:") || strings.Contains(ex.value, "${")) {
		return nil
	}
	switch ex.slot {
	case "for":
		_, err := ParseDuration(ex.value)
		return err
	case "until":
		_, err := ParseInstant(ex.value)
		return err
	default:
		_, err := LoadLocation(ex.value)
		return err
	}
}

// sectionOf returns the text under a heading, up to the next heading of any level.
func sectionOf(body, heading string) string {
	_, after, ok := strings.Cut(body, heading)
	if !ok {
		return ""
	}
	if end := strings.Index(after, "\n#"); end >= 0 {
		return after[:end]
	}
	return after
}

// firstColumnCells returns the first cell of every markdown table row in a section, with
// the backticks stripped. Separator rows (|---|) fall out on their own.
func firstColumnCells(section string) []string {
	var out []string
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cell := strings.TrimSpace(strings.Split(strings.Trim(line, "|"), "|")[0])
		if cell = strings.Trim(cell, "`"); cell != "" && !strings.HasPrefix(cell, "-") {
			out = append(out, cell)
		}
	}
	return out
}
