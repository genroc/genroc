-- name: InsertDefinition :exec
-- The conflict path leaves actor alone: identical content is not a re-deploy, so the first
-- deployer keeps the credit.
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
SELECT DISTINCT child_name, child_version FROM process_dependencies
WHERE parent_name = sqlc.arg(parent_name)
  AND parent_version = sqlc.arg(parent_version)
ORDER BY child_name;

-- name: UpsertChannel :exec
-- Overwrites actor, unlike InsertDefinition: a channel is a mutable pointer, so the actor is
-- whoever moved it last.
INSERT INTO process_channels (name, channel, version, updated_at, actor)
VALUES (sqlc.arg(name), sqlc.arg(channel), sqlc.arg(version), sqlc.arg(updated_at), sqlc.arg(actor))
ON CONFLICT (name, channel) DO UPDATE SET
    version = EXCLUDED.version, updated_at = EXCLUDED.updated_at, actor = EXCLUDED.actor;

-- name: GetChannel :one
SELECT version FROM process_channels
WHERE name = sqlc.arg(name) AND channel = sqlc.arg(channel);

-- name: DeleteChannel :exec
DELETE FROM process_channels WHERE name = sqlc.arg(name) AND channel = sqlc.arg(channel);

-- name: LoadDefinitionsOnChannel :many
SELECT pc.version, pd.definition
FROM process_channels pc
JOIN process_definitions pd ON pd.name = pc.name AND pd.version = pc.version
WHERE pc.channel = sqlc.arg(channel)
ORDER BY pc.name;

-- name: InsertInstance :exec
INSERT INTO process_instances
    (id, process_name, process_version, task,
     input_data, outputs_data, output_data, error_internal, error_data, external_input, external_lost, engine_state,
     parent_id, root_id, spawn_task_id, parent_task_epoch, task_epoch,
     call_stack, retry_count, wake_at, status, phase, error_message, error_code, created_at, updated_at, objects,
     next_replayable)
VALUES
    (sqlc.arg(id), sqlc.arg(process_name), sqlc.arg(process_version), sqlc.arg(task),
     sqlc.arg(input_data), sqlc.arg(outputs_data), sqlc.arg(output_data),
     sqlc.arg(error_internal), sqlc.arg(error_data), sqlc.arg(external_input), sqlc.arg(external_lost), sqlc.arg(engine_state),
     sqlc.arg(parent_id),
     -- Derived from the parent, never bound: CLAUDE.md, root_id.
     COALESCE((SELECT p.root_id FROM process_instances p WHERE p.id = sqlc.arg(parent_id)), sqlc.arg(id)),
     sqlc.arg(spawn_task_id), sqlc.arg(parent_task_epoch), sqlc.arg(task_epoch),
     sqlc.arg(call_stack), sqlc.arg(retry_count), sqlc.arg(wake_at),
     sqlc.arg(status), sqlc.arg(phase), sqlc.arg(error_message), sqlc.arg(error_code),
     sqlc.arg(created_at), sqlc.arg(updated_at), sqlc.arg(objects),
     sqlc.arg(next_replayable));

-- name: UpdateInstance :execrows
-- input_data is never written. The status CASE lands a pause/cancel that arrived mid-lease;
-- moving task_epoch drops the external claim. Fence: CLAUDE.md, the lease fence -- COALESCE
-- so a lease-less caller's unheld row still matches.
UPDATE process_instances
SET task             = sqlc.arg(task),
    next_replayable   = sqlc.arg(next_replayable),
    task_epoch       = sqlc.arg(task_epoch),
    outputs_data     = sqlc.arg(outputs_data),
    output_data      = sqlc.arg(output_data),
    error_internal   = sqlc.arg(error_internal),
    error_data       = sqlc.arg(error_data),
    external_input   = sqlc.arg(external_input),
    external_lost    = sqlc.arg(external_lost),
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
    phase       = sqlc.arg(phase),
    error_message    = sqlc.arg(error_message),
    error_code       = sqlc.arg(error_code),
    updated_at       = sqlc.arg(updated_at),
    external_worker_id        = CASE WHEN task_epoch = sqlc.arg(task_epoch) THEN external_worker_id END,
    external_lease_expires_at = CASE WHEN task_epoch = sqlc.arg(task_epoch) THEN external_lease_expires_at END,
    worker_id        = NULL,
    lease_expires_at = NULL
