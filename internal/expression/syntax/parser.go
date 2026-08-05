package syntax

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/cockroachdb/apd/v3"
	"github.com/expr-lang/expr/file"
	"github.com/expr-lang/expr/parser/lexer"

	"genroc/internal/numeric"
)

// binaryPrec mirrors expr-lang's precedence for every operator the two share, so
// an expression that parses in both languages groups identically. Notably `??`
// binds tighter than arithmetic (500), which is why mixing it with another
// operator without parentheses is rejected outright — see parseBinary.
var binaryPrec = map[string]int{
	"||": 10,
	"&&": 15,
	"==": 20, "!=": 20, "<": 20, ">": 20, "<=": 20, ">=": 20,
	"+": 30, "-": 30,
	"*": 60, "/": 60, "%": 60,
	"??": 500,
}

var unaryOps = map[string]bool{"!": true, "-": true, "+": true}

func Parse(src string) (n Node, err error) {
	tokens, lexErr := lexer.Lex(file.NewSource(src))
	if lexErr != nil {
		return nil, fmt.Errorf("%s", strings.SplitN(lexErr.Error(), "\n", 2)[0])
	}
	p := &parser{src: src, tokens: tokens}
	defer func() {
		if r := recover(); r != nil {
			pe, ok := r.(parseError)
			if !ok {
				panic(r)
			}
			n, err = nil, pe.err
		}
	}()
	node := p.parseExpr()
	if tok := p.cur(); tok.Kind != lexer.EOF {
		p.failAt(tok, "unexpected %s", describe(tok))
	}
	return node, nil
}

// parseError carries a failure out of the recursive descent without threading an
// error return through every production.
type parseError struct{ err error }

type parser struct {
	src    string
	tokens []lexer.Token
	pos    int
	// lambdaOK marks a builtin's lambda slot; parsePrimary consumes it so it cannot leak into
	// nested expressions (map(xs, [x => 1]) must reject the inner lambda). Without it a
	// lambda parses anywhere and fails later with no source quote or caret.
	lambdaOK bool
}

func (p *parser) cur() lexer.Token  { return p.at(0) }
func (p *parser) peek() lexer.Token { return p.at(1) }

func (p *parser) at(n int) lexer.Token {
	if p.pos+n >= len(p.tokens) {
		return lexer.Token{Kind: lexer.EOF}
	}
	return p.tokens[p.pos+n]
}

func (p *parser) next() lexer.Token {
	t := p.cur()
	p.pos++
	return t
}

func (p *parser) is(kind lexer.Kind, val string) bool {
	t := p.cur()
	return t.Kind == kind && t.Value == val
}

func (p *parser) expect(kind lexer.Kind, val string) lexer.Token {
	if !p.is(kind, val) {
		p.failAt(p.cur(), "expected %q, got %s", val, describe(p.cur()))
	}
	return p.next()
}

// fail reports an error against src with a caret pointing at offset. These errors
// reach users through the definition-registration API, so they quote the
// expression the author actually wrote.
func (p *parser) failAt(tok lexer.Token, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	at := tok.From
	if at > len(p.src) {
		at = len(p.src)
	}
	panic(parseError{err: fmt.Errorf("%s\n  %s\n  %s^", msg, p.src, strings.Repeat(" ", at))})
}

func describe(t lexer.Token) string {
	if t.Kind == lexer.EOF {
		return "end of expression"
	}
	return fmt.Sprintf("%q", t.Value)
}

// parseExpr parses a full expression: binary operators, then a trailing ternary.
func (p *parser) parseExpr() Node {
	cond := p.parseBinary(0)
	if !p.is(lexer.Operator, "?") {
		return cond
	}
	p.next()
	if p.is(lexer.Operator, ":") {
		p.failAt(p.cur(), "the elvis operator (?:) is not supported; use ?? for a default value")
	}
	then := p.parseExpr()
	p.expect(lexer.Operator, ":")
	return &CondNode{Cond: cond, Then: then, Else: p.parseExpr()}
}

// parseBinary is precedence climbing. prevOp is local to each invocation, which
// is what makes `a + b ?? c` legal (the ?? is parsed in the recursive call for
// +'s right operand) while `a ?? b + c` is not — matching expr-lang exactly.
func (p *parser) parseBinary(minPrec int) Node {
	left := p.parseUnary()
	prevOp := ""
	for {
		tok := p.cur()
		if tok.Kind != lexer.Operator {
			break
		}
		prec, ok := binaryPrec[tok.Value]
		if !ok || prec < minPrec {
			break
		}
		if prevOp == "??" && tok.Value != "??" {
			p.failAt(tok, "operator (%s) cannot be mixed with coalesce (??); wrap one side in parentheses", tok.Value)
		}
		p.next()
		right := p.parseBinary(prec + 1)
		left = &BinaryNode{Op: tok.Value, Left: left, Right: right}
		prevOp = tok.Value
	}
	return left
}

