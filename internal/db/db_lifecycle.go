package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/model"
)

// FinishChild atomically saves a child that stopped — terminal, or a pause/cancel landing — and
// settles up: it wakes the parent once the batch is terminal, and settles every draining ancestor
// left with nothing busy below it. A root only saves itself; failed children use
// FailInstanceAndAncestors instead.
func (db *DB) FinishChild(child *model.ProcessInstance) error {
	if child.ParentID == "" {
		return db.UpdateInstance(child)
	}
	var settled []settledRow
	err := db.withChildTx(child, lockParent, func(qtx *dbgen.Queries, held lockScope) error {
		ctx := context.Background()
		parent, err := qtx.GetSettleInfo(ctx, child.ParentID)
		// An absent parent is control flow, not a lookup failure — a parent already gone — so
		// this stays sql.ErrNoRows and never becomes ErrNotFound.
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read parent: %w", err)
		}
		if err == nil && model.Status(parent.Status).Draining() && held < lockStack {
			return errWiden{lockStack}
		}

		// The fence sits here; a refused child write rolls back the settle-up too.
		now := nowMillis()
		cols, err := db.persistState(ctx, qtx, child, now)
		if err != nil {
			return err
		}
		if err := requireFenced(qtx.UpdateInstance(ctx, updateInstanceParams(child, cols, now))); err != nil {
			if errors.Is(err, ErrLeaseLost) {
				return err
			}
			return fmt.Errorf("save child: %w", err)
		}
		settled, err = settleUp(ctx, qtx, child, now)
		return err
	})
	if err != nil {
		return err
	}
	db.logSettled(settled)
	return nil
}

// FailInstanceAndAncestors atomically marks a child failed, propagates 'failing' up its call
// stack, pauses the rest of the tree if this failure is the one that poisons the root, and settles
// up as FinishChild does. Use it instead of UpdateInstance + FailAncestors.
func (db *DB) FailInstanceAndAncestors(child *model.ProcessInstance) error {
	var settled []settledRow
	var paused []dbgen.PauseTreeRow
	err := db.withChildTx(child, lockStack, func(qtx *dbgen.Queries, held lockScope) error {
		ctx := context.Background()
		poisons := false
		if len(child.CallStack) > 0 {
			root, err := qtx.GetInstanceStatus(ctx, child.CallStack[0])
			if err != nil {
				return fmt.Errorf("read root status: %w", err)
			}
			// FailAncestors' own predicate: only the first failure finds the rest still running,
			// so later ones skip the tree lock instead of each taking it.
			switch model.Status(root) {
			case model.StatusRunning, model.StatusPausing, model.StatusPaused:
				poisons = true
			}
			if poisons && held < lockLive {
				return errWiden{lockLive}
			}
		}

		now := nowMillis()
		cols, err := db.persistState(ctx, qtx, child, now)
		if err != nil {
			return err
		}
		// The fence sits on the child's write; everything below rolls back with it.
		if err := requireFenced(qtx.UpdateInstance(ctx, updateInstanceParams(child, cols, now))); err != nil {
			return err
		}
		if len(child.CallStack) == 0 {
			return nil
		}

		idsJSON, err := json.Marshal(child.CallStack)
		if err != nil {
			return err
		}
		// Ancestors inherit the code too, so a poisoned tree filters by the failure that
		// started it.
		if err := qtx.FailAncestors(ctx, dbgen.FailAncestorsParams{
			ErrorMessage: child.ErrorMessage,
			ErrorCode:    child.ErrorCode,
			UpdatedAt:    now,
			Ids:          string(idsJSON),
		}); err != nil {
			return err
		}
		// After FailAncestors, so the stack is no longer 'running' and only the rest matches.
		if poisons {
			if paused, err = qtx.PauseTree(ctx, dbgen.PauseTreeParams{Root: child.CallStack[0], UpdatedAt: now}); err != nil {
				return fmt.Errorf("pause the rest of the tree: %w", err)
			}
		}
		settled, err = settleUp(ctx, qtx, child, now)
		return err
	})
	if err != nil {
		return err
	}
	for _, r := range paused {
		event, msg := model.EventPaused, fmt.Sprintf("paused: instance %s failed", child.ID)
		if model.Status(r.Status) == model.StatusPausing {
			event, msg = model.EventPausing, fmt.Sprintf("pause requested while a task was in flight: instance %s failed", child.ID)
		}
		db.logInstances([]string{r.ID}, event, msg, "")
	}
	db.logSettled(settled)
	return nil
}

// lockScope is how much of the tree a child's write locks. Every sibling locks its parent, so a
// scope wider than the write needs serialises a fan-out on its ancestors' rows.
type lockScope int

const (
	lockParent lockScope = iota // the child and its parent
	lockStack                   // the child and every ancestor: a settle-up may climb
	lockLive                    // every live row in the tree: a first failure pauses the rest
)

// errWiden reruns the transaction under a wider scope. It must be returned before the first
// write: persistState updates the instance's in-memory object refs, so a write cannot be replayed.
type errWiden struct{ scope lockScope }

func (errWiden) Error() string { return "lock scope too narrow" }

