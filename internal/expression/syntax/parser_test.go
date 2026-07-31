package syntax

import (
	"strings"
	"testing"
)

// The everyday grammar: one test per behaviour, each a single assertion. Edge
// cases, exhaustive rejection sweeps and error-quality checks live in
// parser_edge_test.go; the shared helpers live in helpers_test.go.

// -----------------------------------------------------------------------------
// Literals and access
// -----------------------------------------------------------------------------

func TestParse_IntLiteral(t *testing.T) {
	assertParses(t, `1`, `1`)
}

func TestParse_FloatLiteral(t *testing.T) {
	assertParses(t, `1.5`, `1.5`)
}

func TestParse_DoubleQuotedString(t *testing.T) {
	assertParses(t, `"s"`, `"s"`)
}

func TestParse_SingleQuotedString(t *testing.T) {
	assertParses(t, `'r'`, `"r"`)
}

func TestParse_BoolLiteral(t *testing.T) {
	assertParses(t, `true`, `true`)
}

func TestParse_NullLiteral(t *testing.T) {
	assertParses(t, `null`, `null`)
}

func TestParse_Identifier(t *testing.T) {
	assertParses(t, `input`, `input`)
}

func TestParse_MemberChain(t *testing.T) {
	assertParses(t, `input.a.b`, `input.a.b`)
}

func TestParse_IndexWithinMemberChain(t *testing.T) {
	assertParses(t, `input.a[2].b`, `input.a[2].b`)
}

// -----------------------------------------------------------------------------
// Precedence, mirroring expr-lang
// -----------------------------------------------------------------------------

func TestParse_MultiplyBindsTighterThanPlus(t *testing.T) {
	assertParses(t, `1 + 2 * 3`, `(+ 1 (* 2 3))`)
}

func TestParse_ParensOverridePrecedence(t *testing.T) {
	assertParses(t, `(1 + 2) * 3`, `(* (+ 1 2) 3)`)
}

func TestParse_AndBindsTighterThanOr(t *testing.T) {
	assertParses(t, `a || b && c`, `(|| a (&& b c))`)
}

func TestParse_UnaryNot(t *testing.T) {
	assertParses(t, `!a`, `(! a)`)
}

func TestParse_UnaryMinusBindsTighterThanPlus(t *testing.T) {
	assertParses(t, `-a + b`, `(+ (- a) b)`)
}

func TestParse_Coalesce(t *testing.T) {
	assertParses(t, `a ?? b`, `(?? a b)`)
}

func TestParse_CoalesceIsLeftAssociative(t *testing.T) {
	assertParses(t, `a ?? b ?? c`, `(?? (?? a b) c)`)
}

// ?? binds tighter than +.
func TestParse_CoalesceBindsTighterThanPlus(t *testing.T) {
	assertParses(t, `a + b ?? c`, `(+ a (?? b c))`)
}

func TestParse_Ternary(t *testing.T) {
	assertParses(t, `a ? b : c`, `(if a b c)`)
}

func TestParse_ComparisonBindsTighterThanTernary(t *testing.T) {
	assertParses(t, `a == 1 ? b : c`, `(if (== a 1) b c)`)
}

// -----------------------------------------------------------------------------
// Literals as values
// -----------------------------------------------------------------------------

func TestParse_ArrayLiteral(t *testing.T) {
	assertParses(t, `[1, 2]`, `[1 2]`)
}

func TestParse_EmptyArrayLiteral(t *testing.T) {
	assertParses(t, `[]`, `[]`)
}

func TestParse_ArrayTrailingComma(t *testing.T) {
	assertParses(t, `[1, 2,]`, `[1 2]`)
}

func TestParse_ObjectLiteral(t *testing.T) {
	assertParses(t, `{a: 1, b: "x"}`, `{a:1 b:"x"}`)
}

func TestParse_EmptyObjectLiteral(t *testing.T) {
	assertParses(t, `{}`, `{}`)
}

func TestParse_QuotedObjectKey(t *testing.T) {
	assertParses(t, `{"dashed-key": 1}`, `{dashed-key:1}`)
}

func TestParse_NestedObjectAndArrayLiterals(t *testing.T) {
	assertParses(t, `{a: {b: [1]}}`, `{a:{b:[1]}}`)
}

// -----------------------------------------------------------------------------
// Lambdas
// -----------------------------------------------------------------------------

func TestParse_Lambda(t *testing.T) {
	assertParses(t, `map(xs, x => x.n)`, `map(xs (\x -> x.n))`)
}

// The object body needs no parentheses — the whole point.
func TestParse_LambdaBodyObjectLiteral(t *testing.T) {
	assertParses(t, `map(xs, x => {id: x.id})`, `map(xs (\x -> {id:x.id}))`)
}

func TestParse_LambdaWithIndexParam(t *testing.T) {
	assertParses(t, `map(xs, (x, i) => {i: i, n: x.n})`, `map(xs (\x,i -> {i:i n:x.n}))`)
}

