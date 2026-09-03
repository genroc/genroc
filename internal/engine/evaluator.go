package engine

import (
	"context"
	"fmt"
	"maps"

	"genroc/internal/expression"
	"genroc/internal/model"
	"genroc/internal/shape"
	tmpl "genroc/internal/template"
)

// context wraps the instance's decoded context for reading: it resolves an externalized value
// only where a walk has to step through one, and NEVER writes a loaded value back. Writing back
// destroys the markers the write path re-emits, which is what made a slot read once cost a
// re-marshal and a re-hash on every write after. specs/lazy-context.md.
func (e *Engine) context(inst *model.ProcessInstance) *model.Context {
	if inst.ResolvedObjects == nil {
		inst.ResolvedObjects = map[string]any{}
	}
	return model.NewContext(inst.State, func(hash string) (any, error) {
		// One closure, every object load in the engine: a dropped connection is ridden out here
		// rather than becoming a terminal failure for an instance that could have retried.
		return retryRead(func() (any, error) {
			return e.db.ResolveObject(context.Background(), &model.ObjectRef{Ref: hash})
		})
	}, inst.ResolvedObjects)
}

// buildEnv assembles the expression environment for inst, resolving only what the expression
// actually reads. Two axes, both from the static analysis in expression.Roots:
//
//   - a slot the expression never names is not included at all (the slot-level laziness);
//   - a slot it only COPIES keeps its references, and one it reads THROUGH is materialized.
//
// The second is what lets a value pass through an advance untouched: the marker flows into the
// evaluated result and on into the next write as the reference it already was, never loaded.
// specs/lazy-context.md.
func (e *Engine) buildEnv(inst *model.ProcessInstance, self any, roots expression.Roots) (map[string]any, error) {
	ctx := e.context(inst)
	config := inst.Config
	if config == nil {
		config = map[string]any{}
	}
	env := map[string]any{"self": self, "config": config}

	// self.previous is this task's own prior output -- the same value as outputs[<this task>],
	// so when that output was externalized it reloads as a marker. Treated exactly like an
	// outputs.<id> read: materialized only if the expression reads into it.
	if roots.SelfPrevious {
		if sm, ok := self.(map[string]any); ok {
			prev := sm["previous"]
			if roots.Through.SelfPrevious {
				rv, err := ctx.Materialize(prev)
				if err != nil {
					return nil, err
				}
				prev = rv
			}
			selfCopy := make(map[string]any, len(sm))
			for k, v := range sm {
				selfCopy[k] = v
			}
			selfCopy["previous"] = prev
			env["self"] = selfCopy
		}
	}

	include := func(key string, referenced, through bool) error {
		v, err := ctx.At(key)
		if err != nil {
			return err
		}
		if _, isRef := v.(*model.ObjectRef); isRef && !referenced {
			env[key] = nil
			return nil
		}
		if !through {
			env[key] = v // copy position: the references travel with the value
			return nil
		}
		rv, err := ctx.Materialize(v)
		if err != nil {
			return err
		}
		env[key] = rv
		return nil
	}
	if err := include("input", roots.Input, roots.Through.Input); err != nil {
		return nil, err
	}
	// `error` itself is always inline; only its `data` can be externalized, and Through.ErrorData
	// is what says the expression reads INTO the body rather than past it. Reading error.code
	// must not pay for a body it never asked for -- which resolveNested used to defeat by
	// materializing every child of the map it walked.
	if err := include("error", roots.Error, false); err != nil {
		return nil, err
	}
	if m, ok := env["error"].(map[string]any); ok && roots.Through.ErrorData {
		// Through the accessor, so a walk that has to step through `error` itself -- a whole
		// slot that was externalized -- resolves it rather than finding a marker where the map
		// should be. Direct map indexing cannot do that.
		d, err := ctx.MaterializeAt("error", "data")
		if err != nil {
			return nil, err
		}
		withData := make(map[string]any, len(m))
		for k, v := range m {
			withData[k] = v
		}
		withData["data"] = d
		env["error"] = withData
	}

	outsVal, err := ctx.At("outputs")
	if err != nil {
		return nil, err
	}
	outs, _ := outsVal.(map[string]any)
	// outputs.<this task> is the PREVIOUS output in every slot, including the switch — where
	// setTaskOutput has already overwritten the stored value. A task is not complete until its
	// switch has routed, and self.output is the name for what this run just produced.
	if sm, ok := self.(map[string]any); ok {
		if prev, has := sm["previous"]; has {
			shadowed := make(map[string]any, len(outs))
			maps.Copy(shadowed, outs)
			shadowed[inst.Task] = prev
			outs = shadowed
		}
	}
	refSet := make(map[string]struct{}, len(roots.Outputs))
	for _, id := range roots.Outputs {
		refSet[id] = struct{}{}
	}
	throughSet := make(map[string]struct{}, len(roots.Through.Outputs))
	for _, id := range roots.Through.Outputs {
		throughSet[id] = struct{}{}
	}
	envOuts := make(map[string]any, len(outs))
	for k, v := range outs {
		if _, isRef := v.(*model.ObjectRef); isRef && !roots.AllOutputs {
			if _, referenced := refSet[k]; !referenced {
				continue // unreferenced big output: don't load it
			}
		}
		_, through := throughSet[k]
		if !through && !roots.Through.AllOutputs {
			envOuts[k] = v
			continue
		}
		rv, err := ctx.Materialize(v)
		if err != nil {
			return nil, err
		}
		envOuts[k] = rv
	}
	env["outputs"] = envOuts
	return env, nil
}

