package validation

import (
	"fmt"
	"slices"
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

// SlotChange is what the author EDITED, never what broke (§6b). Empty Affects means no check
// covers the slot, the one thing this channel exists to say.
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

// SetReport is CompareSet's whole answer. Compatible is the conjunction over the rows actually
// compared: a new or unmoved process does not count, an unanalysable version makes it false.
type SetReport struct {
	Compatible bool `json:"compatible"`
	// Passes is the same question asked of the SELECTION: false only where a GATING member
	// broke. The two disagreeing is the intended reading when something is ignored (§8).
	Passes bool `json:"passes"`
	// Exactly one row per name on either side, whatever became of it — an unanalysable
	// version included, so a reader never crosses two arrays to find out what happened.
	Processes []Report `json:"processes"`
}

// TaskContexts returns, per task id, the context an instance holds on entry, shaped like the row:
// required where guaranteed, optional where merely possible, and no `config`.
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
	// Zero configSchema: config is re-resolved every tick, so nothing persisted corresponds to it.
	scopes := taskScopes{
		tasks: sf.Tasks, processInput: input, configSchema: schema.Schema{}, defs: sf.Defs,
		required: required, optional: optional, errs: errs,
	}
	out := make(map[string]schema.Schema, len(def.Tasks))
	for _, t := range def.Tasks {
		out[t.ID] = scopes.entry(t)
	}
	return out
}

// analysis is computed once per side, so a set comparison infers each version once.
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

// Compare reports whether an instance of old could continue under new (Upgrade), and whether new
// honours old's output contract (Contract). Shapes, not meaning: dollars → cents compares
// compatible. specs/version-compatibility.md §5.
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

// issues files in the order the report reads — input, each task in the OLD order, output —
// because rendering walks this list (§6c).
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

	// Hoisted out of the per-task loop, where it would report one break per task. STORED for the
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
			if issue, ok := typeChangeIssue(t, nt); ok {
				out = append(out, issue)
			}
			// Result schemas are not comparable across types: the submitting party changed, so
			// old ⊆ new would hold a service to a worker's contract.
			continue
		}
		out = append(out, addedChildKeyIssues(t, nt)...)
		out = append(out, resultIssues(t, nt)...)
	}

	add(MemberContract, addressOutput, "", compareOutput(oldA, newA))
	return out
}

// typeChangeIssue: what the old action left — a result, children, a timer — has no counterpart in
// the new one. Shared with the upgrade gate (TypeChangeBreak), so the two cannot disagree.
func typeChangeIssue(old, new *model.Task) (Issue, bool) {
	if !typeChanged(old, new) || !holdsAnInstance(actionTypeOf(old)) {
		return Issue{}, false
	}
	return Issue{
		Member: MemberUpgrade, Address: old.ID + ":action.type", Task: old.ID, Gating: true,
		Message: fmt.Sprintf("%s → %s; an instance sitting there was left by an action the new type cannot take over from",
			actionTypeOf(old), actionTypeOf(new)),
	}, true
}

// A child_map declares one resultContract per key; every other action type at most one.
type resultContract struct {
	address  string
	task     string
	old, new schema.Schema
	// added: declared where the old version had none, so reported directly, as a removed process
	// output is (§3a).
	added bool
	// parks is true where an instance can be sitting on this task when the version changes,
	// so the schema is part of the upgrade question as well as the contract one (§2c).
	parks bool
}

// addedChildKeyIssues is §2b's added-task rule one level down — the only key-set move with a
// rule; CLAUDE.md says why the other two need none.
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

// resultContracts ignores which process a child call names: `old ⊆ new` carries registration's
// premise forward (§2c). A DROPPED schema is not compared — it conforms less, turning nobody away.
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
			// `{}` constrains nothing: declaring it (the carried-but-unread idiom) is no addition.
			if (schema.Schema{}).IsSubset(*b) {
				return nil
			}
			rc.added = true
		} else {
			rc.old = *a
		}
		return []resultContract{rc}
	}
	// A fetch produces ONE value however many statuses declare it, so it is one contract. The
	// child_map below is not the same case: its keys are separately readable outputs.
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

// raiseContract is resultContract for the error channel: old ⊆ new, strictly, because the answer
// comes from OUTSIDE and nothing migrates it.
type raiseContract struct {
	address  string
	task     string
	code     string
	old, new *schema.Schema
	// declared is whether the new version names this code AT ALL. A dropped code is not a
	// narrowing, it is a refusal: the submission is rejected before its payload is looked at.
	declared bool
	parks    bool
}

// raiseContracts pairs the codes the OLD version declared. A code the new version adds
// constrains nobody -- no answer in flight can carry it.
func raiseContracts(old, new *model.Task) []raiseContract {
	if old.Action == nil || new.Action == nil {
		return nil
	}
	actionType := string(old.Action.Type)
	parks := parksMidTask(actionType)
	var out []raiseContract
	add := func(address string, oldR, newR model.Raises) {
		for _, code := range sortedRaiseCodes(oldR) {
			newSchema, declared := newR[code]
			out = append(out, raiseContract{
				address: address, task: old.ID, code: code,
				old: oldR[code], new: newSchema, declared: declared, parks: parks,
			})
		}
	}
	// A child_map declares per entry, since its entries can be different processes -- the same
	// split result_schema has.
	if old.Action.Type == model.ActionTypeChildMap {
		for _, key := range sortedChildKeys(old.Action.Children) {
			newChild, ok := new.Action.Children[key]
			if !ok {
				continue
			}
			add(childKeyAddress(old.ID, actionType, key)+".raises", old.Action.Children[key].Raises, newChild.Raises)
		}
		return out
	}
	add(old.ID+":"+actionType+".raises", old.Action.Raises, new.Action.Raises)
	return out
}

