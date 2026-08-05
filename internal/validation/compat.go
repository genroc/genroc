package validation

import (
	"fmt"
	"sort"
	"strings"

	"genroc/internal/model"
	"genroc/internal/schema"
)

// Side names which of the two compared sets a finding came from. A cross-document
// finding (the parent/child pairing) or an unanalysable version is a fact about one
// side, and the versions are the caller's selectors — so the side is named here and
// resolved to a version above.
type Side string

const (
	SideFrom Side = "from"
	SideTo   Side = "to"
)

// SlotVerdict is one comparison's answer, with the reason it failed.
type SlotVerdict struct {
	Compatible bool   `json:"compatible"`
	Reason     string `json:"reason,omitempty"`
}

// TaskVerdict is one task's continuation answer plus the slots that differ. Changed is
// reported whether or not the task is compatible: the verdict is blind to meaning, so
// the slot list is what lets a reader apply the judgement the machine cannot.
type TaskVerdict struct {
	Task       string   `json:"task"`
	Compatible bool     `json:"compatible"`
	Reason     string   `json:"reason,omitempty"`
	Changed    []string `json:"changed,omitempty"`
}

// Report is Compare's verdict for one process.
type Report struct {
	Name string `json:"name"`
	// Compatible is instance continuation: every context the old definition can present
	// at a task is one the new definition accepts there, plus the input contract, plus
	// no task disappearing under an instance.
	Compatible bool `json:"compatible"`
	// OutputCompatible is the consumer contract, and it runs the other way round. It is
	// never folded into Compatible: a single boolean over two opposite-direction checks
	// would be meaningless.
	OutputCompatible bool   `json:"output_compatible"`
	OutputReason     string `json:"output_reason,omitempty"`
	// Input is hoisted out of the per-task loop — it sits in every task's context, so
	// comparing it there would report the same break once per task.
	Input SlotVerdict   `json:"input"`
	Tasks []TaskVerdict `json:"tasks"`
	// RemovedTasks are ids the old definition has and the new one does not. Listed
	// separately because an instance sitting on one has nowhere to go, and because a
	// rename reads as one removal plus one addition.
	RemovedTasks []string `json:"removed_tasks,omitempty"`
	AddedTasks   []string `json:"added_tasks,omitempty"`
	// Changed names the definition-level slots that differ. They are not symmetry with
	// the per-task list: input_schema and config_schema belong to no task and would
	// otherwise have no entry at all, and a schema changing nothing but `secret: true`
	// compares equal to every verdict here, so this is the only thing that reports it.
	Changed []string `json:"changed,omitempty"`
}

// ChildPairVerdict is one row of the parent/child pairing check: whichever of the two
// moved, does its data still fit the side that did not. ParentSide says which document
// the parent was taken from, which is what distinguishes the two mixed rows.
type ChildPairVerdict struct {
	Parent     string `json:"parent"`
	ParentSide Side   `json:"parent_side"`
	Task       string `json:"task"`
	ChildKey   string `json:"child_key,omitempty"`
	Child      string `json:"child"`
	Compatible bool   `json:"compatible"`
	Reason     string `json:"reason,omitempty"`
}

// Unanalysable is a version whose own inference failed. Old rows were validated under
// the rules of their day, so Generate can fail on one — that is a per-version verdict,
// not a failure of the whole report.
type Unanalysable struct {
	Name   string `json:"name"`
	Side   Side   `json:"side"`
	Reason string `json:"reason"`
}

// Unpaired is a process named on one side only: there is nothing to compare it against,
// and dropping it would let silence read as agreement.
type Unpaired struct {
	Name string `json:"name"`
	Side Side   `json:"side"`
}

// SetReport is CompareSet's whole answer. Compatible is the conjunction over everything
// below it, and an unanalysable or unpaired entry makes it false, never true — a
// top-level answer indistinguishable from "checked, and fine" is worse than no report.
type SetReport struct {
	Compatible   bool               `json:"compatible"`
	Processes    []Report           `json:"processes"`
	Children     []ChildPairVerdict `json:"children,omitempty"`
	Unanalysable []Unanalysable     `json:"unanalysable,omitempty"`
	Unpaired     []Unpaired         `json:"unpaired,omitempty"`
}

