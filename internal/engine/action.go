package engine

import (
	"context"
	"encoding/json"
	"fmt"
	neturl "net/url"
	"strings"
	"time"

	"genroc/internal/db"
	"genroc/internal/delayspec"
	"genroc/internal/errcode"
	"genroc/internal/model"
	"genroc/internal/shape"
	"genroc/internal/template"
	"genroc/internal/transport"
)

// defaultActionTimeout bounds a fetch that declares no timeout of its own. An external
// task has no equivalent default: parking indefinitely is what it is for.
const defaultActionTimeout = 30 * time.Second

// fetchMeta is exposed for the task as self.status and self.headers. Nil for every other
// action type, matching inference (see taskSelf).
type fetchMeta struct {
	status  int
	headers map[string]string
}

// executeAction returns the result and its response metadata, or a non-nil outcome (retry,
// error route, fail) the task loop must stop and persist.
func (e *Engine) executeAction(ctx context.Context, inst *model.ProcessInstance, task *model.Task) (any, *fetchMeta, *advanceOutcome) {
	// Resolved per attempt (a retry gets today's budget), then applied as a DURATION via
	// WithTimeout, never a WithDeadline instant: it was read off db.Now() while context
	// deadlines compare against real time.Now() — subtraction cancels the offset, an instant keeps it.
	now := db.Now()
	timeout, err := e.fetchTimeout(inst, task, now)
	if err != nil {
		return nil, nil, stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q timeout: %v", task.ID, err)))
	}

	taskCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url, err := e.resolveURL(inst, task.Action)
	if err != nil {
		return nil, nil, stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q url: %v", task.ID, err)))
	}
	method, err := e.resolveMethod(inst, task.Action)
	if err != nil {
		return nil, nil, stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q method: %v", task.ID, err)))
	}
	resolvedHeaders, err := e.resolveHeaders(inst, task.Action)
	if err != nil {
		return nil, nil, stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q headers: %v", task.ID, err)))
	}
	// Appended before the URL is logged, so the trail shows the request actually made.
	url, err = e.appendQuery(inst, task.Action, url)
	if err != nil {
		return nil, nil, stop(e.failInstance(inst, declaredFailureCode(err, errcode.EngineInput, errcode.EngineExpression),
			fmt.Sprintf("task %q query: %v", task.ID, err)))
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
		body, err = e.evalShape(inst, shape.Shape{Raw: task.Action.Body.Raw}, e.selfBeforeOutput(inst))
		if err == nil {
			// A fetch body always resolves: its reader is a remote server that cannot fetch an
			// object from genroc, so a reference reaching it is a value that never arrives.
			body, err = e.resolveRefsInPlace(inst, body)
		}
		// The declaration is applied AFTER the refs resolve: the conform has to see values,
		// and a marker it cannot look inside would read as a shape that does not fit.
		if err == nil {
			body, err = conformDeclared(body, task.Action.BodySchema, fmt.Sprintf("task %q body", task.ID))
		}
		if err != nil {
			return nil, nil, stop(e.failInstance(inst, declaredFailureCode(err, errcode.EngineInput, errcode.EngineExpression),
				fmt.Sprintf("task %q body: %v", task.ID, err)))
		}
	}
	resolvedStatus, err := e.resolveAcceptedStatus(inst, task.Action)
	acceptedStatus := task.Action.EffectiveAcceptedStatus(resolvedStatus)
	if err != nil {
		return nil, nil, stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q accepted_status: %v", task.ID, err)))
	}

	// Headers are deliberately not logged: they routinely carry secrets, and the trail persists.
	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventActionStarted, Task: task.ID, Msg: string(task.Action.Type), Data: e.snippet(body), Meta: map[string]any{"url": url}})

	resp, err := transport.Send(taskCtx, task.Action, url, method, acceptedStatus, resolvedHeaders, body)
	if err != nil {
		code := transport.ClassifyGoError(err)
		e.audit(inst, logEvent{Level: model.LogWarn, Event: model.EventActionFailed, Task: task.ID, Code: code, Data: e.snippetRaw(err.Error())})
		return nil, nil, stop(e.handleCallError(inst, task, err.Error(), code))
	}
	// A `null` entry ignores the body on both channels, so whatever it held cannot fail the call.
	if sc, declared := task.Action.ResponseFor(resp.Status); declared && sc == nil {
		resp.Body, resp.BodyCode = nil, ""
	}
	if resp.ErrorCode != "" {
		code, msg, extra := resp.ErrorCode, resp.ErrorMessage, map[string]any(nil)
		if msg == "" {
			msg = string(resp.ErrorCode)
		}
		// The declaration decides whether this body is readable, and a declared body that
		// cannot be read REPLACES the status code — the same enforcement the accepted path
		// applies, so `code: [http.4%]` stops catching a 400 whose body is malformed.
		if value, declared, verr := task.Action.ValidateResponse(resp.Status, resp.Body); declared {
			switch {
			case resp.BodyCode != "":
				code, msg = resp.BodyCode, fmt.Sprintf("status %d: %s", resp.Status, msg)
			case verr != nil:
				code, msg = errcode.ResultInvalid, fmt.Sprintf("status %d: %v", resp.Status, verr)
			default:
				extra = map[string]any{"data": value}
			}
		}
		e.audit(inst, logEvent{Level: model.LogWarn, Event: model.EventActionFailed, Task: task.ID, Code: code, Data: e.snippetRaw(resp.ErrorMessage), Meta: statusMeta(resp.Status)})
		return nil, nil, stop(e.handleCallErrorWith(inst, task, msg, code, extra))
	}

	// An undecodable body fails whatever schema was declared: the decode is JSON-only and an
	// empty body already came back as null, so nothing could have read this.
	if resp.BodyCode != "" {
		msg := resp.ErrorMessage
		if msg == "" {
			msg = string(resp.BodyCode)
		}
		e.audit(inst, logEvent{Level: model.LogWarn, Event: model.EventActionFailed, Task: task.ID, Code: resp.BodyCode, Data: e.snippetRaw(msg), Meta: statusMeta(resp.Status)})
		return nil, nil, stop(e.handleCallError(inst, task, msg, resp.BodyCode))
	}

	// Normalized, not exported: the result is transient (self.result); only an `output`
	// projection adds anything to outputs.<id>.
	normalized, _, err := task.Action.ValidateResponse(resp.Status, resp.Body)
	if err != nil {
		return nil, nil, stop(e.handleCallError(inst, task, err.Error(), errcode.ResultInvalid))
	}
	resp.Body = normalized
	inst.RetryCount = 0

	// Info, not debug: what a call got back IS the task's work; its size is the line clamp's
	// problem, not the level's.
	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventActionSucceeded, Task: task.ID, Data: e.snippet(resp.Body), Meta: statusMeta(resp.Status)})

	return resp.Body, &fetchMeta{status: resp.Status, headers: resp.Headers}, nil
}

