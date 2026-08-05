-- lease_epoch is a per-row fencing token: bumped only when ClaimInstances grants the
-- row (renewal never touches it), bound into every lease-holding write so a stale
-- advance is refused instead of clobbering. specs/lease-fencing.md.
ALTER TABLE process_instances ADD COLUMN lease_epoch BIGINT NOT NULL DEFAULT 0;
