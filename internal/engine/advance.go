package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"

	"genroc/internal/db"
	"genroc/internal/errcode"
	"genroc/internal/model"
	"genroc/internal/shape"
)

// advanceOutcome is the next persisted state advance() computes without writing —
// everything a step changes travels here, so persist is the only writer and one advance
// is one transaction. A path that writes for itself escapes the marker/lease discipline
// in runAdvance (see CLAUDE.md).
type advanceOutcome struct {
	kind        outcomeKind
	children    []*model.ProcessInstance // outcomeSpawn/outcomeRespawn: inserted with the parent's park
	retired     []string                 // outcomeRespawn: the attempts those children replace
	respawnLogs []string                 // outcomeRespawn: one audit line per slot, written after the commit
	arm         *externalArm             // outcomeArm: the wait to install, or the signal to consume
}

type outcomeKind uint8

const (
	outcomeProgress outcomeKind = iota // running checkpoint        → UpdateInstanceProgress
	outcomeUpdate                      // running, status/error set → UpdateInstance
	outcomeTerminal                    // completed/failed/paused   → saveAndNotify
	outcomeSpawn                       // children + parent parked  → SpawnChildrenAndWait
	outcomeArm                         // external wait             → ArmExternalUnlessSignalled
	outcomeRespawn                     // raised slots retried      → RespawnSlotsAndWait
)

// writeVerb names an outcome whose write is the instance's own failure rather than the
// worker's, and how that failure reads. Spawning children and arming an external wait are
// the two writes that can fail on the state of the row itself (a parent already parked, a
// vanished instance); the plain state writes cannot, and their errors belong to the worker.
func (o advanceOutcome) writeVerb() string {
	switch o.kind {
	case outcomeSpawn, outcomeRespawn:
		return "spawn"
	case outcomeArm:
		return "arm"
	}
	return ""
}

// stop wraps an outcome as a non-nil pointer: call helpers return it to halt the task
// loop with this outcome; a nil *advanceOutcome means "continue".
func stop(o advanceOutcome) *advanceOutcome { return &o }

// persist applies an advance outcome in one transaction — the only place an advance
// writes, and every outcome releases the lease in it: the work session ends here.
func (e *Engine) persist(ctx context.Context, inst *model.ProcessInstance, o advanceOutcome) error {
	// Derived here rather than wherever Task is assigned: every engine write goes through
	// this function, so the flag is recomputed from whatever task the row ends up naming
	// and cannot drift from it. specs/durability-levels.md s4.
	inst.NextReplayable = !e.taskIsOnlyOnce(inst)
	switch o.kind {
	case outcomeTerminal:
		return e.saveAndNotify(inst)
	case outcomeProgress:
		return e.db.UpdateInstanceProgress(inst)
	case outcomeUpdate:
		return e.db.UpdateInstance(inst)
	case outcomeSpawn:
		return e.persistSpawn(ctx, inst, o.children)
	case outcomeRespawn:
		if err := e.db.RespawnSlotsAndWait(ctx, inst, o.retired, o.children); err != nil {
			return err
		}
		// After the commit, like spawn's: an audit must never name children that do not exist.
		// One line per slot rather than one per round -- a round is not a unit anyone debugs,
		// and the action path reports each attempt too.
		for _, msg := range o.respawnLogs {
			e.audit(inst, logEvent{Level: model.LogWarn, Event: model.EventRetryScheduled, Task: inst.Task, Msg: msg})
		}
		return nil
	case outcomeArm:
		return e.persistArm(ctx, inst, o.arm)
	default:
		return fmt.Errorf("unknown advance outcome %d", o.kind)
	}
}

