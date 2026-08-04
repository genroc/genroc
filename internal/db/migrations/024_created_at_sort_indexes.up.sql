-- Index the created_at sort every list endpoint now defaults to.
--
-- process_instances had no full index on created_at: migration 006's
-- (status, created_at) was dropped in 010, and the surviving idx_instances_runnable is
-- PARTIAL (status IN running/failing/pausing AND wait_state <> 'waiting'), so it covers
-- the claim path and excludes exactly the completed/failed rows a monitoring list is
-- mostly made of. The default sort was a scan-and-sort. This mirrors
-- idx_instances_updated_at (migration 015), including the UUIDv7 PK tiebreaker that
-- makes the keyset cursor total.
CREATE INDEX idx_instances_created_at ON process_instances (created_at, id);

-- process_definitions is keyed (name, version), so a created_at sort needs both to stay
-- a total order — two versions saved in the same millisecond would otherwise share a
-- cursor position and the keyset walk could skip or repeat one.
CREATE INDEX idx_definitions_created_at ON process_definitions (created_at, name, version);
