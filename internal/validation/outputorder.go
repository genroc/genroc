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
	checked := map[string]bool{}
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
		// A declaration is the task's PUBLISHED output type, so it is what lands in the pool
		// and what `outputs.<id>` reads downstream. It also ends the recursion for free: a
		// declared type is concrete, so nothing below it has to be solved.
		declaredOut := s.OutputSchema
		if declaredOut != nil {
			_, declaredHooks := declaredShape(node, declaredOut, label)
			hooks.Result = declaredHooks.Result
		}
		solver.Declare(id+"_output", func() (schema.Schema, error) {
			shp := shape.Shape{Raw: node, Name: label, Schema: declaredOut, Conformed: declaredOut != nil}
			out, err := shp.CheckWith(ctx, hooks)
			if err != nil {
				b.add(taskSlot(id, slotOutput), CodeExpression, err)
				b.poison(id)
				return schema.Schema{}, nil
			}
			checked[id] = true
			return published(out, declaredOut), nil
		})
		declared = true
	}
	if !declared {
		return nil
	}
	if err := solver.Solve(); err != nil {
		return err
	}
	// A DECLARED output goes back into the pool as written, prose and all. The solver stores
	// what it computes CANONICAL — the fixpoint compares canonical forms — and canonical means
	// no `description`: nothing lost on an inferred type, and the one thing an imported schema
	// was worth importing for on a declaration. The type is the same either way; only the
	// annotation comes back. A slot whose check failed keeps its `{}`, which is what the
	// diagnostics' poison rule expects to find there.
	for _, s := range tasks {
		if s.OutputSchema != nil && checked[s.ID] {
			scopes.defs.Set(s.ID+"_output", *s.OutputSchema)
		}
	}
	return nil
}