// persistSpawn inserts the batch and parks the parent in one transaction, then records it.
// The audits follow the commit so they never name children that do not exist.
func (e *Engine) persistSpawn(ctx context.Context, inst *model.ProcessInstance, children []*model.ProcessInstance) error {
	if err := e.db.SpawnChildrenAndWait(ctx, inst, children); err != nil {
		return err
	}
	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventChildrenSpawned, Task: inst.Task,
		Msg: fmt.Sprintf("%d children", len(children))})
	// Each spawned child is its own process: record its creation + input so its subtree
	// trail bookends the same way a root's does.
	for _, c := range children {
		e.AuditCreated(c)
	}
	return nil
}

// persistArm installs the external wait -- unless an answer arrived first, in which case it
// leaves the row claimable and the next claim consumes it through phase 2. Both release the
// lease.
func (e *Engine) persistArm(ctx context.Context, inst *model.ProcessInstance, a *externalArm) error {
	armed, err := e.db.ArmExternalUnlessSignalled(ctx, inst, a.taskID, a.input, a.wakeAt)
	if err != nil {
		return err
	}
	if armed {
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventExternalArmed, Task: a.taskID, Msg: a.armedMsg})
	}
	return nil
}

// runAdvance is the only place the marker and the lease move: marker off BEFORE the write
// (after = a freed row still marked = dispatch skips forever — a wedged instance); held
// entry off only on return, so the renewer covers the write it protects. Tick keeps no
// marker; the delete is a no-op there.
func (e *Engine) runAdvance(ctx context.Context, inst *model.ProcessInstance) error {
	defer e.dropLease(inst.ID)
	// Read off the row, before the advance moves on: no definition is resolved here, and
	// the flag is exactly what the claim path saw. See hardenClaims for the opening half.
	onlyOnce := !inst.NextReplayable
	outcome := e.advanceGuarded(ctx, inst)
	e.inflight.Delete(inst.ID)
	if err := e.persist(ctx, inst, outcome); err != nil {
		// The grant is gone: anything written now — including a failure — is the
		// clobber the fence exists to prevent. Drop the outcome.
		if errors.Is(err, db.ErrLeaseLost) {
			e.auditLeaseLost(inst)
			return nil
		}
		verb := outcome.writeVerb()
		if verb == "" {
			return err
		}
		// The write these two paths asked for is part of the step, so failing it fails
		// the instance rather than the worker — the verdict advance itself reached back
		// when they still wrote for themselves. failInstance only touches memory, so
		// the terminal state it produces is written the ordinary way.
		fail := e.failInstance(inst, errcode.EngineSpawn, fmt.Sprintf("task %q %s: %v", inst.Task, verb, err))
		if err := e.persist(ctx, inst, fail); err != nil {
			if errors.Is(err, db.ErrLeaseLost) {
				e.auditLeaseLost(inst)
				return nil
			}
			return err
		}
	}
	// The closing half of the only_once bracket: the write recording the result must
	// outlive a power cut too. Losing it does not break at-most-once — recovery reads the
	// durable claim and reports only_once.interrupted, which is a true answer — it loses
	// the work the task already did. specs/durability-levels.md s4.
	if onlyOnce {
		if err := e.db.Flush(ctx); err != nil {
			e.logOnly(logEvent{Level: model.LogError, ID: inst.ID,
				Msg: "could not make an only_once result durable: " + err.Error()})
		}
	}
	// A persisted advance may have made work runnable now (this instance again, spawned
	// children, an un-parked parent) — nudge the pump instead of idling until the tick.
	// A spurious nudge costs one empty claim, so signalling unconditionally stays simple.
	e.signalWork()
	return nil
}

// auditLeaseLost records a dropped outcome on the instance's trail. Unfenced on purpose:
// it is the only trace of the abandoned attempt, whoever owns the row now.
func (e *Engine) auditLeaseLost(inst *model.ProcessInstance) {
	e.audit(inst, logEvent{Level: model.LogWarn, Event: model.EventLeaseLost, Task: inst.Task,
		Msg: "lease lost mid-advance; outcome dropped — the instance was re-granted while this worker was still advancing it. " +
			"A stream of these means lease renewal cannot keep up: lower --max-concurrent or increase --lease-duration",
		Meta: map[string]any{"worker": e.workerID, "lease": e.leaseDuration.String(), "epoch": inst.LeaseEpoch}})
}

