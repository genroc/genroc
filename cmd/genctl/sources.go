package main

// Source resolution: a definition source file is not a definition. A `$<resolver>: <path>`
// leaf is replaced by a string a registered binary produces. See
// specs/source-resolution.md for the phase rule and the manifest contract.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"genroc/internal/model"
	"genroc/internal/numeric"
	"genroc/internal/validation"

	"gopkg.in/yaml.v3"

	"genroc/internal/schema"
)

// A dotfile with no extension, like .eslintrc or .npmrc. Deliberately NOT `*.genroc.yaml`:
// that suffix means "a process definition", and a settings file sharing it reads as one.
const projectConfigName = ".genroc"

// legacyProjectConfigName is what projects created before the rename use. Read only, and only
// when the current name is absent -- an existing checkout keeps working without an edit.
const legacyProjectConfigName = "genroc.yaml"

type resolverConfig struct {
	Phase   string   `yaml:"phase"`
	Ext     string   `yaml:"ext"`
	Command []string `yaml:"command"`
	// Types is what this resolver wants typed, as name → address, and it is the whole reason
	// genctl no longer decides: a toolchain knows which slot its runtime binds, genroc does
	// not. Addresses are `genctl schema type`'s, RELATIVE to the task the directive sits in —
	// `input.input` is the argument an evaluator binds out of the action's input. Absent means
	// the resolver wants none.
	Types map[string]string `yaml:"types"`
}

type projectConfig struct {
	Root string `yaml:"-"`
	// Definitions is what `genctl apply|validate|types` reads when given no paths. Entries are
	// files, directories (walked) or globs, resolved against the config's own directory -- so
	// the command works the same from anywhere in the project.
	Definitions []string                  `yaml:"definitions"`
	Resolvers   map[string]resolverConfig `yaml:"resolvers"`
}

// defaultDefinitionPaths is what a bare `genctl apply` operates on: the `definitions` entries
// from the nearest .genroc, made absolute. Empty when there is no config or no entries, which
// the caller reports as "-f is required" rather than as a project error.
func defaultDefinitionPaths(dir string) []string {
	cfg, err := findProjectConfig(dir)
	if err != nil || len(cfg.Definitions) == 0 {
		return nil
	}
	out := make([]string, 0, len(cfg.Definitions))
	for _, d := range cfg.Definitions {
		if filepath.IsAbs(d) {
			out = append(out, d)
			continue
		}
		out = append(out, filepath.Join(cfg.Root, d))
	}
	return out
}

// sourceDoc is one definition together with the file it was read from: a directive's path is
// relative to that file, and several files in one apply may sit in different directories.
type sourceDoc struct {
	doc  any
	file string
}

// site is one directive occurrence. The exported fields are the manifest's; loc is how
// splice finds the slot again, and is why nothing re-walks the document to apply the result.
type site struct {
	// Resolver groups the sites; it is not on the wire, because the manifest goes to exactly
	// that resolver and a binary being told its own name learns nothing. Process is the same
	// kind of field: the manifest nests sites UNDER their process, so it is not on the wire
	// either — but the pass needs it to know which types to resolve against.
	Resolver string `json:"-"`
	Process  string `json:"-"`
	Task     string `json:"task,omitempty"`
	// Action is the task's action type and Child the process a child action calls: what the
	// site IS, which a resolver would otherwise have to read the definition for. Facts about
	// the site, not steps in its address.
	Action string `json:"action,omitempty"`
	Child  string `json:"child,omitempty"`
	// Pointer is where the directive sits, shaped like the definition: keys and indices rather
	// than an RFC 6901 string, because a recipient would otherwise unescape `~0`/`~1`, and a
	// string cannot tell the object key "0" from index 0 (specs/object-store.md made the same
	// choice for the same reason).
	Pointer []any `json:"pointer"`
	// Argument is everything after `$<resolver>:`, verbatim. genctl does not interpret it —
	// a resolver that takes a file joins it to its process's `dir`, one that takes a URL or a
	// package name reads it as that.
	Argument string `json:"argument"`
	// Types are the fragments this resolver asked for, keyed by the name it chose.
	Types map[string]any `json:"types,omitempty"`

	loc    []any
	docIdx int
}

type manifest struct {
	Mode string `json:"mode"`
	// One entry per process that has a site, in the order the files were read. Sites nest under
	// the process they are in, so nothing has to be joined by name.
	Processes []manifestProcess `json:"processes"`
}

