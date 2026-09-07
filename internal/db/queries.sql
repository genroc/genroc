-- name: InsertDefinition :exec
-- The conflict path leaves actor alone: re-applying identical content does not re-deploy it,
-- so the first deployer keeps the credit rather than the latest caller taking it.
INSERT INTO process_definitions (name, version, definition, content_hash, created_at, actor)
VALUES (sqlc.arg(name), sqlc.arg(version), sqlc.arg(definition), sqlc.arg(content_hash), sqlc.arg(created_at), sqlc.arg(actor))
ON CONFLICT (name, version) DO UPDATE SET definition = EXCLUDED.definition;

-- name: GetDefinition :one
SELECT name, version, definition, content_hash, created_at, actor
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
-- The conflict path overwrites actor, unlike InsertDefinition's: a channel is a mutable
-- pointer, so the useful actor is whoever moved it last, not whoever created it.
INSERT INTO process_channels (name, channel, version, updated_at, actor)
VALUES (sqlc.arg(name), sqlc.arg(channel), sqlc.arg(version), sqlc.arg(updated_at), sqlc.arg(actor))
ON CONFLICT (name, channel) DO UPDATE SET
    version = EXCLUDED.version, updated_at = EXCLUDED.updated_at, actor = EXCLUDED.actor;

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
     input_data, outputs_data, output_data, error_internal, error_data, external_data, engine_state,
     parent_id, root_id, spawn_task_id, parent_task_epoch, task_epoch,
     call_stack, retry_count, wake_at, status, wait_state, error_message, error_code, created_at, updated_at, objects,
     next_replayable)
VALUES
    (sqlc.arg(id), sqlc.arg(process_name), sqlc.arg(process_version), sqlc.arg(task),
     sqlc.arg(input_data), sqlc.arg(outputs_data), sqlc.arg(output_data),
     sqlc.arg(error_internal), sqlc.arg(error_data), sqlc.arg(external_data), sqlc.arg(engine_state),
     sqlc.arg(parent_id),
     -- The tree, read off the PARENT rather than taken from the caller: parent_id is the one
     -- edge the whole system agrees on, so deriving from anything else (a call_stack a fixture
     -- forgot, a field a new creation site did not set) would put a row in a tree of its own
     -- and lose its rows from the trail without erroring.
     COALESCE((SELECT p.root_id FROM process_instances p WHERE p.id = sqlc.arg(parent_id)), sqlc.arg(id)),
     sqlc.arg(spawn_task_id), sqlc.arg(parent_task_epoch), sqlc.arg(task_epoch),
     sqlc.arg(call_stack), sqlc.arg(retry_count), sqlc.arg(wake_at),
     sqlc.arg(status), sqlc.arg(wait_state), sqlc.arg(error_message), sqlc.arg(error_code),
     sqlc.arg(created_at), sqlc.arg(updated_at), sqlc.arg(objects),
     sqlc.arg(next_replayable));

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
    next_replayable   = sqlc.arg(next_replayable),
    task_epoch       = sqlc.arg(task_epoch),
    outputs_data     = sqlc.arg(outputs_data),
    output_data      = sqlc.arg(output_data),
    error_internal   = sqlc.arg(error_internal),
    error_data       = sqlc.arg(error_data),
    external_data    = sqlc.arg(external_data),
    engine_state     = sqlc.arg(engine_state),
    objects          = sqlc.arg(objects),
    retry_count      = sqlc.arg(retry_count),
    wake_at    = sqlc.arg(wake_at),
    status           = CASE WHEN status = 'pausing'
                            AND CAST(sqlc.arg(status) AS TEXT) = 'running'
                            THEN 'paused'
                            WHEN status = 'cancelling'
                            AND CAST(sqlc.arg(status) AS TEXT) = 'running'
                            THEN 'cancelled' ELSE CAST(sqlc.arg(status) AS TEXT) END,
    wait_state       = sqlc.arg(wait_state),
    error_message    = sqlc.arg(error_message),
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
    next_replayable   = sqlc.arg(next_replayable),
    task_epoch       = sqlc.arg(task_epoch),
    outputs_data     = sqlc.arg(outputs_data),
    error_internal   = sqlc.arg(error_internal),
    external_data    = sqlc.arg(external_data),
    engine_state     = sqlc.arg(engine_state),
    objects          = sqlc.arg(objects),
    retry_count      = sqlc.arg(retry_count),
    wake_at    = sqlc.arg(wake_at),
    status           = CASE WHEN status = 'pausing'    THEN 'paused'
                            WHEN status = 'cancelling' THEN 'cancelled' ELSE status END,
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
-- error_code trails the list instead of sitting beside `error_message`: the order is the table's, not
-- a reading order. A column added to the table must be appended HERE too, or sqlc emits a
-- subset row type and every toInstance caller stops compiling.
SELECT id, process_name, process_version, parent_id,
       call_stack, retry_count, wake_at, status, error_message,
       created_at, updated_at, worker_id, lease_expires_at, wait_state, spawn_task_id,
       input_data, outputs_data, output_data, error_internal, external_data, engine_state, task,
       error_code, lease_epoch, task_epoch, parent_task_epoch,
       external_worker_id, external_lease_expires_at, external_claim_epoch, objects,
       next_replayable, error_data, superseded_at, root_id
