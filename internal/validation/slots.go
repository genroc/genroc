package validation

// The expression environment, addressable: the contexts inference checks against, keyed by slot.
// specs/schema-command.md owns the address grammar, specs/task-scopes.md the phases.

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
	slotCase          = "case"
	slotRetry         = "retry"
	slotPanic         = "panic"
	slotRaise         = "raise"
)

// SlotContexts returns the expression context at every addressable slot, keyed by canonical
// address. Each carries its `$defs` pool, so one answer can be handed on whole.
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
			// One per CASE, since earlier negations narrow each; the whole-switch address stays —
			// three things name it. specs/guard-narrowing.md.
			for i, c := range t.Switch {
				out[caseSlot(t.ID, i)] = scopes.switchCase(t, i, ctx)
				// A `panic`/`raise` fires only when the case held, so it may assume it. One level
				// down, so enclosingSlot finds it first and the `case` keeps the proving scope.
				addClauseSlots(out, caseSlot(t.ID, i), c.Panic, c.Raise, nil,
					func() schema.Schema { return scopes.switchClause(t, i, ctx) })
			}
		}

		// One per rule, because the error axis is per rule: each catches a different set of
		// codes, so `error` is a different declared payload in each.
		for i, ec := range t.OnError {
			out[ruleSlot(t.ID, i)] = scopes.rule(t, i, ec)
			retry := &ec.Retry
			if ec.Retry.IsZero() {
				retry = nil
			}
			addClauseSlots(out, ruleSlot(t.ID, i), ec.Panic, ec.Raise, retry,
				func() schema.Schema { return scopes.ruleClause(t, i, ec) })
		}
	}

	if def.Output.Present() {
		out[SlotProcessOutput] = scopes.processOutputContext(def)
	}
	return out, nil
}

// Only clauses actually written get an address: listing slots the document lacks is noise.
func addClauseSlots(out map[string]schema.Schema, base string, panics, raise *model.Fault, retry *model.Retry, ctx func() schema.Schema) {
	var present []string
	if panics != nil {
		present = append(present, slotPanic)
	}
	if raise != nil {
		present = append(present, slotRaise)
	}
	if retry != nil {
		present = append(present, slotRetry)
	}
	if len(present) == 0 {
		return
	}
	c := ctx()
	for _, name := range present {
		out[base+"."+name] = c
	}
}

// Type slots. The contract boundaries — what a generator is handed — addressed in the same
// space as the contexts above: one slot, two questions. specs/schema-command.md §7.
const (
	slotInput    = "input"
	slotBody     = "body"
	slotQuery    = "query"
	slotChildren = "children"
	slotResult   = "result"
	slotLastErr  = "last_error"
	slotRaises   = "raises"
)

// TypeSlots returns the type of every addressable slot, keyed by address. Each carries the
// pool it resolves against, so one answer can be handed on whole.
func TypeSlots(def *model.ProcessDefinition) (map[string]schema.Schema, error) {
	sf, _ := Check(def)
	return typeSlots(sf), nil
}

// TypeDocumentFrom is TypeDocument for a caller that already has the SchemaFile — the resolver
// pass, which infers every definition once and then addresses into them.
func TypeDocumentFrom(sf SchemaFile) (schema.Schema, error) {
	return nest(typeSlots(sf))
}

func typeSlots(sf SchemaFile) map[string]schema.Schema {
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
		// `input`/`result` are the ACTION's and sit under it, `output`/`last_error` the task's.
		// The `action` segment stops a later task-level slot colliding with an action's.
		action := taskSlot(id, slotAction)
		// The payload takes the name the DEFINITION gives it (`body` on a fetch), so a pointer
		// into it and its type are one address.
		payload := slotInput
		if ts.ActionType == model.ActionTypeFetch {
			payload = slotBody
		}
		put(schema.JoinPath(action, payload), ts.Input)
		// The other two things an action SENDS, addressed where the definition writes them.
		put(schema.JoinPath(action, slotQuery), ts.Query)
		for key, child := range ts.Children {
			put(schema.JoinPath(schema.JoinPath(schema.JoinPath(action, slotChildren), key), slotInput), child)
		}
		// A routing task's result is `null` — what `self.result` reads there — and that is a
		// fact about the scope, not a contract a caller generates from.
		if ts.ActionType != "" {
			put(schema.JoinPath(action, slotResult), ts.Result)
		}
		put(taskSlot(id, slotOutput), ts.Output)
		put(taskSlot(id, slotLastErr), ts.Error)
	}
	return out
}