// manifestProcess is one definition's sites, and the definitions they need. `$defs` is narrowed
// to what the fragments below it actually reach — a `$ref` survives because a task output may
// reference itself, but nothing unreferenced travels.
type manifestProcess struct {
	Name string `json:"name"`
	// Dir and File are the definition's own location, split because a relative argument is
	// relative to the DIRECTORY: joining is the resolver's to do, and it needs the base.
	Dir   string         `json:"dir"`
	File  string         `json:"file"`
	Sites []site         `json:"sites"`
	Defs  map[string]any `json:"$defs,omitempty"`
}

// flatten is the order `code` answers in: processes as they appear, sites within each as they
// do. The splice reads the reply by position, so the manifest's own order is the contract.
func (m manifest) flatten() []site {
	var out []site
	for _, p := range m.Processes {
		out = append(out, p.Sites...)
	}
	return out
}

type resolverReply struct {
	Code []string `json:"code"`
}

// directiveRe matches a whole leaf of the form `$name: path`. A leaf beginning `$$` cannot
// match — the second character must be a letter — which is what leaves the escape to the
// template layer instead of unescaping it twice (specs/typed-values.md).
var directiveRe = regexp.MustCompile(`^\s*\$([a-zA-Z][a-zA-Z0-9_-]*):\s*(\S.*?)\s*$`)

// ── project config ─────────────────────────────────────────────────────────────

// findProjectConfig walks up from dir for .genroc. Absent is not an error: a project
// with no resolvers registered is the normal case, and a directive then fails by name.
// readProjectConfig returns the first config present in dir, current name before legacy.
func readProjectConfig(dir string) (string, []byte, bool) {
	for _, name := range []string{projectConfigName, legacyProjectConfigName} {
		path := filepath.Join(dir, name)
		if data, err := os.ReadFile(path); err == nil {
			return path, data, true
		}
	}
	return "", nil, false
}

func findProjectConfig(dir string) (projectConfig, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return projectConfig{}, err
	}
	for {
		path, data, found := readProjectConfig(abs)
		if found {
			var cfg projectConfig
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return projectConfig{}, fmt.Errorf("%s: %w", path, err)
			}
			cfg.Root = abs
			for name, r := range cfg.Resolvers {
				if r.Phase != "code" {
					return projectConfig{}, fmt.Errorf("%s: resolver %q has phase %q - only \"code\" is implemented", path, name, r.Phase)
				}
				if len(r.Command) == 0 {
					return projectConfig{}, fmt.Errorf("%s: resolver %q has no command", path, name)
				}
				for typeName, address := range r.Types {
					if _, err := framed(address, "x"); err != nil {
						return projectConfig{}, fmt.Errorf("%s: resolver %q, type %q: %w",
							path, name, typeName, err)
					}
				}
			}
			return cfg, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return projectConfig{}, nil
		}
		abs = parent
	}
}

// ── finding sites ──────────────────────────────────────────────────────────────

