-- One counter, incremented once per process that mints instance ids. The number it hands
-- back is that process's id NAMESPACE, so ids need no coordination after startup and no
-- randomness to avoid collisions: `<worker>-<seq>` is unique by construction.
--
-- It only ever increases. A number is never recycled, which is what removes the whole
-- lease-and-reclaim problem a fixed slot would have: a worker that dies takes its number
-- with it, and the next one gets a fresh namespace rather than inheriting a dead worker's.
CREATE TABLE id_counters (
    name  TEXT   NOT NULL PRIMARY KEY,
    value BIGINT NOT NULL
);
INSERT INTO id_counters (name, value) VALUES ('worker', 0);
