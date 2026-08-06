package syntax

import (
	"fmt"
	"strings"
	"testing"
)

// Shared fixtures for the parser tests. Every test body in parser_test.go and
// parser_edge_test.go is a single call to one of the assert* helpers below, so a
// failure names the behaviour rather than a table row.

// parseCase is a named row for a homogeneous sweep: `name` becomes the subtest
// name, so any single case runs with `go test -run 'TestX/name'`.
type parseCase struct{ name, in, want string }

// -----------------------------------------------------------------------------
// Tree rendering
// -----------------------------------------------------------------------------

// dump renders a tree in a compact prefix form so grouping is asserted exactly.
func dump(n Node) string {
	switch x := n.(type) {
	case *IntNode:
		return x.Text
	case *FloatNode:
		return x.Text
	case *StringNode:
		return fmt.Sprintf("%q", x.Value)
	case *BoolNode:
		return fmt.Sprintf("%t", x.Value)
	case *NullNode:
		return "null"
	case *IdentNode:
		return x.Name
	case *MemberNode:
		return fmt.Sprintf("%s.%s", dump(x.Base), x.Name)
	case *IndexNode:
		return fmt.Sprintf("%s[%d]", dump(x.Base), x.Index)
	case *KeyNode:
		return fmt.Sprintf("%s[%s]", dump(x.Base), dump(x.Key))
	case *ArrayNode:
		return "[" + strings.Join(dumpAll(x.Items), " ") + "]"
	case *ObjectNode:
		parts := make([]string, len(x.Keys))
		for i, k := range x.Keys {
			parts[i] = fmt.Sprintf("%s:%s", k, dump(x.Values[i]))
		}
		return "{" + strings.Join(parts, " ") + "}"
	case *LambdaNode:
		p := x.Param
		if x.IndexParam != "" {
			p += "," + x.IndexParam
		}
		return fmt.Sprintf("(\\%s -> %s)", p, dump(x.Body))
	case *CallNode:
		return fmt.Sprintf("%s(%s)", x.Name, strings.Join(dumpAll(x.Args), " "))
	case *UnaryNode:
		return fmt.Sprintf("(%s %s)", x.Op, dump(x.Operand))
	case *BinaryNode:
		return fmt.Sprintf("(%s %s %s)", x.Op, dump(x.Left), dump(x.Right))
	case *CondNode:
		return fmt.Sprintf("(if %s %s %s)", dump(x.Cond), dump(x.Then), dump(x.Else))
	}
	return "?"
}

func dumpAll(ns []Node) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = dump(n)
	}
	return out
}

// -----------------------------------------------------------------------------
// Parse outcomes
// -----------------------------------------------------------------------------

// parseOK fails the test on a parse error and returns the tree.
func parseOK(t *testing.T, src string) Node {
	t.Helper()
	n, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): unexpected error:\n%v", src, err)
	}
	return n
}

// parseErr fails the test if src parses, and returns the error otherwise.
func parseErr(t *testing.T, src string) error {
	t.Helper()
	n, err := Parse(src)
	if err == nil {
		t.Fatalf("Parse(%q): expected an error, got %s", src, dump(n))
	}
	return err
}

// assertParses asserts src parses to exactly the given tree.
func assertParses(t *testing.T, src, want string) {
	t.Helper()
	if got := dump(parseOK(t, src)); got != want {
		t.Errorf("Parse(%q)\n got: %s\nwant: %s", src, got, want)
	}
}

// assertSameTree asserts two spellings produce the same tree.
func assertSameTree(t *testing.T, a, b string) {
	t.Helper()
	da, db := dump(parseOK(t, a)), dump(parseOK(t, b))
	if da != db {
		t.Errorf("Parse(%q) = %s differs from Parse(%q) = %s", a, da, b, db)
	}
}

// assertRejected asserts src fails to parse, without constraining the message.
func assertRejected(t *testing.T, src string) {
	t.Helper()
	if n, err := Parse(src); err == nil {
		t.Errorf("Parse(%q): expected an error, got %s", src, dump(n))
	}
}

// assertParseError asserts src fails to parse with a message containing want.
func assertParseError(t *testing.T, src, want string) {
	t.Helper()
	if err := parseErr(t, src); !strings.Contains(err.Error(), want) {
		t.Errorf("Parse(%q): error %q does not contain %q", src, err.Error(), want)
	}
}

// Literal node assertions. dump renders FloatNode with %g, so `1e3` and `1.` both print as "1"
// and a dump-only assertion would miss an IntNode/FloatNode mixup — inference reports integer
// and number as different types, so these assert the node type too.

// assertIntLiteral checks the normalised decimal text of an integer literal.
// Text rather than a Go int: literals are arbitrary precision, so there is no
// int to compare against for a value past int64.
func assertIntLiteral(t *testing.T, src string, want string) {
	t.Helper()
	n := parseOK(t, src)
	x, ok := n.(*IntNode)
	if !ok {
		t.Fatalf("Parse(%q): got %T, want *IntNode (dump: %s)", src, n, dump(n))
	}
	if x.Text != want {
		t.Errorf("Parse(%q): IntNode = %s, want %s", src, x.Text, want)
	}
}

