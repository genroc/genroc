package validation

import (
	"encoding/json"
	"fmt"
	"slices"

	"genroc/internal/delayspec"
	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/shape"
	"genroc/internal/template"
)

func buildInputs(tasks []*model.Task, taskSchemas map[string]TaskSchemas, processInput, configSchema schema.Schema, defs schema.Defs, rd *raiseData) error {
	if err := checkReachability(tasks); err != nil {
		return err
	}
	required, optional, mustErr, mayErr, errSrc := computeContextSets(tasks)
	errs := errContexts(tasks, mustErr, mayErr, errSrc, defs)
	scopes := taskScopes{
		tasks: taskSchemas, processInput: processInput, configSchema: configSchema, defs: defs,
		required: required, optional: optional, errs: errs,
	}

	// Phase 1: infer every output-map task's exported type, in dependency order
	// (mutually-recursive tasks resolved jointly), writing each to defs so the
	// switches and later tasks below see the final types.
	if err := inferOutputs(tasks, scopes); err != nil {
		return err
	}

	// Phase 2: action inputs and switch type-checks.
	for _, s := range tasks {
		loops := taskLoops(s, required, optional)
		// Ahead of the per-slot checks, so a member that does not exist here is reported as
		// the rule it breaks rather than as the schema's "field not found".
		if err := checkPreOutputScopes(s, loops); err != nil {
			return err
		}
		if s.Action != nil {
			ts, inMap := taskSchemas[s.ID]
			isFetch := s.Action.Type == model.ActionTypeFetch
			hasURL := isFetch && s.Action.URL != ""
			hasMethod := isFetch && s.Action.Method != ""
			hasHeaders := isFetch && s.Action.Headers.Present()
			hasQuery := isFetch && s.Action.Query.Present()
			hasAcceptedStatus := isFetch && s.Action.AcceptedStatus.Present()
			hasBody := s.Action.Body.Present()
			hasInput := s.Action.Input.Present()
			hasOver := s.Action.Type == model.ActionTypeChildList && s.Action.Over != ""
			isDelay := s.Action.Type == model.ActionTypeDelay
			hasFor := isDelay && s.Action.For != nil
			hasUntil := isDelay && s.Action.Until != nil
			hasTimeout := !s.Timeout.IsZero()
			if inMap || hasBody || hasInput || hasURL || hasMethod || hasHeaders || hasQuery || hasAcceptedStatus || hasOver || hasFor || hasUntil || hasTimeout {
				ctx := scopes.action(s)
				// The child_list `over` expression must be a non-null array; each
				// element becomes one child's input. Type-check it here so a malformed or
				// non-array expression is rejected at registration.
				if hasOver {
					if _, err := checkArrayTemplate(s.Action.Over, ctx, s.ID); err != nil {
						return err
					}
				}
				// A delay `for` / `until` is classified syntactically: a literal is parsed
				// against the delayspec grammar here, a $: expression is type-checked to a
				// number, and a ${ } interpolation is rejected — so a malformed duration or
				// instant fails at registration rather than when the task is reached.
				if hasFor {
					if err := checkDelaySlot(s.Action.For, ctx, s.ID, "delay", "for"); err != nil {
						return err
					}
				}
				if hasUntil {
					if err := checkDelaySlot(s.Action.Until, ctx, s.ID, "delay", "until"); err != nil {
						return err
					}
				}
				// A timeout is the same two slots pointed at a deadline, so it is checked the
				// same way — a literal against the grammar, a $: expression to a number.
				if hasTimeout {
					if err := checkTimeout(&s.Timeout, ctx, s.ID); err != nil {
						return err
					}
				}
				// The fetch url and method are templates evaluated against the context;
				// type-check them and reject a possibly-null result (a null URL or method
				// would silently stringify to "null").
				if hasURL {
					if err := checkNonNullTemplate(s.Action.URL, ctx, fmt.Sprintf("task %q url", s.ID)); err != nil {
						return err
					}
				}
				if hasMethod {
					if err := checkNonNullTemplate(s.Action.Method, ctx, fmt.Sprintf("task %q method", s.ID)); err != nil {
						return err
					}
				}
				// Headers is a shape that must evaluate to a non-null object.
				if hasHeaders {
					if err := checkHeadersShape(s.Action.Headers.Raw, ctx, s.ID); err != nil {
						return err
					}
				}
				if hasQuery {
					if err := checkQueryShape(s.Action.Query.Raw, ctx, s.ID); err != nil {
						return err
					}
				}
				// accepted_status is a shape that must evaluate to an array of strings.
				// The per-pattern format ("2xx"/"404") is not checked — an expression's
				// elements aren't known statically, and an unrecognized pattern simply
				// never matches at runtime.
				if hasAcceptedStatus {
					if err := checkAcceptedStatusShape(s.Action.AcceptedStatus.Raw, ctx, s.ID); err != nil {
						return err
					}
				}
				if inMap || hasBody || hasInput {
					input, err := inferActionPayload(s, ctx)
					if err != nil {
						return err
					}
					if !inMap {
						ts.ActionType = s.Action.Type
					}
					ts.Input = input
					taskSchemas[s.ID] = ts
				}
			}
		}

		if len(s.Switch) > 0 {
			switchCtx, err := scopes.switchScope(s)
			if err != nil {
				return fmt.Errorf("task %q: %w", s.ID, err)
			}
			// An untyped action result cannot be read in a case any more than it can be
			// exported through an output, so a case that touches self.result gets the same
			// actionable message the output slot gives.
			untypedResult := false
			if s.Action != nil {
				if _, typed, err := actionResultType(s, defs); err != nil {
					return fmt.Errorf("task %q: %w", s.ID, err)
				} else {
					untypedResult = !typed
				}
			}
			for _, c := range s.Switch {
				if c.Case == "" {
					continue
				}
				// A case is an expression-only shape: a bare boolean expression, checked
				// through the same object API so it shares the roots machinery.
				hooks := shape.CheckHooks{
					Result: func(inferred, _ schema.Schema) error {
						return fmt.Errorf("task %q switch case %q: expression must evaluate to boolean, got %q", s.ID, c.Case, inferred.TypeName())
					},
				}
				label := fmt.Sprintf("task %q switch case %q", s.ID, c.Case)
				hooks.Roots = slotRoots(s, label, loops, !untypedResult, afterOutput)
				shp := shape.Shape{Raw: c.Case, Schema: &boolSchema, Name: fmt.Sprintf("task %q switch case %q", s.ID, c.Case), Expr: true}
				if _, err := shp.CheckWith(switchCtx, hooks); err != nil {
					return err
				}
			}
			// A message is a template rendered when the clause fires, so it is checked in
			// that clause's own scope — a switch case sees `self`, which is why this runs
			// here rather than beside the code's shape rule in model.
			for i := range s.Switch {
				where := fmt.Sprintf("switch case %d", i)
				if err := checkFaultClauses(s.Switch[i].Raise, s.Switch[i].Panic, switchCtx, s.ID, where, rd); err != nil {
					return err
				}
			}
		}

		// An on_error rule sees the error it CAUGHT, not the one that reaches a task it
		// routes to — so its context is built per rule rather than from errs[s.ID].
		for i, ec := range s.OnError {
			if ec.Raise == nil && ec.Panic == nil && ec.Case == "" && ec.Retry.IsZero() {
				continue
			}
			// The task's own context — `last_error` and all — plus `error`, the failure THIS
			// rule caught. Both are readable here and they are different errors.
			ruleCtx := scopes.rule(s, ec)
			where := fmt.Sprintf("on_error[%d]", i)
			// The case is checked in the SAME per-rule scope as the clauses: `code` has
			// already said which error this is, so `error.data` here is that code's declared
			// shape rather than the union a routed task sees. `self` is previous-only: the
			// task failed, so it has no result. specs/child-error-handling.md M2.
			if ec.Case != "" {
				hooks := shape.CheckHooks{
					Result: func(inferred, _ schema.Schema) error {
						return fmt.Errorf("task %q %s case %q: expression must evaluate to boolean, got %q", s.ID, where, ec.Case, inferred.TypeName())
					},
				}
				shp := shape.Shape{Raw: ec.Case, Schema: &boolSchema, Name: fmt.Sprintf("task %q %s case %q", s.ID, where, ec.Case), Expr: true}
				if _, err := shp.CheckWith(ruleCtx, hooks); err != nil {
					return err
				}
			}
			if err := checkFaultClauses(ec.Raise, ec.Panic, ruleCtx, s.ID, where, rd); err != nil {
				return err
			}
			// A retry policy's slots are the same syntactic split as a delay's: a literal was
			// checked by the decoder, a $: expression is type-checked here — in the rule's own
			// scope, like the case above it.
			if err := checkRetrySlots(s.ID, i, ec, ruleCtx); err != nil {
				return err
			}
		}
	}
	return nil
}

