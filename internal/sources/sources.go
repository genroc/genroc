package sources

// Source resolution: a `$<resolver>: <argument>` leaf is replaced by what a registered resolver
// produces. specs/source-resolution.md has the phase rule and the manifest contract.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"genroc/internal/defdoc"
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

// The two phases, named by PERMISSION: structural may change what the typechecker sees and runs
// before validation; typed may not, and runs after it. specs/source-resolution.md §The two phases.
const (
	phaseStructural = "structural"
	phaseTyped      = "typed"
)

// The manifest's `mode`: whether genctl uses the answer. A resolver's phase is its entry's, so the
// manifest does not repeat it.
const (
	modeResolve  = "resolve"
	modeGenerate = "generate"
)

// builtinProcess spreads another definition's name/result_schema/raises into a child task.
const builtinProcess = "process"

// builtins are appended after everything a .genroc registers, so a local entry of the same name
// wins by first-match. No Command: genctl answers them itself.
func builtins() []resolverConfig {
	return []resolverConfig{{
		Name:  builtinProcess,
		Phase: phaseStructural,
		Ext:   []string{".genroc.yaml", ".genroc.yml", ".genroc.json"},
	}}
}

// The `json` tags feed defschema.Config, the editor's `.genroc` schema; yaml.v3 ignores them.
// Both must spell a key the same -- TestConfigTagsAgree.
type resolverConfig struct {
	Name string `yaml:"name" json:"name" description:"What a directive names: \"$<name>: <argument>\"."`
	// Phase is "typed" or "structural" -- what the resolver MAY do, never what it contains.
	// specs/source-resolution.md §The two phases.
	Phase string `yaml:"phase" json:"phase" enum:"typed,structural" description:"What the resolver may do. \"typed\" fills a slot with text and runs after inference, with types in hand; \"structural\" fills a slot or spreads a mapping with any value and runs before it, so it is handed no types."`
	// Ext is a list of accepted SUFFIXES, not extensions: `.genroc.yaml` has to be
	// expressible and filepath.Ext answers `.yaml` for it. Empty accepts anything.
	Ext []string `yaml:"ext" json:"ext,omitempty" description:"Suffixes the argument's first word may end with (\".ts\", \".genroc.yaml\"). Whole suffixes, not extensions; empty accepts anything, no argument included."`
	// Command is absent exactly for a built-in, which runs inside genctl. A file entry
	// without one is refused when the config is read.
	Command []string `yaml:"command" json:"command" description:"The resolver binary and its arguments, run from this file's directory with the manifest on stdin."`
	// Types is what this resolver wants typed: name → frame-prefixed `genctl schema type`
	// address. Absent means none. specs/source-resolution.md §The project config.
	Types map[string]string `yaml:"types" json:"types,omitempty" description:"Declarations the resolver wants generated, as name to address: a genctl schema type address, relative to the task the directive sits in, e.g. task.action.input.input."`
}

type projectConfig struct {
	Root string `yaml:"-" json:"-"`
	// Definitions is what a bare `genctl apply|validate|types` reads: files, directories or
	// globs, resolved against the config's own directory.
	Definitions []string `yaml:"definitions" json:"definitions,omitempty" description:"What genctl apply, types and schema read when given no -f: files or globs (** matches any depth), resolved against this file."`
	// Resolvers is ORDERED and taken first-match on (name, suffix) -- which is what makes
	// overriding a built-in need no rule of its own, since builtins() is appended last.
	Resolvers []resolverConfig `yaml:"resolvers" json:"resolvers,omitempty" description:"Source resolvers, tried in order and taken first-match on name and suffix; the built-in \"process\" is appended last, so listing one under that name overrides it."`
}

