package validation

import (
	"encoding/json"
	"sort"

	"genroc/internal/model"
	"genroc/internal/schema"
)

// ContextSchema composes a single navigable schema for a process's entire runtime
// context. Its root is an object shaped like context_data:
//
//	{
//	  "input":   <process input schema>,
//	  "outputs": { "<taskID>": <task output schema>, ... },
//	  "output":  <process output schema>
//	}
//
// with every $ref resolving against a shared root $defs. Because it is a plain
// schema.Schema, the whole process shape can then be queried uniformly:
//
//	ctx.ValidateAt("outputs.charge", value)  // validate + normalize a subpath
//	ctx.SecretAt("input.password")           // is a path secret?
//	ctx.Infer("outputs.charge.amount")       // subschema / type at a path
//	ctx.Redact(contextData)                  // scrub the whole context for logs
//
// This is the composition half of the design: Generate does the dataflow analysis
// (which prior outputs are available where, recursion, etc.); ContextSchema folds
// its SchemaFile into one object so callers no longer juggle ProcessInput/Tasks/
// ProcessOutput/Defs separately.
func ContextSchema(def *model.ProcessDefinition) (schema.Schema, error) {
	sf, err := Generate(def)
	if err != nil {
		return schema.Schema{}, err
	}
	return SchemaFileContext(sf), nil
}

// SchemaFileContext assembles the context schema from an already-computed
// SchemaFile, avoiding a re-run of Generate on hot paths (a caller that already
// holds the SchemaFile — e.g. cached per process version — reuses it here).
func SchemaFileContext(sf SchemaFile) schema.Schema {
	ctx := schema.Object()
	if !sf.ProcessInput.IsZero() {
		ctx = ctx.WithProperty("input", sf.ProcessInput, true)
	}
	if len(sf.Tasks) > 0 {
		outputs := schema.Object()
		n := 0
		for tid, ts := range sf.Tasks {
			if !ts.Output.IsZero() {
				outputs = outputs.WithProperty(tid, ts.Output, false)
				n++
			}
		}
		if n > 0 {
			ctx = ctx.WithProperty("outputs", outputs, false)
		}
	}
	if !sf.ProcessOutput.IsZero() {
		ctx = ctx.WithProperty("output", sf.ProcessOutput, false)
	}
	if data := ErrorDataSchema(sf); !data.IsZero() {
		ctx = ctx.WithProperty("error", schema.Object().WithProperty("data", data, false), false)
	}
	return ctx.WithDefs(sf.Defs)
}

// ErrorDataSchema unions what `error.data` can hold ANYWHERE in the definition. A reader of a
// stored context cannot know which task wrote the error it is holding — a routed instance
// sits past the task that caught it, a failed one sits on it — so redaction takes every
// declared payload at once: over-marking costs a "***", missing one prints a secret.
// Identical declarations collapse, because a union no arm is alone in hides its own secrets.
func ErrorDataSchema(sf SchemaFile) schema.Schema {
	ids := make([]string, 0, len(sf.Tasks))
	for id := range sf.Tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var arms []schema.Schema
	seen := map[string]bool{}
	for _, id := range ids {
		e := sf.Tasks[id].Error
		if e.IsZero() {
			continue
		}
		key, err := json.Marshal(e)
		if err != nil || seen[string(key)] {
			continue
		}
		seen[string(key)] = true
		arms = append(arms, e)
	}
	switch len(arms) {
	case 0:
		return schema.Schema{}
	case 1:
		return arms[0]
	}
	return schema.AnyOf(arms...)
}
