// Solver tests: demand ordering, cycle detection on contact, the joint
// fixpoint, collapse-or-keep, and determinism. See specs/recursive-type-inference.md.
package schematest

import (
	"strings"
	"testing"

	"genroc/internal/schema"
)

// inferAgainst returns a compute closure inferring expr against ctx, stripping
// the attached resolution context from the result the way the validation
// layer's inferShape does (a solved definition must not carry a copy of the
// defs map it lives in).
func inferAgainst(ctx schema.Schema, expr string) func() (schema.Schema, error) {
	return func() (schema.Schema, error) {
		res, err := ctx.Infer(expr)
		if err != nil {
			return schema.Schema{}, err
		}
		return res.WithoutDefs(), nil
	}
}

func mustGet(t *testing.T, defs schema.Defs, name string) schema.Schema {
	t.Helper()
	s, ok := defs.Get(name)
	if !ok {
		t.Fatalf("definition %q not solved", name)
	}
	return s
}

// TestSolverDemandOrder: A reads inside B, so B must be solved first even
// though A sorts first and is declared first.
func TestSolverDemandOrder(t *testing.T) {
	defs := schema.NewDefs()
	ctx := schema.Object().
		WithProperty("dep", schema.Ref("B"), true).
		WithDefs(defs)

	solver := schema.NewSolver(defs)
	solver.Declare("A", inferAgainst(ctx, "dep.x"))
	solver.Declare("B", func() (schema.Schema, error) {
		return schema.Object().WithProperty("x", schema.Type("integer"), true), nil
	})
	if err := solver.Solve(); err != nil {
		t.Fatalf("Solve: %v", err)
	}
	assertRaw(t, mustGet(t, defs, "A"), `{"type":"integer"}`)
}

// TestSolverSelfRecursionScalar: the classic accumulator — reading your own
// previous value with a ?? base case converges to the scalar type.
func TestSolverSelfRecursionScalar(t *testing.T) {
	defs := schema.NewDefs()
	ctx := schema.Object().
		WithProperty("prev", schema.Ref("S"), false). // optional: nullable wrapper
		WithDefs(defs)

	solver := schema.NewSolver(defs)
	solver.Declare("S", inferAgainst(ctx, "(prev ?? 0) + 1"))
	if err := solver.Solve(); err != nil {
		t.Fatalf("Solve: %v", err)
	}
	assertRaw(t, mustGet(t, defs, "S"), `{"type":"integer"}`)
}

// TestSolverStructuralKeep: passing the previous value through whole keeps the
// reference — the solved definition is a genuine recursive type, converging
// without widening.
func TestSolverStructuralKeep(t *testing.T) {
	defs := schema.NewDefs()
	ctx := schema.Object().
		WithProperty("prev", schema.Ref("S"), false).
		WithDefs(defs)

	solver := schema.NewSolver(defs)
	solver.Declare("S", func() (schema.Schema, error) {
		child, err := ctx.Infer("prev")
		if err != nil {
			return schema.Schema{}, err
		}
		return schema.Object().WithProperty("child", child.WithoutDefs(), true), nil
	})
	if err := solver.Solve(); err != nil {
		t.Fatalf("Solve: %v", err)
	}
	got := mustGet(t, defs, "S")
	raw, _ := got.MarshalJSON()
	if !strings.Contains(string(raw), `"$ref":"#/$defs/S"`) {
		t.Errorf("recursive ref not preserved: %s", raw)
	}
	// The kept recursive type must be a well-formed (productive) document.
	root := schema.Ref("S").WithDefs(defs)
	if err := root.CheckDoc(); err != nil {
		t.Errorf("kept recursive type fails CheckDoc: %v", err)
	}
}

// TestSolverDegenerateSelfCollapse: X = oneOf[$ref X, integer] is a coinductive
// tautology, not a recursive type — it collapses to integer exactly.
func TestSolverDegenerateSelfCollapse(t *testing.T) {
	defs := schema.NewDefs()
	solver := schema.NewSolver(defs)
	solver.Declare("X", func() (schema.Schema, error) {
		return schema.OneOf(schema.Ref("X"), schema.Type("integer")), nil
	})
	if err := solver.Solve(); err != nil {
		t.Fatalf("Solve: %v", err)
	}
	assertRaw(t, mustGet(t, defs, "X"), `{"type":"integer"}`)
}

