package template

import (
	"encoding/json"
	"strings"
	"testing"
)

// --- Splitting: where does a ${ } interpolation end? ---
//
// The nested- and string-literal cases below are the ones a "scan to the next }"
// or a brace-counting splitter gets wrong. They are what motivated the splitter's
// candidate-and-reparse design, so each is named separately.

func TestSplit_EmptySource(t *testing.T) {
	assertSplit(t, ``, ``)
}

func TestSplit_PlainTextOnly(t *testing.T) {
	assertSplit(t, `plain text`, `LIT("plain text")`)
}

// A single interpolation makes the whole template one expression chunk.

func TestSplit_SingleExpressionPadded(t *testing.T) {
	assertSplit(t, `${ input.x }`, `EXPR(" input.x ")`)
}

func TestSplit_SingleExpressionUnpadded(t *testing.T) {
	assertSplit(t, `${input.x}`, `EXPR("input.x")`)
}

// Mixed templates.

func TestSplit_LiteralsAroundExpression(t *testing.T) {
	assertSplit(t, `Hello ${ input.name }!`, `LIT("Hello ") EXPR(" input.name ") LIT("!")`)
}

func TestSplit_LiteralBetweenExpressions(t *testing.T) {
	assertSplit(t, `${ a } and ${ b }`, `EXPR(" a ") LIT(" and ") EXPR(" b ")`)
}

func TestSplit_AdjacentExpressions(t *testing.T) {
	assertSplit(t, `${ a }${ b }`, `EXPR(" a ") EXPR(" b ")`)
}

// A "}" that closes nested braces must not end the block.

func TestSplit_NestedObjectLiteralClosesWithDelimiter(t *testing.T) {
	assertSplit(t, `${ {a: {b: 1}} }`, `EXPR(" {a: {b: 1}} ")`)
}

func TestSplit_ObjectLiteralInLambdaBody(t *testing.T) {
	assertSplit(t, `${ map(a, x => {z: x.n}) }`, `EXPR(" map(a, x => {z: x.n}) ")`)
}

func TestSplit_NestedObjectLiteralInLambdaBody(t *testing.T) {
	assertSplit(t, `${ map(a, x => {z: {deep: x.n}}) }`, `EXPR(" map(a, x => {z: {deep: x.n}}) ")`)
}

func TestSplit_ObjectLiteralWithNoPadding(t *testing.T) {
	assertSplit(t, `${{a: 1}}`, `EXPR("{a: 1}")`)
}

func TestSplit_ObjectLiteralAbuttingCloseDelimiter(t *testing.T) {
	assertSplit(t, `${ {a: 1}}`, `EXPR(" {a: 1}")`)
}

// A "}" inside any supported string form must not end the block. Byte literals
// (b'...') are rejected by the grammar, but the lexer still treats them as one
// token, so a candidate cutting through one still fails.

func TestSplit_DelimiterInsideDoubleQuotedString(t *testing.T) {
	assertSplit(t, `${ "x}y" }`, `EXPR(" \"x}y\" ")`)
}

func TestSplit_DelimiterInsideSingleQuotedString(t *testing.T) {
	assertSplit(t, `${ 'it}s' }`, `EXPR(" 'it}s' ")`)
}

func TestSplit_DelimiterInsideRawString(t *testing.T) {
	assertSplit(t, "${ `raw}str` }", "EXPR(\" `raw}str` \")")
}

func TestSplit_DelimiterAfterEscapedQuote(t *testing.T) {
	assertSplit(t, `${ "esc\"}" }`, `EXPR(" \"esc\\\"}\" ")`)
}

// An unbalanced brace inside a string desynchronizes a brace counter; this
// template parses today and must keep parsing.
func TestSplit_UnbalancedBraceInsideString(t *testing.T) {
	assertSplit(t, `${ "a{b" }`, `EXPR(" \"a{b\" ")`)
}

// Trailing "}" with no opening block is literal text.
func TestSplit_TrailingDelimiterIsLiteral(t *testing.T) {
	assertSplit(t, `${ a } b }`, `EXPR(" a ") LIT(" b }")`)
}

