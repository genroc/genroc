package expressiontest

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"genroc/internal/expression"
	"genroc/internal/expression/syntax"
)

// Runtime-evaluation edge cases through expression.Eval with a hand-built context — also how a
// stale definition reaches the evaluator with a shape inference never approved. Results are
// cross-checked against real expr-lang; deliberate divergences say so and skip the oracle.

// ---- ?? distinguishes null from falsy ----

// The classic way to break ??: implement it as "left if truthy". Every falsy but
// non-null value must survive, or `?? 0` style defaults silently overwrite real
// data (a false flag becoming true, an empty list becoming a populated one).
// expr-lang agrees on all of these, so the oracle pins it too.
func TestEvalEdge_CoalesceKeepsFalsyNonNullLeft(t *testing.T) {
	edgeOracleAll(t, []edgePair{
		{"false", `false ?? true`, `false ?? true`},
		{"zero", `0 ?? 9`, `0 ?? 9`},
		{"zero_float", `0.0 ?? 9`, `0.0 ?? 9`},
		{"empty_string", `"" ?? "x"`, `"" ?? "x"`},
		{"empty_array", `[] ?? [1]`, `[] ?? [1]`},
		{"false_from_context", `no ?? true`, `no ?? true`},
	})
}

// Checked exactly as well: the oracle compares JSON, which cannot tell `false`
// from a boxed false coming back as something else.
func TestEvalEdge_CoalesceKeepsFalsyNonNullLeftExactly(t *testing.T) {
	edgeExactAll(t, []edgeValueCase{
		{"false", `false ?? true`, false},
		{"zero", `0 ?? 9`, 0},
		{"empty_string", `"" ?? "x"`, ""},
		{"empty_array", `[] ?? [1]`, []any{}},
	})
}

// An explicit null field still coalesces; only null does.
func TestEvalEdge_CoalesceOnNullFieldTakesRight(t *testing.T) {
	edgeExact(t, evalOK(t, `nullmid.b ?? "dflt"`, edgeEnv), "dflt")
}

// A null root value coalesces the same way.
func TestEvalEdge_CoalesceOnNullRootTakesRight(t *testing.T) {
	edgeExact(t, evalOK(t, `nul ?? 1`, edgeEnv), 1)
}

// ---- short-circuit proofs ----

// Guards the short-circuit tests below: if either of these ever stopped
// erroring, they would pass without proving anything.
func TestEvalEdge_BoomReallyErrors(t *testing.T) {
	edgeErrAll(t, []edgeCase{
		{"absent_identifier", edgeBoom},
		{"division_by_zero", `1 / 0`},
	})
}

func TestEvalEdge_FalseAndSkipsRightOperand(t *testing.T) {
	edgeExact(t, evalOK(t, `false && `+edgeBoom, edgeEnv), false)
}

func TestEvalEdge_TrueOrSkipsRightOperand(t *testing.T) {
	edgeExact(t, evalOK(t, `true || `+edgeBoom, edgeEnv), true)
}

func TestEvalEdge_NonNullCoalesceSkipsRightOperand(t *testing.T) {
	edgeExact(t, evalOK(t, `1 ?? `+edgeBoom, edgeEnv), 1)
}

// A false left operand of ?? is non-null, so ?? must also short-circuit here —
// the falsy/null bug would surface as an error, not a wrong value.
func TestEvalEdge_FalseCoalesceSkipsRightOperand(t *testing.T) {
	edgeExact(t, evalOK(t, `false ?? `+edgeBoom, edgeEnv), false)
}

// Same with a division-by-zero right side, which errors for a different reason
// than an unbound name.
func TestEvalEdge_FalseAndSkipsDividingRightOperand(t *testing.T) {
	edgeExact(t, evalOK(t, `false && (1 / 0 == 0)`, edgeEnv), false)
}

func TestEvalEdge_TrueOrSkipsDividingRightOperand(t *testing.T) {
	edgeExact(t, evalOK(t, `true || (1 / 0 == 0)`, edgeEnv), true)
}

// The untaken branch of a ternary is never evaluated either, so a guard like
// `x != null ? x.n : 0` cannot be defeated by the branch it is guarding.
func TestEvalEdge_ConditionalSkipsUntakenBranch(t *testing.T) {
	edgeExactAll(t, []edgeValueCase{
		{"true_skips_else", `true ? 1 : ` + edgeBoom, 1},
		{"false_skips_then", `false ? ` + edgeBoom + ` : 2`, 2},
	})
}

