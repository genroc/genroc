package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"genroc/internal/db"
	"genroc/internal/delayspec"
	"genroc/internal/errcode"
	"genroc/internal/idgen"
	"genroc/internal/model"
	"genroc/internal/shape"
	"genroc/internal/template"
	"genroc/internal/transport"
)

// executeAction sends a request to the task's endpoint and returns (output, done):
//   - done=nil: action succeeded; output is the task result.
//   - done!=nil: the task loop should stop and persist this outcome (retry, error
//     route, or permanent fail).
func (e *Engine) executeAction(ctx context.Context, inst *model.ProcessInstance, task *model.Task) (any, *advanceOutcome) {
	timeout := time.Duration(task.TimeoutMs) * time.Millisecond
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	taskCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Resolve the request. The URL template can pull a base URL from config or input;
	// secret values it carries are scrubbed from the logged URL/errors in audit().
	url, err := e.resolveURL(inst, task.Action)
	if err != nil {
		return nil, stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q url: %v", task.ID, err)))
	}
	method, err := e.resolveMethod(inst, task.Action)
	if err != nil {
		return nil, stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q method: %v", task.ID, err)))
	}
	resolvedHeaders, err := e.resolveHeaders(inst, task.Action)
	if err != nil {
		return nil, stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q headers: %v", task.ID, err)))
	}
	// Stamp the caller's identity on every request (set last so it is authoritative and
	// a user-supplied header of the same name cannot spoof it).
	if resolvedHeaders == nil {
		resolvedHeaders = make(map[string]string, 2)
	}
	resolvedHeaders[transport.HeaderInstanceID] = inst.ID
	resolvedHeaders[transport.HeaderTaskID] = task.ID
	var body any
	if task.Action.Body.Present() {
		body, err = e.evalShape(inst, shape.Shape{Raw: task.Action.Body.Raw}, nil)
		if err != nil {
			return nil, stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q body: %v", task.ID, err)))
		}
	}
	acceptedStatus, err := e.resolveAcceptedStatus(inst, task.Action)
	if err != nil {
		return nil, stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q accepted_status: %v", task.ID, err)))
	}

	// action_started (debug): message = the action type; data = the request body; meta =
	// {url} so the trail shows which URL was hit. Headers are intentionally omitted — they
	// routinely carry secrets and the audit log is persisted.
	e.audit(inst, logEvent{Level: model.LogDebug, Event: model.EventActionStarted, Task: task.ID, Msg: string(task.Action.Type), Data: e.snippet(body), Meta: map[string]any{"url": url}})

	resp, err := transport.Send(taskCtx, task.Action, url, method, acceptedStatus, resolvedHeaders, body)
	if err != nil {
		code := transport.ClassifyGoError(err)
		// action_failed (debug) records the call failure — error detail in data,
		// code in code — separate from the operational retry/route event that follows.
		// A transport error has no HTTP status, so meta stays absent.
		e.audit(inst, logEvent{Level: model.LogDebug, Event: model.EventActionFailed, Task: task.ID, Code: code, Data: e.snippetRaw(err.Error())})
		return nil, stop(e.handleCallError(inst, task, err.Error(), code))
	}
	if resp.ErrorCode != "" {
		msg := resp.ErrorMessage
		if msg == "" {
			msg = string(resp.ErrorCode)
		}
		// action_failed (debug): error body in data, status in meta, code in code.
		e.audit(inst, logEvent{Level: model.LogDebug, Event: model.EventActionFailed, Task: task.ID, Code: resp.ErrorCode, Data: e.snippetRaw(resp.ErrorMessage), Meta: statusMeta(resp.Status)})
		return nil, stop(e.handleCallError(inst, task, msg, resp.ErrorCode))
	}

	// result_schema validates the raw result and normalizes it (undeclared keys
	// dropped, defaults filled); it does not export it. The result is transient —
	// available to this task's own output/switch as self.result. Only an `output`
	// projection adds anything to outputs.<id>.
	normalized, err := task.Action.ValidateOutput(resp.Body)
	if err != nil {
		return nil, stop(e.handleCallError(inst, task, err.Error(), errcode.OutputInvalid))
	}
	resp.Body = normalized
	inst.RetryCount = 0

	// action_succeeded (debug): the response body in data, the HTTP status in meta.
	// Like action_started it carries an action payload, so it is gated behind
	// --level debug rather than cluttering the default info trail.
	e.audit(inst, logEvent{Level: model.LogDebug, Event: model.EventActionSucceeded, Task: task.ID, Data: e.snippetResult(task, resp.Body), Meta: statusMeta(resp.Status)})

	return resp.Body, nil
}

