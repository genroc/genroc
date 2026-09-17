package main

// `genctl schema` hands back a piece of a definition's inferred view, as a schema document
// something else can generate from. Local only: genctl infers the types itself (sources.go),
// so this answers with no server and runs no resolver — an unresolved `$import` types as the
// string it is. specs/schema-command.md.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/sources"
	"genroc/internal/validation"
)

func runSchemaCmd(args []string) {
	if len(args) == 0 {
		missingSubcommand("schema")
	}
	if hasHelpArg(args[:1]) {
		helpFor("schema")
		return
	}
	switch args[0] {
	case "context":
		runSchemaViewCmd(contextView, args[1:])
	case "type":
		runSchemaViewCmd(typeView, args[1:])
	default:
		fatal("unknown subcommand %q: genctl schema <context|type> <process> [address]", args[0])
	}
}

// A view is one question asked of a process. The two differ in the document they build and how
// a listing reads — everything else (the file rules, `--json`, navigation, `-e`, what stdout may
// carry) is the command's, so a change to any of it cannot reach one view and not the other.
type schemaView struct {
	name string
	// document is the whole view as one schema. An address is a path into it and nothing else,
	// so there is no address grammar left to differ between the views.
	document func(*model.ProcessDefinition) (schema.Schema, error)
	// slots is the same content flattened, for the listing: one line per address.
	slots    func(*model.ProcessDefinition) (map[string]schema.Schema, error)
	render   func(map[string]schema.Schema)
	exprHelp string
	example  string
	jsonHelp string
}

var contextView = schemaView{
	name:     "context",
	slots:    validation.SlotContexts,
	document: validation.ContextDocument,
	render:   printInScope,
	exprHelp: "type this expression against the schema the address selected, bare: self.result.fee",
	example:  "tasks.price.output",
	jsonHelp: "print JSON: the documents rather than a summary of what is in scope, and a schema as JSON rather than YAML",
}

var typeView = schemaView{
	name:     "type",
	slots:    validation.TypeSlots,
	document: validation.TypeDocument,
	render:   printTypes,
	exprHelp: "type this expression against the schema the address selected, bare: items[0].sku",
	example:  "tasks.price.result",
	jsonHelp: "print JSON: the documents rather than a summary of what each is, and a schema as JSON rather than YAML",
}

// runSchemaViewCmd is both subcommands: an address answers with one document, no address lists
// what can be asked. specs/schema-command.md.
func runSchemaViewCmd(v schemaView, args []string) {
	fs := newFlagSet("schema "+v.name, args)
	fs.String("f", "", "definition file or glob; an existing path is never globbed. Takes several, "+
		"and repeats")
	asJSON := fs.Bool("json", false, v.jsonHelp)
	expr := fs.String("e", "", v.exprHelp)
	files, rest := takeFileValues(args)
	pos := parseArgs(fs, rest)
	if len(pos) == 0 {
		fatal("genctl schema %s <process> [address]: name the process", v.name)
	}
	if len(pos) > 2 {
		fatal("%s: unexpected argument. A slot is one address, e.g. %s", pos[2], v.example)
	}
	if *expr != "" && len(pos) < 2 {
		fatal("-e types an expression at one slot, so it needs an address:\n"+
			"  genctl schema %s %s <address> -e '%s'", v.name, pos[0], *expr)
	}

	def := loadDefinition(files, pos[0])
	if len(pos) == 2 {
		path, err := schema.ParsePath(pos[1])
		if err != nil {
			fatal("%v", err)
		}
		// The flat slots answer first, because a slot address may be a PREFIX of another one
		// and the nested document cannot tell the two apart: `tasks.a.switch` would come back
		// carrying its own case indexes as names in scope. The document still answers for an
		// intermediate node and words every miss.
		slots, err := v.slots(def)
		if err != nil {
			fatal("%s: %v", def.Name, err)
		}
		s, found, err := validation.SlotAt(slots, pos[1])
		if found && err != nil {
			fatal("%v%s", err, otherView(v, def, path))
		}
		if !found {
			doc, err := v.document(def)
			if err != nil {
				fatal("%s: %v", def.Name, err)
			}
			s, err = validation.Navigate(doc, pos[1], path)
			if err != nil {
				fatal("%v%s", err, otherView(v, def, path))
			}
		}
		if *expr != "" {
			// Availability before inference, the order the checker runs them in: "not readable
			// here" beats the "field not found" the schema would answer with. It answers only
			// where the address named a slot; inside one, nothing is being written.
			if err := validation.CheckSlotRoots(def, pos[1], *expr); err != nil {
				fatal("%v", err)
			}
			s = inferExpr(s, *expr)
		}
		printDoc(*asJSON, mustSelfContained(mustSchemaDoc(s)))
		return
	}

	slots, err := v.slots(def)
	if err != nil {
		fatal("%s: %v", def.Name, err)
	}
	if *asJSON {
		printJSON(listing(slots))
		return
	}
	v.render(slots)
}

