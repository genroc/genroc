-- The tree a row belongs to, denormalized so a tree's logs page in ONE index range scan
-- instead of a recursive walk over parent_id. Measured against a 5000-instance tree: 23
-- buffers read per 20-row page rather than 18,616, and flat as the tree grows where the
-- walk is linear in it.
--
-- Copying is safe here because neither source can move: an instance's parent_id is set at
-- spawn and never updated, and a log row is written once and only ever deleted. root_id on
-- the log row is what makes the read a single-table scan -- reaching through
-- process_instances for it puts the join and the sort back.
ALTER TABLE process_instances ADD COLUMN root_id TEXT NOT NULL DEFAULT '';
ALTER TABLE process_logs ADD COLUMN root_id TEXT NOT NULL DEFAULT '';

-- Backfill. A recursive walk once, here, rather than a JSON read of call_stack: the two
-- engines spell json_extract differently and the Postgres json_each helper is created after
-- migrations run, so the portable spelling is the CTE both drivers already support.
WITH RECURSIVE tree(id, root_id) AS (
    SELECT id, id FROM process_instances WHERE parent_id = ''
    UNION ALL
    SELECT pi.id, t.root_id FROM process_instances pi JOIN tree t ON pi.parent_id = t.id
)
UPDATE process_instances SET
    root_id = COALESCE((SELECT t.root_id FROM tree t WHERE t.id = process_instances.id), process_instances.id);

-- Logs follow their instance. An orphan (its instance pruned) keeps its own id as the root,
-- so it stays reachable under exactly the id it names rather than disappearing from listings.
UPDATE process_logs SET
    root_id = COALESCE((SELECT pi.root_id FROM process_instances pi WHERE pi.id = process_logs.instance_id),
                       process_logs.instance_id);

-- The tree read: root_id first, then the (created_at, id) the keyset cursor pages on.
CREATE INDEX idx_process_logs_root ON process_logs (root_id, created_at, id);
-- Instances by tree, for everything that wants a tree without walking to find it.
CREATE INDEX idx_instances_root ON process_instances (root_id);
