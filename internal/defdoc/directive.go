package defdoc

import (
	"fmt"
	"regexp"
	"strings"
)

// directiveRe cannot match `$$` (the second character must be a letter), leaving the escape to the
// template layer. An argument needs the SPACE after the colon: it alone separates a directive from
// a `goto: $a:b` routing target. specs/source-resolution.md §Directive syntax.
var directiveRe = regexp.MustCompile(`^\s*\$([a-zA-Z][a-zA-Z0-9_-]*):(?:[ \t]+(\S.*?))?\s*$`)

// Directive splits a source-resolution directive into its resolver name and verbatim argument,
// which may be empty (`$now:`). SplitArgs reads the argument's words.
// It lives here because defdoc must recognise a `<<` directive before genctl sees it; whether a
// name is REGISTERED is genctl's question alone.
func Directive(s string) (name, argument string, ok bool) {
	m := directiveRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// UnescapeDirective drops the doubling from a leaf written `$$name: argument`, an escaped literal
// `$name: argument`. It inverts Directive by construction; a second regexp here would drift.
func UnescapeDirective(s string) (string, bool) {
	i := strings.IndexByte(s, '$')
	if i < 0 || i+1 >= len(s) || s[i+1] != '$' || strings.TrimSpace(s[:i]) != "" {
		return s, false
	}
	un := s[:i] + s[i+1:]
	if _, _, ok := Directive(un); !ok {
		return s, false
	}
	return un, true
}

// EscapeDirective is UnescapeDirective's inverse: a leaf that reads as a directive gets its `$`
// doubled, so text copied out of an unescaped document stays text.
func EscapeDirective(s string) (string, bool) {
	if _, _, ok := Directive(s); !ok {
		return s, false
	}
	i := strings.IndexByte(s, '$')
	return s[:i] + "$" + s[i:], true
}

// Arg is one word of a directive's argument. Start and End are byte offsets into the argument as
// written, quotes included.
type Arg struct {
	Value      string
	Start, End int
}

// ArgError is an argument SplitArgs refuses, at a byte offset into it. Unterminated is the one an
// editor expects mid-typing.
type ArgError struct {
	Offset       int
	Msg          string
	Unterminated bool
}

func (e *ArgError) Error() string {
	return fmt.Sprintf("column %d of the argument: %s", e.Offset+1, e.Msg)
}

// SplitArgs splits an argument into words the way sh does with single quotes and nothing else:
// blanks separate, '…' is literal, and `"` and `\` are refused outside it so a later move to sh's
// full quoting changes no directive. specs/source-resolution.md §Directive syntax.
//
// On an unterminated quote the words so far come back WITH the error, the open one last.
func SplitArgs(argument string) ([]Arg, error) {
	var out []Arg
	var word strings.Builder
	start, inWord := 0, false
	for i := 0; i < len(argument); i++ {
		switch c := argument[i]; c {
		case ' ', '\t':
			if inWord {
				out = append(out, Arg{Value: word.String(), Start: start, End: i})
				word.Reset()
				inWord = false
			}
		case '\'':
			if !inWord {
				start, inWord = i, true
			}
			end := strings.IndexByte(argument[i+1:], '\'')
			if end < 0 {
				word.WriteString(argument[i+1:])
				out = append(out, Arg{Value: word.String(), Start: start, End: len(argument)})
				return out, &ArgError{Offset: i, Msg: "unterminated ' quote", Unterminated: true}
			}
			word.WriteString(argument[i+1 : i+1+end])
			i += end + 1
		case '"', '\\':
			return out, &ArgError{Offset: i, Msg: fmt.Sprintf("%c is reserved outside single quotes; "+
				"quote the word instead: '...'", c)}
		default:
			if !inWord {
				start, inWord = i, true
			}
			word.WriteByte(c)
		}
	}
	if inWord {
		out = append(out, Arg{Value: word.String(), Start: start, End: len(argument)})
	}
	return out, nil
}

// ArgValues is SplitArgs without the offsets, never nil.
func ArgValues(argument string) ([]string, error) {
	args, err := SplitArgs(argument)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = a.Value
	}
	return out, nil
}
