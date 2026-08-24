package model

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"genroc/internal/delayspec"
	"genroc/internal/errcode"
	"genroc/internal/schema"

	"github.com/go-playground/validator/v10"
)

// Validate checks the definition and its tasks against the struct-tag rules, that attached
// JSON Schemas are well-formed, and that switch goto targets name known tasks.
func (d *ProcessDefinition) Validate() error {
	if err := fmtValidationErr(v.Struct(d)); err != nil {
		return err
	}
	if err := d.validateDefs(); err != nil {
		return err
	}
	if err := checkSchemaDoc("input_schema", d.InputSchema, d.Defs); err != nil {
		return err
	}
	if err := checkSchemaDocAllowingSecrets("config_schema", d.ConfigSchema, schema.Defs{}); err != nil {
		return err
	}
	if err := validateConfigSchema(d.ConfigSchema); err != nil {
		return err
	}
	taskIDs := make(map[string]struct{}, len(d.Tasks))
	for _, s := range d.Tasks {
		taskIDs[s.ID] = struct{}{}
	}
	lastIdx := len(d.Tasks) - 1
	for i, s := range d.Tasks {
		if err := validateTask(s, taskIDs, i, lastIdx, d.Defs); err != nil {
			return err
		}
	}
	return d.validateFaultCodeKinds()
}

