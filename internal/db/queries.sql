-- name: InsertDefinition :exec
INSERT INTO process_definitions (name, version, definition, content_hash, created_at)
VALUES (sqlc.arg(name), sqlc.arg(version), sqlc.arg(definition), sqlc.arg(content_hash), sqlc.arg(created_at))
ON CONFLICT (name, version) DO UPDATE SET definition = EXCLUDED.definition;

-- name: GetDefinition :one
SELECT name, version, definition, content_hash, created_at
FROM process_definitions
WHERE name = sqlc.arg(name) AND version = sqlc.arg(version);

-- name: LatestVersion :one
SELECT MAX(version) FROM process_definitions WHERE name = sqlc.arg(name);

-- name: FindVersionByHash :one
SELECT MAX(version) FROM process_definitions
WHERE name = sqlc.arg(name) AND content_hash = sqlc.arg(content_hash);

-- ListDefinitions is hand-written in db_registry.go (dynamic ORDER BY + keyset
-- cursor; see paginate.go).

-- name: DeleteDependencies :exec
DELETE FROM process_dependencies
WHERE parent_name = sqlc.arg(parent_name) AND parent_version = sqlc.arg(parent_version);

-- name: InsertDependency :exec
INSERT INTO process_dependencies (parent_name, parent_version, task_id, child_key, child_name, child_version)
VALUES (sqlc.arg(parent_name), sqlc.arg(parent_version), sqlc.arg(task_id), sqlc.arg(child_key), sqlc.arg(child_name), sqlc.arg(child_version));

-- name: GetDependencyVersion :one
SELECT child_version FROM process_dependencies
WHERE parent_name = sqlc.arg(parent_name)
  AND parent_version = sqlc.arg(parent_version)
  AND task_id = sqlc.arg(task_id)
  AND child_key = sqlc.arg(child_key);

-- name: ListDependencies :many
-- Every child version one definition version was registered against. A comparison uses
-- it to close a named process over the versions it actually runs, so a parent is never
-- judged without the children it calls.
SELECT DISTINCT child_name, child_version FROM process_dependencies
WHERE parent_name = sqlc.arg(parent_name)
  AND parent_version = sqlc.arg(parent_version)
ORDER BY child_name;

-- name: UpsertChannel :exec
INSERT INTO process_channels (name, channel, version, updated_at)
VALUES (sqlc.arg(name), sqlc.arg(channel), sqlc.arg(version), sqlc.arg(updated_at))
ON CONFLICT (name, channel) DO UPDATE SET version = EXCLUDED.version, updated_at = EXCLUDED.updated_at;

-- name: GetChannel :one
SELECT version FROM process_channels
WHERE name = sqlc.arg(name) AND channel = sqlc.arg(channel);

-- name: DeleteChannel :exec
DELETE FROM process_channels WHERE name = sqlc.arg(name) AND channel = sqlc.arg(channel);

-- ListChannels is hand-written in db_registry.go (dynamic ORDER BY + keyset
-- cursor; see paginate.go).

-- name: LoadDefinitionsOnChannel :many
SELECT pc.version, pd.definition
FROM process_channels pc
JOIN process_definitions pd ON pd.name = pc.name AND pd.version = pc.version
WHERE pc.channel = sqlc.arg(channel)
ORDER BY pc.name;

-- name: InsertInstance :exec
INSERT INTO process_instances
    (id, process_name, process_version, task,
     input_data, outputs_data, output_data, error_data, external_data, engine_state,
     parent_id, spawn_task_id, parent_task_epoch, task_epoch,
     call_stack, retry_count, wake_at, status, wait_state, error, error_code, created_at, updated_at, objects)
