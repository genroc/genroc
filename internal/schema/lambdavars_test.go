package schema

import "testing"

// A scope shaped like a task's: `rows` to map over, `name` to collide with a parameter.
func lambdaScope(t *testing.T) Schema {
	t.Helper()
	return mustParse(t, `{"type":"object","properties":{
		"rows":{"type":"array","items":{"type":"object",
			"properties":{"id":{"type":"integer"}},"required":["id"]}},
		"names":{"type":"array","items":{"type":"string"}},
		"name":{"type":"string"},
		"loose":{"type":"array"}},
		"required":["rows","names","name","loose"]}`)
}

func varSummary(t *testing.T, s Schema, expr, name string) string {
	t.Helper()
	v, ok := s.LambdaVars(expr)[name]
	if !ok {
		return ""
	}
	return v.Summary()
}

// The claim the whole thing rests on: what a reader is told a parameter is must be what
// inference bound it to. `map(xs, p => p)` returns an array OF that binding, so the two are
// comparable without reaching inside inferCall.
func TestLambdaVarsAgreesWithWhatInferenceBinds(t *testing.T) {
	s := lambdaScope(t)
	for _, src := range []string{"rows", "names", "names ?? []"} {
		bound, err := s.Infer("map(" + src + ", (p) => p)")
		if err != nil {
			t.Fatalf("infer over %s: %v", src, err)
		}
		elem, err := elementOf(resolveTolerant(bound))
		if err != nil {
			t.Fatalf("element of %s: %v", src, err)
		}
		got := varSummary(t, s, "map("+src+", (p) => { v: p })", "p")
		if got != elem.Summary() {
			t.Errorf("map(%s): parameter typed %q, inference bound %q", src, got, elem.Summary())
		}
	}
}

func TestLambdaVarsTypesTheIndexParameter(t *testing.T) {
	s := lambdaScope(t)
	if got := varSummary(t, s, "map(names, (v, i) => { at: i })", "i"); got != "integer" {
		t.Errorf("index parameter typed %q, want integer", got)
	}
}

// A nested map's source is an outer parameter, so the inner binding exists only if the walk
// descends under the outer one rather than re-reading the slot's own scope.
func TestLambdaVarsBindsANestedMapThroughTheOuterParameter(t *testing.T) {
	s := mustParse(t, `{"type":"object","properties":{
		"groups":{"type":"array","items":{"type":"object",
			"properties":{"tags":{"type":"array","items":{"type":"string"}}},
			"required":["tags"]}}},
		"required":["groups"]}`)

	if got := varSummary(t, s, "map(groups, (g) => map(g.tags, (tag) => { t: tag }))", "tag"); got != "string" {
		t.Errorf("inner parameter typed %q, want string", got)
	}
}

// Both exclusions. A wrong type at a position that answers correctly today is worse than the
// silence this feature exists to remove, and neither case is answerable without node offsets.
func TestLambdaVarsDropsAParameterItCannotPlace(t *testing.T) {
	s := lambdaScope(t)
	for _, tc := range []struct{ why, expr, param string }{
		{"two lambdas bind it", "map(rows, (x) => map(names, (x) => { v: x }))", "x"},
		{"it also names a context root", "map(names, (name) => { v: name })", "name"},
	} {
		if got := varSummary(t, s, tc.expr, tc.param); got != "" {
			t.Errorf("%s: parameter %q typed %q, want no answer", tc.why, tc.param, got)
		}
	}
}

// An element type guessed from a source that does not type is fiction, and the body reads
// against it. Both sources here are real mistakes an author makes mid-edit.
func TestLambdaVarsBindsNothingWithoutAnElementType(t *testing.T) {
	s := lambdaScope(t)
	for _, expr := range []string{
		"map(nope, (p) => { v: p })",  // no such root
		"map(loose, (p) => { v: p })", // an array declaring no element type
		"map(name, (p) => { v: p })",  // not an array at all
	} {
		if got := varSummary(t, s, expr, "p"); got != "" {
			t.Errorf("%s: parameter typed %q, want no answer", expr, got)
		}
	}
}

// A union context is alternative states and the parameter has a type in each, so the answer is
// their join — the same rule inference follows for the expression around it.
func TestLambdaVarsJoinsAcrossContextStates(t *testing.T) {
	s := mustParse(t, `{"anyOf":[
		{"type":"object","properties":{"xs":{"type":"array","items":{"type":"string"}}},"required":["xs"]},
		{"type":"object","properties":{"xs":{"type":"array","items":{"type":"integer"}}},"required":["xs"]}]}`)

	if got := varSummary(t, s, "map(xs, (p) => { v: p })", "p"); got != "integer|string" {
		t.Errorf("parameter typed %q, want integer|string", got)
	}
}

// The consuming half: a fragment of a lambda body types on its own only with the bindings
// carried on the context, which is how hover answers about one symbol inside a map.
func TestWithVarsTypesAFragmentOfALambdaBody(t *testing.T) {
	s := lambdaScope(t)
	expr := "map(rows, (r) => { n: r.id })"

	if _, err := s.Infer("r.id"); err == nil {
		t.Fatal("`r.id` typed against the bare scope; the binding must come from the expression")
	}
	got, err := s.WithVars(s.LambdaVars(expr)).Infer("r.id")
	if err != nil {
		t.Fatalf("infer `r.id` under the lambda's bindings: %v", err)
	}
	if got.Summary() != "integer" {
		t.Errorf("`r.id` typed %q, want integer", got.Summary())
	}
}
