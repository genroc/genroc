package model

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
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
	if err := checkSchemaDoc("config_schema", d.ConfigSchema, schema.Defs{}); err != nil {
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
// panic code, never both. Otherwise error_code would be ambiguous for exactly the
// observers it exists to serve — the same value would appear on 'raised' and 'failed'
// instances of the same process and mean two different things, so a dashboard could not
// tell "the caller may handle this" from "this tree is broken" by the code alone.
//
// The check runs after per-task validation, so every Fault here has already passed R1.
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
	return validateActionSchemas(s, pool)
}

// validateTimeout enforces the two rules a timeout has beyond its grammar: which action
// types honour one at all, and that `until` is confined to the one that parks.
//
// A timeout on a child or delay task is rejected rather than ignored — the engine reads it
// in exactly two places, and a deadline that is silently never applied is the failure this
// check exists for.
//
// `until` is confined to external because a deadline already past has to mean something,
// and it only means something coherent there: the task parks on that instant, so a
// deadline behind now is simply due. A fetch would instead build a context that is expired
// before the request is sent, and transport.ClassifyGoError reports that as http.timeout —
// an unknowable code, so on an only_once task it can never be retried, for a request that
// provably never left. There is no honest code to report for it, so the slot is refused.
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
var faultCodeRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// validateFault enforces R1 (code shape, message present) and R2 (both literal) on one
// raise or panic clause. where locates the case ("switch case 0", "on_error[1]") and
// clause names it, so the message points at the offending line without the author
// having to count.
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
	// R2: a computed code would make the definition's raise set uncomputable and
	// error_code unqueryable; a computed message would smuggle data across the process
	// boundary that a payload-free error design exists to keep closed.
	if strings.Contains(f.Code, "${") {
		return fmt.Errorf("task %q %s: %s: code must be a literal, not an expression", taskID, where, clause)
	}
	if strings.Contains(f.Message, "${") {
		return fmt.Errorf("task %q %s: %s: message must be a literal, not an expression", taskID, where, clause)
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

// validateOnError checks a task's on_error rules: terminal-clause arity (R3), pattern
// shape, catch-all last, goto targets, and the task-kind rules — a child task forbids
// parent-side retry (D7), an action task carries the only_once restrictions below.
//
// Both kinds share the pattern syntax; only what the codes are checked *against* differs.
// A child task's are checked against the child's raise set (R5, in the validation package,
// where children resolve); an action task's engine-code space is open and has no analogue.
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
			// D7: no parent-side retry — re-spawning a batch is not a retry (§10.1), so a
			// retry field would be silently ignored, which is worse than refusing it.
			// (Reachability of each pattern against the child raise set is R5, checked in
			// the validation package where children can be resolved.)
			if ec.Retries > 0 {
				return fmt.Errorf("task %q %s: retries is not supported on a child task; retry inside the child, then raise", s.ID, where)
			}
			if ec.NotReached != nil {
				return fmt.Errorf("task %q %s: not_reached has no meaning on a child task", s.ID, where)
			}
			continue
		}

		// Retries on an only_once task, in three tiers (docs/only-once-interrupted.md):
		// pre.*-only patterns are safe alone; anything else needs not_reached:true *and*
		// exact codes; the unknowable set is refused however it is named.
		//
		// Applied per pattern, not per rule, so a rule may mix a safe pre.% with a named
		// exception. Tier 3 is tested first, or naming http.timeout gets tier-2 advice
		// that leads nowhere.
		if onlyOnce && ec.Retries > 0 {
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
