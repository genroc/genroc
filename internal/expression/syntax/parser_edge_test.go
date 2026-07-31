package syntax

import (
	"testing"
)

// Edge-case coverage for the grammar only: what it accepts, what it rejects, and
// whether a rejection points the author at the right character. Evaluation and
// inference are covered elsewhere; nothing here builds an env or a Schema.
//
// Each table is a homogeneous sweep — one assertion applied to many inputs — so
// every row is a named subtest and runs on its own with
// `go test -run 'TestEdgeX/row_name'`. Shared assertions live in helpers_test.go.

// -----------------------------------------------------------------------------
// Numbers
// -----------------------------------------------------------------------------

// Radix prefixes are integers in every form the lexer emits; there is no
// fractional hex/binary/octal spelling, which is why isIntLiteral can take the
// prefix as proof without looking for '.' or 'e'.
func TestEdgeRadixPrefixedIntegers(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"hex", `0x1F`, "31"},
		{"hex_uppercase_prefix", `0X1f`, "31"}, // uppercase prefix must take the same branch
		{"hex_digit_e", `0xE`, "14"},           // 'e' inside a hex literal is a digit, not an exponent
		{"binary", `0b1010`, "10"},             // 0b/0o are lexed by expr-lang and must survive
		{"octal", `0o17`, "15"},
		{"octal_uppercase_prefix", `0O17`, "15"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertIntLiteral(t, c.in, c.want) })
	}
}

// Digit separators are stripped before conversion, in both branches.
func TestEdgeDigitSeparators(t *testing.T) {
	t.Run("thousands", func(t *testing.T) { assertIntLiteral(t, `1_000`, "1000") })
	t.Run("millions", func(t *testing.T) { assertIntLiteral(t, `1_000_000`, "1000000") })
	t.Run("in_float", func(t *testing.T) { assertFloatLiteral(t, `1_000.5`, "1000.5") })
}

// An exponent makes the literal a float even when the value is integral — this
// is the case a dump-only test cannot see.
func TestEdgeExponentMakesFloat(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"lowercase_e", `1e3`, "1000"},
		{"uppercase_e", `1E3`, "1000"},
		{"negative_exponent", `1.5e-2`, "0.015"},
		{"explicit_plus_exponent", `2e+3`, "2000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertFloatLiteral(t, c.in, c.want) })
	}
}

// A '.' always means float, including the degenerate spellings. `.5` in
// particular must reach the number branch and not the '.field' shorthand
// rejection, which fires on a leading '.' operator.
func TestEdgeDecimalPointMakesFloat(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"leading_dot", `.5`, "0.5"},   // normalised: ".5" is not valid JSON
		{"trailing_dot", `1.`, "1"},    // likewise "1."
		{"integral_value", `3.0`, "3"}, // still a FloatNode, so it types as "number"
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertFloatLiteral(t, c.in, c.want) })
	}
}

// Literals are arbitrary precision, so int64 is not a boundary at all. It used
// to be: a literal one past int64 was rejected while the identical value arriving
// as data was exact, which made the language inconsistent with its own pipeline.
func TestEdgeMaxInt64Literal(t *testing.T) {
	assertIntLiteral(t, `9223372036854775807`, "9223372036854775807")
}

func TestEdgeLiteralPastInt64IsExact(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"int64_max_plus_one", `9223372036854775808`, "9223372036854775808"},
		{"far_past_int64", `99999999999999999999999`, "99999999999999999999999"},
		{"beyond_float64", `9007199254740993`, "9007199254740993"},
		{"fits_uint64_not_int64", `0xFFFFFFFFFFFFFFFF`, "18446744073709551615"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertIntLiteral(t, c.in, c.want) })
	}
}

// The same value nested inside a literal must survive too — the array element
// goes through the same number path.
func TestEdgeLiteralPastInt64InsideArray(t *testing.T) {
	assertParses(t, `[9223372036854775808]`, `[9223372036854775808]`)
}

// An index is the one place a Go int is genuinely required, since it indexes a
// slice; there the range check stays.
func TestEdgeIndexPastInt64Rejected(t *testing.T) {
	assertParseError(t, `a[9223372036854775808]`, "invalid index")
}

// A magnitude no decimal can hold is still an ordinary parse error rather than a
// silently clamped or non-finite value.
func TestEdgeNumberLiteralOverflow(t *testing.T) {
	assertParseError(t, `1e1000000000`, `invalid number`)
}

// Regression: a zero-padded decimal is base 10, not octal. This test found a
// real bug and now pins the fix.
//
// parseInt used strconv.ParseInt(s, 0, 64). Base 0 infers the radix from the
// prefix, and Go's base-0 rules honour C's legacy leading-zero octal — but the
// lexer's decimal branch has no prefix, and expr-lang parses it as base 10. So a
// zero-padded decimal silently diverged from expr-lang, breaking the package
// doc's promise that number rules stay identical to it:
//
//	017    was 15, expr-lang 17     <- silently wrong value, no error at all
//	a[010] was index 8, expr-lang 10 <- same bug on the index path
//	08     was rejected "out of range"; expr-lang reads 8
//
// The `08` message was doubly wrong: 8 is nowhere near out of range, it is just
// not an octal digit. parseInt now uses base 10 unless the literal carries an
// 0x/0b/0o prefix (see parser.go; isIntLiteral distinguishes exactly those).
func TestEdgeLeadingZeroIntegerIsDecimal(t *testing.T) {
	cases := []parseCase{
		{"octal_digits", `017`, `17`},
		{"non_octal_digits", `0900`, `900`},
		{"single_non_octal_digit", `08`, `8`},
		{"leading_zero_one", `01`, `1`},
		{"index_path", `a[010]`, `a[10]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

// -----------------------------------------------------------------------------
// Strings
// -----------------------------------------------------------------------------
//
// StringNode holds the already-unescaped value, so a regression in which quote
// form reaches which unescaper would show up as a literal backslash in output.

func TestEdgeDoubleQuotedEscapes(t *testing.T) {
	cases := []parseCase{
		{"newline", `"a\nb"`, "a\nb"},
		{"tab", `"a\tb"`, "a\tb"},
		{"escaped_quote", `"a\"b"`, `a"b`},
		{"escaped_backslash", `"a\\b"`, `a\b`},
		{"hex_escape", `"\x41"`, "A"},
		{"accented_char", `"é"`, "é"},
		{"empty", `""`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertStringLiteral(t, c.in, c.want) })
	}
}