// TaskContexts returns the context schema at every task of def, keyed by task id: the
// state an instance sitting there holds on entry, as one object shaped like the row —
// `input`, `outputs.<id>` per task that projects an output, and `error` — with each
// property required where the value is guaranteed and optional where it is merely
// possible.
//
// The "config" property is stripped. It is re-resolved from the environment on every
// tick, so nothing persisted corresponds to it.
func TaskContexts(def *model.ProcessDefinition) (map[string]schema.Schema, error) {
	sf, err := Generate(def)
	if err != nil {
		return nil, err
	}
	return taskContexts(def, sf, sf.ProcessInput), nil
}

// taskContexts is TaskContexts with the input slot supplied by the caller: Compare
// passes the zero schema, having hoisted `input` out of the per-task loop.
func taskContexts(def *model.ProcessDefinition, sf SchemaFile, input schema.Schema) map[string]schema.Schema {
	required, optional, mustErr, mayErr := computeContextSets(def.Tasks)
	out := make(map[string]schema.Schema, len(def.Tasks))
	for _, t := range def.Tasks {
		out[t.ID] = contextSchema(required[t.ID], optional[t.ID], sf.Tasks, input,
			schema.Schema{}, mustErr[t.ID], mayErr[t.ID]).WithDefs(sf.Defs)
	}
	return out
}

// analysis is one definition's inferred view, computed once per side so a set
// comparison infers each version once rather than once per pair it appears in.
type analysis struct {
	def      *model.ProcessDefinition
	sf       SchemaFile
	contexts map[string]schema.Schema // per task; input and config hoisted out
}

func analyze(def *model.ProcessDefinition) (analysis, error) {
	sf, err := Generate(def)
	if err != nil {
		return analysis{}, err
	}
	return analysis{def: def, sf: sf, contexts: taskContexts(def, sf, schema.Schema{})}, nil
}

// Compare answers, for two versions of one process, whether an instance running the old
// one could continue under the new one — every context the old definition can present at
// a task is one the new definition accepts there — and separately whether the new version
// still honours the output contract its consumers were written against.
//
// It is a shape check: it compares inferred schemas, not meaning. Dollars → cents is
// `number` before and after and comes back compatible. The value is catching the
// accidental break, not certifying a migration; specs/version-compatibility.md §7 is the
// full list of what it cannot see.
//
// It reads two documents and never sees an instance, so it must assume every reachable
// state and report a structural difference wherever one exists. The upgrade gate refines
// that monotonically — never the reverse — with presence taken from the row.
func Compare(old, new *model.ProcessDefinition) (Report, error) {
	oldA, err := analyze(old)
	if err != nil {
		return Report{}, fmt.Errorf("analyse %q (from): %w", old.Name, err)
	}
	newA, err := analyze(new)
	if err != nil {
		return Report{}, fmt.Errorf("analyse %q (to): %w", new.Name, err)
	}
	return compare(oldA, newA), nil
}

func compare(oldA, newA analysis) Report {
	r := Report{
		Name:    newA.def.Name,
		Changed: changedDefinitionSlots(oldA.def, newA.def),
		Input:   compareInput(oldA, newA),
	}
	r.Compatible = r.Input.Compatible

	newTasks := tasksByID(newA.def)
	oldTasks := tasksByID(oldA.def)
	for _, t := range oldA.def.Tasks {
		nt, ok := newTasks[t.ID]
		if !ok {
			r.RemovedTasks = append(r.RemovedTasks, t.ID)
			r.Compatible = false
			continue
		}
		v := TaskVerdict{Task: t.ID, Compatible: true, Changed: changedTaskSlots(t, nt)}
		// One context per task is enough for the whole remaining run: a task output's
		// type is position-independent, and the must-analysis is monotone along a path,
		// so anything reachable from here is satisfied by what was checked here plus what
		// the new definition produces on the way. See the spec's §2.
		if reason := forwardExplainer.explain("", oldA.contexts[t.ID], newA.contexts[t.ID], 0); reason != "" {
			v.Compatible, v.Reason = false, reason
			r.Compatible = false
		}
		r.Tasks = append(r.Tasks, v)
	}
	for _, t := range newA.def.Tasks {
		if _, ok := oldTasks[t.ID]; !ok {
			r.AddedTasks = append(r.AddedTasks, t.ID)
		}
	}

	r.OutputCompatible, r.OutputReason = compareOutput(oldA, newA)
	return r
}