func (e *Engine) buildTaskData(inst *model.ProcessInstance, task *model.Task) (any, error) {
	if !task.Action.Input.Present() {
		return conformDeclared(map[string]any{}, task.Action.InputSchema, fmt.Sprintf("task %q input", task.ID))
	}
	val, err := e.evalShape(inst, shape.Shape{Raw: task.Action.Input.Raw}, e.selfBeforeOutput(inst))
	if err != nil {
		return nil, err
	}
	return conformDeclared(val, task.Action.InputSchema, fmt.Sprintf("task %q input", task.ID))
}

// runDelay: first entry (WakeAt nil, reset per task transition) evaluates and parks by
// stamping wake_at; re-entry (WakeAt set ⇒ the claim guarantees the timer is due) returns
// nil to continue. A non-nil outcome parked or failed — caller stops and persists.
func (e *Engine) runDelay(inst *model.ProcessInstance, task *model.Task) *advanceOutcome {
	if inst.WakeAt == nil {
		now := db.Now()
		wake, spec, err := e.resolveDelay(inst, task, now)
		if err != nil {
			return stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q delay: %v", task.ID, err)))
		}
		// A past target clamps, never fails: timers run while paused, so an `until` can
		// legitimately resolve behind now on resume.
		msg := fmt.Sprintf("%s -> %s", spec, wake.Format(time.RFC3339))
		if wake.Before(now) {
			msg = fmt.Sprintf("%s -> %s (already past; waking now)", spec, wake.Format(time.RFC3339))
			wake = now
		}
		inst.WakeAt = &wake
		// Both the spec and the instant: a calendar target is undebuggable from either alone.
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventDelayArmed, Task: task.ID, Msg: msg})
		return stop(advanceOutcome{kind: outcomeProgress})
	}
	return nil
}

