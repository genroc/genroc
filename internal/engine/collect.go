package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"genroc/internal/errcode"
	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/shape"
)

// resolveRaisedBatch decides a settled batch containing raised children (runs before
// buildChildOutput). Only the FIRST raised child in key order routes (I3); the routing
// mirrors handleCallError minus retries (D7); no matching rule degrades the raise to a
// defect carrying the child's own code. specs/child-error-handling.md.
func (e *Engine) resolveRaisedBatch(ctx context.Context, inst *model.ProcessInstance, task *model.Task, raised []*model.ProcessInstance) advanceOutcome {
	// Admission runs before anything is written: a slot under its rule's limit is re-spawned
	// and the batch goes back to waiting, so `error` is never set for a parent that is only
	// backing off -- the action path reaches its own error write the same way, by returning
	// from the retry branch first. specs/child-error-handling.md s5.5.
	retired, replacements, logs, fail := e.admitRetries(ctx, inst, task, raised)
	if fail != nil {
		return *fail
	}
	if len(replacements) > 0 {
		inst.WaitState = model.WaitStateWaiting
		return advanceOutcome{kind: outcomeRespawn, children: replacements, retired: retired, respawnLogs: logs}
	}

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
			first.ProcessName, childSlotLabel(task, first), first.ErrorCode, err)
		var invalid outputInvalid
		if errors.As(err, &invalid) {
			return e.handleCallError(inst, task, msg, errcode.OutputInvalid)
		}
		// Not a lost bet: the payload could not be read at all, which is the same corruption
		// the collect path reports rather than a shape the caller got wrong.
		return e.failInstance(inst, errcode.EngineCollect, fmt.Sprintf("task %q collect: %s", task.ID, msg))
	}
	// Written before matching here, unlike in admission: this path routes or fails whatever
	// the rules say, so the error is the instance's state either way.
	e.setBatchError(inst, task, first, data, declared)
	rule, matchErr := matchOnErrorWith(task, raisedCode, e.caseEvaluator(inst, batchErrorValue(task, first, data, declared)))
	if matchErr != nil {
		return e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q: %v", task.ID, matchErr))
	}

	switch {
	case rule == nil || (rule.Goto == "" && rule.Raise == nil && rule.Panic == nil):
		// Unhandled: the raise degrades to a defect and fails the parent, which fails fast
		// up its own tree. The parent inherits the child's code and message verbatim, so
		// error_code stays the raised code an operator would filter on.
		return e.failInstance(inst, raisedCode, fmt.Sprintf(
			"task %q: child %q (%s) raised %q: %s; no on_error rule matches",
			task.ID, first.ProcessName, childSlotLabel(task, first), first.ErrorCode, first.ErrorMessage))
	case rule.Raise != nil:
		return e.raiseInstance(inst, task, rule.Raise, nil)
	case rule.Panic != nil:
		return e.panicInstance(inst, task, rule.Panic, nil)
	case rule.Goto == model.GotoEnd:
		return e.completeViaErrorHandler(inst, task, first.ErrorMessage, raisedCode)
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

// admitRetries decides which raised slots get another attempt. Each slot is its own call
// with its own budget: it conforms its own payload first -- a payload that fails its
// declaration REPLACES the code, and the code is what picks the rule -- then matches that
// code and compares its own `_spawn_attempt` against the limit the matched rule names.
// A slot whose code matches no retry rule is simply never re-spawned, so a permanently
// broken one stops dragging the batch through rounds. specs/child-error-handling.md s5.5.
func (e *Engine) admitRetries(ctx context.Context, inst *model.ProcessInstance, task *model.Task, raised []*model.ProcessInstance) (retired []string, replacements []*model.ProcessInstance, logs []string, fail *advanceOutcome) {
	// Built lazily: a batch with nothing admissible must not pay for a rebuild, and the
	// rebuild can fail the instance (an upgraded parent may no longer declare the slot).
	var fresh map[string]*model.ProcessInstance
	// The operator's `retry` marks the parent instead of re-spawning in the db layer, so the
	// replacement's input is re-evaluated here against the parent's CURRENT definition. It
	// grants one attempt past the budget and is one-shot: the count still advances, so the
	// next automatic round declines on its own. specs/child-error-handling.md s12.
	override := inst.State[retryOverrideKey] == true
	delete(inst.State, retryOverrideKey)
	for _, child := range raised {
		code, errVal, err := e.slotError(task, child)
		if err != nil {
			msg := fmt.Sprintf("child %q (%s) raised %q: %v", child.ProcessName, childSlotLabel(task, child), child.ErrorCode, err)
			return nil, nil, nil, stop(e.failInstance(inst, errcode.EngineCollect, fmt.Sprintf("task %q collect: %s", task.ID, msg)))
		}
		// Under an override the rules are not consulted at all: an operator naming a failed
		// tree has decided, and a zero policy also means no backoff — someone asking for a
		// retry wants it now.
		var policy model.ResolvedRetry
		if !override {
			// The case sees THIS slot's error: per-slot admission means a per-slot predicate.
			rule, err := matchOnErrorWith(task, code, e.caseEvaluator(inst, errVal))
			if err != nil {
				return nil, nil, nil, stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q: %v", task.ID, err)))
			}
			if rule == nil || rule.Retry.IsZero() {
				continue
			}
			resolved, resErr := rule.Retry.Resolve(func(expr string) (any, error) {
				return e.evalShape(inst, shape.Shape{Raw: expr}, nil)
			})
			if resErr != nil {
				// Same reading as the action path: a policy that quietly became "no retries"
				// is an author's budget vanishing with nothing reporting it.
				return nil, nil, nil, stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q on_error: %v", task.ID, resErr)))
			}
			policy = resolved
			if spawnAttempt(child) >= int64(policy.Attempts) {
				continue
			}
		}
		attempt := spawnAttempt(child)
		if fresh == nil {
			built, rebuildFail := e.freshBatch(ctx, inst, task)
			if rebuildFail != nil {
				return nil, nil, nil, rebuildFail
			}
			fresh = built
		}
		replacement, err := e.respawnChild(child, fresh, attempt+1, policy)
		if err != nil {
			return nil, nil, nil, stop(e.failInstance(inst, errcode.EngineSpawn, fmt.Sprintf("task %q retry: %v", task.ID, err)))
		}
		retired = append(retired, child.ID)
		replacements = append(replacements, replacement)
		if override {
			logs = append(logs, fmt.Sprintf("child %q (%s) raised %q; re-spawning on operator retry (attempt %d)",
				child.ProcessName, childSlotLabel(task, child), child.ErrorCode, attempt+1))
		} else {
			logs = append(logs, fmt.Sprintf("child %q (%s) raised %q; re-spawning (attempt %d/%d)",
				child.ProcessName, childSlotLabel(task, child), child.ErrorCode, attempt+1, policy.Attempts))
		}
	}
	return retired, replacements, logs, nil
}

// slotError is what a raised slot is judged by: its code — its own, unless the payload it
// carries fails the shape the call declared, which replaces it with output.invalid before any
// rule is consulted (the fetch precedent — a malformed declared body takes a 400 away from
// `http.4%`) — and the `error` value an M2 case reads. A payload that cannot be read at all is
// corruption, not a lost bet.
func (e *Engine) slotError(task *model.Task, child *model.ProcessInstance) (errcode.Code, map[string]any, error) {
	code := errcode.Code(child.ErrorCode)
	data, declared, err := e.raisedData(task, child, code)
	if err != nil {
		var invalid outputInvalid
		if errors.As(err, &invalid) {
			return errcode.OutputInvalid, batchErrorValue(task, child, nil, false), nil
		}
		return "", nil, err
	}
	return code, batchErrorValue(task, child, data, declared), nil
}

// respawnChild builds the replacement for a raised slot: the slot's identity and the input it
// was given, none of what the attempt produced, and a wake_at measured from the attempt's own
// conclusion rather than from now -- the wall-clock it spent waiting on its siblings already
// served what a backoff is for, so charging the delay again would charge the wait twice.
func (e *Engine) respawnChild(old *model.ProcessInstance, fresh map[string]*model.ProcessInstance, attempt int64, policy model.ResolvedRetry) (*model.ProcessInstance, error) {
	slot := slotID(old)
	replacement, ok := fresh[slot]
	if !ok {
		// The parent no longer declares this slot -- an upgrade removed the entry, or a
		// `child_list` fan-out came back shorter. Nothing here can invent an input for it.
		return nil, fmt.Errorf("slot %s is no longer declared by task %q", slot, old.SpawnTaskID)
	}
	// Rebuilt as phase 1 would build it, then placed in the batch it is replacing INTO:
	// same epoch, so it is collected beside the siblings the parent kept.
	replacement.ParentTaskEpoch = old.ParentTaskEpoch
	replacement.State[spawnAttemptKey] = attempt
	// Measured from the attempt's own conclusion, not from now: the wall-clock it spent
	// waiting on its siblings already served what a backoff is for.
	wake := old.UpdatedAt.Add(e.retryDelay(int(attempt), policy))
	replacement.WakeAt = &wake
	return replacement, nil
}

// spawnAttemptKey records how many times this slot has been re-spawned. It rides the child's
// own `_spawn_*` bookkeeping beside the slot identity, so the sibling queries gain neither a
// column nor a predicate -- the cost D7 mispriced. specs/child-error-handling.md s5.5.
const spawnAttemptKey = "_spawn_attempt"

// retryOverrideKey is the one-shot grant RetryProcess leaves for the next collect (s12).
const retryOverrideKey = "_retry_override"

// spawnAttempt reads it. Zero for a first-generation child, so `attempt < limit` admits
// exactly `limit` retries -- the same base as inst.RetryCount on an action task.
func spawnAttempt(child *model.ProcessInstance) int64 {
	switch v := child.State[spawnAttemptKey].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case json.Number:
		// engine_state decodes with UseNumber, so a stored count arrives as its literal.
		// Missing this case reads as zero, and a budget that never advances never ends.
		n, err := v.Int64()
		if err == nil {
			return n
		}
	}
	return 0
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
	inst.State["error"] = batchErrorValue(task, first, data, declared)
}

