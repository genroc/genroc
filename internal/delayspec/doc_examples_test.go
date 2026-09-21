package delayspec

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// specs/delay-syntax.md carries the syntax reference, and a reference nobody executes rots:
// this runs both halves (accepted spellings, rejected table) through the parser, so a
// spelling that stops working — or one the doc forgot — fails here, not in a definition.
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

// parseSlot routes an example to its slot's grammar. skipExpressions is set for the accepted
// half: "$:" forms belong to the validator, while the rejected table's "${ }" row IS a claim
// about this grammar and is checked.
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

// The reference page teaches the same grammar to a wider audience, and the sweep above stops
// at specs/ — so nothing executed it. Every literal it shows as valid is parsed here.
const referencePage = "../../docs/src/content/docs/reference/definition/delay-syntax.mdx"

// The sentence that splits the tz section into the zones it accepts and the ones it names as
// refused. Both halves are claims about LoadLocation, so both are checked.
const tzRefusedSentence = "\nAbbreviations such as"

// A backticked span in the `for` section that opens with a digit is a duration the page is
// showing. Matching on the leading digit rather than on the grammar is deliberate: a shape
// test would skip exactly the malformed example this is here to catch.
var durationLike = regexp.MustCompile(`^\d`)

func TestDocExamples_ReferencePageParses(t *testing.T) {
	body, err := os.ReadFile(referencePage)
	if err != nil {
		t.Fatalf("read %s: %v", referencePage, err)
	}
	page := string(body)

	forSection := sectionOf(page, "## `for` — a duration")
	// Backtick-aware, so the table's "Unit" header falls out with the separator row.
	units := codeSpansInColumn(forSection, 0)
	for _, u := range units {
		if _, err := ParseDuration("1" + u); err != nil {
			t.Errorf("%s lists %q as a unit, but 1%s does not parse: %v", referencePage, u, u, err)
		}
	}
	durations := codeSpans(forSection, durationLike.MatchString)
	for _, d := range durations {
		if _, err := ParseDuration(d); err != nil {
			t.Errorf("%s shows the duration %q, which does not parse: %v", referencePage, d, err)
		}
	}

	instants := codeSpansInColumn(sectionOf(page, "## `until` — an instant"), 1)
	for _, s := range instants {
		if _, err := ParseInstant(s); err != nil {
			t.Errorf("%s shows the instant %q, which does not parse: %v", referencePage, s, err)
		}
	}

	tzAccepted, tzRefused, ok := strings.Cut(sectionOf(page, "## `tz`"), tzRefusedSentence)
	if !ok {
		t.Fatalf("%s: the tz section no longer contains %q; this test keys on it to tell accepted from refused", referencePage, strings.TrimSpace(tzRefusedSentence))
	}
	zones := codeSpans(tzAccepted, func(s string) bool { return s != "tz" })
	for _, z := range zones {
		if _, err := LoadLocation(z); err != nil {
			t.Errorf("%s offers %q as a tz, but it does not load: %v", referencePage, z, err)
		}
	}
	for _, z := range codeSpans(tzRefused, func(string) bool { return true }) {
		if _, err := LoadLocation(z); err == nil {
			t.Errorf("%s says %q is refused, but it loads", referencePage, z)
		}
	}

	// Extraction that silently matches nothing would pass every loop above.
	for _, c := range []struct {
		what string
		n    int
		min  int
	}{{"units", len(units), 8}, {"durations", len(durations), 3}, {"instants", len(instants), 5}, {"zones", len(zones), 3}} {
		if c.n < c.min {
			t.Errorf("only %d %s found in %s; the extraction has drifted from the page", c.n, c.what, referencePage)
		}
	}
}

// codeSpans returns every `code` span in s that keep accepts.
func codeSpans(s string, keep func(string) bool) []string {
	var out []string
	for _, m := range codeSpan.FindAllStringSubmatch(s, -1) {
		if keep(m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

var codeSpan = regexp.MustCompile("`([^`]+)`")

// codeSpansInColumn returns the `code` spans of column idx of every markdown table row in a
// section — one cell may hold several, which is how the page lists the pattern forms.
func codeSpansInColumn(section string, idx int) []string {
	var out []string
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if idx >= len(cells) {
			continue
		}
		out = append(out, codeSpans(cells[idx], func(string) bool { return true })...)
	}
	return out
}
