-- The grace window stops being a claim and becomes a timestamp the sweep maintains.
-- specs/object-store.md.
--
-- A grace "claim" was a row in an ownership table whose owner_id was '' -- not an owner, a timer
-- wearing a claim's costume. Worse, stamping it was a DISTRIBUTED obligation: every path that
-- dropped a claim had to remember, and no such path can even tell whether it dropped the LAST
-- one. That rule needed an exception the first time a new releaser appeared (orphaned log
-- claims), and any claim retired by the expiry sweep got no window at all.
--
-- The sweep is the one component that sees every claim, so it decides: it marks an object when it
-- observes that nothing claims it, clears the mark if something claims it again, and collects once
-- the mark is older than --object-grace. Every owner is then free to care only about its own
-- references.
ALTER TABLE objects ADD COLUMN released_at BIGINT;

-- A grace ref's created_at IS the instant of the release it was stamped for, so the conversion is
-- exact rather than approximate.
UPDATE objects SET released_at = (
    SELECT MAX(r.created_at) FROM object_refs r
    WHERE r.hash = objects.hash AND r.owner_kind = 'grace'
)
WHERE EXISTS (
    SELECT 1 FROM object_refs r WHERE r.hash = objects.hash AND r.owner_kind = 'grace'
);

DELETE FROM object_refs WHERE owner_kind = 'grace';

-- Log claims written before the owner became the log ROW name an instance, which cannot be mapped
-- back to the rows that need them. They go, and their objects fall to the mark with a fresh
-- window rather than vanishing on the next sweep. Only reachable in a database written before the
-- row-owned scheme, which has never shipped.
DELETE FROM object_refs WHERE owner_kind = 'log' AND expires_at IS NOT NULL;

-- Nothing carries an expiry any more: an instance claim is released, a log claim lives as long as
-- its row, and the grace window is the column above.
DROP INDEX idx_object_refs_hash;
CREATE INDEX idx_object_refs_hash ON object_refs (hash);
ALTER TABLE object_refs DROP COLUMN expires_at;

-- The sweep asks "is anything claiming this" and "how long has nothing been", so it reads the mark
-- across the table.
CREATE INDEX idx_objects_released ON objects (released_at);