// compareInput checks the one slot hoisted out of the per-task loop. Both sides are
// wrapped in a one-property object so isSubset answers both halves with no second code
// path: a newly required input (the property is in the new version's required set and not
// the old's) and an input whose type changed.
func compareInput(oldA, newA analysis) SlotVerdict {
	wrap := func(a analysis) schema.Schema {
		o := schema.Object()
		if !a.sf.ProcessInput.IsZero() {
			o = o.WithProperty("input", a.sf.ProcessInput, true)
		}
		return o.WithDefs(a.sf.Defs)
	}
	if reason := forwardExplainer.explain("", wrap(oldA), wrap(newA), 0); reason != "" {
		return SlotVerdict{Reason: reason}
	}
	return SlotVerdict{Compatible: true}
}

// compareOutput is the consumer contract, reversed: a parent's result_schema and an API
// caller were written against the old output shape, so every value the new version can
// produce must be one the old could. Skipped when either version declares no process
// output — there is no contract to break.
//
// IsSubset, not the NarrowsTo used for a child's result_schema. That is the privilege of
// a slot where a runtime conform stands behind the claim; here two inferred types are
// compared with nothing conforming either.
func compareOutput(oldA, newA analysis) (bool, string) {
	oldOut, hasOld, err := schemaFileOutput(oldA.sf)
	if err != nil {
		return false, err.Error()
	}
	newOut, hasNew, err := schemaFileOutput(newA.sf)
	if err != nil {
		return false, err.Error()
	}
	if !hasOld || !hasNew {
		return true, ""
	}
	if reason := reverseExplainer.explain("output", newOut, oldOut, 0); reason != "" {
		return false, reason
	}
	return true, ""
}

// CompareSet is Compare over a name-paired set, plus the parent/child pairing check that
// no single-process comparison can compute: it needs old[parent] with new[child] AND
// new[parent] with old[child] in one frame, which is why the set form exists rather than
// being left to the client. A single pair is CompareSet with one entry.
//
// Nothing is dropped. A version that no longer analyses is reported unanalysable, and a
// name present on one side only is reported unpaired; both make the roll-up false.
func CompareSet(old, new map[string]*model.ProcessDefinition) (SetReport, error) {
	report := SetReport{Compatible: true, Processes: []Report{}}

	analysed := map[Side]map[string]analysis{SideFrom: {}, SideTo: {}}
	for _, side := range []struct {
		defs map[string]*model.ProcessDefinition
		side Side
	}{{old, SideFrom}, {new, SideTo}} {
		for _, name := range sortedNames(side.defs) {
			a, err := analyze(side.defs[name])
			if err != nil {
				report.Unanalysable = append(report.Unanalysable,
					Unanalysable{Name: name, Side: side.side, Reason: err.Error()})
				report.Compatible = false
				continue
			}
			analysed[side.side][name] = a
		}
	}

	for _, name := range sortedNames(old) {
		if _, ok := new[name]; !ok {
			report.Unpaired = append(report.Unpaired, Unpaired{Name: name, Side: SideFrom})
			report.Compatible = false
		}
	}
	for _, name := range sortedNames(new) {
		if _, ok := old[name]; !ok {
			report.Unpaired = append(report.Unpaired, Unpaired{Name: name, Side: SideTo})
			report.Compatible = false
		}
	}

	for _, name := range sortedNames(new) {
		o, okOld := analysed[SideFrom][name]
		n, okNew := analysed[SideTo][name]
		if !okOld || !okNew {
			continue // unpaired or unanalysable, and already reported as such
		}
		r := compare(o, n)
		if !r.Compatible || !r.OutputCompatible {
			report.Compatible = false
		}
		report.Processes = append(report.Processes, r)
	}

	report.Children = childPairVerdicts(analysed[SideFrom], analysed[SideTo])
	for _, c := range report.Children {
		if !c.Compatible {
			report.Compatible = false
		}
	}
	return report, nil
}

