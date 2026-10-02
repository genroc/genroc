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

// advanceOutcome is everything a step changes: persist is the only writer, so one advance
// is one transaction. A path that writes for itself escapes runAdvance's discipline (CLAUDE.md).
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

// writeVerb names the outcomes whose failed write fails the instance, not the worker: only
// spawn and arm can fail on the row's own state.
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
	// Derived here, where every engine write passes, so the flag cannot drift from the task
	// the row ends up naming. specs/durability-levels.md s4.
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
	for _, c := range children {
		e.AuditCreated(c, "")
	}
	return nil
}

// persistArm parks unless an answer arrived first, which the next claim consumes through
// phase 2. Both release the lease.
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

// runAdvance is the only place the marker and the lease move: marker off BEFORE the write,
// held entry off only on return (CLAUDE.md). Tick keeps no marker; the delete is a no-op.
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
		// failInstance only touches memory, so its terminal state is written the ordinary way.
		fail := e.failInstance(inst, errcode.EngineSpawn, fmt.Sprintf("task %q %s: %v", inst.Task, verb, err))
		if err := e.persist(ctx, inst, fail); err != nil {
			if errors.Is(err, db.ErrLeaseLost) {
				e.auditLeaseLost(inst)
				return nil
			}
			return err
		}
	}
	// The closing half of the only_once bracket. Losing it loses the work done, not
	// at-most-once. specs/durability-levels.md s4.
	if onlyOnce {
		if err := e.db.Flush(ctx); err != nil {
			e.logOnly(logEvent{Level: model.LogError, ID: inst.ID,
				Msg: "could not make an only_once result durable: " + err.Error()})
		}
	}
	// Unconditional: a spurious nudge costs one empty claim.
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

// advanceGuarded fails the instance on a panic under advance. Never extend it over
// persist(): that panic is not the definition's. specs/error-handling-audit.md.
func (e *Engine) advanceGuarded(ctx context.Context, inst *model.ProcessInstance) (outcome advanceOutcome) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		reason := fmt.Sprintf("panic while advancing task %q: %v", inst.Task, r)
		stack := string(debug.Stack())

		// Console first: it touches neither the database nor the definition, so it cannot fail.
		e.logOnly(logEvent{Level: model.LogError, ID: inst.ID, Msg: reason + "\n" + stack})

		// Pre-set: the recording below can panic in turn (audit resolves the same malformed
		// definition); failInstance assigns terminal fields BEFORE auditing, so failed persists.
		outcome = advanceOutcome{kind: outcomeTerminal}
		defer func() {
			if r2 := recover(); r2 != nil {
				e.logOnly(logEvent{Level: model.LogError, ID: inst.ID,
					Msg: fmt.Sprintf("panic while recording the panic above: %v", r2)})
			}
		}()
		outcome = e.failInstance(inst, errcode.EnginePanic, reason)
		e.audit(inst, logEvent{Level: model.LogError, Event: model.EventInstanceFailed, Task: inst.Task,
			Msg: reason, Code: errcode.EnginePanic, Data: stack})
	}()
	return e.advance(ctx, inst)
}

// prepareAdvance returns a non-nil outcome the caller must return immediately.
func (e *Engine) prepareAdvance(inst *model.ProcessInstance) (*model.ProcessDefinition, int, *advanceOutcome) {
	def, err := e.definition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return nil, 0, stop(e.failInstance(inst, errcode.EngineDefinition, fmt.Sprintf("load definition: %v", err)))
	}

	// Config is never persisted: it is re-resolved every tick.
	if def.ConfigSchema != nil {
		cfg, err := def.ResolveConfig(os.LookupEnv)
		if err != nil {
			return nil, 0, stop(e.failInstance(inst, errcode.EngineConfig, fmt.Sprintf("config: %v", err)))
		}
		inst.Config = cfg
	}

	// An empty Task has run off the end, and the loop completes it.
	idx := taskIndex(def.Tasks, inst.Task)
	if inst.Task != "" && idx < 0 {
		return nil, 0, stop(e.failInstance(inst, errcode.EngineDefinition, fmt.Sprintf("current task %q not found in definition", inst.Task)))
	}

	// The task may already have run on the previous owner, which matters only to only_once.
	// specs/only-once-interrupted.md.
	if inst.ReclaimedExpired {
		e.logOnly(logEvent{Level: model.LogWarn, ID: inst.ID,
			Msg:  "reclaimed expired lease; previous owner crashed or stalled mid-task",
			Meta: map[string]any{"task": inst.Task, "process": inst.ProcessName}})
		if idx >= 0 && interruptedOnlyOnce(def.Tasks[idx]) {
			return nil, 0, stop(e.handleCallError(inst, def.Tasks[idx], interruptedMessage, errcode.OnlyOnceInterrupted))
		}
	}

	// Debug: the only per-ADVANCE event, and which worker holds a row is about the engine,
	// not the run.
	if idx >= 0 {
		e.audit(inst, logEvent{Level: model.LogDebug, Event: model.EventWorkStarted, Task: inst.Task, Meta: map[string]any{"worker": e.workerID}})
	}

	return def, idx, nil
}

// enterTask is EVERY transition, a goto to itself included: TaskEpoch addresses a spawned
// batch, so assigning inst.Task directly re-spawns under the epoch the predecessor claimed.
// Pointing at the task about to run (advance's loop head) is not an entry.
func enterTask(inst *model.ProcessInstance, taskID string) {
	inst.Task = taskID
	inst.TaskEpoch++
}

