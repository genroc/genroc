package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"genroc/internal/errcode"
	"genroc/internal/idgen"
	"genroc/internal/model"
	"genroc/internal/shape"
)

// runChildProcesses: WaitStateNone → spawn and park the parent on 'waiting';
// WaitStateCollecting → merge the settled batch into context and continue. A parent
// paused mid-spawn spawns paused children — a suspended tree queues nothing runnable.
func (e *Engine) runChildProcesses(ctx context.Context, inst *model.ProcessInstance, task *model.Task) (any, *advanceOutcome) {
	// Phase 2: parent woke up with the batch settled. Read the children once, then either
	// resolve a raised batch (route via on_error) or, if every child completed, merge
	// their outputs into the action result (self.result, exported only if the task
	// projects it). The one read is shared by resolution and collection.
	if inst.WaitState == model.WaitStateCollecting {
		siblings, err := e.db.ChildrenForTask(ctx, inst.ID, task.ID, inst.TaskEpoch)
		if err != nil {
			inst.WaitState = model.WaitStateNone
			return nil, stop(e.failInstance(inst, errcode.EngineCollect, fmt.Sprintf("task %q collect: %v", task.ID, err)))
		}

		// A batch with any raised child is the parent's to resolve: match on_error rules
		// against the raised codes and route accordingly. resolveRaisedBatch clears the
		// wait state and returns the terminal/route outcome itself.
		if raised := raisedInSlotOrder(siblings, task); len(raised) > 0 {
			return nil, stop(e.resolveRaisedBatch(ctx, inst, task, raised))
		}

		output, err := e.buildChildOutput(task, siblings)
		if err != nil {
			inst.WaitState = model.WaitStateNone
			// A failed conform is the caller's narrowing bet losing, so it routes through
			// on_error as output.invalid; every other failure here is corruption of the
			// batch and stays a defect. specs/error-extensions.md §X2-c.
			var invalid outputInvalid
			if errors.As(err, &invalid) {
				return nil, stop(e.handleCallError(inst, task, invalid.Error(), errcode.OutputInvalid))
			}
			return nil, stop(e.failInstance(inst, errcode.EngineCollect, fmt.Sprintf("task %q collect: %v", task.ID, err)))
		}
		inst.WaitState = model.WaitStateNone
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventChildrenCollect, Task: task.ID})
		return output, nil
	}

	// Phase 1: spawn children. The parent stores no list of them -- the child rows carry
	// parent_id, and the detail view rebuilds the placeholder from that (db.ChildrenOfInstance).
	childCallStack := append(inst.CallStack, inst.ID)

	var children []*model.ProcessInstance
	switch task.Action.Type {
	case model.ActionTypeChild:
		single, fail := e.buildSingleChild(inst, task, childCallStack)
		if fail != nil {
			return nil, fail
		}
		// Metadata mirrors the result shape: a single child records its one id as a
		// scalar (child_map records an object, child_list an array).
		children = []*model.ProcessInstance{single}
	case model.ActionTypeChildMap:
		mapped, fail := e.buildMapChildren(ctx, inst, task, childCallStack)
		if fail != nil {
			return nil, fail
		}
		ids := make(map[string]any, len(mapped))
		for _, c := range mapped {
			key, _ := c.State["_spawn_child_key"].(string)
			ids[key] = c.ID
		}
		children = mapped
	case model.ActionTypeChildList:
		listChildren, fail := e.buildListChildren(ctx, inst, task, childCallStack)
		if fail != nil {
			return nil, fail
		}
		if len(listChildren) == 0 {
			// Empty `over` array: there is nothing to spawn. Yield an empty-array
			// result and continue inline — do NOT park. SpawnChildrenAndWait is a
			// no-op on zero children, so parking here would leave the parent to
			// re-run this task forever.
			e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventChildrenSpawned, Task: task.ID, Msg: "0 children"})
			return []any{}, nil
		}
		ids := make([]any, len(listChildren))
		for i, c := range listChildren {
			ids[i] = c.ID
		}
		children = listChildren
	}

	inst.RetryCount = 0
	inst.WakeAt = nil

	// The batch travels to persist, which inserts it in the same transaction that parks
	// this parent — the children and the wait state have to land together or a crash
	// between them strands one side.
	return nil, stop(advanceOutcome{kind: outcomeSpawn, children: children})
}

// freshBatch is the batch phase 1 would spawn from the parent AS IT STANDS NOW — versions
// re-resolved against its current task, inputs re-evaluated against its context and
// re-validated — indexed by slot. Built ONCE per retry round rather than per slot, so a
// hundred-slot fan-out retrying forty of them still evaluates each input once.
//
// A replacement must not re-send what its attempt was given. A definition upgrade is how a
// caller changes what a child receives, and a `$import`ed script IS an input, so copying
// makes a fix impossible to deliver: the operator edits the code, applies, upgrades, retries,
// and the old string is handed to the replacement.
func (e *Engine) freshBatch(ctx context.Context, inst *model.ProcessInstance, task *model.Task) (map[string]*model.ProcessInstance, *advanceOutcome) {
	callStack := append(inst.CallStack, inst.ID)
	var built []*model.ProcessInstance
	switch task.Action.Type {
	case model.ActionTypeChildMap:
		mapped, fail := e.buildMapChildren(ctx, inst, task, callStack)
		if fail != nil {
			return nil, fail
		}
		built = mapped
	case model.ActionTypeChildList:
		listed, fail := e.buildListChildren(ctx, inst, task, callStack)
		if fail != nil {
			return nil, fail
		}
		built = listed
	default:
		single, fail := e.buildSingleChild(inst, task, callStack)
		if fail != nil {
			return nil, fail
		}
		built = []*model.ProcessInstance{single}
	}
	out := make(map[string]*model.ProcessInstance, len(built))
	for _, c := range built {
		out[slotID(c)] = c
	}
	return out, nil
}