func (e *Engine) buildTaskData(inst *model.ProcessInstance, task *model.Task) (any, error) {
	if !task.Action.Input.Present() {
		return map[string]any{}, nil
	}
	return e.evalShape(inst, shape.Shape{Raw: task.Action.Input.Raw}, nil)
}

// runDelay implements the delay action. First entry (WakeAt nil, reset on every task
// transition) evaluates the duration and parks by stamping wake_at (the progress outcome
// releases the worker); the claim loop re-claims once the timer elapses. Re-entry (WakeAt
// set, so the claim guarantees the timer is due) returns nil to continue to the switch.
// A non-nil outcome means it parked or failed (the caller stops and persists it).
func (e *Engine) runDelay(inst *model.ProcessInstance, task *model.Task) *advanceOutcome {
	if inst.WakeAt == nil {
		now := db.Now()
		wake, spec, err := e.resolveDelay(inst, task, now)
		if err != nil {
			return stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q delay: %v", task.ID, err)))
		}
		// A target in the past clamps rather than failing. Timers keep running while an
		// instance is paused, so an `until` can legitimately resolve behind now on resume —
		// the pause design requires that to be a no-op wait, not an error.
		msg := fmt.Sprintf("%s -> %s", spec, wake.Format(time.RFC3339))
		if wake.Before(now) {
			msg = fmt.Sprintf("%s -> %s (already past; waking now)", spec, wake.Format(time.RFC3339))
			wake = now
		}
		inst.WakeAt = &wake
		// Log the source spec alongside the resolved absolute instant: without both, a
		// calendar target is undebuggable after the fact.
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventDelayArmed, Task: task.ID, Msg: msg})
		return stop(advanceOutcome{kind: outcomeProgress})
	}
	return nil
}

// resolveDelay turns a delay task's `for` / `until` slot into the absolute instant to wake
// at, plus the source spec for the audit log. It is called once per task entry (runDelay
// guards on WakeAt), so a calendar target cannot drift when the instance is re-claimed.
func (e *Engine) resolveDelay(inst *model.ProcessInstance, task *model.Task, now time.Time) (time.Time, string, error) {
	a := task.Action
	loc, err := delayspec.LoadLocation(a.TZ)
	if err != nil {
		return time.Time{}, "", err
	}
	if err := delayArity(a); err != nil {
		return time.Time{}, "", err
	}
	switch {
	case a.For != nil:
		d, spec, err := e.resolveDuration(inst, a.For)
		if err != nil {
			return time.Time{}, "", fmt.Errorf("for: %w", err)
		}
		return d.Resolve(now, loc), spec, nil

	default:
		// `until`, since delayArity has established exactly one slot is set. A number (bare
		// or from an expression) is unix milliseconds; only a literal goes through the
		// instant grammar.
		if lit, ok := delayLiteral(a.Until); ok {
			target, err := delayspec.ParseInstant(lit)
			if err != nil {
				return time.Time{}, "", fmt.Errorf("until: %w", err)
			}
			at, err := target.Resolve(now, loc)
			if err != nil {
				return time.Time{}, "", fmt.Errorf("until: %w", err)
			}
			return at, target.Source(), nil
		}
		ms, err := e.delayNumber(inst, a.Until)
		if err != nil {
			return time.Time{}, "", fmt.Errorf("until: %w", err)
		}
		return time.UnixMilli(ms).In(loc), fmt.Sprintf("%d (unix ms)", ms), nil
	}
}

// delayArity rejects the two slot counts that are not exactly one. Both are unreachable by
// design — model.Validate rejects them at registration — but the decoder also runs over
// stored rows, which never re-validate, so neither may be resolved to a default here:
//
//   - Neither slot: a row predating these fields (one carrying only the removed `ms`)
//     decodes to this, since Action takes no DisallowUnknownFields. Treating it as zero is
//     the two-day-wait-becomes-no-wait failure this syntax exists to prevent.
//   - Both slots: preferring one silently is worse than failing. If `until` held the
//     far-future deadline, falling back to `for` waits a fraction of the intended time and
//     nothing anywhere reports it.
//
// Split from resolveDelay so both branches are testable without building an engine.
func delayArity(a *model.Action) error {
	switch {
	case a.For != nil && a.Until != nil:
		return fmt.Errorf("both `for` and `until` are set: exactly one is required")
	case a.For == nil && a.Until == nil:
		return fmt.Errorf("no delay set: exactly one of `for` or `until` is required")
	default:
		return nil
	}
}

