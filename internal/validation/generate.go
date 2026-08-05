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
}

// SchemaFile is the top-level output.
type SchemaFile struct {
	Process       string                 `json:"process"`
	ProcessInput  schema.Schema          `json:"process_input,omitzero"`
	ProcessOutput schema.Schema          `json:"process_output,omitzero"`
	Tasks         map[string]TaskSchemas `json:"tasks,omitempty"`
	Defs          schema.Defs            `json:"$defs,omitzero"`
}

// RedactContext returns a copy of an instance's context_data with secret-derived
// values replaced by "***", using the schemas inferred for the process: input is
// scrubbed against ProcessInput, each outputs.<task> against that task's output
// schema, and output against ProcessOutput. Keys with no inferred schema (unknown
// tasks, $error, bookkeeping) pass through unchanged. It runs the whole scrub as a
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
	// Process-level $defs reach the generation pool through the schemas that use
	// them: FlattenNamed hoists the input schema's baked copies here, and
	// actionResultType/childMapOutputSchema MergeInto the pool when a result
	// schema is embedded in a context — renaming safely on collision, so the
	// generated names seeded above always keep theirs. Unused definitions simply
	// never arrive.
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

	if err := buildInputs(def.Tasks, tasks, processInput, configSchema, defs); err != nil {
		return SchemaFile{}, err
	}

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
	shp := shape.Shape{Raw: def.Output.Raw, Name: "output"}
	terminals := outputTerminals(def)
	if len(terminals) < 2 {
		req, opt, errReq, errOpt := outputContextSets(def)
		ctx := contextSchema(req, opt, tasks, processInput, configSchema, errReq, errOpt).WithDefs(defs)
		return shp.Check(ctx)
	}

	// Task outputs reachable on at least one terminal. A reference to anything outside
	// this set is absent everywhere, so it is left out of every per-terminal context and
	// still reported as an access error instead of silently typing null.
	everMay := make(map[string]bool)
	for _, t := range terminals {
		for id := range t.may {
			everMay[id] = true
		}
	}

	var (
		joined      schema.Schema
		ok          int
		firstErr    error
		firstErrTsk string
	)
	for _, t := range terminals {
		var must, opt, absent []string
		for id := range t.must {
			must = append(must, id)
		}
		for id := range t.may {
			if !t.must[id] {
				opt = append(opt, id)
			}
		}
		for id := range everMay {
			if !t.may[id] {
				absent = append(absent, id)
			}
		}
		sort.Strings(must)
		sort.Strings(opt)
		sort.Strings(absent)

		ctx := contextSchemaAbsent(must, opt, absent, tasks, processInput, configSchema, t.errMin, t.errMax).WithDefs(defs)
		s, err := shp.Check(ctx)
		if err != nil {
			if firstErr == nil {
				firstErr, firstErrTsk = err, t.task
			}
			continue
		}
		if ok == 0 {
			joined = s
		} else {
			joined = joined.Join(s)
		}
		ok++
	}

	if firstErr != nil {
		// Name the path only when another one type-checked: an expression that is simply
		// wrong fails on every terminal and deserves its plain message, unprefixed.
		if ok > 0 {
			return schema.Schema{}, fmt.Errorf("on the path ending at task %q: %w", firstErrTsk, firstErr)
		}
		return schema.Schema{}, firstErr
	}
	return joined.Canonicalize(), nil
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

// childMapOutputSchema types a child_map result as an object with one property per child
// that declares a result_schema. A child WITHOUT a result_schema is omitted entirely — its
// output is not accessible or exportable (there is no permissive fallback). The bool return
// reports whether any child declared a result_schema; when none did, the whole result is
// untyped (routable in a switch, not exportable), like a schema-less child/child_list.
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
