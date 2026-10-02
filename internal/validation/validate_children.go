package validation

import (
	"fmt"
	"sort"
	"strings"

	"genroc/internal/errcode"
	"genroc/internal/model"
	"genroc/internal/schema"
)

// DefinitionGetter looks up process definitions. *db.DB satisfies this interface.
type DefinitionGetter interface {
	GetDefinition(name string, version int) (*model.ProcessDefinition, error)
	LatestVersion(name string) (int, error)
}

// ValidateChildProcessRefs checks every child task's process exists (version 0 is latest) and
// agrees with the call's input, result and raises. currentVersion is def's own version, for
// self-reference; def must already be normalised.
func ValidateChildProcessRefs(def *model.ProcessDefinition, currentVersion int, getter DefinitionGetter) error {
	defs, tasks, processInput, configSchema, err := buildSchemaContext(def)
	if err != nil {
		return err
	}

	required, optional, mustErr, mayErr, errSrc := computeContextSets(def.Tasks)
	errs := errContexts(def.Tasks, mustErr, mayErr, errSrc, defs)
	scopes := taskScopes{
		tasks: tasks, processInput: processInput, configSchema: configSchema, defs: defs,
		required: required, optional: optional, errs: errs,
	}
	// Diagnostics are discarded: this runs after Generate, which already reported them, and
	// an output that failed types as {} here — enough to check the child refs below.
	if err := inferOutputs(def.Tasks, scopes, newBag()); err != nil {
		return err
	}

	for _, s := range def.Tasks {
		if s.Action == nil {
			continue
		}
		ctx := scopes.action(s)

		switch s.Action.Type {
		case model.ActionTypeChild:
			// A single child is checked like a one-entry child_map: same input-subset and
			// output-subset checks against the referenced child's schemas.
			entry := model.ChildEntry{
				Name:         s.Action.Name,
				Version:      s.Action.Version,
				Input:        s.Action.Input,
				InputSchema:  s.Action.InputSchema,
				ResultSchema: s.Action.ResultSchema,
				Raises:       s.Action.Raises,
			}
			if err := validateChildEntry(s.ID, "child", entry, ctx, defs, def, currentVersion, getter); err != nil {
				return err
			}
		case model.ActionTypeChildMap:
			for key, entry := range s.Action.Children {
				if err := validateChildEntry(s.ID, fmt.Sprintf("children[%q]", key), entry, ctx, defs, def, currentVersion, getter); err != nil {
					return err
				}
			}
		case model.ActionTypeChildList:
			if err := validateChildListEntry(s.ID, s.Action, ctx, defs, def, currentVersion, getter); err != nil {
				return err
			}
		}

		if err := validateChildOnErrorReachability(s, def, currentVersion, getter); err != nil {
			return err
		}
	}
	return nil
}

// R5: every code a child task's on_error names must match something its children can raise.
// One direction only (D3): typos are caught, uncovered raisables surface at runtime.
func validateChildOnErrorReachability(s *model.Task, current *model.ProcessDefinition, currentVersion int, getter DefinitionGetter) error {
	if len(s.OnError) == 0 || s.Action == nil {
		return nil
	}

	var raisable []string
	addRaises := func(name string, version int) error {
		child, _, err := resolveChild(name, version, current, currentVersion, getter)
		if err != nil {
			return err // already reported by the input-compat pass; resolve again defensively
		}
		raisable = append(raisable, child.Raises()...)
		return nil
	}

	switch s.Action.Type {
	case model.ActionTypeChild:
		if err := addRaises(s.Action.Name, s.Action.Version); err != nil {
			return nil
		}
	case model.ActionTypeChildMap:
		for _, entry := range s.Action.Children {
			if err := addRaises(entry.Name, entry.Version); err != nil {
				return nil // resolution failed; let the input-compat pass own that error
			}
		}
	case model.ActionTypeChildList:
		if err := addRaises(s.Action.Name, s.Action.Version); err != nil {
			return nil
		}
	default:
		return nil // not a child task: on_error codes are engine codes, not raised ones
	}

	// Plus result.invalid: a child's output failing the result_schema this task narrowed it with
	// is reactable. specs/error-extensions.md §X2-c.
	raisable = append(raisable, string(errcode.ResultInvalid))

	matchesSomeRaise := func(pattern string) bool {
		for _, code := range raisable {
			if errcode.MatchCode(pattern, code) {
				return true
			}
		}
		return false
	}

	for i, ec := range s.OnError {
		for _, pattern := range ec.Code {
			if !matchesSomeRaise(pattern) {
				return fmt.Errorf("task %q on_error[%d]: no child of this task can raise a code matching %q", s.ID, i, pattern)
			}
		}
	}
	return nil
}

