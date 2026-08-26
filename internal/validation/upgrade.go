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

// MigrateState conforms an instance's stored state to `to`, returning the state to write.
//
// The mode is ConformToSchemaExactly, not Strict: this is a migration of data already written,
// not a document arriving at a boundary. It closes the null-versus-missing gap in both
// directions and does NOT fill defaults (one filled into a half-run instance disagrees with
// every value already computed in its absence).
//
// The layer is deliberately PARTIAL: it describes the slots a definition owns -- input, outputs,
// error -- and the engine's own bookkeeping is not its business. So the conform strips what the
// layer cannot see, and this puts the untouched half back. Inside what it CAN see the schema is
// complete, which is what prunes the output of a task the target version no longer has: nothing
// on the new version can read it (an expression naming it is refused at registration), so
// carrying it forward stores weight that only grows and pins whatever it references.
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
	described := layer.Properties()
	for k, v := range state {
		if _, isLayers := described[k]; !isLayers {
			out[k] = v
		}
	}
	pruneOutputOrder(out)
	return out, nil
}

// pruneOutputOrder drops the completion-order entries whose output the conform removed. The
// order is bookkeeping and so survives untouched, which is exactly why it has to be filtered
// here: left alone it would name a task the state no longer holds an output for.
func pruneOutputOrder(state map[string]any) {
	outputs, _ := state["outputs"].(map[string]any)
	survives := func(name string) bool { _, ok := outputs[name]; return ok }

	// The order arrives as []string read from the row and as []any decoded from JSON, and this
	// runs on both paths -- so handling only one silently prunes nothing.
	switch order := state["output_order"].(type) {
	case []string:
		kept := make([]string, 0, len(order))
		for _, name := range order {
			if survives(name) {
				kept = append(kept, name)
			}
		}
		if len(kept) != len(order) {
			state["output_order"] = kept
		}
	case []any:
		kept := make([]any, 0, len(order))
		for _, id := range order {
			if name, _ := id.(string); survives(name) {
				kept = append(kept, id)
			}
		}
		if len(kept) != len(order) {
			state["output_order"] = kept
		}
	}
}