VALUES
    (sqlc.arg(id), sqlc.arg(process_name), sqlc.arg(process_version), sqlc.arg(task),
     sqlc.arg(input_data), sqlc.arg(outputs_data), sqlc.arg(output_data),
     sqlc.arg(error_data), sqlc.arg(external_data), sqlc.arg(engine_state),
     sqlc.arg(parent_id), sqlc.arg(spawn_task_id), sqlc.arg(parent_task_epoch), sqlc.arg(task_epoch),
     sqlc.arg(call_stack), sqlc.arg(retry_count), sqlc.arg(wake_at),
     sqlc.arg(status), sqlc.arg(wait_state), sqlc.arg(error), sqlc.arg(error_code),
     sqlc.arg(created_at), sqlc.arg(updated_at), sqlc.arg(objects));

-- name: UpdateInstance :execrows
-- input_data is never written (immutable). The status CASE lands a pause that arrived
-- while this instance was leased, decided in SQL against the row's current value; only
-- a still-running instance settles into 'paused' (pause invariants: CLAUDE.md).
-- lease_epoch + worker_id are the fence: zero rows = grant gone = ErrLeaseLost; lease-less
-- callers bind both as read under their row lock. worker_id is there because a rewind can
-- re-issue an epoch to a second worker; it does not replace the epoch, which is what fences
-- a self-reclaim. COALESCE so an unheld row compares. specs/lease-fencing.md.
UPDATE process_instances
SET task             = sqlc.arg(task),
    task_epoch       = sqlc.arg(task_epoch),
    outputs_data     = sqlc.arg(outputs_data),
    output_data      = sqlc.arg(output_data),
    error_data       = sqlc.arg(error_data),
    external_data    = sqlc.arg(external_data),
    engine_state     = sqlc.arg(engine_state),
    objects          = sqlc.arg(objects),
    retry_count      = sqlc.arg(retry_count),
    wake_at    = sqlc.arg(wake_at),
    status           = CASE WHEN status = 'pausing'
                            AND CAST(sqlc.arg(status) AS TEXT) = 'running'
                            THEN 'paused' ELSE CAST(sqlc.arg(status) AS TEXT) END,
    wait_state       = sqlc.arg(wait_state),
    error            = sqlc.arg(error),
    error_code       = sqlc.arg(error_code),
    updated_at       = sqlc.arg(updated_at),
    worker_id        = NULL,
    lease_expires_at = NULL
WHERE id = sqlc.arg(id) AND lease_epoch = sqlc.arg(lease_epoch)
  AND COALESCE(worker_id, '') = CAST(sqlc.arg(worker_id) AS TEXT);

-- name: UpdateInstanceProgress :execrows
-- Mid-process write: input_data (immutable) and output_data (completion-only) are not
-- touched. A checkpoint means "still running", so a pending pause lands unconditionally
-- here -- including on the write that parks the instance out of the claim predicate,
-- its last chance to settle. lease_epoch: see UpdateInstance.
UPDATE process_instances
SET task             = sqlc.arg(task),
    task_epoch       = sqlc.arg(task_epoch),
    outputs_data     = sqlc.arg(outputs_data),
    error_data       = sqlc.arg(error_data),
    external_data    = sqlc.arg(external_data),
    engine_state     = sqlc.arg(engine_state),
    objects          = sqlc.arg(objects),
    retry_count      = sqlc.arg(retry_count),
    wake_at    = sqlc.arg(wake_at),
    status           = CASE WHEN status = 'pausing' THEN 'paused' ELSE status END,
    wait_state       = sqlc.arg(wait_state),
    updated_at       = sqlc.arg(updated_at),
    worker_id        = NULL,
    lease_expires_at = NULL
WHERE id = sqlc.arg(id) AND lease_epoch = sqlc.arg(lease_epoch)
  AND COALESCE(worker_id, '') = CAST(sqlc.arg(worker_id) AS TEXT);

-- name: GetInstance :one
-- Column order matches the process_instances row struct (context columns then task then
-- error_code then lease_epoch then the external-claim trio, appended by migrations 019, 020,
-- 023, 025, 026 and 028) so sqlc returns dbgen.ProcessInstance directly. That is why
-- error_code trails the list instead of sitting beside `error`: the order is the table's, not
-- a reading order. A column added to the table must be appended HERE too, or sqlc emits a
-- subset row type and every toInstance caller stops compiling.
SELECT id, process_name, process_version, parent_id,
       call_stack, retry_count, wake_at, status, error,
       created_at, updated_at, worker_id, lease_expires_at, wait_state, spawn_task_id,
       input_data, outputs_data, output_data, error_data, external_data, engine_state, task,
       error_code, lease_epoch, task_epoch, parent_task_epoch,
       external_worker_id, external_lease_expires_at, external_claim_epoch, objects