WHERE id = sqlc.arg(id) AND lease_epoch = sqlc.arg(lease_epoch)
  AND COALESCE(worker_id, '') = CAST(sqlc.arg(worker_id) AS TEXT);

-- name: UpdateInstanceProgress :execrows
-- input_data is immutable, output_data completion-only. A pending pause lands unconditionally,
-- including on the write that parks the row out of the claim predicate. Fence: UpdateInstance.
UPDATE process_instances
SET task             = sqlc.arg(task),
    next_replayable   = sqlc.arg(next_replayable),
    task_epoch       = sqlc.arg(task_epoch),
    outputs_data     = sqlc.arg(outputs_data),
    error_internal   = sqlc.arg(error_internal),
    external_input   = sqlc.arg(external_input),
    external_lost    = sqlc.arg(external_lost),
    engine_state     = sqlc.arg(engine_state),
    objects          = sqlc.arg(objects),
    retry_count      = sqlc.arg(retry_count),
    wake_at    = sqlc.arg(wake_at),
    status           = CASE WHEN status = 'pausing'    THEN 'paused'
                            WHEN status = 'cancelling' THEN 'cancelled' ELSE status END,
    phase       = sqlc.arg(phase),
    updated_at       = sqlc.arg(updated_at),
    external_worker_id        = CASE WHEN task_epoch = sqlc.arg(task_epoch) THEN external_worker_id END,
    external_lease_expires_at = CASE WHEN task_epoch = sqlc.arg(task_epoch) THEN external_lease_expires_at END,
    worker_id        = NULL,
    lease_expires_at = NULL
WHERE id = sqlc.arg(id) AND lease_epoch = sqlc.arg(lease_epoch)
  AND COALESCE(worker_id, '') = CAST(sqlc.arg(worker_id) AS TEXT);

-- name: GetInstance :one
-- Column order is the table's, so sqlc returns dbgen.ProcessInstance: append a new table column
-- here, in GetChildrenForTask and in NonTerminalSubtree, or every toInstance caller breaks.
SELECT id, process_name, process_version, parent_id,
       call_stack, retry_count, wake_at, status, error_message,
       created_at, updated_at, worker_id, lease_expires_at, phase, spawn_task_id,
       input_data, outputs_data, output_data, error_internal, engine_state, task,
       error_code, lease_epoch, task_epoch, parent_task_epoch,
       external_worker_id, external_lease_expires_at, external_claim_epoch, objects,
       next_replayable, error_data, superseded_at, root_id, external_input, external_lost
FROM process_instances
WHERE id = sqlc.arg(id);

-- name: InsertSignal :exec
INSERT INTO process_signals (id, instance_id, task_id, seq, outcome, created_at)
VALUES (sqlc.arg(id), sqlc.arg(instance_id), sqlc.arg(task_id), sqlc.arg(seq), sqlc.arg(outcome), sqlc.arg(created_at));

-- name: PeekOldestSignal :one
-- READ ONLY: DeleteSignal removes it in the same transaction as the state it produced.
SELECT id, outcome FROM process_signals
WHERE instance_id = sqlc.arg(instance_id) AND task_id = sqlc.arg(task_id)
ORDER BY created_at, seq, id LIMIT 1;

-- name: DeleteSignal :exec
DELETE FROM process_signals WHERE id = sqlc.arg(id);

