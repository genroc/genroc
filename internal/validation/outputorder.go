package validation

import (
	"fmt"

	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/shape"
)

// inferOutputs types every output into defs (<id>_output), demand-driven: the solver orders by
// exact dependency and fixpoints each cycle, so no dependency graph can drift.
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
		// The hook words a reference to an untyped self.result as the rule it breaks.
		hooks := shape.CheckHooks{Roots: slotRoots(s, label, loops, typed, afterAction)}
		// A failed slot recovers as {} rather than ending the pass (specs/language-server.md §2).
		// A declaration lands in the pool as the published type, and ends any recursion: it is
		// concrete.
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
	// Declared outputs go back as written: the solver stores canonical forms, which drop
	// `description`. A failed slot keeps the `{}` the poison rule expects. See CLAUDE.md.
	for _, s := range tasks {
		if s.OutputSchema != nil && checked[s.ID] {
			scopes.defs.Set(s.ID+"_output", *s.OutputSchema)
		}
	}
	return nil
}
