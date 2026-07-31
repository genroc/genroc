package api

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"genroc/internal/idgen"
	"genroc/internal/model"
	"genroc/internal/schema"
)

func (h *Handlers) listExternalTasks(raw json.RawMessage) Reply {
	req, err := decodeOptionalBody[ListExternalTasksReq](raw)
	if err != nil {
		return errReply(err)
	}
	instances, info, err := h.db.ListExternalTasks(req.Process, req.Version, req.Task, req.page())
	if err != nil {
		return errReply(err)
	}
	resp := make([]ExternalTaskResp, 0, len(instances))
	for _, inst := range instances {
		task, err := h.db.CurrentTask(inst)
		if err != nil || task == nil {
			// Not a resolvable external task (no current task), which a concurrent
			// transition could momentarily produce — skip it.
			continue
		}
		resp = append(resp, externalTaskToResp(inst, task))
	}
	return okReply(PageResp[ExternalTaskResp]{Items: resp, Page: info})
}

func externalTaskToResp(inst *model.ProcessInstance, task *model.Task) ExternalTaskResp {
	ext, _ := inst.ContextData[model.CtxExternal].(map[string]any)
	token, _ := ext["token"].(string)
	var resultSchema *schema.Schema
	if task.Action != nil {
		resultSchema = task.Action.ResultSchema
	}
	return ExternalTaskResp{
		Token:        token,
		Process:      inst.ProcessName,
		Version:      inst.ProcessVersion,
		TaskID:       task.ID,
		Input:        ext["input"],
		ResultSchema: resultSchema,
		WaitingSince: inst.UpdatedAt.Format(time.RFC3339),
	}
}

func (h *Handlers) resolveExternalTask(raw json.RawMessage) Reply {
	req, err := decodeBody[ResolveExternalTaskReq](raw)
	if err != nil {
		return errReply(err)
	}
	if req.Token == "" {
		return invalid("token is required").reply()
	}
	// The token is instanceID.nonce; instance ids are UUIDs (no '.'), so the part before
	// the first '.' is the instance id for a PK lookup. The exact-token check happens
	// under lock in ResolveExternalTask.
	instanceID, _, ok := strings.Cut(req.Token, ".")
	if !ok || instanceID == "" {
		return invalid("malformed token").reply()
	}
	inst, err := h.db.GetInstance(instanceID)
	if err != nil {
		return errReply(err)
	}
	task, err := h.db.CurrentTask(inst)
	if err != nil {
		return errReply(err)
	}
	if inst.Status != model.StatusRunning || inst.WaitState != model.WaitStateExternal || task == nil {
		return conflict("task is not waiting for an external result").reply()
	}
	// Validate the submitted result against the parked task's result_schema (no-op when
	// absent). The task definition is immutable, so validating the pre-lock snapshot is
	// safe; ResolveExternalTask re-checks the parked state + token atomically.
	if task.Action != nil {
		normalized, err := task.Action.ValidateOutput(req.Result)
		if err != nil {
			// The "result validation: " prefix is load-bearing — genctl keys on it
			// (cmd/genctl/commands.go, resultValidationError).
			return invalid("result validation: %w", err).reply()
		}
		req.Result = normalized
	}
	if err := h.db.ResolveExternalTask(context.Background(), instanceID, req.Token, req.Result); err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"resolved": true})
}

func (h *Handlers) signalInstance(id string, raw json.RawMessage) Reply {
	if id == "" {
		return invalid("id is required").reply()
	}
	req, err := decodeBody[SignalInstanceReq](raw)
	if err != nil {
		return errReply(err)
	}
	if req.TaskID == "" {
		return invalid("task_id is required").reply()
	}
	inst, err := h.db.GetInstance(id)
	if err != nil {
		return errReply(err)
	}
	// Paused instances still accept signals — SignalInstance buffers them FIFO and the
	// task consumes one when it next arms after a resume. A pause suspends execution,
	// not delivery; rejecting here would make a pause lose events. The correlation
	// decision (deliver now vs buffer) is made under the row lock in SignalInstance.
	if inst.Status != model.StatusRunning &&
		inst.Status != model.StatusPaused && inst.Status != model.StatusPausing {
		return conflict("instance is not running (status %s)", inst.Status).reply()
	}
	// Resolve the target external task from the pinned definition — it may be a wait point
	// reached later, not the current front task. The definition (and its result_schema) is
	// immutable for this version, so validating against it before the atomic deliver is safe.
	def, err := h.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return errReply(err)
	}
	var target *model.Task
	for _, t := range def.Tasks {
		if t.ID == req.TaskID {
			target = t
			break
		}
	}
	if target == nil {
		return notFound("no task %q in %s v%d", req.TaskID, inst.ProcessName, inst.ProcessVersion).reply()
	}
	if target.Action == nil || target.Action.Type != model.ActionTypeExternal {
		return invalid("task %q is not an external task", req.TaskID).reply()
	}
	normalized, err := target.Action.ValidateOutput(req.Result)
	if err != nil {
		return invalid("result validation: %w", err).reply()
	}
	req.Result = normalized
	delivered, err := h.db.DeliverSignal(context.Background(), id, req.TaskID, idgen.New(), req.Result)
	if err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"delivered": delivered, "buffered": !delivered})
}