// Both operands of a non-short-circuit operator are evaluated, so an error on
// the right must still surface (the mirror image of the tests above).
func TestEvalEdge_NonShortCircuitPropagatesRightError(t *testing.T) {
	edgeErrAll(t, []edgeCase{
		{"addition", `1 + ` + edgeBoom},
		{"and_with_true_left", `true && ` + edgeBoom},
		{"or_with_false_left", `false || ` + edgeBoom},
		{"coalesce_with_null_left", `null ?? ` + edgeBoom},
	})
}

// ---- null propagation ----

// Optional-chaining semantics: a chain that hits null, a missing key, or a
// non-object keeps returning null instead of erroring, so a partially populated
// context degrades to a default rather than failing the whole tick. This is a
// deliberate divergence from expr-lang, which errors on every case here.
func TestEvalEdge_MemberChainYieldsNull(t *testing.T) {
	edgeNullAll(t, []edgeCase{
		{"missing_leaf", `deep.b.c.missing`},
		{"missing_intermediate", `deep.b.missing.d`}, // chain continues
		{"null_intermediate", `nullmid.b.c.d`},       // explicit null intermediate
		{"scalar_intermediate", `scalar.b.c.d`},
		{"array_intermediate", `arrmid.b.c.d`}, // member access on an array
		{"string_intermediate", `strmid.b.c.d`},
		{"null_root", `nul.a.b.c`},
		{"member_on_number", `num.x`},
		{"no_host_object_properties", `str.length`},
		{"member_on_array_not_elementwise", `xs.n`},
		{"one_step_past_the_leaf", `deep.b.c.d.deeper`},
	})
}

// The chain still resolves when every step exists.
func TestEvalEdge_MemberChainResolvesWhenComplete(t *testing.T) {
	edgeExact(t, evalOK(t, `deep.b.c.d`, edgeEnv), "leaf")
}

// ?? recovers a broken chain — the idiom this behaviour exists to support.
func TestEvalEdge_MemberChainRecoveredByCoalesce(t *testing.T) {
	edgeExact(t, evalOK(t, `deep.b.missing.d ?? "dflt"`, edgeEnv), "dflt")
}

func TestEvalEdge_IndexYieldsNull(t *testing.T) {
	edgeNullAll(t, []edgeCase{
		{"into_null", `nul[0]`},
		{"into_number", `num[0]`},
		{"into_object", `obj[0]`},
		{"into_string", `str[0]`}, // no byte/char indexing (expr-lang gives 115)
		{"out_of_bounds_by_one", `ys[2]`},
		{"far_out_of_bounds", `ys[99]`},
		{"any_index_into_empty_array", `empty[0]`},
		{"member_access_continues_off_the_null", `xs[9].n`},
		{"out_of_bounds_mid_chain", `arrmid.b[7]`},
	})
}

func TestEvalEdge_IndexInBoundsReturnsElement(t *testing.T) {
	edgeExactAll(t, []edgeValueCase{
		{"element", `ys[1]`, 20},
		{"member_of_element", `xs[1].n`, 2},
	})
}

// The parser rejects `xs[-1]`, but IndexNode is a plain struct: template code or
// a future parser change could hand the evaluator a negative index. It must
// yield null like any other out-of-range index rather than panic on a negative
// slice offset.
func TestEvalEdge_NegativeIndexFromASTYieldsNull(t *testing.T) {
	node := &syntax.IndexNode{Base: &syntax.IdentNode{Name: "ys"}, Index: -1}
	got, err := expression.EvalNode(node, edgeEnv)
	if err != nil {
		t.Fatalf("EvalNode(ys[-1]): %v", err)
	}
	if got != nil {
		t.Errorf("ys[-1] = %#v, want nil", got)
	}
}

// A missing root is an error while a missing *field* is null: the two are not
// interchangeable, because a typo'd root name is a definition bug that should be
// loud, whereas an absent field is normal for a partially populated context.
func TestEvalEdge_MissingRootErrors(t *testing.T) {
	edgeErrContains(t, edgeBoom, "not found in context")
}