// resolveDuration turns a `for` slot into a Duration: a literal parses against the grammar,
// anything else evaluates to a bare millisecond count.
func (e *Engine) resolveDuration(inst *model.ProcessInstance, raw any) (*delayspec.Duration, string, error) {
	if lit, ok := delayLiteral(raw); ok {
		d, err := delayspec.ParseDuration(lit)
		if err != nil {
			return nil, "", err
		}
		return d, d.Source(), nil
	}
	ms, err := e.delayNumber(inst, raw)
	if err != nil {
		return nil, "", err
	}
	if ms < 0 {
		return nil, "", fmt.Errorf("must be non-negative, got %d", ms)
	}
	return delayspec.Millis(ms), fmt.Sprintf("%dms", ms), nil
}

// delayLiteral reports whether raw is a pure literal string — the only form the delayspec
// grammars apply to. A "$:" leaf and a bare number both evaluate to a count instead.
// Classification mirrors checkDelaySlot in the validation package, which rejected the
// remaining form ("${ }") at registration.
func delayLiteral(raw any) (string, bool) {
	src, ok := raw.(string)
	if !ok {
		return "", false
	}
	tmpl, err := template.Parse(src)
	if err != nil {
		return "", false
	}
	return tmpl.Static()
}

// delayNumber evaluates a delay slot to a whole number: a "$:" expression against the
// instance context, or a bare JSON number passed through.
func (e *Engine) delayNumber(inst *model.ProcessInstance, raw any) (int64, error) {
	v := raw
	if src, ok := raw.(string); ok {
		var err error
		if v, err = e.evalShape(inst, shape.Shape{Raw: src}, nil); err != nil {
			return 0, err
		}
	}
	return delayMillis(v)
}

// runExternal implements the external (pull/callback) task, with three entry states
// told apart by wait_state and the presence of _external_result:
//
//  1. First arrival: snapshot the input, mint a per-occurrence token, and park
//     (wait_state='external'); timeout_ms>0 also stamps wake_at as the deadline. No worker
//     is held while parked, and the claim won't return it until the result arrives (which
//     clears wait_state) or the timeout fires.
//  2. Result submitted: the resolve API cleared wait_state and stored the validated
//     result; consume it as self.result and continue.
//  3. Timeout: the claim only returns a parked external once wake_at passed, so wait_state
//     still 'external' means no result arrived → external.timeout via on_error. A retry on
//     that code re-arms the wait with a fresh token.
//
// Returns (result, nil) to continue advancing, or (nil, outcome) to stop and persist.
func (e *Engine) runExternal(ctx context.Context, inst *model.ProcessInstance, task *model.Task) (any, *advanceOutcome) {
	// Phase 2: a result was submitted (the resolve API or a direct signal already un-parked
	// us by storing _external_result).
	if res, ok := inst.ContextData[model.CtxExternalResult]; ok {
		delete(inst.ContextData, model.CtxExternalResult)
		delete(inst.ContextData, model.CtxExternal)
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventExternalResolved, Task: task.ID})
		return res, nil
	}

	// Phase 3: still parked at 'external' — the claim only returns us once the timeout
	// deadline passed, so no result arrived in time.
	if inst.WaitState == model.WaitStateExternal {
		inst.WaitState = model.WaitStateNone
		delete(inst.ContextData, model.CtxExternal)
		e.audit(inst, logEvent{Level: model.LogWarn, Event: model.EventExternalTimeout, Task: task.ID, Msg: "external task timed out", Code: errcode.ExternalTimeout})
		return nil, stop(e.handleCallError(inst, task, "external task timed out", errcode.ExternalTimeout))
	}

	// Phase 1: first arrival. Atomically either consume a signal already buffered for this
	// task (the push/webhook case — it raced ahead of the process reaching the task) or
	// park and wait. RetryCount is intentionally left untouched so a re-arm after an
	// external.timeout retry keeps its counter and on_error budgeting terminates.
	input, err := e.buildTaskData(inst, task)
	if err != nil {
		return nil, stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q input: %v", task.ID, err)))
	}
	token := inst.ID + "." + idgen.New()
	var wakeAt *time.Time
	if task.TimeoutMs > 0 {
		wake := db.Now().Add(time.Duration(task.TimeoutMs) * time.Millisecond)
		wakeAt = &wake
	}
	consumed, result, err := e.db.ArmExternalOrConsumeSignal(ctx, inst, task.ID, token, input, wakeAt)
	if err != nil {
		return nil, stop(e.failInstance(inst, errcode.EngineSpawn, fmt.Sprintf("task %q arm: %v", task.ID, err)))
	}
	if consumed {
		// A buffered signal fed the task immediately. Continue advancing with it as the
		// result; ArmExternalOrConsumeSignal kept this worker's lease, so the normal
		// progress/terminal write at the end of advance releases it — the instance never
		// sits claimable while still in flight.
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventExternalResolved, Task: task.ID, Msg: "buffered"})
		return result, nil
	}
	// Parked. ArmExternalOrConsumeSignal persisted the parked state and released the lease,
	// so (like a child spawn) advance returns noop and writes nothing further.
	armedMsg := "token=" + token
	if task.TimeoutMs > 0 {
		armedMsg += fmt.Sprintf(" timeout=%dms", task.TimeoutMs)
	}
	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventExternalArmed, Task: task.ID, Msg: armedMsg})
	return nil, stop(advanceOutcome{kind: outcomeNoop})
}

