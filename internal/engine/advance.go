package engine

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"

	"genroc/internal/errcode"
	"genroc/internal/model"
	"genroc/internal/shape"
)

// advanceOutcome is the next persisted state that advance() computes without writing it.
// runAdvance drops the in-flight marker before persist applies it, so an instance is never
// free in the DB while still marked in memory — dispatch would misread that as re-claiming
// live work. A child spawn is the exception: it commits its own transaction and returns
// outcomeNoop.
type advanceOutcome struct {
	kind outcomeKind
}

type outcomeKind uint8

const (
	outcomeProgress outcomeKind = iota // running checkpoint        → UpdateInstanceProgress
	outcomeUpdate                      // running, status/error set → UpdateInstance
	outcomeTerminal                    // completed/failed/paused → saveAndNotify
	outcomeNoop                        // advance already persisted (child spawn parked the parent)
)

// stop wraps an outcome as a non-nil pointer: call helpers return it to halt the task
// loop with this outcome; a nil *advanceOutcome means "continue".
func stop(o advanceOutcome) *advanceOutcome { return &o }

// persist applies an advance outcome to the DB — the single place an in-flight advance
// releases its lease (runAdvance calls it after dropping the marker).
func (e *Engine) persist(inst *model.ProcessInstance, o advanceOutcome) error {
	switch o.kind {
	case outcomeTerminal:
		return e.saveAndNotify(inst)
	case outcomeProgress:
		return e.db.UpdateInstanceProgress(inst)
	case outcomeUpdate:
		return e.db.UpdateInstance(inst)
	case outcomeNoop:
		return nil
	default:
		return fmt.Errorf("unknown advance outcome %d", o.kind)
	}
}

// runAdvance advances one instance, then drops its in-flight marker before persisting,
// closing the window where the instance is free in the DB but still marked in memory.
// (For Tick, which keeps no marker, the delete is a harmless no-op.)
func (e *Engine) runAdvance(ctx context.Context, inst *model.ProcessInstance) error {
	outcome := e.advanceGuarded(ctx, inst)
	e.inflight.Delete(inst.ID)
	if err := e.persist(inst, outcome); err != nil {
		return err
	}
	// A persisted advance may have produced immediately-runnable work: this instance
	// again (a running checkpoint), children spawned by a parked parent, or a parent
	// un-parked by this instance finishing. Nudge the pump to re-scan now rather than
	// idle until the next tick. A spurious nudge (nothing actually runnable) costs only
	// one empty claim, so signalling unconditionally keeps this correct and simple.
	e.signalWork()
	return nil
}

// advanceGuarded runs advance under a panic barrier, converting a panic into a terminal
// failure carrying errcode.EnginePanic: a panic is almost always attributable to the one
// definition being advanced, and killing the worker punishes every other in-flight advance
// for it. Rationale and residuals: docs/error-handling-audit.md.
//
// The barrier must cover advance() only, never persist(). A panic in the write path is not
// definition-attributable, and there would be nothing left to write the failure with.
func (e *Engine) advanceGuarded(ctx context.Context, inst *model.ProcessInstance) (outcome advanceOutcome) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		reason := fmt.Sprintf("panic while advancing task %q: %v", inst.Task, r)
		stack := string(debug.Stack())

		// Console first, via logOnly: it writes neither to the database nor through
		// the definition, so it is the one report that cannot itself fail. Whatever
		// happens below, the panic is on the record somewhere.
		e.logOnly(logEvent{Level: model.LogError, ID: inst.ID, Msg: reason + "\n" + stack})

		// Pre-set the outcome, because the durable recording below can panic in turn
		// and this value has to survive that. It is not defensive padding: audit
		// resolves the instance's definition to redact secrets from the entry, so a
		// definition malformed enough to panic advance is a good bet to panic the
		// recording too — and failInstance audits. The order matters: failInstance
		// assigns the terminal fields *before* it audits, so an instance that dies in
		// the audit is still correctly marked failed and is persisted as such.
		outcome = advanceOutcome{kind: outcomeTerminal}
		defer func() {
			if r2 := recover(); r2 != nil {
				e.logOnly(logEvent{Level: model.LogError, ID: inst.ID,
					Msg: fmt.Sprintf("panic while recording the panic above: %v", r2)})
			}
		}()
		outcome = e.failInstance(inst, errcode.EnginePanic, reason)
		// The stack in the instance's own trail is what makes a panicked instance
		// debuggable from the API alone, without the worker's console.
		e.audit(inst, logEvent{Level: model.LogError, Event: model.EventInstanceFailed, Task: inst.Task,
			Msg: reason, Code: errcode.EnginePanic, Data: stack})
	}()
	return e.advance(ctx, inst)
}

