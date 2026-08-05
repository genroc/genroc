-- lease_epoch is a per-row fencing token: how many times the row has been GRANTED to
-- an executor. ClaimInstances bumps it in the same UPDATE that stamps worker_id /
-- lease_expires_at; renewal never touches it (a renewal extends a grant, it does not
-- create one). Every write made on the strength of holding the lease carries
-- AND lease_epoch = ? so a stale advance's write is refused instead of clobbering.
-- See specs/lease-fencing.md.
ALTER TABLE process_instances ADD COLUMN lease_epoch BIGINT NOT NULL DEFAULT 0;
