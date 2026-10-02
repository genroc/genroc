package validation

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

	"genroc/internal/delayspec"
	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/shape"
	"genroc/internal/template"
)

func buildInputs(tasks []*model.Task, taskSchemas map[string]TaskSchemas, processInput, configSchema schema.Schema, defs schema.Defs, rd *raiseData, b *bag) error {
	// Reachability is a property of the whole graph, so there is no slot to hang it on and
	// nothing below it is worth analysing: a task nothing reaches has no context.
	if err := checkReachability(tasks); err != nil {
		b.add("", CodeStructure, err)
		return nil
	}
	required, optional, mustErr, mayErr, errSrc := computeContextSets(tasks)
	b.observe(required, optional)
	errs := errContexts(tasks, mustErr, mayErr, errSrc, defs)
	scopes := taskScopes{
		tasks: taskSchemas, processInput: processInput, configSchema: configSchema, defs: defs,
		required: required, optional: optional, errs: errs,
		refinements: computeRefinements(tasks),
	}

	// Phase 1: every exported output type, in dependency order (mutually recursive tasks jointly),
	// written to defs before phase 2 reads any of them.
	if err := inferOutputs(tasks, scopes, b); err != nil {
		return err
	}

	// Phase 2: action inputs and switch type-checks.
	for _, s := range tasks {
		loops := taskLoops(s, required, optional)
		// Ahead of the per-slot checks, so a member that does not exist here is reported as
		// the rule it breaks rather than as the schema's "field not found".
		if err := checkPreOutputScopes(s, loops); err != nil {
			b.add(schema.JoinPath(slotTasks, s.ID), CodeStructure, err)
			continue
		}
		if s.Action != nil {
			// Each section is one slot: its first failure stops it and is recorded at its
			// address, and the sections after it are still analysed.
			b.add(taskSlot(s.ID, slotAction), CodeExpression, func() error {
				ts, inMap := taskSchemas[s.ID]
				touched := inMap
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
				hasTimeout := !s.Action.Timeout.IsZero()
				hasDeclaredPayload := s.Action.BodySchema != nil || s.Action.InputSchema != nil
				hasDeclared := hasDeclaredPayload || s.Action.QuerySchema != nil ||
					s.Action.Type == model.ActionTypeChildMap
				if inMap || hasBody || hasInput || hasURL || hasMethod || hasHeaders || hasQuery || hasAcceptedStatus || hasOver || hasFor || hasUntil || hasTimeout || hasDeclared {
					ctx := scopes.action(s)
					// `over` must be a non-null array; each element is one child's input.
					if hasOver {
						arr, err := checkArrayTemplate(s.Action.Over, ctx, s.ID)
						if err != nil {
							return inField("over", err)
						}
						// A declaration types one ELEMENT, as result_schema does here.
						if err := checkDeclaredListElement(s, arr, scopes.defs); err != nil {
							return inField("over", err)
						}
					}
					// A literal is parsed against delayspec, a $: expression typed to a number, and
					// ${ } refused — so a malformed one fails at registration, not when reached.
					if hasFor {
						if err := inField("for", checkDelaySlot(s.Action.For, ctx, s.ID, "delay", "for")); err != nil {
							return err
						}
					}
					if hasUntil {
						if err := inField("until", checkDelaySlot(s.Action.Until, ctx, s.ID, "delay", "until")); err != nil {
							return err
						}
					}
					// A timeout is the same two slots pointed at a deadline, so it is checked the
					// same way — a literal against the grammar, a $: expression to a number.
					if hasTimeout {
						if err := inField("timeout", checkTimeout(&s.Action.Timeout, ctx, s.ID)); err != nil {
							return err
						}
					}
					// A null URL or method would silently stringify to "null".
					if hasURL {
						if err := inField("url", checkNonNullTemplate(s.Action.URL, ctx, fmt.Sprintf("task %q url", s.ID))); err != nil {
							return err
						}
					}
					if hasMethod {
						if err := inField("method", checkNonNullTemplate(s.Action.Method, ctx, fmt.Sprintf("task %q method", s.ID))); err != nil {
							return err
						}
					}
					if hasHeaders {
						if err := inField("headers", checkHeadersShape(s.Action.Headers.Raw, ctx, s.ID)); err != nil {
							return err
						}
					}
					if hasQuery {
						q, err := checkQueryShape(s.Action.Query.Raw, ctx, s.ID)
						if err != nil {
							return inField("query", err)
						}
						// BESIDE the fixed target, not instead of it: running both keeps each
						// failure named as itself.
						if isFetch && s.Action.QuerySchema != nil {
							if q, err = checkDeclaredQuery(s, ctx); err != nil {
								return inField("query", err)
							}
						}
						ts.Query = q
						touched = true
					}
					// An expression's elements are unknown statically; an unrecognized pattern
					// simply never matches at runtime.
					if hasAcceptedStatus {
						if err := inField("accepted_status", checkAcceptedStatusShape(s.Action.AcceptedStatus.Raw, ctx, s.ID)); err != nil {
							return err
						}
					}
					if s.Action.Type == model.ActionTypeChildMap {
						children, err := checkChildMapInputs(s, ctx)
						if err != nil {
							return err
						}
						if len(children) > 0 {
							ts.Children = children
							touched = true
						}
					}
					if inMap || hasBody || hasInput || hasDeclaredPayload {
						input, err := inferActionPayload(s, ctx)
						if err != nil {
							return err
						}
						ts.Input = input
						touched = true
					}
					if touched {
						if !inMap {
							ts.ActionType = s.Action.Type
						}
						taskSchemas[s.ID] = ts
					}
				}
				return nil
			}())
		}

		if len(s.Switch) > 0 {
			b.add(taskSlot(s.ID, slotSwitch), CodeExpression, func() error {
				switchCtx, err := scopes.switchScope(s)
				if err != nil {
					return fmt.Errorf("task %q: %w", s.ID, err)
				}
				// An untyped result is no more readable in a case than in an output, and gets the
				// same message.
				untypedResult := s.Action != nil && !taskSchemas[s.ID].resultTyped
				for i, c := range s.Switch {
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
					if _, err := shp.CheckWith(scopes.switchCase(s, i, switchCtx), hooks); err != nil {
						return inField(strconv.Itoa(i)+"."+slotCase, err)
					}
				}
				// Checked in the clause's own scope (a case sees `self`), which is why this runs
				// here rather than beside the code's shape rule in model.
				for i := range s.Switch {
					where := fmt.Sprintf("switch case %d", i)
					clauseCtx := scopes.switchClause(s, i, switchCtx)
					if err := checkFaultClauses(s.Switch[i].Raise, s.Switch[i].Panic, clauseCtx, s.ID, where, rd); err != nil {
						return err
					}
				}
				return nil
			}())
		}

		// An on_error rule sees the error it CAUGHT, not the one that reaches a task it
		// routes to — so its context is built per rule rather than from errs[s.ID].
		for i, ec := range s.OnError {
			if ec.Raise == nil && ec.Panic == nil && ec.Case == "" && ec.Retry.IsZero() {
				continue
			}
			// The task's own context — `last_error` and all — plus `error`, the failure THIS
			// rule caught. Both are readable here and they are different errors.
			b.add(ruleSlot(s.ID, i), CodeExpression, func() error {
				ruleCtx := scopes.rule(s, i, ec)
				where := fmt.Sprintf("on_error[%d]", i)
				// The SAME per-rule scope as the clauses: `code` already chose the error, so
				// `error.data` is that code's shape. specs/child-error-handling.md M2.
				if ec.Case != "" {
					hooks := shape.CheckHooks{
						Result: func(inferred, _ schema.Schema) error {
							return fmt.Errorf("task %q %s case %q: expression must evaluate to boolean, got %q", s.ID, where, ec.Case, inferred.TypeName())
						},
					}
					shp := shape.Shape{Raw: ec.Case, Schema: &boolSchema, Name: fmt.Sprintf("task %q %s case %q", s.ID, where, ec.Case), Expr: true}
					if _, err := shp.CheckWith(ruleCtx, hooks); err != nil {
						return inField(slotCase, err)
					}
				}
				// The clauses run only when the rule CAUGHT, so they read a scope its own
				// `case` has narrowed — the case expression above cannot, being what proves it.
				clauseCtx := scopes.ruleClause(s, i, ec)
				if err := checkFaultClauses(ec.Raise, ec.Panic, clauseCtx, s.ID, where, rd); err != nil {
					return err
				}
				// A literal retry slot was checked by the decoder; a $: expression is typed here,
				// in the rule's own scope.
				if err := checkRetrySlots(s.ID, i, ec, clauseCtx); err != nil {
					return inField(slotRetry, err)
				}
				return nil
			}())
		}
	}
	return nil
}