// Shortest match: the first candidate that parses wins.
func TestSplit_ShortestMatchWins(t *testing.T) {
	assertSplit(t, `${ a } + b`, `EXPR(" a ") LIT(" + b")`)
}

// --- Parse errors ---

func TestParseError_UnclosedBlock(t *testing.T) {
	assertParseError(t, `${ unclosed`, "unclosed ${")
}

func TestParseError_UnexpectedToken(t *testing.T) {
	assertParseError(t, `${ a b }`, "unexpected")
}

func TestParseError_EmptyBlock(t *testing.T) {
	assertParseError(t, `${}`, "expression")
}

// TestParseErrorReportsLongestCandidate pins the diagnostic rule: when no candidate
// parses, the error must come from the full body, not from a truncated prefix whose
// failure ("unexpected EOF") tells the author nothing.
func TestParseErrorReportsLongestCandidate(t *testing.T) {
	// The first "}" candidate closes the nested literal, so its body
	// `map(xs, {a: {b: 1}` is merely truncated. The real error — the missing
	// lambda — is only visible in the full body.
	_, err := Parse(`${ map(xs, {a: {b: 1}}) }`)
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if !strings.Contains(err.Error(), `map(xs, {a: {b: 1}}) `) {
		t.Errorf("error should quote the full body, got: %v", err)
	}
}

// --- EvalAny ---

func TestEvalAny_PlainText(t *testing.T) {
	assertEvalAny(t, `plain`, "plain")
}

func TestEvalAny_EmptySource(t *testing.T) {
	assertEvalAny(t, ``, "")
}

// A $: expression preserves the value as given, rather than stringifying it.
func TestEvalAny_ExprPreservesInt(t *testing.T) {
	assertEvalAny(t, `$: input.n`, 3)
}

func TestEvalAny_ExprPreservesBool(t *testing.T) {
	assertEvalAny(t, `$: input.ok`, true)
}

// Interpolation stringifies.
func TestEvalAny_MixedWithLiteralStringifies(t *testing.T) {
	assertEvalAny(t, `n=${ input.n }`, "n=3")
}

func TestEvalAny_TwoInterpolationsStringify(t *testing.T) {
	assertEvalAny(t, `${ input.name }-${ input.n }`, "ann-3")
}

// Even a lone interpolation stringifies — only $: preserves type.
func TestEvalAny_LoneInterpolationStringifies(t *testing.T) {
	assertEvalAny(t, `${ input.n }`, "3")
}

// Arithmetic yields an exact decimal as json.Number, not a Go int.
func TestEvalAny_ArithmeticYieldsJSONNumber(t *testing.T) {
	assertEvalAny(t, `$: input.n + 1`, json.Number("4"))
}

// --- InferType ---

func TestInferType_PlainTextIsString(t *testing.T) {
	assertInferType(t, `plain`, "string")
}

// A $: expression preserves its type.
func TestInferType_ExprPreservesType(t *testing.T) {
	assertInferType(t, `$: input.n`, "integer")
}

// Interpolation is always a string.
func TestInferType_MixedWithLiteralIsString(t *testing.T) {
	assertInferType(t, `n=${ input.n }`, "string")
}

func TestInferType_TwoInterpolationsIsString(t *testing.T) {
	assertInferType(t, `${ input.name }${ input.n }`, "string")
}

// A lone interpolation is still a string (no type preservation).
func TestInferType_LoneInterpolationIsString(t *testing.T) {
	assertInferType(t, `${ input.n }`, "string")
}

// A nullable interpolation would stringify to "null".
func TestInferType_RejectsNullableInterpolation(t *testing.T) {
	assertInferRejects(t, `x=${ input.opt }`)
}

// The same expression as a $: leaf is fine — the null is preserved, not stringified.
func TestInferType_AllowsNullableAsExpr(t *testing.T) {
	assertInferAccepts(t, `$: input.opt`)
}

// --- ReferencesSecret ---

func TestReferencesSecret_PlainText(t *testing.T) {
	assertReferencesSecret(t, `plain`, false)
}

func TestReferencesSecret_NonSecretField(t *testing.T) {
	assertReferencesSecret(t, `${ input.name }`, false)
}