func TestEvalEdge_MissingFieldIsNull(t *testing.T) {
	assertEq(t, evalOK(t, `deep.nope`, edgeEnv), nil)
}

// A nil context map is not the same as a missing root either — lookup must not
// panic on it.
func TestEvalEdge_NilContextErrorsWithoutPanicking(t *testing.T) {
	evalErr(t, `anything`, nil)
}

// ---- arithmetic ----

func TestEvalEdge_ArithmeticMatchesExprLang(t *testing.T) {
	edgeOracleAll(t, []edgePair{
		{"int_plus_int", `1 + 1`, `1 + 1`},
		{"int_plus_float", `1 + 1.5`, `1 + 1.5`},
		{"fractional_quotient", `10 / 4`, `10 / 4`},
		{"whole_quotient", `6 / 3`, `6 / 3`},
		{"modulo", `7 % 2`, `7 % 2`},
		{"modulo_negative_left", `-7 % 2`, `-7 % 2`},
		{"subtraction_below_zero", `2 - 5`, `2 - 5`},
		{"product_with_negative", `3 * -2`, `3 * -2`},
		{"string_concat", `"a" + "b"`, `"a" + "b"`},
		{"int_equals_float", `1 == 1.0`, `1 == 1.0`},
		{"int_equals_string", `1 == "1"`, `1 == "1"`},
		{"int_plus_int64_from_context", `num + i64`, `num + i64`},
		{"float_times_int", `flt * 2`, `flt * 2`},
	})
}

// int+int stays int; anything touching a float becomes float. Templates render straight into
// JSON, so a silent widening turns `qty: 2` into 2.0-shaped output and an integer-typed
// downstream schema rejects it. Arithmetic is exact base-10 (0.1+0.2, 1.1*1.1).
func TestEvalEdge_ArithmeticIsExactDecimal(t *testing.T) {
	edgeDecimalAll(t, []edgeDecCase{
		{"sum", `1 + 2`, "3"},
		{"difference", `2 - 5`, "-3"},
		{"product", `3 * 4`, "12"},
		{"modulo", `7 % 2`, "1"},
		{"int_plus_float", `1 + 0.5`, "1.5"},
		{"tenths_sum", `0.1 + 0.2`, "0.3"},
		{"tenths_product", `1.1 * 1.1`, "1.21"},
		// A whole result is presented in its short form regardless of how it arose;
		// whether it type-checks as integer or number is decided by inference.
		{"whole_result_from_float_sum", `2.0 + 1`, "3"},
		{"whole_result_from_float_product", `1.5 * 2`, "3"},
		// int64 and float32 arrive from JSON decoding and DB scans.
		{"int64_from_context", `i64 + 1`, "8"},
		{"float32_from_context", `f32 + 1`, "2.5"},
	})
}

// The equality that a float64 pipeline famously gets wrong.
func TestEvalEdge_ExactArithmeticComparesEqual(t *testing.T) {
	assertEq(t, evalOK(t, `0.1 + 0.2 == 0.3`, edgeEnv), true)
}

// And the matching strict inequality: the sum is not *above* 0.3 either.
func TestEvalEdge_ExactArithmeticIsNotGreater(t *testing.T) {
	assertEq(t, evalOK(t, `0.1 + 0.2 > 0.3`, edgeEnv), false)
}

// Division is exact where the quotient terminates, and rounds at a documented
// precision where it cannot. It still types as `number` whatever the runtime
// values, so `a / b` has one static type.
func TestEvalEdge_DivisionIsExactWhereItTerminates(t *testing.T) {
	edgeDecimalAll(t, []edgeDecCase{
		{"whole_quotient", `6 / 3`, "2"},
		{"fractional_quotient", `10 / 4`, "2.5"},
		{"eighth", `1 / 8`, "0.125"},
		{"int64_operands", `i64 / i64`, "1"},
	})
}

// Non-terminating: rounded to the division context's 34 significant digits
// rather than erroring, which would be surprising for plain 10/3.
func TestEvalEdge_DivisionRoundsNonTerminatingQuotient(t *testing.T) {
	n, ok := evalOK(t, `10 / 3`, edgeEnv).(json.Number)
	if !ok || !strings.HasPrefix(n.String(), "3.33333333333333333333") {
		t.Errorf("10/3 = %#v, want a 34-digit rounded 3.333…", n)
	}
}

