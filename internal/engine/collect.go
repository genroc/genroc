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

// resolveRaisedBatch routes only the FIRST raised child in slot order (I3).
// specs/child-error-handling.md.
func (e *Engine) resolveRaisedBatch(ctx context.Context, inst *model.ProcessInstance, task *model.Task, raised []*model.ProcessInstance) advanceOutcome {
	// Admission runs before anything is written: a parent only backing off carries no `error`.
	// specs/child-error-handling.md s5.5.
	retired, replacements, logs, fail := e.admitRetries(ctx, inst, task, raised)
	if fail != nil {
		return *fail
	}
	if len(replacements) > 0 {
		inst.Phase = model.PhaseChildren
		return advanceOutcome{kind: outcomeRespawn, children: replacements, retired: retired, respawnLogs: logs}
	}

	inst.Phase = model.PhaseNone
	first := raised[0]
	// A child's error_code arrives as a persisted string — it may be an authored raise
	// code as easily as an engine one — so it is converted once, here, at the boundary.
	raisedCode := errcode.Code(first.ErrorCode)

	// The conform runs BEFORE the rules: a payload failing its declaration REPLACES the code.
	data, declared, err := e.raisedData(task, first, raisedCode)
	if err != nil {
		msg := fmt.Sprintf("child %q (%s) raised %q: %v",
			first.ProcessName, childSlotLabel(task, first), first.ErrorCode, err)
		var invalid resultInvalid
		if errors.As(err, &invalid) {
			return e.handleCallError(inst, task, msg, errcode.ResultInvalid)
		}
		// Not a lost bet: the payload could not be read at all, which is the same corruption
		// the collect path reports rather than a shape the caller got wrong.
		return e.failInstance(inst, errcode.EngineCollect, fmt.Sprintf("task %q collect: %s", task.ID, msg))
	}
	// Deferred past the clause: until then `last_error` is still the failure that routed INTO
	// this task, which a rule may name beside the one it caught. specs/task-scopes.md.
	errVal := batchErrorValue(task, first, data, declared)
	release := bindCaught(inst, errVal)
	defer func() {
		release()
		e.setBatchError(inst, task, first, data, declared)
	}()
	rule, matchErr := matchOnErrorWith(task, raisedCode, e.caseEvaluator(inst, errVal))
	if matchErr != nil {
		return e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q: %v", task.ID, matchErr))
	}

	switch {
	case rule == nil || (rule.Goto == "" && rule.Raise == nil && rule.Panic == nil):
		// Unhandled: the parent inherits the child's code verbatim, so error_code stays the
		// raised code an operator would filter on.
		return e.failInstance(inst, raisedCode, fmt.Sprintf(
			"task %q: child %q (%s) raised %q: %s; no on_error rule matches",
			task.ID, first.ProcessName, childSlotLabel(task, first), first.ErrorCode, first.ErrorMessage))
	case rule.Raise != nil:
		return e.raiseInstance(inst, task, rule.Raise, e.selfBeforeOutput(inst))
	case rule.Panic != nil:
		return e.panicInstance(inst, task, rule.Panic, e.selfBeforeOutput(inst))
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

// admitRetries conforms each slot's payload before matching (it can replace the code, and the
// code picks the rule). specs/child-error-handling.md s5.5.
func (e *Engine) admitRetries(ctx context.Context, inst *model.ProcessInstance, task *model.Task, raised []*model.ProcessInstance) (retired []string, replacements []*model.ProcessInstance, logs []string, fail *advanceOutcome) {
	// Built lazily: a batch with nothing admissible must not pay for a rebuild, and the
	// rebuild can fail the instance (an upgraded parent may no longer declare the slot).
	var fresh map[string]*model.ProcessInstance
	// The operator's one-shot grant: the count still advances, so the next automatic round
	// declines on its own. specs/child-error-handling.md s12.
	override := inst.State[retryOverrideKey] == true
	delete(inst.State, retryOverrideKey)
	for _, child := range raised {
		code, errVal, err := e.slotError(task, child)
		if err != nil {
			msg := fmt.Sprintf("child %q (%s) raised %q: %v", child.ProcessName, childSlotLabel(task, child), child.ErrorCode, err)
			return nil, nil, nil, stop(e.failInstance(inst, errcode.EngineCollect, fmt.Sprintf("task %q collect: %s", task.ID, msg)))
		}
		// Under an override the rules are not consulted, and the zero policy means no backoff.
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
			// This slot's own failure, the same one the case above was matched against.
			resolved, resErr := e.resolveRetry(inst, rule.Retry, errVal)
			if resErr != nil {
				// Same reading as the action path: a policy that quietly became "no retries"
				// is an author's budget vanishing with nothing reporting it.
				return nil, nil, nil, stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q on_error: %v", task.ID, resErr)))
			}
			policy = resolved
			if spawnAttempt(child) >= int64(policy.Retries) {
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
			logs = append(logs, fmt.Sprintf("child %q (%s) raised %q; re-spawning on operator retry (retry %d)",
				child.ProcessName, childSlotLabel(task, child), child.ErrorCode, attempt+1))
		} else {
			logs = append(logs, fmt.Sprintf("child %q (%s) raised %q; re-spawning (retry %d/%d)",
				child.ProcessName, childSlotLabel(task, child), child.ErrorCode, attempt+1, policy.Retries))
		}
	}
	return retired, replacements, logs, nil
}

// slotError returns result.invalid in place of the slot's code when its payload fails the
// declaration. An unreadable payload is corruption, not a lost bet.
func (e *Engine) slotError(task *model.Task, child *model.ProcessInstance) (errcode.Code, map[string]any, error) {
	code := errcode.Code(child.ErrorCode)
	data, declared, err := e.raisedData(task, child, code)
	if err != nil {
		var invalid resultInvalid
		if errors.As(err, &invalid) {
			return errcode.ResultInvalid, batchErrorValue(task, child, nil, false), nil
		}
		return "", nil, err
	}
	return code, batchErrorValue(task, child, data, declared), nil
}

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

// spawnAttemptKey lives on the child, never the parent's retry_count (CLAUDE.md), so the
// sibling queries gain neither a column nor a predicate. specs/child-error-handling.md s5.5.
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

// raisedInSlotOrder makes raised[0] the first-slot raise (I3), whatever order the children
// completed in.
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

func (e *Engine) setBatchError(inst *model.ProcessInstance, task *model.Task, first *model.ProcessInstance, data any, declared bool) {
	inst.State[model.StateLastError] = batchErrorValue(task, first, data, declared)
}

// resolveRetry binds the failure as `error`, the scope its case matched in. Bound, never
// written: a granted retry must leave nothing behind. specs/task-scopes.md.
func (e *Engine) resolveRetry(inst *model.ProcessInstance, r model.Retry, errVal map[string]any) (model.ResolvedRetry, error) {
	defer bindCaught(inst, errVal)()
	return r.Resolve(func(expr string) (any, error) {
		return e.evalShape(inst, shape.Shape{Raw: expr}, e.selfBeforeOutput(inst))
	})
}

// bindCaught binds `error` (not `last_error`) for `defer bindCaught(inst, v)()`. It RESTORES
// rather than deletes because binds nest. specs/task-scopes.md.
func bindCaught(inst *model.ProcessInstance, errVal map[string]any) func() {
	prev, had := inst.State[model.StateError]
	inst.State[model.StateError] = errVal
	return func() {
		if had {
			inst.State[model.StateError] = prev
			return
		}
		delete(inst.State, model.StateError)
	}
}

// batchErrorValue is separate from the write so admission can BIND it without persisting: a
// declining rule must leave nothing behind. `data` key presence says the payload is readable.
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
// `error` for the evaluation and unbound afterwards — bound, never written (M2).
func (e *Engine) caseEvaluator(inst *model.ProcessInstance, errVal map[string]any) func(string) (bool, error) {
	return func(expr string) (bool, error) {
		defer bindCaught(inst, errVal)()
		// Expr: true — a case is a bare boolean expression, not a template. Without it the
		// text is rendered as a string and never compares as anything.
		v, err := e.evalShape(inst, shape.Shape{Raw: expr, Expr: true}, e.selfBeforeOutput(inst))
		if err != nil {
			return false, fmt.Errorf("on_error case %q: %w", expr, err)
		}
		b, ok := v.(bool)
		if !ok {
			// Erroring beats declining: a silent non-match routes the error somewhere the
			// author never wrote.
			return false, fmt.Errorf("on_error case %q evaluated to %T, not a boolean", expr, v)
		}
		return b, nil
	}
}

// addChildSlot keeps child_key and child_index separate, single-typed fields so an
// expression never type-switches.
func addChildSlot(m map[string]any, child *model.ProcessInstance) {
	if key := spawnKey(child); key != "" {
		m["child_key"] = key
		return
	}
	if idx, ok := spawnIndex(child); ok {
		m["child_index"] = idx
	}
}

// childSlotLabel reads the single-child case off the PARENT's task: a copy on the child is
// one an upgrade can leave stale.
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

// resultInvalid is a value failing a shape THIS task declared: a lost bet, so catchable. Every
// other collect failure is corruption and stays engine.collect. specs/error-extensions.md §X2-c.
type resultInvalid struct{ error }

// buildChildOutput is reached only with every child completed; the guard asserts that.
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

func (e *Engine) buildSingleChildOutput(task *model.Task, siblings []*model.ProcessInstance) (any, error) {
	if len(siblings) != 1 {
		return nil, fmt.Errorf("child task expected exactly one child, got %d", len(siblings))
	}
	return e.resolveAndValidateChildOutput(task.Action.ResultSchema, siblings[0])
}

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

// Siblings arrive unordered, so each lands at its recorded _spawn_index.
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
		return nil, resultInvalid{fmt.Errorf("child process %q (%s) output validation: %v", child.ID, child.ProcessName, err)}
	}
	return normalized, nil
}

// raisedData is resolveAndValidateChildOutput for `raises`, under the same CURRENT-task rule.
// declared=false keeps an undeclared code's slot absent, not null. specs/error-extensions.md §X2-c.
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
		return nil, false, resultInvalid{fmt.Errorf("data validation: %v", err)}
	}
	return normalized, true, nil
}

// A child_map declares per entry, child and child_list on the action, as result_schema does.
func declaredRaiseSchema(task *model.Task, child *model.ProcessInstance, code string) *schema.Schema {
	if task.Action.Type == model.ActionTypeChildMap {
		return task.Action.Children[spawnKey(child)].Raises[code]
	}
	return task.Action.Raises[code]
}

// childRaisedData is never the child's `error`: that is what it CAUGHT, which its raise did
// not choose to forward.
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
		// UseNumber: a stored index arrives as its literal. Missing this case is
		// silent: children lose their order.
		n, err := v.Int64()
		return int(n), err == nil
	}
	return 0, false
}