func TestParse_LambdaWithoutSpaces(t *testing.T) {
	assertParses(t, `map(xs,x=>x.n)`, `map(xs (\x -> x.n))`)
}

// Nested lambda capturing the outer parameter — impossible with expr-lang's #.
func TestParse_NestedLambdaCapturesOuterParam(t *testing.T) {
	assertParses(t, `map(a, x => map(b, y => x.n + y.m))`, `map(a (\x -> map(b (\y -> (+ x.n y.m)))))`)
}

func TestParse_IndexOnCallResult(t *testing.T) {
	assertParses(t, `map(xs, x => x.n) [0]`, `map(xs (\x -> x.n))[0]`)
}

// A parenthesised expression that merely looks like a parameter list.
func TestParse_ParenthesizedExprIsNotAParamList(t *testing.T) {
	assertParses(t, `(a + b) * 2`, `(* (+ a b) 2)`)
}

func TestParse_RedundantParens(t *testing.T) {
	assertParses(t, `(a)`, `a`)
}

// -----------------------------------------------------------------------------
// Rejections
// -----------------------------------------------------------------------------

// The expr-lang coalesce mixing rule, replicated so grouping never differs
// silently between the two languages.

func TestParseError_CoalesceMixedWithPlus(t *testing.T) {
	assertParseError(t, `a ?? 0 + 1`, "cannot be mixed")
}

func TestParseError_CoalesceMixedWithEquality(t *testing.T) {
	assertParseError(t, `a ?? b == c`, "cannot be mixed")
}

// Predicate syntax is gone; both forms point at lambdas.

func TestParseError_PointerShorthand(t *testing.T) {
	assertParseError(t, `map(xs, #.n)`, "name the parameter")
}

func TestParseError_LeadingDotShorthand(t *testing.T) {
	assertParseError(t, `map(xs, .n)`, "name the parameter")
}

// Builtin arity and shape.

func TestParseError_MapMissingSecondArgument(t *testing.T) {
	assertParseError(t, `map(xs)`, "takes 2 arguments")
}

func TestParseError_MapSecondArgumentNotALambda(t *testing.T) {
	assertParseError(t, `map(xs, ys)`, "expects a lambda")
}

func TestParseError_UnknownFunctionFilter(t *testing.T) {
	assertParseError(t, `filter(xs, x => x.n)`, "unknown function")
}

func TestParseError_UnknownFunctionLen(t *testing.T) {
	assertParseError(t, `len(xs)`, "unknown function")
}

// Object and index constraints.

func TestParseError_DuplicateObjectKey(t *testing.T) {
	assertParseError(t, `{a: 1, a: 2}`, "duplicate object key")
}

func TestParseError_NumericObjectKey(t *testing.T) {
	assertParseError(t, `{1: 2}`, "object key must be")
}

// A computed key parses against any base; only inference knows whether the base
// has one type for every key. Literal subscripts keep their own nodes, so nothing
// that worked before now takes the computed path.
func TestComputedKeyParses(t *testing.T) {
	assertParses(t, `a[b]`, `a[b]`)
	assertParses(t, `a[i + 1]`, `a[(+ i 1)]`)
	assertParses(t, `a["k" + "j"]`, `a[(+ "k" "j")]`)
	assertParses(t, `a[-1]`, `a[(- 1)]`)
	assertParses(t, `a[1.5]`, `a[1.5]`)
	assertParses(t, `headers[k].x`, `headers[k].x`)
	assertParses(t, `a[b[c]]`, `a[b[c]]`)
	// Still the literal forms, not KeyNode.
	assertParses(t, `a[0]`, `a[0]`)
	assertParses(t, `a["k"]`, `a.k`)
}

// The bracket takes a full expression, so everything the grammar allows elsewhere
// composes inside it.
func TestComputedKeyNesting(t *testing.T) {
	assertParses(t, `a[b][c]`, `a[b][c]`)
	assertParses(t, `a[b.c.d]`, `a[b.c.d]`)
	assertParses(t, `a[b[c[d]]]`, `a[b[c[d]]]`)
	assertParses(t, `a[b].c[d].e`, `a[b].c[d].e`)
	assertParses(t, `a[b ? c : d]`, `a[(if b c d)]`)
	assertParses(t, `a[b ?? "d"]`, `a[(?? b "d")]`)
	assertParses(t, `a[ b ]`, `a[b]`) // whitespace inside the bracket
	assertParses(t, `a[b == 1]`, `a[(== b 1)]`)
	assertParses(t, `map(xs, x => m[x])`, `map(xs (\x -> m[x]))`)
	assertParses(t, `map(xs, (v, i) => m[i])`, `map(xs (\v,i -> m[i]))`)
}

// A lambda is only ever a builtin's callback argument. The permission is consumed
// by the enclosing primary, so it does not leak into a bracket — including a
// bracket that sits inside a lambda that was itself permitted.
func TestComputedKeyRejectsLambda(t *testing.T) {
	assertParseError(t, `a[x => 1]`, "lambda is only valid")
	assertParseError(t, `map(xs, x => m[y => 1])`, "lambda is only valid")
}