// Per-slot required structures for fetch slots: stringifiable scalar for url/method
// (rendered with %v — null or a struct corrupts the request), array for `over`, object
// for headers. shape.CheckWith turns mismatches into each slot's tailored message.
var (
	scalarSchema = schema.Type("string", "number", "boolean")
	arraySchema  = schema.Array(schema.Schema{})
	// Headers must be a non-null object whose values are all strings (HTTP header values).
	headersSchema = schema.Map(schema.Type("string"))
	// accepted_status must be an array whose elements are all strings (HTTP status patterns).
	acceptedStatusSchema = schema.Array(schema.Type("string"))
	// query must be a non-null object of scalars; a null VALUE is fine and omits its
	// parameter, which is the whole ergonomic point of the slot.
	querySchema = schema.Map(schema.AnyOf(
		schema.Type("string", "number", "boolean", "null"),
		schema.Array(schema.Type("string", "number", "boolean", "null")),
	))
	boolSchema = schema.Type("boolean")
	// A raise/panic message is rendered into inst.Error and the audit log, both of which
	// are text — so unlike url/method it does not take a bare number or boolean. An
	// interpolation stringifies and always satisfies this; a `$:` leaf must already be one.
	messageSchema = schema.Type("string")
	// A delay `for` / `until` expression must be a number: milliseconds for `for`, unix
	// milliseconds for `until`. The literal grammars never reach the type system — they are
	// parsed by delayspec at registration — so unlike the old ms slot, string is not
	// accepted here.
	delaySchema = schema.Type("number")
)