FROM process_instances
WHERE id = sqlc.arg(id);

-- ListInstances is hand-written in db_instances.go and ListExternalTasks in
-- db_external.go (dynamic ORDER BY + keyset cursor; see paginate.go). The
-- external-task queue is still served by the partial idx_external_queue index.

-- name: InsertSignal :exec
INSERT INTO process_signals (id, instance_id, task_id, outcome, created_at)
VALUES (sqlc.arg(id), sqlc.arg(instance_id), sqlc.arg(task_id), sqlc.arg(outcome), sqlc.arg(created_at));

-- name: PeekOldestSignal :one
-- The oldest buffered outcome for (instance, task), FIFO. READ ONLY: the advance decides on it
-- and persist deletes it (DeleteSignal) in the same transaction as the state it produced, so a
-- crash between the two cannot lose an answer or apply it twice.
SELECT id, outcome FROM process_signals
WHERE instance_id = sqlc.arg(instance_id) AND task_id = sqlc.arg(task_id)
ORDER BY created_at, id LIMIT 1;

-- name: DeleteSignal :exec
DELETE FROM process_signals WHERE id = sqlc.arg(id);

-- name: UnparkExternal :exec
-- Makes a parked external task claimable, after its answer has been buffered. Callers act on a
-- PARKED row under the row lock, so there is no grant to fence; worker_id stays -- clearing it
-- destroys a crashed owner's ReclaimedExpired evidence. Clearing wake_at is load-bearing beyond
-- tidiness: an answered wait must not later fire external.timeout, which on an only_once task
-- can never be retried.
UPDATE process_instances
SET wait_state = '',
    wake_at    = NULL,
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- name: CountBufferedSignals :one
SELECT COUNT(*) FROM process_signals
WHERE instance_id = sqlc.arg(instance_id) AND task_id = sqlc.arg(task_id);

-- name: RenewWorkerLeasesChunk :execrows
-- Renews up to chunk_size of the listed (held-set) leases, soonest-to-expire first, in
-- a loop of small transactions; the new_expiry predicate makes each row eligible once
-- per pass, so the loop terminates. Must NOT bump lease_epoch (it would fence out the
-- advance it rescues) and must NOT clear worker_id: an unlisted row expires with it
-- set, which is the ReclaimedExpired/only_once evidence. specs/lease-fencing.md.
UPDATE process_instances
SET lease_expires_at = sqlc.arg(new_expiry)
WHERE id IN (
    SELECT pi.id FROM process_instances pi
    WHERE pi.id IN (SELECT value FROM json_each(sqlc.arg(ids)))
      AND pi.worker_id = sqlc.arg(worker_id)
      AND pi.lease_expires_at < sqlc.arg(new_expiry)
    ORDER BY pi.lease_expires_at ASC
    LIMIT sqlc.arg(chunk_size)
);

-- name: CountActiveSiblings :one
-- Only completed/failed/raised are settled; a paused sibling counts as active, so a
-- parent never collects while a child is suspended. 'raised' must stay or the parent
-- hangs in 'waiting'. The SQL half of model.Status.Terminal(); kept in step by hand.
SELECT COUNT(*) FROM process_instances
WHERE parent_id = sqlc.arg(parent_id)
  AND spawn_task_id = sqlc.arg(spawn_task_id)
  AND parent_task_epoch = sqlc.arg(parent_task_epoch)
  AND status NOT IN ('completed', 'failed', 'raised');

-- name: GetWaitState :one
SELECT wait_state FROM process_instances WHERE id = sqlc.arg(id);