// withChildTx takes the scope's locks in id order — the global order shared with lockTree, or
// Postgres deadlocks — before body runs. SQLite's single writer already holds everything.
func (db *DB) withChildTx(child *model.ProcessInstance, scope lockScope, body func(*dbgen.Queries, lockScope) error) error {
	ctx := context.Background()
	if db.dialect != "postgres" {
		scope = lockLive
	}
	for {
		// The child is terminal; the ancestors this settles are not, and the max of the two is
		// the child's — one transaction takes the strongest floor it writes for (§8).
		err := db.withTxAt(ctx, instanceWriteFloor(child.Status), func(qtx *dbgen.Queries, raw dbgen.DBTX) error {
			if db.dialect == "postgres" {
				if err := db.lockForChild(ctx, raw, child, scope); err != nil {
					return err
				}
			}
			return body(qtx, scope)
		})
		var widen errWiden
		if errors.As(err, &widen) && widen.scope > scope {
			scope = widen.scope
			continue
		}
		return err
	}
}

func (db *DB) lockForChild(ctx context.Context, raw dbgen.DBTX, child *model.ProcessInstance, scope lockScope) error {
	var rows *sql.Rows
	var err error
	switch scope {
	case lockParent:
		rows, err = raw.QueryContext(ctx, `SELECT id FROM process_instances WHERE id IN (?, ?)
			ORDER BY id FOR UPDATE`, child.ID, child.ParentID)
	case lockStack:
		rows, err = raw.QueryContext(ctx, `SELECT id FROM process_instances
			WHERE id = ? OR id IN (SELECT value FROM json_each(
			                           (SELECT call_stack FROM process_instances WHERE id = ?)))
			ORDER BY id FOR UPDATE`, child.ID, child.ID)
	default:
		// Live rows only: the child and its stack are live, and every row PauseTree can write is.
		return db.lockTree(ctx, raw, `id`, `status NOT IN ('completed', 'failed', 'raised', 'cancelled')`,
			func(*sql.Rows) error { return nil }, child.CallStack[0])
	}
	if err != nil {
		return fmt.Errorf("lock rows: %w", err)
	}
	return rows.Close()
}

// settledRow is one ancestor a settle-up moved on, logged after the commit.
type settledRow struct {
	id, status, msg string
}

// settleUp climbs from a child that just stopped: each parent on 'children' is woken once its
// batch is terminal, and settled if it is draining with nothing in the batch busy. The caller
// holds the stack's locks whenever the parent is draining. specs/pause-resume.md §3.
func settleUp(ctx context.Context, qtx *dbgen.Queries, child *model.ProcessInstance, now int64) ([]settledRow, error) {
	var settled []settledRow
	parentID, spawnTask, epoch := child.ParentID, child.SpawnTaskID, int64(child.ParentTaskEpoch)
	for parentID != "" {
		p, err := qtx.GetSettleInfo(ctx, parentID)
		if errors.Is(err, sql.ErrNoRows) {
			return settled, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read parent: %w", err)
		}
		if model.Phase(p.Phase) != model.PhaseChildren {
			return settled, nil
		}
		batch := dbgen.CountActiveSiblingsParams{ParentID: parentID, SpawnTaskID: spawnTask, ParentTaskEpoch: epoch}
		active, err := qtx.CountActiveSiblings(ctx, batch)
		if err != nil {
			return nil, fmt.Errorf("count siblings: %w", err)
		}
		if active == 0 {
			if err := qtx.WakeParent(ctx, dbgen.WakeParentParams{ID: parentID, UpdatedAt: now}); err != nil {
				return nil, fmt.Errorf("wake parent: %w", err)
			}
		}
		status := model.Status(p.Status)
		if !status.Draining() {
			return settled, nil
		}
		if active > 0 {
			busy, err := qtx.CountBusySiblings(ctx, dbgen.CountBusySiblingsParams(batch))
			if err != nil {
				return nil, fmt.Errorf("count busy siblings: %w", err)
			}
			if busy > 0 {
				return settled, nil
			}
		}
		if err := qtx.SettleDrained(ctx, dbgen.SettleDrainedParams{ID: parentID, UpdatedAt: now}); err != nil {
			return nil, fmt.Errorf("settle parent: %w", err)
		}
		settled = append(settled, settledRow{id: parentID, status: string(status), msg: p.ErrorMessage})
		parentID, spawnTask, epoch = p.ParentID, p.SpawnTaskID, p.ParentTaskEpoch
	}
	return settled, nil
}

// logSettled writes what settleUp did, after its commit. A failure's settle is info, like the
// engine's own settle of a failing row; the stop that a pause or cancel completes is debug.
func (db *DB) logSettled(rows []settledRow) {
	for _, r := range rows {
		switch model.Status(r.status) {
		case model.StatusPausing:
			db.logInstances([]string{r.id}, model.EventPaused, "children settled; instance paused", "")
		case model.StatusCancelling:
			db.logInstances([]string{r.id}, model.EventCancelled, "children settled; instance cancelled", "")
		case model.StatusFailing:
			_ = db.AppendLog(&model.LogEntry{
				InstanceID: r.id, Level: model.LogInfo, Event: model.EventInstanceSettled, Message: r.msg,
			})
		}
	}
}

// inTree takes NO row locks: a mutating caller goes through lockTree, or Postgres deadlocks. The
// bound id must be a ROOT -- a child matches nothing -- hence requireRoot on every caller.
const inTree = `root_id = ?`

