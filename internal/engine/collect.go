package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"genroc/internal/errcode"
	"genroc/internal/model"
	"genroc/internal/schema"
)

// resolveRaisedBatch decides a settled batch containing raised children (runs before
// buildChildOutput). Only the FIRST raised child in key order routes (I3); the routing
// mirrors handleCallError minus retries (D7); no matching rule degrades the raise to a
// defect carrying the child's own code. specs/child-error-handling.md.
func (e *Engine) resolveRaisedBatch(inst *model.ProcessInstance, task *model.Task, raised []*model.ProcessInstance) advanceOutcome {
	inst.WaitState = model.WaitStateNone
	first := raised[0]
	// A child's error_code arrives as a persisted string — it may be an authored raise
	// code as easily as an engine one — so it is converted once, here, at the boundary.
	raisedCode := errcode.Code(first.ErrorCode)

	// The conform runs BEFORE the rules: a payload that does not satisfy what this call
	// declared REPLACES the code, so a rule naming the raised code stops catching it — the
	// fetch precedent, where a malformed declared body takes a 400 away from `http.4%`.
	data, declared, err := e.raisedData(task, first, raisedCode)
	if err != nil {
		msg := fmt.Sprintf("child %q (%s) raised %q: %v",
			first.ProcessName, childSlotLabel(first), first.ErrorCode, err)
		var invalid outputInvalid
		if errors.As(err, &invalid) {
			return e.handleCallError(inst, task, msg, errcode.OutputInvalid)
		}
		// Not a lost bet: the payload could not be read at all, which is the same corruption
		// the collect path reports rather than a shape the caller got wrong.
		return e.failInstance(inst, errcode.EngineCollect, fmt.Sprintf("task %q collect: %s", task.ID, msg))
	}
	e.setBatchError(inst, task, first, data, declared)
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
		return e.raiseInstance(inst, task, rule.Raise, nil)
	case rule.Panic != nil:
		return e.panicInstance(inst, task, rule.Panic, nil)
	case rule.Goto == model.GotoEnd:
		return e.completeViaErrorHandler(inst, task, first.Error, raisedCode)
	default: // goto $id
		if err := e.resolveGoto(inst, rule.Goto); err != nil {
			return e.failInstance(inst, errcode.EngineDefinition, err.Error())
		}
		enterTask(inst, rule.Goto)
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

// setBatchError writes `error` for a routed batch: the first raised child's identity, code,
// message, and its `data` where this call declared a shape for the code — key presence is
// what says the payload is readable, so an undeclared code leaves the slot absent rather than
// null (I6 as amended). child_key (string) and child_index (integer) are separate
// single-typed fields so an expression never type-switches.
func (e *Engine) setBatchError(inst *model.ProcessInstance, task *model.Task, first *model.ProcessInstance, data any, declared bool) {
	errCtx := map[string]any{
		"task":    task.ID,
		"code":    first.ErrorCode,
		"message": first.Error,
	}
	if declared {
		errCtx["data"] = data
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

// outputInvalid marks the collect failures a caller may react to: a value that failed a shape
// THIS task declared for it — an output against result_schema, a raised fault's data against
// raises. A lost bet, not a defect, since the child states no shape to disagree with. Every
// other failure here is corruption and stays engine.collect. specs/error-extensions.md §X2-c.
type outputInvalid struct{ error }

// buildChildOutput merges a settled batch into self.result (map for child_map, array for
// child_list). Reached only with every child completed — failed/paused/raised are each
// blocked upstream — so the guard asserts an invariant, not a case to handle.
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

// buildListChildOutput returns outputs as an array in input order: siblings arrive
// unordered, so each lands at its recorded _spawn_index. Each is schema-validated and
// resolved from the object store if externalized.
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

// Resolves a child's output and conforms it against the parent's CURRENT task schema —
// never a spawn-time copy: the conform normalizes, and a stale schema silently strips
// fields both sides already agreed on. specs/version-compatibility.md §3a; CLAUDE.md.
func (e *Engine) resolveAndValidateChildOutput(resultSchema *schema.Schema, child *model.ProcessInstance) (any, error) {
	// The child's own context: an object is addressed by content, but the memo lives per
	// instance. Materialized because a schema conform reads every field.
	output, err := e.context(child).Materialize(child.ContextData["output"])
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
		return nil, outputInvalid{fmt.Errorf("child process %q (%s) output validation: %v", child.ID, child.ProcessName, err)}
	}
	return normalized, nil
}

// raisedData conforms a raised child's payload against what THIS call declared for the code
// under `raises` — the error channel's resolveAndValidateChildOutput, and read from the
// parent's CURRENT task for the same reason. declared=false is how an undeclared code stays
// unreadable: the slot is absent rather than null. specs/error-extensions.md §X2-c.
func (e *Engine) raisedData(task *model.Task, child *model.ProcessInstance, code errcode.Code) (any, bool, error) {
	sc := declaredRaiseSchema(task, child, string(code))
	if sc == nil {
		return nil, false, nil
	}
	raw, err := e.context(child).Materialize(childFaultData(child))
	if err != nil {
		return nil, false, fmt.Errorf("resolving its data: %v", err)
	}
	normalized, err := sc.Validate(raw)
	if err != nil {
		return nil, false, outputInvalid{fmt.Errorf("data validation: %v", err)}
	}
	return normalized, true, nil
}

// declaredRaiseSchema reads the declaration for one code: a child_map declares per entry,
// since its entries can be different processes, while child and child_list declare on the
// action — the same split result_schema has.
func declaredRaiseSchema(task *model.Task, child *model.ProcessInstance, code string) *schema.Schema {
	if task.Action.Type == model.ActionTypeChildMap {
		return task.Action.Children[spawnKey(child)].Raises[code]
	}
	return task.Action.Raises[code]
}

// childFaultData reads what the raise clause attached, which applyFaultData left on the
// child's own error.data. A raise that attached nothing reads as nil, and a declared shape
// that does not admit null reports the mismatch — the caller declared what it did not get.
func childFaultData(child *model.ProcessInstance) any {
	errCtx, _ := child.ContextData["error"].(map[string]any)
	return errCtx["data"]
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