FROM process_instances
WHERE id = sqlc.arg(id);

-- ListInstances is hand-written in db_instances.go (dynamic ORDER BY + keyset cursor; see
-- paginate.go). idx_external_queue now serves FILTERED claims -- (process_name,
-- process_version, updated_at) is the claim's predicate once a worker names a process or task.

-- name: InsertSignal :exec
INSERT INTO process_signals (id, instance_id, task_id, seq, outcome, created_at)
VALUES (sqlc.arg(id), sqlc.arg(instance_id), sqlc.arg(task_id), sqlc.arg(seq), sqlc.arg(outcome), sqlc.arg(created_at));

-- name: PeekOldestSignal :one
-- The oldest buffered outcome for (instance, task), FIFO. READ ONLY: the advance decides on it
-- and persist deletes it (DeleteSignal) in the same transaction as the state it produced, so a
-- crash between the two cannot lose an answer or apply it twice.
SELECT id, outcome FROM process_signals
WHERE instance_id = sqlc.arg(instance_id) AND task_id = sqlc.arg(task_id)
ORDER BY created_at, seq, id LIMIT 1;

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
-- No superseded_at predicate on purpose: a retired attempt is 'raised', so it is already
-- outside this test, and the check would cost the child-settle hot path nothing but time.
SELECT COUNT(*) FROM process_instances
WHERE parent_id = sqlc.arg(parent_id)
  AND spawn_task_id = sqlc.arg(spawn_task_id)
  AND parent_task_epoch = sqlc.arg(parent_task_epoch)
  AND status NOT IN ('completed', 'failed', 'raised', 'cancelled');

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
       call_stack, retry_count, wake_at, status, error_message,
       created_at, updated_at, worker_id, lease_expires_at, wait_state, spawn_task_id,
       input_data, outputs_data, output_data, error_internal, external_data, engine_state, task,
       error_code, lease_epoch, task_epoch, parent_task_epoch,
       external_worker_id, external_lease_expires_at, external_claim_epoch, objects,
       next_replayable, error_data, superseded_at, root_id
FROM process_instances
WHERE parent_id = sqlc.arg(parent_id)
  AND spawn_task_id = sqlc.arg(spawn_task_id)
  AND parent_task_epoch = sqlc.arg(parent_task_epoch)
  AND superseded_at IS NULL;

-- name: ChildrenOfInstance :many
-- Every child a parent has spawned, for the detail view. Derived from parent_id rather than
-- read off a slot on the parent: the rows already carry the relation, and a copy kept on the
-- parent is a second source nothing keeps in step with deletes or reparenting. engine_state
-- comes along because the slot a child occupies -- its child_map key, its child_list index --
-- is recorded on the CHILD.
SELECT id, spawn_task_id, engine_state, superseded_at
FROM process_instances
WHERE parent_id = sqlc.arg(parent_id)
ORDER BY created_at, id;