-- name: WakeParent :exec
-- A healthy parent moves to 'collecting' to merge its children's outputs; a doomed
-- one ('failing') clears the wait state and just settles. A paused parent is healthy
-- (it is suspended, not doomed) so it is armed for the collect it will run when
-- resumed. Its status keeps it unclaimable in the meantime.
UPDATE process_instances
SET wait_state = CASE WHEN status IN ('running', 'pausing', 'paused')
                      THEN 'collecting' ELSE '' END,
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- name: GetChildrenForTask :many
SELECT id, process_name, process_version, parent_id,
       call_stack, retry_count, wake_at, status, error,
       created_at, updated_at, worker_id, lease_expires_at, wait_state, spawn_task_id,
       input_data, outputs_data, output_data, error_data, external_data, engine_state, task,
       error_code, lease_epoch, task_epoch, parent_task_epoch,
       external_worker_id, external_lease_expires_at, external_claim_epoch, objects
FROM process_instances
WHERE parent_id = sqlc.arg(parent_id)
  AND spawn_task_id = sqlc.arg(spawn_task_id)
  AND parent_task_epoch = sqlc.arg(parent_task_epoch);

-- name: FailAncestors :exec
-- Paused ancestors are included: pause suppresses advancement, not settlement, so a
-- dead branch still poisons upward. 'raised' is deliberately absent (a settled outcome
-- never reopens into 'failing' -- terminal for "batch done", yet neither poisoning nor
-- poisonable). error_code travels along so a poisoned tree filters by its origin code.
UPDATE process_instances
SET status = 'failing', error = sqlc.arg(error), error_code = sqlc.arg(error_code),
    updated_at = sqlc.arg(updated_at)
WHERE id IN (SELECT value FROM json_each(sqlc.arg(ids)))
  AND status IN ('running', 'pausing', 'paused');

-- name: FindStaleRefs :many
SELECT pd.parent_name, pc.version AS parent_version,
       pd.task_id, pd.child_name,
       pd.child_version AS baked_version, pc2.version AS channel_version
FROM process_dependencies pd
JOIN process_channels pc  ON pc.name  = pd.parent_name AND pc.channel = sqlc.arg(channel)
JOIN process_channels pc2 ON pc2.name = pd.child_name  AND pc2.channel = sqlc.arg(channel)
WHERE pd.parent_version = pc.version
  AND pd.child_version < pc2.version
ORDER BY pd.parent_name, pd.child_name, pd.task_id;

-- name: InsertLog :exec
INSERT INTO process_logs
    (id, instance_id, level, event, task_id, message, code, data, objects, meta, created_at)
VALUES
    (sqlc.arg(id), sqlc.arg(instance_id), sqlc.arg(level), sqlc.arg(event),
     sqlc.arg(task_id), sqlc.arg(message), sqlc.arg(code), sqlc.arg(data), sqlc.arg(objects), sqlc.arg(meta), sqlc.arg(created_at));

-- ListLogs (per-instance) and ListTreeLogs (subtree) are hand-written in
-- db_logs.go: both take a dynamic ORDER BY + keyset cursor (see paginate.go), and
-- the subtree view additionally needs a WITH RECURSIVE walk over
-- process_instances.parent_id that sqlc's SQLite grammar can't parse. Both runtime
-- drivers support it.

-- name: DeleteLogsBefore :execrows
DELETE FROM process_logs WHERE created_at < sqlc.arg(before);

-- name: PutObject :exec
-- Write the content once, globally. Immutable by construction -- the hash IS the content -- so
-- the conflict path has nothing to change.
--
-- DO UPDATE, not DO NOTHING, and that is load-bearing rather than style. DO NOTHING writes
-- nothing and takes no row lock, so a concurrent sweep never even pauses before deleting the
-- object this statement is about to claim. The update is what makes the sweep WAIT.
--
-- It also clears the release mark, and this is the right moment for it: the writer is about to
-- claim the object, so any window the sweep opened is void. Doing it here rather than leaving it
-- to the sweep's own clear closes the gap where an object is marked, claimed and released again
-- BETWEEN two sweeps -- which would leave a mark already older than the window and collect the
-- content with no grace at all. size is written for the lock; released_at is written because it
-- is true.
--
-- Waiting is only half of it, and the half this comment used to claim on its own was wrong: a
-- one-statement sweep wakes and re-checks the row, not its subquery, so it deleted anyway. The
-- other half is the sweep's lock-then-delete split (collectUnreferencedPG). Neither works alone.
-- specs/object-store.md.
INSERT INTO objects (hash, content, size, created_at)
VALUES (sqlc.arg(hash), sqlc.arg(content), sqlc.arg(size), sqlc.arg(created_at))
ON CONFLICT (hash) DO UPDATE SET size = excluded.size, released_at = NULL;

