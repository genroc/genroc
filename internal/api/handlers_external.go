package api

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"genroc/internal/db"
	"genroc/internal/idgen"
	"genroc/internal/model"
	"genroc/internal/schema"
)

func (h *Handlers) listExternalTasks(raw json.RawMessage) Reply {
	req, err := decodeOptionalBody[ListExternalTasksReq](raw)
	if err != nil {
		return errReply(err)
	}
	instances, info, err := h.db.ListExternalTasks(req.Process, req.Version, req.Task,
		db.Window{After: req.UpdatedAfter, Before: req.UpdatedBefore}, req.page())
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
	ext, _ := inst.State[model.StateExternal].(map[string]any)
	// Derived from the row, not read back from external_data — the epoch IS the occurrence.
	token := model.ExternalToken(inst.ID, inst.TaskEpoch)
	var resultSchema *schema.Schema
	if task.Action != nil {
		resultSchema = task.Action.ResultSchema
	}
	var raises model.Raises
	if task.Action != nil {
		raises = task.Action.Raises
	}
	var claimedBy, claimExpires string
	// A holder whose visibility timeout has passed is not reported: the row is claimable
	// again, and naming a dead worker would read as work in progress. The column keeps the id
	// regardless — that is the evidence a lost claim is recognised by.
	if inst.ExternalWorkerID != nil && inst.ExternalLeaseExpiresAt != nil && inst.ExternalLeaseExpiresAt.After(db.Now()) {
		claimedBy = *inst.ExternalWorkerID
		claimExpires = inst.ExternalLeaseExpiresAt.Format(time.RFC3339)
	}
	var deadline string
	if inst.WakeAt != nil {
		deadline = inst.WakeAt.Format(time.RFC3339)
	}
	// The task input can hold externalized values (a bundle embedded in a definition, once
	// those become objects), so a queue entry lists them the same way a log entry does.
	var objects []ObjectEntry
	input := extractObjects(ext["input"], []any{"input"}, &objects)
	return ExternalTaskResp{
		Token:        token,
		Process:      inst.ProcessName,
		Version:      inst.ProcessVersion,
		TaskID:       task.ID,
		Input:        input,
		Objects:      objects,
		ResultSchema: resultSchema,
		Raises:       raises,
		WaitingSince: inst.UpdatedAt.Format(time.RFC3339),
		Deadline:     deadline,
		ClaimedBy:    claimedBy,
		ClaimExpires: claimExpires,
	}
}

// buildOutcome validates a submitted outcome against what the task declares and returns the
// value to store. Both addressing modes go through it, so resolve (by token) and signal (by
// instance + task) cannot drift on what they accept — the drift that left signal able to
// report success and not failure in the first place.
//
// The failure payload is conformed HERE rather than in the engine: unlike a child's raise,
// whose producer is another process that cannot be told, the submitter is an HTTP caller
// holding the connection, so a mismatch is a 400 it can act on and answer again after.
func buildOutcome(task *model.Task, result any, fail *FailureReq) (model.ExternalOutcome, *Error) {
	if fail != nil && result != nil {
		return model.ExternalOutcome{}, invalid("a submission carries one outcome: `result` or `error`, not both")
	}
	if fail == nil {
		if task.Action != nil {
			normalized, err := task.Action.ValidateOutput(result)
			if err != nil {
				// The "result validation: " prefix is load-bearing — genctl keys on it
				// (cmd/genctl/commands.go, resultValidationError).
				return model.ExternalOutcome{}, invalid("result validation: %w", err)
			}
			result = normalized
		}
		return model.ExternalOutcome{Result: result}, nil
	}
	if fail.Message == "" {
		return model.ExternalOutcome{}, invalid("error.message is required — it is what error.message carries and what the audit trail shows")
	}
	// The code lands in error_code and is what on_error rules match, so a caller must not be
	// able to spell an engine code: "http.500" would be caught by a rule written for the wire,
	// and "external.timeout" is unknowable, which an only_once task can never retry.
	if strings.Contains(fail.Code, ".") {
		return model.ExternalOutcome{}, invalid("error.code %q must not contain '.' — dots are reserved for engine-produced codes", fail.Code)
	}
	if !model.ValidFaultCode(fail.Code) {
		return model.ExternalOutcome{}, invalid("error.code %q is not a valid error code (lower_snake_case, no dots)", fail.Code)
	}

	// `raises` IS the error channel's contract on an external task: a code outside it is
	// refused rather than routed to whatever catch-all happens to exist. Unlike a child --
	// whose raisable set comes from its own definition, so a typo in an on_error pattern is
	// already caught at registration -- nothing about a caller is knowable until it submits,
	// which makes this the only place a wrong code can ever be caught.
	var declared model.Raises
	if task.Action != nil {
		declared = task.Action.Raises
	}
	sc, ok := declared[fail.Code]
	if !ok {
		if len(declared) == 0 {
			return model.ExternalOutcome{}, invalid("task %q declares no raises, so it has no error channel — declare the codes a caller may submit before answering with one", task.ID)
		}
		return model.ExternalOutcome{}, invalid("error.code %q is not declared by task %q; it accepts: %s", fail.Code, task.ID, strings.Join(sortedRaiseCodes(declared), ", "))
	}

	out := &model.ExternalFailure{Code: fail.Code, Message: fail.Message}
	if sc == nil {
		// `raises: {code: null}` — declared to carry nothing. Sending a payload anyway is a
		// contract violation like any shape mismatch, and `data` stays absent rather than
		// null: absence is what the validator infers for this code, and a context richer than
		// its type is how an expression comes to read a slot the next reader cannot.
		if fail.Data != nil {
			return model.ExternalOutcome{}, invalid("error.code %q is declared as carrying no data (raises[%q] is null), but data was submitted", fail.Code, fail.Code)
		}
		return model.ExternalOutcome{Failure: out}, nil
	}
	normalized, err := sc.Validate(fail.Data)
	if err != nil {
		return model.ExternalOutcome{}, invalid("error.data validation: %w", err)
	}
	out.Data, out.HasData = normalized, true
	return model.ExternalOutcome{Failure: out}, nil
}

