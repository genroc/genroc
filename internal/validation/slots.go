package validation

// The expression environment, addressable. Inference builds a context for every slot it checks
// and then spends it on error messages; this is the same computation, keyed so a caller can ask
// for one. specs/schema-command.md owns the address grammar, specs/task-scopes.md the phases.

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/shape"
)

// Slot addresses. A task has one context per phase, and every finer slot resolves into one of
// them — `tasks.<id>.url` and `tasks.<id>.timeout` are both the action phase.
const (
	SlotProcessOutput = "output"
	slotTasks         = "tasks"
	slotAction        = "action"
	slotOutput        = "output"
	slotSwitch        = "switch"
	slotOnError       = "on_error"
)

// SlotContexts returns the expression context at every addressable slot, keyed by canonical
// address. The `$defs` pool every context resolves against travels with each of them, so one
// answer can be handed on whole.
func SlotContexts(def *model.ProcessDefinition) (map[string]schema.Schema, error) {
	scopes, err := newTaskScopes(def)
	if err != nil {
		return nil, err
	}

	out := make(map[string]schema.Schema)
	for _, t := range def.Tasks {
		out[taskSlot(t.ID, slotAction)] = scopes.action(t)

		if t.Output.Present() {
			ctx, _, err := scopes.outputMap(t)
			if err != nil {
				return nil, fmt.Errorf("task %q: %w", t.ID, err)
			}
			out[taskSlot(t.ID, slotOutput)] = ctx
		}

		if len(t.Switch) > 0 {
			ctx, err := scopes.switchScope(t)
			if err != nil {
				return nil, fmt.Errorf("task %q: %w", t.ID, err)
			}
			out[taskSlot(t.ID, slotSwitch)] = ctx
		}

		// One per rule, because the error axis is per rule: each catches a different set of
		// codes, so `error` is a different declared payload in each.
		for i, ec := range t.OnError {
			out[ruleSlot(t.ID, i)] = scopes.rule(t, ec)
		}
	}

	if def.Output.Present() {
		out[SlotProcessOutput] = scopes.processOutputContext(def)
	}
	return out, nil
}

// Type slots. The contract boundaries — what a generator is handed — addressed in the same
// space as the contexts above: one slot, two questions. specs/schema-command.md §7.
const (
	slotInput   = "input"
	slotResult  = "result"
	slotLastErr = "last_error"
	slotRaises  = "raises"
)

// TypeSlots returns the type of every addressable slot, keyed by address. Each carries the
// pool it resolves against, so one answer can be handed on whole.
func TypeSlots(def *model.ProcessDefinition) (map[string]schema.Schema, error) {
	sf, err := Generate(def)
	if err != nil {
		return nil, err
	}
	out := map[string]schema.Schema{}
	put := func(address string, s schema.Schema) {
		if !s.IsZero() {
			out[address] = s.WithDefs(sf.Defs)
		}
	}
	put(slotInput, sf.ProcessInput)
	put(SlotProcessOutput, sf.ProcessOutput)
	for code, raised := range sf.Raises {
		put(schema.JoinPath(slotRaises, code), raised)
	}
	for id, ts := range sf.Tasks {
		put(taskSlot(id, slotInput), ts.Input)
		put(taskSlot(id, slotResult), ts.Result)
		put(taskSlot(id, slotOutput), ts.Output)
		put(taskSlot(id, slotLastErr), ts.Error)
	}
	return out, nil
}

// ContextDocument and TypeDocument are the two views as ONE schema each: the addresses are
// paths into them, so an address is navigation and nothing else. specs/schema-command.md §2.
func ContextDocument(def *model.ProcessDefinition) (schema.Schema, error) {
	slots, err := SlotContexts(def)
	if err != nil {
		return schema.Schema{}, err
	}
	return nest(slots)
}

func TypeDocument(def *model.ProcessDefinition) (schema.Schema, error) {
	slots, err := TypeSlots(def)
	if err != nil {
		return schema.Schema{}, err
	}
	return nest(slots)
}

