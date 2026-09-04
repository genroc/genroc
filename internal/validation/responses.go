package validation

import (
	"sort"
	"strings"

	"genroc/internal/errcode"
	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/template"
)

// The status space a fetch can answer with. Coverage, reachability and the success/failure
// split are all decided by walking it rather than by reasoning about the pattern syntax —
// exact where a syntactic rule would be approximate, and cheap enough to run per task.
const (
	minStatus = 100
	maxStatus = 599
)

// staticAcceptedStatus returns the patterns accepted_status names, and whether the set is
// knowable at all. static=false is the generic-poller shape — the caller supplies the
// accepted set at runtime ([specs] §2) — and nothing about which statuses succeed can be
// decided here. An absent slot IS static: the 2xx default is a known set.
func staticAcceptedStatus(a *model.Action) ([]string, bool) {
	if !a.AcceptedStatus.Present() {
		return nil, true
	}
	elems, ok := a.AcceptedStatus.Raw.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(elems))
	for _, el := range elems {
		s, ok := el.(string)
		if !ok {
			return nil, false
		}
		t, err := template.Get(s)
		if err != nil {
			return nil, false
		}
		pat, isStatic := t.Static()
		if !isStatic {
			return nil, false
		}
		out = append(out, pat)
	}
	return out, true
}

// acceptedPatterns is rule 1: accepted_status when present, otherwise the 2xx patterns of
// responses, otherwise every 2xx. The bool is whether the answer is knowable statically.
func acceptedPatterns(a *model.Action) ([]string, bool) {
	explicit, static := staticAcceptedStatus(a)
	if !static {
		return nil, false
	}
	if effective := a.EffectiveAcceptedStatus(explicit); len(effective) > 0 {
		return effective, true
	}
	return []string{"2xx"}, true
}

// isAccepted reports whether code succeeds. Under a dynamic accepted_status every status is
// possibly-accepted AND possibly-not, which is why a declared schema then appears on both
// channels: the honest answer is both, not a guess at one.
func isAccepted(code int, accepted []string, static bool) bool {
	if !static {
		return true
	}
	return model.MatchAnyStatus(code, accepted)
}

// fetchResultType is rule 2 for the success channel: the union of the schemas declared for
// statuses that can be accepted, plus a null arm where an accepted status is described by no
// pattern. Coverage is what makes {"2xx": T} exactly T — no null arm — without enumerating
// 200, 201 and 204. typed=false only when nothing is declared at all, which keeps a fetch
// with no responses exactly as untyped as one with no result_schema used to be.
func fetchResultType(a *model.Action, defs schema.Defs) (schema.Schema, bool, error) {
	if len(a.Responses) == 0 {
		return schema.Schema{}, false, nil
	}
	accepted, static := acceptedPatterns(a)

	var arms []schema.Schema
	nullable, describesAccepted := false, false
	for _, key := range sortedResponseKeys(a.Responses) {
		patterns, err := model.ParseResponseKey(key)
		if err != nil {
			continue // refused at registration
		}
		if !anyStatusMatching(patterns, func(code int) bool { return isAccepted(code, accepted, static) }) {
			continue
		}
		describesAccepted = true
		sc := a.Responses[key]
		if sc == nil {
			nullable = true // declared to carry no body
			continue
		}
		merged, err := sc.MergeInto(defs)
		if err != nil {
			return schema.Schema{}, false, err
		}
		arms = append(arms, merged)
	}

	// Declaring only error statuses says nothing about what a success carries, so the body
	// that arrives is undeclared — exactly as if no responses were given. Typing it `null`
	// instead would be a claim the runtime contradicts: the 2xx default still accepts the
	// response, and its body reaches self.result unvalidated.
	if !describesAccepted {
		return schema.Schema{}, false, nil
	}

	// An accepted status no pattern describes carries a body nothing typed, so the union
	// admits null. A dynamic accepted set can always land on one, hence never covered.
	if !static {
		nullable = true
	} else {
		for code := minStatus; code <= maxStatus; code++ {
			if !model.MatchAnyStatus(code, accepted) {
				continue
			}
			if _, declared := a.ResponseFor(code); !declared {
				nullable = true
				break
			}
		}
	}

	switch {
	case len(arms) == 0:
		return schema.Type("null"), true, nil
	case len(arms) == 1 && !nullable:
		return arms[0], true, nil
	case len(arms) == 1:
		return arms[0].WithNull(), true, nil
	}
	if nullable {
		arms = append(arms, schema.Type("null"))
	}
	// anyOf, never oneOf: two status bodies routinely overlap (objects whose properties are
	// all optional both admit {}), and oneOf means EXACTLY one arm — an overlapping union
	// would reject a value that fits two of them.
	return schema.AnyOf(arms...), true, nil
}