func TestEvalEdge_DivisionByZeroErrors(t *testing.T) {
	evalErr(t, `1 / 0`, edgeEnv)
}

// Unary minus preserves int-ness the same way binary arithmetic does.
func TestEvalEdge_UnaryMinusIsExactDecimal(t *testing.T) {
	edgeDecimalAll(t, []edgeDecCase{
		{"int_literal", `-5`, "-5"},
		{"float_literal", `-2.5`, "-2.5"},
		{"float_from_context", `-flt`, "-2.5"},
		{"int64_from_context", `-i64`, "-7"},
		{"double_negation", `- -5`, "5"},
	})
}

// Unary plus is a no-op that still type-checks its operand.
func TestEvalEdge_UnaryPlusIsANoOp(t *testing.T) {
	edgeExact(t, evalOK(t, `+num`, edgeEnv), 5)
}

func TestEvalEdge_DoubleNotIsIdentityOnBool(t *testing.T) {
	edgeExact(t, evalOK(t, `!!true`, edgeEnv), true)
}

func TestEvalEdge_UnaryTypeErrors(t *testing.T) {
	edgeErrAll(t, []edgeCase{
		{"plus_on_string", `+str`},
		{"minus_on_null", `-nul`},
		{"not_on_number", `!num`},
		{"not_on_null", `!nul`},
	})
}

// `%` is gated statically, not dynamically: inference requires both operands to type as
// integer, so `7 % 2.0` is rejected at registration while evaluation accepts it (2.0 is
// whole). The runtime being the more permissive side is the safe direction.
func TestEvalEdge_ModuloIsGatedByInferenceNotRuntime(t *testing.T) {
	edgeDecimal(t, evalOK(t, `7 % 2.0`, edgeEnv), "1")
	inferErr(t, `7 % 2.0`, ctx(t, `{"type":"object"}`), "requires integer operands")
}

func TestEvalEdge_ArithmeticTypeErrors(t *testing.T) {
	edgeErrAll(t, []edgeCase{
		{"string_plus_number", `"a" + 1`}, // no implicit stringification
		{"number_plus_string", `1 + "a"`},
		{"string_plus_null", `str + nul`}, // a null operand is not an empty string
		{"null_plus_number", `nul + 1`},
		{"bool_plus_number", `true + 1`},          // booleans are not numbers
		{"string_minus_string", `"a" - "b"`},      // - has no string form
		{"array_plus_array", `ys + ys`},           // arrays do not concatenate
		{"object_plus_object", `obj + obj`},       // objects do not merge
		{"modulo_float_left", `2.5 % 2`},          // % is integer-only...
		{"modulo_string_left", `str % 2`},         // ...on both sides
		{"compare_number_to_string", `num < str`}, // comparison needs two numbers
		{"compare_string_to_string", `str < str`}, // including two strings
		{"compare_bool_to_bool", `no < true`},
		{"compare_null_to_number", `nul < 1`},
		{"divide_by_zero", `1 / 0`}, // an error, not +Inf (expr-lang gives +Inf)
		{"divide_by_zero_float", `1 / 0.0`},
		{"divide_by_computed_zero", `num / (2-2)`},
		{"modulo_by_zero", `1 % 0`}, // distinct message
	})
}

// The two zero-divisor errors are distinguishable, so a log names the operator
// that actually failed.
func TestEvalEdge_DivisionByZeroErrorNamesTheOperator(t *testing.T) {
	edgeErrContains(t, `1 / 0`, "division by zero")
}

func TestEvalEdge_ModuloByZeroErrorNamesTheOperator(t *testing.T) {
	edgeErrContains(t, `1 % 0`, "modulo by zero")
}