// findSites walks every document for directive leaves. What follows the resolver's name is
// passed on VERBATIM: genctl does not know that an argument is a path, let alone that it is a
// file, so it neither resolves nor stats it. The resolver joins it to the definition's own
// directory, which the manifest carries beside it.
func findSites(docs []sourceDoc, cfg projectConfig) ([]site, error) {
	var out []site
	for i, sd := range docs {
		name, _ := sd.doc.(map[string]any)["name"].(string)
		var walk func(node any, loc []any) error
		walk = func(node any, loc []any) error {
			switch v := node.(type) {
			case map[string]any:
				for k, child := range v {
					if err := walk(child, append(loc, k)); err != nil {
						return err
					}
				}
			case []any:
				for j, child := range v {
					if err := walk(child, append(loc, j)); err != nil {
						return err
					}
				}
			case string:
				m := directiveRe.FindStringSubmatch(v)
				if m == nil {
					return nil
				}
				resolver, argument := m[1], m[2]
				rc, ok := cfg.Resolvers[resolver]
				if !ok {
					return fmt.Errorf("%s: %s: no resolver named %q is registered in %s",
						sd.file, renderPointer(slotPointer(sd.doc, loc)), resolver, projectConfigName)
				}
				// `ext` is a suffix assertion on the ARGUMENT, not a claim that it names a
				// file: it is what makes a `.py` handed to the TypeScript toolchain fail here
				// with a sentence rather than inside `tsc` with a stack.
				if rc.Ext != "" && !strings.EqualFold(filepath.Ext(argument), rc.Ext) {
					return fmt.Errorf("%s: %s: resolver %q accepts %s files, but %q is not one",
						sd.file, renderPointer(slotPointer(sd.doc, loc)), resolver, rc.Ext, argument)
				}
				s := site{
					Resolver: resolver,
					Process:  name,
					Pointer:  slotPointer(sd.doc, loc),
					Argument: argument,
					loc:      append([]any(nil), loc...),
					docIdx:   i,
				}
				s.Task = enclosingTaskID(sd.doc, loc)
				if task := enclosingTask(sd.doc, loc); task != nil {
					action, _ := task["action"].(map[string]any)
					s.Action, _ = action["type"].(string)
					s.Child, _ = action["name"].(string)
				}
				out = append(out, s)
			}
			return nil
		}
		if err := walk(sd.doc, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// enclosingTaskID reports the id of the task a site sits under. What that task RETURNS is not
// read here: it is `TaskSchemas.Result`, which inference already computed (taskResult).
func enclosingTaskID(doc any, loc []any) string {
	task := enclosingTask(doc, loc)
	if task == nil {
		return ""
	}
	id, _ := task["id"].(string)
	return id
}

// enclosingTask is the task a site sits under, as the raw document holds it.
func enclosingTask(doc any, loc []any) map[string]any {
	if len(loc) < 2 {
		return nil
	}
	key, ok := loc[0].(string)
	if !ok || key != "tasks" {
		return nil
	}
	idx, ok := loc[1].(int)
	if !ok {
		return nil
	}
	tasks, ok := doc.(map[string]any)["tasks"].([]any)
	if !ok || idx >= len(tasks) {
		return nil
	}
	task, _ := tasks[idx].(map[string]any)
	return task
}

// slotPointer is where a directive sits, shaped like the definition: the task by ID rather than
// by index, and then the document's own keys, `action` included. What KIND of action it is, and
// which process a child calls, are facts about the site rather than steps in a path — they are
// fields beside it. specs/source-resolution.md.
func slotPointer(doc any, loc []any) []any {
	if len(loc) < 2 || loc[0] != "tasks" {
		return append([]any(nil), loc...)
	}
	id, _ := enclosingTask(doc, loc)["id"].(string)
	if id == "" {
		return append([]any(nil), loc...)
	}
	return append([]any{"tasks", id}, loc[2:]...)
}

// actionType is what the task's action declares, empty for a routing task or a definition the
// server has not judged yet.
func actionType(task map[string]any) string {
	action, _ := task["action"].(map[string]any)
	kind, _ := action["type"].(string)
	return kind
}

// renderPointer spells a pointer as the address it is, for a message. A key no identifier can
// spell is quoted, which is what keeps a task id holding a dot readable.
func renderPointer(pointer []any) string {
	out := ""
	for _, seg := range pointer {
		switch v := seg.(type) {
		case string:
			out = schema.JoinPath(out, v)
		case int:
			out = schema.JoinIndex(out, v)
		}
	}
	if out == "" {
		return "."
	}
	return out
}

// ── splicing ───────────────────────────────────────────────────────────────────

// splice writes value at the site's slot. It mutates the document in place, which is what
// lets the placeholder pass and the real pass share one parse.
func splice(docs []sourceDoc, s site, value any) error {
	node := docs[s.docIdx].doc
	for i, seg := range s.loc {
		last := i == len(s.loc)-1
		switch k := seg.(type) {
		case string:
			m, ok := node.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: cannot descend into %s", docs[s.docIdx].file, s.Pointer)
			}
			if last {
				m[k] = value
				return nil
			}
			node = m[k]
		case int:
			a, ok := node.([]any)
			if !ok || k >= len(a) {
				return fmt.Errorf("%s: cannot descend into %s", docs[s.docIdx].file, s.Pointer)
			}
			if last {
				a[k] = value
				return nil
			}
			node = a[k]
		}
	}
	return fmt.Errorf("%s: empty pointer", docs[s.docIdx].file)
}

// escapeDollars doubles every `$`. Every string leaf is read as a template
// (internal/shape → template.Get), and scanTemplate collapses `$$` to a literal `$`
// unconditionally — so doubling round-trips ANY byte sequence, where escaping only `${`
// would corrupt a script that contains a literal `$$`.
func escapeDollars(s string) string { return strings.ReplaceAll(s, "$", "$$") }

// ── running a resolver ─────────────────────────────────────────────────────────

func runResolver(cfg projectConfig, rc resolverConfig, m manifest) ([]string, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(rc.Command[0], rc.Command[1:]...)
	cmd.Dir = cfg.Root
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// The exit code IS the type check: stderr is the diagnostic, printed as the
		// resolver wrote it rather than wrapped in a Go error.
		msg := strings.TrimRight(stderr.String(), "\n")
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("resolver %q failed:\n%s", strings.Join(rc.Command, " "), msg)
	}
	if m.Mode == "types" {
		println(stdout.String())
		return nil, nil
	}
	var reply resolverReply
	if err := json.Unmarshal(stdout.Bytes(), &reply); err != nil {
		return nil, fmt.Errorf("resolver %q: stdout is not the expected {\"code\": [...]}: %w",
			strings.Join(rc.Command, " "), err)
	}
	if want := len(m.flatten()); len(reply.Code) != want {
		return nil, fmt.Errorf("resolver %q returned %d strings for %d sites",
			strings.Join(rc.Command, " "), len(reply.Code), want)
	}
	return reply.Code, nil
}

// ── the pass ───────────────────────────────────────────────────────────────────

// resolveDocs resolves every code-phase directive in docs, mutating them in place. mode
// "build" splices the returned strings; mode "types" stops after the resolver has written
// its declarations, which is what `genctl types` runs between applies.
//
// It returns the number of sites resolved; zero means nothing was imported and — the point
// of the check — no work was done.
func resolveDocs(docs []sourceDoc, mode string) (int, error) {
	if len(docs) == 0 {
		return 0, nil
	}
	cfg, err := findProjectConfig(filepath.Dir(docs[0].file))
	if err != nil {
		return 0, err
	}
	sites, err := findSites(docs, cfg)
	if err != nil {
		return 0, err
	}
	if len(sites) == 0 {
		return 0, nil
	}

	// The placeholder pass. A code string is opaque to inference, so an empty string types
	// identically to the real one and the schemas below are the schemas of what is applied.
	for _, s := range sites {
		if err := splice(docs, s, ""); err != nil {
			return 0, err
		}
	}
	schemas, err := inferSchemas(docs, sites)
	if err != nil {
		return 0, err
	}

	byResolver := map[string][]site{}
	var order []string
	for _, s := range sites {
		if _, seen := byResolver[s.Resolver]; !seen {
			order = append(order, s.Resolver)
		}
		byResolver[s.Resolver] = append(byResolver[s.Resolver], s)
	}

	for _, name := range order {
		group := byResolver[name]
		for i := range group {
			types, err := siteTypes(schemas, cfg.Resolvers[name].Types, group[i])
			if err != nil {
				return 0, err
			}
			group[i].Types = types
		}
		m := manifest{Mode: mode, Processes: byProcess(schemas, docs, group)}
		code, err := runResolver(cfg, cfg.Resolvers[name], m)
		if err != nil {
			return 0, err
		}
		if mode == "types" {
			continue
		}
		// By the manifest's own order, not the group's: nesting sites under their process may
		// interleave two files differently, and `code` answers what the resolver was shown.
		for i, s := range m.flatten() {
			if err := splice(docs, s, escapeDollars(code[i])); err != nil {
				return 0, err
			}
		}
	}
	return len(sites), nil
}

// inferSchemas types the definitions that carry a directive. genctl computes the types and
// the server decides validity, which is why neither the strict decode, nor Validate, nor the
// child-reference check the endpoint ran is reproduced here. specs/source-resolution.md.
func inferSchemas(docs []sourceDoc, sites []site) (map[string]validation.SchemaFile, error) {
	needed := make(map[string]bool, len(sites))
	for _, s := range sites {
		needed[s.Process] = true
	}
	out := make(map[string]validation.SchemaFile, len(needed))
	for _, sd := range docs {
		// Keyed off the raw document, like the sites this answers. A definition with no
		// directive is never typed: one broken file must not stop a project-wide `types`.
		name, _ := sd.doc.(map[string]any)["name"].(string)
		if !needed[name] {
			continue
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("two definitions named %q in one apply - schemas are keyed by process name", name)
		}
		def, err := decodeDefinition(sd)
		if err != nil {
			return nil, err
		}
		sf, err := validation.Generate(def)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", sd.file, name, err)
		}
		out[name] = sf
	}
	return out, nil
}