// delayMillis coerces an evaluated delay value to a whole number of milliseconds. Note the
// absent string case: a bare numeric string ("30000") was the old `ms` spelling and is now
// a literal, routed to the delayspec grammar — which rejects it as unitless on purpose.
func delayMillis(v any) (int64, error) {
	switch n := v.(type) {
	case int:
		return int64(n), nil
	case int64:
		return n, nil
	case float64:
		return int64(n), nil
	case json.Number:
		parsed, err := n.Int64()
		if err != nil {
			return 0, fmt.Errorf("%v is not a whole number of milliseconds", n)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("must evaluate to a number, got %T", v)
	}
}

// resolveURL evaluates the fetch URL as a template so a base URL can come from config or
// input (e.g. "${ config.server_url }/path"). Returns "" for actions without a URL;
// secrets it carries are scrubbed from logged URLs/errors by audit().
func (e *Engine) resolveURL(inst *model.ProcessInstance, call *model.Action) (string, error) {
	if call.URL == "" {
		return "", nil
	}
	val, err := e.evalShape(inst, shape.Shape{Raw: call.URL}, nil)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%v", val), nil
}

// resolveMethod evaluates the fetch method expression, upper-cased, defaulting to POST.
func (e *Engine) resolveMethod(inst *model.ProcessInstance, call *model.Action) (string, error) {
	if call.Method == "" {
		return "POST", nil
	}
	val, err := e.evalShape(inst, shape.Shape{Raw: call.Method}, nil)
	if err != nil {
		return "", err
	}
	m := strings.ToUpper(strings.TrimSpace(fmt.Sprintf("%v", val)))
	if m == "" {
		return "POST", nil
	}
	return m, nil
}

// resolveHeaders evaluates the fetch Headers shape to a string map. The shape may be a
// literal map of templated values or a single expression yielding a map; either way it
// must resolve to an object, whose values are coerced to strings. Returns nil when the
// call has no headers.
func (e *Engine) resolveHeaders(inst *model.ProcessInstance, call *model.Action) (map[string]string, error) {
	if !call.Headers.Present() {
		return nil, nil
	}
	val, err := e.evalShape(inst, shape.Shape{Raw: call.Headers.Raw}, nil)
	if err != nil {
		return nil, err
	}
	m, ok := val.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("headers must evaluate to an object, got %T", val)
	}
	resolved := make(map[string]string, len(m))
	for k, v := range m {
		resolved[k] = fmt.Sprintf("%v", v)
	}
	return resolved, nil
}

// resolveAcceptedStatus evaluates the fetch AcceptedStatus shape to a list of status
// patterns. The shape may be a literal array of templated values or a single expression
// yielding an array; either way it must resolve to an array, whose elements are coerced to
// strings. Returns nil when the call sets no accepted_status (matchAcceptedStatus then
// defaults to any 2xx).
func (e *Engine) resolveAcceptedStatus(inst *model.ProcessInstance, call *model.Action) ([]string, error) {
	if !call.AcceptedStatus.Present() {
		return nil, nil
	}
	val, err := e.evalShape(inst, shape.Shape{Raw: call.AcceptedStatus.Raw}, nil)
	if err != nil {
		return nil, err
	}
	list, ok := val.([]any)
	if !ok {
		return nil, fmt.Errorf("accepted_status must evaluate to an array, got %T", val)
	}
	resolved := make([]string, len(list))
	for i, v := range list {
		resolved[i] = fmt.Sprintf("%v", v)
	}
	return resolved, nil
}