// Equality is defined across types (unlike ordering, which requires numbers):
// mismatched types compare unequal instead of erroring, so `x == null` and
// `x == "done"` work on a nullable field.
func TestEvalEdge_EqualityAcrossTypes(t *testing.T) {
	edgeExactAll(t, []edgeValueCase{
		{"number_vs_string", `1 == "1"`, false},
		{"null_vs_zero", `nul == 0`, false},
		{"null_vs_false", `nul == false`, false},
		{"null_vs_empty_string", `nul == ""`, false},
		{"null_vs_null", `nul == null`, true},
		{"array_vs_null", `ys == null`, false},
		{"array_not_null", `ys != null`, true},
		{"int_vs_float", `num == 5.0`, true}, // numeric equality ignores int vs float
		{"int64_vs_int", `i64 == 7`, true},
		{"float32_vs_float", `f32 == 1.5`, true},
		{"bool_vs_bool", `no == false`, true},
		{"string_vs_string", `str == "abc"`, true},
		{"string_vs_other_string", `"a" != "b"`, true},
		{"null_field_vs_null", `nullmid.b == null`, true},
		{"missing_field_vs_null", `deep.gone == null`, true}, // a missing field equals null
	})
}

// Integers beyond 2^53 survive arithmetic. Under the old float64 pipeline 9007199254740993
// came back as …992, silently corrupting any id-sized value.
func TestEvalEdge_LargeIntArithmeticIsExact(t *testing.T) {
	edgeDecimalAll(t, []edgeDecCase{
		{"plus_zero", `9007199254740993 + 0`, "9007199254740993"},
		{"plus_two", `9007199254740993 + 2`, "9007199254740995"},
	})
}

// ---- conditional condition ----

// A non-boolean or null condition takes the else branch silently: mustBool
// (ops.go) type-asserts and discards the failure, unlike !, && and || which all
// error on a non-boolean operand. Pinned because the asymmetry is invisible from
// the call site — `flag ? a : b` on a null flag quietly yields b.
func TestEvalEdge_NonBooleanConditionTakesElseBranch(t *testing.T) {
	edgeExactAll(t, []edgeValueCase{
		{"number", `1 ? "y" : "n"`, "n"},
		{"string", `"x" ? "y" : "n"`, "n"},
		{"null_literal", `null ? "y" : "n"`, "n"},
		{"null_root", `nul ? "y" : "n"`, "n"},
		{"null_field", `nullmid.b ? "y" : "n"`, "n"},
		{"array", `ys ? "y" : "n"`, "n"},
	})
}

// ---- map: runtime results ----

func TestEvalEdge_MapMatchesExprLang(t *testing.T) {
	edgeOracleAll(t, []edgePair{
		// Empty source: an empty array, not null and not an error.
		{"empty_source", `map(empty, x => x.n)`, `map(empty, {let x = #; x.n})`},
		// Single element.
		{"single_element", `map(one, x => x.n * 2)`, `map(one, {let x = #; x.n * 2})`},
		// Body shapes: object, array, null, nested object.
		{"object_body", `map(xs, x => {id: x.id})`, `map(xs, {let x = #; {id: x.id}})`},
		{"array_body", `map(xs, x => [x.n, x.id])`, `map(xs, {let x = #; [x.n, x.id]})`},
		{"null_body", `map(xs, x => null)`, `map(xs, {let x = #; null})`},
		{"nested_object_body", `map(xs, x => {a: {b: [x.n]}})`, `map(xs, {let x = #; {a: {b: [x.n]}}})`},
		// The index parameter is 0-based and advances per element.
		{"index_param", `map(ys, (y, i) => i)`, `map(ys, {let y = #; let i = #index; i})`},
		{"index_param_with_element", `map(xs, (x, i) => {i: i, n: x.n})`, `map(xs, {let x = #; let i = #index; {i: i, n: x.n}})`},
		{"index_param_over_empty_source", `map(empty, (x, i) => i)`, `map(empty, {let x = #; let i = #index; i})`},
		// Nested map capturing the outer element in the inner body.
		{"nested_map_captures_outer", `map(xs, x => map(ys, y => x.n + y))`, `map(xs, {let x = #; map(ys, {let y = #; x.n + y})})`},
		// Both index parameters visible at once, inner shadowing nothing.
		{"two_index_params", `map(xs, (x, i) => map(ys, (y, j) => i * 10 + j))`,
			`map(xs, {let x = #; let i = #index; map(ys, {let y = #; let j = #index; i * 10 + j})})`},
		// Map result indexed straight away, and a map over a map result.
		{"result_indexed", `map(xs, x => x.n)[1]`, `map(xs, {let x = #; x.n})[1]`},
		{"map_over_map_result", `map(map(xs, x => {v: x.n}), y => y.v + 1)`,
			`map(map(xs, {let x = #; {v: x.n}}), {let y = #; y.v + 1})`},
		// Conditional and ?? inside a body.
		{"conditional_body", `map(xs, x => x.n > 1 ? x.id : "small")`, `map(xs, {let x = #; x.n > 1 ? x.id : "small"})`},
		// A body that ignores its parameter still runs once per element.
		{"body_ignoring_the_parameter", `map(xs, x => 1)`, `map(xs, {let x = #; 1})`},
	})
}

