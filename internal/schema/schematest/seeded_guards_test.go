package schematest

import (
	"testing"

	"genroc/internal/schema"
)

// A refinement proved on the edge that routed here reaches the inferrer as a seeded guard,
// so a task can read what the `switch` case before it proved. Same mechanism the expression
// narrowing uses, keyed by the same rendered path — which is what keeps an element path
// narrowing that element and no other. specs/guard-narrowing.md.
func TestInferWithGuards(t *testing.T) {
	ctx := mustSchema(t, `{
		"properties": {
			"outputs": {"type":"object","properties":{
				"a": {"type":"object","properties":{
					"v": {"type":["integer","null"]},
					"w": {"type":["integer","null"]},
					"items": {"type":"array","items":{"type":["integer","null"]}}
				},"required":["v","w","items"]}
			},"required":["a"]}
		},
		"required": ["outputs"]
	}`)
	nonNullInt := schema.Type("integer")

	t.Run("unguarded, the read is still nullable", func(t *testing.T) {
		if _, err := ctx.Infer(`outputs.a.v > 2`); err == nil {
			t.Fatal("without the proof this must stay refused")
		}
	})

	t.Run("the proved path narrows", func(t *testing.T) {
		got, err := ctx.InferWithGuards(`outputs.a.v > 2`, map[string]schema.Schema{"outputs.a.v": nonNullInt})
		if err != nil {
			t.Fatalf("a proved path must be readable: %v", err)
		}
		assertJSON(t, got, `{"type":"boolean"}`)
	})

	t.Run("a sibling the edge did not prove is untouched", func(t *testing.T) {
		if _, err := ctx.InferWithGuards(`outputs.a.w > 2`, map[string]schema.Schema{"outputs.a.v": nonNullInt}); err == nil {
			t.Fatal("proving v says nothing about w")
		}
	})

	t.Run("one element narrows, its neighbours do not", func(t *testing.T) {
		g := map[string]schema.Schema{"outputs.a.items[0]": nonNullInt}
		if _, err := ctx.InferWithGuards(`outputs.a.items[0] > 2`, g); err != nil {
			t.Fatalf("the proved element must be readable: %v", err)
		}
		if _, err := ctx.InferWithGuards(`outputs.a.items[1] > 2`, g); err == nil {
			t.Fatal("proving element 0 must not be claimed for every element")
		}
	})

	t.Run("a path this context does not have is ignored", func(t *testing.T) {
		if _, err := ctx.InferWithGuards(`outputs.a.v ?? 0`, map[string]schema.Schema{"outputs.zz.q": nonNullInt}); err != nil {
			t.Fatalf("an inapplicable proof must not break inference: %v", err)
		}
	})
}

// The guards ride on the CONTEXT value, so every caller that already threads a context
// inherits them — `shape.Shape.CheckWith`, the template checker, the LSP. A guarded context
// that is copied, re-anchored to a defs pool, or handed down a shape tree must keep them, or
// the refinement silently stops applying somewhere between the edge and the expression.
func TestWithGuards_RidesOnTheContext(t *testing.T) {
	// A context WITH $defs, so the re-anchoring below actually rebuilds rather than
	// short-circuiting on an empty pool — which is how this silently passed at first.
	ctx := mustSchema(t, `{
		"$defs": {"AOut": {"type":"object","properties":{"v":{"type":["integer","null"]}},"required":["v"]}},
		"properties": {"outputs": {"type":"object","properties":{
			"a": {"$ref": "#/$defs/AOut"}
		},"required":["a"]}},
		"required": ["outputs"]
	}`)
	guarded := ctx.WithGuards(map[string]schema.Schema{"outputs.a.v": schema.Type("integer")})

	if _, err := guarded.Infer(`outputs.a.v > 2`); err != nil {
		t.Fatalf("a guarded context must narrow: %v", err)
	}
	if _, err := ctx.Infer(`outputs.a.v > 2`); err == nil {
		t.Fatal("the original context must be unchanged — WithGuards returns a copy")
	}
	// Re-anchoring to a defs pool is what shape.Infer does on the way down.
	if _, err := guarded.WithDefs(ctx.DefsHandle()).Infer(`outputs.a.v > 2`); err != nil {
		t.Fatalf("guards must survive WithDefs: %v", err)
	}
}