// advanceGuarded converts a panic under advance into a terminal EnginePanic failure (a
// panic is definition-attributable; killing the worker punishes every healthy advance).
// Never extend it over persist(): that panic is not the definition's, and there is
// nothing left to write a failure with. specs/error-handling-audit.md.
func (e *Engine) advanceGuarded(ctx context.Context, inst *model.ProcessInstance) (outcome advanceOutcome) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		reason := fmt.Sprintf("panic while advancing task %q: %v", inst.Task, r)
		stack := string(debug.Stack())

		// Console first, via logOnly: it writes neither to the database nor through
		// the definition, so it is the one report that cannot itself fail. Whatever
		// happens below, the panic is on the record somewhere.
		e.logOnly(logEvent{Level: model.LogError, ID: inst.ID, Msg: reason + "\n" + stack})

		// Pre-set the outcome: the recording below can panic in turn (audit resolves the same
		// malformed definition to redact secrets), and this value must survive it. failInstance
		// assigns terminal fields BEFORE auditing, so a death in the audit still persists failed.
		outcome = advanceOutcome{kind: outcomeTerminal}
		defer func() {
			if r2 := recover(); r2 != nil {
				e.logOnly(logEvent{Level: model.LogError, ID: inst.ID,
					Msg: fmt.Sprintf("panic while recording the panic above: %v", r2)})
			}
		}()
		outcome = e.failInstance(inst, errcode.EnginePanic, reason)
		// The stack in the instance's own trail is what makes a panicked instance
		// debuggable from the API alone, without the worker's console.
		e.audit(inst, logEvent{Level: model.LogError, Event: model.EventInstanceFailed, Task: inst.Task,
			Msg: reason, Code: errcode.EnginePanic, Data: stack})
	}()
	return e.advance(ctx, inst)
}

// prepareAdvance runs the once-per-claim setup before the task loop: load the definition,
// resolve config from the environment, locate the current task, handle a lease-takeover
// reclaim (failing an interrupted only_once task), and emit work_started. Returns the
// definition and task index, or a non-nil outcome the caller must return immediately.
func (e *Engine) prepareAdvance(inst *model.ProcessInstance) (*model.ProcessDefinition, int, *advanceOutcome) {
	// Load the definition once for the whole tick: it drives config resolution and
	// is the source of truth for the task list (the instance stores only its current
	// task id; successors are implied by definition order). An instance whose
	// definition cannot be loaded cannot run, so fail it with a clear reason.
	def, err := e.definition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return nil, 0, stop(e.failInstance(inst, errcode.EngineDefinition, fmt.Sprintf("load definition: %v", err)))
	}

	// Resolve config from the OS environment for this tick. Config is never
	// persisted — it is re-resolved every tick and exposed to expressions as
	// "config". A resolution failure (missing required var, bad coercion) fails
	// the instance with a clear reason.
	if def.ConfigSchema != nil {
		cfg, err := def.ResolveConfig(os.LookupEnv)
		if err != nil {
			return nil, 0, stop(e.failInstance(inst, errcode.EngineConfig, fmt.Sprintf("config: %v", err)))
		}
		inst.Config = cfg
	}

	// Resolve the instance's position in the task list. An empty Task means it has
	// run off the end (nothing left) — the loop completes it. A non-empty Task that
	// isn't in the definition is a corrupt/mismatched row: fail it.
	idx := taskIndex(def.Tasks, inst.Task)
	if inst.Task != "" && idx < 0 {
		return nil, 0, stop(e.failInstance(inst, errcode.EngineDefinition, fmt.Sprintf("current task %q not found in definition", inst.Task)))
	}

	// Reclaimed from an expired lease: the task may already have run on the previous owner.
	// Re-running is fine unless only_once — handed to the definition as only_once.interrupted
	// (routable, never retryable; uncaught = the same terminal failure). specs/only-once-interrupted.md.
	if inst.ReclaimedExpired {
		e.logOnly(logEvent{Level: model.LogWarn, ID: inst.ID,
			Msg:  "reclaimed expired lease; previous owner crashed or stalled mid-task",
			Meta: map[string]any{"task": inst.Task, "process": inst.ProcessName}})
		if idx >= 0 && interruptedOnlyOnce(def.Tasks[idx]) {
			return nil, 0, stop(e.handleCallError(inst, def.Tasks[idx], interruptedMessage, errcode.OnlyOnceInterrupted))
		}
	}

	// work_started: a worker has picked this instance up and is about to work its
	// current task. One per claim (a resume after parking emits it again), tagged with
	// the worker so the unified log shows who is doing what.
	if idx >= 0 {
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventWorkStarted, Task: inst.Task, Meta: map[string]any{"worker": e.workerID}})
	}

	return def, idx, nil
}