// isAcceptedStrict is isAccepted for the error channel: under a dynamic accepted set a status
// is possibly-unaccepted, so it reaches error.data as well as self.result.
func isAcceptedStrict(code int, accepted []string, static bool) bool {
	if !static {
		return false
	}
	return model.MatchAnyStatus(code, accepted)
}

func anyStatusMatching(patterns []string, pred func(int) bool) bool {
	for code := minStatus; code <= maxStatus; code++ {
		for _, p := range patterns {
			if model.MatchStatusPattern(p, code) && pred(code) {
				return true
			}
		}
	}
	return false
}

// untypedResultAdvice words the "self.result is not available" message for the slot that
// would have typed it. A fetch types per status, so pointing its author at result_schema —
// a field a fetch now refuses — would send them somewhere they cannot go.
func untypedResultAdvice(a *model.Action) string {
	if a != nil && a.Type == model.ActionTypeFetch {
		return "the action declares no responses — add `responses: {\"2xx\": {...}}` to type the body, or `{}` (the top type) to export it opaquely for a caller to narrow"
	}
	return "the action has no result_schema — add a result_schema to type the response, or `result_schema: {}` (the top type) to export it opaquely for a caller to narrow"
}

// fetchResultContract is the schema a fetch's result is compared AS: the merged union, not
// the per-status parts. The statuses are not separately observable — every declared body
// feeds the one self.result — so comparing them one at a time judges something no consumer
// can read, and it goes wrong in both directions. Dropping a bodyless status narrows the
// union (harmless) while reading per-status as a removed declaration; adding one widens it
// to `T | null` (a real break for every consumer) while carrying no schema to compare at all.
// nil means the fetch declares nothing, which is what an absent result_schema always meant.
func fetchResultContract(a *model.Action) (*schema.Schema, error) {
	if a == nil || a.Type != model.ActionTypeFetch || len(a.Responses) == 0 {
		return nil, nil
	}
	pool := schema.NewDefs()
	sc, typed, err := fetchResultType(a, pool)
	if err != nil || !typed {
		return nil, err
	}
	// Bake the pool back in: a compared schema is read on its own, with no context to
	// resolve a bare #/$defs ref against.
	out := sc.WithMergedDefs(pool)
	return &out, nil
}

// nonStatusProbes are the catchable codes a fetch can report that carry no response body.
// A rule whose patterns reach any of them can arrive at its handler with nothing in hand, so
// error.data admits null — this is the list `%` and `http.%` are caught by. Child raise codes
// are not here: they belong to a child task, whose error.data is absent outright.
var nonStatusProbes = []errcode.Code{
	errcode.HTTPTimeout, errcode.PreTimeout, errcode.PreError,
	errcode.OutputParse, errcode.OutputTooLarge, errcode.OutputInvalid,
	errcode.OnlyOnceInterrupted,
}

func ruleCatches(rule model.ErrorCase, code errcode.Code) bool {
	if len(rule.Code) == 0 {
		return true // empty list is the catch-all
	}
	for _, p := range rule.Code {
		if errcode.MatchCode(p, string(code)) {
			return true
		}
	}
	return false
}