func (p *parser) parseUnary() Node {
	tok := p.cur()
	if tok.Kind == lexer.Operator && unaryOps[tok.Value] {
		p.next()
		return &UnaryNode{Op: tok.Value, Operand: p.parseUnary()}
	}
	return p.parsePostfix(p.parsePrimary())
}

// parsePostfix applies the trailing .name and [i] accessors.
func (p *parser) parsePostfix(base Node) Node {
	for {
		switch {
		case p.is(lexer.Operator, "."):
			p.next()
			name := p.cur()
			if name.Kind != lexer.Identifier {
				p.failAt(name, "expected a property name after '.', got %s", describe(name))
			}
			p.next()
			base = &MemberNode{Base: base, Name: name.Value}
		case p.is(lexer.Bracket, "["):
			p.next()
			idx := p.cur()
			closes := p.peek().Kind == lexer.Bracket && p.peek().Value == "]"
			switch {
			// A string subscript is property access spelled so a non-identifier key
			// ("retry-after") is reachable at all — `.retry-after` lexes as subtraction. It
			// desugars to the dot form's node, so nothing downstream learns the difference.
			case idx.Kind == lexer.String && closes:
				p.next()
				p.next()
				base = &MemberNode{Base: base, Name: idx.Value}
			case idx.Kind == lexer.Number && isIntLiteral(idx.Value) && closes:
				p.next()
				n, err := parseIndex(idx.Value)
				if err != nil {
					p.failAt(idx, "invalid index %q", idx.Value)
				}
				p.next()
				base = &IndexNode{Base: base, Index: n}
			// Anything else is a computed key. Only inference can say whether the
			// base admits one, so the grammar takes any expression here and lets the
			// type error carry the explanation.
			default:
				if p.is(lexer.Bracket, "]") {
					p.failAt(idx, "an index must not be empty")
				}
				key := p.parseExpr()
				p.expect(lexer.Bracket, "]")
				base = &KeyNode{Base: base, Key: key}
			}
		default:
			return base
		}
	}
}

func (p *parser) parsePrimary() Node {
	// Consume the lambda permission: it applies to this expression only, not to
	// anything nested inside it.
	lambdaOK := p.lambdaOK
	p.lambdaOK = false

	tok := p.cur()

	// The pointer and shorthand forms of expr-lang predicates: rejected in favour
	// of a named lambda parameter, which nested lambdas also need in order to
	// reach an outer element.
	if tok.Kind == lexer.Operator && tok.Value == "#" {
		p.failAt(tok, "'#' is not supported; name the parameter instead, e.g. map(xs, x => x.field)")
	}
	if tok.Kind == lexer.Operator && tok.Value == "." {
		p.failAt(tok, "'.field' shorthand is not supported; name the parameter instead, e.g. map(xs, x => x.field)")
	}

	switch tok.Kind {
	case lexer.Number:
		p.next()
		return numberNode(p, tok)

	case lexer.String:
		p.next()
		return &StringNode{Value: tok.Value}

	case lexer.Bytes:
		// expr-lang's b'…' produces a []byte, which has no JSON counterpart.
		p.failAt(tok, "byte string literals (b'…') are not supported; use a plain string")

	case lexer.Identifier:
		switch tok.Value {
		case "true", "false":
			p.next()
			return &BoolNode{Value: tok.Value == "true"}
		case "null":
			p.next()
			return &NullNode{}
		case "nil":
			p.failAt(tok, "'nil' is not supported; use null")
		}
		if p.isArrow(1) {
			if !lambdaOK {
				p.failAt(tok, "a lambda is only valid as the callback argument, e.g. map(xs, x => x.field)")
			}
			// name, "=", ">" — the body starts at offset 3.
			return p.parseLambda([]string{tok.Value}, 3)
		}
		if p.peek().Kind == lexer.Bracket && p.peek().Value == "(" {
			return p.parseCall(tok)
		}
		p.next()
		return &IdentNode{Name: tok.Value}

	case lexer.Bracket:
		switch tok.Value {
		case "(":
			if params, after, ok := p.lambdaParams(); ok {
				if !lambdaOK {
					p.failAt(tok, "a lambda is only valid as the callback argument, e.g. map(xs, x => x.field)")
				}
				return p.parseLambda(params, after)
			}
			p.next()
			// Parentheses are transparent, so map(xs, (x => x)) stays legal.
			p.lambdaOK = lambdaOK
			n := p.parseExpr()
			p.expect(lexer.Bracket, ")")
			return n
		case "[":
			return p.parseArray()
		case "{":
			return p.parseObject()
		}
	}

	p.failAt(tok, "unexpected %s", describe(tok))
	return nil
}