// matchResolver returns the first entry accepting this name and first word. nameKnown separates the
// two failures a caller words differently: an unknown name, or no entry accepting the suffix.
func (c projectConfig) matchResolver(name string, args []string) (idx int, nameKnown, ok bool) {
	for i, r := range c.Resolvers {
		if r.Name != name {
			continue
		}
		nameKnown = true
		if len(r.Ext) == 0 {
			return i, true, true
		}
		if len(args) == 0 {
			continue
		}
		lower := strings.ToLower(args[0])
		for _, ext := range r.Ext {
			if strings.HasSuffix(lower, strings.ToLower(ext)) {
				return i, true, true
			}
		}
	}
	return -1, nameKnown, false
}

// acceptedBy renders every suffix the entries carrying this name accept, for the error that
// says the name is known and the argument is not one of its files.
func (c projectConfig) acceptedBy(name string) string {
	var out []string
	for _, r := range c.Resolvers {
		if r.Name == name {
			out = append(out, r.Ext...)
		}
	}
	if len(out) == 0 {
		return "any"
	}
	return strings.Join(slices.Compact(slices.Sorted(slices.Values(out))), ", ")
}

// defaultDefinitionPaths is the nearest .genroc's `definitions`, made absolute. Empty when there
// are none, which the caller reports as "-f is required" rather than as a project error.
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
	Value any
	File  string
	// Index turns a diagnostic the server reports by slot address into a line in this file.
	// Nil for a .json source. specs/language-server.md §3.
	Index *defdoc.Doc
}

// site is one directive occurrence. The exported fields are the manifest's; loc is how
// splice finds the slot again, and is why nothing re-walks the document to apply the result.
// The `description` tags are the resolver protocol's reference page -- genrocspec TestEveryFieldIsDescribed.
type site struct {
	// Off the wire: the manifest goes only to that resolver and nests sites under their
	// process. The pass still needs both, to group sites and pick their types.
	Resolver string `json:"-"`
	Process  string `json:"-"`
	// resolverIdx is the ENTRY that matched, not just its name: one name may carry several
	// entries with different suffixes and different commands, and each is its own batch.
	resolverIdx int    `json:"-"`
	Level       string `json:"level" enum:"process,task,action" description:"Which namespace the directive sits in, so a resolver need not parse pointer to know where it landed."`
	Task        string `json:"task,omitempty" description:"The id of the task the directive is in. Absent at level process."`
	// Set only AT action level: a switch case is not in the action, so naming its type would
	// describe the wrong thing.
	Action string `json:"action,omitempty" description:"The action's type. Only at level action."`
	Child  string `json:"child,omitempty" description:"The process the action spawns. Only at level action, on an action that names one."`
	// Keys and indices, not an RFC 6901 string: no `~0`/`~1` to unescape, and key "0" stays
	// distinct from index 0.
	Pointer []any `json:"pointer" description:"Where the directive is: keys and array indices from the definition's root, a task named by its id rather than its index. Where the slot has a type, this is its genctl schema type address."`
	// genctl reads only the first word, for `ext`, and never as a path.
	Args  []string       `json:"args" description:"The argument's words (blanks separate them, '...' quotes one literally). Not paths: a resolver may take a URL, a package name or nothing, which is []."`
	Types map[string]any `json:"types,omitempty" description:"What this resolver's types entry asked for, by the name it gave each: a JSON Schema whose $ref points into the process's $defs, or null where nothing is at that address. Absent for a structural resolver."`

	loc    []any
	docIdx int
	// ord is the site's position in the slice a batch was built from, so a reply read in the
	// manifest's order finds its way back: byProcess regroups sites under their process.
	ord int
}

// manifest is what a resolver reads on stdin. specs/source-resolution.md §The manifest.
type manifest struct {
	Mode string `json:"mode" enum:"resolve,generate" description:"Whether genctl uses the reply. \"generate\" (genctl generate, typed resolvers only) reads none, so a resolver may skip the work behind it and only write its files; answering anyway is still correct."`
	// Sites nest under their process, so nothing has to be joined by name.
	Processes []manifestProcess `json:"processes" description:"One entry per definition with a site for this resolver, in the order the files were read. The reply answers every site, in this order."`
}

