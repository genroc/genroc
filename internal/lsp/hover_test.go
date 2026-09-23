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
//	10       method: post
//	11       url: "https://x/p?a=${ input.amount }"
//	12       responses:
//	13         "200": { type: object, properties: { total: { type: number } }, required: [total] }
//	14     output:
//	15       grand: "$: self.result.total * 2"
//	16     switch: end
const hoverDoc = `name: demo
input_schema:
  type: object
  properties: { amount: { type: number } }
  required: [amount]
tasks:
  - id: price
    action:
      type: fetch
      method: post
      url: "https://x/p?a=${ input.amount }"
      responses:
        "200": { type: object, properties: { total: { type: number } }, required: [total] }
    output:
      grand: "$: self.result.total * 2"
    switch: end
`

func hoverOf(t *testing.T, text string, line, col int) string {
	t.Helper()
	md, _, ok := hoverAt(text, "", line, col)
	if !ok {
		t.Fatalf("nothing to say at %d:%d", line, col)
	}
	return md
}

// Line 15 is `      grand: "$: self.result.total * 2"`, so column 30 is inside `total`, 19 is
// inside `self`, and 36 is the `*` — where there is no symbol and the leaf answers.

// The reason to build hover: the type an author is otherwise guessing at.
func TestHoverOverASymbolTypesThatSymbol(t *testing.T) {
	if md := hoverOf(t, hoverDoc, 15, 32); md != "`self.result.total` → `number`" {
		t.Errorf("got: %s", md)
	}
}

// Truncated AT the segment, so walking a path shows each level rather than always the leaf.
func TestHoverOverAnIntermediateSegmentTypesThePathUpToIt(t *testing.T) {
	if md := hoverOf(t, hoverDoc, 15, 19); !strings.HasPrefix(md, "`self` → `object{") {
		t.Errorf("got: %s", md)
	}
}

// No symbol under the cursor: the expression it sits in is the answer.
func TestHoverOnAnOperatorTypesTheWholeExpression(t *testing.T) {
	if md := hoverOf(t, hoverDoc, 15, 36); md != "`self.result.total * 2` → `number`" {
		t.Errorf("got: %s", md)
	}
}

// A `${ }` inside a longer string types as the string it renders into, so the leaf says
// nothing — but the interpolation being written has a type of its own, and a URL is where most
// expressions in a definition live.
func TestHoverInsideAnInterpolationTypesThatInterpolation(t *testing.T) {
	if md := hoverOf(t, hoverDoc, 11, 38); md != "`input.amount` → `number`" {
		t.Errorf("got: %s", md)
	}
}

func TestHoverOnASlotNamesItsType(t *testing.T) {
	md := hoverOf(t, hoverDoc, 14, 6)
	if !strings.Contains(md, "tasks.price.output") || !strings.Contains(md, "object{grand}") {
		t.Errorf("want the slot address and its type, got: %s", md)
	}
}

