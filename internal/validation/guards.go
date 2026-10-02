package validation

import (
	"strings"

	"genroc/internal/model"
	"genroc/internal/schema"
)

// translateGuard moves a proved reference into the target task's frame, or reports false; the
// rules are specs/guard-narrowing.md's (`config` never travels). An unparseable path is a computed
// key (`m[k]`), dropped too: `k` is a different value in the target's frame.
func translateGuard(path, guardTask string, exportsOutput bool) (string, bool) {
	segs, err := schema.ParsePath(path)
	if err != nil || len(segs) == 0 || segs[0].IsIndex {
		return "", false
	}
	switch segs[0].Name {
	case "input", "outputs":
		return path, true
	case "self":
		if !exportsOutput || len(segs) < 2 || segs[1].IsIndex || segs[1].Name != "output" {
			return "", false
		}
		out := schema.JoinPath(schema.JoinPath("", "outputs"), guardTask)
		for _, s := range segs[2:] {
			if s.IsIndex {
				out = schema.JoinIndex(out, s.Index)
			} else {
				out = schema.JoinPath(out, s.Name)
			}
		}
		return out, true
	}
	return "", false
}

// refState is what an edge proved about one reference. Three states with `unrefined` as the
// absence of a key, so the lattice is finite and the fixpoint terminates on its own.
type refState uint8

const (
	refNonNull refState = iota + 1
	refExactlyNull
)

// refs maps a rendered access path, in the READING task's frame, to what is known about it.
type refs map[string]refState

// factState reads one catalogue fact as a refinement, or reports that it proves nothing:
// knowing a value is not one particular NON-null literal says nothing about its type.
func factState(f schema.GuardFact) (refState, bool) {
	switch {
	case f.IsNull && f.Equal:
		return refExactlyNull, true
	case f.IsNull:
		return refNonNull, true
	case f.Equal:
		return refNonNull, true
	}
	return 0, false
}

// clause is a switch case or an `on_error` rule: a guard, plus whether its FALSITY may be read. A
// rule naming a code proves nothing by not firing (`matchOnErrorWith`). specs/guard-narrowing.md.
type clause struct {
	cond    string
	negates bool
}

func switchClauses(t *model.Task) []clause {
	out := make([]clause, len(t.Switch))
	for i, c := range t.Switch {
		// An unguarded case is always true, and `factsOf` answers nothing for it either way.
		out[i] = clause{cond: c.Case, negates: true}
	}
	return out
}

func ruleClauses(t *model.Task) []clause {
	out := make([]clause, len(t.OnError))
	for i, ec := range t.OnError {
		out[i] = clause{cond: ec.Case, negates: len(ec.Code) == 0}
	}
	return out
}

// clauseFacts: reaching clause k proves every earlier readable clause false and, where `own`, k
// true — which the guard's own expression never may assume. `frame` may refuse a reference.
func clauseFacts(cs []clause, k int, own bool, frame func(string) (string, bool)) refs {
	out := refs{}
	if k < 0 || k >= len(cs) {
		return out
	}
	for j := 0; j < k; j++ {
		if cs[j].negates {
			out.addFacts(factsOf(cs[j].cond, false), frame)
		}
	}
	if own {
		out.addFacts(factsOf(cs[k].cond, true), frame)
	}
	return out
}

// factsOf reads one guard on one branch. An absent or unparseable expression proves nothing,
// which is the same answer either way.
func factsOf(cond string, whenTrue bool) []schema.GuardFact {
	if cond == "" {
		return nil
	}
	yes, no, err := schema.GuardFacts(cond)
	if err != nil {
		return nil
	}
	if whenTrue {
		return yes
	}
	return no
}

// A reference one predicate proves both null and non-null is dropped, not picked: the predicate
// cannot hold, and either answer would be a guess.
func (r refs) addFacts(facts []schema.GuardFact, frame func(string) (string, bool)) {
	for _, f := range facts {
		state, ok := factState(f)
		if !ok {
			continue
		}
		path, ok := frame(f.Path)
		if !ok {
			continue
		}
		if prev, seen := r[path]; seen && prev != state {
			delete(r, path)
			continue
		}
		r[path] = state
	}
}

