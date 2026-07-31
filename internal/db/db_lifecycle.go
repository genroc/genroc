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

// FinishChild atomically saves the child as terminal and, if all siblings are now done,
// wakes the waiting parent — to 'collecting' if healthy (outputs will be merged, whether
// it is running or paused), to ” if draining ('failing'; just settles). The parent row is
// locked first (FOR UPDATE on PostgreSQL; SQLite single-writer) to serialize concurrent
// sibling completions — the same lock order as PauseProcess/FailInstanceAndAncestors.
// Root instances (no parent) only save the child; failed children use FailAncestors instead.
func (db *DB) FinishChild(child *model.ProcessInstance) error {
	if child.ParentID == "" {
		return db.UpdateInstance(child)
	}

	ctx := context.Background()
	return db.withTx(ctx, func(qtx *dbgen.Queries, raw dbgen.DBTX) error {

		// Acquire row locks (oldest-first) and read the parent's wait_state in one shot.
		// The locking CTE keeps the same lock order as PauseProcess and
		// FailInstanceAndAncestors, preventing deadlocks; the FOR UPDATE is appended only
		// on PostgreSQL — SQLite serialises via its single writer and runs the CTE without it.
		var parentWaitState string
		err := raw.QueryRowContext(ctx, `
		WITH locked AS (
			SELECT id, wait_state FROM process_instances
			WHERE id IN (?, ?)
			ORDER BY id`+db.forUpdate()+`
		)
		SELECT wait_state FROM locked WHERE id = ?`,
			child.ID, child.ParentID, child.ParentID).Scan(&parentWaitState)
		// An absent parent is control flow, not a lookup failure — a root child, or a
		// parent already gone — so this stays sql.ErrNoRows and never becomes ErrNotFound.
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lock parent: %w", err)
		}
		parentFound := err == nil

		// Save child as terminal.
		now := nowMillis()
		cols, err := db.persistContext(ctx, qtx, child, now)
		if err != nil {
			return err
		}
		childParams := updateInstanceParams(child, cols, now)
		if err := qtx.UpdateInstance(ctx, childParams); err != nil {
			return fmt.Errorf("save child: %w", err)
		}

		// If the parent was found and is waiting, check whether all siblings are terminal.
		if parentFound && model.WaitState(parentWaitState) == model.WaitStateWaiting {
			active, err := qtx.CountActiveSiblings(ctx, dbgen.CountActiveSiblingsParams{
				ParentID:    child.ParentID,
				SpawnTaskID: child.SpawnTaskID,
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

// FailInstanceAndAncestors atomically marks a child failed, propagates 'failing' to all
// ancestors in its call stack, and — when the child was the last active member of its
// spawn batch — wakes the parent (to ”, it is failing by then) so the engine settles it
// next tick. One transaction; the safe replacement for UpdateInstance + FailAncestors.
func (db *DB) FailInstanceAndAncestors(child *model.ProcessInstance) error {
	ctx := context.Background()
	return db.withTx(ctx, func(qtx *dbgen.Queries, raw dbgen.DBTX) error {
		now := nowMillis()

		// Lock child and all ancestors oldest-first — consistent with FinishChild and
		// PauseProcess. This task exists only to take FOR UPDATE locks, so it runs on
		// PostgreSQL alone; SQLite serialises via its single writer. Ancestors are read
		// from the child's call_stack in the DB; no Go-side ID list needed.
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

		cols, err := db.persistContext(ctx, qtx, child, now)
		if err != nil {
			return err
		}
		childParams := updateInstanceParams(child, cols, now)
		if err := qtx.UpdateInstance(ctx, childParams); err != nil {
			return err
		}

		// Bulk-mark all ancestors as failing in a single UPDATE via json_each.
		if len(child.CallStack) > 0 {
			idsJSON, err := json.Marshal(child.CallStack)
			if err != nil {
				return err
			}
			// Ancestors inherit the child's code as well as its message: a poisoned tree
			// then filters by the code of the failure that actually started it, not just
			// at the single instance that observed it.
			if err := qtx.FailAncestors(ctx, dbgen.FailAncestorsParams{
				Error:     child.Error,
				ErrorCode: child.ErrorCode,
				UpdatedAt: now,
				Ids:       string(idsJSON),
			}); err != nil {
				return err
			}
		}

		// If this failure settled the last active child of the batch, wake the
		// waiting parent (mirrors FinishChild) so the engine can claim it and
		// transition failing → failed. WakeParent picks '' here — the parent is
		// failing, so it must never enter the collect phase.
		if child.ParentID != "" {
			parentWaitState, err := qtx.GetWaitState(ctx, child.ParentID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("read parent wait_state: %w", err)
			}
			if err == nil && model.WaitState(parentWaitState) == model.WaitStateWaiting {
				active, err := qtx.CountActiveSiblings(ctx, dbgen.CountActiveSiblingsParams{
					ParentID:    child.ParentID,
					SpawnTaskID: child.SpawnTaskID,
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

// subtreeCTE is a `WITH RECURSIVE subtree(id) AS (...)` clause binding the CTE
// `subtree` to the instance with the bound id and every descendant, by walking
// process_instances.parent_id down (riding idx_instances_parent_task, indexed on
// both engines).
//
// The CTE is a plain SELECT and takes no row locks, so it can't deadlock. Callers
// that mutate the tree lock the enumerated rows in a SEPARATE task,
// ORDER BY id FOR UPDATE — the global order shared with FinishChild
// and FailInstanceAndAncestors, which is what prevents deadlocks. (Postgres also
// forbids FOR UPDATE inside a recursive CTE, so this split is mandatory.)
const subtreeCTE = `WITH RECURSIVE subtree(id) AS (
	SELECT id FROM process_instances WHERE id = ?
	UNION ALL
	SELECT pi.id FROM process_instances pi JOIN subtree s ON pi.parent_id = s.id
)`

// forUpdate is the lock clause appended to the subtree-locking SELECT on Postgres;
// SQLite serialises via its single writer and has no FOR UPDATE syntax.
func (db *DB) forUpdate() string {
	if db.dialect == "postgres" {
		return " FOR UPDATE"
	}
	return ""
}

// PauseProcess atomically suspends an entire process tree (root + every running
// descendant). Pausing means only "stop advancing automatically": wait_state, wake_at,
// retry_count and context are left untouched, which is what lets ResumeProcess be a
// plain status flip instead of the revival RetryProcess performs.
//
// A row lands in 'pausing' only if it is currently leased, i.e. a worker is mid-task on
// it; the pause becomes 'paused' when that task's write releases the lease (the CASE in
// UpdateInstance/UpdateInstanceProgress). Everything else -- parked on children, on a
// delay, on an external task -- has nothing in flight and goes straight to 'paused'.
// That distinction is load-bearing, not cosmetic: an instance parked with
// wait_state='waiting' is excluded from ClaimInstances, so if it were marked 'pausing'
// no claim could ever settle it and it would hang in the draining state forever.
//
// Root-only: suspending is a decision about the whole tree, so a descendant id is
// rejected with the root's. The tree is enumerated by a recursive parent_id walk
// (subtreeCTE), then the locking CTE takes every row lock in id order -- the same order
// as FinishChild/FailInstanceAndAncestors -- to avoid PostgreSQL deadlocks (SQLite
// single-writer, empty lock clause).
func (db *DB) PauseProcess(ctx context.Context, id string) error {
	row, err := db.loadInstanceRow(ctx, id)
	if err != nil {
		return err
	}
	if err := requireRoot(row, "pause"); err != nil {
		return err
	}

	// settled is the ids that reached 'paused' in this call; leased is those left draining
	// in 'pausing'. They are logged as different events because they are different facts:
	// a leased row is not paused yet, it has only been asked to stop.
	var settled, leased []string
	if err := db.withTx(ctx, func(_ *dbgen.Queries, exec dbgen.DBTX) error {
		now := nowMillis()

		// Lock the rows this call will mutate, in id order -- the global order shared
		// with FinishChild/FailInstanceAndAncestors that prevents deadlocks (Postgres;
		// on SQLite the ORDER BY is a harmless no-op under the single writer). Selecting
		// rather than blind-updating also yields the exact per-instance outcome, which
		// the UPDATE's row count alone cannot express and the audit trail needs.
		rows, err := exec.QueryContext(ctx, subtreeCTE+`
		SELECT id, CASE WHEN worker_id IS NOT NULL AND lease_expires_at > ?
		                THEN 1 ELSE 0 END AS held
		FROM process_instances
		WHERE id IN (SELECT id FROM subtree) AND status = 'running'
		ORDER BY id`+db.forUpdate(), id, now)
		if err != nil {
			return fmt.Errorf("lock tree: %w", err)
		}
		for rows.Next() {
			var rowID string
			var held int
			if err := rows.Scan(&rowID, &held); err != nil {
				rows.Close()
				return fmt.Errorf("scan tree row: %w", err)
			}
			if held == 1 {
				leased = append(leased, rowID)
			} else {
				settled = append(settled, rowID)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close() // release the cursor before the UPDATE (SQLite single connection)

		// Nothing running anywhere in the tree: the process has already settled (or is
		// already paused). Report it rather than silently succeeding.
		if len(settled)+len(leased) == 0 {
			return fmt.Errorf("process has no running instances to pause (status: %s): %w", row.Status, ErrConflict)
		}

		// A worker mid-task cannot be stopped, so a leased row only records the request
		// ('pausing') and lands in 'paused' on the write that finishes its task.
		if err := updateStatusIn(ctx, exec, settled, string(model.StatusPaused), now); err != nil {
			return fmt.Errorf("pause process: %w", err)
		}
		if err := updateStatusIn(ctx, exec, leased, string(model.StatusPausing), now); err != nil {
			return fmt.Errorf("pause process: %w", err)
		}

		return nil
	}); err != nil {
		return err
	}

	db.logTreeAction(id, model.EventPauseRequested, "pause requested",
		int64(len(settled)+len(leased)), map[string]any{"pausing": len(leased)})
	db.logInstances(settled, model.EventPaused, "paused")
	db.logInstances(leased, model.EventPausing, "pause requested while a task was in flight")
	return nil
}

// updateStatusIn sets status on an explicit id list (already locked by the caller),
// binding the ids as a JSON array through json_each -- the same dynamic-IN pattern as
// FailAncestors, and the reason no dialect branch is needed here.
func updateStatusIn(ctx context.Context, exec dbgen.DBTX, ids []string, status string, now int64) error {
	if len(ids) == 0 {
		return nil
	}
	idsJSON, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	_, err = exec.ExecContext(ctx,
		`UPDATE process_instances SET status = ?, updated_at = ?
		 WHERE id IN (SELECT value FROM json_each(?))`,
		status, now, string(idsJSON))
	return err
}

// logTreeAction records an operator's pause on the tree root, at info level: it is the
// human-facing line, and the counts are what make it useful — how much of the tree was
// live, and how much of that could not be stopped mid-task. Written after the transaction
// commits, so a rejected or rolled-back call leaves no trace. Best-effort like every
// audit write: a logging failure must not fail the action the user already committed.
func (db *DB) logTreeAction(rootID, event, msg string, instances int64, extra map[string]any) {
	meta := map[string]any{"instances": instances}
	for k, v := range extra {
		meta[k] = v
	}
	_ = db.AppendLog(&model.LogEntry{
		InstanceID: rootID,
		Level:      model.LogInfo,
		Event:      event,
		Message:    fmt.Sprintf("%s (%d instance(s))", msg, instances),
		Meta:       meta,
	})
}

// logInstances records the per-instance consequence of a tree-wide pause/resume, at debug
// level: one call fans out over the whole subtree, so this is high-volume detail in the
// same class as the action_* events, filtered out of the default view.
func (db *DB) logInstances(ids []string, event, msg string) {
	for _, instID := range ids {
		_ = db.AppendLog(&model.LogEntry{
			InstanceID: instID,
			Level:      model.LogDebug,
			Event:      event,
			Message:    msg,
		})
	}
}

// ResumeProcess atomically un-suspends a paused process tree. Because PauseProcess
// preserves every other field, this is the whole operation: no revival walk, no
// wait_state reconstruction, no only_once question -- the instances resume exactly
// where they were, including any delay or retry-backoff timer, which kept running
// while they were paused.
//
// 'pausing' rows are included so a resume issued before a pause finished landing
// simply un-requests it. Root-only, same lock order as PauseProcess.
//
// The precondition is on the subtree, not on the root's own status, because the two
// can legitimately disagree: if one branch dies while the tree is paused, its ancestors
// go 'failing' but cannot settle -- a failing parent waits for every child, and paused
// children count as active. That leaves a failing root over paused descendants, and
// resuming is exactly how the operator unblocks it (the branches drain, the tree
// settles to failed, and it becomes retryable).
func (db *DB) ResumeProcess(ctx context.Context, id string) error {
	row, err := db.loadInstanceRow(ctx, id)
	if err != nil {
		return err
	}
	if err := requireRoot(row, "resume"); err != nil {
		return err
	}

	// Collected under the same lock as PauseProcess, and for the same reason: the ids
	// are what the per-instance audit entries are keyed on.
	var resumed []string
	if err := db.withTx(ctx, func(_ *dbgen.Queries, exec dbgen.DBTX) error {
		now := nowMillis()

		rows, err := exec.QueryContext(ctx, subtreeCTE+`
		SELECT id FROM process_instances
		WHERE id IN (SELECT id FROM subtree) AND status IN ('paused', 'pausing')
		ORDER BY id`+db.forUpdate(), id)
		if err != nil {
			return fmt.Errorf("lock tree: %w", err)
		}
		for rows.Next() {
			var rowID string
			if err := rows.Scan(&rowID); err != nil {
				rows.Close()
				return fmt.Errorf("scan tree row: %w", err)
			}
			resumed = append(resumed, rowID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close() // release the cursor before the UPDATE (SQLite single connection)

		if len(resumed) == 0 {
			return fmt.Errorf("process is not paused (status: %s): %w", row.Status, ErrConflict)
		}
		if err := updateStatusIn(ctx, exec, resumed, string(model.StatusRunning), now); err != nil {
			return fmt.Errorf("resume process: %w", err)
		}

		return nil
	}); err != nil {
		return err
	}

	// No root-level entry to match PauseProcess's: a resume is atomic, so the
	// per-instance events already say everything a tree-level one would.
	db.logInstances(resumed, model.EventResumed, "resumed")
	return nil
}

// loadInstanceRow reads the row a tree-wide operation (pause/resume/retry) was asked
// to act on. It exists so an absent instance comes back as ErrNotFound: the generated
// db.q.GetInstance returns a bare sql.ErrNoRows, which callers above the db package
// have no business inspecting.
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

// RetryProcess revives a failed root process from where its tree died: failed nodes on
// the current path are revived in place (leaves re-run their pending task; parents
// reconstructed as waiting for live children or collecting when all are done), while
// completed work is never redone. force overrides only_once protection on pending tasks.
// Root-only — a terminal root implies the whole tree has settled; draining roots are
// rejected by the status check.
//
// Failed-only by design. Retrying is an override of the definition: the tree already
// spent the on_error budget its author configured, and this hands it another attempt
// (with force, one that skips only_once protection too). Un-suspending a paused process
// grants nothing and is ResumeProcess, a plain status flip.
func (db *DB) RetryProcess(ctx context.Context, id string, force bool) error {
	rootRow, err := db.loadInstanceRow(ctx, id)
	if err != nil {
		return err
	}
	if err := requireRoot(rootRow, "retry"); err != nil {
		return err
	}
	if status := model.Status(rootRow.Status); status != model.StatusFailed {
		if status == model.StatusPaused || status == model.StatusPausing {
			return fmt.Errorf("process is paused, not failed (status: %s); resume it instead: %w", status, ErrConflict)
		}
		// A raised root is settled, not interrupted, so retry has nothing to re-attempt:
		// it can only re-run the task the instance sits on, and that task is the one
		// whose switch *decided* to raise -- against upstream state a retry cannot have
		// changed. It would re-raise identically, after possibly re-running a side effect
		// that already succeeded. Both special cases exist because "not retryable" alone
		// invites a bug report; the difference is that pause has another verb to point at
		// and a raise does not.
		if status == model.StatusRaised {
			return fmt.Errorf("process concluded with error %q (status: raised); a raised error is "+
				"a declared outcome, not a fault -- start a new instance, or publish a new version "+
				"if the outcome should be handled differently: %w", rootRow.ErrorCode, ErrConflict)
		}
		return fmt.Errorf("process is not retryable (status: %s): %w", status, ErrConflict)
	}

	tx, qtx, exec, err := db.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Lock and load the whole tree (root + descendants) in id order —
	// the same lock order as PauseProcess/FinishChild/FailInstanceAndAncestors —
	// so concurrent pauses and child completions serialize against the revival.
	// The tree is enumerated with a recursive walk over parent_id (subtreeCTE); the
	// FOR UPDATE on the outer SELECT (Postgres only) locks the rows in that order.
	rows, err := exec.QueryContext(ctx, subtreeCTE+`
		SELECT `+instanceColumns+` FROM process_instances
		WHERE id IN (SELECT id FROM subtree)
		ORDER BY id`+db.forUpdate(), id)
	if err != nil {
		return fmt.Errorf("lock tree: %w", err)
	}
	defer rows.Close()

	nodes := make(map[string]*model.ProcessInstance)
	rawRows := make(map[string]dbgen.ProcessInstance)
	children := make(map[string]map[string][]*model.ProcessInstance) // parentID → spawnTaskID → batch
	for rows.Next() {
		r, err := scanInstance(rows)
		if err != nil {
			return fmt.Errorf("scan tree row: %w", err)
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
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close() // release the connection for the updates below (SQLite single-conn)
	root, ok := nodes[id]
	if !ok {
		return fmt.Errorf("instance not found")
	}

	// loadTask resolves a node's current task object via the transaction's own
	// connection. The pooled db.GetDefinition must NOT be used here: this runs inside
	// the tree-locking transaction, which on SQLite holds the single pooled connection
	// db.GetDefinition would block waiting for — a deadlock. Definitions are immutable,
	// so a per-call cache keyed by name+version avoids re-reading shared defs.
	defCache := map[string]*model.ProcessDefinition{}
	loadTask := func(node *model.ProcessInstance) (*model.Task, error) {
		if node.Task == "" {
			return nil, nil
		}
		key := fmt.Sprintf("%s\x00%d", node.ProcessName, node.ProcessVersion)
		def, ok := defCache[key]
		if !ok {
			row, err := qtx.GetDefinition(ctx, dbgen.GetDefinitionParams{Name: node.ProcessName, Version: int64(node.ProcessVersion)})
			if err != nil {
				return nil, fmt.Errorf("load definition for %q: %w", node.ID, err)
			}
			def = &model.ProcessDefinition{}
			if err := json.Unmarshal([]byte(row.Definition), def); err != nil {
				return nil, fmt.Errorf("decode definition for %q: %w", node.ID, err)
			}
			defCache[key] = def
		}
		for _, t := range def.Tasks {
			if t.ID == node.Task {
				return t, nil
			}
		}
		return nil, fmt.Errorf("task %q not found in %s v%d", node.Task, node.ProcessName, node.ProcessVersion)
	}

	// Walk the tree top-down, reviving the interrupted path. Only the root and
	// the front-task children of revived nodes are visited, so completed tasks
	// and finished side branches are never touched.
	var dirty []*model.ProcessInstance
	var revive func(node *model.ProcessInstance) error
	revive = func(node *model.ProcessInstance) error {
		switch node.Status {
		case model.StatusCompleted, model.StatusRaised:
			// Settled work is kept. A raised node was not interrupted -- it concluded, by
			// design, with a declared outcome -- so it belongs here and not below: the
			// status means the same thing at every depth, root or leaf.
			//
			// This does leave one case retry cannot help: a parent that failed at
			// resolution because no rule matched a child's raised code re-resolves to the
			// same unmatched code and fails identically. Reviving the child would not have
			// helped either, since the missing on_error rule lives in a version-pinned
			// definition a retry does not re-read. The fix is a new parent version.
			return nil
		case model.StatusRunning, model.StatusFailing, model.StatusPausing, model.StatusPaused:
			// Unreachable under a failed root: it settles only once every child is
			// terminal, and paused children count as active (CountActiveSiblings), so
			// a tree holding one stays 'failing'. Kept as defense — a live or
			// suspended node belongs to the engine, or to ResumeProcess.
			return nil
		}
		// node is failed
		newWaitState := model.WaitStateNone
		if node.Task != "" {
			kids := children[node.ID][node.Task]
			if len(kids) > 0 {
				// Interrupted inside this spawn task's wait/collect cycle —
				// revive the batch and reconstruct the wait state. (Kids exist
				// only for spawn tasks: SpawnChildrenAndWait is atomic.)
				anyActive := false
				for _, k := range kids {
					if err := revive(k); err != nil {
						return err
					}
					if !k.Status.Terminal() {
						anyActive = true
					}
				}
				if anyActive {
					newWaitState = model.WaitStateWaiting
				} else {
					newWaitState = model.WaitStateCollecting // re-run the lost collect
				}
			} else if !force {
				// Reviving with wait_state none re-executes the front task, so a
				// only_once task that may already have run is rejected unless forced.
				// (force skips the lookup — it overrides the check regardless.)
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
		node.WaitState = newWaitState
		node.Error = ""
		// Keep RetryCount (don't reset): a manual retry is a human-in-the-loop action,
		// so the failing task — its on_error budget already spent — runs once and
		// surfaces any failure immediately instead of grinding through more backoffs the
		// operator can't observe. They diagnose, fix, and retry again.
		//
		// RetryCount also tells the two timer kinds apart: a retry-backoff parks with
		// RetryCount > 0 (clear it so the retry runs now); a delay parks with
		// RetryCount == 0 (keep wake_at so the delay resumes toward its deadline).
		if node.RetryCount > 0 {
			node.WakeAt = nil
		}
		dirty = append(dirty, node)
		return nil
	}
	if err := revive(root); err != nil {
		return err
	}

	now := nowMillis()
	for _, node := range dirty {
		// Retry preserves the instance's context verbatim, so the already-encoded
		// column strings (and the objects they reference) pass straight through —
		// references are unchanged, so no object pin/deref is needed. input_data is
		// immutable and not written by UpdateInstance.
		raw := rawRows[node.ID]
		if err := qtx.UpdateInstance(ctx, dbgen.UpdateInstanceParams{
			ID:          node.ID,
			Task:        raw.Task,
			OutputsData: raw.OutputsData,
			OutputData:  raw.OutputData,
			// The three error slots are cleared together, and must stay that way. Leaving
			// error_code behind fails quietly: a revived instance that then completes
			// still reports having died of the old code, corrupting exactly the column the
			// code exists to serve. error_data ($error) is cleared for the same reason --
			// a batch error route writes it, and a stale one would survive the revival.
			// Nothing valid can read a cleared $error: the mustErr/mayErr analysis only
			// admits a read where an error is present on every path reaching it.
			ErrorData:    "",
			ExternalData: raw.ExternalData,
			EngineState:  raw.EngineState,
			RetryCount:   int64(node.RetryCount),
			WakeAt:       fromTimePtr(node.WakeAt),
			Status:       string(node.Status),
			WaitState:    string(node.WaitState),
			Error:        "",
			ErrorCode:    "",
			UpdatedAt:    now,
		}); err != nil {
			return fmt.Errorf("revive instance %q: %w", node.ID, err)
		}
	}

	return tx.Commit()
}

// SpawnChildrenAndWait atomically inserts child instances and transitions the parent to
// wait_state='waiting'. Children inherit the parent's current status, so a
// concurrently-paused parent spawns paused children rather than work that would run on
// behalf of a suspended tree. Zero children is a no-op (the parent does not enter the
// wait state).
func (db *DB) SpawnChildrenAndWait(ctx context.Context, parent *model.ProcessInstance, children []*model.ProcessInstance) error {
	if len(children) == 0 {
		return nil
	}

	return db.withTx(ctx, func(qtx *dbgen.Queries, raw dbgen.DBTX) error {

		// Lock parent and read its current status to propagate to children. FOR UPDATE
		// is appended only on PostgreSQL; SQLite serialises via its single writer.
		var currentStatus, currentWaitState string
		if err := raw.QueryRowContext(ctx,
			`SELECT status, wait_state FROM process_instances WHERE id = ?`+db.forUpdate(),
			parent.ID).Scan(&currentStatus, &currentWaitState); err != nil {
			return fmt.Errorf("lock parent: %w", err)
		}
		if currentWaitState != "" {
			return fmt.Errorf("parent %q is already in wait_state %q", parent.ID, currentWaitState)
		}

		// A pause that landed while this parent was mid-spawn has to settle here or
		// never: the write below parks it on wait_state='waiting', which removes it from
		// the claim predicate, so no later claim could move it out of 'pausing'. The
		// children inherit the settled status too — a paused tree must not spawn
		// runnable work.
		if model.Status(currentStatus) == model.StatusPausing {
			currentStatus = string(model.StatusPaused)
		}

		// Insert children with the parent's current status (propagates a pause if needed).
		// Each child gets created_at = now+i so siblings have a strict ordering by definition
		// position — ClaimInstances (ORDER BY created_at) always processes them in spawn order.
		now := nowMillis()
		for i, child := range children {
			ts := now + int64(i)
			cols, err := db.persistContext(ctx, qtx, child, ts)
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

		// Suspend parent: keep status, set wait_state='waiting'.
		parentCols, err := db.persistContext(ctx, qtx, parent, now)
		if err != nil {
			return err
		}
		if err := qtx.UpdateInstance(ctx, dbgen.UpdateInstanceParams{
			ID:           parent.ID,
			Task:         parent.Task,
			OutputsData:  parentCols.OutputsData,
			OutputData:   parentCols.OutputData,
			ErrorData:    parentCols.ErrorData,
			ExternalData: parentCols.ExternalData,
			EngineState:  parentCols.EngineState,
			RetryCount:   int64(parent.RetryCount),
			WakeAt:       sql.NullInt64{},
			Status:       currentStatus,
			WaitState:    string(model.WaitStateWaiting),
			Error:        parent.Error,
			ErrorCode:    parent.ErrorCode,
			UpdatedAt:    now,
		}); err != nil {
			return fmt.Errorf("suspend parent: %w", err)
		}

		return nil
	})
}
