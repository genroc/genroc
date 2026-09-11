package lsp

import (
	"slices"
	"strings"
	"testing"

	"genroc/internal/errcode"
)

//	1 name: demo
//	2 input_schema:
//	3   type: object
//	4   properties: { amount: { type: number }, currency: { type: string } }
//	5   required: [amount, currency]
//	6 tasks:
//	7   - id: price
//	8     action:
//	9       type: fetch
//
// 10       method: post
//
// 11       url: "https://x/p?a=${ input. }"
// 12       responses:
// 13         "200": { type: object, properties: { total: { type: number } }, required: [total] }
// 14     output:
// 15       grand: "$: self.result."
// 16     switch: end
const completionDoc = `name: demo
input_schema:
  type: object
  properties: { amount: { type: number }, currency: { type: string } }
  required: [amount, currency]
tasks:
  - id: price
    action:
      type: fetch
      method: post
      url: "https://x/p?a=${ input. }"
      responses:
        "200": { type: object, properties: { total: { type: number } }, required: [total] }
    output:
      grand: "$: self.result."
    switch: end
`

func labels(items []completionItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Label
	}
	slices.Sort(out)
	return out
}

func completed(t *testing.T, text string, line, col int) []string {
	t.Helper()
	items := completeAt(text, line, col)
	if len(items) == 0 {
		t.Fatalf("no completions at %d:%d", line, col)
	}
	return labels(items)
}

// ── expressions: the scope, which no schema can describe ─────────────────────────

func TestCompletingAfterADotOffersThatValuesMembers(t *testing.T) {
	got := completed(t, completionDoc, 11, 36) // after `input.`
	if !slices.Equal(got, []string{"amount", "currency"}) {
		t.Errorf("want the input's members, got %v", got)
	}
}

func TestCompletingIntoAnActionResultUsesItsDeclaredShape(t *testing.T) {
	got := completed(t, completionDoc, 15, 30) // after `self.result.`
	if !slices.Equal(got, []string{"total"}) {
		t.Errorf("want the 200 response's members, got %v", got)
	}
}

// A bare `$: ` offers the roots of the scope — and which roots depend on the slot, which is
// the whole reason completion consults the context view rather than the schema.
func TestABareExpressionOffersTheScopeRoots(t *testing.T) {
	got := completed(t, completionDoc, 15, 18)
	if !slices.Contains(got, "self") || !slices.Contains(got, "input") {
		t.Errorf("an output sees self and input, got %v", got)
	}
}

func TestTheActionScopeHasNoSelf(t *testing.T) {
	// Column 32 is inside `${ }` on the url, before `input`.
	got := completed(t, completionDoc, 11, 32)
	if slices.Contains(got, "self") {
		t.Errorf("an action runs before its own result exists, got %v", got)
	}
}

// The case the repair exists for: this is what the buffer holds WHILE someone types, and it is
// not valid YAML. Refusing until the quote is closed refuses exactly when help is wanted.
func TestCompletionWorksOnTextThatDoesNotParse(t *testing.T) {
	midTyping := strings.Replace(completionDoc, `      grand: "$: self.result."`, `      grand: "$: self.result.`, 1)
	got := completed(t, midTyping, 15, 30)
	if !slices.Equal(got, []string{"total"}) {
		t.Errorf("want the result's members while the quote is still open, got %v", got)
	}
}

func TestCompletionWorksInsideAnUnclosedInterpolation(t *testing.T) {
	midTyping := strings.Replace(completionDoc, `      url: "https://x/p?a=${ input. }"`, `      url: "https://x/p?a=${ input.`, 1)
	got := completed(t, midTyping, 11, 36)
	if !slices.Equal(got, []string{"amount", "currency"}) {
		t.Errorf("want the input's members with the interpolation still open, got %v", got)
	}
}

// ── keys: the schema, discriminated ──────────────────────────────────────────────

// What yaml-language-server cannot do: `discriminator` is an OpenAPI keyword it ignores, so it
// offers the union of six action shapes. We read the `type` and descend into one.
func TestAFetchActionOffersFetchKeysOnly(t *testing.T) {
	got := completed(t, completionDoc, 12, 7) // on `responses:`, so the action is the mapping
	if slices.Contains(got, "process") || slices.Contains(got, "children") {
		t.Errorf("a fetch was offered another action's keys: %v", got)
	}
	if !slices.Contains(got, "headers") || !slices.Contains(got, "query") {
		t.Errorf("want fetch's own keys, got %v", got)
	}
}

