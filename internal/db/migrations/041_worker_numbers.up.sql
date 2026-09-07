-- One counter, incremented once per process that mints ids. The number it hands back is that
-- process's id namespace, so ids need no coordination after startup and no randomness.
--
-- It only ever increases: a number is never recycled, which is what a fixed slot could not
-- promise without a lease and a reclaim path. A dead worker takes its namespace with it.
CREATE TABLE id_counters (
    name  TEXT   NOT NULL PRIMARY KEY,
    value BIGINT NOT NULL
);
INSERT INTO id_counters (name, value) VALUES ('worker', 0);