// advance executes the next task in the instance's queue and returns the outcome to
// persist (it does no lease-releasing write — runAdvance does). Each task may have a call
// and/or a switch: the call runs first, then the switch evaluates with the call's output
// as "self"; a matching case jumps to the named task, else the next task in the queue runs.
// enterTask moves the instance to a task and counts the entry. EVERY transition goes
// through it -- next, a goto, and a goto back to the task just run -- because TaskEpoch is
// what addresses a spawned batch: assigning inst.Task directly leaves a re-entered child
// task spawning a second batch under the epoch its predecessor already claimed, and the
// collect then gathers both. Pointing at the task about to run (advance's loop head) is NOT
// an entry: a parked parent resumes there to collect and must keep the epoch it spawned under.
func enterTask(inst *model.ProcessInstance, taskID string) {
	inst.Task = taskID
	inst.TaskEpoch++
}

func (e *Engine) advance(ctx context.Context, inst *model.ProcessInstance) advanceOutcome {
	if inst.Status == model.StatusFailing {
		return e.settleFailing(inst)
	}
	if inst.Status == model.StatusPausing {
		// Crash recovery only (a live pause lands in SQL on the owner's write). The interrupted
		// only_once verdict must run BEFORE the pause settles — its evidence (worker_id) does not
		// survive that write; status 'running' + the UpdateInstance CASE still land the pause.
		// specs/only-once-interrupted.md.
		if inst.ReclaimedExpired {
			if task := e.lookupTask(inst); interruptedOnlyOnce(task) {
				inst.Status = model.StatusRunning
				return e.handleCallError(inst, task, interruptedMessage, errcode.OnlyOnceInterrupted)
			}
		}
		return e.settlePausing(inst)
	}

	def, idx, done := e.prepareAdvance(inst)
	if done != nil {
		return *done
	}

	// A call-less task chain collapses into one claim and one write (bounded by maxInlineTasks
	// against an all-switch loop). Crash-safe: a switch only re-evaluates persisted context,
	// so resuming from the last written inst.Task is deterministic.
	const maxInlineTasks = 1000
	for i := 0; ; i++ {
		if idx < 0 || idx >= len(def.Tasks) {
			// Ran off the end of the task list: nothing left to do.
			inst.Task = ""
			inst.Status = model.StatusCompleted
			inst.WakeAt = nil
			if err := e.computeOutput(inst); err != nil {
				return e.failInstance(inst, errcode.EngineExpression, err.Error())
			}
			e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventInstanceDone, Data: e.outputData(inst)})
			return advanceOutcome{kind: outcomeTerminal}
		}

		task := def.Tasks[idx]
		// Point the instance at the task about to run, so any mid-task persist (park,
		// retry, error route, fail) records this task as the resume point.
		inst.Task = task.ID
		hasCall := task.Action != nil
		var actionResult any

		// Capture this task's prior output before the action can overwrite it, so an
		// output map may reference self.previous (the value from the last loop iteration).
		var meta *fetchMeta
		var priorOutput any
		if task.Output.Present() {
			if outs, ok := inst.State["outputs"].(map[string]any); ok {
				priorOutput = outs[task.ID]
			}
		}

		// An only_once action is never executed in the same advance that MOVED to it. The
		// row still names the task this advance was claimed at, and that name is all
		// recovery has: reaching this one inline and running it would leave a crash
		// indistinguishable from "never started", so prepareAdvance would re-run a request
		// that already left. Checkpointing here makes the row name this task, which is the
		// case the bracket already protects -- the next claim sees only_once, hardens, and
		// runs it. Costs one claim round trip, and only for definitions that put an
		// only_once action behind a call-less chain.
		//
		// The checkpoint itself need not be durable: losing it rewinds to before the action
		// ran, and re-running the chain that led here is free.
		// specs/durability-levels.md s4.
		if hasCall && i > 0 && interruptedOnlyOnce(task) {
			return advanceOutcome{kind: outcomeProgress}
		}

		if hasCall {
			switch task.Action.Type {
			case model.ActionTypeChild, model.ActionTypeChildMap, model.ActionTypeChildList:
				out, done := e.runChildProcesses(ctx, inst, task)
				if done != nil {
					return *done
				}
				actionResult = out
			case model.ActionTypeDelay:
				if done := e.runDelay(inst, task); done != nil {
					return *done
				}
				// Timer fired: fall through to the switch with no action result.
			case model.ActionTypeExternal:
				out, done := e.runExternal(ctx, inst, task)
				if done != nil {
					return *done
				}
				actionResult = out
			default: // fetch
				out, fm, done := e.executeAction(ctx, inst, task)
				if done != nil {
					return *done
				}
				actionResult, meta = out, fm
			}
		}

		// The output projection (if any) is the only thing exported (outputs.taskID).
		// The raw result is never stored; it is exposed transiently to this task's own
		// output/switch as self.result.
		var taskOutput any
		hasOutput := task.Output.Present()
		if hasOutput {
			remapped, err := e.evalTaskOutput(inst, task, actionResult, priorOutput, meta)
			if err != nil {
				return e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q output: %v", task.ID, err))
			}
			e.setTaskOutput(inst, task.ID, remapped)
			taskOutput = remapped
		}

		// self is this task's transient scope: result (raw action result) and
		// previous (its own prior output), plus output (the projection) only when one
		// is defined. None of these but the projection persist beyond this task.
		self := taskSelf(actionResult, priorOutput, meta)
		if hasOutput {
			self["output"] = taskOutput
		}
		matched, err := e.evalSwitch(inst, task, self)
		if err != nil {
			return e.failInstance(inst, errcode.EngineExpression, fmt.Sprintf("task %q switch: %v", task.ID, err))
		}
		if matched == nil {
			// Validation requires a catch-all case, but legacy rows in the DB may
			// predate that rule — fail the instance rather than panic on gotoID[1:].
			return e.failInstance(inst, errcode.EngineDefinition, fmt.Sprintf("task %q switch: no case matched", task.ID))
		}

		// A terminal clause ends the process here, in place of routing. Neither computes
		// the process output: only `goto: end` finishes a process, and a raise or panic
		// is an exit from wherever the instance happens to have got to.
		if matched.Raise != nil {
			return e.raiseInstance(inst, task, matched.Raise, self)
		}
		if matched.Panic != nil {
			return e.panicInstance(inst, task, matched.Panic, self)
		}
		gotoID := matched.Goto

		if gotoID == model.GotoEnd {
			inst.Status = model.StatusCompleted
			inst.RetryCount = 0
			inst.WakeAt = nil
			if err := e.computeOutput(inst); err != nil {
				return e.failInstance(inst, errcode.EngineExpression, err.Error())
			}
			e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventInstanceDone, Task: task.ID, Data: e.outputData(inst)})
			return advanceOutcome{kind: outcomeTerminal}
		}

		if gotoID == model.GotoNext {
			idx++
		} else {
			// gotoID is a task reference like "$ship" — strip the sigil.
			if idx = taskIndex(def.Tasks, gotoID[1:]); idx < 0 {
				return e.failInstance(inst, errcode.EngineDefinition, fmt.Sprintf("goto task %q not found in %q v%d", gotoID[1:], inst.ProcessName, inst.ProcessVersion))
			}
		}
		// Reflect the new position (empty once we run past the last task) so a
		// checkpoint here persists the next task to run, not the one just completed.
		enterTask(inst, taskIDAt(def.Tasks, idx))
		// `error` is scoped to the task its on_error rule routed to. An ordinary transition
		// leaves that task, so the failure stops being in scope here — a handler that wants
		// it to travel projects it into its own output, which is how every other value
		// moves. Inference types `error` on exactly the tasks an error edge enters, so
		// leaving it in the context would make it readable where nothing declares it.
		delete(inst.State, "error")

		inst.RetryCount = 0
		inst.WakeAt = nil
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventTaskCompleted, Task: task.ID, Msg: "→ " + gotoID})

		// A task with a call has just executed a side effect — checkpoint and yield.
		// A call-less routing task had none, so continue in-memory to the next task
		// unless we've hit the inline-task guard.
		if hasCall || i >= maxInlineTasks {
			return advanceOutcome{kind: outcomeProgress}
		}
	}
}