// sortedRaiseCodes renders the accepted set for a refusal message, in a stable order so the
// same wrong code reports the same line every time.
func sortedRaiseCodes(r model.Raises) []string {
	codes := make([]string, 0, len(r))
	for code := range r {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

func (h *Handlers) resolveExternalTask(raw json.RawMessage) Reply {
	req, err := decodeBody[ResolveExternalTaskReq](raw)
	if err != nil {
		return errReply(err)
	}
	if req.Token == "" {
		return invalid("token is required").reply()
	}
	// instanceID for the PK lookup, the epochs for the occurrence and grant checks — both
	// happen under lock in ResolveExternalTask, against the row's own columns.
	instanceID, epoch, claimEpoch, hasClaim, ok := model.ParseExternalToken(req.Token)
	if !ok {
		return invalid("malformed token").reply()
	}
	claim := db.Unclaimed
	if hasClaim {
		claim = db.BoundToClaim(claimEpoch)
	}
	inst, err := h.db.GetInstance(instanceID)
	if err != nil {
		return errReply(err)
	}
	task, err := h.db.CurrentTask(inst)
	if err != nil {
		return errReply(err)
	}
	if !inst.Status.AcceptsExternalOutcome() || inst.WaitState != model.WaitStateExternal || task == nil {
		return conflict("task is not waiting for an external result").reply()
	}
	// The task definition is immutable, so validating the pre-lock snapshot is safe;
	// ResolveExternalTask re-checks the parked state + token atomically.
	outcome, bad := buildOutcome(task, req.Result, req.Error)
	if bad != nil {
		return bad.reply()
	}
	if err := h.db.ResolveExternalTask(context.Background(), instanceID, epoch, claim, outcome); err != nil {
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
	if !inst.Status.AcceptsExternalOutcome() {
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
	outcome, bad := buildOutcome(target, req.Result, req.Error)
	if bad != nil {
		return bad.reply()
	}
	delivered, err := h.db.DeliverSignal(context.Background(), id, req.TaskID, idgen.New(), outcome)
	if err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"delivered": delivered, "buffered": !delivered})
}

// Defaults for a claim's visibility timeout and batch size. The lease is short on purpose: a
// worker that dies should return its work quickly, and one that needs longer renews rather than
// asking for a long grant it may not survive.
const (
	defaultClaimLeaseMs = 30_000
	maxClaimLeaseMs     = 3_600_000
	defaultClaimLimit   = 1
	maxClaimLimit       = 100
)

func claimLease(ms int64) (time.Duration, *Error) {
	if ms == 0 {
		return defaultClaimLeaseMs * time.Millisecond, nil
	}
	if ms < 0 || ms > maxClaimLeaseMs {
		return 0, invalid("lease_ms must be between 1 and %d", maxClaimLeaseMs)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// claimExternalTasks leases parked external tasks to a worker. The response is the queue entry
// plus a three-part token: the handle that names the grant, and the only one accepted while the
// claim is live.
func (h *Handlers) claimExternalTasks(raw json.RawMessage) Reply {
	req, err := decodeBody[ClaimExternalTasksReq](raw)
	if err != nil {
		return errReply(err)
	}
	if req.WorkerID == "" {
		return invalid("worker_id is required — it is the claim's holder, and what renew is scoped to").reply()
	}
	lease, bad := claimLease(req.LeaseMs)
	if bad != nil {
		return bad.reply()
	}
	limit := req.Limit
	if limit == 0 {
		limit = defaultClaimLimit
	}
	if limit < 0 || limit > maxClaimLimit {
		return invalid("limit must be between 1 and %d", maxClaimLimit).reply()
	}

	instances, err := h.db.ClaimExternalTasks(req.WorkerID, lease, limit, req.Process, req.Version, req.Task)
	if err != nil {
		return errReply(err)
	}
	resp := make([]ExternalTaskResp, 0, len(instances))
	for _, inst := range instances {
		task, err := h.db.CurrentTask(inst)
		if err != nil || task == nil {
			continue // a concurrent transition; the claim expires on its own
		}
		// An only_once task whose previous holder let its claim lapse must NOT be handed out
		// again: the first worker may already have done the work, which is the whole guarantee.
		// The decision lives here rather than in the claim's SQL because only_once is a
		// property of the definition, and this loop already has it in hand. The grant is undone
		// and the arming marked instead, so the engine reports external.lost rather than the
		// instance sitting unclaimable with nothing saying why.
		if inst.ExternalReclaimed && task.OnlyOnce != nil && *task.OnlyOnce {
			// A conflict here means the row moved on between the grant and this write -- the
			// lapsed holder came back late and answered, which is allowed and is the whole
			// point of "expiry alone writes nothing". It is this ROW's news, not the request's:
			// failing the call would strand every other task the same batch legitimately
			// claimed, since those grants are already written and nothing would return them.
			err := h.db.MarkExternalClaimLost(context.Background(), inst.ID, inst.TaskEpoch)
			if err != nil && !errors.Is(err, db.ErrConflict) && !errors.Is(err, db.ErrNotFound) {
				return errReply(err)
			}
			continue
		}
		entry := externalTaskToResp(inst, task)
		entry.Token = model.ClaimToken(inst.ID, inst.TaskEpoch, inst.ExternalClaimEpoch)
		resp = append(resp, entry)
	}
	return okReply(map[string]any{"items": resp})
}

// renewExternalClaims extends this worker's claims. Renewing is scoped to the holder and never
// bumps the claim epoch: a renewal extends a grant, and bumping would fence the worker out of
// its own answer. renewed reports how many were still held, so a worker learns it lost one here
// rather than when its answer is refused.
func (h *Handlers) renewExternalClaims(raw json.RawMessage) Reply {
	req, err := decodeBody[RenewExternalClaimsReq](raw)
	if err != nil {
		return errReply(err)
	}
	if req.WorkerID == "" {
		return invalid("worker_id is required").reply()
	}
	if len(req.Tokens) == 0 {
		return invalid("tokens is required").reply()
	}
	lease, bad := claimLease(req.LeaseMs)
	if bad != nil {
		return bad.reply()
	}
	ids := make([]string, 0, len(req.Tokens))
	for _, t := range req.Tokens {
		id, _, _, hasClaim, ok := model.ParseExternalToken(t)
		if !ok || !hasClaim {
			return invalid("token %q is not a claim token — renew takes the three-part token a claim granted", t).reply()
		}
		ids = append(ids, id)
	}
	n, err := h.db.RenewExternalClaims(context.Background(), req.WorkerID, ids, lease)
	if err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"renewed": n, "requested": len(req.Tokens)})
}

// releaseExternalTask hands a claim back to the queue immediately instead of waiting out its
// lease. It bumps the claim epoch, unlike an expiry, which writes nothing: a release is
// deliberate, so the releasing worker's own handle must stop working at once.
func (h *Handlers) releaseExternalTask(raw json.RawMessage) Reply {
	req, err := decodeBody[ReleaseExternalTaskReq](raw)
	if err != nil {
		return errReply(err)
	}
	id, epoch, claimEpoch, hasClaim, ok := model.ParseExternalToken(req.Token)
	if !ok || !hasClaim {
		return invalid("token must be the three-part token a claim granted").reply()
	}
	if err := h.db.ReleaseExternalClaim(context.Background(), id, epoch, claimEpoch); err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"released": true})
}