// slotID names the position a child occupies in its batch, across all three shapes. A single
// child has no discriminant, which is itself the discriminant.
func slotID(c *model.ProcessInstance) string {
	if k := spawnKey(c); k != "" {
		return "key:" + k
	}
	if i, ok := spawnIndex(c); ok {
		return fmt.Sprintf("idx:%d", i)
	}
	return "single"
}

// resolveChildVersion asks the shared rule (db.ResolveChildVersion) about THIS instance's
// version. An upgrade asks the same rule about the version a parent is moving to, which is
// why the rule does not live here.
func (e *Engine) resolveChildVersion(inst *model.ProcessInstance, taskID, name string, declared int, depKey string) (int, error) {
	return e.db.ResolveChildVersion(inst.ProcessName, inst.ProcessVersion, taskID, name, declared, depKey)
}

// newChildInstance builds a running child. id is base+i so siblings sort after the parent
// in spawn order; spawnCtx carries only per-CHILD discriminants (_spawn_child_key,
// _spawn_index) — what SHAPE the batch is lives on the parent's definition, not here.
func newChildInstance(parent *model.ProcessInstance, task *model.Task, def *model.ProcessDefinition, version int, input any, callStack []string, id string, spawnCtx map[string]any) *model.ProcessInstance {
	childCtx := map[string]any{
		"input":   input,
		"outputs": map[string]any{},
		"error":   nil,
	}
	for k, v := range spawnCtx {
		childCtx[k] = v
	}
	return &model.ProcessInstance{
		ID:             id,
		ProcessName:    def.Name,
		ProcessVersion: version,
		Task:           def.Tasks[0].ID,
		// Same reason as the API's create path: without it every spawned child's first
		// claim pays an fsync. specs/durability-levels.md s4.
		NextReplayable: !def.Tasks[0].OnlyOnceAction(),
		State:          childCtx,
		Status:         model.StatusRunning,
		ParentID:       parent.ID,
		SpawnTaskID:    task.ID,
		// The batch this child belongs to. The parent's TaskEpoch does not move while it is
		// parked, so the value here is the one its collect will bind.
		ParentTaskEpoch: parent.TaskEpoch,
		CallStack:       callStack,
	}
}

// buildSingleChild constructs the one instance a "child" task spawns — no slot
// discriminant, output collected unwrapped. Persists nothing; a non-nil outcome means the
// parent failed and the caller must stop and persist it.
func (e *Engine) buildSingleChild(inst *model.ProcessInstance, task *model.Task, callStack []string) (*model.ProcessInstance, *advanceOutcome) {
	version, err := e.resolveChildVersion(inst, task.ID, task.Action.Name, task.Action.Version, "")
	if err != nil {
		return nil, stop(e.failInstance(inst, errcode.EngineDefinition, fmt.Sprintf("task %q child: %v", task.ID, err)))
	}
	def, err := e.definition(task.Action.Name, version)
	if err != nil {
		return nil, stop(e.failInstance(inst, errcode.EngineDefinition, fmt.Sprintf("task %q child: %v", task.ID, err)))
	}
	input, err := e.evalChildInput(inst, task.ID, "child", task.Action.Input)
	if err != nil {
		return nil, stop(e.failInstance(inst, errcode.EngineExpression, err.Error()))
	}
	input, err = def.ValidateInput(input)
	if err != nil {
		return nil, stop(e.failInstance(inst, errcode.EngineInput, fmt.Sprintf("task %q child input validation: %v", task.ID, err)))
	}
	spawnCtx := map[string]any{}
	base := idgen.ChildBase(inst.ID)
	return newChildInstance(inst, task, def, version, input, callStack, idgen.Add(base, 0).String(), spawnCtx), nil
}

