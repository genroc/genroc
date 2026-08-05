package validation

import (
	"fmt"

	"genroc/internal/errcode"
	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/shape"
)

// DefinitionGetter looks up process definitions. *db.DB satisfies this interface.
type DefinitionGetter interface {
	GetDefinition(name string, version int) (*model.ProcessDefinition, error)
	LatestVersion(name string) (int, error)
}

// ValidateChildProcessRefs checks every child/child_map/child_list task in def:
//  1. The referenced process exists (version 0 resolves to latest).
//  2. The schema inferred from the input expressions is a subset of the child's InputSchema.
//
// currentVersion is the server-assigned version of def (used for self-reference detection).
// def must already be normalised (Generate calls Normalize internally, so call this after Generate).
func ValidateChildProcessRefs(def *model.ProcessDefinition, currentVersion int, getter DefinitionGetter) error {
	defs, tasks, processInput, configSchema, err := buildSchemaContext(def)
	if err != nil {
		return err
	}

	required, optional, mustErr, mayErr := computeContextSets(def.Tasks)

	for _, s := range def.Tasks {
		if s.Action == nil {
			continue
		}
		ctx := contextSchema(required[s.ID], optional[s.ID], tasks, processInput, configSchema, mustErr[s.ID], mayErr[s.ID]).WithDefs(defs)

		switch s.Action.Type {
		case model.ActionTypeChild:
			// A single child is checked like a one-entry child_map: same input-subset and
			// output-subset checks against the referenced child's schemas.
			entry := model.ChildEntry{
				Name:         s.Action.Name,
				Version:      s.Action.Version,
				Input:        s.Action.Input,
				ResultSchema: s.Action.ResultSchema,
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

// R5: every code pattern a child task's on_error names must match something its children
// can raise — one direction only (D3): typos are caught, uncovered raisables surface at
// runtime. Matched with the runtime's errcode.MatchCode; catch-alls exempt; the raise set
// unions the task's children via Raises().
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

// resolveChild resolves the (name, version) a child task references to its definition
// and concrete version. A self-reference (same name, version 0 or the current version)
// resolves to current without a lookup; otherwise version 0 means the child's latest
// published version. Shared by the child_map and child_list validators.
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
	// output check runs regardless (not gated behind it). CheckWith infers, normalizes over
	// shared defs, subset-checks; an absent input is an empty object.
	if child.InputSchema != nil {
		var raw any = map[string]any{}
		if p.Input.Present() {
			raw = p.Input.Raw
		}
		shp := shape.Shape{Raw: raw, Schema: child.InputSchema, Name: fmt.Sprintf("%s input", prefix)}
		if _, err := shp.CheckWith(ctx.WithDefs(defs), shape.CheckHooks{
			Result: func(_, _ schema.Schema) error {
				return fmt.Errorf("%s: input is not compatible with %q v%d input_schema", prefix, p.Name, childVersion)
			},
		}); err != nil {
			return err
		}
	}

	return checkChildOutputType(prefix, child, p.ResultSchema)
}

// checkChildOutputType: the child's declared output must NarrowsTo the parent's
// result_schema — the output analogue of the input subset check. NarrowsTo (not IsSubset)
// is THIS slot's privilege alone: collect conforms the value at runtime, so an unknown {}
// is backed by a real check. Skipped when either side declares nothing.
func checkChildOutputType(prefix string, child *model.ProcessDefinition, resultSchema *schema.Schema) error {
	if resultSchema == nil {
		return nil
	}
	childOut, ok, err := processOutputType(child)
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	if !ok {
		return nil
	}
	if !childOut.NarrowsTo(*resultSchema) {
		return fmt.Errorf("%s: the child's output type is not compatible with the declared result_schema", prefix)
	}
	return nil
}

// processOutputType infers and RESOLVES the definition's output (Generate returns a $ref
// into its own $defs; resolving makes it comparable against another pool). ok=false =
// no declared output = open type = nothing to check, at every caller.
func processOutputType(def *model.ProcessDefinition) (schema.Schema, bool, error) {
	if !def.Output.Present() {
		return schema.Schema{}, false, nil
	}
	sf, err := Generate(def)
	if err != nil {
		return schema.Schema{}, false, fmt.Errorf("infer output type: %w", err)
	}
	return schemaFileOutput(sf)
}

// schemaFileOutput is processOutputType for a caller that already holds the SchemaFile.
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

// validateChildListEntry checks a child_list task: the referenced child exists,
// `over` is a non-null array, and the array's element type (each element is one
// child's input) is a subset of the child's InputSchema.
func validateChildListEntry(taskID string, action *model.Action, ctx schema.Schema, defs schema.Defs, current *model.ProcessDefinition, currentVersion int, getter DefinitionGetter) error {
	prefix := fmt.Sprintf("task %q: child_list", taskID)

	child, childVersion, err := resolveChild(action.Name, action.Version, current, currentVersion, getter)
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}

	// Infer `over` and confirm it is a non-null array. This also type-checks the
	// expression itself (done again here — with the child in scope — after buildInputs).
	arr, err := checkArrayTemplate(action.Over, ctx, taskID)
	if err != nil {
		return err
	}

	// Element/input compatibility is only checkable when the child declares an input
	// schema; the output check runs regardless, so it is not gated behind the input one.
	if child.InputSchema != nil {
		// Extract the element type (resolving `over` through a $ref first, so an array
		// reached via a shared definition still yields its item schema), then subset-check
		// it against the child's input schema.
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

	// result_schema types each element of the child_list output, and each child's output
	// is validated against it individually — so the per-child check is childOutput ⊆
	// action.ResultSchema, the same shape as child_map's.
	return checkChildOutputType(prefix, child, action.ResultSchema)
}
