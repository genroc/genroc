package validation

// Moving an instance to another version of its definition.
//
// compat builds a schema per task -- one layer for each state an instance could be sitting
// in, because a report has to speak about all of them at once. An upgrade is the opposite
// situation: the instance is in exactly one state, so exactly one layer applies. Take that
// layer from the version it is moving TO and conform the stored state through it. The
// result is the state to write, and the validator's refusal is the reason it cannot move --
// there is no second predicate beside it to fall out of step.
// specs/version-compatibility.md s1.

import (
	"fmt"

	"genroc/internal/model"
	"genroc/internal/schema"
)

// MigrateState conforms an instance's stored context to `to`, returning the state to write.
//
// The mode is ConformToSchemaExactly, not Strict: this is a migration of data already
// written, not a document arriving at a boundary. It closes the null-versus-missing gap in
// both directions, KEEPS undeclared keys (a dropped task's output is real data nobody asked
// to delete) and does NOT fill defaults (one filled into a half-run instance disagrees with
// every value already computed in its absence).
func MigrateState(to *model.ProcessDefinition, task string, state map[string]any) (map[string]any, error) {
	if task == "" {
		return nil, fmt.Errorf("instance holds no task to resume at")
	}
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
		return nil, fmt.Errorf("state at task %q does not fit: %w", task, err)
	}
	out, ok := moved.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("conformed state is %T, not an object", moved)
	}
	return out, nil
}