-- name: FailAncestors :exec
-- Paused ancestors are included: pause suppresses advancement, not settlement, so a
-- dead branch still poisons upward. 'raised' is deliberately absent (a settled outcome
-- never reopens into 'failing' -- terminal for "batch done", yet neither poisoning nor
-- poisonable). error_code travels along so a poisoned tree filters by its origin code.
--
-- 'cancelling'/'cancelled' are absent for the opposite reason to 'paused': a pause is
-- reversible, so the tree must still record that it broke, but a cancel is terminal and
-- there is no later run for the failure to matter to. Recording it would overwrite the
-- operator's stop with a fault nobody will act on.
UPDATE process_instances
SET status = 'failing', error_message = sqlc.arg(error_message), error_code = sqlc.arg(error_code),
    updated_at = sqlc.arg(updated_at)
WHERE id IN (SELECT value FROM json_each(sqlc.arg(ids)))
  AND status IN ('running', 'pausing', 'paused');

-- name: NextWorkerNumber :one
-- Allocates this process's id namespace. One statement, so the read and the increment cannot
-- interleave: Postgres takes the row lock, SQLite serialises on its single writer.
UPDATE id_counters SET value = value + 1 WHERE name = 'worker' RETURNING value;

-- name: SetStatusIn :exec
-- Sets one status on an explicit id list the CALLER has already locked -- pause and resume
-- both write their tree this way. The ids bind as a JSON array through json_each, the same
-- dynamic-IN pattern as FailAncestors, which is why neither needs a dialect branch.
UPDATE process_instances
SET status = sqlc.arg(status), updated_at = sqlc.arg(updated_at)
WHERE id IN (SELECT value FROM json_each(sqlc.arg(ids)));

-- name: GrantLeases :exec
-- The SQLite claim's second half: it selects the runnable rows, then grants them here.
-- Postgres does both in one statement with FOR UPDATE SKIP LOCKED, which is the dialect gap
-- that keeps ClaimInstances hand-written -- this half is portable and lives here.
-- A claim IS a grant, so this is one of the two places lease_epoch may move.
UPDATE process_instances
SET worker_id = sqlc.arg(worker_id), lease_expires_at = sqlc.arg(lease_expires_at),
    lease_epoch = lease_epoch + 1
WHERE id IN (SELECT value FROM json_each(sqlc.arg(ids)));

-- name: GrantExternalLeases :exec
-- The external queue's twin of GrantLeases, on the external-claim trio.
UPDATE process_instances
SET external_worker_id = sqlc.arg(external_worker_id),
    external_lease_expires_at = sqlc.arg(external_lease_expires_at),
    external_claim_epoch = external_claim_epoch + 1
WHERE id IN (SELECT value FROM json_each(sqlc.arg(ids)));

-- name: RenewExternalLeasesChunk :execrows
-- RenewWorkerLeasesChunk's external twin, scoped by external_worker_id for the same reason:
-- a renewal must not resurrect a claim on a row someone else now holds.
--
-- A cancelled row is deliberately NOT renewed: the answer the worker is owed is "stop",
-- and extending a lease on work nobody wants would hold the claim open until the worker
-- noticed some other way. HeldExternalClaimsChunk reports it in the same transaction.
UPDATE process_instances
SET external_lease_expires_at = sqlc.arg(new_expiry)
WHERE id IN (SELECT value FROM json_each(sqlc.arg(ids)))
  AND external_worker_id = sqlc.arg(external_worker_id)
  AND status NOT IN ('cancelling', 'cancelled');

-- name: HeldExternalClaimsChunk :many
-- Which of the ids a renewing worker still holds, and what its instance is doing. Run in
-- the SAME transaction as RenewExternalLeasesChunk so the classification describes exactly
-- the rows that write touched: present and live is renewed, present and cancelled is
-- cancelled, and ABSENT is lost -- the id is not reported, so the caller derives it by
-- difference against what it asked for. specs/external-task-queue.md.
SELECT id, status FROM process_instances
WHERE id IN (SELECT value FROM json_each(sqlc.arg(ids)))
  AND external_worker_id = sqlc.arg(external_worker_id);

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
    (id, instance_id, root_id, seq, level, event, task_id, message, code, data, objects, meta, created_at, actor)
