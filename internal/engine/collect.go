package engine

import (
	"encoding/json"
	"fmt"
	"sort"

	"genroc/internal/errcode"
	"genroc/internal/model"
	"genroc/internal/schema"
)

// resolveRaisedBatch is the parent's decision procedure over a settled batch that
// contains at least one raised child (§5.2). It runs before buildChildOutput, so the
// happy path (no raise) never reaches it.
//
// raised is the errored children in child-key order. Only the first one routes
// (deterministic, I3): a batch that mixes an unhandled child with a handled one fails on
// the first, exactly as a defect in one slot dominates a raise in another (§5.4).
//
// The routing mirrors handleCallError's, minus retries (there is no parent-side retry,
// D7): a matching rule's raise / panic / goto:end / goto:$id decides the parent's fate;
// no matching rule degrades the raise to a defect and fails the parent — carrying the
// child's own code and message forward, so the failure reads as the raise that caused it
// rather than a generic collect error.
func (e *Engine) resolveRaisedBatch(inst *model.ProcessInstance, task *model.Task, raised []*model.ProcessInstance) advanceOutcome {
	inst.WaitState = model.WaitStateNone
	first := raised[0]
	e.setBatchError(inst, task, first)
	// A child's error_code arrives as a persisted string — it may be an authored raise
	// code as easily as an engine one — so it is converted once, here, at the boundary.
	raisedCode := errcode.Code(first.ErrorCode)
	rule := matchOnError(task, raisedCode)

	switch {
	case rule == nil || (rule.Goto == "" && rule.Raise == nil && rule.Panic == nil):
		// Unhandled: the raise degrades to a defect and fails the parent, which fails fast
		// up its own tree. The parent inherits the child's code and message verbatim, so
		// error_code stays the raised code an operator would filter on.
		return e.failInstance(inst, raisedCode, fmt.Sprintf(
			"task %q: child %q (%s) raised %q: %s; no on_error rule matches",
			task.ID, first.ProcessName, childSlotLabel(first), first.ErrorCode, first.Error))
	case rule.Raise != nil:
		return e.raiseInstance(inst, task, rule.Raise)
	case rule.Panic != nil:
		return e.panicInstance(inst, task, rule.Panic)
	case rule.Goto == model.GotoEnd:
		return e.completeViaErrorHandler(inst, task, first.Error, raisedCode)
	default: // goto $id
		if err := e.resolveGoto(inst, rule.Goto); err != nil {
			return e.failInstance(inst, errcode.EngineDefinition, err.Error())
		}
		inst.Task = rule.Goto
		inst.RetryCount = 0
		inst.WakeAt = nil
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventErrorRoute, Task: task.ID, Code: raisedCode,
			Msg: fmt.Sprintf("child raised %q → %s", first.ErrorCode, rule.Goto)})
		return advanceOutcome{kind: outcomeUpdate}
	}
}

// raisedInSlotOrder returns the batch's raised children in slot order — by _spawn_index
// for a child_list, by sorted _spawn_child_key for a child_map — so that raised[0] is
// deterministically the first-slot raise that routes (I3), regardless of the order the
// children happened to complete in.
func raisedInSlotOrder(siblings []*model.ProcessInstance, task *model.Task) []*model.ProcessInstance {
	var raised []*model.ProcessInstance
	for _, c := range siblings {
		if c.Status == model.StatusRaised {
			raised = append(raised, c)
		}
	}
	if task.Action.Type == model.ActionTypeChildList {
		sort.SliceStable(raised, func(i, j int) bool {
			a, _ := spawnIndex(raised[i])
			b, _ := spawnIndex(raised[j])
			return a < b
		})
	} else {
		sort.SliceStable(raised, func(i, j int) bool {
			return spawnKey(raised[i]) < spawnKey(raised[j])
		})
	}
	return raised
}

// setBatchError writes the $error context for a routed batch (§5.3): the first raised
// child's identity + code + message. No child data crosses — only code, message and
// which child (I6).
//
// A child_map child is identified by a string "child_key", a child_list child by an
// integer "child_index" — separate single-typed fields rather than one string|integer
// value, so an expression reads exactly one and never has to type-switch.
func (e *Engine) setBatchError(inst *model.ProcessInstance, task *model.Task, first *model.ProcessInstance) {
	errCtx := map[string]any{
		"task":    task.ID,
		"code":    first.ErrorCode,
		"message": first.Error,
	}
	addChildSlot(errCtx, first)
	inst.ContextData["error"] = errCtx
}

// addChildSlot sets the one identity field a child carries: "child_key" (string) for a
// child_map child, "child_index" (int) for a child_list child.
func addChildSlot(m map[string]any, child *model.ProcessInstance) {
	if key := spawnKey(child); key != "" {
		m["child_key"] = key
		return
	}
	if idx, ok := spawnIndex(child); ok {
		m["child_index"] = idx
	}
}

// childSlotLabel renders a child's identity for a human-readable message
// (`child_key "charge"`, `child_index 3`).
func childSlotLabel(child *model.ProcessInstance) string {
	if key := spawnKey(child); key != "" {
		return fmt.Sprintf("child_key %q", key)
	}
	if idx, ok := spawnIndex(child); ok {
		return fmt.Sprintf("child_index %d", idx)
	}
	if at, _ := child.ContextData["_spawn_action_type"].(string); at == string(model.ActionTypeChild) {
		return "single child"
	}
	return "child ?"
}