// isArrow reports whether the tokens at offset n form "=>". expr-lang's lexer has
// no "=>" token, so it arrives as adjacent "=" and ">" operators; adjacency is
// checked by byte offset so "a = > b" is not mistaken for a lambda. ">=" lexes as
// a single token, so there is no ambiguity with comparison.
func (p *parser) isArrow(n int) bool {
	eq, gt := p.at(n), p.at(n+1)
	return eq.Kind == lexer.Operator && eq.Value == "=" &&
		gt.Kind == lexer.Operator && gt.Value == ">" && eq.To == gt.From
}

// lambdaParams recognises a parenthesised parameter list followed by "=>",
// returning the names and the token offset of the "=>". It scans to the matching
// ")" first, because "(a, b) => x" and "(a + b) * 2" share a prefix.
func (p *parser) lambdaParams() (params []string, arrowAt int, ok bool) {
	depth := 0
	for i := 0; ; i++ {
		t := p.at(i)
		if t.Kind == lexer.EOF {
			return nil, 0, false
		}
		if t.Kind == lexer.Bracket {
			switch t.Value {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
				if depth == 0 {
					if !p.isArrow(i + 1) {
						return nil, 0, false
					}
					names, valid := p.paramNames(1, i)
					if !valid {
						p.failAt(p.at(1), "lambda parameters must be plain names, e.g. (item, index) => …")
					}
					return names, i + 3, true
				}
			}
		}
	}
}

// paramNames reads the comma-separated identifiers between token offsets from
// (exclusive of the opening bracket) and end (the closing bracket), alternating
// name and comma so a trailing or doubled comma is rejected.
func (p *parser) paramNames(from, end int) ([]string, bool) {
	var names []string
	wantName := true
	for i := from; i < end; i++ {
		t := p.at(i)
		if wantName {
			if t.Kind != lexer.Identifier {
				return nil, false
			}
			names = append(names, t.Value)
			wantName = false
			continue
		}
		if t.Kind == lexer.Operator && t.Value == "," {
			wantName = true
			continue
		}
		return nil, false
	}
	if wantName || len(names) == 0 || len(names) > 2 {
		return nil, false
	}
	return names, true
}

// parseLambda skips the header (advance tokens: the parameters plus "=>") and
// parses the body.
func (p *parser) parseLambda(params []string, advance int) Node {
	p.pos += advance
	lam := &LambdaNode{Param: params[0]}
	if len(params) > 1 {
		lam.IndexParam = params[1]
	}
	if lam.Param == lam.IndexParam {
		p.failAt(p.cur(), "lambda parameters must have distinct names")
	}
	lam.Body = p.parseExpr()
	return lam
}

func (p *parser) parseCall(name lexer.Token) Node {
	arity, known := builtins[name.Value]
	if !known {
		p.failAt(name, "unknown function %q; supported: %s", name.Value, builtinNames())
	}
	p.next() // name
	p.expect(lexer.Bracket, "(")
	wantsLambdaAt, hasLambdaArg := lambdaArg[name.Value]
	var args []Node
	for !p.is(lexer.Bracket, ")") {
		if len(args) > 0 {
			p.expect(lexer.Operator, ",")
		}
		// Only this builtin's designated slot may hold a lambda.
		p.lambdaOK = hasLambdaArg && len(args) == wantsLambdaAt
		args = append(args, p.parseExpr())
	}
	p.lambdaOK = false
	closing := p.cur()
	p.expect(lexer.Bracket, ")")
	if len(args) != arity {
		p.failAt(closing, "%s takes %d arguments, got %d", name.Value, arity, len(args))
	}
	if idx, needs := lambdaArg[name.Value]; needs {
		if _, isLambda := args[idx].(*LambdaNode); !isLambda {
			p.failAt(closing, "%s expects a lambda as argument %d, e.g. %s(xs, x => x.field)", name.Value, idx+1, name.Value)
		}
	}
	return &CallNode{Name: name.Value, Args: args}
}

func (p *parser) parseArray() Node {
	p.expect(lexer.Bracket, "[")
	arr := &ArrayNode{}
	for !p.is(lexer.Bracket, "]") {
		if len(arr.Items) > 0 {
			p.expect(lexer.Operator, ",")
			if p.is(lexer.Bracket, "]") { // tolerate a trailing comma
				break
			}
		}
		arr.Items = append(arr.Items, p.parseExpr())
	}
	p.expect(lexer.Bracket, "]")
	return arr
}

