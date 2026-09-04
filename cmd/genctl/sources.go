package main

// Source resolution: a definition source file is not a definition. A `$<resolver>: <path>`
// leaf is replaced by a string a registered binary produces. See
// specs/source-resolution.md for the phase rule and the manifest contract.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"genroc/internal/model"
	"genroc/internal/numeric"
	"genroc/internal/validation"

	"gopkg.in/yaml.v3"
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
	Resolver string `json:"resolver"`
	Process  string `json:"process"`
	Task     string `json:"task,omitempty"`
	Pointer  string `json:"pointer"`
	Path     string `json:"path"`
	Input    any    `json:"input,omitempty"`
	Output   any    `json:"output,omitempty"`

	loc    []any
	docIdx int
}

type manifest struct {
	Mode    string                           `json:"mode"`
	Root    string                           `json:"root"`
	Schemas map[string]validation.SchemaFile `json:"schemas"`
	Sites   []site                           `json:"sites"`
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

// findSites walks every document for directive leaves. Paths resolve against the file the
// directive appeared in and are returned absolute, so a resolver needs no cwd convention.
func findSites(docs []sourceDoc, cfg projectConfig) ([]site, error) {
	var out []site
	for i, sd := range docs {
		name, _ := sd.doc.(map[string]any)["name"].(string)
		dir := filepath.Dir(sd.file)
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
				resolver, rel := m[1], m[2]
				rc, ok := cfg.Resolvers[resolver]
				if !ok {
					return fmt.Errorf("%s: %s: no resolver named %q is registered in %s",
						sd.file, pointerOf(loc), resolver, projectConfigName)
				}
				if rc.Ext != "" && !strings.EqualFold(filepath.Ext(rel), rc.Ext) {
					return fmt.Errorf("%s: %s: resolver %q accepts %s files, but %q is not one",
						sd.file, pointerOf(loc), resolver, rc.Ext, rel)
				}
				// Absolute, always: the resolver's cwd is the project root, not the
				// directory the -f path was relative to, so a relative path here would
				// resolve against the wrong place on the far side of the manifest.
				abs, err := filepath.Abs(filepath.Join(dir, rel))
				if err != nil {
					return fmt.Errorf("%s: %s: %w", sd.file, pointerOf(loc), err)
				}
				if _, err := os.Stat(abs); err != nil {
					return fmt.Errorf("%s: %s: %w", sd.file, pointerOf(loc), err)
				}
				s := site{
					Resolver: resolver,
					Process:  name,
					Pointer:  pointerOf(loc),
					Path:     abs,
					loc:      append([]any(nil), loc...),
					docIdx:   i,
				}
				s.Task = enclosingTaskID(sd.doc, loc)
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
	if len(loc) < 2 {
		return ""
	}
	key, ok := loc[0].(string)
	if !ok || key != "tasks" {
		return ""
	}
	idx, ok := loc[1].(int)
	if !ok {
		return ""
	}
	tasks, ok := doc.(map[string]any)["tasks"].([]any)
	if !ok || idx >= len(tasks) {
		return ""
	}
	task, ok := tasks[idx].(map[string]any)
	if !ok {
		return ""
	}
	id, _ := task["id"].(string)
	return id
}

func pointerOf(loc []any) string {
	var b strings.Builder
	for _, seg := range loc {
		b.WriteByte('/')
		switch v := seg.(type) {
		case string:
			r := strings.NewReplacer("~", "~0", "/", "~1")
			b.WriteString(r.Replace(v))
		case int:
			b.WriteString(strconv.Itoa(v))
		}
	}
	if b.Len() == 0 {
		return "/"
	}
	return b.String()
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
		return nil, nil
	}
	var reply resolverReply
	if err := json.Unmarshal(stdout.Bytes(), &reply); err != nil {
		return nil, fmt.Errorf("resolver %q: stdout is not the expected {\"code\": [...]}: %w",
			strings.Join(rc.Command, " "), err)
	}
	if len(reply.Code) != len(m.Sites) {
		return nil, fmt.Errorf("resolver %q returned %d strings for %d sites",
			strings.Join(rc.Command, " "), len(reply.Code), len(m.Sites))
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
			group[i].Input = taskInput(schemas, group[i].Process, group[i].Task)
			group[i].Output = taskResult(schemas, group[i].Process, group[i].Task)
		}
		code, err := runResolver(cfg, cfg.Resolvers[name], manifest{
			Mode: mode, Root: cfg.Root, Schemas: schemas, Sites: group,
		})
		if err != nil {
			return 0, err
		}
		if mode == "types" {
			continue
		}
		for i, s := range group {
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

// taskResult is what the task hands back, which the resolver types the script's RETURN against.
// Declared, never inferred from the script — that is the direction $infer runs.
func taskResult(schemas map[string]validation.SchemaFile, process, task string) any {
	if task == "" {
		return nil
	}
	ts, ok := schemas[process].Tasks[task]
	if !ok || ts.Result.IsZero() {
		return nil
	}
	return ts.Result
}

func taskInput(schemas map[string]validation.SchemaFile, process, task string) any {
	if task == "" {
		return nil
	}
	ts, ok := schemas[process].Tasks[task]
	if !ok || ts.Input.IsZero() {
		return nil
	}
	return ts.Input
}