// evalTaskOutput evaluates a task's output map against the context plus self,
// where self.result is the raw action result and self.previous is this task's
// prior output (its value from the last loop iteration, or nil on the first run).
func (e *Engine) evalTaskOutput(inst *model.ProcessInstance, task *model.Task, result, previous any, meta *fetchMeta) (any, error) {
	return e.evalShape(inst, shape.Shape{Raw: task.Output.Raw}, taskSelf(result, previous, meta))
}

// taskSelf builds the transient self scope. status/headers appear ONLY where a fetch
// answered, which is the same gate inference applies — a slot present at runtime but absent
// from the schema is unreadable, and one present in the schema but absent at runtime reads
// null where the type promised a value.
func taskSelf(result, previous any, meta *fetchMeta) map[string]any {
	self := map[string]any{"result": result, "previous": previous}
	if meta != nil {
		self["status"] = meta.status
		// map[string]any, not map[string]string: the evaluator navigates JSON-native values
		// only, and a typed Go map reads as an opaque scalar — every header would come back
		// null while the schema promised a string.
		headers := make(map[string]any, len(meta.headers))
		for k, v := range meta.headers {
			headers[k] = v
		}
		self["headers"] = headers
	}
	return self
}

// setTaskOutput stores value as the task's exported output (outputs.taskID). A loop
// re-execution overwrites the value; appendOutputOrder owns keeping the position unique.
func (e *Engine) setTaskOutput(inst *model.ProcessInstance, taskID string, value any) {
	if inst.State["outputs"] == nil {
		inst.State["outputs"] = map[string]any{}
	}
	inst.State["outputs"].(map[string]any)[taskID] = value
}