// prepareAdvance runs the once-per-tick setup before the task loop: load the definition,
// resolve config from the environment, locate the current task, handle a lease-takeover
// reclaim (failing an interrupted only_once task), and emit work_started. Returns the
// definition and task index, or a non-nil outcome the caller must return immediately.
func (e *Engine) prepareAdvance(inst *model.ProcessInstance) (*model.ProcessDefinition, int, *advanceOutcome) {
	// Load the definition once for the whole tick: it drives config resolution and
	// is the source of truth for the task list (the instance stores only its current
	// task id; successors are implied by definition order). An instance whose
	// definition cannot be loaded cannot run, so fail it with a clear reason.
	def, err := e.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return nil, 0, stop(e.failInstance(inst, errcode.EngineDefinition, fmt.Sprintf("load definition: %v", err)))
	}

	// Resolve config from the OS environment for this tick. Config is never
	// persisted — it is re-resolved every tick and exposed to expressions as
	// "config". A resolution failure (missing required var, bad coercion) fails
	// the instance with a clear reason.
	if def.ConfigSchema != nil {
		cfg, err := def.ResolveConfig(os.LookupEnv)
		if err != nil {
			return nil, 0, stop(e.failInstance(inst, errcode.EngineConfig, fmt.Sprintf("config: %v", err)))
		}
		inst.Config = cfg
	}

	// Resolve the instance's position in the task list. An empty Task means it has
	// run off the end (nothing left) — the loop completes it. A non-empty Task that
	// isn't in the definition is a corrupt/mismatched row: fail it.
	idx := taskIndex(def.Tasks, inst.Task)
	if inst.Task != "" && idx < 0 {
		return nil, 0, stop(e.failInstance(inst, errcode.EngineDefinition, fmt.Sprintf("current task %q not found in definition", inst.Task)))
	}

	// Reclaimed from an expired lease, so the current task may already have executed on
	// the previous owner. Re-running is fine unless the task is only_once, which is handed
	// to the definition as only_once.interrupted instead — routable, never retryable, and
	// the same terminal failure as before when nothing catches it.
	// See docs/only-once-interrupted.md.
	if inst.ReclaimedExpired {
		e.logOnly(logEvent{Level: model.LogWarn, ID: inst.ID,
			Msg:  "reclaimed expired lease; previous owner crashed or stalled mid-task",
			Meta: map[string]any{"task": inst.Task, "process": inst.ProcessName}})
		if idx >= 0 && interruptedOnlyOnce(def.Tasks[idx]) {
			return nil, 0, stop(e.handleCallError(inst, def.Tasks[idx], interruptedMessage, errcode.OnlyOnceInterrupted))
		}
	}

	// work_started: a worker has picked this instance up and is about to work its
	// current task. One per work session (a resume after parking emits it again),
	// tagged with the worker so the unified log shows who is doing what.
	if idx >= 0 {
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventWorkStarted, Task: inst.Task, Meta: map[string]any{"worker": e.workerID}})
	}

	return def, idx, nil
}