// taskInput is the inferred type of the task's action input, which validation already
// computed (validation.buildInputs). It may be a $ref into that process's own $defs pool.
// A zero schema returns nil so the manifest omits the key rather than carrying `null`.
// decodeDefinition reads one source document as a definition. Non-strict and without
// Validate: genctl computes the types, the server decides validity (§inferSchemas).
func decodeDefinition(sd sourceDoc) (*model.ProcessDefinition, error) {
	raw, err := json.Marshal(sd.doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", sd.file, err)
	}
	var def model.ProcessDefinition
	if err := numeric.Decode(raw, &def); err != nil {
		return nil, fmt.Errorf("%s: %w", sd.file, err)
	}
	return &def, nil
}

// Frames. An address in `types` names the one it is relative to, because a site can be inside a
// task or not and `input` would otherwise mean the action's here and the process's there —
// silently, since both frames have one. specs/source-resolution.md.
const (
	frameTask    = "task"
	frameProcess = "process"
)

// framed turns a request into a path into the type document. A `task.` address at a site that is
// in no task returns nil, nil: the answer is null, which is the honest one — a task-relative
// request has nothing to resolve against there.
func framed(address, task string) ([]schema.Segment, error) {
	segs, err := schema.ParsePath(address)
	if err != nil {
		return nil, err
	}
	switch segs[0].Name {
	case frameTask:
		if task == "" {
			return nil, nil
		}
		return append([]schema.Segment{{Name: "tasks"}, {Name: task}}, segs[1:]...), nil
	case frameProcess:
		return segs[1:], nil
	}
	return nil, fmt.Errorf("%q names no frame: an address starts with %q (the task this import "+
		"sits in) or %q (the definition)", address, frameTask, frameProcess)
}

