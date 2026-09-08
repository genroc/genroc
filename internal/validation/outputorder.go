package validation

import (
	"fmt"

	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/shape"
)

// inferOutputs types every output-map task into defs (<id>_output), demand-driven: the
// solver orders work by exact dependency, detects recursion on contact, and fixpoints
// each cycle (null seed, re-infer, join). No separate dependency graph to drift.
// specs/recursive-type-inference.md.
func inferOutputs(tasks []*model.Task, scopes taskScopes, b *bag) error {
	solver := schema.NewSolver(scopes.defs)
	declared := false
	for _, s := range tasks {
		if !s.Output.Present() {
			continue
		}
		id := s.ID
		// The task loops iff it is its own predecessor: computeContextSets then
		// lists its own output among its available (optional) outputs.
		loops := scopes.loops(s)
		ctx, typed, err := scopes.outputMap(s)
		if err != nil {
			b.add(taskSlot(id, slotOutput), CodeExpression, err)
			b.poison(id)
			continue
		}
		node := s.Output.Raw
		label := fmt.Sprintf("task %q output", id)
		// An untyped result (fetch/external with no result_schema) cannot be exported: the
		// Roots hook turns a reference to the unavailable self.result into a clear message
		// rather than an opaque navigation failure.
		hooks := shape.CheckHooks{Roots: slotRoots(s, label, loops, typed, afterAction)}
		// A failed output slot recovers as {}, the unknown, rather than ending the pass:
		// inference is sequential, so returning the error here would cost every diagnostic
		// below it. The {} is what later tasks then read, and `bag.derived` drops the reads
		// it makes fail. specs/language-server.md §2, unknown-type.md.
		solver.Declare(id+"_output", func() (schema.Schema, error) {
			shp := shape.Shape{Raw: node, Name: label}
			out, err := shp.CheckWith(ctx, hooks)
			if err != nil {
				b.add(taskSlot(id, slotOutput), CodeExpression, err)
				b.poison(id)
				return schema.Schema{}, nil
			}
			return out, nil
		})
		declared = true
	}
	if !declared {
		return nil
	}
	return solver.Solve()
}