// evalSwitch returns the first matching case (empty Case = catch-all; nil never happens on
// validated definitions). The whole case, not its Goto: a case may raise or panic instead
// of routing, and "" cannot say which.
func (e *Engine) evalSwitch(inst *model.ProcessInstance, task *model.Task, selfOutput any) (*model.SwitchCase, error) {
	for i := range task.Switch {
		c := &task.Switch[i]
		if c.Case == "" {
			return c, nil
		}
		v, err := e.evalShape(inst, shape.Shape{Raw: c.Case, Expr: true}, selfOutput)
		if err != nil {
			return nil, fmt.Errorf("case %q: %w", c.Case, err)
		}
		ok, isBool := v.(bool)
		if !isBool {
			return nil, fmt.Errorf("case %q: expected bool, got %T", c.Case, v)
		}
		if ok {
			return c, nil
		}
	}
	return nil, nil
}

// Returns nil when there is no current task or the definition cannot be read. Callers
// that must fail on a missing definition use prepareAdvance instead; this is for the
// settle paths, which must not turn a transient read error into a failed process.
// taskIsOnlyOnce resolves the flag from the definition, for the write that stores it. This
// is the ONE place a definition is resolved for durability -- the claim path reads the
// stored flag instead, which is the point of storing it.
//
// It recovers, and recovers to TRUE: persist runs after advanceGuarded, so a definition
// malformed enough to panic (a null in `tasks`) reaches it having already failed the
// instance, and must not take the worker down on the way out. True is the safe answer for
// the same reason an unclassified write path syncs -- not knowing costs an fsync, never a
// guarantee.
func (e *Engine) taskIsOnlyOnce(inst *model.ProcessInstance) (onlyOnce bool) {
	defer func() {
		if recover() != nil {
			onlyOnce = true
		}
	}()
	return interruptedOnlyOnce(e.lookupTask(inst))
}

