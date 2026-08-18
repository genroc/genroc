package engine

import (
	"fmt"

	"genroc/internal/db"
	"genroc/internal/errcode"
	"genroc/internal/model"
)

// isRetryAllowed: on an only_once task a retry needs a pre.* code or not_reached:true,
// and the unknowable codes nothing can buy back. Not redundant with validateOnError —
// definitions stored before that rule never re-validate, so this holds the line at runtime.
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
	return e.handleCallErrorWith(inst, task, errMsg, errCode, nil)
}

// handleCallErrorWith is handleCallError plus fields merged into $error beyond
// task/message/code — today only `data`, the body of an unaccepted response whose status a
// `responses` key declared. A nil map leaves $error exactly as it was; a map holding a nil
// `data` is NOT the same thing, because key presence is what says the status was described.
func (e *Engine) handleCallErrorWith(inst *model.ProcessInstance, task *model.Task, errMsg string, errCode errcode.Code, extra map[string]any) advanceOutcome {
	matched := matchOnError(task, errCode)

	if matched != nil && inst.RetryCount < matched.Retry.Attempts && isRetryAllowed(task, errCode, matched) {
		inst.RetryCount++
		next := db.Now().Add(e.retryDelay(inst.RetryCount, matched.Retry))
		inst.WakeAt = &next
		retryMsg := fmt.Sprintf("%s (attempt %d/%d)", errMsg, inst.RetryCount, matched.Retry.Attempts)
		e.audit(inst, logEvent{Level: model.LogWarn, Event: model.EventRetryScheduled, Task: task.ID, Msg: retryMsg, Code: errCode})
		return advanceOutcome{kind: outcomeUpdate}
	}

	errCtx := map[string]any{
		"task":    task.ID,
		"message": errMsg,
		"code":    string(errCode),
	}
	for k, v := range extra {
		errCtx[k] = v
	}
	inst.ContextData["error"] = errCtx

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

// completeViaErrorHandler finalizes an on_error → end route; the action path and the batch
// path both come through here so they cannot drift — the process output is computed
// exactly as on a normal end (a fork of this once silently dropped it). msg/code are the
// caught error's, recorded on EventErrorCompleted.
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

// raiseInstance concludes as 'raised' (specs/child-error-handling.md). It must keep
// falling through to FinishChild — a raise is a normal outcome, never marks ancestors
// failing — and computes no process output (a raise site is not an output terminal).
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

// settlePausing lands 'pausing' in 'paused' touching nothing else (resume = status flip);
// reached only when a worker died holding the instance. It must NOT regain an only_once
// check — advance() resolves that first, before the evidence dies. specs/pause-resume.md.
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