-- name: UnparkExternal :exec
-- Unfenced on purpose (CLAUDE.md, the lease fence). Keep worker_id: it is the ReclaimedExpired
-- evidence. Clearing wake_at stops an answered wait firing external.timeout.
UPDATE process_instances
SET phase = '',
    wake_at    = NULL,
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- name: UnparkAnsweredExternal :many
-- Statuses kept in step with model.Status.AcceptsExternalOutcome by hand.
UPDATE process_instances
SET phase = '',
    wake_at    = NULL,
    updated_at = sqlc.arg(updated_at)
WHERE id IN (SELECT value FROM json_each(sqlc.arg(ids)))
  AND phase = 'external'
  AND status IN ('running', 'paused', 'pausing')
  AND EXISTS (SELECT 1 FROM process_signals s
              WHERE s.instance_id = process_instances.id AND s.task_id = process_instances.task)
RETURNING id;

-- name: CountBufferedSignals :one
SELECT COUNT(*) FROM process_signals
WHERE instance_id = sqlc.arg(instance_id) AND task_id = sqlc.arg(task_id);

-- name: RenewWorkerLeasesChunk :execrows
-- The new_expiry predicate makes a row eligible once per pass, so the chunk loop terminates.
-- Never bump lease_epoch or clear worker_id here: CLAUDE.md, the lease fence.
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
-- The SQL half of model.Status.Terminal() plus 'raised' (drop it and the parent hangs in
-- 'children'), kept in step by hand; paused counts as active. No superseded_at predicate on
-- purpose: a retired attempt is already 'raised'.
SELECT COUNT(*) FROM process_instances
WHERE parent_id = sqlc.arg(parent_id)
  AND spawn_task_id = sqlc.arg(spawn_task_id)
  AND parent_task_epoch = sqlc.arg(parent_task_epoch)
  AND status NOT IN ('completed', 'failed', 'raised', 'cancelled');

-- name: GetPhase :one
SELECT phase FROM process_instances WHERE id = sqlc.arg(id);

-- name: WakeParent :exec
-- A paused parent is armed too (suspended, not doomed); its status keeps it unclaimable.
UPDATE process_instances
SET phase = CASE WHEN status IN ('running', 'pausing', 'paused')
                      THEN 'collecting' ELSE '' END,
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- name: GetChildrenForTask :many
SELECT id, process_name, process_version, parent_id,
       call_stack, retry_count, wake_at, status, error_message,
       created_at, updated_at, worker_id, lease_expires_at, phase, spawn_task_id,
       input_data, outputs_data, output_data, error_internal, engine_state, task,
       error_code, lease_epoch, task_epoch, parent_task_epoch,
       external_worker_id, external_lease_expires_at, external_claim_epoch, objects,
       next_replayable, error_data, superseded_at, root_id, external_input, external_lost
FROM process_instances
WHERE parent_id = sqlc.arg(parent_id)
  AND spawn_task_id = sqlc.arg(spawn_task_id)
  AND parent_task_epoch = sqlc.arg(parent_task_epoch)
  AND superseded_at IS NULL;

-- name: ChildrenOfInstance :many
-- engine_state: a child's slot (child_map key, child_list index) is recorded on the CHILD.
SELECT id, spawn_task_id, engine_state, superseded_at
FROM process_instances
WHERE parent_id = sqlc.arg(parent_id)
ORDER BY created_at, id;

-- name: FailAncestors :exec
-- Paused included (CLAUDE.md, pause/resume). 'raised' absent: a settled outcome never reopens.
-- 'cancelling'/'cancelled' absent: a fault would only overwrite the operator's stop.
UPDATE process_instances
SET status = 'failing', error_message = sqlc.arg(error_message), error_code = sqlc.arg(error_code),
    updated_at = sqlc.arg(updated_at)
WHERE id IN (SELECT value FROM json_each(sqlc.arg(ids)))
  AND status IN ('running', 'pausing', 'paused');

-- name: NextWorkerNumber :one
-- One statement, so the read and the increment cannot interleave.
UPDATE id_counters SET value = value + 1 WHERE name = 'worker' RETURNING value;

