package api

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"genroc/internal/db"
	"genroc/internal/model"
	"genroc/internal/schema"
)

func externalTaskToResp(inst *model.ProcessInstance, task *model.Task) ExternalTaskResp {
	// Derived from the row, not read back from a column — the epoch IS the occurrence.
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
	// A lapsed holder is not reported: the row is claimable again. The column keeps the id
	// regardless — it is the evidence a lost claim is recognised by.
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
	// Rooted at the field name, which is the same word the slot, the column and both instance
	// views use -- a claim's paths address the ENTRY it hands out, not the instance it came from.
	input := extractObjects(inst.State[model.StateExternalInput], []any{model.StateExternalInput}, &objects)
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

// buildOutcome is shared by resolve and signal so they cannot drift. The failure payload is
// conformed here, not in the engine: the submitter holds the connection, so a 400 is actionable.
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
	// A caller must not spell an engine code: "http.500" would match rules written for the
	// wire, and "external.timeout" is unknowable.
	if strings.Contains(fail.Code, ".") {
		return model.ExternalOutcome{}, invalid("error.code %q must not contain '.' — dots are reserved for engine-produced codes", fail.Code)
	}
	if !model.ValidFaultCode(fail.Code) {
		return model.ExternalOutcome{}, invalid("error.code %q is not a valid error code (lower_snake_case, no dots)", fail.Code)
	}

	// `raises` is the error channel's contract: an undeclared code is refused, not routed to a
	// catch-all. Unlike a child's, a worker's codes are unknowable before it submits.
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
		// `raises: {code: null}`: `data` stays absent, not null, because absence is what the
		// validator infers for this code.
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
	// A cancelled instance gets its own sentence: the generic one reads as a race the worker
	// should retry, and this is the one case where it must stop instead.
	if inst.Status == model.StatusCancelled || inst.Status == model.StatusCancelling {
		return conflict("instance was cancelled; stop the work and release the claim").reply()
	}
	if !inst.Status.AcceptsExternalOutcome() || inst.Phase != model.PhaseExternal || task == nil {
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

func (h *Handlers) signalInstance(raw json.RawMessage) Reply {
	req, err := decodeBody[SignalInstanceReq](raw)
	if err != nil {
		return errReply(err)
	}
	if req.InstanceID == "" {
		return invalid("instance_id is required").reply()
	}
	if req.TaskID == "" {
		return invalid("task is required").reply()
	}
	id := req.InstanceID
	inst, err := h.db.GetInstance(id)
	if err != nil {
		return errReply(err)
	}
	// A pause suspends execution, not delivery: SignalInstance buffers under the row lock and
	// the task consumes it when it re-arms after resume.
	if !inst.Status.AcceptsExternalOutcome() {
		return conflict("instance is not running (status %s)", inst.Status).reply()
	}
	// The target may be a later wait point, not the front task. The pinned definition is
	// immutable, so validating before the atomic deliver is safe.
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
	delivered, err := h.db.DeliverSignal(context.Background(), id, req.TaskID, outcome)
	if err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"delivered": delivered, "buffered": !delivered})
}

// Claim defaults. The lease is short so a dead worker's work returns quickly; one that needs
// longer renews.
const (
	defaultClaimLeaseMs = 30_000
	maxClaimLeaseMs     = 3_600_000
	defaultClaimLimit   = 1
	maxClaimLimit       = 100
)

// renewBefore is a third of the lease, leaving room for two failed renewals. Reported on every
// grant so the worker never guesses it. specs/external-task-queue.md.
func renewBefore(lease time.Duration) int64 { return lease.Milliseconds() / 3 }

func claimLease(ms int64) (time.Duration, *Error) {
	if ms == 0 {
		return defaultClaimLeaseMs * time.Millisecond, nil
	}
	if ms < 0 || ms > maxClaimLeaseMs {
		return 0, invalid("lease_ms must be between 1 and %d", maxClaimLeaseMs)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// claimExternalTasks grants a three-part token, the only handle accepted while the claim is
// live.
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
		// A lapsed claim on an only_once task must NOT be handed out again: the first worker may
		// have done the work. Here, not in SQL, because only_once is the definition's.
		if inst.ExternalReclaimed && task.OnlyOnce != nil && *task.OnlyOnce {
			// A conflict means the lapsed holder answered late, which is allowed. It is this ROW's
			// news: failing the call would strand the batch's other grants, already written.
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
	return okReply(map[string]any{"items": resp, "renew_before_ms": renewBefore(lease)})
}

// renewExternalClaims must not bump the claim epoch, which would fence the worker out of its own
// answer. It answers per TOKEN, and `cancelled` rides it. specs/external-task-queue.md.
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
	out, err := h.db.RenewExternalClaims(context.Background(), req.WorkerID, ids, lease)
	if err != nil {
		return errReply(err)
	}
	// Answered in the caller's tokens, not instance ids; re-walked over req.Tokens so two
	// tokens naming one instance both get an answer.
	bucket := make(map[string]*[]string, len(ids))
	renewed, lost, cancelled := []string{}, []string{}, []string{}
	for _, id := range out.Renewed {
		bucket[id] = &renewed
	}
	for _, id := range out.Lost {
		bucket[id] = &lost
	}
	for _, id := range out.Cancelled {
		bucket[id] = &cancelled
	}
	for i, t := range req.Tokens {
		if b := bucket[ids[i]]; b != nil {
			*b = append(*b, t)
		}
	}
	return okReply(map[string]any{
		"renewed": renewed, "lost": lost, "cancelled": cancelled,
		"renew_before_ms": renewBefore(lease),
	})
}

// releaseExternalTask bumps the claim epoch, unlike an expiry, which writes nothing: the
// releasing worker's own handle must stop working at once.
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
