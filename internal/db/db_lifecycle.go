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

// FinishChild atomically saves the child as terminal and, if all siblings are now done, wakes the
// waiting parent ('collecting' if healthy, the empty phase if draining). A root only saves itself;
// failed children use FailInstanceAndAncestors instead.
func (db *DB) FinishChild(child *model.ProcessInstance) error {
	if child.ParentID == "" {
		return db.UpdateInstance(child)
	}

	ctx := context.Background()
	return db.withTxAt(ctx, instanceWriteFloor(child.Status), func(qtx *dbgen.Queries, raw dbgen.DBTX) error {

		// The global id order shared with lockTree and FailInstanceAndAncestors, or Postgres
		// deadlocks.
		var parentPhase string
		err := raw.QueryRowContext(ctx, `
		WITH locked AS (
			SELECT id, phase FROM process_instances
			WHERE id IN (?, ?)
			ORDER BY id`+db.forUpdate()+`
		)
		SELECT phase FROM locked WHERE id = ?`,
			child.ID, child.ParentID, child.ParentID).Scan(&parentPhase)
		// An absent parent is control flow, not a lookup failure — a root child, or a
		// parent already gone — so this stays sql.ErrNoRows and never becomes ErrNotFound.
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lock parent: %w", err)
		}
		parentFound := err == nil

		// The fence sits here; a refused child write rolls back the parent wake too.
		now := nowMillis()
		cols, err := db.persistState(ctx, qtx, child, now)
		if err != nil {
			return err
		}
		childParams := updateInstanceParams(child, cols, now)
		if err := requireFenced(qtx.UpdateInstance(ctx, childParams)); err != nil {
			if errors.Is(err, ErrLeaseLost) {
				return err
			}
			return fmt.Errorf("save child: %w", err)
		}

		if parentFound && model.Phase(parentPhase) == model.PhaseChildren {
			active, err := qtx.CountActiveSiblings(ctx, dbgen.CountActiveSiblingsParams{
				ParentID:        child.ParentID,
				SpawnTaskID:     child.SpawnTaskID,
				ParentTaskEpoch: child.ParentTaskEpoch,
			})
			if err != nil {
				return fmt.Errorf("count siblings: %w", err)
			}
			if active == 0 {
				if err := qtx.WakeParent(ctx, dbgen.WakeParentParams{
					ID:        child.ParentID,
					UpdatedAt: nowMillis(),
				}); err != nil {
					return fmt.Errorf("wake parent: %w", err)
				}
			}
		}

		return nil
	})
}

