// Package selector lexes the tokens `genctl compat --check` / `--ignore` take — a slot named
// by segments, scoped leftward by process and task:
//
//	order_proc:charge:fetch.result.fee
//	"odd:name":"a.task":external.result
//
// It reads the sequence and nothing more: names and the delimiters between them, in the order
// written. Which delimiter may follow what — that a colon scopes and a dot navigates, that a
// scope precedes the member — is the caller's grammar, decided against the vocabulary the
// caller knows.
//
// A whole flag value goes through ONE pass, its commas lexed alongside the rest. Lexing the
// list first and each token after would refuse `"odd,name":charge`, since the quoted name
// ends at a colon the outer pass does not know about — and a name may hold a comma for
// exactly the reason it may hold a colon.
//
// The syntax is NOT the expression language's accessor form, deliberately: a selector names
// report slots, not values, so it has no indices and no computed keys, and nothing it spells
// is ever evaluated.
package selector

import (
	"fmt"
	"strings"
)

// Lex reads s into an alternating sequence: element 0 is a name, element 1 the delimiter
// that followed it, element 2 the next name, and so on. The length is therefore always odd,
// and a name always sits between two delimiters — empty when nothing was written there, so a
// trailing `order_proc:` ends in an empty name rather than losing its scope.
//
// Quoting removes a delimiter's meaning and nothing else: `"a:b":c` is the name `a:b`, a
// colon, and the name `c`. A quote may only OPEN a name and must CLOSE it at a delimiter —
// `a"b"c` is refused, because one name with two spellings is something every later comparison
// would have to know about. `\"` and `\\` are the only escapes.
//
// An empty name comes back empty however it was written, so `x.""` and `x..` are one case to
// the caller. That is deliberate: the only empty name worth a meaning is the trailing one,
// and a property literally named "" is not selectable.
func Lex(s string, delims ...byte) ([]string, error) {
	isDelim := func(b byte) bool {
		for _, d := range delims {
			if b == d {
				return true
			}
		}
		return false
	}

	var out []string
	for i := 0; ; {
		start := i
		var name string
		if i < len(s) && s[i] == '"' {
			unquoted, next, err := unescape(s, i)
			if err != nil {
				return nil, err
			}
			name, i = unquoted, next
			if i < len(s) && !isDelim(s[i]) {
				return nil, fmt.Errorf("invalid segment %q: a quoted name must end at a delimiter", s[start:])
			}
		} else {
			for i < len(s) && !isDelim(s[i]) {
				if s[i] == '"' || s[i] == '\\' {
					return nil, fmt.Errorf("invalid segment %q: quote the whole name to use %q or %q in it",
						s[start:], `"`, `\`)
				}
				i++
			}
			name = s[start:i]
		}
		out = append(out, name)
		if i == len(s) {
			return out, nil
		}
		out = append(out, s[i:i+1])
		i++
	}
}

// unescape reads the quoted name opening at start, returning it and the offset just past the
// closing quote. `\"` and `\\` are the only escapes: a selector names things an operator
// typed, so a character with no keyboard spelling has nothing to gain from one, and every
// escape is a second way to write a name.
func unescape(s string, start int) (string, int, error) {
	var b strings.Builder
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 >= len(s) {
				return "", 0, fmt.Errorf("invalid segment %q: trailing backslash", s[start:])
			}
			i++
			if s[i] != '"' && s[i] != '\\' {
				return "", 0, fmt.Errorf(`invalid segment %q: \%c is not an escape`, s[start:], s[i])
			}
			b.WriteByte(s[i])
		case '"':
			return b.String(), i + 1, nil
		default:
			b.WriteByte(s[i])
		}
	}
	return "", 0, fmt.Errorf("invalid segment %q: unterminated quote", s[start:])
}