func TestReferencesSecret_SecretField(t *testing.T) {
	assertReferencesSecret(t, `${ input.token }`, true)
}

func TestReferencesSecret_SecretInMixedTemplate(t *testing.T) {
	assertReferencesSecret(t, `prefix-${ input.token }`, true)
}

func TestReferencesSecret_SecretInSecondBlock(t *testing.T) {
	assertReferencesSecret(t, `${ input.name }${ input.token }`, true)
}

// --- RootRefs / OutputRefs ---

func TestRootRefs_DetectsInputAndErrorRoots(t *testing.T) {
	r := mustParse(t, `${ input.a }-${ outputs.fetch.b }-${ error.code }`).RootRefs()
	if !r.Input || !r.Error {
		t.Errorf("input/error roots not detected: %+v", r)
	}
}

func TestRootRefs_CollectsOutputRoots(t *testing.T) {
	r := mustParse(t, `${ input.a }-${ outputs.fetch.b }-${ error.code }`).RootRefs()
	if len(r.Outputs) != 1 || r.Outputs[0] != "fetch" {
		t.Errorf("Outputs = %v, want [fetch]", r.Outputs)
	}
}

func TestRootRefs_DetectsSelfResult(t *testing.T) {
	if r := mustParse(t, `$: self.result.x`).RootRefs(); !r.SelfResult {
		t.Error("self.result not reported")
	}
}

func TestRootRefs_DetectsSelfPrevious(t *testing.T) {
	if r := mustParse(t, `$: self.previous.x`).RootRefs(); !r.SelfPrevious {
		t.Error("self.previous not reported")
	}
}

// SelfResult and SelfPrevious must both survive the merge across blocks. The
// engine's shape roots previously dropped SelfResult, so an externalized
// self.result read from a shape came back nil.
func TestRootRefs_MergesSelfRootsAcrossBlocks(t *testing.T) {
	r := mustParse(t, `${ self.result.x }/${ self.previous.y }`).RootRefs()
	if !r.SelfResult || !r.SelfPrevious {
		t.Errorf("merged self roots = %+v, want both", r)
	}
}

func TestOutputRefs_DedupesAndSorts(t *testing.T) {
	got := mustParse(t, `${ outputs.b.x }-${ outputs.a.y }-${ outputs.b.z }`).OutputRefs()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("OutputRefs = %v, want [a b] (deduped, sorted)", got)
	}
}

func TestOutputRefs_EmptyForLiteralTemplate(t *testing.T) {
	if got := mustParse(t, `plain`).OutputRefs(); len(got) != 0 {
		t.Errorf("OutputRefs on a literal = %v, want empty", got)
	}
}

// --- Get memoisation ---

func TestGetMemoisesParsedTemplate(t *testing.T) {
	const src = `${ input.memo_probe }`
	a, err := Get(src)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	b, err := Get(src)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if a != b {
		t.Error("Get should return the same parsed Template for the same source")
	}
}

// Failures are cached too, so a bad template does not re-parse every tick.
func TestGetMemoisesParseFailure(t *testing.T) {
	if _, err := Get(`${ bad bad }`); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := Get(`${ bad bad }`); err == nil {
		t.Fatal("expected the cached error")
	}
}

// --- map and literals inside templates ---

// A $: expression preserves the array a map produces; it is only interpolation
// that must flatten to text.
func TestExprMapPreservesArray(t *testing.T) {
	env := map[string]any{"input": map[string]any{
		"rows": []any{
			map[string]any{"code": "A", "count": 1},
			map[string]any{"code": "B", "count": 2},
		},
	}}
	got, err := mustParse(t, `$: map(input.rows, r => {sku: r.code, qty: r.count + 1})`).EvalAny(env)
	if err != nil {
		t.Fatalf("EvalAny: %v", err)
	}
	rows, ok := got.([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %#v", got)
	}
	first, _ := rows[0].(map[string]any)
	if first["sku"] != "A" || first["qty"] != json.Number("2") {
		t.Errorf("first row = %#v, want {sku:A qty:2}", first)
	}
}

// A value that stringify cannot render is a guaranteed runtime failure, so it
// must be rejected when the definition is registered rather than when the
// process runs. Object and array literals make this trivially reachable, but the
// same hole existed for any array-typed field.
func TestInterpolationRejectsUnstringifiable(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"array_typed_field", `x=${ input.tags }`},
		{"array_literal", `x=${ [1, 2] }`},
		{"object_literal", `x=${ {a: 1} }`},
		{"map_result", `x=${ map(input.tags, s => s) }`},
	} {
		t.Run(c.name, func(t *testing.T) { assertInferRejects(t, c.src) })
	}
}

