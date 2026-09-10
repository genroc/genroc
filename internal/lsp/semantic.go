package lsp

// Semantic tokens: which scalars in a definition actually compute, and what is inside them.
//
// This is the half a TextMate grammar cannot do. A grammar sees `"$: tick"` and nothing else —
// whether that scalar evaluates depends on WHICH SLOT holds it, which is schema knowledge, so a
// grammar highlights `id: "$: tick"` as an expression and is simply wrong. The server knows the
// slot. specs/language-server.md §5.
//
// The expression's insides come from the language's own lexer (`syntax.Tokens`) and the marker
// positions from the template scanner (`template.Scan`), so nothing here re-derives a rule.

import (
	"sort"
	"strings"

	"genroc/internal/defdoc"
	"genroc/internal/expression/syntax"
	"genroc/internal/schema"
	"genroc/internal/template"
	"genroc/internal/validation"
)

// The legend, in index order. Only STANDARD LSP types: a custom name is legal and goes
// uncoloured in most editors, which is indistinguishable from the server not working.
var semanticTokenTypes = []string{
	"keyword",   // the $: / ${ } markers, and true/false/null
	"operator",  // ?? + . == and the rest
	"variable",  // every segment of a member path, root included
	"number",    //
	"string",    // a string literal INSIDE an expression
	"function",  // the map builtin, and a routing target
	"parameter", // a lambda's argument
}

const (
	typeKeyword = iota
	typeOperator
	typeVariable
	typeNumber
	typeString
	typeFunction
	typeParameter
)

// token is one highlighted range, in protocol coordinates.
type token struct {
	line   int // 0-based
	start  int // 0-based, UTF-16 code units
	length int // UTF-16 code units
	kind   int
}

// semanticTokens returns the `data` array for textDocument/semanticTokens/full: five integers
// per token, each position delta-encoded against the one before it.
func semanticTokens(text string) []uint32 {
	lines := splitLines(text)
	docs, err := defdoc.ParseAll([]byte(text))
	if err != nil {
		return []uint32{}
	}

	var out []token
	for _, doc := range docs {
		def, ok := definitionOf(doc)
		if !ok {
			continue
		}
		contexts, err := validation.SlotContexts(def)
		if err != nil {
			continue
		}
		for _, path := range doc.Paths() {
			out = append(out, tokensAt(doc, lines, contexts, path)...)
		}
	}
	return encode(out)
}

func tokensAt(doc *defdoc.Doc, lines []string, contexts map[string]schema.Schema, path string) []token {
	span, ok := doc.Span(path)
	if !ok {
		return nil
	}
	// One line only. A block scalar's expression would need the span mapped through YAML's
	// folding rules, which is a second grammar to keep true for a form nobody writes an
	// expression in.
	if span.Value.Line != span.Value.EndLine {
		return nil
	}
	// Scalars only. A container's span covers its children, so scanning it would find the
	// same `$:` a second time and emit a token overlapping the leaf's own.
	if v, ok := doc.ValueAt(path); !ok {
		return nil
	} else if _, isString := v.(string); !isString {
		return nil
	}
	raw, base, ok := scalarSource(lines, span.Value)
	if !ok {
		return nil
	}
	// The quotes belong to YAML, and Scan reads a scalar's CONTENT: `$:` is a marker only as
	// the first non-whitespace content, which a leading quote would hide.
	inner, quote := unquote(raw)
	base += quote

	switch {
	case isRoutingSlot(path):
		return routingTokens(inner, base, lines, span.Value.Line)
	case isBareExpression(path):
		// A `case` carries no marker: the whole scalar is the expression.
		return exprTokens(inner, base, lines, span.Value.Line, template.Span{From: 0, To: len(inner)}, nil)
	case isExpressionSlot(contexts, path):
		var out []token
		for _, r := range template.Scan(inner) {
			out = append(out, exprTokens(inner, base, lines, span.Value.Line, r.Body, markerSpans(r))...)
		}
		return out
	}
	return nil
}

func markerSpans(r template.Region) []template.Span {
	out := []template.Span{r.Marker}
	if !r.Close.Empty() {
		out = append(out, r.Close)
	}
	return out
}

// isExpressionSlot reports whether a leaf's text is evaluated. A slot is an expression position
// when a task phase's context governs it — the same test hover uses, so the two cannot disagree
// about what a scalar is — minus the leaves under that phase which are plainly not templates.
func isExpressionSlot(contexts map[string]schema.Schema, path string) bool {
	if _, _, found := enclosingSlot(contexts, path); !found {
		return false
	}
	seg := strings.Split(path, ".")
	for _, s := range seg {
		// A user schema lives inside the action, so the phase test admits it. Its `default`
		// and `description` are data about a type, never templates.
		if s == "result_schema" || s == "responses" {
			return false
		}
	}
	return !literalKeys[seg[len(seg)-1]]
}

// The leaves under a task phase that hold text rather than a template. Everything else an
// action carries — url, body, headers, query, input, over, the output map — is a Shape.
var literalKeys = map[string]bool{
	"id": true, "name": true, "type": true, "code": true,
	"goto": true, "description": true, "method": true,
}

