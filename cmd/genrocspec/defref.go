package main

// The definition-language reference, generated from the JSON Schema internal/defschema already
// projects the language into. The `description:` struct tags behind it are maintained prose that
// until now only an editor's completion popup ever showed. specs/docs-site.md.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"genroc/internal/defschema"
)

// The names an author knows these by. A schema `$def` is named after the Go type that produced
// it, which is an implementation detail nobody writing YAML has any reason to learn.
var defTitles = map[string]string{
	"ModelTask":             "Task",
	"ModelAction":           "Action",
	"ModelShape":            "Shape",
	"ModelSwitchMap":        "Switch",
	"ModelErrorCase":        "Error rule",
	"ModelFault":            "Fault",
	"ModelRetry":            "Retry",
	"SchemaSchema":          "JSON Schema",
	"SchemaDefs":            "JSON Schema map",
	"SourcesResolverConfig": "Resolver",
}

const processSchemaNote = "the process-definition schema"

type jsonSchema = map[string]any

type defField struct {
	name, typ, desc string
	required        bool
}

// defArm is one alternative form of a union. An arm that is an object carries its own shape,
// and rendering only the word `object` was the whole of what a reader needed withheld.
type defArm struct {
	typ    string
	desc   string
	fields []defField
	each   bool // the fields describe ONE ELEMENT, the arm being an array
}

// A section is a table of fields OR a list of alternative forms, never both: the shorthand
// types (Switch, Shape, Retry) are unions of whole values, and a field table cannot say that.
type defSection struct {
	title  string
	intro  string
	fields []defField
	arms   []defArm
}

type defPage struct {
	title, slug, blurb string
	order              int
	from               string // what the generated-by note names as the source
	sections           []defSection
}

