package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"genroc/internal/db"
	"genroc/internal/idgen"
	"genroc/internal/model"
	"genroc/internal/numeric"
)

func (h *Handlers) startInstance(raw json.RawMessage) Reply {
	req, err := decodeBody[StartInstanceReq](raw)
	if err != nil {
		return errReply(err)
	}
	if req.Process == "" {
		return invalid("process name is required").reply()
	}

	version := 0
	switch {
	case req.Version != nil:
		version = *req.Version
	case req.Channel != nil:
		v, err := h.db.GetChannel(req.Process, *req.Channel)
		if err != nil {
			return errReply(err)
		}
		version = v
	default:
		v, err := h.resolveDefaultVersion(req.Process)
		if err != nil {
			return errReply(err)
		}
		version = v
	}

	def, err := h.db.GetDefinition(req.Process, version)
	if err != nil {
		return errReply(err)
	}

	var input any
	if req.Input != nil {
		input = *req.Input
	}

	input, err = def.ValidateInput(input)
	if err != nil {
		// The "input validation: " prefix is load-bearing — genctl keys on it to
		// print a dedicated message (cmd/genctl/commands.go, inputValidationError).
		return invalid("input validation: %w", err).reply()
	}

	// Resolve config up front so a missing required var or bad value rejects the
	// start request rather than producing an instance that fails on first tick.
	if _, err := def.ResolveConfig(os.LookupEnv); err != nil {
		// A required config var missing from the server environment is the operator's
		// problem, not the caller's: the same request succeeds once it is set.
		return errReply(fmt.Errorf("config: %w", err))
	}

	inst := &model.ProcessInstance{
		ID:             idgen.New(),
		ProcessName:    def.Name,
		ProcessVersion: version,
		Task:           def.Tasks[0].ID,
		// Set here or the first claim of every new instance pays an fsync: the flag's
		// zero value is the safe one, not the common one. specs/durability-levels.md s4.
		NextReplayable: !def.Tasks[0].OnlyOnceAction(),
		State:          map[string]any{"input": input, "outputs": map[string]any{}, "error": nil},
		Status:         model.StatusRunning,
		// Cosmetic (SaveInstance re-stamps from the DB clock); same clock, so it
		// cannot drift from the row under a shifted test clock.
		CreatedAt: db.Now(),
	}

	if err := h.db.SaveInstance(inst); err != nil {
		return errReply(fmt.Errorf("save instance: %w", err))
	}
	if h.engine != nil {
		h.engine.AuditCreated(inst) // bookend: instance_created with the process input
		h.engine.NotifyWork()       // start advancing now instead of waiting for the next poll tick
	}

	return okReply(StartInstanceResp{
		ID:      inst.ID,
		Process: inst.ProcessName,
		Version: inst.ProcessVersion,
		Status:  inst.Status,
	})
}

func (h *Handlers) listInstances(raw json.RawMessage) Reply {
	req, err := decodeOptionalBody[ListInstancesReq](raw)
	if err != nil {
		return errReply(err)
	}
	// Roots only unless children were asked for: the flag is an opt-IN, so the default
	// listing is one row per tree. specs/id-list-commands.md.
	instances, info, err := h.db.ListInstances(req.Status, req.ErrorCode, req.Process, req.Version, !req.Children,
		db.Window{After: req.CreatedAfter, Before: req.CreatedBefore},
		db.Window{After: req.UpdatedAfter, Before: req.UpdatedBefore},
		req.page())
	if err != nil {
		return errReply(err)
	}
	resp := make([]InstanceSummaryResp, len(instances))
	for i, inst := range instances {
		resp[i] = instanceSummaryToResp(inst)
	}
	return okReply(PageResp[InstanceSummaryResp]{Items: resp, Page: info})
}

// maxInlineResolveBytes bounds what ?resolve=true will splice into one response. Per OBJECT, not
// per response: an object under it is materialized, one over it stays listed for the caller to
// fetch. That answers the objection resolve=true was removed for -- an unbounded response behind
// one query parameter -- while degrading rather than failing, so the answer is always usable.
const maxInlineResolveBytes = 1 << 20

func (h *Handlers) getInstance(id string) Reply {
	if id == "" {
		return invalid("id is required").reply()
	}
	inst, err := h.db.GetInstance(id)
	if err != nil {
		return errReply(err)
	}
	return okReply(instanceToResp(inst))
}