// TestSolverDegenerateMutualCollapse: X = Y ∨ int, Y = X ∨ string — every
// member of a bare cycle collapses to the union of the cycle's non-cyclic
// remainders (here int|string), identically.
func TestSolverDegenerateMutualCollapse(t *testing.T) {
	defs := schema.NewDefs()
	solver := schema.NewSolver(defs)
	solver.Declare("X", func() (schema.Schema, error) {
		return schema.OneOf(schema.Ref("Y"), schema.Type("integer")), nil
	})
	solver.Declare("Y", func() (schema.Schema, error) {
		return schema.OneOf(schema.Ref("X"), schema.Type("string")), nil
	})
	if err := solver.Solve(); err != nil {
		t.Fatalf("Solve: %v", err)
	}
	x := mustGet(t, defs, "X").Canonicalize()
	y := mustGet(t, defs, "Y").Canonicalize()
	if !x.Equal(y) {
		t.Errorf("mutual bare cycle members differ:\n x: %s\n y: %s", mustJSON(t, x), mustJSON(t, y))
	}
	assertRaw(t, x, `{"type":["integer","string"]}`)
}

// TestSolverNoBaseCase: X defined only in terms of itself has no base case.
func TestSolverNoBaseCase(t *testing.T) {
	defs := schema.NewDefs()
	solver := schema.NewSolver(defs)
	solver.Declare("X", func() (schema.Schema, error) {
		return schema.Ref("X"), nil
	})
	err := solver.Solve()
	if err == nil {
		s, _ := defs.Get("X")
		t.Fatalf("expected a no-base-case error, got %s", mustJSON(t, s))
	}
	if !strings.Contains(err.Error(), "no base case") {
		t.Errorf("error %q does not mention the missing base case", err)
	}
}

// TestSolverClusterExpansion: the cycle is only discoverable through a chain of
// demands (A→B→C→A), and grows while already mid-fixpoint. All members are
// computational (object shapes with accumulator fields, like real output maps)
// and must converge jointly.
func TestSolverClusterExpansion(t *testing.T) {
	defs := schema.NewDefs()
	ctx := schema.Object().
		WithProperty("a", schema.Ref("A"), false).
		WithProperty("b", schema.Ref("B"), false).
		WithProperty("c", schema.Ref("C"), false).
		WithDefs(defs)

	// field builds an output-map-like compute: {<key>: <inferred expr type>}.
	field := func(key, expr string) func() (schema.Schema, error) {
		return func() (schema.Schema, error) {
			v, err := ctx.Infer(expr)
			if err != nil {
				return schema.Schema{}, err
			}
			return schema.Object().WithProperty(key, v.WithoutDefs(), true), nil
		}
	}

	solver := schema.NewSolver(defs)
	solver.Declare("A", field("x", "(b.y ?? 0) + 1"))
	solver.Declare("B", field("y", "(c.z ?? 0) + (a.x ?? 0)"))
	solver.Declare("C", field("z", "(a.x ?? 0) + 1"))
	if err := solver.Solve(); err != nil {
		t.Fatalf("Solve: %v", err)
	}
	for name, key := range map[string]string{"A": "x", "B": "y", "C": "z"} {
		assertRaw(t, mustGet(t, defs, name),
			`{"type":"object","properties":{"`+key+`":{"type":"integer"}},"required":["`+key+`"]}`)
	}
}

// TestSolverDeterminism: the same system solved from scratch twice — with
// different declaration orders — yields byte-identical definitions.
func TestSolverDeterminism(t *testing.T) {
	build := func(order []string) map[string]string {
		defs := schema.NewDefs()
		ctx := schema.Object().
			WithProperty("a", schema.Ref("A"), false).
			WithProperty("b", schema.Ref("B"), false).
			WithDefs(defs)
		computes := map[string]func() (schema.Schema, error){
			"A": inferAgainst(ctx, "(b ?? 1) + 0"),
			"B": inferAgainst(ctx, "(a ?? 2) + 0"),
		}
		solver := schema.NewSolver(defs)
		for _, n := range order {
			solver.Declare(n, computes[n])
		}
		if err := solver.Solve(); err != nil {
			t.Fatalf("Solve(%v): %v", order, err)
		}
		out := map[string]string{}
		for _, n := range []string{"A", "B"} {
			s, _ := defs.Get(n)
			out[n] = mustJSON(t, s.Canonicalize())
		}
		return out
	}
	first := build([]string{"A", "B"})
	second := build([]string{"B", "A"})
	for n := range first {
		if first[n] != second[n] {
			t.Errorf("definition %q differs across declaration orders:\n  %s\n  %s", n, first[n], second[n])
		}
	}
}

// TestSolverErrorPropagatesDemandChain: a failure inside a demanded definition
// carries the demanded name so the chain is attributable.
func TestSolverErrorPropagatesDemandChain(t *testing.T) {
	defs := schema.NewDefs()
	ctx := schema.Object().
		WithProperty("dep", schema.Ref("B"), true).
		WithDefs(defs)

	solver := schema.NewSolver(defs)
	solver.Declare("A", inferAgainst(ctx, "dep.x"))
	solver.Declare("B", inferAgainst(ctx, "missing.x")) // fails: no such context root
	err := solver.Solve()
	if err == nil {
		t.Fatal("expected an error from the failing definition")
	}
	if !strings.Contains(err.Error(), "B") {
		t.Errorf("error %q does not attribute the failing definition B", err)
	}
}
