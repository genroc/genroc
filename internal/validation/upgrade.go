package validation

// An upgrade's instance sits at one task, so one layer of the target version conforms its state
// -- unlike compat, which speaks about every state at once. specs/version-compatibility.md s1.

import (
	"fmt"

	"genroc/internal/model"
	"genroc/internal/schema"
)

// MigrateState conforms an instance's stored state to `to` and returns the state to write; a
// dropped task's output is pruned, keys outside the layer (engine bookkeeping) pass through.
// Exactly, not Strict: a default filled into a half-run instance contradicts what ran without it.
func MigrateState(to *model.ProcessDefinition, task string, state map[string]any, load func(hash string) (any, error)) (map[string]any, error) {
	if task == "" {
		return nil, fmt.Errorf("instance holds no task to resume at")
	}
	// Materialized first: the conform cannot normalize inside an object it has not loaded. The
	// write re-cuts identical content to the same hashes, so nothing churns. specs/lazy-context.md.
	materialized, err := model.NewContext(state, load, nil).Materialize(state)
	if err != nil {
		return nil, fmt.Errorf("resolve the externalized values at task %q: %w", task, err)
	}
	state, _ = materialized.(map[string]any)

	layers, err := TaskContexts(to)
	if err != nil {
		return nil, fmt.Errorf("analyse %q: %w", to.Name, err)
	}
	layer, ok := layers[task]
	if !ok {
		return nil, fmt.Errorf("task %q does not exist in %s; an instance there has nowhere to continue", task, to.Name)
	}

	moved, err := layer.Validate(state, schema.ConformToSchemaExactly)
	if err != nil {
		return nil, fmt.Errorf("at %q: %w", task, err)
	}
	out, ok := moved.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("conformed state is %T, not an object", moved)
	}
	described := layer.Properties()
	for k, v := range state {
		if _, isLayers := described[k]; !isLayers {
			out[k] = v
		}
	}
	return out, nil
}

// InFlightResultBreaks is the half MigrateState cannot see: a parked task's result is on its way
// back, so the new version must accept what the old promised (old ⊆ new). Only MemberUpgrade
// issues return; the REQUEST goes unchecked -- only the engine can evaluate `input`, at arm time.
func InFlightResultBreaks(from, to *model.Task) []Issue {
	if from == nil || to == nil {
		return nil
	}
	var out []Issue
	for _, issue := range resultIssues(from, to) {
		if issue.Member == MemberUpgrade {
			out = append(out, issue)
		}
	}
	return out
}

// TypeChangeBreak reports a task whose action type changed under an instance held in it. Ask it
// only of a held row: one at the task's entry has nothing to hand over.
func TypeChangeBreak(from, to *model.Task) (Issue, bool) {
	if from == nil || to == nil {
		return Issue{}, false
	}
	return typeChangeIssue(from, to)
}