// advance executes the next task in the instance's queue and returns the outcome to
// persist (it does no lease-releasing write — runAdvance does). Each task may have a call
// and/or a switch: the call runs first, then the switch evaluates with the call's output
// as "self"; a matching case jumps to the named task, else the next task in the queue runs.
func (e *Engine) advance(ctx context.Context, inst *model.ProcessInstance) advanceOutcome {
	if inst.Status == model.StatusFailing {
		return e.settleFailing(inst)
	}
	if inst.Status == model.StatusPausing {
		// Crash recovery only: a pause normally lands in SQL when the owning worker
		// writes its finished task, so reaching a claim means that worker died mid-task.
		//
		// An interrupted only_once task is resolved here rather than parked, ignoring the
		// pending pause, because its evidence (ReclaimedExpired, from worker_id) does not
		// survive the write that settles a pause — deferring would decide on evidence
		// that no longer exists. Writing status 'running' then lets the CASE in
		// UpdateInstance land the pause, so a routed instance parks at its handler and
		// runs it on resume. See docs/only-once-interrupted.md.
		if inst.ReclaimedExpired {
			if task := e.lookupTask(inst); interruptedOnlyOnce(task) {
				inst.Status = model.StatusRunning
				return e.handleCallError(inst, task, interruptedMessage, errcode.OnlyOnceInterrupted)
			}
		}
		return e.settlePausing(inst)
	}

	def, idx, done := e.prepareAdvance(inst)
	if done != nil {
		return *done
	}

	// A call-less task has no external side effect, so a chain of them collapses into one
	// claim and one write: we continue in-memory and persist only at a task with a call, a
	// terminal state, or maxInlineTasks (a guard against an all-switch loop holding the
	// lease forever). Crash-safe because a switch only re-evaluates already-persisted
	// context, so resuming from the last written inst.Task is deterministic.
	const maxInlineTasks = 1000
	for i := 0; ; i++ {
		if idx < 0 || idx >= len(def.Tasks) {
			// Ran off the end of the task list: nothing left to do.
			inst.Task = ""
			inst.Status = model.StatusCompleted
			inst.WakeAt = nil
			if err := e.computeOutput(inst); err != nil {
				return e.failInstance(inst, errcode.EngineExpression, err.Error())
			}
			e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventInstanceDone, Data: e.outputData(inst)})
			return advanceOutcome{kind: outcomeTerminal}
		}

		task := def.Tasks[idx]
		// Point the instance at the task about to run, so any mid-task persist (park,
		// retry, error route, fail) records this task as the resume point.
		inst.Task = task.ID
		hasCall := task.Action != nil
		var actionResult any

		// Capture this task's prior output before the action can overwrite it, so an
		// output map may reference self.previous (the value from the last loop iteration).
		var priorOutput any
		if task.Output.Present() {
			if outs, ok := inst.ContextData["outputs"].(map[string]any); ok {
				priorOutput = outs[task.ID]
			}
		}

		if hasCall {
			switch task.Action.Type {
			case model.ActionTypeChild, model.ActionTypeChildMap, model.ActionTypeChildList:
				out, done := e.runChildProcesses(ctx, inst, task)
				if done != nil {
					return *done
				}
				actionResult = out
			case model.ActionTypeDelay:
				if done := e.runDelay(inst, task); done != nil {
					return *done
				}
				// Timer fired: fall through to the switch with no action result.
			case model.ActionTypeExternal:
				out, done := e.runExternal(ctx, inst, task)
				if done != nil {
					return *done
				}
				actionResult = out
			default: // fetch
				out, done := e.executeAction(ctx, inst, task)
				if done != nil {
					return *done
				}
				actionResult = out
			}
		}

		// The output projection (if any) is the only thing exported (outputs.taskID).
		// The raw result is never stored; it is exposed transiently to this task's own
		// output/switch as self.result.
		var taskOutput any
		hasOutput := task.Output.Present()
		if hasOutput {
			remapped, err := e.evalTaskOutput(inst, task, actionResult, priorOutput)
			if err != nil {
				return e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q output: %v", task.ID, err))
			}
			e.setTaskOutput(inst, task.ID, remapped)
			taskOutput = remapped
		}

		// self is this task's transient scope: result (raw action result) and
		// previous (its own prior output), plus output (the projection) only when one
		// is defined. None of these but the projection persist beyond this task.
		self := map[string]any{"result": actionResult, "previous": priorOutput}
		if hasOutput {
			self["output"] = taskOutput
		}
		matched, err := e.evalSwitch(inst, task, self)
		if err != nil {
			return e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q switch: %v", task.ID, err))
		}
		if matched == nil {
			// Validation requires a catch-all case, but legacy rows in the DB may
			// predate that rule — fail the instance rather than panic on gotoID[1:].
			return e.failInstance(inst, errcode.EngineDefinition, fmt.Sprintf("task %q switch: no case matched", task.ID))
		}

		// A terminal clause ends the process here, in place of routing. Neither computes
		// the process output: only `goto: end` finishes a process, and a raise or panic
		// is an exit from wherever the instance happens to have got to.
		if matched.Raise != nil {
			return e.raiseInstance(inst, task, matched.Raise)
		}
		if matched.Panic != nil {
			return e.panicInstance(inst, task, matched.Panic)
		}
		gotoID := matched.Goto

		if gotoID == model.GotoEnd {
			inst.Status = model.StatusCompleted
			inst.RetryCount = 0
			inst.WakeAt = nil
			if err := e.computeOutput(inst); err != nil {
				return e.failInstance(inst, errcode.EngineExpression, err.Error())
			}
			e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventInstanceDone, Task: task.ID, Data: e.outputData(inst)})
			return advanceOutcome{kind: outcomeTerminal}
		}

		if gotoID == model.GotoNext {
			idx++
		} else {
			// gotoID is a task reference like "$ship" — strip the sigil.
			if idx = taskIndex(def.Tasks, gotoID[1:]); idx < 0 {
				return e.failInstance(inst, errcode.EngineDefinition, fmt.Sprintf("goto task %q not found in %q v%d", gotoID[1:], inst.ProcessName, inst.ProcessVersion))
			}
		}
		// Reflect the new position (empty once we run past the last task) so a
		// checkpoint here persists the next task to run, not the one just completed.
		inst.Task = taskIDAt(def.Tasks, idx)

		inst.RetryCount = 0
		inst.WakeAt = nil
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventTaskCompleted, Task: task.ID, Msg: "→ " + gotoID})

		// A task with a call has just executed a side effect — checkpoint and yield.
		// A call-less routing task had none, so continue in-memory to the next task
		// unless we've hit the inline-task guard.
		if hasCall || i >= maxInlineTasks {
			return advanceOutcome{kind: outcomeProgress}
		}
	}
}