// routingTokens marks `$task`, `end` and `next` — a closed set, and the one thing about a
// routing slot worth a colour. A quoted target is the same target.
func routingTokens(inner string, base int, lines []string, line int) []token {
	name := strings.TrimSpace(inner)
	if name == "" {
		return nil
	}
	if name != "end" && name != "next" && !strings.HasPrefix(name, "$") {
		return nil
	}
	from := strings.Index(inner, name)
	return []token{tokenAt(lines, line, base+from, base+from+len(name), typeFunction)}
}

// exprTokens lexes one expression body and marks its tokens, plus the markers around it.
func exprTokens(raw string, base int, lines []string, line int, body template.Span, markers []template.Span) []token {
	out := make([]token, 0, 8)
	for _, m := range markers {
		out = append(out, tokenAt(lines, line, base+m.From, base+m.To, typeKeyword))
	}
	src := raw[body.From:body.To]
	lexed := syntax.Tokens(src)
	if len(lexed) == 0 && strings.TrimSpace(src) != "" {
		// A body the lexer refuses — a YAML-escaped quote inside it, or half a word being
		// typed. One token over the whole thing still says "this computes", which is the
		// part worth keeping.
		return append(out, tokenAt(lines, line, base+body.From, base+body.To, typeVariable))
	}
	for i, t := range lexed {
		kind, ok := classify(lexed, i)
		if !ok {
			continue
		}
		out = append(out, tokenAt(lines, line, base+body.From+t.From, base+body.From+t.To, kind))
	}
	return out
}

// classify maps one lexical token to a semantic type. Every segment of a member path gets the
// SAME type, root included: `self` and the names after it are one referent, and two types put
// two colours in one path.
func classify(toks []syntax.Token, i int) (int, bool) {
	t := toks[i]
	switch t.Kind {
	case syntax.KindNumber:
		return typeNumber, true
	case syntax.KindString:
		return typeString, true
	case syntax.KindOperator:
		return typeOperator, true
	case syntax.KindBracket:
		return 0, false // brackets read as punctuation; no editor colours them usefully
	}
	switch t.Text {
	case "true", "false", "nil", "null":
		return typeKeyword, true
	}
	if next, ok := peek(toks, i+1); ok {
		// `=>` reaches here as two operator tokens: expr-lang has no arrow token.
		if next.Kind == syntax.KindOperator && next.Text == "=" {
			if after, ok := peek(toks, i+2); ok && after.Text == ">" {
				return typeParameter, true
			}
		}
		if next.Kind == syntax.KindBracket && next.Text == "(" {
			return typeFunction, true
		}
	}
	return typeVariable, true
}

func peek(toks []syntax.Token, i int) (syntax.Token, bool) {
	if i < 0 || i >= len(toks) {
		return syntax.Token{}, false
	}
	return toks[i], true
}

// scalarSource returns a value's text as WRITTEN and the byte column it starts at. Raw, not
// decoded: a column is what the protocol hands back, and mapping one through YAML's escaping
// would be a second grammar to keep true.
func scalarSource(lines []string, r defdoc.Range) (string, int, bool) {
	i := r.Line - 1
	if i < 0 || i >= len(lines) {
		return "", 0, false
	}
	line := lines[i]
	from, to := r.Col-1, r.EndCol-1
	if from < 0 || to > len(line) || from >= to {
		return "", 0, false
	}
	return line[from:to], from, true
}

// unquote strips a flow scalar's surrounding quotes, returning the inside and its offset.
func unquote(raw string) (string, int) {
	if len(raw) >= 2 && (raw[0] == '"' || raw[0] == '\'') && raw[len(raw)-1] == raw[0] {
		return raw[1 : len(raw)-1], 1
	}
	return raw, 0
}

// at converts a byte range on one line into a protocol token.
func tokenAt(lines []string, line, fromByte, toByte, kind int) token {
	start := utf16Column(lines[line-1], fromByte+1)
	end := utf16Column(lines[line-1], toByte+1)
	return token{line: line - 1, start: start, length: end - start, kind: kind}
}

// encode sorts and delta-encodes. The protocol requires ascending order and expresses each
// position relative to the token before it, so an unsorted list is not merely ugly — it paints
// the wrong ranges.
func encode(toks []token) []uint32 {
	out := make([]uint32, 0, len(toks)*5)
	sort.SliceStable(toks, func(a, b int) bool {
		if toks[a].line != toks[b].line {
			return toks[a].line < toks[b].line
		}
		return toks[a].start < toks[b].start
	})
	prevLine, prevStart := 0, 0
	for _, t := range toks {
		if t.length <= 0 {
			continue
		}
		deltaLine := t.line - prevLine
		deltaStart := t.start
		if deltaLine == 0 {
			deltaStart = t.start - prevStart
		}
		if deltaStart < 0 {
			continue // overlapping ranges cannot be encoded; drop rather than corrupt the rest
		}
		out = append(out, uint32(deltaLine), uint32(deltaStart), uint32(t.length), uint32(t.kind), 0)
		prevLine, prevStart = t.line, t.start
	}
	return out
}