-- name: SetStatusIn :exec
-- The caller already holds the locks. Status ONLY, cancel included: ReleaseExternalClaim finds
-- a claim by phase='external'.
UPDATE process_instances
SET status = sqlc.arg(status), updated_at = sqlc.arg(updated_at)
WHERE id IN (SELECT value FROM json_each(sqlc.arg(ids)));

-- name: GrantLeases :exec
-- The SQLite claim's grant half (CLAUDE.md, exceptions); with the Postgres claim, the only
-- places lease_epoch moves.
UPDATE process_instances
SET worker_id = sqlc.arg(worker_id), lease_expires_at = sqlc.arg(lease_expires_at),
    lease_epoch = lease_epoch + 1
WHERE id IN (SELECT value FROM json_each(sqlc.arg(ids)));

-- name: GrantExternalLeases :exec
UPDATE process_instances
SET external_worker_id = sqlc.arg(external_worker_id),
    external_lease_expires_at = sqlc.arg(external_lease_expires_at),
    external_claim_epoch = external_claim_epoch + 1
WHERE id IN (SELECT value FROM json_each(sqlc.arg(ids)));

-- name: RenewExternalLeasesChunk :execrows
-- Scoped by external_worker_id so a renewal never resurrects another holder's claim. Cancelled
-- rows not renewed on purpose: the worker is owed "stop" (HeldExternalClaimsChunk reports it).
UPDATE process_instances
SET external_lease_expires_at = sqlc.arg(new_expiry)
WHERE id IN (SELECT value FROM json_each(sqlc.arg(ids)))
  AND external_worker_id = sqlc.arg(external_worker_id)
  AND status NOT IN ('cancelling', 'cancelled');

-- name: HeldExternalClaimsChunk :many
-- Same transaction as RenewExternalLeasesChunk. An absent id is lost; the caller derives it by
-- difference. specs/external-task-queue.md.
SELECT id, status FROM process_instances
WHERE id IN (SELECT value FROM json_each(sqlc.arg(ids)))
  AND external_worker_id = sqlc.arg(external_worker_id);

-- name: ReleaseExternalClaim :execrows
-- The epoch bump voids the releaser's handle at once; claim_epoch must name the current grant.
UPDATE process_instances
SET external_worker_id = NULL, external_lease_expires_at = NULL,
    external_claim_epoch = external_claim_epoch + 1
WHERE id = sqlc.arg(id) AND task_epoch = sqlc.arg(task_epoch)
  AND external_claim_epoch = sqlc.arg(claim_epoch)
  AND phase = 'external' AND external_worker_id IS NOT NULL;

-- name: MarkExternalClaimLost :execrows
-- wake_at = now so the next poll raises external.lost; external_input is left untouched.
UPDATE process_instances
SET external_lost = 1, wake_at = sqlc.arg(now), updated_at = sqlc.arg(now),
    external_worker_id = NULL, external_lease_expires_at = NULL,
    external_claim_epoch = external_claim_epoch + 1
WHERE id = sqlc.arg(id) AND task_epoch = sqlc.arg(task_epoch) AND phase = 'external';

-- name: ClaimExternalTaskDirect :exec
-- Tests only: a holder ClaimExternalTasks would not grant.
UPDATE process_instances
SET external_worker_id = sqlc.arg(external_worker_id),
    external_lease_expires_at = sqlc.arg(external_lease_expires_at),
    external_claim_epoch = external_claim_epoch + 1
WHERE id = sqlc.arg(id);

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
     -- Derived, never bound (CLAUDE.md, root_id); an orphan is its own root.
     COALESCE((SELECT p.root_id FROM process_instances p WHERE p.id = sqlc.arg(instance_id)), sqlc.arg(instance_id)),
     sqlc.arg(seq), sqlc.arg(level), sqlc.arg(event),
     sqlc.arg(task_id), sqlc.arg(message), sqlc.arg(code), sqlc.arg(data), sqlc.arg(objects), sqlc.arg(meta), sqlc.arg(created_at), sqlc.arg(actor));