// byProcess nests the group's sites under the definition they are in, in the order the files
// were read, and gives each the definitions its own fragments reach — never the whole pool, and
// never another process's: a `$ref` inside a fragment points into the pool printed beside it.
func byProcess(schemas map[string]validation.SchemaFile, docs []sourceDoc, group []site) []manifestProcess {
	var order []string
	sites := map[string][]site{}
	file := map[string]string{}
	for _, s := range group {
		if _, seen := sites[s.Process]; !seen {
			order = append(order, s.Process)
			// Absolute: definitions in one call come from different directories, so no single
			// cwd reads them all — and a relative argument is joined to this, not to the cwd.
			abs, err := filepath.Abs(docs[s.docIdx].file)
			if err != nil {
				abs = docs[s.docIdx].file
			}
			file[s.Process] = abs
		}
		sites[s.Process] = append(sites[s.Process], s)
	}

	out := make([]manifestProcess, 0, len(order))
	for _, name := range order {
		p := manifestProcess{
			Name: name, Dir: filepath.Dir(file[name]), File: filepath.Base(file[name]),
			Sites: sites[name],
		}
		var fragments []any
		for _, s := range p.Sites {
			for _, frag := range s.Types {
				fragments = append(fragments, frag)
			}
		}
		if sf, ok := schemas[name]; ok {
			p.Defs = reachableDefs(poolOf(sf), fragments...)
		}
		out = append(out, p)
	}
	return out
}

// poolOf renders a process's $defs as the documents they are printed as, so the reachability
// walk reads refs the same way it does everywhere else.
func poolOf(sf validation.SchemaFile) map[string]any {
	pool := map[string]any{}
	for _, name := range sf.Defs.Names() {
		if def, ok := sf.Defs.Get(name); ok {
			pool[name] = schemaDoc(def.WithoutDefs())
		}
	}
	return pool
}

// siteTypes answers the resolver's request at one site: each address is resolved against the
// TYPE view of that site's process, relative to the task the directive sits in. The schemas come
// back as inference wrote them — a `$ref` into that process's own pool, which the manifest ships
// beside them.
func siteTypes(schemas map[string]validation.SchemaFile, want map[string]string, s site) (map[string]any, error) {
	if len(want) == 0 {
		return nil, nil
	}
	sf, ok := schemas[s.Process]
	if !ok {
		return nil, nil
	}
	doc, err := validation.TypeDocumentFrom(sf)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.Process, err)
	}
	out := make(map[string]any, len(want))
	for _, name := range slices.Sorted(maps.Keys(want)) {
		path, err := framed(want[name], s.Task)
		if err != nil {
			return nil, fmt.Errorf("resolver type %q: %w", name, err)
		}
		if path == nil {
			out[name] = nil // a task frame at a site that is in no task
			continue
		}
		at, err := validation.Navigate(doc, want[name], path)
		if err != nil {
			// Null, not absent, and not fatal. Null because the key was ASKED for and there is
			// nothing at it — the same reason a `raise` attaching nothing types as null rather
			// than dropping out: absent would mean "not requested", which is a different fact.
			// Not fatal because what a site can answer varies legitimately (a script taking no
			// argument has no `input.input`), and whether that matters is the resolver's to say.
			out[name] = nil
			continue
		}
		// As the document it is printed as, without its pool: a `$ref` inside it points into the
		// `$defs` beside it, and a copy per fragment would repeat most of the answer.
		out[name] = schemaDoc(at.WithoutDefs())
	}
	return out, nil
}