// spawnKey reads a child_map child's _spawn_child_key ("" for a child_list child).
func spawnKey(child *model.ProcessInstance) string {
	key, _ := child.ContextData["_spawn_child_key"].(string)
	return key
}

// buildChildOutput merges a settled batch's outputs into the task's action result
// (self.result) — a keyed map for child_map, an ordered array for child_list. Exported to
// outputs.<id> only if the task projects it via `output`.
//
// It is reached only after resolveRaisedBatch has confirmed no child raised, so every
// child here is completed. Each other status is blocked by its own mechanism: a failed
// child makes the parent failing, which exits advance() before this phase; a paused child
// keeps the parent from being woken at all (it counts as active in CountActiveSiblings);
// a raised child was routed by resolution and never reaches here. The guard therefore
// asserts a settled invariant rather than handling a live case — a non-completed child
// here is a bug, not a merge to attempt.
func (e *Engine) buildChildOutput(task *model.Task, siblings []*model.ProcessInstance) (any, error) {
	for _, c := range siblings {
		if c.Status != model.StatusCompleted {
			return nil, fmt.Errorf("child %q is %s; outputs can only be collected when all children completed", c.ID, c.Status)
		}
	}
	switch task.Action.Type {
	case model.ActionTypeChild:
		return e.buildSingleChildOutput(task, siblings)
	case model.ActionTypeChildList:
		return e.buildListChildOutput(task, siblings)
	default:
		return e.buildMapChildOutput(task, siblings)
	}
}

// buildSingleChildOutput returns the one child's output unwrapped — the child result is
// the task result directly, not keyed (child_map) or arrayed (child_list). Validated
// against the declared result_schema and resolved from the object store when externalized.
func (e *Engine) buildSingleChildOutput(task *model.Task, siblings []*model.ProcessInstance) (any, error) {
	if len(siblings) != 1 {
		return nil, fmt.Errorf("child task expected exactly one child, got %d", len(siblings))
	}
	return e.resolveAndValidateChildOutput(task.Action.ResultSchema, siblings[0])
}

// buildMapChildOutput returns each sibling's output keyed by its child key, validated
// against the declared result_schema (if any) and resolved from the object store when
// externalized.
func (e *Engine) buildMapChildOutput(task *model.Task, siblings []*model.ProcessInstance) (any, error) {
	result := make(map[string]any, len(siblings))
	for _, child := range siblings {
		key, _ := child.ContextData["_spawn_child_key"].(string)
		output, err := e.resolveAndValidateChildOutput(task.Action.Children[key].ResultSchema, child)
		if err != nil {
			return nil, err
		}
		result[key] = output
	}
	return result, nil
}

// buildListChildOutput returns the children's outputs as an array in input order.
// Siblings come back unordered, so each is placed at its recorded _spawn_index —
// guaranteeing result order matches the `over` array regardless of scan order. Each is
// validated against the declared result_schema and resolved from the object store if
// externalized.
func (e *Engine) buildListChildOutput(task *model.Task, siblings []*model.ProcessInstance) (any, error) {
	result := make([]any, len(siblings))
	for _, child := range siblings {
		idx, ok := spawnIndex(child)
		if !ok || idx < 0 || idx >= len(siblings) {
			return nil, fmt.Errorf("child process %q has an invalid _spawn_index", child.ID)
		}
		output, err := e.resolveAndValidateChildOutput(task.Action.ResultSchema, child)
		if err != nil {
			return nil, err
		}
		result[idx] = output
	}
	return result, nil
}

// resolveAndValidateChildOutput reads a completed child's projected output, resolving it
// from the object store if externalized and conforming it to resultSchema when the parent
// declares one.
//
// The schema comes from the parent's task as it stands now, never from a copy taken at
// spawn: an upgraded parent must collect against the version it is running, and the
// conform normalizes, so a stale schema would silently strip a field both sides had
// already agreed to add. The external path resolves it the same way, from the pinned
// definition. See specs/version-compatibility.md §5a.
func (e *Engine) resolveAndValidateChildOutput(resultSchema *schema.Schema, child *model.ProcessInstance) (any, error) {
	output, err := e.resolveValue(child, child.ContextData["output"])
	if err != nil {
		return nil, err
	}
	if resultSchema == nil {
		return output, nil
	}
	// Stored definitions are normalized before they are written, so the schema is used
	// as-is rather than re-normalized per collected child.
	normalized, err := resultSchema.Validate(output)
	if err != nil {
		return nil, fmt.Errorf("child process %q (%s) output validation: %v", child.ID, child.ProcessName, err)
	}
	return normalized, nil
}

// spawnIndex reads a child's _spawn_index. It round-trips through JSON (engine_state),
// so it may come back as any numeric kind; a missing/foreign value reports !ok.
func spawnIndex(child *model.ProcessInstance) (int, bool) {
	switch v := child.ContextData["_spawn_index"].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case json.Number:
		// Context data decodes with UseNumber, so a stored index arrives as its
		// literal. Missing this case is silent: children lose their order rather
		// than erroring.
		n, err := v.Int64()
		return int(n), err == nil
	}
	return 0, false
}