// nest folds addressed slots into the object they are addresses INTO. Every property is
// required: a slot listed here exists, and an optional one would come back nullable and stop
// navigating.
func nest(slots map[string]schema.Schema) (schema.Schema, error) {
	root := schema.Object()
	var defs schema.Defs
	for _, address := range slices.Sorted(maps.Keys(slots)) {
		segs, err := schema.ParsePath(address)
		if err != nil {
			return schema.Schema{}, err
		}
		if defs.IsZero() {
			// Every slot came from one Generate, so they share one pool; the ROOT must carry it
			// or a $ref inside a leaf has nothing to resolve against when navigated.
			defs = slots[address].DefsHandle()
		}
		root = put(root, segs, slots[address].WithoutDefs())
	}
	return root.WithDefs(defs), nil
}

// put writes leaf at path, creating the objects on the way. A segment is a property name: an
// index would need `items`, which types every element alike.
func put(node schema.Schema, path []schema.Segment, leaf schema.Schema) schema.Schema {
	name := path[0].Name
	if len(path) == 1 {
		return node.WithProperty(name, leaf, true)
	}
	child, ok := node.Properties()[name]
	if !ok {
		child = schema.Object()
	}
	return node.WithProperty(name, put(child, path[1:], leaf), true)
}

// Navigate walks path into s. On a miss it says what IS there: the document is the address
// space, so the keys at the point of failure are the list of what could be typed instead.
func Navigate(s schema.Schema, address string, path []schema.Segment) (schema.Schema, error) {
	walked := ""
	for i, seg := range path {
		step := path[i : i+1]
		// An index into an OBJECT reads the key spelled with that number: `on_error[0]` is the
		// first rule, which is keyed rather than indexed because one `items` cannot type each
		// rule differently. Nothing is conflated — indexing an object is otherwise an error.
		if seg.IsIndex {
			if _, ok := rootProperties(s)[strconv.Itoa(seg.Index)]; ok {
				step = []schema.Segment{{Name: strconv.Itoa(seg.Index)}}
			}
		}
		next, err := s.At(renderSegments(step))
		if err != nil {
			where := "this process"
			if walked != "" {
				where = walked
			}
			// The address as TYPED: re-rendering it would quote a key the author spelled bare,
			// and an error that echoes something else reads as a second mistake.
			return schema.Schema{}, fmt.Errorf("%s: no %q in %s%s%s",
				address, name(seg), where, holds(s), quotedHint(s, seg))
		}
		walked = renderSegments(path[:i+1])
		s = next
	}
	return s, nil
}

// name is what the author wrote for one step, index or key alike.
func name(seg schema.Segment) string {
	if seg.IsIndex {
		return strconv.Itoa(seg.Index)
	}
	return seg.Name
}

// quotedHint catches the one key a path grammar splits by accident: a dot inside a name. The
// listing above already shows it, but not that it has to be quoted to be read as one segment.
func quotedHint(s schema.Schema, seg schema.Segment) string {
	for _, key := range slices.Sorted(maps.Keys(rootProperties(s))) {
		if strings.HasPrefix(key, seg.Name+".") {
			return fmt.Sprintf(" (a key holding a dot is quoted: [%s])", strconv.Quote(key))
		}
	}
	return ""
}

// holds names what an object carries, so a miss teaches the address space rather than only
// reporting one. Empty for a leaf, which has nothing to offer instead.
func holds(s schema.Schema) string {
	names := slices.Sorted(maps.Keys(rootProperties(s)))
	if len(names) == 0 {
		return ""
	}
	return ", which holds: " + strings.Join(names, ", ")
}

// rootProperties reads through a union: a context with ARMS (the process output has one per
// ending) carries its roots inside them, so the arms are where the answer is.
func rootProperties(s schema.Schema) map[string]schema.Schema {
	if props := s.Properties(); len(props) > 0 {
		return props
	}
	out := map[string]schema.Schema{}
	for _, arm := range s.Variants() {
		maps.Copy(out, arm.Properties())
	}
	return out
}

