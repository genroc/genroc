package validation

import (
	"fmt"
	"slices"

	"genroc/internal/expression"
	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/shape"
)

// inferOutputs types every output-map task into defs (<id>_output), demand-driven: the
// solver orders work by exact dependency, detects recursion on contact, and fixpoints
// each cycle (null seed, re-infer, join). No separate dependency graph to drift.
// specs/recursive-type-inference.md.
func inferOutputs(tasks []*model.Task, taskSchemas map[string]TaskSchemas, processInput, configSchema schema.Schema,
	defs schema.Defs, required, optional map[string][]string, mustErr, mayErr map[string]bool) error {

	solver := schema.NewSolver(defs)
	declared := false
	for _, s := range tasks {
		if !s.Output.Present() {
			continue
		}
		id := s.ID
		base := contextSchema(required[id], optional[id], taskSchemas, processInput, configSchema, mustErr[id], mayErr[id])
		// The task loops iff it is its own predecessor: computeContextSets then
		// lists its own output among its available (optional) outputs.
		loops := slices.Contains(optional[id], id) || slices.Contains(required[id], id)
		resultType, typed, err := actionResultType(s, defs)
		if err != nil {
			return fmt.Errorf("task %q: %w", id, err)
		}
		ctx := outputMapContext(base, resultType, typed, id, loops).WithDefs(defs)
		node := s.Output.Raw
		label := fmt.Sprintf("task %q output", id)
		// An untyped result (fetch/external with no result_schema) cannot be exported: the
		// Roots hook turns a reference to the unavailable self.result into a clear message
		// rather than an opaque navigation failure.
		hooks := shape.CheckHooks{}
		if !typed {
			hooks.Roots = func(refs expression.Roots) error {
				if refs.SelfResult {
					return fmt.Errorf("task %q: output references self.result, but the action has no result_schema — add a result_schema to type the response, or `result_schema: {}` (the top type) to export it opaquely for a caller to narrow", id)
				}
				return nil
			}
		}
		solver.Declare(id+"_output", func() (schema.Schema, error) {
			shp := shape.Shape{Raw: node, Name: label}
			return shp.CheckWith(ctx, hooks)
		})
		declared = true
	}
	if !declared {
		return nil
	}
	return solver.Solve()
}
