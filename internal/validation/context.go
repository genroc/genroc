package validation

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"genroc/internal/model"
	"genroc/internal/schema"
)

// On an isErr (on_error) edge the failing task has no output.
type predEdge struct {
	idx   int  // predecessor task index; -1 = process start
	isErr bool // true = on_error route
	// rule indexes the predecessor's OnError for an error edge, -1 otherwise — the only way to
	// know which statuses, and so which declared bodies, can reach a handler.
	rule int
	// sw indexes the predecessor's Switch for a non-error edge, -1 otherwise. Two cases to one
	// target prove different things, so do not collapse the `next` cases below into one edge.
	// specs/guard-narrowing.md.
	sw int
}

// Only an `output` projection exports; a raw result, even typed, is self.result to the task's
// own output and switch and never reaches the shared context.
func taskHasOutput(s *model.Task) bool {
	return s.Output.Present()
}

// terminalEnd is one way the process can finish: the task it ends on, the task outputs
// guaranteed present there (must) and possibly present (may), and whether `error` is.
type terminalEnd struct {
	task   string
	must   map[string]bool
	may    map[string]bool
	errMin bool
	errMax bool
}

// One constructor per phase (specs/task-scopes.md), so the checker and slots.go cannot build a
// context differently. TestSlotContextsAreTheCheckersOwn holds them together.
type taskScopes struct {
	tasks              map[string]TaskSchemas
	processInput       schema.Schema
	configSchema       schema.Schema
	defs               schema.Defs
	required, optional map[string][]string
	errs               map[string]errAt
	// refinements is what the edges into each task proved about references it can read —
	// empty for a caller that does not compute them, which simply means no narrowing.
	refinements map[string]refs
}

// base is the part every slot of a task shares: the process input, config, the outputs that
// reach it, and the failure that routed control here. `self` is what the phases add.
func (sc taskScopes) base(t *model.Task) schema.Schema {
	ctx := contextSchema(sc.required[t.ID], sc.optional[t.ID], sc.tasks, sc.processInput, sc.configSchema, sc.errs[t.ID])
	// Here because every phase goes through base. The lookup needs the defs pool (an output is a
	// $ref); the guards ride on the bare context either way.
	return applyRefinements(ctx, ctx.WithDefs(sc.defs), sc.refinements[t.ID])
}

// switchCase narrows by what reaching case k proves, in the engine's evaluation order, so a value
// guarded in one case is readable in the next. specs/guard-narrowing.md.
func (sc taskScopes) switchCase(t *model.Task, k int, switchCtx schema.Schema) schema.Schema {
	return sc.narrow(t, switchCtx, clauseFacts(switchClauses(t), k, false, sameFrame))
}

// A case's `panic` and `raise` render only when it MATCHED, so they may assume it — refusing
// splits a guard from the message it was written to make safe.
func (sc taskScopes) switchClause(t *model.Task, k int, switchCtx schema.Schema) schema.Schema {
	return sc.narrow(t, switchCtx, clauseFacts(switchClauses(t), k, true, sameFrame))
}

// narrow applies what a slot proved on top of what the edges into the task did. The context is
// its own resolvable: every caller here already carries the pool.
func (sc taskScopes) narrow(t *model.Task, ctx schema.Schema, local refs) schema.Schema {
	if len(local) == 0 {
		return ctx
	}
	return applyRefinements(ctx, ctx, unionRefs(sc.refinements[t.ID], local))
}

func (sc taskScopes) loops(t *model.Task) bool { return taskLoops(t, sc.required, sc.optional) }

// entry is the context on entry to the task, with no `self` at all — what an instance sitting
// there holds. Compare uses it against a stored row.
func (sc taskScopes) entry(t *model.Task) schema.Schema {
	return sc.base(t).WithDefs(sc.defs)
}

// action is the scope of every slot evaluated before the task's output exists: the action's
// own, `timeout`, and a batch's per-entry inputs.
func (sc taskScopes) action(t *model.Task) schema.Schema {
	return addPreviousOnly(sc.base(t), t, sc.loops(t)).WithDefs(sc.defs)
}

// The bool is whether the result is typed: an untyped one is absent rather than unknown, so a
// reference to it reports as the rule it breaks.
func (sc taskScopes) outputMap(t *model.Task) (schema.Schema, bool, error) {
	ts := sc.tasks[t.ID]
	return outputMapContext(sc.base(t), ts.Result, ts.resultTyped, t.ID, sc.loops(t), t.Action).WithDefs(sc.defs), ts.resultTyped, nil
}

