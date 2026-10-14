-- The instance-retention sweep's search: roots that concluded, oldest first. A tree goes as one,
-- after its root concluded, so children are never in it. The status list is Status.Terminal().
CREATE INDEX idx_instances_concluded_roots ON process_instances (updated_at, id)
    WHERE parent_id = '' AND status IN ('completed', 'failed', 'raised', 'cancelled');