// evalTaskOutput evaluates a task's output map against the context plus self,
// where self.result is the raw action result and self.previous is this task's
// prior output (its value from the last loop iteration, or nil on the first run).
func (e *Engine) evalTaskOutput(inst *model.ProcessInstance, task *model.Task, result, previous any) (any, error) {
	self := map[string]any{"result": result, "previous": previous}
	return e.evalShape(inst, shape.Shape{Raw: task.Output.Raw}, self)
}

// setTaskOutput stores value as the task's exported output (outputs.taskID), appending to
// output_order only the first time the task produces output (a loop re-execution
// overwrites the value without re-appending).
func (e *Engine) setTaskOutput(inst *model.ProcessInstance, taskID string, value any) {
	if inst.ContextData["outputs"] == nil {
		inst.ContextData["outputs"] = map[string]any{}
	}
	outs := inst.ContextData["outputs"].(map[string]any)
	_, existed := outs[taskID]
	outs[taskID] = value
	if existed {
		return
	}
	appendOutputOrder(inst, taskID)
}

// appendOutputOrder appends id to the instance's output_order list, tolerating the
// []any shape the field takes after a JSON round-trip through engine_state.
func appendOutputOrder(inst *model.ProcessInstance, id string) {
	var order []string
	switch v := inst.ContextData["output_order"].(type) {
	case []string:
		order = v
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				order = append(order, s)
			}
		}
	}
	inst.ContextData["output_order"] = append(order, id)
}

