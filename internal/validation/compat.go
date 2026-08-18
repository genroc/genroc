package validation

import (
	"fmt"
	"sort"
	"strings"

	"genroc/internal/model"
	"genroc/internal/schema"
)

// Side names which of the two compared sets a row's failure came from; the caller resolves
// it to a version.
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

// Issue is one difference, addressed by the schema compared and the path inside it (§6a).
// Nothing here names an edit: no comparison can prove which edit produced which break.
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
// what broke; the two never share a row (§6b). Empty Affects means no check covers the
// slot, which is the one thing this channel exists to say.
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
// mean anything: a row that carries no judgement must not be read as having passed one.
type Report struct {
	Name   string        `json:"name"`
	Status CompareStatus `json:"status,omitempty"`
	// Absent means the side carries no version for this name: a submitted document, or a
	// side that does not carry it at all — Status is what tells those apart.
	FromVersion int `json:"from,omitempty"`
	ToVersion   int `json:"to,omitempty"`
	// Set only on an unanalysable row, naming the version that failed its own inference.
	Side   Side   `json:"side,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Upgrade is instance continuation; Contract is what the outside world was written
	// against. Never folded: they run in opposite directions (§1).
	Upgrade  Verdict `json:"upgrade"`
	Contract Verdict `json:"contract"`
	// Three kinds of row, each addressed its own way. A removed task is an Issue — an
	// instance sitting on one has nowhere to go — so a rename reads as an addition and a break.
	Changed []SlotChange `json:"changed,omitempty"`
	Added   []string     `json:"added,omitempty"`
	Issues  []Issue      `json:"issues,omitempty"`
}

// SideEntry is one process on one side of a comparison, as the caller resolved it. Version
// 0 means a submitted document, which is always treated as having moved — see moved.
type SideEntry struct {
	Def     *model.ProcessDefinition
	Version int
}

// CompareStatus says why a process row reads the way it does, so a row with no verdict
// is never mistaken for one that was checked and passed.
type CompareStatus string

const (
	// Both sides carry it, at different versions. The verdicts mean something.
	StatusCompared CompareStatus = "compared"
	// Both sides resolve to one version, so comparing it would compare a document with
	// itself. The common case in a channel-wide report.
	StatusNothingToCompare CompareStatus = "nothing_to_compare"
	// Only the target side carries it: nothing is being upgraded and nothing can break.
	StatusNew CompareStatus = "new"
	// A version whose own inference failed — a per-version verdict, since old rows were
	// validated under the rules of their day. It still makes the roll-up false.
	StatusUnanalysable CompareStatus = "unanalysable"
)

// SetReport is CompareSet's whole answer. Compatible is the conjunction over the rows
// actually compared: a new or unmoved process cannot break anything and must not drag the
// roll-up down, since a deployed channel always carries processes a bundle does not. An
// unanalysable version does make it false — an answer indistinguishable from "checked, and
// fine" is worse than no report.
type SetReport struct {
	Compatible bool `json:"compatible"`
	// Passes is the same question asked of the SELECTION: false only where a GATING member
	// broke. The two disagreeing is the intended reading when something is ignored (§8).
	Passes bool `json:"passes"`
	// Exactly one row per name on either side, whatever became of it — an unanalysable
	// version included, so a reader never crosses two arrays to find out what happened.
	Processes []Report `json:"processes"`
}

// TaskContexts returns the context schema at every task of def, keyed by task id: the state
// an instance sitting there holds on entry, shaped like the row — `input`, `outputs.<id>`
// per task that projects one, and `error` — required where guaranteed, optional where merely
// possible. "config" is stripped: it is re-resolved from the environment every tick, so
// nothing persisted corresponds to it.
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
	required, optional, mustErr, mayErr, errSrc := computeContextSets(def.Tasks)
	errs := errContexts(def.Tasks, mustErr, mayErr, errSrc, sf.Defs)
	out := make(map[string]schema.Schema, len(def.Tasks))
	for _, t := range def.Tasks {
		out[t.ID] = contextSchema(required[t.ID], optional[t.ID], sf.Tasks, input,
			schema.Schema{}, errs[t.ID]).WithDefs(sf.Defs)
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

// Compare answers, for two versions of one process, whether an instance running the old one
// could continue under the new one, and separately whether the new version still honours the
// output contract its consumers were written against.
//
// It is a shape check over inferred schemas, not meaning: dollars → cents is `number` on
// both sides and comes back compatible. specs/version-compatibility.md §5 lists what it
// cannot see; internal/validation/CLAUDE.md has why it is a conservative floor.
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
	r.Changed = accountedFor(r.Changed, r.Issues)
	r.Upgrade = Verdict{Compatible: !anyMember(r.Issues, MemberUpgrade)}
	r.Contract = Verdict{Compatible: !anyMember(r.Issues, MemberContract)}
	return r
}

// accountedFor drops the slots a finding already reports (§6b), so `Changed` is what is LEFT
// OVER and every consumer of the wire holds the same report. Do not move this to a renderer.
func accountedFor(changed []SlotChange, issues []Issue) []SlotChange {
	broke := make(map[string]bool, len(issues))
	for _, i := range issues {
		broke[i.Address] = true
	}
	out := changed[:0]
	for _, c := range changed {
		if !broke[c.Address] {
			out = append(out, c)
		}
	}
	return out
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
	add := func(member Member, address, task string, found []finding) {
		for _, f := range found {
			// Deduplicated on the path as the relation reported it, before any address strips
			// its own prefix: the same difference must key the same way wherever it surfaces.
			key := string(member) + "\x00" + f.path + "\x00" + f.msg
			if seen[key] {
				continue
			}
			seen[key] = true
			path := f.path
			if address == addressInput {
				path = insideInput(path)
			}
			out = append(out, Issue{
				Member: member, Address: address, Task: task,
				Path: path, Message: f.msg, Gating: true,
			})
		}
	}

	// Hoisted out of the per-task loop — the input sits in every context, so comparing it
	// there reports one break once per task. Two readings of one pair: STORED for the
	// upgrade (§2e), STRICT for the contract (what ValidateInput does to the next caller).
	oldIn, newIn := inputObject(oldA), inputObject(newA)
	add(MemberUpgrade, addressInput, "", storedExplainer.explain(oldIn, newIn))
	add(MemberContract, addressInput, "", (explainer{}).explain(oldIn, newIn))

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
		add(MemberUpgrade, t.ID, t.ID, storedExplainer.explain(oldA.contexts[t.ID], newA.contexts[t.ID]))
		if typeChanged(t, nt) {
			// State an instance left behind — a submitted result, children in flight, a timer —
			// belongs to the old action, and no schema relation describes handing it over. Where
			// the old type holds nothing, any instance here is at ENTRY and the new action runs.
			if holdsAnInstance(actionTypeOf(t)) {
				out = append(out, Issue{
					Member: MemberUpgrade, Address: t.ID + ":action.type", Task: t.ID, Gating: true,
					Message: fmt.Sprintf("%s → %s; an instance sitting there was left by an action the new type cannot take over from",
						actionTypeOf(t), actionTypeOf(nt)),
				})
			}
			// Result schemas are not comparable across types either: the party that submits
			// the value changed, so old ⊆ new would be asking a service to honour a contract
			// written for a worker.
			continue
		}
		out = append(out, addedChildKeyIssues(t, nt)...)
		out = append(out, resultIssues(t, nt)...)
	}

	add(MemberContract, addressOutput, "", compareOutput(oldA, newA))
	return out
}

// resultContract is one result schema pair, with the address it is reported under. A
// child_map declares one per key, so a task can carry several; every other action type
// carries at most one.
type resultContract struct {
	address  string
	task     string
	old, new schema.Schema
	// added marks a schema the new version declares where the old one declared none. There is
	// no old schema to compare it against, so it is reported directly — the way a removed
	// process output is (§3a).
	added bool
	// parks is true where an instance can be sitting on this task when the version changes,
	// so the schema is part of the upgrade question as well as the contract one (§2c).
	parks bool
}

// addedChildKeyIssues reports a key the new version declares and the old one did not — §2b's
// added-task rule one level down, reported directly because an addition has no old schema to
// be compared against. It is the only key-set move with a rule; CLAUDE.md says why the other
// two need none.
func addedChildKeyIssues(old, new *model.Task) []Issue {
	if old.Action == nil || new.Action == nil || old.Action.Type != model.ActionTypeChildMap {
		return nil
	}
	var out []Issue
	for _, key := range sortedChildKeys(new.Action.Children) {
		if _, ok := old.Action.Children[key]; ok {
			continue
		}
		out = append(out, Issue{
			Member: MemberUpgrade, Address: childKeyAddress(old.ID, actionTypeOf(old), key),
			Task: old.ID, Gating: true,
			Message: "added; a parent already collecting spawned no child for it",
		})
	}
	return out
}

// resultContracts pairs what two versions of a task declare they will accept back. It does
// not care which process a child call names — `old ⊆ new` carries registration's premise
// forward, so a renamed call needs no rule of its own (§2c). A schema DROPPED is not
// compared: we conform less than we did, so nobody is turned away.
func resultContracts(old, new *model.Task) []resultContract {
	if old.Action == nil || new.Action == nil {
		return nil
	}
	actionType := string(old.Action.Type)
	parks := parksMidTask(actionType)
	pair := func(address string, a, b *schema.Schema) []resultContract {
		if b == nil {
			return nil
		}
		rc := resultContract{address: address, task: old.ID, new: *b, parks: parks}
		if a == nil {
			// A schema that accepts everything constrains nothing, so declaring `{}` — the
			// carried-but-unread idiom, which only makes the value exportable — is not an
			// addition anything can fail.
			if (schema.Schema{}).IsSubset(*b) {
				return nil
			}
			rc.added = true
		} else {
			rc.old = *a
		}
		return []resultContract{rc}
	}
	// A fetch declares its body per status but produces ONE value, so it is compared as one:
	// the merged union under the same `.result` address every other action type uses. The
	// child_map below looks similar and is not — its keys are separately readable outputs,
	// so each is its own contract.
	if old.Action.Type == model.ActionTypeFetch {
		oldRS, err := fetchResultContract(old.Action)
		if err != nil {
			return nil
		}
		newRS, err := fetchResultContract(new.Action)
		if err != nil {
			return nil
		}
		return pair(old.ID+":"+actionType+".result", oldRS, newRS)
	}
	if old.Action.Type == model.ActionTypeChildMap {
		var out []resultContract
		for _, key := range sortedChildKeys(old.Action.Children) {
			newChild, ok := new.Action.Children[key]
			if !ok {
				continue
			}
			out = append(out, pair(childKeyAddress(old.ID, actionType, key)+".result",
				old.Action.Children[key].ResultSchema, newChild.ResultSchema)...)
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

// addedResultMessage is what a result schema declared where none was says. It covers both
// members deliberately: the party that produces the value never saw this schema, and an
// instance already parked will meet it on the way out.
const addedResultMessage = "added; a result written against the version that declared none may not satisfy it"

// resultIssues compares what each task expects back, in §3a's direction and with the strict
// relation, because a real conform stands there. Where the task can park (§2c) the same
// difference is also an upgrade break, filed as a SECOND issue rather than a combined one:
// they gate separately, and the renderer is what merges them into a line.
func resultIssues(old, new *model.Task) []Issue {
	var out []Issue
	for _, rc := range resultContracts(old, new) {
		found := (explainer{}).explain(rc.old, rc.new)
		if rc.added {
			// No path: the finding is about the schema arriving at all. The zero schema it would
			// otherwise be compared against is not a document anybody wrote, so its breaks are
			// not findings.
			found = []finding{{msg: addedResultMessage}}
		}
		for _, f := range found {
			if rc.parks {
				out = append(out, Issue{
					Member: MemberUpgrade, Address: rc.address, Task: rc.task,
					Path: f.path, Message: f.msg, Gating: true,
				})
			}
			out = append(out, Issue{
				Member: MemberContract, Address: rc.address, Task: rc.task,
				Path: f.path, Message: f.msg, Gating: true,
			})
		}
	}
	return out
}

// insideInput strips the wrapper inputObject adds: a path is relative to the schema its
// address names (§6a), and the address here IS the input. The wrapper's own break — the
// process gaining an input at all — is left with no path, so the message says it.
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
func compareOutput(oldA, newA analysis) []finding {
	oldOut, hasOld, err := schemaFileOutput(oldA.sf)
	if err != nil {
		return []finding{{msg: err.Error()}}
	}
	newOut, hasNew, err := schemaFileOutput(newA.sf)
	if err != nil {
		return []finding{{msg: err.Error()}}
	}
	// Adding an output is free: consumers were written against a process that produced
	// nothing. Removing one has no new schema to be compared against, so it is reported
	// directly, the way a removed task is (§2b).
	if !hasOld {
		return nil
	}
	if !hasNew {
		return []finding{{msg: "removed; consumers were written against it"}}
	}
	return contractExplainer.explain(newOut, oldOut)
}

// CompareSet is Compare over a name-paired set; a single pair is one entry. The caller
// resolves and reconciles both sides first — what arrives here is already paired, and this
// decides what is worth comparing.
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

// ── diagnostics ───────────────────────────────────────────────────────────────

// explainer WORDS a subset break; it does not walk. The relation reports its own breaks, so
// a message cannot disagree with the verdict it explains. See CLAUDE.md for the three
// configurations and why a parallel walker was removed.
type explainer struct {
	// Both schemas read as descriptions of data a conform already produced. Sound ONLY where
	// the value is never conformed again — a slot with a runtime conform must not use it.
	asStored bool
	// The check runs new ⊆ old, so every message is written from the READER's point of view.
	// Flips BOTH halves: the arrow, and what a missing required property means — see word.
	swap bool
}

var (
	storedExplainer = explainer{asStored: true}
	// Deliberately strict: the output's consumers include a waiting parent, whose conform
	// rejects an absent required key however nullable its type.
	contractExplainer = explainer{swap: true}
)

// finding is one break as the report files it: where, and what, kept apart. The path stays a
// field the whole way to the renderer, so a property name is never recovered by searching
// prose for a colon — which silently dropped any name containing a space.
type finding struct{ path, msg string }

// explain names EVERY place sub fails to fit super, in the relation's own walk order; empty
// means it fits. All of them because they are independent — one issue per run means one
// release per difference.
func (e explainer) explain(sub, super schema.Schema) []finding {
	var breaks []*schema.SubsetBreak
	if e.asStored {
		breaks = sub.ExplainSubsetAsStored(super)
	} else {
		breaks = sub.ExplainSubset(super)
	}
	out := make([]finding, 0, len(breaks))
	for _, brk := range breaks {
		out = append(out, finding{path: brk.Path, msg: e.word(brk)})
	}
	return out
}

// word says WHAT broke, from the READER's point of view; the path says where. Under swap a
// property the super side requires is one the old version guaranteed, not one the new added.
func (e explainer) word(b *schema.SubsetBreak) string {
	if b.Kind == schema.BreakMissingRequired {
		if e.swap {
			return "no longer guaranteed"
		}
		return "newly required field"
	}
	from, to := b.Sub, b.Super
	if e.swap {
		from, to = to, from
	}
	if b.Path == "" {
		return fmt.Sprintf("%s is not accepted where %s is expected", from, to)
	}
	return fmt.Sprintf("%s → %s", from, to)
}

// ApplySelection marks every issue gating or excused and computes Passes. Only contract
// findings may be excused — the upgrade check is not negotiable (§5) — and an `unanalysable`
// row cannot be, being the absence of a verdict rather than one. A verdict never moves: only
// Gating and Passes answer to the selection.
//
// The one token accepted today is `contract`; specs/compat-selection.md has the grammar this
// restricts. An unknown token is an error, never a no-op.
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