// An empty source yields an empty array, never null: `map(input.opt ?? [], …)`
// is the documented idiom, and a null result would break every consumer of it.
func TestEvalEdge_MapEmptySourceIsEmptyArray(t *testing.T) {
	got := evalOK(t, `map(empty, x => x.n)`, edgeEnv)
	arr, ok := got.([]any)
	if !ok {
		t.Fatalf("map over an empty source returned %#v (%T), want []any", got, got)
	}
	if arr == nil || len(arr) != 0 {
		t.Errorf("map over an empty source = %#v, want a non-nil empty slice", arr)
	}
	edgeJSON(t, `map(empty, x => x)`, `[]`)
}

// The `?? []` idiom: a null source becomes an empty source, and a present one is
// used as is (an empty array is non-null, so it wins over the default rather
// than being replaced by it).
func TestEvalEdge_MapCoalescedSource(t *testing.T) {
	edgeJSONAll(t, []edgeJSONCase{
		{"null_source_takes_the_default", `map(nul ?? [1], x => x)`, `[1]`},
		{"empty_source_beats_the_default", `map(empty ?? [1], x => x)`, `[]`},
	})
}

// Elements that are themselves null: member access inside the body follows the
// same optional-chaining rule as anywhere else, so one hole does not fail the
// whole map. expr-lang errors here ("cannot fetch n from <nil>"), so this is a
// deliberate divergence and has no oracle.
func TestEvalEdge_MapOverArrayWithNullElements(t *testing.T) {
	edgeJSONAll(t, []edgeJSONCase{
		{"member_of_null_element", `map(holes, x => x.n)`, `[null, 1]`},
		{"member_of_null_element_coalesced", `map(holes, x => x.n ?? 0)`, `[0, 1]`},
		{"null_element_inside_object_body", `map(holes, x => {v: x.n})`, `[{"v": null}, {"v": 1}]`},
		// The element itself, null included, round-trips unchanged.
		{"null_element_round_trips", `map(holes, x => x)`, `[null, {"n": 1}]`},
	})
}

// An error inside the body aborts the whole map. Swallowing it (yielding null
// for the failing element) would turn a definition bug into silently wrong data
// spread across an array.
func TestEvalEdge_MapBodyErrorPropagates(t *testing.T) {
	edgeErrAll(t, []edgeCase{
		{"absent_identifier", `map(xs, x => ` + edgeBoom + `)`},
		{"division_by_zero", `map(xs, x => x.n / 0)`},
		{"string_plus_number", `map(xs, x => x.id + 1)`},
		{"not_on_number", `map(xs, x => !x.n)`},
		{"error_in_a_nested_body", `map(xs, x => map(ys, y => ` + edgeBoom + `))`},
		{"error_in_an_object_value", `map(xs, x => {k: ` + edgeBoom + `})`},
	})
}

// Only the last element errors — the earlier successful elements must not mask
// it.
func TestEvalEdge_MapBodyErrorOnTheLastElementPropagates(t *testing.T) {
	evalErr(t, `map(ys, y => 10 / (y - 20))`, edgeEnv)
}

// ---- map: scoping ----

// A parameter shadows a context root only inside the lambda; the root is intact
// in the rest of the expression. Shadowing that leaked outward would corrupt any
// sibling reference to `input`/`outputs`.
func TestEvalEdge_MapParamShadowsRootOnlyInsideBody(t *testing.T) {
	edgeOracle(t,
		`{shadowed: map(xs, ys => ys.n), root: ys}`,
		`{shadowed: map(xs, {let ys = #; ys.n}), root: ys}`)
}

