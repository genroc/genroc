-- 'failing' leaves the claim predicate: a failing row is a parent parked on its batch, settled in
-- SQL by the child that lands last (specs/pause-resume.md §3), so it is never claimed. One
-- predicate with ClaimInstances' status list, written twice.
DROP INDEX IF EXISTS idx_instances_runnable;
CREATE INDEX idx_instances_runnable ON process_instances (created_at)
    WHERE status IN ('running', 'pausing', 'cancelling') AND phase <> 'children';
