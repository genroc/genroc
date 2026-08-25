-- The only_once bracket needs a way to make everything committed so far durable, at a
-- durability level where ordinary commits are not flushed. specs/durability-levels.md s4.
--
-- Both engines append to one WAL, so a flushed commit hardens every commit behind it (s3).
-- That is the whole mechanism: this row carries no meaning and is never read. It exists so
-- there is something cheap to write when the engine needs a commit to flush -- one row, one
-- WAL frame, one fsync, and the claim behind it becomes durable.
--
-- A dedicated row rather than re-writing the instance: the instance write is fenced and
-- carries the whole context, so using it would couple "make this durable" to lease state
-- and to an object diff, and would cost far more than a counter.
CREATE TABLE durability_marker (
    id INTEGER PRIMARY KEY,
    n  BIGINT NOT NULL
);

INSERT INTO durability_marker (id, n) VALUES (1, 0);