// ruleErrorData is one rule's contribution to error.data: the bodies its patterns can catch,
// and whether it can also arrive carrying nothing. A source that is not a fetch declaring
// statuses contributes only the null — there is no body on that path to type.
func ruleErrorData(t *model.Task, rule model.ErrorCase, defs schema.Defs) ([]schema.Schema, bool, error) {
	a := t.Action
	if a == nil {
		return nil, true, nil
	}
	if a.Type != model.ActionTypeFetch {
		return childRuleErrorData(a, rule, defs)
	}
	if len(a.Responses) == 0 {
		return nil, true, nil
	}
	accepted, static := acceptedPatterns(a)
	unaccepted := func(code int) bool { return !isAcceptedStrict(code, accepted, static) }

	var arms []schema.Schema
	nullable := false
	for _, key := range sortedResponseKeys(a.Responses) {
		patterns, err := model.ParseResponseKey(key)
		if err != nil {
			continue
		}
		// Walk the keys rather than the codes: a "4xx" declaration covers a hundred statuses
		// and must contribute its schema once, not a hundred identical arms.
		if !anyStatusMatching(patterns, func(code int) bool {
			return unaccepted(code) && ruleCatches(rule, errcode.HTTP(code))
		}) {
			continue
		}
		sc := a.Responses[key]
		if sc == nil {
			nullable = true
			continue
		}
		merged, err := sc.MergeInto(defs)
		if err != nil {
			return nil, false, err
		}
		arms = append(arms, merged)
	}
	// A status this rule catches that no key describes arrives with an unreadable body.
	for code := minStatus; code <= maxStatus && !nullable; code++ {
		if !unaccepted(code) || !ruleCatches(rule, errcode.HTTP(code)) {
			continue
		}
		if _, declared := a.ResponseFor(code); !declared {
			nullable = true
		}
	}
	for _, probe := range nonStatusProbes {
		if ruleCatches(rule, probe) {
			nullable = true
			break
		}
	}
	return arms, nullable, nil
}

// childRuleErrorData is ruleErrorData for the tasks that declare `raises` — the child family
// and external, whose payload shapes the CALLER declared for the codes this rule can catch.
// Any other action type declares nothing, so it contributes only the null.
func childRuleErrorData(a *model.Action, rule model.ErrorCase, defs schema.Defs) ([]schema.Schema, bool, error) {
	decl, partial := declaredRaises(a)
	if len(decl) == 0 {
		return nil, true, nil
	}
	var arms []schema.Schema
	nullable := reachesUndeclaredCode(rule, decl)
	for _, code := range sortedDeclaredCodes(decl) {
		if !ruleCatches(rule, errcode.Code(code)) {
			continue
		}
		// One entry of a child_map declaring a code says nothing about the entry that
		// actually raised it: that one may declare no shape, and its payload is then absent
		// at runtime. The arm admits null so the handler cannot read a slot that is not there.
		nullable = nullable || partial[code]
		for _, sc := range decl[code] {
			// A nil declaration is `raises: {code: null}` — the code is declared and carries
			// nothing. It contributes no ARM, so a rule catching only such codes types
			// error.data as absent; caught alongside a code that does carry a payload it
			// admits null, since the handler cannot know which one arrived.
			if sc == nil {
				nullable = true
				continue
			}
			merged, err := sc.MergeInto(defs)
			if err != nil {
				return nil, false, err
			}
			arms = append(arms, merged)
		}
	}
	return arms, nullable, nil
}

// reachesUndeclaredCode: whether this rule can fire on a code no key declares, which is what
// puts the null arm in. A wildcard counts even where the child happens to raise only declared
// codes — the raise set belongs to another definition and is not read here, so the answer
// stays conservative in the direction that costs a narrowing rather than a wrong type.
// output.invalid is an undeclared literal like any other and falls out of the same rule.
func reachesUndeclaredCode(rule model.ErrorCase, decl map[string][]*schema.Schema) bool {
	if len(rule.Code) == 0 {
		return true // the catch-all reaches everything
	}
	for _, p := range rule.Code {
		if strings.ContainsRune(p, '%') || len(decl[p]) == 0 {
			return true
		}
	}
	return false
}

// declaredRaises collects a child task's declarations, keyed by raise code, plus the codes
// only SOME entries of a child_map declare: the entries can be different processes, so one
// code's payload is whatever the entry that raised it declared — the type is the union over
// them, and a gap in that cover is what `partial` reports.
func declaredRaises(a *model.Action) (decl map[string][]*schema.Schema, partial map[string]bool) {
	switch a.Type {
	case model.ActionTypeChild, model.ActionTypeChildList, model.ActionTypeExternal:
		if len(a.Raises) == 0 {
			return nil, nil
		}
		decl = make(map[string][]*schema.Schema, len(a.Raises))
		for code, sc := range a.Raises {
			decl[code] = []*schema.Schema{sc}
		}
		return decl, nil
	case model.ActionTypeChildMap:
		decl, partial = map[string][]*schema.Schema{}, map[string]bool{}
		keys := make([]string, 0, len(a.Children))
		for key := range a.Children {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			entry := a.Children[key]
			codes := make([]string, 0, len(entry.Raises))
			for code := range entry.Raises {
				codes = append(codes, code)
			}
			sort.Strings(codes)
			for _, code := range codes {
				decl[code] = append(decl[code], entry.Raises[code])
			}
		}
		for code := range decl {
			partial[code] = len(decl[code]) < len(a.Children)
		}
		return decl, partial
	}
	return nil, nil
}