// resolveDelay must run once per task entry (runDelay guards on WakeAt), so a calendar
// target cannot drift when the instance is re-claimed.
func (e *Engine) resolveDelay(inst *model.ProcessInstance, task *model.Task, now time.Time) (time.Time, string, error) {
	return e.resolveSpec(inst, task.Action.DelaySpec, now)
}

// resolveSpec is shared by delay and timeout. It returns a past instant untouched: each
// caller decides what that means.
func (e *Engine) resolveSpec(inst *model.ProcessInstance, spec model.DelaySpec, now time.Time) (time.Time, string, error) {
	loc, err := delayspec.LoadLocation(spec.TZ)
	if err != nil {
		return time.Time{}, "", err
	}
	if err := delayArity(spec); err != nil {
		return time.Time{}, "", err
	}
	switch {
	case spec.For != nil:
		d, src, err := e.resolveDuration(inst, spec.For)
		if err != nil {
			return time.Time{}, "", fmt.Errorf("for: %w", err)
		}
		return d.Resolve(now, loc), src, nil

	default:
		// `until`: a number (bare or from an expression) is unix ms; only a literal goes
		// through the instant grammar.
		if lit, ok := delayLiteral(spec.Until); ok {
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
		ms, err := e.delayNumber(inst, spec.Until)
		if err != nil {
			return time.Time{}, "", fmt.Errorf("until: %w", err)
		}
		return time.UnixMilli(ms).In(loc), fmt.Sprintf("%d (unix ms)", ms), nil
	}
}

// resolveTimeout returns the instant an attempt must finish by; ok=false means absence,
// not zero — the caller supplies its own default. A deadline already past is returned
// as-is: the two callers answer it oppositely (fetchTimeout refuses, runExternal clamps).
func (e *Engine) resolveTimeout(inst *model.ProcessInstance, task *model.Task, now time.Time) (time.Time, string, bool, error) {
	if task.Action == nil || task.Action.Timeout.IsZero() {
		return time.Time{}, "", false, nil
	}
	at, src, err := e.resolveSpec(inst, task.Action.Timeout.DelaySpec, now)
	if err != nil {
		return time.Time{}, "", false, err
	}
	return at, src, true, nil
}

// fetchTimeout is one fetch attempt's budget. A past deadline is REFUSED, never clamped:
// a pre-expired context classifies as http.timeout — unknowable, so unretryable forever
// on only_once, for a request that never left. (external clamps; its code is truthful.)
func (e *Engine) fetchTimeout(inst *model.ProcessInstance, task *model.Task, now time.Time) (time.Duration, error) {
	at, src, ok, err := e.resolveTimeout(inst, task, now)
	if err != nil {
		return 0, err
	}
	if !ok {
		return defaultActionTimeout, nil
	}
	if !at.After(now) {
		return 0, fmt.Errorf("%s resolves to %s, which is not in the future — a request would time out before it was sent", src, at.Format(time.RFC3339))
	}
	return at.Sub(now), nil
}

// delayArity rejects any slot count but one — at decode time too, over stored rows that
// never re-validate: a row carrying only the removed `ms` decodes to NO slot and would
// wait zero. Timeout guards its own absence first. specs/delay-syntax.md.
func delayArity(spec model.DelaySpec) error {
	switch {
	case spec.For != nil && spec.Until != nil:
		return fmt.Errorf("both `for` and `until` are set: exactly one is required")
	case spec.For == nil && spec.Until == nil:
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

// delayLiteral reports whether raw is a pure literal, the only form the grammars parse. Must
// classify as validation's checkDelaySlot does, which rejected "${ }" at registration.
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

func (e *Engine) delayNumber(inst *model.ProcessInstance, raw any) (int64, error) {
	v := raw
	if src, ok := raw.(string); ok {
		var err error
		if v, err = e.evalShape(inst, shape.Shape{Raw: src}, e.selfBeforeOutput(inst)); err != nil {
			return 0, err
		}
	}
	return delayMillis(v)
}

// runExternal returns (result, nil) to continue or (nil, outcome) to stop and persist.
func (e *Engine) runExternal(ctx context.Context, inst *model.ProcessInstance, task *model.Task) (any, *advanceOutcome) {
	// Phase 2: a buffered answer, the ONE way an outcome arrives. Peek, never pop: persist
	// deletes it with the state it produced. specs/external-outcome-as-signal.md.
	sig, err := retryRead(func() (bufferedSignal, error) {
		id, o, ok, err := e.db.PeekSignal(inst.ID, task.ID)
		return bufferedSignal{id: id, outcome: o, ok: ok}, err
	})
	sigID, outcome, buffered := sig.id, sig.outcome, sig.ok
	if err != nil {
		return nil, stop(e.failInstance(inst, errcode.EngineSpawn, fmt.Sprintf("task %q: reading the buffered answer: %v", task.ID, err)))
	}
	if buffered {
		inst.ConsumedSignalID = sigID
		clearExternalPark(inst)
		inst.Phase = model.PhaseNone
		if f := outcome.Failure; f != nil {
			// Routed here, not at submission: retry_count/wake_at are writes on the leased row,
			// which the fail API does not hold.
			var extra map[string]any
			// Key PRESENCE is the signal: the fail API drops `data` for an undeclared code, so
			// error.data stays absent rather than null.
			if f.HasData {
				extra = map[string]any{"data": f.Data}
			}
			e.audit(inst, logEvent{Level: model.LogWarn, Event: model.EventExternalFailed, Task: task.ID, Msg: f.Message, Code: errcode.Code(f.Code)})
			return nil, stop(e.handleCallErrorWith(inst, task, f.Message, errcode.Code(f.Code), extra))
		}
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventExternalResolved, Task: task.ID})
		return outcome.Result, nil
	}

	// Phase 3: still parked with no answer — a lapsed only_once claim or a passed deadline.
	// Separate codes so an on_error rule can tell the two apart.
	if inst.Phase == model.PhaseExternal {
		code, msg, event := errcode.ExternalTimeout, "external task timed out", model.EventExternalTimeout
		if inst.ExternalLost {
			code, msg, event = errcode.ExternalLost, "the worker holding this task did not answer before its claim expired", model.EventExternalLost
		}
		inst.Phase = model.PhaseNone
		clearExternalPark(inst)
		e.audit(inst, logEvent{Level: model.LogWarn, Event: event, Task: task.ID, Msg: msg, Code: code})
		return nil, stop(e.handleCallError(inst, task, msg, code))
	}

	// Phase 1: first arrival. RetryCount is left untouched so a re-arm after an
	// external.timeout retry keeps its counter and the on_error budget terminates.
	input, err := e.buildTaskData(inst, task)
	if err != nil {
		return nil, stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q input: %v", task.ID, err)))
	}
	// Derived from TaskEpoch, never stored. Not a secret: the queue hands it to any caller.
	token := model.ExternalToken(inst.ID, inst.TaskEpoch)
	// Resolved per arming: a re-arm keeps an `until` instant but restarts a `for` budget.
	// wake_at is a DB timestamp, so unlike fetch there is no clock offset to cancel.
	armedAt := db.Now()
	deadline, spec, hasDeadline, err := e.resolveTimeout(inst, task, armedAt)
	if err != nil {
		return nil, stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q timeout: %v", task.ID, err)))
	}
	var wakeAt *time.Time
	if hasDeadline {
		// A past deadline clamps (parks already due, then raises external.timeout); failing would
		// be an uncatchable engine.expression for a legitimate state.
		if deadline.Before(armedAt) {
			spec = fmt.Sprintf("%s (already past; due now)", spec)
			deadline = armedAt
		}
		wakeAt = &deadline
	}
	armedMsg := "token=" + token
	if hasDeadline {
		// Both spec and instant, as delay_armed logs.
		armedMsg += fmt.Sprintf(" timeout=%s -> %s", spec, deadline.Format(time.RFC3339))
	}
	// Park-or-not is the database's call under the row lock, which is what makes a signal
	// racing the arm impossible to lose. A buffered signal returns through phase 2.
	return nil, stop(advanceOutcome{kind: outcomeArm, arm: &externalArm{
		taskID:   task.ID,
		input:    input,
		wakeAt:   wakeAt,
		armedMsg: armedMsg,
	}})
}

// clearExternalPark drops both halves of the park together. The marker is a column of its own
// now, so leaving it set would make the NEXT arming of this task report external.lost.
func clearExternalPark(inst *model.ProcessInstance) {
	delete(inst.State, model.StateExternalInput)
	inst.ExternalLost = false
}

// externalArm is the wait persist installs unless a signal is already buffered. armedMsg is
// built here because only advance holds the resolved timeout spec.
type externalArm struct {
	taskID   string
	input    any
	wakeAt   *time.Time
	armedMsg string
}

// delayMillis has no string case on purpose: a bare "30000" is a literal, which the
// delayspec grammar rejects as unitless.
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

// resolveURL returns "" for an action without a URL.
func (e *Engine) resolveURL(inst *model.ProcessInstance, call *model.Action) (string, error) {
	if call.URL == "" {
		return "", nil
	}
	val, err := e.evalShape(inst, shape.Shape{Raw: call.URL}, e.selfBeforeOutput(inst))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%v", val), nil
}

// resolveMethod upper-cases because the wire is case-sensitive. Empty is an error, not a
// fallback: net/http reads it as GET, the verb-guessing a required `method` prevents.
func (e *Engine) resolveMethod(inst *model.ProcessInstance, call *model.Action) (string, error) {
	val, err := e.evalShape(inst, shape.Shape{Raw: call.Method}, e.selfBeforeOutput(inst))
	if err != nil {
		return "", err
	}
	m := strings.ToUpper(strings.TrimSpace(fmt.Sprintf("%v", val)))
	if m == "" {
		return "", fmt.Errorf("resolved to empty — name the verb, e.g. %s", "method: post")
	}
	return m, nil
}

func (e *Engine) resolveHeaders(inst *model.ProcessInstance, call *model.Action) (map[string]string, error) {
	if !call.Headers.Present() {
		return nil, nil
	}
	val, err := e.evalShape(inst, shape.Shape{Raw: call.Headers.Raw}, e.selfBeforeOutput(inst))
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

// appendQuery: keep url.Values.Encode (or its key sort) -- it keeps the url byte-identical
// across attempts. specs/fetch-http-surface.md §1.
func (e *Engine) appendQuery(inst *model.ProcessInstance, call *model.Action, rawURL string) (string, error) {
	if !call.Query.Present() {
		return rawURL, nil
	}
	val, err := e.evalShape(inst, shape.Shape{Raw: call.Query.Raw}, e.selfBeforeOutput(inst))
	if err != nil {
		return "", err
	}
	// Redundant with the null-omit below, deliberately: it makes the declaration describe
	// what is sent rather than a claim beside it.
	if val, err = conformDeclared(val, call.QuerySchema, "query"); err != nil {
		return "", err
	}
	m, ok := val.(map[string]any)
	if !ok {
		return "", fmt.Errorf("query must evaluate to an object, got %T", val)
	}
	values := neturl.Values{}
	for k := range m {
		switch v := m[k].(type) {
		case nil:
			// a null omits its parameter
		case []any:
			// `?tag=a&tag=b`: OpenAPI's default (form/explode).
			for _, item := range v {
				if item == nil {
					continue // the same omission, one level down
				}
				values.Add(k, fmt.Sprintf("%v", item))
			}
		default:
			values.Set(k, fmt.Sprintf("%v", v))
		}
	}
	if len(values) == 0 {
		return rawURL, nil
	}
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	// Encode renders a space as `+`, a literal plus to an RFC 3986 reader. %20 is a space under
	// both, and the swap is exact: a literal plus is already `%2B`.
	return rawURL + sep + strings.ReplaceAll(values.Encode(), "+", "%20"), nil
}

// resolveAcceptedStatus returns nil when unset; matchAcceptedStatus then means any 2xx.
func (e *Engine) resolveAcceptedStatus(inst *model.ProcessInstance, call *model.Action) ([]string, error) {
	if !call.AcceptedStatus.Present() {
		return nil, nil
	}
	val, err := e.evalShape(inst, shape.Shape{Raw: call.AcceptedStatus.Raw}, e.selfBeforeOutput(inst))
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
