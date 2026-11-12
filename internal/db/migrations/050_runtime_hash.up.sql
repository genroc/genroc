-- runtime_hash: what a version RUNS, beside content_hash, which is now over the document as
-- submitted (x- keys and spelling included). An apply compares a new version's runtime_hash with
-- the one the channel pointed at to report "same runtime". specs/extension-keys.md section 3.
--
-- Every existing content_hash was computed over the normalized model plus the child pins, which
-- is exactly the runtime basis, so it is the backfill.
ALTER TABLE process_definitions ADD COLUMN runtime_hash TEXT NOT NULL DEFAULT '';
UPDATE process_definitions SET runtime_hash = content_hash;