// reportedPayload is error_data as the wire carries it: externalized pieces lifted out into the
// listing, rooted at the field they belong to on THIS response. Without this the raw marker
// ships inline, naming a state slot the response does not have.
func reportedPayload(inst *model.ProcessInstance) (any, []ObjectEntry) {
	raw, ok := inst.State[model.StateErrorData]
	if !ok {
		return nil, nil
	}
	var objects []ObjectEntry
	return extractObjects(raw, []any{"error_data"}, &objects), objects
}

// getInstanceDetail returns the row as stored. Unlike getInstance it hides nothing: the state
// it returns is the object an upgrade validates and a migration rewrites, so an operator
// diagnosing a refusal is looking at what was actually checked.
func (h *Handlers) getInstanceDetail(id string, resolve bool) Reply {
	if id == "" {
		return invalid("id is required").reply()
	}
	inst, err := h.db.GetInstance(id)
	if err != nil {
		return errReply(err)
	}
	// No redaction here. `secret: true` keeps a value out of the server's stdout, where an
	// operator reads it without asking; an API response is someone asking. specs/object-store.md
	// §Redaction.
	//
	// Rooted at "state", the field these paths point into on THIS response. Externalized slots
	// leave it entirely and are listed instead, at the path they belong to.
	var objects []ObjectEntry
	state, _ := extractObjects(inst.State, []any{"state"}, &objects).(map[string]any)
	if state == nil {
		state = map[string]any{}
	}
	if resolve {
		kept := objects[:0]
		for _, e := range objects {
			if e.Size > maxInlineResolveBytes {
				kept = append(kept, e) // too big to splice; the caller fetches this one
				continue
			}
			content, _, err := h.db.GetObjectContent(e.Ref)
			if err != nil {
				kept = append(kept, e)
				continue
			}
			var value any
			if err := numeric.Decode([]byte(content), &value); err != nil {
				kept = append(kept, e)
				continue
			}
			// The path is rooted at the response, so drop the leading "state" to place it in the
			// map this handler still holds separately.
			if !model.Place(state, e.Path[1:], value) {
				kept = append(kept, e)
			}
		}
		objects = kept
	}
	kids, err := h.db.ChildrenOfInstance(id)
	if err != nil {
		return errReply(err)
	}
	// What shape a batch was spawned in is the PARENT definition's to say. Reading it from a
	// discriminant on the child would be reading a copy, and a copy is what an upgrade leaves
	// stale when the parent's task changes shape.
	spawnShape := map[string]model.ActionType{}
	if def, err := h.db.GetDefinition(inst.ProcessName, inst.ProcessVersion); err == nil {
		for _, t := range def.Tasks {
			if t != nil && t.Action != nil {
				spawnShape[t.ID] = t.Action.Type
			}
		}
	}
	return okReply(InstanceDetailResp{
		Children:    spawnPlaceholder(kids, spawnShape),
		ID:          inst.ID,
		Process:     inst.ProcessName,
		Version:     inst.ProcessVersion,
		ParentID:    inst.ParentID,
		SpawnTaskID: inst.SpawnTaskID,
		CallStack:   inst.CallStack,

		Status:     inst.Status,
		WaitState:  inst.WaitState,
		Task:       inst.Task,
		RetryCount: inst.RetryCount,
		WakeAt:     formatTimePtr(inst.WakeAt),
		CreatedAt:  inst.CreatedAt.Format(time.RFC3339),
		UpdatedAt:  inst.UpdatedAt.Format(time.RFC3339),

		ErrorCode:    inst.ErrorCode,
		ErrorMessage: inst.ErrorMessage,
		// From the EXTRACTED state, so an externalized payload is the same marker-free value the
		// status endpoint shows rather than a reference the caller cannot place.
		ErrorData: state[model.StateErrorData],
		State:     state,

		WorkerID:        derefString(inst.WorkerID),
		LeaseExpiresAt:  formatTimePtr(inst.LeaseExpiresAt),
		LeaseEpoch:      inst.LeaseEpoch,
		TaskEpoch:       inst.TaskEpoch,
		ParentTaskEpoch: inst.ParentTaskEpoch,
		NextReplayable:  inst.NextReplayable,

		ExternalWorkerID:       derefString(inst.ExternalWorkerID),
		ExternalLeaseExpiresAt: formatTimePtr(inst.ExternalLeaseExpiresAt),
		ExternalClaimEpoch:     inst.ExternalClaimEpoch,

		Objects: objects,
	})
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

func (h *Handlers) pauseInstance(id string) Reply {
	if id == "" {
		return invalid("id is required").reply()
	}
	res, err := h.db.PauseProcess(context.Background(), id)
	if err != nil {
		return errReply(err)
	}
	return outcomeReply(res)
}

func (h *Handlers) resumeInstance(id string) Reply {
	if id == "" {
		return invalid("id is required").reply()
	}
	res, err := h.db.ResumeProcess(context.Background(), id)
	if err != nil {
		return errReply(err)
	}
	return outcomeReply(res)
}

func (h *Handlers) retryInstance(id string, raw json.RawMessage) Reply {
	if id == "" {
		return invalid("id is required").reply()
	}
	req, err := decodeOptionalBody[RetryInstanceReq](raw)
	if err != nil {
		return errReply(err)
	}
	res, err := h.db.RetryProcess(context.Background(), id, req.Force)
	if err != nil {
		return errReply(err)
	}
	return outcomeReply(res)
}

func (h *Handlers) tick(raw json.RawMessage) Reply {
	// Both of these are configuration facts, not request faults: the endpoint is
	// routed but this server cannot serve it, which is what "unsupported" (501) says.
	if h.engine == nil {
		return unsupported("engine not available").reply()
	}
	if !h.engine.ManualTick() {
		return unsupported("tick is only available in manual mode; start the server with --poll 0").reply()
	}
	req, err := decodeOptionalBody[TickReq](raw)
	if err != nil {
		return errReply(err)
	}
	if req.AdvanceMs < 0 {
		return invalid("advance_ms must not be negative").reply()
	}
	if req.AdvanceMs > 0 {
		db.AdvanceClock(time.Duration(req.AdvanceMs) * time.Millisecond)
	}
	n, err := h.engine.Tick(context.Background())
	if err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"count": n})
}

