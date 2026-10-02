package main

// `genctl schema`: a piece of a definition's inferred view, answered locally with no server. Runs
// the structural phase, never the code phase, so an unresolved `$import` types as a string.
// specs/schema-command.md.

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

// Views differ only in the document they build and how a listing reads; everything else is the
// command's, so a change to it cannot reach one view and not the other.
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
	example:  "tasks.price.action.result",
	jsonHelp: "print JSON: the documents rather than a summary of what each is, and a schema as JSON rather than YAML",
}

// runSchemaViewCmd is both subcommands: an address answers with one document, no address lists
// what can be asked. specs/schema-command.md.
func runSchemaViewCmd(v schemaView, args []string) {
	fs := newFlagSet("schema "+v.name, args)
	fs.String("f", "", "definition file or glob; takes several, and repeats")
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
		// Flat slots first: a slot address may PREFIX another, and the nested document would answer
		// `tasks.a.switch` with its case indexes in scope. The document handles the rest.
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
			// Availability before inference, as the checker orders them: "not readable here" beats
			// the schema's "field not found".
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

// otherView points at the sibling view when it answers the missed address: `tasks.x.switch` has a
// context and no type, `tasks.x.result` a type and no context.
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

// inferExpr takes the expression BARE: the `${…}` wrapper belongs to the template layer, which
// types every interpolated string as `string`.
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

// loadDefinition leaves code directives in place: a code string is opaque to inference.
// specs/source-resolution.md §"Why the placeholder is sound".
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
	// The STRUCTURAL phase moves types, so it must run; the code phase shells out and moves none.
	// A malformed config is fatal: swallowed, it surfaces as a decode error in the definition.
	cfg, err := sources.FindProjectConfig(filepath.Dir(files[0]))
	if err != nil {
		fatal("%v", err)
	}
	if _, err := sources.ResolveStructuralPass(docs, cfg, nil); err != nil {
		fatal("%v", err)
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
	return mustSelfContained(out)
}

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

// inScope spells out members only for `self` and `outputs`, the two roots that vary. A context's
// ARMS are one per state the slot can be evaluated in, each named by its description.
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

func memberNamesOf(s schema.Schema) string { return s.MemberNames() }

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

// ordered puts keywords in reading order; non-keyword keys (`properties`, `$defs`, an address
// listing) stay sorted, the order they are looked up in.
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

// printDoc: stdout carries the document and nothing else, in either syntax.
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

// printYAML is the default: definitions are written in it, so an answer can be pasted into one.
func printYAML(v any) { printYAMLDoc(v, schema.KeywordOrder()) }

// Exit here, never in sources: the language server shares it, and a library that exits takes the
// editor's session with it.
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
