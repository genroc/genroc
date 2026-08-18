package validation

import (
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
	if len(explicit) > 0 {
		return explicit, true
	}
	if success := a.SuccessPatterns(); len(success) > 0 {
		return success, true
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
	nullable := false
	for _, key := range sortedResponseKeys(a.Responses) {
		patterns, err := model.ParseResponseKey(key)
		if err != nil {
			continue // refused at registration
		}
		if !anyStatusMatching(patterns, func(code int) bool { return isAccepted(code, accepted, static) }) {
			continue
		}
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

// errorDataType is rule 2 for the error channel, restricted to the statuses codes can reach.
// The union is over what the handler's on_error patterns can match, so a rule naming one
// status gets exactly that status's schema.
func errorDataType(a *model.Action, defs schema.Defs, reaches func(code int) bool) (schema.Schema, bool, error) {
	if len(a.Responses) == 0 {
		return schema.Schema{}, false, nil
	}
	accepted, static := acceptedPatterns(a)
	var arms []schema.Schema
	nullable := false
	declaredAny := false
	for _, key := range sortedResponseKeys(a.Responses) {
		patterns, err := model.ParseResponseKey(key)
		if err != nil {
			continue
		}
		hit := anyStatusMatching(patterns, func(code int) bool {
			return !isAcceptedStrict(code, accepted, static) && reaches(code)
		})
		if !hit {
			continue
		}
		declaredAny = true
		sc := a.Responses[key]
		if sc == nil {
			nullable = true
			continue
		}
		merged, err := sc.MergeInto(defs)
		if err != nil {
			return schema.Schema{}, false, err
		}
		arms = append(arms, merged)
	}
	if !declaredAny {
		return schema.Schema{}, false, nil
	}
	if len(arms) == 0 {
		return schema.Type("null"), true, nil
	}
	if nullable {
		arms = append(arms, schema.Type("null"))
	}
	if len(arms) == 1 {
		return arms[0], true, nil
	}
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
