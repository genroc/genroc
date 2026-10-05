package defdoc

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// A drifted Directive/UnescapeDirective pair leaves an author no way to write the text: the bare
// spelling is claimed and refused, the escaped one keeps its doubling.

var directiveLeaves = []string{
	"$yaml: ./a.json",
	"$process: ./child.genroc.yaml",
	"$import: ./x.ts",
	"  $yaml: ./a.json name",
	"$a-b_9: anything at all",
	"$now:",
	"  $now:  ",
}

func TestUnescapingIsTheInverseOfRecognising(t *testing.T) {
	for _, leaf := range directiveLeaves {
		t.Run(leaf, func(t *testing.T) {
			if _, _, ok := Directive(leaf); !ok {
				t.Fatalf("premise gone: %q is not a directive, so there is nothing to escape", leaf)
			}
			// The escape is written by doubling the leaf's own `$`, which is what an author does.
			i := 0
			for leaf[i] != '$' {
				i++
			}
			escaped := leaf[:i] + "$" + leaf[i:]
			if got, ok := EscapeDirective(leaf); !ok || got != escaped {
				t.Fatalf("EscapeDirective(%q) = %q, %v; want the author's own doubling %q", leaf, got, ok, escaped)
			}
			if _, _, ok := Directive(escaped); ok {
				t.Fatalf("%q is still claimed as a directive, so the escape does not escape", escaped)
			}
			got, ok := UnescapeDirective(escaped)
			if !ok {
				t.Fatalf("%q was not recognised as an escaped directive", escaped)
			}
			if got != leaf {
				t.Fatalf("unescaped to %q, want the original %q", got, leaf)
			}
		})
	}
}

// Everything the pass must leave exactly as written. A doubling that is not an escape is data,
// and collapsing it would corrupt the value it sits in.
func TestUnescapeLeavesEverythingElseAlone(t *testing.T) {
	for _, leaf := range []string{
		"$yaml: ./a.json",   // a directive: claimed, not escaped
		"cost: $$5",         // a doubling that is not at the start
		"$$5.00",            // no name after the dollars
		"$: input.x",        // an expression
		"${ input.x }",      // an interpolation
		"$$$yaml: ./a.json", // one too many: the single-`$` form is still not a directive
		"",
	} {
		t.Run(leaf, func(t *testing.T) {
			got, ok := UnescapeDirective(leaf)
			if ok || got != leaf {
				t.Fatalf("rewrote %q to %q (escaped=%v); it is data, not an escape", leaf, got, ok)
			}
			if _, _, isDirective := Directive(leaf); !isDirective {
				if got, ok := EscapeDirective(leaf); ok || got != leaf {
					t.Fatalf("EscapeDirective rewrote %q to %q; only a directive is escaped", leaf, got)
				}
			}
		})
	}
}

func TestOnlyTheSpaceSeparatesADirectiveFromARoutingTarget(t *testing.T) {
	for _, leaf := range []string{
		"$a:b",           // a goto to a task called `a:b`
		"$yaml:b",        // the same, where `yaml` IS a registered resolver
		"$scheme://host", // a url someone templated by hand
		"$import:./x.ts", // a directive written without the space
		"$tick",          // an ordinary routing target: no colon at all
	} {
		t.Run(leaf, func(t *testing.T) {
			if name, arg, ok := Directive(leaf); ok {
				t.Fatalf("claimed as a directive %q with argument %q; nothing here asks for a resolver", name, arg)
			}
		})
	}
	// Or the test would pass by claiming nothing at all.
	if _, _, ok := Directive("$import: ./x.ts"); !ok {
		t.Fatal("the spaced form is no longer a directive either, so this proves nothing")
	}
}

func TestAnEmptyArgumentIsADirective(t *testing.T) {
	name, arg, ok := Directive("$now:")
	if !ok || name != "now" || arg != "" {
		t.Fatalf(`Directive("$now:") = %q, %q, %v; a resolver needs no argument at all`, name, arg, ok)
	}
}

