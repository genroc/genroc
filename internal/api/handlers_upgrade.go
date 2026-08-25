package api

// Moving a process tree to another version of its definition. This handler owns the
// composition: PLAN which versions the tree moves to (db), MIGRATE each state to the
// definition it is moving to (validation), then WRITE them together (db). Neither of those
// packages knows about the other, which is why the operation lives here.
// specs/version-compatibility.md s4.

import (
	"context"
	"encoding/json"
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
	DryRun   bool          `json:"dry_run,omitempty"`
	Moves    []UpgradeMove `json:"moves"`
}

type UpgradeMove struct {
	ID          string `json:"id"`
	Process     string `json:"process"`
	Task        string `json:"task"`
	FromVersion int    `json:"from_version"`
	ToVersion   int    `json:"to_version"`
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
	// Asserted rather than read: a tree that moved since the caller looked at it is refused,
	// not migrated against a plan made for a version it has left.
	if req.FromVersion != 0 && root.ProcessVersion != req.FromVersion {
		return conflict("instance %q is on version %d, not %d", id, root.ProcessVersion, req.FromVersion).reply()
	}

	plan, err := h.db.PlanUpgrade(ctx, id, req.ToVersion)
	if err != nil {
		return errReply(err)
	}

	resp := UpgradeResp{DryRun: req.DryRun, Moves: make([]UpgradeMove, 0, len(plan))}
	ups := make([]db.InstanceUpgrade, 0, len(plan))
	for _, m := range plan {
		move := UpgradeMove{
			ID: m.Instance.ID, Process: m.Instance.ProcessName, Task: m.Instance.Task,
			FromVersion: m.Instance.ProcessVersion, ToVersion: m.ToVersion,
		}
		if m.ToVersion == m.Instance.ProcessVersion {
			move.Skipped = true
			resp.Moves = append(resp.Moves, move)
			continue
		}
		// Checked here and not only by the write's predicate, so a dry run is honest: the
		// write would refuse this anyway, but a plan that reported "fine" and then failed for
		// real is worse than no plan at all. paused and failed are the settled states -- a
		// running instance can be claimed and advanced between this plan and the write, and
		// failing/pausing are draining, with descendants still moving.
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
		state, migErr := validation.MigrateState(def, m.Instance.Task, m.Instance.ContextData)
		if migErr != nil {
			// Reported, not returned as an error: a refusal names which member blocked the
			// tree and why, and that is the answer rather than a failure to produce one.
			move.Reason = migErr.Error()
			resp.Moves = append(resp.Moves, move)
			return okReply(resp)
		}
		resp.Moves = append(resp.Moves, move)
		ups = append(ups, db.InstanceUpgrade{Instance: m.Instance, ToVersion: m.ToVersion, NewContext: state})
	}

	if req.DryRun {
		resp.Upgraded = false
		return okReply(resp)
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
