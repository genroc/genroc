// Package validation infers and type-checks JSON Schemas for process definitions.
package validation

import (
	"fmt"
	"slices"
	"sort"

	"genroc/internal/model"
	"genroc/internal/schema"
)

type TaskSchemas struct {
	ActionType model.ActionType `json:"action_type"`
	// Input, Query and Children are what the definition SENDS (a fetch's body is Input), each
	// computed by `sent` and read by every consumer — see CLAUDE.md.
	Input    schema.Schema            `json:"input,omitzero"`
	Query    schema.Schema            `json:"query,omitzero"`
	Children map[string]schema.Schema `json:"children,omitempty"`
	// Result is `self.result`'s type (a declared result_schema, or a fetch's responses), absent
	// where none is declared. Filled for every task, output map or not: it is a worker's contract.
	Result schema.Schema `json:"result,omitzero"`
	// resultTyped carries what Result's zero value cannot: an untyped child_map types as an
	// empty object, not as nothing.
	resultTyped bool
	Output      schema.Schema `json:"output,omitzero"`
	// Error is `last_error.data` here: the failure that ROUTED here, unioned over every path. Not
	// a rule's caught error, which is that rule's context (specs/task-scopes.md §The error axis).
	Error schema.Schema `json:"last_error,omitzero"`
}

// SchemaFile is the top-level output.
type SchemaFile struct {
	Process       string                 `json:"process"`
	ProcessInput  schema.Schema          `json:"process_input,omitzero"`
	ProcessOutput schema.Schema          `json:"process_output,omitzero"`
	Tasks         map[string]TaskSchemas `json:"tasks,omitempty"`
	// Raises is the error channel's ProcessOutput, keyed by exactly ProcessDefinition.Raises(): a
	// clause attaching nothing types as null rather than dropping out. See CLAUDE.md.
	Raises map[string]schema.Schema `json:"raises,omitempty"`
	Defs   schema.Defs              `json:"$defs,omitzero"`
}

func buildSchemaContext(def *model.ProcessDefinition) (defs schema.Defs, tasks map[string]TaskSchemas, processInput schema.Schema, configSchema schema.Schema, err error) {
	named := make(map[string]schema.Schema)
	if def.InputSchema != nil {
		named["input"] = *def.InputSchema
	}
	collectNamedOutputs(def.Tasks, named)
	defs = schema.NewDefs()
	if len(named) > 0 {
		defs, err = schema.FlattenNamed(named)
		if err != nil {
			return
		}
	}
	// Process-level $defs reach the pool only through the schemas that use them; MergeInto renames
	// on collision, so generated names keep theirs.
	tasks = make(map[string]TaskSchemas)
	collectTaskRefs(def.Tasks, tasks)
	if err = collectResults(def.Tasks, tasks, defs); err != nil {
		return
	}
	if _, ok := named["input"]; ok {
		processInput = schema.Ref("input")
	}
	configSchema = buildConfigSchema(def.ConfigSchema)
	return
}

// Non-null only when guaranteed at runtime (required or defaulted); the rest stay nullable so
// unsafe uses get flagged.
func buildConfigSchema(cs *schema.Schema) schema.Schema {
	if cs == nil {
		return schema.Schema{}
	}
	props := cs.Properties()
	if len(props) == 0 {
		return schema.Schema{}
	}
	present := make(map[string]bool, len(props))
	for _, r := range cs.Required() {
		present[r] = true
	}
	for name, prop := range props {
		if prop.Default() != nil {
			present[name] = true
		}
	}
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	slices.Sort(names)
	out := schema.Object()
	for _, name := range names {
		out = out.WithProperty(name, props[name], present[name])
	}
	return out
}

// Generate is Check as the registration gate: it normalises def and returns the diagnostics as
// an error.
func Generate(def *model.ProcessDefinition) (SchemaFile, error) {
	sf, ds := Check(def)
	if len(ds) > 0 {
		return SchemaFile{}, ds
	}
	return sf, nil
}

// Check returns every diagnostic, addressed by slot, AND the partial view it built: a failed slot
// types as {} downstream. The view is empty only when def will not normalise or its context will
// not build. specs/language-server.md §2, §7b.
func Check(def *model.ProcessDefinition) (SchemaFile, Diagnostics) {
	b := newBag()
	if err := def.Normalize(); err != nil {
		b.add("", CodeStructure, err)
		return SchemaFile{}, b.diagnostics()
	}
	result := SchemaFile{Process: def.Name}

	defs, tasks, processInput, configSchema, err := buildSchemaContext(def)
	if err != nil {
		b.add("", CodeStructure, err)
		return SchemaFile{}, b.diagnostics()
	}
	result.ProcessInput = processInput

	rd := newRaiseData()
	if err := buildInputs(def.Tasks, tasks, processInput, configSchema, defs, rd, b); err != nil {
		b.add("", CodeStructure, err)
	}
	result.Raises = rd.types()

	for _, s := range def.Tasks {
		if ts, ok := tasks[s.ID]; ok {
			if ts.Input.HasProperties() {
				name := uniqueDefName(s.ID+"_input", defs)
				defs.Set(name, ts.Input)
				ts.Input = schema.Ref(name)
				tasks[s.ID] = ts
			}
		}
	}

	if def.Output.Present() {
		outputSchema, err := inferProcessOutput(def, tasks, result.ProcessInput, configSchema, defs)
		if err != nil {
			// The process output is the last slot; everything below still describes the tasks.
			b.add(SlotProcessOutput, CodeExpression, err)
			outputSchema = schema.Schema{}
		}
		name := uniqueDefName("output", defs)
		defs.Set(name, outputSchema)
		result.ProcessOutput = schema.Ref(name)
	}

	// Kept so redaction can see inside a declared payload (specs/error-extensions.md §X2-c). The
	// entry is CREATED where missing: a handler reading error.data usually exports nothing.
	_, _, mustErr, mayErr, errSrc := computeContextSets(def.Tasks)
	errs := errContexts(def.Tasks, mustErr, mayErr, errSrc, defs)
	for _, t := range def.Tasks {
		e, ok := errs[t.ID]
		if !ok || e.data.IsZero() {
			continue
		}
		ts := tasks[t.ID]
		if ts.ActionType == "" && t.Action != nil {
			ts.ActionType = t.Action.Type
		}
		ts.Error = e.data
		tasks[t.ID] = ts
	}

	if len(tasks) > 0 {
		result.Tasks = tasks
	}
	result.Defs = defs
	return result, b.diagnostics()
}