// otherView names the sibling when the address it could not find is one the OTHER view answers.
// The two share an address space, so a miss here is usually a question asked of the wrong half —
// `tasks.x.switch` has a context and no type, `tasks.x.result` a type and no context.
func otherView(v schemaView, def *model.ProcessDefinition, path []schema.Segment) string {
	other := typeView
	if v.name == typeView.name {
		other = contextView
	}
	doc, err := other.document(def)
	if err != nil {
		return ""
	}
	if _, err := validation.Navigate(doc, "", path); err != nil {
		return ""
	}
	return fmt.Sprintf("\n`genctl schema %s` has it: that address is a %s, not a %s",
		other.name, other.name, v.name)
}

// printTypes is the human answer: one line per address naming what is there. The documents are
// `--json`, or one address at a time.
func printTypes(slots map[string]schema.Schema) {
	addresses := slices.Sorted(maps.Keys(slots))
	width := 0
	for _, a := range addresses {
		width = max(width, len(a))
	}
	for _, a := range addresses {
		fmt.Printf("%-*s  %s\n", width, a, summaryOf(slots[a]))
	}
}

// summaryOf is schema.Summary, kept as a name this file already reads by.
func summaryOf(s schema.Schema) string { return s.Summary() }

// inferExpr types one expression against a slot's context: the context query with its last
// step taken. The expression is BARE — the `${…}` a leaf wraps it in belongs to the template
// layer, which types every interpolated string as `string` and so answers nothing.
func inferExpr(ctx schema.Schema, expr string) schema.Schema {
	t, err := ctx.Infer(expr)
	if err != nil {
		fatal("%v%s", err, unwrapHint(expr))
	}
	return t
}

// unwrapHint catches the likely paste — a leaf copied out of the YAML — whose parse error
// otherwise names a `$` and not the wrapper it came from.
func unwrapHint(expr string) string {
	trimmed := strings.TrimSpace(expr)
	inner, ok := strings.CutPrefix(trimmed, "$:")
	if !ok {
		if body, isBlock := strings.CutPrefix(trimmed, "${"); isBlock {
			inner, ok = strings.CutSuffix(body, "}")
		}
	}
	if !ok {
		if strings.Contains(trimmed, "${") {
			return "\n-e takes one expression, without the ${…} a leaf wraps it in"
		}
		return ""
	}
	return fmt.Sprintf("\n-e takes the expression itself: -e '%s'", strings.TrimSpace(inner))
}

// loadDefinition reads the named process out of the file set, leaving directives where they
// are: a code string is opaque to inference, so `$import: ./x.ts` types as the string it is
// and no resolver has to run for a query. specs/source-resolution.md §"Why the placeholder is
// sound".
func loadDefinition(files []string, process string) *model.ProcessDefinition {
	files, err := definitionPaths(files)
	if err != nil {
		fatal("%v", err)
	}
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "genctl: no files given, and no `definitions:` in .genroc")
		os.Exit(1)
	}
	docs, err := sources.LoadDocs(files)
	if err != nil {
		fatal("%v", err)
	}
	// The STRUCTURAL phase only: it changes the types this command reports, so skipping it
	// would answer about a definition nobody applies. The code phase is skipped on purpose --
	// it shells out, and a string splice cannot move a type anyway.
	if cfg, err := sources.FindProjectConfig(filepath.Dir(files[0])); err == nil {
		if _, err := sources.ResolveStructuralPass(docs, cfg, nil); err != nil {
			fatal("%v", err)
		}
	}
	var names []string
	for _, sd := range docs {
		name, _ := sd.Value.(map[string]any)["name"].(string)
		if name != process {
			names = append(names, name)
			continue
		}
		def, err := sources.DecodeDefinition(sd)
		if err != nil {
			fatal("%v", err)
		}
		return def
	}
	slices.Sort(names)
	fatal("no process named %q in the files read. Found: %s", process, strings.Join(names, ", "))
	return nil
}

// listing is every slot keyed by its address, over one shared pool: the same schema appears at
// several addresses, so a pool per entry would repeat most of the answer.
func listing(slots map[string]schema.Schema) map[string]any {
	out := map[string]any{}
	for address, s := range slots {
		out[address] = mustSchemaDoc(s)
	}
	// selfContained hoists every entry's pool into one at the root — the same schema appears at
	// several addresses, so a pool per entry would repeat most of the answer.
	return mustSelfContained(out)
}

