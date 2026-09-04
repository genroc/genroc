package validation

// The expression environment, addressable. Inference builds a context for every slot it checks
// and then spends it on error messages; this is the same computation, keyed so a caller can ask
// for one. specs/schema-command.md owns the address grammar, specs/task-scopes.md the phases.

import (
	"fmt"
	"strconv"
	"strings"

	"genroc/internal/model"
	"genroc/internal/schema"
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
	sf, err := Generate(def)
	if err != nil {
		return nil, err
	}
	configSchema := buildConfigSchema(def.ConfigSchema)
	required, optional, mustErr, mayErr, errSrc := computeContextSets(def.Tasks)
	errs := errContexts(def.Tasks, mustErr, mayErr, errSrc, sf.Defs)
	// The checker's own builder, off the finished SchemaFile: these are not contexts LIKE the
	// ones it used, they are built by the same constructors it calls.
	scopes := taskScopes{
		tasks: sf.Tasks, processInput: sf.ProcessInput, configSchema: configSchema, defs: sf.Defs,
		required: required, optional: optional, errs: errs,
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

// An address is a path in the expression language's own accessor syntax, so a task id that no
// identifier can spell is quoted — `tasks["step.one"].output` — and the address a listing
// prints can be pasted straight back. schema.JoinPath decides which form a name needs.
func taskSlot(id, phase string) string {
	return schema.JoinPath(schema.JoinPath(slotTasks, id), phase)
}

func ruleSlot(id string, i int) string {
	return schema.JoinIndex(taskSlot(id, slotOnError), i)
}

// CanonicalSlot maps any slot address onto the phase whose context it is evaluated in:
// `tasks.price.url` and `tasks.price.action.input.code` are both `tasks.price.action`. The
// table it implements is specs/task-scopes.md's, which is why it lives beside the contexts
// rather than in the CLI that parses the address.
func CanonicalSlot(def *model.ProcessDefinition, address string) (string, error) {
	segs, err := schema.ParsePath(address)
	if err != nil {
		return "", err
	}
	if len(segs) == 1 && segs[0].Name == SlotProcessOutput {
		if !def.Output.Present() {
			return "", fmt.Errorf("%s: this process declares no output", address)
		}
		return SlotProcessOutput, nil
	}
	if segs[0].Name != slotTasks || len(segs) < 2 {
		return "", fmt.Errorf("%q is not a slot address: expected `output` or `tasks.<id>.<slot>`", address)
	}
	task := findTask(def, segs[1])
	if task == nil {
		return "", unknownTask(def, address, segs[1])
	}
	rest := segs[2:]
	// `action.` is optional: the phase is named by what follows it either way. A lone `action`
	// stays — it is the phase's own canonical address, and stripping it would name no slot.
	if len(rest) > 1 && rest[0].Name == slotAction {
		rest = rest[1:]
	}

	switch head := headSegment(rest); head {
	case "":
		return "", fmt.Errorf("%q names a task, not a slot in it: add %s, %s, %s or %s[<n>]",
			address, slotAction, slotOutput, slotSwitch, slotOnError)
	case slotOnError:
		i, err := ruleIndex(rest, task, address)
		if err != nil {
			return "", err
		}
		return ruleSlot(task.ID, i), nil
	case slotSwitch:
		if len(task.Switch) == 0 {
			return "", fmt.Errorf("%s: task %q has no switch", address, task.ID)
		}
		return taskSlot(task.ID, slotSwitch), nil
	case slotOutput:
		if !task.Output.Present() {
			return "", fmt.Errorf("%s: task %q projects no output", address, task.ID)
		}
		return taskSlot(task.ID, slotOutput), nil
	default:
		// Everything else a task holds is evaluated before its output exists: the action's
		// slots, `timeout`, and the per-entry inputs of a batch.
		return taskSlot(task.ID, slotAction), nil
	}
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

// unknownTask names the quoted form when the id looks split rather than absent: a dot is the
// one character an id may hold that the grammar reads as a step, so `tasks.step.one` names the
// task "step" and never "step.one".
func unknownTask(def *model.ProcessDefinition, address string, seg schema.Segment) error {
	for _, t := range def.Tasks {
		if strings.HasPrefix(t.ID, seg.Name+".") {
			return fmt.Errorf("%q names no task in %q: an id holding a dot is quoted, tasks[%s]",
				address, def.Name, strconv.Quote(t.ID))
		}
	}
	return fmt.Errorf("%q names no task in %q", address, def.Name)
}

func headSegment(rest []schema.Segment) string {
	if len(rest) == 0 {
		return ""
	}
	return rest[0].Name
}

func ruleIndex(rest []schema.Segment, task *model.Task, address string) (int, error) {
	if len(rest) < 2 || !rest[1].IsIndex {
		return 0, fmt.Errorf("%s: an on_error rule is addressed by index, e.g. %s[0]", address, slotOnError)
	}
	if i := rest[1].Index; i >= 0 && i < len(task.OnError) {
		return i, nil
	}
	return 0, fmt.Errorf("%s: task %q has %d on_error rule(s)", address, task.ID, len(task.OnError))
}