// childPairVerdicts is the cross-document half. One schema governs both steps of a child
// call — the child produces its output, and collect conforms that against the parent's
// result_schema as the parent currently stands, which is also the type the parent reads it
// at. So the constraint is outC.NarrowsTo(S_parent), NarrowsTo because that conform is
// real, matching checkChildOutputType.
//
// The case where both sides move needs no row here: applied together, buildResolvedDeps
// bakes the new parent onto the new child and ValidateChildProcessRefs checks exactly
// this. What is left is the pair where one moved and the other did not, and the symmetry
// of the two rows is the sign the model is right — a pair is compatible when whichever
// side moved still fits the one that did not.
//
// Scope is bounded by what was submitted: a child whose counterpart is not in the set is
// skipped, and covered instead by the gate, from its parent row's pinned version. A
// self-reference is the same check with one process, and is single-level, so unlike
// topoSort it needs no cycle guard.
func childPairVerdicts(oldA, newA map[string]analysis) []ChildPairVerdict {
	var out []ChildPairVerdict
	for _, parent := range sortedAnalysisNames(newA) {
		oldParent, ok := oldA[parent]
		if !ok {
			continue
		}
		oldTasks := tasksByID(oldParent.def)
		for _, nt := range newA[parent].def.Tasks {
			ot, ok := oldTasks[nt.ID]
			if !ok {
				continue // a task with no counterpart has no in-flight batch to pair
			}
			oldSlots := childSlotsByKey(ot)
			for _, ns := range childSlots(nt) {
				os, ok := oldSlots[ns.key]
				// A slot whose child name changed is a different call entirely; the
				// changed-slot list reports it and there is no pair to check.
				if !ok || os.name != ns.name {
					continue
				}
				row := func(parentSide Side, parentSchema *schema.Schema, childSide map[string]analysis) {
					child, ok := childSide[ns.name]
					if !ok || parentSchema == nil {
						return
					}
					childOut, hasOut, err := schemaFileOutput(child.sf)
					if err != nil || !hasOut {
						return
					}
					v := ChildPairVerdict{
						Parent: parent, ParentSide: parentSide, Task: nt.ID,
						ChildKey: ns.key, Child: ns.name, Compatible: true,
					}
					// parentSchema is used bare: Normalize leaves every result_schema
					// self-contained, its shared definitions baked into its own root
					// $defs — and WithDefs REPLACES a root pool rather than merging, so
					// attaching the child's here would silently strip the parent's.
					if reason := narrowExplainer.explain("output", childOut, *parentSchema, 0); reason != "" {
						v.Compatible, v.Reason = false, reason
					}
					out = append(out, v)
				}
				row(SideFrom, os.schema, newA) // only the child moved
				row(SideTo, ns.schema, oldA)   // only the parent moved
			}
		}
	}
	return out
}

// childSlot is one child call a task makes: the key it is reached by ("" for a single
// child or a list fan-out), the process it names, and the result_schema the parent
// declares for it.
type childSlot struct {
	key    string
	name   string
	schema *schema.Schema
}

func childSlots(t *model.Task) []childSlot {
	if t.Action == nil {
		return nil
	}
	switch t.Action.Type {
	case model.ActionTypeChild, model.ActionTypeChildList:
		return []childSlot{{name: t.Action.Name, schema: t.Action.ResultSchema}}
	case model.ActionTypeChildMap:
		keys := make([]string, 0, len(t.Action.Children))
		for k := range t.Action.Children {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]childSlot, 0, len(keys))
		for _, k := range keys {
			e := t.Action.Children[k]
			out = append(out, childSlot{key: k, name: e.Name, schema: e.ResultSchema})
		}
		return out
	}
	return nil
}