// inferProcessOutput types the output per terminal path and joins, so `a ?? b` resolves as at
// runtime rather than against the collapsed context. specs/path-sensitive-output.md.
func inferProcessOutput(def *model.ProcessDefinition, tasks map[string]TaskSchemas, processInput, configSchema schema.Schema, defs schema.Defs) (schema.Schema, error) {
	// The process output reads `error` at whichever terminal ran, so its `data` is that
	// terminal's — one arm of the context below per ending.
	_, _, mustErr, mayErr, errSrc := computeContextSets(def.Tasks)
	errs := errContexts(def.Tasks, mustErr, mayErr, errSrc, defs)
	scopes := taskScopes{tasks: tasks, processInput: processInput, configSchema: configSchema, defs: defs, errs: errs}
	// A declared output_schema is what this process PUBLISHES: the value is conformed to it at
	// completion, so it is what `$process` spreads and what the comparison reads.
	shp, hooks := declaredShape(def.Output.Raw, def.OutputSchema, "output")
	out, err := shp.CheckWith(scopes.processOutputContext(def), hooks)
	if err != nil {
		return schema.Schema{}, err
	}
	return published(out, def.OutputSchema), nil
}

func collectNamedOutputs(tasks []*model.Task, named map[string]schema.Schema) {
	for _, s := range tasks {
		if !s.Output.Present() {
			continue
		}
		// Inferred during the per-task walk (it may be recursive); a permissive
		// placeholder holds the $defs slot until then.
		named[s.ID+"_output"] = schema.Object()
	}
}

// collectResults types each action's result ONCE: every `self.result` scope, the switch's
// availability rule and TaskSchemas.Result read it from here.
func collectResults(tasks []*model.Task, out map[string]TaskSchemas, defs schema.Defs) error {
	for _, s := range tasks {
		ts, described := out[s.ID]
		if !described && s.Action == nil {
			continue // a routing task describes nothing: no action to type, no output to export
		}
		res, typed, err := actionResultType(s, defs)
		if err != nil {
			return err
		}
		if !typed && !described {
			continue // an untyped result is not a schema, so it puts nothing in the map
		}
		if ts.ActionType == "" && s.Action != nil {
			ts.ActionType = s.Action.Type
		}
		ts.Result, ts.resultTyped = res, typed
		out[s.ID] = ts
	}
	return nil
}

func collectTaskRefs(tasks []*model.Task, out map[string]TaskSchemas) {
	for _, s := range tasks {
		if !s.Output.Present() {
			continue
		}
		var at model.ActionType // empty for a no-action (routing) task
		if s.Action != nil {
			at = s.Action.Type
		}
		out[s.ID] = TaskSchemas{ActionType: at, Output: schema.Ref(s.ID + "_output")}
	}
}

// A child with no result_schema is omitted, with no permissive fallback; ok=false when none
// declares one.
func childMapOutputSchema(s *model.Task, defs schema.Defs) (schema.Schema, bool, error) {
	keys := make([]string, 0, len(s.Action.Children))
	for key := range s.Action.Children {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := schema.Object()
	typed := false
	for _, key := range keys {
		entry := s.Action.Children[key]
		if entry.ResultSchema == nil {
			continue // no schema → not accessible; omit the key
		}
		merged, err := entry.ResultSchema.MergeInto(defs)
		if err != nil {
			return schema.Schema{}, false, err
		}
		out = out.WithProperty(key, merged, true)
		typed = true
	}
	return out, typed, nil
}

// Only called with a result_schema declared; without one the result is untyped
// (actionResultType), with no permissive-array fallback.
func childListOutputSchema(s *model.Task, defs schema.Defs) (schema.Schema, error) {
	merged, err := s.Action.ResultSchema.MergeInto(defs)
	if err != nil {
		return schema.Schema{}, err
	}
	return schema.Array(merged), nil
}

func uniqueDefName(base string, defs schema.Defs) string {
	name := base
	for i := 1; defs.Has(name); i++ {
		name = fmt.Sprintf("%s_%d", base, i)
	}
	return name
}
