package api

// Moving a process tree to another version of its definition. This handler owns the
// composition: PLAN which versions the tree moves to (db), MIGRATE each state to the
// definition it is moving to (validation), then WRITE them together (db). Neither of those
// packages knows about the other, which is why the operation lives here.
// specs/version-compatibility.md s4.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"genroc/internal/db"
	"genroc/internal/idgen"
	"genroc/internal/model"
	"genroc/internal/validation"
)

// UpgradeResp reports what moved, or what stopped it. Every member is named: "the tree
// cannot move" is not actionable when a caller has to find out which one blocked it.
type UpgradeResp struct {
	Upgraded bool          `json:"upgraded"`
	Moves    []UpgradeMove `json:"moves"`
}

type UpgradeMove struct {
	ID          string `json:"id"`
	Process     string `json:"process"`
	Task        string `json:"task"`
	FromVersion int    `json:"from_version"`
	ToVersion   int    `json:"to_version"`
	Status      string `json:"status"`
	Skipped     bool   `json:"skipped,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

func (h *Handlers) upgradeInstance(id string, raw json.RawMessage) Reply {
	if id == "" {
		return invalid("id is required").reply()
	}
	req, err := decodeOptionalBody[UpgradeInstanceReq](raw)
	if err != nil {
		return errReply(err)
	}
	if req.ToVersion <= 0 {
		return invalid("to_version is required").reply()
	}
	ctx := context.Background()

	root, err := h.db.GetInstance(id)
	if err != nil {
		return errReply(err)
	}
	// The unit of upgrade is a tree, and a child is not one. Moving it alone would leave its
	// parent collecting a version its own definition does not name -- the drift s3c exists to
	// prevent -- and the parent is not in this plan to be moved with it. Refused here rather
	// than in the CLI because it is an invariant of the operation, not a convenience.
	if root.ParentID != "" {
		return conflict("instance %q has a parent (%s); upgrade its root instead, which moves the whole tree",
			id, root.ParentID).reply()
	}
	// Asserted rather than read: a tree that moved since the caller looked at it is refused,
	// not migrated against a plan made for a version it has left.
	if req.FromVersion != 0 && root.ProcessVersion != req.FromVersion {
		return conflict("instance %q is on version %d, not %d", id, root.ProcessVersion, req.FromVersion).reply()
	}

	plan, err := h.db.PlanUpgrade(ctx, id, req.ToVersion)
	if err != nil {
		// A tree the plan cannot form is a refusal, not a failure to answer: it names the child
		// and the reason, which is what the caller wants, and reads the same as every other
		// refusal rather than as an internal error.
		if errors.Is(err, db.ErrUpgradeBlocked) {
			return okReply(UpgradeResp{Moves: []UpgradeMove{{
				ID: root.ID, Process: root.ProcessName, Task: root.Task,
				FromVersion: root.ProcessVersion, ToVersion: req.ToVersion,
				Status: string(root.Status), Reason: err.Error(),
			}}})
		}
		return errReply(err)
	}

	resp := UpgradeResp{Moves: make([]UpgradeMove, 0, len(plan))}
	ups := make([]db.InstanceUpgrade, 0, len(plan))
	for _, m := range plan {
		move := UpgradeMove{
			ID: m.Instance.ID, Process: m.Instance.ProcessName, Task: m.Instance.Task,
			FromVersion: m.Instance.ProcessVersion, ToVersion: m.ToVersion,
			Status: string(m.Instance.Status),
		}
		if m.ToVersion == m.Instance.ProcessVersion {
			move.Skipped = true
			resp.Moves = append(resp.Moves, move)
			continue
		}
		// Checked here as well as in the write's SQL predicate, so the refusal names the
		// instance and the reason rather than surfacing as a row count that did not match.
		// paused and failed are the settled states: a running instance can be claimed and
		// advanced between this plan and the write, and failing/pausing are draining.
		if !movableStatus(m.Instance.Status) {
			move.Reason = fmt.Sprintf("status is %s; only paused or failed instances can be moved", m.Instance.Status)
			resp.Moves = append(resp.Moves, move)
			return okReply(resp)
		}
		def, defErr := h.db.GetDefinition(m.Instance.ProcessName, m.ToVersion)
		if defErr != nil {
			move.Reason = defErr.Error()
			resp.Moves = append(resp.Moves, move)
			return okReply(resp)
		}
		state, migErr := validation.MigrateState(def, m.Instance.Task, m.Instance.State)
		if migErr != nil {
			// Reported, not returned as an error: a refusal names which member blocked the
			// tree and why, and that is the answer rather than a failure to produce one.
			move.Reason = migErr.Error()
			resp.Moves = append(resp.Moves, move)
			return okReply(resp)
		}
		// The state is only half of it. An instance PARKED mid-task also has a result on its
		// way back: a worker was handed the old version's contract and will answer against it,
		// and no layer describes a value that is not on the row yet. So the new version has to
		// accept what the old one promised, or the work already in flight lands on a schema
		// that refuses it.
		if reason := h.inFlightBreak(m.Instance, def, m.ToVersion); reason != "" {
			move.Reason = reason
			resp.Moves = append(resp.Moves, move)
			return okReply(resp)
		}
		resp.Moves = append(resp.Moves, move)
		ups = append(ups, db.InstanceUpgrade{Instance: m.Instance, ToVersion: m.ToVersion, NewContext: state})
	}

	if err := h.db.UpgradeInstances(ctx, ups); err != nil {
		return errReply(err)
	}
	h.auditUpgrades(ups)
	resp.Upgraded = true
	return okReply(resp)
}

// auditUpgrades records each move on its own instance's trail. Best-effort, like every
// other audit write: the upgrade already committed, and losing the entry costs the story,
// not the state.
func (h *Handlers) auditUpgrades(ups []db.InstanceUpgrade) {
	for _, up := range ups {
		h.db.AppendLog(&model.LogEntry{
			ID:         idgen.New(),
			InstanceID: up.Instance.ID,
			Level:      model.LogInfo,
			Event:      model.EventInstanceUpgraded,
			TaskID:     up.Instance.Task,
			Message: fmt.Sprintf("%s@%d -> %s@%d",
				up.Instance.ProcessName, up.Instance.ProcessVersion, up.Instance.ProcessName, up.ToVersion),
			Meta: map[string]any{"from_version": up.Instance.ProcessVersion, "to_version": up.ToVersion},
		})
	}
}

// movableStatus is the operational precondition, not a schema question: whether the row is
// settled enough to be rewritten. specs/version-compatibility.md s2.
func movableStatus(s model.Status) bool {
	return s == model.StatusPaused || s == model.StatusFailed
}

// inFlightBreak reports why an instance parked on a task that holds an outstanding RESULT
// cannot move, or "" when it can. Scoped to a parked instance on purpose: a task that merely
// COULD park is registration's concern, while this asks whether this row has work outstanding
// right now.
//
// It compares SCHEMAS, and deliberately OVER-REFUSES on a child task. Schema-versus-schema is
// forced only for `external`, where the result is with a worker and there is no data to look
// at. A child batch is different: a running child moves with its parent and registration
// already guarantees the version it moves TO fits, and a child that has completed has its
// output sitting on its row, where conforming the actual value would answer precisely. Both are
// judged by the coarse relation instead, so a move that would in fact have been safe can be
// refused.
//
// Kept for now because the failure it prevents is worse than the one it causes: refusing leaves
// a tree paused and an operator informed, while allowing it wedges the parent at collect with a
// result nothing can accept. Refining it means conforming the DATA where data exists, which
// needs materialisation for an externalized output -- so it belongs here, where h.db is, and
// not in validation.
func (h *Handlers) inFlightBreak(inst *model.ProcessInstance, to *model.ProcessDefinition, toVersion int) string {
	if inst.WaitState == model.WaitStateNone {
		return ""
	}
	from, err := h.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return err.Error()
	}
	oldTask, newTask := taskByID(from, inst.Task), taskByID(to, inst.Task)
	if oldTask == nil || newTask == nil || oldTask.Action == nil || !oldTask.Action.Type.Holds().Result {
		return ""
	}
	breaks := validation.InFlightResultBreaks(oldTask, newTask)
	if len(breaks) == 0 {
		return ""
	}
	b := breaks[0]
	where := b.Address
	if b.Path != "" {
		where += "." + b.Path
	}
	return fmt.Sprintf("task %q is waiting on a result promised by v%d, which v%d would refuse: %s (%s)",
		inst.Task, inst.ProcessVersion, toVersion, b.Message, where)
}

func taskByID(def *model.ProcessDefinition, id string) *model.Task {
	for _, t := range def.Tasks {
		if t != nil && t.ID == id {
			return t
		}
	}
	return nil
}