// batchErrorValue is what a routed task reads as `error` (§5.3). Separated from the write so
// admission can BIND it for an M2 case without persisting it: a rule that declines must leave
// nothing behind, and a retrying parent carries no `error` at all (§5.5).
func batchErrorValue(task *model.Task, child *model.ProcessInstance, data any, declared bool) map[string]any {
	errCtx := map[string]any{
		"task":    task.ID,
		"code":    child.ErrorCode,
		"message": child.ErrorMessage,
	}
	if declared {
		errCtx["data"] = data
	}
	addChildSlot(errCtx, child)
	return errCtx
}

// caseEvaluator returns the predicate hook matchOnErrorWith needs, with errVal bound as
// `error` for the evaluation and restored afterwards — bound, never written (M2).
func (e *Engine) caseEvaluator(inst *model.ProcessInstance, errVal map[string]any) func(string) (bool, error) {
	return func(expr string) (bool, error) {
		prev, had := inst.State["error"]
		inst.State["error"] = errVal
		defer func() {
			if had {
				inst.State["error"] = prev
			} else {
				delete(inst.State, "error")
			}
		}()
		// Expr: true — a case is a bare boolean expression, not a template. Without it the
		// text is rendered as a string and never compares as anything.
		v, err := e.evalShape(inst, shape.Shape{Raw: expr, Expr: true}, nil)
		if err != nil {
			return false, fmt.Errorf("on_error case %q: %w", expr, err)
		}
		b, ok := v.(bool)
		if !ok {
			// Registration type-checks the case to a boolean, so this means that guarantee
			// did not hold. Erroring beats declining: a silent non-match routes the error
			// somewhere the author never wrote.
			return false, fmt.Errorf("on_error case %q evaluated to %T, not a boolean", expr, v)
		}
		return b, nil
	}
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
// (`child_key "charge"`, `child_index 3`). The single-child case is read off the PARENT's task
// rather than a discriminant on the child: what shape a batch was spawned in is the parent
// definition's to say, and a copy carried by the child is one an upgrade can leave stale.
func childSlotLabel(task *model.Task, child *model.ProcessInstance) string {
	if key := spawnKey(child); key != "" {
		return fmt.Sprintf("child_key %q", key)
	}
	if idx, ok := spawnIndex(child); ok {
		return fmt.Sprintf("child_index %d", idx)
	}
	if task.Action != nil && task.Action.Type == model.ActionTypeChild {
		return "single child"
	}
	return "child ?"
}

// spawnKey reads a child_map child's _spawn_child_key ("" for a child_list child).
func spawnKey(child *model.ProcessInstance) string {
	key, _ := child.State["_spawn_child_key"].(string)
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
		key, _ := child.State["_spawn_child_key"].(string)
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
	output, err := e.context(child).Materialize(child.State["output"])
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
	raw, err := e.context(child).Materialize(childRaisedData(child))
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

// childRaisedData reads what the raise clause attached, which setErrorData left in the
// child's outbound slot. A raise that attached nothing reads as nil, and a declared shape that
// does not admit null reports the mismatch — the caller declared what it did not get. Never the
// child's `error`: that is the error it CAUGHT, which its raise did not choose to forward.
func childRaisedData(child *model.ProcessInstance) any {
	return child.State[model.StateErrorData]
}

// spawnIndex reads a child's _spawn_index. It round-trips through JSON (engine_state),
// so it may come back as any numeric kind; a missing/foreign value reports !ok.
func spawnIndex(child *model.ProcessInstance) (int, bool) {
	switch v := child.State["_spawn_index"].(type) {
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