// A self-reference (same name, version 0 or current) resolves without a lookup; otherwise
// version 0 means the child's latest.
func resolveChild(name string, version int, current *model.ProcessDefinition, currentVersion int, getter DefinitionGetter) (*model.ProcessDefinition, int, error) {
	if name == current.Name && (version == 0 || version == currentVersion) {
		return current, currentVersion, nil
	}
	if version == 0 {
		v, err := getter.LatestVersion(name)
		if err != nil {
			return nil, 0, err
		}
		version = v
	}
	child, err := getter.GetDefinition(name, version)
	if err != nil {
		return nil, 0, err
	}
	return child, version, nil
}

func validateChildEntry(taskID string, label string, p model.ChildEntry, ctx schema.Schema, defs schema.Defs, current *model.ProcessDefinition, currentVersion int, getter DefinitionGetter) error {
	prefix := fmt.Sprintf("task %q: %s", taskID, label)

	child, childVersion, err := resolveChild(p.Name, p.Version, current, currentVersion, getter)
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}

	// Input compatibility is checkable only when the child declares an input schema; the
	// output check runs regardless (not gated behind it).
	if child.InputSchema != nil {
		// Declaration-to-declaration: the inferred type still carries nulls the conform removes,
		// and would refuse a call that works. specs/declared-slot-schemas.md §5.
		if p.InputSchema != nil {
			if err := checkDeclaredAgainstChild(prefix, *p.InputSchema, child, childVersion); err != nil {
				return err
			}
		} else if err := checkInputShapeAgainstChild(prefix, p, ctx, defs, child, childVersion); err != nil {
			return err
		}
	}

	return checkChildContract(prefix, child, childVersion, p.ResultSchema, p.Raises)
}

// checkChildContract checks both channels a call declares against the child it names:
// `result_schema` against the child's output type, `raises[code]` against that code's payload
// type. One SchemaFile serves both — they are one contract. See CLAUDE.md.
func checkChildContract(prefix string, child *model.ProcessDefinition, childVersion int, resultSchema *schema.Schema, raises model.Raises) error {
	// Short-circuit, so a child_map of many entries naming an open-output child does not infer
	// it once per entry.
	needOutput := resultSchema != nil && child.Output.Present()
	if !needOutput && len(raises) == 0 {
		return nil
	}
	sf, err := Generate(child)
	if err != nil {
		return fmt.Errorf("%s: infer %q v%d: %w", prefix, child.Name, childVersion, err)
	}
	if err := checkDeclaredRaises(prefix, sf, child.Name, childVersion, raises); err != nil {
		return err
	}
	if !needOutput {
		return nil
	}
	return checkChildOutputType(prefix, sf, resultSchema)
}

// checkDeclaredRaises: the code must be one the child can raise (R5's direction — a typo'd key
// is a declaration no rule can ever read), and its payload must narrow to the shape declared.
// NarrowsTo is sound because Engine.raisedData conforms the payload against this very schema.
func checkDeclaredRaises(prefix string, sf SchemaFile, childName string, childVersion int, raises model.Raises) error {
	for _, code := range sortedCodes(raises) {
		// sf.Raises is keyed by exactly the codes ProcessDefinition.Raises() reports: a clause
		// attaching nothing still records, as null.
		actual, raisable := sf.Raises[code]
		if !raisable {
			return fmt.Errorf("%s: raises declares %q, which %q v%d never raises", prefix, code, childName, childVersion)
		}
		declared := raises[code]
		// `raises: {code: null}` — the code is declared and carries nothing. collect conforms
		// nothing against nothing, so there is no shape here to disagree with.
		if declared == nil {
			continue
		}
		actual = actual.WithDefs(sf.Defs)
		if actual.NarrowsTo(*declared) {
			continue
		}
		return fmt.Errorf("%s: raises[%q] does not fit what %q v%d raises there: %s",
			prefix, code, childName, childVersion, narrowBreaks(actual, *declared))
	}
	return nil
}

