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
		return v, nil
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