-- name: CountDrainingInTree :one
-- Tells a stopped tree from a draining one. Count only the caller's own draining status, or a
-- cancel reads a paused tree as still stopping. root must BE a root. specs/id-list-commands.md.
SELECT COUNT(*) FROM process_instances
WHERE root_id = sqlc.arg(root) AND status = sqlc.arg(draining);

-- name: GetInstanceRoot :one
SELECT root_id FROM process_instances WHERE id = sqlc.arg(id);

-- name: GetInstanceStatus :one
SELECT status FROM process_instances WHERE id = sqlc.arg(id);

-- name: DeleteLogsBefore :execrows
DELETE FROM process_logs WHERE created_at < sqlc.arg(before);

-- name: PutObject :exec
-- DO UPDATE, never DO NOTHING: the row lock makes a racing sweep wait, and clearing released_at
-- voids its mark. CLAUDE.md, the object store, items 2-3.
INSERT INTO objects (hash, content, size, created_at)
VALUES (sqlc.arg(hash), sqlc.arg(content), sqlc.arg(size), sqlc.arg(created_at))
ON CONFLICT (hash) DO UPDATE SET size = excluded.size, released_at = NULL;

-- name: PutObjectRef :exec
INSERT INTO object_refs (hash, owner_kind, owner_id, created_at)
VALUES (sqlc.arg(hash), sqlc.arg(owner_kind), sqlc.arg(owner_id), sqlc.arg(created_at))
ON CONFLICT (hash, owner_kind, owner_id) DO UPDATE SET created_at = object_refs.created_at;

-- name: DropObjectRef :exec
-- Never deletes content: another owner may share the hash. CLAUDE.md, the object store, item 1.
DELETE FROM object_refs
WHERE hash = sqlc.arg(hash) AND owner_kind = sqlc.arg(owner_kind) AND owner_id = sqlc.arg(owner_id);

-- name: GetObject :one
-- Consults no claim on purpose: CLAUDE.md, the object store, item 4.
SELECT content, size FROM objects WHERE hash = sqlc.arg(hash);

-- name: CollectUnreferencedObjects :execrows
-- On Postgres only after collectUnreferencedPG's FOR UPDATE, in the same transaction.
-- CLAUDE.md, the object store, items 1-2.
DELETE FROM objects
WHERE NOT EXISTS (SELECT 1 FROM object_refs r WHERE r.hash = objects.hash)
  AND released_at IS NOT NULL AND released_at < sqlc.arg(before);

-- name: ClearObjectRelease :execrows
-- Runs before MarkObjectReleased, so a re-claimed object is never collected on a stale mark.
UPDATE objects SET released_at = NULL
WHERE released_at IS NOT NULL
  AND EXISTS (SELECT 1 FROM object_refs r WHERE r.hash = objects.hash);

-- name: MarkObjectReleased :execrows
UPDATE objects SET released_at = sqlc.arg(now)
WHERE released_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM object_refs r WHERE r.hash = objects.hash);

-- name: CountObjectRefs :one
SELECT COUNT(*) FROM object_refs WHERE hash = sqlc.arg(hash);

-- name: OrphanedLogRefs :many
-- owner_id IS the log row's id, so no time horizon is needed.
SELECT hash, owner_id FROM object_refs
WHERE owner_kind = 'log'
  AND NOT EXISTS (SELECT 1 FROM process_logs l WHERE l.id = object_refs.owner_id);

-- name: BumpDurabilityMarker :exec
-- A commit that flushes; the value is never read. specs/durability-levels.md s4.
UPDATE durability_marker SET n = n + 1 WHERE id = 1;