func sortedCodes(raises model.Raises) []string {
	codes := make([]string, 0, len(raises))
	for code := range raises {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

// NarrowsTo, not IsSubset: collect conforms the value at runtime, so an unknown {} is backed by
// a real check.
func checkChildOutputType(prefix string, sf SchemaFile, resultSchema *schema.Schema) error {
	childOut, ok, err := schemaFileOutput(sf)
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	if !ok {
		return nil
	}
	if childOut.NarrowsTo(*resultSchema) {
		return nil
	}
	return fmt.Errorf("%s: the child's output type is not compatible with the declared result_schema: %s",
		prefix, narrowBreaks(childOut, *resultSchema))
}

// Both channels print through here, so a caller reads the same sentence whichever broke.
func narrowBreaks(actual, declared schema.Schema) string {
	breaks := actual.ExplainNarrowsTo(declared)
	parts := make([]string, 0, len(breaks))
	for _, b := range breaks {
		switch {
		case b.Kind == schema.BreakMissingRequired:
			parts = append(parts, fmt.Sprintf("%s: declared required, never set", b.Path))
		case b.Path == "":
			parts = append(parts, fmt.Sprintf("%s is not accepted where %s is expected", b.Sub, b.Super))
		default:
			parts = append(parts, fmt.Sprintf("%s: %s → %s", b.Path, b.Sub, b.Super))
		}
	}
	return strings.Join(parts, "; ")
}

// Resolved, so it compares against another pool. ok=false is no declared output: the open type,
// nothing to check, at every caller.
func schemaFileOutput(sf SchemaFile) (schema.Schema, bool, error) {
	if sf.ProcessOutput.IsZero() {
		return schema.Schema{}, false, nil
	}
	out, err := sf.ProcessOutput.WithDefs(sf.Defs).Resolve()
	if err != nil {
		return schema.Schema{}, false, fmt.Errorf("resolve output type: %w", err)
	}
	return out, true, nil
}

func validateChildListEntry(taskID string, action *model.Action, ctx schema.Schema, defs schema.Defs, current *model.ProcessDefinition, currentVersion int, getter DefinitionGetter) error {
	prefix := fmt.Sprintf("task %q: child_list", taskID)

	child, childVersion, err := resolveChild(action.Name, action.Version, current, currentVersion, getter)
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}

	// Re-checked here, after buildInputs, with the child in scope.
	arr, err := checkArrayTemplate(action.Over, ctx, taskID)
	if err != nil {
		return err
	}

	// Element/input compatibility is only checkable when the child declares an input
	// schema; the output check runs regardless, so it is not gated behind the input one.
	if child.InputSchema != nil && action.InputSchema != nil {
		// Declared: the element is conformed to it per element before the child is spawned,
		// so the declaration is what the child receives.
		if err := checkDeclaredAgainstChild(prefix, *action.InputSchema, child, childVersion); err != nil {
			return err
		}
	} else if child.InputSchema != nil {
		// Reading `Items` off a $ref node finds nothing.
		if arr.HasRef() {
			if resolved, rerr := arr.Resolve(); rerr == nil {
				arr = resolved
			}
		}
		if !arr.HasItems() {
			return fmt.Errorf("%s: over is an array with no declared element type, so it cannot be checked against %q v%d input_schema; give the array a typed item schema", prefix, action.Name, childVersion)
		}
		elem := arr.Items()

		normalized, err := elem.WithDefs(defs).Normalize()
		if err != nil {
			return fmt.Errorf("%s: normalize element type: %w", prefix, err)
		}
		if !normalized.IsSubset(*child.InputSchema) {
			return fmt.Errorf("%s: array element type is not compatible with %q v%d input_schema", prefix, action.Name, childVersion)
		}
	}

	// result_schema types each element, so each child's output is checked against it, as in
	// child_map.
	return checkChildContract(prefix, child, childVersion, action.ResultSchema, action.Raises)
}