// switchScope is the scope of every switch clause: the output map has run, so `self.output` is
// the projection this run just produced.
func (sc taskScopes) switchScope(t *model.Task) (schema.Schema, error) {
	ctx := sc.base(t)
	if t.Action == nil && !t.Output.Present() {
		return ctx.WithDefs(sc.defs), nil
	}
	ts := sc.tasks[t.ID]
	return addSelfSchema(ctx, t, sc.loops(t), ts.Result, ts.resultTyped).WithDefs(sc.defs), nil
}

// processOutput is the JOINED scope over terminal paths, with no `self`; processOutputContext
// holds the per-path arms.
func (sc taskScopes) processOutput(def *model.ProcessDefinition, errData schema.Schema) schema.Schema {
	req, opt, errReq, errOpt := outputContextSets(def)
	e := errAt{must: errReq, may: errOpt, data: errData}
	return contextSchema(req, opt, sc.tasks, sc.processInput, sc.configSchema, e).WithDefs(sc.defs)
}

// processOutputContext has one arm per way the process can end, typing null what that path does
// not set. The arms carry the correlation inference distributes over (schema.InferNode), so the
// precision lives in the CONTEXT. specs/path-sensitive-output.md.
func (sc taskScopes) processOutputContext(def *model.ProcessDefinition) schema.Schema {
	terminals := outputTerminals(def)
	if len(terminals) == 0 {
		// Nothing reaches the end: the expression is still checked, against no outputs.
		return sc.processOutput(def, schema.Schema{})
	}
	// An output no terminal reaches stays out of every arm, so reading it is still an access
	// error rather than silently null.
	everMay := make(map[string]bool)
	for _, t := range terminals {
		for id := range t.may {
			everMay[id] = true
		}
	}
	arms := make([]schema.Schema, 0, len(terminals))
	for _, t := range terminals {
		arms = append(arms, sc.processOutputAt(t, everMay))
	}
	if len(arms) == 1 {
		return arms[0]
	}
	return schema.AnyOf(arms...).WithDefs(sc.defs)
}

// processOutputAt types null what only OTHER paths set: the correlation the join destroys, which
// lets `outputs.a ?? outputs.b` be non-null when a and b between them cover every ending.
func (sc taskScopes) processOutputAt(t terminalEnd, everMay map[string]bool) schema.Schema {
	var must, opt, absent []string
	for id := range t.must {
		must = append(must, id)
	}
	for id := range t.may {
		if !t.must[id] {
			opt = append(opt, id)
		}
	}
	for id := range everMay {
		if !t.may[id] {
			absent = append(absent, id)
		}
	}
	sort.Strings(must)
	sort.Strings(opt)
	sort.Strings(absent)
	e := errAt{must: t.errMin, may: t.errMax, data: sc.errs[t.task].data}
	// Named ON the arm, so a reader of the context gets the checker's "on the path ending at
	// task …" sentence.
	return contextSchemaAbsent(must, opt, absent, sc.tasks, sc.processInput, sc.configSchema, e).
		WithDescription(fmt.Sprintf("on the path ending at task %q", t.task)).
		WithDefs(sc.defs)
}

// rule is one on_error rule's scope: the task's own, plus `error` — the failure THIS rule
// caught, which is not the `last_error` that routed control here.
func (sc taskScopes) rule(t *model.Task, k int, ec model.ErrorCase) schema.Schema {
	return sc.narrow(t, sc.ruleScope(t, ec), clauseFacts(ruleClauses(t), k, false, sameFrame))
}

// ruleClause scopes rule k's `retry`, `panic` and `raise`: they run only when it CAUGHT, which
// proves its case true — the one direction negation cannot give. specs/guard-narrowing.md.
func (sc taskScopes) ruleClause(t *model.Task, k int, ec model.ErrorCase) schema.Schema {
	return sc.narrow(t, sc.ruleScope(t, ec), clauseFacts(ruleClauses(t), k, true, sameFrame))
}

func (sc taskScopes) ruleScope(t *model.Task, ec model.ErrorCase) schema.Schema {
	ctx := withErrorProperty(sc.base(t), model.StateError, ruleErrAt(t, ec, sc.defs))
	return addPreviousOnly(ctx, t, sc.loops(t)).WithDefs(sc.defs)
}