func (e *Engine) advance(ctx context.Context, inst *model.ProcessInstance) advanceOutcome {
	if inst.Status == model.StatusFailing {
		return e.settleFailing(inst)
	}
	if inst.Status == model.StatusCancelling {
		return e.settleCancelling(inst)
	}
	if inst.Status == model.StatusPausing {
		// Crash recovery only. The interrupted verdict must run BEFORE the pause settles: its
		// evidence (worker_id) does not survive that write. CLAUDE.md.
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

	// A call-less chain runs in one claim. Crash-safe: a switch only re-evaluates persisted
	// context, so resuming from the last written inst.Task is deterministic.
	const maxInlineTasks = 1000
	for i := 0; ; i++ {
		if idx < 0 || idx >= len(def.Tasks) {
			// Ran off the end of the task list: nothing left to do.
			inst.Task = ""
			inst.Status = model.StatusCompleted
			inst.WakeAt = nil
			if err := e.computeOutput(inst); err != nil {
				return e.failInstance(inst, declaredFailureCode(err, errcode.EngineOutput, errcode.EngineExpression), err.Error())
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

		// Never run an only_once action in the advance that MOVED to it: the row still names the
		// claimed task, so a crash would read as "never started". specs/durability-levels.md s4.
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

		var taskOutput any
		hasOutput := task.Output.Present()
		if hasOutput {
			remapped, err := e.evalTaskOutput(inst, task, actionResult, priorOutput, meta)
			if err != nil {
				return e.failInstance(inst, declaredFailureCode(err, errcode.EngineOutput, errcode.EngineExpression),
					fmt.Sprintf("task %q output: %v", task.ID, err))
			}
			e.setTaskOutput(inst, task.ID, remapped)
			taskOutput = remapped
		}

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

		// Neither computes the process output: only `goto: end` finishes a process.
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
				return e.failInstance(inst, declaredFailureCode(err, errcode.EngineOutput, errcode.EngineExpression), err.Error())
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
		// Inference types `last_error` only on tasks an error edge enters; leaving it would make
		// it readable where nothing declares it.
		delete(inst.State, model.StateLastError)

		inst.RetryCount = 0
		inst.WakeAt = nil
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventTaskCompleted, Task: task.ID, Msg: "→ " + gotoID})

		// A call has just executed a side effect: checkpoint and yield.
		if hasCall || i >= maxInlineTasks {
			return advanceOutcome{kind: outcomeProgress}
		}
	}
}

func (e *Engine) evalTaskOutput(inst *model.ProcessInstance, task *model.Task, result, previous any, meta *fetchMeta) (any, error) {
	out, err := e.evalShape(inst, shape.Shape{Raw: task.Output.Raw}, taskSelf(result, previous, meta))
	if err != nil {
		return nil, err
	}
	return conformDeclared(out, task.OutputSchema, fmt.Sprintf("task %q output", task.ID))
}

// selfBeforeOutput carries only `previous`: setTaskOutput has not run, so outputs[inst.Task]
// is still advance's priorOutput. specs/task-scopes.md; internal/validation/scope.go pairs it.
func (e *Engine) selfBeforeOutput(inst *model.ProcessInstance) map[string]any {
	var prev any
	if outs, ok := inst.State["outputs"].(map[string]any); ok {
		prev = outs[inst.Task]
	}
	return map[string]any{"previous": prev}
}

// taskSelf has status/headers ONLY where a fetch answered: the gate inference applies, and
// any mismatch is unreadable or null where a value was promised.
func taskSelf(result, previous any, meta *fetchMeta) map[string]any {
	self := map[string]any{"result": result, "previous": previous}
	if meta != nil {
		self["status"] = meta.status
		// Not map[string]string: the evaluator navigates JSON-native values only, and every
		// header would read null.
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

// evalSwitch returns the whole case, not its Goto: a case may raise or panic instead.
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

// taskIsOnlyOnce recovers to TRUE: persist runs outside advanceGuarded, so a panic here
// would take the worker down, and not knowing costs an fsync, never a guarantee.
func (e *Engine) taskIsOnlyOnce(inst *model.ProcessInstance) (onlyOnce bool) {
	defer func() {
		if recover() != nil {
			onlyOnce = true
		}
	}()
	return interruptedOnlyOnce(e.lookupTask(inst))
}

// lookupTask returns nil on any miss: the settle paths must not turn a transient read error
// into a failed process.
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

func taskIDAt(tasks []*model.Task, idx int) string {
	if idx < 0 || idx >= len(tasks) {
		return ""
	}
	return tasks[idx].ID
}

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

// saveAndNotify is the single exit for terminal states: a child's must also wake or fail
// its parent in the same transaction.
func (e *Engine) saveAndNotify(inst *model.ProcessInstance) error {
	if inst.ParentID == "" {
		return e.db.UpdateInstance(inst)
	}
	if inst.Status == model.StatusFailed {
		return e.db.FailInstanceAndAncestors(inst)
	}
	return e.db.FinishChild(inst)
}

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
	if out, err = conformDeclared(out, def.OutputSchema, "output"); err != nil {
		return err
	}
	inst.State["output"] = out
	return nil
}