func (e *Engine) lookupTask(inst *model.ProcessInstance) *model.Task {
	if inst.Task == "" {
		return nil
	}
	def, err := e.definition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return nil
	}
	idx := taskIndex(def.Tasks, inst.Task)
	if idx < 0 {
		return nil
	}
	return def.Tasks[idx]
}

// taskIndex returns the position of taskID in tasks, or -1 if absent (the empty id —
// "no current task" — is always absent).
func taskIndex(tasks []*model.Task, taskID string) int {
	if taskID == "" {
		return -1
	}
	for i, t := range tasks {
		if t.ID == taskID {
			return i
		}
	}
	return -1
}

// taskIDAt returns the id of the task at idx, or "" when idx is out of range (the
// instance has advanced past the last task).
func taskIDAt(tasks []*model.Task, idx int) string {
	if idx < 0 || idx >= len(tasks) {
		return ""
	}
	return tasks[idx].ID
}

// resolveGoto validates that the instance's definition contains taskID so the engine can
// point the instance at it (no queue is built — successors are implied by definition
// order). Used by the on-error route, which has no definition in scope.
func (e *Engine) resolveGoto(inst *model.ProcessInstance, taskID string) error {
	def, err := e.definition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return fmt.Errorf("resolve goto: %w", err)
	}
	if taskIndex(def.Tasks, taskID) < 0 {
		return fmt.Errorf("goto task %q not found in %q v%d", taskID, inst.ProcessName, inst.ProcessVersion)
	}
	return nil
}

// saveAndNotify is the single exit point for all terminal instance states. Root and
// failed instances save directly; a non-failed child uses FinishChild, which atomically
// saves it and moves the parent to WaitStateCollecting once all siblings are done.
func (e *Engine) saveAndNotify(inst *model.ProcessInstance) error {
	if inst.ParentID == "" {
		return e.db.UpdateInstance(inst)
	}
	if inst.Status == model.StatusFailed {
		return e.db.FailInstanceAndAncestors(inst)
	}
	return e.db.FinishChild(inst)
}

// computeOutput evaluates the definition's Output map against the final context and
// stores it in context_data["output"]. No-op when the definition has no Output map.
func (e *Engine) computeOutput(inst *model.ProcessInstance) error {
	def, err := e.definition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return fmt.Errorf("load definition for output: %w", err)
	}
	if !def.Output.Present() {
		return nil
	}
	out, err := e.evalShape(inst, shape.Shape{Raw: def.Output.Raw}, nil)
	if err != nil {
		return fmt.Errorf("output: %w", err)
	}
	inst.State["output"] = out
	return nil
}
