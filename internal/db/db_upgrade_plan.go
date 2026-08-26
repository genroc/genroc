package db

// Planning which versions a tree moves to. specs/version-compatibility.md s3c.

import (
	"context"
	"fmt"

	"genroc/internal/model"
)

// PlannedMove is one instance and the version it must move to.
type PlannedMove struct {
	Instance  *model.ProcessInstance
	ToVersion int
}

// PlanUpgrade works out the whole move from a root and the version the ROOT goes to. Only
// the root's target is a choice: every descendant's is DERIVED from the definition its
// parent is moving to, through the same rule the engine resolves at spawn. Letting a caller
// name a child's version is how a parent ends up running one its own definition never
// mentions -- the instance-level twin of registry drift.
//
// The unit is the non-terminal closure, so a tree moves whole or not at all. Terminal
// descendants stay put: their outputs are frozen, and a moved parent reads them as they
// stand.
func (db *DB) PlanUpgrade(ctx context.Context, rootID string, rootVersion int) ([]PlannedMove, error) {
	tree, err := db.NonTerminalSubtree(ctx, rootID)
	if err != nil {
		return nil, err
	}
	if len(tree) == 0 {
		return nil, fmt.Errorf("instance %q has no live tree to move", rootID)
	}

	target := map[string]int{rootID: rootVersion}
	byParent := map[string][]*model.ProcessInstance{}
	var root *model.ProcessInstance
	for _, inst := range tree {
		if inst.ID == rootID {
			root = inst
			continue
		}
		byParent[inst.ParentID] = append(byParent[inst.ParentID], inst)
	}
	if root == nil {
		return nil, fmt.Errorf("instance %q is not live", rootID)
	}

	// Breadth-first from the root: a child's target needs its parent's, so the parent must
	// be resolved first. A descendant whose own parent is terminal is unreachable here, and
	// that is correct -- nothing will collect it under a new version.
	out := []PlannedMove{{Instance: root, ToVersion: rootVersion}}
	queue := []*model.ProcessInstance{root}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]

		kids := byParent[parent.ID]
		if len(kids) == 0 {
			continue
		}
		// The version the parent is MOVING TO is what names the children, not the one it is
		// on now. That is the whole point: after the move the parent's definition and its
		// children have to agree.
		parentDef, err := db.GetDefinition(parent.ProcessName, target[parent.ID])
		if err != nil {
			return nil, fmt.Errorf("load %s@%d (target of %q): %w", parent.ProcessName, target[parent.ID], parent.ID, err)
		}
		for _, kid := range kids {
			v, err := db.childTargetVersion(parent, parentDef, target[parent.ID], kid)
			if err != nil {
				return nil, err
			}
			target[kid.ID] = v
			out = append(out, PlannedMove{Instance: kid, ToVersion: v})
			queue = append(queue, kid)
		}
	}
	return out, nil
}

// childTargetVersion asks the shared rule which version of this child the parent's target
// definition names, from the task that spawned it and the slot it occupies.
func (db *DB) childTargetVersion(parent *model.ProcessInstance, parentDef *model.ProcessDefinition, parentTarget int, kid *model.ProcessInstance) (int, error) {
	var task *model.Task
	for _, t := range parentDef.Tasks {
		if t != nil && t.ID == kid.SpawnTaskID {
			task = t
			break
		}
	}
	if task == nil || task.Action == nil {
		return 0, fmt.Errorf("child %q was spawned from task %q, which %s@%d does not declare as a spawning task: %w",
			kid.ID, kid.SpawnTaskID, parent.ProcessName, parentTarget, ErrUpgradeBlocked)
	}

	// The slot a child occupies: a child_map key, empty for child and child_list. It is the
	// discriminant the child carries from its own spawn, so it survives the version change.
	key, _ := kid.State["_spawn_child_key"].(string)
	declared, name := task.Action.Version, task.Action.Name
	if key != "" {
		entry, ok := task.Action.Children[key]
		if !ok {
			return 0, fmt.Errorf("child %q occupies slot %q of task %q, which the target version no longer declares: %w",
				kid.ID, key, kid.SpawnTaskID, ErrUpgradeBlocked)
		}
		declared, name = entry.Version, entry.Name
	}
	if name != kid.ProcessName {
		return 0, fmt.Errorf("child %q runs %s but the target version spawns %s from task %q; a rename is not a move: %w",
			kid.ID, kid.ProcessName, name, kid.SpawnTaskID, ErrUpgradeBlocked)
	}
	return db.ResolveChildVersion(parent.ProcessName, parentTarget, kid.SpawnTaskID, name, declared, key)
}