func sortedDeclaredCodes(decl map[string][]*schema.Schema) []string {
	codes := make([]string, 0, len(decl))
	for code := range decl {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

// errorDataSchema types error.data at a task: the union over every on_error rule that can
// have set the `error` visible there. One rule reaching one handler gives exactly that rule's
// schema; widening the patterns, or letting a second rule reach the same handler, widens the
// type — which is the narrowing story the design leans on, done by the rules themselves
// rather than by any type-system machinery. A zero schema means no reaching rule declares a
// body, and error.data is then absent entirely (undeclared data is never accessible).
func errorDataSchema(tasks []*model.Task, srcs []errSource, defs schema.Defs) (schema.Schema, error) {
	var arms []schema.Schema
	nullable, any := false, false
	for _, src := range srcs {
		if src.task < 0 || src.task >= len(tasks) {
			continue
		}
		t := tasks[src.task]
		if src.rule < 0 || src.rule >= len(t.OnError) {
			continue
		}
		ruleArms, ruleNull, err := ruleErrorData(t, t.OnError[src.rule], defs)
		if err != nil {
			return schema.Schema{}, err
		}
		arms = append(arms, ruleArms...)
		nullable = nullable || ruleNull
		any = any || len(ruleArms) > 0
	}
	if !any {
		return schema.Schema{}, nil
	}
	return combineErrData(arms, nullable), nil
}

// combineErrData joins the bodies an `error.data` slot can hold.
//
// One body reads as a nullable body — the same spelling the success channel uses. Only a
// union of two BODIES needs anyOf, where the arms can overlap. These schemas are served,
// so the two channels must not describe one concept two ways.
func combineErrData(arms []schema.Schema, nullable bool) schema.Schema {
	if len(arms) == 0 {
		return schema.Schema{}
	}
	if len(arms) == 1 {
		if nullable {
			return arms[0].WithNull()
		}
		return arms[0]
	}
	if nullable {
		arms = append(arms, schema.Type("null"))
	}
	return schema.AnyOf(arms...)
}

// ruleErrAt is what `error` looks like INSIDE one on_error rule, as opposed to on entry to
// a task the rule routes to: it is always present (the rule only runs because the task
// failed), and its data is what this rule alone can catch.
func ruleErrAt(t *model.Task, rule model.ErrorCase, defs schema.Defs) errAt {
	arms, nullable, err := ruleErrorData(t, rule, defs)
	if err != nil {
		return errAt{must: true}
	}
	return errAt{must: true, data: combineErrData(arms, nullable)}
}

// errAt is what `error` looks like on entry to one task: whether it is always or possibly
// present, and the type of its `data` slot. A zero Data means no reaching rule declares a
// body, and `error.data` is then absent from the context entirely.
type errAt struct {
	must bool
	may  bool
	data schema.Schema
}

// errContexts resolves the per-task error facts once, so every context built for a task
// agrees about what `error` holds there — an output projection and a switch case that
// disagreed would accept an expression in one slot and reject it in the other.
// A schema that fails to merge leaves the slot ABSENT rather than taking the caller down:
// an expression reading it then fails loudly as "not in schema", which is the safe
// direction, and the same merge runs on the success channel where the error does surface.
func errContexts(tasks []*model.Task, mustErr, mayErr map[string]bool, errSrc map[string][]errSource, defs schema.Defs) map[string]errAt {
	out := make(map[string]errAt, len(tasks))
	for _, t := range tasks {
		data, err := errorDataSchema(tasks, errSrc[t.ID], defs)
		if err != nil {
			data = schema.Schema{}
		}
		out[t.ID] = errAt{must: mustErr[t.ID], may: mayErr[t.ID], data: data}
	}
	return out
}