// An inner lambda parameter shadows the outer one, and the outer binding is
// restored for the rest of the outer body. env.bind copies rather than mutates
// precisely so the inner binding cannot survive past the inner map.
func TestEvalEdge_MapInnerParamShadowsOuterParam(t *testing.T) {
	edgeOracleAll(t, []edgePair{
		{"same_name_rebound", `map(xs, x => map(ys, x => x))`, `map(xs, {let x = #; map(ys, {let x = #; x})})`},
		{"outer_binding_restored_after_inner_map", `map(xs, x => {inner: map(ys, x => x), outer: x.n})`,
			`map(xs, {let x = #; {inner: map(ys, {let x = #; x}), outer: x.n}})`},
		{"index_param_rebound", `map(xs, (x, i) => map(ys, (y, i) => i))`,
			`map(xs, {let x = #; let i = #index; map(ys, {let y = #; let i = #index; i})})`},
	})
}

// Cross-element leakage guard: every inner iteration must see the *current*
// outer element. If bind mutated a shared map, later elements would overwrite
// the binding earlier iterations still reference and every row would collapse to
// the last one.
func TestEvalEdge_MapNoCrossElementLeakage(t *testing.T) {
	edgeOracle(t,
		`map(xs, x => map(ys, y => x.n))`,
		`map(xs, {let x = #; map(ys, {let y = #; x.n})})`)
	edgeJSON(t, `map(xs, x => map(ys, y => x.n))`, `[[1, 1], [2, 2]]`)
}

// Three levels deep, with the outermost element read at the innermost level.
func TestEvalEdge_MapThreeLevelsDeepKeepsEveryBinding(t *testing.T) {
	edgeJSON(t, `map(xs, x => map(ys, y => map(one, z => {n: x.n, y: y, z: z.n})))`,
		`[[[{"n":1,"y":10,"z":7}], [{"n":1,"y":20,"z":7}]],
		  [[{"n":2,"y":10,"z":7}], [{"n":2,"y":20,"z":7}]]]`)
}

// The index parameters of both levels stay independent.
func TestEvalEdge_MapNestedIndexParamsStayIndependent(t *testing.T) {
	edgeJSON(t, `map(xs, (x, i) => map(ys, (y, j) => i * 10 + j))`, `[[0, 1], [10, 11]]`)
}

// ---- map: source errors ----

// Inference rejects a null or non-array source at registration, so these only
// fire for a hand-built context or a definition registered before the check.
// They must be errors rather than a silent empty array: a null source usually
// means an upstream task produced nothing, which is worth failing on.
func TestEvalEdge_MapNullSourceErrors(t *testing.T) {
	edgeErrContainsAll(t, []edgeCase{
		{"null_root", `map(nul, x => x)`},
		{"null_field", `map(nullmid.b, x => x)`},
	}, "null")
}

// A missing field is null, so it fails the same way rather than mapping over
// nothing.
func TestEvalEdge_MapMissingFieldSourceErrors(t *testing.T) {
	evalErr(t, `map(deep.nope, x => x)`, edgeEnv)
}

func TestEvalEdge_MapNonArraySourceErrors(t *testing.T) {
	edgeErrContainsAll(t, []edgeCase{
		{"number", `map(num, x => x)`},
		{"float", `map(flt, x => x)`},
		{"string", `map(str, x => x)`}, // string is not a sequence here
		{"object", `map(obj, x => x)`}, // object is not iterated by value
		{"boolean", `map(no, x => x)`},
		{"object_literal", `map({a: 1}, x => x)`},
	}, "must be an array")
}

// The source expression is evaluated before the element loop, so an error in it
// surfaces unchanged.
func TestEvalEdge_MapSourceExpressionErrorPropagates(t *testing.T) {
	evalErr(t, `map(`+edgeBoom+`, x => x)`, edgeEnv)
}

// ---- object and array literals ----

func TestEvalEdge_ObjectLiteralEval(t *testing.T) {
	edgeOracleAll(t, []edgePair{
		{"empty", `{}`, `{}`},
		{"scalar_values", `{a: 1, b: "s", c: true, d: null}`, `{a: 1, b: "s", c: true, d: null}`},
		{"nested", `{a: {b: {c: [1, null]}}}`, `{a: {b: {c: [1, null]}}}`},
		{"quoted_and_plain_keys", `{"dashed-key": 1, plain: 2}`, `{"dashed-key": 1, plain: 2}`},
		// A value that is a map call, and one that is a broken chain.
		{"map_call_as_value", `{items: map(xs, x => x.id), n: 1}`, `{items: map(xs, {let x = #; x.id}), n: 1}`},
	})
}

