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

// matchOnError returns the first ErrorCase matching errCode, or the catch-all (empty Code
// list), or nil. Serves both action tasks (engine codes) and child tasks (a child's raised
// code) through the same matcher.
func matchOnError(task *model.Task, errCode errcode.Code) *model.ErrorCase {
	c, _ := matchOnErrorWith(task, errCode, nil)
	return c
}

// matchOnErrorWith is matchOnError with M2's predicate: a rule carrying a `case` applies only
// when the code matches AND the case is true, and a false case falls THROUGH to the next rule
// — without that the guard could only ever turn a match into a failure.
//
// eval is nil where no case can appear (the caller has no scope to evaluate one in); a rule
// with a case is then skipped rather than silently treated as matching. A case that will not
// evaluate is an error, never a non-match: it is type-checked at registration, so a runtime
// failure means that guarantee did not hold, and quietly declining would route the error
// somewhere the author never wrote. specs/child-error-handling.md M2.
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

// handleCallError evaluates on_error rules, retries if allowed, injects `error`, and routes
// to the matching goto or fails the instance, returning the outcome for runAdvance to
// write. A pending pause needs no case here: the write that persists the outcome lands it
// (the CASE in UpdateInstance), so a paused instance keeps the attempt it was granted.
func (e *Engine) handleCallError(inst *model.ProcessInstance, task *model.Task, errMsg string, errCode errcode.Code) advanceOutcome {
	return e.handleCallErrorWith(inst, task, errMsg, errCode, nil)
}

// handleCallErrorWith is handleCallError plus fields merged into `error` beyond
// task/message/code — today only `data`: the body of an unaccepted response whose status a
// `responses` key declared, or the payload a worker submitted for a code an external task
// declared under `raises`. A nil map leaves `error` exactly as it was; a map holding a nil
// `data` is NOT the same thing, because key presence is what says the shape was described.
func (e *Engine) handleCallErrorWith(inst *model.ProcessInstance, task *model.Task, errMsg string, errCode errcode.Code, extra map[string]any) advanceOutcome {
	// The `error` an M2 case reads is the one this call is about to report, built here rather
	// than at the write below: matching needs it, and the retry branch returns before any
	// write, so a rule that declines — or a retry that is granted — leaves nothing behind.
	caseErr := map[string]any{"task": task.ID, "message": errMsg, "code": string(errCode)}
	for k, v := range extra {
		caseErr[k] = v
	}
	matched, matchErr := matchOnErrorWith(task, errCode, e.caseEvaluator(inst, caseErr))
	if matchErr != nil {
		return e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q: %v", task.ID, matchErr))
	}

	// Any slot of a policy may be a "$:" expression, so it is resolved before it is
	// consulted. A resolution failure fails the instance rather than falling through: a
	// policy that quietly became "no retries" is the attempt budget an author wrote
	// vanishing with nothing reporting it.
	var policy model.ResolvedRetry
	if matched != nil && !matched.Retry.IsZero() {
		resolved, err := matched.Retry.Resolve(func(expr string) (any, error) {
			return e.evalShape(inst, shape.Shape{Raw: expr}, nil)
		})
		if err != nil {
			return e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q on_error: %v", task.ID, err))
		}
		policy = resolved
	}

	if inst.RetryCount < policy.Attempts && isRetryAllowed(task, errCode, matched) {
		inst.RetryCount++
		// A retry re-attempts the task without transitioning, so nothing else moves the
		// epoch here -- and the next attempt is a new OCCURRENCE, which is what an external
		// task's token has to be unique per (see runExternal). Making the epoch count
		// attempts rather than only entries is what lets the token be derived from it.
		inst.TaskEpoch++
		next := db.Now().Add(e.retryDelay(inst.RetryCount, policy))
		inst.WakeAt = &next
		retryMsg := fmt.Sprintf("%s (attempt %d/%d)", errMsg, inst.RetryCount, policy.Attempts)
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
	inst.State["error"] = errCtx

	// An authored terminal clause outranks routing. Both keep the engine's own code in
	// `error` (above) so the underlying cause stays visible on the instance detail, while
	// error_code becomes the authored one -- the code an operator filters and alerts on.
	if matched != nil && matched.Raise != nil {
		return e.raiseInstance(inst, task, matched.Raise, nil)
	}
	if matched != nil && matched.Panic != nil {
		return e.panicInstance(inst, task, matched.Panic, nil)
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

// faultMessage renders a fault's message against the scope its clause fires in: a switch
// case passes `self`, an on_error or collect rule passes nil and reads the `error` already
// written to the context. The CODE is never rendered — it stays a literal so the raise set
// remains computable (model.faultCodeRe).
//
// A render failure falls back to the source text rather than escalating. This runs while the
// instance is already concluding, so there is nowhere to report a second error to; the
// unrendered template is then visible in the message, where turning a clean raise into an
// engine fault would lose the outcome the author asked for.
func (e *Engine) faultMessage(inst *model.ProcessInstance, f *model.Fault, self any) string {
	rendered, err := e.evalShape(inst, shape.Shape{Raw: f.Message}, self)
	if err != nil {
		// Degrading is right -- escalating would trade the outcome the author asked for
		// against a cosmetic failure -- but it must not be SILENT. `${ }` is also legal
		// literal text (that is what `$${` escapes), so an unrendered template is
		// indistinguishable from an intended one to whoever reads the message.
		e.audit(inst, logEvent{Level: model.LogWarn, Event: model.EventRetryScheduled, Task: inst.Task,
			Msg: fmt.Sprintf("fault message did not render, emitting its source text: %v", err)})
		return f.Message
	}
	if s, ok := rendered.(string); ok {
		return s
	}
	// Registration type-checks a message to a non-null string, so arriving here means that
	// guarantee did not hold at runtime. Show the value rather than the template: it is the
	// evidence, and the template is what the reader already has.
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
	inst.WaitState = model.WaitStateNone
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

// evalFaultData evaluates a clause's `data` in the scope its message renders in. It must run
// BEFORE the clause concludes: the scope includes the `error` this instance is handling, which
// a fault reached through on_error reads to recompose its own payload.
//
// A failed evaluation is not degraded the way a message is: the payload is a contract, so
// dropping it silently would report the loss at the caller's conform rather than here.
// specs/error-extensions.md §X2-c.
func (e *Engine) evalFaultData(inst *model.ProcessInstance, f *model.Fault, self any) (any, error) {
	if !f.Data.Present() {
		return nil, nil
	}
	return e.evalShape(inst, *f.Data, self)
}

// setErrorData lands the clause's payload in its own slot; the code and message it concluded
// with are already on the row. The slot is ABSENT where the clause carried nothing, which is
// what tells a parent's collect there is no payload to conform.
//
// It must not touch `error`: that slot is the error this instance CAUGHT and is part of its
// state at this task, so a fault editing it leaves a concluded instance holding a context no
// layer describes -- the shape an upgrade validates against.
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
	inst.WaitState = model.WaitStateNone
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

// settleFailing finalises a draining 'failing' instance once its children have settled
// (it only becomes claimable then). The error was recorded when the failure propagated up.
func (e *Engine) settleFailing(inst *model.ProcessInstance) advanceOutcome {
	inst.Status = model.StatusFailed
	inst.WaitState = model.WaitStateNone
	inst.WakeAt = nil
	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventInstanceSettled, Msg: inst.ErrorMessage})
	return advanceOutcome{kind: outcomeTerminal}
}