// evalSwitch returns the first switch case whose Case expression is true. An empty Case
// is a catch-all (must be last when present). Returns nil when no case matches (should
// not happen on validated definitions).
//
// It returns the whole case rather than just its Goto because a case may terminate
// instead of routing (raise/panic), and "" is not a distinguishable answer for that --
// it is also what a nil case would yield.
func (e *Engine) evalSwitch(inst *model.ProcessInstance, task *model.Task, selfOutput any) (*model.SwitchCase, error) {
	for i := range task.Switch {
		c := &task.Switch[i]
		if c.Case == "" {
			return c, nil
		}
		v, err := e.evalShape(inst, shape.Shape{Raw: c.Case, Expr: true}, selfOutput)
		if err != nil {
			return nil, fmt.Errorf("case %q: %w", c.Case, err)
		}
		ok, isBool := v.(bool)
		if !isBool {
			return nil, fmt.Errorf("case %q: expected bool, got %T", c.Case, v)
		}
		if ok {
			return c, nil
		}
	}
	return nil, nil
}

// Returns nil when there is no current task or the definition cannot be read. Callers
// that must fail on a missing definition use prepareAdvance instead; this is for the
// settle paths, which must not turn a transient read error into a failed process.
func (e *Engine) lookupTask(inst *model.ProcessInstance) *model.Task {
	if inst.Task == "" {
		return nil
	}
	def, err := e.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return nil
	}
	idx := taskIndex(def.Tasks, inst.Task)
	if idx < 0 {
		return nil
	}
	return def.Tasks[idx]
}

// taskIndex returns the position of taskID in tasks, or -1 if absent (the empty id —
// "no current task" — is always absent).
func taskIndex(tasks []*model.Task, taskID string) int {
	if taskID == "" {
		return -1
	}
	for i, t := range tasks {
		if t.ID == taskID {
			return i
		}
	}
	return -1
}

// taskIDAt returns the id of the task at idx, or "" when idx is out of range (the
// instance has advanced past the last task).
func taskIDAt(tasks []*model.Task, idx int) string {
	if idx < 0 || idx >= len(tasks) {
		return ""
	}
	return tasks[idx].ID
}

// resolveGoto validates that the instance's definition contains taskID so the engine can
// point the instance at it (no queue is built — successors are implied by definition
// order). Used by the on-error route, which has no definition in scope.
func (e *Engine) resolveGoto(inst *model.ProcessInstance, taskID string) error {
	def, err := e.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return fmt.Errorf("resolve goto: %w", err)
	}
	if taskIndex(def.Tasks, taskID) < 0 {
		return fmt.Errorf("goto task %q not found in %q v%d", taskID, inst.ProcessName, inst.ProcessVersion)
	}
	return nil
}

// saveAndNotify is the single exit point for all terminal instance states. Root and
// failed instances save directly; a non-failed child uses FinishChild, which atomically
// saves it and moves the parent to WaitStateCollecting once all siblings are done.
func (e *Engine) saveAndNotify(inst *model.ProcessInstance) error {
	if inst.ParentID == "" {
		return e.db.UpdateInstance(inst)
	}
	if inst.Status == model.StatusFailed {
		return e.db.FailInstanceAndAncestors(inst)
	}
	return e.db.FinishChild(inst)
}

// computeOutput evaluates the definition's Output map against the final context and
// stores it in context_data["output"]. No-op when the definition has no Output map.
func (e *Engine) computeOutput(inst *model.ProcessInstance) error {
	def, err := e.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return fmt.Errorf("load definition for output: %w", err)
	}
	if !def.Output.Present() {
		return nil
	}
	out, err := e.evalShape(inst, shape.Shape{Raw: def.Output.Raw}, nil)
	if err != nil {
		return fmt.Errorf("output: %w", err)
	}
	inst.ContextData["output"] = out
	return nil
}