// buildMapChildren resolves definitions, evaluates inputs, and constructs
// ProcessInstances for all keyed (child_map) children. Persists nothing; a non-nil
// outcome means the parent failed and the caller must stop and persist it.
func (e *Engine) buildMapChildren(ctx context.Context, inst *model.ProcessInstance, task *model.Task, callStack []string) ([]*model.ProcessInstance, *advanceOutcome) {
	keys := make([]string, 0, len(task.Action.Children))
	for key := range task.Action.Children {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	// One base id (guaranteed to sort after the parent); siblings are base, base+1,
	// … in sorted-key order, so the whole batch sorts after the parent and among
	// itself in spawn order.
	base := idgen.ChildBase(inst.ID)

	children := make([]*model.ProcessInstance, 0, len(task.Action.Children))
	for i, key := range keys {
		entry := task.Action.Children[key]
		version, err := e.resolveChildVersion(inst, task.ID, entry.Name, entry.Version, key)
		if err != nil {
			return nil, stop(e.failInstance(inst, errcode.EngineDefinition, fmt.Sprintf("task %q child_map[%q]: %v", task.ID, key, err)))
		}
		def, err := e.definition(entry.Name, version)
		if err != nil {
			return nil, stop(e.failInstance(inst, errcode.EngineDefinition, fmt.Sprintf("task %q child_map[%q]: %v", task.ID, key, err)))
		}
		input, err := e.evalChildInput(inst, task.ID, fmt.Sprintf("child_map[%q]", key), entry.Input)
		if err != nil {
			return nil, stop(e.failInstance(inst, errcode.EngineExpression, err.Error()))
		}
		input, err = def.ValidateInput(input)
		if err != nil {
			return nil, stop(e.failInstance(inst, errcode.EngineInput, fmt.Sprintf("task %q child_map[%q] input validation: %v", task.ID, key, err)))
		}
		spawnCtx := map[string]any{
			"_spawn_child_key": key,
		}
		children = append(children, newChildInstance(inst, task, def, version, input, callStack, idgen.Add(base, uint64(i)).String(), spawnCtx))
	}
	return children, nil
}

// buildListChildren evaluates `over` to an array, one child per element in order. Empty
// array or null yields an empty slice, no error (the caller handles the empty fan-out).
// Persists nothing; a non-nil outcome fails the parent.
func (e *Engine) buildListChildren(ctx context.Context, inst *model.ProcessInstance, task *model.Task, callStack []string) ([]*model.ProcessInstance, *advanceOutcome) {
	version, err := e.resolveChildVersion(inst, task.ID, task.Action.Name, task.Action.Version, "")
	if err != nil {
		return nil, stop(e.failInstance(inst, errcode.EngineDefinition, fmt.Sprintf("task %q child_list: %v", task.ID, err)))
	}
	def, err := e.definition(task.Action.Name, version)
	if err != nil {
		return nil, stop(e.failInstance(inst, errcode.EngineDefinition, fmt.Sprintf("task %q child_list: %v", task.ID, err)))
	}

	// Evaluate `over` to the input array. Registration guarantees a non-null array
	// type, but guard defensively: a null evaluates to the empty fan-out.
	arrVal, err := e.evalShape(inst, shape.Shape{Raw: task.Action.Over}, e.selfBeforeOutput(inst))
	if err != nil {
		return nil, stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q child_list over: %v", task.ID, err)))
	}
	if arrVal == nil {
		return nil, nil
	}
	// Same boundary as a child input: each element is conformed and stored on a child row.
	concreteArr, err := e.concrete(inst, arrVal)
	if err != nil {
		return nil, stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q child_list over: %v", task.ID, err)))
	}
	items, ok := concreteArr.([]any)
	if !ok {
		return nil, stop(e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q child_list: over did not evaluate to an array (got %T)", task.ID, concreteArr)))
	}

	// One base id (sorts after the parent); siblings are base, base+1, … in element
	// order, so the batch sorts after the parent and among itself in input order.
	base := idgen.ChildBase(inst.ID)
	children := make([]*model.ProcessInstance, 0, len(items))
	for i, elem := range items {
		input, err := def.ValidateInput(elem)
		if err != nil {
			return nil, stop(e.failInstance(inst, errcode.EngineInput, fmt.Sprintf("task %q child_list[%d] input validation: %v", task.ID, i, err)))
		}
		spawnCtx := map[string]any{
			"_spawn_index": i,
		}
		children = append(children, newChildInstance(inst, task, def, version, input, callStack, idgen.Add(base, uint64(i)).String(), spawnCtx))
	}
	return children, nil
}

func (e *Engine) evalChildInput(inst *model.ProcessInstance, taskID, label string, input *model.Shape) (any, error) {
	if !input.Present() {
		return map[string]any{}, nil
	}
	val, err := e.evalShape(inst, shape.Shape{Raw: input.Raw}, e.selfBeforeOutput(inst))
	if err != nil {
		return nil, fmt.Errorf("task %q %s input: %v", taskID, label, err)
	}
	return e.concrete(inst, val)
}

// concrete materializes the references left in an evaluated value, for the two boundaries a
// marker must not cross. specs/lazy-context.md.
//
//   - A CONFORM inspects and normalizes the value (strips undeclared keys, fills defaults), and
//     cannot do either inside an object it would have to load to see.
//   - The value lands on ANOTHER instance's row, and a claim is written only for an object this
//     write produced -- a passed-through marker would leave the child referencing content it
//     never claimed, which the sweep is entitled to delete.
//
// Cross-instance sharing does not need the marker anyway: the child re-cuts the value, the
// content is identical, and content addressing lands it on the same object with a second claim.
func (e *Engine) concrete(inst *model.ProcessInstance, v any) (any, error) {
	return e.context(inst).Materialize(v)
}