// SlotAt resolves the longest SLOT an address names, then walks the rest INSIDE it: one slot
// address may prefix another, which the nested document cannot hold. false means no slot
// matched — fall back to the document.
func SlotAt(slots map[string]schema.Schema, address string) (schema.Schema, bool, error) {
	slot, rest, ok, err := SlotOf(slots, address)
	if err != nil || !ok {
		return schema.Schema{}, false, err
	}
	inside, err := Navigate(slots[slot], address, rest)
	return inside, true, err
}

// SlotOf is SlotAt's split without the walk, for a caller that reads the parent and then asks
// about one member. The prefix rule lives only here.
func SlotOf(slots map[string]schema.Schema, address string) (slot string, rest []schema.Segment, ok bool, err error) {
	segs, err := schema.ParsePath(address)
	if err != nil {
		return "", nil, false, err
	}
	for n := len(segs); n > 0; n-- {
		key := slotKey(segs[:n])
		if _, found := slots[key]; found {
			return key, segs[n:], true, nil
		}
	}
	return "", nil, false, nil
}

// slotKey renders segments the way the slot constructors do: JoinPath for names, a bare dot for
// an index, so `on_error[0]`, `on_error["0"]` and `on_error.0` all reach the one rule.
func slotKey(segs []schema.Segment) string {
	out := ""
	for _, seg := range segs {
		if seg.IsIndex {
			out += "." + strconv.Itoa(seg.Index)
			continue
		}
		if isDigits(seg.Name) {
			out += "." + seg.Name
			continue
		}
		out = schema.JoinPath(out, seg.Name)
	}
	return out
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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

// Every property is required: a slot listed here exists, and an optional one would come back
// nullable and stop navigating.
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
		// An index into an OBJECT reads the key spelled with that number (`on_error[0]`, see
		// ruleSlot). Nothing is conflated: indexing an object is otherwise an error.
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

// newTaskScopes is the checker's own scope builder, fed what Check MANAGED — usually a document
// mid-edit, so a slot it could not type reads as {}. specs/schema-command.md §1.
func newTaskScopes(def *model.ProcessDefinition) (taskScopes, error) {
	// Diagnostics are DISCARDED, not swallowed: this view answers "what can be read here", and
	// a caller wanting the verdict asks Check.
	sf, _ := Check(def)
	required, optional, mustErr, mayErr, errSrc := computeContextSets(def.Tasks)
	return taskScopes{
		tasks: sf.Tasks, processInput: sf.ProcessInput,
		configSchema: buildConfigSchema(def.ConfigSchema), defs: sf.Defs,
		required: required, optional: optional,
		errs:        errContexts(def.Tasks, mustErr, mayErr, errSrc, sf.Defs),
		refinements: computeRefinements(def.Tasks),
	}, nil
}

// CheckSlotRoots refuses, with registration's message, an expression at address that reads what
// its slot cannot. Only where address names a SLOT, and never the slot's required TYPE (one
// context serves many). specs/schema-command.md §2.
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
	case phase == slotSwitch && (depth == 3 || depth == 4):
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

// Accessor syntax, so an id no identifier can spell is quoted (`tasks["step.one"].output`) and a
// printed address pastes straight back.
func taskSlot(id, phase string) string {
	return schema.JoinPath(schema.JoinPath(slotTasks, id), phase)
}

// Keyed, not indexed: one `items` cannot type each rule differently. Dotted, not `["0"]`, because
// zsh globs `[0]` before genctl sees it; both bracket forms still parse.
func ruleSlot(id string, i int) string {
	return taskSlot(id, slotOnError) + "." + strconv.Itoa(i)
}

// caseSlot keys a switch case by index, dotted for the same reason ruleSlot is.
func caseSlot(id string, i int) string {
	return taskSlot(id, slotSwitch) + "." + strconv.Itoa(i)
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
