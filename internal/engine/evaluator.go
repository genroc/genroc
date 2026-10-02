package engine

import (
	"fmt"
	"maps"

	"genroc/internal/expression"
	"genroc/internal/model"
	"genroc/internal/shape"
	tmpl "genroc/internal/template"
)

// context NEVER writes a loaded value back: that destroys the markers the write path re-emits,
// so every later write re-marshals and re-hashes. specs/lazy-context.md.
func (e *Engine) context(inst *model.ProcessInstance) *model.Context {
	if inst.ResolvedObjects == nil {
		inst.ResolvedObjects = map[string]any{}
	}
	load := e.db.ObjectLoader()
	return model.NewContext(inst.State, func(hash string) (any, error) {
		// The retry is the engine's, not the loader's: a one-shot reader wants the plain error.
		return retryRead(func() (any, error) { return load(hash) })
	}, inst.ResolvedObjects)
}

// buildEnv resolves only what the expression reads: a slot it only COPIES keeps its references,
// so the value reaches the next write as the reference it already was. specs/lazy-context.md.
func (e *Engine) buildEnv(inst *model.ProcessInstance, self any, roots expression.Roots) (map[string]any, error) {
	ctx := e.context(inst)
	config := inst.Config
	if config == nil {
		config = map[string]any{}
	}
	env := map[string]any{"self": self, "config": config}

	// self.previous is outputs[<this task>], so it may reload as a marker: materialized only if
	// the expression reads into it.
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
	// Only an error namespace's `data` can be externalized, and reading a code must not pay for
	// a body it never asked for.
	for _, ns := range []struct {
		key           string
		read, through bool
	}{
		{model.StateError, roots.Error, roots.Through.ErrorData},
		{model.StateLastError, roots.LastError, roots.Through.LastErrorData},
	} {
		if err := include(ns.key, ns.read, false); err != nil {
			return nil, err
		}
		m, ok := env[ns.key].(map[string]any)
		if !ok || !ns.through {
			continue
		}
		// Through the accessor, not map indexing: a wholly externalized namespace must resolve
		// rather than leave a marker where the map should be.
		d, err := ctx.MaterializeAt(ns.key, "data")
		if err != nil {
			return nil, err
		}
		withData := maps.Clone(m)
		withData["data"] = d
		env[ns.key] = withData
	}

	outsVal, err := ctx.At("outputs")
	if err != nil {
		return nil, err
	}
	outs, _ := outsVal.(map[string]any)
	// outputs.<this task> is the PREVIOUS output everywhere, the switch included, where
	// setTaskOutput has already overwritten it. CLAUDE.md.
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

// evalShape is the single runtime entry for every templated slot; the same Shape drives
// registration, so the two phases cannot drift.
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
		"input":              contextData["input"],
		"outputs":            outputs,
		"self":               self,
		model.StateError:     contextData[model.StateError],
		model.StateLastError: contextData[model.StateLastError],
		"config":             config,
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

// maxInlineResolveBytes is a safety limit, not a tuning knob: past it, change what the
// definition sends.
const maxInlineResolveBytes = 8 << 20

// resolveRefsInPlace is for a consumer that cannot follow a reference, such as a fetch body's
// remote server. specs/object-store.md §Resolution.
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
