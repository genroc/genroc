package defdoc

import "testing"

// UnescapeDirective is Directive's inverse, and this file holds it to exactly that: for every
// leaf that IS a directive, doubling the `$` must stop it being one, and unescaping must give
// the leaf back. A pair that drifts costs an author the ability to write the text at all — the
// bare spelling is claimed and refused, the escaped one keeps its doubling.

var directiveLeaves = []string{
	"$yaml: ./a.json",
	"$process: ./child.genroc.yaml",
	"$import: ./x.ts",
	"  $yaml: ./a.json name",
	"$a-b_9: anything at all",
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
		"$$yaml:",           // no argument, so the single-`$` form is no directive either
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
		})
	}
}

// What separates a directive from a ROUTING TARGET, which wears the same sigil. Only the space
// after the colon does, so a task id carrying a colon used to make `goto: $a:b` a resolver call
// -- and where a resolver happened to carry the name, it ran. YAML draws the same line.
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
