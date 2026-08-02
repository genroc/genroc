package engine

import (
	"fmt"

	"genroc/internal/db"
	"genroc/internal/errcode"
	"genroc/internal/model"
)

// isRetryAllowed reports whether a retry is safe for this task and error. On an only_once
// task that means a pre.* code or an explicit not_reached:true — except for the unknowable
// codes, which nothing can buy back.
//
// validateOnError rejects such a rule at registration, but a definition stored before that
// rule never re-validates, so this test is what holds the line at runtime.
func isRetryAllowed(task *model.Task, errCode errcode.Code, matched *model.ErrorCase) bool {
	if task.OnlyOnce == nil || !*task.OnlyOnce {
		return true
	}
	if errCode.IsUnknowable() {
		return false
	}
	if matched != nil && matched.NotReached != nil && *matched.NotReached {
		return true
	}
	return errCode.IsNotReached()
}

// interruptedOnlyOnce is the question both reclaim paths ask, and the only situation that
// produces errcode.OnlyOnceInterrupted.
func interruptedOnlyOnce(task *model.Task) bool {
	return task != nil && task.Action != nil && task.OnlyOnce != nil && *task.OnlyOnce
}

// interruptedMessage says what happened to the task, not to the worker: a definition has
// no lease to reason about.
const interruptedMessage = "its previous attempt was interrupted; the engine will not re-run it"

// matchOnError returns the first ErrorCase matching errCode, or the catch-all (empty Code
// list), or nil. Serves both action tasks (engine codes) and child tasks (a child's raised
// code) through the same matcher.
func matchOnError(task *model.Task, errCode errcode.Code) *model.ErrorCase {
	for i := range task.OnError {
		c := &task.OnError[i]
		if len(c.Code) == 0 {
			return c
		}
		for _, pat := range c.Code {
			if errcode.MatchCode(pat, string(errCode)) {
				return c
			}
		}
	}
	return nil
}

// handleCallError evaluates on_error rules, retries if allowed, injects $error, and routes
// to the matching goto or fails the instance, returning the outcome for runAdvance to
// write. A pending pause needs no case here: the write that persists the outcome lands it
// (the CASE in UpdateInstance), so a paused instance keeps the attempt it was granted.
func (e *Engine) handleCallError(inst *model.ProcessInstance, task *model.Task, errMsg string, errCode errcode.Code) advanceOutcome {
	matched := matchOnError(task, errCode)

	if matched != nil && inst.RetryCount < matched.Retries && isRetryAllowed(task, errCode, matched) {
		inst.RetryCount++
		next := db.Now().Add(e.retryDelay(inst.RetryCount))
		inst.WakeAt = &next
		retryMsg := fmt.Sprintf("%s (attempt %d/%d)", errMsg, inst.RetryCount, matched.Retries)
		e.audit(inst, logEvent{Level: model.LogWarn, Event: model.EventRetryScheduled, Task: task.ID, Msg: retryMsg, Code: errCode})
		return advanceOutcome{kind: outcomeUpdate}
	}

	inst.ContextData["error"] = map[string]any{
		"task":    task.ID,
		"message": errMsg,
		"code":    string(errCode),
	}

	// An authored terminal clause outranks routing. Both keep the engine's own code in
	// $error (above) so the underlying cause stays visible on the instance detail, while
	// error_code becomes the authored one -- the code an operator filters and alerts on.
	if matched != nil && matched.Raise != nil {
		return e.raiseInstance(inst, task, matched.Raise)
	}
	if matched != nil && matched.Panic != nil {
		return e.panicInstance(inst, task, matched.Panic)
	}

	if matched != nil && matched.Goto != "" {
		if matched.Goto == model.GotoEnd {
			return e.completeViaErrorHandler(inst, task, errMsg, errCode)
		}
		if err := e.resolveGoto(inst, matched.Goto); err != nil {
			return e.failInstance(inst, errcode.EngineDefinition, err.Error())
		}
		inst.Task = matched.Goto
		inst.RetryCount = 0
		inst.WakeAt = nil
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventErrorRoute, Task: task.ID, Msg: errMsg + " → " + matched.Goto, Code: errCode})
		return advanceOutcome{kind: outcomeUpdate}
	}

	return e.failInstance(inst, errCode, fmt.Sprintf("task %q: %s: %s", task.ID, errCode, errMsg))
}

