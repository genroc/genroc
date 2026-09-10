package lsp

import (
	"strings"
	"testing"
)

// decoded is one token as a reader would see it: the text it covers and the legend name.
type decoded struct {
	text string
	kind string
	line int
}

// decodeTokens reverses the wire encoding, and fails the test on any range it cannot place: a
// delta-encoded stream that drifts paints the WRONG text rather than none, which is the failure
// nobody notices.
func decodeTokens(t *testing.T, text string) []decoded {
	t.Helper()
	lines := splitLines(text)
	data := semanticTokens(text)
	if len(data)%5 != 0 {
		t.Fatalf("data has %d ints, which is not a whole number of tokens", len(data))
	}
	var out []decoded
	line, start := 0, 0
	for i := 0; i < len(data); i += 5 {
		dl, ds, length, kind := int(data[i]), int(data[i+1]), int(data[i+2]), int(data[i+3])
		line += dl
		if dl == 0 {
			start += ds
		} else {
			start = ds
		}
		if line >= len(lines) {
			t.Fatalf("token on line %d, past the end of a %d-line document", line, len(lines))
		}
		src := lines[line]
		from, to := byteColumn(lines, position{Line: line, Character: start})-1, byteColumn(lines, position{Line: line, Character: start + length})-1
		if from < 0 || to > len(src) || from > to {
			t.Fatalf("token range [%d,%d) is outside %q", from, to, src)
		}
		out = append(out, decoded{text: src[from:to], kind: semanticTokenTypes[kind], line: line})
	}
	return out
}

func kindOfText(toks []decoded, text string) string {
	for _, tk := range toks {
		if tk.text == text {
			return tk.kind
		}
	}
	return ""
}

const tickerDef = `name: ticker
tasks:
  - id: tick
    action:
      type: delay
      for: 10s
    output:
      count: "$: (self.previous.count ?? 0) + 1"
    switch:
      - case: "self.output.count < 3"
        goto: $tick
      - goto: end
output:
  ticks: "$: outputs.tick.count"
`

// The whole point of moving this off the grammar: `id` is a plain string, so a `$:` written
// there is the literal text `$: tick` and must not be painted as an expression. A grammar
// cannot know that — it sees the same characters in both slots. Reported from the editor.
func TestALiteralSlotIsNotAnExpression(t *testing.T) {
	def := strings.Replace(tickerDef, `- id: tick`, `- id: "$: tick"`, 1)
	idLine := 0
	for n, l := range splitLines(def) {
		if strings.Contains(l, "id:") {
			idLine = n
		}
	}
	for _, tk := range decodeTokens(t, def) {
		if tk.line == idLine {
			t.Errorf("marked %q on the id line as %s; id holds text, not a template", tk.text, tk.kind)
		}
	}
	// The same marker one slot over IS an expression, or the test would pass by marking nothing.
	if got := kindOfText(decodeTokens(t, def), "$:"); got != "keyword" {
		t.Errorf(`no "$:" was marked anywhere, so this test proves nothing`)
	}
}

func TestATypedLeafIsMarkedAndLexed(t *testing.T) {
	toks := decodeTokens(t, tickerDef)
	if got := kindOfText(toks, "$:"); got != "keyword" {
		t.Errorf(`the "$:" marker is %q, want keyword — nothing else says the scalar computes`, got)
	}
	for _, want := range []struct{ text, kind string }{
		{"0", "number"},
		{"??", "operator"},
		{"3", "number"},
	} {
		if got := kindOfText(toks, want.text); got != want.kind {
			t.Errorf("%q is %q, want %q", want.text, got, want.kind)
		}
	}
}

// One referent, one type. `self` scoped apart from the names after it paints a path in two
// colours in any editor that follows the convention, which is what was reported twice.
func TestAMemberPathIsOneKindFromRootToLeaf(t *testing.T) {
	toks := decodeTokens(t, tickerDef)
	kinds := map[string]bool{}
	for _, seg := range []string{"self", "previous", "count"} {
		k := kindOfText(toks, seg)
		if k == "" {
			t.Fatalf("%q was not marked at all", seg)
		}
		kinds[k] = true
	}
	if len(kinds) != 1 {
		t.Errorf("self.previous.count is marked with %d kinds, want 1", len(kinds))
	}
}

// A `case` is written bare, so nothing in the text says it is an expression — only the slot.
func TestABareCaseIsLexedAsAnExpression(t *testing.T) {
	toks := decodeTokens(t, tickerDef)
	if got := kindOfText(toks, "<"); got != "operator" {
		t.Errorf("the case condition was not lexed: `<` is %q", got)
	}
}

// `$task`, `end` and `next` fill one slot and mean one thing.
func TestRoutingTargetsShareOneKind(t *testing.T) {
	toks := decodeTokens(t, tickerDef)
	tick, end := kindOfText(toks, "$tick"), kindOfText(toks, "end")
	if tick == "" || end == "" {
		t.Fatalf("routing targets not marked: $tick=%q end=%q", tick, end)
	}
	if tick != end {
		t.Errorf("$tick is %q and end is %q; one slot must not render in two colours", tick, end)
	}
}

// A document being typed in is the normal input. Producing a token past the end of a line, or
// an unsortable delta, corrupts every token after it — the editor paints the wrong ranges
// rather than showing nothing, which is far harder to notice.
func TestPartialDocumentsProduceSaneRanges(t *testing.T) {
	for _, def := range []string{
		strings.Replace(tickerDef, `"$: (self.previous.count ?? 0) + 1"`, `"$: (self.previous.`, 1),
		strings.Replace(tickerDef, `"$: (self.previous.count ?? 0) + 1"`, `"${ input.`, 1),
		strings.Replace(tickerDef, `goto: $tick`, `goto: $`, 1),
		"name: ticker\ntasks:\n  - id: tick\n    output:\n      x: \"$: \"\n",
		"",
	} {
		decodeTokens(t, def) // decodeTokens fails the test on any range it cannot place
	}
}