// evalShape is the single runtime entry for every templated slot, resolving only the
// value-slots sh references (self is the task's self value, nil before the action). The
// same Shape drives registration checks, so the two phases cannot drift.
func (e *Engine) evalShape(inst *model.ProcessInstance, sh shape.Shape, self any) (any, error) {
	roots, err := sh.Roots()
	if err != nil {
		return nil, err
	}
	env, err := e.buildEnv(inst, self, roots)
	if err != nil {
		return nil, err
	}
	return sh.Eval(env)
}

func evalEnv(contextData, config map[string]any, self any) map[string]any {
	outputs, _ := contextData["outputs"].(map[string]any)
	if outputs == nil {
		outputs = map[string]any{}
	}
	if config == nil {
		config = map[string]any{}
	}
	env := map[string]any{
		"input":   contextData["input"],
		"outputs": outputs,
		"self":    self,
		"error":   contextData["error"],
		"config":  config,
	}
	return env
}

func evalAny(expression string, contextData, config map[string]any) (any, error) {
	t, err := tmpl.Get(expression)
	if err != nil {
		return nil, fmt.Errorf("param %q: %w", expression, err)
	}
	result, err := t.EvalAny(evalEnv(contextData, config, nil))
	if err != nil {
		return nil, fmt.Errorf("param %q: %w", expression, err)
	}
	return result, nil
}

func evalBool(expr string, contextData, config map[string]any, self any) (bool, error) {
	result, err := expression.Eval(expr, evalEnv(contextData, config, self))
	if err != nil {
		return false, fmt.Errorf("switch %q: %w", expr, err)
	}
	b, ok := result.(bool)
	if !ok {
		return false, fmt.Errorf("switch %q: expected bool, got %T", expr, result)
	}
	return b, nil
}

// maxInlineResolveBytes bounds what the engine will materialize into an outgoing request. A
// safety limit rather than a tuning knob, in the sense the 8 MiB response cap already
// established: past it the fix is to change what the definition sends, not to raise the number.
const maxInlineResolveBytes = 8 << 20

// resolveRefsInPlace materializes every reference inside v, for a consumer that cannot follow
// one. A fetch body is read by a remote server with no way to call genroc, so a reference
// reaching it is a value that never arrives -- unlike an external task's input, which a worker
// fetches for itself. specs/object-store.md §Resolution.
func (e *Engine) resolveRefsInPlace(inst *model.ProcessInstance, v any) (any, error) {
	var refs []*model.ObjectRef
	// On a COPY: Extract strips the markers, and v may be part of the live context.
	model.Extract(deepCopyValue(v), nil, &refs)
	if len(refs) == 0 {
		return v, nil
	}
	var total int64
	for _, r := range refs {
		total += r.Size
	}
	if total > maxInlineResolveBytes {
		return nil, fmt.Errorf("request body needs %d bytes of externalized values, over the %d-byte limit", total, maxInlineResolveBytes)
	}
	return e.context(inst).Materialize(v)
}

// deepCopyValue copies the containers of v so a traversal that strips markers cannot reach back
// into the live context. Leaves are shared: nothing here mutates one.
func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = deepCopyValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = deepCopyValue(val)
		}
		return out
	}
	return v
}
