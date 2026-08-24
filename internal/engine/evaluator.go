package engine

import (
	"context"
	"fmt"

	"genroc/internal/expression"
	"genroc/internal/model"
	"genroc/internal/shape"
	tmpl "genroc/internal/template"
)

// resolveValue returns v as-is unless it is an *model.ObjectRef marker (an externalized,
// not-yet-loaded value), which it loads from the store and memoises on the instance for
// the rest of the advance. inst must be the instance that OWNS the value (e.g. a child
// for its own output).
func (e *Engine) resolveValue(inst *model.ProcessInstance, v any) (any, error) {
	ref, ok := v.(*model.ObjectRef)
	if !ok {
		// A slot can be cut in several places rather than wholly externalized, so a value that
		// is not itself a marker may still contain them. Resolving the whole slot once it is
		// read keeps the semantics a whole-slot ref always had: laziness is per SLOT, and a slot
		// nothing reads is never loaded.
		return e.resolveNested(inst, v)
	}
	if cached, ok := inst.ResolvedObjects[ref.Ref]; ok {
		return cached, nil
	}
	val, err := e.db.ResolveObject(context.Background(), ref)
	if err != nil {
		return nil, err
	}
	if inst.ResolvedObjects == nil {
		inst.ResolvedObjects = map[string]any{}
	}
	inst.ResolvedObjects[ref.Ref] = val
	return val, nil
}

// buildEnv assembles the expression environment for inst, resolving only the externalized
// value-slots the expression reads (per roots). A small inline value is always included; a
// big externalized value (an *model.ObjectRef marker) is loaded only when referenced —
// the slot-level lazy load.
func (e *Engine) buildEnv(inst *model.ProcessInstance, self any, roots expression.Roots) (map[string]any, error) {
	config := inst.Config
	if config == nil {
		config = map[string]any{}
	}
	env := map[string]any{"self": self, "config": config}

	// self.previous is this task's own prior output — the same value as outputs[<this
	// task>], so when that output was externalized it reloads as an *ObjectRef marker.
	// Resolve it just like an outputs.<id> ref (lazily — only when the expression reads
	// it), otherwise self.previous.<field> would read through the marker and yield null.
	if roots.SelfPrevious {
		if sm, ok := self.(map[string]any); ok {
			prev, err := e.resolveValue(inst, sm["previous"])
			if err != nil {
				return nil, err
			}
			selfCopy := make(map[string]any, len(sm))
			for k, v := range sm {
				selfCopy[k] = v
			}
			selfCopy["previous"] = prev
			env["self"] = selfCopy
		}
	}

	include := func(key string, referenced bool) error {
		v := inst.ContextData[key]
		if _, isRef := v.(*model.ObjectRef); isRef && !referenced {
			env[key] = nil
			return nil
		}
		rv, err := e.resolveValue(inst, v)
		if err != nil {
			return err
		}
		env[key] = rv
		return nil
	}
	if err := include("input", roots.Input); err != nil {
		return nil, err
	}
	if err := include("error", roots.Error); err != nil {
		return nil, err
	}
	// `error` itself is always inline; only its `data` can be an externalized marker, and it
	// is loaded only where the expression actually reads it. Reading error.code must not pay
	// for a body it never asked for.
	if m, ok := env["error"].(map[string]any); ok && roots.ErrorData {
		if d, hasData := m["data"]; hasData {
			rv, err := e.resolveValue(inst, d)
			if err != nil {
				return nil, err
			}
			withData := make(map[string]any, len(m))
			for k, v := range m {
				withData[k] = v
			}
			withData["data"] = rv
			env["error"] = withData
		}
	}

	outs, _ := inst.ContextData["outputs"].(map[string]any)
	refSet := make(map[string]struct{}, len(roots.Outputs))
	for _, id := range roots.Outputs {
		refSet[id] = struct{}{}
	}
	envOuts := make(map[string]any, len(outs))
	for k, v := range outs {
		if _, isRef := v.(*model.ObjectRef); isRef && !roots.AllOutputs {
			if _, referenced := refSet[k]; !referenced {
				continue // unreferenced big output: don't load it
			}
		}
		rv, err := e.resolveValue(inst, v)
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

// ResolveRefsInPlace materializes every *ObjectRef inside v, for a consumer that cannot follow
// one. A fetch body is read by a remote server with no way to call genroc, so a reference
// reaching it is a value that never arrives -- unlike an external task's input, which a worker
// fetches for itself. specs/object-store.md §Resolution.
func (e *Engine) resolveRefsInPlace(inst *model.ProcessInstance, v any) (any, error) {
	var refs []*model.ObjectRef
	stripped := model.Extract(v, nil, &refs)
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
	for _, r := range refs {
		val, err := e.resolveValue(inst, &model.ObjectRef{Ref: r.Ref, Size: r.Size})
		if err != nil {
			return nil, err
		}
		if len(r.Path) == 0 {
			return val, nil
		}
		if !model.Place(stripped, r.Path, val) {
			return nil, fmt.Errorf("resolved value for %s has nowhere to go", r.Ref)
		}
	}
	return stripped, nil
}

// resolveNested materializes every marker inside a partially externalized slot.
func (e *Engine) resolveNested(inst *model.ProcessInstance, v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			rv, err := e.resolveValue(inst, val)
			if err != nil {
				return nil, err
			}
			t[k] = rv
		}
		return t, nil
	case []any:
		for i, val := range t {
			rv, err := e.resolveValue(inst, val)
			if err != nil {
				return nil, err
			}
			t[i] = rv
		}
		return t, nil
	}
	return v, nil
}
