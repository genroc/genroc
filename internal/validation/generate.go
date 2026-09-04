// Package validation infers and type-checks JSON Schemas for process definitions.
package validation

import (
	"fmt"
	"slices"
	"sort"

	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/shape"
)

type TaskSchemas struct {
	ActionType model.ActionType `json:"action_type"`
	Input      schema.Schema    `json:"input,omitzero"`
	Output     schema.Schema    `json:"output,omitzero"`
	// Error is the type of `error.data` at this task — a declared fetch response body or a
	// child's declared raise payload. It rides in the SchemaFile because redaction reads it
	// from here: a `secret: true` inside a declared payload is invisible to a context schema
	// that has no error slot, and the value reaches the API and the logs in the clear.
	Error schema.Schema `json:"error,omitzero"`
}

// SchemaFile is the top-level output.
type SchemaFile struct {
	Process       string                 `json:"process"`
	ProcessInput  schema.Schema          `json:"process_input,omitzero"`
	ProcessOutput schema.Schema          `json:"process_output,omitzero"`
	Tasks         map[string]TaskSchemas `json:"tasks,omitempty"`
	// Raises types the payload each code this process can raise carries — the error channel's
	// ProcessOutput. Its keys are exactly ProcessDefinition.Raises(): a clause attaching
	// nothing types as null rather than dropping out. See CLAUDE.md.
	Raises map[string]schema.Schema `json:"raises,omitempty"`
	Defs   schema.Defs              `json:"$defs,omitzero"`
}

// RedactContext returns a copy of an instance's context_data with secret-derived
// values replaced by "***", using the schemas inferred for the process: input is
// scrubbed against ProcessInput, each outputs.<task> against that task's output
// schema, and output against ProcessOutput. Keys with no inferred schema (unknown
// tasks, `error`, bookkeeping) pass through unchanged. It runs the whole scrub as a
// single walk of the composed context schema.
func RedactContext(ctxData map[string]any, sf SchemaFile) map[string]any {
	out, _ := SchemaFileContext(sf).Redact(ctxData).(map[string]any)
	return out
}

// buildSchemaContext derives the shared defs, tasks, and processInput from a definition.
// Both Generate and ValidateChildProcessRefs use it to avoid duplicating setup.
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
	// Process-level $defs reach the pool through the schemas that use them (FlattenNamed
	// hoists; MergeInto renames safely on collision, so generated names keep theirs). Unused
	// definitions never arrive.
	tasks = make(map[string]TaskSchemas)
	collectTaskRefs(def.Tasks, tasks)
	if _, ok := named["input"]; ok {
		processInput = schema.Ref("input")
	}
	configSchema = buildConfigSchema(def.ConfigSchema)
	return
}

// buildConfigSchema types the config namespace so config.<NAME> is checked and undeclared
// names are rejected at registration. Non-null only when guaranteed at runtime (required
// or defaulted); the rest stay nullable so unsafe uses get flagged.
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

// Generate normalises all schemas in def and builds the SchemaFile output.
func Generate(def *model.ProcessDefinition) (SchemaFile, error) {
	if err := def.Normalize(); err != nil {
		return SchemaFile{}, err
	}
	result := SchemaFile{Process: def.Name}

	defs, tasks, processInput, configSchema, err := buildSchemaContext(def)
	if err != nil {
		return SchemaFile{}, err
	}
	result.ProcessInput = processInput

	rd := newRaiseData()
	if err := buildInputs(def.Tasks, tasks, processInput, configSchema, defs, rd); err != nil {
		return SchemaFile{}, err
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
			return SchemaFile{}, err
		}
		name := uniqueDefName("output", defs)
		defs.Set(name, outputSchema)
		result.ProcessOutput = schema.Ref(name)
	}

	// The same per-task error facts inference uses, kept so redaction can see inside a
	// declared payload (specs/error-extensions.md §X2-c). The entry is CREATED where there is
	// none: collectTaskRefs lists only tasks that export an output, and a handler that merely
	// reads error.data usually exports nothing — which is exactly where a secret would hide.
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
	return result, nil
}

// inferProcessOutput types the output expression PER TERMINAL PATH and joins: the
// collapsed context makes covering outputs merely "optional" and `a ?? b` nullable even
// when one is always set. Per terminal, ?? resolves as at runtime; uncovered terminals
// still contribute null. specs/path-sensitive-output.md.
func inferProcessOutput(def *model.ProcessDefinition, tasks map[string]TaskSchemas, processInput, configSchema schema.Schema, defs schema.Defs) (schema.Schema, error) {
	// The process output reads `error` at whichever terminal ran, so its `data` is that
	// terminal's — one arm of the context below per ending.
	_, _, mustErr, mayErr, errSrc := computeContextSets(def.Tasks)
	errs := errContexts(def.Tasks, mustErr, mayErr, errSrc, defs)
	scopes := taskScopes{tasks: tasks, processInput: processInput, configSchema: configSchema, defs: defs, errs: errs}
	shp := shape.Shape{Raw: def.Output.Raw, Name: "output"}
	return shp.Check(scopes.processOutputContext(def))
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

// childMapOutputSchema: one property per child that declares a result_schema; a child
// without one is omitted entirely (not accessible, not exportable — no permissive
// fallback). ok=false when none declared: the whole result is untyped, like a schema-less child.
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

// childListOutputSchema types a child_list result as an array whose element type is the
// child's declared result_schema — one entry per element of `over`, in order. Only called
// when a result_schema is declared; without one the result is untyped and not exportable
// (see actionResultType), with no permissive-array fallback.
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