// The same values are fine as a $: expression, where the type is preserved
// rather than stringified.
func TestExprAllowsUnstringifiable(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"array_typed_field", `$: input.tags`},
		{"array_literal", `$: [1, 2]`},
		{"object_literal", `$: {a: 1}`},
	} {
		t.Run(c.name, func(t *testing.T) { assertInferAccepts(t, c.src) })
	}
}

// Interpolating a scalar still works — the guard must not over-reject.
func TestInterpolationAllowsScalars(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"integer_field", `n=${ input.n }`},
		{"string_field", `s=${ input.name }`},
		{"comparison", `b=${ input.n > 1 }`},
		{"coalesce", `c=${ input.opt ?? "none" }`},
	} {
		t.Run(c.name, func(t *testing.T) { assertInferType(t, c.src, "string") })
	}
}

// A secret reached only through a lambda parameter has no path from the root
// context, so the taint walk must follow the binding or the value reaches logs
// unredacted.
func TestExprSecretThroughMapBodyTaints(t *testing.T) {
	if !mustParse(t, `$: map(input.people, p => p.secret)`).ReferencesSecret(ctxSchema(t)) {
		t.Error("a secret on the element type must taint the expression")
	}
}

func TestExprNonSecretThroughMapBodyDoesNotTaint(t *testing.T) {
	if mustParse(t, `$: map(input.people, p => p.name)`).ReferencesSecret(ctxSchema(t)) {
		t.Error("a non-secret element field must not taint")
	}
}

// A lambda parameter shadows a context root, so it must not be reported as a
// read of that root — buildEnv would otherwise load a slot nothing references.
func TestRootRefs_LambdaParamShadowingInputRoot(t *testing.T) {
	if r := mustParse(t, `$: map(outputs.a.items, input => input.x)`).RootRefs(); r.Input {
		t.Error("a lambda parameter named input must not be reported as reading the input root")
	}
}

// ...and, worse, the shadowing must not hide a genuine read either.
func TestRootRefs_GenuineInputReadInsideMapSource(t *testing.T) {
	if r := mustParse(t, `$: map(input.rows, r => r.x)`).RootRefs(); !r.Input {
		t.Error("a genuine input read inside a map source must still be reported")
	}
}

func TestOutputRefs_InsideMapSource(t *testing.T) {
	if got := mustParse(t, `$: map(outputs.a.items, x => x.y)`).OutputRefs(); len(got) != 1 || got[0] != "a" {
		t.Errorf("OutputRefs = %v, want [a]", got)
	}
}

// Static reports the constant value for a pure literal and false for any dynamic leaf
// (a ${ } interpolation or a $: expression), so callers can format-check hand-written
// literals while leaving dynamic values to runtime. $$ unescapes in a literal.
func TestStatic(t *testing.T) {
	cases := []struct {
		src        string
		wantVal    string
		wantStatic bool
	}{
		{"2xx", "2xx", true},
		{"404", "404", true},
		{"", "", true},
		{"a$$b", "a$b", true},         // $$ -> literal $
		{"${ 1 }", "", false},         // interpolation is dynamic
		{"pre${ x }post", "", false},  // mixed literal + interpolation
		{"$: input.codes", "", false}, // typed expression leaf
	}
	for _, c := range cases {
		tpl, err := Get(c.src)
		if err != nil {
			t.Fatalf("Get(%q): %v", c.src, err)
		}
		val, static := tpl.Static()
		if static != c.wantStatic || val != c.wantVal {
			t.Errorf("Static(%q) = (%q, %v), want (%q, %v)", c.src, val, static, c.wantVal, c.wantStatic)
		}
	}
}