func TestAChildActionOffersChildKeys(t *testing.T) {
	child := strings.Replace(completionDoc,
		"      type: fetch\n      method: post\n      url: \"https://x/p?a=${ input. }\"\n", "      type: child\n", 1)
	got := completed(t, child, 9, 7)
	if !slices.Contains(got, "name") || slices.Contains(got, "url") {
		t.Errorf("want the child action's keys and not fetch's, got %v", got)
	}
}

// A key already written is not a suggestion. This is the half that silently did nothing while
// only scalars carried a value in the index.
func TestKeysAlreadyWrittenAreNotOffered(t *testing.T) {
	got := completed(t, completionDoc, 16, 5) // on `switch:`, so the task is the mapping
	for _, written := range []string{"id", "action", "switch"} {
		if slices.Contains(got, written) {
			t.Errorf("%q is already written and was offered anyway: %v", written, got)
		}
	}
	if !slices.Contains(got, "on_error") {
		t.Errorf("want the keys still missing, got %v", got)
	}
}

func TestTheDocumentRootOffersDefinitionKeys(t *testing.T) {
	got := completed(t, completionDoc, 1, 1)
	if !slices.Contains(got, "config_schema") || slices.Contains(got, "name") {
		t.Errorf("want the root keys not yet written, got %v", got)
	}
}

// A description is what makes a completion list readable rather than a guessing game, and the
// schema already carries the prose from the struct tags.
func TestKeyCompletionsCarryTheirDocumentation(t *testing.T) {
	for _, it := range completeAt(completionDoc, 16, 5) {
		if it.Label == "on_error" {
			if !strings.Contains(it.Documentation, "error") {
				t.Errorf("on_error came with no usable documentation: %q", it.Documentation)
			}
			return
		}
	}
	t.Fatal("on_error was not offered")
}

// The vocabulary belongs to errcode, which is where a code is declared and described: adding
// one there must reach the editor with no edit here, so this asserts the whole set rather than
// a member of it. A list of CODES kept in this package would be the drift it is written
// against; the patterns beside them are spellings, and errcode stores none of them.
func TestTheOfferedCodesAreErrcodesOwn(t *testing.T) {
	doc := strings.Replace(completionDoc, "    switch: end\n", "    on_error:\n      - code: []\n        goto: end\n    switch: end\n", 1)
	line := 1 + strings.Count(doc[:strings.Index(doc, "- code: []")], "\n")
	col := 1 + strings.Index(lineAt(doc, line), "[]")

	var want []string
	for _, p := range fetchPatterns {
		want = append(want, p.code)
	}
	for _, info := range errcode.Catchable(errcode.KindFetch) {
		want = append(want, string(info.Code))
	}
	slices.Sort(want)

	if got := completed(t, doc, line, col+1); !slices.Equal(got, want) {
		t.Errorf("the fetch codes offered are not errcode's set\n got: %v\nwant: %v", got, want)
	}
}

func TestCompletionIsAdvertisedAndAnswered(t *testing.T) {
	msgs, _ := session(t,
		frame("initialize", 1, map[string]any{}),
		openDoc(uri, completionDoc),
		frame("textDocument/completion", 2, map[string]any{
			"textDocument": map[string]any{"uri": uri},
			"position":     map[string]any{"line": 14, "character": 29}, // after `self.result.`
		}),
		frame("exit", nil, nil))

	var res initializeResult
	if err := jsonUnmarshal(msgs[0]["result"], &res); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if res.Capabilities.CompletionProvider == nil ||
		!slices.Contains(res.Capabilities.CompletionProvider.TriggerCharacters, ".") {
		t.Error("a member list is wanted after `.`, so the editor has to be asked to re-request there")
	}

	var items []completionItem
	if err := jsonUnmarshal(msgs[len(msgs)-1]["result"], &items); err != nil {
		t.Fatalf("completion result: %v", err)
	}
	if !slices.Equal(labels(items), []string{"total"}) {
		t.Errorf("over the wire: %v", labels(items))
	}
}

// An empty list, never null: some clients fall back to guessing from the buffer's words.
func TestNothingToCompleteIsAnEmptyListNotNull(t *testing.T) {
	msgs, _ := session(t,
		openDoc(uri, completionDoc),
		frame("textDocument/completion", 1, map[string]any{
			"textDocument": map[string]any{"uri": "file:///w/other.yaml"},
			"position":     map[string]any{"line": 0, "character": 0},
		}),
		frame("exit", nil, nil))
	raw := string(msgs[len(msgs)-1]["result"])
	if raw != "[]" {
		t.Errorf("result = %s, want []", raw)
	}
}