// manifestProcess is one definition's sites, with `$defs` narrowed to what their fragments reach;
// a `$ref` survives because a task output may reference itself.
type manifestProcess struct {
	Name string `json:"name" description:"The definition's name."`
	// Split from File because a relative argument is relative to the DIRECTORY: joining is the
	// resolver's to do, and it needs the base.
	Dir   string         `json:"dir" description:"The definition's directory, absolute: what a relative argument joins to. One call can span several directories, so the resolver's working directory (the .genroc directory) is not it."`
	File  string         `json:"file" description:"The definition's file name, within dir."`
	Sites []site         `json:"sites" description:"The directives in this definition that name this resolver."`
	Defs  map[string]any `json:"$defs,omitempty" description:"The definitions the sites' types reach, and no others. Absent when they reach none, and for a structural resolver."`
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

// resolverReply is either phase's answer: one value per site, parallel to the manifest. Raw so a
// structural splice keeps a number exact (specs/number-precision.md).
type resolverReply struct {
	Values []json.RawMessage `json:"values" description:"One value per site, in the manifest's order: processes as listed, then sites within each. A typed resolver answers strings; a structural one answers any JSON value, and a mapping where the directive is a spread."`
}

// ── project config ─────────────────────────────────────────────────────────────

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
			for i, r := range cfg.Resolvers {
				if r.Name == "" {
					return projectConfig{}, fmt.Errorf("%s: resolver %d has no name", path, i)
				}
				if r.Phase != phaseTyped && r.Phase != phaseStructural {
					return projectConfig{}, fmt.Errorf("%s: resolver %q has phase %q - it is %q or %q",
						path, r.Name, r.Phase, phaseStructural, phaseTyped)
				}
				if len(r.Command) == 0 {
					return projectConfig{}, fmt.Errorf("%s: resolver %q has no command", path, r.Name)
				}
				// Runs before inference, so there is nothing to hand it: a `types` here would be
				// answered with null at every site and read as a resolver that does not work.
				if r.Phase == phaseStructural && len(r.Types) > 0 {
					return projectConfig{}, fmt.Errorf("%s: resolver %q is structural and runs before "+
						"inference, so it cannot be handed types", path, r.Name)
				}
				for typeName, address := range r.Types {
					if _, err := framed(address, "x"); err != nil {
						return projectConfig{}, fmt.Errorf("%s: resolver %q, type %q: %w",
							path, r.Name, typeName, err)
					}
				}
			}
			cfg.Resolvers = append(cfg.Resolvers, builtins()...)
			return cfg, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			// No config is not an error, and the built-ins still apply: `$process` needs no
			// registration, so a project with nothing to declare declares nothing.
			return projectConfig{Resolvers: builtins()}, nil
		}
		abs = parent
	}
}

// ── finding sites ──────────────────────────────────────────────────────────────

