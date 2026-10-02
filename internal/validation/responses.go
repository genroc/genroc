package validation

import (
	"sort"
	"strings"

	"genroc/internal/errcode"
	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/template"
)

// Coverage, reachability and the success split walk the whole status space rather than reason
// about pattern syntax: exact, and cheap enough per task.
const (
	minStatus = 100
	maxStatus = 599
)

// static=false is the generic-poller shape: the caller supplies the accepted set at runtime
// (specs/fetch-http-surface.md §2). An absent slot IS static — the 2xx default is known.
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

// Under a dynamic accepted_status every status is possibly accepted AND possibly not, so a
// declared schema appears on both channels.
func isAccepted(code int, accepted []string, static bool) bool {
	if !static {
		return true
	}
	return model.MatchAnyStatus(code, accepted)
}

// fetchResultType: the union of bodies declared for statuses that can be accepted, plus null where
// an accepted status has no pattern — so {"2xx": T} is exactly T. typed=false only when nothing
// is declared. specs/fetch-http-surface.md §2.
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

	// Only error statuses declared: the success body is undeclared. Not `null` — the 2xx default
	// still accepts the response, and its body reaches self.result unvalidated.
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
	// anyOf, never oneOf: status bodies routinely overlap.
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

// A fetch types per status: pointing its author at result_schema, which a fetch refuses, would
// send them nowhere.
func untypedResultAdvice(a *model.Action) string {
	if a == nil {
		return "a task with no action has no result to read"
	}
	if a.Type == model.ActionTypeDelay {
		return "a delay waits and hands nothing back — there is no result"
	}
	if a.Type == model.ActionTypeFetch {
		return "the action declares no responses — add `responses: {\"2xx\": {...}}` to type the body, or `{}` (the top type) to export it opaquely for a caller to narrow"
	}
	return "the action has no result_schema — add a result_schema to type the response, or `result_schema: {}` (the top type) to export it opaquely for a caller to narrow"
}

// Compared AS the merged union: every body feeds one self.result, so comparing statuses one at a
// time judges what no consumer reads. nil means nothing is declared.
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

// Catchable fetch codes with no response body: a rule reaching one can arrive with nothing, so
// error.data admits null. No child raise codes — a child task's error.data is absent outright.
var nonStatusProbes = []errcode.Code{
	errcode.HTTPTimeout, errcode.PreTimeout, errcode.PreError,
	errcode.ResultParse, errcode.ResultTooLarge, errcode.ResultInvalid,
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

// ruleErrorData: the bodies one rule's patterns can catch, and whether it can arrive carrying
// nothing.
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

// childRuleErrorData: the child family and external, whose payload shapes the CALLER declared in
// `raises`. Any other action type contributes only the null.
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
		// The child_map entry that raised a code may not declare it, so a partial code admits
		// null.
		nullable = nullable || partial[code]
		for _, sc := range decl[code] {
			// `raises: {code: null}` adds no ARM: caught alone, error.data is absent; caught
			// beside a code with a payload, it admits null.
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

// A wildcard counts even if the child raises only declared codes: its raise set is another
// definition's, so this stays conservative — costing a narrowing, never a wrong type.
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

// partial marks codes only SOME child_map entries declare: entries can be different processes,
// so a code's type is the union over them, and partial is a gap in that cover.
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

// errorDataSchema unions every on_error rule that can have set `error` here, so the rules do the
// narrowing. A zero schema means no reaching rule declares a body: error.data is absent.
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

// One body reads as a nullable body, as on the success channel; only two BODIES need anyOf.
// These schemas are served, so the two channels must not spell one concept two ways.
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

// Inside a rule `error` is always present (it runs only because the task failed), and its data
// is what this rule alone can catch.
func ruleErrAt(t *model.Task, rule model.ErrorCase, defs schema.Defs) errAt {
	arms, nullable, err := ruleErrorData(t, rule, defs)
	if err != nil {
		return errAt{must: true}
	}
	return errAt{must: true, data: combineErrData(arms, nullable)}
}

// errAt is `error` on entry to one task. A zero data means no reaching rule declares a body, so
// `error.data` is absent from the context.
type errAt struct {
	must bool
	may  bool
	data schema.Schema
}

// A schema that fails to merge leaves the slot ABSENT rather than failing the caller: a read then
// fails loudly, and the same merge surfaces the error on the success channel.
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