// Missing fields become explicit nulls rather than absent keys, so the rendered
// JSON shape does not change with the data.
func TestEvalEdge_ObjectLiteralMissingFieldIsAnExplicitNull(t *testing.T) {
	edgeJSON(t, `{present: deep.b.c.d, absent: deep.nope}`, `{"present": "leaf", "absent": null}`)
}

// An empty object literal is an empty map, not nil.
func TestEvalEdge_EmptyObjectLiteralIsAnEmptyMap(t *testing.T) {
	got := evalOK(t, `{}`, edgeEnv)
	if m, ok := got.(map[string]any); !ok || m == nil || len(m) != 0 {
		t.Errorf("{} = %#v (%T), want an empty map", got, got)
	}
}

// Key order in the source is irrelevant to the value: the result is a map, and
// two literals with the same pairs in different order must be equal. (Order only
// matters for the *schema*, where keys are emitted sorted.)
func TestEvalEdge_ObjectLiteralKeyOrderIrrelevant(t *testing.T) {
	a := evalOK(t, `{a: 1, b: map(ys, y => y), c: null}`, edgeEnv)
	b := evalOK(t, `{c: null, b: map(ys, y => y), a: 1}`, edgeEnv)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("key order changed the value:\n  %#v\n  %#v", a, b)
	}
}

// An error inside a value names the key it came from, which is the only clue an
// author gets when a large literal fails.
func TestEvalEdge_ObjectLiteralErrorNamesKey(t *testing.T) {
	err := evalErr(t, `{ok: 1, bad: `+edgeBoom+`}`, edgeEnv)
	if !strings.Contains(err.Error(), `"bad"`) {
		t.Errorf("expected the failing key in the error, got %v", err)
	}
}

func TestEvalEdge_ArrayLiteralEval(t *testing.T) {
	edgeOracleAll(t, []edgePair{
		{"empty", `[]`, `[]`},
		{"scalar_elements", `[1, "a", true, null]`, `[1, "a", true, null]`},
		{"nested_arrays", `[[1, [2, [3]]], []]`, `[[1, [2, [3]]], []]`},
		{"object_elements", `[{a: 1}, {a: 2}]`, `[{a: 1}, {a: 2}]`},
		{"map_call_as_element", `[map(ys, y => y + 1), []]`, `[map(ys, {let y = #; y + 1}), []]`},
	})
}

// A broken chain inside an array literal contributes null, keeping the element
// count stable.
func TestEvalEdge_ArrayLiteralBrokenChainIsANullElement(t *testing.T) {
	edgeJSON(t, `[deep.b.c.d, deep.nope, nul]`, `["leaf", null, null]`)
}

// An empty array literal is a non-nil empty slice, not nil.
func TestEvalEdge_EmptyArrayLiteralIsANonNilEmptySlice(t *testing.T) {
	got := evalOK(t, `[]`, edgeEnv)
	if arr, ok := got.([]any); !ok || arr == nil || len(arr) != 0 {
		t.Errorf("[] = %#v (%T), want a non-nil empty slice", got, got)
	}
}

func TestEvalEdge_ArrayLiteralErrorPropagates(t *testing.T) {
	edgeErrAll(t, []edgeCase{
		{"absent_identifier", `[1, ` + edgeBoom + `]`},
		{"division_by_zero", `[1, 2 / 0]`},
	})
}

// ---- equality on containers ----

// Regression: comparing two containers used to PANIC (Go's == on matching uncomparable types),
// taking the whole process down, and inference let it register because == was always boolean.
// Both halves now reject it; the assertion stays loose — the crash must never return.
func TestEvalEdge_EqualityOnContainersMustNotPanic(t *testing.T) {
	edgeNoPanicAll(t, []edgeCase{
		{"array_literals_equal", `[1] == [1]`},
		{"array_literals_not_equal", `[1] != [2]`},
		{"object_literals_equal", `{k: 1} == {k: 1}`},
		{"same_array_from_context", `ys == ys`},
		{"same_object_from_context", `obj == obj`},
		{"same_array_inequality", `xs != xs`},
		{"empty_object_literals", `{} == {}`},
		{"empty_array_literals", `[] == []`},
	})
}