// findSites walks every document for directive leaves. An argument passes on as its words: genctl
// neither resolves nor stats them.
func findSites(docs []sourceDoc, cfg projectConfig) ([]site, error) {
	var out []site
	for i, sd := range docs {
		// Not one chained assertion: an editor sends a sequence or scalar root too.
		root, _ := sd.Value.(map[string]any)
		name, _ := root["name"].(string)
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
				resolver, argument, isDirective := defdoc.Directive(v)
				if !isDirective {
					return nil
				}
				at := renderPointer(slotPointer(sd.Value, loc))
				args, err := defdoc.ArgValues(argument)
				if err != nil {
					return fmt.Errorf("%s: %s: %q: %w", sd.File, at, v, err)
				}
				// `ext` asserts a suffix on the FIRST word, so a `.py` handed to the TypeScript
				// toolchain fails here with a sentence rather than inside `tsc`.
				idx, nameKnown, ok := cfg.matchResolver(resolver, args)
				if !nameKnown {
					return fmt.Errorf("%s: %s: no resolver named %q is registered in %s (write $$%s: to keep it as text)",
						sd.File, at, resolver, projectConfigName, resolver)
				}
				if !ok && len(args) == 0 {
					return fmt.Errorf("%s: %s: resolver %q takes a %s file, and the directive names none",
						sd.File, at, resolver, cfg.acceptedBy(resolver))
				}
				if !ok {
					return fmt.Errorf("%s: %s: resolver %q accepts %s files, but %q is not one",
						sd.File, at, resolver, cfg.acceptedBy(resolver), args[0])
				}
				s := site{
					Resolver:    resolver,
					Process:     name,
					Pointer:     slotPointer(sd.Value, loc),
					Args:        args,
					loc:         append([]any(nil), loc...),
					docIdx:      i,
					resolverIdx: idx,
				}
				s.Task = enclosingTaskID(sd.Value, loc)
				s.Level = levelOf(loc)
				if task := enclosingTask(sd.Value, loc); task != nil && s.Level == levelAction {
					action, _ := task["action"].(map[string]any)
					s.Action, _ = action["type"].(string)
					s.Child, _ = action["name"].(string)
				}
				out = append(out, s)
			}
			return nil
		}
		if err := walk(sd.Value, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

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

// The three namespaces a directive can sit in. `task` and `action` are separate because the
// `action` segment keeps their slot names apart, and a site in one is not in the other.
const (
	levelProcess = "process"
	levelTask    = "task"
	levelAction  = "action"
)

func levelOf(loc []any) string {
	if len(loc) < 2 || loc[0] != "tasks" {
		return levelProcess
	}
	if len(loc) > 2 && loc[2] == "action" {
		return levelAction
	}
	return levelTask
}

// slotPointer addresses the task by ID rather than index, then the document's own keys. The
// action's kind and a child's process are fields beside it, not path steps.
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
	node := docs[s.docIdx].Value
	for i, seg := range s.loc {
		last := i == len(s.loc)-1
		switch k := seg.(type) {
		case string:
			m, ok := node.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: cannot descend into %s", docs[s.docIdx].File, s.Pointer)
			}
			if last {
				m[k] = value
				return nil
			}
			node = m[k]
		case int:
			a, ok := node.([]any)
			if !ok || k >= len(a) {
				return fmt.Errorf("%s: cannot descend into %s", docs[s.docIdx].File, s.Pointer)
			}
			if last {
				a[k] = value
				return nil
			}
			node = a[k]
		}
	}
	return fmt.Errorf("%s: empty pointer", docs[s.docIdx].File)
}

// escapeDollars doubles every `$`: the template layer collapses `$$` unconditionally, so this
// round-trips ANY bytes, where escaping only `${` would corrupt a literal `$$`.
func escapeDollars(s string) string { return strings.ReplaceAll(s, "$", "$$") }

// ── running a resolver ─────────────────────────────────────────────────────────

func execResolver(cfg projectConfig, rc resolverConfig, m manifest) ([]byte, error) {
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
	return stdout.Bytes(), nil
}

// runResolver returns one value per site, in the manifest's order. In generate mode the answer is
// not read at all: the resolver ran for the files it writes.
func runResolver(cfg projectConfig, rc resolverConfig, m manifest) ([]json.RawMessage, error) {
	stdout, err := execResolver(cfg, rc, m)
	if err != nil || m.Mode == modeGenerate {
		return nil, err
	}
	var reply resolverReply
	if err := json.Unmarshal(stdout, &reply); err != nil {
		return nil, fmt.Errorf("resolver %q: stdout is not the expected {\"values\": [...]}: %w",
			strings.Join(rc.Command, " "), err)
	}
	if want := len(m.flatten()); len(reply.Values) != want {
		return nil, fmt.Errorf("resolver %q returned %d values for %d sites",
			strings.Join(rc.Command, " "), len(reply.Values), want)
	}
	return reply.Values, nil
}

// runTypedResolver holds a typed resolver to strings: a string cannot change what inference saw.
func runTypedResolver(cfg projectConfig, rc resolverConfig, m manifest) ([]string, error) {
	values, err := runResolver(cfg, rc, m)
	if err != nil || values == nil {
		return nil, err
	}
	out := make([]string, len(values))
	for i, raw := range values {
		if err := json.Unmarshal(raw, &out[i]); err != nil {
			return nil, fmt.Errorf("resolver %q: value %d is not a string, and a typed resolver may "+
				"return only strings: %s", strings.Join(rc.Command, " "), i, raw)
		}
	}
	return out, nil
}