// A literal subscript still takes the literal path even where a computed one would
// also be grammatical, so the desugaring to MemberNode is not shadowed.
func TestComputedKeyLiteralsKeepTheirNodes(t *testing.T) {
	assertSameTree(t, `a["k"]`, `a.k`)
	assertSameTree(t, `a[0]`, `a[0]`)
	// Parenthesised or signed, it is an expression again — same value, different node.
	assertParses(t, `a[(0)]`, `a[0]`)
	assertParses(t, `a[+0]`, `a[(+ 0)]`)
}

// A string subscript is property access, not indexing: it must produce the exact
// tree the dot form produces, since that is what leaves inference and evaluation
// untouched by the new syntax.
func TestStringSubscriptIsMemberAccess(t *testing.T) {
	assertSameTree(t, `a["k"]`, `a.k`)
	assertSameTree(t, `a['k']`, `a.k`)
	assertSameTree(t, `a["k"].b["c"]`, `a.k.b.c`)
}

// The keys that motivate the syntax: unreachable via `.` because they lex as
// something else (`retry-after` is a subtraction) or not at all.
func TestStringSubscriptNonIdentifierKeys(t *testing.T) {
	assertParses(t, `self.headers["retry-after"]`, `self.headers.retry-after`)
	assertParses(t, `config["a.b"]`, `config.a.b`)
	assertParses(t, `outputs["step-1"].value`, `outputs.step-1.value`)
	assertParses(t, `a[""]`, `a.`)
}

// A subscript is an ordinary string literal, so it inherits expr-lang's string
// rules wholesale — both quote styles and the usual escapes. Asserted on the
// decoded key rather than the source, since that is what property lookup uses.
func TestStringSubscriptEscapes(t *testing.T) {
	cases := []struct{ src, wantKey string }{
		{`a["with space"]`, "with space"},
		{`a["say \"hi\""]`, `say "hi"`},
		{`a['it\'s']`, "it's"},
		{`a['single "double" inside']`, `single "double" inside`},
		{`a["back\\slash"]`, `back\slash`},
		{`a["tab\there"]`, "tab\there"},
		{`a["nl\nhere"]`, "nl\nhere"},
		{`a["\x41"]`, "A"},
		{`a["café"]`, "café"},
		{`a["emoji 🎉"]`, "emoji 🎉"},
		{`a["]"]`, "]"}, // a bracket in the key does not end the subscript
	}
	for _, c := range cases {
		t.Run(c.wantKey, func(t *testing.T) {
			n, err := Parse(c.src)
			if err != nil {
				t.Fatalf("Parse(%s): %v", c.src, err)
			}
			m, ok := n.(*MemberNode)
			if !ok {
				t.Fatalf("Parse(%s) = %T, want *MemberNode", c.src, n)
			}
			if m.Name != c.wantKey {
				t.Errorf("Parse(%s) key = %q, want %q", c.src, m.Name, c.wantKey)
			}
		})
	}
}

// Leftovers and unsupported spellings.

func TestParseError_LeftoverToken(t *testing.T) {
	assertParseError(t, `a b`, "unexpected")
}

func TestParseError_WordOperatorAnd(t *testing.T) {
	assertParseError(t, `a and b`, "unexpected")
}

func TestParseError_NilKeyword(t *testing.T) {
	assertParseError(t, `nil`, "use null")
}

func TestParseError_ElvisOperator(t *testing.T) {
	assertParseError(t, `a ?: b`, "not supported")
}

func TestParseError_LetStatement(t *testing.T) {
	assertParseError(t, `let x = 1; x`, "unexpected")
}

func TestParseError_DuplicateLambdaParams(t *testing.T) {
	assertParseError(t, `map(xs, (x, x) => x)`, "distinct names")
}

func TestParseError_UnterminatedString(t *testing.T) {
	assertParseError(t, `"unterminated`, "literal not terminated")
}

// Byte literals are lexed by expr-lang but have no JSON counterpart, so the
// grammar rejects them with a message that names the alternative.
func TestParseRejectsByteLiteral(t *testing.T) {
	assertParseError(t, `b'bytes'`, "byte string literals")
}

// -----------------------------------------------------------------------------
// Error quality
// -----------------------------------------------------------------------------

// TestParseErrorPointsAtSource is the reason for owning the parser: errors quote
// what the author wrote, with a caret at the offending token.
func TestParseErrorPointsAtSource(t *testing.T) {
	got := parseErr(t, `map(a, x => x.n + )`).Error()
	if !strings.Contains(got, `map(a, x => x.n + )`) {
		t.Errorf("error should quote the original source, got:\n%s", got)
	}
	if strings.Contains(got, "let ") || strings.Contains(got, "#") {
		t.Errorf("error must not leak a rewritten form, got:\n%s", got)
	}
	t.Logf("sample error:\n%s", got)
}
