package engine

import (
	"fmt"

	"genroc/internal/db"
	"genroc/internal/errcode"
	"genroc/internal/model"
	"genroc/internal/transport"
)

// All engine-produced codes live in package errcode (the single source of truth). The
// engine.* ones below are the failures the engine detects itself, as opposed to the ones a
// call reports (http.*, pre.*, output.*, external.timeout) or an author declares with
// panic. Every terminal failure carries a code so error_code is uniformly queryable.

// isRetryAllowed reports whether a retry is safe for the given task and error.
// For idempotent tasks (the default) retries are always governed by on_error rules.
// For non-idempotent tasks, a retry is only allowed when we know the remote call
// never started: a pre.* (not-reached) code, or an on_error rule with not_reached:true.
func isRetryAllowed(task *model.Task, errCode errcode.Code, matched *model.ErrorCase) bool {
	if task.OnlyOnce == nil || !*task.OnlyOnce {
		return true
	}
	if matched != nil && matched.NotReached != nil && *matched.NotReached {
		return true
	}
	return errCode.IsNotReached()
}

// matchOnError returns the first ErrorCase whose Code patterns match errCode,
// or whose Code list is empty (catch-all). Returns nil when no rule matches.
// Used for both action tasks (matching engine codes) and child tasks (matching a child's
// raised code) — the same transport.MatchCode, where `%` is the only wildcard, so
// `order_%` matches `order_placed` but not `order.placed`. A child task's patterns were
// checked at registration against the child raise set (R5), so a rule that fires here can
// always match something the child actually raises.
func matchOnError(task *model.Task, errCode errcode.Code) *model.ErrorCase {
	for i := range task.OnError {
		c := &task.OnError[i]
		if len(c.Code) == 0 {
			return c
		}
		for _, pat := range c.Code {
			if transport.MatchCode(pat, string(errCode)) {
				return c
			}
		}
	}
	return nil
}

// handleCallError evaluates on_error rules, retries if allowed, injects $error
// context, and routes to the matching goto or fails the instance. It returns the
// outcome to persist (runAdvance writes it).
// A pause needs no special case here. The on_error rules run exactly as they would
// otherwise: a retry is scheduled with its normal backoff, and the write that persists
// it lands the pending pause (the CASE in UpdateInstance), so the instance settles into
// 'paused' still holding the attempt the definition granted it. Resuming continues that
// schedule with nothing spent and nothing skipped — which is the whole difference
// between resuming a pause and retrying a failure.
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

// raiseInstance concludes the instance as 'raised': an anticipated condition the
// definition declared, which the parent may react to by naming the code.
//
// It needs no draining state and no special plumbing. saveAndNotify branches on
// StatusFailed, so a raised child falls through to FinishChild -- the right
// destination, because a raise is a normal outcome and must not mark ancestors
// 'failing'. And a raise happens at a task boundary where this instance's own children
// have already collected, so there is nothing left to drain.
//
// No process output is computed: a raise is not an `end`, and its context is whatever
// the instance had reached. That is also why a raise site is not a terminal for the
// purpose of validating the process `output:` expression.
func (e *Engine) raiseInstance(inst *model.ProcessInstance, task *model.Task, f *model.Fault) advanceOutcome {
	inst.Status = model.StatusRaised
	inst.WaitState = model.WaitStateNone
	inst.Error = f.Message
	inst.ErrorCode = f.Code
	inst.WakeAt = nil
	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventInstanceRaised, Task: task.ID, Msg: f.Message, Code: errcode.Code(f.Code)})
	return advanceOutcome{kind: outcomeTerminal}
}

// panicInstance fails the instance with an authored code and message. It is exactly
// failInstance with the author's words instead of the engine's: a panic is a defect like
// any other, and authoring one grants it no special status -- nothing can catch it, and
// it poisons its ancestors through the same path an engine failure does.
func (e *Engine) panicInstance(inst *model.ProcessInstance, task *model.Task, f *model.Fault) advanceOutcome {
	// The authored code becomes a Code here: this conversion is the one place a
	// definition's panic clause enters the engine's error_code slot.
	return e.failInstance(inst, errcode.Code(f.Code), f.Message)
}

// failInstance moves the instance to its failed state and returns the terminal
// outcome (persisted by runAdvance via saveAndNotify). code is the machine-readable
// discriminator stored in error_code; every caller must supply one, so that no failure
// path can quietly leave the column empty.
func (e *Engine) failInstance(inst *model.ProcessInstance, code errcode.Code, reason string) advanceOutcome {
	inst.Status = model.StatusFailed
	inst.WaitState = model.WaitStateNone
	inst.Error = reason
	inst.ErrorCode = string(code)
	inst.WakeAt = nil
	e.audit(inst, logEvent{Level: model.LogError, Event: model.EventInstanceFailed, Msg: reason, Code: code})
	return advanceOutcome{kind: outcomeTerminal}
}

// settlePausing lands a 'pausing' instance in 'paused' without touching anything else:
// wait_state, wake_at, retry_count and context all carry over untouched, so resuming is
// a status flip and the instance picks up exactly where it stopped. Timers keep running
// while paused, so a delay or backoff that elapses meanwhile is simply due on resume.
//
// This path is reached only when a worker died holding the instance — in the normal case
// the pause lands in SQL when the owning worker writes its finished task. That makes the
// reclaim check below load-bearing rather than defensive: the interrupted task may have
// already executed on the dead worker, and pausing here would launder that into a silent
// re-execution when the operator resumes.
func (e *Engine) settlePausing(inst *model.ProcessInstance, task *model.Task) advanceOutcome {
	if inst.ReclaimedExpired && task != nil && task.Action != nil && task.OnlyOnce != nil && *task.OnlyOnce {
		return e.failInstance(inst, errcode.EngineOnlyOnce, fmt.Sprintf(
			"task %q is only_once and was interrupted by a lease takeover; cannot re-execute", task.ID))
	}
	inst.Status = model.StatusPaused
	// The other half of inst_paused: PauseProcess logs the rows it settled directly, and
	// this covers the leased one it could only mark 'pausing'. Same event and level, so
	// the trail reads uniformly — every instance that becomes paused says so once.
	e.audit(inst, logEvent{Level: model.LogDebug, Event: model.EventPaused, Task: inst.Task,
		Msg: "in-flight task settled; instance paused"})
	return advanceOutcome{kind: outcomeTerminal}
}

// settleFailing finalises a draining 'failing' instance once its children have
// settled (it only becomes claimable then). The error was already recorded when
// the failure propagated up; saveAndNotify (via the terminal outcome) cascades the
// settlement one level up.
func (e *Engine) settleFailing(inst *model.ProcessInstance) advanceOutcome {
	inst.Status = model.StatusFailed
	inst.WaitState = model.WaitStateNone
	inst.WakeAt = nil
	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventInstanceSettled, Msg: inst.Error})
	return advanceOutcome{kind: outcomeTerminal}
}
