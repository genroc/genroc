package main

// `genctl schema` hands back a piece of a definition's inferred view, as a schema document
// something else can generate from. Local only: genctl infers the types itself (sources.go),
// so this answers with no server and runs no resolver — an unresolved `$import` types as the
// string it is. specs/schema-command.md.

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/validation"
)

func runSchemaCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: genctl schema context <process> [address] [-e <expression>] [-f <path|glob> ...]\n"+
			"       genctl schema type    <process> [address] [-f <path|glob> ...]")
		os.Exit(1)
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
	fs := flag.NewFlagSet("schema "+v.name, flag.ExitOnError)
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
		doc, err := v.document(def)
		if err != nil {
			fatal("%s: %v", def.Name, err)
		}
		path, err := schema.ParsePath(pos[1])
		if err != nil {
			fatal("%v", err)
		}
		s, err := validation.Navigate(doc, pos[1], path)
		if err != nil {
			fatal("%v%s", err, otherView(v, def, path))
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
		printDoc(*asJSON, selfContained(schemaDoc(s)))
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
		fmt.Printf("%-*s  %s\n", width, a, typeSummary(slots[a]))
	}
}

// typeSummary names a type in one line — its kind, an object's members, an array's element —
// enough to pick the address whose document you want. A `$ref` is followed: the name of a
// definition says less than what it holds.
func typeSummary(s schema.Schema) string {
	if resolved, err := s.Resolve(); err == nil {
		s = resolved
	}
	if members := memberNames(s); members != "" {
		return "object{" + members + "}"
	}
	if items := s.Items(); !items.IsZero() {
		return "array<" + typeSummary(items) + ">"
	}
	return s.TypeName()
}

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
	docs, err := loadSourceDocs(files)
	if err != nil {
		fatal("%v", err)
	}
	var names []string
	for _, sd := range docs {
		name, _ := sd.doc.(map[string]any)["name"].(string)
		if name != process {
			names = append(names, name)
			continue
		}
		def, err := decodeDefinition(sd)
		if err != nil {
			fatal("%v", err)
		}
		return def
	}
	slices.Sort(names)
	fatal("no process named %q in the files read. Found: %s", process, strings.Join(names, ", "))
	return nil
}

// schemaDoc renders one schema as the JSON document it is, `$defs` included — a Schema carries
// its pool, which is what makes an answer self-contained before it is narrowed.
func schemaDoc(s schema.Schema) map[string]any {
	raw, err := json.Marshal(s)
	if err != nil {
		fatal("render schema: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		fatal("render schema: %v", err)
	}
	return doc
}

// selfContained narrows `$defs` to what the document's refs actually reach, so what is printed
// can be piped into a generator whole. Refs BETWEEN definitions are followed, which is what
// keeps a task output that references itself resolvable.
func selfContained(doc map[string]any) map[string]any {
	// The pool travels with every arm of a union, not only with the root, so it is collected
	// from wherever it sits and printed once — three copies of the same definitions is not a
	// document anyone wants to read or pipe.
	pool := map[string]any{}
	body, _ := hoistDefs(doc, pool).(map[string]any)
	if len(pool) == 0 {
		return body
	}

	if kept := reachableDefs(pool, body); len(kept) > 0 {
		body["$defs"] = kept
	}
	return body
}

// reachableDefs is the subset of pool that from can reach, following refs between definitions —
// which is what keeps a task output that references itself resolvable. Shared with the resolver
// manifest, which narrows a process's pool to what its sites' fragments name.
func reachableDefs(pool map[string]any, from ...any) map[string]any {
	want := map[string]bool{}
	for _, v := range from {
		collectRefs(v, want)
	}
	kept := map[string]any{}
	for {
		next := ""
		for name := range want {
			if _, done := kept[name]; !done {
				next = name
				break
			}
		}
		if next == "" {
			return kept
		}
		def, ok := pool[next]
		if !ok {
			// A ref with no definition is a bug upstream, not something to hide by dropping it.
			fatal("schema references $defs/%s, which the pool does not carry", next)
		}
		kept[next] = def
		collectRefs(def, want)
	}
}

// hoistDefs returns v with every `$defs` removed, merging them into pool. The pool is one
// object shared by every level, so merging cannot lose a definition.
func hoistDefs(v any, pool map[string]any) any {
	switch node := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for k, sub := range node {
			if k == "$defs" {
				defs, _ := sub.(map[string]any)
				maps.Copy(pool, defs)
				continue
			}
			out[k] = hoistDefs(sub, pool)
		}
		return out
	case []any:
		out := make([]any, len(node))
		for i, sub := range node {
			out[i] = hoistDefs(sub, pool)
		}
		return out
	}
	return v
}

