package api

// Moving a process tree to another definition version: PLAN (db), MIGRATE each state
// (validation), WRITE together (db). Neither package knows the other, so it lives here.
// specs/version-compatibility.md s4.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"genroc/internal/db"
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

func (h *Handlers) upgradeInstance(id string, raw json.RawMessage, actor string) Reply {
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
	// A child alone would leave its parent collecting a version its definition does not name
	// (s3c). An invariant of the operation, so refused here, not in the CLI.
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
		// An unformable tree is a refusal naming the child and reason, like every other
		// refusal, not an internal error.
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
		// Also in the write's SQL predicate; checked here so the refusal names the reason. A
		// running instance can advance between plan and write; failing/pausing are draining.
		if !movableStatus(m.Instance.Status) {
			move.Reason = fmt.Sprintf("status is %s; only paused or failed instances can be moved", m.Instance.Status)
			resp.Moves = append(resp.Moves, move)
			return okReply(resp)
		}
		// Pause settles a row whose lease lapsed without clearing worker_id, and the write refuses
		// it. Never clear worker_id to admit the move: it is the ReclaimedExpired/only_once evidence.
		if m.Instance.WorkerID != nil {
			verb := "resume"
			if m.Instance.Status == model.StatusFailed {
				verb = "retry"
			}
			move.Reason = fmt.Sprintf("a lapsed lease from worker %s is still recorded on it; %s the instance "+
				"so a worker reclaims it, then pause and upgrade", *m.Instance.WorkerID, verb)
			resp.Moves = append(resp.Moves, move)
			return okReply(resp)
		}
		def, defErr := h.db.GetDefinition(m.Instance.ProcessName, m.ToVersion)
		if defErr != nil {
			move.Reason = defErr.Error()
			resp.Moves = append(resp.Moves, move)
			return okReply(resp)
		}
		state, migErr := validation.MigrateState(def, m.Instance.Task, m.Instance.State, h.db.ObjectLoader())
		if migErr != nil {
			// Reported, not returned as an error: a refusal names which member blocked the
			// tree and why, and that is the answer rather than a failure to produce one.
			move.Reason = migErr.Error()
			resp.Moves = append(resp.Moves, move)
			return okReply(resp)
		}
		// A PARKED instance also has a result in flight against the old contract, so the new
		// version must accept what the old one promised.
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
	h.auditUpgrades(ups, actor)
	resp.Upgraded = true
	return okReply(resp)
}

// auditUpgrades is best-effort: the upgrade already committed, so a lost entry costs the
// story, not the state.
func (h *Handlers) auditUpgrades(ups []db.InstanceUpgrade, actor string) {
	for _, up := range ups {
		h.db.AppendLog(&model.LogEntry{
			Actor:      actor,
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

// inFlightBreak says why a HELD instance cannot move ("" if it can): its task changed type, or the
// new version would refuse the result it waits for. Over-refuses on a child task on purpose —
// refusing leaves an operator informed; allowing wedges the parent at collect.
func (h *Handlers) inFlightBreak(inst *model.ProcessInstance, to *model.ProcessDefinition, toVersion int) string {
	// A delay holds its instance with no phase, only a wake_at.
	if inst.Phase == model.PhaseNone && inst.WakeAt == nil {
		return ""
	}
	from, err := h.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return err.Error()
	}
	oldTask, newTask := taskByID(from, inst.Task), taskByID(to, inst.Task)
	if oldTask == nil || newTask == nil || oldTask.Action == nil {
		return ""
	}
	if b, ok := validation.TypeChangeBreak(oldTask, newTask); ok {
		return fmt.Sprintf("task %q: %s", inst.Task, b.Message)
	}
	if inst.Phase == model.PhaseNone || !oldTask.Action.Type.Holds().Result {
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