// One entry per way of ending, kept apart because outputContextSets INTERSECTS must-sets and
// destroys the correlation. specs/path-sensitive-output.md.
func outputTerminals(def *model.ProcessDefinition) []terminalEnd {
	tasks := def.Tasks
	n := len(tasks)
	if n == 0 {
		return nil
	}

	reqMap, optMap, mustErrMap, mayErrMap, _ := computeContextSets(tasks)

	var terminals []terminalEnd

	addTerminal := func(s *model.Task, includeOwnOutput bool, errMin, errMax bool) {
		must := make(map[string]bool)
		for _, id := range reqMap[s.ID] {
			must[id] = true
		}
		if includeOwnOutput && taskHasOutput(s) {
			must[s.ID] = true
		}
		may := make(map[string]bool)
		for id := range must {
			may[id] = true
		}
		for _, id := range optMap[s.ID] {
			may[id] = true
		}
		terminals = append(terminals, terminalEnd{task: s.ID, must: must, may: may, errMin: errMin, errMax: errMax})
	}

	for i, s := range tasks {
		isNormal := (len(s.Switch) == 0 && i == n-1) ||
			func() bool {
				for _, c := range s.Switch {
					if c.Goto == model.GotoEnd {
						return true
					}
				}
				return false
			}()
		isErrEnd := func() bool {
			for _, ec := range s.OnError {
				if ec.Goto == model.GotoEnd {
					return true
				}
			}
			return false
		}()

		if isNormal {
			addTerminal(s, true, mustErrMap[s.ID], mayErrMap[s.ID])
		}
		if isErrEnd {
			// The failing task produced no output; error is always present.
			addTerminal(s, false, true, true)
		}
	}

	return terminals
}

// outputContextSets is the single COLLAPSED answer at the process output, for callers that
// cannot use outputTerminals' per-path detail.
func outputContextSets(def *model.ProcessDefinition) (required, optional []string, errRequired, errOptional bool) {
	terminals := outputTerminals(def)
	if len(terminals) == 0 {
		return
	}

	mustAtEnd := make(map[string]bool)
	for id := range terminals[0].must {
		mustAtEnd[id] = true
	}
	for _, t := range terminals[1:] {
		for id := range mustAtEnd {
			if !t.must[id] {
				delete(mustAtEnd, id)
			}
		}
	}

	mayAtEnd := make(map[string]bool)
	for _, t := range terminals {
		for id := range t.may {
			mayAtEnd[id] = true
		}
	}

	// Sorted below: these become a `required` array, and `genctl schema` prints a document
	// people diff.
	for id := range mustAtEnd {
		required = append(required, id)
	}
	for id := range mayAtEnd {
		if !mustAtEnd[id] {
			optional = append(optional, id)
		}
	}

	allErrMin := true
	for _, t := range terminals {
		if !t.errMin {
			allErrMin = false
			break
		}
	}
	anyErrMax := false
	for _, t := range terminals {
		if t.errMax {
			anyErrMax = true
			break
		}
	}
	errRequired = allErrMin
	errOptional = anyErrMax && !allErrMin
	sort.Strings(required)
	sort.Strings(optional)
	return
}

// The process start is predEdge{idx: -1} on task 0.
func buildPreds(tasks []*model.Task) [][]predEdge {
	n := len(tasks)
	idx := make(map[string]int, n)
	for i, s := range tasks {
		idx[s.ID] = i
	}
	preds := make([][]predEdge, n)
	preds[0] = append(preds[0], predEdge{idx: -1, rule: -1, sw: -1})
	for i, s := range tasks {
		for k, c := range s.Switch {
			if strings.HasPrefix(c.Goto, "$") {
				if j, ok := idx[c.Goto[1:]]; ok {
					preds[j] = append(preds[j], predEdge{idx: i, rule: -1, sw: k})
				}
			} else if c.Goto == model.GotoNext && i+1 < n {
				preds[i+1] = append(preds[i+1], predEdge{idx: i, rule: -1, sw: k})
			}
		}
		// No switch falls through to the next task.
		if len(s.Switch) == 0 && i+1 < n {
			preds[i+1] = append(preds[i+1], predEdge{idx: i, rule: -1, sw: -1})
		}
		for r, ec := range s.OnError {
			if ec.Goto != "" && ec.Goto != model.GotoEnd {
				if j, ok := idx[ec.Goto]; ok {
					preds[j] = append(preds[j], predEdge{idx: i, isErr: true, rule: r, sw: -1})
				}
			}
		}
	}
	return preds
}

