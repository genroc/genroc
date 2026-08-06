// Package syntax defines the genroc expression AST and its parser.
//
// The language is small and every construct is an expression — there are no
// statements. That is what lets `{...}` mean an object literal everywhere,
// including directly as a lambda body, without JavaScript's `({...})`
// workaround.
//
//	literals    1, 1.5, "s", 'r', true, false, null
//	identifier  input, outputs, config, self, error
//	member      a.b, a["a-b"]     index    a[0], a[expr]
//	array       [a, b]            object   {k: v, "k-2": v}
//	lambda      x => body         (x, i) => body
//	call        map(src, x => body)
//	unary       ! - +
//	binary      ?? || && == != < > <= >= + - * / %
//	ternary     c ? a : b
//
// Lexing is delegated to expr-lang's lexer, so string, number and escape rules
// stay identical to expr-lang's across upgrades; only the grammar is ours. The
// grammar diverges deliberately in two places: lambdas (expr-lang has no `=>`)
// and `{...}` as an object literal in every position (expr-lang reads a leading
// `{` in a predicate as a statement block). expr-lang's `#` pointer and the `.x`
// predicate shorthand are rejected — a lambda names its parameter instead, which
// is also the only way to reach an outer element from a nested lambda.
//
// Node types deliberately carry no source positions: parse errors report a
// position from the token stream, while evaluation and inference errors are
// reported against the whole expression, as they always have been.
package syntax

type Node interface{ isNode() }

type (
	// IntNode is an integer literal carrying its exact decimal text, not a Go int: an id past
	// int64 is an ordinary value here, and parsing rejected it while the same value arriving as
	// data stayed exact. Radix prefixes are normalised away, so Text is always valid JSON.
	IntNode   struct{ Text string }
	FloatNode struct{ Text string }
	// StringNode holds the already-unescaped value.
	StringNode struct{ Value string }
	BoolNode   struct{ Value bool }
	NullNode   struct{}

	// IdentNode is a bare name: a context root (input, outputs, config, self,
	// error) or a lambda parameter.
	IdentNode struct{ Name string }

	// MemberNode is property access: a.b, or a["b"] when the key is not spellable as an
	// identifier. Both forms parse here — the brackets are pure surface syntax. Access on a
	// null base yields null (optional chaining), so a missing field is never a hard error.
	MemberNode struct {
		Base Node
		Name string
	}

	// IndexNode is constant integer indexing, a[0].
	IndexNode struct {
		Base  Node
		Index int
	}

	// KeyNode is computed access, a[expr]. Accepted only where the answer cannot depend on
	// which key is read: an array, or a map declaring only additionalProperties. Objects with
	// declared properties are rejected by inference; the grammar admits the form anywhere.
	KeyNode struct {
		Base Node
		Key  Node
	}

	ArrayNode struct{ Items []Node }

	// ObjectNode is an object literal. Keys and Values are parallel and hold
	// source order; duplicate keys are rejected at parse time.
	ObjectNode struct {
		Keys   []string
		Values []Node
	}

	// LambdaNode is `param => body` or `(param, indexParam) => body`.
	// IndexParam is empty when the second parameter is omitted.
	LambdaNode struct {
		Param      string
		IndexParam string
		Body       Node
	}

	// CallNode is a builtin call. Name is always a member of builtins.
	CallNode struct {
		Name string
		Args []Node
	}

	UnaryNode struct {
		Op      string
		Operand Node
	}

	BinaryNode struct {
		Op          string
		Left, Right Node
	}

	CondNode struct{ Cond, Then, Else Node }
)

func (*IntNode) isNode()    {}
func (*FloatNode) isNode()  {}
func (*StringNode) isNode() {}
func (*BoolNode) isNode()   {}
func (*NullNode) isNode()   {}
func (*IdentNode) isNode()  {}
func (*MemberNode) isNode() {}
func (*IndexNode) isNode()  {}
func (*KeyNode) isNode()    {}
func (*ArrayNode) isNode()  {}
func (*ObjectNode) isNode() {}
func (*LambdaNode) isNode() {}
func (*CallNode) isNode()   {}
func (*UnaryNode) isNode()  {}
func (*BinaryNode) isNode() {}
func (*CondNode) isNode()   {}

// builtins maps each supported function to its arity. A call whose second
// argument must be a lambda is listed in lambdaArg.
var builtins = map[string]int{
	"map": 2,
}

// lambdaArg records which argument index must be a lambda, so the parser can
// reject `map(xs, xs)` as a syntax error rather than deferring it to inference.
var lambdaArg = map[string]int{
	"map": 1,
}