// printInScope is the human answer: one line per slot naming what it can read. The schemas
// themselves repeat their fixed part at every address, so the names are the readable part —
// `--json` is for the documents.
func printInScope(slots map[string]schema.Schema) {
	addresses := slices.Sorted(maps.Keys(slots))
	width := 0
	for _, a := range addresses {
		width = max(width, len(a))
	}
	for _, a := range addresses {
		// A context with arms prints one line each, indented under the address it belongs to.
		lines := strings.ReplaceAll(inScope(slots[a]), "\n", "\n"+strings.Repeat(" ", width+2))
		fmt.Printf("%-*s  %s\n", width, a, lines)
	}
}

// inScope names a context's roots, spelling out the members of the two that vary — `self` and
// `outputs` — and marking with `?` what may be absent. A context with several ARMS is one per
// state the slot can be evaluated in — the process output has one per way the process ends —
// and each is named by its own description.
func inScope(ctx schema.Schema) string {
	if arms := ctx.Variants(); len(arms) > 0 {
		lines := make([]string, 0, len(arms))
		for _, arm := range arms {
			label := arm.Description()
			if label == "" {
				label = "one state"
			}
			lines = append(lines, label+": "+inScope(arm))
		}
		return strings.Join(lines, "\n")
	}
	props := ctx.Properties()
	var roots []string
	for _, name := range slices.Sorted(maps.Keys(props)) {
		root := name
		if ctx.MayBeAbsent(name) {
			root += "?"
		}
		if name == "self" || name == "outputs" {
			if inner := memberNamesOf(props[name]); inner != "" {
				root += "{" + inner + "}"
			}
		}
		roots = append(roots, root)
	}
	if len(roots) == 0 {
		return "(nothing)"
	}
	return strings.Join(roots, ", ")
}

// memberNames spells out one root's own properties, `?` for the ones a path may not set —
// which outputs and self differ by, and is the whole reason to look.
func memberNamesOf(s schema.Schema) string { return s.MemberNames() }

// pair is one key and its value, in the order it is printed.
type pair struct {
	key string
	val any
}

// document is an ordered object, and the only reason it exists is that encoding/json sorts. The
// YAML half is yamlout.go's, which orders from the same schema.KeywordOrder().
type document []pair

func (d document) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, p := range d {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := json.Marshal(p.key)
		if err != nil {
			return nil, err
		}
		val, err := json.Marshal(p.val)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(val)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// ordered rebuilds a decoded document with its keys in reading order, recursively. A map whose
// keys are not keywords — `properties`, `$defs`, an address listing — keeps them sorted, which
// is the order they are looked up in.
func ordered(v any) any {
	switch node := v.(type) {
	case map[string]any:
		out := make(document, 0, len(node))
		seen := make(map[string]bool, len(node))
		for _, key := range schema.KeywordOrder() {
			if val, ok := node[key]; ok {
				out = append(out, pair{key, ordered(val)})
				seen[key] = true
			}
		}
		for _, key := range slices.Sorted(maps.Keys(node)) {
			if !seen[key] {
				out = append(out, pair{key, ordered(val(node, key))})
			}
		}
		return out
	case []any:
		out := make([]any, len(node))
		for i, item := range node {
			out[i] = ordered(item)
		}
		return out
	}
	return v
}

func val(m map[string]any, key string) any { return m[key] }

// printDoc: a schema is YAML unless JSON was asked for. stdout still carries the document and
// nothing else — the choice is which surface syntax, not whether to decorate it.
func printDoc(asJSON bool, v any) {
	if asJSON {
		printJSON(v)
		return
	}
	printYAML(v)
}

func printJSON(v any) {
	b, err := json.MarshalIndent(ordered(v), "", "  ")
	if err != nil {
		fatal("render: %v", err)
	}
	fmt.Println(string(b))
}

// printYAML is the default for a schema: it is the language definitions are written in, so an
// answer can be pasted into one, and it spends no lines on punctuation.
func printYAML(v any) { printYAMLDoc(v, schema.KeywordOrder()) }

// The CLI's answer to a render failure is what it always was -- exit with the message. The
// library returns an error instead because the language server shares this code and a
// library that exits takes the editor's session with it.
func mustSchemaDoc(s schema.Schema) map[string]any {
	doc, err := sources.SchemaDoc(s)
	if err != nil {
		fatal("%v", err)
	}
	return doc
}

func mustSelfContained(doc map[string]any) map[string]any {
	out, err := sources.SelfContained(doc)
	if err != nil {
		fatal("%v", err)
	}
	return out
}
