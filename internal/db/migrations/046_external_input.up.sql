-- Split external_data into the two unrelated things it held.
--
--   external_input - the parked task's evaluated input snapshot, the only VALUE here
--   external_lost  - a marker that a holder's claim lapsed without an answer, which is a
--                    fact about the CLAIM and belongs beside external_worker_id, not inside
--                    a payload column
--
-- The name is the same at every layer now: column, context key, objects path root and the
-- API field are all external_input, because a stored objects path that spells the slot
-- differently from the context cannot be placed on read. specs/object-store.md.
--
-- Prototype: external_data is dropped with no backfill, as migration 019 dropped context_data.
-- An instance parked on an external task at upgrade loses its input snapshot; its stale
-- ["_external",...] objects paths fail to place and the next write releases those claims,
-- so nothing is leaked.
ALTER TABLE process_instances DROP COLUMN external_data;
ALTER TABLE process_instances ADD COLUMN external_input TEXT NOT NULL DEFAULT '';
ALTER TABLE process_instances ADD COLUMN external_lost INTEGER NOT NULL DEFAULT 0;