func checkReachability(tasks []*model.Task) error {
	if len(tasks) == 0 {
		return nil
	}
	preds := buildPreds(tasks)
	reachable := make([]bool, len(tasks))
	reachable[0] = true
	for {
		changed := false
		for i, ps := range preds {
			if reachable[i] {
				continue
			}
			for _, p := range ps {
				if p.idx >= 0 && reachable[p.idx] {
					reachable[i] = true
					changed = true
					break
				}
			}
		}
		if !changed {
			break
		}
	}
	for i, s := range tasks {
		if !reachable[i] {
			return fmt.Errorf("task %q is unreachable: no switch or error handler routes to it", s.ID)
		}
	}
	return nil
}

// errSource names one on_error rule that can have set the `error` a task reads: the task the
// rule belongs to, and its index in that task's OnError slice.
type errSource struct {
	task int
	rule int
}

// computeContextSets: per task, the prior outputs always (required) or sometimes (optional)
// present on entry, and the same for `error` (mustErr, mayErr).
func computeContextSets(tasks []*model.Task) (required, optional map[string][]string, mustErr, mayErr map[string]bool, errSrc map[string][]errSource) {
	n := len(tasks)
	required = make(map[string][]string, n)
	optional = make(map[string][]string, n)
	mustErr = make(map[string]bool, n)
	mayErr = make(map[string]bool, n)
	errSrc = make(map[string][]errSource, n)
	if n == 0 {
		return
	}

	preds := buildPreds(tasks)

	hasOutput := make([]bool, n)
	for i, s := range tasks {
		hasOutput[i] = taskHasOutput(s)
	}

	// mustIn[i][j]: task j's output is ALWAYS available on entry to task i; mayIn: POSSIBLY.
	// The out-sets only drive the fixpoint; the projection below reads what held on ENTRY.
	_, mustIn := availability(preds, hasOutput, true, func(a, b bool) bool { return a && b })
	_, mayIn := availability(preds, hasOutput, false, func(a, b bool) bool { return a || b })

	// `error` is scoped to the task an on_error rule routes TO: the engine drops it on every
	// ordinary transition, so these are local questions about one task's incoming edges.
	mustErrArr := make([]bool, n)
	mayErrArr := make([]bool, n)
	srcArr := make([][]errSource, n)
	for i := range tasks {
		allErr := len(preds[i]) > 0
		for _, p := range preds[i] {
			if !p.isErr {
				allErr = false
				continue
			}
			mayErrArr[i] = true
			srcArr[i] = append(srcArr[i], errSource{task: p.idx, rule: p.rule})
		}
		mustErrArr[i] = allErr
	}

	for i, s := range tasks {
		errSrc[s.ID] = srcArr[i]
	}

	for i, s := range tasks {
		for j, ss := range tasks {
			switch {
			case mustIn[i][j]:
				required[s.ID] = append(required[s.ID], ss.ID)
			case mayIn[i][j]:
				optional[s.ID] = append(optional[s.ID], ss.ID)
			}
		}

		mustErr[s.ID] = mustErrArr[i]
		mayErr[s.ID] = mayErrArr[i]
	}
	return
}

// availability is the fixpoint both analyses share: true/AND for "always", false/OR for
// "possibly". An error edge clears the failing task's own bit. A task no edge reaches starts
// empty, not at `top` — for AND that would claim everything.
func availability(preds [][]predEdge, hasOutput []bool, top bool, combine func(a, b bool) bool) (outs, ins [][]bool) {
	n := len(preds)
	fill := func(v bool) []bool {
		s := make([]bool, n)
		for i := range s {
			s[i] = v
		}
		return s
	}
	outs = make([][]bool, n)
	ins = make([][]bool, n)
	for i := range outs {
		outs[i] = fill(top)
	}
	for changed := true; changed; {
		changed = false
		for i := range preds {
			in := fill(top)
			if len(preds[i]) == 0 {
				in = fill(false)
			}
			for _, p := range preds[i] {
				src := fill(false)
				if p.idx != -1 {
					src = outs[p.idx]
					if p.isErr && hasOutput[p.idx] {
						src = append([]bool{}, src...)
						src[p.idx] = false
					}
				}
				for j := range in {
					in[j] = combine(in[j], src[j])
				}
			}
			ins[i] = in
			out := append([]bool{}, in...)
			if hasOutput[i] {
				out[i] = true
			}
			if !slices.Equal(outs[i], out) {
				outs[i] = out
				changed = true
			}
		}
	}
	return outs, ins
}
