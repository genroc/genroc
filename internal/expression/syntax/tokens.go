package syntax

import (
	"github.com/expr-lang/expr/file"
	"github.com/expr-lang/expr/parser/lexer"
)

// The lexical view of an expression, which the AST does not carry: nodes have no offsets
// (specs/language-server.md §6), so anything that has to point AT a piece of source — a
// semantic token, a squiggle under one symbol — reads the token stream instead.

type TokenKind int

const (
	KindIdentifier TokenKind = iota
	KindNumber
	KindString
	KindOperator
	KindBracket
)

// Token is one lexical unit and the byte range it occupies in the source it was lexed from.
type Token struct {
	Kind TokenKind
	Text string
	From int // byte offset, inclusive
	To   int // byte offset, exclusive
}

// Tokens lexes src. A source that does not PARSE still tokenizes, which is what a buffer being
// typed in needs; a source that does not lex yields nothing rather than a partial stream.
func Tokens(src string) []Token {
	raw, err := lexer.Lex(file.NewSource(src))
	if err != nil {
		return nil
	}
	out := make([]Token, 0, len(raw))
	for _, t := range raw {
		kind, ok := kindOf(t.Kind)
		if !ok {
			continue
		}
		// A string token's Value is the DECODED text, so its length is not the span it
		// occupies; From/To are the only trustworthy extent.
		out = append(out, Token{Kind: kind, Text: t.Value, From: t.From, To: t.To})
	}
	return out
}

func kindOf(k lexer.Kind) (TokenKind, bool) {
	switch k {
	case lexer.Identifier:
		return KindIdentifier, true
	case lexer.Number:
		return KindNumber, true
	case lexer.String, lexer.Bytes:
		return KindString, true
	case lexer.Operator:
		return KindOperator, true
	case lexer.Bracket:
		return KindBracket, true
	}
	return 0, false // EOF
}