// checkNonNullTemplate type-checks a fetch url/method against ctx: it must produce a
// non-null scalar, or the %v-rendered request would carry "null" or "[a b c]".
func checkNonNullTemplate(expr string, ctx schema.Schema, label string) error {
	shp := shape.Shape{Raw: expr, Schema: &scalarSchema, Name: label}
	_, err := shp.CheckWith(ctx, shape.CheckHooks{
		Result: func(inferred, _ schema.Schema) error {
			if inferred.HasNull() {
				return fmt.Errorf("%s may be null; use ?? to provide a default value", label)
			}
			return fmt.Errorf("%s is %s; it must be a string, number or boolean", label, inferred.TypeName())
		},
	})
	return err
}

// checkMessageTemplate type-checks one raise/panic message against the scope its clause
// fires in. It must be a non-null string: the value lands in inst.Error, the audit log and
// — via collect — a parent's `error.message`, none of which can hold anything else.
func checkMessageTemplate(expr string, ctx schema.Schema, label string) error {
	shp := shape.Shape{Raw: expr, Schema: &messageSchema, Name: label}
	_, err := shp.CheckWith(ctx, shape.CheckHooks{
		Result: func(inferred, _ schema.Schema) error {
			if inferred.HasNull() {
				return fmt.Errorf("%s may be null; use ?? to provide a default", label)
			}
			return fmt.Errorf("%s is %s; a message must be a string", label, inferred.TypeName())
		},
	})
	return err
}