-- name: PutObjectRef :exec
-- Claim an object for an owner. Idempotent: a repeat claim keeps the row it already has, so a
-- caller never needs to know which of the hashes it references are new.
INSERT INTO object_refs (hash, owner_kind, owner_id, created_at)
VALUES (sqlc.arg(hash), sqlc.arg(owner_kind), sqlc.arg(owner_id), sqlc.arg(created_at))
ON CONFLICT (hash, owner_kind, owner_id) DO UPDATE SET created_at = object_refs.created_at;

-- name: DropObjectRef :exec
-- Release one owner's claim. It does NOT touch content: another owner may hold the same hash,
-- and deleting here is exactly how a shared store loses an unrelated instance's value. The
-- caller stamps a grace claim instead, so a reference already handed out stays fetchable.
DELETE FROM object_refs
WHERE hash = sqlc.arg(hash) AND owner_kind = sqlc.arg(owner_kind) AND owner_id = sqlc.arg(owner_id);

-- name: GetObject :one
-- The only read. Addressed by content hash and consulting no ref: knowing a hash is knowing the
-- bytes that produce it, so this discloses nothing a holder of the hash did not already have.
SELECT content, size FROM objects WHERE hash = sqlc.arg(hash);

-- name: CollectUnreferencedObjects :execrows
-- Sweep, step two, and the whole GC rule: an object goes when no claim remains. Never "was mine
-- the last one", which is the question a refcount would have to get right and the way a shared
-- store loses someone else's value.
--
-- SQLITE ONLY. Its single writer means no claim can commit between this statement's snapshot and
-- its delete. Postgres needs the lock-then-delete split in collectUnreferencedPG: one statement
-- has one snapshot, so a concurrent claim stays invisible to the NOT EXISTS however long the
-- statement waited on a row lock.
DELETE FROM objects
WHERE NOT EXISTS (SELECT 1 FROM object_refs r WHERE r.hash = objects.hash)
  AND released_at IS NOT NULL AND released_at < sqlc.arg(before);

-- name: ClearObjectRelease :execrows
-- Something claims it again, so the mark is void. Runs before the mark, so an object that gained
-- and kept a claim is never collected on a stale one.
UPDATE objects SET released_at = NULL
WHERE released_at IS NOT NULL
  AND EXISTS (SELECT 1 FROM object_refs r WHERE r.hash = objects.hash);

-- name: MarkObjectReleased :execrows
-- Nothing claims it, and nothing had noticed yet: start the clock. The sweep decides this rather
-- than the releaser, because no owner dropping ITS claim can tell whether it dropped the last one
-- -- which is what made stamping a grace claim a distributed obligation nobody could satisfy.
UPDATE objects SET released_at = sqlc.arg(now)
WHERE released_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM object_refs r WHERE r.hash = objects.hash);

-- name: CountObjectRefs :one
-- How many owners hold this object. Diagnostics, and the only way a test can see the
-- cross-instance sharing this store exists for.
SELECT COUNT(*) FROM object_refs WHERE hash = sqlc.arg(hash);

-- name: OrphanedLogRefs :many
-- Log claims whose owner row is gone. owner_id IS the log row's id, so a claim is wanted exactly
-- while its row is and needs no horizon to say so.
--
SELECT hash, owner_id FROM object_refs
WHERE owner_kind = 'log'
  AND NOT EXISTS (SELECT 1 FROM process_logs l WHERE l.id = object_refs.owner_id);