func assertFloatLiteral(t *testing.T, src string, want string) {
	t.Helper()
	n := parseOK(t, src)
	x, ok := n.(*FloatNode)
	if !ok {
		t.Fatalf("Parse(%q): got %T, want *FloatNode (dump: %s)", src, n, dump(n))
	}
	if x.Text != want {
		t.Errorf("Parse(%q): FloatNode = %s, want %s", src, x.Text, want)
	}
}

func assertStringLiteral(t *testing.T, src, want string) {
	t.Helper()
	n := parseOK(t, src)
	x, ok := n.(*StringNode)
	if !ok {
		t.Fatalf("Parse(%q): got %T, want *StringNode", src, n)
	}
	if x.Value != want {
		t.Errorf("Parse(%q): value %q, want %q", src, x.Value, want)
	}
}

// -----------------------------------------------------------------------------
// Error quality
// -----------------------------------------------------------------------------

// caretOffset extracts the column the caret points at. failAt writes three
// lines — message, two-space-indented source, two-space-indented caret — so the
// offset is the caret's indentation minus the two-space gutter.
func caretOffset(t *testing.T, errText string) int {
	t.Helper()
	lines := strings.Split(errText, "\n")
	if len(lines) < 3 {
		t.Fatalf("error is not in the three-line source-quoting form:\n%s", errText)
	}
	last := lines[len(lines)-1]
	if strings.TrimSpace(last) != "^" {
		t.Fatalf("last line is not a lone caret: %q", last)
	}
	return len(last) - len(strings.TrimLeft(last, " ")) - 2
}

// assertCaretAt asserts the caret sits under the given 0-based byte offset into
// the source. `why` names the token that should have been blamed.
func assertCaretAt(t *testing.T, src string, want int, why string) {
	t.Helper()
	err := parseErr(t, src)
	if got := caretOffset(t, err.Error()); got != want {
		t.Errorf("Parse(%q): caret at %d, want %d (%s)\n%s", src, got, want, why, err.Error())
	}
}

// assertCaretWithinSource asserts the caret stays inside the quoted source; a
// caret past the end would print a ragged line, so failAt clamps it.
func assertCaretWithinSource(t *testing.T, src string) {
	t.Helper()
	err := parseErr(t, src)
	if got := caretOffset(t, err.Error()); got < 0 || got > len(src) {
		t.Errorf("Parse(%q): caret at %d, outside [0,%d]\n%s", src, got, len(src), err.Error())
	}
}

// assertErrorQuotesSource asserts the error is exactly message/source/caret,
// with the source verbatim on its own line, no rewriting (the `let` translation
// used by the conformance oracle must never leak) and no truncation.
func assertErrorQuotesSource(t *testing.T, src string) {
	t.Helper()
	got := parseErr(t, src).Error()
	if !strings.Contains(got, "\n  "+src+"\n") {
		t.Errorf("Parse(%q): source is not quoted on its own line, got:\n%s", src, got)
	}
	if strings.Contains(got, "let ") || strings.Contains(got, "#") {
		t.Errorf("Parse(%q): error leaks a rewritten form:\n%s", src, got)
	}
	if lines := strings.Split(got, "\n"); len(lines) != 3 {
		t.Errorf("Parse(%q): want message/source/caret, got %d lines:\n%s", src, len(lines), got)
	}
}

// assertSingleLineError asserts a lexer failure is truncated to its first line,
// so expr-lang's own multi-line source rendering never reaches an API response.
func assertSingleLineError(t *testing.T, src string) {
	t.Helper()
	if got := parseErr(t, src).Error(); strings.Contains(got, "\n") {
		t.Errorf("Parse(%q): lexer error should be one line, got:\n%s", src, got)
	}
}

// assertLexErrorContains is assertParseError plus the single-line guarantee.
// The parser's own three-line caret form is exempt.
func assertLexErrorContains(t *testing.T, src, want string) {
	t.Helper()
	err := parseErr(t, src)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("Parse(%q): error %q does not contain %q", src, err.Error(), want)
	}
	if strings.Contains(err.Error(), "\n") && !strings.Contains(err.Error(), "^") {
		t.Errorf("Parse(%q): lexer error should be one line, got:\n%s", src, err.Error())
	}
}

// -----------------------------------------------------------------------------
// Robustness
// -----------------------------------------------------------------------------

// assertEveryPrefixHandled parses every prefix of src. Each is malformed in some
// way; none may panic, hang, or come back as a nil node with a nil error. This
// is the cheapest guard against a production reaching for a token that is not
// there.
func assertEveryPrefixHandled(t *testing.T, src string) {
	t.Helper()
	for i := 0; i <= len(src); i++ {
		prefix := src[:i]
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Parse(%q) panicked: %v", prefix, r)
				}
			}()
			n, err := Parse(prefix)
			if err == nil && n == nil {
				t.Errorf("Parse(%q): nil node with nil error", prefix)
			}
		}()
	}
}