// Fixed slot targets. url/method render with %v, so null or a struct corrupts the request.
var (
	scalarSchema         = schema.Type("string", "number", "boolean")
	arraySchema          = schema.Array(schema.Schema{})
	headersSchema        = schema.Map(schema.Type("string"))
	acceptedStatusSchema = schema.Array(schema.Type("string"))
	// A null VALUE omits its parameter, which is the whole point of the slot.
	querySchema = schema.Map(schema.AnyOf(
		schema.Type("string", "number", "boolean", "null"),
		schema.Array(schema.Type("string", "number", "boolean", "null")),
	))
	boolSchema = schema.Type("boolean")
	// A message lands in inst.Error and the audit log, both text, so unlike url/method it takes
	// no bare number or boolean; an interpolation always satisfies it.
	messageSchema = schema.Type("string")
	// Milliseconds for `for`, unix milliseconds for `until`. Literals never reach the type system
	// (delayspec parses them), so string is not accepted.
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

// A message must be a non-null string: it lands in inst.Error, the audit log and, via collect,
// a parent's `error.message`.
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

// Fixed order, so a clause with both reports the same one every run. A raise RECORDS what its
// data inferred -- the only place its scope is in hand; a panic records nothing, for the reason
// ProcessDefinition.Raises excludes its code.
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

// absent is not a no-op: the slot is CLEARED, so a caller conforms null — which a declared object
// shape refuses, and that refusal is the point.
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

// checkDelaySlot: a bare number needs no check, a literal parses delayspec, "$:" must infer to
// number, and ${ } is refused BY NAME — it produces a string, the failure this syntax removes.
func checkDelaySlot(raw any, ctx schema.Schema, taskID, where, slot string) error {
	label := fmt.Sprintf("task %q %s %s", taskID, where, slot)

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
	// Not an Expr shape: src still has its "$:" marker, and a plain shape's $: leaf is
	// type-preserving anyway.
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

// Positivity is deliberately not checked: an `until` judged at registration would validate
// differently on different days. The engine judges the resolved instant against its clock.
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

// Every retry slot must infer to a number; the bounds (whole, non-negative, factor >= 1,
// max_delay >= delay) can only be judged when the rule fires, by Retry.Resolve.
func checkRetrySlots(taskID string, i int, ec model.ErrorCase, ctx schema.Schema) error {
	slots := []struct{ name, expr string }{
		{"retries", ec.Retry.Retries.Expr()},
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

// Parsing alone is the whole check: it is clock-independent, so a definition always validates
// the same way.
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

// inferActionPayload types a fetch's Body or any other action's Input. A declaration, where one
// exists, is both the check and the published type.
func inferActionPayload(s *model.Task, ctx schema.Schema) (schema.Schema, error) {
	sh := s.Action.Input
	label := "input"
	declared := s.Action.InputSchema
	switch s.Action.Type {
	case model.ActionTypeFetch:
		sh = s.Action.Body
		label = "body"
		declared = s.Action.BodySchema
	case model.ActionTypeChildList:
		// There is no `input` shape to check here — the elements of `over` are the inputs, and
		// the `over` branch checks them.
		return schema.Object(), nil
	}
	// An absent shape is the empty object rather than nothing to check: a declaration with a
	// required property and no payload beside it is a mistake worth the same sentence.
	var raw any = map[string]any{}
	if sh.Present() {
		raw = sh.Raw
	} else if declared == nil {
		return schema.Object(), nil
	}
	shp, hooks := declaredShape(raw, declared, fmt.Sprintf("task %q %s", s.ID, label))
	inferred, err := shp.CheckWith(ctx, hooks)
	if err != nil {
		return schema.Schema{}, err
	}
	return sent(inferred, declared), nil
}

// Here, not in validate_children.go: a declaration needs NO database, so an editor and an
// offline genctl can run this check.
func checkChildMapInputs(s *model.Task, ctx schema.Schema) (map[string]schema.Schema, error) {
	out := make(map[string]schema.Schema, len(s.Action.Children))
	for _, key := range sortedChildKeys(s.Action.Children) {
		entry := s.Action.Children[key]
		// Typing such an entry as an empty object would put a task with no typed surface in the
		// type view.
		if !entry.Input.Present() && entry.InputSchema == nil {
			continue
		}
		var raw any = map[string]any{}
		if entry.Input.Present() {
			raw = entry.Input.Raw
		}
		label := fmt.Sprintf("task %q children[%q] input", s.ID, key)
		shp, hooks := declaredShape(raw, entry.InputSchema, label)
		inferred, err := shp.CheckWith(ctx, hooks)
		if err != nil {
			return nil, inField("children."+key+".input", err)
		}
		out[key] = sent(inferred, entry.InputSchema)
	}
	return out, nil
}

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

// checkQueryShape is checkHeadersShape except a null VALUE is accepted (it omits the parameter).
// A null MAP is still a mistake, not an empty query.
func checkQueryShape(raw any, ctx schema.Schema, taskID string) (schema.Schema, error) {
	shp := shape.Shape{Raw: raw, Schema: &querySchema, Name: fmt.Sprintf("task %q query", taskID)}
	return shp.CheckWith(ctx, shape.CheckHooks{
		Result: func(inferred, _ schema.Schema) error {
			if inferred.HasNull() || !inferred.IsType("object") {
				return fmt.Errorf("task %q query must evaluate to a non-null object", taskID)
			}
			return fmt.Errorf("task %q query values must be scalars, null (which omits the parameter), or an array of them (which repeats it)", taskID)
		},
	})
}

// Static literal elements are format-checked now; ${ }/$: leaves are left to
// matchAcceptedStatus, where an unrecognized value never matches.
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
	// Only a literal array has static elements whose FORMAT can be checked.
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

// absent outputs type null rather than being omitted: omission errors the access, null lets ??
// take the other arm. Only the per-terminal arms pass any.
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

// name is `last_error` (the failure that routed here) or `error` (the one a rule is handling):
// same shape, filled by different failures. specs/task-scopes.md.
func withErrorProperty(ctx schema.Schema, name string, e errAt) schema.Schema {
	if !e.must && !e.may {
		return ctx
	}
	// child_key/child_index come only from batch resolution (child-error-handling §5.3), and the
	// schema cannot tell which produced a failure — so both are optional.
	errSchema := schema.Object().
		WithProperty("task", schema.Type("string"), true).
		WithProperty("message", schema.Type("string"), true).
		WithProperty("code", schema.Type("string"), true).
		WithProperty("child_key", schema.Type("string"), false).
		WithProperty("child_index", schema.Type("integer"), false)
	// Where sources disagree the union already carries the null arm, so `data` is required and
	// nullable rather than optional.
	if !e.data.IsZero() {
		errSchema = errSchema.WithProperty("data", e.data, true)
	}
	if e.must {
		return ctx.WithProperty(name, errSchema, true)
	}
	return ctx.WithProperty(name, errSchema.WithNull(), false)
}

// addPreviousOnly is `self` for every slot evaluated before the task's own output is written:
// only `previous` exists there. specs/task-scopes.md has the table.
func addPreviousOnly(ctx schema.Schema, s *model.Task, loops bool) schema.Schema {
	if !s.Output.Present() || !loops {
		return ctx
	}
	self := schema.Object().WithProperty("previous", schema.Ref(s.ID+"_output"), false)
	return ctx.WithProperty("self", self, true)
}

// s's own id is in its entry sets exactly when some path returns to it.
func taskLoops(s *model.Task, required, optional map[string][]string) bool {
	return slices.Contains(optional[s.ID], s.ID) || slices.Contains(required[s.ID], s.ID)
}

// addSelfSchema: self.result only when typed (undeclared data is never readable), self.output
// only when projected, self.previous only when the task loops.
func addSelfSchema(ctx schema.Schema, s *model.Task, loops bool, resultType schema.Schema, typed bool) schema.Schema {
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
	return ctx.WithProperty("self", self, true)
}

// The bool is true only where a DECLARATION types the result. Otherwise there is no self.result
// at all: reading it gets its own sentence (untypedResultAdvice), not a slot reading null forever.
func actionResultType(s *model.Task, defs schema.Defs) (schema.Schema, bool, error) {
	if s.Action == nil {
		return schema.Schema{}, false, nil
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
		// Not `null`: a self.result that is always null sends the author to `?? 0` instead of
		// to the reference that cannot work.
		return schema.Schema{}, false, nil
	case model.ActionTypeFetch:
		// The body is typed per status, so the result is the union over the statuses that can
		// be accepted — plus null where an accepted status is described by no pattern.
		return fetchResultType(s.Action, defs)
	default:
		if s.Action.ResultSchema != nil {
			// Hoisted into the pool (content-equal reuse, collisions renamed) so it resolves
			// in every context it is embedded in.
			sc, err := s.Action.ResultSchema.MergeInto(defs)
			return sc, true, err
		}
		return schema.Schema{}, false, nil
	}
}

// self.status / self.headers are SIBLINGS of self.result, fetch only — the gate engine.taskSelf
// shares, or a slot is unreadable or reads null where a value was promised.
func withFetchMeta(self schema.Schema, a *model.Action) schema.Schema {
	if a == nil || a.Type != model.ActionTypeFetch {
		return self
	}
	self = self.WithProperty("status", schema.Type("integer"), true)
	return self.WithProperty("headers", schema.Map(schema.Type("string")), true)
}

// self.previous ONLY when the task loops; it resolves through $defs[<id>_output], the
// placeholder the fixpoint drives.
func outputMapContext(base schema.Schema, resultType schema.Schema, typed bool, taskID string, loops bool, action *model.Action) schema.Schema {
	self := withFetchMeta(schema.Object(), action)
	// An untyped result is omitted, so an output reading self.result is a registration error.
	if typed {
		self = self.WithProperty("result", resultType, true)
	}
	if loops {
		self = self.WithProperty("previous", schema.Ref(taskID+"_output"), false)
	}
	return base.WithProperty("self", self, true)
}