// checkFaultClauses checks whichever of a clause's raise/panic is set: the message renders
// to a non-null string, and `data` type-checks against the same scope — any type will do
// there, since a caller declares the shape it expects. Ordered rather than ranged over a
// map so a definition with both reports the same one every run.
//
// A `raise` also RECORDS what its data inferred to. This is the only place the scope a clause
// fires in is still in hand, and rd carries the answer out to the caller's check; a panic
// records nothing, for the reason ProcessDefinition.Raises excludes its code.
func checkFaultClauses(raise, panics *model.Fault, ctx schema.Schema, taskID, where string, rd *raiseData) error {
	for _, c := range []struct {
		name     string
		fault    *model.Fault
		raisable bool
	}{{"raise", raise, true}, {"panic", panics, false}} {
		if c.fault == nil {
			continue
		}
		label := fmt.Sprintf("task %q %s %s", taskID, where, c.name)
		if err := checkMessageTemplate(c.fault.Message, ctx, label+" message"); err != nil {
			return err
		}
		if !c.fault.Data.Present() {
			if c.raisable {
				rd.absent(c.fault.Code)
			}
			continue
		}
		data := *c.fault.Data
		data.Name = label + " data"
		inferred, err := data.Check(ctx)
		if err != nil {
			return err
		}
		if c.raisable {
			rd.add(c.fault.Code, inferred)
		}
	}
	return nil
}

// raiseData accumulates the payload type of every raise clause, keyed by code. Two clauses
// raising one code make it a union: either may fire, so a caller has to accept both.
type raiseData struct {
	arms     map[string][]schema.Schema
	nullable map[string]bool
}

func newRaiseData() *raiseData {
	return &raiseData{arms: map[string][]schema.Schema{}, nullable: map[string]bool{}}
}

func (r *raiseData) add(code string, t schema.Schema) { r.arms[code] = append(r.arms[code], t) }

// absent records a clause that attached nothing. Not a no-op and not a zero schema: the slot
// is CLEARED, so the payload a caller conforms is null — which a declared object shape does
// not admit, and that refusal is the point.
func (r *raiseData) absent(code string) { r.nullable[code] = true }