-- name: UpgradeInstanceVersion :execrows
-- Every predicate pins what the state was conformed against, so a stale move loses the race
-- instead of clobbering (specs/version-compatibility.md s4). Never clear worker_id to admit a
-- move: it is the only_once evidence.
UPDATE process_instances
SET process_version = sqlc.arg(to_version),
    input_data      = sqlc.arg(input_data),
    outputs_data    = sqlc.arg(outputs_data),
    output_data     = sqlc.arg(output_data),
    error_internal  = sqlc.arg(error_internal),
    error_data      = sqlc.arg(error_data),
    external_input  = sqlc.arg(external_input),
    external_lost   = sqlc.arg(external_lost),
    engine_state    = sqlc.arg(engine_state),
    objects         = sqlc.arg(objects),
    updated_at      = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id)
  AND process_version = sqlc.arg(from_version)
  AND task = sqlc.arg(task)
  AND status IN ('paused', 'failed')
  AND worker_id IS NULL;

-- name: NonTerminalSubtree :many
-- The root is returned whatever its status: a failed root is what an upgrade is for.
-- specs/version-compatibility.md s3c.
SELECT id, process_name, process_version, parent_id,
       call_stack, retry_count, wake_at, status, error_message,
       created_at, updated_at, worker_id, lease_expires_at, phase, spawn_task_id,
       input_data, outputs_data, output_data, error_internal, engine_state, task,
       error_code, lease_epoch, task_epoch, parent_task_epoch,
       external_worker_id, external_lease_expires_at, external_claim_epoch, objects,
       next_replayable, error_data, superseded_at, root_id, external_input, external_lost
FROM process_instances
WHERE root_id = sqlc.arg(root)
  AND (process_instances.id = sqlc.arg(root)
       OR status NOT IN ('completed', 'failed', 'raised', 'cancelled'))
ORDER BY created_at ASC, id ASC;

-- name: SupersedeInstance :exec
-- The row stays (history, logs, object claims); GetChildrenForTask and the revive walk skip
-- it. specs/child-error-handling.md s12.
UPDATE process_instances SET superseded_at = sqlc.arg(superseded_at) WHERE id = sqlc.arg(id);

-- name: InsertAPIToken :exec
INSERT INTO api_tokens (id, hash, label, perms, created_at, expires_at, actor)
VALUES (sqlc.arg(id), sqlc.arg(hash), sqlc.arg(label), sqlc.arg(perms), sqlc.arg(created_at),
        sqlc.narg(expires_at), sqlc.arg(actor));

-- name: GetAPITokenByHash :one
-- Revoked and expired rows are filtered here, not by callers, so no call site can forget.
SELECT id, perms, label FROM api_tokens
WHERE hash = sqlc.arg(hash) AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > sqlc.arg(now));

-- name: GetAnyAPITokenByHash :one
-- Dead rows INCLUDED, so seeding can tell a dead secret from an absent one (UNIQUE(hash)).
-- Never authenticate from it.
SELECT id, label, revoked_at, expires_at FROM api_tokens WHERE hash = sqlc.arg(hash);

-- name: TouchAPIToken :exec
UPDATE api_tokens SET last_used_at = sqlc.arg(last_used_at) WHERE id = sqlc.arg(id);

-- name: ListAPITokens :many
SELECT id, label, perms, created_at, last_used_at, revoked_at, expires_at, actor, revoked_by
FROM api_tokens
ORDER BY created_at DESC, id;

-- name: RevokeAPIToken :execrows
UPDATE api_tokens SET revoked_at = sqlc.arg(revoked_at), revoked_by = sqlc.arg(revoked_by)
WHERE id = sqlc.arg(id) AND revoked_at IS NULL;

-- name: CountLiveAdminTokens :one
-- Runs in bootstrap's insert transaction. Live ADMIN rows only: the question is "is there
-- still a way in", which worker or expired tokens do not answer.
SELECT COUNT(*) FROM api_tokens
WHERE revoked_at IS NULL AND perms LIKE '%"admin"%'
  AND (expires_at IS NULL OR expires_at > sqlc.arg(now));