func childSlotsByKey(t *model.Task) map[string]childSlot {
	out := map[string]childSlot{}
	for _, s := range childSlots(t) {
		out[s.key] = s
	}
	return out
}

func tasksByID(def *model.ProcessDefinition) map[string]*model.Task {
	out := make(map[string]*model.Task, len(def.Tasks))
	for _, t := range def.Tasks {
		out[t.ID] = t
	}
	return out
}

func sortedNames(m map[string]*model.ProcessDefinition) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedAnalysisNames(m map[string]analysis) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── diagnostics ───────────────────────────────────────────────────────────────

// explainer names the first place one schema fails to fit another, decomposing slot by
// slot and then into properties within the failing slot.
//
// It decomposes ABOVE the subset check rather than threading an error through it:
// isSubset returns a bool and sits on every hot validation path, so making it explain
// itself would be a large change to load-bearing code for one caller.
type explainer struct {
	// narrow runs the check as NarrowsTo — an unknown in the sub position is accepted,
	// sound only where a runtime conform stands behind it (a child's result_schema).
	narrow bool
	// swap renders the arrow old → new even when the check runs new ⊆ old. The reader is
	// asking what they changed, not which direction the subset ran in.
	swap bool
}

var (
	forwardExplainer = explainer{}
	reverseExplainer = explainer{swap: true}
	narrowExplainer  = explainer{narrow: true}
)

// maxExplainDepth bounds the descent: a recursive schema has no bottom to reach, and one
// level into a failing slot is already the useful part.
const maxExplainDepth = 4

func (e explainer) fits(sub, super schema.Schema) bool {
	if e.narrow {
		return sub.NarrowsTo(super)
	}
	return sub.IsSubset(super)
}

func (e explainer) explain(path string, sub, super schema.Schema, depth int) string {
	if e.fits(sub, super) {
		return ""
	}
	if depth < maxExplainDepth {
		sub, super := deref(sub), deref(super)

		subRequired := make(map[string]bool, len(sub.Required()))
		for _, f := range sub.Required() {
			subRequired[f] = true
		}
		superRequired := append([]string(nil), super.Required()...)
		sort.Strings(superRequired)
		for _, f := range superRequired {
			if !subRequired[f] {
				return joinPath(path, f) + ": newly required"
			}
		}

		subProps, superProps := sub.Properties(), super.Properties()
		names := make([]string, 0, len(superProps))
		for name := range superProps {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			sp, ok := subProps[name]
			if !ok {
				continue // an undeclared property imposes nothing; isSubset skips it too
			}
			if msg := e.explain(joinPath(path, name), sp, superProps[name], depth+1); msg != "" {
				return msg
			}
		}

		if sub.HasItems() && super.HasItems() {
			if msg := e.explain(path+"[]", sub.Items(), super.Items(), depth+1); msg != "" {
				return msg
			}
		}
	}
	from, to := describe(sub), describe(super)
	if e.swap {
		from, to = to, from
	}
	if path == "" {
		return fmt.Sprintf("%s is not accepted where %s is expected", from, to)
	}
	return fmt.Sprintf("%s: %s → %s", path, from, to)
}

func joinPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func deref(s schema.Schema) schema.Schema {
	if r, err := s.Resolve(); err == nil {
		return r
	}
	return s
}

// describe renders a schema as a short type name for a diagnostic. It is not a
// serialization: two different schemas may describe identically, which is why the path
// carries the weight and this only says what kind of thing sits there.
func describe(s schema.Schema) string { return describeDepth(s, 0) }

func describeDepth(s schema.Schema, depth int) string {
	if s.IsZero() {
		return "absent"
	}
	s = deref(s)
	if types := s.Type(); len(types) > 0 {
		if types.Contains("array") && s.HasItems() && depth < 2 {
			return "array<" + describeDepth(s.Items(), depth+1) + ">"
		}
		return strings.Join([]string(types), "|")
	}
	switch {
	case s.HasCombinators():
		return "union"
	case s.HasProperties():
		return "object"
	}
	return "unknown"
}