// types collapses each code's arms into the one type its payload can carry. Identical arms
// collapse for ErrorDataSchema's reason: a union no arm is alone in says nothing extra.
func (r *raiseData) types() map[string]schema.Schema {
	out := make(map[string]schema.Schema, len(r.arms)+len(r.nullable))
	for code, arms := range r.arms {
		out[code] = combineErrData(dedupeSchemas(arms), r.nullable[code])
	}
	for code := range r.nullable {
		if _, ok := out[code]; !ok {
			out[code] = schema.Type("null")
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func dedupeSchemas(arms []schema.Schema) []schema.Schema {
	if len(arms) < 2 {
		return arms
	}
	out := make([]schema.Schema, 0, len(arms))
	seen := map[string]bool{}
	for _, a := range arms {
		key, err := json.Marshal(a)
		if err != nil {
			out = append(out, a)
			continue
		}
		if seen[string(key)] {
			continue
		}
		seen[string(key)] = true
		out = append(out, a)
	}
	return out
}

// checkArrayTemplate type-checks a child_list `over` against ctx: it must produce a
// non-null array, the source of the per-child inputs.
func checkArrayTemplate(expr string, ctx schema.Schema, taskID string) (schema.Schema, error) {
	shp := shape.Shape{Raw: expr, Schema: &arraySchema, Name: fmt.Sprintf("task %q over", taskID)}
	return shp.CheckWith(ctx, shape.CheckHooks{
		Result: func(inferred, _ schema.Schema) error {
			if inferred.HasNull() {
				return fmt.Errorf("task %q over may be null; use ?? to provide a default array", taskID)
			}
			return fmt.Errorf("task %q over must evaluate to an array, got %q", taskID, inferred.TypeName())
		},
	})
}

// checkDelaySlot type-checks a delay for/until: a bare number is ms (nothing to check), a
// pure literal parses the delayspec grammar, "$:" must infer to number, and ${ }
// interpolation is rejected BY NAME — it produces a string, the failure this syntax
// removes. where names the construct so the message points at the author's line.
func checkDelaySlot(raw any, ctx schema.Schema, taskID, where, slot string) error {
	label := fmt.Sprintf("task %q %s %s", taskID, where, slot)

	// A bare number is milliseconds (for) or unix milliseconds (until): no parse, no check.
	switch raw.(type) {
	case float64, int, int64, json.Number:
		return nil
	}
	src, ok := raw.(string)
	if !ok {
		return fmt.Errorf("%s must be a string or a number, got %T", label, raw)
	}

	tmpl, err := template.Parse(src)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if lit, isLit := tmpl.Static(); isLit {
		return checkDelayLiteral(lit, label, slot)
	}
	if !tmpl.IsExpr() {
		return fmt.Errorf("%s: a ${ } interpolation is not allowed here — it would produce a string at runtime. "+
			"Write a literal (e.g. %s) or a whole-value $: expression evaluating to a number (e.g. %q)",
			label, delaySlotExample(slot), "$: input.wait_ms")
	}
	// Not an Expr shape: those carry a bare expression body (as a switch case does), while
	// src still has its "$:" marker. A plain shape routes it through template.Parse, whose
	// $: leaf is type-preserving — so the inferred type is the expression's own.
	shp := shape.Shape{Raw: src, Schema: &delaySchema, Name: label}
	_, err = shp.CheckWith(ctx, shape.CheckHooks{
		Result: func(inferred, _ schema.Schema) error {
			unit := "a number of milliseconds"
			if slot == "until" {
				unit = "a number of unix milliseconds"
			}
			return fmt.Errorf("%s must evaluate to %s, got %q", label, unit, inferred.TypeName())
		},
	})
	return err
}

// checkTimeout: same slots and arity as delay. Positivity is deliberately not checked —
// "0s" parses, and the resolved instant is judged by the engine against its real clock;
// an `until` judged at registration would validate differently on different days.
func checkTimeout(t *model.Timeout, ctx schema.Schema, taskID string) error {
	if t.For != nil && t.Until != nil {
		return fmt.Errorf("task %q timeout: for and until are mutually exclusive — %q is a budget from when the task is reached, %q is a fixed deadline", taskID, "for", "until")
	}
	if t.For == nil && t.Until == nil {
		return fmt.Errorf("task %q timeout: one of for or until is required (write %s for a plain duration)", taskID, `timeout: "30s"`)
	}
	if t.TZ != "" {
		if _, err := delayspec.LoadLocation(t.TZ); err != nil {
			return fmt.Errorf("task %q timeout: %v", taskID, err)
		}
	}
	if t.For != nil {
		return checkDelaySlot(t.For, ctx, taskID, "timeout", "for")
	}
	return checkDelaySlot(t.Until, ctx, taskID, "timeout", "until")
}

// checkRetrySlots type-checks the $: slots of every on_error retry policy. The engine
// reduces the whole policy to numbers when the rule fires, so every slot must infer to one;
// the bounds (attempts whole and non-negative, factor >= 1, max_delay >= delay) can only be
// judged then, and Retry.Resolve judges them.
func checkRetrySlots(taskID string, i int, ec model.ErrorCase, ctx schema.Schema) error {
	slots := []struct{ name, expr string }{
		{"attempts", ec.Retry.Attempts.Expr()},
		{"delay", ec.Retry.Delay.Expr()},
		{"factor", ec.Retry.Factor.Expr()},
		{"max_delay", ec.Retry.MaxDelay.Expr()},
	}
	for _, slot := range slots {
		if slot.expr == "" {
			continue
		}
		label := fmt.Sprintf("task %q on_error[%d] retry.%s", taskID, i, slot.name)
		shp := shape.Shape{Raw: slot.expr, Schema: &delaySchema, Name: label}
		_, err := shp.CheckWith(ctx, shape.CheckHooks{
			Result: func(inferred, _ schema.Schema) error {
				return fmt.Errorf("%s must evaluate to a number, got %q", label, inferred.TypeName())
			},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// checkDelayLiteral parses a pure literal at registration, so a typo fails when the
// definition is applied rather than three days into a run. Parsing alone is the whole
// check: it is clock-independent, so the same definition always validates the same way.
func checkDelayLiteral(lit, label, slot string) error {
	var err error
	if slot == "for" {
		_, err = delayspec.ParseDuration(lit)
	} else {
		_, err = delayspec.ParseInstant(lit)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

func delaySlotExample(slot string) string {
	if slot == "until" {
		return `"+2d 08:00"`
	}
	return `"2h30m"`
}

// inferActionPayload infers the schema of an action's payload shape — the fetch request
// body (Body) or the external snapshot (Input). Free projection: no required structure.
func inferActionPayload(s *model.Task, ctx schema.Schema) (schema.Schema, error) {
	sh := s.Action.Input
	label := "input"
	if s.Action.Type == model.ActionTypeFetch {
		sh = s.Action.Body
		label = "body"
	}
	if !sh.Present() {
		return schema.Object(), nil
	}
	shp := shape.Shape{Raw: sh.Raw, Name: fmt.Sprintf("task %q %s", s.ID, label)}
	return shp.Check(ctx)
}

// checkHeadersShape verifies the fetch Headers shape produces a non-null object (a literal
// map of templated values, or an expression yielding a map).
func checkHeadersShape(raw any, ctx schema.Schema, taskID string) error {
	shp := shape.Shape{Raw: raw, Schema: &headersSchema, Name: fmt.Sprintf("task %q headers", taskID)}
	_, err := shp.CheckWith(ctx, shape.CheckHooks{
		Result: func(inferred, _ schema.Schema) error {
			if inferred.HasNull() || !inferred.IsType("object") {
				return fmt.Errorf("task %q headers must evaluate to a non-null object", taskID)
			}
			return fmt.Errorf("task %q headers values must all be strings", taskID)
		},
	})
	return err
}

// checkQueryShape verifies the fetch Query shape produces a non-null object of scalars. It is
// checkHeadersShape with one difference that carries the feature: a null VALUE is accepted,
// because omitting a parameter is what saves the author a conditional. The MAP itself may
// still not be null — a null query is a mistake, not an empty one.
func checkQueryShape(raw any, ctx schema.Schema, taskID string) error {
	shp := shape.Shape{Raw: raw, Schema: &querySchema, Name: fmt.Sprintf("task %q query", taskID)}
	_, err := shp.CheckWith(ctx, shape.CheckHooks{
		Result: func(inferred, _ schema.Schema) error {
			if inferred.HasNull() || !inferred.IsType("object") {
				return fmt.Errorf("task %q query must evaluate to a non-null object", taskID)
			}
			return fmt.Errorf("task %q query values must be scalars, null (which omits the parameter), or an array of them (which repeats it)", taskID)
		},
	})
	return err
}

// checkAcceptedStatusShape: must produce an array of strings; static literal elements are
// format-checked now, ${ }/$: leaves are left to matchAcceptedStatus, where an
// unrecognized value never matches.
func checkAcceptedStatusShape(raw any, ctx schema.Schema, taskID string) error {
	shp := shape.Shape{Raw: raw, Schema: &acceptedStatusSchema, Name: fmt.Sprintf("task %q accepted_status", taskID)}
	if _, err := shp.CheckWith(ctx, shape.CheckHooks{
		Result: func(inferred, _ schema.Schema) error {
			if inferred.HasNull() || !inferred.IsType("array") {
				return fmt.Errorf("task %q accepted_status must evaluate to an array of strings", taskID)
			}
			return fmt.Errorf("task %q accepted_status values must all be strings", taskID)
		},
	}); err != nil {
		return err
	}
	// The structural check above guarantees array<string>. For a literal array, also
	// validate the FORMAT of each statically-known element; a whole-value expression (raw
	// is not an array) has no static elements to check.
	elems, ok := raw.([]any)
	if !ok {
		return nil
	}
	for _, el := range elems {
		s, ok := el.(string)
		if !ok {
			continue // non-string elements were already rejected by the structural check
		}
		t, err := template.Get(s)
		if err != nil {
			continue // a malformed template already surfaced through inference above
		}
		if pat, static := t.Static(); static && !model.ValidStatusPattern(pat) {
			return fmt.Errorf("task %q accepted_status %q must be \"2xx\"/\"3xx\"/\"4xx\"/\"5xx\" or a 3-digit code", taskID, pat)
		}
	}
	return nil
}

func contextSchema(preceding []string, optional []string, tasks map[string]TaskSchemas, processInput, configSchema schema.Schema, e errAt) schema.Schema {
	return contextSchemaAbsent(preceding, optional, nil, tasks, processInput, configSchema, e)
}

// contextSchema plus the outputs definitely NOT set on this path, typed null rather than
// omitted — omission errors the access; null lets ?? take the other arm. A non-empty
// absent list comes only from the per-terminal walk, only for tasks other terminals reach.
func contextSchemaAbsent(preceding, optional, absent []string, tasks map[string]TaskSchemas, processInput, configSchema schema.Schema, e errAt) schema.Schema {
	ctx := schema.Object()
	if !processInput.IsZero() {
		ctx = ctx.WithProperty("input", processInput, true)
	}
	if !configSchema.IsZero() {
		ctx = ctx.WithProperty("config", configSchema, true)
	}

	outputs := schema.Object()
	seen := make(map[string]bool)
	for _, id := range preceding {
		if ts, ok := tasks[id]; ok && !ts.Output.IsZero() {
			outputs = outputs.WithProperty(id, ts.Output, true)
			seen[id] = true
		}
	}
	for _, id := range optional {
		if seen[id] {
			continue
		}
		if ts, ok := tasks[id]; ok && !ts.Output.IsZero() {
			outputs = outputs.WithProperty(id, ts.Output, false)
			seen[id] = true
		}
	}
	for _, id := range absent {
		if seen[id] {
			continue
		}
		if ts, ok := tasks[id]; ok && !ts.Output.IsZero() {
			// Required, so it is exactly null here rather than "maybe absent".
			outputs = outputs.WithProperty(id, schema.Type("null"), true)
			seen[id] = true
		}
	}
	ctx = ctx.WithProperty("outputs", outputs, true)

	return withErrorProperty(ctx, model.StateLastError, e)
}

// withErrorProperty adds one error namespace under name: `last_error` for the failure that
// routed control into the task, `error` for the one a rule is handling. Same shape, and the
// two differ only in which failure fills it. specs/task-scopes.md.
func withErrorProperty(ctx schema.Schema, name string, e errAt) schema.Schema {
	if !e.must && !e.may {
		return ctx
	}
	// child_key/child_index populate only from batch resolution (child-error-handling §5.3);
	// an action task's on_error leaves them absent, and the schema cannot tell which produced
	// a given failure — so both are optional, and separate single-typed fields (no type-switch).
	errSchema := schema.Object().
		WithProperty("task", schema.Type("string"), true).
		WithProperty("message", schema.Type("string"), true).
		WithProperty("code", schema.Type("string"), true).
		WithProperty("child_key", schema.Type("string"), false).
		WithProperty("child_index", schema.Type("integer"), false)
	// `data` is present exactly where a reaching rule declared a body for the status it
	// catches; where sources disagree the union already carries the null arm, so the
	// property is required and nullable rather than optional.
	if !e.data.IsZero() {
		errSchema = errSchema.WithProperty("data", e.data, true)
	}
	if e.must {
		return ctx.WithProperty(name, errSchema, true)
	}
	return ctx.WithProperty(name, errSchema.WithNull(), false)
}

// addPreActionSelf adds the half of the self scope that exists BEFORE the action runs:
// addPreviousOnly is the self scope for every slot evaluated before this task's own output
// is written — the action's own slots, and the on_error rules a failure routes through.
// `previous` is the only member that exists there; specs/task-scopes.md has the table.
func addPreviousOnly(ctx schema.Schema, s *model.Task, loops bool) schema.Schema {
	if !s.Output.Present() || !loops {
		return ctx
	}
	self := schema.Object().WithProperty("previous", schema.Ref(s.ID+"_output"), false)
	return ctx.WithProperty("self", self, true)
}

// taskLoops reports whether control can re-enter s, which is what makes a previous output
// exist. Both sets are the entry sets computeContextSets derived, so s's own id appears in
// one of them exactly when some path returns to it.
func taskLoops(s *model.Task, required, optional map[string][]string) bool {
	return slices.Contains(optional[s.ID], s.ID) || slices.Contains(required[s.ID], s.ID)
}

// addSelfSchema adds the transient self scope: self.result only when typed (no
// result_schema ⇒ no self.result at all — undeclared data is never accessible),
// self.output only when the task projects one, self.previous only when it loops. The
// latter two resolve through $defs[<id>_output].
func addSelfSchema(ctx schema.Schema, s *model.Task, loops bool, defs schema.Defs) (schema.Schema, error) {
	resultType, typed, err := actionResultType(s, defs)
	if err != nil {
		return schema.Schema{}, err
	}
	self := schema.Object()
	if typed {
		self = self.WithProperty("result", resultType, true)
	}
	self = withFetchMeta(self, s.Action)
	if s.Output.Present() {
		self = self.WithProperty("output", schema.Ref(s.ID+"_output"), true)
		if loops {
			self = self.WithProperty("previous", schema.Ref(s.ID+"_output"), false)
		}
	}
	return ctx.WithProperty("self", self, true), nil
}

// actionResultType types self.result; the bool is "typed" — true for delay/no-action
// (null) and schema-declared results, false otherwise. An untyped result stays usable in
// the switch (transient routing) but never exports through an output; no permissive fallback.
func actionResultType(s *model.Task, defs schema.Defs) (schema.Schema, bool, error) {
	if s.Action == nil {
		return schema.Type("null"), true, nil
	}
	switch s.Action.Type {
	case model.ActionTypeChildMap:
		// Typed only for the children that declare a result_schema; if none do, the whole
		// result is untyped and cannot be exported.
		sc, typed, err := childMapOutputSchema(s, defs)
		return sc, typed, err
	case model.ActionTypeChildList:
		// The single result_schema types every element; without it the array is untyped and
		// cannot be exported (no permissive fallback).
		if s.Action.ResultSchema == nil {
			return schema.Schema{}, false, nil
		}
		sc, err := childListOutputSchema(s, defs)
		return sc, true, err
	case model.ActionTypeDelay:
		return schema.Type("null"), true, nil
	case model.ActionTypeFetch:
		// The body is typed per status, so the result is the union over the statuses that can
		// be accepted — plus null where an accepted status is described by no pattern.
		return fetchResultType(s.Action, defs)
	default:
		if s.Action.ResultSchema != nil {
			// The result schema is self-contained (shared $defs baked in at
			// Normalize). Hoist its definitions into the generation pool — reusing
			// content-equal entries, renaming collisions and rewriting the schema's
			// $refs — so they resolve in every inference context it is embedded in.
			sc, err := s.Action.ResultSchema.MergeInto(defs)
			return sc, true, err
		}
		return schema.Schema{}, false, nil
	}
}

// outputMapContext: the base context plus self.result, plus self.previous ONLY when the
// task loops — only then is there a prior iteration, and previous and outputs.<id> both
// resolve through $defs[<id>_output], the placeholder the fixpoint drives.
// withFetchMeta adds self.status / self.headers, and only for a fetch. They are SIBLINGS of
// self.result, never a wrapper around it: re-shaping the result into {body, status, headers}
// would be tidier and would break every definition that reads it. The gate keeps a delay or a
// child from growing an always-null self.status — a slot every context would then carry for
// nothing — and the runtime builds its map under the same gate (engine.taskSelf). A slot in
// one and not the other is either unreadable or reads null where the type promised a value.
// Header keys are lowercased by the transport; reading one yields `string | null`, since any
// key may be absent.
func withFetchMeta(self schema.Schema, a *model.Action) schema.Schema {
	if a == nil || a.Type != model.ActionTypeFetch {
		return self
	}
	self = self.WithProperty("status", schema.Type("integer"), true)
	return self.WithProperty("headers", schema.Map(schema.Type("string")), true)
}

func outputMapContext(base schema.Schema, resultType schema.Schema, typed bool, taskID string, loops bool, action *model.Action) schema.Schema {
	self := withFetchMeta(schema.Object(), action)
	// An untyped result (fetch/external with no result_schema) is omitted here, so an
	// output that references self.result is a registration error: you cannot export an
	// untyped value — add a result_schema to type the response.
	if typed {
		self = self.WithProperty("result", resultType, true)
	}
	if loops {
		self = self.WithProperty("previous", schema.Ref(taskID+"_output"), false)
	}
	return base.WithProperty("self", self, true)
}
