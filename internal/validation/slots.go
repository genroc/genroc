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

func taskSlot(id, phase string) string { return "tasks." + id + "." + phase }
func ruleSlot(id string, i int) string {
	return fmt.Sprintf("tasks.%s.%s[%d]", id, slotOnError, i)
}

// CanonicalSlot maps any slot address onto the phase whose context it is evaluated in:
// `tasks.price.url` and `tasks.price.action.input.code` are both `tasks.price.action`. The
// table it implements is specs/task-scopes.md's, which is why it lives beside the contexts
// rather than in the CLI that parses the address.
func CanonicalSlot(def *model.ProcessDefinition, address string) (string, error) {
	if address == SlotProcessOutput {
		if !def.Output.Present() {
			return "", fmt.Errorf("%s: this process declares no output", address)
		}
		return SlotProcessOutput, nil
	}
	rest, ok := strings.CutPrefix(address, "tasks.")
	if !ok {
		return "", fmt.Errorf("%q is not a slot address: expected `output` or `tasks.<id>.<slot>`", address)
	}
	task, rest, ok := splitTaskID(def, rest)
	if !ok {
		return "", fmt.Errorf("%q names no task in %q", address, def.Name)
	}
	// `action.` is optional: the phase is named by what follows it either way.
	rest = strings.TrimPrefix(rest, slotAction+".")

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

// splitTaskID takes the longest task id that prefixes rest, because a task id is unconstrained
// and may itself contain a dot — matching up to the first one would name the wrong task.
func splitTaskID(def *model.ProcessDefinition, rest string) (*model.Task, string, bool) {
	var best *model.Task
	var tail string
	for _, t := range def.Tasks {
		switch {
		case rest == t.ID:
			if best == nil || len(t.ID) > len(best.ID) {
				best, tail = t, ""
			}
		case strings.HasPrefix(rest, t.ID+"."), strings.HasPrefix(rest, t.ID+"["):
			if best == nil || len(t.ID) > len(best.ID) {
				best, tail = t, strings.TrimPrefix(rest[len(t.ID):], ".")
			}
		}
	}
	return best, tail, best != nil
}

func headSegment(rest string) string {
	if i := strings.IndexAny(rest, ".["); i >= 0 {
		return rest[:i]
	}
	return rest
}

func ruleIndex(rest string, task *model.Task, address string) (int, error) {
	open := strings.Index(rest, "[")
	closing := strings.Index(rest, "]")
	if open < 0 || closing < open {
		return 0, fmt.Errorf("%s: an on_error rule is addressed by index, e.g. %s[0]", address, slotOnError)
	}
	i, err := strconv.Atoi(rest[open+1 : closing])
	if err != nil || i < 0 || i >= len(task.OnError) {
		return 0, fmt.Errorf("%s: task %q has %d on_error rule(s)", address, task.ID, len(task.OnError))
	}
	return i, nil
}
