package validation

import (
	"fmt"
	"sort"
	"strings"

	"genroc/internal/model"
	"genroc/internal/schema"
)

// Side names which of the two compared sets a finding came from. An unanalysable version
// is a fact about one side, and the versions are the caller's selectors — so the side is
// named here and resolved to a version above.
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

// Report is Compare's verdict for one process. Status says whether the verdicts below it
// mean anything: a process with nothing to compare, or one with no previous version,
// carries no judgement and must not be read as having passed one.
type Report struct {
	Name   string        `json:"name"`
	Status CompareStatus `json:"status,omitempty"`
	// FromVersion / ToVersion are what the caller's selectors landed on. Absent means the
	// side carries no version for this name: a submitted document, or a side that does not
	// carry it at all — Status is what tells those apart.
	FromVersion int `json:"from,omitempty"`
	ToVersion   int `json:"to,omitempty"`
	// Side and Reason are set only on an unanalysable row, naming the version that failed
	// its own inference and why.
	Side   Side   `json:"side,omitempty"`
	Reason string `json:"reason,omitempty"`
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

// SideEntry is one process on one side of a comparison, as the caller resolved it.
// Version is what the caller's selector landed on; 0 means a submitted document that has
// no version yet, which is always treated as having moved — it is the thing being asked
// about.
type SideEntry struct {
	Def     *model.ProcessDefinition
	Version int
}

// CompareStatus says why a process row reads the way it does, so a row with no verdict
// is never mistaken for one that was checked and passed.
type CompareStatus string

const (
	// StatusCompared — both sides carry it, at different versions. The verdicts mean
	// something.
	StatusCompared CompareStatus = "compared"
	// StatusNothingToCompare — both sides resolve to the same version, so comparing it
	// would be comparing a document with itself. This is the common case in a
	// channel-wide report: most processes do not move between two channels.
	StatusNothingToCompare CompareStatus = "nothing_to_compare"
	// StatusNew — only the target side carries it. No previous version exists, so
	// nothing is being upgraded and there is nothing that could break.
	StatusNew CompareStatus = "new"
	// StatusUnanalysable — a version whose own inference failed. Old rows were validated
	// under the rules of their day, so this is a per-version verdict rather than a failure
	// of the whole report. It still makes the roll-up false: it was compared against
	// nothing, and an answer indistinguishable from "checked, and fine" is worse than none.
	StatusUnanalysable CompareStatus = "unanalysable"
)

// SetReport is CompareSet's whole answer. Compatible is the conjunction over the rows
// that were actually compared: a process with nothing to compare, or one that is new,
// cannot break anything and must not drag the roll-up down — otherwise almost every real
// comparison reports false, since a deployed channel always carries processes a bundle
// does not. An unanalysable version DOES make it false: it was compared against nothing,
// and an answer indistinguishable from "checked, and fine" is worse than no report.
type SetReport struct {
	Compatible bool `json:"compatible"`
	// Processes carries exactly one row per name on either side, whatever became of it.
	// An unanalysable version is a row here too rather than a list of its own: every
	// process has one place to look, and a reader never has to cross-reference two arrays
	// to find out what happened to a name.
	Processes []Report `json:"processes"`
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
		if reason := readExplainer.explain("", oldA.contexts[t.ID], newA.contexts[t.ID], 0); reason != "" {
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
	// An existing instance's input was validated once, at creation, under the schema of the
	// day; nothing re-validates it. So it is read from here on, like the rest of the row.
	if reason := readExplainer.explain("", wrap(oldA), wrap(newA), 0); reason != "" {
		return SlotVerdict{Reason: reason}
	}
	return SlotVerdict{Compatible: true}
}

// compareOutput is the consumer contract reversed: everything the new version can produce
// must satisfy readers of the old. IsSubset, not NarrowsTo — narrowing is the privilege of
// a slot with a runtime conform behind it, and nothing conforms here.
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
	if reason := contractExplainer.explain("output", newOut, oldOut, 0); reason != "" {
		return false, reason
	}
	return true, ""
}

// CompareSet is Compare over a name-paired set. A single pair is CompareSet with one entry.
//
// The caller resolves each side to a table of one version per process name and reconciles
// the two before calling. What arrives here is already paired; this decides what is worth
// comparing and what the verdicts are.
func CompareSet(old, new map[string]SideEntry) (SetReport, error) {
	report := SetReport{Compatible: true, Processes: []Report{}}

	// Every name on either side gets a row, and the status comes from the versions alone —
	// no inference needed. Analysis happens only for a pair that is actually being
	// compared, which is what keeps an unchanged process from being judged at all: a
	// registry accumulates definitions validated under older rules, and one of those
	// failing to analyse must not make a report about two OTHER versions come back false.
	for _, name := range unionOfNames(old, new) {
		from, inOld := old[name]
		to, inNew := new[name]

		row := Report{Name: name, FromVersion: from.Version, ToVersion: to.Version}
		unjudged := func(status CompareStatus) {
			row.Status, row.Compatible, row.OutputCompatible = status, true, true
			row.Input = SlotVerdict{Compatible: true}
			report.Processes = append(report.Processes, row)
		}
		switch {
		case !inOld:
			// No previous version exists, so nothing is being upgraded and nothing can break.
			unjudged(StatusNew)
			continue
		case !inNew, !moved(from, to):
			// Either the caller did not carry it over, or both sides landed on one version;
			// comparing a document with itself is a tautology.
			unjudged(StatusNothingToCompare)
			continue
		}

		// A submitted document has no version, so version equality cannot say whether it
		// changed — the documents themselves have to. Two STORED versions are left to their
		// numbers, which say MORE than the documents do: a version also pins the child
		// versions it runs, so two identical documents at different versions are genuinely
		// different processes. Normalizing first is what makes a raw submitted document and
		// a canonical stored one comparable at all.
		if from.Version == 0 || to.Version == 0 {
			if err := from.Def.Normalize(); err != nil {
				report.unanalysable(row, SideFrom, err)
				continue
			}
			if err := to.Def.Normalize(); err != nil {
				report.unanalysable(row, SideTo, err)
				continue
			}
			if !documentsDiffer(from.Def, to.Def) {
				unjudged(StatusNothingToCompare)
				continue
			}
		}

		oldA, err := analyze(from.Def)
		if err != nil {
			report.unanalysable(row, SideFrom, err)
			continue
		}
		newA, err := analyze(to.Def)
		if err != nil {
			report.unanalysable(row, SideTo, err)
			continue
		}

		r := compare(oldA, newA)
		r.Status, r.FromVersion, r.ToVersion = StatusCompared, from.Version, to.Version
		if !r.Compatible || !r.OutputCompatible {
			report.Compatible = false
		}
		report.Processes = append(report.Processes, r)
	}

	return report, nil
}

func (r *SetReport) unanalysable(row Report, side Side, err error) {
	row.Status, row.Side, row.Reason = StatusUnanalysable, side, err.Error()
	r.Processes = append(r.Processes, row)
	r.Compatible = false
}

// moved reports whether a process differs between the two sides. A submitted document has
// no version yet and always counts as moved — it is the thing being asked about.
func moved(from, to SideEntry) bool {
	return from.Version == 0 || to.Version == 0 || from.Version != to.Version
}

func tasksByID(def *model.ProcessDefinition) map[string]*model.Task {
	out := make(map[string]*model.Task, len(def.Tasks))
	for _, t := range def.Tasks {
		out[t.ID] = t
	}
	return out
}

// unionOfNames returns every process named on either side, sorted.
func unionOfNames(a, b map[string]SideEntry) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string]SideEntry{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

func sortedEntries(m map[string]SideEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
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

// ── diagnostics ───────────────────────────────────────────────────────────────

// explainer names the first place one schema fails to fit another, decomposing slot by
// slot and then into properties within the failing slot.
//
// It decomposes ABOVE the subset check rather than threading an error through it:
// isSubset returns a bool and sits on every hot validation path, so making it explain
// itself would be a large change to load-bearing code for one caller.
type explainer struct {
	// absentAsNull stops requiring the presence of a nullable property. It is sound ONLY
	// where the value is read and never conformed — conformObject rejects an absent
	// required key whatever its type, so a slot with a runtime conform behind it must not
	// use this.
	absentAsNull bool
	// swap renders the arrow old → new even when the check runs new ⊆ old. The reader is
	// asking what they changed, not which direction the subset ran in.
	swap bool
}

var (
	// readExplainer is for the continuation checks. An instance's stored context and its
	// stored input are READ from here on — nothing re-validates them — so a key that is
	// simply absent reads as null, and a nullable slot demanding presence would refuse
	// data that works.
	readExplainer = explainer{absentAsNull: true}
	// contractExplainer is for the output contract, and is deliberately STRICT. Its
	// consumers include a waiting parent, which conforms the value against its
	// result_schema — and that conform rejects an absent required key however nullable its
	// type. Relaxing here would promise what the runtime refuses.
	contractExplainer = explainer{swap: true}
)

// maxExplainDepth bounds the descent: a recursive schema has no bottom to reach, and one
// level into a failing slot is already the useful part.
const maxExplainDepth = 4

func (e explainer) fits(sub, super schema.Schema) bool {
	if e.absentAsNull {
		return sub.IsSubsetAbsentAsNull(super)
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
		superProps := super.Properties()
		superRequired := append([]string(nil), super.Required()...)
		sort.Strings(superRequired)
		for _, f := range superRequired {
			if subRequired[f] {
				continue
			}
			// Mirrors the subset rule exactly, `declared` included: a required name with no
			// property has no type to call nullable, and a message naming a break the check
			// tolerates is worse than none — it sends a reader after a non-problem.
			if prop, declared := superProps[f]; e.absentAsNull && declared && prop.HasNull() {
				continue
			}
			return joinPath(path, f) + ": newly required field"
		}

		subProps := sub.Properties()
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
