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

// Member is the group a finding is filed under. specs/compat-command.md §1: the two
// questions, and the contract slots each answers for.
type Member string

const (
	MemberUpgrade  Member = "upgrade"
	MemberContract Member = "contract"
)

// Issue is one difference, addressed by the schema that was compared and the path
// `isSubset` reported inside it (§6a). Nothing here names an edit: no comparison can prove
// which edit produced which break, and a slot printed beside a message is that claim
// whether or not it is worded as one.
type Issue struct {
	Member  Member `json:"member"`
	Address string `json:"address"`
	// Task is set where the address names one, so a consumer can scope by task without
	// taking an address apart.
	Task    string `json:"task,omitempty"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
	Gating  bool   `json:"gating"`
}

// SlotChange is one definition slot that differs — what the author EDITED, as opposed to
// what broke. The two never share a row (§6b).
//
// Affects is the question the slot bears on, a property of the slot rather than of this
// comparison. Empty means no check covers it — a URL repointed, an `only_once` flipped —
// which is the one thing the changed-slot channel exists to say.
type SlotChange struct {
	Address string   `json:"address"`
	Task    string   `json:"task,omitempty"`
	Affects []Member `json:"affects,omitempty"`
}

// Verdict is one category's answer, derived from the issues filed and never tracked beside
// them: a column that can disagree with the lines under it is a report arguing with itself.
type Verdict struct {
	Compatible bool `json:"compatible"`
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
	// Upgrade is instance continuation; Contract is what the outside world was written
	// against. They run in opposite directions and are never folded: one boolean over two
	// opposite-direction checks would be meaningless (§1).
	Upgrade  Verdict `json:"upgrade"`
	Contract Verdict `json:"contract"`
	// Changed, Added and Issues are three kinds of row, each addressed its own way: a slot
	// that differs, a task that did not exist, and a value that broke. A removed task is an
	// Issue — an instance sitting on one has nowhere to go — so a rename reads as one
	// addition and one break.
	Changed []SlotChange `json:"changed,omitempty"`
	Added   []string     `json:"added,omitempty"`
	Issues  []Issue      `json:"issues,omitempty"`
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
	// Passes is the same question asked of the SELECTION: false only where a GATING member
	// broke. With nothing ignored the two agree; with something ignored they are meant to
	// disagree — Compatible is what was found, Passes is what this caller asked about.
	Passes bool `json:"passes"`
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
// accidental break, not certifying a migration; specs/version-compatibility.md §5 is the
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
	r := Report{Name: newA.def.Name}
	newTasks, oldTasks := tasksByID(newA.def), tasksByID(oldA.def)

	for _, c := range changedDefinitionSlots(oldA.def, newA.def) {
		r.Changed = append(r.Changed, c)
	}
	for _, t := range oldA.def.Tasks {
		if nt, ok := newTasks[t.ID]; ok {
			r.Changed = append(r.Changed, changedTaskSlots(t, nt)...)
		}
	}
	for _, t := range newA.def.Tasks {
		if _, ok := oldTasks[t.ID]; !ok {
			r.Added = append(r.Added, t.ID)
		}
	}

	r.Issues = issues(oldA, newA, newTasks)
	// Derived, never tracked beside the issues: a column that can disagree with the lines
	// under it is a report arguing with itself.
	r.Upgrade = Verdict{Compatible: !anyMember(r.Issues, MemberUpgrade)}
	r.Contract = Verdict{Compatible: !anyMember(r.Issues, MemberContract)}
	return r
}

func anyMember(issues []Issue, m Member) bool {
	for _, i := range issues {
		if i.Member == m {
			return true
		}
	}
	return false
}

// issues runs both checks in the order the report reads: the input, then each task as the
// OLD version ordered them, then the output. Rendering walks this list, so the order is the
// process's shape rather than the order the checks happened to run in (§6c).
func issues(oldA, newA analysis, newTasks map[string]*model.Task) []Issue {
	var out []Issue
	// One difference in the data surfaces at EVERY task that can see it. It is a fact about
	// the value, not about who reads it, so the first task to surface it keeps it (§6a).
	seen := map[string]bool{}
	add := func(member Member, address, task, reason string) {
		path, msg := splitReason(reason)
		// Deduplicated on the path as the explainer built it, before any address strips its
		// own prefix: the same difference must key the same way wherever it surfaces.
		key := string(member) + "\x00" + path + "\x00" + msg
		if seen[key] {
			return
		}
		seen[key] = true
		if address == addressInput {
			path = insideInput(path)
		}
		out = append(out, Issue{
			Member: member, Address: address, Task: task,
			Path: path, Message: msg, Gating: true,
		})
	}

	// The input is hoisted out of the per-task loop — it sits in every task's context, so
	// comparing it there would report one break once per task. Two readings of one pair:
	// as STORED for the upgrade (a defaulted property is present because creation filled it,
	// §2e) and STRICT for the contract (what ValidateInput will do to the next caller).
	oldIn, newIn := inputObject(oldA), inputObject(newA)
	if reason := storedExplainer.explain("", oldIn, newIn, 0); reason != "" {
		add(MemberUpgrade, addressInput, "", reason)
	}
	if reason := (explainer{}).explain("", oldIn, newIn, 0); reason != "" {
		add(MemberContract, addressInput, "", reason)
	}

	for _, t := range oldA.def.Tasks {
		nt, ok := newTasks[t.ID]
		if !ok {
			// An instance sitting on a task the new version dropped has nowhere to continue.
			// A set difference, not a schema relation (§2b).
			out = append(out, Issue{
				Member: MemberUpgrade, Address: t.ID, Task: t.ID, Gating: true,
				Message: "removed; an instance there has nowhere to continue",
			})
			continue
		}
		// One context per task is enough for the whole remaining run: a task output's type
		// is position-independent and the must-analysis is monotone along a path (§2a).
		if reason := storedExplainer.explain("", oldA.contexts[t.ID], newA.contexts[t.ID], 0); reason != "" {
			add(MemberUpgrade, t.ID, t.ID, reason)
		}
		if typeChanged(t, nt) {
			// An instance can only be MID-task on an action that parks, and that state — a
			// submitted result, or children in flight — belongs to the old action type. No
			// schema relation describes handing it to a different one, so it is checked
			// directly (version-compatibility.md §2, which refuses the same move at the gate).
			//
			// Where the old type cannot park, any instance at this task is at ENTRY and the
			// new action simply runs: the output shapes are compared like any other, and a
			// type change alone breaks nothing.
			if parksMidTask(actionTypeOf(t)) {
				out = append(out, Issue{
					Member: MemberUpgrade, Address: t.ID + ":action.type", Task: t.ID, Gating: true,
					Message: fmt.Sprintf("%s → %s; an instance parked there holds state the new type cannot describe",
						actionTypeOf(t), actionTypeOf(nt)),
				})
			}
			// Result schemas are not comparable across types either: the party that submits
			// the value changed, so old ⊆ new would be asking a service to honour a contract
			// written for a worker.
			continue
		}
		out = append(out, resultIssues(t, nt)...)
	}

	if reason := compareOutput(oldA, newA); reason != "" {
		add(MemberContract, addressOutput, "", reason)
	}
	return out
}

// resultContract is one result schema pair, with the address it is reported under. A
// child_map declares one per key, so a task can carry several; every other action type
// carries at most one.
type resultContract struct {
	address  string
	task     string
	old, new schema.Schema
	// parks is true where an instance can be sitting on this task when the version changes,
	// so the schema is part of the upgrade question as well as the contract one (§2c).
	parks bool
}

// resultContracts pairs what two versions of a task declare they will accept back. Skipped
// where either side declares nothing: an omitted result_schema is a third state, not the top
// type — the result stays untyped and unexportable — so there is no schema to compare against
// (internal/schema/CLAUDE.md). The changed-slot row is what reports that edit.
func resultContracts(old, new *model.Task) []resultContract {
	if old.Action == nil || new.Action == nil {
		return nil
	}
	actionType := string(old.Action.Type)
	parks := parksMidTask(actionType)
	pair := func(address string, a, b *schema.Schema) []resultContract {
		if a == nil || b == nil {
			return nil
		}
		return []resultContract{{address: address, task: old.ID, old: *a, new: *b, parks: parks}}
	}
	if old.Action.Type == model.ActionTypeChildMap {
		var out []resultContract
		for _, key := range sortedChildKeys(old.Action.Children) {
			newChild, ok := new.Action.Children[key]
			if !ok {
				continue
			}
			oldChild := old.Action.Children[key]
			out = append(out, pair(old.ID+":"+actionType+"."+key+".result",
				oldChild.ResultSchema, newChild.ResultSchema)...)
		}
		return out
	}
	return pair(old.ID+":"+actionType+".result", old.Action.ResultSchema, new.Action.ResultSchema)
}

func sortedChildKeys(children map[string]model.ChildEntry) []string {
	keys := make([]string, 0, len(children))
	for k := range children {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// resultIssues compares what each task expects back. The direction is §3a's — the service,
// the worker or the child SUBMITS the value, so the new version must accept everything the
// old one did — and the relation is strict, because a real conform stands there.
//
// Where the task can park (§2c) the same difference is also an upgrade break, filed as a
// SECOND issue rather than a combined one: they gate separately, and the renderer is what
// merges them into a line. The comparison uses the strict relation for both halves even
// though a result already submitted is only ever read back: it cannot tell which state a
// parked instance is in, so it reports the stricter answer and leaves the gate to relax it.
func resultIssues(old, new *model.Task) []Issue {
	var out []Issue
	for _, rc := range resultContracts(old, new) {
		reason := (explainer{}).explain("", rc.old, rc.new, 0)
		if reason == "" {
			continue
		}
		path, msg := splitReason(reason)
		if rc.parks {
			out = append(out, Issue{
				Member: MemberUpgrade, Address: rc.address, Task: rc.task,
				Path: path, Message: msg, Gating: true,
			})
		}
		out = append(out, Issue{
			Member: MemberContract, Address: rc.address, Task: rc.task,
			Path: path, Message: msg, Gating: true,
		})
	}
	return out
}

// splitReason peels the path off an explainer message. It is the producer's own split, not
// a reader's guess: explain builds "<path>: <message>" and a whole-schema break carries no
// path at all, so nothing here has to know what a path may contain.
func splitReason(reason string) (path, msg string) {
	i := strings.Index(reason, ": ")
	if i <= 0 || strings.Contains(reason[:i], " ") {
		return "", reason
	}
	return reason[:i], reason[i+2:]
}

// insideInput strips the wrapper inputObject adds. A path is relative to the schema its
// address names (§6a), and the address here IS the input — so `input.retries` reads
// `retries`, and the wrapper's own break (the process gaining an input at all) reads as no
// path at all, leaving the message to say it.
func insideInput(path string) string {
	if path == "input" {
		return ""
	}
	return strings.TrimPrefix(path, "input.")
}

// inputObject wraps a side's process input in a one-property object so isSubset answers both
// halves with no second code path: a newly required input, and an input whose type changed.
// The wrapper is a device for the relation, never part of an address — see insideInput.
func inputObject(a analysis) schema.Schema {
	o := schema.Object()
	if !a.sf.ProcessInput.IsZero() {
		o = o.WithProperty("input", a.sf.ProcessInput, true)
	}
	return o.WithDefs(a.sf.Defs)
}

// compareOutput is the consumer contract reversed: everything the new version can produce
// must satisfy readers of the old. IsSubset, not NarrowsTo — narrowing is the privilege of
// a slot with a runtime conform behind it, and nothing conforms here.
func compareOutput(oldA, newA analysis) string {
	oldOut, hasOld, err := schemaFileOutput(oldA.sf)
	if err != nil {
		return err.Error()
	}
	newOut, hasNew, err := schemaFileOutput(newA.sf)
	if err != nil {
		return err.Error()
	}
	// Adding an output is free — consumers were written against a process that produced
	// nothing, so nothing they read can stop working. Removing one is not, and it is not a
	// schema comparison either: there is no new schema to compare against, so it is reported
	// directly, the way a removed task is (§2b).
	if !hasOld {
		return ""
	}
	if !hasNew {
		return "removed; consumers were written against it"
	}
	return contractExplainer.explain("", newOut, oldOut, 0)
}

// CompareSet is Compare over a name-paired set. A single pair is CompareSet with one entry.
//
// The caller resolves each side to a table of one version per process name and reconciles
// the two before calling. What arrives here is already paired; this decides what is worth
// comparing and what the verdicts are.
func CompareSet(old, new map[string]SideEntry) (SetReport, error) {
	report := SetReport{Compatible: true, Processes: []Report{}}

	// Every name on either side gets a row; status comes from versions alone. Analysis runs
	// only for pairs actually compared — a legacy version failing to analyse must not make a
	// report about two OTHER versions come back false.
	for _, name := range unionOfNames(old, new) {
		from, inOld := old[name]
		to, inNew := new[name]

		row := Report{Name: name, FromVersion: from.Version, ToVersion: to.Version}
		unjudged := func(status CompareStatus) {
			row.Status = status
			row.Upgrade, row.Contract = Verdict{Compatible: true}, Verdict{Compatible: true}
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

		// A submitted document has no version, so the documents must decide; two STORED versions
		// are left to their numbers, which say MORE (a version pins child versions, so identical
		// documents at different versions differ). Normalizing makes raw and canonical comparable.
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
		if !r.Upgrade.Compatible || !r.Contract.Compatible {
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

// explainer names the first place one schema fails to fit another, decomposing ABOVE the
// subset check — isSubset returns a bool on every hot validation path, and making it
// explain itself is a big change to load-bearing code for one caller.
type explainer struct {
	// asStored reads both schemas as descriptions of data a conform already produced:
	// absence-as-null, plus a defaulted property counting as present. It is sound ONLY
	// where the value is read and never conformed again — conformObject rejects an absent
	// required key whatever its type, so a slot with a runtime conform behind it must not
	// use this.
	//
	// It must name the SAME relation `fits` dispatches to. An explainer that disagrees with
	// the relation reports a break with nothing to say about it, or says something about a
	// pair the verdict called fine.
	asStored bool
	// swap tells the explainer the check runs new ⊆ old, so every message must be written
	// from the READER's point of view rather than the relation's. It affects both halves:
	// the arrow renders old → new, and a property super requires that sub lacks is not a
	// newly required field — it is one the old side guaranteed and the new side no longer
	// does. The reader is asking what THEY changed, not which direction the subset ran in.
	swap bool
}

var (
	// storedExplainer is for the upgrade checks. An instance's stored context and its stored
	// input are READ from here on — nothing re-validates them — so a key that is simply
	// absent reads as null, and a property carrying a default is present because creation
	// filled it (specs/compat-command.md §2e).
	storedExplainer = explainer{asStored: true}
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
	if e.asStored {
		return sub.IsSubsetAsStored(super)
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
			if prop, declared := superProps[f]; e.asStored && declared && prop.HasNull() {
				continue
			}
			// The other half of asStored: sub guarantees the value because creation filled
			// its default, so super requiring it is no gap at all (§2e).
			if e.asStored {
				if p, declared := sub.Properties()[f]; declared && p.Default() != nil {
					continue
				}
			}
			if e.swap {
				return joinPath(path, f) + ": no longer guaranteed"
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

// ApplySelection marks every issue gating or excused and computes Passes.
//
// **The upgrade check is not negotiable** (specs/compat-command.md §5): only contract
// findings can be excused, and an `unanalysable` row cannot be — it is the absence of a
// verdict rather than one, so excluding it would produce exactly the answer
// indistinguishable from "checked, and fine" that the status exists to prevent.
//
// A verdict never moves: Upgrade and Contract stay what was FOUND, ignoring nothing, and
// only Gating and Passes answer to the selection. The two disagreeing — a green run with a
// break in it — is the intended reading, not a bug (§8).
//
// Today the one token accepted is `contract`; the general grammar it is a restriction of is
// specs/compat-selection.md. An unknown token is an error rather than a no-op: a flag that
// silently does nothing is the failure a selection feature most easily introduces.
func (r *SetReport) ApplySelection(ignore []string) error {
	excused := map[Member]bool{}
	for _, token := range ignore {
		if Member(token) != MemberContract {
			return fmt.Errorf("cannot ignore %q; the only member that may be excused is %q — "+
				"the upgrade check answers for rows this deployment already owns", token, MemberContract)
		}
		excused[MemberContract] = true
	}

	r.Passes = true
	for pi := range r.Processes {
		p := &r.Processes[pi]
		if p.Status == StatusUnanalysable {
			r.Passes = false
		}
		for ii := range p.Issues {
			issue := &p.Issues[ii]
			issue.Gating = !excused[issue.Member]
			if issue.Gating {
				r.Passes = false
			}
		}
	}
	return nil
}
