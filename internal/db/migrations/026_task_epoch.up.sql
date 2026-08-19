-- Two counters, both distinct from lease_epoch (a worker grant, bumped on every claim).
--
-- task_epoch numbers an instance's task ENTRIES: it moves when the instance transitions to
-- a task and stays put while it is parked on one, so spawn and collect are the same epoch.
--
-- parent_task_epoch is the parent's task_epoch a child was spawned under, and is what makes
-- a batch addressable. Children live under (parent_id, spawn_task_id), which is not unique
-- in time: a child task re-entered by a loop spawns a fresh batch under the same pair, and
-- an unscoped collect gathered every child ever spawned there.
ALTER TABLE process_instances ADD COLUMN task_epoch BIGINT NOT NULL DEFAULT 0;
ALTER TABLE process_instances ADD COLUMN parent_task_epoch BIGINT NOT NULL DEFAULT 0;
