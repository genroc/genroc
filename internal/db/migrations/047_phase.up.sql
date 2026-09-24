-- wait_state -> phase, and its 'waiting' value -> 'children'.
--
-- The old name was untrue of one of its three values: 'collecting' is CLAIMABLE -- the parent
-- has work to do, merging the settled batch -- so it was never a wait. 'phase' covers all three
-- ('children', 'collecting', 'external') without claiming they are the same kind of thing, and
-- drops the phase=waiting stutter on the way. Empty stays "running a task like any other".
--
-- The column rename carries dependent index predicates on both engines. The VALUE rename does
-- not, so idx_instances_runnable is rebuilt: its predicate names 'waiting' literally, and a
-- claim reading a stale predicate would either skip runnable rows or offer parked ones.
ALTER TABLE process_instances RENAME COLUMN wait_state TO phase;
UPDATE process_instances SET phase = 'children' WHERE phase = 'waiting';

DROP INDEX IF EXISTS idx_instances_runnable;
CREATE INDEX idx_instances_runnable ON process_instances (created_at)
    WHERE status IN ('running', 'failing', 'pausing', 'cancelling') AND phase <> 'children';