VALUES
    (sqlc.arg(id), sqlc.arg(instance_id),
     -- Read off the instance rather than taken from the writer: four call sites append rows and
     -- a forgotten field would drop a child's rows out of its tree's trail without erroring.
     -- An orphan (instance already gone) is its own root, which is what the migration backfilled.
     COALESCE((SELECT p.root_id FROM process_instances p WHERE p.id = sqlc.arg(instance_id)), sqlc.arg(instance_id)),
     sqlc.arg(seq), sqlc.arg(level), sqlc.arg(event),
     sqlc.arg(task_id), sqlc.arg(message), sqlc.arg(code), sqlc.arg(data), sqlc.arg(objects), sqlc.arg(meta), sqlc.arg(created_at), sqlc.arg(actor));

-- ListLogs (one instance) and ListTreeLogs (a whole tree) are hand-written in db_logs.go:
-- both take a dynamic ORDER BY + keyset cursor (see paginate.go). They differ only in
-- which indexed column they filter on -- instance_id or root_id -- since migration 040.

-- CountDrainingInTree counts the rows a previous pause or cancel left mid-task, which is
-- what tells a tree that has STOPPED from one still draining: both select 'running' only,
-- so a second call on a draining tree writes nothing and would otherwise report it as
-- stopped while a worker is still inside a task. `draining` is the caller's own draining
-- state ('pausing' or 'cancelling') -- counting the other verb's would report a tree as
-- still stopping because someone paused it.
-- specs/id-list-commands.md.
--
-- Unlike its neighbours in db_lifecycle.go this one is expressible here: it takes no row
-- locks (no dialect-dependent FOR UPDATE) and binds no dynamic id list.
--
-- `root` must BE a root: root_id names the tree (migration 040), so a child counts nothing.

-- name: CountDrainingInTree :one
SELECT COUNT(*) FROM process_instances
WHERE root_id = sqlc.arg(root) AND status = sqlc.arg(draining);

-- GetInstanceStatus reads one root's status inside the transaction that already holds the
-- tree, which is what lets ResumeProcess decide "already advancing" from "settled and
-- never will" on the same snapshot as the outcome itself.

-- name: GetInstanceRoot :one
SELECT root_id FROM process_instances WHERE id = sqlc.arg(id);

-- name: GetInstanceStatus :one
SELECT status FROM process_instances WHERE id = sqlc.arg(id);

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

-- name: BumpDurabilityMarker :exec
-- Written only to be a commit that flushes; the value is never read.
-- specs/durability-levels.md s4.
UPDATE durability_marker SET n = n + 1 WHERE id = 1;

-- name: UpgradeInstanceVersion :execrows
-- Moves an instance to another version of its definition, writing the migrated state with
-- it: the version is the lens through which the row is read, so the two cannot be written
-- apart. specs/version-compatibility.md s4.
--
-- Conditional on everything that would make the migration stale. process_version and task
-- pin what the state was conformed against. status keeps it to the settled states -- a
-- running instance can be claimed and advanced between the read and this write, and the
-- task predicate is what turns that into a lost race a re-run picks up rather than a
-- clobber.
--
-- worker_id IS NULL is defence, not a live case: a claim only takes the live and draining
-- rows, so a paused or failed one is never leased. It is here because the status filter and
-- the claim predicate are separate statements that could drift apart, and this write must
-- not be the place that discovers it.
UPDATE process_instances
SET process_version = sqlc.arg(to_version),
    input_data      = sqlc.arg(input_data),
    outputs_data    = sqlc.arg(outputs_data),
    output_data     = sqlc.arg(output_data),
    error_internal  = sqlc.arg(error_internal),
    error_data      = sqlc.arg(error_data),
    external_data   = sqlc.arg(external_data),
    engine_state    = sqlc.arg(engine_state),
    objects         = sqlc.arg(objects),
    updated_at      = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id)
  AND process_version = sqlc.arg(from_version)
  AND task = sqlc.arg(task)
  AND status IN ('paused', 'failed')
  AND worker_id IS NULL;

