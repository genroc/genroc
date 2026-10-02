package engine

import (
	"fmt"

	"genroc/internal/db"
	"genroc/internal/errcode"
	"genroc/internal/model"
	"genroc/internal/shape"
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
	return task.OnlyOnceAction()
}

// interruptedMessage says what happened to the task, not to the worker: a definition has
// no lease to reason about.
const interruptedMessage = "its previous attempt was interrupted; the engine will not re-run it"

// matchOnErrorWith: a false `case` falls THROUGH to the next rule, and one that fails to
// evaluate is an error, never a non-match. With eval nil, a rule with a case is skipped.
// specs/child-error-handling.md M2.
func matchOnErrorWith(task *model.Task, errCode errcode.Code, eval func(string) (bool, error)) (*model.ErrorCase, error) {
	for i := range task.OnError {
		c := &task.OnError[i]
		matched := len(c.Code) == 0
		for _, pat := range c.Code {
			if errcode.MatchCode(pat, string(errCode)) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if c.Case == "" {
			return c, nil
		}
		if eval == nil {
			continue
		}
		ok, err := eval(c.Case)
		if err != nil {
			return nil, err
		}
		if ok {
			return c, nil
		}
	}
	return nil, nil
}

// handleCallError needs no pause case: the CASE in UpdateInstance lands it on the write,
// so a paused instance keeps the attempt it was granted.
func (e *Engine) handleCallError(inst *model.ProcessInstance, task *model.Task, errMsg string, errCode errcode.Code) advanceOutcome {
	return e.handleCallErrorWith(inst, task, errMsg, errCode, nil)
}

// handleCallErrorWith merges extra into `error`. A nil map is NOT a map holding a nil `data`:
// key presence is what says the shape was described.
func (e *Engine) handleCallErrorWith(inst *model.ProcessInstance, task *model.Task, errMsg string, errCode errcode.Code, extra map[string]any) advanceOutcome {
	// Built here, not at the write below: matching needs it, and a declined rule or a granted
	// retry must leave nothing behind.
	caseErr := map[string]any{"task": task.ID, "message": errMsg, "code": string(errCode)}
	for k, v := range extra {
		caseErr[k] = v
	}
	matched, matchErr := matchOnErrorWith(task, errCode, e.caseEvaluator(inst, caseErr))
	if matchErr != nil {
		return e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q: %v", task.ID, matchErr))
	}

	// A resolution failure fails the instance: a policy that quietly became "no retries" is an
	// author's budget vanishing with nothing reporting it.
	var policy model.ResolvedRetry
	if matched != nil && !matched.Retry.IsZero() {
		resolved, err := e.resolveRetry(inst, matched.Retry, caseErr)
		if err != nil {
			return e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q on_error: %v", task.ID, err))
		}
		policy = resolved
	}

	if inst.RetryCount < policy.Retries && isRetryAllowed(task, errCode, matched) {
		inst.RetryCount++
		// A retry is a new OCCURRENCE without a transition, and an external token is derived from
		// the epoch (see runExternal).
		inst.TaskEpoch++
		next := db.Now().Add(e.retryDelay(inst.RetryCount, policy))
		inst.WakeAt = &next
		retryMsg := fmt.Sprintf("%s (retry %d/%d)", errMsg, inst.RetryCount, policy.Retries)
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
	// The write is deferred: until this rule is done, `last_error` is still the failure that
	// routed INTO this task, which a rule may name beside the one it caught. specs/task-scopes.md.
	release := bindCaught(inst, errCtx)
	defer func() {
		release()
		inst.State[model.StateLastError] = errCtx
	}()

	// `last_error` keeps the engine's code so the cause stays visible; error_code becomes the
	// authored one.
	if matched != nil && matched.Raise != nil {
		return e.raiseInstance(inst, task, matched.Raise, e.selfBeforeOutput(inst))
	}
	if matched != nil && matched.Panic != nil {
		return e.panicInstance(inst, task, matched.Panic, e.selfBeforeOutput(inst))
	}

	if matched != nil && matched.Goto != "" {
		if matched.Goto == model.GotoEnd {
			return e.completeViaErrorHandler(inst, task, errMsg, errCode)
		}
		if err := e.resolveGoto(inst, matched.Goto); err != nil {
			return e.failInstance(inst, errcode.EngineDefinition, err.Error())
		}
		enterTask(inst, matched.Goto)
		inst.RetryCount = 0
		inst.WakeAt = nil
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventErrorRoute, Task: task.ID, Msg: errMsg + " → " + matched.Goto, Code: errCode})
		return advanceOutcome{kind: outcomeUpdate}
	}

	return e.failInstance(inst, errCode, fmt.Sprintf("task %q: %s: %s", task.ID, errCode, errMsg))
}

// completeViaErrorHandler is shared by the action and batch paths so they cannot drift: a
// fork once silently dropped the process output.
func (e *Engine) completeViaErrorHandler(inst *model.ProcessInstance, task *model.Task, msg string, code errcode.Code) advanceOutcome {
	inst.Status = model.StatusCompleted
	inst.RetryCount = 0
	inst.WakeAt = nil
	if err := e.computeOutput(inst); err != nil {
		return e.failInstance(inst, declaredFailureCode(err, errcode.EngineOutput, errcode.EngineExpression), err.Error())
	}
	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventErrorCompleted, Task: task.ID, Msg: msg, Code: code})
	return advanceOutcome{kind: outcomeTerminal}
}