// runStructuralResolver decodes each value exactly: a structural answer may be any JSON.
func runStructuralResolver(cfg projectConfig, rc resolverConfig, m manifest) ([]any, error) {
	values, err := runResolver(cfg, rc, m)
	if err != nil {
		return nil, err
	}
	out := make([]any, len(values))
	for i, raw := range values {
		if err := numeric.Decode(raw, &out[i]); err != nil {
			return nil, fmt.Errorf("resolver %q: value %d: %w", strings.Join(rc.Command, " "), i, err)
		}
	}
	return out, nil
}

// ── the pass ───────────────────────────────────────────────────────────────────

// resolveDocs resolves every directive in docs, mutating them in place. In generate mode the typed
// sites keep their placeholder: the resolvers ran for the files they write. It returns the typed
// sites each resolver was shown, so empty means no typed resolver ran.
func resolveDocs(docs []sourceDoc, mode string) ([]ResolverSites, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	cfg, err := findProjectConfig(filepath.Dir(docs[0].File))
	if err != nil {
		return nil, err
	}
	// Phase 1 first, and its result is what phase 2 is typed against: a structural resolver
	// may change what the typechecker sees, which is the whole difference between the phases.
	if _, err := resolveStructuralPass(docs, cfg, nil); err != nil {
		return nil, err
	}

	// Re-walked rather than filtered from one pass: a spread adds keys to a mapping, so a
	// location found before it ran can name a different slot after.
	all, err := findSites(docs, cfg)
	if err != nil {
		return nil, err
	}
	var sites []site
	for _, s := range all {
		if cfg.Resolvers[s.resolverIdx].Phase == phaseTyped {
			sites = append(sites, s)
		}
	}
	if len(sites) == 0 {
		unescapeDocs(docs)
		return nil, nil
	}

	// The placeholder pass. A typed string is opaque to inference, so an empty string types
	// identically to the real one and the schemas below are the schemas of what is applied.
	for _, s := range sites {
		if err := splice(docs, s, ""); err != nil {
			return nil, err
		}
	}
	schemas, err := inferSchemas(docs, sites)
	if err != nil {
		return nil, err
	}

	byResolver := map[int][]site{}
	var order []int
	var counts []ResolverSites
	for _, s := range sites {
		if _, seen := byResolver[s.resolverIdx]; !seen {
			order = append(order, s.resolverIdx)
		}
		byResolver[s.resolverIdx] = append(byResolver[s.resolverIdx], s)
		counts = countSite(counts, cfg.Resolvers[s.resolverIdx].Name)
	}

	for _, idx := range order {
		group := byResolver[idx]
		for i := range group {
			types, err := siteTypes(schemas, cfg.Resolvers[idx].Types, group[i])
			if err != nil {
				return nil, err
			}
			group[i].Types = types
		}
		processes, err := byProcess(schemas, docs, group)
		if err != nil {
			return nil, err
		}
		m := manifest{Mode: mode, Processes: processes}
		values, err := runTypedResolver(cfg, cfg.Resolvers[idx], m)
		if err != nil {
			return nil, err
		}
		if mode == modeGenerate {
			continue
		}
		// By the manifest's own order, not the group's: nesting sites under their process may
		// interleave two files differently, and `values` answers what the resolver was shown.
		for i, s := range m.flatten() {
			if err := splice(docs, s, escapeDollars(values[i])); err != nil {
				return nil, err
			}
		}
	}
	// Last, once no walk will look for a directive again. A spliced string is untouched: it was
	// escaped on the way in and the template layer undoes that at run time.
	unescapeDocs(docs)
	return counts, nil
}

