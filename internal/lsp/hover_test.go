package lsp

import (
	"strings"
	"testing"
)

// A definition with one of everything hover has to answer for. Line numbers are asserted
// against it, so a change here moves the cases below.
//
//	 1 name: demo
//	 2 input_schema:
//	 3   type: object
//	 4   properties: { amount: { type: number } }
//	 5   required: [amount]
//	 6 tasks:
//	 7   - id: price
//	 8     action:
//	 9       type: fetch
//	10       url: "https://x/p?a=${ input.amount }"
//	11       responses:
//	12         "200": { type: object, properties: { total: { type: number } }, required: [total] }
//	13     output:
//	14       grand: "$: self.result.total * 2"
//	15     switch: end
const hoverDoc = `name: demo
input_schema:
  type: object
  properties: { amount: { type: number } }
  required: [amount]
tasks:
  - id: price
    action:
      type: fetch
      url: "https://x/p?a=${ input.amount }"
      responses:
        "200": { type: object, properties: { total: { type: number } }, required: [total] }
    output:
      grand: "$: self.result.total * 2"
    switch: end
`

func hoverOf(t *testing.T, text string, line, col int) string {
	t.Helper()
	md, _, ok := hoverAt(text, line, col)
	if !ok {
		t.Fatalf("nothing to say at %d:%d", line, col)
	}
	return md
}

// Line 14 is `      grand: "$: self.result.total * 2"`, so column 30 is inside `total`, 19 is
// inside `self`, and 36 is the `*` — where there is no symbol and the leaf answers.

// The reason to build hover: the type an author is otherwise guessing at.
func TestHoverOverASymbolTypesThatSymbol(t *testing.T) {
	if md := hoverOf(t, hoverDoc, 14, 32); md != "`self.result.total` → **number**" {
		t.Errorf("got: %s", md)
	}
}

// Truncated AT the segment, so walking a path shows each level rather than always the leaf.
func TestHoverOverAnIntermediateSegmentTypesThePathUpToIt(t *testing.T) {
	if md := hoverOf(t, hoverDoc, 14, 19); !strings.HasPrefix(md, "`self` → **object{") {
		t.Errorf("got: %s", md)
	}
}

// No symbol under the cursor: the expression it sits in is the answer.
func TestHoverOnAnOperatorTypesTheWholeExpression(t *testing.T) {
	if md := hoverOf(t, hoverDoc, 14, 36); md != "`self.result.total * 2` → **number**" {
		t.Errorf("got: %s", md)
	}
}

// A `${ }` inside a longer string types as the string it renders into, so the leaf says
// nothing — but the interpolation being written has a type of its own, and a URL is where most
// expressions in a definition live.
func TestHoverInsideAnInterpolationTypesThatInterpolation(t *testing.T) {
	if md := hoverOf(t, hoverDoc, 10, 38); md != "`input.amount` → **number**" {
		t.Errorf("got: %s", md)
	}
}

func TestHoverOnASlotNamesItsType(t *testing.T) {
	md := hoverOf(t, hoverDoc, 13, 6)
	if !strings.Contains(md, "tasks.price.output") || !strings.Contains(md, "object{grand}") {
		t.Errorf("want the slot address and its type, got: %s", md)
	}
}

// One line. A hover is read at a glance, and the scope a slot carries is a different question
// — `genctl schema context` is where that one is asked.
func TestHoverIsOneLine(t *testing.T) {
	for _, at := range [][2]int{{14, 32}, {14, 36}, {10, 38}, {13, 6}} {
		md := hoverOf(t, hoverDoc, at[0], at[1])
		if strings.Contains(md, "\n") {
			t.Errorf("hover at %d:%d carries more than one line:\n%s", at[0], at[1], md)
		}
	}
}

// Hover decodes leniently: a reader asking about one slot is not asking about a typo in
// another, and refusing until the file is clean is what §7b was written against.
func TestHoverStillAnswersWhenAnotherPartOfTheFileIsBroken(t *testing.T) {
	broken := strings.Replace(hoverDoc, "    switch: end\n", "    switch: end\n    on_eror: []\n", 1)
	if md := hoverOf(t, broken, 14, 32); !strings.Contains(md, "number") {
		t.Errorf("a typo elsewhere must not silence hover: %s", md)
	}
}