// One line. A hover is read at a glance, and the scope a slot carries is a different question
// — `genctl schema context` is where that one is asked.
func TestHoverIsOneLine(t *testing.T) {
	for _, at := range [][2]int{{15, 32}, {15, 36}, {11, 38}, {14, 6}} {
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
	if md := hoverOf(t, broken, 15, 32); !strings.Contains(md, "number") {
		t.Errorf("a typo elsewhere must not silence hover: %s", md)
	}
}

// An expression that does not type is silent, because the editor puts the DIAGNOSTIC for that
// position at the top of the same popup — saying it twice is what a reader sees.
func TestHoverOverABrokenExpressionLeavesItToTheDiagnostic(t *testing.T) {
	broken := strings.Replace(hoverDoc, "self.result.total * 2", "self.result.nope * 2", 1)
	if md, _, ok := hoverAt(broken, "", 15, 36); ok {
		t.Errorf("the diagnostic already says this; hover added: %s", md)
	}
}

// A symbol inside it still types, though, and that is what the diagnostic does NOT say.
func TestHoverStillTypesAWorkingSymbolInsideABrokenExpression(t *testing.T) {
	broken := strings.Replace(hoverDoc, "self.result.total * 2", "self.result.nope * 2", 1)
	if md := hoverOf(t, broken, 15, 19); !strings.HasPrefix(md, "`self` → `object{") {
		t.Errorf("got: %s", md)
	}
}

// A name the AUTHOR chose — a property in their own schema — is the one thing here that means
// nothing to the definition language, so it is the one thing with no answer.
func TestHoverOverAnAuthorsOwnNameIsSilent(t *testing.T) {
	//	 4   properties: { amount: { type: number } }
	if md, _, ok := hoverAt(hoverDoc, "", 4, 18); ok {
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
			"position": map[string]any{"line": 14, "character": 31},
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
	if hov.Range == nil || hov.Range.Start.Line != 14 {
		t.Errorf("the range must cover the hovered value on line 15 (0-based 14), got %+v", hov.Range)
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

// A lambda parameter is the one name in an expression that the SLOT's context does not carry:
// the expression binds it. Line numbers are asserted against this, so a change here moves the
// cases below.
//
//	 1 name: fanout
//	 2 input_schema:
//	 3   type: array
//	 4   items:
//	 5     type: object
//	 6     properties: { who: { type: string } }
//	 7     required: [who]
//	 8 tasks:
//	 9   - id: prepare
//	10     action:
//	11       type: child_list
//	12       name: greet
//	13       over: "$: map(input, (row) => { to: row.who })"
//	14       result_schema: { type: object }
//	15     output: "$: self.result"
//	16     switch: end
const lambdaDoc = `name: fanout
input_schema:
  type: array
  items:
    type: object
    properties: { who: { type: string } }
    required: [who]
tasks:
  - id: prepare
    action:
      type: child_list
      name: greet
      over: "$: map(input, (row) => { to: row.who })"
      result_schema: { type: object }
    output: "$: self.result"
    switch: end
`

// Line 13 is the `over:` expression: column 30 is inside the binder `(row)`, 44 inside `row`
// in the body, 48 inside `who`, and 22 inside the map's source `input`.

// The reported bug: hover fell through to the whole expression on every name in the lambda,
// because the parameter is bound nowhere the slot's context can see.
func TestHoverOverALambdaParameterTypesTheElement(t *testing.T) {
	for _, at := range []struct {
		name string
		col  int
	}{
		{"on the binder", 30},
		{"on the use in the body", 44},
	} {
		t.Run(at.name, func(t *testing.T) {
			if md := hoverOf(t, lambdaDoc, 13, at.col); md != "`row` → `object{who}`" {
				t.Errorf("got: %s", md)
			}
		})
	}
}

// The parameter is a path root like any other, so the segments after it must walk.
func TestHoverInsideALambdaBodyWalksThroughTheParameter(t *testing.T) {
	if md := hoverOf(t, lambdaDoc, 13, 48); md != "`row.who` → `string`" {
		t.Errorf("got: %s", md)
	}
}

// The map's SOURCE is written in the slot's own scope, not the lambda's — a parameter reaching
// it would retype a position that has always answered correctly.
func TestHoverOverAMapSourceIsUnaffectedByTheParameter(t *testing.T) {
	if md := hoverOf(t, lambdaDoc, 13, 22); md != "`input` → `array<object{who}>`" {
		t.Errorf("got: %s", md)
	}
}

// A parameter SHADOWING a root is bound nowhere: the AST carries no offsets, so nothing can
// tell the source's `input` from the body's, and the root's answer stands at both. That is
// right at the source and stale in the body — the trade is deliberate, since binding instead
// would retype the source, which has always been correct. specs/language-server.md §6.
func TestHoverDoesNotRebindARootAParameterShadows(t *testing.T) {
	shadowed := strings.Replace(lambdaDoc, "(row) => { to: row.who }", "(input) => { to: input.who }", 1)
	// Column 22 is the map's source, 30 the binder: `input` either way.
	for _, col := range []int{22, 30} {
		if md := hoverOf(t, shadowed, 13, col); md != "`input` → `array<object{who}>`" {
			t.Errorf("column %d must keep the root's own type, got: %s", col, md)
		}
	}
}

// The answer is MARKDOWN, and `array<string>` in it is bold text followed by an unknown HTML
// tag: the renderer drops the tag and the popup reads `array`. Every assertion above passed
// while that was true, because they compare the string the server sends, not what an editor
// makes of it.
// A lone `>` is prose — the struct tags are written with `->` — so only `<` is looked for.
func TestNoTypeIsRenderedOutsideACodeSpan(t *testing.T) {
	for _, doc := range []string{hoverDoc, lambdaDoc} {
		lines := strings.Split(doc, "\n")
		for line, text := range lines {
			for col := 1; col <= len(text); col++ {
				md, _, ok := hoverAt(doc, "", line+1, col)
				if !ok {
					continue
				}
				if strings.Contains(outsideCodeSpans(md), "<") {
					t.Errorf("%d:%d is markup an editor eats: %s", line+1, col, md)
				}
			}
		}
	}
}

// outsideCodeSpans is the part of a markdown line a renderer reads as markup.
func outsideCodeSpans(md string) string {
	var prose strings.Builder
	for i, part := range strings.Split(md, "`") {
		if i%2 == 0 {
			prose.WriteString(part)
		}
	}
	return prose.String()
}

// Comments are where the cursor rests while an author reads, and they are prose. The position
// resolves to the mapping around them, so a hover there answered with the enclosing key's
// description -- `action`'s, over a note about a delay.
//
//	 1 name: demo
//	 2 # prose, and not a slot
//	 3 tasks:
//	 4   - id: wait
//	 5     action:
//	 6       type: delay   # the process is parked for two hours
//	 7       for: 2h
//	 8     switch: next
//	 9   - id: call
//	10     action:
//	11       type: fetch
//	12       method: get
//	13       url: "https://x/p#frag"  # the one on the left is not a comment
//	14     switch: end
const commentDoc = `name: demo
# prose, and not a slot
tasks:
  - id: wait
    action:
      type: delay   # the process is parked for two hours
      for: 2h
    switch: next
  - id: call
    action:
      type: fetch
      method: get
      url: "https://x/p#frag"  # the one on the left is not a comment
    switch: end
`

func TestHoverSaysNothingInsideAComment(t *testing.T) {
	for _, tc := range []struct {
		name      string
		line, col int
	}{
		{"the # that opens one", 6, 21},
		{"the prose after it", 6, 35},
		{"a line that is only a comment", 2, 5},
		{"one that follows a quoted scalar", 13, 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if md, _, ok := hoverAt(commentDoc, "", tc.line, tc.col); ok {
				t.Fatalf("answered inside a comment: %s", md)
			}
		})
	}
}

// The other half: a comment silences its own line, and only from where it opens.
func TestHoverAnswersBesideAComment(t *testing.T) {
	for _, tc := range []struct {
		name      string
		line, col int
		want      string
	}{
		{"the key a trailing comment follows", 6, 7, "Delay action"},
		{"a `#` inside a quoted scalar is not one", 13, 25, "Request URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md, _, ok := hoverAt(commentDoc, "", tc.line, tc.col)
			if !ok || !strings.Contains(md, tc.want) {
				t.Fatalf("want something containing %q, got %q (ok=%v)", tc.want, md, ok)
			}
		})
	}
}