// sameFrame drops nothing — `config` included, since one `evalSwitch` pass reads one resolved
// value.
func sameFrame(path string) (string, bool) { return path, true }

// edgeRefs is what taking switch case k proves, in the frame of the task it routes to.
func edgeRefs(t *model.Task, k int) refs {
	return clauseFacts(switchClauses(t), k, true, func(path string) (string, bool) {
		return translateGuard(path, t.ID, taskHasOutput(t))
	})
}

// ruleEdgeRefs is edgeRefs for an `on_error` goto: the task FAILED, so it exported no output and
// nothing under `self` has a downstream name.
func ruleEdgeRefs(t *model.Task, k int) refs {
	return clauseFacts(ruleClauses(t), k, true, func(path string) (string, bool) {
		return translateGuard(path, t.ID, false)
	})
}

// meetRefs keeps only what BOTH sides prove, identically. A nil map is top ("not computed yet")
// and yields the other side.
func meetRefs(a, b refs) refs {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := make(refs, len(a))
	for k, v := range a {
		if w, ok := b[k]; ok && w == v {
			out[k] = v
		}
	}
	return out
}

// Two callers, both load-bearing: a re-entered task overwrites its own `outputs.<id>`, and a
// failed one produced none.
func killOutput(in refs, taskID string) refs {
	prefix := schema.JoinPath(schema.JoinPath("", "outputs"), taskID)
	out := make(refs, len(in))
	for k, v := range in {
		if k == prefix || strings.HasPrefix(k, prefix+".") || strings.HasPrefix(k, prefix+"[") {
			continue
		}
		out[k] = v
	}
	return out
}

// computeRefinements converges downward from "everything", so a loop cannot admit a fact its
// back edge never proved.
func computeRefinements(tasks []*model.Task) map[string]refs {
	n := len(tasks)
	out := make(map[string]refs, n)
	if n == 0 {
		return out
	}
	preds := buildPreds(tasks)
	state := make([]refs, n) // nil = top, not yet computed
	for {
		changed := false
		for i, s := range tasks {
			var in refs
			for _, p := range preds[i] {
				if p.idx == -1 {
					in = meetRefs(in, refs{}) // the process start proves nothing
					continue
				}
				carried := state[p.idx]
				if carried == nil {
					continue // predecessor still top; it constrains nothing yet
				}
				// No kill for the failing task's own output: the carried set was stripped of it
				// when it was computed.
				proved := edgeRefs(tasks[p.idx], p.sw)
				if p.isErr {
					proved = ruleEdgeRefs(tasks[p.idx], p.rule)
				}
				edge := unionRefs(carried, proved)
				in = meetRefs(in, edge)
			}
			if in == nil {
				continue
			}
			in = killOutput(in, s.ID)
			if !sameRefs(state[i], in) {
				state[i] = in
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	for i, s := range tasks {
		if state[i] != nil {
			out[s.ID] = state[i]
		}
	}
	return out
}

// unionRefs layers what an edge proves on top of what already held, the edge winning: it was
// evaluated later, against the same values.
func unionRefs(base, edge refs) refs {
	out := make(refs, len(base)+len(edge))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range edge {
		out[k] = v
	}
	return out
}

func sameRefs(a, b refs) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// The only place a fact touches a type. resolvable carries the $defs pool; a path this context
// lacks is skipped, not invented.
func applyRefinements(ctx, resolvable schema.Schema, r refs) schema.Schema {
	if len(r) == 0 {
		return ctx
	}
	narrowed := make(map[string]schema.Schema, len(r))
	for path, state := range r {
		declared, err := resolvable.At(path)
		if err != nil {
			continue
		}
		switch state {
		case refNonNull:
			// Materialized: a path landing ON a `$ref` hides its null inside the target, which is
			// every guard on a whole task output.
			stripped := declared.StripNull()
			if stripped.IsZero() || stripped.HasNull() {
				continue
			}
			narrowed[path] = stripped
		case refExactlyNull:
			narrowed[path] = schema.Type("null")
		}
	}
	return ctx.WithGuards(narrowed)
}