// An expression that does not type is exactly when someone hovers it, so the reason has to be
// the answer rather than an empty popup.
func TestHoverOverABrokenExpressionSaysWhy(t *testing.T) {
	broken := strings.Replace(hoverDoc, "self.result.total * 2", "self.result.nope * 2", 1)
	if md := hoverOf(t, broken, 14, 36); !strings.Contains(md, "nope") {
		t.Errorf("want the inference failure, got: %s", md)
	}
}

// A name the AUTHOR chose — a property in their own schema — is the one thing here that means
// nothing to the definition language, so it is the one thing with no answer.
func TestHoverOverAnAuthorsOwnNameIsSilent(t *testing.T) {
	//	 4   properties: { amount: { type: number } }
	if md, _, ok := hoverAt(hoverDoc, 4, 18); ok {
		t.Errorf("`amount` is the author's own name; got a popup: %s", md)
	}
}

func TestHoverAdvertisedAndAnswered(t *testing.T) {
	msgs, _ := session(t,
		frame("initialize", 1, map[string]any{}),
		openDoc(uri, hoverDoc),
		frame("textDocument/hover", 2, map[string]any{
			"textDocument": map[string]any{"uri": uri},
			// 0-based: line 14, the `grand:` expression.
			// 0-based: line 14, inside `total`.
			"position": map[string]any{"line": 13, "character": 31},
		}),
		frame("exit", nil, nil))

	var res initializeResult
	if err := jsonUnmarshal(msgs[0]["result"], &res); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if !res.Capabilities.HoverProvider {
		t.Error("a server that answers hover must say so, or no editor will ask")
	}

	var hov hoverResult
	if err := jsonUnmarshal(msgs[len(msgs)-1]["result"], &hov); err != nil {
		t.Fatalf("hover result: %v", err)
	}
	if hov.Contents.Kind != "markdown" {
		t.Errorf("contents.kind = %q", hov.Contents.Kind)
	}
	if !strings.Contains(hov.Contents.Value, "number") {
		t.Errorf("hover said: %q", hov.Contents.Value)
	}
	if hov.Range == nil || hov.Range.Start.Line != 13 {
		t.Errorf("the range must cover the hovered value on line 14 (0-based 13), got %+v", hov.Range)
	}
}

// symbolUnder reads raw text because the expression AST carries no offsets. The edges are
// where that shows, and a wrong answer here is a hover about something the reader is not
// pointing at.
func TestSymbolUnderTruncatesAtTheSegmentTheCursorIsIn(t *testing.T) {
	const expr = `  count: "$: (self.previous.count ?? 0) + 1"`
	for _, c := range []struct {
		name string
		col  int
		want string
	}{
		{"on the root", 16, "self"},
		{"on a middle segment", 24, "self.previous"},
		{"on the last segment", 31, "self.previous.count"},
		{"just past the last segment", 34, "self.previous.count"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := symbolUnder(expr, c.col)
			if !ok {
				t.Fatalf("no symbol at column %d (%q)", c.col, string(expr[c.col-1]))
			}
			if got != c.want {
				t.Errorf("column %d (%q) = %q, want %q", c.col, string(expr[c.col-1]), got, c.want)
			}
		})
	}
}

// Nothing that is not a member path: reporting one would put an error in the popup for text
// the reader is not asking about.
func TestSymbolUnderReportsNothingWhereThereIsNoPath(t *testing.T) {
	const expr = `  count: "$: (self.previous.count ?? 0) + 1"`
	for _, c := range []struct {
		name string
		col  int
	}{
		{"on an operator", 36},
		{"on a numeric literal", 38},
		{"past the end of the line", 200},
		{"before the start", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, ok := symbolUnder(expr, c.col); ok {
				t.Errorf("column %d resolved to %q", c.col, got)
			}
		})
	}
}