// countSite merges by name: two entries may share one, differing only in `ext`.
func countSite(counts []ResolverSites, name string) []ResolverSites {
	for i := range counts {
		if counts[i].Resolver == name {
			counts[i].Sites++
			return counts
		}
	}
	return append(counts, ResolverSites{Resolver: name, Sites: 1})
}

// inferSchemas types the definitions that carry a directive. genctl computes types and the server
// decides validity, so no strict decode or Validate here. specs/source-resolution.md.
func inferSchemas(docs []sourceDoc, sites []site) (map[string]validation.SchemaFile, error) {
	needed := make(map[string]bool, len(sites))
	for _, s := range sites {
		needed[s.Process] = true
	}
	out := make(map[string]validation.SchemaFile, len(needed))
	for _, sd := range docs {
		// A definition with no directive is never typed: one broken file must not stop a
		// project-wide `types`. Not one chained assertion, as in findSites.
		root, _ := sd.Value.(map[string]any)
		name, _ := root["name"].(string)
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
			return nil, fmt.Errorf("%s: %s: %w", sd.File, name, err)
		}
		out[name] = sf
	}
	return out, nil
}

// decodeDefinition reads one source document as a definition. Non-strict and without
// Validate: genctl computes the types, the server decides validity (§inferSchemas).
func decodeDefinition(sd sourceDoc) (*model.ProcessDefinition, error) {
	raw, err := json.Marshal(sd.Value)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", sd.File, err)
	}
	var def model.ProcessDefinition
	if err := numeric.Decode(raw, &def); err != nil {
		return nil, fmt.Errorf("%s: %w", sd.File, err)
	}
	return &def, nil
}

// An address in `types` names its frame, since both a task and the process carry an `input`.
// specs/source-resolution.md §The project config.
const (
	frameTask    = "task"
	frameProcess = "process"
)

// framed turns a `types` address into a type-document path. A `task.` address at a site in no
// task returns nil, nil: the answer there is null.
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

// byProcess nests the group's sites under their definition in file order, each with only the
// `$defs` its own fragments reach: a fragment's `$ref` points into the pool printed beside it.
func byProcess(schemas map[string]validation.SchemaFile, docs []sourceDoc, group []site) ([]manifestProcess, error) {
	var order []string
	sites := map[string][]site{}
	file := map[string]string{}
	for _, s := range group {
		if _, seen := sites[s.Process]; !seen {
			order = append(order, s.Process)
			// Absolute: definitions in one call come from different directories, so no single
			// cwd reads them all — and a relative argument is joined to this, not to the cwd.
			abs, err := filepath.Abs(docs[s.docIdx].File)
			if err != nil {
				abs = docs[s.docIdx].File
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
			pool, err := poolOf(sf)
			if err != nil {
				return nil, err
			}
			collapseAliases(pool, fragments...)
			if p.Defs, err = reachableDefs(pool, fragments...); err != nil {
				return nil, err
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// poolOf renders a process's $defs as the documents they are printed as, so the reachability
// walk reads refs the same way it does everywhere else.
func poolOf(sf validation.SchemaFile) (map[string]any, error) {
	pool := map[string]any{}
	for _, name := range sf.Defs.Names() {
		if def, ok := sf.Defs.Get(name); ok {
			doc, err := schemaDoc(def.WithoutDefs())
			if err != nil {
				return nil, err
			}
			pool[name] = doc
		}
	}
	return pool, nil
}

// siteTypes resolves each requested address against the TYPE view of the site's process. Schemas
// come back as inference wrote them: `$ref`s into the pool the manifest ships beside them.
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
			// Null, not absent (it was asked for), and not fatal: whether a missing answer
			// matters is the resolver's to say. specs/source-resolution.md §The project config.
			out[name] = nil
			continue
		}
		// As the document it is printed as, without its pool: a `$ref` inside it points into the
		// `$defs` beside it, and a copy per fragment would repeat most of the answer.
		doc, err := schemaDoc(at.WithoutDefs())
		if err != nil {
			return nil, err
		}
		out[name] = doc
	}
	return out, nil
}
