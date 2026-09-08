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

// The reason to build hover: the type an author is guessing at, without leaving the file.
func TestHoverOverAnExpressionGivesItsType(t *testing.T) {
	md := hoverOf(t, hoverDoc, 14, 20)
	if !strings.Contains(md, "self.result.total * 2") || !strings.Contains(md, "number") {
		t.Errorf("want the expression and its inferred type, got:\n%s", md)
	}
}

// A `${ }` inside a longer string types as the string it renders into, so the leaf says
// nothing — but the interpolation being written has a type of its own, and a URL is where most
// expressions in a definition live.
func TestHoverInsideAnInterpolationTypesThatInterpolation(t *testing.T) {
	md := hoverOf(t, hoverDoc, 10, 30)
	if !strings.Contains(md, "input.amount") || !strings.Contains(md, "number") {
		t.Errorf("want the interpolation typed, got:\n%s", md)
	}
}

func TestHoverOnASlotNamesItsType(t *testing.T) {
	md := hoverOf(t, hoverDoc, 13, 6)
	if !strings.Contains(md, "tasks.price.output") || !strings.Contains(md, "object{grand}") {
		t.Errorf("want the slot address and its type, got:\n%s", md)
	}
}

// The scope is the other half of "what could I write here", and it is the same answer
// `genctl schema context` gives at that address.
func TestHoverReportsTheScopeAndTheSlotItBelongsTo(t *testing.T) {
	md := hoverOf(t, hoverDoc, 15, 13)
	if !strings.Contains(md, "tasks.price.switch") {
		t.Errorf("want the governing slot named, got:\n%s", md)
	}
	if !strings.Contains(md, "self") {
		t.Errorf("a switch sees self; the scope must say so:\n%s", md)
	}
}

// The action phase runs before the task has a result, so `self` is NOT in scope there — the
// difference between the two scopes is most of why this view exists.
func TestHoverDistinguishesTheActionScopeFromTheSwitchScope(t *testing.T) {
	action := hoverOf(t, hoverDoc, 10, 30)
	if strings.Contains(action, "self") {
		t.Errorf("an action runs before its own result exists:\n%s", action)
	}
}

// Hover decodes leniently: a reader asking about one slot is not asking about a typo in
// another, and refusing until the file is clean is what §7b was written against.
func TestHoverStillAnswersWhenAnotherPartOfTheFileIsBroken(t *testing.T) {
	broken := strings.Replace(hoverDoc, "    switch: end\n", "    switch: end\n    on_eror: []\n", 1)
	md := hoverOf(t, broken, 14, 20)
	if !strings.Contains(md, "number") {
		t.Errorf("a typo elsewhere must not silence hover:\n%s", md)
	}
}

// An expression that does not type is exactly when someone hovers it, so the reason has to be
// the answer rather than an empty popup.
func TestHoverOverABrokenExpressionSaysWhy(t *testing.T) {
	broken := strings.Replace(hoverDoc, "self.result.total * 2", "self.result.nope", 1)
	md := hoverOf(t, broken, 14, 20)
	if !strings.Contains(md, "nope") {
		t.Errorf("want the inference failure, got:\n%s", md)
	}
}

func TestHoverOverNothingInParticularIsSilent(t *testing.T) {
	if md, _, ok := hoverAt(hoverDoc, 1, 1); ok {
		t.Errorf("the document name has no type and no scope; got a popup:\n%s", md)
	}
}

func TestHoverAdvertisedAndAnswered(t *testing.T) {
	msgs, _ := session(t,
		frame("initialize", 1, map[string]any{}),
		openDoc(uri, hoverDoc),
		frame("textDocument/hover", 2, map[string]any{
			"textDocument": map[string]any{"uri": uri},
			// 0-based: line 14, the `grand:` expression.
			"position": map[string]any{"line": 13, "character": 19},
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