func collectRefs(v any, out map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "$ref" {
				if s, ok := val.(string); ok {
					if name, ok := strings.CutPrefix(s, "#/$defs/"); ok {
						out[name] = true
					}
				}
				continue
			}
			collectRefs(val, out)
		}
	case []any:
		for _, e := range t {
			collectRefs(e, out)
		}
	}
}

// listing is every slot keyed by its address, over one shared pool: the same schema appears at
// several addresses, so a pool per entry would repeat most of the answer.
func listing(slots map[string]schema.Schema) map[string]any {
	out := map[string]any{}
	for address, s := range slots {
		out[address] = schemaDoc(s)
	}
	// selfContained hoists every entry's pool into one at the root — the same schema appears at
	// several addresses, so a pool per entry would repeat most of the answer.
	return selfContained(out)
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
	required := requiredSet(ctx)
	var roots []string
	for _, name := range slices.Sorted(maps.Keys(props)) {
		root := name
		if !required[name] {
			root += "?"
		}
		if name == "self" || name == "outputs" {
			if inner := memberNames(props[name]); inner != "" {
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
func memberNames(root schema.Schema) string {
	props := root.Properties()
	if len(props) == 0 {
		return ""
	}
	required := requiredSet(root)
	names := make([]string, 0, len(props))
	for _, name := range slices.Sorted(maps.Keys(props)) {
		label := name
		if !required[name] {
			label += "?"
		}
		// `=null` is what one arm says about an output another arm sets: present, and null
		// here. It is the correlation the arms exist to carry, so it has to be visible.
		if props[name].IsNull() {
			label += "=null"
		}
		names = append(names, label)
	}
	return strings.Join(names, ", ")
}

func requiredSet(s schema.Schema) map[string]bool {
	out := make(map[string]bool)
	for _, name := range s.Required() {
		out[name] = true
	}
	return out
}

// A schema reads in a fixed order — what it IS, then what it holds, then the pool it resolves
// against — which neither encoding/json nor yaml.v3 will do for a map: both sort keys, so
// `properties` lands before `type` and `$defs` before either. The order below is the order the
// keywords are usually read in; anything unrecognised follows, sorted, so a new keyword shows up
// rather than disappearing.
var keywordOrder = []string{
	"description", "$ref", "type", "oneOf", "anyOf", "allOf", "enum", "default",
	"properties", "required", "additionalProperties", "items",
	"minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems",
	"secret", "$anchor", "$id", "$defs",
}

// pair is one key and its value, in the order it is printed.
type pair struct {
	key string
	val any
}

// document is an ordered object, and the only reason it exists is that both encoders sort.
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

func (d document) MarshalYAML() (any, error) {
	out := &yaml.Node{Kind: yaml.MappingNode}
	for _, p := range d {
		key, val := &yaml.Node{}, &yaml.Node{}
		if err := key.Encode(p.key); err != nil {
			return nil, err
		}
		if err := val.Encode(p.val); err != nil {
			return nil, err
		}
		out.Content = append(out.Content, key, val)
	}
	return out, nil
}

// ordered rebuilds a decoded document with its keys in reading order, recursively. A map whose
// keys are not keywords — `properties`, `$defs`, an address listing — keeps them sorted, which
// is the order they are looked up in.
func ordered(v any) any {
	switch node := v.(type) {
	case map[string]any:
		out := make(document, 0, len(node))
		seen := make(map[string]bool, len(node))
		for _, key := range keywordOrder {
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
func printYAML(v any) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(ordered(v)); err != nil {
		fatal("render: %v", err)
	}
	enc.Close()
	fmt.Print(b.String())
}