func sortedRaiseCodes(r model.Raises) []string {
	codes := make([]string, 0, len(r))
	for code := range r {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	return codes
}

func sortedChildKeys(children map[string]model.ChildEntry) []string {
	keys := make([]string, 0, len(children))
	for k := range children {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Both members deliberately: the producer never saw this schema, and a parked instance meets it
// on the way out.
const addedResultMessage = "added; a result written against the version that declared none may not satisfy it"

// resultIssues uses §3a's direction and the strict relation: a real conform stands there. Where the
// task parks (§2c) the upgrade break is a SECOND issue — they gate separately; the renderer merges.
func resultIssues(old, new *model.Task) []Issue {
	var out []Issue
	for _, rc := range resultContracts(old, new) {
		found := (explainer{}).explain(rc.old, rc.new)
		if rc.added {
			// No path: the finding is the schema arriving at all. Breaks against the zero schema
			// are not findings — nobody wrote it.
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
	out = append(out, raiseIssues(old, new)...)
	return out
}

// raiseIssues is resultIssues for the error channel. A finding's path names the code first, so
// one slot row covers every code the way one covers every property.
func raiseIssues(old, new *model.Task) []Issue {
	var out []Issue
	for _, rc := range raiseContracts(old, new) {
		var found []finding
		switch {
		case !rc.declared:
			// Not a narrowing: the submission is refused before its payload is read, and a child
			// that raised this code has nothing left to route on.
			found = []finding{{path: rc.code, msg: "code no longer declared"}}
		case rc.old == nil && rc.new != nil:
			// The old version promised no payload, so an answer carrying none is entitled to
			// exist. A new schema that accepts everything still accepts it.
			if !(schema.Schema{}).IsSubset(*rc.new) {
				found = []finding{{path: rc.code, msg: addedResultMessage}}
			}
		case rc.old != nil && rc.new != nil:
			for _, f := range (explainer{}).explain(*rc.old, *rc.new) {
				path := rc.code
				if f.path != "" {
					path += "." + f.path
				}
				found = append(found, finding{path: path, msg: f.msg})
			}
		}
		// Both members, as for a result: the submitter is the party narrowed. Registration does
		// not cover it — it checks a child's raisable codes, never an external worker's.
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

// A path is relative to the schema its address names (§6a), and that is the input, not
// inputObject's wrapper. The wrapper's own break (gaining an input at all) keeps no path.
func insideInput(path string) string {
	if path == "input" {
		return ""
	}
	return strings.TrimPrefix(path, "input.")
}

// inputObject's wrapper lets one relation answer both a newly required input and a changed one.
// It is never part of an address — see insideInput.
func inputObject(a analysis) schema.Schema {
	o := schema.Object()
	if !a.sf.ProcessInput.IsZero() {
		o = o.WithProperty("input", a.sf.ProcessInput, true)
	}
	return o.WithDefs(a.sf.Defs)
}

// compareOutput runs new ⊆ old. IsSubset, not NarrowsTo: narrowing needs a runtime conform
// behind the slot, and nothing conforms here.
func compareOutput(oldA, newA analysis) []finding {
	oldOut, hasOld, err := schemaFileOutput(oldA.sf)
	if err != nil {
		return []finding{{msg: err.Error()}}
	}
	newOut, hasNew, err := schemaFileOutput(newA.sf)
	if err != nil {
		return []finding{{msg: err.Error()}}
	}
	// Adding an output is free; removing one leaves no new schema to compare, so it is reported
	// directly, as a removed task is (§2b).
	if !hasOld {
		return nil
	}
	if !hasNew {
		return []finding{{msg: "removed; consumers were written against it"}}
	}
	return contractExplainer.explain(newOut, oldOut)
}

// CompareSet is Compare over a name-paired set; a single pair is one entry. The caller resolves
// and reconciles both sides first.
func CompareSet(old, new map[string]SideEntry) (SetReport, error) {
	report := SetReport{Compatible: true, Processes: []Report{}}

	// Status comes from versions alone, and only compared pairs are analysed: a legacy version
	// failing to analyse must not fail a report about two OTHER versions.
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
			unjudged(StatusNew)
			continue
		case !inNew, !moved(from, to):
			// Either the caller did not carry it over, or both sides landed on one version;
			// comparing a document with itself is a tautology.
			unjudged(StatusNothingToCompare)
			continue
		}

		// Only an unversioned (submitted) document is compared by content: a stored version pins
		// child versions too, so its number says more. Normalize makes raw and canonical agree.
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

// explainer only WORDS the breaks the relation reports, so a message cannot disagree with its
// verdict. CLAUDE.md has the three configurations.
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

// The path stays a field all the way to the renderer: recovering it by searching prose for a
// colon silently dropped any name containing a space.
type finding struct{ path, msg string }

// explain names EVERY place sub fails to fit super, in the relation's walk order; empty means it
// fits. One issue per run would mean one release per difference.
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

// ApplySelection sets each issue's Gating and computes Passes; verdicts never move. `contract` is
// the only token (the upgrade check is not negotiable, §5) and any other is an error, never a
// no-op. An unanalysable row always fails.
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
