-- The external-task queue's claim: a worker takes a parked task, holds it for a visibility
-- timeout, and answers or lets it expire back onto the queue. specs/external-task-queue.md.
--
-- Three columns, NOT the engine's worker_id/lease_expires_at/lease_epoch, which mean "an
-- engine worker is ADVANCING this instance" while a claim means the opposite: the instance is
-- parked and no worker is held. Sharing them would lock a claimer out of its own resolve
-- (ResolveExternalTask refuses a submit under a live lease), delay the external.timeout the
-- engine owes at wake_at, and forge the worker_id evidence only_once.interrupted reads.
ALTER TABLE process_instances ADD COLUMN external_worker_id TEXT;
ALTER TABLE process_instances ADD COLUMN external_lease_expires_at BIGINT;
-- The fence, mirroring lease_epoch: bumped ONLY by a claim (a claim is a grant), bound into
-- every write by the holder. Needed on top of the token because two workers can claim the same
-- ARMING in sequence -- the first claim expires, the second is granted, and the first worker's
-- <instance>.<task_epoch> is still valid without it.
ALTER TABLE process_instances ADD COLUMN external_claim_epoch BIGINT NOT NULL DEFAULT 0;

-- Serves the claim predicate. Partial on the same wait_state as idx_external_queue, so it
-- covers only parked rows; ordered by park time, which is the queue's FIFO order (the reverse
-- of the LIST endpoint's newest-first, which is a UI affordance rather than a queue).
CREATE INDEX idx_external_claimable ON process_instances (external_lease_expires_at, updated_at)
    WHERE wait_state = 'external';
