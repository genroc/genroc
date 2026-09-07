-- The tree a row belongs to, so a tree's logs page in one index range scan instead of a
-- recursive walk over parent_id: 23 buffers per 20-row page against 18,616, and flat as the
-- tree grows. Safe to copy because neither source moves -- parent_id is set at spawn, a log row
-- is written once. On the LOG row and not just the instance: reaching through process_instances
-- puts the join and the sort back.
ALTER TABLE process_instances ADD COLUMN root_id TEXT NOT NULL DEFAULT '';
ALTER TABLE process_logs ADD COLUMN root_id TEXT NOT NULL DEFAULT '';

-- A recursive walk once, here, rather than reading call_stack: the engines spell json_extract
-- differently and the Postgres json_each helper is created after migrations run.
WITH RECURSIVE tree(id, root_id) AS (
    SELECT id, id FROM process_instances WHERE parent_id = ''
    UNION ALL
    SELECT pi.id, t.root_id FROM process_instances pi JOIN tree t ON pi.parent_id = t.id
)
UPDATE process_instances SET
    root_id = COALESCE((SELECT t.root_id FROM tree t WHERE t.id = process_instances.id), process_instances.id);

-- An orphan (its instance pruned) keeps its own id as the root, so it stays reachable under the
-- only id it names.
UPDATE process_logs SET
    root_id = COALESCE((SELECT pi.root_id FROM process_instances pi WHERE pi.id = process_logs.instance_id),
                       process_logs.instance_id);

CREATE INDEX idx_process_logs_root ON process_logs (root_id, created_at, id);
CREATE INDEX idx_instances_root ON process_instances (root_id);
