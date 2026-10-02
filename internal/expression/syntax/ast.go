// Package syntax is the expression AST and parser. Everything is an expression, which lets
// `{...}` be an object literal everywhere, a lambda body included. Lexing is expr-lang's; nodes
// carry no source positions, so anything pointing into the source reads Tokens.
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
package syntax

type Node interface{ isNode() }

type (
	// IntNode carries exact decimal text, not a Go int: an id past int64 is an ordinary value.
	// Radix prefixes are normalised away, so Text is always valid JSON.
	IntNode   struct{ Text string }
	FloatNode struct{ Text string }
	// StringNode holds the already-unescaped value.
	StringNode struct{ Value string }
	BoolNode   struct{ Value bool }
	NullNode   struct{}

	// IdentNode is a bare name: a context root (input, outputs, config, self,
	// error) or a lambda parameter.
	IdentNode struct{ Name string }

	// MemberNode is a.b, or a["b"] for a key no identifier spells (pure surface syntax). A null
	// base yields null (optional chaining).
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
