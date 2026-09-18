package validation

// The static half of a declared slot schema. specs/declared-slot-schemas.md.
//
// One rule decides every site: where a declaration exists it IS the slot's type, so the check
// runs against it and the declaration is what the slot publishes. Two answers to "what is this
// slot" is the drift `SlotContexts` already paid for once.

import (
	"fmt"
	"strings"

	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/shape"
)

// declaredShape builds the shape check for a slot carrying a declaration. Conformed selects
// the relation paired with the runtime conform; the hook words the failure, since the default
// message says only that something does not conform and a reader needs the key.
func declaredShape(raw any, declared *schema.Schema, label string) (shape.Shape, shape.CheckHooks) {
	sh := shape.Shape{Raw: raw, Name: label, Schema: declared, Conformed: declared != nil}
	hooks := shape.CheckHooks{}
	if declared != nil {
		hooks.Result = func(inferred, required schema.Schema) error {
			return fmt.Errorf("%s does not fit the schema declared for it: %s",
				label, declaredBreaks(inferred, required))
		}
	}
	return sh, hooks
}

// published is what a slot the definition HANDS BACK publishes: its declaration. That is the
// contract a consumer reads and `$process` spreads, and it is stable across a refactor inside —
// which is the whole reason to declare one on an output.
func published(inferred schema.Schema, declared *schema.Schema) schema.Schema {
	if declared == nil {
		return inferred
	}
	return *declared
}

// sent is the type of a slot the definition SENDS: what this definition produces, conformed to
// the declaration where there is one (schema.Conformed). Not the declaration — that is the far
// side's contract, with its own address — and not the raw inferred type either, which still
// carries the nulls the conform removes. This is computed ONCE, here, and the CLI, the resolver
// manifest and the language server all read the result; none may carry a rule of its own.
func sent(inferred schema.Schema, declared *schema.Schema) schema.Schema {
	if declared == nil {
		return inferred
	}
	return inferred.Conformed(*declared)
}

// declaredBreaks words every place the inferred type fails its declaration. `undeclared` is
// the kind worth its own sentence: a reader who mistyped a key needs to be told that, not that
// a type did not match.
func declaredBreaks(inferred, declared schema.Schema) string {
	breaks := inferred.ExplainConformsExactlyTo(declared)
	parts := make([]string, 0, len(breaks))
	for _, b := range breaks {
		switch {
		case b.Kind == schema.BreakUndeclared && b.Sub == "an open map":
			parts = append(parts, fmt.Sprintf("%s is an open map, and the schema declares a fixed set of properties", at(b.Path)))
		case b.Kind == schema.BreakUndeclared:
			parts = append(parts, fmt.Sprintf("%s is not declared in the schema", b.Path))
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

// at names a location for a message about the node itself rather than about a property of it.
func at(path string) string {
	if path == "" {
		return "the value"
	}
	return path
}

// checkDeclaredQuery checks a fetch's query map against its declaration, and the declaration
// itself against the fixed target above it (`queryValueSchema`: a scalar, null, or an array of
// scalars). The declaration is checked FIRST so a schema that could never be satisfied is
// reported as a bad declaration rather than as a shape doing what it was told.
func checkDeclaredQuery(s *model.Task, ctx schema.Schema) (schema.Schema, error) {
	declared := *s.Action.QuerySchema
	if !declared.IsSubset(querySchema) {
		return schema.Schema{}, fmt.Errorf("task %q query_schema declares a value a query string cannot carry: %s",
			s.ID, narrowBreaks(declared, querySchema))
	}
	label := fmt.Sprintf("task %q query", s.ID)
	shp, hooks := declaredShape(s.Action.Query.Raw, s.Action.QuerySchema, label)
	inferred, err := shp.CheckWith(ctx, hooks)
	if err != nil {
		return schema.Schema{}, err
	}
	return sent(inferred, s.Action.QuerySchema), nil
}

// checkDeclaredListElement checks a child_list's declaration against ONE ELEMENT of `over`.
// The slot has no `input` shape — each element is one child's input — so without this the
// declaration would be compared against an empty object and accept anything.
func checkDeclaredListElement(s *model.Task, arr schema.Schema, defs schema.Defs) error {
	declared := s.Action.InputSchema
	if declared == nil {
		return nil
	}
	// Resolve through a $ref first: an array reached via a shared definition still has an
	// item type, and reading `Items` off the ref node finds nothing.
	if arr.HasRef() {
		if resolved, err := arr.Resolve(); err == nil {
			arr = resolved
		}
	}
	if !arr.HasItems() {
		return fmt.Errorf("task %q over is an array with no declared element type, so it cannot be "+
			"checked against input_schema; give the array a typed item schema", s.ID)
	}
	elem, err := arr.Items().WithDefs(defs).Normalize()
	if err != nil {
		return fmt.Errorf("task %q over: normalize element type: %w", s.ID, err)
	}
	if elem.ConformsExactlyTo(*declared) {
		return nil
	}
	return fmt.Errorf("task %q over: an element does not fit the schema declared for it: %s",
		s.ID, declaredBreaks(elem, *declared))
}

// checkDeclaredAgainstChild is the registration-time half of a declared child input: what the
// call site says it sends must fit what the child accepts. It runs OPEN — the child's own
// schema is not ours to close, and closing it would refuse a declaration that is perfectly
// good. specs/declared-slot-schemas.md §5.
func checkDeclaredAgainstChild(prefix string, declared schema.Schema, child *model.ProcessDefinition, childVersion int) error {
	if declared.IsSubset(*child.InputSchema) {
		return nil
	}
	return fmt.Errorf("%s: input_schema is not compatible with %q v%d input_schema: %s",
		prefix, child.Name, childVersion, narrowBreaks(declared, *child.InputSchema))
}

// checkInputShapeAgainstChild is the undeclared path, unchanged: the inferred type of the
// input shape against the child's schema, since nothing conforms it on the way out.
func checkInputShapeAgainstChild(prefix string, p model.ChildEntry, ctx schema.Schema, defs schema.Defs, child *model.ProcessDefinition, childVersion int) error {
	var raw any = map[string]any{}
	if p.Input.Present() {
		raw = p.Input.Raw
	}
	shp := shape.Shape{Raw: raw, Schema: child.InputSchema, Name: fmt.Sprintf("%s input", prefix)}
	_, err := shp.CheckWith(ctx.WithDefs(defs), shape.CheckHooks{
		Result: func(_, _ schema.Schema) error {
			return fmt.Errorf("%s: input is not compatible with %q v%d input_schema", prefix, p.Name, childVersion)
		},
	})
	return err
}