// completeViaErrorHandler finalizes an instance whose on_error handling routed it to
// `end`: an anticipated error was caught and the process completes normally. Both the
// action-task path (handleCallError) and the child-batch path (resolveRaisedBatch) go
// through here, so the two cannot drift — in particular the process output is computed on
// this path exactly as it is on a normal end. (An earlier version of the action-task path
// skipped computeOutput here, so a process that completed via on_error → end silently
// produced no output; that is the divergence this shared helper removes.) A failing
// output expression fails the instance instead.
//
// msg/code are the caught error's — the engine code on the action path, the child's
// raised code on the batch path — recorded on the EventErrorCompleted audit.
func (e *Engine) completeViaErrorHandler(inst *model.ProcessInstance, task *model.Task, msg string, code errcode.Code) advanceOutcome {
	inst.Status = model.StatusCompleted
	inst.RetryCount = 0
	inst.WakeAt = nil
	if err := e.computeOutput(inst); err != nil {
		return e.failInstance(inst, errcode.EngineExpression, err.Error())
	}
	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventErrorCompleted, Task: task.ID, Msg: msg, Code: code})
	return advanceOutcome{kind: outcomeTerminal}
}

// raiseInstance concludes the instance as 'raised': an anticipated condition the parent
// may react to by naming the code. See docs/child-error-handling.md.
//
// A raise must keep falling through to FinishChild rather than the failure path — it is a
// normal outcome and must not mark ancestors 'failing'. And no process output is computed,
// which is why a raise site is not a terminal for validating the `output:` expression.
func (e *Engine) raiseInstance(inst *model.ProcessInstance, task *model.Task, f *model.Fault) advanceOutcome {
	inst.Status = model.StatusRaised
	inst.WaitState = model.WaitStateNone
	inst.Error = f.Message
	inst.ErrorCode = f.Code
	inst.WakeAt = nil
	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventInstanceRaised, Task: task.ID, Msg: f.Message, Code: errcode.Code(f.Code)})
	return advanceOutcome{kind: outcomeTerminal}
}

// panicInstance is failInstance with the author's words: authoring a defect grants it no
// special status, so nothing can catch it and it poisons ancestors the same way.
func (e *Engine) panicInstance(inst *model.ProcessInstance, task *model.Task, f *model.Fault) advanceOutcome {
	return e.failInstance(inst, errcode.Code(f.Code), f.Message)
}

// failInstance moves the instance to failed and returns the terminal outcome. code is
// required from every caller so no failure path leaves error_code empty.
func (e *Engine) failInstance(inst *model.ProcessInstance, code errcode.Code, reason string) advanceOutcome {
	inst.Status = model.StatusFailed
	inst.WaitState = model.WaitStateNone
	inst.Error = reason
	inst.ErrorCode = string(code)
	inst.WakeAt = nil
	e.audit(inst, logEvent{Level: model.LogError, Event: model.EventInstanceFailed, Msg: reason, Code: code})
	return advanceOutcome{kind: outcomeTerminal}
}

// settlePausing lands a 'pausing' instance in 'paused', touching nothing else so that
// resuming is a status flip. Reached only when a worker died holding the instance; the
// normal pause lands in SQL when the owning worker writes its finished task.
//
// It must not regain an only_once check: advance() resolves an interrupted only_once task
// before the pause, because the evidence (ReclaimedExpired, derived per claim from
// worker_id) does not survive the write that settles a pause. See docs/pause-resume.md.
func (e *Engine) settlePausing(inst *model.ProcessInstance) advanceOutcome {
	inst.Status = model.StatusPaused
	// The other half of inst_paused: PauseProcess logs the rows it settled itself, this
	// covers the leased one it could only mark 'pausing'.
	e.audit(inst, logEvent{Level: model.LogDebug, Event: model.EventPaused, Task: inst.Task,
		Msg: "in-flight task settled; instance paused"})
	return advanceOutcome{kind: outcomeTerminal}
}

// settleFailing finalises a draining 'failing' instance once its children have settled
// (it only becomes claimable then). The error was recorded when the failure propagated up.
func (e *Engine) settleFailing(inst *model.ProcessInstance) advanceOutcome {
	inst.Status = model.StatusFailed
	inst.WaitState = model.WaitStateNone
	inst.WakeAt = nil
	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventInstanceSettled, Msg: inst.Error})
	return advanceOutcome{kind: outcomeTerminal}
}