// renderSegments is ParsePath's inverse over the segments it returns, so a slot address
// rebuilt from a parse is spelled the way a listing prints it.
func renderSegments(segs []schema.Segment) string {
	out := ""
	for _, sg := range segs {
		if sg.IsIndex {
			out = schema.JoinIndex(out, sg.Index)
			continue
		}
		out = schema.JoinPath(out, sg.Name)
	}
	return out
}

// newTaskScopes rebuilds the checker's own scope builder off a finished SchemaFile. These are
// not contexts LIKE the ones it used: they come from the same constructors it calls.
func newTaskScopes(def *model.ProcessDefinition) (taskScopes, error) {
	sf, err := Generate(def)
	if err != nil {
		return taskScopes{}, err
	}
	required, optional, mustErr, mayErr, errSrc := computeContextSets(def.Tasks)
	return taskScopes{
		tasks: sf.Tasks, processInput: sf.ProcessInput,
		configSchema: buildConfigSchema(def.ConfigSchema), defs: sf.Defs,
		required: required, optional: optional,
		errs: errContexts(def.Tasks, mustErr, mayErr, errSrc, sf.Defs),
	}, nil
}

// CheckSlotRoots reports whether an expression written at address may READ what it names, with
// the message registration would give: `self.result` before the action answers, a previous
// output no path returns to, a result the action never types. Inference alone answers those with
// "field not found", which names the member and not the rule.
//
// The availability half only, and only where the address names a SLOT: past that it has walked
// inside one, where no expression is being written and there is nothing to check. A slot's
// required TYPE is never checked — that is per slot, and one context serves many
// (specs/schema-command.md §2).
func CheckSlotRoots(def *model.ProcessDefinition, address, expr string) error {
	segs, err := schema.ParsePath(address)
	if err != nil {
		return err
	}
	if len(segs) < 3 || segs[0].Name != slotTasks {
		return nil
	}
	task := findTask(def, segs[1])
	if task == nil {
		return nil
	}
	var sc selfScope
	switch phase, depth := segs[2].Name, len(segs); {
	case phase == slotAction && depth == 3:
		sc = beforeOutput
	case phase == slotOutput && depth == 3:
		sc = afterAction
	case phase == slotSwitch && depth == 3:
		sc = afterOutput
	case phase == slotOnError && depth == 4:
		sc = beforeOutput
	default:
		return nil
	}
	refs, err := (&shape.Shape{Raw: expr, Expr: true}).Roots()
	if err != nil {
		return nil // a parse failure is inference's to report, with its own message
	}
	scopes, err := newTaskScopes(def)
	if err != nil {
		return err
	}
	_, typedResult, err := scopes.outputMap(task)
	if err != nil {
		return err
	}
	return slotRoots(task, address, scopes.loops(task), typedResult, sc)(refs)
}

// An address is a path in the expression language's own accessor syntax, so a task id that no
// identifier can spell is quoted — `tasks["step.one"].output` — and the address a listing
// prints can be pasted straight back. schema.JoinPath decides which form a name needs.
func taskSlot(id, phase string) string {
	return schema.JoinPath(schema.JoinPath(slotTasks, id), phase)
}

// ruleSlot keys a rule by its index rather than indexing an array: `items` is one schema for
// every element, so an array could not carry a different context per rule.
//
// Dotted, not JoinPath's `["0"]`: a bare segment is always a property name, so `.0` round-trips,
// and an address is not an expression — `tasks.my task.output` is not one either. The reason to
// prefer it is the shell: `[0]` is a glob, and zsh refuses the whole command with "no matches
// found" before genctl sees it. Both bracket forms still parse.
func ruleSlot(id string, i int) string {
	return taskSlot(id, slotOnError) + "." + strconv.Itoa(i)
}

func findTask(def *model.ProcessDefinition, seg schema.Segment) *model.Task {
	if seg.IsIndex {
		return nil
	}
	for _, t := range def.Tasks {
		if t.ID == seg.Name {
			return t
		}
	}
	return nil
}