func (p *parser) parseObject() Node {
	p.expect(lexer.Bracket, "{")
	obj := &ObjectNode{}
	seen := map[string]bool{}
	for !p.is(lexer.Bracket, "}") {
		if len(obj.Keys) > 0 {
			p.expect(lexer.Operator, ",")
			if p.is(lexer.Bracket, "}") { // tolerate a trailing comma
				break
			}
		}
		keyTok := p.cur()
		var key string
		switch keyTok.Kind {
		case lexer.Identifier, lexer.String:
			key = keyTok.Value
		default:
			p.failAt(keyTok, "an object key must be a name or a quoted string, got %s", describe(keyTok))
		}
		p.next()
		if seen[key] {
			p.failAt(keyTok, "duplicate object key %q", key)
		}
		seen[key] = true
		p.expect(lexer.Operator, ":")
		obj.Keys = append(obj.Keys, key)
		obj.Values = append(obj.Values, p.parseExpr())
	}
	p.expect(lexer.Bracket, "}")
	return obj
}

func numberNode(p *parser, tok lexer.Token) Node {
	text, integral, err := normalizeNumber(tok.Value)
	if err != nil {
		p.failAt(tok, "%s", err)
	}
	if integral {
		return &IntNode{Text: text}
	}
	return &FloatNode{Text: text}
}

// normalizeNumber turns a literal into exact decimal text that is also valid JSON
// number syntax, and reports whether it is integral.
//
// There is no size limit: the text is carried through to an arbitrary-precision
// decimal, so a literal is exactly as precise as the same value arriving as data.
// Normalisation is what makes the text safe to emit — a radix prefix (0x1F) and
// the lexer's bare forms (.5, 1.) are all valid input but none is valid JSON.
func normalizeNumber(lit string) (text string, integral bool, err error) {
	clean := strings.ReplaceAll(lit, "_", "")
	if low := strings.ToLower(clean); strings.HasPrefix(low, "0x") || strings.HasPrefix(low, "0b") || strings.HasPrefix(low, "0o") {
		// Base 0 reads the prefix. Only prefixed literals go through it: for a
		// plain decimal it would apply C's leading-zero-octal rule, making 017
		// mean 15 where expr-lang reads 17.
		n, ok := new(big.Int).SetString(clean, 0)
		if !ok {
			return "", false, fmt.Errorf("invalid number %q", lit)
		}
		return n.String(), true, nil
	}
	d, _, derr := apd.NewFromString(clean)
	if derr != nil || d.Form != apd.Finite {
		return "", false, fmt.Errorf("invalid number %q", lit)
	}
	if numeric.ExceedsMaxDigits(d) {
		return "", false, fmt.Errorf("number literal has %d digits, over the %d-digit limit", d.NumDigits(), numeric.MaxDigits)
	}
	out, ok := numeric.Format(d)
	if !ok {
		return "", false, fmt.Errorf("invalid number %q", lit)
	}
	// The literal's spelling decides the static type: a fraction or an exponent
	// makes it a "number", everything else an "integer". The value is exact either
	// way, so this is only about which type inference reports.
	return out.String(), !strings.ContainsAny(strings.ToLower(clean), ".e"), nil
}

// isIntLiteral reports whether a number literal is integral. Hex and binary
// literals have no fractional or exponent form, so only the decimal case needs
// the '.'/'e' check.
func isIntLiteral(s string) bool {
	low := strings.ToLower(s)
	if strings.HasPrefix(low, "0x") || strings.HasPrefix(low, "0b") || strings.HasPrefix(low, "0o") {
		return true
	}
	return !strings.ContainsAny(low, ".e")
}

// parseIndex reads an array index. Unlike a value literal this genuinely must fit
// in a Go int, since it indexes a slice. Non-prefixed literals are decimal: base 0
// would apply C's leading-zero-octal rule, making 017 mean 15 and rejecting 08
// outright, where expr-lang's lexer reads both as plain decimal.
func parseIndex(s string) (int, error) {
	clean := strings.ReplaceAll(s, "_", "")
	base := 10
	if low := strings.ToLower(clean); strings.HasPrefix(low, "0x") || strings.HasPrefix(low, "0b") || strings.HasPrefix(low, "0o") {
		base = 0 // let strconv read the prefix
	}
	n, err := strconv.ParseInt(clean, base, 64)
	return int(n), err
}

func builtinNames() string {
	names := make([]string, 0, len(builtins))
	for n := range builtins {
		names = append(names, n)
	}
	// Sorted so the message is stable; the set is tiny.
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return strings.Join(names, ", ")
}