// faultMessage never renders the CODE: a literal keeps the raise set computable. A render
// failure falls back to the source text, since the instance is already concluding.
func (e *Engine) faultMessage(inst *model.ProcessInstance, f *model.Fault, self any) string {
	rendered, err := e.evalShape(inst, shape.Shape{Raw: f.Message}, self)
	if err != nil {
		// Degrade, but not SILENTLY: `${ }` is legal literal text (`$${` escapes it), so an
		// unrendered template reads as intended.
		e.audit(inst, logEvent{Level: model.LogWarn, Event: model.EventRetryScheduled, Task: inst.Task,
			Msg: fmt.Sprintf("fault message did not render, emitting its source text: %v", err)})
		return f.Message
	}
	if s, ok := rendered.(string); ok {
		return s
	}
	// Registration guaranteed a string. Show the value: it is the evidence, and the reader
	// already has the template.
	e.audit(inst, logEvent{Level: model.LogWarn, Event: model.EventRetryScheduled, Task: inst.Task,
		Msg: fmt.Sprintf("fault message rendered to %T, not a string", rendered)})
	return fmt.Sprint(rendered)
}

// raiseInstance concludes as 'raised' (specs/child-error-handling.md). It must keep
// falling through to FinishChild — a raise is a normal outcome, never marks ancestors
// failing — and computes no process output (a raise site is not an output terminal).
func (e *Engine) raiseInstance(inst *model.ProcessInstance, task *model.Task, f *model.Fault, self any) advanceOutcome {
	data, err := e.evalFaultData(inst, f, self)
	if err != nil {
		return e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q raise data: %v", task.ID, err))
	}
	msg := e.faultMessage(inst, f, self)
	inst.Status = model.StatusRaised
	inst.Phase = model.PhaseNone
	inst.ErrorMessage = msg
	inst.ErrorCode = f.Code
	inst.WakeAt = nil
	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventInstanceRaised, Task: task.ID, Msg: msg, Code: errcode.Code(f.Code)})
	setErrorData(inst, data)
	return advanceOutcome{kind: outcomeTerminal}
}

// panicInstance is failInstance with the author's words: authoring a defect grants it no
// special status, so nothing can catch it and it poisons ancestors the same way.
func (e *Engine) panicInstance(inst *model.ProcessInstance, task *model.Task, f *model.Fault, self any) advanceOutcome {
	data, err := e.evalFaultData(inst, f, self)
	if err != nil {
		return e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q panic data: %v", task.ID, err))
	}
	msg := e.faultMessage(inst, f, self)
	out := e.failInstance(inst, errcode.Code(f.Code), msg)
	setErrorData(inst, data)
	return out
}

// evalFaultData must run BEFORE the clause concludes: its scope includes the `error` being
// handled. Never degraded like a message: the payload is a contract.
// specs/error-extensions.md §X2-c.
func (e *Engine) evalFaultData(inst *model.ProcessInstance, f *model.Fault, self any) (any, error) {
	if !f.Data.Present() {
		return nil, nil
	}
	return e.evalShape(inst, *f.Data, self)
}

// setErrorData leaves the slot ABSENT for no payload, which is what tells a parent's collect
// there is nothing to conform. Never touch `error`: an upgrade validates what was CAUGHT.
func setErrorData(inst *model.ProcessInstance, data any) {
	if data == nil {
		delete(inst.State, model.StateErrorData)
		return
	}
	inst.State[model.StateErrorData] = data
}

// failInstance moves the instance to failed and returns the terminal outcome. code is
// required from every caller so no failure path leaves error_code empty.
func (e *Engine) failInstance(inst *model.ProcessInstance, code errcode.Code, reason string) advanceOutcome {
	inst.Status = model.StatusFailed
	inst.Phase = model.PhaseNone
	inst.ErrorMessage = reason
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

// settleCancelling is reached only when a worker died holding the instance. No interrupted
// only_once resolution first: routing it into on_error carries on what the operator forbade.
func (e *Engine) settleCancelling(inst *model.ProcessInstance) advanceOutcome {
	// Status only: phase and wake_at record what the instance was doing, and
	// ReleaseExternalClaim finds a claim by them.
	inst.Status = model.StatusCancelled
	// The other half of inst_cancelled: CancelProcess logs the rows it settled itself, this
	// covers the leased one it could only mark 'cancelling'.
	e.audit(inst, logEvent{Level: model.LogDebug, Event: model.EventCancelled, Task: inst.Task,
		Msg: "in-flight task settled; instance cancelled"})
	return advanceOutcome{kind: outcomeTerminal}
}

// settleFailing finalises a draining 'failing' instance once its children have settled
// (it only becomes claimable then). The error was recorded when the failure propagated up.
func (e *Engine) settleFailing(inst *model.ProcessInstance) advanceOutcome {
	inst.Status = model.StatusFailed
	inst.Phase = model.PhaseNone
	inst.WakeAt = nil
	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventInstanceSettled, Msg: inst.ErrorMessage})
	return advanceOutcome{kind: outcomeTerminal}
}
