package validation

import (
	"strings"

	"genroc/internal/model"
	"genroc/internal/schema"
)

// translateGuard rewrites a reference proved in the guarding task's frame into the frame the
// task it routes to will read it in, reporting false where the proof cannot travel.
// specs/guard-narrowing.md.
//
// Four rules, and only the last is counter-intuitive:
//
//   - `self.output.v` becomes `outputs.<guard task>.v`, and only where that task exports an
//     output at all — otherwise the name it would translate to does not exist downstream.
//   - `input.*` and `outputs.*` pass through: they name the same value in every frame.
//   - `self.result` / `self.previous` / `last_error` are dropped. They are the GUARDING
//     task's, and the target has its own values under those same names.
//   - `config` is dropped even though the name means the same thing everywhere: it is
//     re-resolved from the environment every tick and never persisted, so proving something
//     about it here proves nothing about the value the next task reads.
//
// A path the parser cannot read back is a computed key (`m[k]`), and it is dropped for a
// reason of its own: `k` is a different value in the target's frame, if it is there at all.
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

// edgeRefs is what taking switch case k of task s proves, in the frame of the task it routes
// to. Reaching case k means every earlier case was FALSE, and that negation is most of what
// makes the feature useful: the guard-clause shape — handle the bad case, fall through with no
// `case:` — gets all of its narrowing from it. specs/guard-narrowing.md.
func edgeRefs(s *model.Task, k int) refs {
	if k < 0 || k >= len(s.Switch) {
		return refs{}
	}
	out := refs{}
	add := func(facts []schema.GuardFact) {
		for _, f := range facts {
			state, ok := factState(f)
			if !ok {
				continue
			}
			path, ok := translateGuard(f.Path, s.ID, taskHasOutput(s))
			if !ok {
				continue
			}
			// A later fact about the same reference wins only by agreeing; two edges'
			// worth of disagreement is handled by the meet, but one case cannot prove a
			// reference is both null and not.
			if prev, seen := out[path]; seen && prev != state {
				delete(out, path)
				continue
			}
			out[path] = state
		}
	}
	for j := 0; j < k; j++ {
		if s.Switch[j].Case == "" {
			continue // an unguarded case is always true; nothing is proved by "not it"
		}
		if _, whenFalse, err := schema.GuardFacts(s.Switch[j].Case); err == nil {
			add(whenFalse)
		}
	}
	if c := s.Switch[k].Case; c != "" {
		if whenTrue, _, err := schema.GuardFacts(c); err == nil {
			add(whenTrue)
		}
	}
	return out
}

// meetRefs keeps only what BOTH sides prove, identically. A nil map is "not computed yet"
// (top) and yields the other side; two edges disagreeing about a reference leave it
// unrefined, which is the conservative direction.
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

// killOutput drops every fact about one task's output. Two callers, both load-bearing: a task
// is about to overwrite its own `outputs.<id>` when it is re-entered, and a task that FAILED
// produced no output at all, so an error edge carries nothing about it.
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

// computeRefinements is the dataflow half: what every task may assume on entry, given the
// edges that reach it. It rides the same predecessor graph as the output sets, converging
// downward from "everything" so a loop cannot admit a fact its back edge never proved.
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
				// An error edge adds nothing: the task failed, so its `case` never ran.
				// It needs no kill for the failing task's own output either — the set a
				// predecessor carries was already stripped of `outputs.<itself>` when it
				// was computed, which is the invariant the kill below maintains.
				edge := carried
				if !p.isErr {
					edge = unionRefs(edge, edgeRefs(tasks[p.idx], p.sw))
				}
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

// applyRefinements turns the symbolic facts into the narrowed types the inferrer consults.
// This is the only place a type is touched: resolvable carries the $defs pool the lookup
// needs, and a path this context does not have is skipped rather than invented.
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
			stripped := declared.StripNull()
			if stripped.IsZero() {
				continue
			}
			narrowed[path] = stripped
		case refExactlyNull:
			narrowed[path] = schema.Type("null")
		}
	}
	return ctx.WithGuards(narrowed)
}