-- name: NonTerminalSubtree :many
-- The instance and every DESCENDANT that is still live, oldest first. Terminal descendants
-- are excluded on purpose: their outputs are frozen and nothing re-runs them, so they are
-- not part of the unit that moves. specs/version-compatibility.md s3c.
--
-- The root is returned whatever its status, because `failed` is a state an upgrade is FOR
-- (move it, then retry it on the new version) and a root filtered out of its own subtree
-- reads as "no tree to move". A failed root has no live descendants anyway: a parent
-- poisoned by a child goes to `failing`, and the claim predicate refuses a waiting row, so
-- it cannot settle to `failed` until its children are terminal.
SELECT id, process_name, process_version, parent_id,
       call_stack, retry_count, wake_at, status, error_message,
       created_at, updated_at, worker_id, lease_expires_at, wait_state, spawn_task_id,
       input_data, outputs_data, output_data, error_internal, external_data, engine_state, task,
       error_code, lease_epoch, task_epoch, parent_task_epoch,
       external_worker_id, external_lease_expires_at, external_claim_epoch, objects,
       next_replayable, error_data, superseded_at, root_id
FROM process_instances
WHERE root_id = sqlc.arg(root)
  AND (process_instances.id = sqlc.arg(root)
       OR status NOT IN ('completed', 'failed', 'raised', 'cancelled'))
ORDER BY created_at ASC, id ASC;

-- name: SupersedeInstance :exec
-- Retires one attempt at a batch slot. The row stays -- it is the attempt's history, its logs
-- and its object claims -- but GetChildrenForTask and the revive walk stop treating it as the
-- slot's live occupant, so the collect merges one value per slot and a replaced raise is not
-- routed again. specs/child-error-handling.md s12.
UPDATE process_instances SET superseded_at = sqlc.arg(superseded_at) WHERE id = sqlc.arg(id);

-- name: InsertAPIToken :exec
INSERT INTO api_tokens (id, hash, label, perms, created_at, expires_at, actor)
VALUES (sqlc.arg(id), sqlc.arg(hash), sqlc.arg(label), sqlc.arg(perms), sqlc.arg(created_at),
        sqlc.narg(expires_at), sqlc.arg(actor));

-- name: GetAPITokenByHash :one
-- The authentication read, on the hot path for every request in token mode. Revoked and expired
-- rows are excluded here rather than by the caller: a revocation that only some call sites
-- honour is the kind of hole that survives review, and an expiry is the same shape of rule.
-- NULL expires_at means the token does not expire.
SELECT id, perms, label FROM api_tokens
WHERE hash = sqlc.arg(hash) AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > sqlc.arg(now));

-- name: TouchAPIToken :exec
UPDATE api_tokens SET last_used_at = sqlc.arg(last_used_at) WHERE id = sqlc.arg(id);

-- name: ListAPITokens :many
SELECT id, label, perms, created_at, last_used_at, revoked_at, expires_at, actor, revoked_by
FROM api_tokens
ORDER BY created_at DESC, id;

-- name: RevokeAPIToken :execrows
-- revoked_by is set in the same statement as revoked_at: they describe one write, and a column
-- only some paths set is the failure section 7 already paid for once.
UPDATE api_tokens SET revoked_at = sqlc.arg(revoked_at), revoked_by = sqlc.arg(revoked_by)
WHERE id = sqlc.arg(id) AND revoked_at IS NULL;

-- name: CountLiveAdminTokens :one
-- Bootstrap asks this under the same transaction as its insert. Counting ADMIN rows rather
-- than all rows is what makes "no way in" the condition, rather than "no tokens at all": a
-- deployment holding only worker tokens has locked its operators out and still needs a way back.
-- An expired admin token cannot authenticate, so it must not satisfy "a way in still exists"
-- either -- otherwise a deployment whose only admin credential lapsed can never bootstrap again.
SELECT COUNT(*) FROM api_tokens
WHERE revoked_at IS NULL AND perms LIKE '%"admin"%'
  AND (expires_at IS NULL OR expires_at > sqlc.arg(now));