// Single quotes are a full string form in expr-lang, not a char literal: same
// escapes, same resulting StringNode.
func TestEdgeSingleQuotedStrings(t *testing.T) {
	cases := []parseCase{
		{"newline_escape", `'a\nb'`, "a\nb"},
		{"escaped_quote", `'a\'b'`, "a'b"},
		{"empty", `''`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertStringLiteral(t, c.in, c.want) })
	}
}

// Non-ASCII must pass through byte-for-byte; the caret arithmetic in failAt
// counts bytes, so a mangled literal here would also skew errors.
func TestEdgeNonASCIIStrings(t *testing.T) {
	cases := []parseCase{
		{"accented_char", `"é"`, "é"},
		{"cjk", `"日本"`, "日本"},
		{"emoji", `"emoji 🙂"`, "emoji 🙂"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertStringLiteral(t, c.in, c.want) })
	}
}

// Backticks are raw: no escape processing at all. The doubled backtick is the
// only way to embed the delimiter.
func TestEdgeBacktickRawStrings(t *testing.T) {
	cases := []parseCase{
		{"no_escape_processing", "`raw\\nstr`", `raw\nstr`},
		{"embedded_double_quotes", "`he said \"hi\"`", `he said "hi"`},
		{"embedded_apostrophe", "`it's fine`", `it's fine`},
		{"doubled_backtick", "`a``b`", "a`b"},
		{"multiline", "`multi\nline`", "multi\nline"},
		{"empty", "``", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertStringLiteral(t, c.in, c.want) })
	}
}

// The two quote forms must be interchangeable: a definition author switching
// quotes to avoid escaping must not change the parsed value.
func TestEdgeSingleAndDoubleQuotesAgree(t *testing.T) {
	cases := []struct{ name, body string }{
		{"plain", `plain`},
		{"escaped_newline", `a\nb`},
		{"escaped_tab", `tab\there`},
		{"non_ascii", `é`},
		{"empty", ``},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertSameTree(t, `"`+c.body+`"`, `'`+c.body+`'`)
		})
	}
}

func TestEdgeStringLiteralErrors(t *testing.T) {
	cases := []parseCase{
		{"unknown_escape", `"\q"`, "invalid char escape"}, // a lex error, surfaced as one line
		{"unterminated_backtick", "`unterminated", "not terminated"},
		{"unterminated_single_quote", `'unterminated`, "not terminated"},
		{"raw_newline_in_quoted_string", "\"a\nb\"", "not terminated"}, // a raw newline does not continue a quoted string
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertLexErrorContains(t, c.in, c.want) })
	}
}

// -----------------------------------------------------------------------------
// Object literals
// -----------------------------------------------------------------------------

func TestEdgeObjectLiteralBasics(t *testing.T) {
	cases := []parseCase{
		{"single_key", `{a: 1}`, `{a:1}`},
		{"whitespace_around_colon", `{ a : 1 }`, `{a:1}`},
		{"source_order_preserved", `{a:1,b:2,c:3,d:4,e:5,f:6}`, `{a:1 b:2 c:3 d:4 e:5 f:6}`},
		{"trailing_comma", `{a: 1,}`, `{a:1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

// A quoted key is the escape hatch for anything not a bare name. The StringNode
// unescaping applies, so the key is the decoded text.
func TestEdgeObjectQuotedKeys(t *testing.T) {
	cases := []parseCase{
		{"dashed_and_spaced", `{"dashed-key": 1, "with space": 2}`, `{dashed-key:1 with space:2}`},
		{"accented", `{"ünï": 1}`, `{ünï:1}`},
		{"cjk", `{"日本": 1}`, `{日本:1}`},
		{"single_quoted", `{'single': 1}`, `{single:1}`},
		{"dot_is_not_member_access", `{"a.b": 1}`, `{a.b:1}`},
		{"empty_key", `{"": 1}`, `{:1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

// true/false/null are Identifier tokens, so they are legal bare keys even though
// they are not legal bare expressions.
func TestEdgeObjectKeywordKeys(t *testing.T) {
	assertParses(t, `{true: 1, false: 2, null: 3}`, `{true:1 false:2 null:3}`)
}

// Nesting three deep, and every value position taking a full expression.
func TestEdgeObjectValuesAreFullExpressions(t *testing.T) {
	cases := []parseCase{
		{"nested_three_deep", `{a: {b: {c: 1}}}`, `{a:{b:{c:1}}}`},
		{"ternary", `{a: b ? c : d}`, `{a:(if b c d)}`},
		{"coalesce_with_array", `{a: x ?? [1]}`, `{a:(?? x [1])}`},
		{"lambda_call", `{a: map(xs, x => {n: x.n})}`, `{a:map(xs (\x -> {n:x.n}))}`},
		{"unary_and_parens", `{a: -1, b: !c, c: (1 + 2) * 3}`, `{a:(- 1) b:(! c) c:(* (+ 1 2) 3)}`},
		{"member_index_chain", `{a: input.x[0].y}`, `{a:input.x[0].y}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

// Duplicate detection compares decoded keys, so the quoting of either occurrence
// is irrelevant — this is the case a naive token comparison would miss and let
// two conflicting values through to the object.
func TestEdgeObjectDuplicateKeys(t *testing.T) {
	cases := []parseCase{
		{"second_quoted", `{a: 1, "a": 2}`, `duplicate object key "a"`},
		{"first_quoted", `{"a": 1, a: 2}`, `duplicate object key "a"`},
		{"mixed_quote_forms", `{'a': 1, "a": 2}`, `duplicate object key "a"`},
		{"empty_keys", `{"": 1, "": 2}`, `duplicate object key ""`},
		{"non_adjacent", `{a: 1, b: 2, a: 3}`, `duplicate object key "a"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParseError(t, c.in, c.want) })
	}
}

// A key must be a name or a quoted string; operators and numbers are not.
func TestEdgeObjectKeyMustBeNameOrString(t *testing.T) {
	cases := []parseCase{
		{"bare_comma", `{,}`, "object key must be"},
		{"operator_word_in", `{in: 1}`, "object key must be"}, // `in` lexes as an operator, not an identifier
		{"unary_minus", `{-a: 1}`, "object key must be"},
		{"computed_key", `{[1]: 2}`, "object key must be"}, // no computed keys
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParseError(t, c.in, c.want) })
	}
}

// Structural mistakes.
func TestEdgeObjectStructuralErrors(t *testing.T) {
	cases := []parseCase{
		{"missing_colon", `{a}`, `expected ":"`},
		{"missing_colon_after_quoted_key", `{"k"}`, `expected ":"`},
		{"missing_value", `{a:}`, "unexpected"},
		{"missing_comma", `{a: 1 b: 2}`, `expected ","`},
		{"dash_in_bare_key", `{a-b: 1}`, `expected ":"`}, // a bare key stops at the first non-name char
		{"doubled_comma", `{a: 1,,}`, "object key must be"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParseError(t, c.in, c.want) })
	}
}