// forUpdate is the lock clause appended to the subtree-locking SELECT on Postgres;
// SQLite serialises via its single writer and has no FOR UPDATE syntax.
func (db *DB) forUpdate() string {
	if db.dialect == "postgres" {
		return " FOR UPDATE"
	}
	return ""
}

// lockTree locks the tree's rows matching `where` in id order — the global order every tree-wide
// verb shares, or Postgres deadlocks — and closes the cursor on EVERY path, since SQLite serves the
// caller's next UPDATE on the same connection. args fill placeholders in order, the root id last.
func (db *DB) lockTree(ctx context.Context, exec dbgen.DBTX, columns, where string, scan func(*sql.Rows) error, args ...any) error {
	rows, err := exec.QueryContext(ctx, `SELECT `+columns+` FROM process_instances WHERE `+inTree+` AND `+where+` ORDER BY id`+db.forUpdate(), args...)
	if err != nil {
		return fmt.Errorf("lock tree: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return fmt.Errorf("scan tree row: %w", err)
		}
	}
	return rows.Err()
}

// PauseProcess atomically suspends a root's tree, leaving phase, wake_at, retry_count and context
// untouched; an already-stopped tree is OutcomeUnchanged, not an error. A row with a task in flight
// at or below it goes to 'pausing' and settles bottom-up. specs/pause-resume.md, specs/id-list-commands.md.
func (db *DB) PauseProcess(ctx context.Context, id, actor string) (LifecycleResult, error) {
	return db.stopTree(ctx, id, actor, stopVerb{
		op: "pause", draining: model.StatusPausing, stopped: model.StatusPaused,
		lockWhere: `status = 'running'`,
		requested: model.EventPauseRequested, stoppedEvent: model.EventPaused, drainingEvent: model.EventPausing,
		update: func(ctx context.Context, qtx *dbgen.Queries, root string, now int64) ([]treeRow, error) {
			rows, err := qtx.PauseTree(ctx, dbgen.PauseTreeParams{Root: root, UpdatedAt: now})
			out := make([]treeRow, len(rows))
			for i, r := range rows {
				out[i] = treeRow(r)
			}
			return out, err
		},
	})
}

// refuseInTransition: an -ing root takes no new verb until it settles. A verb landing mid-way
// would race the settle that ends the transition. specs/pause-resume.md §8.
func refuseInTransition(root model.Status) error {
	settles := map[model.Status]model.Status{
		model.StatusPausing: model.StatusPaused, model.StatusFailing: model.StatusFailed,
		model.StatusCancelling: model.StatusCancelled,
	}[root]
	if settles == "" {
		return nil
	}
	return fmt.Errorf("process is %s, which is a transition: wait until it is %s, then try again: %w",
		root, settles, ErrConflict)
}

// treeRow is one row a tree-wide stop wrote, and the status it wrote.
type treeRow struct{ ID, Status string }

// stopVerb is everything that separates pause from cancel.
type stopVerb struct {
	op                string
	draining, stopped model.Status
	// lockWhere must cover every row update writes or can see become held: a claim between the
	// statement's snapshot and its write would stop a parent over a running child.
	lockWhere                              string
	requested, stoppedEvent, drainingEvent string
	update                                 func(context.Context, *dbgen.Queries, string, int64) ([]treeRow, error)
}

// stopTree is the shared body of PauseProcess and CancelProcess: one statement splits the tree
// into rows that stop now and rows that drain, under lockTree.
func (db *DB) stopTree(ctx context.Context, id, actor string, v stopVerb) (LifecycleResult, error) {
	row, err := db.loadInstanceRow(ctx, id)
	if err != nil {
		return LifecycleResult{}, err
	}
	if err := requireRoot(row, v.op); err != nil {
		return LifecycleResult{}, err
	}

	var rows []treeRow
	var rootStatus model.Status
	if err := db.withTx(ctx, func(qtx *dbgen.Queries, exec dbgen.DBTX) error {
		// SQLite's single writer already excludes every claim.
		if db.dialect == "postgres" {
			if err := db.lockTree(ctx, exec, `id`, v.lockWhere, func(*sql.Rows) error { return nil }, id); err != nil {
				return err
			}
		}
		status, err := qtx.GetInstanceStatus(ctx, id)
		if err != nil {
			return fmt.Errorf("read root status: %w", err)
		}
		rootStatus = model.Status(status)
		// A concluded tree has nothing to stop: what a failure paused waits for a retry, and a
		// cancel there would leave the retried parent waiting on cancelled children.
		if rootStatus.Terminal() {
			return nil
		}
		// Asking again for the stop under way is not a new verb; any other transition is.
		if rootStatus != v.draining {
			if err := refuseInTransition(rootStatus); err != nil {
				return err
			}
		}
		if rows, err = v.update(ctx, qtx, id, nowMillis()); err != nil {
			return fmt.Errorf("%s process: %w", v.op, err)
		}
		return nil
	}); err != nil {
		return LifecycleResult{}, err
	}

	if len(rows) == 0 {
		// Settled, stopped, or still draining, which the root's own status says now that a parent
		// waits on its descendants. An outcome, not an error -- a no-op that fails cannot converge
		// on a re-run. specs/id-list-commands.md.
		if rootStatus == v.draining {
			return db.lifecycleResult(ctx, id, model.OutcomeAccepted, 0)
		}
		return db.lifecycleResult(ctx, id, model.OutcomeUnchanged, 0)
	}
	var stopped, draining []string
	for _, r := range rows {
		if model.Status(r.Status) == v.draining {
			draining = append(draining, r.ID)
		} else {
			stopped = append(stopped, r.ID)
		}
	}
	db.logTreeAction(id, v.requested, v.op+" requested", actor,
		int64(len(rows)), map[string]any{string(v.draining): len(draining)})
	db.logInstances(stopped, v.stoppedEvent, string(v.stopped), actor)
	db.logInstances(draining, v.drainingEvent, v.op+" requested while a task at or below it was in flight", actor)

	// A draining row has only been asked to stop; reporting applied would claim the tree had
	// stopped. specs/id-list-commands.md §202.
	outcome := model.OutcomeApplied
	if len(draining) > 0 {
		outcome = model.OutcomeAccepted
	}
	return db.lifecycleResult(ctx, id, outcome, len(rows))
}