func TestSplitArgs(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []Arg
	}{
		{"", nil},
		{"   ", nil},
		{"./a.ts", []Arg{{"./a.ts", 0, 6}}},
		{"./q.sql  dialect=pg", []Arg{{"./q.sql", 0, 7}, {"dialect=pg", 9, 19}}},
		{"a\tb", []Arg{{"a", 0, 1}, {"b", 2, 3}}},
		{"'./my file.sql' --dry", []Arg{{"./my file.sql", 0, 15}, {"--dry", 16, 21}}},
		{"a '' b", []Arg{{"a", 0, 1}, {"", 2, 4}, {"b", 5, 6}}},
		{"a'b c'd", []Arg{{"ab cd", 0, 7}}},
		{"'it''s'", []Arg{{"its", 0, 7}}},
		{"$HOME ~ *.ts #x a|b;c", []Arg{{"$HOME", 0, 5}, {"~", 6, 7}, {"*.ts", 8, 12}, {"#x", 13, 15}, {"a|b;c", 16, 21}}},
		{`'C:\x' 'say "hi"'`, []Arg{{`C:\x`, 0, 6}, {`say "hi"`, 7, 17}}},
		{"./ü.ts", []Arg{{"./ü.ts", 0, 7}}},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := SplitArgs(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("SplitArgs(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSplitArgsRefuses(t *testing.T) {
	for _, tc := range []struct {
		in           string
		offset       int
		unterminated bool
		partial      []Arg
	}{
		{"a 'b c", 2, true, []Arg{{"a", 0, 1}, {"b c", 2, 6}}},
		{"x'", 1, true, []Arg{{"x", 0, 2}}},
		{`./my\ file.sql`, 4, false, nil},
		{`a "b c"`, 2, false, []Arg{{"a", 0, 1}}},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := SplitArgs(tc.in)
			var ae *ArgError
			if !errors.As(err, &ae) {
				t.Fatalf("SplitArgs(%q) = %+v, %v; want an *ArgError", tc.in, got, err)
			}
			if ae.Offset != tc.offset || ae.Unterminated != tc.unterminated {
				t.Fatalf("error at %d (unterminated=%v), want %d (%v): the offset is where the editor puts it",
					ae.Offset, ae.Unterminated, tc.offset, tc.unterminated)
			}
			if tc.unterminated && !reflect.DeepEqual(got, tc.partial) {
				t.Fatalf("partial words %+v, want %+v: the open word is what an editor completes", got, tc.partial)
			}
		})
	}
}

func TestArgValuesIsNeverNil(t *testing.T) {
	got, err := ArgValues("")
	if err != nil || got == nil {
		t.Fatalf("ArgValues(\"\") = %#v, %v; a manifest sends args: [], never null", got, err)
	}
}

// Over an alphabet sh gives no other meaning, SplitArgs must split exactly as /bin/sh does: that
// is the claim the quoting rules rest on, checked against sh rather than a reading of POSIX.
func TestSplitArgsAgreesWithSh(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh to compare against")
	}
	const alphabet = "ab./-_=:,@+% \t'"
	rng := rand.New(rand.NewSource(1))
	var inputs []string
	for len(inputs) < 2000 {
		b := make([]byte, rng.Intn(12))
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		if _, err := SplitArgs(string(b)); err == nil {
			inputs = append(inputs, string(b))
		}
	}

	// One script for every case, since a process per case is most of the test's time.
	var script strings.Builder
	for _, in := range inputs {
		fmt.Fprintf(&script, "set -- %s\nprintf '%%s\\0' \"$#\"\nfor a; do printf '%%s\\0' \"$a\"; done\n", in)
	}
	out, err := exec.Command(sh, "-c", script.String()).Output()
	if err != nil {
		t.Fatalf("sh: %v", err)
	}
	fields := bytes.Split(out, []byte{0})
	for _, in := range inputs {
		var n int
		fmt.Sscan(string(fields[0]), &n)
		shWords := make([]string, n)
		for i := range shWords {
			shWords[i] = string(fields[1+i])
		}
		fields = fields[1+n:]
		got, _ := ArgValues(in)
		if !reflect.DeepEqual(got, shWords) {
			t.Fatalf("SplitArgs(%q) = %q, sh splits it %q", in, got, shWords)
		}
	}

	// And where SplitArgs refuses an open quote, sh refuses it too.
	for _, in := range []string{"'", "a 'b", "a'b c"} {
		if err := exec.Command(sh, "-c", "set -- "+in).Run(); err == nil {
			t.Fatalf("sh accepts %q, which SplitArgs refuses as unterminated", in)
		}
	}
}