func instanceToResp(inst *model.ProcessInstance) InstanceStatusResp {
	payload, objects := reportedPayload(inst)
	return InstanceStatusResp{
		ID:           inst.ID,
		Process:      inst.ProcessName,
		Version:      inst.ProcessVersion,
		Status:       inst.Status,
		WaitState:    inst.WaitState,
		Task:         inst.Task,
		RetryCount:   inst.RetryCount,
		ErrorCode:    inst.ErrorCode,
		ErrorMessage: inst.ErrorMessage,
		ErrorData:    payload,
		CreatedAt:    inst.CreatedAt.Format(time.RFC3339),
		UpdatedAt:    inst.UpdatedAt.Format(time.RFC3339),
		Objects:      objects,
	}
}

func instanceSummaryToResp(s *model.InstanceSummary) InstanceSummaryResp {
	return InstanceSummaryResp{
		ID:           s.ID,
		ParentID:     s.ParentID,
		Process:      s.ProcessName,
		Version:      s.ProcessVersion,
		Status:       s.Status,
		WaitState:    s.WaitState,
		Task:         s.Task,
		RetryCount:   s.RetryCount,
		ErrorCode:    s.ErrorCode,
		ErrorMessage: s.Error,
		CreatedAt:    s.CreatedAt.Format(time.RFC3339),
		UpdatedAt:    s.UpdatedAt.Format(time.RFC3339),
	}
}

// spawnPlaceholder shapes the child rows the way the spawning action does: a bare id for a
// single child, an object keyed by entry for a child_map, an array in spawn order for a
// child_list. One task, one shape -- so a reader branches on the action type it already knows
// from the definition, never on what the value happens to look like.
//
// RETIRED ATTEMPTS ARE SKIPPED. A retried slot has more than one row (s5.5/s12) and only the
// live one occupies it; including them makes a child_list longer than its fan-out and hands a
// single `child` the attempt it replaced, because the retired row is the older of the two.
// The attempts themselves stay discoverable through the instance listing, which returns every
// row carrying this parent_id.
func spawnPlaceholder(kids []db.ChildSpawn, shape map[string]model.ActionType) map[string]any {
	if len(kids) == 0 {
		return nil
	}
	byTask := map[string][]db.ChildSpawn{}
	order := []string{}
	for _, k := range kids {
		if k.Superseded {
			continue
		}
		if _, seen := byTask[k.TaskID]; !seen {
			order = append(order, k.TaskID)
		}
		byTask[k.TaskID] = append(byTask[k.TaskID], k)
	}
	out := make(map[string]any, len(order))
	for _, task := range order {
		group := byTask[task]
		switch shape[task] {
		case model.ActionTypeChildMap:
			keyed := make(map[string]any, len(group))
			for _, k := range group {
				keyed[k.Key] = k.ID
			}
			out[task] = keyed
		case model.ActionTypeChildList:
			sort.Slice(group, func(i, j int) bool { return group[i].Index < group[j].Index })
			ids := make([]any, len(group))
			for i, k := range group {
				ids[i] = k.ID
			}
			out[task] = ids
		default:
			out[task] = group[0].ID
		}
	}
	return out
}