// -----------------------------------------------------------------------------
// Array literals
// -----------------------------------------------------------------------------

func TestEdgeArrayLiterals(t *testing.T) {
	cases := []parseCase{
		{"whitespace_only", `[ ]`, `[]`},
		{"nested_empty", `[[]]`, `[[]]`},
		{"mixed_depth", `[[1], [2, [3]]]`, `[[1] [2 [3]]]`},
		{"heterogeneous", `[1, "a", true, null, 1.5]`, `[1 "a" true null 1.5]`}, // a grammar-level yes
		{"objects", `[{a: 1}, {b: 2}]`, `[{a:1} {b:2}]`},
		{"objects_and_arrays_nested", `[{a: [{b: 1}]}]`, `[{a:[{b:1}]}]`},
		{"trailing_comma", `[1, 2,]`, `[1 2]`},
		{"trailing_comma_at_both_levels", `[[1,],]`, `[[1]]`},
		{"operator_elements", `[a ?? b, c ? d : e, -1]`, `[(?? a b) (if c d e) (- 1)]`},
		{"lambda_call_element", `[map(xs, x => x.n)]`, `[map(xs (\x -> x.n))]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

func TestEdgeArrayLiteralErrors(t *testing.T) {
	cases := []parseCase{
		{"leading_comma", `[,]`, "unexpected"}, // a leading comma is not an elision
		{"hole", `[1,,2]`, "unexpected"},       // nor is a hole
		{"missing_comma", `[1 2]`, `expected ","`},
		{"wrong_closer", `[1)`, `expected ","`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParseError(t, c.in, c.want) })
	}
}

// -----------------------------------------------------------------------------
// Lambdas
// -----------------------------------------------------------------------------

func TestEdgeNestedLambdas(t *testing.T) {
	cases := []parseCase{
		// Three levels deep, with the innermost body reaching every enclosing
		// parameter — the whole reason `#` was dropped.
		{
			"three_levels_capture_every_param",
			`map(a, x => map(b, y => map(c, z => x.n + y.n + z.n)))`,
			`map(a (\x -> map(b (\y -> map(c (\z -> (+ (+ x.n y.n) z.n)))))))`,
		},
		// An inner parameter may shadow an outer one: the distinct-name rule is
		// per-lambda, not per-scope-chain.
		{"inner_param_shadows_outer", `map(a, x => map(b, x => x.n))`, `map(a (\x -> map(b (\x -> x.n))))`},
		// A parameter may also shadow a context root; the parser has no opinion.
		{"param_shadows_context_root", `map(a, input => input.n)`, `map(a (\input -> input.n))`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

// The parenthesised single-parameter form is the same node as the bare one.
func TestEdgeLambdaParameterForms(t *testing.T) {
	cases := []parseCase{
		{"parenthesized_single_param", `map(xs, (x) => x)`, `map(xs (\x -> x))`},
		{"index_param", `map(xs, (x, i) => [i, x])`, `map(xs (\x,i -> [i x]))`},
		{"no_spaces", `map(xs,(x,i)=>i)`, `map(xs (\x,i -> i))`}, // arrow detection is by byte adjacency, not spacing
		{"extra_spaces", `map( xs , ( x , i ) => i )`, `map(xs (\x,i -> i))`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

// The body is a full expression: ternaries, object literals and further calls
// all parse without wrapping parentheses.
func TestEdgeLambdaBodyIsFullExpression(t *testing.T) {
	cases := []parseCase{
		{"ternary_of_object_literals", `map(xs, x => x.a ? {y: 1} : {y: 2})`, `map(xs (\x -> (if x.a {y:1} {y:2})))`},
		{"coalesce", `map(xs, x => x.n ?? 0)`, `map(xs (\x -> (?? x.n 0)))`},
		{"unary_and_multiply", `map(xs, x => -x.n * 2)`, `map(xs (\x -> (* (- x.n) 2)))`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

func TestEdgeLambdaLookaheadBacktracks(t *testing.T) {
	cases := []parseCase{
		// A parenthesised expression sharing the lambda prefix must still parse as
		// an expression once the lookahead finds no arrow.
		{"parenthesized_source_arg", `map((a), x => x)`, `map(a (\x -> x))`},
		{"parenthesized_indexed_source_arg", `map((a ?? b)[0], x => x)`, `map((?? a b)[0] (\x -> x))`},
		// Parentheses around the lambda are transparent: the argument is still a
		// LambdaNode, so the lambda-argument check still passes.
		{"parens_around_lambda", `map(xs, (x => x))`, `map(xs (\x -> x))`},
		{"parens_around_two_param_lambda", `map(xs, ((x, i) => i))`, `map(xs (\x,i -> i))`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

func TestEdgeLambdaParameterErrors(t *testing.T) {
	cases := []parseCase{
		// A lambda always binds an element, so a zero-parameter form has nothing to
		// name and is rejected rather than silently accepted and ignored.
		{"zero_params_in_call", `map(xs, () => 1)`, "plain names"},
		{"zero_params_bare", `() => 1`, "plain names"},

		// Only element and index exist; a third parameter would bind nothing.
		{"three_params", `map(xs, (a, b, c) => 1)`, "plain names"},
		{"four_params", `map(xs, (a, b, c, d) => 1)`, "plain names"},

		// Comma bookkeeping: paramNames alternates name/comma exactly.
		{"trailing_comma", `map(xs, (a,) => 1)`, "plain names"},
		{"doubled_comma", `map(xs, (a,,b) => 1)`, "plain names"},
		{"leading_comma", `map(xs, (,a) => 1)`, "plain names"},
		{"missing_comma", `map(xs, (a b) => 1)`, "plain names"},

		// Parameters must be plain names, not patterns or literals.
		{"integer_param", `map(xs, (1) => 1)`, "plain names"},
		{"string_param", `map(xs, ("a") => 1)`, "plain names"},
		{"dotted_param", `map(xs, (a.b) => 1)`, "plain names"},

		// Two parameters with one name would make the index unreadable.
		{"duplicate_names", `map(xs, (i, i) => i)`, "distinct names"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParseError(t, c.in, c.want) })
	}
}

// map's lambda must be argument 2. Argument 1 is the source and is evaluated as
// an ordinary expression.
func TestEdgeLambdaMustBeSecondArgument(t *testing.T) {
	cases := []parseCase{
		{"identifier_second_arg", `map(xs, ys)`, "expects a lambda"},
		{"other_identifier_second_arg", `map(xs, x)`, "expects a lambda"},
		{"array_second_arg", `map(xs, [1])`, "expects a lambda"},
		{"object_second_arg", `map(xs, {a: 1})`, "expects a lambda"},
		{"lambda_as_first_arg", `map(x => x, xs)`, "only valid as the callback argument"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParseError(t, c.in, c.want) })
	}
}

// A lambda is only meaningful as a builtin's lambda argument — the design doc
// says so ("lambda ... only as a `map` argument"), and both consumers assert it:
//
//	internal/expression/eval.go:107   "The parser only accepts a lambda in a
//	                                   builtin's lambda argument, so this is
//	                                   unreachable from parsed source."
//	internal/schema/infer.go:224      same ErrUnsupported fallback
//
// Regression: a lambda is valid only in a builtin's callback slot. This test
// found a real bug and now pins the fix.
//
// parsePrimary used to build a LambdaNode wherever an identifier (or a
// parenthesised parameter list) was followed by `=>`, and only parseCall checked
// position — for the arguments of a *known* builtin. So a lambda anywhere else
// sailed through:
//
//	x => 1              parsed as (\x -> 1)
//	[x => 1]            parsed as [(\x -> 1)]
//	{a: x => 1}         parsed as {a:(\x -> 1)}
//	map(x => x, y => y) parsed — argument 2 is a lambda, so the check passed and
//	                    argument 1 was never examined
//
// The author then got a downstream ErrUnsupported with no source quote and no
// caret, instead of the parse error this package exists to produce — and the
// "unreachable from parsed source" comments in eval.go and infer.go were false.
// The parser now grants lambda permission only in the lambdaArg slot, consumed
// by parsePrimary so it cannot leak into nested expressions; those comments are
// true again.
func TestEdgeLambdaOnlyValidInCallbackSlot(t *testing.T) {
	cases := []struct{ name, in string }{
		{"bare", `x => 1`},
		{"parenthesized_param", `(x) => 1`},
		{"two_params", `(x, i) => 1`},
		{"in_array_literal", `[x => 1]`},
		{"in_object_value", `{a: x => 1}`},
		{"both_call_arguments", `map(x => x, y => y)`},
		{"indexed", `(x => 1)[0]`},
		{"in_arithmetic", `1 + (x => 1)`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertRejected(t, c.in) })
	}
}

// -----------------------------------------------------------------------------
// Postfix chains
// -----------------------------------------------------------------------------

// parsePostfix loops, so any primary can carry an arbitrary accessor chain —
// including a call, an object literal or an array literal, which is what
// distinguishes it from a base-must-be-an-identifier grammar.
func TestEdgePostfixChains(t *testing.T) {
	cases := []parseCase{
		{"index_on_call", `map(xs, x => x)[0]`, `map(xs (\x -> x))[0]`},
		{"member_after_index", `map(xs, x => x)[0].field`, `map(xs (\x -> x))[0].field`},
		{"long_chain_on_call", `map(xs, x => x)[0].a.b[1].c`, `map(xs (\x -> x))[0].a.b[1].c`},
		{"hex_index", `map(xs, x => x)[0x10]`, `map(xs (\x -> x))[16]`}, // any integer literal form indexes
		{"ident_chain", `a.b.c[0].d`, `a.b.c[0].d`},
		{"repeated_index", `a[0][1][2]`, `a[0][1][2]`},
		{"index_on_object_literal", `{a: [1]}[0]`, `{a:[1]}[0]`},
		{"member_on_object_literal", `{a: 1}.a`, `{a:1}.a`},
		{"member_on_array_literal", `[1, 2].length`, `[1 2].length`},
		{"index_on_nested_array_literal", `[[1]][0][0]`, `[[1]][0][0]`},
		{"chain_on_parenthesized_expr", `(a ?? b).c[0]`, `(?? a b).c[0]`},
		{"binds_tighter_than_unary_minus", `-a.b[0]`, `(- a.b[0])`},
		{"binds_tighter_than_not", `!a.b`, `(! a.b)`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

func TestEdgePostfixErrors(t *testing.T) {
	cases := []parseCase{
		// A computed index cannot be type-checked statically, so it is a grammar
		// error rather than a runtime concern.
		{"identifier_index", `a[i]`, "literal integer"},
		{"arithmetic_index", `a[0 + 1]`, `expected "]"`},
		{"negative_index", `a[-1]`, "literal integer"}, // a sign makes it an expression, not a literal
		{"float_index", `a[1.5]`, "literal integer"},
		{"empty_index", `a[]`, "literal integer"},
		{"concatenated_string_index", `a["k" + "j"]`, `expected "]"`}, // a computed key is still computed

		// Slicing is expr-lang syntax with no genroc counterpart.
		{"slice", `a[1:3]`, `expected "]"`},
		{"slice_open_start", `a[:2]`, "literal integer"},

		{"trailing_dot", `a.`, "property name after"},
		{"numeric_property", `a.1`, "unexpected"},
		{"quoted_property", `a."b"`, "property name after"},
		{"optional_chaining", `a?.b`, "unexpected"}, // optional chaining is implicit; `.` on null already yields null
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParseError(t, c.in, c.want) })
	}
}

// -----------------------------------------------------------------------------
// Precedence and associativity
// -----------------------------------------------------------------------------
//
// Every case here would still "work" under a wrong grouping for some inputs, so
// the tree shape is asserted exactly. `1-2-3` is the canonical one: right
// association gives 2 instead of -4, and precedence climbing gets it wrong if the
// recursive call is made with prec rather than prec+1.

// Left associativity of the non-commutative operators.
func TestEdgeLeftAssociativity(t *testing.T) {
	cases := []parseCase{
		{"subtraction", `1 - 2 - 3`, `(- (- 1 2) 3)`},
		{"subtraction_chain", `1 - 2 - 3 - 4`, `(- (- (- 1 2) 3) 4)`},
		{"division", `1 / 2 / 3`, `(/ (/ 1 2) 3)`},
		{"minus_then_plus", `1 - 2 + 3`, `(+ (- 1 2) 3)`}, // equal precedence, still left
		{"plus_then_minus", `1 + 2 - 3`, `(- (+ 1 2) 3)`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

// * / % share a precedence tier and associate left.
func TestEdgeMultiplicativeTier(t *testing.T) {
	cases := []parseCase{
		{"modulo_then_multiply", `10 % 3 * 2`, `(* (% 10 3) 2)`},
		{"multiply_then_modulo", `2 * 10 % 3`, `(% (* 2 10) 3)`},
		{"divide_then_modulo", `8 / 4 % 3`, `(% (/ 8 4) 3)`},
		{"modulo_outranks_plus", `1 + 2 % 3`, `(+ 1 (% 2 3))`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

// Comparison sits below arithmetic, so both sides are computed first.
func TestEdgeComparisonBelowArithmetic(t *testing.T) {
	cases := []parseCase{
		{"sum_on_right", `a < b + 1`, `(< a (+ b 1))`},
		{"arithmetic_on_both_sides", `a + 1 < b * 2`, `(< (+ a 1) (* b 2))`},
		{"difference_on_left", `a - 1 >= b`, `(>= (- a 1) b)`},
		{"equality_with_sum", `a == b + c`, `(== a (+ b c))`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

// && outranks ||, and both associate left.
func TestEdgeLogicalOperators(t *testing.T) {
	cases := []parseCase{
		{"and_before_or", `a && b || c`, `(|| (&& a b) c)`},
		{"or_before_and", `a || b && c`, `(|| a (&& b c))`},
		{"or_is_left_associative", `a || b || c`, `(|| (|| a b) c)`},
		{"and_is_left_associative", `a && b && c`, `(&& (&& a b) c)`},
		{"mixed_chain", `a || b && c || d`, `(|| (|| a (&& b c)) d)`},
		{"comparison_outranks_and", `a == 1 && b != 2`, `(&& (== a 1) (!= b 2))`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

// Unary binds tighter than any binary operator.
func TestEdgeUnaryBindsTightest(t *testing.T) {
	cases := []parseCase{
		{"minus_before_multiply", `-a * b`, `(* (- a) b)`},
		{"minus_before_minus", `-a - b`, `(- (- a) b)`},
		{"not_before_and", `!a && b`, `(&& (! a) b)`},
		{"not_before_equality", `!a == b`, `(== (! a) b)`},
		{"minus_on_right_operand", `a * -b`, `(* a (- b))`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

// Unary chains nest right-to-left, which is the only way they can.
func TestEdgeUnaryChains(t *testing.T) {
	cases := []parseCase{
		{"double_not", `!!a`, `(! (! a))`},
		{"triple_not", `!!!a`, `(! (! (! a)))`},
		{"double_minus", `--a`, `(- (- a))`},
		{"mixed_signs", `-+-a`, `(- (+ (- a)))`},
		{"spaced_minus_not_folded_into_literal", `- -1`, `(- (- 1))`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

// Ternary is right-associative and lower than every binary operator.
func TestEdgeTernaryAssociativity(t *testing.T) {
	cases := []parseCase{
		{"nested_in_else", `a ? b : c ? d : e`, `(if a b (if c d e))`},
		{"three_deep_in_else", `a ? b : c ? d : e ? f : g`, `(if a b (if c d (if e f g)))`},
		{"nested_in_then", `a ? b ? c : d : e`, `(if a (if b c d) e)`},
		{"arithmetic_in_every_slot", `a + 1 ? b * 2 : c - 3`, `(if (+ a 1) (* b 2) (- c 3))`},
		{"logical_condition", `a || b ? c : d`, `(if (|| a b) c d)`},
		{"parenthesized_condition", `(a ? b : c) ? d : e`, `(if (if a b c) d e)`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

// Parentheses override everything, including at depth.
func TestEdgeParenthesesOverridePrecedence(t *testing.T) {
	cases := []parseCase{
		{"both_operands_grouped", `(1 + 2) * (3 - 4)`, `(* (+ 1 2) (- 3 4))`},
		{"redundant_nesting", `((((1))))`, `1`},
		{"or_before_and", `(a || b) && c`, `(&& (|| a b) c)`},
		{"negated_sum", `-(a + b)`, `(- (+ a b))`},
		{"ternary_as_operand", `(a ? b : c) + 1`, `(+ (if a b c) 1)`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

// -----------------------------------------------------------------------------
// ?? rules
// -----------------------------------------------------------------------------
//
// `??` deliberately mirrors expr-lang: precedence 500 (above arithmetic) plus a
// hard ban on mixing it with another operator in the same parseBinary frame.
// The asymmetry is the point — `a + b ?? c` is legal because the `??` is parsed
// inside the recursive call for `+`'s right operand, where prevOp is still empty.

func TestEdgeCoalesceIsLeftAssociative(t *testing.T) {
	assertParses(t, `a ?? b ?? c ?? d`, `(?? (?? (?? a b) c) d)`)
}

// A lower-precedence operator on the left is fine: the ?? lands in the right
// operand's own frame.
func TestEdgeCoalesceInRightOperand(t *testing.T) {
	cases := []parseCase{
		{"after_plus", `a + b ?? c`, `(+ a (?? b c))`},
		{"after_minus", `a - b ?? c`, `(- a (?? b c))`},
		{"after_multiply", `a * b ?? c`, `(* a (?? b c))`},
		{"after_equality", `a == b ?? c`, `(== a (?? b c))`},
		{"after_and", `a && b ?? c`, `(&& a (?? b c))`},
		{"between_two_plusses", `a + b ?? c + d`, `(+ (+ a (?? b c)) d)`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

// Parentheses make either direction expressible.
func TestEdgeCoalesceWithParentheses(t *testing.T) {
	cases := []parseCase{
		{"coalesce_left_of_plus", `(a ?? b) + c`, `(+ (?? a b) c)`},
		{"sum_as_right_operand", `a ?? (b + c)`, `(?? a (+ b c))`},
		{"both_sides_of_equality", `(a ?? b) == (c ?? d)`, `(== (?? a b) (?? c d))`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

// Unary and postfix are not binary operators, so they never trip the rule.
func TestEdgeCoalesceWithUnaryAndPostfix(t *testing.T) {
	cases := []parseCase{
		{"unary_minus_on_left", `-a ?? b`, `(?? (- a) b)`},
		{"unary_minus_on_right", `a ?? -b`, `(?? a (- b))`},
		{"not_on_left", `!a ?? b`, `(?? (! a) b)`},
		{"member_index_chains", `a.b[0] ?? c.d`, `(?? a.b[0] c.d)`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

// Inside a lambda body and inside each ternary branch, where a shared prevOp
// would produce spurious errors.
func TestEdgeCoalesceInNestedFrames(t *testing.T) {
	cases := []parseCase{
		{"lambda_body", `map(xs, x => x.n ?? 0)`, `map(xs (\x -> (?? x.n 0)))`},
		{"lambda_body_chained", `map(xs, x => x.n ?? 0 ?? 1)`, `map(xs (\x -> (?? (?? x.n 0) 1)))`},
		{"call_source_argument", `map(xs ?? [], x => x)`, `map((?? xs []) (\x -> x))`},
		{"ternary_then", `a ? b ?? c : d`, `(if a (?? b c) d)`},
		{"ternary_else", `a ? b : c ?? d`, `(if a b (?? c d))`},
		{"ternary_condition", `a ?? b ? c : d`, `(if (?? a b) c d)`}, // the ternary's `?` is not a binary op
		{"array_elements", `[a ?? b, c ?? d]`, `[(?? a b) (?? c d)]`},
		{"object_value", `{k: a ?? b}`, `{k:(?? a b)}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParses(t, c.in, c.want) })
	}
}

// Anything that would follow a ?? in the same frame is refused, whatever its
// precedence: silently picking a grouping here is exactly the divergence from
// expr-lang the rule exists to prevent.
func TestEdgeCoalesceMixingRejected(t *testing.T) {
	cases := []struct{ name, in string }{
		{"plus", `a ?? b + c`},
		{"minus", `a ?? b - c`},
		{"multiply", `a ?? b * c`},
		{"divide", `a ?? b / c`},
		{"modulo", `a ?? b % c`},
		{"equality", `a ?? b == c`},
		{"inequality", `a ?? b != c`},
		{"less_than", `a ?? b < c`},
		{"and", `a ?? b && c`},
		{"or", `a ?? b || c`},
		{"after_chain_plus", `a ?? b ?? c + d`}, // still caught after several coalesces
		{"after_chain_equality", `a ?? b ?? c == d`},
		{"in_lambda_body", `map(xs, x => x ?? 0 + 1)`},
		{"in_array_literal", `[a ?? b + c]`},
		{"in_object_value", `{k: a ?? b + c}`},
		{"in_ternary_then", `a ? b ?? c + d : e`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertParseError(t, c.in, "cannot be mixed with coalesce")
		})
	}
}

// -----------------------------------------------------------------------------
// Rejected spellings
// -----------------------------------------------------------------------------
//
// Constructs a user might reasonably try, mostly because expr-lang or JavaScript
// has them. Each must fail; the ones with a genroc equivalent must name it.

// expr-lang predicate syntax, in every position it could appear.
func TestEdgeRejectsPredicateSyntax(t *testing.T) {
	cases := []parseCase{
		{"pointer_alone", `#`, "name the parameter"},
		{"pointer_member", `#.n`, "name the parameter"},
		{"pointer_in_map", `map(xs, # > 1)`, "name the parameter"},
		{"leading_dot", `.n`, "name the parameter"},
		{"leading_dot_in_object", `map(xs, {id: .id})`, "name the parameter"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParseError(t, c.in, c.want) })
	}
}

// Unknown and misused builtins. The message lists what does exist.
func TestEdgeRejectsUnknownBuiltins(t *testing.T) {
	cases := []parseCase{
		{"filter_lists_supported", `filter(xs, x => x)`, `supported: map`},
		{"len", `len(xs)`, "unknown function"},
		{"wrong_case", `Map(xs, x => x)`, "unknown function"}, // names are case-sensitive
		{"map_no_args", `map()`, "takes 2 arguments"},
		{"map_one_arg", `map(xs)`, "takes 2 arguments"},
		{"map_three_args", `map(xs, x => x, 3)`, "takes 2 arguments"},
		{"map_trailing_comma", `map(xs, x => x,)`, "unexpected"}, // no trailing comma in an argument list
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParseError(t, c.in, c.want) })
	}
}

// Statements. genroc has none, so `let` and `;` are just stray tokens.
func TestEdgeRejectsStatements(t *testing.T) {
	cases := []parseCase{
		{"let_binding", `let x = 1; x`, "unexpected"},
		{"semicolon_sequence", `1; 2`, "unexpected"},
		{"assignment", `a = b`, "unexpected"},
		{"walrus", `a := b`, "unexpected"},
		{"bare_tuple", `(a, b)`, `expected ")"`}, // a tuple exists only as a parameter list
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParseError(t, c.in, c.want) })
	}
}

// Elvis has a dedicated message because ?? is the intended replacement.
func TestEdgeRejectsElvis(t *testing.T) {
	t.Run("simple", func(t *testing.T) { assertParseError(t, `a ?: b`, "elvis") })
	t.Run("chained", func(t *testing.T) { assertParseError(t, `a ?: b ?: c`, "elvis") })
}

// expr-lang's word operators.
func TestEdgeRejectsWordOperators(t *testing.T) {
	cases := []parseCase{
		{"and", `a and b`, "unexpected"},
		{"or", `a or b`, "unexpected"},
		{"not", `not a`, "unexpected"},
		{"in", `a in b`, "unexpected"},
		{"not_in", `a not in b`, "unexpected"},
		{"matches", `a matches "x"`, "unexpected"},
		{"contains", `a contains "x"`, "unexpected"},
		{"starts_with", `a startsWith "x"`, "unexpected"},
		{"ends_with", `a endsWith "x"`, "unexpected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParseError(t, c.in, c.want) })
	}
}

// Other expr-lang operators with no genroc counterpart.
func TestEdgeRejectsOtherExprLangOperators(t *testing.T) {
	cases := []parseCase{
		{"pipe", `a | b`, "unexpected"},
		{"pipe_into_map", `xs | map(.n)`, "unexpected"},
		{"power_star_star", `2 ** 3`, "unexpected"},
		{"power_caret", `2 ^ 3`, "unexpected"},
		{"range_of_idents", `a..b`, "unexpected"},
		{"range_of_ints", `1..3`, "unexpected"},
		{"optional_chaining", `a?.b`, "unexpected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParseError(t, c.in, c.want) })
	}
}

// Spellings from other languages.
func TestEdgeRejectsOtherLanguageSpellings(t *testing.T) {
	cases := []parseCase{
		{"nil", `nil`, "use null"},
		{"byte_literal", `b'bytes'`, "byte string literals"},
		{"triple_equals", `a === b`, "unexpected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParseError(t, c.in, c.want) })
	}
}

// `=` and `>` only form an arrow when adjacent, so a spaced-out pair must not be
// mistaken for a lambda.
func TestEdgeRejectsSpacedArrow(t *testing.T) {
	t.Run("bare", func(t *testing.T) { assertParseError(t, `x = > 1`, "unexpected") })
	t.Run("in_call", func(t *testing.T) { assertParseError(t, `map(xs, x = > 1)`, `expected ","`) })
}

// Leftover input after a complete expression.
func TestEdgeRejectsLeftoverInput(t *testing.T) {
	cases := []parseCase{
		{"two_ints", `1 2`, "unexpected"},
		{"member_then_ident", `a.b c`, "unexpected"},
		{"two_objects", `{a: 1} {b: 2}`, "unexpected"},
		{"two_calls", `map(xs, x => x) map(ys, y => y)`, "unexpected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertParseError(t, c.in, c.want) })
	}
}

// Truncated and unbalanced input must terminate with an error, not spin in one
// of the `for !p.is(...)` loops and not escape as a raw panic.
func TestEdgeUnbalancedAndEmpty(t *testing.T) {
	cases := []struct{ name, in string }{
		{"empty", ``},
		{"spaces_only", `   `},
		{"newline_and_tab", "\n\t"},

		{"open_paren", `(`},
		{"open_paren_with_operand", `(1`},
		{"unbalanced_nested_parens", `((1)`},
		{"close_paren_only", `)`},
		{"trailing_close_paren", `1)`},

		{"open_bracket", `[`},
		{"open_bracket_with_item", `[1`},
		{"unbalanced_nested_brackets", `[[1]`},
		{"close_bracket_only", `]`},
		{"trailing_close_bracket", `1]`},

		{"open_brace", `{`},
		{"open_brace_with_key", `{a`},
		{"open_brace_with_colon", `{a:`},
		{"open_brace_with_value", `{a: 1`},
		{"close_brace_only", `}`},
		{"trailing_close_brace", `1}`},

		{"call_open_paren", `map(`},
		{"call_one_arg", `map(xs`},
		{"call_trailing_comma", `map(xs,`},
		{"call_param_only", `map(xs, x`},
		{"call_arrow_only", `map(xs, x =>`},
		{"call_body_unclosed", `map(xs, x => x`},

		{"trailing_coalesce", `a ??`},
		{"leading_coalesce", `?? a`},
		{"trailing_plus", `1 +`},
		{"plus_alone", `+`},
		{"question_mark_only", `a ?`},
		{"ternary_without_else", `a ? b`},
		{"ternary_empty_else", `a ? b :`},
		{"trailing_dot", `a.`},
		{"open_index", `a[`},
		{"unclosed_index", `a[0`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertRejected(t, c.in) })
	}
}

// Every prefix of a valid expression is malformed in some way; none may panic,
// hang, or come back as a nil node with a nil error.
func TestEdgeEveryPrefixIsHandled(t *testing.T) {
	cases := []struct{ name, in string }{
		{"lambda_building_object", `map(input.rows, r => {sku: r.code, qty: r.count + 1})`},
		{"nested_literals_with_coalesce", `{a: [1, 2], b: {c: "x"}} ?? null`},
		{"chain_with_ternary", `a.b[0].c ? -1 : (x ?? 2) * 3`},
		{"nested_lambdas", `map(a, x => map(b, (y, i) => [x.n, y.m, i]))`},
		{"trailing_commas_and_equality", `[1, 2,] == {k: 'v',}`},
		{"logical_with_hex", `!input.flag && (outputs.n ?? 0) >= 0x10`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertEveryPrefixHandled(t, c.in) })
	}
}

// -----------------------------------------------------------------------------
// Error quality
// -----------------------------------------------------------------------------

// The point of owning the parser is that a registration error quotes the
// expression the author wrote. Assert the source appears verbatim on its own
// line, with no rewriting (the `let` translation used by the conformance oracle
// must never leak) and no truncation.
func TestEdgeErrorQuotesOriginalSource(t *testing.T) {
	cases := []struct{ name, in string }{
		{"missing_object_value_in_lambda", `map(input.rows, r => {sku: r.code, qty: })`},
		{"duplicate_object_key", `{name: input.user.name, name: 2}`},
		{"coalesce_mixed_with_plus", `outputs.step_one ?? [] + 1`},
		{"unknown_function", `filter(input.items, x => x.n)`},
		{"computed_index", `input.items[key]`},
		{"elvis", `config.timeout ?: 30`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertErrorQuotesSource(t, c.in) })
	}
}

// The caret column is the part most likely to rot: it comes from a token offset,
// so any change to lookahead (which token is blamed) moves it silently. `want`
// is the 0-based byte offset into the source that the caret sits under.
func TestEdgeErrorCaretColumn(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
		why  string
	}{
		//                                      0123456789...
		{"leftover_token", `a b`, 2, "the leftover token, not the end of input"},
		{"stray_semicolon", `1;2`, 1, "the stray separator"},
		{"nil_keyword", `nil`, 0, "the whole identifier"},
		{"pointer", `#`, 0, "the pointer"},
		{"leading_dot", `.field`, 0, "the leading dot"},
		{"byte_literal", `b'bytes'`, 0, "the byte literal"},
		{"elvis", `a ?: b`, 3, "the ':' that follows '?'"},
		{"computed_index", `a[b]`, 2, "the computed index, not the bracket"},
		{"slice", `a[1:3]`, 3, "the ':' where ']' was expected"},
		{"unsupported_operator", `2 ** 3`, 2, "the unsupported operator"},
		{"word_operator_after_literal", `[1, 2] and x`, 7, "the word operator after a complete literal"},
		{"object_missing_comma", `{a: 1 b: 2}`, 6, "the key that needed a comma before it"},
		{"duplicate_object_key", `{a: 1, a: 2}`, 7, "the second occurrence of the duplicate key"},
		{"coalesce_mixing", `a ?? b + c`, 7, "the operator that may not be mixed with ??"},
		{"unknown_function", `foo(1)`, 0, "the unknown function name"},
		{"missing_argument", `map(xs)`, 6, "the closing paren, where the missing argument belonged"},
		{"missing_lambda", `map(xs, ys)`, 10, "the closing paren of the call missing its lambda"},
		{"truncated_lambda_body", `map(a, x => x.n + )`, 18, "the paren that cut the expression short"},
		{"offset_after_long_prefix", `map(xs, x => x.n) + nil`, 20, "offsets still track after a long prefix"},
		{"coalesce_mixing_deep_in_source", `map(input.items, x => x.n) ?? 1 + 2`, 32, "the '+' after a coalesce, deep into the source"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertCaretAt(t, c.in, c.want, c.why) })
	}
}

// A caret that lands past the end of the source would print a ragged line, so
// failAt clamps it. Errors blamed on end-of-input are the ones that can get
// there.
func TestEdgeCaretStaysWithinSource(t *testing.T) {
	cases := []struct{ name, in string }{
		{"trailing_plus", `1 +`},
		{"trailing_coalesce", `a ??`},
		{"trailing_dot", `a.`},
		{"unclosed_call", `map(xs, x => x`},
		{"unclosed_object", `{a: 1`},
		{"unclosed_array", `[1`},
		{"unclosed_paren", `(1`},
		{"ternary_empty_else", `a ? b :`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertCaretWithinSource(t, c.in) })
	}
}

// A lexer failure arrives as a multi-line expr-lang dump; Parse keeps only the
// first line, so the caret-form assertions above do not apply to it and, more to
// the point, an API response never carries expr-lang's own source rendering.
func TestEdgeLexerErrorIsSingleLine(t *testing.T) {
	cases := []struct{ name, in string }{
		{"unterminated_double_quote", `"unterminated`},
		{"at_sign", `@`},
		{"tilde", `~a`},
		{"unterminated_backtick", "`open"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertSingleLineError(t, c.in) })
	}
}