// validateFaultCodeKinds enforces R6: within one definition a code is a raise code or a
// panic code, never both — the same value on 'raised' and 'failed' instances would mean
// two things to the dashboards error_code exists for. Runs after per-task validation.
func (d *ProcessDefinition) validateFaultCodeKinds() error {
	raisedBy := map[string]string{} // code → first task that raises it
	for _, s := range d.Tasks {
		for _, c := range s.Switch {
			if c.Raise != nil {
				if _, seen := raisedBy[c.Raise.Code]; !seen {
					raisedBy[c.Raise.Code] = s.ID
				}
			}
		}
		for _, ec := range s.OnError {
			if ec.Raise != nil {
				if _, seen := raisedBy[ec.Raise.Code]; !seen {
					raisedBy[ec.Raise.Code] = s.ID
				}
			}
		}
	}
	check := func(f *Fault, taskID string) error {
		if f == nil {
			return nil
		}
		if origin, ok := raisedBy[f.Code]; ok {
			return fmt.Errorf("task %q: panic %q: this code is already raised by task %q; a code cannot be both raised and panicked", taskID, f.Code, origin)
		}
		return nil
	}
	for _, s := range d.Tasks {
		for _, c := range s.Switch {
			if err := check(c.Panic, s.ID); err != nil {
				return err
			}
		}
		for _, ec := range s.OnError {
			if err := check(ec.Panic, s.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateTask(s *Task, taskIDs map[string]struct{}, taskIdx, lastIdx int, pool schema.Defs) error {
	// Reserved task IDs.
	if s.ID == GotoEnd || s.ID == GotoNext {
		return fmt.Errorf("task ID %q is reserved", s.ID)
	}
	if err := validateActionRequiredFields(s); err != nil {
		return err
	}
	if err := validateTimeout(s); err != nil {
		return err
	}
	if err := validateSwitch(s, taskIDs, taskIdx, lastIdx); err != nil {
		return err
	}
	if err := validateOnError(s, taskIDs); err != nil {
		return err
	}
	if err := validateFetchOnlySlots(s); err != nil {
		return err
	}
	if err := validateResponses(s, pool); err != nil {
		return err
	}
	if err := validateRaises(s, pool); err != nil {
		return err
	}
	return validateActionSchemas(s, pool)
}

// validateTimeout: which action types honour a timeout at all — a silently unapplied
// deadline is the failure this check exists for — and `until` confined to external, the
// one type where a past deadline coherently means "due now". CLAUDE.md has the asymmetry.
func validateTimeout(s *Task) error {
	if s.Timeout.IsZero() {
		return nil
	}
	if s.Action == nil {
		return fmt.Errorf("task %q: timeout is not valid on a switch-only task — there is no call for it to bound", s.ID)
	}
	switch s.Action.Type {
	case ActionTypeFetch:
		if s.Timeout.Until != nil {
			return fmt.Errorf("task %q: timeout.until is only valid on an external task — a fetch deadline that has already passed would report http.timeout for a request that was never sent. Use %s for a budget per attempt", s.ID, `timeout: "30s"`)
		}
	case ActionTypeExternal:
		// Both slots: an external parks on an instant, so a fixed deadline is as meaningful
		// as a budget.
	default:
		return fmt.Errorf("task %q: timeout is not honoured on a %q task — only fetch and external tasks are bounded by one", s.ID, s.Action.Type)
	}
	return nil
}

func validateActionRequiredFields(s *Task) error {
	if s.Action == nil {
		return nil
	}
	switch s.Action.Type {
	case ActionTypeFetch:
		if s.Action.URL == "" {
			return fmt.Errorf("task %q: action.url is required for type %q", s.ID, s.Action.Type)
		}
	case ActionTypeChildMap:
		if len(s.Action.Children) == 0 {
			return fmt.Errorf("task %q: action.children is required for type %q", s.ID, s.Action.Type)
		}
		for key, entry := range s.Action.Children {
			if entry.Name == "" {
				return fmt.Errorf("task %q: action.children[%q].name is required", s.ID, key)
			}
		}
	case ActionTypeChild:
		if s.Action.Name == "" {
			return fmt.Errorf("task %q: action.name is required for type %q", s.ID, s.Action.Type)
		}
	case ActionTypeChildList:
		if s.Action.Name == "" {
			return fmt.Errorf("task %q: action.name is required for type %q", s.ID, s.Action.Type)
		}
		if s.Action.Over == "" {
			return fmt.Errorf("task %q: action.over is required for type %q", s.ID, s.Action.Type)
		}
	case ActionTypeDelay:
		hasFor, hasUntil := s.Action.For != nil, s.Action.Until != nil
		switch {
		case hasFor && hasUntil:
			return fmt.Errorf("task %q: action.for and action.until are mutually exclusive — %q is a duration from now, %q is an instant", s.ID, "for", "until")
		case !hasFor && !hasUntil:
			return fmt.Errorf("task %q: action.for or action.until is required for type %q", s.ID, s.Action.Type)
		}
		if s.Action.TZ != "" {
			if _, err := delayspec.LoadLocation(s.Action.TZ); err != nil {
				return fmt.Errorf("task %q: action.%v", s.ID, err)
			}
		}
	case ActionTypeExternal:
		// No required action fields: input and result_schema are both optional
		// (mirroring fetch). The wait is bounded by the task's timeout, absent = forever;
		// validateTimeout owns its rules, including that this is the one type taking `until`.
	default:
		return fmt.Errorf("task %q: action.type must be one of: fetch, child, child_map, child_list, delay, external", s.ID)
	}
	return nil
}

// faultCodeRe is the R1 shape for an authored error code: lower_snake_case. The two
// excluded characters are load-bearing — '.' spells engine codes, so forbidding it keeps
// the namespaces distinct and stops a raise mirroring a system code; '%' is the on_error
// wildcard, so keeping it out means no pattern ever needs escaping.
//
// It is also what enforces R2: a computed code would make the raise set uncomputable and
// error_code unqueryable, and no expression can be spelled in lower_snake_case.
var faultCodeRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ValidFaultCode reports whether s is a well-formed authored error code. Exported for the
// external-tasks fail API, whose submitter is an outside worker rather than a definition:
// without this check a worker could send "http.500" or "external.timeout" and impersonate an
// engine code, including the unknowable ones an only_once task can never retry.
func ValidFaultCode(s string) bool { return faultCodeRe.MatchString(s) }

// validateFault enforces R1 (code shape, message present) on one raise or panic clause.
// where locates the case ("switch case 0", "on_error[1]") and clause names it, so the
// message points at the offending line without the author having to count.
//
// R2 (the code is a literal) needs no check here: no expression survives faultCodeRe. The
// MESSAGE and DATA are evaluated when the clause fires, so they are type-checked where
// their scope is known — validation.checkFaultClauses, not this package. See
// specs/child-error-handling.md R2.
func validateFault(f *Fault, taskID, where, clause string) error {
	if f == nil {
		return nil
	}
	// '.' and '%' get their own messages: each is not merely an invalid character but one
	// with a specific meaning (an engine-code separator; the on_error wildcard), so the
	// message says why rather than a generic "invalid code".
	if strings.Contains(f.Code, ".") {
		return fmt.Errorf("task %q %s: %s: %q must not contain '.' — dots are reserved for engine-produced codes; give the error a semantic lower_snake_case name of its own rather than re-raising a system code", taskID, where, clause, f.Code)
	}
	if strings.Contains(f.Code, "%") {
		return fmt.Errorf("task %q %s: %s: %q must not contain '%%' — it is the on_error match wildcard, so a code containing it could never be caught", taskID, where, clause, f.Code)
	}
	if !faultCodeRe.MatchString(f.Code) {
		return fmt.Errorf("task %q %s: %s: %q is not a valid error code (lower_snake_case, no dots)", taskID, where, clause, f.Code)
	}
	if f.Message == "" {
		return fmt.Errorf("task %q %s: %s %q: message is required", taskID, where, clause, f.Code)
	}
	return nil
}

// validateRetry checks a retry policy's internal coherence. The decoder rejects the same
// shapes on the JSON path; this is the half that covers definitions built in Go and, more
// importantly, the half whose message can name the task and the rule.
func validateRetry(r Retry, taskID, where string) error {
	if r.IsZero() {
		return nil
	}
	// Every bound below is guarded on the slot being a literal: an expression has no value
	// until the rule fires, so Retry.Resolve repeats each of these at runtime.
	if !r.Attempts.IsExpr() {
		if r.Attempts.Literal() < 0 {
			return fmt.Errorf("task %q %s: retry.attempts must not be negative", taskID, where)
		}
		// A curve without attempts never runs. Refused rather than defaulted, because the
		// alternative is an authored backoff that silently does nothing.
		if r.Attempts.Literal() == 0 {
			return fmt.Errorf("task %q %s: retry names a backoff but no attempts, so it would never retry; add attempts, or drop retry entirely", taskID, where)
		}
	}
	if !r.Factor.IsExpr() && r.Factor.Literal() != 0 && r.Factor.Literal() < 1 {
		return fmt.Errorf("task %q %s: retry.factor %g would shrink the wait after every attempt; use 1 for a constant delay", taskID, where, r.Factor.Literal())
	}
	// Checked against the authored slots, not against Ceiling(), which papers over exactly
	// this by widening a default cap to fit the base.
	if !r.Delay.IsExpr() && !r.MaxDelay.IsExpr() {
		if base, capped := r.Delay.Duration(), r.MaxDelay.Duration(); base > 0 && capped > 0 && capped < base {
			return fmt.Errorf("task %q %s: retry.max_delay (%s) is shorter than retry.delay (%s), so the first wait would already be clamped and the delay never applied", taskID, where, capped, base)
		}
	}
	return nil
}

// validateSwitch checks the task's switch cases: catch-all ordering, goto targets, and
// the raise/panic clauses (R1-R3).
func validateSwitch(s *Task, taskIDs map[string]struct{}, taskIdx, lastIdx int) error {
	if len(s.Switch) == 0 {
		return fmt.Errorf("task %q: switch is required", s.ID)
	}
	for i, c := range s.Switch {
		isLast := i == len(s.Switch)-1
		if c.Case == "" && !isLast {
			return fmt.Errorf("task %q switch: catch-all at index %d must be the last case (unreachable cases after it)", s.ID, i)
		}
		where := fmt.Sprintf("switch case %d", i)

		// R3: a case either routes or terminates, never both and never neither. This
		// is checked here rather than on decode so it can name the task and the index.
		set := 0
		for _, on := range []bool{c.Goto != "", c.Raise != nil, c.Panic != nil} {
			if on {
				set++
			}
		}
		if set != 1 {
			return fmt.Errorf("task %q %s: set exactly one of \"goto\", \"raise\", \"panic\"", s.ID, where)
		}
		if err := validateFault(c.Raise, s.ID, where, "raise"); err != nil {
			return err
		}
		if err := validateFault(c.Panic, s.ID, where, "panic"); err != nil {
			return err
		}
		if c.Goto == "" {
			continue // a raise/panic case has no routing target to check
		}

		switch {
		case c.Goto == GotoEnd:
			// always valid
		case c.Goto == GotoNext:
			if taskIdx == lastIdx {
				return fmt.Errorf("task %q switch: 'next' is not allowed on the last task; use 'end' to terminate", s.ID)
			}
		case strings.HasPrefix(c.Goto, "$"):
			taskID := c.Goto[1:]
			if _, ok := taskIDs[taskID]; !ok {
				return fmt.Errorf("task %q switch: goto %q is not a known task", s.ID, c.Goto)
			}
		default:
			return fmt.Errorf("task %q switch: goto %q must be \"end\", \"next\", or a task reference like \"$task-id\"", s.ID, c.Goto)
		}
	}
	if s.Switch[len(s.Switch)-1].Case != "" {
		return fmt.Errorf("task %q switch: last case must be a catch-all (omit 'case' to match unconditionally)", s.ID)
	}
	return nil
}

// isChildTask reports whether the task's action spawns child processes, which is what
// makes its on_error a list of raised codes rather than engine codes (R4/M1).
func isChildTask(s *Task) bool {
	return s.Action != nil && (s.Action.Type == ActionTypeChild || s.Action.Type == ActionTypeChildMap || s.Action.Type == ActionTypeChildList)
}

// validateOnError checks a task's on_error rules: terminal-clause arity (R3), pattern shape,
// catch-all last, goto targets, and the task-kind rules. Both kinds share the pattern syntax;
// only what codes are checked against differs — a child's raise set (R5) vs an open code space.
func validateOnError(s *Task, taskIDs map[string]struct{}) error {
	onlyOnce := s.OnlyOnce != nil && *s.OnlyOnce
	child := isChildTask(s)
	for i, ec := range s.OnError {
		where := fmt.Sprintf("on_error[%d]", i)

		// R3, in its at-most-one form. Unlike a switch case, a rule setting none of the
		// three is meaningful and long-standing: on an action task it exhausts its retries
		// and then fails the instance with the engine's own code. What must not happen is
		// two answers to "what does this rule do".
		set := 0
		for _, on := range []bool{ec.Goto != "", ec.Raise != nil, ec.Panic != nil} {
			if on {
				set++
			}
		}
		if set > 1 {
			return fmt.Errorf("task %q %s: set at most one of \"goto\", \"raise\", \"panic\"", s.ID, where)
		}
		if err := validateFault(ec.Raise, s.ID, where, "raise"); err != nil {
			return err
		}
		if err := validateFault(ec.Panic, s.ID, where, "panic"); err != nil {
			return err
		}

		// Code shape: each entry is a non-empty LIKE pattern; an empty list is a catch-all
		// and must be last. Common to both task kinds.
		for _, pat := range ec.Code {
			if !validLikePattern(pat) {
				return fmt.Errorf("task %q %s: code pattern must not be empty", s.ID, where)
			}
		}
		isLast := i == len(s.OnError)-1
		if len(ec.Code) == 0 && !isLast {
			return fmt.Errorf("task %q %s: catch-all must be the last rule (unreachable rules after it)", s.ID, where)
		}

		if ec.Goto != "" && ec.Goto != GotoEnd {
			if _, ok := taskIDs[ec.Goto]; !ok {
				return fmt.Errorf("task %q %s: goto %q is not a known task", s.ID, where, ec.Goto)
			}
		}

		if child {
			// D7: no parent-side retry — re-spawning a batch is not a retry, so the field would be
			// silently ignored. Refused whenever the policy names ANYTHING (a delay-only policy still
			// expects retries); `retry: {}` / `retry: 0` are the absent key. R5 lives in validation.
			if !ec.Retry.IsZero() {
				return fmt.Errorf("task %q %s: retry is not supported on a child task; retry inside the child, then raise", s.ID, where)
			}
			if ec.NotReached != nil {
				return fmt.Errorf("task %q %s: not_reached has no meaning on a child task", s.ID, where)
			}
			continue
		}

		// only_once retries in three tiers (specs/only-once-interrupted.md): pre.*-only patterns
		// are safe; anything else needs not_reached AND exact codes; the unknowable set is refused
		// however named. Per pattern, not per rule; tier 3 first so http.timeout gets the truth.
		if err := validateRetry(ec.Retry, s.ID, where); err != nil {
			return err
		}

		// An expression-valued attempts counts as "retries": its value is unknown here, and
		// the conservative reading is the one that keeps the tiers below in force.
		if onlyOnce && (ec.Retry.Attempts.IsExpr() || ec.Retry.Attempts.Literal() > 0) {
			notReached := ec.NotReached != nil && *ec.NotReached
			if len(ec.Code) == 0 {
				return fmt.Errorf("task %q %s: a catch-all rule cannot have retries on an only_once task; restrict it to pre.%% patterns, or add not_reached:true and name the exact codes that are safe to retry", s.ID, where)
			}
			for _, pat := range ec.Code {
				// Checked first, and irrespective of not_reached, so that naming one of
				// these gets the reason it is hopeless rather than advice that leads
				// nowhere.
				if errcode.Code(pat).IsUnknowable() {
					return fmt.Errorf("task %q %s: %s can never be retried on an only_once task, with or without not_reached: the request left and no response came back, so whether the call took effect is unknowable. Catch it with a goto and check the system of record instead", s.ID, where, pat)
				}
				if patternOnlyMatchesPre(pat) {
					continue
				}
				if !notReached {
					return fmt.Errorf("task %q %s: pattern %q can match errors where the call may have executed; restrict it to pre.%% patterns, or add not_reached:true and name the exact codes you know leave the remote untouched", s.ID, where, pat)
				}
				if strings.ContainsRune(pat, '%') {
					return fmt.Errorf("task %q %s: not_reached:true asserts what one specific error means, so pattern %q cannot be a wildcard; name the exact codes instead (e.g. \"http.409\")", s.ID, where, pat)
				}
			}
		}
	}
	return nil
}

// validateFetchOnlySlots refuses the request slots on an action that has no request to make.
// Nothing reads them there, and a field nothing reads is dropped in silence — the same reason
// a fetch refuses `result_schema`. `responses` has its own check below, because its message
// names the alternative.
func validateFetchOnlySlots(s *Task) error {
	if s.Action == nil || s.Action.Type == ActionTypeFetch {
		return nil
	}
	for _, slot := range []struct {
		name string
		set  bool
	}{
		{"url", s.Action.URL != ""},
		{"method", s.Action.Method != ""},
		{"headers", s.Action.Headers.Present()},
		{"query", s.Action.Query.Present()},
		{"accepted_status", s.Action.AcceptedStatus.Present()},
	} {
		if slot.set {
			return fmt.Errorf("task %q: action.%s is only valid on a fetch — a %q task makes no HTTP request, so the value would be ignored", s.ID, slot.name, s.Action.Type)
		}
	}
	return nil
}

// validateResponses checks a fetch's `responses` map: the slot belongs to no other action
// type, its keys are status patterns, and no status is declared twice — an overlap at equal
// specificity has no answer, and picking one silently would make the report a guess. It also
// refuses `result_schema` on a fetch: nothing reads it there, and a field nothing reads is
// dropped in silence. See specs/fetch-http-surface.md §2.
func validateResponses(s *Task, pool schema.Defs) error {
	if s.Action == nil {
		return nil
	}
	if s.Action.Type != ActionTypeFetch {
		if len(s.Action.Responses) > 0 {
			return fmt.Errorf("task %q: action.responses is only valid on a fetch — a %q task has no status to key on; use result_schema", s.ID, s.Action.Type)
		}
		return nil
	}
	if s.Action.ResultSchema != nil {
		return fmt.Errorf("task %q: action.result_schema is not valid on a fetch — declare the body per status, e.g. %s", s.ID, `responses: {"200": {...}}`)
	}
	owner := map[string]string{}
	for _, key := range sortedResponseKeys(s.Action.Responses) {
		patterns, err := ParseResponseKey(key)
		if err != nil {
			return fmt.Errorf("task %q: action.responses key %q: %w", s.ID, key, err)
		}
		for _, p := range patterns {
			if prev, dup := owner[p]; dup {
				return fmt.Errorf("task %q: action.responses declares %q twice, in %q and %q — one status, one schema", s.ID, p, prev, key)
			}
			owner[p] = key
		}
		if err := checkSchemaDoc(fmt.Sprintf("task %q action.responses[%q]", s.ID, key), s.Action.Responses[key], pool); err != nil {
			return err
		}
	}
	return nil
}

func sortedResponseKeys(m map[string]*schema.Schema) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validateRaises places the `raises` slot exactly where result_schema sits: on the action for
// child/child_list, on each entry for a child_map, whose entries can be different processes
// with different payloads for one code. Whether the child can raise a declared code at all is
// checked where the child is resolved (validation.checkDeclaredRaises).
func validateRaises(s *Task, pool schema.Defs) error {
	if s.Action == nil {
		return nil
	}
	switch s.Action.Type {
	// External declares the same slot for a different producer: the code arrives from a
	// worker's /external-tasks/fail rather than from a child, so the declared set is a
	// contract rather than a knowable set — which is why no reachability rule (R5) applies.
	case ActionTypeChild, ActionTypeChildList, ActionTypeExternal:
	case ActionTypeChildMap:
		if len(s.Action.Raises) > 0 {
			return fmt.Errorf("task %q: action.raises is declared per entry on a child_map — move it under children[<key>].raises, beside that entry's result_schema", s.ID)
		}
	default:
		if len(s.Action.Raises) > 0 {
			return fmt.Errorf("task %q: action.raises is only valid on a child, child_map, child_list or external task — a %q task catches engine codes, which carry no declared payload", s.ID, s.Action.Type)
		}
		return nil
	}
	if err := checkRaisesDoc(fmt.Sprintf("task %q action.raises", s.ID), s.Action.Raises, pool); err != nil {
		return err
	}
	keys := make([]string, 0, len(s.Action.Children))
	for key := range s.Action.Children {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		where := fmt.Sprintf("task %q action.children[%q].raises", s.ID, key)
		if err := checkRaisesDoc(where, s.Action.Children[key].Raises, pool); err != nil {
			return err
		}
	}
	return nil
}

// checkRaisesDoc validates one raises map: R1-shaped keys, and a real schema document under
// each key that has one. A nil value is `null` — a code declared to carry nothing — and has
// no document to check.
func checkRaisesDoc(where string, r Raises, pool schema.Defs) error {
	for _, code := range sortedRaiseCodes(r) {
		if !faultCodeRe.MatchString(code) {
			return fmt.Errorf("%s: %q is not a raise code — codes are lower_snake_case with no dots (dots are reserved for engine codes, and no engine code carries a declared payload)", where, code)
		}
		if r[code] == nil {
			continue
		}
		if err := checkSchemaDoc(fmt.Sprintf("%s[%q]", where, code), r[code], pool); err != nil {
			return err
		}
	}
	return nil
}

// sortedRaiseCodes orders the keys so a definition with two bad ones reports the same one
// every run.
func sortedRaiseCodes(r Raises) []string {
	codes := make([]string, 0, len(r))
	for code := range r {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

// validateActionSchemas checks that any attached result_schema documents (task-level and
// child_map entries) are valid schemas. accepted_status is a shape (evaluating to an array
// of status patterns), so — like headers — it is not statically pattern-checked here; an
// unrecognized pattern simply never matches at runtime (matchAcceptedStatus).
func validateActionSchemas(s *Task, pool schema.Defs) error {
	if s.Action == nil {
		return nil
	}
	if err := checkSchemaDoc(fmt.Sprintf("task %q action.result_schema", s.ID), s.Action.ResultSchema, pool); err != nil {
		return err
	}
	if s.Action.Type == ActionTypeChildMap {
		for key, entry := range s.Action.Children {
			if err := checkSchemaDoc(fmt.Sprintf("task %q action.children[%q].result_schema", s.ID, key), entry.ResultSchema, pool); err != nil {
				return err
			}
		}
	}
	return nil
}

func validLikePattern(p string) bool {
	return strings.TrimSpace(p) != ""
}

// patternOnlyMatchesPre reports whether a code pattern can only match error codes in the
// not-reached (pre.*) namespace: its constant prefix (before the first % wildcard) must
// start with errcode.NotReached. '%' is the only wildcard (see errcode.MatchCode), so it
// is the only boundary.
func patternOnlyMatchesPre(p string) bool {
	for i := 0; i < len(p); i++ {
		if p[i] == '%' {
			return strings.HasPrefix(p[:i], errcode.NotReached)
		}
	}
	return strings.HasPrefix(p, errcode.NotReached)
}

// configNameRe matches a valid config var name; it is used in the
// GENROC_<PROCESS>_<NAME> / GENROC_GLOBAL_<NAME> environment variable names, so it
// must be an identifier.
var configNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateConfigSchema enforces the config_schema shape: a flat "object" whose properties
// each declare a single scalar type (string/integer/number/boolean) with no nested
// object/array, combinators, or $ref. Property names must be identifiers that don't
// collide once normalized to their env var suffix; a required property may not carry a default.
func validateConfigSchema(cs *schema.Schema) error {
	if cs == nil {
		return nil
	}
	if t := cs.Type(); len(t) != 1 || !t.Contains("object") {
		return errors.New("config_schema must be type \"object\"")
	}
	if cs.HasCombinators() || cs.HasRef() || cs.HasDefs() {
		return errors.New("config_schema must not use oneOf/anyOf/allOf/$ref/$defs")
	}
	props := cs.Properties()
	required := make(map[string]bool, len(cs.Required()))
	for _, r := range cs.Required() {
		if _, ok := props[r]; !ok {
			return fmt.Errorf("config_schema: required lists unknown property %q", r)
		}
		required[r] = true
	}
	envKeys := make(map[string]string, len(props))
	for name, prop := range props {
		if !configNameRe.MatchString(name) {
			return fmt.Errorf("config %q: name must be a valid identifier [A-Za-z_][A-Za-z0-9_]*", name)
		}
		key := envToken(name)
		if prev, dup := envKeys[key]; dup {
			return fmt.Errorf("config %q and %q both map to the same environment variable suffix %q", name, prev, key)
		}
		envKeys[key] = name
		pt := prop.Type()
		if len(pt) != 1 {
			return fmt.Errorf("config %q: must declare a single primitive type (string, integer, number, or boolean)", name)
		}
		switch pt[0] {
		case "string", "integer", "number", "boolean":
		default:
			return fmt.Errorf("config %q: unsupported type %q (use string, integer, number, or boolean)", name, pt[0])
		}
		if prop.HasProperties() || prop.HasItems() || prop.HasCombinators() || prop.HasRef() {
			return fmt.Errorf("config %q: must be a primitive value (no nested objects, arrays, combinators, or $ref)", name)
		}
		if required[name] && prop.Default() != nil {
			return fmt.Errorf("config %q: cannot be both required and have a default", name)
		}
	}
	return nil
}

// checkSchemaDoc verifies s is a well-formed schema document; the $defs pool is merged in
// so a schema referencing a shared definition validates before Normalize bakes it in.
func checkSchemaDoc(field string, s *schema.Schema, pool schema.Defs) error {
	if err := checkSchemaDocAllowingSecrets(field, s, pool); err != nil {
		return err
	}
	// `secret: true` lives in config_schema and nowhere else. It keeps a value out of the
	// server's stdout, and the scrubber finds values by knowing them verbatim -- which it does
	// for config and cannot for anything a process computes. Accepting the marker elsewhere
	// would promise a protection nothing delivers. specs/object-store.md.
	if s != nil && s.ContainsSecret() {
		return fmt.Errorf("%s: secret: true is only valid in config_schema — it keeps a value out of the server's stdout, which the log scrubber can only do for values it knows verbatim; everything else is returned and stored as it is", field)
	}
	return nil
}

func checkSchemaDocAllowingSecrets(field string, s *schema.Schema, pool schema.Defs) error {
	if s == nil {
		return nil
	}
	if err := s.WithMergedDefs(pool).CheckDoc(); err != nil {
		return fmt.Errorf("%s is not a valid JSON Schema: %w", field, err)
	}
	return nil
}

// validateDefs checks each process-level $defs definition is well-formed, resolving $refs
// against the whole pool (definitions may reference each other). Collisions with generated
// schema names need no check — generation renames the colliding user definition.
func (d *ProcessDefinition) validateDefs() error {
	if d.Defs.IsZero() {
		return nil
	}
	for _, name := range d.Defs.Names() {
		def, _ := d.Defs.Get(name)
		// Merge the pool so definitions referencing each other check clean.
		if err := def.WithMergedDefs(d.Defs).CheckDoc(); err != nil {
			return fmt.Errorf("$defs %q is not a valid JSON Schema: %w", name, err)
		}
	}
	return nil
}

// v is the shared validator, configured to report JSON field names in errors.
var v = func() *validator.Validate {
	val := validator.New()
	val.RegisterTagNameFunc(func(f reflect.StructField) string {
		name := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
		if name == "-" || name == "" {
			return f.Name
		}
		return name
	})
	return val
}()

// FieldError is one failed struct-tag rule, located by its path within the submitted
// document. Field is the JSON path with the root struct name stripped, so it reads as
// the client wrote it: "tasks[0].id", not "ProcessDefinition.tasks[0].id".
type FieldError struct {
	Field   string `json:"field"`
	Rule    string `json:"rule"`
	Param   string `json:"param,omitempty"`
	Message string `json:"message"`
}

// ValidationError is a definition-validation failure that keeps the per-field detail
// the validator produced instead of flattening it to prose. A client submitting a
// process definition is the main consumer of this API, so "which field" has to survive
// the trip; Error() still renders the joined human form, which is what every existing
// caller that only prints the error continues to get.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	msgs := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		msgs[i] = f.Message
	}
	return strings.Join(msgs, "; ")
}

func fmtValidationErr(err error) error {
	if err == nil {
		return nil
	}
	var ve validator.ValidationErrors
	if !errors.As(err, &ve) {
		return err
	}
	fields := make([]FieldError, len(ve))
	for i, fe := range ve {
		fields[i] = FieldError{
			Field:   trimRootNamespace(fe.Namespace()),
			Rule:    fe.Tag(),
			Param:   fe.Param(),
			Message: describeFieldErr(fe),
		}
	}
	return &ValidationError{Fields: fields}
}

// trimRootNamespace drops the leading struct-type segment from a validator namespace
// ("ProcessDefinition.tasks[0].id" → "tasks[0].id"); a namespace with no dot is the
// root struct itself and is returned unchanged.
func trimRootNamespace(ns string) string {
	if i := strings.IndexByte(ns, '.'); i >= 0 {
		return ns[i+1:]
	}
	return ns
}

func describeFieldErr(fe validator.FieldError) string {
	field := fe.Field()
	switch fe.Tag() {
	case "required", "required_if":
		return fmt.Sprintf("%s is required", field)
	case "min":
		return fmt.Sprintf("%s must have at least %s item(s)", field, fe.Param())
	case "oneof":
		return fmt.Sprintf("%s must be one of: %s", field, strings.ReplaceAll(fe.Param(), " ", ", "))
	default:
		return fe.Error()
	}
}