// LifecycleResult is what a tree verb did: the outcome, the root's status once committed, and
// how many rows were written. specs/id-list-commands.md.
type LifecycleResult struct {
	Outcome   model.Outcome
	Status    model.Status
	Instances int
}

// lifecycleResult re-reads the root outside the transaction, deliberately: the status reported
// is what other readers see once the write is visible.
func (db *DB) lifecycleResult(ctx context.Context, id string, outcome model.Outcome, written int) (LifecycleResult, error) {
	row, err := db.loadInstanceRow(ctx, id)
	if err != nil {
		return LifecycleResult{}, err
	}
	return LifecycleResult{Outcome: outcome, Status: model.Status(row.Status), Instances: written}, nil
}

// updateStatusIn sets status on an explicit id list the caller has already locked.
func updateStatusIn(ctx context.Context, qtx *dbgen.Queries, ids []string, status string, now int64) error {
	if len(ids) == 0 {
		return nil
	}
	idsJSON, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	return qtx.SetStatusIn(ctx, dbgen.SetStatusInParams{
		Status: status, UpdatedAt: now, Ids: string(idsJSON),
	})
}

// logTreeAction is the root's info-level entry, written after commit so a rejected call leaves
// no trace.
func (db *DB) logTreeAction(rootID, event, msg, actor string, instances int64, extra map[string]any) {
	meta := map[string]any{"instances": instances}
	for k, v := range extra {
		meta[k] = v
	}
	_ = db.AppendLog(&model.LogEntry{
		Actor:      actor,
		InstanceID: rootID,

		Level:   model.LogInfo,
		Event:   event,
		Message: fmt.Sprintf("%s (%d instance(s))", msg, instances),
		Meta:    meta,
	})
}

// logInstances writes at debug level: one call fans out over the whole tree, and the root's own
// entry is the info one.
func (db *DB) logInstances(ids []string, event, msg, actor string) {
	for _, instID := range ids {
		_ = db.AppendLog(&model.LogEntry{
			Actor:      actor,
			InstanceID: instID,
			Level:      model.LogDebug,
			Event:      event,
			Message:    msg,
		})
	}
}

// ResumeProcess atomically un-suspends a paused tree — a plain status flip, since PauseProcess
// preserved everything else. A tree still in transition is refused until it settles, and a failed
// one outright: its paused rows wait for a retry.
func (db *DB) ResumeProcess(ctx context.Context, id, actor string) (LifecycleResult, error) {
	row, err := db.loadInstanceRow(ctx, id)
	if err != nil {
		return LifecycleResult{}, err
	}
	if err := requireRoot(row, "resume"); err != nil {
		return LifecycleResult{}, err
	}

	// Collected under the same lock as PauseProcess, and for the same reason: the ids
	// are what the per-instance audit entries are keyed on.
	var resumed []string
	if err := db.withTx(ctx, func(qtx *dbgen.Queries, exec dbgen.DBTX) error {
		now := nowMillis()

		if err := db.lockTree(ctx, exec, `id`, `status = 'paused'`, func(rows *sql.Rows) error {
			var rowID string
			if err := rows.Scan(&rowID); err != nil {
				return err
			}
			resumed = append(resumed, rowID)
			return nil
		}, id); err != nil {
			return err
		}
		// Its own statement, after the lock: do not fold the root into the lock's predicate. A
		// lock re-checks only the rows its snapshot matched, so a pause committing while it waited
		// would flip the root and leave the rest paused under it.
		status, err := qtx.GetInstanceStatus(ctx, id)
		if err != nil {
			return fmt.Errorf("read root status: %w", err)
		}
		rootStatus := model.Status(status)
		if err := refuseInTransition(rootStatus); err != nil {
			return err
		}
		// Resuming these would run siblings under a parent that can no longer collect them.
		if rootStatus == model.StatusFailed && len(resumed) > 0 {
			return fmt.Errorf("process failed; retry it, which resumes its paused branches too: %w", ErrConflict)
		}

		if len(resumed) == 0 {
			// Nothing paused: still advancing (the assertion holds) or settled. Split here, under
			// the tree lock; an answer derived afterwards describes a tree that may have moved.
			if rootStatus.Terminal() {
				// Advice per status: naming retry where it refuses sends the operator to a bolted door.
				switch rootStatus {
				case model.StatusCancelled:
					return fmt.Errorf("process was cancelled and a cancel is final; "+
						"start a new instance: %w", ErrConflict)
				case model.StatusFailed:
					return fmt.Errorf("process is not paused and has failed; retry it: %w", ErrConflict)
				}
				return fmt.Errorf("process is not paused and has settled (status: %s); "+
					"start a new instance to run it again: %w", status, ErrConflict)
			}
			return nil
		}
		if err := updateStatusIn(ctx, qtx, resumed, string(model.StatusRunning), now); err != nil {
			return fmt.Errorf("resume process: %w", err)
		}

		return nil
	}); err != nil {
		return LifecycleResult{}, err
	}

	if len(resumed) == 0 {
		return db.lifecycleResult(ctx, id, model.OutcomeUnchanged, 0)
	}
	// No root-level entry to match PauseProcess's: a resume is atomic, so the
	// per-instance events already say everything a tree-level one would.
	db.logInstances(resumed, model.EventResumed, "resumed", actor)
	// Never OutcomeAccepted: a resume is a plain status flip with nothing left in flight.
	return db.lifecycleResult(ctx, id, model.OutcomeApplied, len(resumed))
}

