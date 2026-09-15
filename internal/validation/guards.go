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

// clause is a switch case and an `on_error` rule seen as ONE thing: a guard, plus whether its
// FALSITY may be read. That is the only way the two differ. A rule's predicate is
// `(code == a || code == b) && case`, so a rule naming a code proves nothing by not firing —
// the negation of a conjunction is a fact about neither half, since it may have been skipped on
// the code before its `case` ever ran (`matchOnErrorWith`). Both prove their guard when they DO
// fire. specs/guard-narrowing.md.
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

// clauseFacts is what reaching clause k establishes: every earlier clause that CAN be read
// failed, and — where `own` — this one held. The negation is most of what makes the feature
// useful (the guard-clause shape, handle the bad case and fall through, gets all its narrowing
// from it); `own` is what an outgoing edge and a clause's siblings may assume, and what the
// guard's own expression never may, being what proves it. `frame` decides which frame the facts
// are read in, and may refuse one.
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

// addFacts folds catalogue facts into a set, dropping what proves nothing and what the frame
// refuses. A reference one predicate proves both null and non-null is dropped rather than
// picked: the predicate cannot hold, and either answer would be a guess.
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

// sameFrame reads a guard in the frame it was written in: nothing is translated and nothing is
// dropped, which is what every name still in scope one slot later needs — `config` included,
// since one `evalSwitch` pass reads one resolved value.
func sameFrame(path string) (string, bool) { return path, true }

// edgeRefs is what taking switch case k proves, in the frame of the task it routes to.
func edgeRefs(t *model.Task, k int) refs {
	return clauseFacts(switchClauses(t), k, true, func(path string) (string, bool) {
		return translateGuard(path, t.ID, taskHasOutput(t))
	})
}

// ruleEdgeRefs is edgeRefs for an `on_error` rule's `goto`. The task FAILED, so it exported no
// output and nothing under `self` has a downstream name — which is the whole of the difference,
// and translateGuard's `exportsOutput` says it.
func ruleEdgeRefs(t *model.Task, k int) refs {
	return clauseFacts(ruleClauses(t), k, true, func(path string) (string, bool) {
		return translateGuard(path, t.ID, false)
	})
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
				// What held on entry to the predecessor still holds — a proof about
				// `input.x` does not stop being true because a task ran. On top of it, what
				// the clause that selected this edge proved: a switch case when it matched,
				// an on_error rule when it caught. Neither needs a kill for the failing
				// task's own output, because the set a predecessor carries was already
				// stripped of `outputs.<itself>` when it was computed — the invariant the
				// kill below maintains.
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
			// Materialized: a path landing exactly ON a `$ref` hides its null inside the
			// target, which is every guard on a whole task output — an output is carried as
			// a ref by construction.
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