// FailInstanceAndAncestors atomically marks a child failed, propagates 'failing' up its call stack
// and, if it was its batch's last active member, wakes the parent to the empty phase (it is
// failing). Use it instead of UpdateInstance + FailAncestors.
func (db *DB) FailInstanceAndAncestors(child *model.ProcessInstance) error {
	ctx := context.Background()
	// The child is terminal; the ancestors this poisons are not, and the max of the two is
	// the child's — one transaction takes the strongest floor it writes for (§8).
	return db.withTxAt(ctx, instanceWriteFloor(child.Status), func(qtx *dbgen.Queries, raw dbgen.DBTX) error {
		now := nowMillis()

		// The global id order shared with lockTree and FinishChild, or Postgres deadlocks. It
		// exists only to take the locks, so SQLite skips it.
		if db.dialect == "postgres" {
			lockRows, lockErr := raw.QueryContext(ctx, `
			SELECT id FROM process_instances
			WHERE id = ?
			   OR id IN (SELECT value FROM json_each(
			                 (SELECT call_stack FROM process_instances WHERE id = ?)))
			ORDER BY id FOR UPDATE`, child.ID, child.ID)
			if lockErr != nil {
				return fmt.Errorf("lock rows: %w", lockErr)
			}
			lockRows.Close()
		}

		cols, err := db.persistState(ctx, qtx, child, now)
		if err != nil {
			return err
		}
		// The fence sits on the child's write; FailAncestors and the parent wake roll
		// back with it.
		childParams := updateInstanceParams(child, cols, now)
		if err := requireFenced(qtx.UpdateInstance(ctx, childParams)); err != nil {
			return err
		}

		if len(child.CallStack) > 0 {
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
		}

		// Mirrors FinishChild. WakeParent picks '' here: a failing parent must never collect.
		if child.ParentID != "" {
			parentPhase, err := qtx.GetPhase(ctx, child.ParentID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("read parent phase: %w", err)
			}
			if err == nil && model.Phase(parentPhase) == model.PhaseChildren {
				active, err := qtx.CountActiveSiblings(ctx, dbgen.CountActiveSiblingsParams{
					ParentID:        child.ParentID,
					SpawnTaskID:     child.SpawnTaskID,
					ParentTaskEpoch: child.ParentTaskEpoch,
				})
				if err != nil {
					return fmt.Errorf("count siblings: %w", err)
				}
				if active == 0 {
					if err := qtx.WakeParent(ctx, dbgen.WakeParentParams{
						ID:        child.ParentID,
						UpdatedAt: now,
					}); err != nil {
						return fmt.Errorf("wake parent: %w", err)
					}
				}
			}
		}

		return nil
	})
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

// heldColumns: whether a worker is on the row, lapsed lease or not. Such a row only records the
// request: its owner's write settles it, or a new claim bumps the epoch, fencing that write, and settles it.
const heldColumns = `id, CASE WHEN worker_id IS NOT NULL THEN 1 ELSE 0 END AS held`

// scanHeld sorts each row into settled or leased by its held flag.
func scanHeld(settled, leased *[]string) func(*sql.Rows) error {
	return func(rows *sql.Rows) error {
		var id string
		var held int
		if err := rows.Scan(&id, &held); err != nil {
			return err
		}
		if held == 1 {
			*leased = append(*leased, id)
		} else {
			*settled = append(*settled, id)
		}
		return nil
	}
}

// PauseProcess atomically suspends a root's tree, leaving phase, wake_at, retry_count and context
// untouched; an already-stopped tree is OutcomeUnchanged, not an error. Only a *leased* row goes
// to 'pausing'. specs/pause-resume.md, specs/id-list-commands.md.
func (db *DB) PauseProcess(ctx context.Context, id, actor string) (LifecycleResult, error) {
	row, err := db.loadInstanceRow(ctx, id)
	if err != nil {
		return LifecycleResult{}, err
	}
	if err := requireRoot(row, "pause"); err != nil {
		return LifecycleResult{}, err
	}

	// settled reached 'paused' in this call; leased is left draining in 'pausing'. Logged as
	// different events: a leased row has only been asked to stop.
	var settled, leased []string
	// Rows a previous pause left mid-task. Nothing to write for them, but they are the
	// difference between a tree that has stopped and one still draining.
	var draining int64
	if err := db.withTx(ctx, func(qtx *dbgen.Queries, exec dbgen.DBTX) error {
		now := nowMillis()

		// Selecting rather than blind-updating yields the per-instance outcome the audit trail
		// needs, which a row count cannot express.
		if err := db.lockTree(ctx, exec, heldColumns, `status = 'running'`, scanHeld(&settled, &leased), id); err != nil {
			return err
		}

		// Nothing running: settled, paused, or still draining. An outcome, not an error -- a no-op
		// that fails cannot converge on a re-run. specs/id-list-commands.md.
		if len(settled)+len(leased) == 0 {
			// The selector matches 'running' only, so without this a second pause would report a
			// draining tree as stopped while a worker is still inside a task.
			n, err := qtx.CountDrainingInTree(ctx, dbgen.CountDrainingInTreeParams{
				Root: id, Draining: string(model.StatusPausing),
			})
			if err != nil {
				return fmt.Errorf("count draining instances: %w", err)
			}
			draining = n
			return nil
		}

		if err := updateStatusIn(ctx, qtx, settled, string(model.StatusPaused), now); err != nil {
			return fmt.Errorf("pause process: %w", err)
		}
		if err := updateStatusIn(ctx, qtx, leased, string(model.StatusPausing), now); err != nil {
			return fmt.Errorf("pause process: %w", err)
		}

		return nil
	}); err != nil {
		return LifecycleResult{}, err
	}

	written := len(settled) + len(leased)
	if written == 0 {
		if draining > 0 {
			return db.lifecycleResult(ctx, id, model.OutcomeAccepted, 0)
		}
		return db.lifecycleResult(ctx, id, model.OutcomeUnchanged, 0)
	}
	db.logTreeAction(id, model.EventPauseRequested, "pause requested", actor,
		int64(written), map[string]any{"pausing": len(leased)})
	db.logInstances(settled, model.EventPaused, "paused", actor)
	db.logInstances(leased, model.EventPausing, "pause requested while a task was in flight", actor)

	// A leased row has only been asked to stop; reporting applied would claim the tree had
	// stopped. specs/id-list-commands.md §202.
	outcome := model.OutcomeApplied
	if len(leased) > 0 {
		outcome = model.OutcomeAccepted
	}
	return db.lifecycleResult(ctx, id, outcome, written)
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
// preserved everything else. 'pausing' rows are included, so a resume before a pause landed
// un-requests it. Keyed on the subtree, not the root: a failing root over paused rows resumes.
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

		if err := db.lockTree(ctx, exec, `id`, `status IN ('paused', 'pausing')`, func(rows *sql.Rows) error {
			var rowID string
			if err := rows.Scan(&rowID); err != nil {
				return err
			}
			resumed = append(resumed, rowID)
			return nil
		}, id); err != nil {
			return err
		}

		if len(resumed) == 0 {
			// Nothing paused: still advancing (the assertion holds) or settled. Split here, under
			// the tree lock; an answer derived afterwards describes a tree that may have moved.
			status, err := qtx.GetInstanceStatus(ctx, id)
			if err != nil {
				return fmt.Errorf("read root status: %w", err)
			}
			if model.Status(status).Terminal() {
				// A cancelled tree gets its own advice: retry refuses it by design, so naming
				// retry here would send the operator to the one door that is bolted.
				if model.Status(status) == model.StatusCancelled {
					return fmt.Errorf("process was cancelled and a cancel is final; "+
						"start a new instance: %w", ErrConflict)
				}
				return fmt.Errorf("process is not paused and has settled (status: %s); "+
					"retry it or start a new instance: %w", status, ErrConflict)
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
// terminal. It must not clear external_worker_id: a cleared id renews as "lost", which stops the
// worker WITHOUT releasing. specs/external-task-queue.md.
func (db *DB) CancelProcess(ctx context.Context, id, actor string) (LifecycleResult, error) {
	row, err := db.loadInstanceRow(ctx, id)
	if err != nil {
		return LifecycleResult{}, err
	}
	if err := requireRoot(row, "cancel"); err != nil {
		return LifecycleResult{}, err
	}

	// settled reached 'cancelled' in this call; leased is left draining in 'cancelling'.
	var settled, leased []string
	var draining int64
	if err := db.withTx(ctx, func(qtx *dbgen.Queries, exec dbgen.DBTX) error {
		now := nowMillis()

		if err := db.lockTree(ctx, exec, heldColumns, `status IN ('running', 'failing', 'pausing', 'paused')`,
			scanHeld(&settled, &leased), id); err != nil {
			return err
		}

		if len(settled)+len(leased) == 0 {
			// 'cancelling' is outside the selector above, so without this a second cancel on a
			// draining tree would report it stopped while a worker was still inside a task.
			n, err := qtx.CountDrainingInTree(ctx, dbgen.CountDrainingInTreeParams{
				Root: id, Draining: string(model.StatusCancelling),
			})
			if err != nil {
				return fmt.Errorf("count draining instances: %w", err)
			}
			draining = n
			return nil
		}

		if err := updateStatusIn(ctx, qtx, settled, string(model.StatusCancelled), now); err != nil {
			return fmt.Errorf("cancel process: %w", err)
		}
		if err := updateStatusIn(ctx, qtx, leased, string(model.StatusCancelling), now); err != nil {
			return fmt.Errorf("cancel process: %w", err)
		}
		return nil
	}); err != nil {
		return LifecycleResult{}, err
	}

	written := len(settled) + len(leased)
	if written == 0 {
		if draining > 0 {
			return db.lifecycleResult(ctx, id, model.OutcomeAccepted, 0)
		}
		return db.lifecycleResult(ctx, id, model.OutcomeUnchanged, 0)
	}
	db.logTreeAction(id, model.EventCancelRequested, "cancel requested", actor,
		int64(written), map[string]any{"cancelling": len(leased)})
	db.logInstances(settled, model.EventCancelled, "cancelled", actor)
	db.logInstances(leased, model.EventCancelling, "cancel requested while a task was in flight", actor)

	// A leased row has been asked to stop, not stopped: the tree still has a task running
	// until that worker's write lands. specs/id-list-commands.md s202.
	outcome := model.OutcomeApplied
	if len(leased) > 0 {
		outcome = model.OutcomeAccepted
	}
	return db.lifecycleResult(ctx, id, outcome, written)
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
		if status == model.StatusPaused || status == model.StatusPausing {
			return LifecycleResult{}, fmt.Errorf("process is paused, not failed (status: %s); resume it instead: %w", status, ErrConflict)
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
		case model.StatusRunning, model.StatusFailing, model.StatusPausing, model.StatusPaused,
			model.StatusCancelling:
			// Unreachable under a failed root (paused children count as active, so the tree stays
			// 'failing'); kept as defense — a live node belongs to the engine or ResumeProcess.
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
	// Always applied: retry is an act, not an assertion -- no prior state satisfies "was
	// given a fresh attempt", so it never reports unchanged. specs/id-list-commands.md.
	return db.lifecycleResult(ctx, id, model.OutcomeApplied, len(dirty))
}

// SpawnChildrenAndWait atomically inserts child instances and transitions the parent to
// phase='children'. Children inherit the parent's current status, so a concurrently-paused
// parent spawns paused children. Zero children is a no-op.
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

		// A pause or cancel that landed mid-spawn settles here, and the children inherit it, so a
		// stopped tree spawns nothing that waits for a worker.
		currentStatus = settledAtSpawn(currentStatus)

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

// settledAtSpawn lands a draining stop: the spawn's park is the parent's last write before its
// children, so a pending pause or cancel settles there or the whole tree waits on a claim.
func settledAtSpawn(status string) string {
	switch model.Status(status) {
	case model.StatusPausing:
		return string(model.StatusPaused)
	case model.StatusCancelling:
		return string(model.StatusCancelled)
	}
	return status
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
		// Same landing as a first spawn.
		currentStatus = settledAtSpawn(currentStatus)

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