func writeDefinitionReference(dir string) error {
	var root jsonSchema
	if err := json.Unmarshal(defschema.Process(), &root); err != nil {
		return fmt.Errorf("parse the generated process schema: %w", err)
	}
	defs, _ := root["$defs"].(map[string]any)
	if len(defs) == 0 {
		return fmt.Errorf("the process schema carried no $defs")
	}
	def := func(name string) jsonSchema {
		s, _ := defs[name].(map[string]any)
		return s
	}

	pages := []defPage{
		{
			title: "Process", slug: "process", order: 1,
			blurb:    "The top level of a definition file.",
			from:     processSchemaNote,
			sections: []defSection{{title: "Process", fields: fieldsOf(root)}},
		},
		{
			title: "Task", slug: "task", order: 2,
			blurb: "A task, the routing that follows it, and the value it publishes.",
			from:  processSchemaNote,
			sections: []defSection{
				{title: "Task", fields: fieldsOf(def("ModelTask"))},
				{title: "Switch", intro: "The value of a task's `switch`.", arms: armsOf(def("ModelSwitchMap"))},
				{title: "Shape", intro: "The value of an `output`, a `body`, or any other templated slot.", arms: armsOf(def("ModelShape"))},
			},
		},
		{
			title: "Actions", slug: "actions", order: 3,
			blurb:    "The six action types, and the slots each one takes.",
			from:     processSchemaNote,
			sections: actionSections(def("ModelAction")),
		},
		{
			title: "Error handling", slug: "error-handling", order: 6,
			blurb: "The `on_error` rules, and what a rule can do with the error it matched.",
			from:  processSchemaNote,
			sections: []defSection{
				{title: "Error rule", intro: "One entry of a task's `on_error`.", fields: fieldsOf(def("ModelErrorCase"))},
				{title: "Retry", intro: "The value of a rule's `retry`.", arms: armsOf(def("ModelRetry"))},
				{title: "Fault", intro: "The value of a rule's `raise` or `panic`.", fields: fieldsOf(def("ModelFault"))},
			},
		},
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	for _, p := range pages {
		empty := true
		for _, s := range p.sections {
			if len(s.fields) > 0 || len(s.arms) > 0 {
				empty = false
			}
		}
		if empty {
			return fmt.Errorf("the %s page came out empty; the schema's shape changed", p.title)
		}
		path := filepath.Join(dir, p.slug+".md")
		if err := os.WriteFile(path, []byte(renderDefPage(p, p.order)), 0644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s (%d)\n", path, len(p.sections))
	}
	return nil
}

// actionSections turns the discriminated union into one section per action type, in the order
// the schema declares them — which is the order model.ActionType declares them.
func actionSections(action jsonSchema) []defSection {
	arms, _ := action["oneOf"].([]any)
	out := make([]defSection, 0, len(arms))
	for _, raw := range arms {
		arm, _ := raw.(map[string]any)
		props, _ := arm["properties"].(map[string]any)
		disc, _ := props["type"].(map[string]any)
		name, _ := disc["const"].(string)
		if name == "" {
			continue
		}
		// `type` is the discriminator: it names the section, so a row repeating it says only
		// that `type: fetch` means fetch.
		fields := []defField{}
		for _, f := range fieldsOf(arm) {
			if f.name != "type" {
				fields = append(fields, f)
			}
		}
		out = append(out, defSection{title: name, fields: fields})
	}
	return out
}

func fieldsOf(schema jsonSchema) []defField {
	props, _ := schema["properties"].(map[string]any)
	required := map[string]bool{}
	if list, ok := schema["required"].([]any); ok {
		for _, r := range list {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}
	out := make([]defField, 0, len(props))
	for name, raw := range props {
		p, _ := raw.(map[string]any)
		desc, _ := p["description"].(string)
		out = append(out, defField{name: name, typ: typeName(p), desc: desc, required: required[name]})
	}
	// Required first, then alphabetical: a reader writing the slot for the first time needs
	// what they cannot leave out, and the map gives no order of its own.
	sort.Slice(out, func(i, j int) bool {
		if out[i].required != out[j].required {
			return out[i].required
		}
		return out[i].name < out[j].name
	})
	return out
}

// armsOf renders a union as its alternative forms. The description on each arm is the only
// place the shorthand is explained — the union itself carries none. An arm whose shape is an
// object contributes that shape too: `retry`'s long form IS its four slots, and naming the arm
// `object` tells a reader nothing they did not already know from the colon.
func armsOf(schema jsonSchema) []defArm {
	var raw []any
	for _, key := range []string{"oneOf", "anyOf"} {
		if list, ok := schema[key].([]any); ok {
			raw = list
			break
		}
	}
	out := make([]defArm, 0, len(raw))
	for _, r := range raw {
		arm, _ := r.(map[string]any)
		desc, _ := arm["description"].(string)
		a := defArm{typ: typeName(arm), desc: desc, fields: fieldsOf(arm)}
		// An array arm's shape belongs to its ELEMENT, not to the value the slot takes.
		if len(a.fields) == 0 {
			if items, ok := arm["items"].(map[string]any); ok {
				a.fields, a.each = fieldsOf(items), true
			}
		}
		out = append(out, a)
	}
	return out
}

// typeName renders a schema fragment as the type an author would say out loud. `null` is
// dropped from a union: in this language a nullable slot is an optional one, and "string or
// null" describes the encoding rather than the choice being offered.
func typeName(p jsonSchema) string {
	if ref, ok := p["$ref"].(string); ok {
		name := ref[strings.LastIndex(ref, "/")+1:]
		if title, ok := defTitles[name]; ok {
			return title
		}
		return name
	}
	if c, ok := p["const"]; ok {
		return fmt.Sprintf("%q", c)
	}
	if e, ok := p["enum"].([]any); ok {
		var vs []string
		for _, v := range e {
			vs = append(vs, fmt.Sprintf("%v", v))
		}
		return strings.Join(vs, " | ")
	}
	if names := unionNames(p); names != "" {
		return names
	}

	var types []string
	switch t := p["type"].(type) {
	case string:
		types = []string{t}
	case []any:
		for _, v := range t {
			if s, ok := v.(string); ok && s != "null" {
				types = append(types, s)
			}
		}
	}
	for i, t := range types {
		if t != "array" {
			continue
		}
		if items, ok := p["items"].(map[string]any); ok {
			types[i] = typeName(items) + "[]"
		}
	}
	if len(types) == 0 {
		return "any"
	}
	return strings.Join(types, " | ")
}

func unionNames(p jsonSchema) string {
	for _, key := range []string{"oneOf", "anyOf"} {
		list, ok := p[key].([]any)
		if !ok {
			continue
		}
		var names []string
		for _, raw := range list {
			arm, _ := raw.(map[string]any)
			name := typeName(arm)
			if name != "" && !contains(names, name) {
				names = append(names, name)
			}
		}
		return strings.Join(names, " | ")
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func renderDefPage(p defPage, order int) string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "---\ntitle: %s\ndescription: %s\norder: %d\n---\n",
		yamlString(p.title), yamlString(p.blurb), order)
	fmt.Fprintf(b, "\n<!-- Generated by `make docs-reference` from %s. Do not edit. -->\n", p.from)

	for _, s := range p.sections {
		fmt.Fprintf(b, "\n## %s\n", s.title)
		if s.intro != "" {
			fmt.Fprintf(b, "\n%s\n", escapeProse(s.intro))
		}
		fieldTable(b, s.fields)

		// An arm with a shape needs a heading to hang its table under; a list of scalar forms
		// does not, and giving Shape six headings for six one-line arms buries it.
		shaped := false
		for _, a := range s.arms {
			shaped = shaped || len(a.fields) > 0
		}
		for _, a := range s.arms {
			// `number` and `boolean` carry no description, because a number is a number. A
			// dash with nothing after it reads as prose that went missing.
			if !shaped {
				fmt.Fprintf(b, "\n- `%s`%s\n", a.typ, dashed(a.desc))
				continue
			}
			fmt.Fprintf(b, "\n### `%s`\n%s\n", a.typ, block(a.desc))
			if a.each && len(a.fields) > 0 {
				fmt.Fprint(b, "\nEach element:\n")
			}
			fieldTable(b, a.fields)
		}
	}
	return b.String()
}

func dashed(desc string) string {
	if strings.TrimSpace(desc) == "" {
		return ""
	}
	return " — " + escapeProse(desc)
}

func block(desc string) string {
	if strings.TrimSpace(desc) == "" {
		return ""
	}
	return "\n" + escapeProse(desc) + "\n"
}

func fieldTable(b *strings.Builder, fields []defField) {
	if len(fields) == 0 {
		return
	}
	fmt.Fprint(b, "\n| Field | Type | Required | Description |\n| --- | --- | --- | --- |\n")
	for _, f := range fields {
		req := ""
		if f.required {
			req = "yes"
		}
		// A union renders with pipes, and a pipe ends a table cell -- backticks do not protect
		// it, so the escape has to go inside the code span.
		fmt.Fprintf(b, "| `%s` | `%s` | %s | %s |\n", f.name, strings.ReplaceAll(f.typ, "|", `\|`), req, cell(f.desc))
	}
}

// cell is prose inside a table, where an unescaped pipe would end the column early.
func cell(s string) string {
	return strings.ReplaceAll(escapeProse(s), "|", `\|`)
}

// writeConfigReference renders `.genroc`, the project file genctl reads when no -f is given.
// It shares this file's machinery because it is the same job on a second schema, and it is
// filed under Configuration rather than beside the language: `.genroc` configures the tooling,
// and nothing in it reaches a definition.
func writeConfigReference(dir string) error {
	var root jsonSchema
	if err := json.Unmarshal(defschema.Config(), &root); err != nil {
		return fmt.Errorf("parse the generated config schema: %w", err)
	}
	defs, _ := root["$defs"].(map[string]any)
	resolver, _ := defs["SourcesResolverConfig"].(map[string]any)
	if len(resolver) == 0 {
		return fmt.Errorf("the config schema carries no resolver definition")
	}

	page := defPage{
		title: "Project file", slug: "project-file", order: 5,
		blurb: "`.genroc`, a project's configuration file.",
		from:  "the .genroc config schema",
		sections: []defSection{
			{title: "Keys", fields: fieldsOf(root)},
			{title: "Resolver", intro: "One entry of `resolvers`.", fields: fieldsOf(resolver)},
		},
	}
	if len(page.sections[0].fields) == 0 || len(page.sections[1].fields) == 0 {
		return fmt.Errorf("the project-file page came out empty; the config schema's shape changed")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, page.slug+".md")
	if err := os.WriteFile(path, []byte(renderDefPage(page, page.order)), 0644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d)\n", path, len(page.sections))
	return nil
}
