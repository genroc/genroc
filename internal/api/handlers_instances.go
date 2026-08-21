package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"genroc/internal/db"
	"genroc/internal/idgen"
	"genroc/internal/model"
	"genroc/internal/validation"
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
		ContextData:    map[string]any{"input": input, "outputs": map[string]any{}, "error": nil},
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
	instances, info, err := h.db.ListInstances(req.Status, req.ErrorCode,
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

func (h *Handlers) getInstance(id string, resolve bool) Reply {
	if id == "" {
		return invalid("id is required").reply()
	}
	inst, err := h.db.GetInstance(id)
	if err != nil {
		return errReply(err)
	}
	// By default, externalized value-slots are left as {ref, size} references — a
	// detail read should stay light and not pull large blobs out of the object store.
	// With resolve=true the caller opts into materializing every slot inline (and then
	// redacting), the way the full context used to always be returned.
	if resolve {
		if err := h.db.HydrateContext(inst); err != nil {
			return errReply(err)
		}
	}
	resp := instanceToResp(inst)
	// Redact secret-derived values from the returned context (the DB still holds
	// them plainly; they are just not exposed over the API).
	if sf, ok := h.schemas.Get(inst.ProcessName, inst.ProcessVersion, func() (*model.ProcessDefinition, error) {
		return h.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	}); ok {
		resp.Context = orderedContext(validation.RedactContext(inst.ContextData, sf))
	}
	return okReply(resp)
}

func (h *Handlers) pauseInstance(id string) Reply {
	if id == "" {
		return invalid("id is required").reply()
	}
	if err := h.db.PauseProcess(context.Background(), id); err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"paused": true})
}

func (h *Handlers) resumeInstance(id string) Reply {
	if id == "" {
		return invalid("id is required").reply()
	}
	if err := h.db.ResumeProcess(context.Background(), id); err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"resumed": true})
}

func (h *Handlers) retryInstance(id string, raw json.RawMessage) Reply {
	if id == "" {
		return invalid("id is required").reply()
	}
	req, err := decodeOptionalBody[RetryInstanceReq](raw)
	if err != nil {
		return errReply(err)
	}
	if err := h.db.RetryProcess(context.Background(), id, req.Force); err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"retried": true})
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
	return InstanceStatusResp{
		InstanceSummaryResp: InstanceSummaryResp{
			ID:         inst.ID,
			Process:    inst.ProcessName,
			Version:    inst.ProcessVersion,
			Status:     inst.Status,
			WaitState:  inst.WaitState,
			Task:       inst.Task,
			RetryCount: inst.RetryCount,
			Error:      inst.Error,
			ErrorCode:  inst.ErrorCode,
			CreatedAt:  inst.CreatedAt.Format(time.RFC3339),
			UpdatedAt:  inst.UpdatedAt.Format(time.RFC3339),
		},
		Context: orderedContext(inst.ContextData),
	}
}

func instanceSummaryToResp(s *model.InstanceSummary) InstanceSummaryResp {
	return InstanceSummaryResp{
		ID:         s.ID,
		Process:    s.ProcessName,
		Version:    s.ProcessVersion,
		Status:     s.Status,
		WaitState:  s.WaitState,
		Task:       s.Task,
		RetryCount: s.RetryCount,
		Error:      s.Error,
		ErrorCode:  s.ErrorCode,
		CreatedAt:  s.CreatedAt.Format(time.RFC3339),
		UpdatedAt:  s.UpdatedAt.Format(time.RFC3339),
	}
}

// orderedContext returns a copy of contextData with outputs serialized in task
// completion order (tracked by "output_order"), hiding the order key itself.
func orderedContext(ctxData map[string]any) map[string]any {
	result := make(map[string]any, len(ctxData))
	for k, v := range ctxData {
		if k != "output_order" {
			result[k] = v
		}
	}

	outputs, _ := ctxData["outputs"].(map[string]any)
	if len(outputs) == 0 {
		return result
	}

	var order []string
	switch v := ctxData["output_order"].(type) {
	case []string:
		order = v
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				order = append(order, s)
			}
		}
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	// output_order is written unique (engine.appendOutputOrder), but rows persisted before
	// that was true still hold repeats — and emitting one twice would put a duplicate key
	// in the JSON object, which parsers resolve by silently keeping the last.
	emitted := make(map[string]bool, len(order))
	for _, key := range order {
		val, ok := outputs[key]
		if !ok || emitted[key] {
			continue
		}
		emitted[key] = true
		if !first {
			buf.WriteByte(',')
		}
		keyBytes, _ := json.Marshal(key)
		valBytes, _ := json.Marshal(val)
		buf.Write(keyBytes)
		buf.WriteByte(':')
		buf.Write(valBytes)
		first = false
	}
	buf.WriteByte('}')

	result["outputs"] = json.RawMessage(buf.Bytes())
	return result
}