// loadInstanceRow maps an absent row to ErrNotFound; callers outside db must not see sql.ErrNoRows.
func (db *DB) loadInstanceRow(ctx context.Context, id string) (dbgen.ProcessInstance, error) {
	row, err := db.q.GetInstance(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return row, fmt.Errorf("instance %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return row, fmt.Errorf("get instance: %w", err)
	}
	return row, nil
}

// requireRoot rejects operations on non-root instances, pointing the caller at
// the tree root (call_stack[0]) instead.
func requireRoot(row dbgen.ProcessInstance, op string) error {
	if row.ParentID == "" {
		return nil
	}
	var stack []string
	if err := json.Unmarshal([]byte(row.CallStack), &stack); err != nil || len(stack) == 0 {
		return fmt.Errorf("instance %q is not a root instance: %w", row.ID, ErrInvalid)
	}
	return fmt.Errorf("instance %q is not a root instance; %s root instance %q instead: %w", row.ID, op, stack[0], ErrInvalid)
}

// CancelProcess stops a root's tree for good: every LIVE row, paused and draining included, goes
// terminal, bottom-up past any task in flight. It must not clear external_worker_id: renew reads it
// to answer "cancelled" rather than "lost". specs/external-task-queue.md.
func (db *DB) CancelProcess(ctx context.Context, id, actor string) (LifecycleResult, error) {
	return db.stopTree(ctx, id, actor, stopVerb{
		op: "cancel", draining: model.StatusCancelling, stopped: model.StatusCancelled,
		lockWhere: `status IN ('running', 'paused')`,
		requested: model.EventCancelRequested, stoppedEvent: model.EventCancelled, drainingEvent: model.EventCancelling,
		update: func(ctx context.Context, qtx *dbgen.Queries, root string, now int64) ([]treeRow, error) {
			rows, err := qtx.CancelTree(ctx, dbgen.CancelTreeParams{Root: root, UpdatedAt: now})
			out := make([]treeRow, len(rows))
			for i, r := range rows {
				out[i] = treeRow(r)
			}
			return out, err
		},
	})
}

// RetryProcess revives a failed root's tree in place from where it died: leaves re-run their
// pending task, parents are reconstructed as waiting or collecting, completed work is never
// redone. force overrides only_once. specs/pause-resume.md.
func (db *DB) RetryProcess(ctx context.Context, id string, force bool, actor string) (LifecycleResult, error) {
	rootRow, err := db.loadInstanceRow(ctx, id)
	if err != nil {
		return LifecycleResult{}, err
	}
	if err := requireRoot(rootRow, "retry"); err != nil {
		return LifecycleResult{}, err
	}
	if status := model.Status(rootRow.Status); status != model.StatusFailed {
		if status == model.StatusPausing || status == model.StatusFailing {
			return LifecycleResult{}, refuseInTransition(status)
		}
		if status == model.StatusPaused {
			return LifecycleResult{}, fmt.Errorf("process is paused, not failed; resume it instead: %w", ErrConflict)
		}
		// Retry revives a tree whose DEFINITION ran out of attempts; an operator's stop was
		// never an attempt. specs/pause-resume.md.
		if status == model.StatusCancelled || status == model.StatusCancelling {
			return LifecycleResult{}, fmt.Errorf("process was cancelled (status: %s); a cancel is final -- "+
				"start a new instance instead: %w", status, ErrConflict)
		}
		// A raised root is settled, not interrupted: retry would re-run the task whose switch
		// DECIDED to raise, re-raising identically after possibly repeating a side effect.
		if status == model.StatusRaised {
			return LifecycleResult{}, fmt.Errorf("process concluded with error %q (status: raised); a raised error is "+
				"a declared outcome, not a fault -- start a new instance, or publish a new version "+
				"if the outcome should be handled differently: %w", rootRow.ErrorCode, ErrConflict)
		}
		return LifecycleResult{}, fmt.Errorf("process is not retryable (status: %s): %w", status, ErrConflict)
	}

	tx, qtx, exec, err := db.beginTx(ctx, nil)
	if err != nil {
		return LifecycleResult{}, err
	}
	defer tx.Rollback()

	// Load the whole tree under the lock, so concurrent pauses and child completions
	// serialize against the revival.
	nodes := make(map[string]*model.ProcessInstance)
	rawRows := make(map[string]dbgen.ProcessInstance)
	children := make(map[string]map[string][]*model.ProcessInstance) // parentID → spawnTaskID → batch
	if err := db.lockTree(ctx, exec, instanceColumns, `superseded_at IS NULL`, func(rows *sql.Rows) error {
		r, err := scanInstance(rows)
		if err != nil {
			return err
		}
		inst, err := toInstance(r)
		if err != nil {
			return err
		}
		nodes[inst.ID] = inst
		rawRows[inst.ID] = r
		if inst.ParentID != "" {
			if children[inst.ParentID] == nil {
				children[inst.ParentID] = make(map[string][]*model.ProcessInstance)
			}
			children[inst.ParentID][inst.SpawnTaskID] = append(children[inst.ParentID][inst.SpawnTaskID], inst)
		}
		return nil
	}, id); err != nil {
		return LifecycleResult{}, err
	}
	root, ok := nodes[id]
	if !ok {
		return LifecycleResult{}, fmt.Errorf("instance not found")
	}

	// Through the transaction's own connection: the pooled db.GetDefinition would deadlock on
	// the single SQLite connection this transaction holds.
	defCache := map[string]*model.ProcessDefinition{}
	loadDef := func(name string, version int) (*model.ProcessDefinition, error) {
		key := fmt.Sprintf("%s\x00%d", name, version)
		if def, ok := defCache[key]; ok {
			return def, nil
		}
		row, err := qtx.GetDefinition(ctx, dbgen.GetDefinitionParams{Name: name, Version: int64(version)})
		if err != nil {
			return nil, fmt.Errorf("load definition %s v%d: %w", name, version, err)
		}
		def := &model.ProcessDefinition{}
		if err := json.Unmarshal([]byte(row.Definition), def); err != nil {
			return nil, fmt.Errorf("decode definition %s v%d: %w", name, version, err)
		}
		defCache[key] = def
		return def, nil
	}
	loadTask := func(node *model.ProcessInstance) (*model.Task, error) {
		if node.Task == "" {
			return nil, nil
		}
		def, err := loadDef(node.ProcessName, node.ProcessVersion)
		if err != nil {
			return nil, err
		}
		for _, t := range def.Tasks {
			if t.ID == node.Task {
				return t, nil
			}
		}
		return nil, fmt.Errorf("task %q not found in %s v%d", node.Task, node.ProcessName, node.ProcessVersion)
	}

	// Top-down over the interrupted path only: the root and the front-task children of revived
	// nodes, so completed tasks and finished side branches are never touched.
	var dirty []*model.ProcessInstance
	var overriddenID string // the node whose batch holds a raised slot, marked for the engine
	var revive func(node *model.ProcessInstance) error
	revive = func(node *model.ProcessInstance) error {
		switch node.Status {
		case model.StatusCompleted, model.StatusRaised, model.StatusCancelled:
			// Settled work is kept, raised included: a raise concluded by design, at every depth.
			return nil
		case model.StatusPaused:
			// What the failure paused; resumed with the rest below, keeping its place.
			return nil
		case model.StatusRunning, model.StatusFailing, model.StatusPausing, model.StatusCancelling:
			// Unreachable: a root settles to 'failed' only once nothing below it is busy. Kept as
			// defense — a live node belongs to the engine.
			return nil
		}
		// node is failed
		newPhase := model.PhaseNone
		hasBatch := false
		if node.Task != "" {
			// Scoped to THIS batch's epoch: a loop re-entering the spawn task reuses (parent_id,
			// spawn_task_id), so an unscoped lookup hands the walk two generations.
			var kids []*model.ProcessInstance
			for _, k := range children[node.ID][node.Task] {
				if k.ParentTaskEpoch == node.TaskEpoch {
					kids = append(kids, k)
				}
			}
			if len(kids) > 0 {
				hasBatch = true
				// Interrupted inside this task's wait/collect cycle: revive the batch.
				anyActive := false
				for _, k := range kids {
					// A raised slot needs a FRESH child, whose input only the engine can
					// evaluate: mark the parent and let its next collect re-spawn (s12).
					if k.Status == model.StatusRaised {
						overriddenID = node.ID
						continue
					}
					if err := revive(k); err != nil {
						return err
					}
					if !k.Status.Terminal() {
						anyActive = true
					}
				}
				if anyActive {
					newPhase = model.PhaseChildren
				} else {
					newPhase = model.PhaseCollecting // re-run the lost collect
				}
			} else if !force {
				// Phase none re-executes the front task, so an only_once task that may already
				// have run is refused unless forced.
				front, err := loadTask(node)
				if err != nil {
					return err
				}
				if front != nil && front.OnlyOnce != nil && *front.OnlyOnce {
					return fmt.Errorf("instance %q task %q is marked only_once and may have already been attempted; use force to override: %w", node.ID, node.Task, ErrConflict)
				}
			}
		}
		// No current task: interrupted between the last task and the completed
		// write — advance() completes it on the next claim.
		node.Status = model.StatusRunning
		node.Phase = newPhase
		node.ErrorMessage = ""
		// Bump only a task re-entered from the top (an external token derives from the epoch);
		// a reconstructed batch's epoch IS its identity. internal/db/CLAUDE.md, "The task epoch".
		if !hasBatch {
			node.TaskEpoch++
		}
		// A backoff parks with RetryCount > 0 (clear wake_at so the retry runs now), a delay with
		// 0 (keep it). RetryCount itself is kept, so the revived task runs once and surfaces its
		// failure instead of grinding backoffs.
		if node.RetryCount > 0 {
			node.WakeAt = nil
		}
		dirty = append(dirty, node)
		return nil
	}
	if err := revive(root); err != nil {
		return LifecycleResult{}, err
	}

	now := nowMillis()
	// Every paused row is under the revived path, so the retried tree runs whole. A status flip,
	// as a resume: the pause kept everything else.
	var resumed []string
	for _, node := range nodes {
		if node.Status == model.StatusPaused {
			resumed = append(resumed, node.ID)
		}
	}
	if err := updateStatusIn(ctx, qtx, resumed, string(model.StatusRunning), now); err != nil {
		return LifecycleResult{}, fmt.Errorf("resume paused instances: %w", err)
	}
	for _, node := range dirty {
		// Context is preserved verbatim, so the encoded columns and their references pass
		// straight through with no claim changes. UpdateInstance never writes input_data.
		raw := rawRows[node.ID]
		// No lease held: bind the epoch read under the tree lock, where it cannot move.
		if _, err := qtx.UpdateInstance(ctx, dbgen.UpdateInstanceParams{
			ID:          node.ID,
			Task:        raw.Task,
			OutputsData: raw.OutputsData,
			OutputData:  raw.OutputData,
			// Only the REPORTED slots clear; the CAUGHT one stays: a revived node can stand on an
			// on_error-only task, analysed as always having an `error`.
			ErrorInternal: raw.ErrorInternal,
			// A revived instance has concluded nothing, so the fault it was reporting goes with the
			// status that carried it.
			ErrorData:     "",
			ExternalInput: raw.ExternalInput,
			ExternalLost:  raw.ExternalLost,
			// The one-shot override marker, when this node owns a raised batch; the engine's next
			// collect reads and clears it. specs/child-error-handling.md s12.
			EngineState: engineStateWithOverride(raw.EngineState, node.ID == overriddenID),
			// Passed through: the context is unchanged. "" would erase the declaration while the
			// claims stand.
			Objects:      raw.Objects,
			RetryCount:   int64(node.RetryCount),
			TaskEpoch:    node.TaskEpoch,
			WakeAt:       fromTimePtr(node.WakeAt),
			Status:       string(node.Status),
			Phase:        string(node.Phase),
			ErrorMessage: "",
			ErrorCode:    "",
			UpdatedAt:    now,
			LeaseEpoch:   raw.LeaseEpoch,
			// No lease held: bind worker_id as read under the tree lock too, where it
			// cannot move. NullString's zero is "", which is what an unheld row compares as.
			WorkerID: raw.WorkerID.String,
			// Carried: revival does not move Task, and this layer cannot recompute it. Unset, it
			// zeroes to "needs flush" and every revived instance pays an fsync it does not owe.
			NextReplayable: raw.NextReplayable,
		}); err != nil {
			return LifecycleResult{}, fmt.Errorf("revive instance %q: %w", node.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return LifecycleResult{}, err
	}
	db.logInstances(resumed, model.EventResumed, "resumed by retry", actor)
	// Always applied: retry is an act, not an assertion -- no prior state satisfies "was
	// given a fresh attempt", so it never reports unchanged. specs/id-list-commands.md.
	return db.lifecycleResult(ctx, id, model.OutcomeApplied, len(dirty)+len(resumed))
}

// SpawnChildrenAndWait atomically inserts child instances and transitions the parent to
// phase='children'. Children inherit the parent's current status, so a parent asked to pause
// mid-spawn spawns 'pausing' children, which settle on their first claim. Zero children is a no-op.
func (db *DB) SpawnChildrenAndWait(ctx context.Context, parent *model.ProcessInstance, children []*model.ProcessInstance) error {
	if len(children) == 0 {
		return nil
	}

	// The parent parks and the children start: nothing here is terminal, and a lost spawn
	// replays from a parent that never advanced past it (§3).
	return db.withTxAt(ctx, syncStrict, func(qtx *dbgen.Queries, raw dbgen.DBTX) error {

		var currentStatus, currentPhase string
		if err := raw.QueryRowContext(ctx,
			`SELECT status, phase FROM process_instances WHERE id = ?`+db.forUpdate(),
			parent.ID).Scan(&currentStatus, &currentPhase); err != nil {
			return fmt.Errorf("lock parent: %w", err)
		}
		if currentPhase != "" {
			return fmt.Errorf("parent %q is already in phase %q", parent.ID, currentPhase)
		}

		// Do not settle a pending pause or cancel here: the parent would stop with nothing below
		// it to land, and the ancestors draining above it would never settle.

		// created_at = now+i gives siblings a strict spawn order, which ClaimInstances
		// (ORDER BY created_at) follows.
		now := nowMillis()
		for i, child := range children {
			ts := now + int64(i)
			cols, err := db.persistState(ctx, qtx, child, ts)
			if err != nil {
				return err
			}
			params, err := insertInstanceParams(child, cols, currentStatus, ts, ts)
			if err != nil {
				return err
			}
			if err := qtx.InsertInstance(ctx, params); err != nil {
				return fmt.Errorf("insert child: %w", err)
			}
		}

		// Suspend parent: keep status, set phase='children'. The fence sits here;
		// the child inserts above roll back with it — no children without the park.
		if err := db.parkParentWaiting(ctx, qtx, parent, currentStatus, now); err != nil {
			return err
		}

		return nil
	})
}

// RespawnSlotsAndWait retires raised slots and fills them in one transaction, parking the parent
// back on 'children': a crash between retire and insert would leave a slot with no occupant.
// specs/child-error-handling.md s5.5.
func (db *DB) RespawnSlotsAndWait(ctx context.Context, parent *model.ProcessInstance, retired []string, children []*model.ProcessInstance) error {
	if len(children) == 0 {
		return nil
	}
	return db.withTxAt(ctx, syncStrict, func(qtx *dbgen.Queries, raw dbgen.DBTX) error {
		var currentStatus, currentPhase string
		if err := raw.QueryRowContext(ctx,
			`SELECT status, phase FROM process_instances WHERE id = ?`+db.forUpdate(),
			parent.ID).Scan(&currentStatus, &currentPhase); err != nil {
			return fmt.Errorf("lock parent: %w", err)
		}
		if model.Phase(currentPhase) != model.PhaseCollecting {
			return fmt.Errorf("parent %q is in phase %q, not collecting", parent.ID, currentPhase)
		}
		// Inherited as a first spawn's children inherit it.

		now := nowMillis()
		for _, id := range retired {
			if err := qtx.SupersedeInstance(ctx, dbgen.SupersedeInstanceParams{
				ID:           id,
				SupersededAt: sql.NullInt64{Int64: now, Valid: true},
			}); err != nil {
				return fmt.Errorf("supersede %q: %w", id, err)
			}
		}
		for i, child := range children {
			ts := now + int64(i)
			cols, err := db.persistState(ctx, qtx, child, ts)
			if err != nil {
				return err
			}
			// The child carries its own wake_at -- the backoff measured from the attempt it
			// replaces -- so insertInstanceParams passes it through untouched.
			params, err := insertInstanceParams(child, cols, currentStatus, ts, ts)
			if err != nil {
				return err
			}
			if err := qtx.InsertInstance(ctx, params); err != nil {
				return fmt.Errorf("insert replacement: %w", err)
			}
		}
		return db.parkParentWaiting(ctx, qtx, parent, currentStatus, now)
	})
}

// parkParentWaiting is shared by both parking primitives so the long parameter list cannot
// drift; its epoch line must never be "fixed" independently.
func (db *DB) parkParentWaiting(ctx context.Context, qtx *dbgen.Queries, parent *model.ProcessInstance, status string, now int64) error {
	parentCols, err := db.persistState(ctx, qtx, parent, now)
	if err != nil {
		return err
	}
	if err := requireFenced(qtx.UpdateInstance(ctx, dbgen.UpdateInstanceParams{
		ID:            parent.ID,
		Task:          parent.Task,
		OutputsData:   parentCols.OutputsData,
		OutputData:    parentCols.OutputData,
		ErrorInternal: parentCols.ErrorInternal,
		ErrorData:     parentCols.ErrorData,
		ExternalInput: parentCols.ExternalInput,
		ExternalLost:  boolToInt(parentCols.ExternalLost),
		EngineState:   parentCols.EngineState,
		Objects:       parentCols.Objects,
		RetryCount:    int64(parent.RetryCount),
		// Carried, never bumped: the parent is parking on the task it just spawned from,
		// and this is the epoch its collect will bind against the children.
		TaskEpoch:    parent.TaskEpoch,
		WakeAt:       sql.NullInt64{},
		Status:       status,
		Phase:        string(model.PhaseChildren),
		ErrorMessage: parent.ErrorMessage,
		ErrorCode:    parent.ErrorCode,
		UpdatedAt:    now,
		LeaseEpoch:   parent.LeaseEpoch,
		WorkerID:     fenceWorker(parent),
		// The parent is claimed again to collect; unset, this zeroes to "needs flush" and every
		// parent in a spawning tree pays an fsync on its collect claim.
		NextReplayable: boolToInt(parent.NextReplayable),
	})); err != nil {
		if errors.Is(err, ErrLeaseLost) {
			return err
		}
		return fmt.Errorf("suspend parent: %w", err)
	}
	return nil
}

// engineStateWithOverride sets the one-shot retry_override marker: the one place this package
// edits context JSON, and only a flag, since the re-spawn needs expression evaluation (the
// engine's). specs/child-error-handling.md s12.
func engineStateWithOverride(raw string, set bool) string {
	if !set {
		return raw
	}
	es := map[string]any{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &es); err != nil {
			// Unreadable bookkeeping is not this write's to repair; the collect simply runs
			// without the grant, which fails visibly rather than corrupting the column.
			return raw
		}
	}
	es["retry_override"] = true
	b, err := json.Marshal(es)
	if err != nil {
		return raw
	}
	return string(b)
}
